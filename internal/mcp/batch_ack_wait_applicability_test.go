package mcp

import (
	"context"
	"os"
	"sync"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/indexer"
	"go.uber.org/zap"
)

// Real forward journal writes and the real set watcher. The local policy
// deliberately returns without a passive wait; it must witness that executed
// branch, not invent a zero-duration measured wait or exempt all batches.
func TestBatchLocalACKRecordsExecutedNoWaitApplicability(t *testing.T) {
	t.Setenv(batchTransactionDirEnv, filepath.Join(t.TempDir(), "transactions"))
	s, root := setupTestServer(t)
	s.editAckMode = config.EditAckLocal
	paths := []string{filepath.Join(root, "wait-a.go"), filepath.Join(root, "wait-b.go")}
	before := "package main\nfunc BeforeWait() {}\n"
	after := "package main\nfunc AfterWait() {}\n"
	for _, path := range paths {
		require.NoError(t, os.WriteFile(path, []byte(before), 0600))
	}
	_, err := s.indexer.IncrementalReindexPaths(root, paths)
	require.NoError(t, err)
	watcher, err := indexer.NewWatcher(s.indexer, config.WatchConfig{Enabled: true, DebounceMs: 5}, zap.NewNop())
	require.NoError(t, err)
	require.NoError(t, watcher.Start([]string{t.TempDir()}))
	s.SetWatcher(watcher)
	t.Cleanup(func() { require.NoError(t, watcher.Stop()); s.DrainBackground(); s.indexer.Close() })
	req := editOperationRequest("batch_edit", "")
	req.Params.Arguments = map[string]any{"transaction_id": "local-no-wait-actual", "edits": []any{
		map[string]any{"op": "edit_file", "path": paths[0], "old_string": before, "new_string": after},
		map[string]any{"op": "edit_file", "path": paths[1], "old_string": before, "new_string": after},
	}}
	result, err := s.boundToolHandler(s.handleAtomicBatchEdit)(canonicalTimingContext(), req)
	require.NoError(t, err)
	require.False(t, result.IsError, "%v", result.Content)
	value, ok := s.batchTransactions.Load("local-no-wait-actual")
	require.True(t, ok)
	state := value.(*batchTransactionState)
	require.Equal(t, "committed", state.snapshot().DiskStatus)
	require.Len(t, s.batchFileObservationReferences(state.snapshot()), 2)
	ack := s.batchAcknowledgement(state)
	require.NotNil(t, ack.Snapshot)
	wait := ack.Snapshot.ReindexWait
	require.Equal(t, "not_applicable", wait.State, "only the actual local/no-requireFresh branch grants this disposition")
	require.EqualValues(t, 1, wait.Started)
	require.EqualValues(t, 1, wait.Completed)
	require.EqualValues(t, 1, wait.NotApplicable)
	require.Nil(t, wait.ObservedMS, "no invented zero wait duration")
	require.Nil(t, wait.AckMS)
	for _, path := range paths {
		require.Equal(t, after, readAtomicBatchFixture(t, path))
	}
}

// These controls hold genuine passive-wait completion channels. They prove
// wait applicability and monotonic observations, not physical member proof.
type batchAckWaitTierWatcher struct {
	watchedTierFixture
	done map[string]chan indexer.MutationResult
}

func (w *batchAckWaitTierWatcher) EnqueueFileMutations(_ context.Context, paths []string) (map[string]*indexer.MutationTicket, error) {
	out := make(map[string]*indexer.MutationTicket, len(paths))
	w.mu.Lock()
	defer w.mu.Unlock()
	for i, path := range paths {
		generation := uint64(i + 1)
		w.paths[path] = indexer.MutationTierState{Generation: generation, AckAt: time.Now(), FileGraphAt: time.Now()}
		ch := make(chan indexer.MutationResult, 1)
		w.done[path] = ch
		out[path] = &indexer.MutationTicket{Path: path, Generation: generation, Done: ch}
	}
	return out, nil
}

