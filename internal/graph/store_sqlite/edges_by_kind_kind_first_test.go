package store_sqlite

import (
	"context"
	"fmt"
	"sort"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

// The kind-first scan answers exactly what EdgesByKind answers, on the base
// corpus and on a derived generation, whatever other generations hold.
func TestEdgesByKindKindFirstMatchesEdgesByKind(t *testing.T) {
	store := openCatalogStore(t)
	node := func(id string) *graph.Node {
		return &graph.Node{ID: id, Kind: graph.KindFunction, Name: id, FilePath: "repo/a.go", RepoPrefix: "repo"}
	}
	store.AddBatch([]*graph.Node{node("repo/a.go::A"), node("repo/a.go::B")}, []*graph.Edge{
		{From: "repo/a.go::A", To: "repo/a.go::B", Kind: graph.EdgeCalls, FilePath: "repo/a.go", Line: 1},
		{From: "repo/a.go::A", To: "repo/a.go::B", Kind: graph.EdgeReferences, FilePath: "repo/a.go", Line: 2},
	})
	_, handle, err := store.BeginPayloadGeneration(context.Background(), PayloadGenerationRequest{
		OwnerKind: "ref_view", GraphID: "graph-kind", LayerID: "layer-kind", GenerationKind: "commit", TreeOID: "tree-kind", CreatedAt: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	handle.AddBatch([]*graph.Node{node("repo/a.go::A"), node("repo/a.go::C")}, []*graph.Edge{
		{From: "repo/a.go::A", To: "repo/a.go::C", Kind: graph.EdgeCalls, FilePath: "repo/a.go", Line: 3},
		{From: "repo/a.go::C", To: "repo/a.go::A", Kind: graph.EdgeCalls, FilePath: "repo/a.go", Line: 4},
	})
	render := func(edges func(func(*graph.Edge) bool)) []string {
		var out []string
		edges(func(e *graph.Edge) bool {
			out = append(out, fmt.Sprintf("%s-%s->%s:%d", e.From, e.Kind, e.To, e.Line))
			return true
		})
		sort.Strings(out)
		return out
	}
	for _, s := range []*Store{store, handle} {
		for _, kind := range []graph.EdgeKind{graph.EdgeCalls, graph.EdgeReferences} {
			want, got := render(s.EdgesByKind(kind)), render(s.EdgesByKindKindFirst(kind))
			if fmt.Sprint(want) != fmt.Sprint(got) {
				t.Errorf("generation %d, kind %s: EdgesByKind %v, kind-first %v", s.viewGen, kind, want, got)
			}
		}
	}
	if got := render(handle.EdgesByKindKindFirst(graph.EdgeCalls)); len(got) != 2 {
		t.Fatalf("the derived generation's kind-first scan read %v, want its own 2 calls", got)
	}
}
