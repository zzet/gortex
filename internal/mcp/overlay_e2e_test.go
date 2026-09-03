package mcp

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/daemon"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/query"
)

// setupOverlayServer builds a fully-wired MCP server with an attached
// OverlayManager and an indexed temp repo of two interlinked Go
// files: target.go defines `Target()`, caller.go has a `Caller()`
// that calls `Target()`. The shape lets tests verify (a) overlays
// surface buffer-defined symbols, (b) cross-file edges from base
// (caller→target) survive overlay since base is never mutated, and
// (c) two concurrent sessions see their own overlay independently.
func setupOverlayServer(t *testing.T) (srv *Server, dir, targetFile, callerFile string) {
	t.Helper()
	dir = t.TempDir()
	targetFile = filepath.Join(dir, "target.go")
	callerFile = filepath.Join(dir, "caller.go")
	require.NoError(t, os.WriteFile(targetFile, []byte(`package main

func Target() {}
`), 0o644))
	require.NoError(t, os.WriteFile(callerFile, []byte(`package main

func Caller() {
	Target()
}
`), 0o644))
	// A committed, clean work tree. These tests only ever want an empty diff
	// from the fixture, and before MapGitDiff stopped swallowing failures they
	// got one by accident: git refused to diff a directory that was not a repo
	// at all, and the error was returned as "no changes".
	initCleanGitRepo(t, dir)

	g := graph.New()
	reg := testRegistry()
	cfg := config.Default()
	idx := indexer.New(g, reg, cfg.Index, zap.NewNop())
	_, err := idx.Index(dir)
	require.NoError(t, err)
	idx.ResolveAll()

	eng := query.NewEngine(g)
	srv = NewServer(eng, g, idx, nil, zap.NewNop(), nil)
	srv.SetOverlayManager(daemon.NewOverlayManager(time.Minute))
	srv.RunAnalysis()
	return srv, dir, targetFile, callerFile
}

// initCleanGitRepo turns dir into a git work tree whose HEAD holds everything
// already written there, so every diff scope reports a genuinely empty diff.
func initCleanGitRepo(t *testing.T, dir string) {
	t.Helper()
	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-b", "main")
	run("config", "user.email", "t@t")
	run("config", "user.name", "t")
	run("config", "diff.mnemonicPrefix", "false")
	run("config", "diff.noprefix", "false")
	run("add", ".")
	run("commit", "-m", "base")
}

func callToolByName(t *testing.T, srv *Server, ctx context.Context, name string, args map[string]any) *mcplib.CallToolResult {
	t.Helper()
	tool := srv.MCPServer().GetTool(name)
	require.NotNilf(t, tool, "tool %q must be registered", name)
	req := mcplib.CallToolRequest{Params: mcplib.CallToolParams{
		Name:      name,
		Arguments: args,
	}}
	res, err := tool.Handler(ctx, req)
	require.NoError(t, err)
	return res
}

func toolText(res *mcplib.CallToolResult) string {
	var sb strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(mcplib.TextContent); ok {
			sb.WriteString(tc.Text)
		}
	}
	return sb.String()
}

// TestOverlay_BaseGraphIsImmutable is the load-bearing isolation
// guarantee: pushing and querying an overlay must NOT mutate the
// base graph. A snapshot of base node IDs taken before and after a
// full overlay round-trip must be byte-identical.
func TestOverlay_BaseGraphIsImmutable(t *testing.T) {
	srv, _, targetFile, _ := setupOverlayServer(t)
	beforeIDs := baseNodeIDs(srv)

	sessID := "test-immutability"
	require.NoError(t, srv.OverlayManager().RegisterWithID(sessID, ""))
	require.NoError(t, srv.OverlayManager().Push(sessID, daemon.OverlayFile{
		Path: targetFile,
		Content: `package main

func Target() {}

func Overlay() {}
`,
	}, nil))

	ctx := WithSessionID(context.Background(), sessID)
	res := callToolByName(t, srv, ctx, "get_file_summary", map[string]any{
		"path": filepath.Base(targetFile),
	})
	require.False(t, res.IsError)
	require.Contains(t, toolText(res), "Overlay")

	afterIDs := baseNodeIDs(srv)
	require.Equal(t, beforeIDs, afterIDs,
		"base graph must be byte-identical before and after an overlay round-trip")
}

