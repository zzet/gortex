package query

import (
	"fmt"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

// nameOnlyFixture reproduces the reported shape: a callable with many
// real call sites, exactly one of which the resolver bound. The rest sit
// on the bare-name placeholder — either because no receiver-type evidence
// tied them here, or because the cross-package guard reverted a weak
// guess to it.
func nameOnlyFixture(t *testing.T, unbound int) (*Engine, *graph.Node) {
	t.Helper()
	g := graph.New()
	target := &graph.Node{
		ID: "src/data/Techs.gd::get_tech", Kind: graph.KindFunction, Name: "get_tech",
		FilePath: "src/data/Techs.gd", Language: "gdscript",
	}
	g.AddNode(target)

	bound := &graph.Node{
		ID: "src/data/Codex.gd::_tech_articles", Kind: graph.KindFunction, Name: "_tech_articles",
		FilePath: "src/data/Codex.gd", Language: "gdscript",
	}
	g.AddNode(bound)
	g.AddEdge(&graph.Edge{
		From: bound.ID, To: target.ID, Kind: graph.EdgeCalls,
		FilePath: bound.FilePath, Line: 12, Origin: graph.OriginASTResolved,
	})

	for i := 0; i < unbound; i++ {
		caller := &graph.Node{
			ID:       fmt.Sprintf("src/systems/Research%03d.gd::run", i),
			Kind:     graph.KindFunction,
			Name:     "run",
			FilePath: fmt.Sprintf("src/systems/Research%03d.gd", i),
			Language: "gdscript",
		}
		g.AddNode(caller)
		g.AddEdge(&graph.Edge{
			From: caller.ID, To: "unresolved::*.get_tech", Kind: graph.EdgeCalls,
			FilePath: caller.FilePath, Line: 40 + i,
		})
	}
	return NewEngine(g), target
}

// TestFindNameOnlyCandidates_CountsUnboundCallSites is the reported bug:
// 23 call sites, 1 bound edge, and no way to learn the other 22 exist.
func TestFindNameOnlyCandidates_CountsUnboundCallSites(t *testing.T) {
	eng, target := nameOnlyFixture(t, 22)
	bound := eng.GetCallers(target.ID, QueryOptions{Depth: 1})
	require.Len(t, bound.Edges, 1, "fixture: exactly one caller resolves")

	got := eng.FindNameOnlyCandidates(target, bound.Edges, QueryOptions{})
	assert.Equal(t, 22, got.Total)
	assert.Len(t, got.Edges, 22)
	assert.Len(t, got.Nodes, 22)
}

// TestFindNameOnlyCandidates_ProjectsWithoutMutatingGraph pins the
// invariant that makes this safe to run on the read path: the returned
// edges are copies. The graph's own edge must keep pointing at the
// placeholder so a later resolver pass can still bind it.
func TestFindNameOnlyCandidates_ProjectsWithoutMutatingGraph(t *testing.T) {
	eng, target := nameOnlyFixture(t, 1)
	got := eng.FindNameOnlyCandidates(target, nil, QueryOptions{})
	require.Len(t, got.Edges, 1)

	cand := got.Edges[0]
	assert.Equal(t, target.ID, cand.To, "the projection points at the queried symbol")
	assert.Equal(t, graph.OriginTextMatched, cand.Origin, "name evidence rides the weakest tier")
	assert.Equal(t, true, cand.Meta["name_only"])
	assert.Equal(t, "unresolved::*.get_tech", cand.Meta["unresolved_target"])

	live := eng.Reader().GetInEdges("unresolved::*.get_tech")
	require.Len(t, live, 1)
	assert.Equal(t, "unresolved::*.get_tech", live[0].To, "the graph edge must be untouched")
	assert.Empty(t, live[0].Origin)
	assert.Nil(t, live[0].Meta["name_only"])
}

// TestFindNameOnlyCandidates_FreeFunctionPlaceholder covers the shape the
// old member-only lookup missed entirely: a free-function call parks on
// `unresolved::<name>`, not `unresolved::*.<name>`.
func TestFindNameOnlyCandidates_FreeFunctionPlaceholder(t *testing.T) {
	g := graph.New()
	target := &graph.Node{ID: "lib/util.py::helper", Kind: graph.KindFunction, Name: "helper", FilePath: "lib/util.py"}
	g.AddNode(target)
	g.AddNode(&graph.Node{ID: "app/main.py::run", Kind: graph.KindFunction, Name: "run", FilePath: "app/main.py"})
	g.AddEdge(&graph.Edge{From: "app/main.py::run", To: "unresolved::helper", Kind: graph.EdgeCalls, FilePath: "app/main.py", Line: 9})

	got := NewEngine(g).FindNameOnlyCandidates(target, nil, QueryOptions{})
	assert.Equal(t, 1, got.Total)
}

// TestFindNameOnlyCandidates_RepoPrefixedPlaceholder covers the
// multi-repo COPY rewrite, where every placeholder carries the owning
// repo's prefix.
func TestFindNameOnlyCandidates_RepoPrefixedPlaceholder(t *testing.T) {
	g := graph.New()
	target := &graph.Node{
		ID: "svc/lib/util.go::Helper", Kind: graph.KindFunction, Name: "Helper",
		FilePath: "svc/lib/util.go", RepoPrefix: "svc",
	}
	g.AddNode(target)
	g.AddNode(&graph.Node{ID: "svc/app/main.go::run", Kind: graph.KindFunction, Name: "run", FilePath: "svc/app/main.go", RepoPrefix: "svc"})
	g.AddEdge(&graph.Edge{From: "svc/app/main.go::run", To: "svc::unresolved::Helper", Kind: graph.EdgeCalls, FilePath: "svc/app/main.go", Line: 4})

	got := NewEngine(g).FindNameOnlyCandidates(target, nil, QueryOptions{})
	assert.Equal(t, 1, got.Total)
}

// TestFindNameOnlyCandidates_DoesNotDoubleCountBoundSites pins the
// dedupe: a site the resolver already bound is a verified usage, not a
// candidate, even when a placeholder edge for the same site survives.
func TestFindNameOnlyCandidates_DoesNotDoubleCountBoundSites(t *testing.T) {
	g := graph.New()
	target := &graph.Node{ID: "a.go::Get", Kind: graph.KindMethod, Name: "Get", FilePath: "a.go"}
	g.AddNode(target)
	g.AddNode(&graph.Node{ID: "b.go::use", Kind: graph.KindFunction, Name: "use", FilePath: "b.go"})
	// The same call site, seen twice: once bound, once still on a placeholder.
	boundEdge := &graph.Edge{From: "b.go::use", To: target.ID, Kind: graph.EdgeCalls, FilePath: "b.go", Line: 7}
	g.AddEdge(boundEdge)
	g.AddEdge(&graph.Edge{From: "b.go::use", To: "unresolved::*.Get", Kind: graph.EdgeCalls, FilePath: "b.go", Line: 7})

	got := NewEngine(g).FindNameOnlyCandidates(target, []*graph.Edge{boundEdge}, QueryOptions{})
	assert.Zero(t, got.Total, "an already-bound call site is a usage, not a candidate")
}

// TestFindNameOnlyCandidates_IgnoresStructuredPlaceholders guards the
// boundary: only bare-name shapes belong here. An `import::` / `extern::`
// placeholder carries evidence a name match does not and is owned by
// another resolver pass.
func TestFindNameOnlyCandidates_IgnoresStructuredPlaceholders(t *testing.T) {
	g := graph.New()
	target := &graph.Node{ID: "lib/util.py::helper", Kind: graph.KindFunction, Name: "helper", FilePath: "lib/util.py"}
	g.AddNode(target)
	g.AddNode(&graph.Node{ID: "app/main.py::run", Kind: graph.KindFunction, Name: "run", FilePath: "app/main.py"})
	g.AddEdge(&graph.Edge{From: "app/main.py::run", To: "unresolved::import::helper", Kind: graph.EdgeImports, FilePath: "app/main.py", Line: 1})
	g.AddEdge(&graph.Edge{From: "app/main.py::run", To: "unresolved::extern::pkg::helper", Kind: graph.EdgeCalls, FilePath: "app/main.py", Line: 2})

	got := NewEngine(g).FindNameOnlyCandidates(target, nil, QueryOptions{})
	assert.Zero(t, got.Total)
}

// TestFindNameOnlyCandidates_CapsRowsButNotCount pins that the rollup
// stays exact past the row cap — a truncated list that also truncated its
// own count would under-report the very uncertainty it exists to report.
func TestFindNameOnlyCandidates_CapsRowsButNotCount(t *testing.T) {
	eng, target := nameOnlyFixture(t, NameOnlyCandidateCap+37)

	got := eng.FindNameOnlyCandidates(target, nil, QueryOptions{})
	assert.Equal(t, NameOnlyCandidateCap+37, got.Total)
	assert.Len(t, got.Edges, NameOnlyCandidateCap)
}

// TestFindNameOnlyCandidates_ScopeFilterShrinksTheCount pins that an
// out-of-scope candidate leaves the rollup too. A count that outran the
// rows the same query would return is the same dishonesty in reverse.
func TestFindNameOnlyCandidates_ScopeFilterShrinksTheCount(t *testing.T) {
	g := graph.New()
	target := &graph.Node{
		ID: "svcA/lib/util.go::Helper", Kind: graph.KindFunction, Name: "Helper",
		FilePath: "svcA/lib/util.go", RepoPrefix: "svcA",
	}
	g.AddNode(target)
	for _, repo := range []string{"svcA", "svcB"} {
		g.AddNode(&graph.Node{
			ID: repo + "/app/main.go::run", Kind: graph.KindFunction, Name: "run",
			FilePath: repo + "/app/main.go", RepoPrefix: repo,
		})
		g.AddEdge(&graph.Edge{
			From: repo + "/app/main.go::run", To: "unresolved::Helper",
			Kind: graph.EdgeCalls, FilePath: repo + "/app/main.go", Line: 3,
		})
	}

	eng := NewEngine(g)
	all := eng.FindNameOnlyCandidates(target, nil, QueryOptions{})
	require.Equal(t, 2, all.Total, "fixture: both repos name the symbol")

	scoped := eng.FindNameOnlyCandidates(target, nil, QueryOptions{RepoAllow: map[string]bool{"svcA": true}})
	assert.Equal(t, 1, scoped.Total)
	assert.Len(t, scoped.Edges, 1)
}

// repoNarrowSpy narrows placeholder reads the way the SQLite store does and
// records the repositories each narrowed read named.
type repoNarrowSpy struct {
	*graph.Graph
	narrowed [][]string
}

func (s *repoNarrowSpy) GetInEdgesByNodeIDsFromRepos(ids, repos []string) (map[string][]*graph.Edge, bool, error) {
	s.narrowed = append(s.narrowed, slices.Sorted(slices.Values(repos)))
	out := make(map[string][]*graph.Edge)
	for to, edges := range s.GetInEdgesByNodeIDs(ids) {
		for _, edge := range edges {
			if source := s.GetNode(edge.From); source != nil && slices.Contains(repos, source.RepoPrefix) {
				out[to] = append(out[to], edge)
			}
		}
	}
	return out, true, nil
}

// TestFindNameOnlyCandidates_NarrowsPlaceholderReadOnlyForFewRepos: a scope
// naming a few repositories reads only their call sites, while a wider one,
// up to every repository that calls the name, reads every site instead of
// probing each against a long repository list. Both answer exactly as the
// unnarrowed read does.
func TestFindNameOnlyCandidates_NarrowsPlaceholderReadOnlyForFewRepos(t *testing.T) {
	g := graph.New()
	target := &graph.Node{
		ID: "r0/lib/util.go::Helper", Kind: graph.KindFunction, Name: "Helper",
		FilePath: "r0/lib/util.go", RepoPrefix: "r0",
	}
	g.AddNode(target)
	var repos []string
	for i := range 2*nameOnlyRepoNarrowMax + 1 {
		repo := fmt.Sprintf("r%d", i)
		repos = append(repos, repo)
		g.AddNode(&graph.Node{
			ID: repo + "/app/main.go::run", Kind: graph.KindFunction, Name: "run",
			FilePath: repo + "/app/main.go", RepoPrefix: repo,
		})
		g.AddEdge(&graph.Edge{
			From: repo + "/app/main.go::run", To: "unresolved::Helper",
			Kind: graph.EdgeCalls, FilePath: repo + "/app/main.go", Line: 3,
		})
	}

	spy := &repoNarrowSpy{Graph: g}
	for _, allowed := range []int{1, nameOnlyRepoNarrowMax, nameOnlyRepoNarrowMax + 1, len(repos)} {
		allow := map[string]bool{}
		for _, repo := range repos[:allowed] {
			allow[repo] = true
		}
		opts := QueryOptions{RepoAllow: allow}
		spy.narrowed = nil
		got := NewEngine(spy).FindNameOnlyCandidates(target, nil, opts)
		require.Equal(t, NewEngine(g).FindNameOnlyCandidates(target, nil, opts), got, "allowed=%d", allowed)
		require.Equal(t, allowed, got.Total)
		if allowed <= nameOnlyRepoNarrowMax {
			require.Equal(t, [][]string{append([]string{""}, repos[:allowed]...)}, spy.narrowed, "allowed=%d", allowed)
		} else {
			require.Empty(t, spy.narrowed, "allowed=%d", allowed)
		}
	}
}

// TestAttachNameOnlyCandidates_CountAlwaysRowsOnlyOnOptIn is the
// contract the MCP layer depends on: the count is unconditional so a thin
// answer is honestly uncertain, while the name-matched rows require the
// caller to ask for the weakest tier.
func TestAttachNameOnlyCandidates_CountAlwaysRowsOnlyOnOptIn(t *testing.T) {
	eng, target := nameOnlyFixture(t, 22)
	cands := eng.FindNameOnlyCandidates(target, nil, QueryOptions{})

	sg := eng.GetCallers(target.ID, QueryOptions{Depth: 1})
	sg.AttachNameOnlyCandidates(cands, false)
	assert.Equal(t, 22, sg.NameOnlyCandidates)
	assert.Len(t, sg.Edges, 1, "the default answer stays resolver-verified")

	sg = eng.GetCallers(target.ID, QueryOptions{Depth: 1})
	sg.AttachNameOnlyCandidates(cands, true)
	assert.Equal(t, 22, sg.NameOnlyCandidates)
	assert.Len(t, sg.Edges, 23, "min_tier:text_matched returns the candidate rows")
	assert.Equal(t, len(sg.Edges), sg.TotalEdges)
	assert.Equal(t, len(sg.Nodes), sg.TotalNodes)
}
