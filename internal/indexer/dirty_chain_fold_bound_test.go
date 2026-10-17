package indexer

import (
	"context"
	"testing"
	"time"
)

// stalledFoldBackend is a fold backend whose steps never finish: each step
// takes stepDelay and reports more to do. It stands for a copy too large for
// the inline budget.
type stalledFoldBackend struct {
	inner     chainFoldBackend
	stepDelay time.Duration
}

func (b stalledFoldBackend) BeginChainFold(ctx context.Context, chain []int64, to int64, owner string) (chainFoldSteps, error) {
	fold, err := b.inner.BeginChainFold(ctx, chain, to, owner)
	if err != nil {
		return nil, err
	}
	return stalledFold{chainFoldSteps: fold, delay: b.stepDelay}, nil
}

func (b stalledFoldBackend) RebaseViewGeneration(ctx context.Context, generationID, fromBase, toBase int64) error {
	return b.inner.RebaseViewGeneration(ctx, generationID, fromBase, toBase)
}

func (b stalledFoldBackend) StepRetryable(err error) bool { return b.inner.StepRetryable(err) }

type stalledFold struct {
	chainFoldSteps
	delay time.Duration
}

func (f stalledFold) Step(ctx context.Context) (bool, error) {
	select {
	case <-ctx.Done():
		return false, ctx.Err()
	case <-time.After(f.delay):
		return false, nil
	}
}

// mcpChainAtTheCap drives edits until the routed chain is at the physical cap.
func mcpChainAtTheCap(t *testing.T, f *coordinatorFixture, l *CheckoutLifecycle) {
	t.Helper()
	var out CheckoutCycle
	for i := 0; i < maxChainWalkDepth; i++ {
		out = mcpEdit(t, l, f, func() { chainBurstEdit(t, f, i) })
	}
	if out.DirtyChainDepth != maxChainWalkDepth {
		t.Fatalf("depth %d after %d edits (%s), want the cap", out.DirtyChainDepth, maxChainWalkDepth, out.DirtyChainReason)
	}
}

// The longest an edit at the cap waits for its inline fold is the inline
// budget: a copy that does not finish in it is abandoned and the edit builds
// direct. Overrides, named: background compactions closed (no fold runs, so
// the edit meets the cap); the fold backend stalls (every step takes 200 ms
// and never ends the copy).
func TestInlineFoldAtTheCapIsBoundedInTime(t *testing.T) {
	f, c, l := mcpChainFixture(t, builderTreeA(), false)
	mcpChainAtTheCap(t, f, l)
	c.compaction.mu.Lock()
	c.compaction.backend = stalledFoldBackend{inner: storeChainFoldBackend{store: f.store}, stepDelay: 200 * time.Millisecond}
	c.compaction.mu.Unlock()

	done := make(chan CheckoutCycle, 1)
	started := time.Now()
	go func() { done <- mcpEdit(t, l, f, func() { chainBurstEdit(t, f, maxChainWalkDepth) }) }()
	var at CheckoutCycle
	select {
	case at = <-done:
	case <-time.After(dirtyChainInlineFoldBudget + 10*time.Second):
		t.Fatalf("the edit at the cap waited more than %v", dirtyChainInlineFoldBudget+10*time.Second)
	}
	waited := time.Since(started)
	t.Logf("the edit at the cap: %v (budget %v), parent %d, depth %d, reason %q",
		waited.Round(time.Millisecond), dirtyChainInlineFoldBudget, at.DirtyParentGenerationID, at.DirtyChainDepth, at.DirtyChainReason)
	if waited < dirtyChainInlineFoldBudget {
		t.Fatalf("the edit returned in %v, before the budget: the stalled fold was not attempted", waited)
	}
	if waited > dirtyChainInlineFoldBudget+2*time.Second {
		t.Fatalf("the edit at the cap waited %v, more than the budget %v and its own build", waited, dirtyChainInlineFoldBudget)
	}
	if at.DirtyParentGenerationID != 0 {
		t.Fatalf("the edit stands on %d after an abandoned fold, want a direct build", at.DirtyParentGenerationID)
	}
	// No clean-index comparison: the burst edits grow bodies past the clone
	// threshold, and a changed body's similar_to rows are the follow-up's
	// (deferred_to_followup); the direct build is otherwise the path every
	// other test covers.
}

// An edit at the cap folds inline without verifying, chains on the fold, and
// the fold is verified afterwards in the background. Override, named:
// background compactions closed (no fold runs, so the edit meets the cap).
func TestInlineFoldAtTheCapIsVerifiedInTheBackground(t *testing.T) {
	f, c, l := mcpChainFixture(t, builderTreeA(), false)
	mcpChainAtTheCap(t, f, l)
	verified, wrong := inlineFoldsVerified.Load(), inlineFoldMismatches.Load()
	at := mcpEdit(t, l, f, func() { chainBurstEdit(t, f, maxChainWalkDepth) })
	if at.DirtyParentGenerationID == 0 || !at.DirtyParentPreferred || at.DirtyChainDepth != 2 {
		t.Fatalf("the edit at the cap: parent %d preferred %t depth %d reason %q, want a child of the fold",
			at.DirtyParentGenerationID, at.DirtyParentPreferred, at.DirtyChainDepth, at.DirtyChainReason)
	}
	if err := c.waitDirtyChainCompactions(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := inlineFoldsVerified.Load() - verified; got != 1 {
		t.Fatalf("%d inline folds verified in the background, want 1", got)
	}
	if inlineFoldMismatches.Load() != wrong {
		t.Fatal("the background verification found the inline fold wrong")
	}
}

// A fold the background verification found wrong sends the next build
// direct. Override, named: background compactions closed.
func TestUnverifiedInlineFoldSendsTheNextBuildDirect(t *testing.T) {
	f, c, l := mcpChainFixture(t, builderTreeA(), false)
	for i := 0; i < 3; i++ {
		mcpEdit(t, l, f, func() { chainBurstEdit(t, f, i) })
	}
	c.compaction.forceDirect.Store(true)
	out := mcpEdit(t, l, f, func() { chainBurstEdit(t, f, 3) })
	if out.DirtyParentGenerationID != 0 || out.DirtyChainReason != dirtyChainFallbackFoldUnverified {
		t.Fatalf("the build after an unverified fold: parent %d reason %q, want direct (%s)",
			out.DirtyParentGenerationID, out.DirtyChainReason, dirtyChainFallbackFoldUnverified)
	}
	if next := mcpEdit(t, l, f, func() { chainBurstEdit(t, f, 4) }); next.DirtyParentGenerationID == 0 {
		t.Fatalf("the build after the direct one did not chain again (%s)", next.DirtyChainReason)
	}
	chainAssertFlat(t, f, "after-unverified-fold")
}
