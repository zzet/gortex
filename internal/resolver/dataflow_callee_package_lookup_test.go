package resolver

import (
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

// The same-package callee lookup is by name, filtered to the files of the
// changed file's package: a same-named function in another package is not a
// candidate (the edge binds to the package's own function), and a package
// with no such function leaves the edge unresolved even when another package
// declares one.
func TestDataflowCalleePackageLookupStaysInThePackage(t *testing.T) {
	build := func(withLocal bool) (*graph.Graph, *graph.Edge) {
		nodes := []*graph.Node{
			{ID: "pkg/a.go", Kind: graph.KindFile, Name: "a.go", FilePath: "pkg/a.go", Language: "go", RepoPrefix: "repo"},
			{ID: "pkg/a.go::Run", Kind: graph.KindFunction, Name: "Run", FilePath: "pkg/a.go", Language: "go", RepoPrefix: "repo"},
			{ID: "other/c.go", Kind: graph.KindFile, Name: "c.go", FilePath: "other/c.go", Language: "go", RepoPrefix: "repo"},
			{ID: "other/c.go::Helper", Kind: graph.KindFunction, Name: "Helper", FilePath: "other/c.go", Language: "go", RepoPrefix: "repo"},
		}
		if withLocal {
			nodes = append(nodes,
				&graph.Node{ID: "pkg/b.go", Kind: graph.KindFile, Name: "b.go", FilePath: "pkg/b.go", Language: "go", RepoPrefix: "repo"},
				&graph.Node{ID: "pkg/b.go::Helper", Kind: graph.KindFunction, Name: "Helper", FilePath: "pkg/b.go", Language: "go", RepoPrefix: "repo"})
		}
		flow := &graph.Edge{From: "pkg/a.go::Run", To: graph.UnresolvedMarker + "Helper", Kind: graph.EdgeArgOf, FilePath: "pkg/a.go", Line: 3}
		g := graph.New()
		g.AddBatch(nodes, []*graph.Edge{
			{From: "pkg/a.go::Run", To: graph.UnresolvedMarker + "Missing", Kind: graph.EdgeCalls, FilePath: "pkg/a.go", Line: 2},
			flow,
		})
		return g, flow
	}
	target := func(g *graph.Graph, flow *graph.Edge) string {
		for _, e := range g.GetOutEdges("pkg/a.go::Run") {
			if e.Kind == graph.EdgeArgOf && e.Line == flow.Line {
				return e.To
			}
		}
		return ""
	}

	g, flow := build(true)
	r := New(g)
	r.SetIncrementalSkip([]*graph.Edge{flow})
	r.ResolveFilesAndIncoming([]string{"pkg/a.go"})
	if got := target(g, flow); got != "pkg/b.go::Helper" {
		t.Fatalf("the dataflow edge bound to %q, want the same-package pkg/b.go::Helper", got)
	}

	g, flow = build(false)
	r = New(g)
	r.SetIncrementalSkip([]*graph.Edge{flow})
	r.ResolveFilesAndIncoming([]string{"pkg/a.go"})
	if got := target(g, flow); got != graph.UnresolvedMarker+"Helper" {
		t.Fatalf("the dataflow edge bound to %q; another package's Helper is not a same-package callee", got)
	}
}

// The same-package lookup reads the names in the package's repository only.
// Another repository's function at the same path and name is neither read nor
// a candidate: the edge binds to the package's own function instead of being
// left ambiguous between the two.
func TestDataflowCalleePackageLookupReadsThePackageRepository(t *testing.T) {
	nodes := []*graph.Node{
		{ID: "pkg/a.go", Kind: graph.KindFile, Name: "a.go", FilePath: "pkg/a.go", Language: "go", RepoPrefix: "repo"},
		{ID: "pkg/a.go::Run", Kind: graph.KindFunction, Name: "Run", FilePath: "pkg/a.go", Language: "go", RepoPrefix: "repo"},
		{ID: "pkg/b.go", Kind: graph.KindFile, Name: "b.go", FilePath: "pkg/b.go", Language: "go", RepoPrefix: "repo"},
		{ID: "pkg/b.go::Helper", Kind: graph.KindFunction, Name: "Helper", FilePath: "pkg/b.go", Language: "go", RepoPrefix: "repo"},
		{ID: "elsewhere:pkg/b.go::Helper", Kind: graph.KindFunction, Name: "Helper", FilePath: "pkg/b.go", Language: "go", RepoPrefix: "elsewhere"},
	}
	flow := &graph.Edge{From: "pkg/a.go::Run", To: graph.UnresolvedMarker + "Helper", Kind: graph.EdgeArgOf, FilePath: "pkg/a.go", Line: 3}
	g := graph.New()
	g.AddBatch(nodes, []*graph.Edge{
		{From: "pkg/a.go::Run", To: graph.UnresolvedMarker + "Missing", Kind: graph.EdgeCalls, FilePath: "pkg/a.go", Line: 2},
		flow,
	})
	r := New(g)
	r.SetIncrementalSkip([]*graph.Edge{flow})
	r.ResolveFilesAndIncoming([]string{"pkg/a.go"})
	for _, e := range g.GetOutEdges("pkg/a.go::Run") {
		if e.Kind == graph.EdgeArgOf && e.Line == flow.Line && e.To != "pkg/b.go::Helper" {
			t.Fatalf("the dataflow edge bound to %q, want the package repository's pkg/b.go::Helper", e.To)
		}
	}
}
