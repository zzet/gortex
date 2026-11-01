package mcp

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/indexer"
)

// A read of a tracked file must see the current file-local symbols while the
// watcher still owns an older graph, without synchronously reindexing it.
func TestProjectedReadFindsNewSymbolWithoutMutatingBase(t *testing.T) {
	srv, dir := setupTestServer(t)
	srv.SetWatcher(mutationTestWatcher{})
	path := filepath.Join(dir, "main.go")
	original, err := os.ReadFile(path)
	require.NoError(t, err)
	for _, body := range []string{"return 1", "return 2"} {
		current := string(original) + "\nfunc Added() int { " + body + " }\n"
		require.NoError(t, os.WriteFile(path, []byte(current), 0o644))
		result := callTool(t, srv, "get_symbol_source", map[string]any{"id": "main.go::Added", "context_lines": float64(0), "format": "json"})
		require.False(t, result.IsError, "current symbol must be projected: %v", result.Content)
		payload := extractTextResult(t, result)
		require.Equal(t, "func Added() int { "+body+" }", payload["source"])
		require.Nil(t, srv.graph.GetNode("main.go::Added"), "read-through must not publish into the shared base graph")
	}
}

func TestProjectedReadAfterPendingToolAckDoesNotPublishBase(t *testing.T) {
	srv, dir := setupTestServer(t)
	done := make(chan indexer.MutationResult, 1)
	t.Cleanup(func() { close(done) })
	srv.SetWatcher(mutationTestWatcher{done: done})
	srv.mutationReindexWait = time.Nanosecond
	replacement := "func helper() {}\nfunc AddedByTool() int { return 7 }"
	ack := extractTextResult(t, callTool(t, srv, "edit_file", map[string]any{
		"path": filepath.Join(dir, "main.go"), "old_string": "func helper() {}", "new_string": replacement, "format": "json",
	}))
	require.Equal(t, true, ack["reindex_pending"])
	result := callTool(t, srv, "get_symbol_source", map[string]any{
		"id": "main.go::AddedByTool", "context_lines": float64(0), "format": "json",
	})
	require.False(t, result.IsError, "acknowledged source is readable while graph publication is pending: %v", result.Content)
	require.Equal(t, "func AddedByTool() int { return 7 }", extractTextResult(t, result)["source"])
	require.Nil(t, srv.graph.GetNode("main.go::AddedByTool"), "a read must not mutate the shared graph")
}

func TestProjectedReadDeletionHidesOldSymbolWithoutRemovingBase(t *testing.T) {
	srv, dir := setupTestServer(t)
	srv.SetWatcher(mutationTestWatcher{})
	require.NotNil(t, srv.graph.GetNode("main.go::helper"), "prove the indexed symbol exists")
	require.NoError(t, os.Remove(filepath.Join(dir, "main.go")))
	result := callTool(t, srv, "get_symbol", map[string]any{"id": "main.go::helper", "format": "json"})
	require.Equal(t, "symbol_not_found", extractTextResult(t, result)["condition"], "a missing current file must not serve its old symbol")
	require.NotNil(t, srv.graph.GetNode("main.go::helper"), "the read-side tombstone must not mutate base")
}

func TestProjectedReadRechecksSameStampBytes(t *testing.T) {
	srv, dir := setupTestServer(t)
	srv.SetWatcher(mutationTestWatcher{})
	path := filepath.Join(dir, "main.go")
	original, err := os.ReadFile(path)
	require.NoError(t, err)
	stamp := time.Now().Add(-time.Hour)
	for _, body := range []string{"return 1", "return 2"} {
		require.NoError(t, os.WriteFile(path, append(append([]byte(nil), original...), []byte("\nfunc Added() int { "+body+" }\n")...), 0o644))
		require.NoError(t, os.Chtimes(path, stamp, stamp))
		result := callTool(t, srv, "get_symbol_source", map[string]any{"id": "main.go::Added", "context_lines": float64(0), "format": "json"})
		require.False(t, result.IsError, "%v", result.Content)
		require.Equal(t, "func Added() int { "+body+" }", extractTextResult(t, result)["source"])
	}
	require.Nil(t, srv.graph.GetNode("main.go::Added"))
}

func TestProjectedReadCancellationDoesNotPublish(t *testing.T) {
	srv, dir := setupTestServer(t)
	path := filepath.Join(dir, "main.go")
	before, err := os.ReadFile(path)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, append(before, []byte("\nfunc Added() {}\n")...), 0o644))
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = srv.ensureProjected(ctx, []string{"main.go"})
	require.ErrorIs(t, err, context.Canceled)
	require.Nil(t, srv.graph.GetNode("main.go::Added"))
}

func TestProjectedDiskLayerStaysBelowPinnedEditorLayer(t *testing.T) {
	srv, dir := setupTestServer(t)
	path := filepath.Join(dir, "main.go")
	before, err := os.ReadFile(path)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, append(before, []byte("\nfunc AddedOnDisk() {}\n")...), 0o644))
	layer := graph.NewOverlayLayer()
	layer.MarkFile("main.go", false)
	bufferNode := &graph.Node{ID: "main.go::InBuffer", Name: "InBuffer", FilePath: "main.go", Kind: graph.KindFunction}
	layer.AddNode("main.go", bufferNode)
	ctx := WithOverlayView(t.Context(), graph.NewOverlaidView(srv.graph, layer))
	ctx, err = srv.ensureProjected(ctx, []string{"main.go"})
	require.NoError(t, err)
	require.Equal(t, bufferNode, srv.readerFor(ctx).GetNode(bufferNode.ID))
	require.Nil(t, srv.readerFor(ctx).GetNode("main.go::AddedOnDisk"))
	require.Nil(t, srv.graph.GetNode("main.go::AddedOnDisk"))
}

func TestPinnedEditorFileNeedsNoDiskProjection(t *testing.T) {
	srv, dir := setupTestServer(t)
	path := filepath.Join(dir, "main.go")
	// The pinned editor snapshot owns this entire file. Oversized disk bytes
	// cannot contribute an answer and must not consume its projection budget.
	require.NoError(t, os.WriteFile(path, make([]byte, (1<<20)+1), 0o644))
	layer := graph.NewOverlayLayer()
	layer.MarkFile("main.go", false)
	bufferNode := &graph.Node{ID: "main.go::InBuffer", Name: "InBuffer", FilePath: "main.go", Kind: graph.KindFunction}
	layer.AddNode("main.go", bufferNode)
	pinned := graph.NewOverlaidView(srv.graph, layer)
	ctx := WithOverlayView(t.Context(), pinned)
	ctx, err := srv.ensureProjected(ctx, []string{"main.go"})
	require.NoError(t, err)
	require.Same(t, pinned, OverlayViewFromContext(ctx))
	require.Empty(t, requestProjectionLayers(ctx))
	require.Same(t, bufferNode, srv.readerFor(ctx).GetNode(bufferNode.ID))
}
