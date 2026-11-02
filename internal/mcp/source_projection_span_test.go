package mcp

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/parser"
)

func TestSymbolSpanUsesMatchingPendingProjectionWithoutAnotherParse(t *testing.T) {
	previous := pendingFileProjections
	pendingFileProjections = newPendingProjections()
	t.Cleanup(func() { pendingFileProjections = previous })
	s, root := setupTestServer(t)
	path := filepath.Join(root, "main.go")
	before, err := os.ReadFile(path)
	require.NoError(t, err)
	raw := append([]byte("\n\n"), before...)
	require.NoError(t, os.WriteFile(path, raw, 0o644))
	stamp, err := statDiskStamp(path)
	require.NoError(t, err)
	projection, err := s.indexer.ProjectSource(t.Context(), path, raw)
	require.NoError(t, err)
	entry := newProjectionEntry(path, s.indexer.RepoPrefix(), projection, raw, 1, stamp)
	require.NoError(t, pendingFileProjections.publish(entry))
	node := s.graph.GetNode("main.go::helper")
	require.NotNil(t, node)
	current := entry.layer.NodeByID(node.ID)
	require.NotNil(t, current)
	require.NotEqual(t, node.StartLine, current.StartLine)
	ctx, release, err := acquireMutationPath(t.Context(), path)
	require.NoError(t, err)
	defer release()
	parses := parser.ParseCount()
	start, end, err := s.symbolLinesUnderLock(ctx, node, path, raw)
	require.NoError(t, err)
	require.Equal(t, current.StartLine, start)
	require.Equal(t, current.EndLine, end)
	require.EqualValues(t, 0, parser.ParseCount()-parses, "the same BLAKE3-matched file projection must serve the held-path declaration resolver")
}

func TestSymbolSpanRejectsMismatchedProjectionBytesAndRemovedDeclaration(t *testing.T) {
	for _, removed := range []bool{false, true} {
		t.Run(map[bool]string{false: "shifted again", true: "removed"}[removed], func(t *testing.T) {
			previous := pendingFileProjections
			pendingFileProjections = newPendingProjections()
			t.Cleanup(func() { pendingFileProjections = previous })
			s, root := setupTestServer(t)
			path := filepath.Join(root, "main.go")
			before, err := os.ReadFile(path)
			require.NoError(t, err)
			projectedBytes := append([]byte("\n\n"), before...)
			require.NoError(t, os.WriteFile(path, projectedBytes, 0o644))
			stamp, err := statDiskStamp(path)
			require.NoError(t, err)
			projection, err := s.indexer.ProjectSource(t.Context(), path, projectedBytes)
			require.NoError(t, err)
			entry := newProjectionEntry(path, s.indexer.RepoPrefix(), projection, projectedBytes, 1, stamp)
			require.NoError(t, pendingFileProjections.publish(entry))
			node := s.graph.GetNode("main.go::helper")
			require.NotNil(t, node)
			current := append([]byte("\n\n\n\n"), before...)
			if removed {
				current = []byte(strings.ReplaceAll(string(current), "func helper() {}", "func Gone() {}"))
			}
			require.NoError(t, os.WriteFile(path, current, 0o644))
			ctx, release, err := acquireMutationPath(t.Context(), path)
			require.NoError(t, err)
			defer release()
			parses := parser.ParseCount()
			start, _, err := s.symbolLinesUnderLock(ctx, node, path, current)
			if removed {
				require.ErrorIs(t, err, errSymbolRemoved)
			} else {
				require.NoError(t, err)
				require.Equal(t, node.StartLine+4, start)
			}
			require.EqualValues(t, 1, parser.ParseCount()-parses, "different raw identity must take the conservative full extraction")
		})
	}
}
