package mcp

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestBatchAcknowledgementOriginalExecutionDuplicateEvictionAndRestart(t *testing.T) {
	s := newAtomicBatchTestServer(t, mutationTestWatcher{})
	file := writeAtomicBatchFixture(t, t.TempDir(), "target.txt", "before\n")
	var writes atomic.Int64
	s.batchWriteOverride = func(path string, content []byte, mode os.FileMode) error {
		writes.Add(1)
		return durableAtomicWriteFile(path, content, mode)
	}
	req := editOperationRequest("batch_edit", "")
	req.Params.Arguments = map[string]any{"transaction_id": "original-ack-owner", "edits": []any{
		map[string]any{"op": "edit_file", "path": file, "old_string": "before", "new_string": "after"},
	}}
	h := s.boundToolHandler(s.handleAtomicBatchEdit)
	result, err := h(t.Context(), req)
	require.NoError(t, err)
	require.False(t, result.IsError)
	require.Equal(t, "after\n", readAtomicBatchFixture(t, file))
	value, ok := s.batchTransactions.Load("original-ack-owner")
	require.True(t, ok)
	state := value.(*batchTransactionState)
	first := s.batchAcknowledgement(state)
	require.Equal(t, "observed", first.State)
	require.Equal(t, "measured", first.Snapshot.ReplyState)
	require.True(t, strings.HasPrefix(first.Reference, "batch-ack-"))
	result, err = h(t.Context(), req)
	require.NoError(t, err)
	require.False(t, result.IsError)
	require.EqualValues(t, 1, writes.Load(), "duplicate does not execute another physical batch")
	require.Equal(t, first, s.batchAcknowledgement(state), "duplicate reply cannot replace the original RPC wall")

	manifest, err := os.ReadFile(batchManifestPath("original-ack-owner"))
	require.NoError(t, err)
	require.NotContains(t, string(manifest), first.Reference)
	require.NotContains(t, string(manifest), "acknowledgement", "no new journal field/version")
	restarted := &Server{}
	loaded, action, err := restarted.loadOrCreateBatchTransaction(t.Context(), "original-ack-owner", "")
	require.NoError(t, err)
	require.NotEqual(t, "execute", action)
	require.Equal(t, "unavailable_reference", restarted.batchAcknowledgement(loaded).State)

	for i := range maxMutationCommits {
		s.mutationCommits.put(&mutationCommitRecord{id: fmt.Sprintf("batch-cap-%d", i), startedAt: time.Now()})
	}
	require.Equal(t, "unavailable_ledger_entry", s.batchAcknowledgement(state).State)
	result, err = h(t.Context(), req)
	require.NoError(t, err)
	require.False(t, result.IsError)
	require.Equal(t, first.Reference, s.batchAcknowledgement(state).Reference)
	require.Nil(t, s.batchAcknowledgement(state).Snapshot, "an old scalar cannot resurrect evicted timing")
	require.EqualValues(t, 1, writes.Load())
}

func TestBatchAcknowledgementCannotBecomePhysicalMutationOrSourceEvidence(t *testing.T) {
	assertBatchObservationCannotBecomePhysical(t, batchAcknowledgementCommit)
}

func TestBatchFileObservationCannotBecomePhysicalMutationOrSourceEvidence(t *testing.T) {
	assertBatchObservationCannotBecomePhysical(t, batchFileObservationCommit)
}

