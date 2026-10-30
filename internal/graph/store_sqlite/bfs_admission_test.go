package store_sqlite

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

func TestBFSAdmissionPreservesPointPayloadsOrderAndGeneration(t *testing.T) {
	base, derived := openGenerationReadPair(t)
	for _, s := range []*Store{base, derived} {
		for _, dir := range []graph.Direction{graph.DirectionForward, graph.DirectionBackward} {
			request := graph.BFSAdmissionRequest{Seed: genReadShared, Direction: dir, Kinds: []graph.EdgeKind{graph.EdgeCalls, graph.EdgeReferences, graph.EdgeImports}, Depth: 3, Limit: 64,
				Scope: graph.NodeScope{RepoAllow: map[string]bool{genReadRepo: true, genReadOtherRepo: true}}}
			got, handled, err := s.BFSAdmissionFilteredContext(t.Context(), request, graph.OutlineEdgeFilter{})
			require.NoError(t, err)
			require.True(t, handled)
			for id, node := range got.Nodes {
				require.Equal(t, s.GetNode(id), node, "full neighbor payload generation=%d id=%s", s.viewGen, id)
			}
			for id, adjacency := range got.Adjacency {
				all := s.GetOutEdges(id)
				if dir == graph.DirectionBackward {
					all = s.GetInEdges(id)
				}
				var want []*graph.Edge
				for _, edge := range all {
					if edge.Kind != graph.EdgeCalls && edge.Kind != graph.EdgeReferences && edge.Kind != graph.EdgeImports {
						continue
					}
					neighbor := edge.To
					if dir == graph.DirectionBackward {
						neighbor = edge.From
					}
					node := s.GetNode(neighbor)
					if node == nil || graph.IsUnresolvedTarget(neighbor) || strings.HasPrefix(neighbor, "external::") || request.Scope.Allows(node) {
						want = append(want, edge)
					}
				}
				require.Equal(t, want, adjacency.Edges, "point-order metadata generation=%d anchor=%s", s.viewGen, id)
				require.Equal(t, min(len(all), request.Limit), adjacency.Total)
			}
		}
	}
}

func TestBFSAdmissionIndexPlanAndCancellation(t *testing.T) {
	s := openCatalogStore(t)
	for _, dir := range []graph.Direction{graph.DirectionForward, graph.DirectionBackward} {
		request := graph.BFSAdmissionRequest{Seed: "a/f.go::Root", Direction: dir, Kinds: []graph.EdgeKind{graph.EdgeCalls, graph.EdgeMatches}, Depth: 3, Limit: 20, Scope: graph.NodeScope{RepoAllow: map[string]bool{"a": true}}}
		query := bfsAdmissionQuery(0, request, graph.OutlineEdgeFilter{ExcludeNodeKinds: []graph.NodeKind{graph.KindContract}, ExcludeVia: "spring.Bean"})
		rows, err := s.db.QueryContext(t.Context(), "EXPLAIN QUERY PLAN "+query.sql, query.args...)
		require.NoError(t, err)
		var plan []string
		for rows.Next() {
			var id, parent, unused int
			var detail string
			require.NoError(t, rows.Scan(&id, &parent, &unused, &detail))
			plan = append(plan, detail)
		}
		require.NoError(t, rows.Err())
		require.NoError(t, rows.Close())
		joined := strings.Join(plan, "\n")
		require.GreaterOrEqual(t, strings.Count(joined, "SEARCH e USING INDEX edges_by_"), 3, "recursive, bounded-count and result reads stay endpoint-driven: %s", joined)
		require.Contains(t, joined, "kind=?")
		require.NotContains(t, joined, "SCAN e ")
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		_, handled, err := s.BFSAdmissionFilteredContext(ctx, request, graph.OutlineEdgeFilter{})
		require.ErrorIs(t, err, context.Canceled)
		require.False(t, handled)
	}
}

