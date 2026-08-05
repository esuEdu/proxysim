# 004 — Plain HTTP Forwarding

**Status:** ready to implement
**Package:** `internal/proxy`
**Depends on:** 003 (writes `flow.Flow` into a `flow.Sink`)

---

## Intent

Handle the simplest interception path first: a client configured to use us as an
HTTP proxy sends a plain `http://` request in absolute form, and we forward it,
capture the exchange, and emit one `Flow`. No TLS, no CONNECT — those are 005.

This spec exists on its own, ahead of the TLS work, because everything hard
about capturing an exchange faithfully — hop-by-hop header handling, body
capture without breaking forwarding, compression fidelity, emit-on-completion —
is identical whether the transport underneath is cleartext or a terminated TLS
socket. Getting it right here means 005 only has to add the socket, not
reinvent the forwarding. If it is invented while deep in TLS plumbing it will be
wrong in ways that are hard to see.

## Scope

In: an `http.Handler` that forwards absolute-form `http://` requests upstream,
strips hop-by-hop headers, captures request and response into a capped `Flow`,
and emits it once complete. Upstream errors become a `502` plus an errored flow.

Out: `CONNECT` and TLS termination (→ 005), deciding which hosts to intercept
(→ 005), body decompression for display (→ 006 — this spec stores bytes and
decodes nothing), the listener/flag wiring in `main.go`.

---

## The forward-proxy request shape

A configured HTTP proxy does not receive origin-form requests (`GET /path`). It
receives **absolute-form** ones (RFC 9112 §3.2.2):

```
GET http://example.com/path?q=1 HTTP/1.1
Host: example.com
Proxy-Connection: keep-alive
```

`net/http`'s server parses this: `r.URL` is absolute (scheme + host + path),
`r.Host` is set, `r.RequestURI` holds the raw target. The handler forwards to
`r.URL`, it does not re-dial `r.Host` blindly.

A request whose `r.URL` is **not** absolute (missing scheme/host) is not a proxy
request — reject it `400`, do not treat the proxy's own address as the origin.

A `CONNECT` reaching this handler is out of scope here; until 005 wires it,
respond `405 Method Not Allowed`. Do not silently 200 it.

---

## Behaviour

For each forwardable request:

1. **Build the upstream request.** Copy method, URL, and end-to-end headers.
   Strip the hop-by-hop set (below). Give the outbound request a context tied to
   the inbound one so a client disconnect cancels the upstream call.
2. **Round-trip** through a shared `*http.Transport`, timing the call.
3. **Relay the response**: strip hop-by-hop headers from it, copy status and
   end-to-end headers to the client, stream the body back.
4. **Emit one completed `Flow`** — see the flow mapping. Emit after the response
   body is fully relayed, not before (emit-once, per 003).

### Hop-by-hop headers

These are connection-scoped and must never be forwarded in either direction
(RFC 9110 §7.6.1):

```
Connection, Proxy-Connection, Keep-Alive, Proxy-Authenticate,
Proxy-Authorization, TE, Trailer, Transfer-Encoding, Upgrade
```

Additionally, **every header named in the `Connection` header** is itself
hop-by-hop and must be stripped — a server may list custom headers there. Strip
those before stripping `Connection` itself.

`Proxy-Connection` is not in any RFC but is sent by real clients; treat it as
hop-by-hop.

### Bodies: capture capped, forward whole

The flow keeps at most `flow.DefaultBodyCap` bytes (configurable), but the client
and upstream must always receive the **complete** body. Capture therefore tees:
bytes flow through to their destination while the first cap bytes are retained
and `Truncated` is set if more arrive. Never buffer the whole body to memory to
capture a prefix of it, and never forward only the captured prefix. A body larger
than the cap must still round-trip in full.

### Compression fidelity

Set `Transport.DisableCompression = true`. Without it, Go's transport
transparently adds `Accept-Encoding: gzip` and decompresses the response,
so the captured bytes would not be what crossed the wire and the client's own
`Accept-Encoding` negotiation would be overridden. With it, whatever
content-coding the origin returns is stored and relayed verbatim; decoding for
display is 006's job.

Note the one thing `net/http` normalises unavoidably: **transfer-encoding**
(chunked) is stripped by the transport, so a chunked response is captured
de-chunked. That is acceptable — chunking is framing, not content. Content-*
encoding* (gzip/br) is what must be preserved, and `DisableCompression` preserves
it.

### Errors never take the proxy down

