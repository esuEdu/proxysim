// Command proxysim is a local HTTP/HTTPS intercepting proxy for the iOS
// Simulator. It generates a per-machine root CA, mints per-host leaves to
// terminate TLS, and prints each captured exchange to the console.
//
// This file is deliberately thin: flag parsing, wiring, and signal handling.
// The engine lives in internal/ca, internal/proxy, and internal/flow.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"proxysim/internal/ca"
	"proxysim/internal/flow"
	"proxysim/internal/origin"
	"proxysim/internal/proxy"
	"proxysim/internal/sim"
	"proxysim/internal/sysproxy"
	"proxysim/internal/ui"
)

func main() {
	port := flag.Int("port", 8888, "port to listen on (always bound to 127.0.0.1)")
	caDir := flag.String("ca-dir", "~/.proxysim", "directory holding ca.crt and ca.key")
	verbose := flag.Bool("verbose", false, "print request/response headers and bodies, not just a summary line")
	exclude := flag.String("exclude", "", "comma-separated host suffixes to never intercept (blind-tunnel instead)")
	trust := flag.Bool("trust", false, "install the CA into the booted simulator's trust store, then exit (does not serve)")
	trustSet := flag.String("trust-set", "", "simulator set to target (e.g. \"previews\" for Xcode Previews); default set when empty")
	device := flag.String("device", "", "UDID of the booted simulator to target when several are booted")
	ui := flag.Bool("ui", false, "serve the live traffic web UI (loopback only, separate port)")
	uiPort := flag.Int("ui-port", 8889, "port for the web UI (always bound to 127.0.0.1)")
	uiHistory := flag.Int("ui-history", 1000, "number of recent flows the UI keeps for a freshly opened tab")
	onlySim := flag.Bool("only-sim", false, "intercept only connections originating from the iOS Simulator; tunnel the rest (macOS only)")
	app := flag.String("app", "", "comma-separated app bundle ids to intercept exclusively (implies -only-sim; macOS only)")
	systemProxy := flag.Bool("system-proxy", false, "point the macOS system proxy at proxysim on startup and restore it on exit (implies -only-sim; macOS only)")
	noTrust := flag.Bool("no-trust", false, "do not auto-trust the CA in the booted simulator on startup")
	flag.Parse()

	// -trust is a one-shot action: install and exit, never bind a listener.
	if *trust {
		if err := trustCA(*caDir, *trustSet, *device); err != nil {
			log.Fatalf("proxysim: %v", err)
		}
		return
	}

	cfg := config{
		port:        *port,
		caDir:       *caDir,
		verbose:     *verbose,
		exclude:     *exclude,
		trustSet:    *trustSet,
		device:      *device,
		ui:          *ui,
		uiPort:      *uiPort,
		uiHistory:   *uiHistory,
		onlySim:     *onlySim,
		apps:        splitSuffixes(*app),
		systemProxy: *systemProxy,
		noTrust:     *noTrust,
	}
	if err := run(cfg); err != nil {
		log.Fatalf("proxysim: %v", err)
	}
}

// config is the resolved serving configuration, threaded into run so the
// signature stays readable as options accrue.
type config struct {
	port        int
	caDir       string
	verbose     bool
	exclude     string
	trustSet    string
	device      string
	ui          bool
	uiPort      int
	uiHistory   int
	onlySim     bool
	apps        []string
	systemProxy bool
	noTrust     bool
}

// trustCA installs the machine-local CA into a booted simulator's trust store.
// It is deliberately separate from CA lifecycle: it never generates a CA, only
// installs the one the user already has under -ca-dir.
func trustCA(caDir, set, device string) error {
	dir, err := expandPath(caDir)
	if err != nil {
		return err
	}
	certPath := filepath.Join(dir, "ca.crt")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := sim.InstallRootCert(ctx, sim.ExecRunner, certPath, set, device); err != nil {
		return err
	}

	if set != "" {
		fmt.Fprintf(os.Stderr, "proxysim: trusted %s in simulator set %q\n", certPath, set)
	} else {
		fmt.Fprintf(os.Stderr, "proxysim: trusted %s in the booted simulator\n", certPath)
	}
	fmt.Fprintln(os.Stderr, "Trust is lost on `simctl erase`; re-run `proxysim -trust` after resetting a device.")
	return nil
}

