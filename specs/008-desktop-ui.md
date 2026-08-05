# 008 — Desktop UI

**Status:** ready to implement
**Package:** `internal/ui` (new), wired through `main.go`
**Depends on:** 003 (the Flow it consumes), 006 (decoding bodies for display)

---

## Intent

Give the captured traffic a live, browsable window: rows that appear as requests
complete, click one to see its headers and decoded bodies. This is the moment 003
was built for — the UI is **a new consumer of the Flow model, not a rewrite of
the engine.** If any part of this spec finds itself reaching into `internal/proxy`
or reformatting capture internals, the seam failed and that is the bug to fix, not
to work around.

## Scope

In: a `Sink` that records completed flows and streams them to a local web app; an
HTTP server (separate listener, loopback only) serving that app, a live event
stream, a history list, and a per-flow detail view with decoded bodies. The app
is vanilla HTML/CSS/JS embedded in the binary.

Out: live *pending* rows for in-flight requests (needs the two-phase change — see
decisions), `.har`/persistence (a different future consumer of the same Flow),
editing/replaying requests, capture pause/clear controls beyond the minimum,
authentication.

---

## Non-negotiable: loopback only, same as the proxy

The UI serves **decrypted traffic** — bodies, headers, tokens, cookies. Its
listener binds `127.0.0.1` only, exactly like the proxy (CLAUDE.md). A UI that
exposed captured secrets on the network would be a worse hazard than the proxy
itself. Not `0.0.0.0`, not behind a flag, not for testing. Different port from the
proxy, same binding rule.

There is no authentication in v1: single user, single machine, loopback. That is
acceptable only because of the binding, and it is why the binding is not
negotiable.

---

## Design decisions

The expensive-to-reverse ones.

**The UI is additive.** It is a `flow.Sink` plus a web server, wired in `main.go`
alongside the console via `MultiSink`. The proxy does not know it exists. Turning
the UI off is not compiling it out; it is not registering the sink.

**Emit-once stands for v1.** 003 left this open: a UI ideally shows a row the
instant a request begins and fills in the response, so a hung request is visible.
That needs two-phase emit, which changes every consumer. v1 ships the honest
smaller thing — a list of *completed* flows, like a live HAR viewer — keeping the
engine untouched. Live pending rows are the first enhancement, and the point at
which two-phase earns its cost. Stated so the limitation is deliberate, not an
oversight: an in-flight or hung request does not appear until it completes or
errors.

**Stream with Server-Sent Events, not WebSockets.** The flow is one-way,
server→browser. SSE is plain `text/event-stream` over `net/http` — no dependency,
no upgrade handshake, automatic browser reconnect. WebSockets would add a library
and bidirectionality we do not need. (Constitution: no web framework; SSE keeps
that true.)

**History is bounded and in-memory.** A ring buffer of the last N flows
(configurable), served to a client on connect so a freshly opened tab is not
blank. No disk persistence in v1 — that is the `.har` exporter's job, a separate
consumer.

**The browser wire format is the Flow JSON from 003.** The same stable tags.
Bodies stream as the raw (still-compressed) bytes they are stored as; the detail
view decodes them with 006 on request. The list does not ship bodies — only
metadata — so a chatty session does not flood the stream.

**Backpressure drops, never blocks.** `Emit` is on the request path (003). Each
connected client has a bounded send buffer; a slow client's overflow is dropped
and counted, surfaced in the UI as "N events dropped", never allowed to stall the
proxy. This is the AsyncSink policy applied per-client.

---

## Surface

An HTTP server on `127.0.0.1:<ui-port>`:

| Route | Purpose |
|---|---|
| `GET /` | the embedded single-page app (HTML/CSS/JS) |
| `GET /events` | SSE stream: live completed flows as `data: <flow-json>` (metadata only) |
| `GET /flows` | JSON array of buffered history, newest first (metadata only) |
| `GET /flows/{id}` | one flow in full: headers plus request/response bodies, **decoded** via 006 with raw sizes reported |
| static assets | embedded via `embed.FS`, correct content types |

