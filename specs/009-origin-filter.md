# 009 — Origin Filtering (simulator / per-app)

**Status:** implemented. Automated criteria 1–9 pass. The 009 probe was run
against a real booted simulator: a real app's native traffic is intercepted by
`-only-sim` and attributable for `-app` (the shared-daemon worry did not
materialise), and the probe revealed that simulator processes take two path
shapes — installed apps under the device container and runtime/system processes
under the runtime root — so classification recognises both.
**Package:** `internal/origin` (new), wired through `internal/proxy`
**Depends on:** 004 (plain HTTP forwarding), 005 (CONNECT + TLS termination, the
blind-tunnel fallback this reuses)

---

## Intent

Interception is host-wide today, and not because we chose it: the Simulator has
no network stack of its own, so the only way to route it through the proxy is the
**system-wide** macOS proxy — which drags every host app (Safari, update daemons,
your other tools) through us too. The UI fills with traffic you did not ask to
see, and the tool decrypts far more than it should.

009 narrows capture by **who opened the connection**. Because the Simulator's
apps run as ordinary macOS processes over loopback, the proxy can resolve the
originating process of each connection and intercept only the ones that come from
the Simulator (`-only-sim`), or from a named app (`-app <bundle-id>`). Everything
else is blind-tunnelled untouched — host traffic still flows, it is just neither
decrypted nor shown.

This is a filtering seam, not a change to how interception works. It sits in
front of 005's decision and reuses 005's tunnel for the "not ours" path.

## Scope

In: resolving the local process that owns an accepted connection (socket 4-tuple
→ PID → executable path, at connection time); classifying whether that process is
a Simulator app and which bundle it is; two flags — `-only-sim` and
`-app <bundle-id>` — that intercept only matching connections and blind-tunnel
the rest; an injectable resolver so the proxy logic is testable without a real
simulator or cgo.

Out: setting or reverting the system proxy (still separate — 007's open
question); **blocking** connections (we tunnel, never fail — constitution);
filtering by host/URL (that is client-side UI filtering, 008's open question, and
`-exclude` already covers host-level opt-out); physical devices (they do not
share the host stack, so none of this applies); non-macOS platforms (the project
is macOS-only; the resolver is a no-op stub elsewhere so the build still works).

---

## Why this is fiddly

The lessons that shaped the design, some learned in the 009 spike:

- **The lookup must happen while the connection is open.** The originating socket
  is ephemeral; a deferred or queued resolution races its close and finds
  nothing. The proxy resolves origin **once, synchronously, at accept (plain
  HTTP) or at CONNECT (TLS)**, before it knows the host, and caches the verdict
  for that connection's lifetime.
- **There is no stdlib way to map a socket to a PID.** The spike proved the
  mechanism with `lsof`, but forking `lsof` per connection is too slow and still
  racy. The real path is `libproc` (`proc_pidfdinfo` with
  `PROC_PIDFDSOCKETINFO`), which needs **cgo** — the project's first. It earns
  its place: `libproc` is a macOS system library, not a third-party dependency,
  and the stdlib genuinely cannot do this. Isolate it behind build tags
  (`origin_darwin.go` real, `origin_other.go` stub) so non-Darwin builds compile.
- **The match is on the connection's *remote* end.** For a loopback connection
  the proxy accepted, `conn.RemoteAddr()` is `127.0.0.1:<client-ephemeral-port>`.
  The originating process is the one whose TCP socket has that as its *local*
  port and our listen port as its *foreign* port. Match the full 4-tuple, not
  just the port, to be unambiguous.
- **Unresolvable origin fails toward "not ours."** If the PID is already gone, or
  the socket cannot be attributed, treat it as **no match → blind-tunnel**. This
  errs on the side of *not* decrypting traffic we cannot attribute, and it never
  fails a connection. Silent, safe, private.
- **Bundle id is not in the path.** The executable path
  (`…/CoreSimulator/Devices/<UDID>/…/YourApp.app/YourApp`) carries the app *name*
  and the device UDID but not the bundle id; that lives in the `.app`'s
  `Info.plist` (`CFBundleIdentifier`). Read it once per `.app` path and cache it.
