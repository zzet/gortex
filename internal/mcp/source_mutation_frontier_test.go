package mcp

import (
	"context"
	"path/filepath"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/indexer"
)

type sourceFrontierCoverageWatcher struct {
	mutationTestWatcher
	partial bool
	points  int
}

func (w *sourceFrontierCoverageWatcher) EnqueueToolMutation(ctx context.Context, path string) (*indexer.MutationTicket, error) {
	w.points++
	return w.mutationTestWatcher.EnqueueToolMutation(ctx, path)
}

func (w *sourceFrontierCoverageWatcher) EnqueueFileMutations(ctx context.Context, paths []string) (map[string]*indexer.MutationTicket, error) {
	if !w.partial {
		return nil, nil // Explicitly no file was admitted.
	}
	ticket, err := w.mutationTestWatcher.EnqueueToolMutation(ctx, paths[0])
	return map[string]*indexer.MutationTicket{paths[0]: ticket}, err
}

func TestSourceMutationFrontierNeverDuplicatesPartialAdmission(t *testing.T) {
	s := newMutationServer()
	watcher := &sourceFrontierCoverageWatcher{partial: true}
	s.SetWatcher(watcher)
	root := t.TempDir()
	targets := []sourceMutationTarget{{path: filepath.Join(root, "a.go")}, {path: filepath.Join(root, "b.go")}}
	admissions := s.admitSourceMutationFrontier(t.Context(), targets)
	require.Len(t, admissions, 2)
	for _, admission := range admissions {
		require.ErrorContains(t, admission.immediate.Err, "coverage")
		require.Nil(t, admission.receipt)
		require.Empty(t, admission.inlinePath)
	}
	require.Zero(t, watcher.points, "an incomplete admitted frontier must never be admitted again pointwise")
}

func TestSourceMutationFrontierNilSetFallsBackOnce(t *testing.T) {
	s := newMutationServer()
	watcher := &sourceFrontierCoverageWatcher{}
	s.SetWatcher(watcher)
	root := t.TempDir()
	targets := []sourceMutationTarget{{path: filepath.Join(root, "a.go")}, {path: filepath.Join(root, "b.go")}}
	admissions := s.admitSourceMutationFrontier(t.Context(), targets)
	require.Len(t, admissions, 2)
	for _, admission := range admissions {
		require.NotNil(t, admission.receipt)
		<-admission.receipt.done
	}
	require.Equal(t, 2, watcher.points)
}

func TestSourceMutationFrontierSharesDeadlineAndOneDispatchHandoff(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := newMutationServer()
		s.mutationReindexWait = 100 * time.Millisecond
		first := &mutationReceipt{id: "first", done: make(chan struct{})}
		second := &mutationReceipt{id: "second", done: make(chan struct{})}
		s.mutationReceipts.Store(first.id, first)
		s.mutationReceipts.Store(second.id, second)
		calls := 0
		s.releaseDispatchSlot = func(context.Context, <-chan struct{}) bool { calls++; return true }
		time.AfterFunc(60*time.Millisecond, func() {
			first.mu.Lock()
			first.completed = true
			first.result = indexer.MutationResult{Reindexed: true}
			first.mu.Unlock()
			close(first.done)
		})
		start := time.Now()
		outcomes := s.awaitSourceMutationFrontier(t.Context(), []sourceMutationAdmission{{receipt: first}, {receipt: second}})
		require.Equal(t, 100*time.Millisecond, time.Since(start), "the second file must use the original deadline, not another wait allowance")
		require.Equal(t, 1, calls)
		require.True(t, outcomes[0].Reindexed)
		require.False(t, outcomes[0].Pending)
		require.True(t, outcomes[1].Pending)
		require.Equal(t, second.id, outcomes[1].Receipt)
	})
}