func TestOverlay_AbsoluteMissingAliasMapsToCanonicalGraphPath(t *testing.T) {
	srv, dir, _, _ := setupOverlayServer(t)
	aliasRoot := filepath.Join(t.TempDir(), "repo-alias")
	if err := os.Symlink(dir, aliasRoot); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	aliasFile := filepath.Join(aliasRoot, "new-buffer.go")
	layer, paths, err := srv.constructOverlayLayer(context.Background(), []daemon.OverlayFile{{
		Path:    aliasFile,
		Content: "package main\n\nfunc AliasBuffer() {}\n",
	}})
	require.NoError(t, err)
	require.Equal(t, []string{"new-buffer.go"}, paths)
	require.NotNil(t, layer)

	view := graph.NewOverlaidView(srv.graph, layer)
	nodes := view.FindNodesByName("AliasBuffer")
	require.Len(t, nodes, 1)
	require.Equal(t, "new-buffer.go::AliasBuffer", nodes[0].ID)
}

// TestOverlay_FindUsagesPreservesCrossFileCallers exercises the
// regression the in-place-mutation design had: an editor overlays
// target.go (defining Target), and find_usages(target.go::Target)
// must still surface caller.go's call site — base's resolved edge
// from caller→target survives because the base graph isn't touched.
func TestOverlay_FindUsagesPreservesCrossFileCallers(t *testing.T) {
	srv, _, targetFile, _ := setupOverlayServer(t)
	sessID := "test-find-usages"
	require.NoError(t, srv.OverlayManager().RegisterWithID(sessID, ""))
	// Overlay rewrites target.go but keeps Target() with the same
	// signature, so its node ID is unchanged.
	require.NoError(t, srv.OverlayManager().Push(sessID, daemon.OverlayFile{
		Path: targetFile,
		Content: `package main

func Target() {}

func NewSibling() {}
`,
	}, nil))

	ctx := WithSessionID(context.Background(), sessID)
	res := callToolByName(t, srv, ctx, "find_usages", map[string]any{
		"id": "target.go::Target",
	})
	require.False(t, res.IsError, "find_usages: %s", toolText(res))
	require.Contains(t, toolText(res), "Caller",
		"overlay must preserve base's caller.go → target.go::Target edge")
}

// TestOverlay_DriftSurfacesAsToolError: a stale BaseSHA must turn
// the very next tool call into an MCP error result so the client
// re-reads and resubmits.
func TestOverlay_DriftSurfacesAsToolError(t *testing.T) {
	srv, _, targetFile, _ := setupOverlayServer(t)
	sessID := "test-session-drift"
	require.NoError(t, srv.OverlayManager().RegisterWithID(sessID, ""))
	require.NoError(t, srv.OverlayManager().Push(sessID, daemon.OverlayFile{
		Path:    targetFile,
		Content: "package main\n\nfunc Target() {}\n",
		BaseSHA: "0000000000000000000000000000000000000000",
	}, nil))

	ctx := WithSessionID(context.Background(), sessID)
	res := callToolByName(t, srv, ctx, "get_file_summary", map[string]any{
		"path": filepath.Base(targetFile),
	})
	require.True(t, res.IsError, "drift must surface as an MCP tool error")
	require.Contains(t, toolText(res), "overlay base SHA mismatch")
}

// TestOverlay_BaseSHA_MatchProceeds: when the editor's base SHA
// agrees with the on-disk hash, the overlay applies and the new
// symbol is visible.
func TestOverlay_BaseSHA_MatchProceeds(t *testing.T) {
	srv, _, targetFile, _ := setupOverlayServer(t)
	data, err := os.ReadFile(targetFile)
	require.NoError(t, err)
	h := sha1.New()
	fmt.Fprintf(h, "blob %d\x00", len(data))
	_, _ = h.Write(data)
	baseSHA := hex.EncodeToString(h.Sum(nil))

	sessID := "test-session-match"
	require.NoError(t, srv.OverlayManager().RegisterWithID(sessID, ""))
	require.NoError(t, srv.OverlayManager().Push(sessID, daemon.OverlayFile{
		Path:    targetFile,
		Content: "package main\n\nfunc Target() {}\n\nfunc Overlay() {}\n",
		BaseSHA: baseSHA,
	}, nil))

	ctx := WithSessionID(context.Background(), sessID)
	res := callToolByName(t, srv, ctx, "get_file_summary", map[string]any{
		"path": filepath.Base(targetFile),
	})
	require.False(t, res.IsError)
	require.Contains(t, toolText(res), "Overlay")
}

