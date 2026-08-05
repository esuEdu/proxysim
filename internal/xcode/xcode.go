// Package xcode activates proxysim from an Xcode scheme Run pre-action. Xcode has
// no supported way to host a panel inside its own window (Source Editor Extensions
// operate on editor text only, and in-process plugin injection has been blocked
// since Xcode 8), so the "it just turns on when I build" experience is delivered
// by a tiny pre-action that launches this standalone tool.
//
// The pieces here are: a persisted "armed" flag the pre-action reads while
// proxysim is not running; resolution of the target app+device from Xcode's build
// environment; a loopback control socket so a second build retargets the running
// instance instead of spawning a duplicate; and a watchdog that tears the session
// down when the debugged app leaves the simulator — the primary teardown path,
// because Xcode does NOT run scheme post-actions on a cancelled, failed, or
// crashed run. The system-proxy restore promise itself lives in internal/sysproxy
// (spec 010); this package only decides *when* to trigger it. See
// specs/012-xcode-integration.md.
//
// Everything here is portable Go: the simulator-specific probes are injected
// (the production ones shell out via internal/sim's Runner), so the whole package
// is testable without Xcode or a real simulator.
package xcode

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	armedFile   = "armed"        // presence under the ca-dir means auto-launch is on
	controlSock = "control.sock" // loopback retarget channel for a running instance
)

// ErrNoInstance means no proxysim is currently listening on the control socket, so
// the caller (an -xcode-run pre-action) should become the serving instance itself
// rather than hand off to one.
var ErrNoInstance = errors.New("xcode: no running proxysim instance to retarget")

// ErrNoBundleID means the Xcode environment carried no PRODUCT_BUNDLE_IDENTIFIER,
// so there is no app to scope to. Without it the pre-action cannot target anything
// and reports rather than guessing.
var ErrNoBundleID = errors.New("xcode: PRODUCT_BUNDLE_IDENTIFIER not set in the Xcode environment")

// Target is what one Xcode Run resolves to: the app bundle id to intercept and the
// simulator device it runs on. The bundle id drives the origin filter; the device
// drives CA trust and the teardown watchdog.
type Target struct {
	BundleID   string `json:"bundleID"`
	DeviceUDID string `json:"deviceUDID"`
}

// Armed reports whether auto-launch-on-build is enabled. It is a file check so the
// pre-action can read the flag cold, while no proxysim process is running. dir is
// the proxysim data directory (the resolved -ca-dir).
func Armed(dir string) (bool, error) {
	_, err := os.Stat(ArmedPath(dir))
	if err == nil {
		return true, nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return false, fmt.Errorf("checking armed flag %s: %w", ArmedPath(dir), err)
}

// SetArmed toggles the persisted armed flag. Arming writes a 0600 marker beside
// the CA material (it records local state, not a secret, but there is no reason
// for it to be world-readable); disarming removes it. Both are idempotent.
func SetArmed(dir string, on bool) error {
	path := ArmedPath(dir)
	if on {
		if err := os.WriteFile(path, []byte("armed\n"), 0o600); err != nil {
			return fmt.Errorf("arming (%s): %w", path, err)
		}
		return nil
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("disarming (%s): %w", path, err)
	}
	return nil
}

// ArmedPath is the on-disk location of the armed flag.
func ArmedPath(dir string) string { return filepath.Join(dir, armedFile) }

// ControlPath is the loopback control socket a serving instance listens on and a
// pre-action dials to retarget it. It lives under the data dir (0700), so the
// socket inherits that directory's access control.
func ControlPath(dir string) string { return filepath.Join(dir, controlSock) }

// FromEnv resolves the Target from the Xcode pre-action environment. getenv is
// injected (os.Getenv in production) for testability. PRODUCT_BUNDLE_IDENTIFIER is
// required; a missing TARGET_DEVICE_IDENTIFIER is not fatal — Xcode omits it in
// some pre-action contexts — so we fall back to the booted simulator via
// bootedFallback. A fallback that itself fails is tolerated (the device is left
// empty and only the watchdog degrades), because the origin filter matches an app
// across any simulator regardless.
func FromEnv(getenv func(string) string, bootedFallback func() (string, error)) (Target, error) {
	bundle := strings.TrimSpace(getenv("PRODUCT_BUNDLE_IDENTIFIER"))
	if bundle == "" {
		return Target{}, ErrNoBundleID
	}
	device := strings.TrimSpace(getenv("TARGET_DEVICE_IDENTIFIER"))
	if device == "" && bootedFallback != nil {
		if udid, err := bootedFallback(); err == nil {
			device = udid
		}
	}
	return Target{BundleID: bundle, DeviceUDID: device}, nil
}

// SendTarget hands t to an already-running instance over the control socket and
// returns nil on success. If nothing is listening (no live instance), it returns
// an error wrapping ErrNoInstance so the caller becomes the instance instead. The
// only reason a dial to this socket fails is the absence of a listener, so every
// dial error is treated as "no instance" rather than surfaced raw.
func SendTarget(dir string, t Target) error {
	conn, err := net.DialTimeout("unix", ControlPath(dir), 2*time.Second)
	if err != nil {
		return fmt.Errorf("%w (%v)", ErrNoInstance, err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
	if err := json.NewEncoder(conn).Encode(t); err != nil {
		return fmt.Errorf("sending target to running instance: %w", err)
	}
	return nil
}

// Server is the serving instance's end of the control socket: it accepts retarget
// messages from later builds and reports each new Target to a callback.
type Server struct {
	ln   net.Listener
	path string
}

// Listen binds the control socket for a serving instance. A stale socket file from
// a dead instance is removed first, so a crash that left the file behind does not
// stop the next instance from binding.
func Listen(dir string) (*Server, error) {
	path := ControlPath(dir)
	// A leftover socket from a previous run makes bind fail with EADDRINUSE even
	// though nobody is listening; clear it. Ignore a missing file.
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("clearing stale control socket %s: %w", path, err)
	}
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, fmt.Errorf("listening on control socket %s: %w", path, err)
	}
	return &Server{ln: ln, path: path}, nil
}

// Serve accepts retarget messages until Close, invoking onRetarget for each. It
// blocks, so callers run it on a goroutine. A malformed message is dropped rather
// than fatal — the control channel must never take the serving instance down.
func (s *Server) Serve(onRetarget func(Target)) {
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			return // listener closed by Close
		}
		go func() {
			defer conn.Close()
			_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
			var t Target
			if err := json.NewDecoder(conn).Decode(&t); err == nil {
				onRetarget(t)
			}
		}()
	}
}

