// Package control serves proxysim's control API on a Unix domain socket, so a host
// application (a menu bar app, a script) can observe and steer a running instance
// without a terminal and without the web UI.
//
// A Unix socket rather than a loopback port because this API can shut the process
// down and re-trust a CA: a TCP port is reachable by every process on the machine
// with no way to distinguish them, while a socket file is restricted by the
// filesystem — the same reasoning that governs ca.key (CLAUDE.md). HTTP over it
// rather than the bespoke line-JSON of spec 012's Xcode socket because there are
// four operations, two verbs, and status codes needed to say "no", all of which
// net/http provides for free on any net.Listener.
//
// This package deliberately imports nothing from internal/proxy, internal/ca or
// internal/sim. Everything it reports or triggers arrives as an injected function,
// so it is testable with no proxy, no CA and no simulator — and so the dependency
// points one way, exactly as internal/flow does. See specs/013-control-api.md.
package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"time"
)

// Status is the /status response: the small, stable contract a host app's UI binds
// to. Fields may be added but never renamed — a client in another language cannot
// be recompiled alongside this one.
type Status struct {
	Port              int     `json:"port"`
	UptimeSeconds     float64 `json:"uptimeSeconds"`
	ActiveConnections int64   `json:"activeConnections"`
	TotalFlows        int64   `json:"totalFlows"`

	// Scope renders the origin filter currently in force, so a host app can show
	// what proxysim is actually doing rather than what it was asked to do. Read
	// live, so a runtime change through the spec-011 UI is reflected here.
	Scope string `json:"scope"`
}

// ReloadResult is the /reload response.
//
// TrustError is a field rather than an HTTP error because CA trust installation is
// best-effort by design (spec 010): no booted simulator is not a failure, since the
// proxy remains perfectly useful for curl. Returning 500 for it would tell the
// caller nothing worked, when in fact the CA reloaded fine.
type ReloadResult struct {
	Reloaded    bool   `json:"reloaded"`
	Fingerprint string `json:"fingerprint,omitempty"`
	TrustError  string `json:"trustError,omitempty"`
}

// Handlers is everything the control API needs from the rest of the program,
// injected so this package stays free of proxysim's internals.
//
// A nil field means the operation is unavailable and is answered 501 rather than
// panicking: a partially-wired server is a programming error, but one that must not
// take down a serving proxy.
type Handlers struct {
	// Status is read per request, so figures are live rather than sampled at start.
	Status func() Status

	// Stats returns the unstable diagnostic dump. Any JSON-marshalable value;
	// internal/proxy.Stats is what production passes.
	Stats func() any

	// Reload re-reads the CA and re-installs simulator trust. An error means the CA
	// itself could not be read — that one genuinely did not work.
	Reload func(ctx context.Context) (ReloadResult, error)

	// Shutdown triggers the same graceful path as SIGINT. It must return promptly:
	// it is called *before* the response is written, and the actual teardown belongs
	// on another goroutine.
	Shutdown func()
}

// Server is the control API listening on a Unix socket.
type Server struct {
	listener net.Listener
	http     *http.Server
	path     string
}

// socketMode is the socket file's permission. 0600 because this API can stop the
// process: the directory it sits in may be shared, so the socket does not rely on
// the directory alone for access control.
const socketMode = 0o600