// TestOverlay_NoSessionNoOp: a tools/call with no overlay session
// bound to ctx must NOT pay any overlay cost and must observe the
// on-disk view. Failing this would mean every non-overlay call
// pays an extra parse pass.
func TestOverlay_NoSessionNoOp(t *testing.T) {
	srv, _, targetFile, _ := setupOverlayServer(t)
	require.NoError(t, srv.OverlayManager().RegisterWithID("idle", ""))
	ctx := WithSessionID(context.Background(), "idle")
	res := callToolByName(t, srv, ctx, "get_file_summary", map[string]any{
		"path": filepath.Base(targetFile),
	})
	require.False(t, res.IsError)
	require.Contains(t, toolText(res), "Target")
	require.NotContains(t, toolText(res), "Overlay")
}

// TestOverlay_TwoSessionsIsolated proves multi-tenant isolation: two
// sessions with conflicting overlays on the same path each see their
// own overlay, run concurrently, and don't contaminate each other.
// This was the failure mode of the prior in-place-mutation design.
func TestOverlay_TwoSessionsIsolated(t *testing.T) {
	srv, _, targetFile, _ := setupOverlayServer(t)
	sessA := "alpha"
	sessB := "beta"
	require.NoError(t, srv.OverlayManager().RegisterWithID(sessA, ""))
	require.NoError(t, srv.OverlayManager().RegisterWithID(sessB, ""))
	require.NoError(t, srv.OverlayManager().Push(sessA, daemon.OverlayFile{
		Path:    targetFile,
		Content: "package main\n\nfunc Target() {}\n\nfunc AlphaOnly() {}\n",
	}, nil))
	require.NoError(t, srv.OverlayManager().Push(sessB, daemon.OverlayFile{
		Path:    targetFile,
		Content: "package main\n\nfunc Target() {}\n\nfunc BetaOnly() {}\n",
	}, nil))

	const iterations = 8
	var wg sync.WaitGroup
	var aErr, bErr error
	wg.Add(2)
	go func() {
		defer wg.Done()
		ctx := WithSessionID(context.Background(), sessA)
		for i := 0; i < iterations; i++ {
			res := callToolByName(t, srv, ctx, "get_file_summary", map[string]any{
				"path": filepath.Base(targetFile),
			})
			body := toolText(res)
			if !strings.Contains(body, "AlphaOnly") {
				aErr = fmt.Errorf("alpha did not see AlphaOnly: %s", body)
				return
			}
			if strings.Contains(body, "BetaOnly") {
				aErr = fmt.Errorf("alpha leaked BetaOnly: %s", body)
				return
			}
		}
	}()
	go func() {
		defer wg.Done()
		ctx := WithSessionID(context.Background(), sessB)
		for i := 0; i < iterations; i++ {
			res := callToolByName(t, srv, ctx, "get_file_summary", map[string]any{
				"path": filepath.Base(targetFile),
			})
			body := toolText(res)
			if !strings.Contains(body, "BetaOnly") {
				bErr = fmt.Errorf("beta did not see BetaOnly: %s", body)
				return
			}
			if strings.Contains(body, "AlphaOnly") {
				bErr = fmt.Errorf("beta leaked AlphaOnly: %s", body)
				return
			}
		}
	}()
	wg.Wait()
	require.NoError(t, aErr)
	require.NoError(t, bErr)
}

