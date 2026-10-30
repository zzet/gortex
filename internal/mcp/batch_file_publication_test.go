package mcp

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"github.com/zzet/gortex/internal/indexer"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestBatchFilePublicationBindsOnlyOriginalCommittedObservation(t *testing.T) {
	s := newMutationServer()
	path := filepath.Join(t.TempDir(), "fixture.go")
	transaction := batchTransactionReceipt{TransactionID: "transaction-original", Fingerprint: "offered-content"}
	record := &mutationCommitRecord{kind: batchFileObservationCommit, owner: s, id: "batch-file-original", key: transaction.TransactionID, fingerprint: transaction.Fingerprint, absPath: path, disk: mutationDiskCommitted, commitSeq: 1}
	s.mutationCommits.put(record)
	require.Same(t, record, s.batchPublicationObservation(transaction, path))
	foreign := transaction
	foreign.TransactionID = "transaction-other"
	require.Nil(t, s.batchPublicationObservation(foreign, path))
	foreign = transaction
	foreign.Fingerprint = "different-offer"
	require.Nil(t, s.batchPublicationObservation(foreign, path))
	require.Nil(t, s.batchPublicationObservation(transaction, path+".other"))
	record.disk = mutationDiskInFlight
	require.Nil(t, s.batchPublicationObservation(transaction, path), "writer return alone cannot supply durable batch authority")
	record.disk = mutationDiskCommitted
	s.bindBatchFilePublication(record, mutationReindexOutcome{Receipt: "publication-original", Generation: 7, Pending: true})
	require.Equal(t, "publication-original", record.reindexReceipt)
	require.EqualValues(t, 7, record.reindexGeneration)
	require.Nil(t, s.batchPublicationObservation(transaction, path), "recovery cannot open another original timeline")
	s.bindBatchFilePublication(record, mutationReindexOutcome{Receipt: "publication-retry", Generation: 8, Pending: true})
	require.Equal(t, "publication-original", record.reindexReceipt)
	_, found := s.mutationCommits.byFreshnessReceipt("publication-original")
	require.False(t, found, "observation cannot gain physical freshness lookup authority")
}

func TestBatchFileObservationStatusSeparatesWriterReturnFromDurableVerdict(t *testing.T) {
	s := newMutationServer()
	transaction := batchTransactionReceipt{TransactionID: "transaction-wire", Fingerprint: "offered"}
	record := &mutationCommitRecord{kind: batchFileObservationCommit, owner: s, id: "batch-file-wire", key: transaction.TransactionID, fingerprint: transaction.Fingerprint, relPath: "fixture.ts", disk: mutationDiskInFlight}
	s.mutationCommits.put(record)
	references := s.batchFileObservationReferences(transaction)
	require.Equal(t, []batchFileObservationReference{{Path: "fixture.ts", Receipt: record.id}}, references)
	references[0].Receipt = "changed-client-copy"
	require.Equal(t, record.id, s.batchFileObservationReferences(transaction)[0].Receipt)
	payload, handled := s.batchFileObservationPayload(t.Context(), record.id)
	require.True(t, handled)
	require.Equal(t, true, payload["found"])
	require.Equal(t, "in_flight", payload["batch_disk_verdict"])
	require.Equal(t, false, payload["writer_return_observed"])
	require.Equal(t, "not_applicable_shared_transaction_rpc", payload["acknowledgement_state"])
	require.NotContains(t, payload, "mutation_id")
	require.NotContains(t, payload, "graph_status_terminal")
	record.disk = mutationDiskFailed
	later, _ := s.batchFileObservationPayload(t.Context(), record.id)
	require.Equal(t, "failed", later["batch_disk_verdict"])
	require.Equal(t, "in_flight", payload["batch_disk_verdict"], "an earlier scalar snapshot stays frozen")
	foreign := transaction
	foreign.TransactionID = "other"
	require.Empty(t, s.batchFileObservationReferences(foreign))
}

