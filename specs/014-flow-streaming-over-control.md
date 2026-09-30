# Spec 014 — Flow Streaming over the Control Socket

**Status:** proposed
**Depends on:** 003 (flow model), 006 (body decoding), 008 (the UI Hub), 013 (control API)

## Motivation

Spec 013 gave a host application proxysim's *counters*. A host app that wants to be
a traffic inspector needs the traffic: a live list of flows, and one flow in full.

proxysim already does exactly this for its own web UI — `internal/ui.Hub` is a
bounded ring of completed flows plus an SSE fan-out plus a per-flow detail endpoint
with spec-006 decoding. There is no reason to write a second one, and every reason
not to: two flow stores would drift, and the second would be the untested one.

So this spec adds no new flow machinery. It makes the Hub's existing flow endpoints
mountable on a second listener.

## What is added

`ui.Hub` gains `FlowHandler()`: the same `/flows`, `/flows/{id}` and `/events`
routes its web handler already serves, without the embedded web app, the origin
control endpoints, or the Xcode armed toggle.

`control.Handlers` gains a `Flows http.Handler` field. The control server mounts it
under its own paths. `internal/control` still imports nothing from `internal/ui` —
the handler arrives injected, exactly as every other capability there does.

| Endpoint | Behaviour |
|---|---|
| `GET /flows` | buffered history, newest first, metadata only — no bodies |
| `GET /flows/{id}` | one flow in full, bodies decoded per spec 006 |
| `GET /events` | SSE: one `data:` frame per completed flow, plus `event: dropped` |

Bodies are deliberately absent from the list. A chatty session would otherwise ship
megabytes per second to a client rendering a table of one-line rows; the detail
endpoint is where bodies live, fetched for the one flow a user selected.

## The Hub is created whenever there is a consumer

Today the Hub exists only when `-ui` is passed. It now also exists when
`-control-socket` is passed, because the control socket is a consumer of flows in
exactly the same way a browser is. With both, there is still **one** Hub feeding
both listeners — one ring, one set of ids, so a flow id from `/events` resolves on
`/flows/{id}` regardless of which listener asked.

`-ui` remains what decides whether the *web app* is served. The two are now
independent: a host app gets flows without a web server being started.

## Ring size

`-ui-history` (default 1000) already sizes the ring and keeps doing so. It is not
renamed, because renaming a flag to make it read better for a second consumer is a
worse outcome than a flag whose name is slightly narrow.

## Non-negotiable, restated from 013

With neither `-ui` nor `-control-socket`, nothing here is constructed and no
listener exists. A terminal `proxysim` is unchanged, and the console sink remains
the only consumer.

## SSE over a Unix socket

Nothing special is required of the server: SSE is a normal HTTP response that is
never finished, and `http.Flusher` works on any `net.Listener`. The constraint
lands on the *client*, which must read the response incrementally rather than to
EOF. That is the client's problem, and is noted here only so the next reader does
not go looking for special-casing that is absent.

One server-side consequence does matter: `ReadHeaderTimeout` bounds only the
headers, so an SSE response can stay open indefinitely as intended. A write
deadline would break streaming, and there is deliberately none.

## Acceptance criteria

1. `go build ./...`, `go vet ./...`, `gofmt -l .` silent, `go test ./... -race` passes.
2. `-control-socket` alone serves `/flows`, `/flows/{id}` and `/events`, and starts
   no web server.
3. `-ui` alone behaves exactly as before.
4. With both, a flow id seen on the control socket's `/events` resolves on the web
   UI's `/flows/{id}` and vice versa — one ring, one id space.
5. `/flows` carries no bodies; `/flows/{id}` carries decoded bodies.
6. With neither flag, no Hub is constructed.
7. `curl --unix-socket <path> -N http://localhost/events` streams flows as they
   complete.
