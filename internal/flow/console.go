package flow

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
)

// ConsoleSink renders flows as human-readable blocks on an io.Writer. It is
// synchronous and fast enough to sit directly on the request path. Writes are
// serialized so concurrent flows do not interleave mid-line.
//
// Headers and bodies print only when Verbose is set: a chatty app makes full
// output unreadable within seconds, so the default is a one-line summary per
// exchange.
type ConsoleSink struct {
	w       io.Writer
	verbose bool
	mu      sync.Mutex
}

// NewConsoleSink writes flows to w. When verbose is true, headers and bodies
// are included; otherwise each flow is a single summary line.
func NewConsoleSink(w io.Writer, verbose bool) *ConsoleSink {
	return &ConsoleSink{w: w, verbose: verbose}
}

// Emit renders one flow. It builds the whole block in memory and writes it
// under a lock in a single call, so output from concurrent flows stays intact.
func (c *ConsoleSink) Emit(f *Flow) {
	var b strings.Builder
	c.summary(&b, f)
	if c.verbose {
		c.verboseDetail(&b, f)
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	io.WriteString(c.w, b.String())
}

func (c *ConsoleSink) summary(b *strings.Builder, f *Flow) {
	fmt.Fprintf(b, "[#%d] %s %s://%s%s", f.ID, f.Method, f.Scheme, f.Host, f.Path)

	switch {
	case f.Error != "":
		fmt.Fprintf(b, " ERROR: %s", f.Error)
	case !f.Intercepted:
		// A blind tunnel: the host could not be intercepted (pinned, or opaque
		// CONNECT). Say so rather than implying a captured exchange.
		b.WriteString(" [tunnelled, not inspected]")
	default:
		fmt.Fprintf(b, " %d", f.StatusCode)
	}

	fmt.Fprintf(b, " (%s) ↑%s ↓%s\n",
		f.Duration.Round(1e6), // to milliseconds
		humanBytes(len(f.RequestBody)),
		humanBytes(len(f.ResponseBody)),
	)
}

func (c *ConsoleSink) verboseDetail(b *strings.Builder, f *Flow) {
	b.WriteString("  Request headers:\n")
	writeHeaders(b, f.RequestHeaders)
	writeBody(b, "  Request body", f.RequestHeaders, f.RequestBody, f.RequestTruncated)

	if f.Intercepted && f.Error == "" {
		b.WriteString("  Response headers:\n")
		writeHeaders(b, f.ResponseHeaders)
		writeBody(b, "  Response body", f.ResponseHeaders, f.ResponseBody, f.ResponseTruncated)
	}
	b.WriteString("\n")
}

func writeHeaders(b *strings.Builder, h http.Header) {
	keys := make([]string, 0, len(h))
	for k := range h {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		for _, v := range h[k] {
			fmt.Fprintf(b, "    %s: %s\n", k, v)
		}
	}
}

func writeBody(b *strings.Builder, label string, h http.Header, body []byte, truncated bool) {
	// An empty body and a truncated one must be visibly different: the former
	// says nothing crossed, the latter that we declined to buffer the rest.
	if len(body) == 0 {
		if truncated {
			fmt.Fprintf(b, "%s: [truncated; body not buffered]\n", label)
		} else {
			fmt.Fprintf(b, "%s: <empty>\n", label)
		}
		return
	}

	// Decode Content-Encoding for display; the stored body stays compressed.
	decoded, report := Decode(h.Get("Content-Encoding"), body, 0)

	ct := h.Get("Content-Type")
	if isTextual(ct, decoded) {
		fmt.Fprintf(b, "%s (%s%s):\n", label, humanBytes(len(decoded)), codingSuffix(report))
		b.WriteString(renderText(ct, decoded))
		if !strings.HasSuffix(b.String(), "\n") {
			b.WriteString("\n")
		}
	} else {
		// Binary content: describe it rather than dumping bytes at the terminal.
		fmt.Fprintf(b, "%s: <binary %s, %s%s>\n", label, contentTypeOr(ct, "application/octet-stream"), humanBytes(len(decoded)), codingSuffix(report))
	}

	// A partially decodable body and a capture-truncated one are different
	// facts; show each on its own so neither is mistaken for the other.
	if report.Partial {
		fmt.Fprintf(b, "%s  [%s]\n", label, report.Note)
	}
	if truncated {
		fmt.Fprintf(b, "%s  [captured %s, truncated]\n", label, humanBytes(len(body)))
	}
}

// codingSuffix annotates a decoded body line with the coding it was decoded
// from, e.g. " gzip → " sits between the label and the decoded size.
func codingSuffix(report DecodeReport) string {
	if !report.Decoded || len(report.Encodings) == 0 {
		return ""
	}
	return ", decoded from " + strings.Join(report.Encodings, ", ")
}

// renderText pretty-prints JSON bodies and passes other text through verbatim.
func renderText(ct string, body []byte) string {
	if strings.Contains(strings.ToLower(ct), "json") {
		var pretty bytes.Buffer
		if err := json.Indent(&pretty, body, "    ", "  "); err == nil {
			return "    " + pretty.String()
		}
		// Fall through on invalid JSON: show the raw bytes rather than nothing.
	}
	return "    " + string(body)
}

// isTextual reports whether body should be shown as text, preferring the
// declared Content-Type and falling back to a valid-UTF-8 sniff.
func isTextual(ct string, body []byte) bool {
	lc := strings.ToLower(ct)
	switch {
	case strings.HasPrefix(lc, "text/"),
		strings.Contains(lc, "json"),
		strings.Contains(lc, "xml"),
		strings.Contains(lc, "javascript"),
		strings.Contains(lc, "x-www-form-urlencoded"):
		return true
	case ct == "":
		return utf8Printable(body)
	default:
		return false
	}
}

func utf8Printable(body []byte) bool {
	// Treat a NUL byte as the tell for binary; otherwise assume text.
	return !bytes.ContainsRune(body, 0)
}

func contentTypeOr(ct, fallback string) string {
	if ct == "" {
		return fallback
	}
	if i := strings.IndexByte(ct, ';'); i >= 0 {
		ct = ct[:i]
	}
	return strings.TrimSpace(ct)
}

func humanBytes(n int) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%dB", n)
	}
	div, exp := int64(unit), 0
	for m := int64(n) / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f%ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
