package flow

import (
	"bytes"
	"compress/flate"
	"compress/gzip"
	"compress/zlib"
	"fmt"
	"io"
	"strings"

	"github.com/andybalholm/brotli"
)

// DecodeReport explains what Decode did to a body. It exists so a renderer can
// distinguish "shown as sent" from "decompressed" from "could not fully decode",
// and say so to the user rather than silently showing raw or partial bytes.
type DecodeReport struct {
	// Encodings are the content codings seen, in wire order (outermost first).
	Encodings []string
	// Decoded reports whether any decoding was actually applied.
	Decoded bool
	// Partial reports that decoding stopped early: a truncated stream, the cap,
	// or an unsupported coding. decoded then holds the bytes obtained so far.
	Partial bool
	// Note is a human-facing reason, set when Partial or an unknown coding was
	// hit; empty otherwise.
	Note string
}

// Decode returns body decoded per the Content-Encoding header value, bounded by
// cap bytes (0 means DefaultBodyCap). It never returns an error: a truncated,
// bomb-sized, or unknown-encoding body yields the best partial result with the
// report explaining what happened. The input slice is never modified.
//
// Bodies are stored compressed (spec 003), so this runs at render time; storing
// its output would make byte counts lie and lose what actually crossed the wire.
func Decode(contentEncoding string, body []byte, cap int) ([]byte, DecodeReport) {
	if cap <= 0 {
		cap = DefaultBodyCap
	}

	codings := parseCodings(contentEncoding)
	report := DecodeReport{Encodings: codings}
	if len(codings) == 0 {
		// Absent, empty, or identity: shown as sent.
		return body, report
	}

	// Content-Encoding lists codings in the order they were applied, so undo
	// them in reverse: the last-applied (outermost) coding comes off first.
	out := body
	for i := len(codings) - 1; i >= 0; i-- {
		coding := codings[i]

		dec, ok := decoderFor(coding)
		if !ok {
			// Unknown coding: stop and hand back what we have, still compressed
			// under this layer, so the UI can say why rather than show garbage.
			report.Partial = true
			report.Note = "unsupported content-encoding: " + coding
			return out, report
		}

		decoded, partial, err := decodeOne(dec, out, cap)
		if err != nil {
			// A layer failed outright (e.g. not actually this coding). Return the
			// bytes as they were before this layer, noted, rather than erroring.
			report.Partial = true
			report.Note = fmt.Sprintf("could not decode %s: %v", coding, err)
			return out, report
		}
		out = decoded
		report.Decoded = true
		if partial {
			// Truncated stream or hit the cap: what we have is the readable
			// prefix; decoding further layers would only compound the loss.
			report.Partial = true
			report.Note = "body truncated before decoding completed"
			return out, report
		}
	}

	return out, report
}

// decoder builds a decompressing reader over r for one content coding.
type decoder func(r io.Reader) (io.Reader, error)

func decoderFor(coding string) (decoder, bool) {
	switch coding {
	case "gzip", "x-gzip":
		return func(r io.Reader) (io.Reader, error) { return gzip.NewReader(r) }, true
	case "deflate":
		return inflate, true
	case "br":
		return func(r io.Reader) (io.Reader, error) { return brotli.NewReader(r), nil }, true
	default:
		return nil, false
	}
}

// inflate handles the deflate ambiguity: the token nominally means zlib-wrapped
// DEFLATE (RFC 1950), but many servers send raw DEFLATE (RFC 1951). Try zlib
// first, then fall back to raw flate, so valid raw bodies do not show as errors.
func inflate(r io.Reader) (io.Reader, error) {
	buf, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	if zr, err := zlib.NewReader(bytes.NewReader(buf)); err == nil {
		return zr, nil
	}
	return flate.NewReader(bytes.NewReader(buf)), nil
}

// decodeOne runs one decoder over in, reading at most cap bytes. A truncated
// stream reports partial with the bytes read so far; a cap hit reports partial
// too. Only a genuine construction failure returns an error.
func decodeOne(dec decoder, in []byte, cap int) (out []byte, partial bool, err error) {
	r, err := dec(bytes.NewReader(in))
	if err != nil {
		return nil, false, err
	}
	if c, ok := r.(io.Closer); ok {
		defer c.Close()
	}

	// Read one byte past the cap to detect overflow without buffering it all.
	limited := io.LimitReader(r, int64(cap)+1)
	buf, readErr := io.ReadAll(limited)

	if len(buf) > cap {
		return buf[:cap], true, nil // bomb / oversized: keep cap bytes, mark partial
	}
	if readErr != nil {
		// Unexpected EOF and friends mean a truncated compressed stream; the
		// bytes decoded so far are the readable prefix, not a failure.
		if isTruncation(readErr) {
			return buf, true, nil
		}
		return nil, false, readErr
	}
	return buf, false, nil
}

func isTruncation(err error) bool {
	return err == io.ErrUnexpectedEOF ||
		strings.Contains(err.Error(), "unexpected EOF") ||
		strings.Contains(err.Error(), "truncated") ||
		strings.Contains(err.Error(), "corrupt")
}

// parseCodings splits a Content-Encoding value into lowercase tokens, dropping
// identity, which is a no-op coding.
func parseCodings(value string) []string {
	var out []string
	for token := range strings.SplitSeq(value, ",") {
		t := strings.ToLower(strings.TrimSpace(token))
		if t == "" || t == "identity" {
			continue
		}
		out = append(out, t)
	}
	return out
}
