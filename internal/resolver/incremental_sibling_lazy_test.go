package resolver

import (
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

// siblingReadStore records every file path a batch file-node read asks for.
type siblingReadStore struct {
	graph.Store
	requested map[string]int
}

func (s *siblingReadStore) GetFileNodesByPaths(paths []string) map[string][]*graph.Node {
	for _, path := range paths {
		s.requested[path]++
	}
	return s.Store.GetFileNodesByPaths(paths)
}

func (s *siblingReadStore) GetFileNodes(path string) []*graph.Node {
	s.requested[path]++
	return s.Store.GetFileNodes(path)
}

// The incremental attribution tail never reads the changed file's
// same-package files: a dataflow edge that needs a same-package callee is
// bound through a name lookup filtered to the package's files, so a file
// without one reads nothing and a file with one still binds exactly as
// before, without loading its package's nodes.
func TestIncrementalAttributionReadsSiblingsOnlyWhenADataflowEdgeNeedsThem(t *testing.T) {
	build := func(withDataflow bool) (*graph.Graph, *graph.Edge) {
		g := graph.New()
		nodes := []*graph.Node{
			{ID: "pkg/a.go", Kind: graph.KindFile, Name: "a.go", FilePath: "pkg/a.go", Language: "go", RepoPrefix: "repo"},
			{ID: "pkg/b.go", Kind: graph.KindFile, Name: "b.go", FilePath: "pkg/b.go", Language: "go", RepoPrefix: "repo"},
			{ID: "pkg/a.go::Run", Kind: graph.KindFunction, Name: "Run", FilePath: "pkg/a.go", Language: "go", RepoPrefix: "repo"},
			{ID: "pkg/b.go::Helper", Kind: graph.KindFunction, Name: "Helper", FilePath: "pkg/b.go", Language: "go", RepoPrefix: "repo"},
		}
		edges := []*graph.Edge{
			// Pending work so the incremental resolve runs its attribution tail.
			{From: "pkg/a.go::Run", To: graph.UnresolvedMarker + "Missing", Kind: graph.EdgeCalls, FilePath: "pkg/a.go", Line: 2},
		}
		var flow *graph.Edge
		if withDataflow {
			flow = &graph.Edge{From: "pkg/a.go::Run", To: graph.UnresolvedMarker + "Helper", Kind: graph.EdgeArgOf,
				FilePath: "pkg/a.go", Line: 3}
			edges = append(edges, flow)
		}
		g.AddBatch(nodes, edges)
		return g, flow
	}

	g, _ := build(false)
	store := &siblingReadStore{Store: g, requested: map[string]int{}}
	New(store).ResolveFilesAndIncoming([]string{"pkg/a.go"})
	if store.requested["pkg/b.go"] != 0 {
		t.Fatalf("a file without a dataflow edge read its sibling %d time(s)", store.requested["pkg/b.go"])
	}

	// The forward leg would bind the edge itself; the prior-unresolved skip
	// leaves it to the attribution tail's dataflow callee pass.
	g, flow := build(true)
	store = &siblingReadStore{Store: g, requested: map[string]int{}}
	r := New(store)
	r.SetIncrementalSkip([]*graph.Edge{flow})
	r.ResolveFilesAndIncoming([]string{"pkg/a.go"})
	if store.requested["pkg/b.go"] != 0 {
		t.Fatalf("the sibling file was read %d time(s); the same-package callee is found by name", store.requested["pkg/b.go"])
	}
	var bound bool
	for _, e := range g.GetOutEdges("pkg/a.go::Run") {
		if e.Kind == graph.EdgeArgOf && e.Line == flow.Line && e.To == "pkg/b.go::Helper" {
			bound = true
		}
	}
	if !bound {
		t.Fatal("the dataflow edge did not bind to the same-package callee")
	}
}
