package mcp

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/parser"
)

func TestEnsureProjectedKeepsAlreadyBorrowedRequestFileVersion(t *testing.T) {
	for _, supersession := range []string{"new names", "same bytes new physical owner"} {
		t.Run(supersession, func(t *testing.T) {
			previous := pendingFileProjections
			pendingFileProjections = newPendingProjections()
			t.Cleanup(func() { pendingFileProjections = previous })
			s, root := setupTestServer(t)
			path := filepath.Join(root, "main.go")
			original, err := os.ReadFile(path)
			require.NoError(t, err)
			raw := append(append([]byte(nil), original...), []byte("\nfunc RequestVersionOne() {}\n")...)
			require.NoError(t, os.WriteFile(path, raw, 0o644))
			first, err := s.ensureProjected(t.Context(), []string{"main.go"})
			require.NoError(t, err)
			firstLayers := requestProjectionLayers(first)
			require.Len(t, firstLayers, 1)
			require.NotNil(t, s.readerFor(first).GetNode("main.go::RequestVersionOne"))
			req := mcplib.CallToolRequest{}
			req.Params.Name = "get_file_summary"
			req.Params.Arguments = map[string]any{"path": "main.go", "format": "json"}
			before := parser.ParseCount()
			stable, err := s.handleGetFileSummary(first, req)
			require.NoError(t, err)
			require.False(t, stable.IsError, "%v", stable.Content)
			require.Equal(t, before, parser.ParseCount(), "a stable borrowed file does not need another parse")
			if supersession == "new names" {
				require.NoError(t, os.WriteFile(path, append(append([]byte(nil), original...), []byte("\nfunc RequestVersionTwo() {}\n")...), 0o644))
			} else {
				held, release, lockErr := acquireMutationPath(t.Context(), path)
				require.NoError(t, lockErr)
				commit, writeErr := s.commitFileMutation(held, "write_file", "", "", "main.go", path, raw, 0o644)
				release()
				require.NoError(t, writeErr)
				require.Positive(t, commit.commitSeq, "this is an actual unchanged-byte newer physical execution")
			}
			second, err := s.ensureProjected(t.Context(), []string{"main.go"})
			require.NoError(t, err)
			if supersession == "new names" {
				require.NotNil(t, s.readerFor(second).GetNode("main.go::RequestVersionTwo"))
				require.Nil(t, s.readerFor(second).GetNode("main.go::RequestVersionOne"))
			} else {
				require.NotNil(t, s.readerFor(second).GetNode("main.go::RequestVersionOne"))
			}
			before = parser.ParseCount()
			refused, err := s.handleGetFileSummary(first, req)
			require.NoError(t, err)
			require.True(t, refused.IsError, "a superseded candidate/row selection must explicitly refuse, never mix newer rows into it: %v", refused.Content)
			require.Contains(t, toolResultText(refused), "superseded")
			require.Equal(t, before, parser.ParseCount(), "supersession must be detected by existing pointer ownership")
			require.Len(t, requestProjectionLayers(first), 1)
			require.Same(t, firstLayers[0], requestProjectionLayers(first)[0])
			require.NotNil(t, s.readerFor(first).GetNode("main.go::RequestVersionOne"))
			require.Nil(t, s.graph.GetNode("main.go::RequestVersionTwo"))
		})
	}
}

func TestProjectedReadWrapperRefusesPhysicalSupersessionAfterLeafRead(t *testing.T) {
	previous := pendingFileProjections
	pendingFileProjections = newPendingProjections()
	t.Cleanup(func() { pendingFileProjections = previous })
	s, root := setupTestServer(t)
	path := filepath.Join(root, "main.go")
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	raw = append(raw, []byte("\nfunc SupersededBeforeResponse() {}\n")...)
	require.NoError(t, os.WriteFile(path, raw, 0o644))
	req := mcplib.CallToolRequest{}
	req.Params.Name = "get_file_summary"
	req.Params.Arguments = map[string]any{"path": "main.go", "format": "json"}
	leafRan := false
	wrapped := s.wrapToolHandler(func(ctx context.Context, request mcplib.CallToolRequest) (*mcplib.CallToolResult, error) {
		result, readErr := s.handleGetFileSummary(ctx, request)
		require.NoError(t, readErr)
		require.False(t, result.IsError, "%v", result.Content)
		require.True(t, s.hasRequestProjectionFact(ctx, "main.go"))
		leafRan = true
		held, release, lockErr := acquireMutationPath(t.Context(), path)
		require.NoError(t, lockErr)
		commit, writeErr := s.commitFileMutation(held, "write_file", "", "", "main.go", path, raw, 0o644)
		release()
		require.NoError(t, writeErr)
		require.Positive(t, commit.commitSeq)
		assert.False(t, s.hasRequestProjectionFact(ctx, "main.go"), "a superseded layer cannot supply current-file freshness to response decoration")
		return result, nil
	})
	result, err := wrapped(t.Context(), req)
	require.NoError(t, err)
	require.True(t, leafRan)
	require.True(t, result.IsError, "the public wrapper must refuse the complete answer if its selected source owner changed after the leaf read: %v", result.Content)
	require.Contains(t, toolResultText(result), "superseded")
}
