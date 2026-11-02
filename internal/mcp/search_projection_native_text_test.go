package mcp

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/query"
	"github.com/zzet/gortex/internal/reach"
)

// The first public post-ACK search must find a new trigram in the live native
// cache. A deliberately unannounced write first proves that streaming or old
// postings cannot satisfy this fixture. No file filter or intervening read is
// allowed to repair the measured search.
func TestSearchTextFindsNewTrigramOnFirstLocalACKBeforeBasePublication(t *testing.T) {
	previous := pendingFileProjections
	pendingFileProjections = newPendingProjections()
	t.Cleanup(func() { pendingFileProjections = previous })
	root := t.TempDir()
	path := filepath.Join(root, "main.go")
	const oldName = "OriginalNativeTextBefore"
	const newName = "PendingZebraQuartzAck"
	original := []byte("package main\n\nfunc " + oldName + "() {}\n")
	updated := []byte(strings.ReplaceAll(string(original), oldName, newName))
	require.NoError(t, os.WriteFile(path, original, 0o600))
	stamp, err := os.Stat(path)
	require.NoError(t, err)
	store, err := store_sqlite.Open(filepath.Join(t.TempDir(), "native.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	idx := indexer.New(store, testRegistry(), config.Default().Index, zap.NewNop())
	t.Cleanup(idx.Close)
	_, err = idx.Index(root)
	require.NoError(t, err)
	for range 4 {
		require.Len(t, idx.GrepText("func "+oldName+"(", 10), 1)
	}
	require.NoError(t, os.WriteFile(path, updated, 0o600))
	require.Empty(t, idx.GrepText("func "+newName+"(", 10), "unannounced new trigram must be absent: a streaming fallback cannot establish this native-cache control")
	require.NoError(t, os.WriteFile(path, original, 0o600))
	require.NoError(t, os.Chtimes(path, stamp.ModTime(), stamp.ModTime()))
	server := NewServer(query.NewEngine(store), store, idx, nil, zap.NewNop(), nil, MultiRepoOptions{EditAckMode: config.EditAckLocal})
	watcher, err := indexer.NewWatcher(idx, config.WatchConfig{Enabled: true, DebounceMs: 60000}, zap.NewNop())
	require.NoError(t, err)
	server.SetWatcher(watcher)
	require.NoError(t, watcher.Start([]string{root}))
	t.Cleanup(func() { require.NoError(t, watcher.Stop()) })
	// This actual topology writer gate precedes patching, unlike ResolveMutex,
	// which is acquired only after the base declaration may already be replaced.
	finish := reach.BeginTopologyMutation(store)
	release := sync.OnceFunc(func() { finish(false) })
	t.Cleanup(release)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	result := callToolByName(t, server, ctx, "edit_file", map[string]any{"path": path, "old_string": oldName, "new_string": newName, "format": "json"})
	require.False(t, result.IsError, "%v", result.Content)
	ack := extractTextResult(t, result)
	require.Equal(t, "local", ack["ack_mode"])
	require.IsType(t, map[string]any{}, ack["file_view"])
	// This is the first public read/search after the actual ACK.
	result = callToolByName(t, server, ctx, "search_text", map[string]any{"query": "func " + newName + "(", "limit": 10, "max_bytes": 0, "format": "json"})
	require.False(t, result.IsError, "%v", result.Content)
	rows, _ := extractTextResult(t, result)["matches"].([]any)
	require.Len(t, rows, 1)
	row := rows[0].(map[string]any)
	require.Equal(t, "main.go", row["path"])
	require.Equal(t, float64(3), row["line"])
	require.Contains(t, row["text"], "func "+newName+"(")
	require.Nil(t, store.GetNode("main.go::"+newName), "the first search must not wait for or cause base publication")
	require.NotNil(t, store.GetNode("main.go::"+oldName))
}