// TestOverlay_OverlayAndBaseSessionsIsolated: a session WITH an
// overlay and a session WITHOUT any overlay run concurrently. The
// overlay session sees its buffer; the bare session sees disk.
// Neither contaminates the other. This is the case the prior
// in-place design failed because non-overlay calls observed the
// graph in its mid-mutation state.
func TestOverlay_OverlayAndBaseSessionsIsolated(t *testing.T) {
	srv, _, targetFile, _ := setupOverlayServer(t)
	overlaySess := "with-overlay"
	require.NoError(t, srv.OverlayManager().RegisterWithID(overlaySess, ""))
	require.NoError(t, srv.OverlayManager().Push(overlaySess, daemon.OverlayFile{
		Path:    targetFile,
		Content: "package main\n\nfunc Target() {}\n\nfunc EditorOnly() {}\n",
	}, nil))

	const iterations = 8
	var wg sync.WaitGroup
	var withErr, withoutErr error
	wg.Add(2)
	go func() {
		defer wg.Done()
		ctx := WithSessionID(context.Background(), overlaySess)
		for i := 0; i < iterations; i++ {
			res := callToolByName(t, srv, ctx, "get_file_summary", map[string]any{
				"path": filepath.Base(targetFile),
			})
			if !strings.Contains(toolText(res), "EditorOnly") {
				withErr = fmt.Errorf("overlay session lost overlay: %s", toolText(res))
				return
			}
		}
	}()
	go func() {
		defer wg.Done()
		ctx := context.Background() // no session ID
		for i := 0; i < iterations; i++ {
			res := callToolByName(t, srv, ctx, "get_file_summary", map[string]any{
				"path": filepath.Base(targetFile),
			})
			if strings.Contains(toolText(res), "EditorOnly") {
				withoutErr = fmt.Errorf("base session leaked overlay: %s", toolText(res))
				return
			}
		}
	}()
	wg.Wait()
	require.NoError(t, withErr)
	require.NoError(t, withoutErr)
}

// TestOverlay_GetSymbolSourceReturnsBufferContent: get_symbol_source
// must surface the editor's unsaved bytes for the overlaid file,
// not the on-disk version. Without overlay-aware content reads the
// I19 contract for source-reading tools is unmet.
func TestOverlay_GetSymbolSourceReturnsBufferContent(t *testing.T) {
	srv, _, targetFile, _ := setupOverlayServer(t)
	sessID := "src-test"
	require.NoError(t, srv.OverlayManager().RegisterWithID(sessID, ""))
	require.NoError(t, srv.OverlayManager().Push(sessID, daemon.OverlayFile{
		Path: targetFile,
		Content: `package main

// EditorSentinel is a unique comment line that only exists in the
// overlay buffer, never on disk.
func Target() {}
`,
	}, nil))

	ctx := WithSessionID(context.Background(), sessID)
	res := callToolByName(t, srv, ctx, "get_symbol_source", map[string]any{
		"id":            "target.go::Target",
		"context_lines": 10,
	})
	require.False(t, res.IsError, "get_symbol_source: %s", toolText(res))
	require.Contains(t, toolText(res), "EditorSentinel",
		"get_symbol_source must return overlay buffer content, not disk")
}

// TestOverlay_DeletionTombstone: deleted=true overlays hide the
// file's symbols. find_usages on a deleted symbol returns no
// results in the overlay view.
func TestOverlay_DeletionTombstone(t *testing.T) {
	srv, _, targetFile, _ := setupOverlayServer(t)
	sessID := "tombstone-test"
	require.NoError(t, srv.OverlayManager().RegisterWithID(sessID, ""))
	require.NoError(t, srv.OverlayManager().Push(sessID, daemon.OverlayFile{
		Path:    targetFile,
		Deleted: true,
	}, nil))

	ctx := WithSessionID(context.Background(), sessID)
	res := callToolByName(t, srv, ctx, "get_file_summary", map[string]any{
		"path": filepath.Base(targetFile),
	})
	// Deletion overlay: either an error (no nodes) or an empty body,
	// but Target must not appear.
	require.NotContains(t, toolText(res), "Target",
		"deletion overlay must hide every symbol from the file")
}

