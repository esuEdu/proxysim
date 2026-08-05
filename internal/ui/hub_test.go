package ui

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"proxysim/internal/flow"
	"proxysim/internal/flow/flowtest"
)

// sampleFlow builds a completed flow with the given id and populated bodies.
func sampleFlow(id uint64) *flow.Flow {
	return &flow.Flow{
		ID: id, Started: time.Unix(0, int64(id)).UTC(), Duration: time.Millisecond,
		Scheme: "https", Method: "GET", Host: "api.example.com", Path: "/v1/thing",
		RequestHeaders:  http.Header{"Accept": {"application/json"}},
		RequestBody:     []byte("request-body"),
		StatusCode:      200,
		ResponseHeaders: http.Header{"Content-Type": {"application/json"}},
		ResponseBody:    []byte(`{"ok":true}`),
		Intercepted:     true,
	}
}

// countingSink records how many flows it received, to prove other consumers in a
// MultiSink are unaffected by a stalled UI client.
type countingSink struct {
	mu sync.Mutex
	n  int
}

func (c *countingSink) Emit(*flow.Flow) {
	c.mu.Lock()
	c.n++
	c.mu.Unlock()
}

func (c *countingSink) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.n
}

// Criterion 1: additive and non-blocking. A stalled client (full buffer, nobody
// draining) causes drops, counted, while Emit returns and the co-sink still sees
// every flow.
func TestEmitNonBlockingWithStalledClient(t *testing.T) {
	h := New(100)
	other := &countingSink{}
	sink := flow.MultiSink{other, h}

	// A registered client whose buffer we deliberately never drain.
	stalled := &client{ch: make(chan []byte, 1)}
	h.addClient(stalled)

	const n = 50
	done := make(chan struct{})
	go func() {
		for i := 1; i <= n; i++ {
			sink.Emit(sampleFlow(uint64(i)))
		}
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Emit blocked on a stalled client")
	}

	if other.count() != n {
		t.Errorf("co-sink got %d flows, want %d — a stalled UI client must not affect other consumers", other.count(), n)
	}
	// One event fits the buffer; the rest overflow and are counted.
	if got := h.Dropped(); got != n-1 {
		t.Errorf("dropped = %d, want %d", got, n-1)
	}
}

// Criterion 2: history is bounded and newest-first.
func TestHistoryBounded(t *testing.T) {
	h := New(3)
	for i := 1; i <= 4; i++ { // one past capacity
		h.Emit(sampleFlow(uint64(i)))
	}

	srv := httptest.NewServer(h.Handler())
	defer srv.Close()

	var got []flow.Flow
	getJSON(t, srv.URL+"/flows", &got)

	if len(got) != 3 {
		t.Fatalf("history len = %d, want 3 (oldest evicted)", len(got))
	}
	wantIDs := []uint64{4, 3, 2} // newest first, id 1 evicted
	for i, id := range wantIDs {
		if got[i].ID != id {
			t.Errorf("history[%d].ID = %d, want %d", i, got[i].ID, id)
		}
	}
}

// Criterion 3: a live SSE client receives a flow emitted after it connects, as a
// well-formed data: event carrying the flow JSON.
func TestLiveStream(t *testing.T) {
	h := New(10)
	srv := httptest.NewServer(h.Handler())
	defer srv.Close()

	sc := openStream(t, srv.URL+"/events")
	defer sc.close()

	h.Emit(sampleFlow(7))

	f := sc.nextFlow(t)
	if f.ID != 7 || f.Host != "api.example.com" {
		t.Errorf("streamed flow = %+v, want id 7 for api.example.com", f)
	}
	// Metadata only: bodies must not ride the stream.
	if f.ResponseBody != nil || f.RequestBody != nil {
		t.Errorf("stream shipped bodies; it must carry metadata only: %+v", f)
	}
}

// Criterion 4: detail decodes a gzipped body and reports the raw size; unknown id
// is a 404.
func TestDetailDecodeAndNotFound(t *testing.T) {
	h := New(10)

	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	zw.Write([]byte(`{"hello":"world"}`))
	zw.Close()
	raw := gz.Bytes()

	f := sampleFlow(11)
	f.ResponseHeaders = http.Header{
		"Content-Type":     {"application/json"},
		"Content-Encoding": {"gzip"},
	}
	f.ResponseBody = raw
	h.Emit(f)

	srv := httptest.NewServer(h.Handler())
	defer srv.Close()

	var d detailResponse
	getJSON(t, srv.URL+"/flows/11", &d)

	if got := string(d.Response.Data); got != `{"hello":"world"}` {
		t.Errorf("decoded response = %q, want the gunzipped JSON", got)
	}
	if d.Response.RawSize != len(raw) {
		t.Errorf("raw_size = %d, want %d (the stored compressed size)", d.Response.RawSize, len(raw))
	}
	if !d.Response.Decoded {
		t.Error("decoded flag should be set for a gzipped body")
	}

	res, err := http.Get(srv.URL + "/flows/9999")
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusNotFound {
		t.Errorf("unknown id status = %d, want 404", res.StatusCode)
	}
}

