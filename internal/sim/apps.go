package sim

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
)

// App is one installed simulator app, as offered in the UI's picker: the bundle
// id the origin filter matches on, and a human name to show.
type App struct {
	BundleID string `json:"bundleID"`
	Name     string `json:"name"`
}

// InstalledApps lists the user-installed apps on the given simulator (by UDID),
// for the UI to offer as intercept targets (spec 011). It returns only apps whose
// ApplicationType is "User" — the developer's own apps, not the dozens of bundled
// system apps — so the dropdown is the short list they recognise.
//
// simctl reports apps as an old-style plist (no JSON option), so the output is
// converted with plutil, exactly as bundle ids are read in internal/origin. Both
// shell-outs go through the injected Runner, so enumeration is testable without a
// real simulator.
func InstalledApps(ctx context.Context, run Runner, udid string) ([]App, error) {
	if udid == "" {
		return nil, errors.New("no simulator UDID given")
	}
	plist, err := run(ctx, "xcrun", "simctl", "listapps", udid)
	if err != nil {
		if errors.Is(err, exec.ErrNotFound) {
			return nil, fmt.Errorf("xcrun/simctl not available: install the Xcode command-line tools (xcode-select --install)")
		}
		return nil, fmt.Errorf("listing apps on simulator %s: %w: %s", udid, err, strings.TrimSpace(string(plist)))
	}

	data, err := plistToJSON(ctx, run, plist)
	if err != nil {
		return nil, fmt.Errorf("converting app list for simulator %s: %w", udid, err)
	}
	return parseApps(data)
}

// plistToJSON converts an old-style plist to JSON via plutil. plutil reads a file
// or stdin; the injected Runner has no stdin, so the bytes go through a temp file.
func plistToJSON(ctx context.Context, run Runner, plist []byte) ([]byte, error) {
	tmp, err := os.CreateTemp("", "proxysim-listapps-*.plist")
	if err != nil {
		return nil, fmt.Errorf("creating temp file: %w", err)
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(plist); err != nil {
		tmp.Close()
		return nil, fmt.Errorf("writing temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return nil, fmt.Errorf("closing temp file: %w", err)
	}

	out, err := run(ctx, "plutil", "-convert", "json", "-o", "-", tmp.Name())
	if err != nil {
		return nil, fmt.Errorf("plutil: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return out, nil
}

// parseApps reads the plutil-converted JSON of `simctl listapps`: a top-level
// object keyed by bundle id, each value an app's Info.plist-derived fields. It
// keeps only User apps and resolves a display name (display → bundle name → id).
func parseApps(data []byte) ([]App, error) {
	var raw map[string]struct {
		ApplicationType     string `json:"ApplicationType"`
		CFBundleIdentifier  string `json:"CFBundleIdentifier"`
		CFBundleDisplayName string `json:"CFBundleDisplayName"`
		CFBundleName        string `json:"CFBundleName"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("parsing app list: %w", err)
	}

	apps := make([]App, 0, len(raw))
	for key, info := range raw {
		if info.ApplicationType != "User" {
			continue
		}
		id := info.CFBundleIdentifier
		if id == "" {
			id = key // the map key is the bundle id when the field is absent
		}
		apps = append(apps, App{BundleID: id, Name: firstNonEmpty(info.CFBundleDisplayName, info.CFBundleName, id)})
	}
	// Deterministic order for a stable dropdown and stable tests: by name, then id.
	sort.Slice(apps, func(i, j int) bool {
		if apps[i].Name != apps[j].Name {
			return apps[i].Name < apps[j].Name
		}
		return apps[i].BundleID < apps[j].BundleID
	})
	return apps, nil
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