// TestOverlay_MCP_RegisterPushList exercises the MCP-tool surface
// for overlay management: overlay_register, overlay_push,
// overlay_list. The MCP-native path is what IDE extensions speaking
// MCP take instead of the /v1/overlay/* HTTP endpoints.
func TestOverlay_MCP_RegisterPushList(t *testing.T) {
	srv, _, targetFile, _ := setupOverlayServer(t)
	sessID := "mcp-register"

	ctx := WithSessionID(context.Background(), sessID)
	regRes := callToolByName(t, srv, ctx, "overlay_register", map[string]any{})
	require.False(t, regRes.IsError, "overlay_register: %s", toolText(regRes))

	pushRes := callToolByName(t, srv, ctx, "overlay_push", map[string]any{
		"path":    targetFile,
		"content": "package main\n\nfunc Target() {}\n\nfunc PushedViaMCP() {}\n",
	})
	require.False(t, pushRes.IsError, "overlay_push: %s", toolText(pushRes))

	listRes := callToolByName(t, srv, ctx, "overlay_list", map[string]any{})
	listText := toolText(listRes)
	require.Contains(t, listText, targetFile)
	require.Contains(t, listText, `"count":1`)

	summary := callToolByName(t, srv, ctx, "get_file_summary", map[string]any{
		"path": filepath.Base(targetFile),
	})
	require.Contains(t, toolText(summary), "PushedViaMCP")
}

// TestOverlay_CompareWithOverlay_DiffSurface exercises the diff
// tool: an overlay adds a NewSibling function inside target.go but
// leaves caller.go untouched. find_usages of NewSibling against
// base returns nothing (the symbol doesn't exist on disk); against
// overlay it returns nothing either (caller.go doesn't call it).
// The base and overlay sides should disagree on the existence of
// NewSibling itself via the layer's overlay_paths metadata.
func TestOverlay_CompareWithOverlay_DiffSurface(t *testing.T) {
	srv, _, targetFile, callerFile := setupOverlayServer(t)
	// Edit caller.go in the overlay so it now calls NewSibling
	// instead of Target. Edit target.go in the overlay to define
	// NewSibling. compare_with_overlay against caller.go::Caller
	// should show NewSibling as an added dependency that doesn't
	// exist in base.
	sessID := "diff-test"
	require.NoError(t, srv.OverlayManager().RegisterWithID(sessID, ""))
	require.NoError(t, srv.OverlayManager().Push(sessID, daemon.OverlayFile{
		Path: targetFile,
		Content: `package main

func Target() {}

func NewSibling() {}
`,
	}, nil))
	require.NoError(t, srv.OverlayManager().Push(sessID, daemon.OverlayFile{
		Path: callerFile,
		Content: `package main

func Caller() {
	NewSibling()
}
`,
	}, nil))

	ctx := WithSessionID(context.Background(), sessID)
	res := callToolByName(t, srv, ctx, "compare_with_overlay", map[string]any{
		"kind": "get_call_chain",
		"id":   "caller.go::Caller",
	})
	require.False(t, res.IsError, "compare_with_overlay: %s", toolText(res))
	body := toolText(res)
	require.Contains(t, body, `"overlay_paths"`)
	require.Contains(t, body, "target.go")
	require.Contains(t, body, "caller.go")
}

// TestOverlay_DroppedOnMCPSessionRelease is the security-critical
// guarantee: when an MCP session ends, its overlay must die
// immediately so no future connection that learns or guesses the
// same session ID can re-attach to abandoned buffers. The TTL is a
// fail-safe; ReleaseSession is the fast path.
func TestOverlay_DroppedOnMCPSessionRelease(t *testing.T) {
	srv, _, targetFile, _ := setupOverlayServer(t)
	sessID := "secure-bind"
	require.NoError(t, srv.OverlayManager().RegisterWithID(sessID, ""))
	require.NoError(t, srv.OverlayManager().Push(sessID, daemon.OverlayFile{
		Path:    targetFile,
		Content: "package main\n\nfunc Target() {}\n\nfunc Secret() {}\n",
	}, nil))
	require.True(t, srv.OverlayManager().Has(sessID))

	srv.ReleaseSession(sessID)
	require.False(t, srv.OverlayManager().Has(sessID),
		"MCP session release must drop the overlay synchronously")

	// A subsequent tools/call carrying the (now-stale) session ID
	// must fall through to base — NOT re-attach to abandoned state.
	ctx := WithSessionID(context.Background(), sessID)
	res := callToolByName(t, srv, ctx, "get_file_summary", map[string]any{
		"path": filepath.Base(targetFile),
	})
	require.NotContains(t, toolText(res), "Secret",
		"a tools/call after MCP session release must not see the dropped overlay")
}

