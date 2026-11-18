package store_sqlite

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

func TestAnalysisProjectionStreamsScopedRowsAndHonorsEarlyStop(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "analysis-projection.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	nodes := make([]*graph.Node, 0, 258)
	for i := 0; i < 256; i++ {
		nodes = append(nodes, &graph.Node{
			ID:       "leaf-" + formatProjectionIndex(i),
			Kind:     graph.KindVariable,
			Name:     "leaf",
			FilePath: "pkg/leaf.go",
			Meta:     map[string]any{"large": "metadata stays out of the light projection"},
		})
	}
	nodes = append(nodes,
		&graph.Node{ID: "entry", Kind: graph.KindFunction, Name: "Serve", FilePath: "cmd/main.go", Meta: map[string]any{"entry_point": true}},
		&graph.Node{ID: "method", Kind: graph.KindMethod, Name: "Run", FilePath: "pkg/run.go", Meta: map[string]any{"visibility": "public"}},
	)
	store.AddBatch(nodes, []*graph.Edge{
		{From: "entry", To: "method", Kind: graph.EdgeCalls, Meta: map[string]any{"large": "edge metadata"}},
		{From: "entry", To: "leaf-000", Kind: graph.EdgeReferences, Meta: map[string]any{"large": "unrequested"}},
	})

	seenNodes := 0
	for node := range store.NodesLightSeq() {
		seenNodes++
		if node.Meta != nil {
			t.Fatalf("light node projection decoded Meta: %#v", node.Meta)
		}
		break
	}
	if seenNodes != 1 {
		t.Fatalf("early-stop light nodes = %d, want 1", seenNodes)
	}

	seenEdges := 0
	for edge := range store.EdgesLightSeq(graph.EdgeCalls) {
		seenEdges++
		if edge.Kind != graph.EdgeCalls || edge.Meta != nil {
			t.Fatalf("unexpected light edge projection: %#v", edge)
		}
	}
	if seenEdges != 1 {
		t.Fatalf("call projection rows = %d, want 1", seenEdges)
	}

	fullKinds := map[graph.NodeKind]map[string]any{}
	for node := range store.NodesByKindsSeq(graph.KindFunction, graph.KindMethod) {
		fullKinds[node.Kind] = node.Meta
	}
	if len(fullKinds) != 2 || fullKinds[graph.KindFunction]["entry_point"] != true || fullKinds[graph.KindMethod]["visibility"] != "public" {
		t.Fatalf("full kind projection lost scoring metadata: %#v", fullKinds)
	}

	assertAnalysisProjectionUsesIndex(t, store,
		edgesLightSnapshotSQL(2, 0),
		[]any{string(graph.EdgeCalls), string(graph.EdgeReferences), int64(0)},
		"edges_by_kind")
	assertAnalysisProjectionUsesIndex(t, store,
		`SELECT `+lookupNodeCols+` FROM nodes WHERE kind IN (?, ?)`,
		[]any{string(graph.KindFunction), string(graph.KindMethod)},
		"nodes_by_kind")
}

func TestEdgesLightSeqUsesSelectedGenerationKindPlan(t *testing.T) {
	store, _ := openTempStore(t)
	for _, kinds := range [][]graph.EdgeKind{{graph.EdgeImports}, {graph.EdgeCalls, graph.EdgeReferences}} {
		_, args := aggDedupeEdgeKinds(kinds)
		assertAnalysisProjectionUsesIndex(t, store,
			edgesLightSnapshotSQL(len(args), 0), append(args, int64(0)), "edges_by_kind")
	}
	// Positive generations keep the original selective predicate; they must
	// not scan matching kinds across every retained generation.
	positive := edgesLightSnapshotSQL(1, 1)
	require.Equal(t, `SELECT `+edgeColsLight+` FROM edges WHERE kind IN (?) AND view_gen = ?`, positive)
}

func TestEdgesLightSeqKeepsGenerationAndScalarParity(t *testing.T) {
	store, _ := openTempStore(t)
	stopStoreBackgroundReaders(store)
	for _, generation := range []int64{0, 1} {
		handle := store.AtGeneration(generation)
		require.NoError(t, handle.AddBatchChecked(nil, []*graph.Edge{
			{From: "pkg/a.go::Caller", To: "pkg/b.go::Imported", Kind: graph.EdgeImports,
				FilePath: "pkg/a.go", Line: int(generation)*10 + 1, Confidence: .8123456789,
				ConfidenceLabel: "high", Origin: graph.OriginASTResolved, Tier: "ast", CrossRepo: true,
				Meta: map[string]any{"source": "must stay out of light rows"}},
			{From: "pkg/a.go::Caller", To: "pkg/c.go::Called", Kind: graph.EdgeCalls,
				FilePath: "pkg/a.go", Line: int(generation)*10 + 2, Confidence: .625,
				ConfidenceLabel: "medium", Origin: graph.OriginASTInferred, Tier: "ast"},
		}))
	}
	for _, generation := range []int64{0, 1} {
		t.Run(formatProjectionIndex(int(generation)), func(t *testing.T) {
			handle := store.AtGeneration(generation)
			full := handle.GetOutEdges("pkg/a.go::Caller")
			require.Len(t, full, 2)
			var want, imports []*graph.Edge
			for _, edge := range full {
				require.GreaterOrEqual(t, edge.Line, int(generation)*10+1)
				require.LessOrEqual(t, edge.Line, int(generation)*10+2)
				copy := *edge
				copy.Meta = nil
				want = append(want, &copy)
				if copy.Kind == graph.EdgeImports {
					imports = append(imports, &copy)
				}
			}
			require.ElementsMatch(t, want, drainEdges(handle.EdgesLightSeq(graph.EdgeCalls, graph.EdgeImports, graph.EdgeCalls, "")))
			require.ElementsMatch(t, imports, drainEdges(handle.EdgesLightSeq(graph.EdgeImports)))
			require.Empty(t, drainEdges(handle.EdgesLightSeq()))
			require.Empty(t, drainEdges(handle.EdgesLightSeq("")))
			for range handle.EdgesLightSeq(graph.EdgeImports) {
				break
			}
			require.Zero(t, store.db.Stats().InUse, "early stop must release the single-statement cursor")
		})
	}
}

func TestEdgesLightSeqRaisesOnCorruptScalar(t *testing.T) {
	store := openScanFailStore(t)
	seedScanFixture(t, store)
	require.Len(t, drainEdges(store.EdgesLightSeq(graph.EdgeCalls)), 2)
	_, err := store.writerDB.Exec(`UPDATE edges SET line = 'not-an-integer' WHERE from_id = 'pkg/a.go::Alpha'`)
	require.NoError(t, err)
	assertStoreReadRaises(t, "EdgesLightSeq", func() {
		drainEdges(store.EdgesLightSeq(graph.EdgeCalls))
	})
}

func formatProjectionIndex(value int) string {
	return string([]byte{'0' + byte(value/100), '0' + byte((value/10)%10), '0' + byte(value%10)})
}

func assertAnalysisProjectionUsesIndex(t *testing.T, store *Store, query string, args []any, index string) {
	t.Helper()
	rows, err := store.db.Query(`EXPLAIN QUERY PLAN `+query, args...)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var plan strings.Builder
	for rows.Next() {
		var id, parent, notUsed int
		var detail string
		if err := rows.Scan(&id, &parent, &notUsed, &detail); err != nil {
			t.Fatal(err)
		}
		plan.WriteString(detail)
		plan.WriteByte('\n')
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(plan.String(), index) {
		t.Fatalf("query plan does not use %s:\n%s", index, plan.String())
	}
}
