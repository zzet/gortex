package mcp

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/indexer"
)

// Exercise the durable writer boundary rather than manually constructing a
// projection cohort. A partial physical forward write is not batch authority.
func TestBatchPendingViewsRequireCompleteDurableFrontier(t *testing.T) {
	for _, rollback := range []bool{false, true} {
		name := "commit"
		if rollback {
			name = "rollback"
		}
		t.Run(name, func(t *testing.T) {
			previous := pendingFileProjections
			pendingFileProjections = newPendingProjections()
			t.Cleanup(func() { pendingFileProjections = previous })
			t.Setenv(batchTransactionDirEnv, filepath.Join(t.TempDir(), "transactions"))
			s, root := setupTestServer(t)
			done := make(chan indexer.MutationResult)
			t.Cleanup(func() { close(done) })
			s.SetWatcher(mutationTestWatcher{done: done})
			s.mutationReindexWait = time.Nanosecond
			a := filepath.Join(root, "main.go")
			b := writeAtomicBatchFixture(t, root, "second.go", "package main\nfunc SecondBefore() {}\n")
			_, err := s.indexer.IncrementalReindexPaths(root, []string{b})
			require.NoError(t, err)
			beforeA := readAtomicBatchFixture(t, a)
			beforeB := readAtomicBatchFixture(t, b)
			writes, failed := 0, false
			s.batchWriteOverride = func(path string, content []byte, mode os.FileMode) error {
				if failed {
					return durableAtomicWriteFile(path, content, mode)
				}
				writes++
				require.NoError(t, durableAtomicWriteFile(path, content, mode))
				if writes == 1 {
					require.Nil(t, liveEntry(pendingFileProjections, a), "one successful writer cannot publish half a batch")
					require.Nil(t, liveEntry(pendingFileProjections, b), "unwritten sibling cannot become a pending view")
					require.Nil(t, s.graph.GetNode("main.go::BatchFirstAfter"))
					require.Nil(t, s.graph.GetNode("second.go::BatchSecondAfter"))
				}
				if writes == 2 && rollback {
					failed = true
					return errors.New("deliberate second forward writer failure after bytes changed")
				}
				return nil
			}
			s.batchDurabilityOverride = &batchDurabilityOps{syncDirectory: func(path string) error {
				if filepath.Clean(path) == filepath.Clean(root) {
					require.Nil(t, liveEntry(pendingFileProjections, a), "directory durability must precede cache admission")
					require.Nil(t, liveEntry(pendingFileProjections, b))
				}
				return syncBatchDirectory(path)
			}}
			res := callBatchEdit(t, s, map[string]any{"transaction_id": "pending-view-" + name, "edits": []any{
				map[string]any{"op": "edit_file", "path": a, "old_string": "func helper() {}", "new_string": "func helper() {}\nfunc BatchFirstAfter() {}"},
				map[string]any{"op": "edit_file", "path": b, "old_string": "func SecondBefore() {}", "new_string": "func BatchSecondAfter() {}"},
			}})
			var receipt batchTransactionReceipt
			require.NoError(t, json.Unmarshal([]byte(readText(t, res)), &receipt))
			require.GreaterOrEqual(t, writes, 2)
			if rollback {
				require.Equal(t, "rolled_back", receipt.DiskStatus)
				require.Equal(t, beforeA, readAtomicBatchFixture(t, a))
				require.Equal(t, beforeB, readAtomicBatchFixture(t, b))
				require.Nil(t, liveEntry(pendingFileProjections, a))
				require.Nil(t, liveEntry(pendingFileProjections, b))
				return
			}
			require.Equal(t, "committed", receipt.DiskStatus)
			require.Equal(t, "pending", receipt.GraphStatus)
			first, second := liveEntry(pendingFileProjections, a), liveEntry(pendingFileProjections, b)
			require.NotNil(t, first, "durable batch bytes need a pending file view before the watcher finishes")
			require.NotNil(t, second, "the complete durable frontier must publish together")
			require.NotNil(t, graph.NewOverlaidView(s.graph, first.layer).GetNode("main.go::BatchFirstAfter"))
			require.NotNil(t, graph.NewOverlaidView(s.graph, second.layer).GetNode("second.go::BatchSecondAfter"))
			require.Positive(t, first.commitSeq)
			require.Equal(t, first.commitSeq, second.commitSeq, "one successful durable frontier has one process-local sequence")
			response := s.batchProjectionResponse(receipt)
			require.Equal(t, first.commitSeq, response.CommitSeq)
			require.Len(t, response.FileViews, 2)
			for _, view := range response.FileViews {
				require.True(t, view.ReadbackExact)
				require.Equal(t, "projected", view.FileView.State)
				require.NotEmpty(t, view.ContentSHA256)
				require.Equal(t, "file current; recorded graph status: pending", view.GraphStatusHint)
			}
			journal, err := os.ReadFile(batchManifestPath(receipt.TransactionID))
			require.NoError(t, err)
			require.NotContains(t, string(journal), "commit_seq", "process-local projection origin is never journal authority")
			status := callBatchEdit(t, s, map[string]any{"transaction_id": receipt.TransactionID, "status_only": true})
			require.False(t, status.IsError)
			require.Equal(t, 2, writes, "status cannot repeat forward source effects")
			require.Equal(t, response.CommitSeq, s.batchProjectionResponse(receipt).CommitSeq)
			restarted := &Server{watcher: mutationTestWatcher{done: done}, session: newSessionState(), mutationReindexWait: time.Nanosecond}
			recovered, err := restarted.batchTransactionStatus(t.Context(), receipt.TransactionID)
			require.NoError(t, err)
			require.Equal(t, "committed", recovered.DiskStatus)
			restartResponse := restarted.batchProjectionResponse(recovered)
			require.Zero(t, restartResponse.CommitSeq)
			for _, view := range restartResponse.FileViews {
				require.False(t, view.ReadbackExact, "even an existing live cache cannot reconstruct an original process-local origin")
				require.Equal(t, "unavailable", view.FileView.State)
			}
			require.Nil(t, s.graph.GetNode("main.go::BatchFirstAfter"), "pending file evidence is not store freshness")
			require.Nil(t, s.graph.GetNode("second.go::BatchSecondAfter"))
		})
	}
}