// Criterion 5: the served flow JSON key set matches 003's golden set (shared
// assertion), so the browser format cannot drift. Checked both on the type the
// UI serializes and on the real detail endpoint's embedded flow.
func TestJSONContractMatchesGolden(t *testing.T) {
	full := &flow.Flow{
		ID: 1, Started: time.Unix(0, 0).UTC(), Duration: 1,
		Scheme: "https", Method: "GET", Host: "h", Path: "/",
		RequestHeaders: http.Header{}, RequestBody: []byte("x"), RequestTruncated: true,
		StatusCode: 200, ResponseHeaders: http.Header{}, ResponseBody: []byte("y"), ResponseTruncated: true,
		Intercepted: true, Error: "boom",
	}
	if got := flowtest.FieldSet(t, full); !reflect.DeepEqual(got, flowtest.GoldenFieldSet) {
		t.Errorf("UI flow field set drifted.\n got = %v\nwant = %v", got, flowtest.GoldenFieldSet)
	}

	h := New(10)
	h.Emit(full)
	srv := httptest.NewServer(h.Handler())
	defer srv.Close()

	var raw map[string]json.RawMessage
	getJSON(t, srv.URL+"/flows/1", &raw)
	if got := flowtest.FieldSet(t, jsonValue(t, raw["flow"])); !reflect.DeepEqual(got, flowtest.GoldenFieldSet) {
		t.Errorf("detail endpoint flow field set drifted.\n got = %v\nwant = %v", got, flowtest.GoldenFieldSet)
	}
}

// Criterion 6: the UI listener binds loopback only, mirroring the proxy.
func TestListenLoopbackOnly(t *testing.T) {
	ln, err := Listen(0)
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer ln.Close()

	tcp, ok := ln.Addr().(*net.TCPAddr)
	if !ok {
		t.Fatalf("addr type = %T, want *net.TCPAddr", ln.Addr())
	}
	if !tcp.IP.IsLoopback() {
		t.Errorf("UI bound to %s; it must bind loopback only", tcp.IP)
	}
}

// Criterion 7: several concurrent SSE clients each receive every flow emitted
// after they connect, race-clean under -race.
func TestConcurrentClients(t *testing.T) {
	h := New(50)
	srv := httptest.NewServer(h.Handler())
	defer srv.Close()

	const clients = 4
	const flowsN = 20

	streams := make([]*streamConn, clients)
	for i := range streams {
		streams[i] = openStream(t, srv.URL+"/events")
		defer streams[i].close()
	}
	// All clients are registered before we emit; give the goroutines a beat to
	// land in the select loop so no early flow is missed.
	waitForClients(t, h, clients)

	for i := 1; i <= flowsN; i++ {
		h.Emit(sampleFlow(uint64(i)))
	}

	var wg sync.WaitGroup
	for _, sc := range streams {
		wg.Add(1)
		go func(sc *streamConn) {
			defer wg.Done()
			seen := make(map[uint64]bool)
			for len(seen) < flowsN {
				seen[sc.nextFlow(t).ID] = true
			}
		}(sc)
	}
	wg.Wait()
}

// Criterion 8: the app is served at / with an HTML content type, and embedded
// assets serve with correct types.
func TestAppServed(t *testing.T) {
	h := New(10)
	srv := httptest.NewServer(h.Handler())
	defer srv.Close()

	cases := []struct{ path, wantType string }{
		{"/", "text/html"},
		{"/app.css", "text/css"},
		{"/app.js", "javascript"},
	}
	for _, c := range cases {
		res, err := http.Get(srv.URL + c.path)
		if err != nil {
			t.Fatalf("GET %s: %v", c.path, err)
		}
		body, _ := readAll(res)
		if res.StatusCode != http.StatusOK {
			t.Errorf("GET %s status = %d, want 200", c.path, res.StatusCode)
		}
		if ct := res.Header.Get("Content-Type"); !strings.Contains(ct, c.wantType) {
			t.Errorf("GET %s content-type = %q, want to contain %q", c.path, ct, c.wantType)
		}
		if len(body) == 0 {
			t.Errorf("GET %s served empty body", c.path)
		}
	}
}

// ---- helpers ----------------------------------------------------------------

func getJSON(t *testing.T, url string, into any) {
	t.Helper()
	res, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("GET %s status = %d", url, res.StatusCode)
	}
	if err := json.NewDecoder(res.Body).Decode(into); err != nil {
		t.Fatalf("decode %s: %v", url, err)
	}
}

func jsonValue(t *testing.T, raw json.RawMessage) any {
	t.Helper()
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatalf("unmarshal sub-value: %v", err)
	}
	return v
}

func readAll(res *http.Response) ([]byte, error) {
	defer res.Body.Close()
	var buf bytes.Buffer
	_, err := buf.ReadFrom(res.Body)
	return buf.Bytes(), err
}

func waitForClients(t *testing.T, h *Hub, n int) {
	t.Helper()
	for range 200 {
		h.mu.Lock()
		c := len(h.clients)
		h.mu.Unlock()
		if c >= n {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d SSE clients to register", n)
}

// streamConn reads an SSE response line by line, exposing the next data: flow.
type streamConn struct {
	res *http.Response
	sc  *bufio.Scanner
}

func openStream(t *testing.T, url string) *streamConn {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("open stream %s: %v", url, err)
	}
	if ct := res.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		res.Body.Close()
		t.Fatalf("stream content-type = %q, want text/event-stream", ct)
	}
	return &streamConn{res: res, sc: bufio.NewScanner(res.Body)}
}

// nextFlow returns the next flow from a data: line, skipping other SSE fields.
func (s *streamConn) nextFlow(t *testing.T) flow.Flow {
	t.Helper()
	for s.sc.Scan() {
		line := s.sc.Text()
		payload, ok := strings.CutPrefix(line, "data: ")
		if !ok || !strings.HasPrefix(payload, "{") {
			continue
		}
		var f flow.Flow
		if err := json.Unmarshal([]byte(payload), &f); err != nil {
			t.Fatalf("decode streamed flow %q: %v", payload, err)
		}
		return f
	}
	if err := s.sc.Err(); err != nil {
		t.Fatalf("stream read error: %v", err)
	}
	t.Fatal("stream closed before a flow arrived")
	return flow.Flow{}
}

func (s *streamConn) close() { s.res.Body.Close() }
