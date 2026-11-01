package mcp

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/query"
)

type bfsAdmissionSpy struct {
	graph.Reader
	backend                                          graph.FilteredBFSAdmissionReader
	calls, handled, returnedRows, allNodes, allEdges int
	walked                                           map[string]bool
}

func (r *bfsAdmissionSpy) BFSAdmissionFilteredContext(ctx context.Context, request graph.BFSAdmissionRequest, filter graph.OutlineEdgeFilter) (graph.BFSAdmissionSnapshot, bool, error) {
	r.calls++
	rows, handled, err := r.backend.BFSAdmissionFilteredContext(ctx, request, filter)
	if handled {
		r.handled++
	}
	for _, adjacency := range rows.Adjacency {
		r.returnedRows += len(adjacency.Edges)
	}
	for id := range rows.Nodes {
		if r.walked == nil {
			r.walked = map[string]bool{}
		}
		r.walked[id] = true
	}
	return rows, handled, err
}
func (r *bfsAdmissionSpy) AllNodes() []*graph.Node { r.allNodes++; return r.Reader.AllNodes() }
func (r *bfsAdmissionSpy) AllEdges() []*graph.Edge { r.allEdges++; return r.Reader.AllEdges() }

const admissionRoot = "a/start.go::Root"

func seedAdmissionFixture(store graph.Store) {
	node := func(id, path, repo string) *graph.Node {
		return &graph.Node{ID: id, Kind: graph.KindFunction, FilePath: path, RepoPrefix: repo, WorkspaceID: "work", ProjectID: "project", Meta: map[string]any{"signature": "func Work()", "payload": "retained"}}
	}
	nodes := []*graph.Node{
		node(admissionRoot, "a/start.go", "a"), node("a/child.go::Child", "a/child.go", "a"), node("a/leaf.go::Leaf", "a/leaf.go", "a"),
		node("b/api.go::Handler", "b/api.go", "b"), node("b/test_test.go::Test", "b/test_test.go", "b"), node("a/marked.go::Caller", "a/marked.go", "a"),
		node("a/impl.go::Impl", "a/impl.go", "a"), node("a::unresolved::Ghost", "a/ghost.go", "a"), node("a/ghost.go::Never", "a/ghost.go", "a"),
		node("other/out.go::Out", "other/out.go", "other"), node("a/foreign.go::Never", "a/foreign.go", "a"),
		{ID: "contract", Kind: graph.KindContract, FilePath: "api.yaml"},
		{ID: "a/marked.go", Kind: graph.KindFile, FilePath: "a/marked.go", RepoPrefix: "a", WorkspaceID: "work", ProjectID: "project", Meta: map[string]any{"is_test": true}},
	}
	nodes[9].WorkspaceID = "foreign"
	edges := []*graph.Edge{
		{From: admissionRoot, To: "a/child.go::Child", Kind: graph.EdgeCalls, FilePath: "a/start.go", Line: 50, Origin: "ast_inferred", Confidence: .7, Meta: map[string]any{"evidence": "retained"}},
		{From: admissionRoot, To: "a/leaf.go::Leaf", Kind: graph.EdgeCalls, FilePath: "a/start.go", Line: 90, Origin: "lsp_resolved", Confidence: .9},
		{From: admissionRoot, To: "external::dynamic", Kind: graph.EdgeCalls, FilePath: "a/start.go", Line: 1, Origin: "ast_resolved"},
		{From: admissionRoot, To: "a::unresolved::Ghost", Kind: graph.EdgeCalls, FilePath: "a/start.go", Line: 2, Origin: "ast_resolved"},
		{From: admissionRoot, To: "missing-intermediate", Kind: graph.EdgeCalls, FilePath: "a/start.go", Line: 3, Origin: "ast_resolved"},
		{From: "a/child.go::Child", To: admissionRoot, Kind: graph.EdgeCalls, FilePath: "a/child.go", Line: 1},
		{From: "a/child.go::Child", To: "a/leaf.go::Leaf", Kind: graph.EdgeCalls, FilePath: "a/child.go", Line: 2},
		{From: admissionRoot, To: "b/api.go::Handler", Kind: graph.EdgeMatches, FilePath: "a/start.go", Line: 4},
		{From: "b/test_test.go::Test", To: admissionRoot, Kind: graph.EdgeCalls},
		{From: "a/marked.go::Caller", To: admissionRoot, Kind: graph.EdgeReferences},
		{From: "a/impl.go::Impl", To: admissionRoot, Kind: graph.EdgeOverrides},
		{From: admissionRoot, To: "contract", Kind: graph.EdgeCalls},
		{From: "contract", To: admissionRoot, Kind: graph.EdgeCalls},
		{From: admissionRoot, To: "a/impl.go::Impl", Kind: graph.EdgeCalls, Meta: map[string]any{"via": "spring.Bean"}},
		{From: "a::unresolved::Ghost", To: "a/ghost.go::Never", Kind: graph.EdgeCalls},
		{From: admissionRoot, To: "other/out.go::Out", Kind: graph.EdgeCalls},
		{From: "other/out.go::Out", To: "a/foreign.go::Never", Kind: graph.EdgeCalls},
	}
	for i := range 40 {
		id := fmt.Sprintf("a/start.go::value%02d", i)
		nodes = append(nodes, &graph.Node{ID: id, Kind: graph.KindVariable, RepoPrefix: "a", FilePath: "a/start.go"})
		edges = append(edges, &graph.Edge{From: admissionRoot, To: id, Kind: graph.EdgeDefines})
	}
	store.AddBatch(nodes, edges)
}

