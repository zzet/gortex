package store_sqlite

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// No planner-statistics ANALYZE runs inside an edit window: while the build
// lane predicate reports an edit-driven cycle the refresh defers with the
// edit-cycle reason and leaves the verdict standing, and once the lane is
// free the same store refreshes.
func TestEnsurePlannerStatsFreshDefersInsideAnEditCycle(t *testing.T) {
	s := freshStatsStore(t, "stats_edit_cycle.sqlite", 100)
	if first := mustEnsure(t, s); first.Refreshed || first.Stale {
		t.Fatalf("fixture was not fresh: refreshed=%v reason=%q", first.Refreshed, first.Reason)
	}
	growStoreToDouble(t, s)

	var busy atomic.Bool
	busy.Store(true)
	s.SetBuildLaneBusy(busy.Load)
	t.Cleanup(func() { s.SetBuildLaneBusy(nil) })

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	deferred, err := s.EnsurePlannerStatsFresh(ctx)
	if err != nil {
		t.Fatalf("an edit cycle is not a failure: %v", err)
	}
	if deferred.Refreshed || !strings.HasPrefix(deferred.Reason, plannerStatsEditCycleReason) || !deferred.Stale {
		t.Fatalf("inside an edit cycle: refreshed=%v stale=%v reason=%q; want a %q deferral that keeps the verdict",
			deferred.Refreshed, deferred.Stale, deferred.Reason, plannerStatsEditCycleReason)
	}
	if h := mustHealth(t, s); h.Refreshes != 0 {
		t.Fatalf("an ANALYZE ran inside the edit cycle (%d refreshes)", h.Refreshes)
	}

	busy.Store(false)
	if after := mustEnsure(t, s); !after.Refreshed {
		t.Fatalf("with the lane free the refresh did not run: reason=%q", after.Reason)
	}
}

// A statement already running when an edit cycle takes the lane is cancelled
// within the poll, and the watch reports that it fired; without a cycle it
// never cancels.
func TestCancelOnEditCycleInterruptsWhenTheLaneIsTaken(t *testing.T) {
	s := freshStatsStore(t, "stats_edit_cycle_watch.sqlite", 10)
	var busy atomic.Bool
	s.SetBuildLaneBusy(busy.Load)
	t.Cleanup(func() { s.SetBuildLaneBusy(nil) })

	ctx, cancel := context.WithCancel(context.Background())
	stop, yielded := s.cancelOnEditCycle(cancel)
	time.Sleep(5 * walCheckpointCycleYieldPoll)
	if ctx.Err() != nil || yielded.Load() {
		t.Fatal("the watch cancelled with the lane free")
	}
	busy.Store(true)
	select {
	case <-ctx.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("the watch did not cancel the statement once an edit cycle held the lane")
	}
	stop()
	if !yielded.Load() {
		t.Fatal("the watch cancelled without reporting the yield")
	}
}
