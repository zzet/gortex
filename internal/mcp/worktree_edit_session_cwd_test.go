package mcp

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/graphview"
)

// Tests for the 2026-09-19 wrong-checkout edit incident: a session whose cwd
// sits inside a linked worktree of a tracked repository must never have its
// mutations served from the base corpus. The base corpus path resolvers
// anchor prefixed and repo-relative paths against the MAIN checkout's root,
// and the worktreeRootedPath existence heuristic then leaves the file there
// when it exists in both checkouts — silently editing the wrong copy.
//
// The binding happy path (cwd inside a ready+automatic checkout, route
// materializable, bytes land in the working copy) is already pinned
// end-to-end by TestWorktreeMutationCoordinatorEndToEnd's "cwd" subtest.
// These tests pin the NEW defence: when the route cannot serve, a mutation
// refuses loudly (view_building) instead of degrading to base, and a read
// degrades only with a labelled rider.

// retireRoute flips the worktree checkout's route to Retired with both
// generation slots zeroed: the checkout row itself stays ready+automatic,
// but no generation stack can serve it anymore.
func retireRoute(t *testing.T, stack *viewStack) {
	t.Helper()
	require.NoError(t, stack.srv.materializer.Catalog.UpsertCheckoutRoute(
		context.Background(), store_sqlite.CheckoutRoute{
			CheckoutID: viewTestWorktree,
			GraphID:    stack.graphID,
			State:      store_sqlite.RouteRetired,
		}))
}

func editFileViaMiddleware(stack *viewStack, cwd string, args map[string]any) (*mcplib.CallToolResult, error) {
	req := mcplib.CallToolRequest{}
	req.Params.Name = "edit_file"
	req.Params.Arguments = args
	ctx := WithAuthorizedToolCall(
		WithSessionCWD(WithSessionID(context.Background(), viewTestSession), cwd),
		"edit_file")
	return stack.srv.wrapToolHandler(stack.srv.handleEditFile)(ctx, req)
}

func TestCWDBindingRouteNotReadyMutationsRefuseLoudly(t *testing.T) {
	stack := newViewStack(t)
	retireRoute(t, stack)

	// Divergent exists-in-both: the corpus root owns "func Old() {}" while
	// the worktree carries a different body. Before the defence this state
	// silently wrote the main copy.
	require.NoError(t, os.WriteFile(filepath.Join(stack.worktreeRoot, "edit.go"),
		[]byte("package repo\n\n// worktree copy\n"), 0o644))

	result, err := editFileViaMiddleware(stack, stack.worktreeRoot, map[string]any{
		"path":       "repo/edit.go",
		"old_string": "func Old() {}",
		"new_string": "func Never() {}",
	})
	require.NoError(t, err)
	require.NotNil(t, result)
	require.True(t, result.IsError,
		"a mutation on an unrouted checkout must refuse, not silently use base")
	text := viewResultText(t, result)
	require.Contains(t, text, graphview.CodeViewBuilding,
		"refusal must carry view_building, got: %s", text)

	// And crucially: no bytes moved anywhere.
	mainAfter, readErr := os.ReadFile(filepath.Join(stack.repoRoot, "edit.go"))
	require.NoError(t, readErr)
	require.NotContains(t, string(mainAfter), "func Never() {}",
		"the refused edit still wrote the MAIN copy")
	worktreeAfter, readErr := os.ReadFile(filepath.Join(stack.worktreeRoot, "edit.go"))
	require.NoError(t, readErr)
	require.NotContains(t, string(worktreeAfter), "func Never() {}",
		"the refused edit still wrote the worktree copy")
}

// TestCWDBindingRouteNotReadyReadFileIsLabeled pins the incident's read
// twin: a read_file with a repo-prefixed path from a worktree-anchored
// session whose route cannot serve must degrade to base WITH the rider
// (visible degradation), and the rider must say which checkout the session
// actually wanted so the client can reconstruct the misroute.
func TestCWDBindingRouteNotReadyReadFileIsLabeled(t *testing.T) {
	stack := newViewStack(t)
	retireRoute(t, stack)

	req := mcplib.CallToolRequest{}
	req.Params.Arguments = map[string]any{"path": "repo/edit.go"}
	ctx := WithSessionCWD(WithSessionID(context.Background(), viewTestSession), stack.worktreeRoot)
	res, err := stack.srv.wrapToolHandler(stack.srv.handleReadFile)(ctx, req)
	require.NoError(t, err)
	require.NotNil(t, res)
	require.False(t, res.IsError, "a prefixed read may degrade to base: %s", viewResultText(t, res))

	// The bytes themselves come from the base corpus (main checkout's index
	// state) — that is the declared degradation. What must NOT happen is an
	// exact-looking answer: the rider says so.
	rider := resultFreshness(t, res)
	require.NotNil(t, rider, "degraded read_file must say so on the rider")
	require.Equal(t, "worktree:"+viewTestWorktree, rider["requested_view"],
		"the rider must name the checkout the session actually bound, not just auto")
	require.Equal(t, false, rider["exact"])
	require.Equal(t, graphview.CodeViewBuilding, rider["fallback_reason"])
	require.Equal(t, viewTestWorktree, rider["checkout_id"],
		"rider must name the checkout the cwd wanted, so the client sees the misroute")
	require.NotEqual(t, "", rider["graph_id"],
		"single-family fallback names the primary base graph it answered from")
}

