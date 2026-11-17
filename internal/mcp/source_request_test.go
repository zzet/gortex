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

// A worktree session reads a sibling repository of its own workspace the way a
// primary-checkout session does: the source lane, which proves bytes of its own
// checkout only, steps aside instead of refusing the path.
func TestWorktreeSessionReadsSiblingRepoFile(t *testing.T) {
	stack := newViewStackWithSibling(t)
	ctx := WithSessionCWD(WithSessionID(context.Background(), viewTestSession), stack.worktreeRoot)
	siblingFile := filepath.Join(stack.siblingRoot, "sibling.go")

	res, err := stack.callHandler(t, stack.worktreeRoot, "read_file", map[string]any{"path": siblingFile}, stack.srv.handleReadFile)
	require.NoError(t, err)
	require.False(t, res.IsError, viewResultText(t, res))
	require.Contains(t, viewResultText(t, res), "func Sibling()")

	res, err = stack.callHandler(t, stack.worktreeRoot, "read_file", map[string]any{"path": "sibling/sibling.go"}, stack.srv.handleReadFile)
	require.NoError(t, err)
	require.False(t, res.IsError, viewResultText(t, res))
	require.Contains(t, viewResultText(t, res), "func Sibling()")

	facade := mcplib.CallToolRequest{}
	facade.Params.Name = "read"
	facade.Params.Arguments = map[string]any{"operation": "file", "target": map[string]any{"file": siblingFile}}
	view, err := stack.srv.resolveSourceRequestView(ctx, graphview.Selector{Kind: graphview.SelectorAuto}, &facade, "read_file", requestFreshness{}, capabilityRequest{})
	require.NoError(t, err)
	require.Nil(t, view, "a facade read of a sibling repo must leave the source lane")

	own := mcplib.CallToolRequest{}
	own.Params.Arguments = map[string]any{"path": filepath.Join(stack.worktreeRoot, "keep.go")}
	view, err = stack.srv.resolveSourceRequestView(ctx, graphview.Selector{Kind: graphview.SelectorAuto}, &own, "read_file", requestFreshness{}, capabilityRequest{})
	require.NoError(t, err)
	require.NotNil(t, view, "a read of the worktree's own file keeps the source lane")

	require.True(t, stack.srv.readsSiblingRepo(ctx, siblingFile, stack.worktreeRoot, "repo"))
	require.False(t, stack.srv.readsSiblingRepo(ctx, filepath.Join(stack.repoRoot, "keep.go"), stack.worktreeRoot, ""),
		"a checkout with no known prefix cannot tell its primary from a sibling")
	require.False(t, stack.srv.readsSiblingRepo(ctx, "repo/keep.go", stack.worktreeRoot, "repo"),
		"its own prefix is not a sibling")
}

// Ownership follows the physical file: an alias beneath a sibling that
// resolves into the worktree family's primary keeps the source-lane refusal,
// the same as naming the primary file directly.
func TestWorktreeSessionSiblingAliasIntoPrimaryStaysRefused(t *testing.T) {
	stack := newViewStackWithSibling(t)
	ctx := WithSessionCWD(WithSessionID(context.Background(), viewTestSession), stack.worktreeRoot)
	require.NoError(t, os.WriteFile(filepath.Join(stack.worktreeRoot, "keep.go"), []byte("package repo\n\nfunc WorktreeMarker() {}\n"), 0o644))
	primaryAlias := filepath.Join(stack.siblingRoot, "primary-alias.go")
	require.NoError(t, os.Symlink(filepath.Join(stack.repoRoot, "keep.go"), primaryAlias))
	siblingAlias := filepath.Join(stack.siblingRoot, "sibling-alias.go")
	require.NoError(t, os.Symlink(filepath.Join(stack.siblingRoot, "sibling.go"), siblingAlias))

	for name, path := range map[string]string{
		"primary path":             filepath.Join(stack.repoRoot, "keep.go"),
		"primary alias (abs)":      primaryAlias,
		"primary alias (prefixed)": "sibling/primary-alias.go",
	} {
		res, err := stack.callHandler(t, stack.worktreeRoot, "read_file", map[string]any{"path": path}, stack.srv.handleReadFile)
		require.NoError(t, err, name)
		require.True(t, res.IsError, "%s: %s", name, viewResultText(t, res))
		require.Contains(t, viewResultText(t, res), "outside the selected checkout", name)
		require.NotContains(t, viewResultText(t, res), "func Keeper()", name)
	}

	for name, path := range map[string]string{
		"sibling path":  filepath.Join(stack.siblingRoot, "sibling.go"),
		"sibling alias": siblingAlias,
	} {
		res, err := stack.callHandler(t, stack.worktreeRoot, "read_file", map[string]any{"path": path}, stack.srv.handleReadFile)
		require.NoError(t, err, name)
		require.False(t, res.IsError, "%s: %s", name, viewResultText(t, res))
		require.Contains(t, viewResultText(t, res), "func Sibling()", name)
	}

	res, err := stack.callHandler(t, stack.worktreeRoot, "read_file", map[string]any{"path": filepath.Join(stack.worktreeRoot, "keep.go")}, stack.srv.handleReadFile)
	require.NoError(t, err)
	require.False(t, res.IsError, viewResultText(t, res))
	require.Contains(t, viewResultText(t, res), "func WorktreeMarker()")

	req := mcplib.CallToolRequest{}
	req.Params.Arguments = map[string]any{"path": primaryAlias}
	view, err := stack.srv.resolveSourceRequestView(ctx, graphview.Selector{Kind: graphview.SelectorWorktree, CheckoutID: viewTestWorktree}, &req, "read_file", requestFreshness{}, capabilityRequest{})
	require.NoError(t, err)
	require.NotNil(t, view, "an explicit worktree selector keeps the checkout boundary")
}

// The sibling exception stops at the session workspace and at an explicit
// worktree selector: both still refuse a path outside the checkout.
func TestWorktreeSessionSiblingReadStaysInsideWorkspace(t *testing.T) {
	stack := newViewStackWithSibling(t)
	ctx := WithSessionCWD(WithSessionID(context.Background(), viewTestSession), stack.worktreeRoot)

	res, err := stack.callHandler(t, stack.worktreeRoot, "read_file", map[string]any{"path": filepath.Join(stack.otherRoot, "other.go")}, stack.srv.handleReadFile)
	require.NoError(t, err)
	require.True(t, res.IsError, "a repo in another workspace must stay refused")
	require.Contains(t, viewResultText(t, res), "outside the selected checkout")

	req := mcplib.CallToolRequest{}
	req.Params.Arguments = map[string]any{"path": filepath.Join(stack.siblingRoot, "sibling.go")}
	view, err := stack.srv.resolveSourceRequestView(ctx, graphview.Selector{Kind: graphview.SelectorWorktree, CheckoutID: viewTestWorktree}, &req, "read_file", requestFreshness{}, capabilityRequest{})
	require.NoError(t, err)
	require.NotNil(t, view, "an explicit worktree selector keeps the checkout boundary")
}
