package mcp

import (
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
