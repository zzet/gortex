package mcp

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/indexer"
)

type batchSetSchedulerSpy struct {
	watcherHistory
	sets         [][]string
	points       []string
	unsupported  bool
	admissionErr error
}

func (w *batchSetSchedulerSpy) EnqueueFileMutation(_ context.Context, path string) (*indexer.MutationTicket, error) {
	w.points = append(w.points, path)
	return completedBatchSetTicket(path, uint64(len(w.points))), nil
}
func completedBatchSetTicket(path string, generation uint64) *indexer.MutationTicket {
	done := make(chan indexer.MutationResult, 1)
	done <- indexer.MutationResult{RequestedGeneration: generation, AppliedGeneration: generation, Reindexed: true}
	close(done)
	return &indexer.MutationTicket{Path: path, Generation: generation, Done: done}
}
func (w *batchSetSchedulerSpy) EnqueueFileMutations(_ context.Context, paths []string) (map[string]*indexer.MutationTicket, error) {
	w.sets = append(w.sets, append([]string(nil), paths...))
	if w.admissionErr != nil {
		return nil, w.admissionErr
	}
	if w.unsupported {
		return nil, nil
	}
	out := make(map[string]*indexer.MutationTicket, len(paths))
	for i, path := range paths {
		out[path] = completedBatchSetTicket(path, uint64(i+1))
	}
	return out, nil
}
func batchSetState(paths ...string) *batchTransactionState {
	state := &batchTransactionState{done: make(chan struct{})}
	receipt := batchTransactionReceipt{Status: "committed", DiskStatus: "committed", GraphStatus: "pending"}
	for _, path := range paths {
		receipt.Files = append(receipt.Files, batchTransactionFile{Path: path})
	}
	state.publish(receipt, false)
	return state
}
func TestBatchMutationSetCompleteFrontierReadmission(t *testing.T) {
	w := &batchSetSchedulerSpy{}
	s := newAtomicBatchTestServer(t, w)
	root := t.TempDir()
	a, b := filepath.Join(root, "caller.go"), filepath.Join(root, "target.go")
	state := batchSetState(a, b)
	// Simulate restart with durable IDs that have no daemon-local state.
	receipt := state.snapshot()
	receipt.Files[0].ReindexReceipt = "expired"
	state.publish(receipt, false)
	s.refreshBatchGraph(context.Background(), state)
	require.Equal(t, [][]string{{a, b}}, w.sets)
	require.Empty(t, w.points)
	got := state.snapshot()
	require.Equal(t, "fresh", got.GraphStatus)
	for _, file := range got.Files {
		require.NotEmpty(t, file.ReindexReceipt)
		require.NotEqual(t, "expired", file.ReindexReceipt)
	}
	s.refreshBatchGraph(context.Background(), state)
	require.Len(t, w.sets, 1, "live fresh status must not reschedule")
}
func TestBatchMutationSetUnsupportedAndSingleKeepPointAdmission(t *testing.T) {
	for _, count := range []int{1, 2} {
		t.Run(string(rune('0'+count)), func(t *testing.T) {
			w := &batchSetSchedulerSpy{unsupported: true}
			s := newAtomicBatchTestServer(t, w)
			paths := []string{filepath.Join(t.TempDir(), "a.go")}
			if count == 2 {
				paths = append(paths, filepath.Join(t.TempDir(), "b.go"))
			}
			state := batchSetState(paths...)
			s.refreshBatchGraph(context.Background(), state)
			require.Equal(t, paths, w.points)
			require.Equal(t, "fresh", state.snapshot().GraphStatus)
			if count == 1 {
				require.Empty(t, w.sets)
			}
		})
	}
}
func TestBatchMutationSetAdmissionErrorDoesNotSplitCommittedFrontier(t *testing.T) {
	w := &batchSetSchedulerSpy{admissionErr: errors.New("watcher stopped")}
	s := newAtomicBatchTestServer(t, w)
	state := batchSetState(filepath.Join(t.TempDir(), "a.go"), filepath.Join(t.TempDir(), "b.go"))
	s.refreshBatchGraph(context.Background(), state)
	require.Empty(t, w.points)
	require.Equal(t, "committed", state.snapshot().DiskStatus)
	require.Equal(t, "failed", state.snapshot().GraphStatus)
}
