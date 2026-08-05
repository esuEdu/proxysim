# 005 — CONNECT, TLS Termination, and MITM Exclusions

**Status:** ready to implement
**Package:** `internal/proxy`
**Depends on:** 002 (leaf minting + `TLSConfigFor`), 003 (flow), 004 (forwarding core)

---

## Intent

This is where the proxy earns its name. A client wanting `https://api.example.com`
through a proxy first sends `CONNECT api.example.com:443`. We answer `200`, then
either **terminate the TLS** ourselves — presenting a leaf from 002 so we can
read and forward the plaintext, capturing it exactly as 004 does for cleartext —
or, when a host cannot or should not be intercepted, fall back to a **blind TCP
tunnel** and record it as un-inspectable.

Everything hard about faithful capture was already solved in 004. 005 adds only
the socket underneath: the CONNECT dance, the TLS handshake as a server toward
the client and as a client toward the origin, and the decision of which hosts to
intercept. It deliberately reuses 004's forwarding core rather than reinventing
it.

## Scope

In: the `CONNECT` handshake, connection hijacking, TLS termination using
`ca.Authority.TLSConfigFor`, forwarding decrypted requests over the tunnel
(keep-alive included), the blind-tunnel fallback, static and learned host
exclusions, and the flow mapping for both intercepted-HTTPS and tunnelled flows.

Out: HTTP/2 interception (→ later; we force HTTP/1.1), WebSocket over TLS,
body decompression for display (→ 006), `simctl` trust automation (→ 007), the
listener/flag wiring in `main.go`.

---

## The CONNECT dance (the expensive-to-rediscover part)

1. Client sends `CONNECT host:port HTTP/1.1`. `net/http` delivers it with
   `r.Method == "CONNECT"` and `r.Host == "host:port"`.
2. **Hijack** the connection (`http.Hijacker`). The `ResponseWriter` cannot be
   used after this — we own the raw `net.Conn`.
3. Write exactly `HTTP/1.1 200 Connection Established\r\n\r\n` — status line, no
   headers, blank line. Anything else and clients balk.
4. From here the bytes on the socket are the client's TLS handshake (if we
   intercept) or opaque TLS to be relayed (if we tunnel).

**The buffering trap.** `Hijack` returns a `*bufio.ReadWriter` whose reader may
**already hold bytes past the CONNECT line** — a client that pipelines its TLS
`ClientHello` immediately after `CONNECT` will have those bytes sitting in the
bufio reader, not on the raw socket. Reading straight from the `net.Conn` loses
them and the TLS handshake hangs or fails. Every read after hijack must go
through a conn that first drains the buffered reader, then the socket. Wrap the
hijacked conn so buffered bytes are consumed first. This bug is invisible in
tests that use non-pipelining clients and appears only against real ones.

## The interception decision

On CONNECT to `host`:

- **Host is excluded** (static config or learned, below) → **blind tunnel**.
- **Otherwise** → **intercept**.

There is no reliable way to know in advance that a client will pin. So the
default is to intercept, and pinning is discovered by observing the client reject
our certificate — see the fallback.

### Intercept path

1. Respond `200`, wrap the (buffer-drained) client conn in `tls.Server` using
   `authority.TLSConfigFor(host)`. The leaf is chosen by SNI, falling back to the
   CONNECT host when the client sends none — exactly the closure 002 built. A
   CONNECT to a bare IP (no SNI) therefore still gets a correct leaf.
2. Force **HTTP/1.1**: set `NextProtos: ["http/1.1"]` on the server side so we
   never negotiate h2 we cannot yet parse. (HTTP/2 interception is out of scope.)
3. Over the decrypted conn, read requests in a loop (`http.ReadRequest` or a
   single-conn `http.Serve`), reconstruct each request's absolute URL as
   `https://` + `r.Host` + target, and hand it to **004's forwarding core** with
   `Scheme:"https"`. Multiple requests per connection (keep-alive) each produce
   their own flow.
4. Upstream, the proxy is a **TLS client** to the origin: dial `host:port`,
   `tls.Client` with `ServerName` = the SNI/CONNECT host, and **verify the
   origin's real certificate against system roots**. We decrypt for the user; we
   do not become a soft target ourselves. A test seam allows injecting trust or
   disabling verification for httptest origins.

### Blind-tunnel path

Dial `host:port`, write the `200`, then `io.Copy` in both directions until either
side closes. The client's TLS is end-to-end with the origin; we see only
ciphertext. Emit one flow with `Intercepted:false` so the UI shows the host as
seen-but-not-inspected. Never fail the connection, never retry (per the
constitution).

### Learned exclusions (the pinning fallback)

A pinning app rejects our leaf even when our CA is trusted, aborting the
client-side TLS handshake (a `bad_certificate` alert or an abrupt close). We
cannot rescue **that** connection — its `ClientHello` is already consumed by our
`tls.Server` — but we can stop breaking the **next** one:

- When the client-side handshake fails, **add `host` to a runtime exclusion set**
  and emit a flow marked `Intercepted:false` with an `Error` noting the failed
  interception.
- Subsequent CONNECTs to that host take the blind-tunnel path from the start.

