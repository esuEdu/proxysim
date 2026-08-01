// Package origin resolves the local process that opened a proxied connection and
// classifies whether it belongs to the iOS Simulator, so the proxy can intercept
// only simulator (or a single app's) traffic and blind-tunnel everything else.
// See specs/009-origin-filter.md.
//
// The socket→PID resolution is platform-specific and lives behind build tags
// (origin_darwin.go via libproc; origin_other.go a stub). Everything here — the
// filter decision and the path→simulator classification — is portable and
// testable without cgo or a real simulator.
package origin

import (
	"net"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
)

// Process is the resolved originator of a connection.
type Process struct {
	PID        int
	Path       string // executable path
	Simulator  bool   // Path is under a CoreSimulator device container
	DeviceUDID string // the simulator device, when Simulator
	BundleID   string // CFBundleIdentifier of the owning .app, when resolvable
}

// Resolver maps a proxied connection's endpoints to the process that owns its
// client end. local is the address the proxy accepted on; remote is the client's
// address (its ephemeral local port). Injected so the proxy is testable without
// cgo or real processes; the real Resolve (Darwin) uses libproc and must be
// called while the connection is open.
type Resolver func(local, remote net.Addr) (Process, error)

// Filter is the intercept decision over a resolved origin. The zero Filter
// matches everything, i.e. the feature is off.
type Filter struct {
	OnlySim bool
	Apps    map[string]bool // bundle ids; a non-empty set implies OnlySim
}

// BuildFilter constructs a Filter from the -only-sim and -app flags. Naming an
// app implies simulator-only: an app match is by definition a simulator match.
func BuildFilter(onlySim bool, apps []string) Filter {
	f := Filter{OnlySim: onlySim}
	for _, a := range apps {
		if a = strings.TrimSpace(a); a != "" {
			if f.Apps == nil {
				f.Apps = make(map[string]bool)
			}
			f.Apps[a] = true
			f.OnlySim = true
		}
	}
	return f
}

// Active reports whether the filter narrows anything. When false the proxy skips
// origin resolution entirely and intercepts as before.
func (f Filter) Active() bool {
	return f.OnlySim || len(f.Apps) > 0
}

// Controller carries the Filter in force for a session and lets it be swapped at
// runtime — the seam the UI uses to re-scope capture without a restart (spec 011).
// Reads happen on the request path, once per accepted connection, so they are
// lock-free via an atomic pointer; a published Filter is treated as immutable, so
// Set stores a fresh Filter rather than mutating one in place (its Apps map must
// never be written after publication).
type Controller struct {
	p atomic.Pointer[Filter]
}

// NewController returns a Controller seeded with initial (typically built from the
// -only-sim/-app flags). The initial filter may be inactive, meaning "intercept
// everything" until the UI narrows it.
func NewController(initial Filter) *Controller {
	c := &Controller{}
	c.p.Store(&initial)
	return c
}

// Current returns the filter in force. Safe to call from the request path.
func (c *Controller) Current() Filter {
	if f := c.p.Load(); f != nil {
		return *f
	}
	return Filter{}
}

// Set publishes f as the filter in force from the next connection onward.
// In-flight connections keep the verdict they were accepted with. f must not be
// mutated after this call — build a new Filter to change the selection again.
func (c *Controller) Set(f Filter) {
	c.p.Store(&f)
}

// Match reports whether a connection from p should be intercepted. With an app
// set, only those bundle ids match (and only in the simulator); with -only-sim,
// any simulator process matches; the zero Filter matches everything.
func (f Filter) Match(p Process) bool {
	if len(f.Apps) > 0 {
		return p.Simulator && f.Apps[p.BundleID]
	}
	if f.OnlySim {
		return p.Simulator
	}
	return true
}

// classify fills the simulator fields of a Process from its executable path. It
// is where a resolved PID becomes a "this is simulator app X" decision, kept
// portable so the platform resolvers only have to supply pid+path.
func classify(pid int, path string) Process {
	p := Process{PID: pid, Path: path}
	udid, appDir, ok := simParts(path)
	if !ok {
		return p
	}
	p.Simulator = true
	p.DeviceUDID = udid
	p.BundleID = bundleID(appDir)
	return p
}

// A simulator process wears one of two path shapes (confirmed by the 009 probe):
//
//   - an installed app lives under the per-device container, so its path carries
//     the device UDID and a resolvable .app bundle;
//   - a runtime/system process (MobileSafari, com.apple.WebKit.Networking, trustd,
//     …) lives under the shared runtime root, with no per-device UDID and often no
//     user-facing .app.
//
// Recognising only the first would silently miss all of Safari and the system
// daemons under -only-sim, so we match both.
const (
	deviceMarker  = "/CoreSimulator/Devices/"
	runtimeMarker = ".simruntime/"
)

// simParts reports whether path belongs to the simulator and, if so, returns the
// device UDID (empty for shared runtime processes) and the enclosing .app
// directory (empty when the binary is not inside a bundle).
func simParts(path string) (udid, appDir string, ok bool) {
	if i := strings.Index(path, deviceMarker); i >= 0 {
		rest := path[i+len(deviceMarker):]
		if slash := strings.IndexByte(rest, '/'); slash >= 0 {
			return rest[:slash], appDirOf(path), true
		}
	}
	// Runtime/system process: simulator traffic, but shared across devices — no
	// UDID, and networking/extension processes have no user-facing .app bundle.
	if strings.Contains(path, runtimeMarker) {
		return "", appDirOf(path), true
	}
	return "", "", false
}

// appDirOf returns the enclosing .app directory of an executable path, or "" if
// the binary is not inside a bundle. It matches ".app/" exactly, so an ".appex/"
// extension container (e.g. WebKit's NetworkingExtension) is correctly not
// treated as a bundle.
func appDirOf(path string) string {
	if k := strings.Index(path, ".app/"); k >= 0 {
		return path[:k+len(".app")]
	}
	return ""
}

// bundleCache memoises the .app-directory → bundle-id lookup: the mapping is
// stable per app, so we read Info.plist at most once per bundle rather than per
// connection.
var bundleCache sync.Map // string -> string

// bundleID returns the CFBundleIdentifier of the app at appDir, or "" if it
// cannot be read. A missing id simply never matches an -app filter, which is the
// safe direction (do not intercept what we cannot attribute).
func bundleID(appDir string) string {
	if appDir == "" {
		return ""
	}
	if v, ok := bundleCache.Load(appDir); ok {
		return v.(string)
	}
	id := readBundleID(appDir)
	bundleCache.Store(appDir, id)
	return id
}

// readBundleID extracts CFBundleIdentifier via plutil, which reads both binary
// and XML plists (simulator Info.plists are usually binary, which the stdlib
// cannot parse). plutil is macOS-only; elsewhere the exec fails and we return "".
func readBundleID(appDir string) string {
	out, err := exec.Command("plutil",
		"-extract", "CFBundleIdentifier", "raw", "-o", "-",
		filepath.Join(appDir, "Info.plist")).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}
