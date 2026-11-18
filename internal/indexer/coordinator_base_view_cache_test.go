package indexer

import "testing"

// TestCheckoutBuildsReuseTheLayerBelowAcrossEdits pins that a checkout's
// builds open their layer below through one long-lived materializer, so the
// second edit reuses the masks of every published generation the first one
// opened instead of re-reading the identity masks of the whole chain (on the
// live store: most of an edit's plan).
func TestCheckoutBuildsReuseTheLayerBelowAcrossEdits(t *testing.T) {
	f, c, _ := semanticChainFixtureWith(t, semanticBindingTree(), false)
	coordinatorReconcile(t, c)

	semanticWriteUnit(t, f.worktree, accumulatedDirtyIndependent, 0, true)
	coordinatorReconcile(t, c)
	hits, misses, entries := c.baseViewMaterializer().LayerCacheStats()
	if misses == 0 || entries == 0 {
		t.Fatalf("first edit opened no cached layer: hits=%d misses=%d entries=%d", hits, misses, entries)
	}

	semanticWriteUnit(t, f.worktree, accumulatedDirtyIndependent, 1, true)
	coordinatorReconcile(t, c)
	after, _, _ := c.baseViewMaterializer().LayerCacheStats()
	if after <= hits {
		t.Fatalf("second edit reused no layer of the first: hits %d -> %d", hits, after)
	}
}
