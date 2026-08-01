# proxysim

A local HTTP/HTTPS forward proxy and traffic sniffer for the iOS Simulator.
Charles/Proxyman in miniature, built to be understood rather than to be complete.

**Status:** specs 001–008 implemented. Proxy, TLS interception, body decoding,
`simctl` trust automation, and a live web UI all work end-to-end.

## Quick start

```bash
go build -o proxysim .

# first run generates the CA under -ca-dir; leave it running (Ctrl-C to stop)
./proxysim -port 8888 -ui -ui-port 8889 -ca-dir ~/.proxysim
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
| `-trust` | `false` | install the CA into the booted simulator's trust store, then exit |
| `-trust-set` | — | simulator set to target (e.g. `previews` for Xcode Previews) |
| `-device` | — | UDID of the booted simulator, when several are booted |

## The web UI

`-ui` serves a two-pane app on `127.0.0.1:8889`:

- Rows appear live as requests complete (Server-Sent Events). A `● live` dot
  shows the stream is connected; a filter box narrows by host/path/method/status;
  **Clear** empties the list.
- Click a row for headers plus **decoded** bodies — gzip/deflate/brotli
  decompressed, JSON pretty-printed, binary summarized. The raw (on-wire) size is
  reported alongside the decoded view.
- Loopback only, no auth: it serves decrypted traffic, so it is never reachable
  from the network. This is not configurable — see `CLAUDE.md`.

## Testing against the iOS Simulator

The Simulator routes through the **host Mac's** network stack, so it inherits
macOS system proxy settings. Two steps: trust the CA, and point the system proxy
at us.

```bash
# 1) trust the CA in the booted simulator (folded into the tool)
./proxysim -trust -ca-dir ~/.proxysim
#    SwiftUI Previews uses a separate simulator set:
./proxysim -trust -trust-set previews
#    several sims booted? it lists them; pick one:
./proxysim -trust -device <UDID>

# 2) route the Mac's web traffic through the proxy (manual for now)
networksetup -setsecurewebproxy Wi-Fi 127.0.0.1 8888
networksetup -setwebproxy       Wi-Fi 127.0.0.1 8888
```

Run your app in the Simulator and watch traffic in the UI. **Revert the system
proxy when done**, or all Mac traffic keeps routing through proxysim:

```bash
networksetup -setsecurewebproxystate Wi-Fi off
networksetup -setwebproxystate       Wi-Fi off
```

Notes:

- `-trust` needs Xcode's command-line tools and a **booted** simulator; it prints
  an actionable error otherwise (no Xcode, no booted device, ambiguous choice).
- Trust does not survive `simctl erase`. Re-run `./proxysim -trust` after
  resetting a device.
- System-proxy automation is deliberately **not** in the tool yet: it reroutes
  *all* Mac traffic and must be reliably reverted, so it is left manual pending
  its own spec with a restore-on-exit guarantee.
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
| 009 | Origin filtering (simulator / per-app) | spec written |

Each spec carries a human-run manual criterion (real simulator, real browser)
that the automated tests do not cover; those are yours to exercise via the steps
above.

## Not a general-purpose tool

Loopback only, one developer, one machine. It decrypts TLS, which is only
acceptable because it is local and explicit. See the security section of
`CLAUDE.md` before changing anything about how it binds or where the CA key
lives.
