package sim

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// representativeJSON is what plutil emits from `simctl listapps`: a top-level
// object keyed by bundle id, mixing User apps (the developer's) and System apps.
const representativeJSON = `{
  "com.example.MyApp": {
    "ApplicationType": "User",
    "CFBundleIdentifier": "com.example.MyApp",
    "CFBundleDisplayName": "My App",
    "CFBundleName": "MyApp"
  },
  "com.example.NoDisplay": {
    "ApplicationType": "User",
    "CFBundleIdentifier": "com.example.NoDisplay",
    "CFBundleName": "FallbackName"
  },
  "com.example.Bare": {
    "ApplicationType": "User"
  },
  "com.apple.mobilesafari": {
    "ApplicationType": "System",
    "CFBundleIdentifier": "com.apple.mobilesafari",
    "CFBundleDisplayName": "Safari"
  }
}`

// Criterion 8: parseApps keeps only User apps and resolves the name
// display → bundle-name → id.
func TestParseAppsUserOnly(t *testing.T) {
	apps, err := parseApps([]byte(representativeJSON))
	if err != nil {
		t.Fatalf("parseApps: %v", err)
	}
	if len(apps) != 3 {
		t.Fatalf("got %d apps, want 3 (system app dropped): %+v", len(apps), apps)
	}

	byID := map[string]App{}
	for _, a := range apps {
		byID[a.BundleID] = a
	}
	if got := byID["com.example.MyApp"].Name; got != "My App" {
		t.Errorf("MyApp name = %q, want display name %q", got, "My App")
	}
	if got := byID["com.example.NoDisplay"].Name; got != "FallbackName" {
		t.Errorf("NoDisplay name = %q, want bundle name %q", got, "FallbackName")
	}
	if got := byID["com.example.Bare"].Name; got != "com.example.Bare" {
		t.Errorf("Bare name = %q, want bundle id fallback", got)
	}
	if _, ok := byID["com.apple.mobilesafari"]; ok {
		t.Error("system app com.apple.mobilesafari must be excluded")
	}

	// Deterministic order: by name, then bundle id.
	for i := 1; i < len(apps); i++ {
		if apps[i-1].Name > apps[i].Name {
			t.Errorf("apps not sorted by name: %q before %q", apps[i-1].Name, apps[i].Name)
		}
	}
}

// Criterion 8 (failure path): a simctl failure returns a wrapped error naming the
// device, not a bare exec error.
func TestInstalledAppsSimctlError(t *testing.T) {
	run := func(_ context.Context, name string, _ ...string) ([]byte, error) {
		if name == "xcrun" {
			return []byte("No devices are booted."), errors.New("exit status 149")
		}
		return nil, nil
	}
	_, err := InstalledApps(context.Background(), run, "DEAD-BEEF")
	if err == nil {
		t.Fatal("expected an error")
	}
	if got := err.Error(); !strings.Contains(got, "DEAD-BEEF") {
		t.Errorf("error should name the device, got %q", got)
	}
}

// Criterion 7 support: end-to-end through the injected Runner, faking both the
// simctl plist read and the plutil conversion.
func TestInstalledAppsEndToEnd(t *testing.T) {
	run := func(_ context.Context, name string, _ ...string) ([]byte, error) {
		switch name {
		case "xcrun":
			return []byte("{ /* an opaque old-style plist */ }"), nil
		case "plutil":
			return []byte(representativeJSON), nil
		default:
			return nil, nil
		}
	}
	apps, err := InstalledApps(context.Background(), run, "DEAD-BEEF")
	if err != nil {
		t.Fatalf("InstalledApps: %v", err)
	}
	if len(apps) != 3 {
		t.Fatalf("got %d apps, want 3: %+v", len(apps), apps)
	}
}

func TestInstalledAppsRequiresUDID(t *testing.T) {
	if _, err := InstalledApps(context.Background(), nil, ""); err == nil {
		t.Fatal("empty UDID should error before shelling out")
	}
}