// autoTrust installs the CA into the booted simulator on startup so the user does
// not run `-trust` by hand. It is best-effort: any failure (no booted device, no
// Xcode toolchain) is reported and swallowed — the proxy is still useful for curl
// and pre-boot startup, so trust trouble must never stop it serving (spec 010).
func autoTrust(certPath, set, device string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := sim.InstallRootCert(ctx, sim.ExecRunner, certPath, set, device); err != nil {
		fmt.Fprintf(os.Stderr, "proxysim: auto-trust skipped: %v\n", err)
		return
	}
	fmt.Fprintln(os.Stderr, "proxysim: CA trusted in the booted simulator")
}

// bootedDevices and installedApps are the UI's simulator/app enumerators (spec
// 011), thin adapters over internal/sim's production ExecRunner. They are passed
// to the Hub so its control endpoints can populate the sim and app pickers.
func bootedDevices(ctx context.Context) ([]sim.Device, error) {
	return sim.BootedDevices(ctx, sim.ExecRunner)
}

func installedApps(ctx context.Context, udid string) ([]sim.App, error) {
	return sim.InstalledApps(ctx, sim.ExecRunner, udid)
}

// setupSystemProxy recovers any stale takeover a previous run left behind, then
// points the macOS system proxy at proxysim and returns an idempotent restore.
// On a non-fatal failure (offline, admin rights required) it prints an actionable
// hint and returns an error so the caller skips wiring restore but keeps serving.
func setupSystemProxy(dir string, port int) (func() error, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	mgr := sysproxy.NewManager(sysproxy.ExecRunner, filepath.Join(dir, "sysproxy.json"))
	if err := mgr.Recover(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "proxysim: could not recover a prior system-proxy snapshot: %v\n", err)
	}

	restore, err := mgr.Apply(ctx, port)
	if err != nil {
		switch {
		case errors.Is(err, sysproxy.ErrNoPrimaryService):
			fmt.Fprintln(os.Stderr, "proxysim: no active network service; set the system proxy to 127.0.0.1 manually if needed")
		case errors.Is(err, sysproxy.ErrNeedAdmin):
			fmt.Fprintf(os.Stderr, "proxysim: could not set the system proxy automatically (admin rights required); set it manually to 127.0.0.1:%d\n", port)
		default:
			fmt.Fprintf(os.Stderr, "proxysim: could not set the system proxy automatically: %v\n", err)
		}
		return nil, err
	}
	fmt.Fprintf(os.Stderr, "System proxy: %s → 127.0.0.1:%d (restored on exit)\n\n", mgr.Service(), port)
	return restore, nil
}

