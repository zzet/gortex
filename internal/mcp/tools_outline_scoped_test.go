package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/analysis"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

// Embedding Reader hides optional capabilities to exercise the original
// outline implementation as an independent oracle for the golden results.
type legacyOutlineReader struct{ graph.Reader }

type outlineCountingReader struct {
	*store_sqlite.Store
	allNodes, allNodesLight, allEdges, aggregates int
	lookedUp                                      []string
	lookupBatches                                 [][]string
	communityQueries                              int
	source                                        *outlineCountingReader
}

func (r *outlineCountingReader) AllNodes() []*graph.Node {
	r.counters().allNodes++
	return r.Store.AllNodes()
}
func (r *outlineCountingReader) AllNodesLight() []*graph.Node {
	r.counters().allNodesLight++
	return r.Store.AllNodesLight()
}
func (r *outlineCountingReader) AllEdges() []*graph.Edge {
	r.counters().allEdges++
	return r.Store.AllEdges()
}
func (r *outlineCountingReader) GetNodesByIDs(ids []string) map[string]*graph.Node {
	r.counters().lookedUp = append(r.counters().lookedUp, ids...)
	r.counters().lookupBatches = append(r.counters().lookupBatches, append([]string(nil), ids...))
	return r.Store.GetNodesByIDs(ids)
}
func (r *outlineCountingReader) OutlineNodeIDsInScope(scope graph.NodeScope, ids []string) ([]string, error) {
	r.counters().lookedUp = append(r.counters().lookedUp, ids...)
	r.counters().lookupBatches = append(r.counters().lookupBatches, append([]string(nil), ids...))
	return r.Store.OutlineNodeIDsInScope(scope, ids)
}

func (r *outlineCountingReader) ScopedOutlineCounts(scope graph.NodeScope) (graph.OutlineCounts, error) {
	r.counters().aggregates++
	return r.Store.ScopedOutlineCounts(scope)
}

func (r *outlineCountingReader) ScopedOutlineCountsFiltered(scope graph.NodeScope, filter graph.OutlineEdgeFilter) (graph.OutlineCounts, error) {
	r.counters().aggregates++
	return r.Store.ScopedOutlineCountsFiltered(scope, filter)
}

func (r *outlineCountingReader) ScopedAnalysisCommunitySummaries(id int64, scope graph.NodeScope, limit int, cursor string) ([]graph.AnalysisCommunitySummary, string, error) {
	r.counters().communityQueries++
	return r.Store.ScopedAnalysisCommunitySummaries(id, scope, limit, cursor)
}

func (r *outlineCountingReader) counters() *outlineCountingReader {
	if r.source != nil {
		return r.source
	}
	return r
}

func (r *outlineCountingReader) BindReadContext(ctx context.Context) graph.Reader {
	return &outlineCountingReader{Store: r.WithReadContext(ctx), source: r.counters()}
}

