# 010 — Frictionless Launch (system proxy + CA auto-trust)

**Status:** implemented. Automated criteria 1–10 pass; the manual criteria 11–12
(real simulator + System Settings, crash recovery) are for a human to run.
**Package:** `internal/sysproxy` (new), reuses `internal/sim` (007); wired through
`main.go`
**Depends on:** 007 (simctl trust — the auto-trust step reuses it), 009 (origin
filtering — auto-proxy implies `-only-sim`)

---

## Intent

Today, going from `proxysim` running to actually seeing Simulator traffic is three
manual steps the user does by hand every session:

1. trust the CA in the booted simulator (`xcrun simctl … add-root-cert`),
2. open System Settings → Network → Proxies and point the active service at
   `127.0.0.1:<port>` (Web + Secure Web),
3. remember to turn that proxy **off** when done — a proxy left pointing at a
   dead `proxysim` breaks all networking until noticed.

Step 3 is the sharp edge: forget it and the Mac silently loses the web until the
user rediscovers the stale setting. 010 makes `proxysim` own this lifecycle so the
one command both *arms* and *disarms* the environment. The tool that turned the
system proxy on is responsible for turning it back off — including after a clean
`Ctrl-C`, and, best-effort, after a crash on the next run.

This is a **host-configuration and lifecycle** seam. It does not touch the engine,
the CA format, or the loopback binding.

## Scope

In:
- A `sysproxy` package that **snapshots** the current macOS proxy configuration of
  the active network service, **sets** it to `127.0.0.1:<port>` (Web + Secure
  Web), and **restores** the exact prior state on shutdown.
- Persisting the snapshot to disk (`~/.proxysim/sysproxy.json`) so a run that died
  without restoring can be recovered by the next run.
- Auto-trusting the CA into the booted simulator on startup (idempotent; reuses
  `sim.InstallRootCert`), best-effort and non-fatal.
- Coupling: enabling the system-proxy takeover **implies `-only-sim`** (decision
  below), so the user's other Mac apps are tunnelled untouched, never decrypted.
- A stub on non-Darwin so the build stays green (the project is macOS-only; this
  keeps `go build` honest elsewhere).

Out:
- Per-app / VPN / PAC-file proxy configs, and setting the proxy on *every* network
  service at once. 010 targets the **one active (primary) service** — the one
  carrying the default route — and restores exactly that one.
