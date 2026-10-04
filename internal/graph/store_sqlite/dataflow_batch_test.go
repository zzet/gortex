package store_sqlite

import (
	"fmt"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

func TestScanEdgesByKindsBatchedUsesFixedHighWaterAcrossReindex(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "graph.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })

	const count = 8
	edges := make([]*graph.Edge, 0, count+1)
	for i := 0; i < count; i++ {
		edges = append(edges, &graph.Edge{
			From: fmt.Sprintf("caller-%d", i), To: "callee",
			Kind: graph.EdgeArgOf, FilePath: "repo/caller.go", Line: i + 1,
			Meta: map[string]any{"arg_position": 0, "keep": i},
		})
	}
	edges = append(edges, &graph.Edge{From: "ignored", To: "callee", Kind: graph.EdgeCalls})
	store.AddBatch(nil, edges)

	seen := make(map[string]int, count)
	batches := 0
	store.ScanEdgesByKindsBatched([]graph.EdgeKind{graph.EdgeArgOf, graph.EdgeReturnsTo}, 3, func(batch []*graph.Edge) bool {
		batches++
		reindexes := make([]graph.EdgeReindex, 0, len(batch))
		for _, edge := range batch {
			seen[edge.From]++
			oldTo := edge.To
			edge.To = "param#param:0"
			reindexes = append(reindexes, graph.EdgeReindex{
				Edge: edge, OldFrom: edge.From, OldTo: oldTo,
				RefreshIdentity: true, OldFilePath: edge.FilePath, OldLine: edge.Line,
			})
		}
		store.ReindexEdges(reindexes)
		return true
	})

	assert.Equal(t, 3, batches)
	require.Len(t, seen, count)
	for from, visits := range seen {
		assert.Equalf(t, 1, visits, "rewritten row %s re-entered the same scan", from)
	}
}

func TestReindexEdgesMovesSourceIdentitySetOrientedAndWarmNoops(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "graph.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })

	const (
		oldFrom = "repo/caller.go::Caller"
		newFrom = "repo/callee.go::Callee"
		to      = "repo/caller.go::result"
		file    = "repo/caller.go"
	)
	store.AddBatch(nil, []*graph.Edge{{
		From: oldFrom, To: to, Kind: graph.EdgeReturnsTo,
		FilePath: file, Line: 23, Origin: "dataflow", Confidence: 0.91,
		Meta: map[string]any{"returns_to_call": true, "keep": "metadata"},
	}})
	moved := &graph.Edge{
		From: newFrom, To: to, Kind: graph.EdgeReturnsTo,
		FilePath: file, Line: 23, Origin: "dataflow", Confidence: 0.91,
		Meta: map[string]any{"returns_to_call": true, "keep": "metadata"},
	}
	mutation := graph.EdgeReindex{
		Edge: moved, OldFrom: oldFrom, OldTo: to,
		RefreshIdentity: true, OldFilePath: file, OldLine: 23,
	}
	stats, err := store.reindexEdgesSetOriented([]graph.EdgeReindex{mutation})
	require.NoError(t, err)
	assert.Equal(t, 1, stats.selectStatements)
	assert.Equal(t, 2, stats.writeStatements())
	assert.Equal(t, 1, stats.deletedRows)
	assert.Equal(t, 1, stats.insertedRows)
	assert.Empty(t, store.GetOutEdges(oldFrom))
	out := store.GetOutEdges(newFrom)
	require.Len(t, out, 1)
	assert.Equal(t, to, out[0].To)
	assert.Equal(t, "metadata", out[0].Meta["keep"])
	assert.Equal(t, 0.91, out[0].Confidence)

	warmStats, err := store.reindexEdgesSetOriented([]graph.EdgeReindex{mutation})
	require.NoError(t, err)
	assert.Zero(t, warmStats.writeStatements())
	assert.Zero(t, warmStats.deletedRows)
	assert.Zero(t, warmStats.insertedRows)
}

func TestDataflowAdjacencyBatchesFilterKindsAndSkipMetaDecode(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "graph.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })

	store.AddBatch(nil, []*graph.Edge{
		{From: "p", To: "callee", Kind: graph.EdgeParamOf, Meta: map[string]any{"large": "ignored"}},
		{From: "other", To: "callee", Kind: graph.EdgeReferences, Meta: map[string]any{"large": "ignored"}},
		{From: "caller", To: "target", Kind: graph.EdgeCalls, Line: 9, Meta: map[string]any{"large": "ignored"}},
		{From: "caller", To: "x", Kind: graph.EdgeContains, Meta: map[string]any{"large": "ignored"}},
	})

	params := store.GetDataflowParamEdgesByOwnerIDs([]string{"callee", "callee"})
	require.Len(t, params["callee"], 1)
	assert.Equal(t, graph.EdgeParamOf, params["callee"][0].Kind)
	assert.Nil(t, params["callee"][0].Meta)
	calls := store.GetDataflowCallEdgesByCallerIDs([]string{"caller", "caller"})
	require.Len(t, calls["caller"], 1)
	assert.Equal(t, graph.EdgeCalls, calls["caller"][0].Kind)
	assert.Nil(t, calls["caller"][0].Meta)
}

func TestDataflowBatchQueriesUseRowIDAndAdjacencyIndexes(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "graph.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })

	for _, viewGen := range []int64{0, 17} {
		t.Run(fmt.Sprintf("view-gen-%d", viewGen), func(t *testing.T) {
			pagePlan := queryPlan(t, store, `
				SELECT id, `+lookupEdgeCols+`
				FROM edges NOT INDEXED
				WHERE view_gen = ? AND id > ? AND id <= ? AND kind IN (?, ?)
				ORDER BY id LIMIT ?`, viewGen, 0, 100, string(graph.EdgeArgOf), string(graph.EdgeReturnsTo), 20)
			assert.Contains(t, pagePlan, "INTEGER PRIMARY KEY", "dataflow paging must seek in row-id order")
			assert.NotContains(t, pagePlan, "SCAN edges")
			assert.NotContains(t, pagePlan, "TEMP B-TREE", "a per-page residual sort would make pagination quadratic")

			paramPlan := queryPlan(t, store, `SELECT `+edgeColsLight+`
				FROM edges WHERE to_id IN (?) AND view_gen = ? AND kind = ? ORDER BY id`,
				"callee", viewGen, string(graph.EdgeParamOf))
			assert.Contains(t, paramPlan, "edges_by_to")
			assert.Contains(t, paramPlan, "to_id=?")
			assert.Contains(t, paramPlan, "view_gen=?")

			callPlan := queryPlan(t, store, `SELECT `+edgeColsLight+`
				FROM edges WHERE from_id IN (?) AND view_gen = ? AND kind = ? ORDER BY id`,
				"caller", viewGen, string(graph.EdgeCalls))
			assert.Contains(t, callPlan, "edges_by_from")
			assert.Contains(t, callPlan, "from_id=?")
			assert.Contains(t, callPlan, "view_gen=?")
		})
	}
}
