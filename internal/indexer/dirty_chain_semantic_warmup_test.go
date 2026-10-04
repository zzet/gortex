package indexer

import (
	"testing"
	"time"

	"github.com/zzet/gortex/internal/semantic/goanalysis"
)

// TestDirtyChainSemanticTypecheckCacheWarmupServesFirstEdit: a working-tree
// build's enrichment stage starts the background whole-module listing of the
// checkout after its pass, and the first chained edit in a package no build
// listed is then a closure hit (no go command) with parity against a clean
// semantic index.
func TestDirtyChainSemanticTypecheckCacheWarmupServesFirstEdit(t *testing.T) {
	f, c, mgr := semanticChainFixtureWith(t, semanticBindingTree(), true)
	coordinatorReconcile(t, c)
	for i := 0; i < 8; i++ {
		semanticWriteUnit(t, f.worktree, accumulatedDirtyIndependent, i, true)
	}
	root := coordinatorReconcile(t, c)
	if root.DirtyParentGenerationID != 0 {
		t.Fatalf("the chain root = %+v, want a direct build", root)
	}
	assertSemanticParity(t, f, mgr, "warmup-root")

	var provider *goanalysis.Provider
	for _, p := range mgr.AllProviders() {
		if gp, ok := p.(*goanalysis.Provider); ok {
			provider = gp
		}
	}
	if provider == nil {
		t.Fatal("no go/types provider registered")
	}
	deadline := time.Now().Add(2 * time.Minute)
	var warm goanalysis.CheckoutWarmupStatus
	for {
		statuses := provider.CheckoutWarmups()
		if len(statuses) == 1 && statuses[0].State == "warm" {
			warm = statuses[0]
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the checkout was not warmed after the root build: %+v", statuses)
		}
		time.Sleep(50 * time.Millisecond)
	}
	if warm.Packages == 0 || warm.Attempts == 0 {
		t.Fatalf("warm-up status %+v, want a completed listing", warm)
	}

	// Unit 9 lives in a package no build listed.
	semanticWriteUnitRevision(t, f.worktree, accumulatedDirtyIndependent, 9, 0)
	out := coordinatorReconcile(t, c)
	if out.DirtyParentGenerationID == 0 {
		t.Fatalf("first edit = %+v, want a chained build", out)
	}
	cc := out.DirtyWork.CompilerContext
	if cc.Cache == nil {
		t.Fatalf("no cache counters on the compiler context: %+v", cc)
	}
	t.Logf("first edit after the warm-up: cache %+v", *cc.Cache)
	if cc.Cache.ClosureHits != 1 || cc.Cache.ClosureMisses != 0 || cc.Cache.GoListMs != 0 || !cc.Cache.WarmServed {
		t.Errorf("cache %+v, want a closure hit served from the warm-up with no go list", *cc.Cache)
	}
	if cc.Cache.WarmupState != "warm" || cc.Cache.WarmupPackages == 0 {
		t.Errorf("cache %+v, want the warm-up's state on the counters", *cc.Cache)
	}
	if n := semanticGenerationBindingRows(t, f, out.DirtyGenerationID); n == 0 {
		t.Error("the generation wrote no binding rows; the binding oracle is vacuous")
	}
	assertSemanticParity(t, f, mgr, "warmup-first-edit")
}
