package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
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

func TestProjectedFileFactsDoNotClaimRelationsPublished(t *testing.T) {
	srv, dir := setupTestServer(t)
	done := make(chan indexer.MutationResult, 1)
	t.Cleanup(func() { close(done) })
	srv.SetWatcher(mutationTestWatcher{done: done})
	srv.mutationReindexWait = time.Nanosecond
	ack := extractTextResult(t, callTool(t, srv, "edit_file", map[string]any{
		"path": filepath.Join(dir, "main.go"), "old_string": "func helper() {}", "new_string": "func helper() {}\nfunc AddedForFacts() {}", "format": "json",
	}))
	require.Equal(t, true, ack["reindex_pending"])
	parent := srv.captureToolFreshness(t.Context())
	ctx, err := srv.ensureProjected(parent, []string{"main.go"})
	require.NoError(t, err)
	require.NotNil(t, srv.readerFor(ctx).GetNode("main.go::AddedForFacts"))
	require.Equal(t, "pending", srv.freshnessFor(parent, "relations", nil, []string{"main.go"}).state, "file projection does not publish cross-file relations")
	file := srv.freshnessFor(parent, "file", nil, []string{"main.go"})
	require.Empty(t, file.state, "current projected bytes satisfy only the file tier")
	fields := make(map[string]any)
	file.apply(fields)
	require.Equal(t, "projected", fields["file_view"])
	require.Equal(t, true, fields["readback_exact"])
}

func TestProjectedReadRendersExactFileFacts(t *testing.T) {
	srv, dir := setupTestServer(t)
	srv.SetWatcher(mutationTestWatcher{})
	path := filepath.Join(dir, "main.go")
	before, err := os.ReadFile(path)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, append(before, []byte("\nfunc AddedForFacts() {}\n")...), 0o644))
	result := projectedSourceThroughMiddleware(t, srv, map[string]any{"id": "main.go::AddedForFacts", "format": "json", "context_lines": float64(0)})
	require.False(t, result.IsError)
	payload := extractTextResult(t, result)
	facts, ok := payload["freshness"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "projected", facts["file_view"])
	require.Equal(t, true, facts["readback_exact"])
	require.NotContains(t, facts, "stale", "the old stat-only rider must not contradict a current projected file answer")
}

func TestProjectedFileFactsUseExistingRendererInEveryMode(t *testing.T) {
	for _, format := range []string{"json", "gcx", "toon"} {
		for _, compact := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/compact_%t", format, compact), func(t *testing.T) {
				srv, dir := setupTestServer(t)
				srv.SetWatcher(mutationTestWatcher{})
				path := filepath.Join(dir, "main.go")
				before, err := os.ReadFile(path)
				require.NoError(t, err)
				require.NoError(t, os.WriteFile(path, append(before, []byte("\nfunc AddedForFacts() {}\n")...), 0o644))
				result := projectedSourceThroughMiddleware(t, srv, map[string]any{"id": "main.go::AddedForFacts", "format": format, "compact": compact, "context_lines": float64(0)})
				require.False(t, result.IsError, "%v", result.Content)
				text := resultText(result)
				var payload map[string]any
				if json.Unmarshal([]byte(text), &payload) == nil && payload != nil {
					facts, ok := payload["freshness"].(map[string]any)
					require.True(t, ok)
					require.Equal(t, "projected", facts["file_view"])
					require.Equal(t, true, facts["readback_exact"])
				} else {
					require.Contains(t, text, "file: projected (readback exact)")
					require.Equal(t, 1, strings.Count(text, "file: projected (readback exact)"))
				}
				require.Nil(t, srv.graph.GetNode("main.go::AddedForFacts"))
			})
		}
	}
}

func TestUnchangedFileReadAddsNoProjectionFacts(t *testing.T) {
	srv, _ := setupTestServer(t)
	result := projectedSourceThroughMiddleware(t, srv, map[string]any{"id": "main.go::helper", "format": "json", "context_lines": float64(0)})
	require.False(t, result.IsError)
	require.NotContains(t, extractTextResult(t, result), "freshness", "unchanged indexed answers retain the existing bytes")
}

func projectedSourceThroughMiddleware(t *testing.T, s *Server, arguments map[string]any) *mcp.CallToolResult {
	t.Helper()
	req := editFileRequest(arguments)
	req.Params.Name = "get_symbol_source"
	result, err := s.wrapToolHandler(s.handleGetSymbolSource)(t.Context(), req)
	require.NoError(t, err)
	return result
}

// A file projection contains local edges, but it does not resolve the changed
// file's cross-file relations. This remains true for an external write with no
// watcher or mutation receipt supplying an independent pending label.
func TestProjectedRelationRichAnswersKeepRelationsPending(t *testing.T) {
	for _, tool := range []string{"get_symbol", "get_file_summary", "get_editing_context"} {
		t.Run(tool, func(t *testing.T) {
			srv, dir := setupTestServer(t)
			path := filepath.Join(dir, "main.go")
			before, err := os.ReadFile(path)
			require.NoError(t, err)
			require.NoError(t, os.WriteFile(path, append(before, []byte("\nfunc AddedRelation() { helper() }\n")...), 0o644))
			req := editFileRequest(map[string]any{"id": "main.go::AddedRelation", "path": "main.go", "detail": "full", "format": "json"})
			req.Params.Name = tool
			handlers := map[string]func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error){
				"get_symbol": srv.handleGetSymbol, "get_file_summary": srv.handleGetFileSummary, "get_editing_context": srv.handleGetEditingContext,
			}
			result, err := srv.wrapToolHandler(handlers[tool])(t.Context(), req)
			require.NoError(t, err)
			require.False(t, result.IsError, "%v", result.Content)
			facts, ok := extractTextResult(t, result)["freshness"].(map[string]any)
			require.True(t, ok, "exact file facts and unresolved relation scope must use the same rider")
			require.Equal(t, "relations", facts["freshness_scope"])
			require.Equal(t, "pending", facts["graph_freshness"])
			require.Equal(t, "projected", facts["file_view"])
			require.Equal(t, true, facts["readback_exact"])
			require.Nil(t, srv.graph.GetNode("main.go::AddedRelation"), "read must leave the shared graph unchanged")
		})
	}
}

func TestExternalProjectionSatisfiesOnlyExplicitFileTier(t *testing.T) {
	s, root := setupTestServer(t)
	path := filepath.Join(root, "main.go")
	before, err := os.ReadFile(path)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, append(before, []byte("\nfunc AddedForTiers() {}\n")...), 0o644))
	parent := withRequestProjectionFacts(t.Context())
	_, err = s.ensureProjected(parent, []string{"main.go"})
	require.NoError(t, err)
	for _, scope := range []string{"file", "relations", "derived"} {
		facts := s.freshnessFor(parent, scope, nil, []string{"main.go"})
		require.True(t, facts.projected)
		if scope == "file" {
			require.Empty(t, facts.state, "explicit file scope may describe exact local bytes")
		} else {
			require.Equal(t, "pending", facts.state, "a file projection cannot certify %s publication", scope)
			require.Equal(t, []string{"main.go"}, facts.files)
		}
	}
	require.Nil(t, s.graph.GetNode("main.go::AddedForTiers"))
}
