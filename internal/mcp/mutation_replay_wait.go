package mcp

import (
	"context"
	"fmt"
	"time"
)

// closeResponseReadyLocked requires the record's write lock. Nil after close
// makes retained, terminal, and eviction paths share one completion signal.
func (r *mutationCommitRecord) closeResponseReadyLocked() {
	if r.responseReady != nil {
		close(r.responseReady)
		r.responseReady = nil
	}
}

// The handler may stop after disk commit without retaining a reply (including
// panic recovery). A retry must learn that terminal unavailability too.
func (r *mutationCommitRecord) finishResponseWait() {
	if !r.isPhysicalCommit() {
		return
	}
	r.mu.Lock()
	r.closeResponseReadyLocked()
	r.mu.Unlock()
}

// A matching physical commit is already final, but its original full-policy
// reply may still be waiting for publication. Release the source path before
// waiting; cancellation stops only this caller, never the original request.
func (s *Server) replayMutationAfterPathRelease(ctx context.Context, key, fingerprint string, releasePath func()) (map[string]any, string, bool) {
	if key == "" {
		return nil, "", false
	}
	record, found := s.mutationCommits.byMutationID(key)
	if !found {
		return nil, "", false
	}
	record.mu.RLock()
	matchingCommitted := record.owner == s && record.key == key && record.fingerprint == fingerprint && record.disk == mutationDiskCommitted
	ready := record.responseReady
	pending := record.response == nil
	record.mu.RUnlock()
	if !matchingCommitted {
		return s.replayMutation(key, fingerprint)
	}
	unavailable := func(reason string) (map[string]any, string, bool) {
		return nil, fmt.Sprintf("mutation_id %q already committed; its original response is unavailable (%s). Do not reapply: query mutation_status for receipt %s", key, reason, record.id), true
	}
	if pending && ready != nil {
		releasePath()
		if err := ctx.Err(); err != nil {
			return unavailable(err.Error())
		}
		select {
		case <-ctx.Done():
			return unavailable(ctx.Err().Error())
		case <-ready:
		}
		if err := ctx.Err(); err != nil {
			return unavailable(err.Error())
		}
	}
	// Lock order stays ledger -> record. Copy the immutable retained result
	// while the same exact owner is still registered; render live facts later.
	s.mutationCommits.mu.Lock()
	s.mutationCommits.evictLocked(time.Now())
	if s.mutationCommits.byKey[key] != record || s.mutationCommits.byID[record.id] != record {
		s.mutationCommits.mu.Unlock()
		return unavailable("receipt evicted or replaced")
	}
	record.mu.RLock()
	if record.owner != s || record.key != key || record.fingerprint != fingerprint || record.disk != mutationDiskCommitted || record.response == nil {
		record.mu.RUnlock()
		s.mutationCommits.mu.Unlock()
		return unavailable("original handler finished without a retained response")
	}
	response := make(map[string]any, len(record.response)+1)
	for field, value := range record.response {
		response[field] = value
	}
	record.mu.RUnlock()
	s.mutationCommits.mu.Unlock()
	return s.renderMutationReplay(record, response)
}
