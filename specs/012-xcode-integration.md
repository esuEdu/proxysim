# 012 — Xcode Build Integration (armed auto-launch, auto-target, guaranteed teardown)

**Status:** implemented. Automated criteria 1 (disarmed no-op), 3–8 pass in
`internal/xcode` and `internal/ui`; the manual criteria 9–12 (real Xcode scheme +
Simulator, teardown on every run outcome, crash recovery) are for a human to run.
Criterion 2's scope-forcing is `main.go` wiring exercised by the manual path.
**Package:** `internal/xcode` (new; armed-flag + control/retarget + watchdog),
reuses `internal/sysproxy` (010), `internal/sim` (007), `internal/origin`
(009/011); wired through `main.go`. Ships a double-clickable `.app` wrapper.
**Depends on:** 010 (system-proxy takeover + the restore-is-a-promise machinery —
this feature *is* that promise applied to the Xcode run cycle), 009/011 (origin
filtering — the auto-target scopes the filter), 007 (CA auto-trust into the sim).

---

## The constraint discovered before writing this (do not re-litigate)

The original want was a proxysim panel *docked inside Xcode's window*, appearing at
the bottom while running. **That is not possible with any supported, stable API**,
and this is expensive to rediscover:

- Xcode's only public extension type is a **Source Editor Extension** — sandboxed,
  operates on editor *text* only. It cannot host a panel, a background network
  process, or any custom view.
- There is **no public API to add a panel/tab/drawer** to Xcode's UI.
- In-process plugin injection (the old Alcatraz / `.xcplugin` era hooking
  `IDEKit`/`DVTKit`) has been **dead since Xcode 8** — library validation + code
  signing block loading unsigned code into Xcode, and it breaks every update.

So this seam delivers the *behaviour* — auto-start on build, auto-target, on/off,
guaranteed teardown — through **scheme Run pre-actions launching a standalone
app**, not through anything living inside Xcode. The proxysim UI stays its own
window (008/011), launched automatically and pre-scoped.

## Intent

Remove the last manual friction: starting proxysim and pointing it at the right
app + simulator. Today that is a terminal command plus (before 010) proxy/trust
setup. After 012:

- proxysim has an **armed** switch, **default off**. While disarmed, building in
  Xcode does *nothing* — the pre-action is a fast no-op.
- While **armed**, building the app launches proxysim already scoped to *this*
  app bundle id on *this* simulator device, with 010's system-proxy takeover and
  007's CA trust applied. No terminal, no device/app picker.
- When the run ends — cleanly, cancelled, failed, or crashed — the system proxy
  is restored **100% of the time**. This is the load-bearing requirement.

This is a **host-lifecycle + activation** seam. It does not touch the engine, the
CA format, the loopback binding, or how interception/filtering work.

## Scope

In:
- An **armed flag** persisted at `~/.proxysim/armed` (presence = armed; 0600),
  toggled from the proxysim UI ("Auto-start when Xcode builds") so it can be set
  without the app running the next time.
- A new `-xcode-run` mode: reads `PRODUCT_BUNDLE_IDENTIFIER` and
  `TARGET_DEVICE_IDENTIFIER` from the environment, checks the armed flag, and
  either starts proxysim scoped to that app+device or **retargets an already
  running instance** (single instance, no duplicate launches).
- A **scheme pre-action snippet** proxysim can print (documented, pasted once per
  scheme into Product → Scheme → Edit Scheme → Run → Pre-actions). Detached,
  non-blocking, self-contained (absolute path to the app binary).
- A **teardown watchdog**: proxysim watches the target app on the target device
  and, when it terminates for a grace period, restores (via 010) and exits — the
  primary teardown path, since it does not depend on Xcode.
- A **`.app` bundle** wrapper so launch is a double-click, never a command.
- Reuse: 010 `Apply`/`Recover`/`restore`, 007 CA trust, 009/011 origin scoping to
  the resolved app+device.

Out:
- **Auto-editing the `.xcscheme`.** The pre-action is pasted once by hand;
  programmatic scheme installation is a possible later convenience, not this spec.
- **A docked in-Xcode panel** — impossible, see above.
- **Physical devices.** They do not route through the Mac system proxy the way the
  Simulator does; that is a different seam.
