package mcp

import (
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

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
	result := callTool(t, server, "edit_file", map[string]any{"path": filepath.Join(root, "main.go"), "old_string": "func helper() {}", "new_string": "func helper() {}\nfunc " + name + "() {}", "format": "json"})
	require.False(t, result.IsError, "%v", result.Content)
	ack := extractTextResult(t, result)
	require.Equal(t, true, ack["readback_exact"])
	require.Equal(t, "local", ack["ack_mode"], "the fixture must exercise the configured fast ACK")
	view, ok := ack["file_view"].(map[string]any)
	require.True(t, ok, "the current ACK contract is an object, never the old scalar draft")
	require.Equal(t, "projected", view["state"])
	require.Contains(t, view, "counts")
	require.Contains(t, view, "symbols")
	require.Contains(t, view, "truncated")
	syntax, ok := view["syntax_health"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "checked", syntax["state"])
	require.Equal(t, true, syntax["checked"])
	require.Zero(t, syntax["errors"])
	hash, _ := ack["content_sha256"].(string)
	decodedHash, err := hex.DecodeString(hash)
	require.NoError(t, err)
	require.Len(t, decodedHash, 32, "actual committed SHA256 must be represented")
	require.NotEmpty(t, ack["graph_status_hint"], "retain the actual advisory hint without treating it as graph completion")
	require.Nil(t, server.graph.GetNode("main.go::"+name), "held watcher must not have published the base graph")
	return expectedLine
}

func TestSearchSymbolsSeesActualACKProjectionBeforeAnyReadOrGraphPublication(t *testing.T) {
	previous := pendingFileProjections
	pendingFileProjections = newPendingProjections()
	t.Cleanup(func() { pendingFileProjections = previous })
	server, root := ackModeServer(t, "local")
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
	selected, selectedRoot := ackModeServer(t, "local")
	sibling, siblingRoot := ackModeServer(t, "local")
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
