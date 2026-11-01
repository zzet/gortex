package mcp

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/indexer"
)

// The indexer tests exercise the real bind-before-wake critical section. This
// adapter deterministically makes completion available before admission returns,
// exercising the MCP side without depending on a debounce scheduling race.
type canonicalPublicationWatcher struct {
	mutationTestWatcher
	hold      bool
	records   []*indexer.PublicationPhaseRecord
	snapshots []indexer.PublicationPhaseSnapshot
	results   []chan indexer.MutationResult
}

func (w *canonicalPublicationWatcher) EnqueueFileMutation(ctx context.Context, path string) (*indexer.MutationTicket, error) {
	if w.scheduleErr != nil {
		return nil, w.scheduleErr
	}
	generation := uint64(len(w.records) + 1)
	record := indexer.PublicationRecordFromContext(ctx)
	if record != nil {
		record.BindTicket(generation)
		record.Mark(indexer.PublicationTicketEnqueued)
		w.snapshots = append(w.snapshots, record.Snapshot())
	}
	done := make(chan indexer.MutationResult, 1)
	w.records = append(w.records, record)
	w.results = append(w.results, done)
	if !w.hold {
		done <- indexer.MutationResult{RequestedGeneration: generation, AppliedGeneration: generation, Reindexed: true}
		close(done)
	}
	return &indexer.MutationTicket{Path: path, Generation: generation, Done: done, PublicationRecord: record}, nil
}

func canonicalTimingContext() context.Context {
	ctx := withToolReceivedAt(context.Background(), time.Now())
	ctx, _ = withMutationCommitNote(ctx)
	return indexer.WithPublicationStamps(ctx)
}

func commitTimingFixture(t *testing.T, s *Server, ctx context.Context, path, content string) *mutationCommitRecord {
	t.Helper()
	record, err := s.commitFileMutation(ctx, "write_file", "", "", filepath.Base(path), path, []byte(content), 0o600)
	require.NoError(t, err)
	return record
}

func TestCanonicalCommitImmediateSuccessKeepsPublication(t *testing.T) {
	s := newMutationServer()
	w := &canonicalPublicationWatcher{}
	s.watcher = w
	ctx := canonicalTimingContext()
	path := filepath.Join(t.TempDir(), "edit.go")
	record := commitTimingFixture(t, s, ctx, path, "package edited\n")
	outcome := s.mutationReindexState(ctx, path)
	require.True(t, outcome.Reindexed)
	require.False(t, outcome.Pending)
	require.Empty(t, outcome.Receipt, "keep the existing immediate-success public contract")
	record.recordGraph(outcome)
	require.Len(t, w.snapshots, 1)
	require.EqualValues(t, 1, w.snapshots[0].Ticket)
	require.Contains(t, publicationOffsets(w.snapshots[0]), indexer.PublicationReceiptCommitted)
	require.Contains(t, publicationOffsets(w.snapshots[0]), indexer.PublicationTicketEnqueued)
	status := s.mutationStatusPayload(record)
	publication := status["publication_phases"].(indexer.PublicationPhaseSnapshot)
	require.True(t, publication.Terminal)
	require.EqualValues(t, 1, publication.Ticket)
	require.Zero(t, publication.DirtyGenerationID, "a watcher sequence is not a checkout view ID")
	require.Equal(t, "measured", status["phase_states"].(map[string]string)["disk_write"])
	require.Equal(t, "measured", status["phase_states"].(map[string]string)["publish"])
	require.Empty(t, record.snapshot().ReindexReceipt)
}

