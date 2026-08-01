// Package ui is a new consumer of the Flow model (spec 003), not a rewrite of
// the engine: a flow.Sink that records completed flows into a bounded ring and
// fans them out to browsers over Server-Sent Events, plus the http.Handler that
// serves the single-page app, the history, and per-flow detail. The proxy does
// not know it exists — it is wired in main.go alongside the console via a
// MultiSink, and "off" simply means not registering it. See specs/008-desktop-ui.md.
package ui

import (
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"

	"proxysim/internal/flow"
)

//go:embed app
var appFS embed.FS

// appRoot is the embedded app directory served at /. Resolved once; fs.Sub over
// a static embed cannot fail at runtime, but we fall back rather than panic to
// honour the "no panic outside main" rule.
var appRoot = func() fs.FS {
	if sub, err := fs.Sub(appFS, "app"); err == nil {
		return sub
	}
	return appFS
}()

// clientBuffer is the per-client SSE send buffer. Overflow drops, never blocks
// (the AsyncSink policy applied per-client), so one wedged browser tab cannot
// stall the proxy request path that calls Emit.
const clientBuffer = 256

// client is one connected SSE browser. Its channel carries pre-marshalled
// metadata JSON; dropped counts events shed because the buffer was full.
type client struct {
	ch      chan []byte
	dropped atomic.Uint64
}

// Hub records completed flows and streams them to connected browsers. It is both
// the flow.Sink the proxy emits into and the http.Handler serving the UI. All
// state is guarded by mu; everything reachable from Emit is race-clean because
// Emit runs on the request path.
type Hub struct {
	mu       sync.Mutex
	ring     []*flow.Flow // bounded history, oldest first
	capacity int
	clients  map[*client]struct{}

	app http.Handler
}

// New returns a Hub buffering the last history completed flows. A non-positive
// history is clamped to 1 so the ring is always usable.
func New(history int) *Hub {
	if history < 1 {
		history = 1
	}
	return &Hub{
		capacity: history,
		clients:  make(map[*client]struct{}),
		app:      http.FileServer(http.FS(appRoot)),
	}
}

// Emit implements flow.Sink: buffer the flow and fan its metadata out to every
// connected browser, non-blocking. Bodies are stripped here — the list must not
// ship them (a chatty session would flood the stream); they are fetched per-flow
// from the detail endpoint. A slow client's overflow is dropped and counted, the
// required degradation: lose events, never stall traffic.
func (h *Hub) Emit(f *flow.Flow) {
	data, err := json.Marshal(meta(f))
	if err != nil {
		// A flow that will not marshal is dropped, not fatal: Emit is on the
		// request path and must never take the proxy down.
		return
	}

	h.mu.Lock()
	h.ring = append(h.ring, f)
	if len(h.ring) > h.capacity {
		// Evict the oldest. append amortises reallocation, so the backing array
		// stays bounded at roughly capacity.
		h.ring = h.ring[len(h.ring)-h.capacity:]
	}
	targets := make([]*client, 0, len(h.clients))
	for c := range h.clients {
		targets = append(targets, c)
	}
	h.mu.Unlock()

	for _, c := range targets {
		select {
		case c.ch <- data:
		default:
			c.dropped.Add(1)
		}
	}
}

// Dropped reports how many events have been shed across currently connected
// clients because their buffers were full — surfaced in the UI as "N dropped".
func (h *Hub) Dropped() uint64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	var total uint64
	for c := range h.clients {
		total += c.dropped.Load()
	}
	return total
}

// Handler serves the app at /, the live SSE stream at /events, the buffered
// history at /flows, and one flow in full at /flows/{id}.
func (h *Hub) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /events", h.handleEvents)
	mux.HandleFunc("GET /flows", h.handleFlows)
	mux.HandleFunc("GET /flows/{id}", h.handleDetail)
	mux.Handle("GET /", h.app)
	return mux
}

// handleFlows returns the buffered history as metadata-only flows, newest first,
// so a freshly opened tab is not blank.
func (h *Hub) handleFlows(w http.ResponseWriter, r *http.Request) {
	h.mu.Lock()
	metas := make([]*flow.Flow, 0, len(h.ring))
	for i := len(h.ring) - 1; i >= 0; i-- {
		metas = append(metas, meta(h.ring[i]))
	}
	h.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(metas)
}

// handleDetail returns one flow in full: the raw 003 flow (bodies as stored)
// plus its request/response bodies decoded via spec 006 with raw sizes reported.
// An unknown id is a 404.
func (h *Hub) handleDetail(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseUint(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "invalid flow id", http.StatusBadRequest)
		return
	}
	f := h.lookup(id)
	if f == nil {
		http.Error(w, fmt.Sprintf("flow %d not found", id), http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(buildDetail(f))
}

// handleEvents streams completed flows to the browser as they are emitted, over
// text/event-stream. Only flows emitted after the client connects arrive here;
// existing history comes from /flows on load.
func (h *Hub) handleEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	flusher.Flush() // commit headers so the browser opens the stream immediately

	c := &client{ch: make(chan []byte, clientBuffer)}
	h.addClient(c)
	defer h.removeClient(c)

	ctx := r.Context()
	var lastDropped uint64
	for {
		select {
		case <-ctx.Done():
			return
		case data := <-c.ch:
			// Surface any events shed since the last send, so the tab can show
			// "N dropped" rather than silently missing rows.
			if d := c.dropped.Load(); d != lastDropped {
				lastDropped = d
				fmt.Fprintf(w, "event: dropped\ndata: %d\n\n", d)
			}
			if _, err := fmt.Fprintf(w, "data: %s\n\n", data); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

func (h *Hub) addClient(c *client) {
	h.mu.Lock()
	h.clients[c] = struct{}{}
	h.mu.Unlock()
}

func (h *Hub) removeClient(c *client) {
	h.mu.Lock()
	delete(h.clients, c)
	h.mu.Unlock()
}

// lookup returns the buffered flow with the given id, or nil if it has been
// evicted or never existed.
func (h *Hub) lookup(id uint64) *flow.Flow {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, f := range h.ring {
		if f.ID == id {
			return f
		}
	}
	return nil
}

// meta returns a shallow copy of f with bodies stripped. Header maps are shared
// read-only. Because RequestBody/ResponseBody are omitempty, the result is still
// exactly the 003 Flow JSON, only without the (potentially large) body bytes.
func meta(f *flow.Flow) *flow.Flow {
	m := *f
	m.RequestBody = nil
	m.ResponseBody = nil
	return &m
}

// Listen binds a TCP listener for the UI on loopback only. The UI serves
// decrypted traffic — bodies, headers, tokens, cookies — so like the proxy it
// must never be reachable from the network: the address is 127.0.0.1 and is not
// configurable, not behind a flag, not for testing (see CLAUDE.md). Port 0 asks
// the OS for a free port, used by tests.
func Listen(port int) (net.Listener, error) {
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("binding UI listener on %s: %w", addr, err)
	}
	return ln, nil
}
