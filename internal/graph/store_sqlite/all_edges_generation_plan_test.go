package store_sqlite

import (
	"fmt"
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

// The generation-first adjacency indexes must not turn AllEdges into a
// generation-wide sort, including the generation-zero owning handle.
func TestAllEdgesGenerationPlanAndOrder(t *testing.T) {
	s, _ := openTempStore(t)
	names := []string{"source-c", "source-a", "source-b"}
	for _, generation := range []int64{0, 7} {
		var edges []*graph.Edge
		for _, name := range names {
			edges = append(edges, &graph.Edge{From: name, To: fmt.Sprintf("target-%d", generation), Kind: graph.EdgeCalls, FilePath: "order.go", Line: 1})
		}
		if err := s.AtGeneration(generation).AddBatchChecked(nil, edges); err != nil {
			t.Fatal(err)
		}
	}
	for _, generation := range []int64{0, 7} {
		query := baseAllEdgesSQL
		if generation > 0 {
			query = generationAllEdgesSQL
		}
		plan := strings.Join(explainQueryPlanArgs(t, s, query, generation), "\n")
		if !strings.Contains(plan, "edges_by_generation (view_gen=?)") || strings.Contains(plan, "TEMP B-TREE") || strings.Contains(plan, "SCAN edges") {
			t.Fatalf("generation %d export must seek ordered generation index: %s", generation, plan)
		}
		got := s.AtGeneration(generation).AllEdges()
		if len(got) != len(names) {
			t.Fatalf("generation %d rows = %d, want %d", generation, len(got), len(names))
		}
		for i, edge := range got {
			if edge.From != names[i] || edge.To != fmt.Sprintf("target-%d", generation) {
				t.Fatalf("generation %d row %d = %s -> %s; lost order or generation isolation", generation, i, edge.From, edge.To)
			}
		}
	}
}
