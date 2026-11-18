package indexer

import (
	"context"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

// A fold and its landing leave the per-stack key where it was. The key is the
// stack below the dirty chain: folding the chain's lower part into one
// generation and rebasing the lowest layer above it onto that generation (the
// landing the fold stages as Catalog.RebaseViewGeneration, stubbed here by
// the catalog row it rewrites), or flipping to the fold when nothing is above
// it, changes only the chain. An edit built over each shape keys its caches
// as the edits before it did, serves rows they kept, and publishes a
// generation whose chain composes to a clean index of the checkout.
func TestChainOverlayKeyHoldsAcrossAFoldAndItsLanding(t *testing.T) {
	resetChainOverlayCaches()
	t.Cleanup(resetChainOverlayCaches)
	store := builderOpenStore(t, "chain-fold-key")
	repoDir := accumulatedDirtyRepo(t, accumulatedDirtyIndependent, store)
	builder := builderNewBuilder(store)
	h := newDirtyChainBuilder(t, builder, store, repoDir, true)
	h.compact = false
	// Every build is keyed: the commit part is a fixed generation standing
	// for the base corpus (generation zero is not written while the test
	// runs), the chain part the chain's generations.
	const commit = int64(1) << 40
	keyedBuild := func(label string) (int64, *EditDeltaReport) {
		t.Helper()
		ctx := context.Background()
		req := h.request()
		req.Base = commitLayerBase{Reader: store, corpus: store, stack: []int64{commit}}
		if len(h.chain) > 0 {
			manifest, why := loadDirtyChainManifest(ctx, store, h.chain)
			if why != "" {
				t.Fatalf("%s: the chain's manifest: %s", label, why)
			}
			parent := h.chain[len(h.chain)-1]
			req.Base = commitLayerBase{Reader: dirtyChainComposed(t, store, h.chain), corpus: store,
				stack: append([]int64{commit}, h.chain...)}
			req.Identity.BaseGenerationID = parent
			req.parent, req.parentManifest, req.parentDepth = parent, manifest, len(h.chain)
		}
		recordLastEditDelta(nil)
		id, report, err := builder.BuildDirtyLayer(ctx, req)
		if err != nil {
			t.Fatalf("%s: %v", label, err)
		}
		if len(h.chain) > 0 && report.ParentGenerationID != h.chain[len(h.chain)-1] {
			t.Fatalf("%s: built direct (%q), want over the chain", label, report.ChainFallbackReason)
		}
		delta := LastEditDeltaReport()
		if delta == nil {
			t.Fatalf("%s: no edit delta", label)
		}
		if delta.StackCacheKey == "" {
			t.Fatalf("%s: the delta had no per-stack key", label)
		}
		if delta.ChainLayersOverlaid != len(h.chain) {
			t.Fatalf("%s: the delta composed %d chain layers over its caches, want %d", label, delta.ChainLayersOverlaid, len(h.chain))
		}
		h.chain = append(h.chain, id)
		return id, delta
	}
	edit := func(unit int) {
		accumulatedDirtyWriteUnit(t, repoDir, accumulatedDirtyIndependent, unit, true, false)
	}
	parity := func(label string) {
		t.Helper()
		result := assertCleanIndexParityChain(t, store, h.chain, repoDir, label, false)
		if !result.NodesEqual || !result.EdgesEqual {
			t.Fatalf("%s: the chain does not compose to a clean index: %v", label, result.Diffs)
		}
	}

	// The lower chain c1..c3: the first edit direct, the others chained.
	edit(0)
	_, first := keyedBuild("c1")
	key := first.StackCacheKey
	edit(1)
	keyedBuild("c2")
	edit(2)
	keyedBuild("c3")
	lower := append([]int64(nil), h.chain...)
	// The fold: one direct generation of the state the lower chain composes
	// (what a fold publishes), built beside the chain, not above it.
	fold, _, err := builder.BuildDirtyLayer(context.Background(), h.request())
	if err != nil {
		t.Fatalf("the fold: %v", err)
	}
	// u1, u2 above the lower chain while the fold was built.
	edit(3)
	_, u1 := keyedBuild("u1")
	edit(4)
	_, u2 := keyedBuild("u2")
	for label, delta := range map[string]*EditDeltaReport{"u1": u1, "u2": u2} {
		if delta.StackCacheKey != key {
			t.Fatalf("%s moved the key:\n%q\n%q", label, delta.StackCacheKey, key)
		}
	}
	parity("before-landing")

	// The landing: u1's base moves from c3 to the fold, in the catalog row a
	// view's chain walk reads.
	db := parityOpenRaw(t, store)
	if _, err := db.Exec(`UPDATE view_generations SET base_generation_id=? WHERE generation_id=? AND base_generation_id=?`,
		fold, h.chain[len(lower)], lower[len(lower)-1]); err != nil {
		t.Fatalf("rebase u1: %v", err)
	}
	h.chain = append([]int64{fold}, h.chain[len(lower):]...)
	parity("rebased")
	edit(5)
	hitsBefore := chainOverlayCacheHits(editDeltaBaseCache(key))
	_, over := keyedBuild("over-the-rebased-chain")
	if over.StackCacheKey != key {
		t.Fatalf("the edit over the rebased chain moved the key:\n%q\n%q", over.StackCacheKey, key)
	}
	if hits := chainOverlayCacheHits(editDeltaBaseCache(key)) - hitsBefore; hits == 0 {
		t.Fatal("the edit over the rebased chain served nothing the edits before the landing kept")
	}
	parity("after-landing")

	// The flip: nothing above the fold; the next edit stands on it alone.
	h.chain = []int64{fold}
	edit(6)
	_, flipped := keyedBuild("over-the-flipped-fold")
	if flipped.StackCacheKey != key {
		t.Fatalf("the edit over the flipped fold moved the key:\n%q\n%q", flipped.StackCacheKey, key)
	}
}

// chainOverlayCacheHits sums a stack cache's hits over every per-stack read.
func chainOverlayCacheHits(c *graph.BaseProjectionCache) int {
	total := 0
	for _, stats := range []func() (int, int){
		c.StackFileNodeStats, c.StackNodeStats, c.StackFileStats, c.StackNameStats, c.StackPlacementStats,
		c.StackInIdentityStats, c.StackStats, c.StackRecordedEdgeStats, c.StackLayerAdjacencyStats, c.StackRefFactStats,
	} {
		hits, _ := stats()
		total += hits
	}
	return total
}
