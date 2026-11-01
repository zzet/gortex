package mcp

import (
	"testing"

	mcplib "github.com/mark3labs/mcp-go/mcp"

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

// The other symbol-source consumers must relocate the same stale node before
// slicing, including CFG, whose statement offsets refer to the current file.
func TestSymbolSourceConsumersWatcherLineShift(t *testing.T) {
	for _, tool := range []string{"batch_symbols", "suggest_pattern", "smart_context"} {
		t.Run(tool, func(t *testing.T) {
			srv, dir := setupTestServer(t)
			srv.SetWatcher(mutationTestWatcher{})
			shiftTestServerMain(t, dir)
			args := map[string]any{"id": "main.go::helper", "ids": []any{"main.go::helper"}, "include_source": true, "context_lines": float64(0), "task": "helper", "format": "json"}
			var result *mcplib.CallToolResult
			if tool == "suggest_pattern" {
				req := mcplib.CallToolRequest{}
				req.Params.Arguments = args
				var err error
				result, err = srv.handleSuggestPattern(t.Context(), req)
				require.NoError(t, err)
			} else {
				result = callTool(t, srv, tool, args)
			}
			resp := extractTextResult(t, result)
			switch tool {
			case "batch_symbols":
				rows := resp["symbols"].([]any)
				require.Len(t, rows, 1)
				row := rows[0].(map[string]any)
				require.Equal(t, "func helper() {}", row["source"])
				require.EqualValues(t, 13, row["start_line"])
				require.EqualValues(t, 13, row["end_line"])
			case "suggest_pattern":
				require.Equal(t, "func helper() {}", resp["example_source"])
				rows, ok := resp["registration"].([]any)
				require.True(t, ok)
				require.NotEmpty(t, rows, "exercise a caller source as well as the example")
				for _, raw := range rows {
					row := raw.(map[string]any)
					if row["id"] == "main.go::main" {
						require.Equal(t, "func main() {\n\thelper()\n}", row["source"])
						require.EqualValues(t, 9, row["start_line"])
					}
				}
			case "smart_context":
				rows, ok := resp["relevant_symbols"].([]any)
				require.True(t, ok, "response: %v", resp)
				var found bool
				for _, raw := range rows {
					row := raw.(map[string]any)
					if row["id"] == "main.go::helper" {
						found = true
						require.Equal(t, "func helper() {}", row["source"])
						require.EqualValues(t, 13, row["start_line"])
					}
				}
				require.True(t, found, "helper must be selected so the source assertion is exercised")
			}
		})
	}
	t.Run("cfg", func(t *testing.T) {
		srv, dir := setupTestServer(t)
		srv.SetWatcher(mutationTestWatcher{})
		shiftTestServerMain(t, dir)
		sc, err := srv.buildSymbolCFG(t.Context(), "main.go::helper")
		require.NoError(t, err)
		require.Equal(t, 13, sc.node.StartLine)
		require.Equal(t, 13, sc.node.EndLine)
		require.Equal(t, "helper", sc.graph.FuncName)
	})
}