func TestBatchFileObservationRealHandlerCommitAndFailedWriter(t *testing.T) {
	for _, fail := range []bool{false, true} {
		name := "commit"
		if fail {
			name = "failed-writer"
		}
		t.Run(name, func(t *testing.T) {
			s := newAtomicBatchTestServer(t, mutationTestWatcher{})
			s.ToolCallTimeout = -1
			file := writeAtomicBatchFixture(t, t.TempDir(), "target.txt", "before\n")
			transactionID := "file-observation-" + name
			var insideWriter bool
			s.batchWriteOverride = func(path string, content []byte, mode os.FileMode) error {
				if insideWriter {
					return durableAtomicWriteFile(path, content, mode)
				}
				value, ok := s.batchTransactions.Load(transactionID)
				require.True(t, ok)
				snapshot := value.(*batchTransactionState).snapshot()
				refs := s.batchFileObservationReferences(snapshot)
				require.Len(t, refs, 1)
				status, _ := s.batchFileObservationPayload(t.Context(), refs[0].Receipt)
				require.Equal(t, "in_flight", status["batch_disk_verdict"], "actual writer cannot certify the eventual durable verdict")
				insideWriter = true
				if fail {
					return errors.New("deliberate forward writer failure")
				}
				return durableAtomicWriteFile(path, content, mode)
			}
			req := editOperationRequest("batch_edit", "")
			req.Params.Arguments = map[string]any{"transaction_id": transactionID, "edits": []any{
				map[string]any{"op": "edit_file", "path": file, "old_string": "before\n", "new_string": "after\n"},
			}}
			h := s.boundToolHandler(s.handleAtomicBatchEdit)
			_, err := h(t.Context(), req)
			require.NoError(t, err)
			require.True(t, insideWriter)
			value, _ := s.batchTransactions.Load(transactionID)
			receipt := value.(*batchTransactionState).snapshot()
			refs := s.batchFileObservationReferences(receipt)
			require.Len(t, refs, 1)
			status, _ := s.batchFileObservationPayload(t.Context(), refs[0].Receipt)
			if fail {
				require.Equal(t, "failed", status["batch_disk_verdict"])
				require.NotContains(t, status, "publication_phases")
				return
			}
			require.Equal(t, "committed", status["batch_disk_verdict"])
			sum := sha256.Sum256([]byte("after\n"))
			require.Equal(t, hex.EncodeToString(sum[:]), status["after_sha256"])
			require.Equal(t, "after\n", readAtomicBatchFixture(t, file))
			require.Equal(t, true, status["writer_return_observed"])
			manifest, err := os.ReadFile(batchManifestPath(transactionID))
			require.NoError(t, err)
			require.NotContains(t, string(manifest), refs[0].Receipt, "timing is not durable transaction authority")
		})
	}
}

func TestBatchFileObservationRealRollbackRejectsSuccessfulWriter(t *testing.T) {
	s := newAtomicBatchTestServer(t, mutationTestWatcher{})
	dir := t.TempDir()
	file := writeAtomicBatchFixture(t, dir, "target.txt", "before\n")
	var failedSync bool
	s.batchDurabilityOverride = &batchDurabilityOps{syncDirectory: func(path string) error {
		if filepath.Clean(path) == filepath.Clean(dir) && !failedSync {
			failedSync = true
			return errors.New("forward directory sync refused")
		}
		return syncBatchDirectory(path)
	}}
	req := editOperationRequest("batch_edit", "")
	req.Params.Arguments = map[string]any{"transaction_id": "file-observation-rollback", "edits": []any{
		map[string]any{"op": "edit_file", "path": file, "old_string": "before\n", "new_string": "after\n"},
	}}
	_, err := s.boundToolHandler(s.handleAtomicBatchEdit)(t.Context(), req)
	require.NoError(t, err)
	value, _ := s.batchTransactions.Load("file-observation-rollback")
	transaction := value.(*batchTransactionState).snapshot()
	require.Equal(t, "rolled_back", transaction.DiskStatus)
	require.Equal(t, "before\n", readAtomicBatchFixture(t, file))
	refs := s.batchFileObservationReferences(transaction)
	require.Len(t, refs, 1)
	payload, _ := s.batchFileObservationPayload(t.Context(), refs[0].Receipt)
	require.Equal(t, true, payload["writer_return_observed"], "successful forward return really occurred")
	require.Equal(t, "failed", payload["batch_disk_verdict"], "rolled-back bytes never become a committed batch clock")
	require.NotContains(t, payload, "publication_phases")
	require.Empty(t, payload["publication_receipt"])
}

