package sysproxy

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
)

// fakeRunner records every command and answers via a handler, standing in for
// networksetup/route so no test mutates real system state.
type fakeRunner struct {
	mu      sync.Mutex
	calls   [][]string
	handler func(name string, args []string) ([]byte, error)
}

func (f *fakeRunner) run(_ context.Context, name string, args ...string) ([]byte, error) {
	f.mu.Lock()
	f.calls = append(f.calls, append([]string{name}, args...))
	f.mu.Unlock()
	if f.handler != nil {
		return f.handler(name, args)
	}
	return nil, nil
}

// recorded returns a copy of the recorded argv slices.
func (f *fakeRunner) recorded() [][]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([][]string, len(f.calls))
	copy(out, f.calls)
	return out
}

func hasCall(calls [][]string, want ...string) bool {
	for _, c := range calls {
		if len(c) != len(want) {
			continue
		}
		match := true
		for i := range want {
			if c[i] != want[i] {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}

func countCallsWith(calls [][]string, arg string) int {
	n := 0
	for _, c := range calls {
		if slices.Contains(c, arg) {
			n++
		}
	}
	return n
}

const serviceOrder = `An asterisk (*) denotes that a network service is disabled.
(1) Wi-Fi
(Hardware Port: Wi-Fi, Device: en0)

(2) USB 10/100/1000 LAN
(Hardware Port: USB 10/100/1000 LAN, Device: en5)
`

// baseHandler answers route + service-order for a healthy Wi-Fi/en0 primary, and
// delegates proxy get/set to the given hooks.
func baseHandler(getSecure, getWeb string, setErr error) func(name string, args []string) ([]byte, error) {
	return func(name string, args []string) ([]byte, error) {
		switch {
		case name == "route":
			return []byte("   gateway: 192.168.1.1\n  interface: en0\n"), nil
		case name == "networksetup" && len(args) >= 1 && args[0] == "-listnetworkserviceorder":
			return []byte(serviceOrder), nil
		case name == "networksetup" && len(args) >= 1 && args[0] == "-getsecurewebproxy":
			return []byte(getSecure), nil
		case name == "networksetup" && len(args) >= 1 && args[0] == "-getwebproxy":
			return []byte(getWeb), nil
		case name == "networksetup" && len(args) >= 1 && (args[0] == "-setsecurewebproxy" || args[0] == "-setwebproxy"):
			if setErr != nil {
				return []byte("You must have administrator access to enable this."), setErr
			}
			return nil, nil
		default:
			return nil, nil
		}
	}
}

func TestApplySetsAndRestoresRoundTrip(t *testing.T) {
	f := &fakeRunner{handler: baseHandler(
		"Enabled: Yes\nServer: 10.0.0.9\nPort: 3128\n",
		"Enabled: No\nServer:\nPort: 0\n",
		nil,
	)}
	snap := filepath.Join(t.TempDir(), "sysproxy.json")
	m := NewManager(f.run, snap)

	restore, err := m.Apply(context.Background(), 8888)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if m.Service() != "Wi-Fi" {
		t.Fatalf("Service() = %q, want Wi-Fi", m.Service())
	}

	calls := f.recorded()
	if !hasCall(calls, "networksetup", "-setsecurewebproxy", "Wi-Fi", "127.0.0.1", "8888") {
		t.Errorf("expected secure web proxy set to 127.0.0.1:8888; calls: %v", calls)
	}
	if !hasCall(calls, "networksetup", "-setwebproxy", "Wi-Fi", "127.0.0.1", "8888") {
		t.Errorf("expected web proxy set to 127.0.0.1:8888; calls: %v", calls)
	}

	if err := restore(); err != nil {
		t.Fatalf("restore: %v", err)
	}
	calls = f.recorded()
	// Secure Web was enabled with a saved host: restore both host/port and "on".
	if !hasCall(calls, "networksetup", "-setsecurewebproxy", "Wi-Fi", "10.0.0.9", "3128") {
		t.Errorf("expected secure web proxy restored to 10.0.0.9:3128; calls: %v", calls)
	}
	if !hasCall(calls, "networksetup", "-setsecurewebproxystate", "Wi-Fi", "on") {
		t.Errorf("expected secure web proxy state restored on; calls: %v", calls)
	}
	// Web was disabled with no host: only the state is restored, to "off".
	if !hasCall(calls, "networksetup", "-setwebproxystate", "Wi-Fi", "off") {
		t.Errorf("expected web proxy state restored off; calls: %v", calls)
	}
	if hasCall(calls, "networksetup", "-setwebproxy", "Wi-Fi", "", "0") {
		t.Errorf("must not re-set an empty web proxy host; calls: %v", calls)
	}
}

func TestRestoreIsIdempotent(t *testing.T) {
	f := &fakeRunner{handler: baseHandler(
		"Enabled: No\nServer:\nPort: 0\n",
		"Enabled: No\nServer:\nPort: 0\n",
		nil,
	)}
	m := NewManager(f.run, filepath.Join(t.TempDir(), "sysproxy.json"))

	restore, err := m.Apply(context.Background(), 8888)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}

	before := countCallsWith(f.recorded(), "-setsecurewebproxystate")
	if err := restore(); err != nil {
		t.Fatalf("restore 1: %v", err)
	}
	if err := restore(); err != nil {
		t.Fatalf("restore 2: %v", err)
	}
	after := countCallsWith(f.recorded(), "-setsecurewebproxystate")
	if after-before != 1 {
		t.Fatalf("restore issued setsecurewebproxystate %d times, want exactly 1", after-before)
	}
}

func TestSnapshotPersistsAndRecovers(t *testing.T) {
	snap := filepath.Join(t.TempDir(), "sysproxy.json")
	f := &fakeRunner{handler: baseHandler(
		"Enabled: Yes\nServer: 10.0.0.9\nPort: 3128\n",
		"Enabled: No\nServer:\nPort: 0\n",
		nil,
	)}
	m := NewManager(f.run, snap)
	if _, err := m.Apply(context.Background(), 8888); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	info, err := os.Stat(snap)
	if err != nil {
		t.Fatalf("snapshot not written: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("snapshot perm = %o, want 600", perm)
	}

	// A brand-new Manager (simulating the next run) recovers from the file alone.
	f2 := &fakeRunner{handler: baseHandler("", "", nil)}
	m2 := NewManager(f2.run, snap)
	if err := m2.Recover(context.Background()); err != nil {
		t.Fatalf("Recover: %v", err)
	}
	calls := f2.recorded()
	if !hasCall(calls, "networksetup", "-setsecurewebproxy", "Wi-Fi", "10.0.0.9", "3128") {
		t.Errorf("Recover did not restore captured secure proxy; calls: %v", calls)
	}
	if _, err := os.Stat(snap); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("snapshot should be deleted after successful recover, stat err = %v", err)
	}

	// Missing snapshot: Recover is a clean no-op.
	f3 := &fakeRunner{}
	m3 := NewManager(f3.run, snap)
	if err := m3.Recover(context.Background()); err != nil {
		t.Fatalf("Recover with no snapshot: %v", err)
	}
	if len(f3.recorded()) != 0 {
		t.Errorf("Recover with no snapshot should issue no commands, got %v", f3.recorded())
	}
}

func TestPrimaryServiceHandlesSpacedName(t *testing.T) {
	f := &fakeRunner{handler: func(name string, args []string) ([]byte, error) {
		switch {
		case name == "route":
			return []byte("  interface: en5\n"), nil
		case name == "networksetup" && args[0] == "-listnetworkserviceorder":
			return []byte(serviceOrder), nil
		default:
			return nil, nil
		}
	}}
	svc, err := primaryService(context.Background(), f.run)
	if err != nil {
		t.Fatalf("primaryService: %v", err)
	}
	if svc != "USB 10/100/1000 LAN" {
		t.Fatalf("primaryService = %q, want %q", svc, "USB 10/100/1000 LAN")
	}
}

func TestApplyPassesSpacedServiceAsSingleArg(t *testing.T) {
	f := &fakeRunner{handler: func(name string, args []string) ([]byte, error) {
		switch {
		case name == "route":
			return []byte("  interface: en5\n"), nil
		case name == "networksetup" && args[0] == "-listnetworkserviceorder":
			return []byte(serviceOrder), nil
		case name == "networksetup" && (args[0] == "-getsecurewebproxy" || args[0] == "-getwebproxy"):
			return []byte("Enabled: No\nServer:\nPort: 0\n"), nil
		default:
			return nil, nil
		}
	}}
	m := NewManager(f.run, filepath.Join(t.TempDir(), "sysproxy.json"))
	if _, err := m.Apply(context.Background(), 8888); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !hasCall(f.recorded(), "networksetup", "-setsecurewebproxy", "USB 10/100/1000 LAN", "127.0.0.1", "8888") {
		t.Fatalf("service with spaces must be one argv element; calls: %v", f.recorded())
	}
}

func TestApplyPermissionDeniedDegrades(t *testing.T) {
	f := &fakeRunner{handler: baseHandler(
		"Enabled: No\nServer:\nPort: 0\n",
		"Enabled: No\nServer:\nPort: 0\n",
		errors.New("exit status 1"),
	)}
	snap := filepath.Join(t.TempDir(), "sysproxy.json")
	m := NewManager(f.run, snap)

	restore, err := m.Apply(context.Background(), 8888)
	if !errors.Is(err, ErrNeedAdmin) {
		t.Fatalf("Apply err = %v, want ErrNeedAdmin", err)
	}
	if restore == nil {
		t.Fatal("Apply must return a non-nil (no-op) restore even on error")
	}
	if err := restore(); err != nil {
		t.Errorf("no-op restore should succeed, got %v", err)
	}
	// A failed Apply must not leave a snapshot claiming a takeover happened.
	if _, err := os.Stat(snap); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("snapshot should be cleared after failed Apply, stat err = %v", err)
	}
}

func TestApplyOfflineIsNoOp(t *testing.T) {
	f := &fakeRunner{handler: func(name string, args []string) ([]byte, error) {
		if name == "route" {
			return []byte("route: writing to routing socket: not in table\n"), errors.New("exit status 1")
		}
		return nil, nil
	}}
	m := NewManager(f.run, filepath.Join(t.TempDir(), "sysproxy.json"))

	restore, err := m.Apply(context.Background(), 8888)
	if !errors.Is(err, ErrNoPrimaryService) {
		t.Fatalf("Apply err = %v, want ErrNoPrimaryService", err)
	}
	if err := restore(); err != nil {
		t.Errorf("no-op restore should succeed, got %v", err)
	}
	for _, c := range f.recorded() {
		if len(c) > 1 && c[0] == "networksetup" && strings.HasPrefix(c[1], "-set") {
			t.Errorf("offline Apply must not set anything; saw %v", c)
		}
	}
}

func TestParseProxySetting(t *testing.T) {
	got := parseProxySetting([]byte("Enabled: Yes\nServer: 10.0.0.9\nPort: 3128\nAuthenticated Proxy Enabled: 0\n"))
	if !got.Enabled || got.Host != "10.0.0.9" || got.Port != 3128 {
		t.Fatalf("parseProxySetting = %+v", got)
	}
	off := parseProxySetting([]byte("Enabled: No\nServer:\nPort: 0\n"))
	if off.Enabled || off.Host != "" || off.Port != 0 {
		t.Fatalf("parseProxySetting(off) = %+v", off)
	}
}
