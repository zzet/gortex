package indexer

import (
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

// A generation built over nothing (a dedicated base's empty layer below) has
// no redundant pathless copy to prune: the prune is a no-op, not a refusal
// because the empty layer serves no edges by recording file. The refusal made
// every initial dedicated-base publication fail and be retried, each attempt
// copying the whole corpus under the store's write gate.
func TestPathlessPruneOverAnEmptyLayerBelowIsANoOp(t *testing.T) {
	builderIsolateGit(t)
	store := builderOpenStore(t, "empty-below")
	repoDir := builderTempDir(t, "checkout")
	builderWriteTree(t, repoDir, stubAdjacencyTree(false))
	builderIndex(t, store, repoDir)
	unpathed := 0
	for _, n := range store.AllNodes() {
		if n.FilePath != "" || n.Kind == graph.KindBuiltin {
			continue
		}
		for _, e := range store.GetOutEdges(n.ID) {
			if e.FilePath == "" {
				unpathed++
			}
		}
	}
	nodes, edges, pruned, err := pruneRedundantPathless(store, graph.New())
	if err != nil {
		t.Fatalf("prune over an empty layer below refused: %v (fixture carries %d unpathed stub out-edges)", err, unpathed)
	}
	if nodes != 0 || edges != 0 || len(pruned) != 0 {
		t.Fatalf("prune over an empty layer below removed %d nodes, %d edges (%v)", nodes, edges, pruned)
	}
}
