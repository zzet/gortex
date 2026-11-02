package mcp

import (

 "github.com/zzet/gortex/internal/indexer"
)

func (s *Server) bindSharedPhysicalOwner(record *mutationCommitRecord, publication *indexer.PublicationPhaseRecord) {
 if publication==nil || record==nil || !record.isPhysicalCommit() || !s.sharedIdentity.constructorValid(s) { return }
 s.mutationCommits.mu.Lock();defer s.mutationCommits.mu.Unlock()
 record.mu.RLock();defer record.mu.RUnlock()
 if record.owner!=s || s.mutationCommits.byID[record.id]!=record || record.publication!=publication || record.disk!=mutationDiskCommitted || record.commitSeq==0 { return }
 publication.BindSharedPhysicalOwner(indexer.SharedPhysicalOwner{Server:s.sharedIdentity.name,PID:s.sharedIdentity.pid,Birth:s.sharedIdentity.birth,Receipt:record.id,Sequence:record.commitSeq,Hash:record.contentSHA256,Path:record.absPath,Checkout:record.checkoutID,Incarnation:record.checkoutIncarnation})
}

func (s *Server) attachSharedOwnerAccounting(payload map[string]any, record *mutationCommitRecord, publication *indexer.PublicationPhaseRecord) {
 if publication==nil || record==nil || !record.isPhysicalCommit() || !s.validSharedIdentity() { return }
 // Keep the actual current ledger fixed while checking ALL original members.
 s.mutationCommits.mu.Lock();defer s.mutationCommits.mu.Unlock()
 if s.mutationCommits.byID[record.id]!=record || record.owner!=s { return }
 proof:=publication.SharedOwnerProof(indexer.DefaultPublicationPhases())
 if proof==nil { return }
 for _,member:=range proof.PhysicalMembers {
  current:=s.mutationCommits.byID[member.Receipt]
  if current==nil || current.owner!=s { return }
  current.mu.RLock()
  valid:=current.isPhysicalCommit() && current.id==member.Receipt && current.commitSeq==member.Sequence && current.contentSHA256==member.Hash && current.absPath==member.Path && current.checkoutID==member.Checkout && current.checkoutIncarnation==member.Incarnation && current.disk==mutationDiskCommitted && member.Server==s.sharedIdentity.name && member.PID==s.sharedIdentity.pid && member.Birth==s.sharedIdentity.birth
  retained:=current.publication
  current.mu.RUnlock()
  if !valid || retained==nil { return }
 }
 snap:=record.snapshot()
 if snap.AppliedGeneration!=proof.Ticket || snap.GraphStatus!=mutationGraphFresh { return }
 payload["shared_owner_accounting_v1"]=map[string]any{
  "reference":map[string]any{"owner":proof.Owner,"member":proof.Member},
  "definition":proof.Definition,
  "membership":map[string]any{"owner":proof.Owner,"member":proof.Member,"applied_generation":proof.Ticket,"same_status_projection":true,"live_server_ledger_member":true,"live_recorder_member":true,"live_anchor":true,"full_membership_retained":true,"original_invocation_verified":true,"publication_terminal_success":true},
 }
 payload["shared_owner_accounting_v1_availability"]=map[string]any{"state":"frozen","reason":nil}
}