// TestOverlay_KeepaliveExtendsLease exercises the MCP keepalive
// tool: an editor that's paused (on a breakpoint, in a long
// refactor wizard) can extend the lease without re-pushing buffer
// content.
func TestOverlay_KeepaliveExtendsLease(t *testing.T) {
	srv, _, targetFile, _ := setupOverlayServer(t)
	sessID := "keepalive-test"
	require.NoError(t, srv.OverlayManager().RegisterWithID(sessID, ""))
	require.NoError(t, srv.OverlayManager().Push(sessID, daemon.OverlayFile{
		Path:    targetFile,
		Content: "package main\n\nfunc Target() {}\n",
	}, nil))

	ctx := WithSessionID(context.Background(), sessID)
	res := callToolByName(t, srv, ctx, "overlay_keepalive", map[string]any{})
	require.False(t, res.IsError, "overlay_keepalive: %s", toolText(res))
	body := toolText(res)
	require.Contains(t, body, `"session_id":"keepalive-test"`)
	require.Contains(t, body, `"idle_seconds"`)
	require.Contains(t, body, `"idle_ttl_seconds"`)
}

// TestOverlay_KeepaliveOnMissingSessionReturnsError: keepalive on
// an unknown / reaped session surfaces a clear "session has been
// dropped" error so the editor knows to overlay_register and re-push.
func TestOverlay_KeepaliveOnMissingSessionReturnsError(t *testing.T) {
	srv, _, _, _ := setupOverlayServer(t)
	ctx := WithSessionID(context.Background(), "ghost-session")
	res := callToolByName(t, srv, ctx, "overlay_keepalive", map[string]any{})
	require.True(t, res.IsError, "missing session must surface as a structured error")
	require.Contains(t, toolText(res), "dropped or never registered")
}

// TestOverlay_ListExposesExpiryMetadata: overlay_list reports the
// per-session liveness fields so editor extensions can schedule
// the next keepalive proactively.
func TestOverlay_ListExposesExpiryMetadata(t *testing.T) {
	srv, _, targetFile, _ := setupOverlayServer(t)
	sessID := "expires-test"
	require.NoError(t, srv.OverlayManager().RegisterWithID(sessID, ""))
	require.NoError(t, srv.OverlayManager().Push(sessID, daemon.OverlayFile{
		Path:    targetFile,
		Content: "package main\n\nfunc Target() {}\n",
	}, nil))

	ctx := WithSessionID(context.Background(), sessID)
	res := callToolByName(t, srv, ctx, "overlay_list", map[string]any{})
	require.False(t, res.IsError)
	body := toolText(res)
	require.Contains(t, body, `"expires_at"`)
	require.Contains(t, body, `"last_used_at"`)
	require.Contains(t, body, `"idle_seconds"`)
	require.Contains(t, body, `"idle_ttl_seconds"`)
}

