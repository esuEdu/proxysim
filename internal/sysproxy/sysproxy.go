// Package sysproxy snapshots, sets, and restores the macOS system HTTP/HTTPS
// proxy for the primary network service, so proxysim can point the Simulator at
// itself on startup and — the part that actually matters — put the setting back
// on exit. A stale system proxy breaks unrelated apps, so restoration is treated
// as a promise, not a best-effort on the happy path: the prior state is captured
// before any change and persisted to disk, so even a crashed run can be recovered
// on the next startup. See specs/010-frictionless-launch.md.
//
// It shells out to Apple's `networksetup` and `route` behind an injected Runner
// (mirroring internal/sim), so the lifecycle is testable without touching real
// system configuration. macOS-only; the non-Darwin build compiles to a no-op.
package sysproxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Runner executes a command and returns its combined output. Injected so the
// takeover logic can be exercised without mutating real system state; ExecRunner
// is the production implementation. Same shape as sim.Runner deliberately.
type Runner func(ctx context.Context, name string, args ...string) ([]byte, error)

// ExecRunner is the production Runner: it shells out and captures stdout+stderr
// together, so networksetup's diagnostics (notably its admin-rights complaint)
// survive into the error we classify.
func ExecRunner(ctx context.Context, name string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).CombinedOutput()
}

// ErrNoPrimaryService means no network service backs the default route (the Mac
// is offline, or has no routable service). There is nothing to take over; the
// caller reports it and keeps serving.
var ErrNoPrimaryService = errors.New("sysproxy: no primary network service (offline?)")

// ErrNeedAdmin means networksetup refused the change for lack of administrator
// rights. The caller degrades to "set it manually" rather than prompting — we
// never sudo and never cache a password.
var ErrNeedAdmin = errors.New("sysproxy: setting the system proxy requires administrator rights")

// Setting is one proxy's captured state: whether it was enabled, and the host and
// port stored for it. A disabled proxy still carries a stored host/port, so all
// three are snapshotted and restored — restoring only the enabled flag would
// silently drop a host the user had saved.
type Setting struct {
	Enabled bool   `json:"enabled"`
	Host    string `json:"host"`
	Port    int    `json:"port"`
}

// Config is the captured proxy state of one network service: the Web (HTTP) and
// Secure Web (HTTPS) proxies, plus which service it belongs to so restore targets
// exactly what was touched.
type Config struct {
	Service string  `json:"service"`
	Web     Setting `json:"web"`
	Secure  Setting `json:"secure"`
}

// Manager owns the takeover lifecycle for one session: it snapshots the primary
// service's proxy config, points it at proxysim, and restores it on shutdown.
// Restore is idempotent, so the signal path and a deferred call cannot double
// apply.
type Manager struct {
	run          Runner
	snapshotPath string

	mu       sync.Mutex
	service  string // the service taken over, for reporting; set by Apply
	restored bool
}

// NewManager returns a Manager that persists its pre-takeover snapshot to
// snapshotPath (e.g. ~/.proxysim/sysproxy.json), the on-disk safety net that lets
// a later run recover a takeover a crash left in place.
func NewManager(run Runner, snapshotPath string) *Manager {
	return &Manager{run: run, snapshotPath: snapshotPath}
}

// noop is the restore returned when nothing was changed (unsupported platform,
// offline, or a failed Apply): calling it is harmless.
func noop() error { return nil }

// Apply snapshots the primary service's current proxy config, persists it, and
// points both the Web and Secure Web proxies at 127.0.0.1:port. It returns a
// restore func that re-applies the captured state and is safe to call more than
// once. On any failure it rolls back whatever it changed and returns a wrapped
// error (ErrNeedAdmin / ErrNoPrimaryService are distinguished) so main can
// degrade to "set it yourself" and keep serving; the returned restore is then a
// no-op.
func (m *Manager) Apply(ctx context.Context, port int) (func() error, error) {
	if !supported {
		return noop, nil
	}

	service, err := primaryService(ctx, m.run)
	if err != nil {
		return noop, err
	}

	cfg, err := snapshot(ctx, m.run, service)
	if err != nil {
		return noop, fmt.Errorf("reading system proxy config for %q: %w", service, err)
	}

	// Persist before mutating: a crash between here and the set leaves a snapshot
	// equal to the current state, so recovery is a harmless no-op restore.
	if err := m.persist(cfg); err != nil {
		return noop, err
	}

	if err := setProxy(ctx, m.run, service, port); err != nil {
		// Roll back anything we managed to set, and drop the snapshot so the next
		// run does not "recover" a state we never fully applied.
		_ = restoreConfig(ctx, m.run, cfg)
		_ = m.clearSnapshot()
		return noop, err
	}

	m.mu.Lock()
	m.service = service
	m.mu.Unlock()

	return m.makeRestore(cfg), nil
}

