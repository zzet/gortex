package indexer

import (
	"context"
	"testing"

	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

// A working-tree chain is one checkout's: over a commit layer another
// checkout built and this one adopted (sharedCommit), the checkout's own
// layer is rooted and chains, while another checkout's working-tree layer over
// the same commit is never taken for this checkout's parent.
func TestAChainOverAnAdoptedCommitLayerIsTheCheckoutsOwn(t *testing.T) {
	commit := store_sqlite.ViewGeneration{GenerationID: 1, GraphID: "g", TreeOID: "tree", CheckoutID: "primary",
		GenerationKind: CommitLayerGenerationKind, State: store_sqlite.ViewGenerationReady}
	layer := func(checkout string) store_sqlite.ViewGeneration {
		return store_sqlite.ViewGeneration{GenerationID: 2, GraphID: "g", TreeOID: "tree", CheckoutID: checkout,
			LayerID: dirtyLayerID(checkout), GenerationKind: DirtyLayerGenerationKind, BaseGenerationID: 1,
			State: store_sqlite.ViewGenerationReady}
	}
	c := &CheckoutCoordinator{checkoutID: "worktree"}
	if chain, ok, reason, err := c.dirtyChainRootFrom(context.Background(), layer("worktree"), commit, 0); err != nil || !ok || len(chain) != 1 {
		t.Fatalf("the checkout's own layer over an adopted commit layer: ok=%v reason=%q err=%v", ok, reason, err)
	}
	if _, ok, reason, _ := c.dirtyChainRootFrom(context.Background(), layer("other"), commit, 0); ok || reason != dirtyChainFallbackNoParent {
		t.Fatalf("another checkout's layer was taken as this checkout's parent: ok=%v reason=%q", ok, reason)
	}
}