// TestCWDBindingRouteNotReadySoleRepoMutationRefusesLoudly pins the same
// defence in a single-repo topology, where a bare (unprefixed) path anchors
// directly via resolveFilePath's soleTrackedRepo branch instead of being
// refused as ambiguous. The route-not-ready refusal happens in the outer
// middleware before that branch is ever reached, so the sole-repo shortcut
// must not bypass it.
func TestCWDBindingRouteNotReadySoleRepoMutationRefusesLoudly(t *testing.T) {
	stack := newViewStackWithRepos(t, false)
	retireRoute(t, stack)

	require.NoError(t, os.WriteFile(filepath.Join(stack.worktreeRoot, "edit.go"),
		[]byte("package repo\n\n// worktree copy\n"), 0o644))

	result, err := editFileViaMiddleware(stack, stack.worktreeRoot, map[string]any{
		"path":       "edit.go",
		"old_string": "func Old() {}",
		"new_string": "func Never() {}",
	})
	require.NoError(t, err)
	require.NotNil(t, result)
	require.True(t, result.IsError,
		"a mutation on an unrouted sole-repo checkout must refuse, not silently use base via the soleTrackedRepo shortcut")
	text := viewResultText(t, result)
	require.Contains(t, text, graphview.CodeViewBuilding,
		"refusal must carry view_building, got: %s", text)

	mainAfter, readErr := os.ReadFile(filepath.Join(stack.repoRoot, "edit.go"))
	require.NoError(t, readErr)
	require.NotContains(t, string(mainAfter), "func Never() {}",
		"the refused edit still wrote the MAIN copy")
	worktreeAfter, readErr := os.ReadFile(filepath.Join(stack.worktreeRoot, "edit.go"))
	require.NoError(t, readErr)
	require.NotContains(t, string(worktreeAfter), "func Never() {}",
		"the refused edit still wrote the worktree copy")
}

func batchEditViaMiddleware(stack *viewStack, cwd string, edits []map[string]any) (*mcplib.CallToolResult, error) {
	req := mcplib.CallToolRequest{}
	req.Params.Name = "batch_edit"
	req.Params.Arguments = map[string]any{"edits": edits}
	ctx := WithAuthorizedToolCall(
		WithSessionCWD(WithSessionID(context.Background(), viewTestSession), cwd),
		"batch_edit")
	return stack.srv.wrapToolHandler(stack.srv.handleAtomicBatchEdit)(ctx, req)
}

// TestCWDBindingRouteNotReadyBatchEditRefusesLoudly pins the same defence for
// batch_edit, which drives handleAtomicBatchEdit (batch_transaction.go:1356)
// rather than handleEditFile directly.
func TestCWDBindingRouteNotReadyBatchEditRefusesLoudly(t *testing.T) {
	stack := newViewStack(t)
	retireRoute(t, stack)

	require.NoError(t, os.WriteFile(filepath.Join(stack.worktreeRoot, "edit.go"),
		[]byte("package repo\n\n// worktree copy\n"), 0o644))

	result, err := batchEditViaMiddleware(stack, stack.worktreeRoot, []map[string]any{
		{"op": "edit_file", "path": "repo/edit.go", "old_string": "func Old() {}", "new_string": "func Never() {}"},
	})
	require.NoError(t, err)
	require.NotNil(t, result)
	require.True(t, result.IsError,
		"a batch_edit on an unrouted checkout must refuse, not silently use base")
	text := viewResultText(t, result)
	require.Contains(t, text, graphview.CodeViewBuilding,
		"refusal must carry view_building, got: %s", text)

	mainAfter, readErr := os.ReadFile(filepath.Join(stack.repoRoot, "edit.go"))
	require.NoError(t, readErr)
	require.NotContains(t, string(mainAfter), "func Never() {}",
		"the refused batch edit still wrote the MAIN copy")
}