// Recover restores and clears a snapshot a previous run left behind — the case a
// crash or kill -9 could not clean up itself. Called once at startup before
// Apply. A missing snapshot is a no-op; a restore failure keeps the file so a
// later run can retry.
func (m *Manager) Recover(ctx context.Context) error {
	if !supported {
		return nil
	}
	cfg, ok, err := m.loadSnapshot()
	if err != nil {
		return err
	}
	if !ok {
		return nil
	}
	if err := restoreConfig(ctx, m.run, cfg); err != nil {
		return fmt.Errorf("recovering system proxy for %q: %w", cfg.Service, err)
	}
	return m.clearSnapshot()
}

// Service reports the network service Apply took over, for a status line. Empty
// until a successful Apply.
func (m *Manager) Service() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.service
}

// makeRestore returns the idempotent restore closure: it re-applies cfg exactly
// once and then clears the snapshot. It builds its own short-lived context so it
// works from a deferred call after the serving context is already cancelled.
func (m *Manager) makeRestore(cfg Config) func() error {
	return func() error {
		m.mu.Lock()
		if m.restored {
			m.mu.Unlock()
			return nil
		}
		m.restored = true
		m.mu.Unlock()

		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := restoreConfig(ctx, m.run, cfg); err != nil {
			return fmt.Errorf("restoring system proxy for %q: %w", cfg.Service, err)
		}
		return m.clearSnapshot()
	}
}

func (m *Manager) persist(cfg Config) error {
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("marshalling proxy snapshot: %w", err)
	}
	// 0600, same discipline as the CA key: it records system state, not a secret,
	// but it lives beside the CA and there is no reason for it to be world-readable.
	if err := os.WriteFile(m.snapshotPath, data, 0o600); err != nil {
		return fmt.Errorf("writing proxy snapshot %s: %w", m.snapshotPath, err)
	}
	return nil
}

func (m *Manager) loadSnapshot() (Config, bool, error) {
	data, err := os.ReadFile(m.snapshotPath)
	if errors.Is(err, os.ErrNotExist) {
		return Config{}, false, nil
	}
	if err != nil {
		return Config{}, false, fmt.Errorf("reading proxy snapshot %s: %w", m.snapshotPath, err)
	}
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return Config{}, false, fmt.Errorf("parsing proxy snapshot %s: %w", m.snapshotPath, err)
	}
	return cfg, true, nil
}

func (m *Manager) clearSnapshot() error {
	if err := os.Remove(m.snapshotPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("removing proxy snapshot %s: %w", m.snapshotPath, err)
	}
	return nil
}

// primaryService resolves the network service backing the default route. It reads
// the default interface from `route -n get default`, then maps that interface to
// a service name via `networksetup -listnetworkserviceorder`. A missing default
// route means offline, reported as ErrNoPrimaryService.
func primaryService(ctx context.Context, run Runner) (string, error) {
	out, err := run(ctx, "route", "-n", "get", "default")
	if err != nil {
		// A non-zero exit here means there is no default route to read — offline,
		// or no routable service. That is "nothing to take over", not a failure.
		return "", ErrNoPrimaryService
	}
	iface, err := parseDefaultInterface(out)
	if err != nil {
		return "", err
	}

	order, err := run(ctx, "networksetup", "-listnetworkserviceorder")
	if err != nil {
		return "", fmt.Errorf("listing network services: %w: %s", err, strings.TrimSpace(string(order)))
	}
	service, err := serviceForInterface(order, iface)
	if err != nil {
		return "", err
	}
	return service, nil
}

// parseDefaultInterface pulls the interface name (e.g. "en0") out of
// `route -n get default` output, whose relevant line reads "  interface: en0".
func parseDefaultInterface(out []byte) (string, error) {
	for line := range strings.SplitSeq(string(out), "\n") {
		key, val, ok := strings.Cut(line, ":")
		if ok && strings.TrimSpace(key) == "interface" {
			if iface := strings.TrimSpace(val); iface != "" {
				return iface, nil
			}
		}
	}
	return "", ErrNoPrimaryService
}

// serviceForInterface maps a BSD interface name to its user-facing service name
// using `networksetup -listnetworkserviceorder`, whose output pairs a header line
// "(N) Service Name" with a detail line "(Hardware Port: ..., Device: enX)".
// Service names contain spaces ("iPhone USB", "USB 10/100/1000 LAN"), which is
// why the whole name is returned verbatim to be passed as a single argv element.
func serviceForInterface(out []byte, iface string) (string, error) {
	var name string
	for raw := range strings.SplitSeq(string(out), "\n") {
		line := strings.TrimSpace(strings.TrimRight(raw, "\r"))
		switch {
		case line == "":
			name = ""
		case strings.HasPrefix(line, "(Hardware Port:"):
			if deviceOf(line) == iface && name != "" {
				return name, nil
			}
		case strings.HasPrefix(line, "("):
			name = headerName(line)
		}
	}
	return "", fmt.Errorf("no network service found for interface %q", iface)
}

