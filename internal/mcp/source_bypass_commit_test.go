package mcp

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestPropagatedSourceWriteHasCommitReceipt(t *testing.T) {
	srv, _, root := setupDeleteServer(t)
	path := filepath.Join(root, "caller.go")
	ctx, note := withMutationCommitNote(t.Context())
	applied, failed, err := srv.applyRemoveLinePatches(ctx, []callerPatch{{
		File: "caller.go", Line: 4, CurrentText: "Target()", Action: "remove_line", abs: path,
	}})
	require.NoError(t, err)
	require.Empty(t, failed)
	require.Len(t, applied, 1)
	current, err := os.ReadFile(path)
	require.NoError(t, err)
	require.NotContains(t, string(current), "Target()", "prove that a real source write landed")
	verdict := note.verdict()
	require.Equal(t, mutationDiskCommitted, verdict.Status, "a source writer must publish the same disk receipt as the ordinary edit path")
	require.Len(t, verdict.Snapshots, 1)
	require.Equal(t, "caller.go", verdict.Snapshots[0].Path)
	require.True(t, verdict.Snapshots[0].GraphRecorded)
}
