package mcp

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/indexer"
)

func searchProjectedAck(t *testing.T, server *Server, root, name string) int {
	t.Helper()
	before, err := os.ReadFile(filepath.Join(root, "main.go"))
	require.NoError(t, err)
	offset := strings.Index(string(before), "func helper() {}")
	require.GreaterOrEqual(t, offset, 0)
	expectedLine := strings.Count(string(before[:offset]), "\n") + 2
	done := make(chan indexer.MutationResult, 1)
	t.Cleanup(func() { close(done) })
	server.SetWatcher(mutationTestWatcher{done: done})
	server.mutationReindexWait = time.Nanosecond
	result := callTool(t, server, "edit_file", map[string]any{"path": filepath.Join(root, "main.go"), "old_string": "func helper() {}", "new_string": "func helper() {}\nfunc " + name + "() {}", "format": "json"})
	require.False(t, result.IsError, "%v", result.Content)
	ack := extractTextResult(t, result)
	require.Equal(t, true, ack["readback_exact"])
	require.Equal(t, "projected", ack["file_view"])
	require.Nil(t, server.graph.GetNode("main.go::"+name), "held watcher must not have published the base graph")
	return expectedLine
}

func TestSearchSymbolsSeesActualACKProjectionBeforeAnyReadOrGraphPublication(t *testing.T) {
	previous := pendingFileProjections
	pendingFileProjections = newPendingProjections()
	t.Cleanup(func() { pendingFileProjections = previous })
	server, root := setupTestServer(t)
	expectedLine := searchProjectedAck(t, server, root, "ACKSearchOnly")
	result := callTool(t, server, "search_symbols", map[string]any{"query": "ACKSearchOnly", "query_class": "symbol", "expand": "off", "max_bytes": 0, "limit": 1000})
	require.False(t, result.IsError, "%v", result.Content)
	payload := extractTextResult(t, result)
	rows, _ := payload["results"].([]any)
	found := false
	for _, raw := range rows {
		row := raw.(map[string]any)
		if row["id"] == "main.go::ACKSearchOnly" {
			found = row["file_path"] == "main.go" && row["start_line"] == float64(expectedLine)
		}
	}
	require.True(t, found, "actual acknowledged projection was absent or had wrong file:line: %v", payload)
	require.Nil(t, server.graph.GetNode("main.go::ACKSearchOnly"), "search must not publish a graph mutation")
}

func TestSearchSymbolsNeverBorrowsSiblingPhysicalRootWithSamePrefix(t *testing.T) {
	previous := pendingFileProjections
	pendingFileProjections = newPendingProjections()
	t.Cleanup(func() { pendingFileProjections = previous })
	selected, selectedRoot := setupTestServer(t)
	sibling, siblingRoot := setupTestServer(t)
	searchProjectedAck(t, selected, selectedRoot, "SelectedACK")
	searchProjectedAck(t, sibling, siblingRoot, "SiblingACK")
	result := callTool(t, selected, "search_symbols", map[string]any{"query": "SiblingACK", "query_class": "symbol", "expand": "off", "max_bytes": 0, "limit": 1000})
	require.False(t, result.IsError, "%v", result.Content)
	rows, _ := extractTextResult(t, result)["results"].([]any)
	for _, raw := range rows {
		row := raw.(map[string]any)
		require.NotEqual(t, "main.go::SiblingACK", row["id"], "same graph prefix cannot authorize a different physical checkout's projection")
	}
}