// Listen binds the control socket at path.
//
// A stale socket file left by a crashed instance makes bind fail with EADDRINUSE
// even though nothing is listening, so it is cleared first — the same treatment
// spec 012's socket already gets, and for the same reason.
func Listen(path string, handlers Handlers) (*Server, error) {
	if path == "" {
		return nil, errors.New("control: empty socket path")
	}

	// Only remove something that is actually a socket. If a regular file is at this
	// path the caller has misconfigured something, and silently deleting their file
	// is not a repair.
	if info, err := os.Stat(path); err == nil {
		if info.Mode()&os.ModeSocket == 0 {
			return nil, fmt.Errorf("control: %s exists and is not a socket", path)
		}
		if err := os.Remove(path); err != nil {
			return nil, fmt.Errorf("control: clearing stale socket %s: %w", path, err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("control: inspecting %s: %w", path, err)
	}

	listener, err := net.Listen("unix", path)
	if err != nil {
		return nil, fmt.Errorf("control: listening on %s: %w", path, err)
	}

	// Chmod after bind: the mode at creation is umask-dependent, so relying on the
	// umask would leave the permission up to the user's shell configuration.
	if err := os.Chmod(path, socketMode); err != nil {
		listener.Close()
		os.Remove(path)
		return nil, fmt.Errorf("control: securing %s: %w", path, err)
	}

	server := &Server{
		listener: listener,
		path:     path,
	}
	server.http = &http.Server{
		Handler: server.handler(handlers),
		// A control client is local and answers instantly; a stuck one must not hold
		// a goroutine indefinitely.
		ReadHeaderTimeout: 5 * time.Second,
	}
	return server, nil
}

// Path is the bound socket path, for the startup banner.
func (s *Server) Path() string { return s.path }

// Serve accepts control requests until Close. It blocks, so callers run it on a
// goroutine. http.ErrServerClosed is the expected outcome of Close and is reported
// as nil.
func (s *Server) Serve() error {
	err := s.http.Serve(s.listener)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// Close stops serving and removes the socket file so the next instance starts
// clean.
func (s *Server) Close() error {
	err := s.http.Close()
	if rmErr := os.Remove(s.path); rmErr != nil && !errors.Is(rmErr, os.ErrNotExist) && err == nil {
		err = rmErr
	}
	return err
}

// handler builds the mux.
//
// Routed explicitly rather than with http.ServeMux's pattern matching so that a
// wrong method gets a 405 with an Allow header instead of a 404. A caller POSTing
// to /status has a bug, and a 404 would send them looking for a missing route.
func (s *Server) handler(h Handlers) http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("/status", func(w http.ResponseWriter, r *http.Request) {
		if !requireMethod(w, r, http.MethodGet) {
			return
		}
		if h.Status == nil {
			unavailable(w, "status")
			return
		}
		writeJSON(w, http.StatusOK, h.Status())
	})

	mux.HandleFunc("/stats", func(w http.ResponseWriter, r *http.Request) {
		if !requireMethod(w, r, http.MethodGet) {
			return
		}
		if h.Stats == nil {
			unavailable(w, "stats")
			return
		}
		writeJSON(w, http.StatusOK, h.Stats())
	})

	mux.HandleFunc("/reload", func(w http.ResponseWriter, r *http.Request) {
		if !requireMethod(w, r, http.MethodPost) {
			return
		}
		if h.Reload == nil {
			unavailable(w, "reload")
			return
		}
		// Bounded: reload shells out to simctl, which can hang on a wedged simulator,
		// and a control request must not wait on that forever.
		ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
		defer cancel()

		result, err := h.Reload(ctx)
		if err != nil {
			// The CA itself could not be read. Unlike a trust failure, this one really
			// did not work.
			http.Error(w, fmt.Sprintf("reloading CA: %v", err), http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, result)
	})

	mux.HandleFunc("/shutdown", func(w http.ResponseWriter, r *http.Request) {
		if !requireMethod(w, r, http.MethodPost) {
			return
		}
		if h.Shutdown == nil {
			unavailable(w, "shutdown")
			return
		}
		// Answer first, shut down after. This response is served by the very server
		// being asked to stop, so tearing down first would drop it and leave the
		// caller unable to tell success from a broken socket.
		writeJSON(w, http.StatusAccepted, map[string]bool{"shuttingDown": true})
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		go h.Shutdown()
	})

	// A 404 body that names the real routes saves the next person a source dive.
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		http.Error(
			w,
			"proxysim control API: GET /status, GET /stats, POST /reload, POST /shutdown",
			http.StatusNotFound,
		)
	})

	return mux
}

// requireMethod enforces the verb, answering 405 with Allow when it does not match.
func requireMethod(w http.ResponseWriter, r *http.Request, method string) bool {
	if r.Method == method {
		return true
	}
	w.Header().Set("Allow", method)
	http.Error(w, fmt.Sprintf("%s requires %s", r.URL.Path, method), http.StatusMethodNotAllowed)
	return false
}

// unavailable reports an unwired handler. 501 rather than a panic: a partially
// wired server is a programming error, but not one that should take down a proxy
// that is otherwise serving traffic fine.
func unavailable(w http.ResponseWriter, name string) {
	http.Error(w, fmt.Sprintf("%s is not available in this instance", name), http.StatusNotImplemented)
}

// writeJSON marshals before writing the header, so a marshal failure can still
// become a 500 — encoding straight to the ResponseWriter would have already
// committed 200 by the time it failed.
func writeJSON(w http.ResponseWriter, status int, value any) {
	body, err := json.Marshal(value)
	if err != nil {
		http.Error(w, fmt.Sprintf("encoding response: %v", err), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(append(body, '\n'))
}
