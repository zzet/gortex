package mcp

import (
	"context"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/pathkey"
)

func TestTrackedExternalReadAdmitsOnceWithoutWaiting(t *testing.T) {
	selectedProjectionRegistry(t)
	s, root := setupTestServer(t)
	path := filepath.Join(root, "main.go")
	before, err := os.ReadFile(path)
	require.NoError(t, err)
	var admitted atomic.Int64
	held := make(chan indexer.MutationResult)
	defer close(held)
	s.SetWatcher(mutationTestWatcher{scheduled: &admitted, done: held})
	require.NoError(t, os.WriteFile(path, append(before, []byte("\nfunc ExternalRead() {}\n")...), 0600))
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	req := mcplib.CallToolRequest{}
	req.Params.Name = "get_file_summary"
	req.Params.Arguments = map[string]any{"path": "main.go", "format": "json"}
	handler := s.wrapToolHandler(s.handleGetFileSummary)
	result, err := handler(ctx, req)
	require.NoError(t, err)
	require.False(t, result.IsError, "read must return before its admitted ticket completes: %s", resultText(result))
	require.EqualValues(t, 1, admitted.Load())
	require.Nil(t, s.graph.GetNode("main.go::ExternalRead"))
	entry := pendingFileProjections.snap.Load().entries[pathkey.MutationKey(path)]
	require.NotNil(t, entry)
	require.Zero(t, entry.commitSeq, "a read-built projection is not a physical source commit")
	if s.mutationCommits != nil {
		_, committed := s.mutationCommits.recentForPath(path)
		require.False(t, committed)
	}
	result, err = handler(ctx, req)
	require.NoError(t, err)
	require.False(t, result.IsError, "%s", resultText(result))
	require.EqualValues(t, 1, admitted.Load(), "reusing the same accepted cache version must not queue again")
}

func TestReadProjectionDoesNotAdmitWithoutPublicationOrAcrossCheckouts(t *testing.T) {
	for _, selected := range []bool{false, true} {
		t.Run(map[bool]string{false: "disabled", true: "selected_sibling"}[selected], func(t *testing.T) {
			selectedProjectionRegistry(t)
			s, root := setupTestServer(t)
			var admitted atomic.Int64
			s.SetWatcher(mutationTestWatcher{scheduled: &admitted})
			ctx := t.Context()
			if selected {
				root = t.TempDir()
				ctx = withRequestView(ctx, &requestView{kind: requestViewKindWorktree, viewRoot: root, reader: s.graph})
			} else {
				pendingFileProjections.enabled = false
			}
			require.NoError(t, os.WriteFile(filepath.Join(root, "main.go"), []byte("package main\nfunc SelectedOnly() {}\n"), 0600))
			next, err := s.ensureProjected(ctx, []string{"main.go"})
			if selected {
				require.NoError(t, err)
				require.NotNil(t, s.readerFor(next).GetNode("main.go::SelectedOnly"))
			} else {
				require.Error(t, err)
			}
			require.Zero(t, admitted.Load(), "no canonical-sibling admission or admission without accepted publication")
		})
	}
}
