package parent

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

// poll is short so the tests finish quickly; the production cadence is a second.
const poll = 5 * time.Millisecond

// returnsWithin runs Watch on a goroutine and reports whether it returned in time.
// Watch blocking forever is a real failure mode — it would mean proxysim never
// notices its parent is gone — so "did not return" has to be a test outcome rather
// than a hung suite.
func returnsWithin(t *testing.T, d time.Duration, run func()) bool {
	t.Helper()
	done := make(chan struct{})
	go func() {
		run()
		close(done)
	}()
	select {
	case <-done:
		return true
	case <-time.After(d):
		return false
	}
}

func TestWatchReturnsWhenTheParentChanges(t *testing.T) {
	// The whole point: a reparented process has a different ppid, and that is the
	// signal to shut down.
	var ppid atomic.Int64
	ppid.Store(4242)

	go func() {
		time.Sleep(3 * poll)
		ppid.Store(1) // reparented to init
	}()

	if !returnsWithin(t, time.Second, func() {
		Watch(context.Background(), 4242, func() int { return int(ppid.Load()) }, poll)
	}) {
		t.Fatal("Watch did not return after the parent changed")
	}
}

func TestWatchReturnsForAnyNewParentNotJustPID1(t *testing.T) {
	// Reparenting does not always land on 1 — launchd has per-session equivalents —
	// so the check must be "changed", not "is 1".
	var ppid atomic.Int64
	ppid.Store(4242)

	go func() {
		time.Sleep(3 * poll)
		ppid.Store(9999)
	}()

	if !returnsWithin(t, time.Second, func() {
		Watch(context.Background(), 4242, func() int { return int(ppid.Load()) }, poll)
	}) {
		t.Fatal("Watch did not return after the parent changed to a pid other than 1")
	}
}

func TestWatchKeepsWatchingWhileTheParentLives(t *testing.T) {
	// A false positive here would shut proxysim down mid-session for no reason.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	returned := returnsWithin(t, 20*poll, func() {
		Watch(ctx, 4242, func() int { return 4242 }, poll)
	})
	if returned {
		t.Fatal("Watch returned while the parent was still alive")
	}
}

func TestWatchReturnsOnContextCancel(t *testing.T) {
	// Shutting down for another reason must not leave a goroutine polling forever.
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(2 * poll)
		cancel()
	}()

	if !returnsWithin(t, time.Second, func() {
		Watch(ctx, 4242, func() int { return 4242 }, poll)
	}) {
		t.Fatal("Watch did not return when its context was cancelled")
	}
}

func TestWatchIsANoOpForAnAlreadyOrphanedProcess(t *testing.T) {
	// Started by launchd, under nohup, or via a double fork: ppid is already 1, so
	// there is no parent to outlive. Treating that as "the parent died" would make
	// -exit-with-parent exit instantly.
	for _, original := range []int{Orphaned, 0, -1} {
		calls := 0
		if !returnsWithin(t, time.Second, func() {
			Watch(context.Background(), original, func() int { calls++; return 12345 }, poll)
		}) {
			t.Fatalf("Watch(original=%d) did not return immediately", original)
		}
		if calls != 0 {
			t.Errorf("Watch(original=%d) polled %d times; it should not watch at all", original, calls)
		}
	}
}

func TestWatchDefaultsANonPositivePoll(t *testing.T) {
	// A zero interval would make time.NewTicker panic, which in a goroutine takes
	// the whole process down — the opposite of a clean shutdown.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	returned := returnsWithin(t, 50*time.Millisecond, func() {
		Watch(ctx, 4242, func() int { return 4242 }, 0)
	})
	if returned {
		t.Fatal("Watch returned early; a non-positive poll should fall back to the default")
	}
}

func TestShouldWatch(t *testing.T) {
	cases := map[int]bool{
		-1:     false,
		0:      false,
		1:      false, // already orphaned
		2:      true,
		501:    true,
		999999: true,
	}
	for ppid, want := range cases {
		if got := ShouldWatch(ppid); got != want {
			t.Errorf("ShouldWatch(%d) = %v, want %v", ppid, got, want)
		}
	}
}
