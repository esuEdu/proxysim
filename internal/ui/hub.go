// Package ui is a new consumer of the Flow model (spec 003), not a rewrite of
// the engine: a flow.Sink that records completed flows into a bounded ring and
// fans them out to browsers over Server-Sent Events, plus the http.Handler that
// serves the single-page app, the history, and per-flow detail. The proxy does
// not know it exists — it is wired in main.go alongside the console via a
// MultiSink, and "off" simply means not registering it. See specs/008-desktop-ui.md.
package ui

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"proxysim/internal/flow"
	"proxysim/internal/origin"
	"proxysim/internal/sim"
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

	// Runtime origin control (spec 011), wired via SetControl only when the UI
	// serves. While nil, the /sims, /filter, … endpoints report "not enabled" —
	// the UI is otherwise a read-only viewer, exactly as spec 008.
	filter   *origin.Controller
	listSims func(context.Context) ([]sim.Device, error)
	listApps func(context.Context, string) ([]sim.App, error)

	// Xcode auto-start toggle (spec 012), wired via SetArmedControl. Independent of
	// the origin filter: it persists whether building in Xcode launches proxysim at
	// all, so the flag outlives any single process and is read from disk here.
	getArmed func() (bool, error)
	setArmed func(bool) error
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

// SetControl wires the runtime origin filter and the simulator/app enumeration
// the control bar drives (spec 011). Called once at startup when the UI is
// enabled; without it the control endpoints report that control is off. The
// enumerators take a context so a slow simctl cannot wedge a request.
func (h *Hub) SetControl(filter *origin.Controller, sims func(context.Context) ([]sim.Device, error), apps func(context.Context, string) ([]sim.App, error)) {
	h.filter = filter
	h.listSims = sims
	h.listApps = apps
}

// SetArmedControl wires the "auto-start on Xcode build" toggle (spec 012) to the
// on-disk armed flag. Called once at startup when the UI serves; without it the
// /armed endpoints report that control is off, so the toggle stays hidden.
func (h *Hub) SetArmedControl(get func() (bool, error), set func(bool) error) {
	h.getArmed = get
	h.setArmed = set
}

// Handler serves the app at /, the live SSE stream at /events, the buffered
// history at /flows, one flow in full at /flows/{id}, and — when origin control is
// wired — the simulator/app enumeration and the live filter (spec 011). The
// control routes are always registered but reflect "not enabled" until SetControl
// runs, so the same handler works with or without control.
func (h *Hub) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /events", h.handleEvents)
	mux.HandleFunc("GET /flows", h.handleFlows)
	mux.HandleFunc("GET /flows/{id}", h.handleDetail)
	mux.HandleFunc("GET /sims", h.handleSims)
	mux.HandleFunc("GET /sims/{udid}/apps", h.handleApps)
	mux.HandleFunc("GET /filter", h.handleGetFilter)
	mux.HandleFunc("PUT /filter", h.handleSetFilter)
	mux.HandleFunc("GET /armed", h.handleGetArmed)
	mux.HandleFunc("PUT /armed", h.handleSetArmed)
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

// filterJSON is the wire shape of the live origin filter, both directions. It
// mirrors the -only-sim/-app flags: onlySim narrows to the simulator, apps to
// specific bundle ids (a non-empty apps set implies onlySim, per spec 009).
type filterJSON struct {
	OnlySim bool     `json:"onlySim"`
	Apps    []string `json:"apps"`
}

// handleSims lists the booted simulators for the control bar. An empty set is a
// normal state (no sim booted yet), returned as [] rather than an error, so the
// bar can degrade rather than the page break.
func (h *Hub) handleSims(w http.ResponseWriter, r *http.Request) {
	if h.listSims == nil {
		controlDisabled(w)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	devices, err := h.listSims(ctx)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	type simJSON struct {
		UDID    string `json:"udid"`
		Name    string `json:"name"`
		Runtime string `json:"runtime"`
	}
	out := make([]simJSON, 0, len(devices))
	for _, d := range devices {
		out = append(out, simJSON{UDID: d.UDID, Name: d.Name, Runtime: d.Runtime})
	}
	writeJSON(w, out)
}

// handleApps lists a simulator's installed user apps for the app dropdown.
func (h *Hub) handleApps(w http.ResponseWriter, r *http.Request) {
	if h.listApps == nil {
		controlDisabled(w)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	apps, err := h.listApps(ctx, r.PathValue("udid"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	if apps == nil {
		apps = []sim.App{}
	}
	writeJSON(w, apps)
}

// handleGetFilter returns the filter currently in force, so a freshly loaded (or
// second) tab reflects a flag-set or previously-chosen selection.
func (h *Hub) handleGetFilter(w http.ResponseWriter, r *http.Request) {
	if h.filter == nil {
		controlDisabled(w)
		return
	}
	writeJSON(w, toFilterJSON(h.filter.Current()))
}

// handleSetFilter replaces the live filter. It validates the request shape, not
// the existence of the named apps: a bundle id for an app not yet launched is
// accepted and simply matches once that app runs (spec 009's safe direction), so
// the filter is never coupled to the enumerated list.
func (h *Hub) handleSetFilter(w http.ResponseWriter, r *http.Request) {
	if h.filter == nil {
		controlDisabled(w)
		return
	}
	var body filterJSON
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid filter body", http.StatusBadRequest)
		return
	}
	f := origin.BuildFilter(body.OnlySim, body.Apps)
	h.filter.Set(f)
	writeJSON(w, toFilterJSON(f))
}

// armedJSON is the wire shape of the Xcode auto-start toggle, both directions.
type armedJSON struct {
	Armed bool `json:"armed"`
}

// handleGetArmed reports whether auto-start on Xcode build is enabled, so the
// toggle reflects the persisted flag (including a second tab or a prior session).
func (h *Hub) handleGetArmed(w http.ResponseWriter, r *http.Request) {
	if h.getArmed == nil {
		controlDisabled(w)
		return
	}
	armed, err := h.getArmed()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, armedJSON{Armed: armed})
}

// handleSetArmed persists the toggle. The change takes effect on the next Xcode
// build — the pre-action reads this same flag from disk — not on the running
// session, so no proxy state changes here.
func (h *Hub) handleSetArmed(w http.ResponseWriter, r *http.Request) {
	if h.setArmed == nil {
		controlDisabled(w)
		return
	}
	var body armedJSON
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid armed body", http.StatusBadRequest)
		return
	}
	if err := h.setArmed(body.Armed); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, armedJSON{Armed: body.Armed})
}

// toFilterJSON renders a Filter for the wire, its bundle-id set as a sorted slice
// for a stable response.
func toFilterJSON(f origin.Filter) filterJSON {
	apps := make([]string, 0, len(f.Apps))
	for id := range f.Apps {
		apps = append(apps, id)
	}
	sort.Strings(apps)
	return filterJSON{OnlySim: f.OnlySim, Apps: apps}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

// controlDisabled answers the control endpoints when SetControl was never called
// (the UI is a read-only viewer). 501 says "this server does not offer it", as
// distinct from a 404 for an unknown route.
func controlDisabled(w http.ResponseWriter) {
	http.Error(w, "origin control not enabled", http.StatusNotImplemented)
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
