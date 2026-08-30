package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/daemon"
)

func oversizedViewGlob() string {
	return strings.Repeat("segment/", maxGlobSegments) + "**"
}

func TestBoundedInputsAreRejectedBeforeColdRefResolution(t *testing.T) {
	stack := newRefStack(t)
	missingRef := refSelector("git_ref", "refs/heads/does-not-exist")

	for _, tc := range []struct {
		name string
		tool string
		args map[string]any
	}{
		{
			name: "fidelity policy",
			tool: "read_file",
			args: map[string]any{
				"path":           "repo/edit.go",
				"fidelity_globs": oversizedViewGlob() + ":omit,**:full",
			},
		},
		{
			name: "file scan glob",
			tool: "find_files",
			args: map[string]any{"glob": oversizedViewGlob()},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ran := false
			res, err := stack.call(t, tc.tool, missingRef, tc.args,
				func(context.Context, mcplib.CallToolRequest) (*mcplib.CallToolResult, error) {
					ran = true
					return mcplib.NewToolResultText(`{"unexpected":true}`), nil
				})
			require.NoError(t, err)
			require.True(t, res.IsError)
			require.Contains(t, viewResultText(t, res), "too large")
			require.NotContains(t, viewResultText(t, res), "ref_not_available_locally",
				"view/ref resolution must not run before bounded-input admission")
			require.False(t, ran)
		})
	}
}

func TestFacadeBoundedInputsUseOptionsBeforeViewResolution(t *testing.T) {
	stack := newRefStack(t)
	for _, tc := range []struct {
		name      string
		operation string
		option    string
		value     string
	}{
		{name: "read fidelity", operation: "file", option: "fidelity_globs", value: oversizedViewGlob() + ":omit"},
		{name: "search files", operation: "files", option: "glob", value: oversizedViewGlob()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tool := "read"
			if tc.operation == "files" {
				tool = "search"
			}
			req := mcplib.CallToolRequest{}
			req.Params.Name = tool
			req.Params.Arguments = map[string]any{
				"operation": tc.operation,
				"options":   map[string]any{tc.option: tc.value},
			}
			require.ErrorContains(t, stack.srv.validateRequestBeforeView(&req), "too large")
		})
	}
}

func TestRefViewNeverComposesSessionBuffers(t *testing.T) {
	stack := newRefStack(t)
	manager := daemon.NewOverlayManager(time.Hour)
	stack.srv.SetOverlayManager(manager)
	require.NoError(t, manager.RegisterWithID(refTestSession, ""))
	require.NoError(t, manager.Push(refTestSession, daemon.OverlayFile{
		Path:    "repo/edit.go",
		Content: "package repo\n\nfunc RefBufferMustNotAppear() {}\n",
	}, nil))

	res, err := stack.call(t, "get_symbol", refSelector("git_ref", "refs/heads/feature"), nil,
		func(ctx context.Context, _ mcplib.CallToolRequest) (*mcplib.CallToolResult, error) {
			if OverlayViewFromContext(ctx) != nil {
				return nil, errors.New("a committed ref view composed a session buffer")
			}
			if hasNode(stack.srv.readerFor(ctx), "repo/edit.go::RefBufferMustNotAppear") {
				return nil, errors.New("a session-buffer symbol leaked into a committed ref view")
			}
			return mcplib.NewToolResultText(`{"ok":true}`), nil
		})
	require.NoError(t, err)
	require.False(t, res.IsError, viewResultText(t, res))
}

func TestRefEditingContextSupportsBranchOnlyFileWithoutCanonicalSelfHeal(t *testing.T) {
	stack := newRefStack(t)
	res, err := stack.call(t, "get_editing_context", refSelector("git_ref", "refs/heads/feature"),
		map[string]any{"path": "repo/added.go", "compress_bodies": true},
		stack.srv.handleGetEditingContext)
	require.NoError(t, err)
	require.False(t, res.IsError, viewResultText(t, res))
	text := viewResultText(t, res)
	require.Contains(t, text, "Fresh")
	require.Contains(t, text, "source_compressed")
}