func assertBatchObservationCannotBecomePhysical(t *testing.T, kind mutationCommitKind) {
	t.Helper()
	s := newMutationServer()
	_, ack, _ := s.withEditAcknowledgement(t.Context())
	record := &mutationCommitRecord{
		kind: kind, id: "batch-observation-control", startedAt: time.Now(), acknowledgement: ack,
		// Even a wrongly supplied physical-looking field must not enroll the
		// distinct kind in recovery, replay or source-authority paths.
		key: "caller-key", fingerprint: "fingerprint", relPath: "pretend.go", absPath: filepath.Join(t.TempDir(), "pretend.go"),
		reindexReceipt: "mutation-123", owner: s,
	}
	s.mutationCommits.put(record)
	_, found := s.mutationCommits.byReceipt(record.id)
	require.False(t, found)
	_, found = s.mutationCommits.byMutationID(record.key)
	require.False(t, found)
	_, found = s.mutationCommits.byFreshnessReceipt(record.reindexReceipt)
	require.False(t, found)
	_, found = s.mutationCommits.recentForPath(record.relPath)
	require.False(t, found)
	require.Empty(t, s.mutationCommits.recent(maxMutationCommits))
	s.pendingSourceFiles.register(record)
	require.Empty(t, s.pendingSourceFiles.versions)
	note := &mutationCommitNote{}
	note.observe(record)
	require.Empty(t, note.verdict().Status)
	record.markCommitted("pretend", 10)
	record.markFailed(errors.New("not a physical failure"))
	record.markNotApplied(errors.New("not a physical refusal"))
	record.recordGraph(mutationReindexOutcome{Receipt: "mutation-999", Reindexed: true})
	record.markReceiptLost(record.reindexReceipt)
	record.markSourceRecoveredForReceipt(record.reindexReceipt)
	record.retainResponse(map[string]any{"pretend": true})
	_, replayed := record.replayResponse()
	require.False(t, replayed)
	_, _, replayed = s.replayMutation(record.key, record.fingerprint)
	require.False(t, replayed)
	require.Empty(t, record.diskStatus())
	require.Equal(t, mutationCommitSnapshot{}, record.snapshot())
	require.False(t, mutationCommitInScope(t.Context(), record))
	require.Equal(t, map[string]any{"found": false, "reason": "not_physical_commit"}, s.mutationStatusPayload(record))
	require.Nil(t, s.openCommittedPublicationRecord(t.Context(), record))
	require.Nil(t, retainedMutationAcknowledgement(record))
	payload := map[string]any{}
	attachMutationCommit(payload, record)
	require.Empty(t, payload)
	require.Empty(t, record.disk)
	require.Empty(t, record.graph)
	require.False(t, record.pendingSourceRecovered)
}

func TestBatchAcknowledgementConcurrentReservationAndOwnerIsolation(t *testing.T) {
	s := newMutationServer()
	state := &batchTransactionState{receipt: batchTransactionReceipt{TransactionID: "owner"}}
	firstCtx, first, _ := s.withEditAcknowledgement(t.Context())
	secondCtx, second, _ := s.withEditAcknowledgement(t.Context())
	var joined sync.WaitGroup
	joined.Add(2)
	go func() { defer joined.Done(); s.bindBatchAcknowledgement(firstCtx, state) }()
	go func() { defer joined.Done(); s.bindBatchAcknowledgement(secondCtx, state) }()
	joined.Wait()
	got := s.batchAcknowledgement(state)
	require.Equal(t, "observed", got.State)
	require.Contains(t, []uint64{first.id, second.id}, got.Snapshot.RPCID)
	s.mutationCommits.mu.Lock()
	retained := len(s.mutationCommits.ordered)
	s.mutationCommits.mu.Unlock()
	require.Equal(t, 1, retained, "one reserved scalar must produce one bounded owner")
	foreign := &batchTransactionState{receipt: batchTransactionReceipt{TransactionID: "other"}, acknowledgementReceipt: got.Reference}
	require.Equal(t, "unavailable_ledger_entry", s.batchAcknowledgement(foreign).State)

	// Existing ledger expiration also clears the batch payload. A subsequent
	// duplicate/late bind never replaces its original reserved scalar.
	s.mutationCommits.mu.Lock()
	record := s.mutationCommits.byID[got.Reference]
	record.mu.Lock()
	record.startedAt = time.Now().Add(-2 * mutationCommitRetention)
	record.mu.Unlock()
	s.mutationCommits.mu.Unlock()
	s.mutationCommits.put(&mutationCommitRecord{id: "expire-trigger", startedAt: time.Now()})
	s.bindBatchAcknowledgement(secondCtx, state)
	require.Equal(t, "unavailable_ledger_entry", s.batchAcknowledgement(state).State)
}
