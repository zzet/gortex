package mcp

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graphview"
)

func wholeFileAuthorityRequest(f *realCheckoutMutationFixture, facade bool, args map[string]any) (mcplib.CallToolRequest, func(context.Context, mcplib.CallToolRequest) (*mcplib.CallToolResult, error)) {
	req := mcplib.CallToolRequest{}
	req.Params.Name, req.Params.Arguments = "write_file", args
	handler := f.srv.wrapToolHandler(f.srv.handleWriteFile)
	if facade {
		lowered := make(map[string]any, len(args)+2)
		for key, value := range args {
			lowered[key] = value
		}
		lowered["operation"] = "write"
		lowered["target"] = map[string]any{"file": args["path"]}
		delete(lowered, "path")
		if sha, ok := lowered["base_sha"]; ok {
			lowered["guard"] = map[string]any{"base_sha": sha}
			delete(lowered, "base_sha")
		}
		req.Params.Name, req.Params.Arguments = "edit", lowered
		handler = f.srv.mcpServer.GetTool("edit").Handler
	}
	return req, handler
}

func TestWholeFileWriteSourceAuthorityAcrossPendingGraph(t *testing.T) {
	for _, mode := range []struct{ facade, unavailableHead bool }{{false, false}, {true, false}, {false, true}, {true, true}} {
		facade := mode.facade
		name := "legacy"
		if facade {
			name = "facade"
		}
		if mode.unavailableHead {
			name += "/unavailable_HEAD_evidence"
		}
		t.Run(name, func(t *testing.T) {
			if facade {
				t.Setenv("GORTEX_TOOLS", "facade-v1")
			}
			f, blocked := newBlockedLifecycleFixture(t)
			headUsable := byteEditFixtureHeadProof(t, f, mode.unavailableHead)
			var prior *mcplib.CallToolResult
			if facade {
				prior = f.facade(t, f.worktree, "edit", map[string]any{"operation": "file", "target": map[string]any{"file": "repo/edit.go"}, "match": "func New() {}", "replacement": "func LifecycleBlocked() {}", "format": "json"})
			} else {
				prior = f.edit(t, f.worktree, map[string]any{"path": "repo/edit.go", "old_string": "func New() {}", "new_string": "func LifecycleBlocked() {}", "format": "json"})
			}
			require.False(t, prior.IsError, viewResultText(t, prior))
			select {
			case <-blocked.entered:
			case <-time.After(10 * time.Second):
				t.Fatal("prior graph publication never reached parser")
			}
			primaryBefore, err := os.ReadFile(filepath.Join(f.primary, "edit.go"))
			require.NoError(t, err)
			previous := mutationBeforeAdmission
			ordinaryAdmissions := 0
			mutationBeforeAdmission = func(context.Context) { ordinaryAdmissions++ }
			defer func() { mutationBeforeAdmission = previous }()
			content := "package repo\nfunc WholeFilePending() {}\n"
			req, handler := wholeFileAuthorityRequest(f, facade, map[string]any{"path": "repo/whole.go", "content": content, "format": "json"})
			_, eligible := f.srv.byteEditRequest(&req, requestFreshness{}, capabilityRequest{})
			require.True(t, eligible)
			ctx := WithSessionCWD(WithSessionID(t.Context(), "whole-file-pending"), f.worktree)
			result := make(chan *mcplib.CallToolResult, 1)
			go func() {
				r, err := handler(ctx, req)
				if err != nil {
					r = mcplib.NewToolResultError(err.Error())
				}
				result <- r
			}()
			// The source lease still drains the real prior parser before writing.
			select {
			case <-result:
				t.Fatal("write bypassed prior parser drain")
			default:
			}
			_, err = os.Stat(filepath.Join(f.worktree, "whole.go"))
			require.True(t, os.IsNotExist(err))
			blocked.unblock()
			var r *mcplib.CallToolResult
			select {
			case r = <-result:
			case <-time.After(20 * time.Second):
				t.Fatal("whole-file source handoff did not finish")
			}
			require.NotNil(t, r)
			require.False(t, r.IsError, viewResultText(t, r))
			fresh := resultFreshness(t, r)
			if headUsable {
				require.Zero(t, ordinaryAdmissions, "supported whole-file authority must not materialize a graph before handler admission")
				require.Equal(t, "file", fresh["freshness_scope"])
			}
			if fresh["freshness_scope"] == "file" {
				require.Equal(t, "pending", fresh["graph_freshness"])
			} else {
				require.False(t, headUsable, "only unavailable optional HEAD proof may retain ordinary admission")
				require.Positive(t, ordinaryAdmissions)
			}
			require.Equal(t, f.checkoutID, fresh["checkout_id"])
			data, err := os.ReadFile(filepath.Join(f.worktree, "whole.go"))
			require.NoError(t, err)
			require.Equal(t, content, string(data))
			data, err = os.ReadFile(filepath.Join(f.primary, "edit.go"))
			require.NoError(t, err)
			require.Equal(t, primaryBefore, data)
			f.awaitMutation(t, f.worktree, r)
			f.awaitMutation(t, f.worktree, prior)
		})
	}
}

