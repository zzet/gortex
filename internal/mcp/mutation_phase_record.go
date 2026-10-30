package mcp

import (
	"context"
	"time"

	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/pathkey"
)

// committedPublicationRecord opens one timeline for an actual single-file
// commit, after disk success but before refresh admission. The commit note may
// contain several writes; choose the latest committed version of this path,
// never the first request-wide disk stamp or an earlier file's record.
// Atomic batch transactions have a separate durable lifecycle and do not enter
// this helper. No filesystem/store observation is added here.
func (s *Server) committedPublicationRecord(ctx context.Context, path string) *indexer.PublicationPhaseRecord {
	note := mutationCommitNoteFrom(ctx)
	if note == nil {
		return nil
	}
	note.mu.Lock()
	records := append([]*mutationCommitRecord(nil), note.records...)
	note.mu.Unlock()
	var selected *mutationCommitRecord
	var sequence uint64
	for _, record := range records {
		record.mu.RLock()
		match := record.disk == mutationDiskCommitted && record.commitSeq > sequence && pathkey.EqualPaths(record.absPath, path)
		if match {
			selected, sequence = record, record.commitSeq
		}
		record.mu.RUnlock()
	}
	if selected == nil {
		return nil
	}
	return s.openCommittedPublicationRecord(ctx, selected)
}

func (s *Server) openCommittedPublicationRecord(ctx context.Context, record *mutationCommitRecord) *indexer.PublicationPhaseRecord {
	if !record.isPhysicalCommit() {
		return nil
	}
	return s.openOwnedPublicationRecord(ctx,record)
}

// The batch's distinct observation kind can share the bounded recorder without
// gaining physical mutation authority or entering committedPublicationRecord.
func (s *Server) openOwnedPublicationRecord(ctx context.Context, record *mutationCommitRecord) *indexer.PublicationPhaseRecord {
	if !record.isPublicationOwner() {return nil}
	ledger := &s.mutationCommits
	ledger.mu.Lock()
	record.mu.RLock()
	owned := ledger.byID[record.id] == record
	existing := record.publication
	started, committed, checkoutID, key := record.diskWriteStartedAt, record.committedAt, record.checkoutID, record.id
	committedVerdict:=record.kind!=batchFileObservationCommit || record.disk==mutationDiskCommitted
	record.mu.RUnlock()
	ledger.mu.Unlock()
	if !owned || !committedVerdict || started.IsZero() || committed.IsZero() {
		return nil
	}
	if existing != nil {
		return existing
	}
	origin := toolReceivedAt(ctx)
	if origin.IsZero() {
		origin = started
	}
	publication := indexer.DefaultPublicationPhases().Begin(checkoutID, key, indexer.PublicationSourceMCPEdit, origin)
	// Stamp this physical file's boundaries before absorbing the shared
	// request timeline. Its first file's disk marks must not win for this one.
	publication.MarkAt(indexer.PublicationDiskWriteStarted, started)
	publication.MarkAt(indexer.PublicationReceiptCommitted, committed)
	publication.Absorb(indexer.PublicationStampsFrom(ctx))

	// Pending-source evidence can retain a record after ledger eviction. Do
	// not resurrect its timing pointer: completed timing retention is bounded
	// by the existing ledger/recorder, not that evidence's longer lifetime.
	ledger.mu.Lock()
	record.mu.Lock()
	if ledger.byID[record.id] == record && record.publication == nil {
		record.publication = publication
	}
	selected := record.publication
	record.mu.Unlock()
	ledger.mu.Unlock()
	return selected
}

func completeCanonicalPublication(record *indexer.PublicationPhaseRecord, result indexer.MutationResult) {
	if record == nil {
		return
	}
	// A coalesced newer application can complete the older requested ticket.
	// Keep its original binding; canonical AppliedGeneration is a watcher
	// sequence, not a checkout dirty-generation ID or a new per-edit patch span.
	if result.Err == nil && result.Reindexed {
		record.Mark(indexer.PublicationTicketCompleted)
	} else {
		record.Mark(indexer.PublicationTicketFailed)
	}
}

// retainedMutationPublication is status-only. Holding the pointer in one reply
// does not extend its registry lifetime or mutate the record's binding.
func retainedMutationPublication(record *mutationCommitRecord) (*indexer.PublicationPhaseRecord, time.Time) {
	if !record.isPhysicalCommit() {
		return nil, time.Time{}
	}
	record.mu.RLock()
	defer record.mu.RUnlock()
	return record.publication, record.committedAt
}
