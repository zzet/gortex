package indexer

import (
	"context"
	"testing"
	"time"
)

// mcpChainFixtureDefaults is mcpChainFixture with the compactor's production
// settings: the quiet interval, the attempt count and the schedule are the
// daemon's, and the store has the daemon's build-lane predicate. Overrides,
// named: the loop is parked (Debounce of an hour, demand
// debounced) so every cycle is an MCP edit's republish, as in the daemon's
// edit path; background compactions are scheduled by those republishes.
func mcpChainFixtureDefaults(t *testing.T, tree map[string]string) (*coordinatorFixture, *CheckoutCoordinator, *CheckoutLifecycle) {
	t.Helper()
	f := newCoordinatorFixtureWithTree(t, tree)
	gate := NewViewBuildGate()
	gate.Open()
	c := f.coordinator(t, CheckoutCoordinatorConfig{Gate: gate, Debounce: time.Hour, debounceDemand: true})
	// The daemon's build-lane predicate (installBuildLaneBusy): the store's
	// editing state, its marks and its checkpoints key on it.
	installDaemonBuildLaneBusy(f, c)
	if out := c.reconcile(context.Background()); out.Err != nil {
		t.Fatalf("initial reconcile: %+v", out)
	}
	l := &CheckoutLifecycle{catalog: f.catalog, store: f.store, coordinators: map[string]*CheckoutCoordinator{f.checkoutID: c}}
	return f, c, l
}

// A burst of 12 MCP edits 1.5 s apart with the daemon's defaults: a fold
// starts once the chain is 4 deep, steps between the edits, and lands; no
// schedule cancels a queued compaction, and no edit meets the bound.
func TestSteppedFoldRunsDuringABurstWithTheDaemonsDefaults(t *testing.T) {
	f, c, l := mcpChainFixtureDefaults(t, builderTreeA())
	pace := 1500 * time.Millisecond
	if testing.Short() {
		pace = 300 * time.Millisecond
	}
	edits, above := 12, 0
	for i := 0; i < edits; i++ {
		out := mcpEdit(t, l, f, func() { chainBurstEdit(t, f, i) })
		if out.DirtyChainReason == dirtyChainFallbackChainDepthExhausted {
			t.Fatalf("edit %d met the chain bound (depth %d)", i+1, out.DirtyChainDepth)
		}
		if len(c.foldingChain()) > 0 {
			above++
		}
		time.Sleep(pace)
	}
	if err := c.waitDirtyChainCompactions(context.Background()); err != nil {
		t.Fatal(err)
	}
	stats := c.DirtyChainCompactionStats()
	t.Logf("burst with the daemon's defaults: %+v, %d edits while a fold stepped", stats, above)
	if stats.Canceled != 0 {
		t.Fatalf("%d compactions were canceled", stats.Canceled)
	}
	if stats.AttemptsStarted == 0 || stats.Flipped == 0 {
		t.Fatalf("no fold started and landed: %+v", stats)
	}
	// Parity with a clean index is not asserted here: the burst grows the
	// bodies past the clone threshold, and a changed body's clone rows are
	// the follow-up's (deferred_to_followup); the parity family covers the
	// rest with the same edits.
}

// With the stepped fold on, a compaction does not wait for a quiet interval,
// and a queued or running compaction is not replaced by a newer schedule.
func TestSteppedFoldStartsWithoutQuietAndIsNotReplaced(t *testing.T) {
	if got := compactionQuietFor(0); got >= 0 {
		t.Fatalf("stepped fold quiet interval = %v, want none", got)
	}
	owed := make(chan struct{})
	if !compactionOwed(owed) {
		t.Fatal("a compaction still running is not owed")
	}
	close(owed)
	if compactionOwed(owed) || compactionOwed(nil) {
		t.Fatal("a finished compaction is owed")
	}
	_, c, _ := mcpChainFixtureDefaults(t, builderTreeA())
	running := make(chan struct{})
	c.compaction.mu.Lock()
	c.compaction.running = running
	c.compaction.mu.Unlock()
	if c.scheduleDirtyChainCompaction(CheckoutCycle{}) {
		t.Fatal("a schedule replaced the queued compaction")
	}
	close(running)
}
