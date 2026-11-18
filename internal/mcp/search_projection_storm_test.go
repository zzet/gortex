package mcp

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/query"
	"github.com/zzet/gortex/internal/search"
)

// The existing scoped storm-query seam is controlled; the edit, projection,
// native symbol backend and public handlers are real. This is an ownership
// regression, not a storm workload or a performance observation.
func TestNativeSearchACKProjectionSurvivesScopedQueuedStorm(t *testing.T) {
	previous := pendingFileProjections
	pendingFileProjections = newPendingProjections()
	t.Cleanup(func() { pendingFileProjections = previous })
	root := t.TempDir()
	path := filepath.Join(root, "main.go")
	const beforeName = "OriginalStormBefore"
	const currentName = "QueuedProjectionZebraQuartz"
	require.NoError(t, os.WriteFile(path, []byte("package main\n\nfunc "+beforeName+"() {}\n"), 0o600))
	store, err := store_sqlite.Open(filepath.Join(t.TempDir(), "native.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	idx := indexer.New(store, testRegistry(), config.Default().Index, zap.NewNop())
	t.Cleanup(idx.Close)
	_, err = idx.Index(root)
	require.NoError(t, err)
	backend := search.NewSymbolSearcherBackend(store)
	require.NotEmpty(t, backend.Search(beforeName, 10), "the native backend must actually hold the old declaration")
	engine := query.NewEngine(store)
	engine.SetSearch(backend)
	server := NewServer(engine, store, idx, nil, zap.NewNop(), nil, MultiRepoOptions{EditAckMode: config.EditAckLocal})
	done := make(chan indexer.MutationResult, 1)
	t.Cleanup(func() { close(done) })
	server.SetWatcher(projectedStormWatcher{mutationTestWatcher: mutationTestWatcher{done: done}, path: path})
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	ackResult := callToolByName(t, server, ctx, "edit_file", map[string]any{"path": path, "old_string": beforeName, "new_string": currentName, "format": "json"})
	require.False(t, ackResult.IsError, "%v", ackResult.Content)
	ack := extractTextResult(t, ackResult)
	require.Equal(t, "local", ack["ack_mode"])
	require.Equal(t, true, ack["readback_exact"])
	view, ok := ack["file_view"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "projected", view["state"])
	entry := liveEntry(pendingFileProjections, path)
	require.NotNil(t, entry)
	require.True(t, liveProjectionForAcknowledgement(entry))
	require.Empty(t, backend.Search(currentName, 10), "canonical postings cannot supply the new symbol")

	// No file/source read precedes the two public searches.
	result := callToolByName(t, server, ctx, "search_symbols", map[string]any{"query": currentName, "query_class": "symbol", "expand": "off", "limit": 1, "max_bytes": 0, "format": "json"})
	require.False(t, result.IsError, "%v", result.Content)
	rows, ok := extractTextResult(t, result)["results"].([]any)
	require.True(t, ok)
	require.Len(t, rows, 1)
	row := rows[0].(map[string]any)
	require.Equal(t, "main.go::"+currentName, row["id"])
	require.Equal(t, "main.go", row["file_path"])
	require.Equal(t, float64(3), row["start_line"])
	result = callToolByName(t, server, ctx, "search_text", map[string]any{"query": "func " + currentName + "(", "limit": 10, "max_bytes": 0, "format": "json"})
	require.False(t, result.IsError, "%v", result.Content)
	rows, ok = extractTextResult(t, result)["matches"].([]any)
	require.True(t, ok)
	require.Len(t, rows, 1)
	row = rows[0].(map[string]any)
	require.Equal(t, "main.go", row["path"])
	require.Equal(t, float64(3), row["line"])
	require.Contains(t, row["text"], "func "+currentName+"(")
	result = callToolByName(t, server, ctx, "get_symbol_source", map[string]any{"id": "main.go::" + currentName, "context_lines": float64(0), "format": "json"})
	require.False(t, result.IsError, "%v", result.Content)
	require.Equal(t, "func "+currentName+"() {}", extractTextResult(t, result)["source"])
	require.Same(t, entry, liveEntry(pendingFileProjections, path), "a valid physical ACK keeps its immutable owner during queued work")
	require.True(t, liveProjectionForAcknowledgement(entry))
	require.Nil(t, store.GetNode("main.go::"+currentName), "a read must not publish the pending declaration")
	require.NotNil(t, store.GetNode("main.go::"+beforeName))
}
