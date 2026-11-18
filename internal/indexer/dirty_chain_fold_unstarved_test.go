package indexer

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// chainBurstEdit writes the i-th edit of a burst: one of four files of the
// fixture tree, round-robin, each time with one more statement in its body.
func chainBurstEdit(t *testing.T, f *coordinatorFixture, i int) {
	t.Helper()
	files := []string{"island.go", "helper.go", "caller.go", "core.go"}
	names := map[string]string{"island.go": "Island", "helper.go": "Helper", "caller.go": "Run", "core.go": "Compute"}
	file := files[i%len(files)]
	body := ""
	for k := 0; k <= i; k++ {
		body += fmt.Sprintf("\t_ = %d\n", k)
	}
	content := fmt.Sprintf("package fixture\n\nfunc %s() {\n%s}\n", names[file], body)
	if file == "core.go" {
		content = fmt.Sprintf("package fixture\n\ntype Options struct{}\n\nfunc Compute(o Options) {\n%s\tHelper()\n}\n", body)
	}
	if file == "caller.go" {
		content = fmt.Sprintf("package fixture\n\nfunc Run() {\n%s\tCompute(Options{})\n}\n", body)
	}
	builderWriteFile(t, f.worktree, file, content)
}

// A burst of 12 edits, each followed by a search that holds the served view
// while the checkout's scheduled fold runs (the agent's edit, search, edit
// pace, compressed): the fold lands between edits despite the reader, and no
// edit meets the chain bound. The fold at the bound is off here, so the chain
// can stay under the bound only if a background fold landed.
func TestChainFoldLandsDuringABurstWithASearchAfterEachEdit(t *testing.T) {
	saved := foldChainAtBoundEnabled
	foldChainAtBoundEnabled = false
	defer func() { foldChainAtBoundEnabled = saved }()
	f := newCoordinatorFixture(t)
	c := chainCoordinatorOn(t, f, CheckoutCoordinatorConfig{})
	coordinatorReconcile(t, c)
	ctx := context.Background()
	flipped := 0
	for i := 0; i < 12; i++ {
		chainBurstEdit(t, f, i)
		out := coordinatorReconcile(t, c)
		if out.DirtyChainReason == dirtyChainFallbackChainDepthExhausted {
			t.Fatalf("edit %d met the chain bound (depth %d): the fold never landed", i+1, out.DirtyChainDepth)
		}
		// The search: a reader holds the served view for the whole gap.
		lease := f.leases.Acquire(out.DirtyGenerationID)
		if out.CompactionScheduled {
			report := c.compactDirtyChain(ctx, out)
			if report.Outcome == dirtyChainCompactionFlipped || report.Outcome == dirtyChainCompactionPreferred {
				flipped++
			}
		}
		lease.Release()
		time.Sleep(10 * time.Millisecond)
	}
	if flipped == 0 {
		t.Fatal("no fold landed during the burst")
	}
}

// With no background fold landing at all, the edit that meets the chain bound
// folds the chain itself and chains on the fold: no edit builds direct over
// the accumulated dirty set.
func TestEditAtTheChainBoundFoldsInsteadOfBuildingDirect(t *testing.T) {
	f := newCoordinatorFixture(t)
	c := chainCoordinatorOn(t, f, CheckoutCoordinatorConfig{})
	coordinatorReconcile(t, c)
	foldedAtBound := 0
	for i := 0; i < 12; i++ {
		chainBurstEdit(t, f, i)
		out := coordinatorReconcile(t, c)
		if out.DirtyChainReason == dirtyChainFallbackChainDepthExhausted {
			t.Fatalf("edit %d met the chain bound and built direct", i+1)
		}
		if out.DirtyParentPreferred && out.DirtyChainDepth == 2 && i >= maxDirtyChainDepth {
			foldedAtBound++
		}
	}
	if foldedAtBound == 0 {
		t.Fatal("no edit chained on a fold made at the bound")
	}
}

// The sweep stands down while a chain fold holds the build lane.
func TestDeferredRetirementHoldsWhileAChainFoldRuns(t *testing.T) {
	lifecycle, slices, _, _, _ := pacedLifecycle(t)
	folding := true
	lifecycle.foldInFlight = func() bool { return folding }
	if _, pending, _ := lifecycle.SweepDeferredRetirements(context.Background()); !pending || *slices != 0 {
		t.Fatalf("during a fold: slices=%d; want none", *slices)
	}
	folding = false
	if _, _, err := lifecycle.SweepDeferredRetirements(context.Background()); err != nil || *slices != 1 {
		t.Fatalf("after the fold: err=%v slices=%d; want one slice", err, *slices)
	}
}
