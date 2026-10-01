package mcp

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/daemon"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/graphview"
	"github.com/zzet/gortex/internal/indexer"
	"go.uber.org/zap"
)

func TestSourceSearchPendingScopePrecedesLimit(t *testing.T) {
	stack := newViewStack(t)
	require.NoError(t, os.MkdirAll(filepath.Join(stack.worktreeRoot, "selected"), 0755))
	require.NoError(t, os.WriteFile(filepath.Join(stack.worktreeRoot, "aaa.go"), []byte("package outside\n// needle outside\n"), 0644))
	require.NoError(t, os.WriteFile(filepath.Join(stack.worktreeRoot, "selected", "new.go"), []byte("package selected\n// needle current addition\n"), 0644))
	routeViewCheckout(t, stack.store, stack.graphID, stack.commit, stack.dirty, store_sqlite.RoutePending)
	stack.srv.freshnessWaiter = &fakeFreshnessWaiter{answer: func(int, string, string) (*indexer.CheckoutRefreshTicket, error) {
		t.Fatal("scoped text search waited for relationship publication")
		return nil, nil
	}}
	args := freshArgs(map[string]any{"query": "needle", "path": "selected", "limit": 1, requireExactArgName: true}, time.Second)
	res, err := stack.callHandler(t, stack.worktreeRoot, "search_text", args, stack.srv.handleSearchText)
	require.NoError(t, err)
	require.False(t, res.IsError, viewResultText(t, res))
	require.Equal(t, []string{"repo/selected/new.go"}, searchTextMatchPaths(t, res))
	require.Contains(t, viewResultText(t, res), "needle current addition")
	require.NotContains(t, viewResultText(t, res), "needle outside")
	rider := resultFreshness(t, res)
	require.Equal(t, true, rider["fresh"])
	require.Equal(t, "text", rider["freshness_scope"])
	require.Equal(t, "pending", rider["graph_freshness"])
}

func TestSourceSearchUsesCurrentDiskAndPinnedOverlayInventory(t *testing.T) {
	srv, root := setupTestServer(t)
	// Indexed bytes must not survive a disk deletion or an editor tombstone.
	require.NoError(t, os.Remove(filepath.Join(root, "main.go")))
	require.NoError(t, os.WriteFile(filepath.Join(root, "disk.go"), []byte("package current\n// needle disk addition\n"), 0644))
	require.NoError(t, os.WriteFile(filepath.Join(root, "deleted.go"), []byte("package old\n// needle tombstoned\n"), 0644))
	require.NoError(t, os.WriteFile(filepath.Join(root, "replaced.go"), []byte("package old\n// needle obsolete\n"), 0644))
	ctx := withRequestView(context.Background(), &requestView{sourceScope: "text", viewRoot: root})
	ctx = withOverlayRequestSnapshot(ctx, &overlayRequestSnapshot{canonical: true, files: []daemon.OverlayFile{
		{Path: filepath.Join(root, "editor.go"), Content: "package editor\n// needle editor addition\n"},
		{Path: filepath.Join(root, "deleted.go"), Deleted: true},
		{Path: filepath.Join(root, "replaced.go"), Content: "package editor\n// needle editor replacement\n"},
	}})
	res, err := srv.handleSearchText(ctx, newSearchTextRequest("needle"))
	require.NoError(t, err)
	require.False(t, res.IsError, viewResultText(t, res))
	require.Equal(t, []string{"disk.go", "editor.go", "replaced.go"}, searchTextMatchPaths(t, res))
	text := viewResultText(t, res)
	require.Contains(t, text, "needle disk addition")
	require.Contains(t, text, "needle editor addition")
	require.Contains(t, text, "needle editor replacement")
	require.NotContains(t, text, "obsolete")
	require.NotContains(t, text, "tombstoned")
	var answer struct {
		Evidence struct {
			Verified bool `json:"verified"`
			Complete bool `json:"corpus_complete"`
		} `json:"source_evidence"`
	}
	require.NoError(t, json.Unmarshal([]byte(text), &answer))
	require.True(t, answer.Evidence.Verified)
	require.True(t, answer.Evidence.Complete)
	res, err = srv.handleSearchText(ctx, newSearchTextRequest("helper"))
	require.NoError(t, err)
	require.False(t, res.IsError, viewResultText(t, res))
	require.Empty(t, searchTextMatchPaths(t, res), "deleted indexed main.go must not contribute hits")
}