func TestBatchFileObservationRealReadmissionCannotReplaceOriginalTicket(t *testing.T) {
	done := make(chan indexer.MutationResult)
	defer close(done)
	s := newAtomicBatchTestServer(t, mutationTestWatcher{generation: 71, done: done})
	s.mutationReindexWait = time.Nanosecond
	file := writeAtomicBatchFixture(t, t.TempDir(), "target.txt", "before\n")
	req := editOperationRequest("batch_edit", "")
	req.Params.Arguments = map[string]any{"transaction_id": "file-observation-readmission", "edits": []any{
		map[string]any{"op": "edit_file", "path": file, "old_string": "before\n", "new_string": "after\n"},
	}}
	_, err := s.boundToolHandler(s.handleAtomicBatchEdit)(t.Context(), req)
	require.NoError(t, err)
	value, _ := s.batchTransactions.Load("file-observation-readmission")
	state := value.(*batchTransactionState)
	original := state.snapshot()
	require.Equal(t, "pending", original.GraphStatus)
	refs := s.batchFileObservationReferences(original)
	require.Len(t, refs, 1)
	before, _ := s.batchFileObservationPayload(t.Context(), refs[0].Receipt)
	originalTicket := original.Files[0].ReindexReceipt
	require.NotEmpty(t, originalTicket)
	require.Equal(t, originalTicket, before["publication_receipt"])
	s.mutationReceipts.Delete(originalTicket)
	replacement, err := s.batchTransactionStatus(t.Context(), original.TransactionID)
	require.NoError(t, err)
	require.NotEmpty(t, replacement.Files[0].ReindexReceipt)
	require.NotEqual(t, originalTicket, replacement.Files[0].ReindexReceipt, "durable transaction legitimately readmits lost publication")
	after, _ := s.batchFileObservationPayload(t.Context(), refs[0].Receipt)
	require.Equal(t, originalTicket, after["publication_receipt"], "readmission cannot borrow a later ticket for original writer timing")
	require.Equal(t, before["publication_generation"], after["publication_generation"])
}

func TestBatchFileObservationActualAdmissionRefusesEarlyPublisher(t *testing.T) {
	for _, immediate := range []bool{false, true} {
		name := "pending-actual-ticket"
		if immediate {
			name = "completed-before-admission-return"
		}
		t.Run(name, func(t *testing.T) {
			watcher := &canonicalPublicationWatcher{hold: !immediate}
			s := newAtomicBatchTestServer(t, watcher)
			s.mutationReindexWait = time.Nanosecond
			defer func() {
				if !immediate {
					for _, done := range watcher.results {
						close(done)
					}
				}
			}()
			file := writeAtomicBatchFixture(t, t.TempDir(), "target.txt", "before\n")
			idx := &indexer.Indexer{}
			earlyChecked := false
			s.batchWriteOverride = func(path string, content []byte, mode os.FileMode) error {
				require.Nil(t, s.readRepairObserver(t.Context(), path, idx), "publisher during writer cannot acquire eventual durable transaction ownership")
				earlyChecked = true
				return durableAtomicWriteFile(path, content, mode)
			}
			req := editOperationRequest("batch_edit", "")
			req.Params.Arguments = map[string]any{"transaction_id": "early-publisher-" + name, "edits": []any{map[string]any{"op": "edit_file", "path": file, "old_string": "before\n", "new_string": "after\n"}}}
			_, err := s.boundToolHandler(s.handleAtomicBatchEdit)(t.Context(), req)
			require.NoError(t, err)
			require.True(t, earlyChecked)
			require.Len(t, watcher.records, 1)
			require.NotNil(t, watcher.records[0], "real scheduler receives original batch file timeline")
			value, _ := s.batchTransactions.Load("early-publisher-" + name)
			tx := value.(*batchTransactionState).snapshot()
			refs := s.batchFileObservationReferences(tx)
			require.Len(t, refs, 1)
			payload, _ := s.batchFileObservationPayload(t.Context(), refs[0].Receipt)
			require.Equal(t, tx.Files[0].ReindexReceipt, payload["publication_receipt"])
			if immediate {
				// A completed requested publisher cannot be relabeled as a later reader's
				// independently measured execution merely because the bytes now match.
				require.Eventually(t, func() bool { return s.readRepairObserver(t.Context(), file, idx) == nil }, time.Second, time.Millisecond)
				require.NotContains(t, payload, "read_assisted_publication")
			} else {
				witness, ok := s.captureReadRepairWitness(file, idx)
				require.True(t, ok, "only the actual committed+admitted pending ticket can capture a subsequent reader")
				require.Equal(t, tx.Files[0].ReindexReceipt, witness.receipt)
				require.Equal(t, tx.Files[0].ReindexGeneration, witness.generation)
			}
		})
	}
}
