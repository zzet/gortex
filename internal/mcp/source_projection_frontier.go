package mcp

import (
	"context"
	"time"
	"weak"
)

// This caller-owned slice dies after the held source frontier finishes. The
// bounded physical ledger never strongly retains a graph or a prepared delta.
type committedFileProjection struct {
	record   *mutationCommitRecord
	prepared *preparedFileProjection
}

func (s *Server) publishCommittedProjectionCohort(ctx context.Context, writes []committedFileProjection) {
	// A cancellation after a successful physical write cannot discard that
	// write's file facts. This bounded verification does no source mutation.
	checked, cancel := context.WithTimeout(context.WithoutCancel(ctx), 150*time.Millisecond)
	defer cancel()
	members := make([]projectionCohortMember, 0, len(writes))
	owners := make([]*mutationCommitRecord, 0, len(writes))
	for _, write := range writes {
		record, prepared := write.record, write.prepared
		reason := "projection_not_prepared"
		if prepared != nil && prepared.refusal != "" {
			reason = prepared.refusal
		}
		if record == nil {
			continue
		}
		record.mu.RLock()
		sequence, committed, sha := record.commitSeq, record.disk == mutationDiskCommitted, record.contentSHA256
		record.mu.RUnlock()
		if !committed {
			continue
		}
		if prepared != nil && prepared.entry != nil {
			entry, err := s.verifyPreparedProjection(checked, prepared, sequence, sha)
			if err == nil {
				holdProjectionForAcknowledgement(projectionAcknowledgementFrom(ctx, s), pendingFileProjections, entry)
				members = append(members, projectionCohortMember{Entry: entry, Expected: prepared.expected})
				owners = append(owners, record)
				continue
			}
			reason = "readback_unavailable"
		}
		record.mu.Lock()
		record.fileReason = reason
		record.readbackExact = false
		record.mu.Unlock()
	}
	err := pendingFileProjections.publishCohort(members)
	for i, record := range owners {
		entry := members[i].Entry
		record.mu.Lock()
		if err != nil {
			record.fileReason = "projection_refused"
			if refusal, ok := err.(projectionRefusal); ok {
				record.fileReason = string(refusal)
			}
			record.readbackExact = false
		} else {
			record.fileProjection = weak.Make(entry)
			record.readbackExact = true
			record.fileReason = ""
			if !entry.tombstone && !entry.parseChecked {
				record.fileReason = "syntax_unknown"
			}
			if !entry.tombstone && entry.parseChecked && entry.parseErrors > 0 {
				record.fileReason = "parse_errors"
			}
		}
		record.mu.Unlock()
	}
}