func TestBatchWaitApplicabilityPreservesFullAndRequireFreshWaits(t *testing.T) {
	for _, requireFresh := range []bool{false, true} {
		t.Run(map[bool]string{false: "full", true: "local_requires_relations"}[requireFresh], func(t *testing.T) {
			t.Setenv(batchTransactionDirEnv, filepath.Join(t.TempDir(), "transactions"))
			s, root := setupTestServer(t)
			s.editAckMode = config.EditAckFull
			if requireFresh {
				s.editAckMode = config.EditAckLocal
			}
			s.mutationReindexWait = time.Second
			w := &batchAckWaitTierWatcher{watchedTierFixture: watchedTierFixture{paths: make(map[string]indexer.MutationTierState)}, done: make(map[string]chan indexer.MutationResult)}
			s.SetWatcher(w)
			waiting := make(chan struct{})
			var waitOnce sync.Once
			s.releaseDispatchSlot = func(context.Context, <-chan struct{}) bool { waitOnce.Do(func() { close(waiting) }); return true }
			paths := []string{filepath.Join(root, "full-a.go"), filepath.Join(root, "full-b.go")}
			for _, path := range paths {
				require.NoError(t, os.WriteFile(path, []byte("package main\nfunc BeforeFull() {}\n"), 0600))
			}
			req := editOperationRequest("batch_edit", "")
			req.Params.Arguments = map[string]any{"transaction_id": "actual-wait", "edits": []any{
				map[string]any{"op": "edit_file", "path": paths[0], "old_string": "BeforeFull", "new_string": "AfterFull"},
				map[string]any{"op": "edit_file", "path": paths[1], "old_string": "BeforeFull", "new_string": "AfterFull"},
			}}
			if requireFresh {
				req.Params.Arguments.(map[string]any)["require_fresh"] = true
				req.Params.Arguments.(map[string]any)["freshness_scope"] = "relations"
				req.Params.Arguments.(map[string]any)["wait_deadline"] = time.Now().Add(time.Second).Format(time.RFC3339Nano)
			}
			ctx, ack, _ := s.withEditAcknowledgement(canonicalTimingContext())
			joined := make(chan struct{})
			var runErr error
			var result *mcpgo.CallToolResult
			go func() {
				defer close(joined)
				result, runErr = s.boundToolHandler(s.handleAtomicBatchEdit)(ctx, req)
				ack.finishActual()
				ack.finishReply(result, runErr)
			}()
			var completeOnce sync.Once
			complete := func() {
				completeOnce.Do(func() {
					w.mu.Lock()
					defer w.mu.Unlock()
					for path, ch := range w.done {
						state := w.paths[path]
						state.RelationsAt = time.Now()
						state.DerivedAt = time.Now()
						w.paths[path] = state
						ch <- indexer.MutationResult{RequestedGeneration: state.Generation, AppliedGeneration: state.Generation, Reindexed: true}
						close(ch)
					}
				})
			}
			defer func() {
				complete()
				select {
				case <-joined:
				case <-time.After(3 * time.Second):
					t.Error("owned batch wait did not join")
				}
				s.DrainBackground()
				s.indexer.Close()
			}()
			select {
			case <-waiting:
			case <-time.After(2 * time.Second):
				t.Fatal("required wait never entered its real passive-wait branch")
			}
			select {
			case <-joined:
				t.Fatal("required wait returned before its completion")
			default:
			}
			complete()
			select {
			case <-joined:
			case <-time.After(2 * time.Second):
				t.Fatal("required wait did not complete")
			}
			require.NoError(t, runErr)
			require.False(t, result.IsError, "%v", result.Content)
			wait := ack.snapshot().ReindexWait
			require.Equal(t, "measured", wait.State)
			require.Positive(t, wait.Started)
			require.Equal(t, wait.Started, wait.Completed)
			require.Zero(t, wait.NotApplicable)
			require.NotNil(t, wait.ObservedMS)
		})
	}
}

func TestBatchWaitApplicabilityDoesNotExemptRejectedOrUnobservedRoutes(t *testing.T) {
	for _, rejected := range []bool{false, true} {
		t.Run(map[bool]string{false: "full_already_complete", true: "local_rejected_before_policy"}[rejected], func(t *testing.T) {
			s := newAtomicBatchTestServer(t, &batchSetSchedulerSpy{})
			s.editAckMode = config.EditAckFull
			if rejected {
				s.editAckMode = config.EditAckLocal
			}
			root := t.TempDir()
			a := writeAtomicBatchFixture(t, root, "a.txt", "before\n")
			b := writeAtomicBatchFixture(t, root, "b.txt", "before\n")
			req := editOperationRequest("batch_edit", "")
			old := "before"
			if rejected {
				old = "missing-original"
			}
			req.Params.Arguments = map[string]any{"transaction_id": "not-blanket-applicability", "edits": []any{
				map[string]any{"op": "edit_file", "path": a, "old_string": old, "new_string": "after"},
				map[string]any{"op": "edit_file", "path": b, "old_string": old, "new_string": "after"},
			}}
			_, err := s.boundToolHandler(s.handleAtomicBatchEdit)(canonicalTimingContext(), req)
			require.NoError(t, err)
			value, ok := s.batchTransactions.Load("not-blanket-applicability")
			require.True(t, ok)
			state := value.(*batchTransactionState)
			ack := s.batchAcknowledgement(state)
			if !rejected {
				// Completed watcher tickets are consumed asynchronously by the server.
				// The original full call may legitimately measure that passive wait.
				// Join the actual retained receipts before exercising the no-wait route.
				for _, file := range state.snapshot().Files {
					v, found := s.mutationReceipts.Load(file.ReindexReceipt)
					require.True(t, found)
					select {
					case <-v.(*mutationReceipt).done:
					case <-time.After(time.Second):
						t.Fatal("server receipt completion did not join")
					}
				}
				ctx, completeAck, _ := s.withEditAcknowledgement(canonicalTimingContext())
				s.refreshBatchGraph(ctx, state)
				completeAck.finishActual()
				snapshot := completeAck.snapshot()
				ack.Snapshot = &snapshot
			}
			require.NotNil(t, ack.Snapshot)
			require.Equal(t, "unobserved", ack.Snapshot.ReindexWait.State)
			require.Zero(t, ack.Snapshot.ReindexWait.Started)
			require.Zero(t, ack.Snapshot.ReindexWait.NotApplicable)
		})
	}
}
