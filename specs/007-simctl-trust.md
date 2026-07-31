# 007 — simctl Trust Automation

**Status:** ready to implement
**Package:** `internal/sim` (new), wired through `main.go`
**Depends on:** 001 (the CA whose cert gets installed)

---

## Intent

Before a simulator will accept our intercepted TLS, the CA has to be in its trust
store. Today that is a manual `xcrun simctl keychain booted add-root-cert …` the
user has to remember, get the path right for, and re-run after every device
reset. 007 folds that into the tool: one command that installs the local CA into
the booted simulator and tells the user plainly what happened.

This is a convenience seam, not part of the capture engine. It shells out to
`simctl`; it does not reimplement anything Apple provides.

## Scope

In: a `-trust` action that installs `ca.crt` into the booted simulator's trust
store (and, on request, the Xcode Previews simulator set), with clear handling
for the no-booted-device, no-Xcode, and missing-cert cases. A small injectable
command runner so the logic is testable without a real simulator.

Out: configuring the macOS **system proxy** (the networking half of routing the
simulator — separate concern, see open questions), removing/rotating trust,
device management, physical devices (which cannot be trusted this way at all).

---

## Why this is fiddly

We learned the shape of this during the real end-to-end test:

- **Trust is per-simulator and does not survive `simctl erase`.** A user who
  resets a device silently loses trust and gets opaque `-1202`s again. The tool
  should make re-installing a single, memorable command.
- **Xcode Previews uses a *separate* simulator set.** Certs installed into the
  default set do not apply to Previews; that set needs its own
  `--set previews` install. A user debugging a SwiftUI preview will otherwise see
  interception fail for no visible reason.
- **`booted` targets the one booted device.** With none booted, `simctl` errors;
  with several, `booted` is ambiguous. The tool must detect and explain both,
  not pass the raw simctl error through.
- **`simctl` only exists with Xcode's command-line tools.** On a machine without
  them (or not macOS at all) the action must fail with a human explanation, not a
  bare "executable not found".

## Security note

The only certificate this ever installs is the machine-local CA from 001, read
from the user's `-ca-dir`. There is no path by which a prebuilt or downloaded CA
is installed into a trust store — that would violate the constitution. The cert
path is validated to exist and be the user's own before anything is shelled out.

---

## Behaviour

`proxysim -trust` (does not start the proxy; installs and exits):

1. Resolve `ca.crt` under `-ca-dir` (with `~` expansion). If absent → error
   telling the user to run `proxysim` once to generate the CA first. Do **not**
   generate it here; trust is a separate action from CA lifecycle.
2. Check `xcrun`/`simctl` is available. If not → error naming the likely cause
   (Xcode command-line tools not installed, or not macOS).
3. Determine the target set (default set, or `previews` when `-trust-set previews`
   is given). A future value could target any named set.
4. Confirm exactly one booted device for `booted` to be unambiguous. Zero → tell
   the user to boot a simulator. More than one → list them and ask which
   (`-device <udid>`), rather than guessing.
5. Run `xcrun simctl [--set <set>] keychain booted add-root-cert <ca.crt>`.
6. On success, print a confirmation and the reminder that trust is lost on
   `simctl erase`. Installing again is harmless, so the action is idempotent.

## Interface sketch

Indicative, not binding:

```go
// Runner executes a command and returns its combined output. Injected so tests
// can drive the logic without a real simulator or Xcode.
type Runner func(ctx context.Context, name string, args ...string) ([]byte, error)

// InstallRootCert installs certPath into the booted simulator's trust store,
// targeting the given set ("" for the default set). It returns actionable errors
// for the no-device, no-Xcode, and ambiguous-device cases.
func InstallRootCert(ctx context.Context, run Runner, certPath, set, device string) error

// BootedDevices lists currently booted simulators, for disambiguation and
// friendly errors.
func BootedDevices(ctx context.Context, run Runner) ([]Device, error)
```

`main.go` gains a `-trust` bool and `-trust-set` / `-device` strings; when
`-trust` is set it performs the install and exits instead of serving.

---

## Acceptance criteria

The runner is faked; no criterion needs a real simulator except the last.

1. **Command shape.** With one booted device, `InstallRootCert` for the default
   set invokes `xcrun simctl keychain booted add-root-cert <certPath>` — asserted
   on the faked runner's captured args.
2. **Previews set.** `set == "previews"` invokes
   `xcrun simctl --set previews keychain booted add-root-cert <certPath>`.
3. **Missing cert.** A `certPath` that does not exist returns an error naming the
   file and suggesting `proxysim` be run to generate the CA, and the runner is
   never called.
4. **No Xcode.** A runner whose lookup fails (simulating absent `xcrun`) yields an
   error mentioning Xcode command-line tools, not a bare exec error.
5. **No booted device.** A faked device list with none booted returns an error
   telling the user to boot a simulator; no install is attempted.
6. **Ambiguous device.** Two booted devices with no `-device` given returns an
   error that lists them and asks for `-device`; with `-device <udid>` matching
   one, the install proceeds against that device.
7. **Idempotence.** Two successful installs in a row both succeed.
8. **main wiring.** `-trust` performs the install and exits without binding a
   listener; the normal serve path is unaffected when `-trust` is absent.
9. `go test ./internal/sim -race` passes; `go vet` clean; `gofmt -l` silent.

### Manual criterion (human-run)

10. On a machine with Xcode and a booted simulator: `proxysim -trust` followed by
    loading an HTTPS page through the running proxy succeeds — the same proof as
    005's criterion 12, now reached without typing the raw `simctl` line.

---

## Open questions

- **System proxy automation.** The other half of routing a simulator is pointing
  the macOS system proxy at us (`networksetup -setsecurewebproxy …`), which we did
  by hand in testing. It is a strong convenience, but it reroutes **all** Mac
  traffic and must be reliably reverted (including on crash), so it is riskier
  than a trust install. Kept out of 007; worth its own spec with explicit
  enable/disable and a restore-on-exit guarantee.
- **Targeting by device.** `-device <udid>` disambiguates multiple booted
  devices. Booting a specific device for the user (`simctl boot`) is a step
  further and probably out of scope — the user manages their simulators.
- **Verification.** `simctl` offers no clean read-back of installed roots, so
  success rests on its exit code. A deeper check would boot-and-probe, which is
  heavy; defer unless the exit code proves unreliable.
- **Removal.** There is no `remove-root-cert`; `simctl erase` is the only reset.
  A `-untrust` is therefore not straightforwardly implementable and is left out.