func TestBatchPendingCohortRefusalKeepsEveryPriorEntry(t *testing.T) {
	for _, reason := range []string{"file-cap", "byte-cap", "newer-pointer"} {
		t.Run(reason, func(t *testing.T) {
			previous := pendingFileProjections
			pendingFileProjections = newPendingProjections()
			t.Cleanup(func() { pendingFileProjections = previous })
			t.Setenv(batchTransactionDirEnv, filepath.Join(t.TempDir(), "transactions"))
			s, root := setupTestServer(t)
			done := make(chan indexer.MutationResult)
			t.Cleanup(func() { close(done) })
			s.SetWatcher(mutationTestWatcher{done: done})
			s.mutationReindexWait = time.Nanosecond
			a := filepath.Join(root, "main.go")
			b := writeAtomicBatchFixture(t, root, "sibling.go", "package main\nfunc SiblingBefore() {}\n")
			_, err := s.indexer.IncrementalReindexPaths(root, []string{b})
			require.NoError(t, err)
			if reason == "file-cap" {
				for i := 0; i < projectionMaxFiles-1; i++ {
					name := fmt.Sprintf("filler-%03d.go", i)
					require.NoError(t, pendingFileProjections.publish(syntheticProjectionEntry(filepath.Join(root, name), name, "h", 1, diskStamp{}, nil)))
				}
			}
			if reason == "byte-cap" {
				filler := syntheticProjectionEntry(filepath.Join(root, "filler.go"), "filler.go", "h", 1, diskStamp{}, nil)
				filler.size = projectionMaxBytes - 1
				require.NoError(t, pendingFileProjections.publish(filler))
			}
			before := pendingFileProjections.snap.Load()
			var newer *projectionEntry
			s.batchWriteOverride = func(path string, content []byte, mode os.FileMode) error {
				if err := durableAtomicWriteFile(path, content, mode); err != nil {
					return err
				}
				if reason == "newer-pointer" && path == a {
					stamp, err := statDiskStamp(a)
					require.NoError(t, err)
					newer = syntheticProjectionEntry(a, "main.go", "newer", ^uint64(0), stamp, content)
					require.NoError(t, pendingFileProjections.publish(newer))
					before = pendingFileProjections.snap.Load()
				}
				return nil
			}
			result := callBatchEdit(t, s, map[string]any{"transaction_id": "refuse-" + reason, "edits": []any{
				map[string]any{"op": "edit_file", "path": a, "old_string": "func helper() {}", "new_string": "func helper() {}\nfunc RefusedFirst() {}"},
				map[string]any{"op": "edit_file", "path": b, "old_string": "func SiblingBefore() {}", "new_string": "func RefusedSecond() {}"},
			}})
			var receipt batchTransactionReceipt
			require.NoError(t, json.Unmarshal([]byte(readText(t, result)), &receipt))
			require.Equal(t, "committed", receipt.DiskStatus, "cache refusal never reverses a durable batch")
			require.Same(t, before, pendingFileProjections.snap.Load(), "one rejected member cannot replace any prior entry")
			require.Same(t, newer, liveEntry(pendingFileProjections, a))
			require.Nil(t, liveEntry(pendingFileProjections, b))
			for _, view := range s.batchProjectionResponse(receipt).FileViews {
				require.False(t, view.ReadbackExact)
				require.Equal(t, "unavailable", view.FileView.State)
			}
		})
	}
}
