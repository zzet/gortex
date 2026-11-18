package indexer

import (
	"context"
	"reflect"
	"testing"
	"time"
)

// pacedLifecycle is a lifecycle over one failed generation whose foreground
// activity and log size the test sets, counting the slices it starts.
func pacedLifecycle(t *testing.T) (*CheckoutLifecycle, *int, *string, *time.Time, *int64) {
	store, _, started := failedRetirementFixture(t)
	lifecycle := newGenerationRetirementLifecycle(store, started)
	lifecycle.interactiveDemand = func() bool { return false }
	busy, last, wal := new(string), new(time.Time), new(int64)
	lifecycle.foregroundWork = func() (string, time.Time) { return *busy, *last }
	lifecycle.walBytes = func() int64 { return *wal }
	slices := new(int)
	lifecycle.retireOwedSlice = func(context.Context, int64) error {
		*slices++
		return nil
	}
	return lifecycle, slices, busy, last, wal
}

// No chunk starts, and no candidate scan, until no foreground work has been
// seen for the idle time; after it the sweep runs.
func TestDeferredRetirementWaitsForTheEditIdle(t *testing.T) {
	lifecycle, slices, _, last, _ := pacedLifecycle(t)
	*last = time.Now().Add(-5 * time.Second)
	scans := deferredRetirementScans.Load()
	if _, pending, err := lifecycle.SweepDeferredRetirements(context.Background()); !pending || err != nil || *slices != 0 {
		t.Fatalf("5 s after an edit: pending=%v err=%v slices=%d; want the sweep to stand down", pending, err, *slices)
	}
	if deferredRetirementScans.Load() != scans {
		t.Fatal("the sweep scanned for candidates inside the edit idle")
	}
	*last = time.Now().Add(-deferredRetirementEditIdle - time.Second)
	if _, _, err := lifecycle.SweepDeferredRetirements(context.Background()); err != nil || *slices != 1 {
		t.Fatalf("after the edit idle: err=%v slices=%d; want one slice", err, *slices)
	}
}

// A starved sweep waits only the short idle; a pending ticket still holds it.
func TestStarvedDeferredRetirementWaitsTheShortIdleButNeverATicket(t *testing.T) {
	lifecycle, slices, busy, last, _ := pacedLifecycle(t)
	lifecycle.retirementStarvationLimit = time.Minute
	lifecycle.deferredRetirementProgress.Store(time.Now().Add(-2 * time.Minute).UnixNano())
	*busy = "refresh_ticket"
	if _, pending, _ := lifecycle.SweepDeferredRetirements(context.Background()); !pending || *slices != 0 {
		t.Fatalf("starved with a ticket pending: slices=%d; want none", *slices)
	}
	*busy = ""
	*last = time.Now().Add(-deferredRetirementStarvedIdle - time.Second)
	if _, _, err := lifecycle.SweepDeferredRetirements(context.Background()); err != nil || *slices != 1 {
		t.Fatalf("starved after the short idle: err=%v slices=%d; want one slice", err, *slices)
	}
}

// While a checkout is being edited the sweep does not grow a log past the
// pause mark; an idle checkout's sweep is left to the store's own ceiling.
func TestDeferredRetirementPausesOnTheLogWhileEditing(t *testing.T) {
	lifecycle, slices, _, last, wal := pacedLifecycle(t)
	*wal = deferredRetirementWALPause + 1
	*last = time.Now().Add(-deferredRetirementEditIdle - time.Second)
	if _, pending, _ := lifecycle.SweepDeferredRetirements(context.Background()); !pending || *slices != 0 {
		t.Fatalf("log over the pause while editing: slices=%d; want none", *slices)
	}
	*last = time.Now().Add(-deferredRetirementActiveWindow - time.Minute)
	if _, _, err := lifecycle.SweepDeferredRetirements(context.Background()); err != nil || *slices != 1 {
		t.Fatalf("idle checkout: err=%v slices=%d; want one slice", err, *slices)
	}
}

// While a checkout is being edited the smallest generation goes first and a
// large one waits for the long idle (or for starvation); otherwise the order
// is kept.
func TestRetirementOrderForPace(t *testing.T) {
	sizes := map[int64]int64{10: 5 << 20, 11: 200 << 20, 12: 1 << 20, 13: 0}
	size := func(id int64) int64 { return sizes[id] }
	now := time.Now()
	in := []int64{13, 12, 11, 10}
	editing := retirementPace{now: now, last: now.Add(-30 * time.Second)}
	if got := orderForPace(editing, append([]int64(nil), in...), size); !reflect.DeepEqual(got, []int64{13, 12, 10}) {
		t.Fatalf("editing order = %v, want [13 12 10] (smallest first, the large one held)", got)
	}
	starved := editing
	starved.starved = true
	if got := orderForPace(starved, append([]int64(nil), in...), size); !reflect.DeepEqual(got, []int64{13, 12, 10, 11}) {
		t.Fatalf("starved order = %v, want [13 12 10 11]", got)
	}
	idle := retirementPace{now: now, last: now.Add(-deferredRetirementActiveWindow - time.Minute)}
	if got := orderForPace(idle, append([]int64(nil), in...), size); !reflect.DeepEqual(got, in) {
		t.Fatalf("idle order = %v, want the order kept %v", got, in)
	}
	if editing.sliceBudget() != deferredRetirementActiveSlice || idle.sliceBudget() != deferredRetirementSliceBudget {
		t.Fatalf("slice budgets = %s / %s", editing.sliceBudget(), idle.sliceBudget())
	}
}
