package resolver

import (
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

func TestGoModuleFullReindexIncomingUnbounded(t *testing.T) {
	for _, n := range []int{100, graph.MaxIncomingSourceCandidateRows + 1} {
		g := graph.New()
		source := &graph.Node{ID: "a/main.go::Caller", Name: "Caller", Kind: graph.KindFunction, RepoPrefix: "a", FilePath: "a/main.go", Language: "go"}
		target := &graph.Node{ID: "b/lib.go::Hello", Name: "Hello", Kind: graph.KindFunction, RepoPrefix: "b", FilePath: "b/lib.go", Language: "go"}
		g.AddBatch([]*graph.Node{source, target}, nil)
		edges := make([]*graph.Edge, 0, n)
		for i := 0; i < n; i++ {
			edges = append(edges, &graph.Edge{From: source.ID, To: "unresolved::extern::example.org/lib::Hello", Kind: graph.EdgeCalls, FilePath: source.FilePath, Line: i + 1,
				Meta: map[string]any{"go_import": "example.org/lib::Hello"}})
		}
		g.AddBatch(nil, edges)
		cr := NewCrossRepo(g)
		cr.SetGoModules(map[string]string{"a": "example.org/app", "b": "example.org/lib"})
		err := cr.ResolveGoModuleIncomingForRepo("b")
		if err != nil {
			t.Fatal(err)
		}
		bound := 0
		for _, e := range g.GetOutEdges(source.ID) {
			if e.Kind == graph.EdgeCalls && e.To == target.ID {
				bound++
			}
		}
		if bound != n {
			t.Errorf("full-reindex rebind left %d of %d calls parked with a nil error", n-bound, n)
		}
	}
}
