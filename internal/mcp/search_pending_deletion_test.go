package mcp

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/query"
	"github.com/zzet/gortex/internal/search"
)

func TestPendingTextAndSymbolSearchHideDeletedDeclarationBeforePublication(t *testing.T) {
	previous := pendingFileProjections
	pendingFileProjections = newPendingProjections()
	t.Cleanup(func() { pendingFileProjections = previous })
	root := t.TempDir()
	path := filepath.Join(root, "main.go")
	const removed = "func DeletedPendingDeclaration() { println(\"RemovedPendingLiteral\") }"
	require.NoError(t, os.WriteFile(path, []byte("package main\n\nfunc SurvivingDeclaration() {}\n\n"+removed+"\n"), 0o600))
	store, err := store_sqlite.Open(filepath.Join(t.TempDir(), "native.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	idx := indexer.New(store, testRegistry(), config.Default().Index, zap.NewNop())
	t.Cleanup(idx.Close)
	_, err = idx.Index(root)
	require.NoError(t, err)
	backend := search.NewSymbolSearcherBackend(store)
	require.NotEmpty(t, backend.Search("DeletedPendingDeclaration", 10), "native symbol corpus must actually answer the old name")
	for range 4 {
		require.Len(t, idx.GrepText("RemovedPendingLiteral", 10), 1)
	}
	engine := query.NewEngine(store)
	engine.SetSearch(backend)
	server := NewServer(engine, store, idx, nil, zap.NewNop(), nil, MultiRepoOptions{EditAckMode: config.EditAckLocal})
	done := make(chan indexer.MutationResult, 1)
	t.Cleanup(func() { close(done) })
	server.SetWatcher(mutationTestWatcher{done: done})
	ctx := WithSessionID(t.Context(), "pending-delete-negative")
	ack := callToolByName(t, server, ctx, "edit_file", map[string]any{"path": path, "old_string": removed, "new_string": "", "format": "json"})
	require.False(t, ack.IsError, "%v", ack.Content)
	require.Equal(t, "local", extractTextResult(t, ack)["ack_mode"])
	for _, probe := range []struct{ tool, query, field string }{
		{"search_symbols", "DeletedPendingDeclaration", "results"},
		{"search_text", "RemovedPendingLiteral", "matches"},
	} {
		answer := callToolByName(t, server, ctx, probe.tool, map[string]any{"query": probe.query, "format": "json", "limit": 100, "max_bytes": 0})
		require.False(t, answer.IsError, "%s: %v", probe.tool, answer.Content)
		body := extractTextResult(t, answer)
		raw, exists := body[probe.field]
		require.True(t, exists, "missing payload cannot prove absence")
		if raw != nil {
			rows, ok := raw.([]any)
			require.True(t, ok, "absence needs the actual result collection")
			if probe.tool == "search_symbols" {
				for _, item := range rows {
					row, isRow := item.(map[string]any)
					require.True(t, isRow)
					require.NotEqual(t, "main.go::DeletedPendingDeclaration", row["id"], "old native identity survived deletion")
					require.NotEqual(t, "DeletedPendingDeclaration", row["name"], "old native name survived deletion")
				}
				require.Equal(t, float64(len(rows)), body["total"], "the complete negative page must retain all legitimate other matches")
			} else {
				require.Empty(t, rows, "text search returned the deleted declaration's unique old literal")
			}
		}
		if probe.tool == "search_symbols" {
			if raw == nil {
				require.Equal(t, float64(0), body["total"], "an explicit empty collection still needs an exact zero total")
			}
			require.Equal(t, false, body["truncated"])
		} else {
			require.Equal(t, float64(0), body["count"])
		}
	}
	require.NotNil(t, store.GetNode("main.go::DeletedPendingDeclaration"), "publication must still be held")
	require.NotEmpty(t, backend.Search("DeletedPendingDeclaration", 10), "old native symbol postings must still exist")
	// Public text search may legitimately refresh its native cache. The old
	// graph and native symbol posting still prove publication stayed held.

}
