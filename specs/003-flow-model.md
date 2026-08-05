# 003 — Flow Model

**Status:** ready to implement
**Package:** `internal/flow`
**Depends on:** nothing (deliberately — it must not import `proxy` or `ca`)

---

## Intent

`Flow` is the record of one request/response exchange. It is also the
architectural seam between the capture engine and everything that consumes
captured traffic: the console renderer today, a desktop UI later, a `.har`
exporter if it ever gets one.

This spec exists early and separately because of a specific failure mode. If the
flow model is invented ad hoc while the agent is deep in TLS plumbing, it will
end up as pre-formatted console strings — and then the UI is a rewrite of the
engine rather than a new consumer of it. Defining the contract first is cheap;
retrofitting it is not.

**Implement this before 004 and 005.** Both write into it.

## Scope

In: the `Flow` type, its lifecycle, the `Sink` interface, JSON serialisation, the
console sink.

Out: producing flows (→ 004, 005), body decompression (→ 006 — this spec stores
bytes and says nothing about their encoding), any UI.

---

## Design decisions

These are the ones that are expensive to reverse. Everything else is negotiable.

**Store raw bytes, never decoded ones.** A flow holds exactly what crossed the
wire — gzipped, chunked, whatever it was. Decoding is a presentation concern that
happens at render time. Storing decoded bodies destroys the ability to show the
user what was actually sent, and makes byte counts lie.

**Bodies are capped, and truncation is explicit.** A `Truncated bool` alongside
the bytes. A UI that cannot distinguish "empty body" from "body we declined to
buffer" will display a confident lie. Default cap: 10 MiB, configurable.

**Sinks must not block the proxy.** `Emit` is called from the request path. A
slow or wedged sink must degrade to dropping flows, never to stalling traffic.
The console sink is synchronous and fast enough; a future UI sink will need a
buffered channel with a drop-on-full policy. Bake the assumption in now by
documenting `Emit` as non-blocking and required to return promptly.

**Flows are emitted once, complete.** Not streamed as request-then-response. A
partial-flow model complicates every consumer for the benefit of a UI that does
not exist. Errored flows are still emitted, with `Error` set and response fields
zero.

**JSON field names are a stable contract.** Explicit tags on everything. Renaming
a field breaks persisted captures.

---

## Shape

Indicative:

```go
type Flow struct {
    ID       uint64        `json:"id"`        // monotonic, per-process
    Started  time.Time     `json:"started"`
    Duration time.Duration `json:"duration_ns"`

    Scheme string `json:"scheme"`            // "http" | "https"
    Method string `json:"method"`
    Host   string `json:"host"`
    Path   string `json:"path"`              // path + query

    RequestHeaders   http.Header `json:"request_headers"`
    RequestBody      []byte      `json:"request_body,omitempty"`
    RequestTruncated bool        `json:"request_truncated"`

    StatusCode        int         `json:"status_code"`
    ResponseHeaders   http.Header `json:"response_headers"`
    ResponseBody      []byte      `json:"response_body,omitempty"`
    ResponseTruncated bool        `json:"response_truncated"`

    Intercepted bool   `json:"intercepted"`  // false = blind tunnel, pinned host
    Error       string `json:"error,omitempty"`
}

// Sink consumes completed flows. Implementations must return promptly and must
// not block the proxy request path; drop rather than stall.
type Sink interface {
    Emit(*Flow)
}
```

`Error` is a string, not an `error` — it has to survive JSON round-tripping.

Include a `MultiSink` fanning out to several sinks, so console and file capture
can run together without the proxy knowing.

### Console sink

Human-readable, one exchange per block: method, host, path, status, duration,
byte counts. Headers and bodies behind a verbosity flag — a chatty app makes full
output unreadable within seconds. Pretty-print JSON bodies; for binary content,
print a type and size rather than dumping bytes at the terminal.

---

## Acceptance criteria

1. **Round-trip.** A populated `Flow` marshalled to JSON and back is deep-equal
   to the original, including a body containing arbitrary non-UTF-8 bytes.
2. **Field stability.** A golden-file test asserts the exact JSON key set. It
   fails if a field is renamed or dropped — that is its entire purpose.
3. **IDs.** 1000 concurrent `NextID` calls under `-race` yield 1000 distinct
   values.
4. **Non-blocking.** A sink that sleeps 1s in `Emit`, wrapped in the buffered
   async sink, does not delay the caller beyond the buffer filling; flows are
   dropped and the drop is counted.
5. **Truncation honesty.** A flow with `RequestTruncated: true` renders in the
   console with a visible truncation marker, distinguishable from an empty body.
6. **Independence.** `internal/flow` imports nothing from `internal/proxy` or
   `internal/ca`. Enforce it in a test that inspects the import graph, or accept
   it as a review rule — but state it either way, because the natural drift is
   for flow to start reaching into proxy for convenience.
7. `go test ./internal/flow -race` passes.

---

## Open questions

- **`NEEDS DECISION` — emit-once vs two-phase.** The decision above is to emit a
  single completed flow. The alternative is emitting on request-start and again
  on completion, sharing an ID. Charles and Proxyman both show a row the instant
  a request begins and fill in the response as it arrives; a slow or hanging
  request is invisible under emit-once, which is precisely when you most want to
  see it. Against: every consumer gains update-by-ID logic for a UI that does
  not yet exist, and the console sink gains nothing at all.
  This is cheap now and a change to every consumer later, so decide it
  deliberately rather than by default. Emit-once stands until overruled.
- Should `Flow` carry the raw TLS details (negotiated version, cipher suite, peer
  cert chain)? Useful for debugging pinning failures, and cheap to capture at
  handshake time. Likely a later additive field — noted here so the shape leaves
  room for it.
- Timing granularity: one `Duration` now. DNS/connect/TTFB breakdown is a
  plausible later want; adding fields is non-breaking, so defer.
