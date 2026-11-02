package mcp

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"
)

func TestUntrackedFileReadableViaSummary(t *testing.T) {
	selectedProjectionRegistry(t)
	s, root := setupTestServer(t)
	s.SetWatcher(mutationTestWatcher{})
	path := filepath.Join(root, "fresh.go")
	raw := "package main\n\nfunc FreshRead() int { return 7 }\n\nfunc FreshSibling() {}\n"
	require.NoError(t, os.WriteFile(path, []byte(raw), 0600))
	require.False(t, s.indexer.IsTracked("fresh.go"), "native-created file must not be in the tracked inventory")
	require.Nil(t, s.graph.GetNode("fresh.go::FreshRead"))
	req := mcplib.CallToolRequest{}
	req.Params.Name = "get_file_summary"
	req.Params.Arguments = map[string]any{"path": "fresh.go", "format": "json"}
	handler := s.boundToolHandler(s.wrapToolHandler(s.handleGetFileSummary))
	result, err := handler(context.Background(), req)
	require.NoError(t, err)
	require.False(t, result.IsError, "%s", resultText(result))
	payload := extractTextResult(t, result)
	nodes, ok := payload["nodes"].([]any)
	require.True(t, ok, "%v", payload)
	require.Len(t, nodes, 2, "the actual summary must expose current definitions, not file_not_indexed guidance")
	found := make(map[string]map[string]any)
	for _, value := range nodes {
		n := value.(map[string]any)
		found[n["id"].(string)] = n
	}
	require.Contains(t, found, "fresh.go::FreshRead")
	require.Equal(t, float64(3), found["fresh.go::FreshRead"]["start_line"])
	require.Equal(t, float64(3), found["fresh.go::FreshRead"]["end_line"])
	require.Contains(t, found, "fresh.go::FreshSibling")
	require.Equal(t, true, payload["readback_exact"])
	require.Equal(t, "projected", payload["file_view"])
	require.Nil(t, s.graph.GetNode("fresh.go::FreshRead"), "a read cannot publish the projection into the base graph")
	require.False(t, s.indexer.IsTracked("fresh.go"), "projection read-back does not mutate indexer inventory")
	source := callTool(t, s, "get_symbol_source", map[string]any{"id": "fresh.go::FreshRead", "context_lines": 0, "format": "json"})
	require.False(t, source.IsError, "%s", resultText(source))
	require.Equal(t, "func FreshRead() int { return 7 }", extractTextResult(t, source)["source"])
}