func TestSourceSearchSnapshotRejectsByteBudgetWithoutPartialProof(t *testing.T) {
	srv, root := setupTestServer(t)
	srv.indexer = indexer.New(srv.graph, testRegistry(), config.IndexConfig{MaxFileSize: sourceSearchMaxBytes * 2}, zap.NewNop())
	path := filepath.Join(root, "large.go")
	require.NoError(t, os.WriteFile(path, []byte("package large\n"), 0644))
	// A sparse file reaches the bound without allocating a large test buffer.
	require.NoError(t, os.Truncate(path, sourceSearchMaxBytes+1))
	view := &requestView{sourceScope: "text", viewRoot: root}
	ctx := withRequestView(context.Background(), view)
	files, err := srv.sourceSearchSnapshot(ctx, view, []string{"large.go"}, ResolvedScope{})
	require.ErrorIs(t, err, indexer.ErrSourceSearchBudget)
	require.Nil(t, files)
}

func TestSourceSearchRegexpUsesPhysicalLines(t *testing.T) {
	srv, root := setupTestServer(t)
	require.NoError(t, os.WriteFile(filepath.Join(root, "lines.go"), []byte("package lines\r\n// marker\r\n"), 0644))
	ctx := withRequestView(context.Background(), &requestView{sourceScope: "text", viewRoot: root})
	req := newSearchTextRequest("^// marker$")
	req.Params.Arguments = map[string]any{"query": "^// marker$", "regexp": true, "path": "lines.go"}
	res, err := srv.handleSearchText(ctx, req)
	require.NoError(t, err)
	require.False(t, res.IsError, viewResultText(t, res))
	var answer struct {
		Matches []struct {
			Line int    `json:"line"`
			Text string `json:"text"`
		} `json:"matches"`
	}
	require.NoError(t, json.Unmarshal([]byte(viewResultText(t, res)), &answer))
	require.Len(t, answer.Matches, 1)
	require.Equal(t, 2, answer.Matches[0].Line)
	require.Equal(t, "// marker", answer.Matches[0].Text)
	req.Params.Arguments = map[string]any{"query": "^$", "regexp": true, "path": "lines.go"}
	res, err = srv.handleSearchText(ctx, req)
	require.NoError(t, err)
	require.False(t, res.IsError, viewResultText(t, res))
	require.Empty(t, searchTextMatchPaths(t, res), "final newline must not invent an empty source line")
}

func TestSourceSearchWarmGlobalKeepsIndexedRoute(t *testing.T) {
	stack := newViewStack(t)
	ctx := WithSessionCWD(WithSessionID(context.Background(), viewTestSession), stack.worktreeRoot)
	req := newSearchTextRequest("Keeper")
	selector := graphview.Selector{Kind: graphview.SelectorAuto}
	view, err := stack.srv.resolveSourceRequestView(ctx, selector, &req, "search_text", requestFreshness{}, capabilityRequest{})
	require.NoError(t, err)
	require.Nil(t, view, "stable global search must retain the indexed route")
	req.Params.Arguments = map[string]any{"query": "Keeper", "path": "keep.go"}
	view, err = stack.srv.resolveSourceRequestView(ctx, selector, &req, "search_text", requestFreshness{}, capabilityRequest{})
	require.NoError(t, err)
	require.NotNil(t, view)
	require.Equal(t, "text", view.sourceScope)
}

func TestSourceSearchFallbackWaitsWithinOriginalDeadline(t *testing.T) {
	stack := newViewStack(t)
	ctx := WithSessionCWD(WithSessionID(context.Background(), viewTestSession), stack.worktreeRoot)
	req := newSearchTextRequest("Keeper")
	req.Params.Arguments = map[string]any{"query": "Keeper", "path": "keep.go"}
	deadline := time.Now().Add(time.Second)
	freshness := requestFreshness{requireFresh: true, requireExact: true, deadline: deadline, hasDeadline: true}
	view, err := stack.srv.resolveSourceRequestView(ctx, graphview.Selector{Kind: graphview.SelectorAuto}, &req, "search_text", freshness, capabilityRequest{})
	require.NoError(t, err)
	require.NotNil(t, view)
	routeViewCheckout(t, stack.store, stack.graphID, stack.commit, stack.dirty, store_sqlite.RoutePending)
	waiter := &fakeFreshnessWaiter{answerCtx: func(waitCtx context.Context, _ int, _, _ string) (*indexer.CheckoutRefreshTicket, error) {
		actual, ok := waitCtx.Deadline()
		require.True(t, ok)
		require.Equal(t, deadline, actual, "fallback must preserve the caller's absolute wait budget")
		return nil, indexer.ErrCheckoutRefreshStopped
	}}
	stack.srv.freshnessWaiter = waiter
	res, err := stack.srv.fallbackSourceSearch(withRequestView(ctx, view), req, view, func(context.Context, mcplib.CallToolRequest) (*mcplib.CallToolResult, error) {
		t.Fatal("exact source fallback served an unsettled graph")
		return nil, nil
	})
	require.NoError(t, err)
	require.True(t, res.IsError)
	require.Len(t, waiter.observed(), 1, "fallback should wait internally rather than ask the caller to retry")
}
