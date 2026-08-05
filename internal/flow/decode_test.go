package flow

import (
	"bytes"
	"compress/flate"
	"compress/gzip"
	"compress/zlib"
	"net/http"
	"strings"
	"testing"

	"github.com/andybalholm/brotli"
)

func gzipc(b []byte) []byte {
	var buf bytes.Buffer
	w := gzip.NewWriter(&buf)
	w.Write(b)
	w.Close()
	return buf.Bytes()
}

func zlibc(b []byte) []byte {
	var buf bytes.Buffer
	w := zlib.NewWriter(&buf)
	w.Write(b)
	w.Close()
	return buf.Bytes()
}

func rawflatec(b []byte) []byte {
	var buf bytes.Buffer
	w, _ := flate.NewWriter(&buf, flate.DefaultCompression)
	w.Write(b)
	w.Close()
	return buf.Bytes()
}

func brotlic(b []byte) []byte {
	var buf bytes.Buffer
	w := brotli.NewWriter(&buf)
	w.Write(b)
	w.Close()
	return buf.Bytes()
}

// Criterion 1: gzip decodes to the original.
func TestDecodeGzip(t *testing.T) {
	want := []byte("hello, gzip world")
	got, rep := Decode("gzip", gzipc(want), 0)
	if !bytes.Equal(got, want) {
		t.Errorf("decoded %q, want %q", got, want)
	}
	if !rep.Decoded || rep.Partial {
		t.Errorf("report = %+v, want Decoded/!Partial", rep)
	}
}

// Criterion 2: deflate decodes in both the zlib-wrapped and raw-DEFLATE flavours.
func TestDecodeDeflateBothFlavours(t *testing.T) {
	want := []byte("deflate payload that is reasonably long")

	if got, rep := Decode("deflate", zlibc(want), 0); !bytes.Equal(got, want) || !rep.Decoded {
		t.Errorf("zlib-wrapped deflate: got %q rep %+v", got, rep)
	}
	if got, rep := Decode("deflate", rawflatec(want), 0); !bytes.Equal(got, want) || !rep.Decoded {
		t.Errorf("raw deflate: got %q rep %+v", got, rep)
	}
}

// Criterion 3: brotli decodes to the original.
func TestDecodeBrotli(t *testing.T) {
	want := []byte("brotli payload")
	got, rep := Decode("br", brotlic(want), 0)
	if !bytes.Equal(got, want) || !rep.Decoded {
		t.Errorf("br: got %q rep %+v", got, rep)
	}
}

// Criterion 4: identity/absent/whitespace return the body unchanged, undecoded.
func TestDecodeIdentity(t *testing.T) {
	body := []byte("as sent")
	for _, ce := range []string{"", "identity", "   ", " identity "} {
		got, rep := Decode(ce, body, 0)
		if !bytes.Equal(got, body) {
			t.Errorf("ce=%q: got %q, want unchanged", ce, got)
		}
		if rep.Decoded {
			t.Errorf("ce=%q: Decoded=true, want false", ce)
		}
	}
}

// Criterion 5: stacked codings decode in reverse order.
func TestDecodeStacked(t *testing.T) {
	want := []byte("stacked deflate then gzip")
	// Applied deflate first, then gzip: Content-Encoding: deflate, gzip.
	stacked := gzipc(zlibc(want))
	got, rep := Decode("deflate, gzip", stacked, 0)
	if !bytes.Equal(got, want) {
		t.Errorf("decoded %q, want %q", got, want)
	}
	if !rep.Decoded || rep.Partial {
		t.Errorf("report = %+v", rep)
	}
}

// Criterion 6: an unknown coding passes the raw body through, marked partial.
func TestDecodeUnknownCoding(t *testing.T) {
	body := []byte("pretend-zstd-bytes")
	got, rep := Decode("zstd", body, 0)
	if !bytes.Equal(got, body) {
		t.Errorf("got %q, want raw body", got)
	}
	if !rep.Partial || !strings.Contains(rep.Note, "zstd") {
		t.Errorf("report = %+v, want Partial with zstd note", rep)
	}
}

// Criterion 7: a truncated stream yields a non-empty prefix, marked partial.
func TestDecodeTruncatedStream(t *testing.T) {
	// Low-compressibility data so the gzip stream is large enough that its first
	// half still decodes to a non-empty prefix before hitting the cut.
	payload := make([]byte, 20000)
	for i := range payload {
		payload[i] = byte(i*7 + i*i)
	}
	full := gzipc(payload)
	half := full[:len(full)/2] // cut the compressed stream in half

	got, rep := Decode("gzip", half, 0)
	if !rep.Partial {
		t.Errorf("report = %+v, want Partial", rep)
	}
	if len(got) == 0 {
		t.Error("expected a non-empty decoded prefix from a truncated stream")
	}
}

// Criterion 8: a body that inflates past the cap stops at the cap, marked partial.
func TestDecodeBombCap(t *testing.T) {
	body := gzipc(bytes.Repeat([]byte("A"), 100_000))
	got, rep := Decode("gzip", body, 32)
	if len(got) != 32 {
		t.Errorf("decoded %d bytes, want cap of 32", len(got))
	}
	if !rep.Partial {
		t.Errorf("report = %+v, want Partial", rep)
	}
}

// Criterion 9: Decode does not mutate its input slice.
func TestDecodeInputImmutable(t *testing.T) {
	body := gzipc([]byte("do not touch my bytes"))
	orig := append([]byte(nil), body...)
	Decode("gzip", body, 0)
	if !bytes.Equal(body, orig) {
		t.Error("Decode mutated its input slice")
	}
}

// Criterion 10: the console decodes a gzipped JSON body for display while the
// stored flow bytes stay compressed.
func TestConsoleDecodesGzipJSON(t *testing.T) {
	raw := gzipc([]byte(`{"a":1,"b":2}`))
	f := &Flow{
		ID: 1, Method: "GET", Scheme: "https", Host: "h", Path: "/", Intercepted: true,
		RequestHeaders: http.Header{},
		ResponseHeaders: http.Header{
			"Content-Type":     {"application/json"},
			"Content-Encoding": {"gzip"},
		},
		ResponseBody: raw,
	}

	var buf bytes.Buffer
	NewConsoleSink(&buf, true).Emit(f)
	out := buf.String()

	if !strings.Contains(out, "\"a\": 1") {
		t.Errorf("gzipped JSON was not decoded/pretty-printed:\n%s", out)
	}
	if !strings.Contains(out, "decoded from gzip") {
		t.Errorf("output does not note the gzip decoding:\n%s", out)
	}
	if !bytes.Equal(f.ResponseBody, raw) {
		t.Error("stored ResponseBody was mutated; it must stay compressed")
	}
}