func outlineFixture(t *testing.T) (*Server, *store_sqlite.Store) {
	t.Helper()
	store, err := store_sqlite.Open(filepath.Join(t.TempDir(), "outline.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	nodes := []*graph.Node{
		{ID: "a-main", Name: "main", Kind: graph.KindFunction, RepoPrefix: "a", WorkspaceID: "ws", ProjectID: "p", Language: "go", FilePath: "a/main.go"},
		{ID: "a-lib", Name: "Library", Kind: graph.KindFunction, RepoPrefix: "a", WorkspaceID: "ws", ProjectID: "p", Language: "go", FilePath: "a/lib.go"},
		{ID: "b-main", Name: "main", Kind: graph.KindFunction, RepoPrefix: "b", WorkspaceID: "ws", ProjectID: "q", Language: "rust", FilePath: "b/main.rs"},
		{ID: "outside-main", Name: "main", Kind: graph.KindFunction, RepoPrefix: "c", WorkspaceID: "other", ProjectID: "p", Language: "python", FilePath: "c/main.py"},
		{ID: "legacy-main", Name: "main", Kind: graph.KindFunction, RepoPrefix: "legacy", Language: "go", FilePath: "legacy/main.go"},
		{ID: "unowned-main", Name: "main", Kind: graph.KindFunction, WorkspaceID: "ws", ProjectID: "p", Language: "go", FilePath: "source.go"},
		{ID: "global-main", Name: "main", Kind: graph.KindFunction, Language: "go", FilePath: "external::go:fmt"},
		{ID: "global-call", Name: "Call", Kind: graph.KindFunction, ProjectID: "ignored", FilePath: "external-call::symbol"},
	}
	edges := []*graph.Edge{
		{From: "a-main", To: "a-lib", Kind: graph.EdgeImports},
		{From: "b-main", To: "a-lib", Kind: graph.EdgeImports},
		{From: "outside-main", To: "a-lib", Kind: graph.EdgeImports},
		{From: "legacy-main", To: "a-lib", Kind: graph.EdgeImports},
		{From: "a-main", To: "global-main", Kind: graph.EdgeCalls},
		{From: "b-main", To: "global-main", Kind: graph.EdgeImports},
		{From: "outside-main", To: "global-main", Kind: graph.EdgeImports},
		{From: "a-main", To: "unowned-main", Kind: graph.EdgeImports},
		{From: "a-main", To: "b-main", Kind: graph.EdgeCalls},
		{From: "a-main", To: "global-call", Kind: graph.EdgeCalls},
	}
	store.AddBatch(nodes, edges)
	s := &Server{graph: store, session: newSessionState(), sessions: newSessionMap(), logger: zap.NewNop()}
	installCommunitiesForTest(s, &analysis.CommunityResult{Modularity: .75, Communities: []analysis.Community{
		{ID: "a", Label: "A", Size: 3, Cohesion: .5, Members: []string{"missing", "a-lib"}},
		{ID: "b", Label: "B", Size: 2, Cohesion: .6, Members: []string{"b-main"}},
		{ID: "global", Label: "Global", Size: 1, Cohesion: .7, Members: []string{"global-main"}},
		{ID: "stale", Label: "Stale", Size: 0, Members: []string{"missing"}},
	}})
	for _, id := range []string{"outside-main", "a-lib", "b-main", "global-main", "unowned-main", "legacy-main"} {
		n := store.GetNode(id)
		s.hotspots = append(s.hotspots, analysis.HotspotEntry{ID: id, Name: n.Name, Kind: string(n.Kind), FilePath: n.FilePath, FanIn: 2, FanOut: 1, ComplexityScore: 3})
	}
	s.hotspotsReady = true
	return s, store
}

func outlineBoundContext(s *Server, id, workspace string, repos map[string]bool) context.Context {
	ctx := WithSessionID(context.Background(), id)
	ss := s.sessionFor(ctx)
	ss.scopeResolved = true
	ss.scopeBound = true
	ss.scopeWorkspaceID = workspace
	ss.scopeRepoAllow = repos
	return ctx
}

func outlineJSON(t *testing.T, s *Server, ctx context.Context, reader graph.Reader) []byte {
	t.Helper()
	if reader != nil {
		ctx = withRequestView(ctx, &requestView{reader: reader})
	}
	result, err := s.handleGetRepoOutline(ctx, makeReq("get_repo_outline", map[string]any{"format": "json"}))
	require.NoError(t, err)
	require.False(t, result.IsError)
	return []byte(toolResultText(result))
}

func TestOutlineGolden(t *testing.T) {
	s, store := outlineFixture(t)
	for _, tc := range []struct {
		name, workspace string
		repos, narrow   map[string]bool
		unbound         bool
	}{
		{name: "repo", repos: map[string]bool{"a": true}},
		{name: "workspace", workspace: "ws", repos: map[string]bool{"a": true, "b": true}},
		{name: "project", workspace: "ws", repos: map[string]bool{"a": true, "b": true}, narrow: map[string]bool{"a": true}},
		{name: "unbound", unbound: true},
		{name: "global_unowned", workspace: "unknown", repos: map[string]bool{"a": true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			if !tc.unbound {
				ctx = outlineBoundContext(s, tc.name, tc.workspace, tc.repos)
			}
			if tc.narrow != nil {
				ctx = withRepoAllow(ctx, tc.narrow)
			}
			legacy := outlineJSON(t, s, ctx, legacyOutlineReader{Reader: store})
			got := outlineJSON(t, s, ctx, store)
			golden, err := os.ReadFile(filepath.Join("testdata", "outline", tc.name+".json"))
			require.NoError(t, err)
			require.JSONEq(t, string(golden), string(legacy), "legacy output changed")
			require.JSONEq(t, string(golden), string(got), "aggregate output changed")
		})
	}
}

func TestOutlineBoundSessionNeverScansAllEdges(t *testing.T) {
	s, store := outlineFixture(t)
	reader := &outlineCountingReader{Store: store}
	ctx := outlineBoundContext(s, "counting", "ws", map[string]bool{"a": true})
	outlineJSON(t, s, ctx, reader)
	require.Zero(t, reader.allEdges)
	require.Zero(t, reader.allNodes)
	require.Zero(t, reader.allNodesLight)
	require.Equal(t, 1, reader.aggregates)
	require.NotEmpty(t, reader.lookedUp, "analyzer membership must use candidate lookups")
	for _, id := range reader.lookedUp {
		require.Contains(t, []string{"missing", "a-lib", "b-main", "global-main", "outside-main", "unowned-main", "legacy-main", "a-main"}, id)
	}
}

func TestOutlineUnboundKeepsExistingSummaryPath(t *testing.T) {
	s, store := outlineFixture(t)
	reader := &outlineCountingReader{Store: store}
	outlineJSON(t, s, context.Background(), reader)
	require.Zero(t, reader.aggregates)
	require.Zero(t, reader.allEdges)
	require.Zero(t, reader.allNodes)
	require.Zero(t, reader.allNodesLight)
}

func TestOutlineCommunityStopsAtFirstVisibleMember(t *testing.T) {
	s, store := outlineFixture(t)
	members := []string{"a-main"}
	for i := 0; i < 1024; i++ {
		members = append(members, fmt.Sprintf("stale-%d", i))
	}
	installCommunitiesForTest(s, &analysis.CommunityResult{Communities: []analysis.Community{{ID: "large", Size: len(members), Members: members}}})
	s.hotspotsReady = true
	reader := &outlineCountingReader{Store: store}
	outlineJSON(t, s, outlineBoundContext(s, "community", "ws", map[string]bool{"a": true}), reader)
	require.NotContains(t, reader.lookedUp, "stale-0")
	require.LessOrEqual(t, len(reader.lookedUp), 1+6, "one community candidate, plus main candidates")
}

func TestOutlineCommunityFrontiersBatchAcrossGroups(t *testing.T) {
	s, store := outlineFixture(t)
	const groups = 200
	nodes := make([]*graph.Node, 0, groups)
	communities := make([]analysis.Community, 0, groups)
	for i := 0; i < groups; i++ {
		id := fmt.Sprintf("visible-%03d", i)
		nodes = append(nodes, &graph.Node{ID: id, Kind: graph.KindFunction, RepoPrefix: "a", WorkspaceID: "ws", Language: "go", FilePath: fmt.Sprintf("a/group-%03d.go", i)})
		members := []string{id}
		for j := 1; j < 128; j++ {
			members = append(members, fmt.Sprintf("unused-%03d-%03d", i, j))
		}
		communities = append(communities, analysis.Community{ID: fmt.Sprintf("group-%03d", i), Size: len(members), Members: members})
	}
	store.AddBatch(nodes, nil)
	installCommunitiesForTest(s, &analysis.CommunityResult{Communities: communities})
	s.hotspotsReady = true
	ctx := outlineBoundContext(s, "community-frontiers", "ws", map[string]bool{"a": true})
	reader := &outlineCountingReader{Store: store}
	got := outlineJSON(t, s, ctx, reader)
	legacy := outlineJSON(t, s, ctx, legacyOutlineReader{Reader: store})
	require.JSONEq(t, string(legacy), string(got))
	require.LessOrEqual(t, len(reader.lookedUp), groups+6, "fetch one visible member per group, plus main candidates")
	require.LessOrEqual(t, len(reader.lookupBatches), 3, "pack first candidates across groups rather than issuing one query per group")
	for _, id := range reader.lookedUp {
		require.NotContains(t, id, "unused-")
	}
	for _, batch := range reader.lookupBatches {
		require.LessOrEqual(t, len(batch), outlineCandidateBatchSize)
	}
	require.Zero(t, reader.allNodes)
	require.Zero(t, reader.allNodesLight)
	require.Zero(t, reader.allEdges)
}

func TestOutlineCommunityFrontiersPreserveLateMatchesAndTies(t *testing.T) {
	s, store := outlineFixture(t)
	late := make([]string, 512)
	for i := range late {
		late[i] = fmt.Sprintf("late-missing-%03d", i)
	}
	late[10], late[20], late[300] = "shared-missing", "shared-missing", "a-lib"
	communities := []analysis.Community{
		{ID: "late", Size: 20, Members: late},
		{ID: "early", Size: 20, Members: []string{"a-main"}},
		{ID: "missing", Size: 20, Members: []string{"shared-missing", "shared-missing"}},
		{ID: "foreign", Size: 20, Members: []string{"b-main", "outside-main", "b-main"}},
		{ID: "global", Size: 20, Members: []string{"global-main"}},
		{ID: "unowned", Size: 20, Members: []string{"unowned-main"}},
		{ID: "empty", Size: 20},
	}
	installCommunitiesForTest(s, &analysis.CommunityResult{Communities: communities})
	s.hotspotsReady = true
	ctx := outlineBoundContext(s, "community-late", "ws", map[string]bool{"a": true})
	reader := &outlineCountingReader{Store: store}
	got := outlineJSON(t, s, ctx, reader)
	legacy := outlineJSON(t, s, ctx, legacyOutlineReader{Reader: store})
	require.JSONEq(t, string(legacy), string(got))
	var response struct {
		Communities struct {
			Count int `json:"count"`
			Top   []struct {
				ID string `json:"id"`
			} `json:"top"`
		} `json:"communities"`
	}
	require.NoError(t, json.Unmarshal(got, &response))
	require.Equal(t, 4, response.Communities.Count)
	var top []string
	for _, c := range response.Communities.Top {
		top = append(top, c.ID)
	}
	require.Equal(t, []string{"late", "early", "global", "unowned"}, top, "completion order must not reorder equal-size communities")
	missingReads := 0
	for _, id := range reader.lookedUp {
		if id == "shared-missing" {
			missingReads++
		}
	}
	require.Equal(t, 1, missingReads, "cache missing IDs across rounds and communities")
	require.Contains(t, reader.lookedUp, "a-lib")
	require.NotContains(t, reader.lookedUp, "late-missing-400")
	for _, batch := range reader.lookupBatches {
		require.LessOrEqual(t, len(batch), outlineCandidateBatchSize)
	}
}

func TestOutlineCommunityFrontiersExhaustUnmatchedGroupsInBatches(t *testing.T) {
	s, store := outlineFixture(t)
	members := make([]string, 1024)
	for i := range members {
		members[i] = fmt.Sprintf("foreign-missing-%04d", i)
	}
	installCommunitiesForTest(s, &analysis.CommunityResult{Communities: []analysis.Community{{ID: "foreign", Size: len(members), Members: members}}})
	s.hotspotsReady = true
	ctx := outlineBoundContext(s, "community-exhaust", "ws", map[string]bool{"a": true})
	reader := &outlineCountingReader{Store: store}
	got := outlineJSON(t, s, ctx, reader)
	legacy := outlineJSON(t, s, ctx, legacyOutlineReader{Reader: store})
	require.JSONEq(t, string(legacy), string(got))
	require.Contains(t, reader.lookedUp, members[len(members)-1], "exhaust members before excluding the community")
	require.LessOrEqual(t, len(reader.lookupBatches), 20, "adaptive windows must not degrade into one lookup per member")
}

func TestOutlineOverlayReflectsUnsavedBuffers(t *testing.T) {
	s, store := outlineFixture(t)
	ctx := outlineBoundContext(s, "overlay", "ws", map[string]bool{"a": true})
	layer := graph.NewOverlayLayer()
	layer.MarkFile("a/main.go", false)
	// Fixture IDs are deliberately short rather than path-qualified, so
	// explicitly tombstone the declaration replaced by this unsaved buffer.
	layer.MarkRemoved("main", "a-main")
	layer.AddNode("a/main.go", &graph.Node{ID: "unsaved-main", Name: "main", Kind: graph.KindFunction, RepoPrefix: "a", WorkspaceID: "ws", ProjectID: "p", Language: "typescript", FilePath: "a/main.go"})
	layer.AddNode("a/main.go", &graph.Node{ID: "unsaved-helper", Name: "Helper", Kind: graph.KindFunction, RepoPrefix: "a", WorkspaceID: "ws", ProjectID: "p", Language: "typescript", FilePath: "a/main.go"})
	layer.AddEdge(&graph.Edge{From: "unsaved-main", To: "a-lib", Kind: graph.EdgeImports, FilePath: "a/main.go"})
	view := graph.NewOverlaidView(store, layer)
	_, aggregates := any(view).(graph.ScopedOutlineAggregator)
	require.False(t, aggregates)
	got := outlineJSON(t, s, WithOverlayView(ctx, view), nil)
	require.Contains(t, string(got), "unsaved-main")
	require.NotContains(t, string(got), `"id":"a-main"`)
	var decoded struct {
		Summary struct {
			TotalNodes int `json:"total_nodes"`
			Languages  []struct {
				Name  string `json:"name"`
				Nodes int    `json:"nodes"`
			} `json:"languages"`
		} `json:"summary"`
	}
	require.NoError(t, json.Unmarshal(got, &decoded))
	require.Equal(t, 6, decoded.Summary.TotalNodes)
	require.Contains(t, string(got), "typescript")
	require.NotNil(t, store.GetNode("a-main"))
	require.Nil(t, store.GetNode("unsaved-main"))
	installContractCoreKindTestRuntime(t, s)
	_, ok, err := scopedOutlineCountsFor(s.readerFor(WithOverlayView(ctx, view)), graph.NodeScope{WorkspaceID: "ws", RepoAllow: map[string]bool{"a": true}})
	require.NoError(t, err)
	require.False(t, ok, "the runtime must preserve the overlay fallback")
	runtime := outlineJSON(t, s, WithOverlayView(ctx, view), nil)
	require.JSONEq(t, string(got), string(runtime))
}

func TestOutlineContractRuntimeNeverScansAndPreservesFilters(t *testing.T) {
	s, store := outlineFixture(t)
	store.AddBatch([]*graph.Node{
		{ID: "contract", Kind: graph.KindContract, RepoPrefix: "a", WorkspaceID: "ws", FilePath: "a/api.yaml"},
		{ID: "config", Kind: graph.KindConfigKey, RepoPrefix: "a", WorkspaceID: "ws", FilePath: "a/config.yaml"},
		{ID: "table", Kind: graph.KindTable, RepoPrefix: "a", WorkspaceID: "ws", FilePath: "a/schema.sql"},
	}, []*graph.Edge{
		{From: "contract", To: "a-lib", Kind: graph.EdgeImports},
		{From: "a-main", To: "config", Kind: graph.EdgeReadsConfig},
		{From: "a-main", To: "a-lib", Kind: graph.EdgeCalls, Meta: map[string]any{"via": "spring.Bean"}},
		{From: "outside-main", To: "a-lib", Kind: graph.EdgeImports, Line: 7, Meta: map[string]any{"via": "spring.Bean"}},
		{From: "a-main", To: "a-lib", Kind: graph.EdgeMatches},
		{From: "a-main", To: "table", Kind: graph.EdgeProvides},
	})
	installContractCoreKindTestRuntime(t, s)
	ctx := outlineBoundContext(s, "runtime", "ws", map[string]bool{"a": true})
	reader := &outlineCountingReader{Store: store}
	got := outlineJSON(t, s, ctx, reader)
	require.Zero(t, reader.allEdges)
	require.Zero(t, reader.allNodes)
	require.Zero(t, reader.allNodesLight)
	require.Equal(t, 1, reader.aggregates, "the production wrapper must use filtered SQL aggregation")
	legacy := outlineJSON(t, s, ctx, legacyOutlineReader{Reader: store})
	require.JSONEq(t, string(legacy), string(got))
	var decoded struct {
		Summary struct {
			TotalEdges int `json:"total_edges"`
		} `json:"summary"`
	}
	require.NoError(t, json.Unmarshal(got, &decoded))
	require.Equal(t, 5, decoded.Summary.TotalEdges, "table provides remains core; contract/kind/metadata edges are excluded")
	// Explicit ownership sets replace kind classification, even when empty.
	for _, ids := range []map[string]bool{nil, {}, {"a-lib": true}} {
		wrapped := newContractCoreEdges(store, ctx, ids)
		aggregated, ok, err := scopedOutlineCountsFor(wrapped, graph.NodeScope{WorkspaceID: "ws", RepoAllow: map[string]bool{"a": true}})
		require.True(t, ok)
		require.NoError(t, err)
		var count int
		for _, edge := range wrapped.AllEdges() {
			if (graph.NodeScope{WorkspaceID: "ws", RepoAllow: map[string]bool{"a": true}}).Allows(wrapped.GetNode(edge.From)) && (graph.NodeScope{WorkspaceID: "ws", RepoAllow: map[string]bool{"a": true}}).Allows(wrapped.GetNode(edge.To)) {
				count++
			}
		}
		require.Equal(t, count, aggregated.TotalEdges)
	}
}

func TestOutlineHotspotsStopAfterVisibleRankedHead(t *testing.T) {
	s, store := outlineFixture(t)
	for i := 0; i < 1024; i++ {
		s.hotspots = append(s.hotspots, analysis.HotspotEntry{ID: fmt.Sprintf("hot-stale-%d", i)})
	}
	reader := &outlineCountingReader{Store: store}
	outlineJSON(t, s, outlineBoundContext(s, "hotspot-head", "", map[string]bool{"a": true, "b": true, "c": true, "legacy": true}), reader)
	require.NotContains(t, reader.lookedUp, "hot-stale-128")
	require.LessOrEqual(t, len(reader.lookedUp), 128+10, "only the first hotspot head and analyzer/main candidates are needed")
}

func TestOutlineContractRuntimeHonorsCancellation(t *testing.T) {
	s, store := outlineFixture(t)
	installContractCoreKindTestRuntime(t, s)
	ctx := outlineBoundContext(s, "cancel-outline", "ws", map[string]bool{"a": true})
	ctx, cancel := context.WithCancel(ctx)
	cancel()
	reader := &outlineCountingReader{Store: store}
	_, err := s.handleGetRepoOutline(withRequestView(ctx, &requestView{reader: reader}), makeReq("get_repo_outline", nil))
	require.True(t, errors.Is(err, context.Canceled))
	require.Zero(t, reader.allEdges)
	require.Zero(t, reader.allNodes)
}

func TestOutlineGoldenSixRepo(t *testing.T) {
	store := openContractCoreCapStore(t)
	s := &Server{graph: store, session: newSessionState(), sessions: newSessionMap(), logger: zap.NewNop()}
	var nodes []*graph.Node
	var edges []*graph.Edge
	comms := &analysis.CommunityResult{Modularity: .5}
	for i := 1; i <= 6; i++ {
		repo := fmt.Sprintf("repo%d", i)
		nodes = append(nodes,
			&graph.Node{ID: repo + "-main", Name: "main", Kind: graph.KindFunction, RepoPrefix: repo, WorkspaceID: "ws", Language: "go", FilePath: repo + "/main.go"},
			&graph.Node{ID: repo + "-lib", Name: "helper", Kind: graph.KindFunction, RepoPrefix: repo, WorkspaceID: "ws", Language: "go", FilePath: repo + "/lib.go"},
		)
		other := fmt.Sprintf("repo%d", i%6+1)
		edges = append(edges,
			&graph.Edge{From: repo + "-main", To: repo + "-lib", Kind: graph.EdgeCalls},
			&graph.Edge{From: repo + "-main", To: repo + "-lib", Kind: graph.EdgeImports},
			&graph.Edge{From: other + "-main", To: repo + "-lib", Kind: graph.EdgeImports},
		)
		comms.Communities = append(comms.Communities, analysis.Community{ID: repo, Label: repo, Size: 2, Cohesion: .5, Members: []string{repo + "-main", repo + "-lib"}})
	}
	store.AddBatch(nodes, edges)
	installCommunitiesForTest(s, comms)
	for i := 6; i >= 1; i-- {
		repo := fmt.Sprintf("repo%d", i)
		s.hotspots = append(s.hotspots, analysis.HotspotEntry{ID: repo + "-lib", Name: "helper", Kind: "function", FilePath: repo + "/lib.go", FanIn: 2, FanOut: 0, ComplexityScore: 3})
	}
	s.hotspotsReady = true
	for i := 1; i <= 6; i++ {
		repo := fmt.Sprintf("repo%d", i)
		ctx := outlineBoundContext(s, repo, "ws", map[string]bool{repo: true})
		got := outlineJSON(t, s, ctx, store)
		oracle := outlineJSON(t, s, ctx, legacyOutlineReader{store})
		require.JSONEq(t, string(oracle), string(got))
		golden := fmt.Sprintf(`{"summary":{"total_nodes":2,"total_edges":2,"primary_language":"go","languages":[{"name":"go","nodes":2}]},"communities":{"count":1,"modularity":0.5,"top":[{"id":%q,"label":%q,"size":2,"cohesion":0.5}]},"hotspots":[{"id":%q,"name":"helper","kind":"function","file_path":%q,"fan_in":2,"fan_out":0,"complexity_score":3}],"most_imported_files":[{"path":%q,"import_count":2}],"entry_points":[{"id":%q,"name":"main","file_path":%q}]}`, repo, repo, repo+"-lib", repo+"/lib.go", repo+"/lib.go", repo+"-main", repo+"/main.go")
		require.JSONEq(t, golden, string(got), repo)
	}
	// The unbound response still uses the exact legacy Stats/edge-count path
	// and formatting. Even JSON key/number spelling stays byte-identical.
	require.Equal(t, outlineJSON(t, s, context.Background(), legacyOutlineReader{store}), outlineJSON(t, s, context.Background(), store))
}
