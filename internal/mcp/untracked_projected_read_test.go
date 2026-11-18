package mcp

import (
	"context"
	"github.com/zzet/gortex/internal/pathkey"
	"os"
	"path/filepath"
	"strings"
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
	facts := payload["freshness"].(map[string]any)
	require.Equal(t, true, facts["readback_exact"])
	require.Equal(t, "projected", facts["file_view"])
	require.Equal(t, "pending", facts["graph_freshness"])
	require.Nil(t, s.graph.GetNode("fresh.go::FreshRead"), "a read cannot publish the projection into the base graph")
	require.False(t, s.indexer.IsTracked("fresh.go"), "projection read-back does not mutate indexer inventory")
	source := callTool(t, s, "get_symbol_source", map[string]any{"id": "fresh.go::FreshRead", "context_lines": 0, "format": "json"})
	require.False(t, source.IsError, "%s", resultText(source))
	require.Equal(t, "func FreshRead() int { return 7 }", extractTextResult(t, source)["source"])
}

func TestUntrackedProjectionPreservesSourceAdmission(t *testing.T) {
	for _, tc := range []struct {
		name, path string
		raw        []byte
	}{
		{"git_metadata", ".git/hidden.go", []byte("package main\nfunc Hidden() {}\n")},
		{"ignored", "hidden.go", []byte("package main\nfunc Hidden() {}\n")},
		{"unsupported", "hidden.zz_unknown", []byte("package main\nfunc Hidden() {}\n")},
		{"binary", "hidden.go", []byte("package main\nfunc Hidden() {}\n\x00\x00")},
		{"oversize", "hidden.go", []byte(strings.Repeat(" ", (1<<20)+1))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			selectedProjectionRegistry(t)
			s, root := setupTestServer(t)
			if tc.name == "ignored" {
				require.NoError(t, os.WriteFile(filepath.Join(root, ".gortexignore"), []byte("hidden.go\n"), 0600))
			}
			path := filepath.Join(root, filepath.FromSlash(tc.path))
			require.NoError(t, os.MkdirAll(filepath.Dir(path), 0700))
			require.NoError(t, os.WriteFile(path, tc.raw, 0600))
			_, err := s.ensureProjected(t.Context(), []string{tc.path})
			require.Error(t, err)
			require.Nil(t, pendingFileProjections.snap.Load().entries[pathkey.MutationKey(path)])
		})
	}
}

func TestUntrackedProjectionConfinesSelectedCheckout(t *testing.T) {
	for _, kind := range []string{"leaf", "parent", "absolute"} {
		t.Run(kind, func(t *testing.T) {
			selectedProjectionRegistry(t)
			s, root := setupTestServer(t)
			other := t.TempDir()
			raw := []byte("package main\nfunc ForeignSecret() {}\n")
			require.NoError(t, os.WriteFile(filepath.Join(other, "secret.go"), raw, 0600))
			path := "secret.go"
			switch kind {
			case "leaf":
				if err := os.Symlink(filepath.Join(other, "secret.go"), filepath.Join(root, path)); err != nil {
					t.Skipf("symlink unavailable: %v", err)
				}
			case "parent":
				if err := os.Symlink(other, filepath.Join(root, "escape")); err != nil {
					t.Skipf("symlink unavailable: %v", err)
				}
				path = "escape/secret.go"
			case "absolute":
				path = filepath.Join(other, "secret.go")
			}
			_, err := s.ensureProjected(t.Context(), []string{path})
			require.Error(t, err)
			require.Empty(t, pendingFileProjections.snap.Load().entries)
		})
	}
}

func TestUntrackedProjectionUsesSelectedCheckout(t *testing.T) {
	selectedProjectionRegistry(t)
	s, _ := setupTestServer(t)
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "fresh.go"), []byte("package main\nfunc SelectedOnly() {}\n"), 0600))
	ctx := withRequestView(t.Context(), &requestView{kind: requestViewKindWorktree, viewRoot: root, reader: s.graph})
	ctx, err := s.ensureProjected(ctx, []string{"fresh.go"})
	require.NoError(t, err)
	require.NotNil(t, s.readerFor(ctx).GetNode("fresh.go::SelectedOnly"))
	require.Nil(t, s.graph.GetNode("fresh.go::SelectedOnly"))
}

func TestUntrackedProjectionDeletionHidesPriorRead(t *testing.T) {
	selectedProjectionRegistry(t)
	s, root := setupTestServer(t)
	path := filepath.Join(root, "fresh.go")
	require.NoError(t, os.WriteFile(path, []byte("package main\nfunc BeforeDelete() {}\n"), 0600))
	ctx, err := s.ensureProjected(t.Context(), []string{"fresh.go"})
	require.NoError(t, err)
	require.NotNil(t, s.readerFor(ctx).GetNode("fresh.go::BeforeDelete"))
	require.NoError(t, os.Remove(path))
	next, err := s.ensureProjected(t.Context(), []string{"fresh.go"})
	require.NoError(t, err)
	require.Nil(t, s.readerFor(next).GetNode("fresh.go::BeforeDelete"))
	entry := pendingFileProjections.snap.Load().entries[pathkey.MutationKey(path)]
	require.NotNil(t, entry)
	require.True(t, entry.tombstone)
	require.ErrorIs(t, validateRequestProjections(ctx), projectionSuperseded)
}