func TestWholeFileWriteSourceAuthorityKeepsOverwriteSHAReplayAndEvidence(t *testing.T) {
	for _, facade := range []bool{false, true} {
		name := "legacy"
		if facade {
			name = "facade"
		}
		t.Run(name, func(t *testing.T) {
			if facade {
				t.Setenv("GORTEX_TOOLS", "facade-v1")
			}
			f := newRealCheckoutMutationFixture(t)
			previous := mutationBeforeAdmission
			ordinaryAdmissions := 0
			mutationBeforeAdmission = func(context.Context) { ordinaryAdmissions++ }
			defer func() { mutationBeforeAdmission = previous }()
			ctx := WithSessionCWD(WithSessionID(t.Context(), "whole-file-guards"), f.worktree)
			call := func(args map[string]any) *mcplib.CallToolResult {
				req, handler := wholeFileAuthorityRequest(f, facade, args)
				r, err := handler(ctx, req)
				require.NoError(t, err)
				require.NotNil(t, r)
				require.False(t, r.IsError, viewResultText(t, r))
				return r
			}
			before := "package repo\nfunc WholeFileBefore() {}\n"
			create := call(map[string]any{"path": "repo/whole.go", "content": before, "physical_evidence": true, "format": "json"})
			payload := decodeFileOpsResult(t, create)
			require.Zero(t, ordinaryAdmissions, "idle whole-file create must use checked source authority")
			require.Equal(t, "created", payload["status"])
			require.Equal(t, true, payload["before_absent"])
			require.Equal(t, diskSHA256(t, filepath.Join(f.worktree, "whole.go")), payload["after_sha256"])
			f.awaitMutation(t, f.worktree, create)
			after := "package repo\nfunc WholeFileAfter() {}\n"
			args := map[string]any{"path": "repo/whole.go", "content": after, "base_sha": gitBlobSHA([]byte(before)), "mutation_id": "whole-file-source-overwrite", "format": "json"}
			overwrite := call(args)
			first := decodeFileOpsResult(t, overwrite)
			require.Equal(t, "overwritten", first["status"])
			f.awaitMutation(t, f.worktree, overwrite)
			replay := call(args)
			require.Equal(t, first["mutation_receipt"], decodeFileOpsResult(t, replay)["mutation_receipt"], "retry keeps original receipt even though its optional SHA is now stale")
			// No SHA remains an unconditional overwrite, including an existing target.
			last := call(map[string]any{"path": "repo/whole.go", "content": before, "format": "json"})
			require.Equal(t, "overwritten", decodeFileOpsResult(t, last)["status"])
			f.awaitMutation(t, f.worktree, last)
			_, err := os.Stat(filepath.Join(f.primary, "whole.go"))
			require.True(t, os.IsNotExist(err))
		})
	}
}