func TestOverlay_RequestSnapshotPinsWorkspaceAndFilesAcrossConcurrentPush(t *testing.T) {
	srv, _, targetFile, _ := setupOverlayServer(t)
	srv.indexer.SetWorkspaceID("workspace-a")
	const sessionID = "request-snapshot"
	require.NoError(t, srv.OverlayManager().RegisterWithID(sessionID, "workspace-a"))
	const firstContent = "package main\n\nfunc FirstBuffer() {}\n"
	require.NoError(t, srv.OverlayManager().Push(sessionID, daemon.OverlayFile{
		Path: targetFile, Content: firstContent,
	}, nil))

	ctx, firstView, err := srv.prepareOverlayRequest(WithSessionID(context.Background(), sessionID))
	require.NoError(t, err)
	require.NotNil(t, firstView)
	snapshot, ok := overlayRequestSnapshotFromContext(ctx)
	require.True(t, ok)
	require.Equal(t, "workspace-a", snapshot.workspace)
	require.Len(t, snapshot.files, 1)
	require.Equal(t, firstContent, snapshot.files[0].Content)

	pushDone := make(chan error, 1)
	go func() {
		pushDone <- srv.OverlayManager().Push(sessionID, daemon.OverlayFile{
			Path: targetFile, Content: "package main\n\nfunc SecondBuffer() {}\n",
		}, nil)
	}()
	require.NoError(t, <-pushDone)

	content, found := srv.overlayContentFor(ctx, targetFile)
	require.True(t, found)
	require.Equal(t, firstContent, content, "the prepared request must retain its immutable buffer cohort")
	reusedCtx, reusedView, err := srv.prepareOverlayRequest(ctx)
	require.NoError(t, err)
	require.Same(t, firstView, reusedView)
	reusedSnapshot, ok := overlayRequestSnapshotFromContext(reusedCtx)
	require.True(t, ok)
	require.Same(t, snapshot, reusedSnapshot)

	mismatchedCtx := WithSessionID(ctx, "different-session")
	_, _, err = srv.prepareOverlayRequest(mismatchedCtx)
	require.ErrorContains(t, err, "belongs to session")
}

func TestOverlay_RequestSnapshotPinsEmptyAttemptAcrossLaterPush(t *testing.T) {
	srv, _, targetFile, _ := setupOverlayServer(t)
	srv.indexer.SetWorkspaceID("workspace-a")
	const sessionID = "empty-request-snapshot"
	require.NoError(t, srv.OverlayManager().RegisterWithID(sessionID, "workspace-a"))

	ctx, view, err := srv.prepareOverlayRequest(WithSessionID(context.Background(), sessionID))
	require.NoError(t, err)
	require.Nil(t, view)
	snapshot, ok := overlayRequestSnapshotFromContext(ctx)
	require.True(t, ok, "even an empty attempt must be pinned")
	require.Equal(t, "workspace-a", snapshot.workspace)
	require.Empty(t, snapshot.files)

	require.NoError(t, srv.OverlayManager().Push(sessionID, daemon.OverlayFile{
		Path: targetFile, Content: "package main\n\nfunc LaterBuffer() {}\n",
	}, nil))
	reusedCtx, reusedView, err := srv.prepareOverlayRequest(ctx)
	require.NoError(t, err)
	require.Nil(t, reusedView, "a nested call must not resnapshot buffers pushed mid-request")
	reusedSnapshot, ok := overlayRequestSnapshotFromContext(reusedCtx)
	require.True(t, ok)
	require.Same(t, snapshot, reusedSnapshot)
	_, found := srv.overlayContentFor(reusedCtx, targetFile)
	require.False(t, found)
}

func TestOverlay_RequestSnapshotRejectsForeignWorkspaceAndPath(t *testing.T) {
	t.Run("workspace mismatch", func(t *testing.T) {
		srv, _, targetFile, _ := setupOverlayServer(t)
		srv.indexer.SetWorkspaceID("workspace-a")
		const sessionID = "foreign-workspace"
		require.NoError(t, srv.OverlayManager().RegisterWithID(sessionID, "workspace-b"))
		require.NoError(t, srv.OverlayManager().Push(sessionID, daemon.OverlayFile{
			Path: targetFile, Deleted: true,
		}, nil))

		_, _, err := srv.prepareOverlayRequest(WithSessionID(context.Background(), sessionID))
		require.ErrorContains(t, err, "not registered workspace")
	})

	t.Run("untracked path", func(t *testing.T) {
		srv, _, _, _ := setupOverlayServer(t)
		srv.indexer.SetWorkspaceID("workspace-a")
		const sessionID = "foreign-path"
		require.NoError(t, srv.OverlayManager().RegisterWithID(sessionID, "workspace-a"))
		require.NoError(t, srv.OverlayManager().Push(sessionID, daemon.OverlayFile{
			Path: filepath.Join(t.TempDir(), "foreign.go"), Deleted: true,
		}, nil))

		_, _, err := srv.prepareOverlayRequest(WithSessionID(context.Background(), sessionID))
		require.ErrorContains(t, err, "outside the registered workspace")
	})
}

