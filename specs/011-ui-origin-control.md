# 011 — UI-driven Origin Control (pick sim + app live)

**Status:** implemented. Automated criteria 1–11 pass; the control endpoints were
smoke-tested end to end (GET/PUT /filter reflect and mutate the live filter). The
manual criterion 12 (pick sim/app in the browser against a booted simulator) is
for a human to run.
**Package:** `internal/origin` (runtime-mutable filter), `internal/sim`
(enumeration), `internal/ui` (endpoints + controls); wired through `main.go`
**Depends on:** 009 (the Filter and Resolver it now mutates), 008 (the UI it adds
controls to), 007 (`sim.Runner`/`BootedDevices` it extends)

---

## Intent

009 made interception selectable — but only at launch, and only if you already
know the bundle id, and only by restarting to change it. The whole point of a live
UI (008) is that you *don't* restart to change what you are looking at. 011 moves
the origin filter into the UI: a control bar where you pick the booted simulator
and, from a dropdown of that sim's installed apps, choose whether to intercept the
whole simulator or one app — and it takes effect on the **next connection**, no
restart.

This turns two of the tool's expert-only flags (`-only-sim`, `-app <bundle-id>`)
into a point-and-click choice, which is the "make it friendlier" ask made concrete.

This is a **control-surface** seam. It does not change *how* origin resolution or
classification works (009 owns that) — it makes the *decision input* mutable and
gives it a UI.

## Scope

In:
- Make `origin.Filter` **runtime-swappable**: the proxy reads the current filter
  per connection through a provider it can update, instead of capturing one fixed
  Filter at construction.
- Enumerate the booted simulators (already: `sim.BootedDevices`) and, new, the
  **installed apps** of a device (`sim.InstalledApps`) as `{bundleID, name}`.
- UI endpoints to read the sim/app lists, read the current filter, and set a new
  filter live.
- A control bar in the UI: a simulator selector, an app selector (with an "All
  simulator traffic" option = `-only-sim`), and an "off" option (intercept
  everything) — reflecting and mutating the live filter.
- Keeping the CLI flags authoritative for the *initial* state: `-only-sim`/`-app`
  set what the filter starts as; the UI changes it from there.

Out:
- Host/URL filtering in the UI (still client-side over rows — 008's open question;
  unchanged).
- Booting, installing, or launching apps from the UI. We *observe and select*
  among what is already there; we do not drive the simulator.
- Changing the filter when the UI is disabled (`-ui` off) — then the flags are the
  only control, exactly as 009. No new control path without the UI.
- Persisting the UI-chosen filter across restarts (the flags are the persistent
  form; a chosen-in-UI filter is session state).
- Anything about the *system proxy* (that is 010). 011 assumes traffic is already
  arriving.

---

## Non-negotiable: the control surface stays loopback, and only ever re-scopes capture

The new endpoints mutate what gets decrypted, so they inherit 008's binding rule
without exception: they are served **only** on the loopback UI listener, never a
new port, never `0.0.0.0`. Setting a filter cannot *increase* exposure beyond what
the running process already can do — the proxy already holds the CA and can
intercept any host; 011 only chooses *which origins* it does so for. It never
relaxes the loopback binding, never touches the CA, and (like 009) can only ever
narrow or widen between "this simulator/app" and "everything", all of which the
launching user already authorised by running the tool. A future reviewer should
hold: **the filter control changes scope, never privilege.**

## Design decisions

The expensive-to-reverse ones.

**A provider, not a value.** 009 wired `proxy.WithOriginFilter(Resolver, Filter)`
with a by-value Filter fixed for the process. 011 replaces the value with a
**provider the proxy calls per connection** — an atomically-updatable holder. The
proxy's per-connection decision (resolve origin at accept/CONNECT, then `Match`)
is unchanged; it just reads *today's* filter each time. This preserves 009's
"decided per connection, before the host is known" invariant while making the
input mutable. In-flight connections keep the verdict they were accepted with; the
change applies from the next connection — the honest, race-free semantics.

**Always resolve when a provider is present.** 009 could skip origin resolution
entirely when the filter was inactive (a pure fast-path). With a mutable filter,
"inactive" is no longer static — the user may activate it at any moment. Rule: if
a provider is registered, resolve per connection whenever the *current* filter is
Active; when it is inactive, still skip resolution (intercept all) and pay nothing.
So an off filter costs nothing; an on filter costs one resolve per connection,
exactly as 009. Flipping it on via the UI starts resolving from the next
connection. No background work, no change to idle cost.

