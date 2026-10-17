package mcp

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/graphview"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
	"github.com/zzet/gortex/internal/pathkey"
	"github.com/zzet/gortex/internal/query"
	"github.com/zzet/gortex/internal/search"
	"github.com/zzet/gortex/internal/search/rerank"
)

// adjacencyMemoRepoFiles is a small call graph whose symbols share the
// "Widget" token, so one symbol search returns a candidate set whose bounded
// neighbourhood spans several files and both edge kinds the build keeps.
func adjacencyMemoRepoFiles() map[string]string {
	files := map[string]string{
		"edit.go": "package repo\n\nfunc New() {}\n",
	}
	for i := 0; i < 8; i++ {
		var b strings.Builder
		fmt.Fprintf(&b, "package repo\n\ntype Widget%d struct{ next *Widget%d }\n\n", i, (i+1)%8)
		fmt.Fprintf(&b, "func BuildWidget%d() *Widget%d {\n", i, i)
		fmt.Fprintf(&b, "\tw := &Widget%d{}\n", i)
		fmt.Fprintf(&b, "\tRenderWidget%d(w)\n", (i+1)%8)
		fmt.Fprintf(&b, "\tRenderWidget%d(nil)\n", (i+3)%8)
		b.WriteString("\tNew()\n\tmissingHelper()\n\treturn w\n}\n\n")
		fmt.Fprintf(&b, "func RenderWidget%d(w *Widget%d) {\n", i, i)
		fmt.Fprintf(&b, "\tif w != nil {\n\t\tBuildWidget%d()\n\t}\n}\n", (i+2)%8)
		files[fmt.Sprintf("widget%d.go", i)] = b.String()
	}
	return files
}

// newAdjacencyMemoFixture is the real checkout-mutation fixture over a repo
// with a call graph: a primary with its commit, a linked worktree routed
// through the lifecycle, and a server whose materializer composes the stack.
func newAdjacencyMemoFixture(t *testing.T) (*realCheckoutMutationFixture, string) {
	return newAdjacencyMemoFixtureWithFiles(t, nil)
}