- **A simulator process wears one of two paths (confirmed by the probe).**
  *Installed* apps — the user's own — live under the device container
  `…/CoreSimulator/Devices/<UDID>/…/YourApp.app/YourApp`: per-device, with a
  resolvable bundle id. But the simulator's *runtime and system* processes —
  MobileSafari, `com.apple.WebKit.Networking`, `trustd`, `webprivacyd`, and the
  like — live under the shared runtime root
  `…/<ver>.simruntime/Contents/Resources/RuntimeRoot/…`: no per-device UDID, and
  (for the networking/extension processes) no user-facing `.app` bundle.
  `-only-sim` must recognise **both** or it silently misses all of Safari and the
  system daemons — which is exactly what the first cut did. The device marker
  alone is not enough.
- **WebView traffic belongs to a networking process, not the app.** A `WKWebView`
  request egresses through `com.apple.WebKit.Networking` (a runtime-root process),
  not the hosting app, so it resolves as "simulator, no bundle." `-only-sim`
  catches it; `-app` cannot attribute it to your bundle. Only the app's own
  `URLSession`/`CFNetwork` traffic carries the app's identity. The probe confirmed
  a real app's native calls resolve to the app process — so per-app works for
  them — while its web views would not.

## Security note

009 strictly **reduces** exposure: with a filter on, the proxy decrypts and
stores only traffic it can attribute to the Simulator (or one app), and passes
everything else through as an opaque tunnel. It reads process metadata of the
user's own processes on their own machine — no new surface. Nothing here relaxes
the loopback binding or touches the CA. A future reviewer should treat "off by
default, and only ever narrows what is captured" as an invariant.

---

## Behaviour

Flags (both off by default — absent, behaviour is exactly as today: intercept
everything):

- `-only-sim` — intercept only connections originating from a Simulator process.
- `-app <bundle-id>` — comma-separated bundle ids; intercept only connections
  from those apps. Implies `-only-sim` semantics (an app match is a sim match).

