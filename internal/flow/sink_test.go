package flow

import (
	"bytes"
	"net/http"
	"strings"
	"testing"
	"time"
)

// slowSink blocks in Emit, standing in for a wedged consumer.
type slowSink struct{ d time.Duration }

func (s slowSink) Emit(*Flow) { time.Sleep(s.d) }

// Criterion 4: a slow sink wrapped in AsyncSink does not delay the caller past
// the buffer filling; excess flows are dropped and counted.
func TestAsyncSinkDropsRatherThanBlocks(t *testing.T) {
	async := NewAsyncSink(slowSink{d: time.Second}, 1)
	defer async.Close()

	const n = 100
	start := time.Now()
	for range n {
		async.Emit(&Flow{})
	}
	elapsed := time.Since(start)

	if elapsed > 500*time.Millisecond {
		t.Errorf("Emit loop took %v; a full buffer must not block the caller", elapsed)
	}
	// With a 1-deep buffer and a 1s-per-flow drainer, the vast majority drop.
	if async.Dropped() == 0 {
		t.Error("expected dropped flows, got 0")
	}
}

// Criterion 5: a truncated body renders with a visible marker, distinct from an
// empty body.
func TestConsoleTruncationIsVisibleAndDistinct(t *testing.T) {
	var truncated bytes.Buffer
	NewConsoleSink(&truncated, true).Emit(&Flow{
		ID: 1, Method: "GET", Scheme: "https", Host: "h", Path: "/",
		RequestHeaders: http.Header{}, RequestBody: nil, RequestTruncated: true,
		Intercepted: true, ResponseHeaders: http.Header{},
	})

	var empty bytes.Buffer
	NewConsoleSink(&empty, true).Emit(&Flow{
		ID: 2, Method: "GET", Scheme: "https", Host: "h", Path: "/",
		RequestHeaders: http.Header{}, RequestBody: nil, RequestTruncated: false,
		Intercepted: true, ResponseHeaders: http.Header{},
	})

	if !strings.Contains(truncated.String(), "truncated") {
		t.Errorf("truncated flow has no truncation marker:\n%s", truncated.String())
	}
	if strings.Contains(empty.String(), "truncated") {
		t.Errorf("empty flow wrongly shows a truncation marker:\n%s", empty.String())
	}
	if truncated.String() == empty.String() {
		t.Error("truncated and empty bodies render identically")
	}
}

// A JSON body pretty-prints; a binary body is summarized, not dumped.
func TestConsoleBodyRendering(t *testing.T) {
	var jsonBuf bytes.Buffer
	NewConsoleSink(&jsonBuf, true).Emit(&Flow{
		ID: 1, Method: "POST", Scheme: "https", Host: "h", Path: "/",
		RequestHeaders: http.Header{"Content-Type": {"application/json"}},
		RequestBody:    []byte(`{"a":1,"b":[2,3]}`),
		Intercepted:    true, ResponseHeaders: http.Header{},
	})
	if !strings.Contains(jsonBuf.String(), "\"a\": 1") {
		t.Errorf("JSON body was not pretty-printed:\n%s", jsonBuf.String())
	}

	var binBuf bytes.Buffer
	NewConsoleSink(&binBuf, true).Emit(&Flow{
		ID: 2, Method: "POST", Scheme: "https", Host: "h", Path: "/",
		RequestHeaders: http.Header{"Content-Type": {"image/png"}},
		RequestBody:    []byte{0x89, 0x50, 0x4e, 0x47, 0x00, 0x01},
		Intercepted:    true, ResponseHeaders: http.Header{},
	})
	out := binBuf.String()
	if !strings.Contains(out, "<binary image/png") {
		t.Errorf("binary body was not summarized:\n%s", out)
	}
	if strings.Contains(out, "\x89") {
		t.Error("binary bytes were dumped to the terminal")
	}
}

// A blind tunnel and an errored flow render distinctly from a normal 200.
func TestConsoleTunnelAndError(t *testing.T) {
	var tunnel bytes.Buffer
	NewConsoleSink(&tunnel, false).Emit(&Flow{
		ID: 1, Method: "CONNECT", Scheme: "https", Host: "pinned.example.com", Path: "",
		Intercepted: false,
	})
	if !strings.Contains(tunnel.String(), "tunnelled") {
		t.Errorf("blind tunnel not marked:\n%s", tunnel.String())
	}

	var errBuf bytes.Buffer
	NewConsoleSink(&errBuf, false).Emit(&Flow{
		ID: 2, Method: "GET", Scheme: "https", Host: "h", Path: "/",
		Error: "dial tcp: connection refused",
	})
	if !strings.Contains(errBuf.String(), "ERROR: dial tcp") {
		t.Errorf("error not shown:\n%s", errBuf.String())
	}
}
