package indexer

import (
	"context"
	"slices"
	"testing"
)

// TestLargeWorkingTreeIsImportedFileByFileAndReachesTheWorkingTree pins the
// import of a large dirty set: each cycle publishes one file as a chained
// generation (routed, but rescheduled so no refresh ticket completes on it and
// with a partial identity no freshness check accepts), the next cycle imports
// the next file over it — folding the chain by copy whenever it reaches the
// compaction depth — and the last one describes the working tree exactly: its
// chain composes to a clean index of the edited tree. Live, a 389-file working
// tree built direct never completed: every yield and every write to it
// restarted a minutes-long build from zero.
func TestLargeWorkingTreeIsImportedFileByFileAndReachesTheWorkingTree(t *testing.T) {
	old := importInteractivePaths
	importInteractivePaths = 8
	t.Cleanup(func() { importInteractivePaths = old })
	f := newCoordinatorFixtureWithTree(t, retentionTree())
	c := chainCoordinatorOn(t, f, CheckoutCoordinatorConfig{})
	if out := coordinatorReconcile(t, c); out.DirtyGenerationID == 0 {
		t.Fatalf("initial reconcile: %+v", out)
	}
	for i := 0; i < retentionUnits; i++ {
		for _, layout := range []accumulatedDirtyLayout{accumulatedDirtyIndependent, accumulatedDirtySamePackage} {
			accumulatedDirtyWriteUnit(t, f.worktree, layout, i, true, false)
		}
	}
	const dirty = 2 * retentionUnits // 40 paths, one per cycle
	var last CheckoutCycle
	folds := 0
	for cycle := 0; cycle < 2*dirty; cycle++ {
		out := coordinatorReconcile(t, c)
		if !out.DirtyBuilt {
			t.Fatalf("cycle %d built nothing: %+v", cycle, out)
		}
		if want := dirty - cycle - 1; out.DirtyBatchRemaining != want {
			t.Fatalf("cycle %d left %d paths, want %d: one file per cycle", cycle, out.DirtyBatchRemaining, want)
		}
		if out.DirtyBatchRemaining > 0 != out.Rescheduled {
			t.Fatalf("cycle %d: remaining %d but rescheduled %v", cycle, out.DirtyBatchRemaining, out.Rescheduled)
		}
		if out.ImportFolded {
			folds++
		}
		if out.DirtyBatchRemaining > 0 {
			row, _, err := f.catalog.GetViewGeneration(context.Background(), out.DirtyGenerationID)
			if err != nil {
				t.Fatal(err)
			}
			sample, err := c.sampler.Sample(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if row.LowerViewFingerprint == sample.Fingerprint {
				t.Fatalf("cycle %d: an import link claims the working tree's fingerprint", cycle)
			}
		}
		last = out
		if out.DirtyBatchRemaining == 0 {
			break
		}
	}
	if last.DirtyBatchRemaining != 0 {
		t.Fatalf("the import did not reach the working tree: %+v", last)
	}
	if folds == 0 {
		t.Fatal("the import never folded its chain")
	}
	members := c.dirtyChainMembers(context.Background(), last.DirtyGenerationID)
	slices.Reverse(members)
	if result := assertCleanIndexParityChain(t, f.store, members, f.worktree, "imported", true); !result.ok() {
		t.Fatalf("the imported chain does not compose to the working tree: %v", result.Diffs)
	}
	// Settled: the next cycle builds nothing.
	if out := coordinatorReconcile(t, c); out.DirtyBuilt || out.DirtyGenerationID != last.DirtyGenerationID {
		t.Fatalf("after the last link the checkout is not settled: %+v", out)
	}
}

// TestABatchedBuildWatchesOnlyItsOwnPaths pins the narrowed movement abort: a
// write elsewhere in the dirty set does not abandon the import link in flight,
// a write to one of its paths does.
func TestABatchedBuildWatchesOnlyItsOwnPaths(t *testing.T) {
	root := t.TempDir()
	c := &CheckoutCoordinator{root: root}
	fired := false
	y := &backgroundLaneYield{cancel: func() { fired = true }, withdraw: func() {}, stop: make(chan struct{})}
	c.motion.abort = &treeMoveAbort{
		y:     y,
		dirty: map[string]struct{}{"a/one.go": {}, "b/two.go": {}, "docs/ledger.md": {}},
		dirs:  map[string]struct{}{"a": {}, "b": {}, "docs": {}},
	}
	c.narrowTreeMoveAbort([]string{"a/one.go"})
	c.noteFilesystemChange([]string{root + "/docs/ledger.md"})
	if fired {
		t.Fatal("a write outside the batch abandoned it")
	}
	c.noteFilesystemChange([]string{root + "/a/one.go"})
	if !fired {
		t.Fatal("a write to a path the batch builds did not abandon it")
	}
}

// TestAWorkingTreeBatchIsNotTruncatedAtTheOneEditCap pins the closure cap of a
// many-change working-tree build: sized by its change set (as a committed
// base is), so a batch of a large working tree does not truncate at the
// one-edit default — a truncated generation is refused as a chain parent, and
// the next batch would fall back to the direct build of the whole dirty set.
// A one-file edit keeps the default; an operator's cap always wins.
func TestAWorkingTreeBatchIsNotTruncatedAtTheOneEditCap(t *testing.T) {
	b := &SparseGenerationBuilder{}
	changes := func(n int) []LayerPathChange {
		out := make([]LayerPathChange, n)
		for i := range out {
			out[i] = LayerPathChange{Path: "f.go", Kind: LayerPathModified}
		}
		return out
	}
	dirty := GenerationIdentity{GenerationKind: DirtyLayerGenerationKind}
	if got, source := b.builderClosureCap(BuildRequest{Identity: dirty, Changes: changes(1)}); got != defaultAffectedByMax || source != ClosureCapFromDefault {
		t.Fatalf("one-file edit cap = %d (%s), want the default %d", got, source, defaultAffectedByMax)
	}
	if got, source := b.builderClosureCap(BuildRequest{Identity: dirty, Changes: changes(99)}); got != 99*builderCommittedBaseClosurePerChange || source != ClosureCapFromChangeSized {
		t.Fatalf("99-change batch cap = %d (%s), want %d change-sized", got, source, 99*builderCommittedBaseClosurePerChange)
	}
	b.Config.AffectedByReresolveMax = 50
	if got, source := b.builderClosureCap(BuildRequest{Identity: dirty, Changes: changes(99)}); got != 50 || source != ClosureCapFromOperator {
		t.Fatalf("operator cap = %d (%s), want 50", got, source)
	}
}
