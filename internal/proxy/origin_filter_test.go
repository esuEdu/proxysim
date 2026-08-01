package proxy

import (
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"proxysim/internal/origin"
)

// fixedResolver returns the same process (or error) for every connection, so a
// test can drive the intercept decision without real processes or cgo.
func fixedResolver(p origin.Process, err error) origin.Resolver {
	return func(_, _ net.Addr) (origin.Process, error) { return p, err }
}

var (
	simAppA  = origin.Process{PID: 1, Simulator: true, BundleID: "com.example.A"}
	simAppB  = origin.Process{PID: 2, Simulator: true, BundleID: "com.example.B"}
	hostProc = origin.Process{PID: 3, Path: "/usr/bin/curl"}
)

func filter(onlySim bool, apps ...string) origin.Filter {
	return origin.BuildFilter(onlySim, apps)
}

// Criterion 2 (match) & 7: a connection whose origin is a simulator process is
// intercepted and captured under -only-sim. The decision is the origin's alone —
// the same host is tunnelled in TestOriginFilterTunnelsHostSilently below.
func TestOriginFilterInterceptsSimulator(t *testing.T) {
	up := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "hi")
	}))
	defer up.Close()

	a := testAuthority(t)
	sink := &capSink{}
	front := httptest.NewServer(New(sink, a,
		insecureUpstream(),
		WithOriginFilter(fixedResolver(simAppA, nil), filter(true)),
	))
	defer front.Close()

	resp, err := httpsProxyClient(t, front, caPool(a)).Get(up.URL + "/x")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	waitForFlows(t, sink, 1)
	if f := sink.all()[0]; !f.Intercepted {
		t.Errorf("simulator flow should be intercepted: %+v", f)
	}
}

// Criterion 2 (no match) & 6: a host-process connection is tunnelled silently —
// it still works, but no flow is surfaced.
func TestOriginFilterTunnelsHostSilently(t *testing.T) {
	up := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "direct")
	}))
	defer up.Close()

	a := testAuthority(t)
	sink := &capSink{}
	front := httptest.NewServer(New(sink, a,
		WithOriginFilter(fixedResolver(hostProc, nil), filter(true)),
	))
	defer front.Close()

	// Trust only the origin's own cert: success proves a blind tunnel (our CA was
	// never presented).
	resp, err := httpsProxyClient(t, front, originPool(up)).Get(up.URL + "/")
	if err != nil {
		t.Fatalf("host traffic must still flow through a tunnel: %v", err)
	}
	resp.Body.Close()

	if n := len(sink.all()); n != 0 {
		t.Errorf("filtered-out connection produced %d flows, want 0 (must be silent)", n)
	}
}

// Criterion 3: -app intercepts the named app and tunnels every other app.
func TestOriginFilterAppAllowAndDeny(t *testing.T) {
	up := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "hi")
	}))
	defer up.Close()

	a := testAuthority(t)

	// App A is named → intercepted.
	sinkA := &capSink{}
	frontA := httptest.NewServer(New(sinkA, a,
		insecureUpstream(),
		WithOriginFilter(fixedResolver(simAppA, nil), filter(false, "com.example.A")),
	))
	defer frontA.Close()
	respA, err := httpsProxyClient(t, frontA, caPool(a)).Get(up.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	respA.Body.Close()
	waitForFlows(t, sinkA, 1)
	if !sinkA.all()[0].Intercepted {
		t.Error("named app A should be intercepted")
	}

	// App B is a different simulator app → tunnelled silently.
	sinkB := &capSink{}
	frontB := httptest.NewServer(New(sinkB, a,
		WithOriginFilter(fixedResolver(simAppB, nil), filter(false, "com.example.A")),
	))
	defer frontB.Close()
	respB, err := httpsProxyClient(t, frontB, originPool(up)).Get(up.URL + "/")
	if err != nil {
		t.Fatalf("unnamed app B must still flow through a tunnel: %v", err)
	}
	respB.Body.Close()
	if n := len(sinkB.all()); n != 0 {
		t.Errorf("app B produced %d flows, want 0", n)
	}
}

// Spec 011, criterion 1: the filter is read per connection through a provider, so
// flipping it on at runtime takes effect on the next connection. First request
// under an inactive filter is intercepted; after the controller is set to
// only-sim, a host-process connection is tunnelled silently.
func TestOriginFilterFuncMutableAtRuntime(t *testing.T) {
	up := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "hi")
	}))
	defer up.Close()

	a := testAuthority(t)
	sink := &capSink{}
	ctrl := origin.NewController(origin.Filter{}) // inactive: intercept everything
	front := httptest.NewServer(New(sink, a,
		insecureUpstream(),
		WithOriginFilterFunc(fixedResolver(hostProc, nil), ctrl.Current),
	))
	defer front.Close()

	// Filter off: the host-process connection is intercepted like any other.
	resp, err := httpsProxyClient(t, front, caPool(a)).Get(up.URL + "/x")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	waitForFlows(t, sink, 1)
	if !sink.all()[0].Intercepted {
		t.Fatal("with the filter inactive, the connection should be intercepted")
	}

	// Flip to only-sim at runtime: the same host process no longer matches, so the
	// next connection is blind-tunnelled and produces no further flow.
	ctrl.Set(origin.BuildFilter(true, nil))
	resp2, err := httpsProxyClient(t, front, originPool(up)).Get(up.URL + "/y")
	if err != nil {
		t.Fatalf("host traffic must still flow through a tunnel after the switch: %v", err)
	}
	resp2.Body.Close()
	if n := len(sink.all()); n != 1 {
		t.Errorf("after switching to only-sim, host connection produced %d total flows, want 1 (the first)", n)
	}
}

// Spec 011, criterion 10: an inactive filter costs nothing — the resolver is not
// even called, preserving spec 009's fast path.
func TestOriginFilterFuncInactiveSkipsResolver(t *testing.T) {
	up := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "hi")
	}))
	defer up.Close()

	var resolved atomic.Bool
	spyResolver := func(_, _ net.Addr) (origin.Process, error) {
		resolved.Store(true)
		return hostProc, nil
	}

	a := testAuthority(t)
	sink := &capSink{}
	ctrl := origin.NewController(origin.Filter{}) // inactive
	front := httptest.NewServer(New(sink, a,
		insecureUpstream(),
		WithOriginFilterFunc(spyResolver, ctrl.Current),
	))
	defer front.Close()

	resp, err := httpsProxyClient(t, front, caPool(a)).Get(up.URL + "/x")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	waitForFlows(t, sink, 1)
	if resolved.Load() {
		t.Error("resolver must not be called while the filter is inactive")
	}
}

// Criterion 4: an origin that cannot be resolved is tunnelled, not failed and not
// captured — never a panic.
func TestOriginFilterUnresolvedTunnels(t *testing.T) {
	up := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "direct")
	}))
	defer up.Close()

	a := testAuthority(t)
	sink := &capSink{}
	front := httptest.NewServer(New(sink, a,
		WithOriginFilter(fixedResolver(origin.Process{}, fmt.Errorf("gone")), filter(true)),
	))
	defer front.Close()

	resp, err := httpsProxyClient(t, front, originPool(up)).Get(up.URL + "/")
	if err != nil {
		t.Fatalf("unresolved origin must be tunnelled, not failed: %v", err)
	}
	resp.Body.Close()
	if n := len(sink.all()); n != 0 {
		t.Errorf("unresolved origin produced %d flows, want 0", n)
	}
}