func TestOverlay_RequestSnapshotCanonicalizesEquivalentAliases(t *testing.T) {
	srv, _, targetFile, _ := setupOverlayServer(t)
	const sessionID = "canonical-aliases"
	require.NoError(t, srv.OverlayManager().RegisterWithID(sessionID, ""))
	const content = "package main\n\nfunc CanonicalBuffer() {}\n"
	for _, alias := range []string{targetFile, filepath.Base(targetFile), "./" + filepath.Base(targetFile)} {
		require.NoError(t, srv.OverlayManager().Push(sessionID, daemon.OverlayFile{
			Path: alias, Content: content, BaseSHA: "",
		}, nil))
	}

	ctx, view, err := srv.prepareOverlayRequest(WithSessionID(context.Background(), sessionID))
	require.NoError(t, err)
	require.NotNil(t, view)
	snapshot, ok := overlayRequestSnapshotFromContext(ctx)
	require.True(t, ok)
	require.True(t, snapshot.canonical)
	require.Len(t, snapshot.files, 1)
	require.Equal(t, filepath.Base(targetFile), snapshot.files[0].Path)
	require.Equal(t, content, snapshot.files[0].Content)
	got, found := srv.overlayContentFor(ctx, targetFile)
	require.True(t, found)
	require.Equal(t, content, got)
}

func TestOverlay_RequestSnapshotRejectsConflictingAliases(t *testing.T) {
	for _, test := range []struct {
		name   string
		first  daemon.OverlayFile
		second daemon.OverlayFile
	}{
		{
			name:   "content",
			first:  daemon.OverlayFile{Content: "package main\nfunc First() {}\n"},
			second: daemon.OverlayFile{Content: "package main\nfunc Second() {}\n"},
		},
		{
			name:   "tombstone",
			first:  daemon.OverlayFile{Content: "package main\nfunc Present() {}\n"},
			second: daemon.OverlayFile{Deleted: true},
		},
		{
			name:   "base sha",
			first:  daemon.OverlayFile{Content: "package main\n", BaseSHA: "first"},
			second: daemon.OverlayFile{Content: "package main\n", BaseSHA: "second"},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			srv, _, targetFile, _ := setupOverlayServer(t)
			const sessionID = "conflicting-aliases"
			require.NoError(t, srv.OverlayManager().RegisterWithID(sessionID, ""))
			first, second := test.first, test.second
			first.Path = targetFile
			second.Path = filepath.Base(targetFile)
			require.NoError(t, srv.OverlayManager().Push(sessionID, first, nil))
			require.NoError(t, srv.OverlayManager().Push(sessionID, second, nil))

			_, _, err := srv.prepareOverlayRequest(WithSessionID(context.Background(), sessionID))
			require.ErrorContains(t, err, "conflicting overlay aliases")
		})
	}
}

func TestOverlay_RequestSnapshotRejectsNonCanonicalSnapshotWithView(t *testing.T) {
	srv, _, targetFile, _ := setupOverlayServer(t)
	const sessionID = "noncanonical-view"
	ctx := WithSessionID(context.Background(), sessionID)
	ctx = withOverlayRequestSnapshot(ctx, &overlayRequestSnapshot{
		sessionID: sessionID,
		files:     []daemon.OverlayFile{{Path: targetFile, Deleted: true}},
	})
	ctx = WithOverlayView(ctx, graph.NewOverlaidView(srv.graph, graph.NewOverlayLayer()))

	_, _, err := srv.prepareOverlayRequest(ctx)
	require.ErrorContains(t, err, "non-canonical request snapshot")
}

// baseNodeIDs returns a sorted slice of every node ID in the base
// graph. Used to verify the shadow-graph design's load-bearing
// invariant: base is never mutated during overlay processing.
func baseNodeIDs(srv *Server) []string {
	nodes := srv.graph.AllNodes()
	ids := make([]string, 0, len(nodes))
	for _, n := range nodes {
		ids = append(ids, n.ID)
	}
	return ids
}