func TestCanonicalCommitsKeepPerFileAndVersionTiming(t *testing.T) {
	s := newMutationServer()
	w := &canonicalPublicationWatcher{}
	s.watcher = w
	ctx := canonicalTimingContext()
	root := t.TempDir()
	firstPath, secondPath := filepath.Join(root, "first.go"), filepath.Join(root, "second.go")
	first := commitTimingFixture(t, s, ctx, firstPath, "package first\n")
	first.recordGraph(s.mutationReindexState(ctx, firstPath))
	before := w.records[0].Snapshot()
	second := commitTimingFixture(t, s, ctx, secondPath, "package second\n")
	second.recordGraph(s.mutationReindexState(ctx, secondPath))
	newer := commitTimingFixture(t, s, ctx, firstPath, "package newer\n")
	newer.recordGraph(s.mutationReindexState(ctx, firstPath))
	require.Len(t, w.records, 3)
	require.NotSame(t, w.records[0], w.records[1])
	require.NotSame(t, w.records[0], w.records[2])
	require.Equal(t, before, w.records[0].Snapshot(), "the newer file/version must not extend or retarget the first receipt")
	for i, commit := range []*mutationCommitRecord{first, second, newer} {
		publication := w.records[i].Snapshot()
		commit.mu.RLock()
		started := commit.diskWriteStartedAt
		commit.mu.RUnlock()
		require.Equal(t, started.Sub(toolReceivedAt(ctx)).Nanoseconds(), publicationOffsets(publication)[indexer.PublicationDiskWriteStarted],
			"a request's first disk stamp must not be reused for another physical write")
		require.EqualValues(t, i+1, publication.Ticket)
	}
}

func TestCanonicalCoalescedCompletionKeepsOriginalReceiptIdentity(t *testing.T) {
	s := newMutationServer()
	s.mutationReindexWait = time.Nanosecond
	w := &canonicalPublicationWatcher{hold: true}
	s.watcher = w
	ctx := canonicalTimingContext()
	path := filepath.Join(t.TempDir(), "edit.go")
	for _, source := range []string{"package older\n", "package newer\n"} {
		record := commitTimingFixture(t, s, ctx, path, source)
		outcome := s.mutationReindexState(ctx, path)
		require.True(t, outcome.Pending)
		record.recordGraph(outcome)
	}
	for i, done := range w.results {
		done <- indexer.MutationResult{RequestedGeneration: uint64(i + 1), AppliedGeneration: 2, Reindexed: true}
		close(done)
		id := w.records[i].Key()
		value, found := s.mutationReceipts.Load(id)
		require.True(t, found)
		select {
		case <-value.(*mutationReceipt).done:
		case <-time.After(5 * time.Second):
			t.Fatal("the existing receipt consumer did not finish")
		}
		publication := w.records[i].Snapshot()
		require.True(t, publication.Terminal)
		require.EqualValues(t, i+1, publication.Ticket)
		require.Zero(t, publication.DirtyGenerationID)
		require.NotContains(t, publicationOffsets(publication), indexer.PublicationCycleStarted, "do not invent a per-receipt patch interval from a shared completion")
	}
}

func TestCommittedPublicationRetentionEndsWithLedgerOwnership(t *testing.T) {
	for _, eviction := range []string{"expiry", "capacity"} {
		t.Run(eviction, func(t *testing.T) {
			s := newMutationServer()
			ctx := canonicalTimingContext()
			path := filepath.Join(t.TempDir(), "edit.go")
			record := commitTimingFixture(t, s, ctx, path, "package retained\n")
			publication := s.committedPublicationRecord(ctx, path)
			require.NotNil(t, publication)
			if eviction == "expiry" {
				record.mu.Lock()
				record.startedAt = time.Now().Add(-mutationCommitRetention - time.Second)
				record.mu.Unlock()
				s.mutationCommits.mu.Lock()
				s.mutationCommits.evictLocked(time.Now())
				s.mutationCommits.mu.Unlock()
			} else {
				for i := range maxMutationCommits {
					s.mutationCommits.put(&mutationCommitRecord{id: fmt.Sprintf("timing-cap-%d", i), startedAt: time.Now()})
				}
			}
			_, present := s.mutationCommits.byReceipt(record.id)
			require.False(t, present)
			retained, _ := retainedMutationPublication(record)
			require.Nil(t, retained, "source evidence or the commit note may still hold this object, but not its timing history")
			require.Nil(t, s.committedPublicationRecord(ctx, path), "a late callback must not resurrect evicted timing retention")
			require.Equal(t, mutationDiskCommitted, record.snapshot().DiskStatus, "timing eviction must not change the disk verdict")
		})
	}
}