func TestScopedBFSAdmissionMatchesLegacyGoOutputAndUnrelatedRows(t *testing.T) {
	store := openContractCoreCapStore(t)
	seedAdmissionFixture(store)
	for _, masked := range []bool{false, true} {
		t.Run(fmt.Sprintf("masked=%v", masked), func(t *testing.T) {
			baselineRows := -1
			for pass := range 2 {
				spy := &bfsAdmissionSpy{Reader: store, backend: store}
				var fast graph.Reader = spy
				var selected graph.Reader = store
				if masked {
					fast = newContractCoreEdges(spy, t.Context(), nil)
					selected = newContractCoreEdges(store, t.Context(), nil)
					_, ok := fast.(graph.BFSCapable)
					require.True(t, ok, "wrapper retains a visibility-safe canonical BFS")
					_, raw := fast.(graph.FilteredBFSAdmissionReader)
					require.False(t, raw)
				}
				legacy := legacyOutlineReader{selected}
				for _, repos := range []map[string]bool{{"a": true}, {"a": true, "b": true}} {
					for _, limit := range []int{3, 20} {
						for _, forward := range []bool{false, true} {
							opts := query.QueryOptions{WorkspaceID: "work", ProjectID: "project", RepoAllow: repos, Depth: 3, Limit: limit, Context: t.Context()}
							eng := query.NewEngine(store)
							var got, want *query.SubGraph
							if forward {
								got, want = eng.WithReader(fast).GetCallChain(admissionRoot, opts), eng.WithReader(legacy).GetCallChain(admissionRoot, opts)
							} else {
								got, want = eng.WithReader(fast).GetCallers(admissionRoot, opts), eng.WithReader(legacy).GetCallers(admissionRoot, opts)
							}
							require.Equal(t, want, got, "limit=%d forward=%v repos=%v", limit, forward, repos)
						}
					}
				}
				// The root's call fan-out always reads its neighborhood in one
				// statement. Its raw caller fan-in sits at the threshold; the
				// wrapper hides the contract caller, so masked caller walks go
				// hop by hop.
				wantStatements := 8
				if masked {
					wantStatements = 4
				}
				require.Equal(t, wantStatements, spy.calls)
				require.Equal(t, spy.calls, spy.handled, "wide scoped walks use the one-statement projection")
				require.Zero(t, spy.allNodes)
				require.Zero(t, spy.allEdges)
				if pass == 0 {
					baselineRows = spy.returnedRows
					for i := range 5 {
						prefix := fmt.Sprintf("unrelated-%v-%d", masked, i)
						store.AddBatch([]*graph.Node{{ID: prefix + "/a.go::From", Kind: graph.KindFunction, RepoPrefix: prefix}, {ID: prefix + "/b.go::To", Kind: graph.KindFunction, RepoPrefix: prefix}}, []*graph.Edge{{From: prefix + "/a.go::From", To: prefix + "/b.go::To", Kind: graph.EdgeCalls}})
					}
				} else {
					require.Equal(t, baselineRows, spy.returnedRows)
				}
			}
		})
	}
}

