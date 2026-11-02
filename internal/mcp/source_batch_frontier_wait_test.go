package mcp

import (
	"context"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/indexer"
)

func TestDurableBatchReleasesWholeFrontierBeforePublicationWait(t *testing.T) {
	published := make(chan indexer.MutationResult, 2)
	watcher := &renameFrontierWatcher{mutationTestWatcher: mutationTestWatcher{done: published}, announced: make(chan []string, 1)}
	s := newAtomicBatchTestServer(t, watcher)
	s.mutationReindexWait = time.Minute
	root := t.TempDir()
	paths := []string{writeAtomicBatchFixture(t, root, "a.txt", "before\n"), writeAtomicBatchFixture(t, root, "b.txt", "before\n")}
	waiting := make(chan error, 1)
	s.releaseDispatchSlot = func(context.Context, <-chan struct{}) bool {
		check, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
		defer cancel()
		_, release, err := acquireMutationPaths(check, paths)
		if release != nil {
			release()
		}
		waiting <- err
		return true
	}
	complete := sync.OnceFunc(func() {
		for range 2 {
			published <- indexer.MutationResult{Reindexed: true}
		}
		close(published)
	})
	joined := make(chan struct{})
	var receipt batchTransactionReceipt
	var runErr error
	go func() {
		defer close(joined)
		receipt, runErr = s.runBatchTransaction(t.Context(), []batchEditItem{atomicFileEdit(paths[0], "before", "after"), atomicFileEdit(paths[1], "before", "after")}, "unlocked-batch-frontier")
	}()
	defer func() {
		complete()
		select {
		case <-joined:
		case <-time.After(3 * time.Second):
			t.Error("owned durable batch did not join")
		}
	}()
	select {
	case admitted := <-watcher.announced:
		require.ElementsMatch(t, paths, admitted)
	case <-time.After(3 * time.Second):
		t.Fatal("durable batch did not admit its complete file set")
	}
	for _, path := range paths {
		raw, err := os.ReadFile(path)
		require.NoError(t, err)
		require.Equal(t, "after\n", string(raw))
	}
	select {
	case err := <-waiting:
		require.NoError(t, err, "durable batch retained its held paths across graph publication wait")
	case <-time.After(time.Second):
		t.Fatal("durable batch did not enter its shared publication wait")
	}
	complete()
	select {
	case <-joined:
	case <-time.After(3 * time.Second):
		t.Fatal("published durable batch did not join")
	}
	require.NoError(t, runErr)
	require.Equal(t, "committed", receipt.Status)
	require.Equal(t, "fresh", receipt.GraphStatus)
	require.EqualValues(t, 1, watcher.sets.Load())
	require.Zero(t, watcher.points.Load())
}