func TestBFSAdmissionBudgetsDeclineWithoutPartialResult(t *testing.T) {
	s := openCatalogStore(t)
	const root, child = "a/root.go::Root", "a/child.go::Child"
	s.AddBatch([]*graph.Node{{ID: root, Kind: graph.KindFunction, RepoPrefix: "a", FilePath: "a/root.go"}, {ID: child, Kind: graph.KindFunction, RepoPrefix: "a", FilePath: "a/child.go"}}, nil)
	request := graph.BFSAdmissionRequest{Seed: root, Direction: graph.DirectionForward, Kinds: []graph.EdgeKind{graph.EdgeCalls}, Depth: 3, Limit: 3, Scope: graph.NodeScope{RepoAllow: map[string]bool{"a": true}}}
	_, budget, _ := graph.BFSAdmissionBudgets(request)
	var edges []*graph.Edge
	for line := 1; line <= budget+1; line++ {
		edges = append(edges, &graph.Edge{From: root, To: child, Kind: graph.EdgeCalls, FilePath: "a/root.go", Line: line})
	}
	s.AddBatch(nil, edges)
	rows, handled, err := s.BFSAdmissionFilteredContext(t.Context(), request, graph.OutlineEdgeFilter{})
	require.NoError(t, err)
	require.False(t, handled)
	require.Empty(t, rows.Nodes)
	require.Empty(t, rows.Adjacency)
	// A wide state frontier also declines before any partial rows can escape.
	stateStore := openCatalogStore(t)
	stateStore.AddNode(&graph.Node{ID: root, Kind: graph.KindFunction, RepoPrefix: "a", FilePath: "a/root.go"})
	states, _, _ := graph.BFSAdmissionBudgets(request)
	var nodes []*graph.Node
	edges = nil
	for i := range states + 1 {
		id := fmt.Sprintf("a/n%04d.go::N", i)
		nodes = append(nodes, &graph.Node{ID: id, Kind: graph.KindFunction, RepoPrefix: "a"})
		edges = append(edges, &graph.Edge{From: root, To: id, Kind: graph.EdgeCalls})
	}
	stateStore.AddBatch(nodes, edges)
	rows, handled, err = stateStore.BFSAdmissionFilteredContext(t.Context(), request, graph.OutlineEdgeFilter{})
	require.NoError(t, err)
	require.False(t, handled)
	require.Empty(t, rows.Adjacency)
}

func TestBFSAdmissionScopeAndUnresolvedGatePrecedeRecursion(t *testing.T) {
	s := openCatalogStore(t)
	const root = "a/root.go::Root"
	s.AddBatch([]*graph.Node{
		{ID: root, Kind: graph.KindFunction, RepoPrefix: "a"},
		{ID: "b/out.go::Out", Kind: graph.KindFunction, RepoPrefix: "b"},
		{ID: "a/after.go::Foreign", Kind: graph.KindFunction, RepoPrefix: "a"},
		{ID: "a::unresolved::Ghost", Kind: graph.KindFunction, RepoPrefix: "a"},
		{ID: "a/after.go::Ghost", Kind: graph.KindFunction, RepoPrefix: "a"},
	}, []*graph.Edge{
		{From: root, To: "b/out.go::Out", Kind: graph.EdgeCalls},
		{From: "b/out.go::Out", To: "a/after.go::Foreign", Kind: graph.EdgeCalls},
		{From: root, To: "a::unresolved::Ghost", Kind: graph.EdgeCalls},
		{From: "a::unresolved::Ghost", To: "a/after.go::Ghost", Kind: graph.EdgeCalls},
	})
	request := graph.BFSAdmissionRequest{Seed: root, Direction: graph.DirectionForward, Kinds: []graph.EdgeKind{graph.EdgeCalls}, Depth: 3, Limit: 20, Scope: graph.NodeScope{RepoAllow: map[string]bool{"a": true}}}
	rows, handled, err := s.BFSAdmissionFilteredContext(t.Context(), request, graph.OutlineEdgeFilter{})
	require.NoError(t, err)
	require.True(t, handled)
	require.NotContains(t, rows.Nodes, "a/after.go::Foreign")
	require.NotContains(t, rows.Nodes, "a/after.go::Ghost")
	require.Len(t, rows.Adjacency[root].Edges, 1, "unresolved boundary remains, resolved foreign neighbor is rejected")
	require.Equal(t, "a::unresolved::Ghost", rows.Adjacency[root].Edges[0].To)
}
