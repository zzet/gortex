package mcp

import (
	"context"
	"fmt"
	"os"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
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
	record.mu.Lock()
	record.moveFile.writeVerdict = "pending"
	record.mu.Unlock()
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

func TestMoveActualCoalescedPublicReaderAndDirectKeepBothCausesOneExecution(t *testing.T) {
	s, move, ctx, _, _, target := moveObservationExecutionFixture(t)
	root, ok := s.multiIndexer.RepoRoot("fixture")
	require.True(t, ok)
	entered, resume := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(resume) }) }
	gateDone := make(chan error, 1)
	go func() {
		gateDone <- s.multiIndexer.RunRepositoryTopologyBatch(t.Context(), func(context.Context) error { close(entered); <-resume; return nil })
	}()
	type readerResult struct {
		result *mcpgo.CallToolResult
		err    error
	}
	readerDone, directDone := make(chan readerResult, 1), make(chan bool, 1)
	gateJoined, readerJoined, directJoined := false, false, false
	readerStarted, directStarted := false, false
	t.Cleanup(func() {
		release()
		if !gateJoined {
			select {
			case <-gateDone:
			case <-time.After(5 * time.Second):
				t.Error("coalescing gate did not join")
			}
		}
		if readerStarted && !readerJoined {
			select {
			case <-readerDone:
			case <-time.After(5 * time.Second):
				t.Error("actual public reader did not join")
			}
		}
		if directStarted && !directJoined {
			select {
			case <-directDone:
			case <-time.After(5 * time.Second):
				t.Error("actual direct caller did not join")
			}
		}
	})
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("existing topology gate was not held")
	}
	readerStarted = true
	go func() {
		request := mcpgo.CallToolRequest{}
		request.Params.Arguments = map[string]any{"id": "fixture/target.go::Moved", "context_lines": 0}
		result, err := s.handleGetSymbolSource(WithSessionCWD(WithSessionID(ctx, "actual-coalesced-reader"), root), request)
		readerDone <- readerResult{result, err}
	}()
	directStarted = true
	go func() { directDone <- move.reindex(ctx, target) }()
	// The existing gate blocks drain before it takes pending paths. Observe the
	// two real callers parked after coordinator admission, not just goroutine
	// launch or observer construction; then release one genuinely coalesced batch.
	require.Eventually(t, func() bool {
		record := move.records[target]
		record.mu.RLock()
		pending := record.moveFile.directState == "pending_observer" && record.moveFile.readState == "pending_observer"
		record.mu.RUnlock()
		if !pending {
			return false
		}
		stack := make([]byte, 128<<10)
		n := runtime.Stack(stack, true)
		return strings.Count(string(stack[:n]), "(*repositoryMutationCoordinator).reconcileObserved(") >= 2
	}, 5*time.Second, time.Millisecond)
	release()
	select {
	case err := <-gateDone:
		gateJoined = true
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("gate did not finish")
	}
	select {
	case result := <-readerDone:
		readerJoined = true
		require.NoError(t, result.err)
		require.False(t, result.result.IsError)
	case <-time.After(5 * time.Second):
		t.Fatal("public read did not finish")
	}
	select {
	case success := <-directDone:
		directJoined = true
		require.True(t, success)
	case <-time.After(5 * time.Second):
		t.Fatal("direct reconcile did not finish")
	}
	status, _ := s.moveFileObservationPayload(t.Context(), move.records[target].id)
	require.Equal(t, "observed", status["direct_reconcile_state"])
	require.Equal(t, "observed", status["read_assisted_reconcile_state"])
	direct := status["direct_reconcile"].(map[string]any)
	repair := status["read_assisted_reconcile"].(map[string]any)
	for _, key := range []string{"owner_generation", "window_ns", "accepted_blob_sha", "writer_return_to_acceptance_ns", "started_from_ack_ns", "finished_from_ack_ns", "accepted_from_start_ns", "reconcile_causes"} {
		require.Equal(t, direct[key], repair[key], "different executed %s", key)
	}
	require.Equal(t, "actual-coalesced-reader", repair["request_cause"].(mutationReadRepairCause).Session)
	require.NotEqual(t, repair["request_cause"], direct["request_cause"])
	require.Len(t, repair["reconcile_causes"], 2)
	directPhases := direct["phase_accounting"].(map[string]any)
	repairPhases := repair["phase_accounting"].(map[string]any)
	require.Equal(t, "move_direct_reconcile", directPhases["scope"])
	require.Equal(t, "move_read_assisted_reconcile", repairPhases["scope"])
	for key, value := range directPhases {
		if key != "scope" {
			require.Equal(t, value, repairPhases[key], "different partition %s", key)
		}
	}
}
