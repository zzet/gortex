package mcp

import (
	"github.com/zzet/gortex/internal/indexer"
)

func (s *Server) bindSharedPhysicalOwner(record *mutationCommitRecord, publication *indexer.PublicationPhaseRecord) {
	if publication == nil || record == nil || !record.isPhysicalCommit() || !s.sharedIdentity.constructorValid(s) {
		return
	}
	s.mutationCommits.mu.Lock()
	defer s.mutationCommits.mu.Unlock()
	record.mu.RLock()
	defer record.mu.RUnlock()
	if record.owner != s || s.mutationCommits.byID[record.id] != record || record.publication != publication || record.disk != mutationDiskCommitted || record.commitSeq == 0 {
		return
	}
	publication.BindSharedPhysicalOwner(indexer.SharedPhysicalOwner{Server: s.sharedIdentity.name, PID: s.sharedIdentity.pid, Birth: s.sharedIdentity.birth, Receipt: record.id, Sequence: record.commitSeq, Hash: record.contentSHA256, Path: record.absPath, Checkout: record.checkoutID, Incarnation: record.checkoutIncarnation})
}

func (s *Server) attachSharedOwnerAccounting(payload map[string]any, record *mutationCommitRecord, publication *indexer.PublicationPhaseRecord) {
	if publication == nil || record == nil || !record.isPhysicalCommit() {
		return
	}
	state, reason := publication.SharedOwnerProofAvailability()
	if state == "" {
		return
	}
	if !s.validSharedIdentity() {
		return
	}
	if state != "" {
		var why any
		if reason != "" {
			why = reason
		}
		payload["shared_owner_accounting_v1_availability"] = map[string]any{"state": state, "reason": why}
	}
	preliminary := publication.SharedOwnerProof(indexer.DefaultPublicationPhases())
	if preliminary != nil {
		for _, member := range preliminary.PhysicalMembers {
			s.mutationCommits.mu.Lock()
			peer := s.mutationCommits.byID[member.Receipt]
			s.mutationCommits.mu.Unlock()
			if peer != nil && peer.owner == s {
				s.refreshPendingSourceRecord(peer)
			}
		}
	}
	// Keep the actual current ledger fixed while checking ALL original members.
	s.mutationCommits.mu.Lock()
	defer s.mutationCommits.mu.Unlock()
	if s.mutationCommits.byID[record.id] != record || record.owner != s {
		return
	}
	accepted := publication.WithSharedOwnerProof(indexer.DefaultPublicationPhases(), func(proof *indexer.SharedOwnerProjection) bool {
		peers := make([]*mutationCommitRecord, 0, len(proof.PhysicalMembers))
		for _, member := range proof.PhysicalMembers {
			current := s.mutationCommits.byID[member.Receipt]
			if current == nil || current.owner != s {
				return false
			}
			peers = append(peers, current)
		}
		// All locks use the frozen sorted member order. No peer can rebind while
		// the same-status ledger + recorder + complete peer witness is projected.
		for _, peer := range peers {
			peer.mu.RLock()
		}
		defer func() {
			for i := len(peers) - 1; i >= 0; i-- {
				peers[i].mu.RUnlock()
			}
		}()
		for i, member := range proof.PhysicalMembers {
			current := peers[i]
			if !current.isPhysicalCommit() || current.id != member.Receipt || current.commitSeq != member.Sequence || current.contentSHA256 != member.Hash || current.absPath != member.Path || current.checkoutID != member.Checkout || current.checkoutIncarnation != member.Incarnation || current.disk != mutationDiskCommitted || member.Server != s.sharedIdentity.name || member.PID != s.sharedIdentity.pid || member.Birth != s.sharedIdentity.birth || !current.graphRecorded || current.graph != mutationGraphFresh || !proof.MatchesPublication(member.Receipt, current.publication, current.appliedGeneration) {
				return false
			}
		}
		payload["shared_owner_accounting_v1"] = map[string]any{
			"reference":  map[string]any{"owner": proof.Owner, "member": proof.Member},
			"definition": proof.Definition,
			"membership": map[string]any{"owner": proof.Owner, "member": proof.Member, "applied_generation": proof.Ticket, "same_status_projection": true, "live_server_ledger_member": true, "live_recorder_member": true, "live_anchor": true, "full_membership_retained": true, "original_invocation_verified": true, "publication_terminal_success": true},
		}
		return true
	})
	if accepted {
		payload["shared_owner_accounting_v1_availability"] = map[string]any{"state": "frozen", "reason": nil}
	} else if state == "frozen" {
		payload["shared_owner_accounting_v1_availability"] = map[string]any{"state": "unavailable", "reason": "membership_changed"}
	}
}
