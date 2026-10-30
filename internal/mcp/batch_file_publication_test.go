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
