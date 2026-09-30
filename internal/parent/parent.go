// Package parent detects that the process which launched proxysim is gone, so a
// host-managed instance can exit instead of becoming an orphan.
//
// This matters more than it sounds. An orphaned proxysim keeps the proxy port bound
// and — if it was started with -system-proxy — may still hold the macOS system
// proxy pointed at itself. If it is then killed without running its restore, the
// user is left with a Mac routing traffic at a dead listener, which presents as a
// broken network with nothing on screen to explain it. The host app's own teardown
// is the first line of defence; this is the one that survives the host being
// killed, crashing, or force-quit, none of which run any cleanup code.
//
// On macOS there is no PR_SET_PDEATHSIG. The portable signal is that an orphan is
// reparented, so getppid() stops matching what it was at startup. Polling that is
// about fifteen lines with no build tags and no cgo, against roughly sixty for a
// kqueue NOTE_EXIT watch; a one-second detection delay is irrelevant when the
// response is "shut down".
package parent

import (
	"context"
	"time"
)

// DefaultPoll is how often the parent is checked. A second is far below any
// human-visible threshold for a shutdown and costs one syscall.
const DefaultPoll = time.Second

// Orphaned is the parent pid of a reparented process. A process whose parent is
// already 1 at startup has no parent to outlive.
const Orphaned = 1

// Watch blocks until the original parent is gone, then returns.
//
// getppid is injected (os.Getppid in production) so the whole behaviour is testable
// without forking processes. It returns immediately — without watching — when
// original is Orphaned or non-positive: a process started by launchd, under nohup,
// or via a double fork was never anyone's child in the first place, and treating
// that as "the parent already died" would make -exit-with-parent exit instantly.
//
// Returns when ctx is cancelled too, so a caller that shuts down for another reason
// is not left with a goroutine polling a dead process.
func Watch(ctx context.Context, original int, getppid func() int, poll time.Duration) {
	if original <= Orphaned {
		return
	}
	if poll <= 0 {
		poll = DefaultPoll
	}

	ticker := time.NewTicker(poll)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			// Any change means the original parent is gone: the kernel reparents an
			// orphan, normally to 1 (or to launchd's per-session equivalent). Comparing
			// against the captured value rather than testing for 1 specifically avoids
			// depending on which pid it is reparented to.
			if getppid() != original {
				return
			}
		}
	}
}

// ShouldWatch reports whether watching is meaningful for this parent pid, so a
// caller can skip the goroutine and say so in its startup output rather than
// silently doing nothing.
func ShouldWatch(original int) bool {
	return original > Orphaned
}