func makeCanonicalGoExtractorStale(t *testing.T, stack *refStack) {
	t.Helper()
	state, found, err := stack.store.GetRepoIndexState(refTestPrefix)
	require.NoError(t, err)
	require.True(t, found)
	versions, err := json.Marshal(map[string]int{
		"_post_extraction_policy": 2,
		"go":                      1,
	})
	require.NoError(t, err)
	state.ExtractorVersions = string(versions)
	require.NoError(t, stack.store.SetRepoIndexState(state))
}

func requireNoCanonicalFreshness(t *testing.T, res *mcplib.CallToolResult) {
	t.Helper()
	freshness := resultFreshness(t, res)
	for _, key := range []string{
		"stale", "missing", "stale_files", "missing_files",
		"extractor_stale_langs", "extractor_stale_hint", "worktree_mismatch",
	} {
		require.NotContains(t, freshness, key,
			"a routed response inherited canonical freshness: %+v", freshness)
	}
}

func TestRefResponsesSuppressCanonicalFreshnessAndEditingContextSelfHeal(t *testing.T) {
	stack := newRefStack(t)
	makeCanonicalGoExtractorStale(t, stack)

	canonicalPath := filepath.Join(stack.repo, "edit.go")
	future := time.Now().Add(2 * time.Second)
	require.NoError(t, os.Chtimes(canonicalPath, future, future))
	owner, _ := stack.srv.multiIndexer.IndexerForFile(canonicalPath)
	require.NotNil(t, owner)
	require.True(t, owner.IsTrackedStale("edit.go"))
	canonical := stack.srv.freshnessRiderFor(context.Background(), "read_file",
		freshReq(map[string]any{"path": "repo/edit.go"}))
	require.Equal(t, true, canonical["stale"])
	require.Contains(t, canonical["extractor_stale_langs"], "go")

	ref := refSelector("git_ref", "refs/heads/feature")
	readResult, err := stack.readFile(t, ref, "repo/edit.go")
	require.NoError(t, err)
	requireNoCanonicalFreshness(t, readResult)

	searchResult, err := stack.call(t, "search_symbols", ref,
		map[string]any{"query": "New"},
		func(context.Context, mcplib.CallToolRequest) (*mcplib.CallToolResult, error) {
			return mcplib.NewToolResultText(`{"results":[{"file":"repo/edit.go","name":"New"}]}`), nil
		})
	require.NoError(t, err)
	requireNoCanonicalFreshness(t, searchResult)

	editingResult, err := stack.call(t, "get_editing_context", ref,
		map[string]any{"path": "repo/edit.go", "compress_bodies": true},
		stack.srv.handleGetEditingContext)
	require.NoError(t, err)
	require.False(t, editingResult.IsError, viewResultText(t, editingResult))
	requireNoCanonicalFreshness(t, editingResult)
	require.True(t, owner.IsTrackedStale("edit.go"),
		"routed editing context must not self-heal/reindex the canonical checkout")
}

func TestSelectedWorktreeSuppressesPrimaryCheckoutFreshness(t *testing.T) {
	stack := newRefStack(t)
	primaryPath := filepath.Join(stack.repo, "keep.go")
	future := time.Now().Add(time.Hour)
	require.NoError(t, os.Chtimes(primaryPath, future, future))
	owner, _ := stack.srv.multiIndexer.IndexerForFile(primaryPath)
	require.NotNil(t, owner)
	require.True(t, owner.IsTrackedStale("keep.go"))

	canonical := stack.srv.freshnessRiderFor(context.Background(), "read_file",
		freshReq(map[string]any{"path": "repo/keep.go"}))
	require.Equal(t, true, canonical["stale"])

	ctx := withRequestView(context.Background(), &requestView{
		reader:   stack.store,
		viewRoot: filepath.Join(t.TempDir(), "selected-worktree"),
	})
	require.Nil(t, stack.srv.freshnessRiderFor(ctx, "read_file",
		freshReq(map[string]any{"path": "repo/keep.go"})))
	list := mcplib.NewToolResultText(`{"results":[{"file":"repo/keep.go","name":"Keeper"}]}`)
	require.Equal(t, list, stack.srv.decorateListResultWithFreshness(ctx, list))
	require.Empty(t, stack.srv.ensureFreshForRequest(ctx, []string{"repo/keep.go"}))
	require.True(t, owner.IsTrackedStale("keep.go"),
		"a worktree response must not self-heal the primary checkout")
}

