package mcp

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/parser"
)

type renameFrontierWatcher struct {
	mutationTestWatcher
	points, sets atomic.Int64
	announced    chan []string
}

func (w *renameFrontierWatcher) EnqueueToolMutation(ctx context.Context, path string) (*indexer.MutationTicket, error) {
	w.points.Add(1)
	select {
	case w.announced <- []string{path}:
	default:
	}
	return w.mutationTestWatcher.EnqueueToolMutation(ctx, path)
}
func (w *renameFrontierWatcher) EnqueueFileMutations(ctx context.Context, paths []string) (map[string]*indexer.MutationTicket, error) {
	w.sets.Add(1)
	tickets := make(map[string]*indexer.MutationTicket, len(paths))
	for _, path := range paths {
		ticket, err := w.mutationTestWatcher.EnqueueToolMutation(ctx, path)
		if err != nil {
			return nil, err
		}
		tickets[path] = ticket
	}
	select {
	case w.announced <- append([]string(nil), paths...):
	default:
	}
	return tickets, nil
}

func TestRenameAdmitsWholeFrontierAndReleasesPathsBeforeGraphWait(t *testing.T) {
	previous := pendingFileProjections
	pendingFileProjections = newPendingProjections()
	t.Cleanup(func() { pendingFileProjections = previous })
	s, root := setupRenameServer(t, renameTargetSrc, renameCallerSrc)
	publications := make(chan indexer.MutationResult, 2)
	watcher := &renameFrontierWatcher{mutationTestWatcher: mutationTestWatcher{done: publications}, announced: make(chan []string, 1)}
	s.SetWatcher(watcher)
	s.mutationReindexWait = time.Minute
	paths := []string{filepath.Join(root, "caller.go"), filepath.Join(root, "target.go")}
	waiting := make(chan error, 1)
	s.releaseDispatchSlot = func(context.Context, <-chan struct{}) bool {
		check, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
		defer cancel()
		_, release, err := acquireMutationPaths(check, paths)
		if err == nil {
			release()
		}
		waiting <- err
		return true
	}
	ctx, cancel := context.WithCancel(t.Context())
	req := mcplib.CallToolRequest{}
	req.Params.Name = "rename_symbol"
	req.Params.Arguments = map[string]any{"id": "target.go::Target", "new_name": "Renamed", "format": "json"}
	finished := make(chan struct{})
	var result *mcplib.CallToolResult
	var resultErr error
	complete := sync.OnceFunc(func() {
		for range 2 {
			publications <- indexer.MutationResult{RequestedGeneration: 1, AppliedGeneration: 1, Reindexed: true}
		}
		close(publications)
	})
	go func() { defer close(finished); result, resultErr = s.handleRenameSymbol(ctx, req) }()
	defer func() {
		cancel()
		complete()
		select {
		case <-finished:
		case <-time.After(3 * time.Second):
			t.Error("owned rename did not join during cleanup")
		}
	}()
	select {
	case admitted := <-watcher.announced:
		assert.ElementsMatch(t, paths, admitted, "admit the full physical frontier once")
	case <-time.After(3 * time.Second):
		t.Fatal("rename never admitted publication")
	}
	for _, path := range paths {
		raw, err := os.ReadFile(path)
		require.NoError(t, err)
		assert.Contains(t, string(raw), "Renamed", "every file must be committed before admission/wait")
	}
	assert.EqualValues(t, 1, watcher.sets.Load())
	assert.Zero(t, watcher.points.Load(), "whole-set support must not pay point admission per file")
	select {
	case err := <-waiting:
		require.NoError(t, err, "a real competing path admission must succeed while publication is pending")
	case <-time.After(time.Second):
		t.Fatal("rename waits before finishing and releasing its source frontier")
	}
	complete()
	select {
	case <-finished:
	case <-time.After(3 * time.Second):
		t.Fatal("published rename did not return")
	}
	require.NoError(t, resultErr)
	require.False(t, result.IsError, "%v", result.Content)
	require.Equal(t, "applied", decodeRenameResp(t, result)["status"])
}

func TestRenameCandidateProjectionAndGateParseEachFileOnce(t *testing.T) {
	previous := pendingFileProjections
	pendingFileProjections = newPendingProjections()
	t.Cleanup(func() { pendingFileProjections = previous })
	s, _ := setupRenameServer(t, renameTargetSrc, renameCallerSrc)
	publications := make(chan indexer.MutationResult, 2)
	for range 2 {
		publications <- indexer.MutationResult{RequestedGeneration: 1, AppliedGeneration: 1, Reindexed: true}
	}
	close(publications)
	watcher := &renameFrontierWatcher{mutationTestWatcher: mutationTestWatcher{done: publications}, announced: make(chan []string, 1)}
	s.SetWatcher(watcher)
	req := mcplib.CallToolRequest{}
	req.Params.Name = "rename_symbol"
	req.Params.Arguments = map[string]any{"id": "target.go::Target", "new_name": "Renamed", "format": "json"}
	before := parser.ParseCount()
	result, err := s.handleRenameSymbol(t.Context(), req)
	require.NoError(t, err)
	require.False(t, result.IsError, "%v", result.Content)
	require.EqualValues(t, 2, parser.ParseCount()-before, "both clean candidates must share their full parse between syntax gate and pending projection")
}
