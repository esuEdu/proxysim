package xcode

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestArmedRoundTrip(t *testing.T) {
	dir := t.TempDir()

	if armed, err := Armed(dir); err != nil || armed {
		t.Fatalf("fresh dir: Armed = %v, %v; want false, nil", armed, err)
	}

	if err := SetArmed(dir, true); err != nil {
		t.Fatalf("SetArmed(true): %v", err)
	}
	if armed, err := Armed(dir); err != nil || !armed {
		t.Fatalf("after arming: Armed = %v, %v; want true, nil", armed, err)
	}

	if err := SetArmed(dir, false); err != nil {
		t.Fatalf("SetArmed(false): %v", err)
	}
	if armed, err := Armed(dir); err != nil || armed {
		t.Fatalf("after disarming: Armed = %v, %v; want false, nil", armed, err)
	}
	// Disarming again is idempotent, not an error.
	if err := SetArmed(dir, false); err != nil {
		t.Fatalf("SetArmed(false) again: %v", err)
	}
}

func TestArmedFileMode(t *testing.T) {
	dir := t.TempDir()
	if err := SetArmed(dir, true); err != nil {
		t.Fatalf("SetArmed(true): %v", err)
	}
	info, err := statFile(ArmedPath(dir))
	if err != nil {
		t.Fatalf("stat armed flag: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("armed flag mode = %o; want 600", perm)
	}
}

func TestFromEnv(t *testing.T) {
	t.Run("full environment", func(t *testing.T) {
		env := map[string]string{
			"PRODUCT_BUNDLE_IDENTIFIER": "com.example.app",
			"TARGET_DEVICE_IDENTIFIER":  "UDID-123",
		}
		got, err := FromEnv(envGetter(env), failFallback(t))
		if err != nil {
			t.Fatalf("FromEnv: %v", err)
		}
		want := Target{BundleID: "com.example.app", DeviceUDID: "UDID-123"}
		if got != want {
			t.Fatalf("FromEnv = %+v; want %+v", got, want)
		}
	})

	t.Run("missing device falls back to booted", func(t *testing.T) {
		env := map[string]string{"PRODUCT_BUNDLE_IDENTIFIER": "com.example.app"}
		got, err := FromEnv(envGetter(env), func() (string, error) { return "BOOTED-UDID", nil })
		if err != nil {
			t.Fatalf("FromEnv: %v", err)
		}
		if got.DeviceUDID != "BOOTED-UDID" {
			t.Fatalf("device = %q; want fallback BOOTED-UDID", got.DeviceUDID)
		}
	})

	t.Run("fallback failure is tolerated", func(t *testing.T) {
		env := map[string]string{"PRODUCT_BUNDLE_IDENTIFIER": "com.example.app"}
		got, err := FromEnv(envGetter(env), func() (string, error) { return "", errors.New("no sim") })
		if err != nil {
			t.Fatalf("FromEnv should not error on fallback failure: %v", err)
		}
		if got.BundleID != "com.example.app" || got.DeviceUDID != "" {
			t.Fatalf("got %+v; want bundle set, device empty", got)
		}
	})

	t.Run("missing bundle id is a typed error", func(t *testing.T) {
		_, err := FromEnv(envGetter(map[string]string{}), failFallback(t))
		if !errors.Is(err, ErrNoBundleID) {
			t.Fatalf("err = %v; want ErrNoBundleID", err)
		}
	})
}

func TestSendTargetNoInstance(t *testing.T) {
	dir := t.TempDir()
	err := SendTarget(dir, Target{BundleID: "com.example.app"})
	if !errors.Is(err, ErrNoInstance) {
		t.Fatalf("SendTarget with no listener: err = %v; want ErrNoInstance", err)
	}
}

func TestControlRetarget(t *testing.T) {
	dir := t.TempDir()
	srv, err := Listen(dir)
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer srv.Close()

	got := make(chan Target, 1)
	go srv.Serve(func(tg Target) { got <- tg })

	want := Target{BundleID: "com.example.app", DeviceUDID: "UDID-9"}
	if err := SendTarget(dir, want); err != nil {
		t.Fatalf("SendTarget to live instance: %v", err)
	}

	select {
	case tg := <-got:
		if tg != want {
			t.Fatalf("received %+v; want %+v", tg, want)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for retarget message")
	}
}

func TestListenClearsStaleSocket(t *testing.T) {
	dir := t.TempDir()
	// Simulate a prior instance that died leaving its socket file behind.
	srv1, err := Listen(dir)
	if err != nil {
		t.Fatalf("first Listen: %v", err)
	}
	// Close only the listener, leaving the file (Close removes it, so drop the file
	// path directly by re-listening: a second Listen must succeed regardless).
	srv1.ln.Close() // leaves the socket file on disk

	srv2, err := Listen(dir)
	if err != nil {
		t.Fatalf("second Listen over stale socket: %v", err)
	}
	srv2.Close()
}

func TestWatchTeardownOnAppExit(t *testing.T) {
	// The app is up, then a retarget points at an app that is not running, so the
	// probe reports absence and Watch returns — no signal, no post-action involved.
	var mu sync.Mutex
	target := Target{BundleID: "running", DeviceUDID: "d"}
	current := func() Target {
		mu.Lock()
		defer mu.Unlock()
		return target
	}
	probe := func(_ context.Context, tg Target) (bool, error) {
		return tg.BundleID == "running", nil
	}

	done := make(chan struct{})
	go func() {
		Watch(context.Background(), probe, current, time.Millisecond, 10*time.Millisecond, time.Second)
		close(done)
	}()

	// Let it observe the app running, then retarget to an absent app.
	time.Sleep(20 * time.Millisecond)
	mu.Lock()
	target = Target{BundleID: "gone", DeviceUDID: "d"}
	mu.Unlock()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Watch did not tear down after the app disappeared")
	}
}

func TestWatchTeardownWhenAppNeverLaunches(t *testing.T) {
	// An armed build whose app never comes up (e.g. build failed) must still tear
	// down so the system proxy is not left dangling — bounded by startupGrace.
	probe := func(context.Context, Target) (bool, error) { return false, nil }
	current := func() Target { return Target{BundleID: "x", DeviceUDID: "d"} }

	done := make(chan struct{})
	go func() {
		Watch(context.Background(), probe, current, time.Millisecond, time.Second, 20*time.Millisecond)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Watch did not give up waiting for a launch")
	}
}

func TestWatchRespectsContextCancel(t *testing.T) {
	// A still-running app plus a cancelled context: Watch returns because of the
	// cancel, not a teardown decision.
	probe := func(context.Context, Target) (bool, error) { return true, nil }
	current := func() Target { return Target{BundleID: "x", DeviceUDID: "d"} }

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		Watch(ctx, probe, current, time.Millisecond, time.Second, time.Second)
		close(done)
	}()
	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Watch did not return on context cancel")
	}
}

