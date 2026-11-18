package mcp

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/indexer"
)

func TestRenamePublishesNoFileViewBeforeCompletePhysicalFrontier(t *testing.T) {
	previous := pendingFileProjections
	pendingFileProjections = newPendingProjections()
	t.Cleanup(func() { pendingFileProjections = previous })
	s, root := setupRenameServer(t, renameTargetSrc, renameCallerSrc)
	published := make(chan indexer.MutationResult, 2)
	for range 2 {
		published <- indexer.MutationResult{Reindexed: true}
	}
	close(published)
	watcher := &renameFrontierWatcher{mutationTestWatcher: mutationTestWatcher{done: published}, announced: make(chan []string, 1)}
	s.SetWatcher(watcher)
	before := pendingFileProjections.snap.Load()
	calls := 0
	s.mutationPreCommitHook = func(*mutationCommitRecord) {
		calls++
		if calls == 2 {
			assert.Same(t, before, pendingFileProjections.snap.Load(), "a second source file is not yet committed: no member may be visible")
			raw, err := os.ReadFile(filepath.Join(root, "caller.go"))
			require.NoError(t, err)
			assert.Contains(t, string(raw), "Renamed", "the first actual write must have completed before the visibility check")
		}
	}
	req := mcplib.CallToolRequest{}
	req.Params.Name = "rename_symbol"
	req.Params.Arguments = map[string]any{"id": "target.go::Target", "new_name": "Renamed", "format": "json"}
	result, err := s.handleRenameSymbol(t.Context(), req)
	require.NoError(t, err)
	require.False(t, result.IsError, "%v", result.Content)
	require.Equal(t, 2, calls)
	after := pendingFileProjections.snap.Load()
	require.NotSame(t, before, after)
	for _, name := range []string{"caller.go", "target.go"} {
		entry := liveEntry(pendingFileProjections, filepath.Join(root, name))
		require.NotNil(t, entry)
		require.Positive(t, entry.commitSeq)
	}
	require.EqualValues(t, 1, watcher.sets.Load())
	require.Zero(t, watcher.points.Load())
}

func TestRenamePartialCancellationPublishesOnlyActuallyCommittedSubset(t *testing.T) {
	previous := pendingFileProjections
	pendingFileProjections = newPendingProjections()
	t.Cleanup(func() { pendingFileProjections = previous })
	s, root := setupRenameServer(t, renameTargetSrc, renameCallerSrc)
	published := make(chan indexer.MutationResult, 2)
	defer close(published)
	s.SetWatcher(mutationTestWatcher{done: published})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	writes := make([]*renameFileWrite, 0, 2)
	paths := make([]string, 0, 2)
	for _, name := range []string{"caller.go", "target.go"} {
		path := filepath.Join(root, name)
		raw, err := os.ReadFile(path)
		require.NoError(t, err)
		writes = append(writes, &renameFileWrite{RelPath: name, AbsPath: path, Old: raw, New: []byte(strings.ReplaceAll(string(raw), "Target", "Renamed"))})
		paths = append(paths, path)
	}
	held, release, err := acquireMutationPaths(ctx, paths)
	require.NoError(t, err)
	defer release()
	seen := 0
	s.mutationPreCommitHook = func(*mutationCommitRecord) {
		seen++
		if seen == 2 {
			cancel()
		}
	}
	results := s.commitRenameWrites(held, writes, release)
	require.Len(t, results, 2)
	require.Equal(t, "applied", results[0]["status"])
	require.Equal(t, "failed", results[1]["status"])
	require.Equal(t, mutationDiskNotApplied, results[1]["disk_status"])
	require.Equal(t, true, results[0]["readback_exact"], "cancellation after the first physical effect must retain that exact committed subset")
	require.NotNil(t, liveEntry(pendingFileProjections, paths[0]))
	require.Nil(t, liveEntry(pendingFileProjections, paths[1]))
	first, err := os.ReadFile(paths[0])
	require.NoError(t, err)
	require.Equal(t, writes[0].New, first)
	second, err := os.ReadFile(paths[1])
	require.NoError(t, err)
	require.Equal(t, writes[1].Old, second)
}

func TestRenameProjectionCapacityRefusalLeavesWholeCacheUnchanged(t *testing.T) {
	previous := pendingFileProjections
	pendingFileProjections = newPendingProjections()
	t.Cleanup(func() { pendingFileProjections = previous })
	s, root := setupRenameServer(t, renameTargetSrc, renameCallerSrc)
	published := make(chan indexer.MutationResult, 2)
	for range 2 {
		published <- indexer.MutationResult{Reindexed: true}
	}
	close(published)
	watcher := &renameFrontierWatcher{mutationTestWatcher: mutationTestWatcher{done: published}, announced: make(chan []string, 1)}
	s.SetWatcher(watcher)
	require.NoError(t, pendingFileProjections.publish(&projectionEntry{key: "other-physical-owner", graphPath: "other.go", size: projectionMaxBytes - 32}))
	before := pendingFileProjections.snap.Load()
	req := mcplib.CallToolRequest{}
	req.Params.Name = "rename_symbol"
	req.Params.Arguments = map[string]any{"id": "target.go::Target", "new_name": "Renamed", "format": "json"}
	result, err := s.handleRenameSymbol(t.Context(), req)
	require.NoError(t, err)
	require.False(t, result.IsError, "cache refusal must preserve the actual disk verdict: %v", result.Content)
	require.Same(t, before, pendingFileProjections.snap.Load(), "combined cap refusal must not admit even one rename member")
	for _, name := range []string{"caller.go", "target.go"} {
		raw, readErr := os.ReadFile(filepath.Join(root, name))
		require.NoError(t, readErr)
		require.Contains(t, string(raw), "Renamed")
	}
	require.EqualValues(t, 1, watcher.sets.Load())
	require.Zero(t, watcher.points.Load())
}