The app: a two-pane view — a scrolling list of rows (id, method, host, path,
status, duration, sizes; tunnelled and errored flows visually distinct), and a
detail pane for the selected flow showing headers and decoded bodies (JSON
pretty-printed, binary summarized), mirroring the console sink's rules in a
richer form.

## Interface sketch

Indicative, not binding:

```go
// Hub records completed flows and streams them to connected browsers. It is both
// the flow.Sink the proxy emits into and the http.Handler serving the UI.
type Hub struct { /* ring buffer, client registry, embedded assets */ }

func New(history int) *Hub

// Emit implements flow.Sink: buffer the flow and fan it out, non-blocking.
func (h *Hub) Emit(f *flow.Flow)

// Handler serves the app, the SSE stream, and the flow endpoints.
func (h *Hub) Handler() http.Handler
```

`main.go` gains `-ui` (enable) and `-ui-port` (default e.g. 8889); when enabled it
constructs the Hub, adds it to the sink set, and serves `Handler()` on
`127.0.0.1:<ui-port>` alongside the proxy.

---

## Acceptance criteria

HTTP-level tests via `httptest`; the visual app is the one manual criterion.

1. **Additive, non-blocking.** Registered alongside the console in a `MultiSink`,
   `Hub.Emit` returns promptly even with a stalled SSE client; the slow client's
   events drop and are counted; other consumers are unaffected.
2. **History bounded.** With capacity N, emitting N+1 flows evicts the oldest;
   `GET /flows` returns N, newest first.
3. **Live stream.** A client on `GET /events` receives a flow emitted after it
   connected, as a well-formed `text/event-stream` `data:` event carrying the
   flow JSON.
4. **Detail + decode.** `GET /flows/{id}` for a flow with a gzipped body returns
   the decoded body and the raw size; an unknown id returns 404.
5. **JSON contract.** A streamed/served flow's JSON key set matches 003's golden
   field set (shared assertion), so the browser format cannot drift silently.
6. **Loopback only.** The UI server binds `127.0.0.1`; a test asserts the bound
   address is loopback (mirrors the proxy's binding guarantee).
7. **Concurrent clients.** Several simultaneous SSE clients each receive every
   flow emitted after they connect, race-clean under `-race`.
8. **App served.** `GET /` returns the HTML app with a 200 and an HTML content
   type; embedded assets serve with correct types.
9. `go test ./internal/ui -race` passes; `go vet` clean; `gofmt -l` silent.

### Manual criterion (human-run)

10. With `-ui` on, run simulator or curl traffic through the proxy, watch rows
    appear live in the browser, select one, and see its decoded request/response —
    the debugging experience the whole tool exists to provide.

---

## Open questions

- **Two-phase / live pending rows.** The headline follow-up: emit on request
  start and completion sharing an ID, so hung requests are visible. Touches every
  consumer; deferred deliberately. This spec's shape leaves room (a flow has an
  ID; a later "update" event keys on it).
- **Filtering and search.** v1 can filter client-side over the buffered rows;
  server-side search across a large history is a later want. Start client-side.
- **SSE replay on reconnect.** `Last-Event-ID` lets a reconnecting tab catch up
  without a full reload. Cheap and additive; include lightly if it falls out
  naturally, otherwise defer.
- **Capture controls.** Pause/clear are small `POST` endpoints; a read-only v1 is
  fine, but a "clear" is a likely early want. Keep the door open.
- **TLS/timing detail.** 003's deferred additive fields (negotiated cipher, TTFB
  breakdown) surface naturally in the detail pane once captured. Non-breaking, so
  they slot in when the engine records them.
- **Multiple proxysim instances.** Two runs would want two UI ports; the `-ui-port`
  flag already allows it. No shared state, so nothing else to coordinate.