- Upstream dial/round-trip failure → respond `502 Bad Gateway`, emit a flow with
  `Error` set, `StatusCode` 0, response fields zero.
- A malformed request from the client → `400`, no upstream call, and either no
  flow or an errored flow (implementer's choice, but be consistent).
- No `panic` on any request path. A single bad request must not affect others.

---

## Flow mapping

| `Flow` field | Source |
|---|---|
| `ID` | `flow.NextID()` |
| `Started` / `Duration` | wall clock around the round-trip |
| `Scheme` | `"http"` |
| `Method` | `r.Method` |
| `Host` | `r.URL.Host` (host, or host:port as sent) |
| `Path` | `r.URL.Path` plus `?`+`RawQuery` when present |
| `RequestHeaders` | end-to-end headers as received (hop-by-hop removed) |
| `RequestBody` / `RequestTruncated` | capped tee of the request body |
| `StatusCode` | `resp.StatusCode` |
| `ResponseHeaders` | end-to-end response headers |
| `ResponseBody` / `ResponseTruncated` | capped tee of the response body |
| `Intercepted` | `true` — cleartext HTTP is always inspectable |
| `Error` | `""`, or the upstream error string on failure |

Headers stored are the end-to-end set (hop-by-hop removed): the flow records the
meaningful exchange, not the proxy's connection bookkeeping.

## Interface sketch

Indicative, not binding:

```go
// Server forwards plain HTTP proxy requests and emits a Flow per exchange.
// It is an http.Handler so 005 can wrap it to add CONNECT on the same listener.
type Server struct { /* sink, transport, body cap */ }

func New(sink flow.Sink, opts ...Option) *Server

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request)
```

The `Server` owns one `*http.Transport` shared across requests (connection
pooling). The body cap is an option defaulting to `flow.DefaultBodyCap`.

---

## Acceptance criteria

Each executable, using `httptest` for the upstream and a real proxied
`http.Client` (`Transport.Proxy`) as the client.

1. **Forward.** A GET through the proxy to an httptest origin returns the
   origin's status and body byte-for-byte.
2. **Flow emitted.** A capturing sink receives exactly one flow with
   `Scheme:"http"`, correct method/host/path, `StatusCode` matching, and
   `Intercepted:true`.
3. **Hop-by-hop stripping.** A request carrying `Proxy-Connection`, `Connection: X-Custom`,
   and `X-Custom` arrives at the origin with all three absent, and a `Keep-Alive`
   response header does not reach the client.
4. **Body capture and full forwarding.** With the cap set to N, a POST of N+M
   bytes yields a flow whose `RequestBody` is N bytes with `RequestTruncated:true`,
   while the origin receives all N+M bytes. Symmetrically for a response larger
   than the cap.
5. **Compression fidelity.** An origin returning `Content-Encoding: gzip` yields a
   flow whose `ResponseBody` is still gzip (starts with `0x1f 0x8b`), and the
   client receives the identical compressed bytes.
6. **Upstream failure.** Proxying to a closed port returns `502` to the client and
   emits a flow with non-empty `Error` and `StatusCode` 0.
7. **Non-proxy request.** An origin-form request (no absolute URL) gets `400` and
   is not forwarded.
8. **Concurrency.** 50 concurrent proxied requests under `-race` produce 50 flows
   and no race.
9. `go test ./internal/proxy -race` passes; `go vet` clean; `gofmt -l` silent.

Criteria 1–9 are unit-level and self-contained. The end-to-end curl/Simulator
proof lives in 005, where TLS termination joins this forwarding on one listener.

---

## Open questions

- **Request-header fidelity.** We store headers as received minus hop-by-hop.
  An alternative is storing exactly what we forwarded upstream. They differ only
  in the stripped set; received-minus-hop-by-hop is the more faithful record of
  the client's intent. Settled that way unless a case argues otherwise.
- **`Via` header.** RFC 9110 says a proxy should append one. A single-user local
  debugger gains nothing from it and it clutters captures. Omit for now; note it
  here so the omission is deliberate.
- **Upgrade / WebSocket over cleartext.** An `Upgrade: websocket` request cannot
  be forwarded by a plain round-trip. Out of scope for 004; likely a blind tunnel
  like a pinned host (relates to 005's fallback). Reject or 501 for now.
- **Transport timeouts.** A debugging proxy may watch deliberately slow requests,
  so an aggressive overall timeout is wrong. A dial timeout is fine; response
  timeout should be generous or absent. Tune when observed.
