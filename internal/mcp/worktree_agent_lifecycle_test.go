package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/parser"
)

// Delay the real parser, not the receipt or coordinator. Only post-edit bytes
// contain the marker, so registration and the initial exact view build normally.
type lifecycleBlockingExtractor struct {
	parser.Extractor
	marker   []byte
	entered  chan struct{}
	release  chan struct{}
	once     sync.Once
	done     sync.Once
	diskPath atomic.Pointer[string]
}

func (e *lifecycleBlockingExtractor) Extract(path string, src []byte) (*parser.ExtractionResult, error) {
	if path := e.diskPath.Load(); path != nil && bytes.Contains(src, e.marker) {
		// Edit preview parses proposed bytes too. Block only after the actual
		// file has committed, when the coordinator is publishing those bytes.
		current, err := os.ReadFile(*path)
		if err == nil && bytes.Contains(current, e.marker) {
			e.once.Do(func() { close(e.entered) })
			<-e.release
		}
	}
	return e.Extractor.Extract(path, src)
}

func (e *lifecycleBlockingExtractor) unblock() { e.done.Do(func() { close(e.release) }) }

func newBlockedLifecycleFixture(t testing.TB) (*realCheckoutMutationFixture, *lifecycleBlockingExtractor) {
	t.Helper()
	e := &lifecycleBlockingExtractor{
		marker: []byte("LifecycleBlocked"), entered: make(chan struct{}), release: make(chan struct{}),
	}
	f := newRealCheckoutMutationFixtureWithRegistry(t, func(registry *parser.Registry) {
		var found bool
		e.Extractor, found = registry.GetByLanguage("go")
		require.True(t, found)
		registry.Register(e)
	})
	path := filepath.Join(f.worktree, "edit.go")
	e.diskPath.Store(&path)
	// Unblock before the fixture's lifecycle cleanup waits for its real worker.
	t.Cleanup(e.unblock)
	f.srv.mutationReindexWait = 20 * time.Millisecond
	f.srv.mutationSafetyWait = 20 * time.Millisecond
	return f, e
}

func lifecycleResultPayload(t testing.TB, result *mcplib.CallToolResult) map[string]any {
	t.Helper()
	require.NotNil(t, result)
	require.False(t, result.IsError, "%+v", result.Content)
	for _, content := range result.Content {
		if text, ok := content.(mcplib.TextContent); ok {
			var payload map[string]any
			if json.Unmarshal([]byte(text.Text), &payload) == nil {
				return payload
			}
		}
	}
	t.Fatalf("no JSON object in result: %+v", result.Content)
	return nil
}

func (f *realCheckoutMutationFixture) facade(t testing.TB, cwd, name string, args map[string]any) *mcplib.CallToolResult {
	t.Helper()
	tool := f.srv.mcpServer.GetTool(name)
	require.NotNil(t, tool, "registered facade %q is required", name)
	req := mcplib.CallToolRequest{}
	req.Params.Name, req.Params.Arguments = name, args
	ctx := WithSessionCWD(WithSessionID(context.Background(), "real-checkout-lifecycle"), cwd)
	result, err := tool.Handler(ctx, req)
	require.NoError(t, err)
	require.NotNil(t, result)
	return result
}

func (f *realCheckoutMutationFixture) mutationStatus(t testing.TB, cwd, root, receipt string) map[string]any {
	t.Helper()
	if os.Getenv("GORTEX_TOOLS") == "facade-v1" {
		return lifecycleResultPayload(t, f.facade(t, cwd, "change", map[string]any{
			"operation": "receipt", "options": map[string]any{"receipt": receipt},
			"view": map[string]any{"kind": "worktree", "path": root},
		}))
	}
	req := mcplib.CallToolRequest{}
	req.Params.Name = "mutation_status"
	req.Params.Arguments = map[string]any{
		"receipt": receipt,
		"view":    map[string]any{"kind": "worktree", "path": root},
	}
	ctx := WithSessionCWD(WithSessionID(context.Background(), "real-checkout-lifecycle"), cwd)
	result, err := f.srv.wrapToolHandler(f.srv.handleMutationStatus)(ctx, req)
	require.NoError(t, err)
	return lifecycleResultPayload(t, result)
}

func (f *realCheckoutMutationFixture) awaitMutation(t testing.TB, cwd string, result *mcplib.CallToolResult) {
	t.Helper()
	f.awaitMutationInRoot(t, cwd, f.worktree, result)
}