func TestRoutedWorktreeBufferUsesSelectedRootForCanonicalizationBaseSHAAndRawLookup(t *testing.T) {
	stack := newWorktreeSearchStack(t)
	stack.divergeWorktree(t)

	worktreePath := filepath.Join(stack.worktree, "keep.go")
	worktreeBytes, err := os.ReadFile(worktreePath)
	require.NoError(t, err)
	primaryBytes, err := os.ReadFile(filepath.Join(stack.primary, "keep.go"))
	require.NoError(t, err)
	require.NotEqual(t, gitBlobSHA(primaryBytes), gitBlobSHA(worktreeBytes),
		"the BaseSHA fixture must distinguish the selected checkout from canonical")

	manager := daemon.NewOverlayManager(time.Hour)
	stack.srv.SetOverlayManager(manager)
	require.NoError(t, manager.RegisterWithID(wtSearchSession, ""))
	require.NoError(t, manager.Push(wtSearchSession, daemon.OverlayFile{
		Path:    refTestPrefix + "/keep.go",
		BaseSHA: gitBlobSHA(worktreeBytes),
		Content: "package repo\n\nfunc BufferedWorktreeOnly() {}\n",
	}, nil))

	stack.callFrom(t, stack.worktree, "get_symbol",
		map[string]any{"id": refTestPrefix + "/keep.go::BufferedWorktreeOnly"},
		func(ctx context.Context, _ mcplib.CallToolRequest) (*mcplib.CallToolResult, error) {
			if OverlayViewFromContext(ctx) == nil {
				return nil, errors.New("the selected worktree buffer produced no overlay view")
			}
			if !hasNode(stack.srv.readerFor(ctx), "repo/keep.go::BufferedWorktreeOnly") {
				return nil, errors.New("the selected worktree buffer was not parsed into the routed graph")
			}
			if content, ok := stack.srv.overlayContentFor(ctx, worktreePath); !ok || !strings.Contains(content, "BufferedWorktreeOnly") {
				return nil, errors.New("raw overlay lookup did not use the selected worktree root")
			}
			if _, ok := stack.srv.overlayContentFor(ctx, filepath.Join(stack.primary, "keep.go")); ok {
				return nil, errors.New("raw overlay lookup also matched the canonical sibling checkout")
			}
			return mcplib.NewToolResultText(`{"ok":true}`), nil
		})
}

func TestRoutedWorktreeRejectsAbsolutePathFromSiblingCheckout(t *testing.T) {
	stack := newWorktreeSearchStack(t)
	req := mcplib.CallToolRequest{}
	req.Params.Name = "read_file"
	req.Params.Arguments = map[string]any{"path": filepath.Join(stack.primary, "keep.go")}
	ctx := WithSessionCWD(WithSessionID(context.Background(), wtSearchSession), stack.worktree)
	res, err := stack.srv.wrapToolHandler(stack.srv.handleReadFile)(ctx, req)
	require.NoError(t, err)
	require.True(t, res.IsError)
	require.Contains(t, viewResultText(t, res), "different checkout than the selected view")
}

func BenchmarkValidateRequestBeforeView(b *testing.B) {
	req := mcplib.CallToolRequest{}
	req.Params.Name = "read_file"
	req.Params.Arguments = map[string]any{
		"path":           "repo/internal/service.go",
		"fidelity_globs": "internal/**:full,**/*_test.go:omit,**:compress",
	}
	srv := &Server{}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if err := srv.validateRequestBeforeView(&req); err != nil {
			b.Fatal(err)
		}
	}
}
