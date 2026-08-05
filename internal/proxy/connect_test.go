package proxy

import (
	"bufio"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"proxysim/internal/ca"
)

// waitForFlows polls until the sink holds at least n flows, since tunnelled
// flows are emitted only when the connection closes.
func waitForFlows(t *testing.T, sink *capSink, n int) {
	t.Helper()
	for range 200 {
		if len(sink.all()) >= n {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d flows, have %d", n, len(sink.all()))
}

func testAuthority(t *testing.T) *ca.Authority {
	t.Helper()
	a, err := ca.Load(t.TempDir())
	if err != nil {
		t.Fatalf("ca.Load: %v", err)
	}
	return a
}

// caPool returns a cert pool trusting the proxy's root, for a client that should
// accept our minted leaves.
func caPool(a *ca.Authority) *x509.CertPool {
	p := x509.NewCertPool()
	p.AddCert(a.Certificate())
	return p
}

// httpsProxyClient builds a client that CONNECTs through front and trusts the
// certs in pool for the terminated TLS.
func httpsProxyClient(t *testing.T, front *httptest.Server, pool *x509.CertPool) *http.Client {
	t.Helper()
	pu, err := url.Parse(front.URL)
	if err != nil {
		t.Fatal(err)
	}
	return &http.Client{Transport: &http.Transport{
		Proxy:              http.ProxyURL(pu),
		TLSClientConfig:    &tls.Config{RootCAs: pool},
		DisableCompression: true,
	}}
}

// insecureUpstream trusts any origin cert, the seam for httptest TLS servers.
func insecureUpstream() Option {
	return WithUpstreamTLS(&tls.Config{InsecureSkipVerify: true})
}

// Criterion 1: CONNECT is answered with exactly the 200 established line.
func TestConnectHandshake(t *testing.T) {
	origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer origin.Close()

	front := httptest.NewServer(New(&capSink{}, testAuthority(t), insecureUpstream()))
	defer front.Close()

	conn, err := net.Dial("tcp", strings.TrimPrefix(front.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	target := strings.TrimPrefix(origin.URL, "https://")
	fmt.Fprintf(conn, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", target, target)

	line, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(line) != "HTTP/1.1 200 Connection Established" {
		t.Errorf("CONNECT reply = %q", line)
	}
}

// Criteria 2 & 4: an intercepted request is decrypted, forwarded, and captured;
// the httptest origin is at 127.0.0.1, so this also exercises the no-SNI IP leaf.
func TestInterception(t *testing.T) {
	origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "secret for %s", r.URL.Path)
	}))
	defer origin.Close()

	a := testAuthority(t)
	sink := &capSink{}
	front := httptest.NewServer(New(sink, a, insecureUpstream()))
	defer front.Close()

	client := httpsProxyClient(t, front, caPool(a))
	resp, err := client.Get(origin.URL + "/vault")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if string(body) != "secret for /vault" {
		t.Errorf("body = %q", body)
	}
	flows := sink.all()
	if len(flows) != 1 {
		t.Fatalf("got %d flows, want 1", len(flows))
	}
	f := flows[0]
	if !f.Intercepted || f.Scheme != "https" || f.Method != "GET" || f.Path != "/vault" || f.StatusCode != 200 {
		t.Errorf("intercepted flow wrong: %+v", f)
	}
	if string(f.ResponseBody) != "secret for /vault" {
		t.Errorf("captured response body = %q", f.ResponseBody)
	}
}

// Criterion 3: a client that pipelines its ClientHello past the CONNECT line is
// not broken — bufConn drains the buffered bytes before the socket.
func TestBufferedBytesDrainedFirst(t *testing.T) {
	// Simulate a hijack whose buffered reader already holds post-CONNECT bytes.
	buffered := []byte("CLIENT-HELLO-BYTES")
	rest := []byte("...more from the socket")
	br := bufio.NewReader(io.MultiReader(strings.NewReader(string(buffered)), strings.NewReader(string(rest))))

	c := &bufConn{Conn: nil, r: br}
	got, err := io.ReadAll(c)
	if err != nil {
		t.Fatal(err)
	}
	if want := string(buffered) + string(rest); string(got) != want {
		t.Errorf("drained %q, want %q", got, want)
	}
}

// Criterion 5: a statically excluded host is tunnelled — the client's TLS reaches
// the origin's real cert, and the flow is marked not intercepted.
func TestStaticExclusionTunnels(t *testing.T) {
	origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "direct from origin")
	}))
	defer origin.Close()

	a := testAuthority(t)
	sink := &capSink{}
	front := httptest.NewServer(New(sink, a, WithExcludedHosts("127.0.0.1")))
	defer front.Close()

	// The client trusts only the origin's cert, not our CA: it can only succeed
	// if it is talking straight to the origin through a blind tunnel.
	client := httpsProxyClient(t, front, originPool(origin))
	resp, err := client.Get(origin.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if string(body) != "direct from origin" {
		t.Errorf("body = %q", body)
	}
	// The tunnel flow emits when the connection closes.
	client.CloseIdleConnections()
	waitForFlows(t, sink, 1)
	flows := sink.all()
	if len(flows) != 1 || flows[0].Intercepted {
		t.Fatalf("expected one un-intercepted flow, got %+v", flows)
	}
}