**Enumeration shells out, like the rest of sim.** `InstalledApps` runs
`simctl listapps <udid>` behind the injected `sim.Runner` — same testability and
same "reuse Apple's tool" posture as 007. It returns only **user** apps (not the
dozens of system apps) so the dropdown is the list the developer recognises.

**The UI reflects the live filter, it is not a second source of truth.** The
control bar reads `GET /filter` on load and after each change, so two tabs (or a
flag-set initial state) never disagree. The server holds the one filter; the UI is
a view + mutator of it.

**Selecting an app implies its simulator.** Picking an app sets an `-app`-style
filter (that bundle id); picking "All simulator traffic" sets `-only-sim`;
"Everything" clears the filter. The device selector scopes which app list you see;
the actual match is still by bundle id (009: an app match is a simulator match),
so cross-device ambiguity does not arise for the match itself.

---

## Why this is fiddly

- **`simctl listapps` is not JSON.** Unlike `list devices -j`, `simctl listapps`
  emits an old-style NeXT plist, not JSON, and has no `-j`. Parse it by piping
  through `plutil -convert json -o - -` (already the project's approach for
  Info.plist reads in 009). Keep only entries with `ApplicationType == "User"`;
  take the display name from `CFBundleDisplayName`, falling back to
  `CFBundleName`, then the bundle id.
- **The provider must be race-clean.** It is read on every accepted connection
  (the request path) and written from an HTTP handler. An `atomic.Pointer[Filter]`
  (store a whole new Filter, never mutate in place) keeps it lock-free on the read
  side, which is the hot side. Tests run under `-race` (constitution).
- **`Filter` currently carries a map.** `Filter.Apps` is a `map[string]bool`;
  swapping the whole `Filter` under an atomic pointer is safe only if a stored
  Filter is never mutated after publication. Treat published Filters as immutable —
  build a fresh one on each `PUT /filter` and store it. Document it on the setter.
- **A `PUT /filter` that names an unknown/uninstalled bundle id is not an error.**
  009 already treats an unmatched bundle id as "intercept nothing from it" (safe
  direction). The endpoint validates shape, not existence — an app the user
  hasn't launched yet still resolves correctly once it runs. Don't couple the
  filter to the enumerated list.
- **No booted simulator when the UI opens.** The control bar must degrade: show
  "no booted simulator", offer the "Everything"/"All simulator" choices anyway
  (they need no device), and let the app dropdown populate once a sim boots and the
  user refreshes. Never error the page over an empty device list.

## Surface

New/changed endpoints on the existing loopback UI server (008):

| Route | Purpose |
|---|---|
| `GET /sims` | booted simulators: `[{udid, name, runtime}]` (via `sim.BootedDevices`) |
| `GET /sims/{udid}/apps` | that sim's installed **user** apps: `[{bundleID, name}]` |
| `GET /filter` | the live filter: `{onlySim: bool, apps: [bundleID, …]}` |
| `PUT /filter` | set the live filter from that same JSON shape; returns the stored result |