func TestScopedBFSAdmissionRetainsHookAndOverlayFallbacks(t *testing.T) {
	store := openContractCoreCapStore(t)
	seedAdmissionFixture(store)
	spy := &bfsAdmissionSpy{Reader: store, backend: store}
	reader := newContractCoreEdges(spy, t.Context(), nil)
	legacy := legacyOutlineReader{newContractCoreEdges(store, t.Context(), nil)}
	eng := query.NewEngine(store)
	for _, opts := range []query.QueryOptions{
		{RepoAllow: map[string]bool{"a": true}, Depth: 3, Limit: 20, ExcludeTests: true},
		{RepoAllow: map[string]bool{"a": true}, Depth: 3, Limit: 20, IncludeDispatch: true},
	} {
		require.Equal(t, eng.WithReader(legacy).GetCallers(admissionRoot, opts), eng.WithReader(reader).GetCallers(admissionRoot, opts))
		require.Equal(t, eng.WithReader(legacy).GetCallChain(admissionRoot, opts), eng.WithReader(reader).GetCallChain(admissionRoot, opts))
	}
	require.Zero(t, spy.calls, "test exclusion and dispatch keep their original selected-reader hooks")
	outOfScope := eng.WithReader(reader).GetCallChain(admissionRoot, query.QueryOptions{RepoAllow: map[string]bool{"elsewhere": true}, Depth: 3, Limit: 20})
	require.Empty(t, outOfScope.Nodes)
	require.Zero(t, spy.calls, "seed ownership is checked before any recursive statement")
	unscoped := query.QueryOptions{Depth: 3, Limit: 20}
	require.Equal(t, eng.WithReader(legacy).GetCallChain(admissionRoot, unscoped), eng.WithReader(reader).GetCallChain(admissionRoot, unscoped), "advertising BFSCapable cannot change the wrapper's existing Go admission shape")
	require.Equal(t, 1, spy.handled)
	layer := graph.NewOverlayLayer()
	layer.MarkFile("a/child.go", true)
	overlaid := graph.NewOverlaidView(store, layer)
	selected := newContractCoreEdges(overlaid, t.Context(), nil)
	opts := query.QueryOptions{RepoAllow: map[string]bool{"a": true}, Depth: 3, Limit: 20}
	require.Equal(t, eng.WithReader(legacyOutlineReader{selected}).GetCallChain(admissionRoot, opts), eng.WithReader(selected).GetCallChain(admissionRoot, opts))
	hops, err := selected.(graph.BFSCapable).BFS([]string{admissionRoot}, graph.DirectionForward, []graph.EdgeKind{graph.EdgeCalls}, 3, 20)
	require.NoError(t, err)
	for _, hop := range hops {
		require.NotEqual(t, "contract", hop.NodeID)
		require.NotEqual(t, "a/child.go::Child", hop.NodeID)
		require.NotEqual(t, "a/impl.go::Impl", hop.NodeID, "spring.Bean call remains hidden")
	}
}

func TestScopedBFSAdmissionBudgetFallbackMatchesLegacyGo(t *testing.T) {
	store := openContractCoreCapStore(t)
	const root, child = "a/root.go::Root", "a/child.go::Child"
	store.AddBatch([]*graph.Node{{ID: root, Kind: graph.KindFunction, RepoPrefix: "a", FilePath: "a/root.go"}, {ID: child, Kind: graph.KindFunction, RepoPrefix: "a", FilePath: "a/child.go"}}, nil)
	request := graph.BFSAdmissionRequest{Seed: root, Direction: graph.DirectionForward, Kinds: []graph.EdgeKind{graph.EdgeCalls}, Depth: 3, Limit: 3}
	_, budget, _ := graph.BFSAdmissionBudgets(request)
	var edges []*graph.Edge
	for line := 1; line <= budget+1; line++ {
		edges = append(edges, &graph.Edge{From: root, To: child, Kind: graph.EdgeCalls, FilePath: "a/root.go", Line: line})
	}
	store.AddBatch(nil, edges)
	spy := &bfsAdmissionSpy{Reader: store, backend: store}
	fast := newContractCoreEdges(spy, t.Context(), nil)
	legacy := legacyOutlineReader{newContractCoreEdges(store, t.Context(), nil)}
	opts := query.QueryOptions{Depth: 3, Limit: 3, RepoAllow: map[string]bool{"a": true}}
	eng := query.NewEngine(store)
	got := eng.WithReader(fast).GetCallChain(root, opts)
	require.Equal(t, eng.WithReader(legacy).GetCallChain(root, opts), got)
	require.Len(t, got.Edges, budget+1, "a declined snapshot cannot lose real edge rows")
	require.Equal(t, 1, spy.calls)
	require.Zero(t, spy.handled)
}

