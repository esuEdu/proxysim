package proxy

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"proxysim/internal/flow"
)

// capSink records emitted flows.
type capSink struct {
	mu    sync.Mutex
	flows []*flow.Flow
}

func (c *capSink) Emit(f *flow.Flow) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.flows = append(c.flows, f)
}

func (c *capSink) all() []*flow.Flow {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]*flow.Flow(nil), c.flows...)
}

// proxiedClient returns an http.Client that routes through the given proxy
// front, with compression disabled so relayed bytes can be inspected verbatim.
func proxiedClient(t *testing.T, front *httptest.Server) *http.Client {
	t.Helper()
	pu, err := url.Parse(front.URL)
	if err != nil {
		t.Fatal(err)
	}
	return &http.Client{Transport: &http.Transport{
		Proxy:              http.ProxyURL(pu),
		DisableCompression: true,
	}}
}

// rawProxyRequest writes a verbatim request to the proxy front and returns the
// raw response. It sidesteps the Go client, which would rewrite Connection.
func rawProxyRequest(t *testing.T, front *httptest.Server, raw string) *http.Response {
	t.Helper()
	addr := strings.TrimPrefix(front.URL, "http://")
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	if _, err := io.WriteString(conn, raw); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

// Criterion 1 & 2: a GET is forwarded byte-for-byte and produces one accurate flow.
func TestForwardAndFlow(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Origin", "yes")
		fmt.Fprint(w, "hello from origin")
	}))
	defer origin.Close()

	sink := &capSink{}
	front := httptest.NewServer(New(sink, nil))
	defer front.Close()

	client := proxiedClient(t, front)
	resp, err := client.Get(origin.URL + "/greet?x=1")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != 200 || string(body) != "hello from origin" {
		t.Fatalf("relay wrong: %d %q", resp.StatusCode, body)
	}

	flows := sink.all()
	if len(flows) != 1 {
		t.Fatalf("got %d flows, want 1", len(flows))
	}
	f := flows[0]
	if f.Scheme != "http" || f.Method != "GET" || f.StatusCode != 200 || !f.Intercepted {
		t.Errorf("flow fields wrong: %+v", f)
	}
	if f.Path != "/greet?x=1" {
		t.Errorf("Path = %q, want /greet?x=1", f.Path)
	}
	originHost := strings.TrimPrefix(origin.URL, "http://")
	if f.Host != originHost {
		t.Errorf("Host = %q, want %q", f.Host, originHost)
	}
	if string(f.ResponseBody) != "hello from origin" {
		t.Errorf("captured response body = %q", f.ResponseBody)
	}
}

// Criterion 3: hop-by-hop headers are stripped in both directions.
func TestHopByHopStripping(t *testing.T) {
	var gotHeaders http.Header
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeaders = r.Header.Clone()
		w.Header().Set("Keep-Alive", "timeout=5")
		w.Header().Set("X-End-To-End", "kept")
		w.WriteHeader(200)
	}))
	defer origin.Close()

	front := httptest.NewServer(New(&capSink{}, nil))
	defer front.Close()

	raw := fmt.Sprintf("GET %s/ HTTP/1.1\r\nHost: %s\r\nProxy-Connection: keep-alive\r\nConnection: X-Custom\r\nX-Custom: secret\r\nX-Kept: yes\r\n\r\n",
		origin.URL, strings.TrimPrefix(origin.URL, "http://"))
	resp := rawProxyRequest(t, front, raw)
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)

	for _, h := range []string{"Proxy-Connection", "Connection", "X-Custom"} {
		if v := gotHeaders.Get(h); v != "" {
			t.Errorf("origin saw hop-by-hop header %s: %q", h, v)
		}
	}
	if gotHeaders.Get("X-Kept") != "yes" {
		t.Error("end-to-end header X-Kept was dropped")
	}
	if resp.Header.Get("Keep-Alive") != "" {
		t.Error("Keep-Alive response header reached the client")
	}
	if resp.Header.Get("X-End-To-End") != "kept" {
		t.Error("end-to-end response header was dropped")
	}
}