func TestReboundCommitDoesNotBorrowReplacementPublication(t *testing.T) {
	s := newMutationServer()
	ctx := canonicalTimingContext()
	path := filepath.Join(t.TempDir(), "edit.go")
	record := commitTimingFixture(t, s, ctx, path, "package edited\n")
	original := s.committedPublicationRecord(ctx, path)
	require.NotNil(t, original)
	newID := "replacement-" + record.id
	replacement := indexer.DefaultPublicationPhases().Begin("", newID, indexer.PublicationSourceMCPEdit, time.Now())
	replacement.Mark(indexer.PublicationTicketEnqueued)
	s.mutationReceipts.Store(newID, &mutationReceipt{id: newID, generation: 2, done: make(chan struct{})})
	record.recordGraph(mutationReindexOutcome{Pending: true, Receipt: newID, Generation: 2})
	original.Mark(indexer.PublicationTicketCompleted)
	status := s.mutationStatusPayload(record)
	require.Equal(t, mutationGraphPending, status["graph_status"])
	require.NotContains(t, status, "publication_phases", "do not substitute another receipt's timeline for this physical commit")
	for name, value := range status["phases_ms"].(map[string]*float64) {
		require.Nil(t, value, name)
	}
	retained, _ := retainedMutationPublication(record)
	require.Same(t, original, retained, "the historical record itself is never retargeted")
}

func TestCanonicalAdmissionFailureClosesItsCommittedRecord(t *testing.T) {
	s := newMutationServer()
	sentinel := errors.New("admission stopped")
	s.watcher = &canonicalPublicationWatcher{mutationTestWatcher: mutationTestWatcher{scheduleErr: sentinel}}
	ctx := canonicalTimingContext()
	path := filepath.Join(t.TempDir(), "edit.go")
	record := commitTimingFixture(t, s, ctx, path, "package committed\n")
	outcome := s.mutationReindexState(ctx, path)
	require.ErrorIs(t, outcome.Err, sentinel)
	record.recordGraph(outcome)
	status := s.mutationStatusPayload(record)
	require.Equal(t, mutationDiskCommitted, status["disk_status"])
	require.Equal(t, mutationGraphFailed, status["graph_status"])
	publication := status["publication_phases"].(indexer.PublicationPhaseSnapshot)
	require.True(t, publication.Terminal)
	require.Zero(t, publication.Ticket, "no ticket was admitted")
	require.Contains(t, publicationOffsets(publication), indexer.PublicationTicketFailed)
	require.Nil(t, status["phases_ms"].(map[string]*float64)["publish"])
}

func TestCanonicalTimingSurvivesBoundedRecorderHistoryEviction(t *testing.T) {
	s := newMutationServer()
	s.watcher = &canonicalPublicationWatcher{}
	ctx := canonicalTimingContext()
	path := filepath.Join(t.TempDir(), "edit.go")
	record := commitTimingFixture(t, s, ctx, path, "package edited\n")
	record.recordGraph(s.mutationReindexState(ctx, path))
	publication, _ := retainedMutationPublication(record)
	require.NotNil(t, publication)
	for i := range 40 {
		indexer.DefaultPublicationPhases().Begin("", fmt.Sprintf("recorder-cap-%s-%d", record.id, i), indexer.PublicationSourceMCPEdit, time.Now())
	}
	_, found := indexer.DefaultPublicationPhases().Lookup(publication.Key())
	require.False(t, found, "the recorder's independent count bound should have evicted the original entry")
	status := s.mutationStatusPayload(record)
	require.Equal(t, publication.Key(), status["publication_phases"].(indexer.PublicationPhaseSnapshot).Key)
	require.Equal(t, "measured", status["phase_states"].(map[string]string)["disk_write"])
}
