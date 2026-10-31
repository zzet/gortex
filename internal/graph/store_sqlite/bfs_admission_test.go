package store_sqlite

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
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
// ever reached from an endpoint. A workspace gate adds two: the seed's row,
// and the row of a walked neighbor outside the seed's repository.
func TestBFSAdmissionIndexPlanAndCancellation(t *testing.T) {
	s := openCatalogStore(t)
	nodeProbe := regexp.MustCompile(`^SEARCH n (EXISTS )?USING COVERING INDEX (nodes_by_repo|nodes_by_generation|nodes_by_kind) \(`)
	for _, dir := range []graph.Direction{graph.DirectionForward, graph.DirectionBackward} {
		_, _, index := bfsAdmissionColumns(dir)
		edgeSeek := regexp.MustCompile(`^SEARCH e USING (COVERING )?INDEX ` + index + ` \(`)
		for _, scope := range []graph.NodeScope{{RepoAllow: map[string]bool{"a": true}}, {}, {WorkspaceID: "work", ProjectID: "project"}} {
			workspaceGate := scope.WorkspaceID != "" && len(scope.RepoAllow) == 0
			for _, filter := range []graph.OutlineEdgeFilter{
				{ExcludeKinds: []graph.EdgeKind{graph.EdgeMatches}, ExcludeNodeKinds: []graph.NodeKind{graph.KindContract}, ExcludeVia: "spring.Bean"},
				{ExcludeNodeIDs: map[string]bool{"a/f.go::Owned": true}},
				{},
			} {
				request := graph.BFSAdmissionRequest{Seed: "a/f.go::Root", Direction: dir, Kinds: []graph.EdgeKind{graph.EdgeCalls, graph.EdgeMatches}, Depth: 3, Limit: 20, Scope: scope}
				plan := bfsAdmissionPlan(t, s, bfsAdmissionQuery(0, request, filter))
				label := fmt.Sprintf("dir=%v scope=%+v filter=%+v plan=%+v", dir, scope, filter, plan)
				var automatic int
				primary := map[string]int{}
				var hydratedOuter string
				used := map[string]bool{}
				for _, step := range plan {
					if hydratedOuter == "" && len(step.ancestors) > 0 && step.ancestors[0] == "MATERIALIZE hydrated" {
						hydratedOuter = step.detail
					}
					switch {
					case strings.HasPrefix(step.detail, "SEARCH n USING PRIMARY KEY "):
						phase := "elsewhere"
						for _, ancestor := range step.ancestors {
							if ancestor == "MATERIALIZE hydrated" || ancestor == "MATERIALIZE seed" || ancestor == "RECURSIVE STEP" {
								phase = ancestor
								break
							}
						}
						primary[phase]++
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
				wantPrimary := map[string]int{"MATERIALIZE hydrated": 1}
				if workspaceGate {
					wantPrimary["MATERIALIZE seed"], wantPrimary["RECURSIVE STEP"] = 1, 1
				}
				require.Equal(t, wantPrimary, primary, label)
				require.Equal(t, 1, automatic, "rows join hydrated nodes through an automatic index: %s", label)
				require.Equal(t, len(scope.RepoAllow) > 0 || workspaceGate, used["nodes_by_repo"], label)
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

// A workspace scope without a repository narrow is a bound session's default
// for reach tools. The walk still follows its workspace into other
// repositories, but stops where the workspace ends: a foreign workspace's
// neighborhood, wider than the state budget, neither costs walk states nor
// declines the snapshot. Only the seed's repository is walked past the scope.
func TestBFSAdmissionWorkspaceGateStopsAtForeignWorkspaces(t *testing.T) {
	s := openCatalogStore(t)
	const (
		root      = "a/root.go::Root"
		sibling   = "b/api.go::Handler"
		beyond    = "b/next.go::Next"
		other     = "b/other.go::Other"
		elsewhere = "a/else.go::Else"
		foreign   = "c/out.go::Out"
		unowned   = "util.go::Global"
		global    = "ext::go:fmt::Errorf"
	)
	node := func(id, repo, workspace, project string) *graph.Node {
		return &graph.Node{ID: id, Kind: graph.KindFunction, FilePath: strings.Split(id, "::")[0], RepoPrefix: repo, WorkspaceID: workspace, ProjectID: project}
	}
	nodes := []*graph.Node{
		node(root, "a", "work", "a"), node(sibling, "b", "work", "a"), node(beyond, "b", "work", "a"), node(other, "b", "work", "b"),
		node(elsewhere, "a", "elsewhere", "a"), node(foreign, "c", "foreign", "c"), node(unowned, "", "", ""),
		{ID: global, Kind: graph.KindFunction, FilePath: "external::go:fmt"},
	}
	var edges []*graph.Edge
	both := func(from, to string) {
		edges = append(edges, &graph.Edge{From: from, To: to, Kind: graph.EdgeCalls}, &graph.Edge{From: to, To: from, Kind: graph.EdgeCalls})
	}
	for _, neighbor := range []string{sibling, other, elsewhere, foreign, unowned, global} {
		both(root, neighbor)
	}
	both(sibling, beyond)
	request := graph.BFSAdmissionRequest{Seed: root, Kinds: []graph.EdgeKind{graph.EdgeCalls}, Depth: 3, Limit: 20}
	states, _, _ := graph.BFSAdmissionBudgets(request)
	for i := range states + 1 {
		id := fmt.Sprintf("c/fan.go::Fan%04d", i)
		nodes = append(nodes, node(id, "c", "foreign", "c"))
		both(foreign, id)
	}
	s.AddBatch(nodes, edges)

	for _, scope := range []graph.NodeScope{{WorkspaceID: "work"}, {WorkspaceID: "work", ProjectID: "a"}} {
		for _, dir := range []graph.Direction{graph.DirectionForward, graph.DirectionBackward} {
			request.Direction, request.Scope = dir, scope
			label := fmt.Sprintf("dir=%v scope=%+v", dir, scope)
			got, handled, err := s.BFSAdmissionFilteredContext(t.Context(), request, graph.OutlineEdgeFilter{})
			require.NoError(t, err, label)
			require.True(t, handled, "a foreign workspace cannot overflow the walk: %s", label)
			walked := map[string]bool{}
			for id, n := range got.Nodes {
				walked[id] = true
				require.True(t, scope.Allows(n) || n.RepoPrefix == "a", "only the seed's repository is walked past the scope: %s %s", id, label)
			}
			require.True(t, walked[sibling], "another repository in the workspace is walked: %s", label)
			require.True(t, walked[beyond], label)
			require.True(t, walked[global], "a global external is in every scope: %s", label)
			require.True(t, walked[elsewhere], "the seed's repository is walked; admission rejects it: %s", label)
			require.Equal(t, scope.ProjectID == "", walked[other], "the project is checked across repositories: %s", label)
			require.False(t, walked[foreign], label)
			require.False(t, walked[unowned], "an unowned source node is outside the workspace: %s", label)
			var far []string
			for _, edge := range got.Adjacency[root].Edges {
				if dir == graph.DirectionBackward {
					far = append(far, edge.From)
				} else {
					far = append(far, edge.To)
				}
			}
			require.NotContains(t, far, foreign, "the rejected neighbor drops its edge: %s", label)
			require.NotContains(t, far, unowned, label)
			require.Contains(t, far, elsewhere, label)
			fits, hydrated := bfsAdmissionHydration(t, s, request)
			require.True(t, fits, label)
			require.Equal(t, len(got.Nodes), hydrated, label)
		}
	}
}

// previousBFSAdmissionQuery is the statement the covering-index probes
// replaced, kept as the snapshot oracle. It joined the full node row at the
// far end of every walked and returned edge, and at both ends of every
// walked, counted and returned edge under a node-kind mask.
func previousBFSAdmissionQuery(generation int64, request graph.BFSAdmissionRequest, filter graph.OutlineEdgeFilter) outlineQuery {
	states, edges, _ := graph.BFSAdmissionBudgets(request)
	anchor, next, index, neighbor := previousBFSAdmissionColumns(request.Direction)
	kindJSON, _ := json.Marshal(request.Kinds)
	kindPredicate := `e.kind IN (SELECT CAST(value AS TEXT) FROM json_each(?))`
	scope, scopeArgs := outlineScopePredicate(request.Scope, neighbor)
	mask, maskArgs := outlineEdgePredicate(filter, "source", "target")
	via, viaArgs := outlineViaPredicate(filter.ExcludeVia)
	structural := strings.NewReplacer("kind", "e.kind", "to_id", "e.to_id").Replace(structuralTargetInvalidSQL)
	visible := ` AND NOT ` + structural + mask + via
	resolved := `e.` + next + ` NOT GLOB 'unresolved::*' AND instr(e.` + next + `, '::unresolved::') = 0 AND e.` + next + ` NOT GLOB 'external::*'`
	q := `WITH RECURSIVE walk(id, depth) AS (
		SELECT ?, 0
		UNION
		SELECT e.` + next + `, b.depth + 1 FROM walk b
		CROSS JOIN edges e INDEXED BY ` + index + ` ON e.view_gen = ? AND e.` + anchor + ` = b.id AND ` + kindPredicate +
		previousBFSAdmissionJoins(request.Direction, true, true, filter) + `
		WHERE b.depth < ? AND ` + resolved + ` AND ` + scope + visible + `
		LIMIT ` + strconv.Itoa(states+1) + `),
		expanded AS MATERIALIZED (SELECT id FROM walk GROUP BY id HAVING MIN(depth) < ` + strconv.Itoa(request.Depth) + `),
		counts AS MATERIALIZED (SELECT a.id,
		(SELECT COUNT(*) FROM (SELECT 1 FROM edges e INDEXED BY ` + index +
		previousBFSAdmissionJoins(request.Direction, false, false, filter) + `
		WHERE e.view_gen = ? AND e.` + anchor + ` = a.id` + visible + ` LIMIT ` + strconv.Itoa(request.Limit) + `)) AS n
		FROM expanded a)
		SELECT (SELECT COUNT(*) FROM walk) > ` + strconv.Itoa(states) + `, e.id, counts.n, ` + lookupQualifiedEdgeCols + `, ` + bfsAdmissionNeighborColumns(neighbor) + `
		FROM counts CROSS JOIN edges e INDEXED BY ` + index + ` ON e.view_gen = ? AND e.` + anchor + ` = counts.id AND ` + kindPredicate +
		previousBFSAdmissionJoins(request.Direction, true, false, filter) + ` WHERE 1` + visible +
		` AND (` + neighbor + `.id IS NULL OR NOT (` + resolved + `) OR (` + scope + `)) LIMIT ` + strconv.Itoa(edges+1)
	args := []any{request.Seed, generation, string(kindJSON), request.Depth}
	args = append(args, scopeArgs...)
	args = append(args, maskArgs...)
	args = append(args, viaArgs...)
	args = append(args, generation)
	args = append(args, maskArgs...)
	args = append(args, viaArgs...)
	args = append(args, generation, string(kindJSON))
	args = append(args, maskArgs...)
	args = append(args, viaArgs...)
	args = append(args, scopeArgs...)
	return outlineQuery{sql: q, args: args}
}

func previousBFSAdmissionColumns(dir graph.Direction) (anchor, next, index, neighbor string) {
	if dir == graph.DirectionBackward {
		return "to_id", "from_id", "edges_by_to", "source"
	}
	return "from_id", "to_id", "edges_by_from", "target"
}

func previousBFSAdmissionJoins(dir graph.Direction, needNeighbor, inner bool, filter graph.OutlineEdgeFilter) string {
	_, _, _, neighbor := previousBFSAdmissionColumns(dir)
	var out string
	for _, end := range []struct{ alias, column string }{{"source", "from_id"}, {"target", "to_id"}} {
		if end.alias != neighbor || !needNeighbor {
			if len(filter.ExcludeNodeKinds) == 0 {
				continue
			}
		}
		join := " LEFT JOIN "
		if end.alias == neighbor && needNeighbor && inner {
			join = " JOIN "
		}
		out += join + `nodes ` + end.alias + ` ON ` + end.alias + `.id = e.` + end.column + ` AND ` + end.alias + `.view_gen = e.view_gen`
	}
	return out
}

// requireBFSAdmissionMatchesPrevious compares the snapshot with the previous
// statement's. Adjacency rows and their order are identical. Nodes differ only
// where admission never reads them: the seed, which the caller supplies, and
// unresolved or external targets. A Total differs only below the cap left
// after every walked identity, where both values decide the engine's pre-cap
// sort the same way.
//
// The previous statement checked a workspace or project scope on every
// walked row. Now a superset walks on: neighbors outside that scope in a
// narrowed repository or, without a narrow, in the seed's repository, and
// whatever they reach. Admission expands none of them and drops their edges,
// so under such a scope the comparison keeps the previous anchors and nodes
// and drops rows whose far end the scope rejects.
func requireBFSAdmissionMatchesPrevious(t *testing.T, s *Store, request graph.BFSAdmissionRequest, filter graph.OutlineEdgeFilter) (got, want graph.BFSAdmissionSnapshot) {
	t.Helper()
	label := fmt.Sprintf("generation=%d request=%+v filter=%+v", s.viewGen, request, filter)
	got, handled, err := s.BFSAdmissionFilteredContext(t.Context(), request, filter)
	require.NoError(t, err, label)
	want, wantHandled, err := s.readBFSAdmission(t.Context(), request, previousBFSAdmissionQuery(s.viewGen, request, filter))
	require.NoError(t, err, label)
	require.True(t, wantHandled, label)
	require.True(t, handled, label)
	wantNodes := map[string]*graph.Node{}
	for id, node := range want.Nodes {
		if id != request.Seed && !graph.IsUnresolvedTarget(id) && !strings.HasPrefix(id, "external::") {
			wantNodes[id] = node
		}
	}
	scoped := request.Scope.WorkspaceID != ""
	if !scoped {
		require.Equal(t, wantNodes, got.Nodes, label)
		require.Len(t, got.Adjacency, len(want.Adjacency), label)
	}
	seed := s.GetNode(request.Seed)
	for id, node := range got.Nodes {
		if wantNode, ok := wantNodes[id]; ok {
			require.Equal(t, wantNode, node, "node %s: %s", id, label)
			continue
		}
		if !request.Scope.Allows(node) {
			narrowed := node.RepoPrefix == "" || request.Scope.RepoAllow[node.RepoPrefix]
			if len(request.Scope.RepoAllow) == 0 {
				narrowed = seed != nil && node.RepoPrefix == seed.RepoPrefix
			}
			require.True(t, narrowed, "node %s walked outside the gate: %s", id, label)
		}
	}
	for id := range wantNodes {
		require.Contains(t, got.Nodes, id, label)
	}
	held := 1 + len(got.Nodes)
	for anchor, adjacency := range want.Adjacency {
		gotAdjacency, ok := got.Adjacency[anchor]
		require.True(t, ok, "anchor %s: %s", anchor, label)
		admitted := gotAdjacency.Edges
		if scoped {
			admitted = nil
			for _, edge := range gotAdjacency.Edges {
				far := edge.To
				if request.Direction == graph.DirectionBackward {
					far = edge.From
				}
				if node := got.Nodes[far]; node == nil || request.Scope.Allows(node) {
					admitted = append(admitted, edge)
				}
			}
		}
		require.Equal(t, adjacency.Edges, admitted, "anchor %s: %s", anchor, label)
		if room := request.Limit - held; adjacency.Total >= room {
			require.Equal(t, adjacency.Total, gotAdjacency.Total, "anchor %s: %s", anchor, label)
		} else {
			require.GreaterOrEqual(t, gotAdjacency.Total, adjacency.Total, "anchor %s: %s", anchor, label)
			require.Less(t, gotAdjacency.Total, room, "anchor %s: %s", anchor, label)
		}
		for n := range held + 1 {
			require.Equal(t, n+adjacency.Total >= request.Limit, n+gotAdjacency.Total >= request.Limit, "sort decision with %d held nodes, anchor %s: %s", n, anchor, label)
		}
	}
	return got, want
}

const (
	bfsCaseRoot     = "a/root.go::Root"
	bfsCaseChild    = "a/child.go::Child"
	bfsCaseLeaf     = "a/leaf.go::Leaf"
	bfsCasePing     = "a/ping.go::Ping"
	bfsCasePong     = "a/pong.go::Pong"
	bfsCaseGone     = "a/gone.go::Gone"
	bfsCaseAfter    = "a/after.go::After"
	bfsCaseContract = "a/api.go::Contract"
	bfsCaseBehind   = "a/behind.go::Behind"
	bfsCaseNowhere  = "a/nowhere.go::Nowhere"
	bfsCaseGhost    = "a::unresolved::Ghost"
	bfsCaseNever    = "a/ghost.go::Never"
	bfsCaseSpec     = "a::unresolved::Spec"
	bfsCaseElse     = "a/else.go::Else"
	bfsCaseDeep     = "a/deep.go::Deep"
	bfsCaseForeign  = "b/api.go::Handler"
	bfsCaseBack     = "a/back.go::Back"
	bfsCaseGlobal   = "util.go::Global"
	bfsCaseFmt      = "external::go:fmt"
	bfsCaseBean     = "a/bean.go::Bean"
)

// seedBFSAdmissionCases writes the edge-case neighborhood: a self-loop seed, a
// two-node cycle, a missing intermediate node, a masked node and seed, a seed
// without a node, existing nodes with unresolved-looking ids, a
// same-repository node in another workspace, foreign and unowned nodes, and
// edges hidden by the via, structural and edge-kind masks. Generation 1 also
// owns the missing intermediate node and masks the leaf's kind.
func seedBFSAdmissionCases(s *Store, derived bool) {
	node := func(id, path, repo, workspace string) *graph.Node {
		return &graph.Node{ID: id, Kind: graph.KindFunction, Name: id, FilePath: path, RepoPrefix: repo, WorkspaceID: workspace, Meta: map[string]any{"signature": "func " + id}}
	}
	nodes := []*graph.Node{
		node(bfsCaseRoot, "a/root.go", "a", "work"), node(bfsCaseChild, "a/child.go", "a", "work"),
		node(bfsCaseLeaf, "a/leaf.go", "a", "work"), node(bfsCasePing, "a/ping.go", "a", "work"),
		node(bfsCasePong, "a/pong.go", "a", "work"), node(bfsCaseAfter, "a/after.go", "a", "work"),
		{ID: bfsCaseContract, Kind: graph.KindContract, Name: "Contract", FilePath: "a/api.go", RepoPrefix: "a", WorkspaceID: "work"},
		node(bfsCaseBehind, "a/behind.go", "a", "work"), node(bfsCaseGhost, "a/ghost.go", "a", "work"),
		node(bfsCaseNever, "a/ghost.go", "a", "work"), node(bfsCaseElse, "a/else.go", "a", "elsewhere"),
		node(bfsCaseDeep, "a/deep.go", "a", "work"), node(bfsCaseForeign, "b/api.go", "b", "work"),
		node(bfsCaseBack, "a/back.go", "a", "work"), node(bfsCaseGlobal, "util.go", "", ""),
		node(bfsCaseFmt, "external::go:fmt", "", ""), node(bfsCaseBean, "a/bean.go", "a", "work"),
		node("a/caller.go::Caller", "a/caller.go", "a", "work"), node("b/test_test.go::Test", "b/test_test.go", "b", "work"),
		{ID: bfsCaseSpec, Kind: graph.KindContract, Name: "Spec", FilePath: "a/api.go", RepoPrefix: "a", WorkspaceID: "work"},
	}
	edges := []*graph.Edge{
		{From: bfsCaseRoot, To: bfsCaseChild, Kind: graph.EdgeCalls, FilePath: "a/root.go", Line: 30, Origin: "ast_inferred", Confidence: .7, Meta: map[string]any{"evidence": "retained"}},
		{From: bfsCaseRoot, To: bfsCaseLeaf, Kind: graph.EdgeCalls, FilePath: "a/root.go", Line: 20, Origin: "lsp_resolved", Confidence: .9},
		{From: bfsCaseRoot, To: bfsCaseRoot, Kind: graph.EdgeCalls, FilePath: "a/root.go", Line: 1},
		{From: bfsCaseRoot, To: bfsCaseGone, Kind: graph.EdgeCalls, FilePath: "a/root.go", Line: 2},
		{From: bfsCaseGone, To: bfsCaseAfter, Kind: graph.EdgeCalls},
		{From: bfsCaseRoot, To: bfsCaseContract, Kind: graph.EdgeCalls, FilePath: "a/root.go", Line: 3},
		{From: bfsCaseContract, To: bfsCaseBehind, Kind: graph.EdgeCalls},
		{From: bfsCaseRoot, To: bfsCaseGhost, Kind: graph.EdgeCalls, FilePath: "a/root.go", Line: 4},
		{From: bfsCaseGhost, To: bfsCaseNever, Kind: graph.EdgeCalls},
		{From: bfsCaseRoot, To: bfsCaseSpec, Kind: graph.EdgeCalls, FilePath: "a/root.go", Line: 15},
		{From: bfsCaseRoot, To: bfsCaseElse, Kind: graph.EdgeCalls, FilePath: "a/root.go", Line: 5},
		{From: bfsCaseElse, To: bfsCaseDeep, Kind: graph.EdgeCalls},
		{From: bfsCaseRoot, To: bfsCaseForeign, Kind: graph.EdgeCalls, FilePath: "a/root.go", Line: 6},
		{From: bfsCaseForeign, To: bfsCaseBack, Kind: graph.EdgeCalls},
		{From: bfsCaseRoot, To: bfsCaseGlobal, Kind: graph.EdgeCalls, FilePath: "a/root.go", Line: 7},
		{From: bfsCaseRoot, To: bfsCaseBean, Kind: graph.EdgeCalls, FilePath: "a/root.go", Line: 8, Meta: map[string]any{"via": "spring.Bean"}},
		{From: bfsCaseRoot, To: "external::dynamic", Kind: graph.EdgeCalls, FilePath: "a/root.go", Line: 9},
		{From: bfsCaseRoot, To: bfsCaseFmt, Kind: graph.EdgeCalls, FilePath: "a/root.go", Line: 10},
		{From: bfsCaseRoot, To: "a/root.go::Root#param:x", Kind: graph.EdgeImplements, FilePath: "a/root.go", Line: 11},
		{From: bfsCaseRoot, To: bfsCaseForeign, Kind: graph.EdgeMatches, FilePath: "a/root.go", Line: 12},
		{From: bfsCaseRoot, To: "unresolved::Callback", Kind: graph.EdgeReferences, FilePath: "a/root.go", Line: 13},
		{From: bfsCaseRoot, To: "a/root.go::value", Kind: graph.EdgeDefines, FilePath: "a/root.go", Line: 14},
		{From: bfsCaseChild, To: bfsCasePing, Kind: graph.EdgeCalls, Line: 1},
		{From: bfsCasePing, To: bfsCasePong, Kind: graph.EdgeCalls},
		{From: bfsCasePong, To: bfsCasePing, Kind: graph.EdgeCalls},
		{From: bfsCaseChild, To: bfsCaseRoot, Kind: graph.EdgeCalls, Line: 2},
		{From: bfsCaseChild, To: bfsCaseNowhere, Kind: graph.EdgeCalls, Line: 3},
		{From: bfsCaseNowhere, To: bfsCaseChild, Kind: graph.EdgeCalls},
		{From: bfsCaseNowhere, To: bfsCaseLeaf, Kind: graph.EdgeReferences},
		{From: bfsCaseLeaf, To: bfsCaseDeep, Kind: graph.EdgeReferences},
		{From: "a/caller.go::Caller", To: bfsCaseRoot, Kind: graph.EdgeReferences},
		{From: "b/test_test.go::Test", To: bfsCaseRoot, Kind: graph.EdgeCalls},
		{From: bfsCaseContract, To: bfsCaseRoot, Kind: graph.EdgeCalls},
		{From: bfsCaseElse, To: bfsCaseRoot, Kind: graph.EdgeCalls},
		{From: bfsCaseGhost, To: bfsCaseRoot, Kind: graph.EdgeCalls},
		{From: bfsCaseGone, To: bfsCaseRoot, Kind: graph.EdgeCalls},
		{From: bfsCaseGlobal, To: bfsCaseRoot, Kind: graph.EdgeMatches},
	}
	if derived {
		nodes = append(nodes, node(bfsCaseGone, "a/gone.go", "a", "work"))
		nodes[2].Kind = graph.KindContract
	}
	s.AddBatch(nodes, edges)
}

func TestBFSAdmissionSnapshotMatchesPreviousStatement(t *testing.T) {
	base := openCatalogStore(t)
	derived := base.AtGeneration(1)
	seedBFSAdmissionCases(base, false)
	seedBFSAdmissionCases(derived, true)
	filters := []graph.OutlineEdgeFilter{
		{},
		{ExcludeKinds: []graph.EdgeKind{graph.EdgeMatches}, ExcludeNodeKinds: []graph.NodeKind{graph.KindContract}, ExcludeVia: "spring.Bean"},
		{ExcludeNodeIDs: map[string]bool{bfsCaseChild: true}},
	}
	kindsByDirection := map[graph.Direction][]graph.EdgeKind{
		graph.DirectionForward:  {graph.EdgeCalls, graph.EdgeMatches, graph.EdgeImplements},
		graph.DirectionBackward: {graph.EdgeCalls, graph.EdgeMatches, graph.EdgeReferences},
	}
	compared := 0
	workspaceGate := map[string]bool{}
	for _, s := range []*Store{base, derived} {
		for _, seed := range []string{bfsCaseRoot, bfsCaseChild, bfsCaseContract, bfsCaseNowhere, bfsCaseElse, bfsCaseForeign} {
			seedNode := s.GetNode(seed)
			for _, scope := range []graph.NodeScope{
				{}, {RepoAllow: map[string]bool{"a": true}}, {RepoAllow: map[string]bool{"a": true, "b": true}},
				{WorkspaceID: "work"}, {WorkspaceID: "work", ProjectID: "a"}, {WorkspaceID: "elsewhere"},
				{WorkspaceID: "work", RepoAllow: map[string]bool{"a": true, "b": true}},
			} {
				if !scope.Allows(seedNode) {
					continue // the engine never walks from a seed outside its scope
				}
				for dir, kinds := range kindsByDirection {
					for _, filter := range filters {
						for _, limit := range []int{1, 2, 3, 5, 20, 50} {
							for _, depth := range []int{1, 2, 4} {
								request := graph.BFSAdmissionRequest{Seed: seed, Direction: dir, Kinds: kinds, Depth: depth, Limit: limit, Scope: scope}
								got, _ := requireBFSAdmissionMatchesPrevious(t, s, request, filter)
								compared++
								if scope.WorkspaceID == "" || len(scope.RepoAllow) > 0 || seedNode == nil {
									continue
								}
								for _, node := range got.Nodes {
									if !scope.Allows(node) {
										workspaceGate["past the scope"] = true
									}
									if node.RepoPrefix != seedNode.RepoPrefix {
										workspaceGate["across repositories"] = true
									}
								}
							}
						}
					}
				}
			}
		}
	}
	require.Greater(t, compared, 1000)
	require.Equal(t, map[string]bool{"past the scope": true, "across repositories": true}, workspaceGate,
		"a workspace gate without a repository narrow walks both shapes")
}

func TestBFSAdmissionEdgeCaseFixtures(t *testing.T) {
	base := openCatalogStore(t)
	derived := base.AtGeneration(1)
	seedBFSAdmissionCases(base, false)
	seedBFSAdmissionCases(derived, true)
	masked := graph.OutlineEdgeFilter{ExcludeNodeKinds: []graph.NodeKind{graph.KindContract}}
	read := func(s *Store, seed string, filter graph.OutlineEdgeFilter) graph.BFSAdmissionSnapshot {
		request := graph.BFSAdmissionRequest{Seed: seed, Direction: graph.DirectionForward, Kinds: []graph.EdgeKind{graph.EdgeCalls}, Depth: 4, Limit: 50,
			Scope: graph.NodeScope{RepoAllow: map[string]bool{"a": true}}}
		got, _ := requireBFSAdmissionMatchesPrevious(t, s, request, filter)
		return got
	}
	targets := func(snapshot graph.BFSAdmissionSnapshot, anchor string) []string {
		var out []string
		for _, edge := range snapshot.Adjacency[anchor].Edges {
			out = append(out, edge.To)
		}
		return out
	}

	got := read(base, bfsCaseRoot, masked)
	require.Contains(t, targets(got, bfsCaseRoot), bfsCaseRoot, "a self-loop keeps its edge")
	require.NotContains(t, got.Nodes, bfsCaseRoot, "the caller supplies the seed")
	require.Contains(t, targets(got, bfsCaseChild), bfsCaseRoot, "an edge back to the seed stays")
	require.Contains(t, targets(got, bfsCasePing), bfsCasePong)
	require.Contains(t, targets(got, bfsCasePong), bfsCasePing, "a two-node cycle closes")
	require.Contains(t, targets(got, bfsCaseRoot), bfsCaseGone, "a missing node keeps its edge")
	require.NotContains(t, got.Nodes, bfsCaseGone)
	require.NotContains(t, got.Nodes, bfsCaseAfter, "nothing is walked through a missing node")
	require.NotContains(t, targets(got, bfsCaseRoot), bfsCaseContract, "a masked node drops its edge")
	require.NotContains(t, got.Nodes, bfsCaseBehind, "nothing is walked through a masked node")
	require.Contains(t, targets(got, bfsCaseRoot), bfsCaseGhost, "an unresolved-looking boundary stays")
	require.NotContains(t, got.Nodes, bfsCaseGhost, "admission never reads an unresolved-looking node")
	require.NotContains(t, got.Nodes, bfsCaseNever)
	require.NotContains(t, targets(got, bfsCaseRoot), bfsCaseSpec, "a masked unresolved-looking node drops its edge")
	require.Contains(t, targets(got, bfsCaseRoot), bfsCaseFmt)
	require.NotContains(t, got.Nodes, bfsCaseFmt, "admission never reads an external node")
	require.Contains(t, got.Nodes, bfsCaseElse, "another workspace in the same repository is admitted in Go")
	require.Contains(t, got.Nodes, bfsCaseDeep)
	require.NotContains(t, targets(got, bfsCaseRoot), bfsCaseForeign, "a foreign repository drops its edge")
	require.NotContains(t, got.Nodes, bfsCaseBack)
	require.Contains(t, got.Nodes, bfsCaseGlobal, "unowned nodes stay in every repository narrow")
	require.Contains(t, got.Nodes, bfsCaseLeaf)

	got = read(base, bfsCaseContract, masked)
	require.Empty(t, got.Adjacency, "a masked seed has no visible adjacency")
	require.Empty(t, got.Nodes)

	got = read(base, bfsCaseNowhere, graph.OutlineEdgeFilter{})
	require.Contains(t, targets(got, bfsCaseNowhere), bfsCaseChild, "a seed without a node still walks")
	require.Contains(t, targets(got, bfsCaseChild), bfsCaseNowhere)
	require.Contains(t, got.Nodes, bfsCaseChild)

	got = read(derived, bfsCaseRoot, masked)
	require.Contains(t, got.Nodes, bfsCaseGone, "the handle's generation owns the intermediate node")
	require.Contains(t, got.Nodes, bfsCaseAfter)
	require.NotContains(t, got.Nodes, bfsCaseLeaf, "the handle's generation masks the leaf")
	require.Equal(t, derived.GetNode(bfsCaseGone), got.Nodes[bfsCaseGone])
}

// The seed's walked edges carry weaker evidence first, so a flipped sort
// decision would change which neighbors a capped page admits. Hidden rows
// (a masked node, a spring.Bean edge, a structural parameter target, an
// excluded edge kind) make the raw adjacency larger than the visible one.
func TestBFSAdmissionTotalBoundary(t *testing.T) {
	s := openCatalogStore(t)
	const seed, weak, strong, contract, bean = "a/seed.go::Seed", "a/weak.go::Weak", "a/strong.go::Strong", "a/api.go::Contract", "a/bean.go::Bean"
	node := func(id string) *graph.Node {
		return &graph.Node{ID: id, Kind: graph.KindFunction, FilePath: strings.Split(id, "::")[0], RepoPrefix: "a"}
	}
	viaBean := map[string]any{"via": "spring.Bean"}
	s.AddBatch([]*graph.Node{node(seed), node(weak), node(strong), node(bean), {ID: contract, Kind: graph.KindContract, FilePath: "a/api.go", RepoPrefix: "a"}}, []*graph.Edge{
		{From: seed, To: weak, Kind: graph.EdgeCalls, Line: 1, Origin: "ast_inferred", Confidence: .5},
		{From: seed, To: strong, Kind: graph.EdgeCalls, Line: 2, Origin: "lsp_resolved", Confidence: .9},
		{From: seed, To: contract, Kind: graph.EdgeCalls, Line: 3},
		{From: seed, To: bean, Kind: graph.EdgeCalls, Line: 4, Meta: viaBean},
		{From: seed, To: "a/seed.go::Seed#param:x", Kind: graph.EdgeImplements, Line: 5},
		{From: seed, To: weak, Kind: graph.EdgeMatches, Line: 6},
		{From: weak, To: seed, Kind: graph.EdgeCalls, Line: 1, Origin: "ast_inferred", Confidence: .5},
		{From: strong, To: seed, Kind: graph.EdgeCalls, Line: 2, Origin: "lsp_resolved", Confidence: .9},
		{From: contract, To: seed, Kind: graph.EdgeCalls, Line: 3},
		{From: bean, To: seed, Kind: graph.EdgeCalls, Line: 4, Meta: viaBean},
		{From: weak, To: seed, Kind: graph.EdgeMatches, Line: 6},
	})
	filter := graph.OutlineEdgeFilter{ExcludeKinds: []graph.EdgeKind{graph.EdgeMatches}, ExcludeNodeKinds: []graph.NodeKind{graph.KindContract}, ExcludeVia: "spring.Bean"}
	const walked, visible = 3, 2 // seed, weak, strong; the two calls between them
	for _, dir := range []graph.Direction{graph.DirectionForward, graph.DirectionBackward} {
		raw := len(s.GetOutEdges(seed))
		if dir == graph.DirectionBackward {
			raw = len(s.GetInEdges(seed))
		}
		branches := map[string]bool{}
		for limit := 1; limit <= raw+walked+3; limit++ {
			request := graph.BFSAdmissionRequest{Seed: seed, Direction: dir, Kinds: []graph.EdgeKind{graph.EdgeCalls}, Depth: 1, Limit: limit, Scope: graph.NodeScope{RepoAllow: map[string]bool{"a": true}}}
			got, want := requireBFSAdmissionMatchesPrevious(t, s, request, filter)
			require.Len(t, got.Nodes, walked-1)
			require.Equal(t, min(visible, limit), want.Adjacency[seed].Total)
			total := got.Adjacency[seed].Total
			switch room := limit - walked; {
			case raw < room:
				branches["raw"] = true
				require.Equal(t, raw, total, "dir=%v limit=%d: the index-only count stands below the threshold", dir, limit)
			case raw == room:
				branches["at threshold"] = true
				require.Equal(t, visible, total, "dir=%v limit=%d: the threshold itself is counted exactly", dir, limit)
			case visible < room:
				branches["exact below threshold"] = true
				require.Equal(t, visible, total, "dir=%v limit=%d", dir, limit)
			default:
				branches["exact"] = true
				require.Equal(t, min(visible, limit), total, "dir=%v limit=%d", dir, limit)
			}
		}
		require.Len(t, branches, 4, "dir=%v covers every count branch: %v", dir, branches)
	}
}

// A cold bulk load drops the repository and kind indexes. The hinted statement
// then fails, and the unhinted retry must return the same snapshot.
func TestBFSAdmissionDroppedIndexFallback(t *testing.T) {
	s := openCatalogStore(t)
	seedBFSAdmissionCases(s, false)
	filter := graph.OutlineEdgeFilter{ExcludeKinds: []graph.EdgeKind{graph.EdgeMatches}, ExcludeNodeKinds: []graph.NodeKind{graph.KindContract}, ExcludeVia: "spring.Bean"}
	var requests []graph.BFSAdmissionRequest
	for _, dir := range []graph.Direction{graph.DirectionForward, graph.DirectionBackward} {
		for _, scope := range []graph.NodeScope{{}, {RepoAllow: map[string]bool{"a": true}}, {WorkspaceID: "work"}} {
			requests = append(requests, graph.BFSAdmissionRequest{Seed: bfsCaseRoot, Direction: dir, Kinds: []graph.EdgeKind{graph.EdgeCalls, graph.EdgeReferences}, Depth: 3, Limit: 5, Scope: scope})
		}
	}
	indexed := make([]graph.BFSAdmissionSnapshot, len(requests))
	for i, request := range requests {
		indexed[i], _ = requireBFSAdmissionMatchesPrevious(t, s, request, filter)
		require.NotEmpty(t, indexed[i].Adjacency)
	}
	_, err := s.writerDB.Exec(`DROP INDEX nodes_by_repo; DROP INDEX nodes_by_kind`)
	require.NoError(t, err)
	for i, request := range requests {
		query := bfsAdmissionQuery(s.viewGen, request, filter)
		require.ErrorContains(t, s.db.QueryRowContext(t.Context(), query.sql, query.args...).Err(), "no such index")
		got, handled, err := s.BFSAdmissionFilteredContext(t.Context(), request, filter)
		require.NoError(t, err)
		require.True(t, handled)
		require.Equal(t, indexed[i], got, "request=%+v", request)
	}
}
