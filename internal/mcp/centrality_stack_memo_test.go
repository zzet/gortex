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
	t.Helper()
	base, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	primary := filepath.Join(base, "repo")
	worktree := filepath.Join(base, "wt")
	require.NoError(t, os.Mkdir(primary, 0o755))
	for name, content := range adjacencyMemoRepoFiles() {
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
	stackedAdjacencyDisabledForTest = true
	directResult := f.srv.boundedCentralityForRequest(ctx, seeds, candidates)
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
