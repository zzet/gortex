package mcp

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"testing"

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
