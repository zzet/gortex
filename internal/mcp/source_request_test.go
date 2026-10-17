package mcp

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/daemon"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/graphview"
	"github.com/zzet/gortex/internal/indexer"
)

func TestSourceReadDoesNotWaitForRelationshipPublication(t *testing.T) {
	stack := newViewStack(t)
	path := filepath.Join(stack.worktreeRoot, "keep.go")
	require.NoError(t, os.WriteFile(path, []byte("package current\n"), 0644))
	routeViewCheckout(t, stack.store, stack.graphID, stack.commit, stack.dirty, store_sqlite.RoutePending)
	stack.srv.freshnessWaiter = &fakeFreshnessWaiter{answer: func(int, string, string) (*indexer.CheckoutRefreshTicket, error) {
		t.Fatal("source read waited for graph")
		return nil, nil
	}}
	args := freshArgs(map[string]any{"path": path, requireExactArgName: true}, time.Second)
	res, err := stack.callHandler(t, stack.worktreeRoot, "read_file", args, stack.srv.handleReadFile)
	require.NoError(t, err)
	require.False(t, res.IsError, viewResultText(t, res))
	require.Contains(t, viewResultText(t, res), "package current")
	rider := resultFreshness(t, res)
	require.Equal(t, true, rider["fresh"])
	require.Equal(t, "file", rider["freshness_scope"])
	require.Equal(t, "pending", rider["graph_freshness"])
}

func TestSourceCarrierPreservesImmutableAndGraphRequirements(t *testing.T) {
	stack := newViewStack(t)
	req := mcplib.CallToolRequest{}
	req.Params.Arguments = map[string]any{"path": "keep.go"}
	ctx := WithSessionCWD(WithSessionID(context.Background(), viewTestSession), stack.worktreeRoot)
	for _, selector := range []graphview.Selector{{Kind: graphview.SelectorBase}, {Kind: graphview.SelectorCommit}} {
		view, err := stack.srv.resolveSourceRequestView(ctx, selector, &req, "read_file", requestFreshness{}, capabilityRequest{})
		require.NoError(t, err)
		require.Nil(t, view)
	}
	view, err := stack.srv.resolveSourceRequestView(ctx, graphview.Selector{Kind: graphview.SelectorAuto}, &req, "read_file", requestFreshness{}, capabilityRequest{required: []graphview.CapabilityID{graphview.CapSyntaxGraph}})
	require.NoError(t, err)
	require.Nil(t, view)
}

func TestSourceReadBindsOverlayBaseToVerifiedDiskBytes(t *testing.T) {
	srv, root := setupTestServer(t)
	path := filepath.Join(root, "base.go")
	old := []byte("package old\n")
	require.NoError(t, os.WriteFile(path, old, 0644))
	ctx := withRequestView(context.Background(), &requestView{sourceScope: "file", viewRoot: root})
	ctx = withOverlayRequestSnapshot(ctx, &overlayRequestSnapshot{canonical: true, files: []daemon.OverlayFile{{Path: path, Content: "package editor\n", BaseSHA: gitBlobSHA(old)}}})
	srv.physicalEvidenceOverride = func(path string) ([]byte, physicalReadEvidence, error) {
		require.NoError(t, os.WriteFile(path, []byte("package new\n"), 0644))
		return readPhysicalFileEvidence(path)
	}
	req := mcplib.CallToolRequest{}
	req.Params.Arguments = map[string]any{"path": path}
	res, err := srv.handleReadFile(ctx, req)
	require.NoError(t, err)
	require.True(t, res.IsError)
	require.Contains(t, viewResultText(t, res), "overlay drift")
}

func TestSourceReadCurrentOverlayAdditionAndDeletion(t *testing.T) {
	srv, root := setupTestServer(t)
	path := filepath.Join(root, "new.go")
	for _, deleted := range []bool{false, true} {
		ctx := withRequestView(context.Background(), &requestView{sourceScope: "file", viewRoot: root})
		ctx = withOverlayRequestSnapshot(ctx, &overlayRequestSnapshot{canonical: true, files: []daemon.OverlayFile{{Path: path, Content: "package editor\n", Deleted: deleted}}})
		req := mcplib.CallToolRequest{}
		req.Params.Arguments = map[string]any{"path": path}
		res, err := srv.handleReadFile(ctx, req)
		require.NoError(t, err)
		require.Equal(t, deleted, res.IsError, viewResultText(t, res))
		if !deleted {
			require.Contains(t, viewResultText(t, res), "package editor")
		}
	}
}

func TestSourceCarrierNormalizesFacadePolicyArguments(t *testing.T) {
	stack := newViewStack(t)
	ctx := WithSessionCWD(WithSessionID(context.Background(), viewTestSession), stack.worktreeRoot)
	req := mcplib.CallToolRequest{}
	req.Params.Name = "read"
	req.Params.Arguments = map[string]any{"operation": "file", "target": map[string]any{"file": "keep.go"}, "options": map[string]any{"compress_bodies": true, "keep": "Keeper"}}
	view, err := stack.srv.resolveSourceRequestView(ctx, graphview.Selector{Kind: graphview.SelectorAuto}, &req, "read_file", requestFreshness{}, capabilityRequest{})
	require.NoError(t, err)
	require.Nil(t, view, "facade keep predicate must retain indexed symbol selection")
	req.Params.Name = "search"
	req.Params.Arguments = map[string]any{"operation": "text", "query": "Keeper", "options": map[string]any{"path": "keep.go"}}
	view, err = stack.srv.resolveSourceRequestView(ctx, graphview.Selector{Kind: graphview.SelectorAuto}, &req, "search_text", requestFreshness{}, capabilityRequest{})
	require.NoError(t, err)
	require.NotNil(t, view, "facade path scope must select scoped scanner")
	require.Equal(t, "text", view.sourceScope)
}

func TestSourceCarrierRejectsReplacedPhysicalRoot(t *testing.T) {
	stack := newViewStack(t)
	ctx := WithSessionCWD(WithSessionID(context.Background(), viewTestSession), stack.worktreeRoot)
	req := mcplib.CallToolRequest{}
	req.Params.Arguments = map[string]any{"path": "keep.go"}
	view, err := stack.srv.resolveSourceRequestView(ctx, graphview.Selector{Kind: graphview.SelectorAuto}, &req, "read_file", requestFreshness{}, capabilityRequest{})
	require.NoError(t, err)
	require.NotNil(t, view)
	displaced := stack.worktreeRoot + "-displaced"
	require.NoError(t, os.Rename(stack.worktreeRoot, displaced))
	t.Cleanup(func() { _ = os.RemoveAll(stack.worktreeRoot); _ = os.Rename(displaced, stack.worktreeRoot) })
	require.NoError(t, os.MkdirAll(stack.worktreeRoot, 0755))
	require.ErrorContains(t, stack.srv.validateSourceCheckoutIdentity(ctx, view), "physical root changed")
}
