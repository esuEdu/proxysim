package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// dial builds a client that reaches the control socket. A custom DialContext on an
// otherwise ordinary http.Client, so the tests exercise the same request shape any
// real caller sends — including `curl --unix-socket`.
func dial(path string) *http.Client {
	return &http.Client{
		Timeout: 5 * time.Second,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "unix", path)
			},
		},
	}
}

// socketPath returns a short, unique socket path.
//
// Deliberately not under t.TempDir(): macOS caps sockaddr_un.sun_path at 104 bytes,
// and Go builds that directory name from the test's own name, so any test with a
// descriptive name overflows it — surfacing as `bind: invalid argument`, which says
// nothing about path length. A short /tmp path with an explicit cleanup sidesteps
// the whole trap.
func socketPath(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "psctl")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return filepath.Join(dir, "c.sock")
}

// serve starts a control server and returns a client for it.
func serve(t *testing.T, handlers Handlers) (*Server, *http.Client, string) {
	t.Helper()
	path := socketPath(t)
	srv, err := Listen(path, handlers)
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	go srv.Serve()
	t.Cleanup(func() { srv.Close() })
	return srv, dial(path), path
}

func fullHandlers(shutdown func()) Handlers {
	return Handlers{
		Status: func() Status {
			return Status{
				Port:              8888,
				UptimeSeconds:     12.5,
				ActiveConnections: 3,
				TotalFlows:        42,
				Scope:             "iOS Simulator",
			}
		},
		Stats: func() any {
			return map[string]int{"activeRequests": 1, "activeTunnels": 2}
		},
		Reload: func(ctx context.Context) (ReloadResult, error) {
			return ReloadResult{Reloaded: true, Fingerprint: "AB:CD"}, nil
		},
		Shutdown: shutdown,
	}
}

