package mcp

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

func TestGuardedRemovalPublishesExactTombstone(t *testing.T) {
	previous := pendingFileProjections
	pendingFileProjections = newPendingProjections()
	t.Cleanup(func() { pendingFileProjections = previous })
	s, root := setupTestServer(t)
	path := filepath.Join(root, "main.go")
	require.NotNil(t, s.graph.GetNode("main.go::helper"))
	ctx, release, err := acquireMutationPath(t.Context(), path)
	require.NoError(t, err)
	defer release()
	record, err := s.commitFileMutation(ctx, "overlay_merge", "", "", "main.go", path, nil, 0, sourceMutationOperation{kind: sourceRemove})
	require.NoError(t, err)
	require.Equal(t, mutationDiskCommitted, record.diskStatus())
	_, err = os.Lstat(path)
	require.ErrorIs(t, err, os.ErrNotExist)
	entry, err := pendingFileProjections.lookup(path)
	require.NoError(t, err)
	require.NotNil(t, entry, "a guarded source removal must admit a file tombstone at the physical commit")
	require.True(t, entry.tombstone)
	require.Positive(t, entry.commitSeq)
	require.Nil(t, graph.NewOverlaidView(s.graph, entry.layer).GetNode("main.go::helper"))
	require.NotNil(t, s.graph.GetNode("main.go::helper"), "the tombstone does not mutate shared graph state")
	response := make(map[string]any)
	attachMutationCommit(response, record)
	requireMutationFileViewState(t, response, "projected")
	require.Equal(t, true, response["readback_exact"])
}

func TestGuardedRemovalTombstoneOwnsLeafEntryNotLinkTarget(t *testing.T) {
	previous := pendingFileProjections
	pendingFileProjections = newPendingProjections()
	t.Cleanup(func() { pendingFileProjections = previous })
	s, root := setupTestServer(t)
	target := filepath.Join(root, "main.go")
	before, err := os.ReadFile(target)
	require.NoError(t, err)
	link := filepath.Join(root, "link.go")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	ctx, release, err := acquireMutationPath(t.Context(), link)
	require.NoError(t, err)
	defer release()
	_, err = s.commitFileMutation(ctx, "overlay_merge", "", "", "link.go", link, nil, 0, sourceMutationOperation{kind: sourceRemove})
	require.NoError(t, err)
	current, err := os.ReadFile(target)
	require.NoError(t, err)
	require.Equal(t, before, current)
	entry := liveEntry(pendingFileProjections, link)
	require.NotNil(t, entry)
	require.True(t, entry.tombstone)
	require.Equal(t, "link.go", entry.graphPath)
	require.Nil(t, liveEntry(pendingFileProjections, target))
	require.NotNil(t, graph.NewOverlaidView(s.graph, entry.layer).GetNode("main.go::helper"))
}

func TestGuardedRemovalRefusesLiveTombstoneAfterRecreation(t *testing.T) {
	previous := pendingFileProjections
	pendingFileProjections = newPendingProjections()
	t.Cleanup(func() { pendingFileProjections = previous })
	s, root := setupTestServer(t)
	path := filepath.Join(root, "main.go")
	ctx, release, err := acquireMutationPath(t.Context(), path)
	require.NoError(t, err)
	defer release()
	record, err := s.commitFileMutation(ctx, "overlay_merge", "", "", "main.go", path, nil, 0, sourceMutationOperation{kind: sourceRemove})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, []byte("package main\n"), 0o600))
	response := make(map[string]any)
	attachMutationCommit(response, record)
	requireMutationFileViewState(t, response, "unavailable")
	require.Equal(t, false, response["readback_exact"])
	entry, err := pendingFileProjections.lookup(path)
	require.ErrorIs(t, err, errProjectionExternalWrite)
	require.Nil(t, entry)
}

func TestGuardedRemovalHonorsProjectionDisable(t *testing.T) {
	previous := pendingFileProjections
	pendingFileProjections = newPendingProjections()
	pendingFileProjections.enabled = false
	t.Cleanup(func() { pendingFileProjections = previous })
	s, root := setupTestServer(t)
	path := filepath.Join(root, "main.go")
	ctx, release, err := acquireMutationPath(t.Context(), path)
	require.NoError(t, err)
	defer release()
	record, err := s.commitFileMutation(ctx, "overlay_merge", "", "", "main.go", path, nil, 0, sourceMutationOperation{kind: sourceRemove})
	require.NoError(t, err)
	response := make(map[string]any)
	attachMutationCommit(response, record)
	require.Equal(t, "disabled", response["file_reason"])
	requireMutationFileViewState(t, response, "unavailable")
	require.Empty(t, pendingFileProjections.snap.Load().entries)
}
