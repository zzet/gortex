package mcp

import (
	"context"
	"fmt"
	"iter"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

type coverageCandidateSpy struct {
	*kindAdjacencySpy
	nodes           graph.NodeScopedDeadCodeReader
	ancestors       graph.FilteredCallAncestorReader
	candidateRows   int
	ancestorQueries int
}

func (r *coverageCandidateSpy) CallAncestorsFilteredContext(ctx context.Context, ids []string, filter graph.OutlineEdgeFilter) ([]graph.CallAncestorRow, error) {
	r.ancestorQueries++
	rows, err := r.ancestors.CallAncestorsFilteredContext(ctx, ids, filter)
	r.rows += len(rows)
	return rows, err
}

func (r *coverageCandidateSpy) NodesByKindInScope(kind graph.NodeKind, repos []string, scope graph.NodeScope) iter.Seq[*graph.Node] {
	return func(yield func(*graph.Node) bool) {
		for node := range r.nodes.NodesByKindInScope(kind, repos, scope) {
			r.candidateRows++
			if !yield(node) {
				return
			}
		}
	}
}
func (r *coverageCandidateSpy) DeadCodeCandidatesInScope(kinds []graph.NodeKind, incoming map[graph.NodeKind][]graph.EdgeKind, repos []string, scope graph.NodeScope, exclusions graph.DeadCodeCandidateExclusions) []*graph.Node {
	return r.nodes.DeadCodeCandidatesInScope(kinds, incoming, repos, scope, exclusions)
}

func TestUntestedCandidateClosureMatchesWholeGraphOracle(t *testing.T) {
	s := openContractCoreCapStore(t)
	s.AddBatch([]*graph.Node{
		{ID: "a/live.go::Live", Kind: graph.KindFunction, RepoPrefix: "a", FilePath: "a/live.go"},
		{ID: "a/dead.go::Dead", Kind: graph.KindFunction, RepoPrefix: "a", FilePath: "a/dead.go"},
		{ID: "a/bean.go::Bean", Kind: graph.KindFunction, RepoPrefix: "a", FilePath: "a/bean.go"},
		{ID: "a/cycle.go::Cycle1", Kind: graph.KindFunction, RepoPrefix: "a", FilePath: "a/cycle.go"},
		{ID: "a/cycle.go::Cycle2", Kind: graph.KindFunction, RepoPrefix: "a", FilePath: "a/cycle.go"},
		{ID: "a/dangling.go::Dangling", Kind: graph.KindFunction, RepoPrefix: "a", FilePath: "a/dangling.go"},
		{ID: "b/foreign_test.go::Test", Kind: graph.KindFunction, RepoPrefix: "b", FilePath: "b/foreign_test.go"},
		{ID: "b/helper.go::Helper", Kind: graph.KindFunction, RepoPrefix: "b", FilePath: "b/helper.go"},
		{ID: "contract", Kind: graph.KindContract, FilePath: "contract_test.go"},
	}, []*graph.Edge{
		{From: "b/foreign_test.go::Test", To: "b/helper.go::Helper", Kind: graph.EdgeCalls},
		{From: "b/helper.go::Helper", To: "a/live.go::Live", Kind: graph.EdgeReferences},
		{From: "b/foreign_test.go::Test", To: "a/bean.go::Bean", Kind: graph.EdgeCalls, Meta: map[string]any{"via": "spring.Bean"}},
		{From: "contract", To: "a/dead.go::Dead", Kind: graph.EdgeCalls},
		{From: "b/foreign_test.go::Test", To: "a/dead.go::Dead", Kind: graph.EdgeImports},
		{From: "a/cycle.go::Cycle1", To: "a/cycle.go::Cycle2", Kind: graph.EdgeCalls},
		{From: "a/cycle.go::Cycle2", To: "a/cycle.go::Cycle1", Kind: graph.EdgeCalls},
		{From: "b/foreign_test.go::Test", To: "missing-intermediate", Kind: graph.EdgeCalls},
		{From: "missing-intermediate", To: "a/dangling.go::Dangling", Kind: graph.EdgeReferences},
	})
	var baseCandidateRows, baseEdgeRows int
	for pass := range 2 {
		spy := &coverageCandidateSpy{kindAdjacencySpy: &kindAdjacencySpy{Reader: s, backend: s}, nodes: s, ancestors: s}
		reader := newContractCoreEdges(spy, t.Context(), nil)
		scope := graph.NodeScope{RepoAllow: map[string]bool{"a": true}}
		candidates, err := untestedCandidates(t.Context(), reader, scope, "a/")
		require.NoError(t, err)
		require.Len(t, candidates, 6)
		covered, counts, err := candidateTestCoverage(t.Context(), reader, candidates)
		require.NoError(t, err)
		require.Equal(t, 1, spy.ancestorQueries, "one indexed closure statement replaces per-ancestor point reads")
		require.Zero(t, spy.queries)
		require.True(t, covered["a/live.go::Live"], "a real foreign test path still proves coverage")
		require.True(t, covered["a/dangling.go::Dangling"], "the original forward walk follows missing intermediate identities")
		require.False(t, covered["a/dead.go::Dead"])
		require.False(t, covered["a/bean.go::Bean"])
		require.False(t, covered["a/cycle.go::Cycle1"])
		require.Zero(t, spy.allNodes)
		require.Zero(t, spy.allEdges)
		if pass == 0 {
			baseCandidateRows, baseEdgeRows = spy.candidateRows, spy.rows
		} else {
			require.Equal(t, baseCandidateRows, spy.candidateRows)
			require.Equal(t, baseEdgeRows, spy.rows, "disconnected test graphs do not increase returned ancestor rows")
		}
		oracle := newContractCoreEdges(s, t.Context(), nil)
		wantCovered := reachableFromTests(legacyOutlineReader{oracle})
		wantCounts := collectFanInByKind(oracle, []graph.EdgeKind{graph.EdgeCalls, graph.EdgeReferences})
		for _, candidate := range candidates {
			require.Equal(t, wantCovered[candidate.ID], covered[candidate.ID], candidate.ID)
			require.Equal(t, wantCounts[candidate.ID], counts[candidate.ID], candidate.ID)
		}
		if pass == 0 {
			for i := range 5 {
				prefix := fmt.Sprintf("unrelated-%d", i)
				s.AddBatch([]*graph.Node{{ID: prefix + "/test", Kind: graph.KindFunction, RepoPrefix: prefix, FilePath: prefix + "/file_test.go"}, {ID: prefix + "/target", Kind: graph.KindFunction, RepoPrefix: prefix, FilePath: prefix + "/file.go"}}, []*graph.Edge{{From: prefix + "/test", To: prefix + "/target", Kind: graph.EdgeCalls}})
			}
		}
	}
	layer := graph.NewOverlayLayer()
	layer.MarkFile("b/foreign_test.go", true)
	reader := newContractCoreEdges(graph.NewOverlaidView(s, layer), t.Context(), nil)
	candidates, err := untestedCandidates(t.Context(), reader, graph.NodeScope{RepoAllow: map[string]bool{"a": true}}, "")
	require.NoError(t, err)
	covered, _, err := candidateTestCoverage(t.Context(), reader, candidates)
	require.NoError(t, err)
	require.False(t, covered["a/live.go::Live"], "a tombstoned foreign test cannot prove coverage")
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, _, err = candidateTestCoverage(ctx, reader, candidates)
	require.ErrorIs(t, err, context.Canceled)
}