- **A menu-bar app.** Nice-to-have for the armed toggle; noted, not required here.
- Any change to interception, decoding, the CA, or the `127.0.0.1` binding.

---

## Non-negotiable: teardown never depends on the Xcode post-action

The sharp edge is a stale system proxy — it breaks unrelated apps *outside* the
tool. Xcode's scheme **post-actions do not run** when a run is cancelled, the
build fails, or the app crashes. Therefore post-actions are **not** a teardown
mechanism here (they may be used only as a best-effort *nudge*). Restore is
guaranteed by layering, most-reliable last:

1. **App-lifecycle watchdog** — when the debugged app is gone from the target
   device for the grace period, proxysim restores and exits on its own.
2. **010 snapshot + next-launch `Recover`** — if proxysim itself is `SIGKILL`ed or
   the Mac loses power, the on-disk snapshot is re-applied the next time proxysim
   starts. This is the same net 010 already ships; 012 must not weaken it.
3. **Signal + deferred restore** — `SIGINT`/`SIGTERM` and normal exit both restore,
   idempotently (010 already guarantees double-restore is harmless).
4. **UI quit** — an explicit stop that restores before exiting.

Stated as an invariant: **there is no run outcome after which the system proxy is
left pointing at a dead proxysim without recovery on the next launch.**

## Design decisions

The expensive-to-reverse ones.

**Armed is the single master switch, default off, persisted on disk.** Not a
build setting, not per-scheme state. One flag the pre-action reads; disarmed makes
every build a no-op. This is exactly "sometimes I don't want it to start on build"
without editing schemes each time.

**Armed implies active capture.** Because the user rejected the "process on,
interception off" split: arming *is* the on switch. An armed build launches
proxysim capturing, scoped to the app+device. Disarm to stop future builds from
launching it.

**Retarget, never duplicate.** A second build must not spawn a second proxysim. If
an instance is already running, `-xcode-run` hands it the new app+device over a
local control channel (a unix socket under `~/.proxysim/`, 0700 dir) and exits.
Only if none is running does it start one.

**Target the exact app + device from Xcode's environment.** Use
`PRODUCT_BUNDLE_IDENTIFIER` and `TARGET_DEVICE_IDENTIFIER`, not "the booted sim" —
so a multi-simulator setup scopes to the one Xcode actually ran. Missing/empty
values degrade gracefully (fall back to booted sim, or refuse with a clear log).

**Ship a `.app`, keep the binary.** Double-clickable satisfies "no command"; the
plain binary + flags still works for `curl`/CI and stays the tested surface.

**`networksetup`/`simctl` only, as in 007/010.** No private frameworks; the
control channel and watchdog stay on loopback / `~/.proxysim` sockets.

---

## Why this is fiddly

- **Pre-actions run with a minimal environment and discard stdout/stderr and exit
  code.** The snippet must be non-blocking (launch detached, return immediately),
  reference the app by absolute path, and never stall the build if proxysim is
  slow or missing.
- **Post-actions are unreliable** (cancel/fail/crash skip them) — hence the
  watchdog is primary, not the post-action. Easy to get backwards.
- **Detecting "the app stopped" on the Simulator is indirect** — poll `simctl`
  for the app's running state / pid on the target device, with a grace period so a
  quick relaunch (rebuild) does not thrash a teardown.
- **Single-instance retargeting needs IPC** — a small local control channel
  (unix socket in `~/.proxysim/`, dir 0700) so build N+1 retargets instead of
  spawning. Must be race-clean and survive the first instance dying.
- **Env var availability differs between Build and Run pre-actions.**
  `TARGET_DEVICE_IDENTIFIER` is present for Run; absence must be tolerated.
- **The armed flag must be readable when proxysim is not running** (the pre-action
  checks it cold), so it is a file, not in-process state.

## Surface

Indicative, not binding.

