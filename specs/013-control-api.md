# Spec 013 — Control API and Parent-Death Detection

**Status:** proposed
**Depends on:** 003 (flow model), 004/005 (forwarding, CONNECT), 007 (simctl trust),
009/011 (origin filter), 012 (Xcode integration — shares the data directory)

## Motivation

proxysim is grown-up enough to be driven by something other than a terminal. A
menu bar app (Bento) wants to launch it, show whether it is healthy, display a
couple of live figures, ask it to re-read the CA after `simctl erase`, and shut it
down cleanly. Today the only channels are flags at launch, SIGINT, and the web UI
— and the web UI is the wrong answer for a host app, because two live control
surfaces over one process can disagree about state.

So: a small HTTP control API on a Unix socket, and a way for proxysim to notice
its parent is gone.

### Why a Unix socket and not a TCP port

The API can shut the process down and re-trust a CA. A loopback TCP port is
reachable by every process on the machine with no way to distinguish them. A Unix
socket in a `0700` directory, itself `0600`, is restricted by the filesystem — the
same reasoning that already governs the CA private key (CLAUDE.md). It also costs
no port, and cannot be mistaken for the proxy port.

### Why HTTP on it, when spec 012's control socket is line-JSON

Spec 012's socket carries exactly one message shape in one direction. This one
has four operations, two verbs, needs status codes to say "no", and is consumed by
a client in another language. HTTP is what `net/http` gives us for free on any
`net.Listener`, and the alternative — hand-rolled framing plus a hand-rolled error
convention — is strictly more code for strictly less.

**The two sockets stay separate.** Spec 012's `control.sock` lives in the CA
directory and is bound only during an Xcode session; this one is at a path the
caller names. Reusing one path for two protocols would mean a retarget message
arriving at an HTTP parser, which fails in a way nobody would enjoy debugging.

## Non-negotiable: the terminal behaviour does not change

proxysim is a standalone tool first. That is not a nicety, it is the constraint
this spec is most likely to violate, so it is stated as a requirement:

- Both new flags default to off. With neither passed, the process must be
  byte-identical in behaviour to before this spec.
- No existing flag changes name, default, or meaning.
- Parent-death detection is **opt-in**. A terminal proxysim's parent is the shell;
  making it exit when the shell dies would change `nohup`, `&`, and
  `ssh`-disconnect behaviour for everyone, to serve one caller.

## Flags

| Flag | Default | Meaning |
|---|---|---|
| `-control-socket` | `""` | Path to a Unix socket to serve the control API on. Empty disables it entirely — no socket is created. |
| `-exit-with-parent` | `false` | Exit cleanly when the parent process at startup is gone. |

Both are independent. Bento passes both; either is usable alone.

## Endpoints

All responses are JSON except where noted. Unknown paths are `404`. A wrong method
is `405` with an `Allow` header — a caller that POSTs to `/status` has a bug, and
silently treating it as a GET hides it.

### `GET /status`

The UI contract. Field names are stable; a field may be added but never renamed.

```json
{
  "port": 8888,
  "uptimeSeconds": 125.4,
  "activeConnections": 3,
  "totalFlows": 42,
  "scope": "iOS Simulator"
}
```

- `activeConnections` — in-flight forwarded requests plus open CONNECT tunnels.
- `scope` — a human-readable rendering of the origin filter currently in force, so
  the host app can display what proxysim is *actually* doing rather than what it
  was asked to do. Read live, so a spec-011 UI change is reflected.

### `GET /stats`

The diagnostic dump: every counter, deliberately not a stable contract. Adding a
counter here must not require a client change, which is why `/status` exists
separately and stays small.

```json
{
  "uptimeSeconds": 125.4,
  "activeRequests": 1,
  "activeTunnels": 2,
  "totalRequests": 40,
  "totalTunnels": 12,
  "interceptedFlows": 38,
  "tunnelledFlows": 4,
  "flowsEmitted": 42
}
```

### `POST /reload`

Re-reads `ca.crt`/`ca.key` from the CA directory and re-runs the simulator trust
install. This is the one operation that is useful without a restart:

- After `simctl erase`, trust is gone and every HTTPS request in the simulator
  fails. Today the fix is to restart proxysim; there is no reason for that.
- After deleting the CA directory to regenerate a CA.

**It does not reload flags.** Port, scope, and exclusions are read at launch, and
pretending otherwise would be a lie with a 200 status code. A caller that wants
those relaunches.

Trust installation is best-effort, exactly as at startup (spec 010): no booted
simulator is not an error, because the proxy is still perfectly useful for `curl`.
So a successful CA reload with a failed trust install returns `200` with a
`trustError` field, rather than a `500` that suggests nothing worked.

```json
{ "reloaded": true, "fingerprint": "AB:CD:…", "trustError": "no booted device" }
```

A CA that cannot be read at all is `500` — that one genuinely did not work.

### `POST /shutdown`

Returns `202` immediately, then triggers the same graceful shutdown path as
SIGINT. Answering before shutting down is deliberate: the caller's connection is
served by the server it is asking to stop, so shutting down first would drop the
response and leave the caller unable to tell success from a broken socket.

Graceful matters more here than anywhere else, because that path is what restores
the macOS system proxy (spec 010). A shutdown that skipped it would leave the
user's Mac pointed at a dead listener — which presents as a broken network, not as
a proxy that stopped.

## Counters

`internal/proxy` gains atomic counters and a `Stats()` accessor. Additive: no
existing behaviour changes, and everything reachable from the request path stays
race-clean (`-race` is not optional, per CLAUDE.md).

`activeConnections` is incremented on entry to the request path and decremented on
exit via `defer`, so an early return or a panic in a handler cannot leak a count —
a counter that only ever grows is worse than no counter, because it looks like
real data.

## Parent-death detection

On macOS there is no `PR_SET_PDEATHSIG`. The portable, dependency-free signal is
that an orphaned process is reparented, so `getppid()` stops matching what it was
at startup.

- Capture `os.Getppid()` once at startup.
- Poll every second. When it differs, the original parent is gone: trigger the
  same graceful shutdown as SIGINT.

Compared with a kqueue `NOTE_EXIT` watch, this is ~15 lines instead of ~60, needs
no build tags, and is testable by injecting the `getppid` function. A one-second
detection delay is irrelevant for a shutdown.

Guard: if the captured ppid is already `1`, the process was started orphaned
(launchd, `nohup`, a double fork). Watching is then pointless — there is no parent
to outlive — so it is skipped rather than firing immediately.

### Why this matters

Without it, killing Bento leaves proxysim holding port 8888 **and** possibly the
macOS system proxy takeover. The user sees a broken network with nothing in the
menu bar to explain it, and the only fix is finding the process by hand.

## Acceptance criteria

1. `go build ./...`, `go vet ./...`, `gofmt -l .` silent, `go test ./... -race`
   passes.
2. With neither new flag, behaviour is unchanged and no socket file is created.
3. `-control-socket` serves all four endpoints; `404`/`405` as specified.
4. A stale socket file from a dead instance does not prevent binding.
5. The socket file is `0600` and is removed on shutdown.
6. `POST /shutdown` returns `202` and the process exits gracefully, running the
   system-proxy restore.
7. `POST /reload` re-reads the CA; a trust failure is `200` with `trustError`.
8. `-exit-with-parent` exits when the original parent is gone, and is a no-op when
   the captured ppid is `1`.
9. `curl --unix-socket <path> http://localhost/status` works — no bespoke client
   needed to use this.