func writeFileViaMiddleware(stack *viewStack, cwd string, args map[string]any) (*mcplib.CallToolResult, error) {
	req := mcplib.CallToolRequest{}
	req.Params.Name = "write_file"
	req.Params.Arguments = args
	ctx := WithAuthorizedToolCall(
		WithSessionCWD(WithSessionID(context.Background(), viewTestSession), cwd),
		"write_file")
	return stack.srv.wrapToolHandler(stack.srv.handleWriteFile)(ctx, req)
}

func editSymbolViaMiddleware(stack *viewStack, cwd string, args map[string]any) (*mcplib.CallToolResult, error) {
	req := mcplib.CallToolRequest{}
	req.Params.Name = "edit_symbol"
	req.Params.Arguments = args
	ctx := WithAuthorizedToolCall(
		WithSessionCWD(WithSessionID(context.Background(), viewTestSession), cwd),
		"edit_symbol")
	return stack.srv.wrapToolHandler(stack.srv.handleEditSymbol)(ctx, req)
}

// TestCWDBindingRouteNotReadyWriteAndEditSymbolRefuseLoudly pins parity
// across the remaining source-mutating legacy tools: write_file
// (tools_fileops.go:970) and edit_symbol (tools_coding.go:2909) must refuse
// exactly like edit_file rather than falling through to base.
func TestCWDBindingRouteNotReadyWriteAndEditSymbolRefuseLoudly(t *testing.T) {
	stack := newViewStack(t)
	retireRoute(t, stack)

	t.Run("write_file", func(t *testing.T) {
		result, err := writeFileViaMiddleware(stack, stack.worktreeRoot, map[string]any{
			"path":    "repo/added.go",
			"content": "package repo\n\nfunc Never() {}\n",
		})
		require.NoError(t, err)
		require.NotNil(t, result)
		require.True(t, result.IsError,
			"a write on an unrouted checkout must refuse, not silently use base")
		text := viewResultText(t, result)
		require.Contains(t, text, graphview.CodeViewBuilding,
			"refusal must carry view_building, got: %s", text)
		_, statErr := os.Stat(filepath.Join(stack.repoRoot, "added.go"))
		require.True(t, os.IsNotExist(statErr), "the refused write still created the MAIN copy")
	})

	t.Run("edit_symbol", func(t *testing.T) {
		result, err := editSymbolViaMiddleware(stack, stack.worktreeRoot, map[string]any{
			"id":         "repo/edit.go::New",
			"old_source": "func New() {}",
			"new_source": "func Never() {}",
		})
		require.NoError(t, err)
		require.NotNil(t, result)
		require.True(t, result.IsError,
			"an edit_symbol on an unrouted checkout must refuse, not silently use base")
		text := viewResultText(t, result)
		require.Contains(t, text, graphview.CodeViewBuilding,
			"refusal must carry view_building, got: %s", text)
		mainAfter, readErr := os.ReadFile(filepath.Join(stack.repoRoot, "edit.go"))
		require.NoError(t, readErr)
		require.NotContains(t, string(mainAfter), "func Never() {}",
			"the refused edit_symbol still wrote the MAIN copy")
	})
}

// TestCWDBindingRouteNotReadyRefactorFacadeRefusesLoudly extends the same
// defence to the "refactor" facade — sourceMutatingFacades' other member
// (facade_registry.go:166), covering apply_code_action, safe_delete_symbol,
// fix_all_in_file, inline_symbol, move_symbol, and rename_symbol
// (facade_registry.go:387-390). Every prior test in this file exercises only
// the "edit" facade; nothing pinned that the same refusal reaches this
// sibling. Each of these tools is registered through s.addTool, which wraps
// it in the identical s.wrapToolHandler(handler) chain edit_file goes
// through (server.go:3241-3247), so the route-not-ready refusal fires in the
// outer middleware (resolveRequestView -> viewForSessionCWD) before any of
// these handlers run — well before argument validation, which is why
// deliberately empty/minimal args are enough here.
func TestCWDBindingRouteNotReadyRefactorFacadeRefusesLoudly(t *testing.T) {
	stack := newViewStack(t)
	retireRoute(t, stack)

	cases := []struct {
		tool    string
		args    map[string]any
		handler func(context.Context, mcplib.CallToolRequest) (*mcplib.CallToolResult, error)
	}{
		{tool: "rename_symbol", args: map[string]any{"id": "repo/edit.go::New", "new_name": "Renamed"}, handler: stack.srv.handleRenameSymbol},
		{tool: "move_symbol", args: map[string]any{"id": "repo/edit.go::New", "destination": "repo/added.go"}, handler: stack.srv.handleMoveSymbol},
		{tool: "safe_delete_symbol", args: map[string]any{"id": "repo/edit.go::New"}, handler: stack.srv.handleSafeDeleteSymbol},
		{tool: "inline_symbol", args: map[string]any{"id": "repo/edit.go::New"}, handler: stack.srv.handleInlineSymbol},
		{tool: "apply_code_action", args: map[string]any{"file": "repo/edit.go", "line": 3, "action": "quickfix"}, handler: stack.srv.handleApplyCodeAction},
		{tool: "fix_all_in_file", args: map[string]any{"path": "repo/edit.go"}, handler: stack.srv.handleFixAllInFile},
	}

	for _, tc := range cases {
		t.Run(tc.tool, func(t *testing.T) {
			req := mcplib.CallToolRequest{}
			req.Params.Name = tc.tool
			req.Params.Arguments = tc.args
			ctx := WithAuthorizedToolCall(
				WithSessionCWD(WithSessionID(context.Background(), viewTestSession), stack.worktreeRoot),
				tc.tool)
			result, err := stack.srv.wrapToolHandler(tc.handler)(ctx, req)
			require.NoError(t, err)
			require.NotNil(t, result)
			require.True(t, result.IsError,
				"a %s on an unrouted checkout must refuse, not silently use base", tc.tool)
			text := viewResultText(t, result)
			require.Contains(t, text, graphview.CodeViewBuilding,
				"refusal must carry view_building for %s, got: %s", tc.tool, text)
		})
	}

	mainAfter, readErr := os.ReadFile(filepath.Join(stack.repoRoot, "edit.go"))
	require.NoError(t, readErr)
	require.NotContains(t, string(mainAfter), "Renamed",
		"a refused refactor-facade mutation still wrote the MAIN copy")
}

