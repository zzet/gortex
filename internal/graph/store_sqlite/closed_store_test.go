package store_sqlite_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

// TestClosedStoreReadsDoNotPanic pins the teardown-race fix: after Close()
// has shut the store (daemon shutdown / restart / store swap), an in-flight
// reader — e.g. a deferred parallel-enrich goroutine still holding a cached
// *sql.Stmt — must degrade to an empty result, never panic the whole daemon.
// Before the fix this surfaced as `panic: store_sqlite: sql: statement is
// closed` from GetNode under runDeferredEnrichParallel.
func TestClosedStoreReadsDoNotPanic(t *testing.T) {
	s := openTestStore(t)
	s.AddNode(&graph.Node{ID: "p/a.go::Foo", Kind: graph.KindType, Name: "Foo", FilePath: "p/a.go"})
	require.NotNil(t, s.GetNode("p/a.go::Foo"), "sanity: node readable before close")

	require.NoError(t, s.Close())

	assert.NotPanics(t, func() {
		assert.Nil(t, s.GetNode("p/a.go::Foo"))
		assert.Empty(t, s.FindNodesByName("Foo"))
		assert.Empty(t, s.GetFileNodes("p/a.go"))
	}, "reads after Close must degrade gracefully, not panic")
}

// TestClosedStoreAggregatorsDoNotPanic pins the aggregator teardown-race sweep:
// each aggregator read runs its Query error through panicOnFatal, which
// swallows the "database is closed" race — leaving rows == nil. Every such site
// must early-return its empty value instead of dereferencing nil rows. The live
// crash was NodeIDsByKinds (FindHotspots -> RunAnalysis at watch-start) SIGSEGV
// on nil rows right after a long warmup. Each method is called with non-empty
// args so it actually reaches the Query rather than an early argument guard.
func TestClosedStoreAggregatorsDoNotPanic(t *testing.T) {
	s := openTestStore(t)
	s.AddNode(&graph.Node{ID: "p/a.go::Foo", Kind: graph.KindType, Name: "Foo", FilePath: "p/a.go"})
	s.AddNode(&graph.Node{ID: "p/a.go::bar", Kind: graph.KindFunction, Name: "bar", FilePath: "p/a.go"})
	s.AddEdge(&graph.Edge{From: "p/a.go::bar", To: "p/a.go::Foo", Kind: graph.EdgeReferences, FilePath: "p/a.go", Line: 1})
	require.NoError(t, s.Close())

	nodeKinds := []graph.NodeKind{graph.KindType, graph.KindFunction}
	edgeKinds := []graph.EdgeKind{graph.EdgeReferences}
	ids := []string{"p/a.go::Foo", "p/a.go::bar"}
	assert.NotPanics(t, func() {
		assert.Empty(t, s.InEdgeCountsByKind(edgeKinds))
		assert.Empty(t, s.NodeIDsByKinds(nodeKinds))
		assert.Empty(t, s.EdgeKindCounts())
		assert.Empty(t, s.NodeDegreeByKinds(nodeKinds, ""))
		assert.Empty(t, s.FileImportCounts(nil)) // exercises aggScanImportCounts
		assert.Empty(t, s.InDegreeForNodes(ids))
		assert.Empty(t, s.CrossRepoEdgeCounts())
		assert.Empty(t, s.FileImporters("p/a.go"))
		assert.Empty(t, s.FileSymbolNamesByPaths([]string{"p/a.go"}, nodeKinds))
		assert.Empty(t, s.NodeDegreeCounts(ids, edgeKinds))
		assert.Empty(t, s.NodeFanCounts(ids, edgeKinds, edgeKinds))
		assert.Empty(t, s.CommunityCrossingsByKind(edgeKinds, map[string]string{"p/a.go::bar": "c0"}))
		// Iterator-shaped: the Query runs inside the yield closure.
		n := 0
		for range s.EdgeAdjacencyForKinds(edgeKinds, nodeKinds) {
			n++
		}
		assert.Zero(t, n)
	}, "aggregator reads after Close must degrade to empty, not panic")
}

func TestReindexEdgesAppliedClosedStore(t *testing.T) {
	s := openTestStore(t)
	require.NoError(t, s.Close())
	assert.False(t, s.ReindexEdgesApplied([]graph.EdgeReindex{{
		Edge:  &graph.Edge{From: "caller", To: "target", Kind: graph.EdgeCalls, FilePath: "a.go"},
		OldTo: "dep::example.org/lib::Fn",
	}}), "a teardown failure must not report an applied batch")
}