func (f *realCheckoutMutationFixture) awaitMutationInRoot(t testing.TB, cwd, root string, result *mcplib.CallToolResult) {
	t.Helper()
	payload := lifecycleResultPayload(t, result)
	receipt, ok := payload["mutation_receipt"].(string)
	require.True(t, ok, "committed write needs a pollable receipt: %+v", payload)
	require.NotEmpty(t, receipt)
	var status map[string]any
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		status = f.mutationStatus(t, cwd, root, receipt)
		if status["graph_status"] == "fresh" {
			return
		}
		require.NotEqual(t, true, status["graph_status_terminal"], "unexpected terminal receipt: %+v", status)
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("mutation did not become fresh: %+v", status)
}

func TestNewWorktreeSearchDiscoversCheckoutWithoutTracking(t *testing.T) {
	for _, name := range []string{"direct_handler", "daemon_scope_handshake"} {
		t.Run(name, func(t *testing.T) {
			testNewWorktreeAgentLifecycle(t, name == "daemon_scope_handshake")
		})
	}
}

func testNewWorktreeAgentLifecycle(t *testing.T, handshake bool) {
	t.Helper()
	t.Setenv("GORTEX_TOOLS", "facade-v1")
	f := newRealCheckoutMutationFixture(t)
	// Add AFTER registration/startup, not as another entry in the initial Git
	// topology. The first agent request must discover this automatic checkout.
	root := filepath.Join(filepath.Dir(f.primary), "newly-added")
	checkoutMutationGit(t, f.primary, "worktree", "add", "-b", "newly-added", root)
	require.NoError(t, os.WriteFile(filepath.Join(root, "added.go"), []byte("package repo\n\nfunc NewlyDiscovered() {}\n"), 0o644))
	started := time.Now()
	if handshake {
		// The daemon checks this before dispatching tools/call with the CWD.
		require.True(t, f.srv.CheckoutServesCWD(context.Background(), root), "daemon must admit the automatically discovered checkout")
	}
	first := f.facade(t, root, "search", map[string]any{
		"operation": "symbols", "query": "Old", "options": map[string]any{"limit": 10},
	})
	require.False(t, first.IsError, "%+v", first.Content)
	require.Less(t, time.Since(started), 2*time.Second, "cold search must not wait for indexing")
	freshness := resultFreshness(t, first)
	require.Contains(t, freshness, "exact", "a cold fallback must identify whether it served the checkout: %+v", first.Content)
	require.NotEmpty(t, lifecycleResultPayload(t, first)["results"], "the unchanged Old symbol must be available from the checkout or labeled base fallback")

	var last *mcplib.CallToolResult
	require.Eventually(t, func() bool {
		last = f.facade(t, root, "search", map[string]any{
			"operation": "symbols", "query": "NewlyDiscovered", "require_exact": true,
			"options": map[string]any{"limit": 10},
		})
		if last.IsError {
			return false
		}
		for _, content := range last.Content {
			if text, ok := content.(mcplib.TextContent); ok && strings.Contains(text.Text, "NewlyDiscovered") {
				return true
			}
		}
		return false
	}, 20*time.Second, 20*time.Millisecond, "new worktree never became exactly searchable")
	require.Equal(t, true, resultFreshness(t, last)["exact"])
	written := f.facade(t, root, "edit", map[string]any{
		"operation": "file", "target": map[string]any{"file": "repo/added.go"},
		"match": "func NewlyDiscovered() {}", "replacement": "func NewlyEdited() {}",
	})
	f.awaitMutationInRoot(t, root, root, written)
	read := f.facade(t, root, "read", map[string]any{
		"operation": "source", "target": map[string]any{"symbol": "repo/added.go::NewlyEdited"},
		"require_exact": true,
	})
	require.False(t, read.IsError, "%+v", read.Content)
	require.Equal(t, true, resultFreshness(t, read)["exact"])
	_, err := os.Stat(filepath.Join(f.primary, "added.go"))
	require.True(t, os.IsNotExist(err), "new checkout source must not be copied into the primary")
}