// TestCWDBindingRouteNotReadyMissingMarkerFailsOpenToReadOnly pins the
// fail-open path: without WithAuthorizedToolCall, requestIsMutationFromContext
// cannot see the tool name, so viewForSessionCWD takes the read posture and
// soft-falls-back to base with an inexact rider instead of refusing outright.
// The second gate, refuseRoutedViewMutation, keys off the tool name on the
// request itself (not the context marker) and still refuses the write —
// zero bytes must move either way.
func TestCWDBindingRouteNotReadyMissingMarkerFailsOpenToReadOnly(t *testing.T) {
	stack := newViewStack(t)
	retireRoute(t, stack)

	require.NoError(t, os.WriteFile(filepath.Join(stack.worktreeRoot, "edit.go"),
		[]byte("package repo\n\n// worktree copy\n"), 0o644))

	req := mcplib.CallToolRequest{}
	req.Params.Name = "edit_file"
	req.Params.Arguments = map[string]any{
		"path":       "repo/edit.go",
		"old_string": "func Old() {}",
		"new_string": "func Never() {}",
	}
	ctx := WithSessionCWD(WithSessionID(context.Background(), viewTestSession), stack.worktreeRoot)
	result, err := stack.srv.wrapToolHandler(stack.srv.handleEditFile)(ctx, req)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.True(t, result.IsError,
		"a marker-less edit on an unrouted checkout must still refuse, via the read-only gate")
	text := viewResultText(t, result)
	require.NotContains(t, text, graphview.CodeViewBuilding,
		"no authorized-call marker means no top-level view_building refusal, got: %s", text)
	require.Contains(t, text, graphview.CodeViewReadOnly,
		"the second gate (refuseRoutedViewMutation) must catch the marker-less mutation: %s", text)

	mainAfter, readErr := os.ReadFile(filepath.Join(stack.repoRoot, "edit.go"))
	require.NoError(t, readErr)
	require.NotContains(t, string(mainAfter), "func Never() {}",
		"the fail-open path still wrote the MAIN copy")
	worktreeAfter, readErr := os.ReadFile(filepath.Join(stack.worktreeRoot, "edit.go"))
	require.NoError(t, readErr)
	require.NotContains(t, string(worktreeAfter), "func Never() {}",
		"the fail-open path still wrote the worktree copy")
}

func TestCWDBindingRouteNotReadyReadsFallBackWithRider(t *testing.T) {
	stack := newViewStack(t)
	retireRoute(t, stack)

	var reader graph.Reader
	res, err := stack.callWithView(t, stack.worktreeRoot, "get_symbol",
		nil, captureReader(stack.srv, &reader))
	require.NoError(t, err)
	require.False(t, res.IsError, "a read on an unrouted checkout may degrade to base: %s", viewResultText(t, res))
	rider := resultFreshness(t, res)
	require.NotNil(t, rider, "degraded read must say so on the rider")
	require.Equal(t, string(graphview.SelectorBase), rider["actual_view"])
	require.Equal(t, false, rider["exact"], "fallback must not claim exact")
	require.Equal(t, graphview.CodeViewBuilding, rider["fallback_reason"])
}
