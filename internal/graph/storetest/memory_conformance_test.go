package storetest_test

import (
	"testing"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/storetest"
)

// TestMemoryStoreConformance proves the in-memory *graph.Graph satisfies the
// conformance suite. It is no longer a backend the daemon runs on — it is the
// indexer's cold-index staging buffer and the fixture the rest of the tree's
// tests build on — but it still implements the whole Store contract, and this
// is where that stays honest.
//
// It is also the only store that declares aliasing reads: it holds the caller's
// own pointers, so a read hands back the live struct. Production code must not
// depend on that; see storetest.ReadsAliasStore.
func TestMemoryStoreConformance(t *testing.T) {
	storetest.RunConformance(t, func(t *testing.T) graph.Store {
		return graph.New()
	}, storetest.Semantics{
		Reads: storetest.ReadsAliasStore,
		// The empty prefix has never been a bucket here: unprefixed nodes live
		// outside the per-repository index, so EvictRepo("") returns (0, 0).
		EvictsEmptyRepoPrefix: false,
		// Adjacency reads hand back the per-node append slice as it stands.
		// Sorting it would tax the cold-index staging path this store exists
		// to keep fast, for an order no staging caller reads.
		AdjacencyLineOrdered: false,
	})
}

func TestDeltaWriterReindexConvergenceConformance(t *testing.T) {
	factory := func(t *testing.T) graph.Store {
		return graph.NewDeltaWriter(graph.New(), nil)
	}
	t.Run("ReindexConvergencePayload", func(t *testing.T) { storetest.RunReindexConvergencePayload(t, factory) })
	t.Run("ReindexMovingSiblings", func(t *testing.T) { storetest.RunReindexMovingSiblings(t, factory) })
}
