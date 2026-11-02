package mcp

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"
)

type projectedStormWatcher struct {
	mutationTestWatcher
	path string
}

func (w projectedStormWatcher) StormActiveForFile(path string) bool { return path == w.path }

func TestReadDuringStormLabelsPending(t *testing.T) {
	selectedProjectionRegistry(t)
	s, root := setupTestServer(t)
	path := filepath.Join(root, "main.go")
	before, err := os.ReadFile(path)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, append(before, []byte("\nfunc DuringStorm() {}\n")...), 0600))
	s.SetWatcher(projectedStormWatcher{path: path})
	req := mcplib.CallToolRequest{}
	req.Params.Name = "get_file_summary"
	req.Params.Arguments = map[string]any{"path": "main.go", "format": "json"}
	result, err := s.wrapToolHandler(s.handleGetFileSummary)(context.Background(), req)
	require.NoError(t, err)
	require.False(t, result.IsError, "%s", resultText(result))
	fields := extractTextResult(t, result)
	freshness, ok := fields["freshness"].(map[string]any)
	require.True(t, ok, "%v", fields)
	require.Equal(t, "pending", freshness["graph_freshness"])
	require.Equal(t, "storm", freshness["file_reason"])
	require.NotEqual(t, true, freshness["readback_exact"])
	require.Empty(t, pendingFileProjections.snap.Load().entries, "storm read must not publish a projection")
	require.Nil(t, s.graph.GetNode("main.go::DuringStorm"))
}

func TestStormReadDoesNotHideAnUnrelatedFile(t *testing.T) {
	selectedProjectionRegistry(t)
	s, root := setupTestServer(t)
	path := filepath.Join(root, "main.go")
	before, err := os.ReadFile(path)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, append(before, []byte("\nfunc OutsideStorm() {}\n")...), 0600))
	s.SetWatcher(projectedStormWatcher{path: filepath.Join(root, "other.go")})
	req := mcplib.CallToolRequest{}
	req.Params.Name = "get_file_summary"
	req.Params.Arguments = map[string]any{"path": "main.go", "format": "json"}
	result, err := s.wrapToolHandler(s.handleGetFileSummary)(t.Context(), req)
	require.NoError(t, err)
	require.False(t, result.IsError, "%s", resultText(result))
	fields := extractTextResult(t, result)
	freshness := fields["freshness"].(map[string]any)
	require.Equal(t, true, freshness["readback_exact"])
	require.Equal(t, "projected", freshness["file_view"])
	require.NotContains(t, freshness, "file_reason")
	require.NotEmpty(t, pendingFileProjections.snap.Load().entries)
	require.Nil(t, s.graph.GetNode("main.go::OutsideStorm"))
}
