package origin

import "testing"

// simExecPath is a representative booted-simulator app binary path.
const simExecPath = "/Users/dev/Library/Developer/CoreSimulator/Devices/E1B2-DEAD-BEEF/data/Containers/Bundle/Application/AAAA-BBBB/MyApp.app/MyApp"

func TestSimParts(t *testing.T) {
	cases := []struct {
		name       string
		path       string
		wantOK     bool
		wantUDID   string
		wantAppDir string
	}{
		{
			name:       "simulator app in a bundle",
			path:       simExecPath,
			wantOK:     true,
			wantUDID:   "E1B2-DEAD-BEEF",
			wantAppDir: "/Users/dev/Library/Developer/CoreSimulator/Devices/E1B2-DEAD-BEEF/data/Containers/Bundle/Application/AAAA-BBBB/MyApp.app",
		},
		{
			name:     "simulator system daemon, not in a .app",
			path:     "/Users/dev/Library/Developer/CoreSimulator/Devices/E1B2-DEAD-BEEF/data/usr/libexec/somebd",
			wantOK:   true,
			wantUDID: "E1B2-DEAD-BEEF",
		},
		{name: "host process", path: "/Applications/Safari.app/Contents/MacOS/Safari", wantOK: false},
		{name: "plain binary", path: "/usr/bin/curl", wantOK: false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			udid, appDir, ok := simParts(c.path)
			if ok != c.wantOK {
				t.Fatalf("ok = %v, want %v", ok, c.wantOK)
			}
			if ok && udid != c.wantUDID {
				t.Errorf("udid = %q, want %q", udid, c.wantUDID)
			}
			if ok && appDir != c.wantAppDir {
				t.Errorf("appDir = %q, want %q", appDir, c.wantAppDir)
			}
		})
	}
}

func TestClassifyMarksSimulator(t *testing.T) {
	// A non-existent path means bundleID (via plutil) yields "", which is fine —
	// classification of Simulator/UDID must not depend on reading Info.plist.
	p := classify(1234, simExecPath)
	if !p.Simulator || p.DeviceUDID != "E1B2-DEAD-BEEF" || p.PID != 1234 {
		t.Errorf("classify simulator = %+v", p)
	}
	host := classify(5, "/usr/bin/curl")
	if host.Simulator || host.DeviceUDID != "" {
		t.Errorf("classify host = %+v, want non-simulator", host)
	}
}

func TestBuildFilter(t *testing.T) {
	if f := BuildFilter(false, nil); f.Active() {
		t.Error("empty flags should build an inactive filter")
	}
	if f := BuildFilter(true, nil); !f.Active() || !f.OnlySim {
		t.Errorf("-only-sim should build OnlySim filter, got %+v", f)
	}
	// Naming an app implies simulator-only.
	f := BuildFilter(false, []string{" com.example.A ", "", "com.example.B"})
	if !f.Active() || !f.OnlySim {
		t.Errorf("-app should imply OnlySim, got %+v", f)
	}
	if !f.Apps["com.example.A"] || !f.Apps["com.example.B"] || len(f.Apps) != 2 {
		t.Errorf("apps = %v, want A and B (trimmed, blanks dropped)", f.Apps)
	}
}

func TestFilterMatch(t *testing.T) {
	simA := Process{Simulator: true, BundleID: "com.example.A"}
	simB := Process{Simulator: true, BundleID: "com.example.B"}
	host := Process{}

	zero := Filter{}
	if !zero.Match(simA) || !zero.Match(host) {
		t.Error("zero filter must match everything (feature off)")
	}

	onlySim := Filter{OnlySim: true}
	if !onlySim.Match(simA) || onlySim.Match(host) {
		t.Error("-only-sim must match simulator processes only")
	}

	appA := BuildFilter(false, []string{"com.example.A"})
	if !appA.Match(simA) {
		t.Error("-app com.example.A must match app A")
	}
	if appA.Match(simB) {
		t.Error("-app com.example.A must not match app B")
	}
	if appA.Match(host) {
		t.Error("-app must never match a host process")
	}
}
