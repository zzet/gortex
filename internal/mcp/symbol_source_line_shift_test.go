package mcp

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestSymbolSourceSingleRepoWatcherLineShift reads a symbol whose file was
// edited, shifting its lines, while the watcher that owns freshness has not
// reindexed the file yet. The answer must be the symbol's current body and
// range, never the old range cut out of the new bytes.
func TestSymbolSourceSingleRepoWatcherLineShift(t *testing.T) {
	srv, dir := setupTestServer(t)
	srv.SetWatcher(mutationTestWatcher{})
	shiftTestServerMain(t, dir)

	resp := extractTextResult(t, callTool(t, srv, "get_symbol_source", map[string]any{
		"id":            "main.go::helper",
		"context_lines": float64(0),
	}))
	require.Equal(t, "func helper() {}", resp["source"])
	require.EqualValues(t, 13, resp["start_line"])
	require.EqualValues(t, 13, resp["end_line"])
}