- A GUI toggle for the proxy. Enabling is a flag; runtime on/off from the UI is a
  later want (it composes with 011's control surface, noted there).
- Elevated-privilege escalation. If `networksetup` refuses without admin rights we
  report it clearly and continue serving without the takeover — we never prompt
  for or cache a password.
- Any change to how interception or the filter works (that is 009/011).

---

## Non-negotiable: restore is a promise, not a best effort on the happy path

A stale system proxy is the one failure mode of this feature that hurts the user
*outside* the tool — it breaks unrelated apps. So restoration is treated as
critical:

- The prior configuration is captured **before** any change and written to
  `~/.proxysim/sysproxy.json` (0600) immediately, so it survives a crash.
- Restore runs on `SIGINT`/`SIGTERM` via the existing signal path, and also via a
  deferred restore so a normal return path cannot skip it.
- On startup, if a snapshot file already exists, a **previous run did not clean
  up**: restore from it first (re-applying the captured prior state), then proceed.
  The snapshot is deleted only after a successful restore.
- `SIGKILL` and power loss cannot be caught; the on-disk snapshot + startup
  recovery is the safety net for exactly those cases.

This is the same discipline the CA files get (0600, never lost), applied to the
one piece of *system* state we mutate.

## Design decisions

The expensive-to-reverse ones.

**Auto-proxy implies `-only-sim`.** The macOS system proxy is global: point it at
us and Safari, update daemons, and every host app route through the proxy too.
Decrypting all of that is exactly the noise 009 exists to remove, and it is a
privacy regression. So turning on the takeover forces `-only-sim` semantics — host
connections are resolved as "not simulator" and blind-tunnelled untouched, never
decrypted or shown. A user who genuinely wants host-wide capture sets the system
proxy themselves (the manual path still works); the *automated* path is always
narrowed. Stated as an invariant: **proxysim never auto-points the whole system at
itself while also decrypting the whole system.**

**Target the primary service only.** A Mac can have many network services (Wi-Fi,
Ethernet, iPhone USB, VPNs). Setting the proxy on all of them is invasive and
error-prone to restore. We resolve the **primary** service — the one backing the
default route — set only that, snapshot only that, restore only that. If the
primary service changes while running (user switches Wi-Fi→Ethernet), we do not
chase it; the snapshot records what we touched and we restore precisely that. A
`-verbose` line names the service we took over so the choice is visible.

**Snapshot the exact prior state, restore verbatim.** We do not assume the prior
proxy was "off". We capture enabled-state + host + port for both Web and Secure
Web proxies and restore all of it, so a user who *already* had a proxy configured
gets it back unchanged.

**Auto-trust is best-effort, never fatal.** No booted simulator, or no Xcode
toolchain, must not stop `proxysim` from serving — the proxy is still useful for
`curl`, physical-device-less testing, and pre-boot startup. Auto-trust logs what
it did (or why it skipped) and serving continues regardless. It reuses
`sim.InstallRootCert` unchanged; installing an already-trusted CA is idempotent
(007).

**`networksetup`, not a private API.** Same posture as `sim`: shell out to the
documented Apple tool rather than poke `SystemConfiguration` via cgo. `networksetup`
is stable, scriptable, and reversible, and keeps this package testable behind an
injected `Runner` exactly like `sim.Runner`.

---

## Why this is fiddly

- **Restore ordering vs. graceful shutdown.** The existing `run` shuts the servers
  down on signal; the proxy restore must happen on that same path *and* be
  idempotent with the deferred restore, so a double-restore (signal + defer) is
  harmless. Capture-then-defer-restore, guarded by a "already restored" flag.
- **`networksetup` may require admin rights.** On some managed Macs, setting proxy
  state prompts for or requires admin credentials. We must detect the
  permission-denied failure and degrade gracefully: report "could not set the
  system proxy automatically (admin rights required); set it manually to
  127.0.0.1:<port>" and keep serving. Never prompt, never sudo.
- **Finding the primary service is indirect.** `networksetup
  -listnetworkserviceorder` maps hardware ports to services and marks order;
  `route -n get default` gives the primary *interface* (e.g. `en0`), which maps to
  a service name. The mapping must be resolved, not guessed, and must tolerate a
  machine with no default route (offline): then there is nothing to take over —
  say so and continue.
- **Service names contain spaces** ("iPhone USB", "USB 10/100/1000 LAN"). Every
  `networksetup` argv must pass the service as a single argument; never join into
  a shell string.
- **A disabled proxy still has a stored host/port.** `-getsecurewebproxy` returns
  `Enabled: No` plus whatever host/port was last set. Snapshot all three fields;
  restoring only the enabled flag would silently change a host the user had saved.

## Surface

Indicative, not binding.

```go
// Package sysproxy snapshots, sets, and restores the macOS system HTTP/HTTPS
// proxy for the primary network service, so proxysim can point the Simulator at
// itself on startup and — the part that matters — put the setting back on exit.
// macOS-only; other platforms get a stub whose Apply is a no-op. Shells out to
// networksetup behind an injected Runner (mirrors internal/sim).
package sysproxy

// Config is the captured proxy state of one network service: the enabled flag and
// host/port for both the Web (HTTP) and Secure Web (HTTPS) proxies.
type Config struct { /* Service string; Web, Secure Setting */ }

// Manager owns the takeover lifecycle for one session.
type Manager struct { /* runner, snapshot path, restored flag */ }

// Apply snapshots the primary service's current proxy config, persists it to
// snapshotPath (0600), and points Web+Secure Web at 127.0.0.1:port. Returns a
// restore func that is safe to call more than once. A permission error is
// returned wrapped so main can degrade to "set it yourself" rather than fail.
func (m *Manager) Apply(ctx context.Context, port int) (restore func() error, err error)

// Recover restores and clears a snapshot left by a previous run that did not clean
// up. Called once at startup, before Apply. A missing snapshot is a no-op.
func (m *Manager) Recover(ctx context.Context) error
```

`main.go` gains `-system-proxy` (enable the takeover; implies `-only-sim`) and a
startup auto-trust step (on by default when a sim is booted; `-no-trust` opts out).
On enable it: `Recover` → `Apply` → defer `restore` → wire `restore` into the
signal shutdown, and forces the origin filter to at least `-only-sim`.

---

## Acceptance criteria

`networksetup`/`simctl` are faked via the injected `Runner` for all but the manual
criteria; no automated criterion touches the real system config.

1. **Snapshot + set + restore round-trips.** Given a fake runner reporting a prior
   config (say Secure Web enabled → some host:port), `Apply` issues the
   `-setsecurewebproxy`/`-setwebproxy` calls targeting `127.0.0.1:<port>`, and the
   returned `restore` re-issues exactly the captured prior values — asserted from
   the recorded argv.
2. **Restore is idempotent.** Calling `restore` twice issues the restore commands
   at most once; the signal path and the deferred call cannot double-apply.
3. **Snapshot persists and recovers.** `Apply` writes `sysproxy.json` (0600);
   `Recover` on a fresh Manager reads it, restores the captured state, and deletes
   the file. A missing file makes `Recover` a no-op.
4. **Primary-service resolution.** Given faked `route get default` + service-order
   output, the Manager targets the correct service name (including one containing a
   space), passed as a single argv element.
5. **Permission failure degrades.** A runner returning a permission-denied error
   from `-setsecurewebproxy` makes `Apply` return a typed/wrapped error that
   `main` reports as "set it manually" — and `run` still serves (no `Fatal`).
6. **Offline / no default route.** With no primary service resolvable, `Apply` is a
   no-op that reports "nothing to take over" and serving continues.
7. **Implies only-sim.** With `-system-proxy`, the constructed origin filter is
   Active and simulator-only even if `-only-sim`/`-app` were not passed (asserted
   at the wiring level).
8. **Auto-trust is non-fatal.** Startup auto-trust with a fake `sim.Runner`
   reporting "no booted device" logs a skip and does not stop serving; a success
   path installs the cert once.
9. **Non-Darwin builds.** The stub compiles and `Apply` is a no-op returning a
   restore that does nothing; `go build` on a non-Darwin tag succeeds.
10. `go test ./internal/sysproxy -race` passes; `go vet` clean; `gofmt -l` silent.

### Manual criteria (human-run)

11. `proxysim -system-proxy -ui` with a booted simulator: the CA is trusted
    automatically, the macOS system proxy flips to `127.0.0.1:<port>`, Simulator
    traffic appears in the UI with no manual System Settings step, and on `Ctrl-C`
    the system proxy returns to its exact prior state (verify in System Settings →
    Network → Proxies).
12. **Crash recovery.** `kill -9` the process while it holds the proxy, confirm the
    Mac's proxy is left pointing at the dead port, then start `proxysim
    -system-proxy` again and confirm the *next* startup restores the prior state
    from the on-disk snapshot before re-applying.

---

## Open questions

- **Which fields Safari/Simulator actually honour.** The Simulator inherits the
  system proxy; we set both Web and Secure Web to be safe. If the Simulator turns
  out to ignore the plain Web proxy, dropping it is a non-breaking simplification.
- **Multiple concurrent proxysim instances.** Two takeovers would fight over the
  one system proxy and over `sysproxy.json`. Out of scope for v1 (single-user,
  single-instance is the tool's premise); if it ever matters, the snapshot file
  would need per-instance naming and a guard.
- **Runtime on/off from the UI.** A "capture on/off" button that toggles the
  takeover live is a natural pairing with 011's control surface. Deferred; the
  Manager's `Apply`/`restore` shape already supports being driven at runtime.
- **VPN / split-tunnel interaction.** If a VPN owns the default route, we take over
  that service; whether that is desirable depends on the user's setup. We restore
  precisely what we touched, so the blast radius is bounded, but this is untested
  against corporate VPN configs.
