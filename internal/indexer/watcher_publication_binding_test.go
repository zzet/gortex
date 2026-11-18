package indexer

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

func TestCanonicalMutationBindsPublicationBeforeWake(t *testing.T) {
	w := newMutationRetryTestWatcher(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "edit.go")
	writeTestFile(t, path, "package edited\n")
	w.indexer = newTestIndexer(graph.New())
	w.indexer.SetRootPath(dir)
	record := NewPublicationPhaseRecorder(1, 1).Begin("", "commit", PublicationSourceMCPEdit, time.Now())
	claimed := make(chan PublicationPhaseSnapshot, 1)
	w.pointMutationClaimed = func(string) { claimed <- record.Snapshot() }
	w.pointMutationPatch = func(string, ChangeKind, uint64) error { return nil }

	ticket, err := w.EnqueueFileMutation(WithPublicationRecord(t.Context(), record), path)
	require.NoError(t, err)
	require.NotNil(t, ticket)
	require.Same(t, record, ticket.PublicationRecord)
	select {
	case snapshot := <-claimed:
		require.Equal(t, ticket.Generation, snapshot.Ticket, "the demand-woken callback must already know the original ticket")
		require.Contains(t, phaseOffsets(snapshot), PublicationTicketEnqueued)
	case <-time.After(5 * time.Second):
		t.Fatal("mutation never reached the existing claim seam")
	}
	result := awaitMutationRetryResult(t, ticket)
	require.NoError(t, result.Err)
	require.Equal(t, ticket.Generation, result.RequestedGeneration)
	require.Equal(t, ticket.Generation, result.AppliedGeneration)
	require.False(t, record.Snapshot().Terminal, "only Done's existing consumer owns terminal receipt observation")
}

func TestCanonicalMutationCoalescingKeepsOriginalRecordIdentity(t *testing.T) {
	w := newMutationRetryTestWatcher(t)
	// Keep the existing debounce asleep; complete the exact coalesced frontier
	// explicitly, so the oracle needs neither sleeps nor timer-order luck.
	w.config.DebounceMs = int(time.Hour / time.Millisecond)
	recorder := NewPublicationPhaseRecorder(2, 1)
	older := recorder.Begin("", "older", PublicationSourceMCPEdit, time.Now())
	newer := recorder.Begin("", "newer", PublicationSourceMCPEdit, time.Now())
	first := w.scheduleFileMutationWithRecord("edit.go", ChangeModified, older)
	second := w.scheduleFileMutationWithRecord("edit.go", ChangeModified, newer)
	// Reusing a context record must not retarget the old timeline, even though
	// the actual mutation is still admitted and supersedes both earlier ones.
	third := w.scheduleFileMutationWithRecord("edit.go", ChangeModified, older)
	require.Same(t, older, first.PublicationRecord)
	require.Same(t, newer, second.PublicationRecord)
	require.Nil(t, third.PublicationRecord)
	require.Equal(t, first.Generation, older.Snapshot().Ticket)
	require.Equal(t, second.Generation, newer.Snapshot().Ticket)
	require.True(t, w.completeMutationWaitersIfCurrent("edit.go", third.Generation, nil))
	for _, ticket := range []*MutationTicket{first, second, third} {
		result := awaitMutationRetryResult(t, ticket)
		require.Equal(t, ticket.Generation, result.RequestedGeneration)
		require.Equal(t, third.Generation, result.AppliedGeneration)
		require.NoError(t, result.Err)
	}
	require.Equal(t, first.Generation, older.Snapshot().Ticket)
	require.Equal(t, second.Generation, newer.Snapshot().Ticket)
}

func TestCanonicalMutationRejectsTerminalTimingAssociation(t *testing.T) {
	w := newMutationRetryTestWatcher(t)
	w.config.DebounceMs = int(time.Hour / time.Millisecond)
	record := NewPublicationPhaseRecorder(1, 1).Begin("", "settled", PublicationSourceMCPEdit, time.Now())
	record.Mark(PublicationTicketFailed)
	ticket := w.scheduleFileMutationWithRecord("edit.go", ChangeModified, record)
	require.NotNil(t, ticket)
	require.Nil(t, ticket.PublicationRecord)
	require.Zero(t, record.Snapshot().Ticket)
	require.True(t, w.completeMutationWaitersIfCurrent("edit.go", ticket.Generation, nil))
	require.NoError(t, awaitMutationRetryResult(t, ticket).Err)
}

func TestCanonicalMutationCannotReuseRecordAcrossEqualSequences(t *testing.T) {
	firstWatcher, secondWatcher := newMutationRetryTestWatcher(t), newMutationRetryTestWatcher(t)
	firstWatcher.config.DebounceMs = int(time.Hour / time.Millisecond)
	secondWatcher.config.DebounceMs = int(time.Hour / time.Millisecond)
	record := NewPublicationPhaseRecorder(1, 1).Begin("", "single-use", PublicationSourceMCPEdit, time.Now())
	first := firstWatcher.scheduleFileMutationWithRecord("first.go", ChangeModified, record)
	second := secondWatcher.scheduleFileMutationWithRecord("second.go", ChangeModified, record)
	require.Equal(t, first.Generation, second.Generation, "sequences are local to their watcher")
	require.Same(t, record, first.PublicationRecord)
	require.Nil(t, second.PublicationRecord, "equal sequence numbers do not establish the same physical admission")
	require.True(t, firstWatcher.completeMutationWaitersIfCurrent("first.go", first.Generation, nil))
	require.True(t, secondWatcher.completeMutationWaitersIfCurrent("second.go", second.Generation, nil))
	require.NoError(t, awaitMutationRetryResult(t, first).Err)
	require.NoError(t, awaitMutationRetryResult(t, second).Err)
}

func TestMutationWaiterReleasesPublicationWithExistingLifetime(t *testing.T) {
	w := newMutationRetryTestWatcher(t)
	w.config.DebounceMs = int(time.Hour / time.Millisecond)
	recorder := NewPublicationPhaseRecorder(2, 1)
	record := recorder.Begin("", "outstanding", PublicationSourceMCPEdit, time.Now())
	ticket := w.scheduleFileMutationWithRecord("edit.go", ChangeModified, record)
	w.mu.Lock()
	retained := w.mutationWaiters["edit.go"][ticket.Generation].publication
	w.mu.Unlock()
	require.Same(t, record, retained)
	require.True(t, w.completeMutationWaitersIfCurrent("edit.go", ticket.Generation, nil))
	require.NoError(t, awaitMutationRetryResult(t, ticket).Err)
	w.mu.Lock()
	remaining := len(w.mutationWaiters)
	w.mu.Unlock()
	require.Zero(t, remaining, "completion has no separate publication history to prune")

	stopped := recorder.Begin("", "stopped", PublicationSourceMCPEdit, time.Now())
	stoppedTicket := w.scheduleFileMutationWithRecord("stopped.go", ChangeModified, stopped)
	require.NoError(t, w.Stop())
	require.ErrorIs(t, awaitMutationRetryResult(t, stoppedTicket).Err, errWatcherStopped)
	w.mu.Lock()
	remaining = len(w.mutationWaiters)
	w.mu.Unlock()
	require.Zero(t, remaining, "stop releases the same pending ownership")
}