func run(cfg config) error {
	dir, err := expandPath(cfg.caDir)
	if err != nil {
		return err
	}

	authority, err := ca.Load(dir)
	if err != nil {
		return err
	}

	// Remove the manual "trust the CA in the simulator" step: install it on every
	// startup unless opted out. Idempotent and best-effort — no booted device or no
	// Xcode toolchain must not stop the proxy serving (spec 010).
	if !cfg.noTrust {
		autoTrust(filepath.Join(dir, "ca.crt"), cfg.trustSet, cfg.device)
	}

	// Bind loopback only. A MITM proxy reachable from the network is a genuine
	// hazard, so this address is not configurable — not behind a flag, not for
	// testing (see CLAUDE.md).
	addr := net.JoinHostPort("127.0.0.1", fmt.Sprint(cfg.port))
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listening on %s: %w", addr, err)
	}

	// Take over the macOS system proxy so the user does not touch System Settings,
	// and — the part that matters — put it back on exit. The takeover routes every
	// host app through us, so it implies -only-sim: host traffic is tunnelled
	// untouched, never decrypted (spec 010). A failure here (no admin rights,
	// offline) degrades to a printed hint; we keep serving.
	onlySim := cfg.onlySim
	if cfg.systemProxy {
		onlySim = true
		restore, err := setupSystemProxy(dir, cfg.port)
		if err == nil {
			defer func() {
				if err := restore(); err != nil {
					fmt.Fprintf(os.Stderr, "proxysim: restoring system proxy: %v\n", err)
				}
			}()
		}
	}

	// The console is always a consumer. The UI, when enabled, is simply another
	// flow.Sink alongside it — the proxy does not know it exists (spec 008). Its
	// Emit is non-blocking (drops per client), so it sits directly in the fan-out
	// with no AsyncSink wrapper needed.
	// Seed the origin filter from the flags (and -system-proxy's implied only-sim).
	// With the UI on it becomes runtime-mutable through a Controller the control bar
	// drives (spec 011); with the UI off the flags are the only control and the
	// filter is fixed for the process (spec 009).
	seed := origin.BuildFilter(onlySim, cfg.apps)

	var sink flow.Sink = flow.NewConsoleSink(os.Stdout, cfg.verbose)
	var uiSrv *http.Server
	var uiLn net.Listener
	var controller *origin.Controller
	if cfg.ui {
		hub := ui.New(cfg.uiHistory)
		controller = origin.NewController(seed)
		hub.SetControl(controller, bootedDevices, installedApps)
		sink = flow.MultiSink{sink, hub}
		uiLn, err = ui.Listen(cfg.uiPort)
		if err != nil {
			return err
		}
		uiSrv = &http.Server{Handler: hub.Handler()}
	}

	var opts []proxy.Option
	if suffixes := splitSuffixes(cfg.exclude); len(suffixes) > 0 {
		opts = append(opts, proxy.WithExcludedHosts(suffixes...))
	}
	// Origin filter: intercept only the simulator (or a named app) and tunnel
	// everything else untouched. When the UI is on, read it live from the Controller
	// so a selection change takes effect on the next connection; otherwise it is the
	// fixed flag-built filter, wired only when it actually narrows something.
	switch {
	case controller != nil:
		opts = append(opts, proxy.WithOriginFilterFunc(origin.Resolve, controller.Current))
	case seed.Active():
		opts = append(opts, proxy.WithOriginFilter(origin.Resolve, seed))
	}
	handler := proxy.New(sink, authority, opts...)

	srv := &http.Server{Handler: handler}

	printBanner(addr, dir, authority)
	if uiLn != nil {
		fmt.Fprintf(os.Stderr, "UI:          http://%s\n\n", uiLn.Addr())
	}
	if len(cfg.apps) > 0 {
		fmt.Fprintf(os.Stderr, "Intercepting only apps: %s (all other traffic tunnelled)\n\n", strings.Join(cfg.apps, ", "))
	} else if onlySim {
		fmt.Fprint(os.Stderr, "Intercepting only iOS Simulator traffic (host traffic tunnelled)\n\n")
	}

	// Serve until a signal arrives, then shut down gracefully.
	errCh := make(chan error, 1)
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- fmt.Errorf("serving proxy: %w", err)
		}
	}()
	if uiSrv != nil {
		go func() {
			if err := uiSrv.Serve(uiLn); err != nil && !errors.Is(err, http.ErrServerClosed) {
				errCh <- fmt.Errorf("serving UI: %w", err)
			}
		}()
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		fmt.Fprintln(os.Stderr, "\nproxysim: shutting down")
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if uiSrv != nil {
			// SSE clients hold their connections open, so Shutdown would block on
			// them for the full timeout; Close drops them promptly instead.
			uiSrv.Close()
		}
		// Hijacked CONNECT tunnels are not tracked by Shutdown; this drains the
		// plain-HTTP side and returns promptly regardless.
		return srv.Shutdown(shutCtx)
	}
}

func printBanner(addr, dir string, authority *ca.Authority) {
	fmt.Fprintf(os.Stderr, "proxysim listening on http://%s\n", addr)
	fmt.Fprintf(os.Stderr, "CA:          %s\n", filepath.Join(dir, "ca.crt"))
	fmt.Fprintf(os.Stderr, "Fingerprint: %s\n\n", authority.Fingerprint())
	fmt.Fprintln(os.Stderr, "Trust the CA in the booted simulator:")
	fmt.Fprintf(os.Stderr, "  xcrun simctl keychain booted add-root-cert %s\n\n", filepath.Join(dir, "ca.crt"))
}

// expandPath resolves a leading ~ to the user's home directory. A CA inside the
// working tree is one `git add -A` away from being published, so ~ is the sane
// default and this keeps it working.
func expandPath(path string) (string, error) {
	if path == "~" || strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolving home directory: %w", err)
		}
		return filepath.Join(home, strings.TrimPrefix(path, "~")), nil
	}
	return path, nil
}

// splitSuffixes parses the comma-separated exclusion list, trimming blanks.
func splitSuffixes(csv string) []string {
	var out []string
	for s := range strings.SplitSeq(csv, ",") {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}