func TestWholeFileWriteSourceAuthorityKeepsRefusalsAndExclusions(t *testing.T) {
	f := newRealCheckoutMutationFixture(t)
	original, err := os.ReadFile(filepath.Join(f.worktree, "edit.go"))
	require.NoError(t, err)
	primaryBefore, err := os.ReadFile(filepath.Join(f.primary, "edit.go"))
	require.NoError(t, err)
	ctx := WithSessionCWD(WithSessionID(t.Context(), "whole-file-refusals"), f.worktree)
	for _, kind := range []string{"sha", "missing_sha_target", "parse", "foreign", "evidence", "utf16"} {
		t.Run(kind, func(t *testing.T) {
			args := map[string]any{"path": "repo/edit.go", "content": "package repo\nfunc MustNotWrite() {}\n", "format": "json"}
			switch kind {
			case "sha":
				args["base_sha"] = "0000000000000000000000000000000000000000"
			case "missing_sha_target":
				args["path"] = "repo/missing.go"
				args["base_sha"] = gitBlobSHA(original)
			case "parse":
				args["content"] = "package repo\nfunc Broken("
			case "foreign":
				args["path"] = filepath.Join(f.primary, "edit.go")
			case "evidence":
				args["physical_evidence"] = true
				args["digest"] = "md5"
			case "utf16":
				args["path"] = "repo/utf16.go"
				require.NoError(t, os.WriteFile(filepath.Join(f.worktree, "utf16.go"), utf16LEWithBOM(t, "package repo\nfunc LegacySource() {}\n"), 0o644))
			}
			req, handler := wholeFileAuthorityRequest(f, false, args)
			r, err := handler(ctx, req)
			require.NoError(t, err)
			require.True(t, r.IsError, viewResultText(t, r))
			if kind == "utf16" {
				require.Contains(t, viewResultText(t, r), "UTF-16")
				data, err := os.ReadFile(filepath.Join(f.worktree, "utf16.go"))
				require.NoError(t, err)
				require.Equal(t, utf16LEWithBOM(t, "package repo\nfunc LegacySource() {}\n"), data)
			}
			data, err := os.ReadFile(filepath.Join(f.worktree, "edit.go"))
			require.NoError(t, err)
			require.Equal(t, original, data)
			data, err = os.ReadFile(filepath.Join(f.primary, "edit.go"))
			require.NoError(t, err)
			require.Equal(t, primaryBefore, data)
			_, err = os.Stat(filepath.Join(f.worktree, "missing.go"))
			require.True(t, os.IsNotExist(err))
		})
	}
	for _, kind := range []string{"dry_run", "fresh", "deadline", "graph", "optional_graph", "complete", "batch", "symbol", "glob", "missing_content"} {
		t.Run(kind, func(t *testing.T) {
			req := mcplib.CallToolRequest{}
			req.Params.Name = "write_file"
			args := map[string]any{"path": "repo/new.go", "content": "package repo\n"}
			req.Params.Arguments = args
			fresh := requestFreshness{}
			caps := capabilityRequest{}
			switch kind {
			case "dry_run":
				args["dry_run"] = true
			case "fresh":
				fresh.requireFresh = true
			case "deadline":
				fresh.hasDeadline = true
			case "graph":
				caps.required = []graphview.CapabilityID{graphview.CapSyntaxGraph}
			case "optional_graph":
				caps.optional = []graphview.CapabilityID{graphview.CapSyntaxGraph}
			case "complete":
				caps.requireComplete = true
			case "batch":
				req.Params.Name = "batch_edit"
			case "symbol":
				req.Params.Name = "edit_symbol"
			case "glob":
				args["path"] = "repo/*.go"
			case "missing_content":
				delete(args, "content")
			}
			_, eligible := f.srv.byteEditRequest(&req, fresh, caps)
			require.False(t, eligible)
		})
	}
}

// Catalog-only retired checkouts have no coordinator to activate. Optional
// source admission must leave the ordinary route wait time to refuse clearly.
func TestWholeFileWriteRetiredRouteRefusesBeforeToolDeadline(t *testing.T) {
	for _, facade := range []bool{false, true} {
		name := "legacy"
		if facade {
			name = "facade"
		}
		t.Run(name, func(t *testing.T) {
			if facade {
				t.Setenv("GORTEX_TOOLS", "facade-v1")
			}
			stack := newViewStack(t)
			retireRoute(t, stack)
			f := &realCheckoutMutationFixture{srv: stack.srv}
			req, handler := wholeFileAuthorityRequest(f, facade, map[string]any{
				"path": "repo/added.go", "content": "package repo\nfunc Never() {}\n",
			})
			ctx := routeRefusalContext(t, stack.worktreeRoot, req.Params.Name)
			result, err := handler(ctx, req)
			require.NoError(t, err)
			require.NotNil(t, result)
			require.True(t, result.IsError)
			require.Contains(t, viewResultText(t, result), graphview.CodeViewBuilding)
			require.NoError(t, ctx.Err(), "refusal must precede the transport deadline")
			deadline, ok := ctx.Deadline()
			require.True(t, ok)
			require.Greater(t, time.Until(deadline), mutationRouteWaitMargin/2,
				"optional source admission consumed the ordinary refusal margin")
			for _, root := range []string{stack.repoRoot, stack.worktreeRoot} {
				_, err := os.Stat(filepath.Join(root, "added.go"))
				require.True(t, os.IsNotExist(err), "refused write changed %s", root)
			}
		})
	}
}
