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
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/graphview"
	"github.com/zzet/gortex/internal/indexer"
)

func TestSourceSearchRequiresContainedRepositoryDomain(t *testing.T) {
	stack := newViewStack(t)
	view := &requestView{sourceRepoPrefix: "repo"}
	require.True(t, stack.srv.sourceSearchDomainContained(view, ResolvedScope{RepoAllow: map[string]bool{"repo": true}}))
	require.False(t, stack.srv.sourceSearchDomainContained(view, ResolvedScope{RepoAllow: map[string]bool{"repo": true, "other": true}}), "selected checkout cannot prove a broader search corpus")
	require.False(t, stack.srv.sourceSearchDomainContained(view, ResolvedScope{RepoAllow: map[string]bool{"other": true}}))
	require.False(t, stack.srv.sourceSearchDomainContained(view, ResolvedScope{}), "unscoped multi-repository search must retain all repositories")
}

func TestSourceNameSearchPendingUsesCurrentDeclarations(t *testing.T) {
	stack := newViewStack(t)
	require.NoError(t, os.WriteFile(filepath.Join(stack.worktreeRoot, "keep.go"), []byte("package repo\nfunc RenamedKeeper() {}\n"), 0644))
	require.NoError(t, os.WriteFile(filepath.Join(stack.worktreeRoot, "new.go"), []byte("package repo\nfunc KeeperAdded() {}\n"), 0644))
	routeViewCheckout(t, stack.store, stack.graphID, stack.commit, stack.dirty, store_sqlite.RoutePending)
	stack.srv.freshnessWaiter = &fakeFreshnessWaiter{answer: func(int, string, string) (*indexer.CheckoutRefreshTicket, error) {
		t.Fatal("declaration lookup waited for unrelated relationship publication")
		return nil, nil
	}}
	res, err := stack.callHandler(t, stack.worktreeRoot, "search_symbols", freshArgs(map[string]any{"query": "Keeper", "query_class": "symbol", requireExactArgName: true}, time.Second), stack.srv.handleSearchSymbols)
	require.NoError(t, err)
	require.False(t, res.IsError, viewResultText(t, res))
	var answer struct {
		Results []map[string]any `json:"results"`
		Ranking string           `json:"ranking"`
	}
	require.NoError(t, json.Unmarshal([]byte(viewResultText(t, res)), &answer))
	require.Equal(t, "lexical_name", answer.Ranking)
	require.Len(t, answer.Results, 2)
	require.Equal(t, "RenamedKeeper", answer.Results[0]["name"])
	require.Equal(t, "repo/keep.go::RenamedKeeper", answer.Results[0]["id"])
	require.Equal(t, "KeeperAdded", answer.Results[1]["name"])
	for _, result := range answer.Results {
		require.NotContains(t, result, "fan_in")
		require.NotContains(t, result, "fan_out")
	}
	rider := resultFreshness(t, res)
	require.Equal(t, true, rider["fresh"])
	require.Equal(t, "declarations", rider["freshness_scope"])
	require.Equal(t, "pending", rider["graph_freshness"])
	require.NoError(t, os.Remove(filepath.Join(stack.worktreeRoot, "keep.go")))
	res, err = stack.callHandler(t, stack.worktreeRoot, "search_symbols", freshArgs(map[string]any{"query": "RenamedKeeper", "query_class": "symbol", requireExactArgName: true}, time.Second), stack.srv.handleSearchSymbols)
	require.NoError(t, err)
	require.False(t, res.IsError, viewResultText(t, res))
	require.NoError(t, json.Unmarshal([]byte(viewResultText(t, res)), &answer))
	require.Empty(t, answer.Results, "disk deletion must remove the current declaration")
}

func TestSourceNameSearchQualifiedNamePrefilter(t *testing.T) {
	srv, root := setupTestServer(t)
	require.NoError(t, os.WriteFile(filepath.Join(root, "client.go"), []byte("\xef\xbb\xbfpackage main\ntype Client struct{}\nfunc (c Client) Send() {}\n"), 0644))
	view := &requestView{sourceScope: "declarations", viewRoot: root}
	ctx := withRequestView(context.Background(), view)
	req := mcplib.CallToolRequest{}
	req.Params.Arguments = map[string]any{"query": "Client.Send", "path": "client.go"}
	res, err := srv.handleSourceSearchSymbols(ctx, req, view, "Client.Send", fieldQuery{}, ResolvedScope{})
	require.NoError(t, err)
	require.False(t, res.IsError, viewResultText(t, res))
	require.Contains(t, viewResultText(t, res), "client.go::Client.Send")
}

func TestSourceNameSearchRetainsGraphDispatchForGraphRequirements(t *testing.T) {
	stack := newViewStack(t)
	ctx := WithSessionCWD(WithSessionID(context.Background(), viewTestSession), stack.worktreeRoot)
	resolve := func(args map[string]any, capabilities capabilityRequest) *requestView {
		t.Helper()
		req := mcplib.CallToolRequest{}
		req.Params.Arguments = args
		view, err := stack.srv.resolveSourceRequestView(ctx, graphview.Selector{Kind: graphview.SelectorAuto}, &req, "search_symbols", requestFreshness{}, capabilities)
		require.NoError(t, err)
		return view
	}
	require.Nil(t, resolve(map[string]any{"query": "Keeper"}, capabilityRequest{}), "stable global lookup retains the warm indexed route")
	routeViewCheckout(t, stack.store, stack.graphID, stack.commit, stack.dirty, store_sqlite.RoutePending)
	require.NotNil(t, resolve(map[string]any{"query": "Keeper"}, capabilityRequest{}))
	require.Nil(t, resolve(map[string]any{"query": "how request validation works", "query_class": "concept"}, capabilityRequest{}))
	require.Nil(t, resolve(map[string]any{"query": "Keeper", "assist": "deep"}, capabilityRequest{}))
	require.Nil(t, resolve(map[string]any{"query": "Keeper", "assist": "on"}, capabilityRequest{}))
	require.Nil(t, resolve(map[string]any{"query": "how request validation works", "query_class": "symbol"}, capabilityRequest{}), "pinning symbol class cannot turn a concept into literal declaration lookup")
	require.Nil(t, resolve(map[string]any{"query": "Keeper"}, capabilityRequest{required: []graphview.CapabilityID{graphview.CapSyntaxGraph}}))
	require.Nil(t, resolve(map[string]any{"query": "Keeper"}, capabilityRequest{requireComplete: true}))
}
