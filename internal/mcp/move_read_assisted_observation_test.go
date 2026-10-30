package mcp

import (
 "context"
 "fmt"
 "os"
 "testing"
 "time"

 "github.com/stretchr/testify/require"
)

func TestMoveReadAssistedOriginalFileWinnerAndImmutableCallback(t *testing.T) {
 s, move, ctx, _, _, target := moveObservationExecutionFixture(t)
 idx := s.multiIndexer.GetIndexer("fixture")
 readerCtx := WithSessionID(ctx, "independent-move-reader")
 observer := s.readRepairObserver(readerCtx, target, idx)
 require.NotNil(t, observer)
 actual := captureMoveActualExecution(t, s, target, observer)
 require.True(t, actual.AcceptedFor(idx, move.records[target].newSHA))
 status, _ := s.moveFileObservationPayload(t.Context(), move.records[target].id)
 require.Equal(t, "observed", status["read_assisted_reconcile_state"])
 require.Equal(t, "not_started", status["direct_reconcile_state"])
 repair := status["read_assisted_reconcile"].(map[string]any)
 require.Equal(t, "move_read_assisted_reconcile", repair["scope"])
 require.Equal(t, false, repair["additive_with_acknowledgement"])
 require.Equal(t, "independent-move-reader", repair["request_cause"].(mutationReadRepairCause).Session)
 start, end := actual.Bounds()
 require.Equal(t, end.Sub(start).Nanoseconds(), repair["finished_from_ack_ns"].(int64)-repair["started_from_ack_ns"].(int64))
 require.Equal(t, actual.AcceptedAt().Sub(start).Nanoseconds(), repair["accepted_from_start_ns"])
 require.GreaterOrEqual(t, repair["accepted_from_start_ns"].(int64), int64(0))
 require.LessOrEqual(t, repair["accepted_from_start_ns"].(int64), repair["window_ns"].(int64))
 require.Equal(t, "independent-move-reader", repair["reconcile_causes"].([]mutationReadRepairCause)[0].Session)
 require.Nil(t, s.readRepairObserver(readerCtx, target, idx), "original read admission cannot be reissued")
 observer.Complete(actual)
 again, _ := s.moveFileObservationPayload(t.Context(), move.records[target].id)
 require.Equal(t, repair, again["read_assisted_reconcile"])
}

func TestMoveReadAssistedRejectsNewerPublicationOwners(t *testing.T) {
 for _, kind := range []mutationCommitKind{physicalMutationCommit, batchFileObservationCommit, moveFileObservationCommit} {
  t.Run(fmt.Sprintf("kind-%d", kind), func(t *testing.T) {
   s, move, ctx, _, _, target := moveObservationExecutionFixture(t)
   idx := s.multiIndexer.GetIndexer("fixture")
   observer := s.readRepairObserver(WithSessionID(ctx, "original-reader"), target, idx)
   require.NotNil(t, observer)
   // Complete a real later same-byte write. Its separately retained ledger
   // owner has the actual later commit order for each supported owner kind.
   bytes, err := os.ReadFile(target)
   require.NoError(t, err)
   require.NoError(t, writeMoveFileObserved(nil, target, bytes))
   foreign := &mutationCommitRecord{kind: kind, owner: s, id: "foreign-newer", absPath: target, disk: mutationDiskCommitted, newSHA: gitBlobSHA(bytes), committedAt: time.Now(), commitSeq: mutationDiskCommitSequence.Add(1)}
   s.mutationCommits.put(foreign)
   actual := captureMoveActualExecution(t, s, target, observer)
   require.True(t, actual.AcceptedFor(idx, gitBlobSHA(bytes)), "test must reach actual accepted-byte ownership rejection")
   status, _ := s.moveFileObservationPayload(t.Context(), move.records[target].id)
   require.Equal(t, "unavailable_competing_commit", status["read_assisted_reconcile_state"])
   require.NotContains(t, status, "read_assisted_reconcile")
   // A repeated callback cannot rewrite this final refusal.
   observer.Complete(actual)
   repeated, _ := s.moveFileObservationPayload(t.Context(), move.records[target].id)
   require.Equal(t, status["read_assisted_reconcile_state"], repeated["read_assisted_reconcile_state"])
   _, moveOwner := s.moveReadRepairObserver(ctx, target, idx)
   require.Equal(t, kind == moveFileObservationCommit, moveOwner, "latest non-move authority must not fall back to an older move")
  })
 }
}

func TestMoveReadAssistedPendingVerdictAndUnknownOwnerRemainUnavailable(t *testing.T) {
 s, move, ctx, _, _, target := moveObservationExecutionFixture(t)
 idx := s.multiIndexer.GetIndexer("fixture")
 require.Nil(t, s.readRepairObserver(ctx, target, nil))
 record := move.records[target]
 record.mu.Lock(); record.moveFile.writeVerdict = "pending"; record.mu.Unlock()
 observer := s.readRepairObserver(WithSessionID(ctx, "pending-move-reader"), target, idx)
 require.NotNil(t, observer, "capture only a weak known-writer-return candidate")
 actual := captureMoveActualExecution(t, s, target, observer)
 require.True(t, actual.AcceptedFor(idx, record.newSHA))
 status, _ := s.moveFileObservationPayload(context.Background(), record.id)
 require.Equal(t, "unavailable_move_write_verdict", status["read_assisted_reconcile_state"])
 require.NotContains(t, status, "read_assisted_reconcile")
 move.finishWrites(true)
 observer.Complete(actual)
 after, _ := s.moveFileObservationPayload(t.Context(), record.id)
 require.Equal(t, "unavailable_move_write_verdict", after["read_assisted_reconcile_state"], "later final verdict cannot manufacture a callback window")
}