// Close stops accepting and removes the socket file so the next instance starts
// clean.
func (s *Server) Close() error {
	err := s.ln.Close()
	if rmErr := os.Remove(s.path); rmErr != nil && !errors.Is(rmErr, os.ErrNotExist) && err == nil {
		err = rmErr
	}
	return err
}

// AppProbe reports whether the target app is currently running on its simulator.
// An error means "unknown" (a transient simctl failure), which the watchdog treats
// as not-yet-a-reason-to-tear-down, never as absence.
type AppProbe func(ctx context.Context, t Target) (bool, error)

// Runner matches internal/sim.Runner so SimAppProbe can shell out through the same
// injected command runner the rest of the tool uses.
type Runner func(ctx context.Context, name string, args ...string) ([]byte, error)

// SimAppProbe builds the production AppProbe: it asks the simulator's launchd for
// its job list and looks for the app's UIKitApplication entry, which is present
// exactly while the app is running. With no device or bundle id it reports unknown
// (error) rather than absent, so a partially-resolved target does not trigger a
// spurious teardown.
func SimAppProbe(run Runner) AppProbe {
	return func(ctx context.Context, t Target) (bool, error) {
		if t.DeviceUDID == "" || t.BundleID == "" {
			return false, errors.New("xcode: incomplete target for app probe")
		}
		out, err := run(ctx, "xcrun", "simctl", "spawn", t.DeviceUDID, "launchctl", "list")
		if err != nil {
			return false, fmt.Errorf("probing app %s on %s: %w", t.BundleID, t.DeviceUDID, err)
		}
		return strings.Contains(string(out), "UIKitApplication:"+t.BundleID), nil
	}
}

// Watch blocks until the target app has been absent from its simulator long enough
// to conclude the run is over, then returns so the caller restores and exits. It
// is the primary teardown trigger precisely because it does not depend on Xcode
// firing a post-action.
//
// Two guards keep it from tearing down at the wrong moment:
//   - it waits to observe the app running at least once before counting absence,
//     so a slow first launch is not mistaken for a finished run;
//   - if the app never comes up within startupGrace (e.g. the build failed), it
//     returns anyway, so an armed build that never launched still restores the
//     system proxy rather than leaving it dangling.
//
// current is read each tick so a retarget mid-session follows the new app. A probe
// error is treated as "unknown" and resets the absence timer.
func Watch(ctx context.Context, probe AppProbe, current func() Target, poll, grace, startupGrace time.Duration) {
	start := time.Now()
	var seen bool
	var absentSince time.Time

	ticker := time.NewTicker(poll)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			running, err := probe(ctx, current())
			switch {
			case err != nil:
				// Unknown: do not count as absence. But if the app has never been
				// seen and startup has run long, give up waiting and tear down.
				absentSince = time.Time{}
				if !seen && time.Since(start) >= startupGrace {
					return
				}
			case running:
				seen = true
				absentSince = time.Time{}
			case !seen:
				// Not up yet. Bound how long we wait for the first launch.
				if time.Since(start) >= startupGrace {
					return
				}
			case absentSince.IsZero():
				absentSince = time.Now() // first tick of absence after having been up
			case time.Since(absentSince) >= grace:
				return
			}
		}
	}
}

// HookSnippet returns the shell to paste once into a scheme's Run pre-actions
// (Product → Scheme → Edit Scheme → Run → Pre-actions). It is non-blocking
// (backgrounded, output discarded) so it never stalls a build, and references the
// binary by absolute, shell-quoted path so a path with spaces survives. When
// proxysim is disarmed the launched process exits immediately as a no-op.
func HookSnippet(binPath string) string {
	return fmt.Sprintf("# proxysim — auto-start when armed (a no-op otherwise). Non-blocking.\n%q -xcode-run >/dev/null 2>&1 &\n", binPath)
}
