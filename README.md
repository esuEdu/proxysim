# proxysim

A local HTTP/HTTPS forward proxy and traffic sniffer for the iOS Simulator.
Charles/Proxyman in miniature, built to be understood rather than to be complete.

**Status:** specs 001–011 implemented. Proxy, TLS interception, body decoding,
`simctl` trust automation, a live web UI, origin filtering, automatic system-proxy
takeover, and in-UI simulator/app selection all work end-to-end.

## Quick start

```bash
go build -o proxysim .

# first run generates the CA under -ca-dir; leave it running (Ctrl-C to stop)
./proxysim -port 8888 -ui -ui-port 8889 -ca-dir ~/.proxysim
```

For the Simulator, the friendly one-command path trusts the CA, takes over the
macOS system proxy (and restores it on exit), and scopes capture to the simulator:

```bash
./proxysim -system-proxy -ui        # then just open the UI and pick your app
```

Then, in another terminal:

```bash
# plain HTTP — shows up in the console immediately
curl -x 127.0.0.1:8888 http://example.com/ -v

# HTTPS interception — trust the generated CA for this request
curl -x 127.0.0.1:8888 --cacert ~/.proxysim/ca.crt https://example.com/ -v
```

Both return `200`. The HTTPS case proves TLS termination: curl accepts the leaf
proxysim mints on the fly because it chains to your CA and carries the hostname
in its SAN. Open the UI at **http://127.0.0.1:8889** to watch flows arrive live
and click one for its headers and decoded bodies.

## Flags

| Flag | Default | Purpose |
|---|---|---|
| `-port` | `8888` | proxy listen port (always bound to `127.0.0.1`) |
| `-ca-dir` | `~/.proxysim` | directory holding `ca.crt` / `ca.key` |
| `-verbose` | `false` | print full headers + decoded bodies to the console, not just a summary line |
| `-exclude` | — | comma-separated host suffixes to never intercept (blind-tunnel instead) |
| `-ui` | `false` | serve the live web UI (loopback only, separate port) |
| `-ui-port` | `8889` | UI listen port (always bound to `127.0.0.1`) |
| `-ui-history` | `1000` | recent flows the UI keeps for a freshly opened tab |
| `-only-sim` | `false` | intercept only iOS Simulator traffic; tunnel everything else (macOS) |
| `-app` | — | comma-separated app bundle ids to intercept exclusively (implies `-only-sim`) |
| `-system-proxy` | `false` | point the macOS system proxy at proxysim on startup, restore it on exit (implies `-only-sim`) |
| `-no-trust` | `false` | skip auto-trusting the CA in the booted simulator on startup |
| `-trust` | `false` | install the CA into the booted simulator's trust store, then exit |
| `-trust-set` | — | simulator set to target (e.g. `previews` for Xcode Previews) |
| `-device` | — | UDID of the booted simulator, when several are booted |

On startup proxysim auto-trusts the CA in the booted simulator (idempotent,
best-effort — a missing simulator or Xcode just prints a skip line). `-no-trust`
turns that off; `-trust` is the standalone install-and-exit form.

## The web UI

`-ui` serves a two-pane app on `127.0.0.1:8889`:

- Rows appear live as requests complete (Server-Sent Events). A `● live` dot
  shows the stream is connected; a filter box narrows by host/path/method/status;
  **Clear** empties the list.
- Click a row for headers plus **decoded** bodies — gzip/deflate/brotli
  decompressed, JSON pretty-printed, binary summarized. The raw (on-wire) size is
  reported alongside the decoded view.
- A control bar picks the **simulator** and what to **intercept** — *Everything*,
  *All simulator traffic* (`-only-sim`), or one app from a dropdown of the booted
  sim's installed apps. The change applies to the next connection, no restart. It
  reflects whatever `-only-sim`/`-app` you launched with, and any tab stays in
  sync. (The bar appears only when the origin filter is available — i.e. with the
  UI on.)
- Loopback only, no auth: it serves decrypted traffic, so it is never reachable
  from the network. This is not configurable — see `CLAUDE.md`.

## Testing against the iOS Simulator

The Simulator routes through the **host Mac's** network stack, so it inherits
macOS system proxy settings. proxysim automates every step of that setup — and,
crucially, the teardown.

### The easy path

```bash
./proxysim -system-proxy -ui
```