func TestSimAppProbe(t *testing.T) {
	target := Target{BundleID: "com.example.app", DeviceUDID: "UDID-1"}

	t.Run("running", func(t *testing.T) {
		run := func(_ context.Context, name string, args ...string) ([]byte, error) {
			return []byte("123\t0\tUIKitApplication:com.example.app[0x1234]\n"), nil
		}
		ok, err := SimAppProbe(run)(context.Background(), target)
		if err != nil || !ok {
			t.Fatalf("running probe = %v, %v; want true, nil", ok, err)
		}
	})

	t.Run("not running", func(t *testing.T) {
		run := func(context.Context, string, ...string) ([]byte, error) {
			return []byte("123\t0\tcom.apple.somethingelse\n"), nil
		}
		ok, err := SimAppProbe(run)(context.Background(), target)
		if err != nil || ok {
			t.Fatalf("absent probe = %v, %v; want false, nil", ok, err)
		}
	})

	t.Run("runner error is unknown", func(t *testing.T) {
		run := func(context.Context, string, ...string) ([]byte, error) {
			return nil, errors.New("simctl blew up")
		}
		if _, err := SimAppProbe(run)(context.Background(), target); err == nil {
			t.Fatal("want error from failing runner")
		}
	})

	t.Run("incomplete target is unknown", func(t *testing.T) {
		run := func(context.Context, string, ...string) ([]byte, error) {
			t.Fatal("runner should not be called for an incomplete target")
			return nil, nil
		}
		if _, err := SimAppProbe(run)(context.Background(), Target{BundleID: "only-bundle"}); err == nil {
			t.Fatal("want error for missing device")
		}
	})
}

func TestHookSnippet(t *testing.T) {
	snip := HookSnippet("/Applications/proxysim.app/Contents/MacOS/proxysim")
	for _, want := range []string{
		"-xcode-run",
		"&",               // backgrounded, non-blocking
		">/dev/null 2>&1", // output discarded
		"proxysim.app",    // absolute path preserved
	} {
		if !strings.Contains(snip, want) {
			t.Fatalf("snippet missing %q:\n%s", want, snip)
		}
	}

	// A path with spaces must survive as a single shell token (quoted).
	spaced := HookSnippet("/Users/me/My Tools/proxysim")
	if !strings.Contains(spaced, `"/Users/me/My Tools/proxysim"`) {
		t.Fatalf("spaced path not quoted:\n%s", spaced)
	}
}

// --- helpers ---

func statFile(path string) (os.FileInfo, error) { return os.Stat(path) }

func envGetter(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func failFallback(t *testing.T) func() (string, error) {
	return func() (string, error) {
		t.Helper()
		t.Fatal("bootedFallback should not be called")
		return "", nil
	}
}