// Criterion 6: a client that rejects our leaf (a stand-in for a pinned app) fails
// the first interception, and the host is then tunnelled on the next connection.
func TestLearnedExclusionAfterPinning(t *testing.T) {
	origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "origin reached")
	}))
	defer origin.Close()

	a := testAuthority(t)
	sink := &capSink{}
	front := httptest.NewServer(New(sink, a, insecureUpstream()))
	defer front.Close()

	// "Pinned" client: trusts the origin cert but NOT our CA, so it rejects the
	// leaf we present when intercepting.
	pinned := httpsProxyClient(t, front, originPool(origin))

	// First connection: interception is attempted and refused.
	if _, err := pinned.Get(origin.URL + "/"); err == nil {
		t.Fatal("expected first request to fail against the intercepting proxy")
	}

	// Second connection: the host is now learned, so it tunnels to the origin.
	pinned.CloseIdleConnections()
	resp, err := pinned.Get(origin.URL + "/")
	if err != nil {
		t.Fatalf("second request should tunnel and succeed: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(body) != "origin reached" {
		t.Errorf("body = %q", body)
	}

	// Flow 1 (failed interception) emitted synchronously; flow 2 (the tunnel)
	// emits on close.
	pinned.CloseIdleConnections()
	waitForFlows(t, sink, 2)
	flows := sink.all()
	if len(flows) != 2 {
		t.Fatalf("got %d flows, want 2", len(flows))
	}
	for i, f := range flows {
		if f.Intercepted {
			t.Errorf("flow %d intercepted; both should be tunnelled", i)
		}
	}
	if flows[0].Error == "" {
		t.Error("first (failed-interception) flow should carry an error note")
	}
}

// Criterion 7: two requests over one intercepted connection produce two flows.
func TestInterceptKeepAlive(t *testing.T) {
	origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "ok")
	}))
	defer origin.Close()

	a := testAuthority(t)
	sink := &capSink{}
	front := httptest.NewServer(New(sink, a, insecureUpstream()))
	defer front.Close()

	client := httpsProxyClient(t, front, caPool(a))
	for range 2 {
		resp, err := client.Get(origin.URL + "/")
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}

	if got := len(sink.all()); got != 2 {
		t.Errorf("got %d flows, want 2", got)
	}
}

// Criterion 8: an origin whose cert does not verify yields an errored flow and a
// client-visible failure, not a panic.
func TestUpstreamTLSVerificationFailure(t *testing.T) {
	origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer origin.Close()

	a := testAuthority(t)
	sink := &capSink{}
	// No insecureUpstream: the proxy verifies the self-signed origin against
	// system roots and fails.
	front := httptest.NewServer(New(sink, a))
	defer front.Close()

	client := httpsProxyClient(t, front, caPool(a))
	resp, err := client.Get(origin.URL + "/")
	if err != nil {
		t.Fatalf("client should get a 502, not a transport error: %v", err)
	}
	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", resp.StatusCode)
	}
	resp.Body.Close()

	flows := sink.all()
	if len(flows) != 1 || flows[0].Error == "" {
		t.Fatalf("expected one errored flow, got %+v", flows)
	}
}

// Criterion 9: concurrent intercept and tunnel connections are race-clean.
func TestConcurrentConnects(t *testing.T) {
	origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "ok")
	}))
	// Silence the origin's handshake-error logging: tearing tunnels down at the
	// end of the test races the origin mid-handshake, which is cosmetic here.
	origin.Config.ErrorLog = log.New(io.Discard, "", 0)
	defer origin.Close()

	a := testAuthority(t)
	sink := &capSink{}
	// Exclude nothing globally; drive both paths with two clients.
	front := httptest.NewServer(New(sink, a, insecureUpstream()))
	defer front.Close()
	excludingFront := httptest.NewServer(New(&capSink{}, a, WithExcludedHosts("127.0.0.1")))
	defer excludingFront.Close()

	intercepting := httpsProxyClient(t, front, caPool(a))
	tunnelling := httpsProxyClient(t, excludingFront, originPool(origin))

	const n = 20
	var wg sync.WaitGroup
	for i := range n {
		client := intercepting
		if i%2 == 0 {
			client = tunnelling
		}
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

	// Only the intercepting front's sink is asserted; half the requests hit it.
	if got := len(sink.all()); got != n/2 {
		t.Errorf("intercepting sink saw %d flows, want %d", got, n/2)
	}
}

// originPool trusts an httptest server's own certificate.
func originPool(s *httptest.Server) *x509.CertPool {
	p := x509.NewCertPool()
	p.AddCert(s.Certificate())
	return p
}