This, on startup: auto-trusts the CA in the booted simulator; points the Mac's
system proxy at proxysim; and — because a global system proxy would drag Safari
and every daemon through us — scopes capture to the simulator (`-only-sim` is
implied). On `Ctrl-C` it **restores the system proxy to exactly its prior state**.
Open the UI, and pick *All simulator traffic* or one app from the control bar's
dropdown — no restart, no bundle-id lookup.

If a previous run was killed (`kill -9`, crash) before restoring, the next
`-system-proxy` startup restores from an on-disk snapshot before re-applying, so a
stale proxy never lingers.

### Choosing what to capture

Everything below the simulator scope is a filter over the **originating process**;
non-matching connections are blind-tunnelled (they still work), just neither
decrypted nor shown. Pick it in the UI, or pin it from the command line:

```bash
./proxysim -system-proxy -ui                          # pick sim/app in the UI (recommended)
./proxysim -system-proxy -ui -only-sim                # whole simulator, fixed
./proxysim -system-proxy -ui -app br.com.bb.InvestimentosBB   # one app, fixed
# → its API calls (api.mov.investimentos.hm.bb.com.br, firebase, appdynamics…) are
#   captured; Safari and every other app are tunnelled + hidden.
```

Per-app matches the app's own `URLSession`/`CFNetwork` traffic, which resolves to
the app process. Two things it does **not** catch: `WKWebView` traffic (it runs in
WebKit's networking process, not your app — use *All simulator traffic* to see
it), and any background session that egresses through a shared daemon. macOS only.

### The manual path (fallback)

If you'd rather drive the system proxy yourself (or you're not on the primary
network service proxysim auto-detects), skip `-system-proxy` and set it by hand:

```bash
networksetup -setsecurewebproxy Wi-Fi 127.0.0.1 8888
networksetup -setwebproxy       Wi-Fi 127.0.0.1 8888
# ... test ...
networksetup -setsecurewebproxystate Wi-Fi off        # revert when done, or Mac
networksetup -setwebproxystate       Wi-Fi off        # traffic keeps routing through us
```

Notes:

- Auto-trust (and standalone `-trust`) need Xcode's command-line tools and a
  **booted** simulator; both print an actionable skip/error otherwise (no Xcode,
  no booted device, ambiguous choice). SwiftUI Previews uses a separate set —
  `-trust -trust-set previews`; several sims booted — `-trust -device <UDID>`.
- Trust does not survive `simctl erase`. Startup auto-trust re-installs it on the
  next run; or run `./proxysim -trust` after resetting a device.
- `-system-proxy` takes over the **primary** network service (the one backing the
  default route). If it can't (no admin rights, offline), it prints a hint and
  keeps serving so you can set the proxy manually.
- A pinned host cannot be intercepted by design. proxysim falls back to a blind
  TCP tunnel and logs it as `[tunnelled, not inspected]` — never failing the
  connection. Add such hosts to `-exclude` to skip the interception attempt.

## Developer commands

```bash
go build ./...
go test ./... -race          # tests run under -race; that is not optional
go vet ./...
gofmt -l .                   # must print nothing
```

## How this repo works

`CLAUDE.md` is the constitution — mission, security rules, tech stack, and the
Apple TLS requirements that make certificate work here non-obvious. It is loaded
into every Claude Code session.

`specs/` holds one spec per architectural seam. Each states intent, scope, and
executable acceptance criteria. A spec is done when its acceptance criteria run
and pass.

| Spec | Seam | Status |
|---|---|---|
| 001 | Root CA lifecycle | done |
| 002 | Per-host leaf minting | done |
| 003 | Flow model — the engine/UI contract | done |
| 004 | Plain HTTP forwarding | done |
| 005 | CONNECT + TLS termination, MITM exclusions | done |
| 006 | Body decoding (gzip/deflate/br, chunked) | done |
| 007 | `simctl` trust automation | done |
| 008 | Desktop UI | done |
| 009 | Origin filtering (simulator / per-app) | done |
| 010 | Frictionless launch (system proxy + CA auto-trust) | done |
| 011 | UI-driven simulator/app origin control | done |

Each spec carries a human-run manual criterion (real simulator, real browser)
that the automated tests do not cover; those are yours to exercise via the steps
above.

## Not a general-purpose tool

Loopback only, one developer, one machine. It decrypts TLS, which is only
acceptable because it is local and explicit. See the security section of
`CLAUDE.md` before changing anything about how it binds or where the CA key
lives.
