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

// simMarker is the path segment that identifies a booted simulator's device
// container. (Xcode Previews uses a separate "Simulator Devices" tree — a
// follow-up; see spec 009 / 007's previews note.)
const simMarker = "/CoreSimulator/Devices/"

// simParts detects whether path lives inside a simulator device container and,
// if so, returns the device UDID and the enclosing .app directory (empty when the
// binary is not inside a bundle).
func simParts(path string) (udid, appDir string, ok bool) {
	i := strings.Index(path, simMarker)
	if i < 0 {
		return "", "", false
	}
	rest := path[i+len(simMarker):]
	slash := strings.IndexByte(rest, '/')
	if slash < 0 {
		return "", "", false
	}
	udid = rest[:slash]

	if k := strings.Index(path, ".app/"); k >= 0 {
		appDir = path[:k+len(".app")]
	}
	return udid, appDir, true
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