func TestScopedBFSAdmissionKeepsExplicitOwnershipSets(t *testing.T) {
	store := openContractCoreCapStore(t)
	seedAdmissionFixture(store)
	for _, owned := range []map[string]bool{nil, {"contract": true, "a/child.go::Child": true}, {}} {
		spy := &bfsAdmissionSpy{Reader: store, backend: store}
		fast := newContractCoreEdges(spy, t.Context(), owned)
		legacy := legacyOutlineReader{newContractCoreEdges(store, t.Context(), owned)}
		eng := query.NewEngine(store)
		opts := query.QueryOptions{Depth: 3, Limit: 20}
		require.Equal(t, eng.WithReader(legacy).GetCallChain(admissionRoot, opts), eng.WithReader(fast).GetCallChain(admissionRoot, opts), "ownership=%v", owned)
		require.Equal(t, 1, spy.handled, "the wide call chain uses the one-statement projection")
		require.Equal(t, eng.WithReader(legacy).GetCallers(admissionRoot, opts), eng.WithReader(fast).GetCallers(admissionRoot, opts), "ownership=%v", owned)
	}
}

// The statement walks a superset of the workspace and project scope, which Go
// admission narrows, and may report an index-only count below the engine's
// sort threshold. Neither may change a capped page. The wide seeds here
// reorder their edges when they sort before a small cap, and same-repository
// neighbors in another workspace or project reach in-scope nodes by a shorter
// path than admission accepts. Without a repository narrow, a bound session's
// default, the walk crosses repositories inside the workspace but not into a
// foreign workspace whose neighborhood is wider than the state budget.
func TestScopedBFSAdmissionGoldenThresholdAndWorkspaceScope(t *testing.T) {
	const elsewhere, otherProject, deep = "a/else.go::Else", "a/project.go::Other", "a/deep.go::Deep"
	const foreign, sibling = "other/out.go::Out", "b/test_test.go::Test"
	store := openContractCoreCapStore(t)
	seedAdmissionFixture(store)
	node := func(id, workspace, project string) *graph.Node {
		return &graph.Node{ID: id, Kind: graph.KindFunction, FilePath: strings.Split(id, "::")[0], RepoPrefix: "a", WorkspaceID: workspace, ProjectID: project}
	}
	nodes := []*graph.Node{node(elsewhere, "foreign", "project"), node(otherProject, "work", "other"), node(deep, "work", "project"),
		node("a/mid.go::Mid", "work", "project"), node("a/after.go::After", "work", "project")}
	edges := []*graph.Edge{
		{From: admissionRoot, To: elsewhere, Kind: graph.EdgeCalls, FilePath: "a/start.go", Line: 5, Origin: "lsp_resolved", Confidence: .95},
		{From: elsewhere, To: deep, Kind: graph.EdgeCalls},
		{From: elsewhere, To: admissionRoot, Kind: graph.EdgeCalls, Origin: "lsp_resolved", Confidence: .95},
		{From: admissionRoot, To: otherProject, Kind: graph.EdgeCalls, FilePath: "a/start.go", Line: 6, Origin: "lsp_resolved", Confidence: .95},
		{From: otherProject, To: "a/after.go::After", Kind: graph.EdgeCalls},
		{From: otherProject, To: admissionRoot, Kind: graph.EdgeReferences, Origin: "lsp_resolved", Confidence: .95},
		{From: "a/child.go::Child", To: "a/mid.go::Mid", Kind: graph.EdgeCalls, FilePath: "a/child.go", Line: 3},
		{From: "a/mid.go::Mid", To: deep, Kind: graph.EdgeCalls},
		{From: deep, To: elsewhere, Kind: graph.EdgeCalls},
	}
	// Line order is weakest evidence first, so the pre-cap sort reorders it.
	for i := range 12 {
		fan := fmt.Sprintf("a/fan.go::Fan%02d", i)
		caller := fmt.Sprintf("a/callers.go::Caller%02d", i)
		nodes = append(nodes, node(fan, "work", "project"), node(caller, "work", "project"))
		confidence := .3 + float64(i)*.05
		edges = append(edges,
			&graph.Edge{From: admissionRoot, To: fan, Kind: graph.EdgeCalls, FilePath: "a/start.go", Line: 60 + i, Origin: "ast_inferred", Confidence: confidence},
			&graph.Edge{From: caller, To: admissionRoot, Kind: graph.EdgeCalls, FilePath: "a/callers.go", Line: 60 + i, Origin: "ast_inferred", Confidence: confidence})
		if i%3 == 0 {
			edges = append(edges, &graph.Edge{From: fan, To: elsewhere, Kind: graph.EdgeCalls}, &graph.Edge{From: elsewhere, To: caller, Kind: graph.EdgeCalls})
		}
	}
	edges = append(edges, &graph.Edge{From: foreign, To: admissionRoot, Kind: graph.EdgeCalls})
	states, _, _ := graph.BFSAdmissionBudgets(graph.BFSAdmissionRequest{Seed: admissionRoot, Kinds: []graph.EdgeKind{graph.EdgeCalls}, Depth: 4, Limit: 50})
	for i := range states + 1 {
		callee, caller := fmt.Sprintf("other/fan.go::Callee%03d", i), fmt.Sprintf("other/fan.go::Caller%03d", i)
		for _, id := range []string{callee, caller} {
			nodes = append(nodes, &graph.Node{ID: id, Kind: graph.KindFunction, FilePath: "other/fan.go", RepoPrefix: "other", WorkspaceID: "foreign", ProjectID: "project"})
		}
		edges = append(edges, &graph.Edge{From: foreign, To: callee, Kind: graph.EdgeCalls}, &graph.Edge{From: caller, To: foreign, Kind: graph.EdgeCalls})
	}
	store.AddBatch(nodes, edges)

	for _, masked := range []bool{false, true} {
		// The last scope, a workspace alone, is a bound session's default.
		for _, scope := range []graph.NodeScope{
			{WorkspaceID: "work", ProjectID: "project", RepoAllow: map[string]bool{"a": true}},
			{WorkspaceID: "work", ProjectID: "project", RepoAllow: map[string]bool{"a": true, "b": true}},
			{WorkspaceID: "work", ProjectID: "project"},
			{WorkspaceID: "work"},
		} {
			spy := &bfsAdmissionSpy{Reader: store, backend: store}
			var fast graph.Reader = spy
			var selected graph.Reader = store
			if masked {
				fast = newContractCoreEdges(spy, t.Context(), nil)
				selected = newContractCoreEdges(store, t.Context(), nil)
			}
			legacy := legacyOutlineReader{selected}
			eng := query.NewEngine(store)
			arm := fmt.Sprintf("masked=%v scope=%+v", masked, scope)
			rejected := []string{elsewhere, foreign}
			if scope.ProjectID != "" {
				rejected = append(rejected, otherProject)
			}
			for _, limit := range []int{1, 2, 3, 5, 20, 50} {
				for depth := 1; depth <= 4; depth++ {
					opts := query.QueryOptions{WorkspaceID: scope.WorkspaceID, ProjectID: scope.ProjectID, RepoAllow: scope.RepoAllow, Depth: depth, Limit: limit, Context: t.Context()}
					label := fmt.Sprintf("%s limit=%d depth=%d", arm, limit, depth)
					for _, forward := range []bool{true, false} {
						var got, want *query.SubGraph
						if forward {
							got, want = eng.WithReader(fast).GetCallChain(admissionRoot, opts), eng.WithReader(legacy).GetCallChain(admissionRoot, opts)
						} else {
							got, want = eng.WithReader(fast).GetCallers(admissionRoot, opts), eng.WithReader(legacy).GetCallers(admissionRoot, opts)
						}
						require.Equal(t, want, got, "%s forward=%v", label, forward)
						for _, n := range got.Nodes {
							require.NotContains(t, rejected, n.ID, label)
						}
					}
				}
			}
			require.Positive(t, spy.handled, "%s: wide scoped walks read one statement", arm)
			require.Equal(t, spy.calls, spy.handled, "%s: a foreign workspace wider than the state budget declines no snapshot", arm)
			require.True(t, spy.walked[elsewhere], "%s: the statement walks another workspace; Go admission rejects it", arm)
			require.True(t, spy.walked[otherProject], "%s: and another project", arm)
			require.False(t, spy.walked[foreign], "%s: the walk stops at the foreign workspace", arm)
			require.Equal(t, scope.RepoAllow == nil || scope.RepoAllow["b"], spy.walked[sibling], "%s: the walk follows the workspace into another repository", arm)
			require.Zero(t, spy.allNodes)
			require.Zero(t, spy.allEdges)
		}
	}
}
