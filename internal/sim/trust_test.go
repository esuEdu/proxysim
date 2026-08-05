package sim

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// fakeRunner records the argv of each call and replies from a scripted table,
// keyed on the subcommand, so the trust logic runs without a real simulator.
type fakeRunner struct {
	listJSON   string     // reply for `simctl list devices -j`
	listErr    error      // error for the list call (e.g. exec.ErrNotFound)
	installErr error      // error for the add-root-cert call, if any
	installOut string     // combined output paired with installErr
	calls      [][]string // captured argv (name + args) per call
}

func (f *fakeRunner) run(ctx context.Context, name string, args ...string) ([]byte, error) {
	f.calls = append(f.calls, append([]string{name}, args...))
	joined := strings.Join(args, " ")
	switch {
	case strings.Contains(joined, "list devices"):
		if f.listErr != nil {
			return nil, f.listErr
		}
		return []byte(f.listJSON), nil
	case strings.Contains(joined, "add-root-cert"):
		if f.installErr != nil {
			return []byte(f.installOut), f.installErr
		}
		return nil, nil
	default:
		return nil, fmt.Errorf("fakeRunner: unexpected command %q", joined)
	}
}

// devicesJSON builds a simctl list payload. Each entry is "udid,name,state".
func devicesJSON(entries ...string) string {
	var b strings.Builder
	b.WriteString(`{"devices":{"com.apple.CoreSimulator.SimRuntime.iOS-17-0":[`)
	for i, e := range entries {
		parts := strings.SplitN(e, ",", 3)
		if i > 0 {
			b.WriteString(",")
		}
		fmt.Fprintf(&b, `{"udid":%q,"name":%q,"state":%q}`, parts[0], parts[1], parts[2])
	}
	b.WriteString("]}}")
	return b.String()
}

// writeCert drops a placeholder ca.crt in a temp dir and returns its path.
func writeCert(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "ca.crt")
	if err := os.WriteFile(p, []byte("-----BEGIN CERTIFICATE-----\n"), 0o600); err != nil {
		t.Fatalf("writing cert: %v", err)
	}
	return p
}

// installArgs returns the argv of the add-root-cert call, or fails.
func installArgs(t *testing.T, f *fakeRunner) []string {
	t.Helper()
	for _, c := range f.calls {
		if strings.Contains(strings.Join(c, " "), "add-root-cert") {
			return c
		}
	}
	t.Fatalf("no add-root-cert call was made; calls=%v", f.calls)
	return nil
}

func TestInstallDefaultSetCommandShape(t *testing.T) {
	cert := writeCert(t)
	f := &fakeRunner{listJSON: devicesJSON("UDID-1,iPhone 15,Booted")}

	if err := InstallRootCert(context.Background(), f.run, cert, "", ""); err != nil {
		t.Fatalf("InstallRootCert: %v", err)
	}

	got := strings.Join(installArgs(t, f), " ")
	want := "xcrun simctl keychain booted add-root-cert " + cert
	if got != want {
		t.Errorf("install argv = %q, want %q", got, want)
	}
}

func TestInstallPreviewsSet(t *testing.T) {
	cert := writeCert(t)
	f := &fakeRunner{listJSON: devicesJSON("UDID-1,iPhone 15,Booted")}

	if err := InstallRootCert(context.Background(), f.run, cert, "previews", ""); err != nil {
		t.Fatalf("InstallRootCert: %v", err)
	}

	got := strings.Join(installArgs(t, f), " ")
	want := "xcrun simctl --set previews keychain booted add-root-cert " + cert
	if got != want {
		t.Errorf("install argv = %q, want %q", got, want)
	}
}

func TestInstallMissingCert(t *testing.T) {
	f := &fakeRunner{listJSON: devicesJSON("UDID-1,iPhone 15,Booted")}
	missing := filepath.Join(t.TempDir(), "nope.crt")

	err := InstallRootCert(context.Background(), f.run, missing, "", "")
	if err == nil {
		t.Fatal("expected error for missing cert, got nil")
	}
	if !strings.Contains(err.Error(), missing) || !strings.Contains(err.Error(), "proxysim") {
		t.Errorf("error should name the file and suggest running proxysim: %v", err)
	}
	if len(f.calls) != 0 {
		t.Errorf("runner must not be called when cert is missing; calls=%v", f.calls)
	}
}

func TestInstallNoXcode(t *testing.T) {
	cert := writeCert(t)
	f := &fakeRunner{listErr: exec.ErrNotFound}

	err := InstallRootCert(context.Background(), f.run, cert, "", "")
	if err == nil {
		t.Fatal("expected error when xcrun is absent, got nil")
	}
	if !strings.Contains(err.Error(), "Xcode") {
		t.Errorf("error should mention Xcode command-line tools, got: %v", err)
	}
	if errors.Is(err, exec.ErrNotFound) {
		t.Errorf("bare exec error should be replaced with a human explanation: %v", err)
	}
}

func TestInstallNoBootedDevice(t *testing.T) {
	cert := writeCert(t)
	f := &fakeRunner{listJSON: devicesJSON("UDID-1,iPhone 15,Shutdown")}

	err := InstallRootCert(context.Background(), f.run, cert, "", "")
	if err == nil {
		t.Fatal("expected error when no device is booted, got nil")
	}
	if !strings.Contains(err.Error(), "boot") {
		t.Errorf("error should tell the user to boot a simulator, got: %v", err)
	}
	for _, c := range f.calls {
		if strings.Contains(strings.Join(c, " "), "add-root-cert") {
			t.Fatalf("no install should be attempted; calls=%v", f.calls)
		}
	}
}

func TestInstallAmbiguousDevice(t *testing.T) {
	cert := writeCert(t)
	json := devicesJSON("UDID-1,iPhone 15,Booted", "UDID-2,iPad Pro,Booted")

	// No -device: ambiguous, must list both and ask for -device.
	f := &fakeRunner{listJSON: json}
	err := InstallRootCert(context.Background(), f.run, cert, "", "")
	if err == nil {
		t.Fatal("expected ambiguity error with two booted devices, got nil")
	}
	for _, want := range []string{"-device", "UDID-1", "UDID-2"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("ambiguity error missing %q: %v", want, err)
		}
	}

	// With -device: install proceeds against the chosen UDID.
	f2 := &fakeRunner{listJSON: json}
	if err := InstallRootCert(context.Background(), f2.run, cert, "", "UDID-2"); err != nil {
		t.Fatalf("InstallRootCert with -device: %v", err)
	}
	got := strings.Join(installArgs(t, f2), " ")
	want := "xcrun simctl keychain UDID-2 add-root-cert " + cert
	if got != want {
		t.Errorf("install argv = %q, want %q", got, want)
	}
}

func TestInstallIdempotent(t *testing.T) {
	cert := writeCert(t)
	f := &fakeRunner{listJSON: devicesJSON("UDID-1,iPhone 15,Booted")}

	for i := range 2 {
		if err := InstallRootCert(context.Background(), f.run, cert, "", ""); err != nil {
			t.Fatalf("install %d failed: %v", i+1, err)
		}
	}
}
