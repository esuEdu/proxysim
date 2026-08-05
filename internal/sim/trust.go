// Package sim automates the one manual step between generating the CA and
// intercepting simulator traffic: installing that CA into a booted simulator's
// trust store. It shells out to Apple's `xcrun simctl`; it reimplements nothing
// Apple provides. See specs/007-simctl-trust.md.
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

// Runner executes a command and returns its combined output. It is injected so
// the trust logic can be exercised without a real simulator or a working Xcode
// install; ExecRunner is the production implementation.
type Runner func(ctx context.Context, name string, args ...string) ([]byte, error)

// ExecRunner is the production Runner: it shells out to the named command and
// captures stdout+stderr together, so simctl's diagnostics survive into errors.
func ExecRunner(ctx context.Context, name string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).CombinedOutput()
}

// Device is a simulator as reported by simctl. Only the fields we need to
// disambiguate and to print friendly errors are kept.
type Device struct {
	UDID    string
	Name    string
	Runtime string
}

// InstallRootCert installs certPath into a booted simulator's trust store,
// targeting the given set ("" for the default set; "previews" for Xcode
// Previews). When several simulators are booted, device selects one by UDID.
//
// It returns actionable errors — not raw simctl output — for the cases we know
// bite users: a missing CA, an absent Xcode toolchain, no booted device, and an
// ambiguous choice among several. Installing an already-trusted CA is harmless,
// so the operation is idempotent.
func InstallRootCert(ctx context.Context, run Runner, certPath, set, device string) error {
	// Validate the cert before shelling out. The only certificate this ever
	// installs is the user's own machine-local CA; a missing file means the CA
	// has not been generated, not that trust failed.
	if _, err := os.Stat(certPath); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("CA certificate %s not found; run proxysim once to generate the CA before trusting it", certPath)
		}
		return fmt.Errorf("reading CA certificate %s: %w", certPath, err)
	}

	booted, err := BootedDevices(ctx, run)
	if err != nil {
		return err
	}

	target, err := selectTarget(booted, device)
	if err != nil {
		return err
	}

	args := simctlArgs(set, target, certPath)
	if out, err := run(ctx, "xcrun", args...); err != nil {
		return fmt.Errorf("installing root cert into simulator: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// BootedDevices lists the currently booted simulators, used both to disambiguate
// among several and to produce a friendly error when none are booted.
func BootedDevices(ctx context.Context, run Runner) ([]Device, error) {
	out, err := run(ctx, "xcrun", "simctl", "list", "devices", "-j")
	if err != nil {
		// A missing xcrun means the Xcode command-line tools are not installed
		// (or this is not macOS at all) — say so, rather than surfacing a bare
		// "executable not found".
		if errors.Is(err, exec.ErrNotFound) {
			return nil, fmt.Errorf("xcrun/simctl not available: install the Xcode command-line tools (xcode-select --install) — trusting the CA needs the iOS Simulator toolchain")
		}
		return nil, fmt.Errorf("listing simulators: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return parseBooted(out)
}

// selectTarget resolves the simctl keychain target ("booted" or a UDID) from the
// booted set and an optional explicit device. simctl's own "booted" keyword is
// only unambiguous with exactly one booted device, so we pass a UDID otherwise.
func selectTarget(booted []Device, device string) (string, error) {
	if device != "" {
		for _, d := range booted {
			if d.UDID == device {
				return d.UDID, nil
			}
		}
		if len(booted) == 0 {
			return "", fmt.Errorf("device %s is not booted; no simulator is booted", device)
		}
		return "", fmt.Errorf("device %s is not booted; booted devices:\n%s", device, formatDevices(booted))
	}

	switch len(booted) {
	case 0:
		return "", errors.New("no booted simulator; boot one in Xcode (or `xcrun simctl boot <udid>`) and retry")
	case 1:
		return "booted", nil
	default:
		return "", fmt.Errorf("multiple booted simulators; pass -device <udid> to choose one:\n%s", formatDevices(booted))
	}
}

// simctlArgs builds the argv for the install, inserting --set only for a
// non-default set so the common case matches the documented one-liner.
func simctlArgs(set, target, certPath string) []string {
	args := []string{"simctl"}
	if set != "" {
		args = append(args, "--set", set)
	}
	return append(args, "keychain", target, "add-root-cert", certPath)
}

// parseBooted extracts booted devices from `simctl list devices -j`. simctl's
// per-list search terms are unreliable, so we take the full list and filter on
// the authoritative State field ourselves.
func parseBooted(data []byte) ([]Device, error) {
	var list struct {
		Devices map[string][]struct {
			UDID  string `json:"udid"`
			Name  string `json:"name"`
			State string `json:"state"`
		} `json:"devices"`
	}
	if err := json.Unmarshal(data, &list); err != nil {
		return nil, fmt.Errorf("parsing simctl device list: %w", err)
	}

	var booted []Device
	for runtime, devices := range list.Devices {
		for _, d := range devices {
			if d.State == "Booted" {
				booted = append(booted, Device{UDID: d.UDID, Name: d.Name, Runtime: runtime})
			}
		}
	}
	// Deterministic order so disambiguation errors and tests are stable.
	sort.Slice(booted, func(i, j int) bool { return booted[i].UDID < booted[j].UDID })
	return booted, nil
}

// formatDevices renders a device list for a disambiguation error.
func formatDevices(ds []Device) string {
	var b strings.Builder
	for _, d := range ds {
		fmt.Fprintf(&b, "  %s  %s\n", d.UDID, d.Name)
	}
	return strings.TrimRight(b.String(), "\n")
}
