package store_sqlite

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

// BenchmarkGenerationScopedReads measures the invariant the derived-view
// architecture relies on: reading a small generation must scale with that
// generation, not with generation zero beside it in the shared tables.
func BenchmarkGenerationScopedReads(b *testing.B) {
	const (
		baseRows       = 30_000
		generationRows = 32
	)
	_, generation := newGenerationReadBenchmarkStore(b, baseRows, generationRows)
	b.ReportMetric(baseRows, "base_rows")
	b.ReportMetric(generationRows, "generation_rows")

	b.Run("AllNodes", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			if got := len(generation.AllNodes()); got != generationRows+1 {
				b.Fatalf("AllNodes() = %d rows, want %d", got, generationRows+1)
			}
		}
	})
	b.Run("AllEdges", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			if got := len(generation.AllEdges()); got != generationRows {
				b.Fatalf("AllEdges() = %d rows, want %d", got, generationRows)
			}
		}
	})
	b.Run("NodesByKind", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			got := 0
			for range generation.NodesByKind(graph.KindMethod) {
				got++
			}
			if got != generationRows {
				b.Fatalf("NodesByKind(method) = %d rows, want %d", got, generationRows)
			}
		}
	})
	b.Run("EdgesByKind", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			got := 0
			for range generation.EdgesByKind(graph.EdgeMemberOf) {
				got++
			}
			if got != generationRows {
				b.Fatalf("EdgesByKind(member_of) = %d rows, want %d", got, generationRows)
			}
		}
	})
	b.Run("MemberMethodsByType", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			got := generation.MemberMethodsByType()
			if methods := len(got["generation::type"]); methods != generationRows {
				b.Fatalf("MemberMethodsByType()[generation::type] = %d rows, want %d", methods, generationRows)
			}
		}
	})
}

func TestGenerationScopedReadsStayIsolated(t *testing.T) {
	base, generation := newGenerationReadBenchmarkStore(t, 4, 2)

	if got := base.NodeCount(); got != 5 {
		t.Fatalf("base NodeCount() = %d, want 5", got)
	}
	if got := base.EdgeCount(); got != 4 {
		t.Fatalf("base EdgeCount() = %d, want 4", got)
	}
	if got := generation.NodeCount(); got != 3 {
		t.Fatalf("generation NodeCount() = %d, want 3", got)
	}
	if got := generation.EdgeCount(); got != 2 {
		t.Fatalf("generation EdgeCount() = %d, want 2", got)
	}
	if got := len(generation.AllNodes()); got != 3 {
		t.Fatalf("generation AllNodes() = %d rows, want 3", got)
	}
	if got := len(generation.AllEdges()); got != 2 {
		t.Fatalf("generation AllEdges() = %d rows, want 2", got)
	}
	methods := generation.MemberMethodsByType()
	if got := len(methods["generation::type"]); got != 2 {
		t.Fatalf("generation member methods = %d, want 2", got)
	}
	if _, leaked := methods["base::type"]; leaked {
		t.Fatal("generation MemberMethodsByType leaked base rows")
	}
}

func TestGenerationScopedReadPlansUseGenerationIndexes(t *testing.T) {
	_, generation := newGenerationReadBenchmarkStore(t, 4, 2)
	tests := []struct {
		name  string
		query string
		index string
		args  []any
	}{
		{name: "all nodes", query: generationAllNodesSQL, index: "nodes_by_generation", args: []any{generation.viewGen}},
		{name: "node count", query: generationNodeCountSQL, index: "nodes_by_generation", args: []any{generation.viewGen}},
		{name: "nodes by kind", query: generationNodesByKindSQL, index: "nodes_by_generation", args: []any{generation.viewGen, string(graph.KindMethod)}},
		{name: "all edges", query: generationAllEdgesSQL, index: "edges_by_generation", args: []any{generation.viewGen}},
		{name: "edge count", query: generationEdgeCountSQL, index: "edges_by_generation", args: []any{generation.viewGen}},
		{name: "edges by kind", query: generationEdgesByKindSQL, index: "edges_by_generation", args: []any{generation.viewGen, string(graph.EdgeMemberOf)}},
		{name: "member methods", query: generationMemberMethodsByTypeSQL, index: "edges_by_generation", args: []any{generation.viewGen, string(graph.EdgeMemberOf), string(graph.KindMethod)}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			plan := generationQueryPlan(t, generation, tt.query, tt.args...)
			if !strings.Contains(plan, tt.index) {
				t.Fatalf("query plan does not use %s:\n%s", tt.index, plan)
			}
			if strings.Contains(plan, "SCAN nodes") || strings.Contains(plan, "SCAN edges") || strings.Contains(plan, "SCAN e") {
				t.Fatalf("query plan scans a graph table:\n%s", plan)
			}
		})
	}
}

func generationQueryPlan(t *testing.T, store *Store, query string, args ...any) string {
	t.Helper()
	rows, err := store.db.Query("EXPLAIN QUERY PLAN "+query, args...)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()

	var details []string
	for rows.Next() {
		var id, parent, notUsed int
		var detail string
		if err := rows.Scan(&id, &parent, &notUsed, &detail); err != nil {
			t.Fatal(err)
		}
		details = append(details, detail)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return strings.Join(details, "\n")
}

func newGenerationReadBenchmarkStore(tb testing.TB, baseRows, generationRows int) (*Store, *Store) {
	tb.Helper()
	store, err := Open(":memory:")
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() { _ = store.Close() })

	addMemberRows(tb, store, "base", baseRows)
	_, generation, _, err := store.BeginPayloadGenerationWithStatus(context.Background(), PayloadGenerationRequest{
		OwnerKind:      "benchmark",
		GraphID:        "benchmark-graph",
		LayerID:        "benchmark-layer",
		GenerationKind: "dirty",
		CreatedAt:      1,
	})
	if err != nil {
		tb.Fatal(err)
	}
	addMemberRows(tb, generation, "generation", generationRows)
	return store, generation
}

func addMemberRows(tb testing.TB, store *Store, prefix string, count int) {
	tb.Helper()
	typeID := prefix + "::type"
	store.AddNode(&graph.Node{ID: typeID, Name: "Type", Kind: graph.KindType, FilePath: prefix + "/type.go"})
	const chunkSize = 1_000
	for start := 0; start < count; start += chunkSize {
		end := min(start+chunkSize, count)
		nodes := make([]*graph.Node, 0, end-start)
		edges := make([]*graph.Edge, 0, end-start)
		for i := start; i < end; i++ {
			methodID := fmt.Sprintf("%s::method::%06d", prefix, i)
			filePath := fmt.Sprintf("%s/method_%06d.go", prefix, i)
			nodes = append(nodes, &graph.Node{
				ID: methodID, Name: "Method", Kind: graph.KindMethod, FilePath: filePath,
			})
			edges = append(edges, &graph.Edge{
				From: methodID, To: typeID, Kind: graph.EdgeMemberOf, FilePath: filePath, Line: i + 1,
			})
		}
		store.AddBatch(nodes, edges)
	}
}