This makes pinning self-correcting: at most one broken connection per host per
run, then transparent tunnelling — never a hard failure the user has to
diagnose. The set is per-process and concurrency-safe.

Static exclusions (user-configured host suffixes) skip even the first attempt.

---

## Flow mapping

| Field | Intercepted HTTPS | Blind tunnel |
|---|---|---|
| `Scheme` | `"https"` | `"https"` |
| `Method` / `Path` | from the decrypted request | `"CONNECT"` / `""` |
| `Host` | decrypted `r.Host` (or CONNECT host) | CONNECT host |
| request/response fields | as 004 captures them | zero (opaque) |
| `Intercepted` | `true` | `false` |
| `Error` | upstream error, if any | set on a failed-interception fallback, else empty |

Byte counters for tunnelled connections are not part of the current `Flow` shape;
if wanted later they are an additive field (noted in 003's open questions).

## Interface sketch

Indicative. 005 extends the 004 `Server`; it does not introduce a second server.

```go
// New gains the authority and exclusion options.
func New(sink flow.Sink, authority *ca.Authority, opts ...Option) *Server

func WithExcludedHosts(suffixes ...string) Option
func WithUpstreamTLS(cfg *tls.Config) Option // test seam for origin trust

// ServeHTTP now dispatches CONNECT to the tunnel/intercept path; non-CONNECT
// still forwards as in 004.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request)
```

The 004 forwarding core is refactored into a method taking an explicit scheme,
host, and target so both the cleartext path (absolute URL) and the TLS path
(reconstructed URL) call one implementation.

---

## Acceptance criteria

Unit-level criteria use `httptest.NewTLSServer` origins and clients configured to
trust (or not trust) the proxy CA via `RootCAs`.

1. **CONNECT handshake.** A raw `CONNECT host:port` receives
   `HTTP/1.1 200 Connection Established` and nothing more before the tunnel.
2. **Interception.** With a client trusting the proxy CA, a request tunnelled
   through CONNECT reaches the origin and the response is relayed; the emitted
   flow has `Scheme:"https"`, `Intercepted:true`, and the decrypted method, path,
   status, and body.
3. **Pipelined ClientHello.** A client that writes its `ClientHello` immediately
   after the CONNECT line still completes the handshake — the buffered bytes are
   not lost. (Drive by writing CONNECT and handshake back-to-back on one conn.)
4. **No-SNI / IP CONNECT.** `CONNECT 127.0.0.1:port` with no SNI intercepts using
   a leaf whose SAN carries the IP (ties to 002), and the handshake succeeds.
5. **Static exclusion → tunnel.** A host configured as excluded is tunnelled: the
   client completes TLS against the **origin's** real certificate (not our leaf),
   and the flow is `Intercepted:false`.
6. **Learned exclusion.** A client that does not trust our CA (standing in for a
   pinned app) fails the first interception; a second CONNECT to the same host is
   tunnelled and succeeds against the origin cert. Two flows, both
   `Intercepted:false`, the first carrying an `Error`.
7. **Keep-alive.** Two requests over one intercepted TLS connection yield two
   distinct flows.
8. **Upstream verification failure.** Intercepting a host whose origin cert does
   not validate produces an errored flow and a client-visible failure, not a
   panic.
9. **Concurrency.** Concurrent CONNECTs (mixed intercept and tunnel) under
   `-race` are clean and emit one flow each.
10. `go test ./internal/proxy -race` passes; `go vet` clean; `gofmt -l` silent.

### The criteria that actually matter (manual, human-run)

These are the deferred end-to-end proofs from 002 and 004. Nothing above proves
Apple's rules are satisfied — only these do.

11. **curl end-to-end.**
    ```bash
    curl -x 127.0.0.1:8888 --cacert ~/.proxysim/ca.crt https://example.com -o /dev/null -sv
    ```
    completes with `HTTP/1.1 200` and no TLS verification error.
12. **Booted simulator.** With the CA installed (`xcrun simctl keychain booted
    add-root-cert`), a booted simulator loads an HTTPS page through the proxy, and
    a known pinned app falls back to a tunnel and keeps working.

Criteria 11–12 require the listener wired in `main.go`; they are the first tests
that need a human at the machine.

---

## Open questions

- **Where the intercept loop lives.** A single-conn `http.Serve` over a
  one-shot listener is the least code and gets keep-alive and request parsing for
  free; a manual `http.ReadRequest` loop is more explicit but re-implements what
  `net/http` already does. Lean `http.Serve`; revisit only if it fights the
  hijack/tunnel model.
- **Upstream connection reuse across CONNECTs.** Each intercepted request could
  share the 004 `Transport`'s pool keyed by host, rather than dialing per
  tunnel. Probably yes, but confirm it does not tangle with per-tunnel TLS state.
  Defer until measured.
- **HTTP/2.** Forcing http/1.1 loses multiplexing fidelity for apps that use h2.
  A real want eventually; large enough to be its own spec. Explicitly out here.
- **Tunnel byte counts.** Cheap to add to `Flow` and genuinely useful for
  spotting chatty pinned hosts. Deferred to the additive-field decision in 003.
- **Persisting learned exclusions** across runs so the first connection to a
  known-pinned host is never broken. Attractive, but risks staleness (an app
  drops pinning after an update). Leave in-memory for now.