// Criterion 4: bodies are captured up to the cap but forwarded in full.
func TestBodyCaptureCappedButForwardedWhole(t *testing.T) {
	const cap = 8
	var originRead int
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		originRead = len(b)
		// Respond with a body larger than the cap too.
		w.Write(bytes.Repeat([]byte("R"), cap+20))
	}))
	defer origin.Close()

	sink := &capSink{}
	front := httptest.NewServer(New(sink, nil, WithBodyCap(cap)))
	defer front.Close()

	reqBody := bytes.Repeat([]byte("Q"), cap+15)
	client := proxiedClient(t, front)
	resp, err := client.Post(origin.URL+"/", "application/octet-stream", bytes.NewReader(reqBody))
	if err != nil {
		t.Fatal(err)
	}
	respBody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if originRead != len(reqBody) {
		t.Errorf("origin read %d bytes, want full %d", originRead, len(reqBody))
	}
	if len(respBody) != cap+20 {
		t.Errorf("client read %d response bytes, want full %d", len(respBody), cap+20)
	}

	f := sink.all()[0]
	if len(f.RequestBody) != cap || !f.RequestTruncated {
		t.Errorf("request capture = %d bytes trunc=%v, want %d/true", len(f.RequestBody), f.RequestTruncated, cap)
	}
	if len(f.ResponseBody) != cap || !f.ResponseTruncated {
		t.Errorf("response capture = %d bytes trunc=%v, want %d/true", len(f.ResponseBody), f.ResponseTruncated, cap)
	}
}

// Criterion 5: content-encoding is preserved on the wire and in the capture.
func TestCompressionFidelity(t *testing.T) {
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	zw.Write([]byte("compressed payload"))
	zw.Close()
	compressed := gz.Bytes()

	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Encoding", "gzip")
		w.Write(compressed)
	}))
	defer origin.Close()

	sink := &capSink{}
	front := httptest.NewServer(New(sink, nil))
	defer front.Close()

	client := proxiedClient(t, front)
	resp, err := client.Get(origin.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if !bytes.Equal(got, compressed) {
		t.Error("client did not receive the identical compressed bytes")
	}
	f := sink.all()[0]
	if len(f.ResponseBody) < 2 || f.ResponseBody[0] != 0x1f || f.ResponseBody[1] != 0x8b {
		t.Errorf("captured body is not gzip: % x", f.ResponseBody[:min(4, len(f.ResponseBody))])
	}
}

// Criterion 6: an unreachable upstream yields 502 and an errored flow.
func TestUpstreamFailure(t *testing.T) {
	sink := &capSink{}
	front := httptest.NewServer(New(sink, nil))
	defer front.Close()

	client := proxiedClient(t, front)
	// 127.0.0.1:1 is reserved and not listening.
	resp, err := client.Get("http://127.0.0.1:1/")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", resp.StatusCode)
	}
	f := sink.all()[0]
	if f.Error == "" || f.StatusCode != 0 {
		t.Errorf("errored flow wrong: error=%q status=%d", f.Error, f.StatusCode)
	}
}

// Criterion 7: an origin-form (non-proxy) request is rejected, not forwarded.
func TestNonProxyRequestRejected(t *testing.T) {
	sink := &capSink{}
	front := httptest.NewServer(New(sink, nil))
	defer front.Close()

	// Hitting the front directly, without proxy configuration, sends origin-form.
	resp, err := http.Get(front.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
	if len(sink.all()) != 0 {
		t.Errorf("a non-proxy request produced %d flows, want 0", len(sink.all()))
	}
}

// Criterion 8: concurrent requests are race-clean and each yields a flow.
func TestConcurrentRequests(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "ok")
	}))
	defer origin.Close()

	sink := &capSink{}
	front := httptest.NewServer(New(sink, nil))
	defer front.Close()
	client := proxiedClient(t, front)

	const n = 50
	var wg sync.WaitGroup
	for range n {
		wg.Go(func() {
			resp, err := client.Get(origin.URL + "/")
			if err != nil {
				t.Error(err)
				return
			}
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		})
	}
	wg.Wait()

	if got := len(sink.all()); got != n {
		t.Errorf("got %d flows, want %d", got, n)
	}
}