Per connection, before deciding to intercept (i.e. before 005's CONNECT handling
and before 004's forward):

1. Resolve the originating process from the connection's remote address.
2. Decide match:
   - no filter set → **match** (unchanged behaviour);
   - `-only-sim` → match iff the origin is a Simulator process;
   - `-app a,b` → match iff the origin is a Simulator app whose bundle id ∈ {a,b}.
3. **Match** → proceed with normal interception (004/005).
4. **No match** (including unresolved origin) → hand the connection to 005's
   blind-tunnel path and **do not surface it as a flow**. This is the deliberate
   difference from `-exclude`: an excluded host is one you named and still want to
   see listed as "[tunnelled, not inspected]", whereas an origin-filtered-out
   connection is noise you asked to hide, so it is tunnelled silently (a
   `-verbose` line is the most it gets).

Precedence with `-exclude`: origin filtering is the outer gate ("is this
connection ours at all?"). A non-matching origin tunnels silently regardless of
host; a matching origin then still honours `-exclude` and surfaces the excluded
host as a tunnelled flow.

## Interface sketch

Indicative, not binding.

```go
// Package origin resolves the local process that opened a proxied connection and
// classifies whether it belongs to the iOS Simulator, so the proxy can intercept
// only simulator/app traffic and blind-tunnel everything else. macOS-only; other
// platforms get a stub that resolves nothing (filters then match nothing, which
// with the fail-toward-tunnel rule means "intercept all" stays the only useful
// mode off-Darwin).
package origin

// Process is the resolved originator of a connection.
type Process struct {
	PID        int
	Path       string // executable path
	Simulator  bool   // path is under CoreSimulator/Devices/<UDID>
	DeviceUDID string // when Simulator
	BundleID   string // from the .app's Info.plist, when resolvable
}

// Resolver maps a connection to the process that owns its client end. Injected so
// the proxy is testable without cgo or real processes; Darwin's real resolver
// uses libproc. It must be called while conn is open.
type Resolver func(conn net.Conn) (Process, error)

// Filter is the intercept decision over a resolved origin.
type Filter struct {
	OnlySim bool
	Apps    map[string]bool // bundle ids; non-empty implies OnlySim
}

// Match reports whether a connection from p should be intercepted. The zero
// Filter matches everything (feature off).
func (f Filter) Match(p Process) bool
```

`internal/proxy` gains an option, e.g. `proxy.WithOriginFilter(Resolver, Filter)`;
when set it resolves and decides per connection, routing non-matches to the
existing tunnel. `main.go` gains `-only-sim` and `-app`, builds the Filter, and
passes the Darwin resolver.

---

## Acceptance criteria

The resolver is faked for all but one criterion; no criterion needs a real
simulator except the last.

1. **Injectable resolver.** The proxy accepts a Resolver; tests drive interception
   decisions by mapping a connection's remote address to a chosen Process, with no
   cgo and no real simulator.
2. **`-only-sim`.** A connection whose origin is a Simulator process is
   intercepted (a Flow is produced); one whose origin is a host process is
   blind-tunnelled and produces **no** flow.
3. **`-app`.** With `-app com.example.A`: a Simulator app with bundle
   `com.example.A` is intercepted; a Simulator app `com.example.B` and a host
   process are both tunnelled with no flow.
4. **Unresolved origin.** A resolver returning an error yields a blind tunnel and
   no flow — never a failed connection, never a panic.
5. **Off by default.** With neither flag, every connection is intercepted exactly
   as before 009 (the zero Filter matches all); existing 004/005 tests are
   unaffected.
6. **Filtered-out is silent.** Origin-filtered-out connections do not reach the
   flow sink, distinguishing them from `-exclude`, whose tunnelled hosts *do*
   appear. A test asserts the sink is empty after a filtered-out connection.
7. **Decided before host is known.** The intercept decision is a function of the
   connection's origin, taken at accept/CONNECT, not of the request line or host —
   asserted by driving a match/no-match purely from origin with the host held
   constant.
8. **Real resolver, no simulator.** An integration test dials the running proxy
   from the test process itself and asserts the Darwin resolver returns *this*
   process's PID and executable path from the live connection — the one test that
   exercises the real libproc path, needing no simulator.
9. `go test ./internal/origin ./internal/proxy -race` passes; `go vet` clean;
   `gofmt -l` silent; non-Darwin build still compiles (stub).

### Manual criterion (human-run)

10. With a booted simulator, the system proxy pointed at proxysim, and
    `proxysim -ui -app <your bundle id>`: only that app's traffic appears in the
    UI, while Safari or other host traffic on the same system proxy does not —
    and every app still loads normally (non-matches tunnelled, not blocked).

---

## Open questions

- **Shared-daemon traffic — resolved by the probe.** The worry was that a
  Simulator app's traffic might egress through a shared service rather than the
  app process, defeating `-app`. Running the probe against a real app settled it:
  a native app's `URLSession`/`CFNetwork` connections resolve to the **app
  process itself** (path under `…/Devices/<UDID>/…/App.app/App`, parent
  `launchd_sim`), so `-app` works. The one carve-out is **WebView traffic**, which
  runs in `com.apple.WebKit.Networking` (a runtime-root process with no app
  bundle): `-only-sim` catches it, `-app` cannot attribute it. `URLSession`
  background sessions were not observed to funnel through a host daemon; if a
  future case does, it will surface as "simulator, no bundle" and fall to
  `-only-sim`, never mis-attributed.
- **Bundle id vs app name.** Matching by bundle id (Info.plist) is precise but
  costs a plist read (cached per `.app`). Matching by the app name already in the
  path is cheaper but coarser. Start with bundle id; fall back to app-name match
  only if plist reads prove unreliable.
- **PID reuse and caching.** Resolve the socket→PID mapping fresh per connection
  (PIDs recycle); cache only the stable `.app`-path → bundle-id mapping.
- **Cost.** One libproc scan per new connection (connections, not requests) —
  expected to be negligible, but worth a sanity check under a chatty app before
  claiming so.
- **Interaction with future two-phase emit (008).** When pending rows land, a
  tunnelled-and-hidden connection should stay hidden at request-start too; the
  decision is already per-connection, so this slots in without rework.