// headerName extracts the service name from a "(N) Name" or "(*) Name" header.
func headerName(line string) string {
	if _, after, ok := strings.Cut(line, ") "); ok {
		return strings.TrimSpace(after)
	}
	return ""
}

// deviceOf extracts the device from a "(Hardware Port: X, Device: enY)" line.
func deviceOf(line string) string {
	_, after, ok := strings.Cut(line, "Device: ")
	if !ok {
		return ""
	}
	rest := strings.TrimSpace(after)
	return strings.TrimSpace(strings.TrimSuffix(rest, ")"))
}

// snapshot reads the current Web and Secure Web proxy settings of a service.
func snapshot(ctx context.Context, run Runner, service string) (Config, error) {
	web, err := getSetting(ctx, run, service, "webproxy")
	if err != nil {
		return Config{}, err
	}
	secure, err := getSetting(ctx, run, service, "securewebproxy")
	if err != nil {
		return Config{}, err
	}
	return Config{Service: service, Web: web, Secure: secure}, nil
}

// getSetting runs `networksetup -get<kind> <service>` and parses the result.
func getSetting(ctx context.Context, run Runner, service, kind string) (Setting, error) {
	out, err := run(ctx, "networksetup", "-get"+kind, service)
	if err != nil {
		return Setting{}, fmt.Errorf("reading %s for %q: %w: %s", kind, service, err, strings.TrimSpace(string(out)))
	}
	return parseProxySetting(out), nil
}

// parseProxySetting reads networksetup's key/value proxy report:
//
//	Enabled: No
//	Server:
//	Port: 0
//	Authenticated Proxy Enabled: 0
func parseProxySetting(out []byte) Setting {
	var s Setting
	for line := range strings.SplitSeq(string(out), "\n") {
		key, val, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		val = strings.TrimSpace(val)
		switch strings.TrimSpace(key) {
		case "Enabled":
			s.Enabled = strings.EqualFold(val, "Yes")
		case "Server":
			s.Host = val
		case "Port":
			if n, err := strconv.Atoi(val); err == nil {
				s.Port = n
			}
		}
	}
	return s
}

// setProxy points both the Web and Secure Web proxies of a service at
// 127.0.0.1:port. `-set…proxy` also enables the proxy, which is what we want. The
// Simulator inherits the system proxy, so we set both to be safe (see spec 010's
// open question).
func setProxy(ctx context.Context, run Runner, service string, port int) error {
	p := strconv.Itoa(port)
	if out, err := run(ctx, "networksetup", "-setsecurewebproxy", service, "127.0.0.1", p); err != nil {
		return classifySetErr(err, out)
	}
	if out, err := run(ctx, "networksetup", "-setwebproxy", service, "127.0.0.1", p); err != nil {
		return classifySetErr(err, out)
	}
	return nil
}

// classifySetErr maps a networksetup set failure to ErrNeedAdmin when it is a
// rights problem, so main can print an actionable message instead of a raw error.
func classifySetErr(err error, out []byte) error {
	msg := strings.TrimSpace(string(out))
	low := strings.ToLower(msg)
	if strings.Contains(low, "administrator") || strings.Contains(low, "permission denied") {
		return fmt.Errorf("%w: %s", ErrNeedAdmin, msg)
	}
	return fmt.Errorf("setting system proxy: %w: %s", err, msg)
}

// restoreConfig re-applies a captured Config verbatim: host/port then enabled
// state, for both proxies. Restoring the host first (which enables the proxy) and
// then setting the state reproduces a previously-disabled-with-saved-host config
// exactly.
func restoreConfig(ctx context.Context, run Runner, cfg Config) error {
	if err := restoreSetting(ctx, run, cfg.Service, "securewebproxy", cfg.Secure); err != nil {
		return err
	}
	return restoreSetting(ctx, run, cfg.Service, "webproxy", cfg.Web)
}

func restoreSetting(ctx context.Context, run Runner, service, kind string, s Setting) error {
	// Only re-set host/port if one was stored; `-set…proxy` rejects an empty host.
	if s.Host != "" {
		if out, err := run(ctx, "networksetup", "-set"+kind, service, s.Host, strconv.Itoa(s.Port)); err != nil {
			return fmt.Errorf("restoring %s host for %q: %w: %s", kind, service, err, strings.TrimSpace(string(out)))
		}
	}
	state := "off"
	if s.Enabled {
		state = "on"
	}
	if out, err := run(ctx, "networksetup", "-set"+kind+"state", service, state); err != nil {
		return fmt.Errorf("restoring %s state for %q: %w: %s", kind, service, err, strings.TrimSpace(string(out)))
	}
	return nil
}