The app (008's SPA) gains a control bar above the flow list: a **Simulator**
`<select>`, an **App** `<select>` whose first two options are *Everything (no
filter)* and *All simulator traffic*, followed by the enumerated apps. Changing
either issues `PUT /filter` and updates a small status line ("Intercepting: MyApp
only" / "iOS Simulator only" / "Everything"). It reads `GET /filter` on load to
reflect a flag-set initial state.

### Interface sketch

Indicative, not binding.

```go
// origin — a mutable holder the proxy reads per connection.
//
// Controller carries the current Filter for a session, swappable at runtime.
// Reads are lock-free (atomic pointer) because they happen on the request path;
// a stored Filter is immutable — Set publishes a fresh one, never mutates.
type Controller struct { p atomic.Pointer[Filter] }
func NewController(initial Filter) *Controller
func (c *Controller) Current() Filter
func (c *Controller) Set(f Filter)

// proxy gains a provider-based option alongside (or replacing) WithOriginFilter:
//   proxy.WithOriginFilterFunc(resolver Resolver, current func() Filter)
// It resolves + matches per connection using current() each time.

// sim — enumeration for the picker.
type App struct { BundleID, Name string }
func InstalledApps(ctx context.Context, run Runner, udid string) ([]App, error)
```

`main.go`: build a `Controller` seeded from the `-only-sim`/`-app` flags (and, from
010, from `-system-proxy` implying only-sim), pass `WithOriginFilterFunc(resolver,
ctrl.Current)` to the proxy, and hand the `Controller` plus a `sim.Runner` to the
Hub so its handlers can read/set the filter and enumerate devices/apps.

---

## Acceptance criteria

`sim.Runner` is faked for enumeration; the filter/provider tests use the fake
resolver from 009. No automated criterion needs a real simulator.

1. **Mutable filter, per-connection.** With a `Controller` seeded inactive, a
   connection from a simulator process is intercepted (no filter); after
   `ctrl.Set(only-sim)`, the *next* connection from a host process is
   blind-tunnelled with no flow — asserted by driving two connections across a
   `Set`, race-clean under `-race`.
2. **Provider read is lock-free and safe.** Concurrent `Current`/`Set` from many
   goroutines is race-clean; a published Filter is never mutated (asserted by
   storing, then building a different Filter and storing again).
3. **`GET /filter` reflects state.** Returns the seeded filter as
   `{onlySim, apps}`; after `PUT /filter`, returns the new one.
4. **`PUT /filter` sets the live filter.** PUTting `{apps:["com.example.A"]}` makes
   the proxy intercept only that app on subsequent connections (asserted through
   the same proxy+resolver harness as criterion 1); PUTting `{}` clears it.
5. **`PUT /filter` validates shape, not existence.** A well-formed body with an
   uninstalled bundle id is accepted (200) and stored; a malformed body is 400.
6. **`GET /sims`.** With a faked `sim.Runner` reporting two booted devices, returns
   both as `{udid, name, runtime}`, newest/stable order; a fake reporting none
   returns `[]`, not an error.
7. **`GET /sims/{udid}/apps`.** With a faked `listapps` plist, returns only
   `ApplicationType == User` apps as `{bundleID, name}`, name resolved
   display→bundle-name→id; a device with no apps returns `[]`.
8. **Enumeration parse.** `sim.InstalledApps` parses the `plutil`-converted JSON of
   a representative `listapps` output, dropping system apps; a `plutil`/`simctl`
   failure returns a wrapped error naming the device.
9. **Loopback only.** The new endpoints are served only by the 008 UI handler
   (loopback); a test asserts they are absent from the proxy handler and present on
   the UI handler.
10. **Off costs nothing.** With the Controller inactive, the proxy performs no
    origin resolution (asserted with a resolver that fails the test if called),
    preserving 009's fast path.
11. `go test ./internal/origin ./internal/proxy ./internal/sim ./internal/ui
    -race` passes; `go vet` clean; `gofmt -l` silent; non-Darwin build compiles.

### Manual criterion (human-run)

12. With a booted simulator, `proxysim -ui` (and 010's `-system-proxy` for the full
    friendly path): open the UI, pick the simulator, pick one app from the
    dropdown, and see only that app's traffic appear; switch to "All simulator
    traffic" and see the rest of the simulator's traffic join; switch to
    "Everything" and see host traffic appear too — all without restarting.

---

## Open questions

- **Running vs installed apps.** v1 lists *installed* user apps (reliable, from
  `listapps`). Highlighting which are currently running, or listing only running
  ones, needs process enumeration and is racy — deferred. The 009 resolver already
  learns which bundles are live as traffic arrives; a future "seen recently" hint
  could come from there for free.
- **Multi-app selection.** The filter already supports a set of bundle ids
  (`Filter.Apps`); the v1 UI exposes single-app choice for simplicity. A
  multi-select is a small additive UI change over the same endpoint shape.
- **Live device add/remove.** `GET /sims` is pull-only; a sim booted after the page
  loads appears on refresh. Pushing device changes over the existing SSE stream is
  a nicety, not v1.
- **Persisting the UI choice.** The chosen filter is session state; the flags are
  the durable form. If "remember my last selection" is wanted, it writes to
  `~/.proxysim/` like the other session state (010's snapshot), but that is not v1.
- **Filter toggle vs system-proxy toggle.** 010 notes a future "capture on/off"
  button; that is the *system proxy* takeover, distinct from this filter. Both live
  in the same control bar eventually; keep them visually and conceptually separate
  (one changes scope, one changes host routing).
