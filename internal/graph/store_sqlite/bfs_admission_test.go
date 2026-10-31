package store_sqlite

import (
	"context"
	"fmt"
	"regexp"
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

type bfsAdmissionPlanStep struct {
	detail    string
	ancestors []string
}

func bfsAdmissionPlan(t *testing.T, s *Store, query outlineQuery) []bfsAdmissionPlanStep {
	t.Helper()
	rows, err := s.db.QueryContext(t.Context(), "EXPLAIN QUERY PLAN "+query.sql, query.args...)
	require.NoError(t, err)
	defer rows.Close()
	details := map[int]string{}
	parents := map[int]int{}
	var plan []bfsAdmissionPlanStep
	for rows.Next() {
		var id, parent, unused int
		var detail string
		require.NoError(t, rows.Scan(&id, &parent, &unused, &detail))
		details[id], parents[id] = detail, parent
		step := bfsAdmissionPlanStep{detail: detail}
		for up := parent; up != 0; up = parents[up] {
			step.ancestors = append(step.ancestors, details[up])
		}
		plan = append(plan, step)
	}
	require.NoError(t, rows.Err())
	return plan
}

// Every far-end test reads a covering index; the one full-row node seek
// hydrates walked identities behind the walk's fit gate, and edges are only
// ever reached from an endpoint.
func TestBFSAdmissionIndexPlanAndCancellation(t *testing.T) {
	s := openCatalogStore(t)
	nodeProbe := regexp.MustCompile(`^SEARCH n (EXISTS )?USING COVERING INDEX (nodes_by_repo|nodes_by_generation|nodes_by_kind) \(`)
	for _, dir := range []graph.Direction{graph.DirectionForward, graph.DirectionBackward} {
		_, _, index := bfsAdmissionColumns(dir)
		edgeSeek := regexp.MustCompile(`^SEARCH e USING (COVERING )?INDEX ` + index + ` \(`)
		for _, repos := range []map[string]bool{{"a": true}, nil} {
			for _, filter := range []graph.OutlineEdgeFilter{
				{ExcludeKinds: []graph.EdgeKind{graph.EdgeMatches}, ExcludeNodeKinds: []graph.NodeKind{graph.KindContract}, ExcludeVia: "spring.Bean"},
				{ExcludeNodeIDs: map[string]bool{"a/f.go::Owned": true}},
				{},
			} {
				request := graph.BFSAdmissionRequest{Seed: "a/f.go::Root", Direction: dir, Kinds: []graph.EdgeKind{graph.EdgeCalls, graph.EdgeMatches}, Depth: 3, Limit: 20, Scope: graph.NodeScope{RepoAllow: repos}}
				plan := bfsAdmissionPlan(t, s, bfsAdmissionQuery(0, request, filter))
				label := fmt.Sprintf("dir=%v repos=%v filter=%+v plan=%+v", dir, repos, filter, plan)
				var primary, automatic int
				var hydratedOuter string
				used := map[string]bool{}
				for _, step := range plan {
					if hydratedOuter == "" && len(step.ancestors) > 0 && step.ancestors[0] == "MATERIALIZE hydrated" {
						hydratedOuter = step.detail
					}
					switch {
					case strings.HasPrefix(step.detail, "SEARCH n USING PRIMARY KEY "):
						primary++
						require.Contains(t, step.ancestors, "MATERIALIZE hydrated", label)
					case strings.HasPrefix(step.detail, "SEARCH n ") || strings.HasPrefix(step.detail, "SCAN n"):
						match := nodeProbe.FindStringSubmatch(step.detail)
						require.NotNil(t, match, "node probe stays covering: %s", label)
						used[match[2]] = true
					case strings.HasPrefix(step.detail, "SEARCH e ") || strings.HasPrefix(step.detail, "SCAN e"):
						require.Regexp(t, edgeSeek, step.detail, label)
					case strings.HasPrefix(step.detail, "SEARCH h USING AUTOMATIC "):
						automatic++
					}
				}
				// The fit gate is hydrated's outermost loop, so an overflowing
				// walk performs no full-row node seek at all.
				require.Equal(t, "SCAN ok", hydratedOuter, label)
				require.Equal(t, 1, primary, label)
				require.Equal(t, 1, automatic, "rows join hydrated nodes through an automatic index: %s", label)
				require.Equal(t, repos != nil, used["nodes_by_repo"], label)
				require.Equal(t, len(filter.ExcludeNodeKinds) > 0, used["nodes_by_kind"], label)
				require.True(t, used["nodes_by_generation"], "missing far ends are probed: %s", label)
			}
		}
		request := graph.BFSAdmissionRequest{Seed: "a/f.go::Root", Direction: dir, Kinds: []graph.EdgeKind{graph.EdgeCalls}, Depth: 3, Limit: 20}
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
	// Edge overflow is found after hydration, which the fitting walk allowed.
	fits, hydrated := bfsAdmissionHydration(t, s, request)
	require.True(t, fits)
	require.Equal(t, 1, hydrated)
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
	fits, hydrated = bfsAdmissionHydration(t, stateStore, request)
	require.False(t, fits)
	require.Zero(t, hydrated, "an overflowing walk reads no full node row")
}

// bfsAdmissionHydration runs the admission statement's CTEs alone and reports
// the walk's fit flag and how many full node rows hydrated read.
func bfsAdmissionHydration(t *testing.T, s *Store, request graph.BFSAdmissionRequest) (fits bool, hydrated int) {
	t.Helper()
	query := bfsAdmissionQuery(s.viewGen, request, graph.OutlineEdgeFilter{})
	ctes, _, found := strings.Cut(query.sql, "SELECT NOT ok.fits")
	require.True(t, found)
	probe := ctes + "SELECT (SELECT fits FROM ok), (SELECT COUNT(*) FROM hydrated)"
	require.NoError(t, s.db.QueryRowContext(t.Context(), probe, query.args[:strings.Count(ctes, "?")]...).Scan(&fits, &hydrated))
	return fits, hydrated
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
