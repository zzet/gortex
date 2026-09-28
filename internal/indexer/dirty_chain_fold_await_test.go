package indexer

import (
	"context"
	"testing"
	"time"
)

// awaitFixture builds a chain one short of the cap, starts a stepped fold of
// all of it held at its first step by gate, and adds the one layer above it
// that brings the chain to the cap. It returns the fold's report channel.
// Overrides, named: background compactions closed (the fold is run by hand);
// the step hook holds the fold until the test releases gate.
func awaitFixture(t *testing.T, gate chan struct{}) (*coordinatorFixture, *CheckoutCoordinator, *CheckoutLifecycle, <-chan DirtyChainCompaction) {
	t.Helper()
	f, c, l := mcpChainFixture(t, builderTreeA(), false)
	var trigger CheckoutCycle
	for i := 0; i < maxChainWalkDepth-1; i++ {
		trigger = mcpEdit(t, l, f, func() { chainBurstEdit(t, f, i) })
	}
	started := make(chan struct{})
	c.compaction.mu.Lock()
	c.compaction.stepHook = func(ctx context.Context, step int) {
		if step == 0 {
			close(started)
			select {
			case <-gate:
			case <-ctx.Done():
			}
		}
	}
	c.compaction.mu.Unlock()
	reports := make(chan DirtyChainCompaction, 1)
	go func() { reports <- c.compactDirtyChain(context.Background(), trigger) }()
	<-started
	if got := len(c.foldingChain()); got != maxChainWalkDepth-1 {
		t.Fatalf("the running fold folds %d layers, want %d", got, maxChainWalkDepth-1)
	}
	// One layer above the running fold: the chain is at the cap.
	above := mcpEdit(t, l, f, func() { chainBurstEdit(t, f, maxChainWalkDepth-1) })
	if above.DirtyChainDepth != maxChainWalkDepth {
		t.Fatalf("depth %d after the layer above the fold, want the cap %d (%s)", above.DirtyChainDepth, maxChainWalkDepth, above.DirtyChainReason)
	}
	return f, c, l, reports
}

// An edit at the cap over a running fold with one layer above it waits for
// the fold, lands it (re-bases the layer above onto it) and chains on that
// layer, instead of building direct.
func TestEditAtTheCapWaitsForTheRunningFold(t *testing.T) {
	gate := make(chan struct{})
	f, _, l, reports := awaitFixture(t, gate)
	// Release the fold shortly after the edit starts waiting.
	go func() {
		time.Sleep(300 * time.Millisecond)
		close(gate)
	}()
	started := time.Now()
	at := mcpEdit(t, l, f, func() { chainBurstEdit(t, f, maxChainWalkDepth) })
	waited := time.Since(started)
	if at.DirtyParentGenerationID == 0 {
		t.Fatalf("the edit at the cap built direct (%s), want it to chain on the waited fold", at.DirtyChainReason)
	}
	if at.DirtyChainDepth != 3 {
		t.Fatalf("the edit at the cap stands at depth %d, want 3 (fold, the re-based layer, the edit)", at.DirtyChainDepth)
	}
	if waited > dirtyChainInlineFoldBudget+5*time.Second {
		t.Fatalf("the edit took %v", waited)
	}
	report := <-reports
	if report.Landing != foldLandByEdit || report.Outcome != dirtyChainCompactionFlipped {
		t.Fatalf("the fold's report: landing %q outcome %s, want landed by the edit", report.Landing, report.Outcome)
	}
	chainAssertBuildsOnFold(t, f, report.GenerationID)
}

// A fold that does not land within the budget leaves the edit to build
// direct, after at most the budget.
func TestEditAtTheCapBuildsDirectWhenTheFoldDoesNotLand(t *testing.T) {
	gate := make(chan struct{})
	f, c, l, reports := awaitFixture(t, gate)
	c.compaction.mu.Lock()
	c.compaction.inlineBudget = 500 * time.Millisecond
	c.compaction.mu.Unlock()
	started := time.Now()
	at := mcpEdit(t, l, f, func() { chainBurstEdit(t, f, maxChainWalkDepth) })
	waited := time.Since(started)
	close(gate)
	<-reports
	if at.DirtyParentGenerationID != 0 {
		t.Fatalf("the edit stands on %d although the fold did not land in the budget", at.DirtyParentGenerationID)
	}
	if waited < 500*time.Millisecond {
		t.Fatalf("the edit built direct after %v, before the budget", waited)
	}
}

// chainAssertBuildsOnFold checks that the routed chain stands on fold.
func chainAssertBuildsOnFold(t *testing.T, f *coordinatorFixture, fold int64) {
	t.Helper()
	route := f.route()
	row, _ := f.generation(route.DirtyGenerationID)
	for row.GenerationID != 0 && row.BaseGenerationID != route.CommitGenerationID {
		if row.BaseGenerationID == fold {
			return
		}
		row, _ = f.generation(row.BaseGenerationID)
	}
	if row.GenerationID == fold {
		return
	}
	t.Fatalf("the routed chain does not stand on the fold %d", fold)
}