func TestStatusReportsTheInjectedFigures(t *testing.T) {
	_, client, _ := serve(t, fullHandlers(func() {}))

	resp, err := client.Get("http://localhost/status")
	if err != nil {
		t.Fatalf("GET /status: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}

	var got Status
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	want := Status{Port: 8888, UptimeSeconds: 12.5, ActiveConnections: 3, TotalFlows: 42, Scope: "iOS Simulator"}
	if got != want {
		t.Errorf("status = %+v, want %+v", got, want)
	}
}

func TestStatusFieldNamesAreTheStableContract(t *testing.T) {
	// A client in another language cannot be recompiled alongside this one, so a
	// renamed field breaks it silently. These names are pinned deliberately.
	_, client, _ := serve(t, fullHandlers(func() {}))

	resp, err := client.Get("http://localhost/status")
	if err != nil {
		t.Fatalf("GET /status: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	for _, key := range []string{"port", "uptimeSeconds", "activeConnections", "totalFlows", "scope"} {
		if _, ok := raw[key]; !ok {
			t.Errorf("missing %q in /status; got %s", key, body)
		}
	}
}

func TestStatusIsReadPerRequestNotSampledOnce(t *testing.T) {
	// A host app polling this needs live figures; a value captured at Listen would
	// show a frozen connection count forever.
	calls := 0
	_, client, _ := serve(t, Handlers{
		Status: func() Status {
			calls++
			return Status{ActiveConnections: int64(calls)}
		},
	})

	for want := 1; want <= 3; want++ {
		resp, err := client.Get("http://localhost/status")
		if err != nil {
			t.Fatalf("GET /status: %v", err)
		}
		var got Status
		json.NewDecoder(resp.Body).Decode(&got)
		resp.Body.Close()
		if got.ActiveConnections != int64(want) {
			t.Errorf("call %d reported %d", want, got.ActiveConnections)
		}
	}
}

func TestStats(t *testing.T) {
	_, client, _ := serve(t, fullHandlers(func() {}))

	resp, err := client.Get("http://localhost/stats")
	if err != nil {
		t.Fatalf("GET /stats: %v", err)
	}
	defer resp.Body.Close()

	var got map[string]int
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if got["activeRequests"] != 1 || got["activeTunnels"] != 2 {
		t.Errorf("stats = %v", got)
	}
}

func TestReloadReturnsTheFingerprint(t *testing.T) {
	_, client, _ := serve(t, fullHandlers(func() {}))

	resp, err := client.Post("http://localhost/reload", "", nil)
	if err != nil {
		t.Fatalf("POST /reload: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var got ReloadResult
	json.NewDecoder(resp.Body).Decode(&got)
	if !got.Reloaded || got.Fingerprint != "AB:CD" {
		t.Errorf("reload = %+v", got)
	}
}

func TestReloadWithATrustFailureIsStillOK(t *testing.T) {
	// CA trust is best-effort by design (spec 010): no booted simulator is not a
	// failure, because the proxy remains useful for curl. A 500 here would tell the
	// caller nothing worked, when the CA reloaded fine.
	_, client, _ := serve(t, Handlers{
		Reload: func(ctx context.Context) (ReloadResult, error) {
			return ReloadResult{Reloaded: true, Fingerprint: "AB:CD", TrustError: "no booted device"}, nil
		},
	})

	resp, err := client.Post("http://localhost/reload", "", nil)
	if err != nil {
		t.Fatalf("POST /reload: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 for a trust-only failure", resp.StatusCode)
	}
	var got ReloadResult
	json.NewDecoder(resp.Body).Decode(&got)
	if got.TrustError != "no booted device" || !got.Reloaded {
		t.Errorf("reload = %+v", got)
	}
}

func TestReloadWithAnUnreadableCAIsAnError(t *testing.T) {
	// This one genuinely did not work, so it is a 500 rather than a field.
	_, client, _ := serve(t, Handlers{
		Reload: func(ctx context.Context) (ReloadResult, error) {
			return ReloadResult{}, errors.New("open ca.crt: permission denied")
		},
	})

	resp, err := client.Post("http://localhost/reload", "", nil)
	if err != nil {
		t.Fatalf("POST /reload: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "permission denied") {
		t.Errorf("body should carry the reason, got %q", body)
	}
}

func TestReloadIsGivenABoundedContext(t *testing.T) {
	// It shells out to simctl, which can hang on a wedged simulator. A control
	// request must not wait on that forever.
	_, client, _ := serve(t, Handlers{
		Reload: func(ctx context.Context) (ReloadResult, error) {
			if _, ok := ctx.Deadline(); !ok {
				return ReloadResult{}, errors.New("no deadline on the reload context")
			}
			return ReloadResult{Reloaded: true}, nil
		},
	})

	resp, err := client.Post("http://localhost/reload", "", nil)
	if err != nil {
		t.Fatalf("POST /reload: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d: %s", resp.StatusCode, body)
	}
}

func TestShutdownAnswersBeforeShuttingDown(t *testing.T) {
	// The response is served by the very server being asked to stop, so tearing down
	// first would drop it and leave the caller unable to tell success from a broken
	// socket.
	called := make(chan struct{})
	_, client, _ := serve(t, fullHandlers(func() { close(called) }))

	resp, err := client.Post("http://localhost/shutdown", "", nil)
	if err != nil {
		t.Fatalf("POST /shutdown: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "shuttingDown") {
		t.Errorf("body = %q", body)
	}

	select {
	case <-called:
	case <-time.After(2 * time.Second):
		t.Fatal("Shutdown was never invoked")
	}
}

func TestWrongMethodIs405WithAllow(t *testing.T) {
	// A caller POSTing to /status has a bug. A 404 would send them looking for a
	// missing route instead of a wrong verb.
	_, client, _ := serve(t, fullHandlers(func() {}))

	cases := []struct {
		method, path, allow string
	}{
		{http.MethodPost, "/status", http.MethodGet},
		{http.MethodPost, "/stats", http.MethodGet},
		{http.MethodGet, "/reload", http.MethodPost},
		{http.MethodGet, "/shutdown", http.MethodPost},
		{http.MethodDelete, "/status", http.MethodGet},
	}
	for _, tc := range cases {
		req, _ := http.NewRequest(tc.method, "http://localhost"+tc.path, nil)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", tc.method, tc.path, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusMethodNotAllowed {
			t.Errorf("%s %s = %d, want 405", tc.method, tc.path, resp.StatusCode)
		}
		if got := resp.Header.Get("Allow"); got != tc.allow {
			t.Errorf("%s %s Allow = %q, want %q", tc.method, tc.path, got, tc.allow)
		}
	}
}

func TestUnknownPathIs404ThatNamesTheRealRoutes(t *testing.T) {
	_, client, _ := serve(t, fullHandlers(func() {}))

	resp, err := client.Get("http://localhost/nope")
	if err != nil {
		t.Fatalf("GET /nope: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	for _, route := range []string{"/status", "/stats", "/reload", "/shutdown"} {
		if !strings.Contains(string(body), route) {
			t.Errorf("404 body should list %s; got %q", route, body)
		}
	}
}

func TestUnwiredHandlerIs501NotAPanic(t *testing.T) {
	// A partially wired server is a programming error, but it must not take down a
	// proxy that is otherwise serving traffic fine.
	_, client, _ := serve(t, Handlers{})

	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/status"},
		{http.MethodGet, "/stats"},
		{http.MethodPost, "/reload"},
		{http.MethodPost, "/shutdown"},
	} {
		req, _ := http.NewRequest(tc.method, "http://localhost"+tc.path, nil)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", tc.method, tc.path, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotImplemented {
			t.Errorf("%s %s = %d, want 501", tc.method, tc.path, resp.StatusCode)
		}
	}
}

func TestSocketIsCreatedWith0600(t *testing.T) {
	// This API can stop the process, so it does not rely on the containing
	// directory alone for access control. The mode at bind time is umask-dependent,
	// which is why it is chmod'ed explicitly.
	_, _, path := serve(t, fullHandlers(func() {}))

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Mode()&os.ModeSocket == 0 {
		t.Fatalf("%s is not a socket (mode %v)", path, info.Mode())
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("socket mode = %o, want 600", perm)
	}
}

func TestStaleSocketDoesNotPreventBinding(t *testing.T) {
	// A crashed instance leaves the file behind, and bind then fails with EADDRINUSE
	// even though nobody is listening.
	path := socketPath(t)

	first, err := Listen(path, fullHandlers(func() {}))
	if err != nil {
		t.Fatalf("first Listen: %v", err)
	}
	// Leave the socket file behind on close, which is what a SIGKILL produces. Go's
	// UnixListener unlinks on Close by default, so simulating a crash means opting
	// out of that first.
	if unixLn, ok := first.listener.(*net.UnixListener); ok {
		unixLn.SetUnlinkOnClose(false)
	} else {
		t.Fatalf("expected a *net.UnixListener, got %T", first.listener)
	}
	first.listener.Close()
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("expected the socket file to remain: %v", err)
	}

	second, err := Listen(path, fullHandlers(func() {}))
	if err != nil {
		t.Fatalf("second Listen over a stale socket: %v", err)
	}
	second.Close()
}

func TestListenRefusesToDeleteANonSocket(t *testing.T) {
	// If a regular file is at this path the caller has misconfigured something, and
	// silently deleting their file is not a repair.
	path := socketPath(t)
	if err := os.WriteFile(path, []byte("important"), 0o600); err != nil {
		t.Fatalf("writing: %v", err)
	}

	if _, err := Listen(path, fullHandlers(func() {})); err == nil {
		t.Fatal("Listen overwrote a regular file")
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("the existing file was removed anyway: %v", err)
	}
}

func TestListenRejectsAnEmptyPath(t *testing.T) {
	if _, err := Listen("", fullHandlers(func() {})); err == nil {
		t.Fatal("Listen(\"\") should fail rather than binding somewhere surprising")
	}
}

func TestCloseRemovesTheSocketFile(t *testing.T) {
	// So the next instance starts clean rather than relying on stale-socket recovery.
	srv, _, path := serve(t, fullHandlers(func() {}))

	if err := srv.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("socket file still present after Close: %v", err)
	}
}

func TestServeReturnsNilAfterClose(t *testing.T) {
	// http.ErrServerClosed is the expected outcome of Close, not an error to report
	// as a serving failure.
	path := socketPath(t)
	srv, err := Listen(path, fullHandlers(func() {}))
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}

	errCh := make(chan error, 1)
	go func() { errCh <- srv.Serve() }()
	time.Sleep(20 * time.Millisecond)
	srv.Close()

	select {
	case err := <-errCh:
		if err != nil {
			t.Errorf("Serve returned %v, want nil after Close", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Serve did not return after Close")
	}
}

func TestPathIsReported(t *testing.T) {
	srv, _, path := serve(t, fullHandlers(func() {}))

	if srv.Path() != path {
		t.Errorf("Path() = %q, want %q", srv.Path(), path)
	}
}

func TestConcurrentRequestsAreRaceClean(t *testing.T) {
	// Run under -race: a host app polls /status on a timer while the user may be
	// clicking reload.
	_, client, _ := serve(t, fullHandlers(func() {}))

	done := make(chan struct{})
	for i := 0; i < 8; i++ {
		go func(n int) {
			defer func() { done <- struct{}{} }()
			for j := 0; j < 10; j++ {
				path := "/status"
				if n%2 == 0 {
					path = "/stats"
				}
				resp, err := client.Get("http://localhost" + path)
				if err != nil {
					t.Errorf("GET %s: %v", path, err)
					return
				}
				io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
			}
		}(i)
	}
	for i := 0; i < 8; i++ {
		<-done
	}
}

// TestCurlCompatibility documents the acceptance criterion that no bespoke client
// is needed: `curl --unix-socket <path> http://localhost/status` must work. It is
// asserted through a plain http.Client because that sends the same request shape,
// and shelling out to curl in a unit test buys nothing.
func TestCurlCompatibility(t *testing.T) {
	_, client, _ := serve(t, fullHandlers(func() {}))

	req, _ := http.NewRequest(http.MethodGet, "http://localhost/status", nil)
	req.Header.Set("User-Agent", "curl/8.4.0")
	req.Header.Set("Accept", "*/*")

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("curl-shaped GET /status: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
}

// stubFlowHandler stands in for ui.Hub's flow routes, so the control server's
// delegation is tested without importing internal/ui — the decoupling this package
// is built around would be pointless if its tests broke it.
func stubFlowHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /flows", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `[{"id":2},{"id":1}]`)
	})
	mux.HandleFunc("GET /flows/{id}", func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("id") == "99" {
			http.Error(w, "flow 99 not found", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"id":%s,"request_body":"decoded"}`, r.PathValue("id"))
	})
	mux.HandleFunc("GET /events", func(w http.ResponseWriter, r *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "streaming unsupported", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher.Flush()
		for i := 1; i <= 3; i++ {
			fmt.Fprintf(w, "data: {\"id\":%d}\n\n", i)
			flusher.Flush()
		}
		<-r.Context().Done()
	})
	return mux
}

func flowHandlers() Handlers {
	h := fullHandlers(func() {})
	h.Flows = stubFlowHandler()
	return h
}

func TestFlowsAreDelegatedToTheInjectedHandler(t *testing.T) {
	_, client, _ := serve(t, flowHandlers())

	resp, err := client.Get("http://localhost/flows")
	if err != nil {
		t.Fatalf("GET /flows: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), `"id":2`) {
		t.Errorf("body = %q", body)
	}
}

func TestFlowDetailPathIsForwardedIntact(t *testing.T) {
	// Registered on the "/flows/" prefix so the flow handler owns its own routing;
	// a rewritten or stripped path would break its {id} pattern.
	_, client, _ := serve(t, flowHandlers())

	resp, err := client.Get("http://localhost/flows/7")
	if err != nil {
		t.Fatalf("GET /flows/7: %v", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), `"id":7`) {
		t.Errorf("the id did not survive delegation: %q", body)
	}
}

func TestUnknownFlowIDStillReports404ThroughTheControlSocket(t *testing.T) {
	_, client, _ := serve(t, flowHandlers())

	resp, err := client.Get("http://localhost/flows/99")
	if err != nil {
		t.Fatalf("GET /flows/99: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

func TestEventsStreamsIncrementally(t *testing.T) {
	// The point of SSE: frames arrive before the response ends. Reading to EOF would
	// hang here, which is exactly the constraint the client has to respect.
	_, client, _ := serve(t, flowHandlers())

	req, _ := http.NewRequest(http.MethodGet, "http://localhost/events", nil)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	resp, err := client.Do(req.WithContext(ctx))
	if err != nil {
		t.Fatalf("GET /events: %v", err)
	}
	defer resp.Body.Close()

	if got := resp.Header.Get("Content-Type"); got != "text/event-stream" {
		t.Errorf("Content-Type = %q, want text/event-stream", got)
	}

	// Read exactly the three frames the stub sends, then stop. The response is never
	// finished, so this must not wait for one.
	buffer := make([]byte, 0, 256)
	chunk := make([]byte, 64)
	deadline := time.Now().Add(4 * time.Second)
	for strings.Count(string(buffer), "\n\n") < 3 && time.Now().Before(deadline) {
		n, err := resp.Body.Read(chunk)
		if n > 0 {
			buffer = append(buffer, chunk[:n]...)
		}
		if err != nil {
			break
		}
	}

	for i := 1; i <= 3; i++ {
		want := fmt.Sprintf(`data: {"id":%d}`, i)
		if !strings.Contains(string(buffer), want) {
			t.Errorf("missing frame %d in stream: %q", i, buffer)
		}
	}
}

func TestEventsIsNotCutOffByTheHeaderTimeout(t *testing.T) {
	// ReadHeaderTimeout bounds only the request headers. If a write deadline were ever
	// added, SSE would die mid-stream — this pins that it has not been.
	_, client, _ := serve(t, flowHandlers())

	req, _ := http.NewRequest(http.MethodGet, "http://localhost/events", nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	resp, err := client.Do(req.WithContext(ctx))
	if err != nil {
		t.Fatalf("GET /events: %v", err)
	}
	defer resp.Body.Close()

	// Well past the 5s ReadHeaderTimeout, the stream must still be open.
	time.Sleep(100 * time.Millisecond)
	chunk := make([]byte, 64)
	if _, err := resp.Body.Read(chunk); err != nil && err != io.EOF {
		t.Errorf("stream broke early: %v", err)
	}
}

func TestFlowRoutesReport501WhenNoFlowStoreIsWired(t *testing.T) {
	// -control-socket without any flow consumer is a valid configuration, and a host
	// app polling /flows deserves "not available here" rather than a 404 suggesting a
	// typo.
	_, client, _ := serve(t, fullHandlers(func() {}))

	for _, path := range []string{"/flows", "/flows/1", "/events"} {
		resp, err := client.Get("http://localhost" + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotImplemented {
			t.Errorf("GET %s = %d, want 501", path, resp.StatusCode)
		}
	}
}

func TestNotFoundBodyListsTheFlowRoutes(t *testing.T) {
	_, client, _ := serve(t, flowHandlers())

	resp, err := client.Get("http://localhost/nope")
	if err != nil {
		t.Fatalf("GET /nope: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	for _, route := range []string{"/flows", "/events"} {
		if !strings.Contains(string(body), route) {
			t.Errorf("404 body should list %s; got %q", route, body)
		}
	}
}
