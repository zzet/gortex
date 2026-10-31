package store_sqlite

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

func TestKindAdjacencyPreservesFullRowsGenerationOrderAndCount(t *testing.T) {
	base, derived := openGenerationReadPair(t)
	for generation, s := range []*Store{base, derived} {
		var nodes []*graph.Node
		var edges []*graph.Edge
		for i := range 40 {
			id := fmt.Sprintf("typed-%02d", i)
			nodes = append(nodes, &graph.Node{ID: id, Kind: graph.KindFunction, FilePath: "typed.go"})
			kind := graph.EdgeDefines
			if i%3 == 0 {
				kind = graph.EdgeCalls
			}
			edges = append(edges, &graph.Edge{From: "typed-anchor", To: id, Kind: kind, FilePath: "typed.go", Line: 40 - i, Origin: graph.OriginASTResolved, Confidence: .8, Meta: map[string]any{"generation_marker": fmt.Sprint(generation)}})
		}
		nodes = append(nodes, &graph.Node{ID: "typed-anchor", Kind: graph.KindFunction, FilePath: "typed.go"})
		s.AddBatch(nodes, edges)
		for _, dir := range []graph.Direction{graph.DirectionForward, graph.DirectionBackward} {
			id := "typed-anchor"
			if dir == graph.DirectionBackward {
				id = "typed-00"
			}
			all := s.GetOutEdges(id)
			if dir == graph.DirectionBackward {
				all = s.GetInEdges(id)
			}
			var want []*graph.Edge
			for _, edge := range all {
				if edge.Kind == graph.EdgeCalls {
					want = append(want, edge)
				}
			}
			got, err := s.ReadKindAdjacency(t.Context(), id, dir, []graph.EdgeKind{graph.EdgeCalls, graph.EdgeCalls, ""})
			if err != nil || got.Total != len(all) || !reflect.DeepEqual(got.Edges, want) {
				t.Fatalf("generation=%d dir=%v: got=%+v want total=%d edges=%+v error=%v", generation, dir, got, len(all), want, err)
			}
		}
	}
}

func TestKindAdjacencySeeksEndpointAndKindAndCancels(t *testing.T) {
	s := openCatalogStore(t)
	for _, dir := range []graph.Direction{graph.DirectionForward, graph.DirectionBackward} {
		query := kindAdjacencyQuery(0, "anchor", dir, []graph.EdgeKind{graph.EdgeCalls}, graph.OutlineEdgeFilter{})
		rows, err := s.db.QueryContext(t.Context(), "EXPLAIN QUERY PLAN "+query.sql, query.args...)
		if err != nil {
			t.Fatal(err)
		}
		var plan []string
		for rows.Next() {
			var id, parent, unused int
			var detail string
			if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
				t.Fatal(err)
			}
			plan = append(plan, detail)
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		if err := rows.Close(); err != nil {
			t.Fatal(err)
		}
		joined := strings.Join(plan, "\n")
		if !strings.Contains(joined, "SEARCH e USING INDEX edges_by_") || !strings.Contains(joined, "kind=?") || strings.Contains(joined, "SCAN e ") {
			t.Fatalf("typed adjacency lost indexed kind seek: %s", joined)
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := s.ReadKindAdjacency(ctx, "anchor", graph.DirectionForward, []graph.EdgeKind{graph.EdgeCalls}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled typed adjacency: %v", err)
	}
}
