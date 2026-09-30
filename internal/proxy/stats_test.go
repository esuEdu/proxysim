package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"time"
)

func TestStatsStartAtZero(t *testing.T) {
	// A fresh server reporting anything but zero would make the first poll from a
	// host app look like traffic it never saw.
	got := New(&capSink{}, nil).Stats()

	if got != (Stats{}) {
		t.Errorf("Stats() = %+v, want zero", got)
	}
}

func TestActiveSumsBothKinds(t *testing.T) {
	// A host app shows one "connections" number and should not have to know proxysim
	// tracks requests and tunnels separately.
	s := Stats{ActiveRequests: 2, ActiveTunnels: 3}

	if got := s.Active(); got != 5 {
		t.Errorf("Active() = %d, want 5", got)
	}
}

func TestPlainForwardCountsAndReleasesTheActiveSlot(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "ok")
	}))
	defer origin.Close()

	server := New(&capSink{}, nil)
	front := httptest.NewServer(server)
	defer front.Close()

	client := proxiedClient(t, front)
	resp, err := client.Get(origin.URL + "/")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	got := server.Stats()
	if got.TotalRequests != 1 {
		t.Errorf("TotalRequests = %d, want 1", got.TotalRequests)
	}
	// The decrement is deferred, so a finished request must leave nothing in flight.
	// A count that only grows is worse than no count, because it looks like data.
	if got.ActiveRequests != 0 {
		t.Errorf("ActiveRequests = %d after the request finished, want 0", got.ActiveRequests)
	}
}

func TestActiveRequestsIsNonZeroWhileInFlight(t *testing.T) {
	// Proves the counter measures concurrency rather than just counting totals.
	release := make(chan struct{})
	observed := make(chan Stats, 1)

	var server *Server
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Sampled from inside the upstream handler, i.e. while the proxy is mid-relay.
		observed <- server.Stats()
		<-release
		io.WriteString(w, "ok")
	}))
	defer origin.Close()

	server = New(&capSink{}, nil)
	front := httptest.NewServer(server)
	defer front.Close()

	client := proxiedClient(t, front)
	done := make(chan struct{})
	go func() {
		defer close(done)
		resp, err := client.Get(origin.URL + "/")
		if err != nil {
			return
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}()

	inFlight := <-observed
	if inFlight.ActiveRequests != 1 {
		t.Errorf("ActiveRequests mid-relay = %d, want 1", inFlight.ActiveRequests)
	}
	if inFlight.Active() != 1 {
		t.Errorf("Active() mid-relay = %d, want 1", inFlight.Active())
	}

	close(release)
	<-done

	if after := server.Stats(); after.ActiveRequests != 0 {
		t.Errorf("ActiveRequests after completion = %d, want 0", after.ActiveRequests)
	}
}

func TestMalformedRequestIsNotCountedAsAConnection(t *testing.T) {
	// An origin-form request means the client is not using us as a proxy at all. It
	// never reaches upstream, so reporting it as a connection in flight would be a
	// lie about traffic.
	server := New(&capSink{}, nil)
	front := httptest.NewServer(server)
	defer front.Close()

	resp, err := http.Get(front.URL + "/not-a-proxy-request")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}

	if got := server.Stats(); got.TotalRequests != 0 {
		t.Errorf("TotalRequests = %d for a non-proxy request, want 0", got.TotalRequests)
	}
}