```go
// Package xcode activates proxysim from an Xcode scheme Run pre-action: it gates
// on a persisted "armed" flag, resolves the target app+device from the Xcode
// environment, launches-or-retargets a single proxysim instance, and watches the
// debugged app so the session tears down (restoring 010's system-proxy snapshot)
// on any run outcome — never relying on an Xcode post-action.
package xcode

// Armed reports whether auto-launch-on-build is enabled (presence of the flag
// file). SetArmed toggles it; the flag lives on disk so the pre-action can read
// it while proxysim is not running.
func Armed(dir string) (bool, error)
func SetArmed(dir string, on bool) error

// Target is what one Run resolves to: the app bundle id and the simulator device
// the scheme is running, read from PRODUCT_BUNDLE_IDENTIFIER / TARGET_DEVICE_IDENTIFIER.
type Target struct { BundleID, DeviceUDID string }

// FromEnv resolves the Target from the Xcode pre-action environment, degrading to
// the booted simulator when the device id is absent.
func FromEnv() (Target, error)

// LaunchOrRetarget starts a proxysim scoped to t, or, if one is already running,
// sends t over the control socket and returns without spawning a second.
func LaunchOrRetarget(dir string, t Target) error

// Watch blocks until the target app has been absent from the device for the grace
// period, then returns so the caller can restore and exit. It is the primary
// teardown trigger.
func Watch(ctx context.Context, t Target, grace time.Duration) error
```

`main.go` gains `-xcode-run` (pre-action entrypoint: `Recover` → gate on `Armed` →
`FromEnv` → `LaunchOrRetarget`; the running instance scopes the origin filter to
`t`, applies 010, trusts the CA, and starts `Watch` on a goroutine that restores
and exits when it returns). A `-print-xcode-hook` prints the pre-action snippet to
paste. The UI (011) gains the "Auto-start when Xcode builds" toggle backed by
`SetArmed`.

The pasted pre-action snippet is, in shape:

```sh
# proxysim — auto-start when armed (no-op otherwise). Non-blocking.
/Applications/proxysim.app/Contents/MacOS/proxysim -xcode-run &
```

---

## Acceptance criteria

`simctl`/`networksetup` are faked via the injected `Runner` (007/010) for all but
the manual criteria; no automated criterion touches the real system config, and
none requires Xcode.

1. **Disarmed is a no-op.** With no armed flag, `-xcode-run` (fake env set) exits 0
   without launching, without touching the system proxy, and without a control
   socket — asserted from recorded argv (none) and no snapshot written.
2. **Armed launches scoped.** With the armed flag present and a fake env
   (`PRODUCT_BUNDLE_IDENTIFIER`, `TARGET_DEVICE_IDENTIFIER`), the instance scopes
   the origin filter to that app+device and applies 010's `Apply` for the port —
   asserted from the origin config and recorded `networksetup` argv.
3. **Retarget, no duplicate.** With one instance already running (control socket
   present), a second `-xcode-run` sends the new `Target` over the socket and does
   **not** start a second server or re-`Apply`; the first instance's scope updates.
4. **`FromEnv` resolves + degrades.** Full env yields the exact `Target`; a missing
   `TARGET_DEVICE_IDENTIFIER` falls back to the booted device (faked) rather than
   erroring; a missing bundle id is a typed error.
5. **Watchdog triggers teardown.** Given a faked device where the target app is
   present then absent past the grace period, `Watch` returns and the caller's
   restore runs exactly once (idempotent with the deferred/ signal restore).
6. **Teardown does not depend on the post-action.** A run that ends without any
   post-action (simulated: no stop signal, app just disappears) still restores via
   the watchdog; and a killed instance leaves a 010 snapshot that a fresh
   `Recover` re-applies and clears.
7. **Armed flag round-trips on disk.** `SetArmed(true)` then `Armed()` (fresh, no
   process state) reports armed; `SetArmed(false)` clears it; the file is 0600.
8. **Snippet prints.** `-print-xcode-hook` writes the non-blocking, absolute-path
   pre-action snippet to stdout.

Manual (human-run against real Xcode + Simulator):

9. Paste the snippet into a scheme's Run pre-actions once. With proxysim
   **disarmed**, Run the app → no proxysim launches, networking untouched.
10. Toggle **armed** in the UI, Run again → proxysim launches, scoped to that app
    on that simulator, capturing its traffic; other apps/hosts untouched.
11. Stop the run (and separately: cancel a build, force-crash the app) → the
    system proxy is restored every time; verify in System Settings → Network →
    Proxies.
12. `SIGKILL` proxysim mid-run, then launch it again → 010 `Recover` restores the
    prior proxy state on startup.
```