func newAdjacencyMemoFixtureWithFiles(t *testing.T, extra map[string]string) (*realCheckoutMutationFixture, string) {
	t.Helper()
	base, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	primary := filepath.Join(base, "repo")
	worktree := filepath.Join(base, "wt")
	require.NoError(t, os.Mkdir(primary, 0o755))
	files := adjacencyMemoRepoFiles()
	for name, content := range extra {
		files[name] = content
	}
	for name, content := range files {
		require.NoError(t, os.WriteFile(filepath.Join(primary, name), []byte(content), 0o644))
	}
	require.NoError(t, os.WriteFile(filepath.Join(primary, ".gortex.yaml"), []byte("workspace: adjacency-memo\n"), 0o644))
	checkoutMutationGit(t, primary, "init", "--initial-branch=main")
	checkoutMutationGit(t, primary, "add", "-A")
	checkoutMutationGit(t, primary, "commit", "-m", "base")
	checkoutMutationGit(t, primary, "worktree", "add", "-b", "feature", worktree)
	// The feature branch commits a change of its own, so the checkout's
	// commit layer carries content over the dedicated root below it.
	branchFile := filepath.Join(worktree, "widget1.go")
	branchSource, err := os.ReadFile(branchFile)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(branchFile, []byte(strings.Replace(string(branchSource), "\tRenderWidget2(w)\n", "\tRenderWidget5(w)\n\tBuildWidget7()\n", 1)), 0o644))
	checkoutMutationGit(t, worktree, "commit", "-am", "feature change")
	// A second worktree commits a different change: its stack has other
	// generations at the same depths as the first one's.
	second := filepath.Join(base, "wt2")
	checkoutMutationGit(t, primary, "worktree", "add", "-b", "feature2", second)
	secondFile := filepath.Join(second, "widget6.go")
	secondSource, err := os.ReadFile(secondFile)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(secondFile, []byte(strings.Replace(string(secondSource), "\tNew()\n", "\tNew()\n\tRenderWidget0(nil)\n\tBuildWidget2()\n", 1)), 0o644))
	checkoutMutationGit(t, second, "commit", "-am", "second feature change")

	store, err := store_sqlite.Open(filepath.Join(base, "store.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	cfgPath := filepath.Join(base, "config.yaml")
	global := &config.GlobalConfig{}
	global.SetConfigPath(cfgPath)
	require.NoError(t, global.Save())
	cm, err := config.NewConfigManager(cfgPath)
	require.NoError(t, err)
	registry := parser.NewRegistry()
	languages.RegisterAll(registry)
	bm := search.NewNull()
	mi := indexer.NewMultiIndexer(store, registry, bm, cm, zap.NewNop())
	leases := graphview.NewLeaseManager()
	lifecycle, err := indexer.NewCheckoutLifecycle(indexer.CheckoutLifecycleConfig{
		MultiIndexer: mi, ConfigManager: cm, Graph: store,
		Logger: zap.NewNop(), ViewLeases: leases,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = lifecycle.Close() })
	runtime, err := indexer.NewDedicatedBaseRuntime(store, lifecycle.ViewLeases())
	require.NoError(t, err)
	require.NoError(t, lifecycle.SetDedicatedBaseCleanupRuntime(runtime))
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	registered, err := lifecycle.Register(ctx, config.RepoEntry{Path: primary, Name: "repo"}, indexer.TrackSourceCLI)
	require.NoError(t, err)
	require.NoError(t, registered.CatalogErr)
	overview, err := lifecycle.FamiliesOverview(ctx, registered.FamilyID)
	require.NoError(t, err)
	checkoutID, primaryID := "", ""
	for _, family := range overview.Families {
		for _, checkout := range family.Checkouts {
			if pathkey.EqualPaths(checkout.RootPath, worktree) {
				checkoutID = checkout.CheckoutID
			}
			if pathkey.EqualPaths(checkout.RootPath, second) {
				primaryID = checkout.CheckoutID
			}
		}
	}
	require.NotEmpty(t, checkoutID)
	require.True(t, lifecycle.ActivateCheckout(checkoutID, "adjacency-memo-test"))
	require.Eventually(t, func() bool {
		route, found, routeErr := store.Catalog().GetCheckoutRoute(ctx, checkoutID)
		return routeErr == nil && found && route.State == store_sqlite.RouteActive &&
			route.CommitGenerationID > 0 && route.DirtyGenerationID > 0
	}, 40*time.Second, 20*time.Millisecond, "the worktree did not publish its initial route")
	publisher, err := indexer.NewInitialBasePublisher(lifecycle)
	require.NoError(t, err)
	t.Cleanup(publisher.Close)

	engine := query.NewEngine(store)
	engine.SetSearch(bm)
	srv := NewServer(engine, store, nil, nil, zap.NewNop(), nil, MultiRepoOptions{
		MultiIndexer: mi, ConfigManager: cm,
	})
	srv.SetMaterializer(&graphview.Materializer{Store: store, Catalog: store.Catalog(), Leases: leases})
	srv.lifecycle = lifecycle
	// Publications prewarm the route they flip to, masks and the newest
	// generation's rows, as the daemon wires it.
	srv.wireRoutePrewarm()
	f := &realCheckoutMutationFixture{
		srv: srv, store: store,
		primary: primary, worktree: worktree, checkoutID: checkoutID,
	}
	// Publish the family's committed base, as the daemon does after warm-up,
	// and let one edit recompose the route over it: the stack is then the
	// dedicated root with the working tree on top, the live daemon's shape.
	out := publisher.PublishRepo(ctx, registered.Prefix)
	require.NoError(t, out.Err)
	require.Empty(t, out.Skipped, "the committed base was not published: %s", out.Skipped)
	written := f.edit(t, worktree, map[string]any{
		"path": "repo/edit.go", "old_string": "func New() {}", "new_string": "func New() {}\n\nfunc Extra() { New() }",
	})
	require.False(t, written.IsError, viewResultText(t, written))
	f.awaitMutation(t, worktree, written)
	return f, primaryID
}

// adjacencyMemoView selects the fixture checkout's routed view and returns a
// request context carrying it.
func adjacencyMemoView(t testing.TB, f *realCheckoutMutationFixture) (context.Context, *requestView) {
	t.Helper()
	view, err := f.srv.selectRequestView(context.Background(),
		graphview.Selector{Kind: graphview.SelectorWorktree, CheckoutID: f.checkoutID}, requestViewPolicy{})
	require.NoError(t, err)
	require.NotNil(t, view)
	t.Cleanup(view.close)
	return withRequestView(context.Background(), view), view
}

// adjacencyMemoCandidates are every Widget symbol the view serves, in ID order:
// a candidate set whose neighbourhood reaches every file of the fixture.
func adjacencyMemoCandidates(t testing.TB, view *requestView) []string {
	t.Helper()
	var ids []string
	for _, name := range []string{"BuildWidget", "RenderWidget", "Widget"} {
		for i := 0; i < 8; i++ {
			for _, n := range view.reader.FindNodesByName(fmt.Sprintf("%s%d", name, i)) {
				ids = append(ids, n.ID)
			}
		}
	}
	for _, n := range view.reader.FindNodesByName("New") {
		ids = append(ids, n.ID)
	}
	for _, n := range view.reader.FindNodesByName("RenamedNew") {
		ids = append(ids, n.ID)
	}
	sort.Strings(ids)
	require.NotEmpty(t, ids)
	return ids
}

// centralityThroughBoth returns the rerank centrality of one request through
// the memoized stack and through the view's own reader.
func centralityThroughBoth(t testing.TB, f *realCheckoutMutationFixture, ctx context.Context, seeds, candidates []string) (memo, direct centralityObservation) {
	t.Helper()
	require.NotNil(t, f.srv.stackedAdjacencyReader(ctx), "the routed view must be served through the stacked memo")
	memoResult := f.srv.boundedCentralityForRequest(ctx, seeds, candidates)
	// The old path: no stacked memo and no preloaded rows — every level read
	// with SQL.
	stackedAdjacencyDisabledForTest = true
	restoreRows := graphview.DisableGenerationRowsForTest()
	directResult := f.srv.boundedCentralityForRequest(ctx, seeds, candidates)
	restoreRows()
	stackedAdjacencyDisabledForTest = false
	return observeCentrality(memoResult), observeCentrality(directResult)
}

// requireEdgeBatchesMatchTheView compares the memoized candidate edge batches
// with the view reader's own, twice: once cold and once from the memo.
func requireEdgeBatchesMatchTheView(t testing.TB, f *realCheckoutMutationFixture, ctx context.Context, view *requestView, ids []string) {
	t.Helper()
	edges := f.srv.stackedEdgeBatches(ctx)
	require.NotNil(t, edges, "the routed view must serve its edge batches through the stacked memo")
	for pass := 0; pass < 2; pass++ {
		wantOut := view.reader.GetOutEdgesByNodeIDs(ids)
		wantIn := view.reader.GetInEdgesByNodeIDs(ids)
		gotOut := edges.GetOutEdgesByNodeIDs(ids)
		gotIn := edges.GetInEdgesByNodeIDs(ids)
		for _, id := range ids {
			require.Equal(t, memoEdgeKeys(wantOut[id]), memoEdgeKeys(gotOut[id]), "pass %d: out-edges of %s", pass, id)
			require.Equal(t, memoEdgeKeys(wantIn[id]), memoEdgeKeys(gotIn[id]), "pass %d: in-edges of %s", pass, id)
		}
	}
}

func memoEdgeKeys(edges []*graph.Edge) []string {
	out := make([]string, 0, len(edges))
	for _, e := range edges {
		out = append(out, fmt.Sprintf("%s>%s:%s@%s:%d/%s", e.From, e.To, e.Kind, e.FilePath, e.Line, e.Origin))
	}
	return out
}

// stackMemoLevelSizes reports each retained level's adjacency rows.
func stackMemoLevelSizes(m *stackAdjacencyMemo) map[string]int {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make(map[string]int, len(m.entries))
	for key, entry := range m.entries {
		entry.level.mu.RLock()
		out[key] = len(entry.level.nodes) + len(entry.level.full) + len(entry.level.light)
		entry.level.mu.RUnlock()
	}
	return out
}

type centralityObservation struct {
	Scores    map[string]float64
	Nodes     int
	Edges     int
	Truncated bool
}

func observeCentrality(r rerank.CentralityResult) centralityObservation {
	return centralityObservation{Scores: r.Scores, Nodes: r.NodeCount, Edges: r.EdgeCount, Truncated: r.Truncated}
}

func TestStackedAdjacencyMemoRanksLikeTheViewReader(t *testing.T) {
	f, primaryID := newAdjacencyMemoFixture(t)
	f.srv.pprCache.enabled = false // compare fresh walks, never a cached one

	ctx, view := adjacencyMemoView(t, f)
	sources := view.materialized.GenerationSources()
	t.Logf("generations before the edit: %v", view.materialized.Generations())
	t.Logf("stack: %d generation(s), composes base corpus: %v", len(sources), view.materialized.ComposesBaseCorpus())
	require.False(t, view.materialized.ComposesBaseCorpus(), "fixture precondition: the stack is rooted in the dedicated base")
	candidates := adjacencyMemoCandidates(t, view)
	seeds := candidates[:4]

	memo, direct := centralityThroughBoth(t, f, ctx, seeds, candidates)
	requireEdgeBatchesMatchTheView(t, f, ctx, view, candidates)
	require.NotEmpty(t, direct.Scores)
	require.Equal(t, direct, memo, "the memoized build must equal the view reader's build")

	// A second request reads nothing below: every answer is a memo hit.
	_, nodeMissesBefore, _, edgeMissesBefore := f.srv.stackAdjacencyMemoFor().counts()
	again := observeCentrality(f.srv.boundedCentralityForRequest(ctx, seeds, candidates))
	_, nodeMissesAfter, _, edgeMissesAfter := f.srv.stackAdjacencyMemoFor().counts()
	require.Equal(t, direct, again)
	require.Zero(t, nodeMissesAfter-nodeMissesBefore, "a repeated build read node rows again")
	require.Zero(t, edgeMissesAfter-edgeMissesBefore, "a repeated build read edge rows again")

	// An edit publishes a new top generation. The next build through the new
	// route equals the new view's own build, and reads only the new top level.
	written := f.edit(t, f.worktree, map[string]any{
		"path": "repo/widget3.go", "old_string": "\tRenderWidget4(w)\n", "new_string": "\tRenderWidget6(w)\n\tBuildWidget0()\n",
	})
	require.False(t, written.IsError, viewResultText(t, written))
	f.awaitMutation(t, f.worktree, written)

	ctx2, view2 := adjacencyMemoView(t, f)
	require.NotEqual(t, view.materialized.ID.Fingerprint(), view2.materialized.ID.Fingerprint(), "the edit must publish a new route")
	t.Logf("generations after the edit: %v", view2.materialized.Generations())
	sources2 := view2.materialized.GenerationSources()
	top, ok := sources2[len(sources2)-1].Layer.(*graphview.GenerationLayer)
	require.True(t, ok && top.RowsPreloaded(), "the publication's prewarm did not preload the new top generation's rows")
	candidates2 := adjacencyMemoCandidates(t, view2)
	// The first build after the publication reads the new top level only:
	// every level the previous route composed answers from its memo.
	before := stackMemoLevelSizes(f.srv.stackAdjacencyMemoFor())
	require.NotEmpty(t, f.srv.boundedCentralityForRequest(ctx2, seeds, candidates2).Scores)
	after := stackMemoLevelSizes(f.srv.stackAdjacencyMemoFor())
	grown := 0
	for key, size := range after {
		if old, existed := before[key]; existed {
			require.Equal(t, old, size, "level %s was read again after the publication", key)
			continue
		}
		grown++
	}
	require.Equal(t, 1, grown, "the publication's build must add exactly the new top level: %v -> %v", before, after)
	memo2, direct2 := centralityThroughBoth(t, f, ctx2, seeds, candidates2)
	requireEdgeBatchesMatchTheView(t, f, ctx2, view2, candidates2)
	require.Equal(t, direct2, memo2, "after a route flip the memoized build must equal the new view's build")
	require.NotEqual(t, direct.Scores, direct2.Scores, "the edit must change the neighbourhood, or this test proves nothing")

	// The second worktree's stack shares the dedicated root and nothing
	// else: its levels are other generations at the same depths, and must
	// never be answered from the first worktree's memo.
	require.NotEmpty(t, primaryID)
	require.True(t, f.srv.lifecycle.ActivateCheckout(primaryID, "adjacency-memo-test"))
	var primaryView *requestView
	require.Eventually(t, func() bool {
		v, selectErr := f.srv.selectRequestView(context.Background(),
			graphview.Selector{Kind: graphview.SelectorWorktree, CheckoutID: primaryID}, requestViewPolicy{})
		if selectErr != nil || v == nil {
			return false
		}
		primaryView = v
		return true
	}, 40*time.Second, 50*time.Millisecond, "the second worktree was never routed")
	require.NotNil(t, primaryView)
	t.Cleanup(primaryView.close)
	t.Logf("second worktree generations: %v", primaryView.materialized.Generations())
	primaryCtx := withRequestView(context.Background(), primaryView)
	if primaryView.materialized.ComposesBaseCorpus() {
		t.Fatal("fixture precondition: the second worktree's stack is rooted in the dedicated base")
	}
	candidates3 := adjacencyMemoCandidates(t, primaryView)
	memo3, direct3 := centralityThroughBoth(t, f, primaryCtx, seeds, candidates3)
	requireEdgeBatchesMatchTheView(t, f, primaryCtx, primaryView, candidates3)
	require.Equal(t, direct3, memo3, "the second worktree's memoized build must equal its own view's build")
	require.NotEqual(t, direct2.Scores, direct3.Scores, "the two checkouts' neighbourhoods must differ, or this proves nothing")
}

// TestSymbolSearchRanksAlikeThroughTheStackedMemo drives the whole symbol
// search through the handler, before and after an edit, with and without the
// memo, and requires the same ranked answer.
func TestSymbolSearchRanksAlikeThroughTheStackedMemo(t *testing.T) {
	f, _ := newAdjacencyMemoFixture(t)
	f.srv.pprCache.enabled = false // compare fresh walks, never a cached one
	search := func(query string) string {
		req := mcplib.CallToolRequest{}
		req.Params.Name = "search_symbols"
		req.Params.Arguments = map[string]any{
			"query": query, "limit": 20,
			"view": map[string]any{"kind": "worktree", "checkout_id": f.checkoutID},
		}
		ctx := WithSessionCWD(WithSessionID(context.Background(), "adjacency-memo-search"), f.primary)
		result, err := f.srv.wrapToolHandler(f.srv.handleSearchSymbols)(ctx, req)
		require.NoError(t, err)
		require.False(t, result.IsError, viewResultText(t, result))
		return viewResultText(t, result)
	}
	compare := func(label string) {
		for _, q := range []string{"Widget", "RenderWidget3", "BuildWidget", "render widget"} {
			hitsBefore, missesBefore, edgeHitsBefore, edgeMissesBefore := f.srv.stackAdjacencyMemoFor().counts()
			withMemo := search(q)
			restoreRows := graphview.DisableGenerationRowsForTest()
			hitsAfter, missesAfter, edgeHitsAfter, edgeMissesAfter := f.srv.stackAdjacencyMemoFor().counts()
			// The fixture's null text backend answers a multi-word query with
			// no candidates, so only the identifier queries reach the rerank.
			require.True(t, strings.Contains(q, " ") || (hitsAfter-hitsBefore)+(missesAfter-missesBefore)+(edgeHitsAfter-edgeHitsBefore)+(edgeMissesAfter-edgeMissesBefore) > 0,
				"%s: query %q never read through the stacked memo", label, q)
			stackedAdjacencyDisabledForTest = true
			withoutMemo := search(q)
			stackedAdjacencyDisabledForTest = false
			restoreRows()
			require.Equal(t, withoutMemo, withMemo, "%s: query %q ranked differently through the memo", label, q)
		}
	}
	compare("before the edit")
	written := f.edit(t, f.worktree, map[string]any{
		"path": "repo/widget5.go", "old_string": "func RenderWidget5(", "new_string": "func RenderWidgetRenamed5(",
	})
	require.False(t, written.IsError, viewResultText(t, written))
	f.awaitMutation(t, f.worktree, written)
	compare("after the edit")
}

// failingLevelReader answers a batch partially and reports an error, the shape
// of a read the request's end interrupted.
type failingLevelReader struct {
	graph.Reader
	calls int
}

func (r *failingLevelReader) GetNodesByIDsContext(_ context.Context, ids []string) (map[string]*graph.Node, error) {
	r.calls++
	return map[string]*graph.Node{ids[0]: {ID: ids[0]}}, context.Canceled
}

func (r *failingLevelReader) GetNodesByIDs(ids []string) map[string]*graph.Node {
	out, _ := r.GetNodesByIDsContext(context.Background(), ids)
	return out
}

func (r *failingLevelReader) GetOutEdgesByNodeIDsContext(_ context.Context, ids []string, _ int) (map[string][]*graph.Edge, bool, error) {
	r.calls++
	return map[string][]*graph.Edge{ids[0]: {{From: ids[0], To: "b", Kind: graph.EdgeCalls}}}, true, context.Canceled
}

// A batch that did not finish is returned to its caller and never memoized:
// the next request reads the level again instead of trusting a partial row
// set for the lifetime of the generation.
func TestStackedAdjacencyMemoNeverKeepsAnUnfinishedBatch(t *testing.T) {
	memo := newStackAdjacencyMemo()
	underlying := &failingLevelReader{}
	level := &memoLevelReader{Reader: underlying, memo: memo, level: memo.level("repo/1"), light: true}

	nodes, err := level.GetNodesByIDsContext(context.Background(), []string{"a", "c"})
	require.ErrorIs(t, err, context.Canceled)
	require.NotNil(t, nodes["a"], "the partial answer still reaches its caller")
	_, err = level.GetNodesByIDsContext(context.Background(), []string{"a", "c"})
	require.ErrorIs(t, err, context.Canceled)
	require.Equal(t, 2, underlying.calls, "a partial node batch was memoized")

	edges, _, err := level.GetOutEdgesByNodeIDsContext(context.Background(), []string{"a"}, 10)
	require.ErrorIs(t, err, context.Canceled)
	require.Len(t, edges["a"], 1)
	_, _, _ = level.GetOutEdgesByNodeIDsContext(context.Background(), []string{"a"}, 10)
	require.Equal(t, 4, underlying.calls, "a partial edge batch was memoized")
	require.Empty(t, level.level.nodes)
	require.Empty(t, level.level.light)
}
