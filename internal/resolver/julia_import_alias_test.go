package resolver

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser/languages"
)

// buildJuliaGraph extracts every fixture with the real Julia extractor and
// loads the result into a fresh graph, so ResolveAll runs against exactly
// the unresolved edges a live index would hold.
func buildJuliaGraph(t *testing.T, files map[string]string) graph.Store {
	t.Helper()
	g := graph.New()
	jl := languages.NewJuliaExtractor()
	for path, src := range files {
		r, err := jl.Extract(path, []byte(src))
		require.NoError(t, err, "extract %s", path)
		for _, n := range r.Nodes {
			g.AddNode(n)
		}
		for _, e := range r.Edges {
			g.AddEdge(e)
		}
	}
	return g
}

func juliaOutEdge(g graph.Store, fromID string, kind graph.EdgeKind) []*graph.Edge {
	var out []*graph.Edge
	for _, e := range g.GetOutEdges(fromID) {
		if e.Kind == kind {
			out = append(out, e)
		}
	}
	return out
}

// An `import M as A` renames a module for one file only, so `A.f(x)` has
// to reach whatever `M.f(x)` reaches. The extractor used to emit the
// nickname verbatim, giving `unresolved::A.f` — a name no module in the
// graph carries — so the aliased and unaliased spellings produced
// different graphs and nothing downstream could map one back to the
// other. Recording the alias in edge metadata did not help: the resolver
// reads the edge target, and nothing in the repository reads that Meta.
//
// This runs extraction through ResolveAll, which is where the difference
// would show up.
func TestJuliaImports_AliasedCallMatchesUnaliasedCall(t *testing.T) {
	const provider = "foo.jl"
	const providerSrc = `module Foo

process(x) = x + 1

end
`
	aliased := buildJuliaGraph(t, map[string]string{
		provider: providerSrc,
		"run.jl": "import Foo as F\nrun(x) = F.process(x)\n",
	})
	New(aliased).ResolveAll()

	plain := buildJuliaGraph(t, map[string]string{
		provider: providerSrc,
		"run.jl": "import Foo\nrun(x) = Foo.process(x)\n",
	})
	New(plain).ResolveAll()

	aliasedCalls := juliaOutEdge(aliased, "run.jl::run", graph.EdgeCalls)
	plainCalls := juliaOutEdge(plain, "run.jl::run", graph.EdgeCalls)
	require.Len(t, aliasedCalls, 1)
	require.Len(t, plainCalls, 1)
	assert.Equal(t, plainCalls[0].To, aliasedCalls[0].To,
		"the alias is a file-local nickname; it must not change what the call names")
	assert.Equal(t, "unresolved::Foo.process", aliasedCalls[0].To,
		"the call must name the imported module, not the local alias")

	// The module import edge carries the rename in the graph's canonical
	// field as well as in Meta — Meta is the half the SQLite edges table
	// persists, since it has no alias column.
	imports := juliaOutEdge(aliased, "run.jl", graph.EdgeImports)
	require.Len(t, imports, 1)
	assert.Equal(t, "F", imports[0].Alias)
	assert.Equal(t, "F", imports[0].Meta["alias"])
}

// A selective import binds names, not just a module, so each binding gets
// its own edge — the representation JS/TS already emits for
// `import { a, b as c } from "mod"`. Without it the selected names lived
// only in an edge Meta key that nothing in the repository reads.
func TestJuliaImports_SelectiveBindingsAreTraversable(t *testing.T) {
	g := buildJuliaGraph(t, map[string]string{
		"stats.jl": "module Stats\nmean(x) = x\nstd(x) = x\nend\n",
		"use.jl":   "using Stats: mean, std\nimport Stats: mean as average\nrun(x) = mean(x)\n",
	})
	New(g).ResolveAll()

	byTarget := map[string]*graph.Edge{}
	for _, e := range juliaOutEdge(g, "use.jl", graph.EdgeImports) {
		byTarget[e.To] = e
	}
	require.Contains(t, byTarget, "external::Stats::mean",
		"a selected name must be reachable as its own import edge")
	require.Contains(t, byTarget, "external::Stats::std")

	var renamed *graph.Edge
	for _, e := range juliaOutEdge(g, "use.jl", graph.EdgeImports) {
		if e.Alias != "" {
			renamed = e
		}
	}
	require.NotNil(t, renamed, "a renamed binding must record its local name")
	assert.Equal(t, "average", renamed.Alias)
	assert.Equal(t, "external::Stats::mean", renamed.To,
		"the edge still targets the upstream name; the alias is the local one")
}
