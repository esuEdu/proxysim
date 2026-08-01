package origin

import (
	"sync"
	"testing"
)

// Criterion 3 (server side): Current reflects the seed, and Set replaces it.
func TestControllerCurrentAndSet(t *testing.T) {
	c := NewController(Filter{})
	if c.Current().Active() {
		t.Fatal("seed filter should be inactive")
	}

	c.Set(BuildFilter(true, nil))
	if got := c.Current(); !got.OnlySim || got.Active() != true {
		t.Fatalf("after Set(only-sim), Current() = %+v", got)
	}

	c.Set(BuildFilter(false, []string{"com.example.A"}))
	if got := c.Current(); !got.Apps["com.example.A"] || !got.OnlySim {
		t.Fatalf("after Set(app A), Current() = %+v", got)
	}
}

// Criterion 2: concurrent Current/Set is race-clean, and a published Filter is
// never mutated in place — every Set publishes a fresh Filter, so a reader that
// captured an earlier one keeps seeing it unchanged.
func TestControllerConcurrentAndImmutable(t *testing.T) {
	c := NewController(BuildFilter(false, []string{"com.example.A"}))
	captured := c.Current() // hold a reference across later Sets

	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for range 1000 {
				_ = c.Current().Match(Process{Simulator: true, BundleID: "com.example.A"})
			}
		})
	}
	for range 8 {
		wg.Go(func() {
			for range 1000 {
				c.Set(BuildFilter(true, nil))
				c.Set(BuildFilter(false, []string{"com.example.B"}))
			}
		})
	}
	wg.Wait()

	// The Filter captured before the storm still names only app A: publication is
	// copy-on-write, so concurrent Sets could not have reached into its map.
	if !captured.Apps["com.example.A"] || captured.Apps["com.example.B"] {
		t.Fatalf("captured filter was mutated after publication: %+v", captured)
	}
}