func TestTunnelledConnectIsCountedAsTunnelled(t *testing.T) {
	// An excluded host is tunnelled rather than intercepted, so the split must land
	// on the tunnelled side even though a CA is present.
	origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "ok")
	}))
	defer origin.Close()

	sink := &capSink{}
	server := New(sink, testAuthority(t), insecureUpstream(), WithExcludedHosts("127.0.0.1"))
	front := httptest.NewServer(server)
	defer front.Close()

	client := httpsProxyClient(t, front, originPool(origin))
	resp, err := client.Get(origin.URL + "/")
	if err != nil {
		t.Fatalf("GET through tunnel: %v", err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	client.CloseIdleConnections()

	// A tunnelled flow is emitted only when the connection closes, so waiting on the
	// flow is also waiting for the counter's deferred decrement.
	waitForFlows(t, sink, 1)
	got := awaitStats(t, server, func(s Stats) bool { return s.ActiveTunnels == 0 })

	if got.TotalTunnels != 1 {
		t.Errorf("TotalTunnels = %d, want 1", got.TotalTunnels)
	}
	if got.TunnelledFlows != 1 {
		t.Errorf("TunnelledFlows = %d, want 1", got.TunnelledFlows)
	}
	if got.InterceptedFlows != 0 {
		t.Errorf("InterceptedFlows = %d for an excluded host, want 0", got.InterceptedFlows)
	}
}

func TestInterceptedConnectIsCountedAsIntercepted(t *testing.T) {
	// The split records the interception *attempt*, not its outcome: a host that
	// turns out to be pinned is learned and tunnels on its next connection, which is
	// when the count becomes accurate anyway.
	origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "ok")
	}))
	defer origin.Close()

	authority := testAuthority(t)
	server := New(&capSink{}, authority, insecureUpstream())
	front := httptest.NewServer(server)
	defer front.Close()

	client := httpsProxyClient(t, front, caPool(authority))
	resp, err := client.Get(origin.URL + "/")
	if err != nil {
		t.Fatalf("intercepted GET: %v", err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	got := awaitStats(t, server, func(s Stats) bool { return s.TotalTunnels == 1 })

	if got.InterceptedFlows != 1 {
		t.Errorf("InterceptedFlows = %d, want 1", got.InterceptedFlows)
	}
	if got.TunnelledFlows != 0 {
		t.Errorf("TunnelledFlows = %d, want 0", got.TunnelledFlows)
	}
	if got.TotalRequests != 0 {
		t.Errorf("TotalRequests = %d; requests inside a tunnel are not plain forwards", got.TotalRequests)
	}
}

func TestSetAuthoritySwapsTheCAForSubsequentConnections(t *testing.T) {
	// What POST /reload relies on: a live server picking up a reloaded CA without a
	// restart.
	first := testAuthority(t)
	second := testAuthority(t)
	if first.Fingerprint() == second.Fingerprint() {
		t.Fatal("two CAs in separate directories should not share a fingerprint")
	}

	server := New(&capSink{}, first)
	if server.Authority() != first {
		t.Fatal("Authority() did not return the CA passed to New")
	}

	server.SetAuthority(second)

	if server.Authority() != second {
		t.Error("Authority() did not return the swapped-in CA")
	}
	if server.Authority().Fingerprint() != second.Fingerprint() {
		t.Error("the swapped-in CA's fingerprint is not in force")
	}
}

func TestNilAuthorityMeansTunnelEverything(t *testing.T) {
	server := New(&capSink{}, nil)

	if server.Authority() != nil {
		t.Error("Authority() should be nil when New was given nil")
	}
	if !server.shouldTunnel("example.com:443") {
		t.Error("a server with no CA must tunnel every host")
	}
}

func TestCountersAreRaceCleanUnderConcurrentLoad(t *testing.T) {
	// Run under -race. A host app polls Stats() on a timer while traffic flows, so
	// reads and writes genuinely overlap on the request path.
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "ok")
	}))
	defer origin.Close()

	server := New(&capSink{}, nil)
	front := httptest.NewServer(server)
	defer front.Close()

	client := proxiedClient(t, front)

	stop := make(chan struct{})
	pollerDone := make(chan struct{})
	go func() {
		defer close(pollerDone)
		for {
			select {
			case <-stop:
				return
			default:
				_ = server.Stats()
			}
		}
	}()

	const workers, each = 6, 10
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range each {
				resp, err := client.Get(origin.URL + "/")
				if err != nil {
					continue
				}
				io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
			}
		}()
	}
	wg.Wait()
	close(stop)
	<-pollerDone

	got := server.Stats()
	if got.TotalRequests != workers*each {
		t.Errorf("TotalRequests = %d, want %d", got.TotalRequests, workers*each)
	}
	if got.ActiveRequests != 0 {
		t.Errorf("ActiveRequests = %d once everything finished, want 0", got.ActiveRequests)
	}
}

// awaitStats polls until the snapshot satisfies want, because a tunnel's teardown
// (and so its deferred decrement) is concurrent with the client's close.
func awaitStats(t *testing.T, server *Server, want func(Stats) bool) Stats {
	t.Helper()
	var got Stats
	for range 200 {
		got = server.Stats()
		if want(got) {
			return got
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for stats; last snapshot %+v", got)
	return got
}