func TestWorktreeAgentLifecycleSlowRefreshRemainsRecoverable(t *testing.T) {
	t.Setenv("GORTEX_TOOLS", "facade-v1")
	f, blocker := newBlockedLifecycleFixture(t)
	primaryBefore, err := os.ReadFile(filepath.Join(f.primary, "edit.go"))
	require.NoError(t, err)
	view := map[string]any{"kind": "worktree", "path": f.worktree}
	started := time.Now()
	written := f.facade(t, f.primary, "edit", map[string]any{
		"operation": "file", "target": map[string]any{"file": "repo/edit.go"},
		"match": "func New() {}", "replacement": "func LifecycleBlocked() {}", "view": view,
	})
	require.Less(t, time.Since(started), time.Second, "disk commit must not wait for the blocked indexer")
	payload := lifecycleResultPayload(t, written)
	require.Equal(t, false, payload["reindexed"])
	require.Equal(t, "pending", payload["graph_status"])
	receipt, ok := payload["mutation_receipt"].(string)
	require.True(t, ok)
	select {
	case <-blocker.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("real coordinator never reached the post-edit parser")
	}
	// Exceed the old synchronous wait budget while the actual build is held.
	time.Sleep(2 * f.srv.mutationReindexWait)
	statusResult := f.facade(t, f.primary, "change", map[string]any{
		"operation": "receipt", "options": map[string]any{"receipt": receipt}, "view": view,
	})
	status := lifecycleResultPayload(t, statusResult)
	require.Equal(t, "committed", status["disk_status"])
	require.Equal(t, "pending", status["graph_status"])
	require.NotEqual(t, true, status["graph_status_terminal"])

	started = time.Now()
	recovery := f.facade(t, f.primary, "workspace_admin", map[string]any{
		"operation": "reindex", "arguments": map[string]any{"path": f.worktree, "paths": []string{"edit.go"}}, "view": view,
	})
	require.False(t, recovery.IsError, "%+v", recovery.Content)
	require.Less(t, time.Since(started), time.Second, "scoped recovery must queue, not wait on the build lock")

	detected := f.facade(t, f.primary, "change", map[string]any{
		"operation": "detect", "target": map[string]any{"repo": "repo"}, "view": view,
	})
	diff := lifecycleResultPayload(t, detected)
	require.Equal(t, f.worktree, diff["repo_root"])
	require.Equal(t, false, diff["complete"])
	require.Equal(t, "UNKNOWN", diff["risk"])
	files, err := json.Marshal(diff["changed_files"])
	require.NoError(t, err)
	require.Contains(t, string(files), "edit.go")

	// A graph-dependent edit must refuse the pending exact view promptly, not
	// write through the primary fallback or wait behind the held build lock.
	started = time.Now()
	refused := f.facade(t, f.primary, "edit", map[string]any{
		"operation": "file", "target": map[string]any{"file": "repo/edit.go"},
		"match": "LifecycleBlocked", "replacement": "MustNotLand", "view": view, "require_exact": true,
	})
	require.True(t, refused.IsError)
	require.Less(t, time.Since(started), time.Second)
	current, err := os.ReadFile(filepath.Join(f.worktree, "edit.go"))
	require.NoError(t, err)
	require.Contains(t, string(current), "LifecycleBlocked")

	blocker.unblock()
	f.awaitMutation(t, f.primary, written)
	exact := f.facade(t, f.primary, "read", map[string]any{
		"operation": "source", "target": map[string]any{"symbol": "repo/edit.go::LifecycleBlocked"},
		"view": view, "require_exact": true,
	})
	require.False(t, exact.IsError, "%+v", exact.Content)
	require.Equal(t, true, resultFreshness(t, exact)["exact"])

	// Continue normally, without tracking, reapplying the first edit, or
	// restarting anything, and certify the second publication independently.
	second := f.facade(t, f.primary, "edit", map[string]any{
		"operation": "file", "target": map[string]any{"file": "repo/edit.go"},
		"match": "LifecycleBlocked", "replacement": "LifecycleContinued", "view": view,
	})
	f.awaitMutation(t, f.primary, second)
	primaryAfter, err := os.ReadFile(filepath.Join(f.primary, "edit.go"))
	require.NoError(t, err)
	require.Equal(t, primaryBefore, primaryAfter)
}

func BenchmarkWorktreeMutationEnqueueWithBlockedIndexer(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		f, blocker := newBlockedLifecycleFixture(b)
		b.StartTimer()
		written := f.edit(b, f.worktree, map[string]any{
			"path": "repo/edit.go", "old_string": "func New() {}", "new_string": "func LifecycleBlocked() {}",
		})
		b.StopTimer()
		payload := lifecycleResultPayload(b, written)
		require.Equal(b, "pending", payload["graph_status"])
		blocker.unblock()
		f.awaitMutation(b, f.worktree, written)
	}
}
