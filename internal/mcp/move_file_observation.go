package mcp

import (
	"context"
	"strconv"
	"strings"
	"time"
	"weak"

	"github.com/zzet/gortex/internal/daemon"
	"github.com/zzet/gortex/internal/indexer"
)

// These records describe the existing move's two real writes and direct
// reconciles. They are neither a durable transaction nor physical mutation
// receipts, and never participate in replay/pending-source/freshness indexes.
// The first measured fixture is a same-package, two-file Go relocation.
type moveFileObservation struct {
	operationID, sourceID, targetID, role string
	writeVerdict, directState             string
	directClaimed                         bool
	readClaimed                           bool
	readState                             string
	readAssisted                          *moveDirectObservation
	direct                                *moveDirectObservation
}
type moveDirectObservation struct {
	generation           uint64
	windowNS             int64
	writerToAcceptanceNS int64
	acceptedBlob         string
	causes               [8]mutationReadRepairCause
	causeCount           int
	accounting           indexer.ReadRepairPhaseAccounting
}
type moveFileObservationReference struct {
	Path    string `json:"path"`
	Role    string `json:"role"`
	Receipt string `json:"receipt"`
}
type moveObservation struct {
	server  *Server
	id      string
	records map[string]*mutationCommitRecord
	refs    []moveFileObservationReference
}

func (s *Server) beginMoveObservation(ctx context.Context, sourceID, targetID, sourceAbs, targetAbs, sourceRel, targetRel string, twoFileSamePackage bool) *moveObservation {
	ack := editAcknowledgementFrom(ctx, s)
	if ack == nil || !twoFileSamePackage {
		return nil
	}
	checkout, incarnation := mutationCheckoutScope(ctx)
	out := &moveObservation{server: s, id: "move-" + strconv.FormatUint(ack.id, 10), records: make(map[string]*mutationCommitRecord, 2)}
	for _, file := range []struct{ abs, rel, role string }{{sourceAbs, sourceRel, "source"}, {targetAbs, targetRel, "target"}} {
		record := &mutationCommitRecord{kind: moveFileObservationCommit, owner: s, id: "move-file-" + strconv.FormatUint(mutationCommitSequence.Add(1), 10), tool: "move_symbol", key: out.id, absPath: file.abs, relPath: file.rel, startedAt: ack.entered, acknowledgement: ack, disk: mutationDiskInFlight, checkoutID: checkout, checkoutIncarnation: incarnation, moveFile: &moveFileObservation{operationID: out.id, sourceID: sourceID, targetID: targetID, role: file.role, writeVerdict: "pending", directState: "not_started", readState: "not_started"}}
		s.mutationCommits.put(record)
		out.records[file.abs] = record
		out.refs = append(out.refs, moveFileObservationReference{Path: file.rel, Role: file.role, Receipt: record.id})
	}
	return out
}

func (o *moveObservation) write(ack *editAcknowledgement, path string, content []byte) error {
	if o == nil {
		return writeMoveFileObserved(ack, path, content)
	}
	started := time.Now()
	err := writeMoveFileObserved(ack, path, content)
	returned := time.Now()
	record := o.records[path]
	if record == nil {
		return err
	}
	blob := ""
	if err == nil {
		blob = gitBlobSHA(content)
	} // no existing fingerprint is produced by the move writer
	ledger := &o.server.mutationCommits
	ledger.mu.Lock()
	defer ledger.mu.Unlock()
	record.mu.Lock()
	defer record.mu.Unlock()
	if ledger.byID[record.id] != record || record.owner != o.server || record.kind != moveFileObservationCommit || record.moveFile == nil || record.key != o.id {
		return err
	}
	if !record.diskWriteStartedAt.IsZero() {
		record.disk = mutationDiskFailed
		record.errText = "unavailable_repeated_writer"
		return err
	}
	record.diskWriteStartedAt, record.committedAt = started, returned
	if err != nil {
		record.disk = mutationDiskFailed
		record.errText = err.Error()
		return err
	}
	record.disk, record.newSHA, record.bytesWritten = mutationDiskCommitted, blob, len(content)
	record.commitSeq = mutationDiskCommitSequence.Add(1)
	return err // observation never changes the real writer outcome
}

func (o *moveObservation) finishWrites(success bool) {
	if o == nil {
		return
	}
	ledger := &o.server.mutationCommits
	ledger.mu.Lock()
	defer ledger.mu.Unlock()
	for _, record := range o.records {
		record.mu.Lock()
		if ledger.byID[record.id] == record && record.moveFile != nil && record.key == o.id && record.moveFile.writeVerdict == "pending" {
			record.moveFile.writeVerdict = "partial_or_failed"
			if success {
				record.moveFile.writeVerdict = "all_writes_completed"
			}
		}
		record.mu.Unlock()
	}
}

func (o *moveObservation) references() []moveFileObservationReference {
	if o == nil {
		return nil
	}
	out := make([]moveFileObservationReference, len(o.refs))
	copy(out, o.refs)
	return out
}

type moveDirectWitness struct {
	server                                         weak.Pointer[Server]
	record                                         weak.Pointer[mutationCommitRecord]
	indexer                                        weak.Pointer[indexer.Indexer]
	id, operation, path, blob, session, requestKey string
	committed                                      time.Time
	commitSeq                                      uint64
	readAssisted                                   bool
}

func (o *moveObservation) observer(ctx context.Context, path string, idx *indexer.Indexer) *indexer.ReadRepairObserver {
	if o == nil || idx == nil {
		return nil
	}
	record := o.records[path]
	if record == nil {
		return nil
	}
	key, _ := daemon.MCPRequestIDKeyForContext(ctx)
	witness := moveDirectWitness{server: weak.Make(o.server), record: weak.Make(record), indexer: weak.Make(idx), session: SessionIDFromContext(ctx), requestKey: key}
	ledger := &o.server.mutationCommits
	ledger.mu.Lock()
	record.mu.Lock()
	valid := ledger.byID[record.id] == record && record.kind == moveFileObservationCommit && record.owner == o.server && record.moveFile != nil && record.moveFile.writeVerdict == "all_writes_completed" && !record.moveFile.directClaimed && record.disk == mutationDiskCommitted && record.newSHA != "" && !record.committedAt.IsZero() && record.commitSeq != 0
	if valid {
		witness.id, witness.operation, witness.path, witness.blob, witness.committed, witness.commitSeq = record.id, record.key, record.absPath, record.newSHA, record.committedAt, record.commitSeq
		record.moveFile.directClaimed = true
		record.moveFile.directState = "pending_observer"
	}
	record.mu.Unlock()
	ledger.mu.Unlock()
	if !valid {
		return nil
	}
	return &indexer.ReadRepairObserver{Session: witness.session, RequestIDKey: witness.requestKey, Complete: witness.complete}
}
// The newest same-path publication owner is authoritative. Move observations
// remain excluded from physical/batch ticket binding; this is a separate actual
// reader cause attached to the same retained move file and returned write.
func (s *Server) moveReadRepairObserver(ctx context.Context, path string, idx *indexer.Indexer) (*indexer.ReadRepairObserver, bool) {
 if idx == nil { return nil, false }
 ledger := &s.mutationCommits
 ledger.mu.Lock()
 defer ledger.mu.Unlock()
 for i := len(ledger.ordered)-1; i >= 0; i-- {
  record := ledger.ordered[i]
  if !(record.isPublicationOwner() || record.kind == moveFileObservationCommit) || record.absPath != path { continue }
  if record.kind != moveFileObservationCommit { return nil, false }
  record.mu.Lock()
  defer record.mu.Unlock()
  if ledger.byID[record.id] != record || record.owner != s || record.moveFile == nil || record.moveFile.readClaimed || record.disk != mutationDiskCommitted || record.newSHA == "" || record.committedAt.IsZero() || record.commitSeq == 0 { return nil, true }
  // Capture can precede the overall write verdict. Completion still requires
  // that exact record's final successful verdict; no later retry can rebind it.
  if record.moveFile.writeVerdict != "pending" && record.moveFile.writeVerdict != "all_writes_completed" { return nil, true }
  key, _ := daemon.MCPRequestIDKeyForContext(ctx)
  witness := moveDirectWitness{server: weak.Make(s), record: weak.Make(record), indexer: weak.Make(idx), id: record.id, operation: record.key, path: record.absPath, blob: record.newSHA, committed: record.committedAt, commitSeq: record.commitSeq, session: SessionIDFromContext(ctx), requestKey: key, readAssisted: true}
  record.moveFile.readClaimed = true
  record.moveFile.readState = "pending_observer"
  return &indexer.ReadRepairObserver{Session: witness.session, RequestIDKey: witness.requestKey, Complete: witness.complete}, true
 }
 return nil, false
}

// Both routes retain their own immutable result and actual request cause.
// The slot is accessed only while the owning record mutex is held.
func (w moveDirectWitness) slot(file *moveFileObservation) (**moveDirectObservation, *string) {
 if w.readAssisted { return &file.readAssisted, &file.readState }
 return &file.direct, &file.directState
}

func (w moveDirectWitness) complete(observed indexer.ReadRepairObservation) {
	s, record, idx := w.server.Value(), w.record.Value(), w.indexer.Value()
	if s == nil || record == nil {
		return
	}
	if idx == nil {
		w.unavailable("unavailable_observer_owner")
		return
	}
	start, end := observed.Bounds()
	if !observed.AcceptedFor(idx, w.blob) || observed.AcceptedAt().Before(w.committed) {
		w.unavailable("unavailable_owner_bytes_or_acceptance")
		return
	}
	accepted := moveDirectObservation{generation: observed.Generation(), windowNS: end.Sub(start).Nanoseconds(), acceptedBlob: w.blob, writerToAcceptanceNS: observed.AcceptedAt().Sub(w.committed).Nanoseconds(), accounting: observed.PhaseAccounting()}
	ownedCause := false
	for i := range accepted.causes {
		session, key, generation, ok := observed.Cause(i)
		if !ok {
			break
		}
		accepted.causes[i] = mutationReadRepairCause{Session: session, RequestIDKey: key, AdmissionGeneration: generation}
		accepted.causeCount++
		if session == w.session && key == w.requestKey && generation != 0 {
			ownedCause = true
		}
	}
	if _, _, _, more := observed.Cause(len(accepted.causes)); more || !ownedCause {
		w.unavailable("unavailable_original_reconcile_cause")
		return
	}
	ledger := &s.mutationCommits
	ledger.mu.Lock()
	defer ledger.mu.Unlock()
	if ledger.byID[w.id] != record {
		return
	}
	// A later real writer, including another observed move, cannot donate its
	// accepted publication to this earlier file record.
	// Keep competing rows read-locked through binding. An older admitted
	// physical writer cannot start/commit between this check and acceptance.
	locked := make([]*mutationCommitRecord, 0)
	defer func() {
		for i := len(locked) - 1; i >= 0; i-- {
			locked[i].mu.RUnlock()
		}
	}()
	firstPath := true
	refused := ""
	for i := len(ledger.ordered) - 1; i >= 0; i-- {
		candidate := ledger.ordered[i]
		if !(candidate.isPublicationOwner() || candidate.kind == moveFileObservationCommit) || candidate.absPath != w.path {
			continue
		}
		if firstPath {
			firstPath = false
			if candidate != record {
				refused = "unavailable_superseded_owner"
			}
		}
		if candidate == record {
			continue
		}
		candidate.mu.RLock()
		locked = append(locked, candidate)
		if candidate.disk == mutationDiskCommitted && candidate.commitSeq > w.commitSeq {
			refused = "unavailable_competing_commit"
		}
		if candidate.disk == mutationDiskInFlight && !candidate.diskWriteStartedAt.IsZero() && !candidate.diskWriteStartedAt.After(observed.AcceptedAt()) {
			refused = "unavailable_competing_writer"
		}
	}
	if refused != "" {
		record.mu.Lock()
		if w.matchesWrite(record) {
   value, state := w.slot(record.moveFile)
   if *value == nil && *state == "pending_observer" { *state = refused }
  }
		record.mu.Unlock()
		return
	}
	record.mu.Lock()
	defer record.mu.Unlock()
	if !w.matchesWrite(record) { return }
 value, state := w.slot(record.moveFile)
 if *value != nil || *state != "pending_observer" { return }
 if record.moveFile.writeVerdict != "all_writes_completed" {
  *state = "unavailable_move_write_verdict"
  return
 }
 *value = &accepted
 *state = "observed"
}
func (w moveDirectWitness) matchesWrite(record *mutationCommitRecord) bool {
	return record.owner == w.server.Value() && record.id == w.id && record.kind == moveFileObservationCommit && record.moveFile != nil && record.key == w.operation && record.absPath == w.path && record.newSHA == w.blob && record.committedAt == w.committed && record.commitSeq == w.commitSeq && record.disk == mutationDiskCommitted
}
func (w moveDirectWitness) unavailable(reason string) {
	s, record := w.server.Value(), w.record.Value()
	if s == nil || record == nil {
		return
	}
	ledger := &s.mutationCommits
	ledger.mu.Lock()
	defer ledger.mu.Unlock()
	record.mu.Lock()
	defer record.mu.Unlock()
	if ledger.byID[w.id] == record && w.matchesWrite(record) {
  value, state := w.slot(record.moveFile)
  if *value == nil && *state == "pending_observer" { *state = reason }
 }
}

func (o *moveObservation) reindex(ctx context.Context, path string) bool {
	if o == nil {
		return false
	}
	var idx *indexer.Indexer
	if o.server.multiIndexer != nil {
		idx, _ = o.server.multiIndexer.IndexerForFile(path)
	}
	observer := o.observer(ctx, path, idx)
	if observer == nil && idx == nil {
		o.noteDirectUnavailable(path, "unavailable_observer_owner")
	}
	// The existing path and fallback order remain unchanged. No scheduler is
	// introduced, and no watcher ticket is invented for this direct reconcile.
	success := o.server.reindexFileObserved(path, observer)
	if !success && observer != nil {
		o.noteDirectUnavailable(path, "unavailable_reconcile_not_confirmed")
	}
	return success
}
func (o *moveObservation) noteDirectUnavailable(path, reason string) {
	record := o.records[path]
	if record == nil {
		return
	}
	ledger := &o.server.mutationCommits
	ledger.mu.Lock()
	defer ledger.mu.Unlock()
	record.mu.Lock()
	defer record.mu.Unlock()
	if ledger.byID[record.id] == record && record.kind == moveFileObservationCommit && record.moveFile != nil && record.key == o.id && record.moveFile.direct == nil && (record.moveFile.directState == "not_started" || record.moveFile.directState == "pending_observer") {
		record.moveFile.directClaimed = true
		record.moveFile.directState = reason
	}
}

func (s *Server) moveFileObservationPayload(ctx context.Context, id string) (map[string]any, bool) {
	if !strings.HasPrefix(id, "move-file-") {
		return nil, false
	}
	payload := map[string]any{"found": false, "receipt": id, "receipt_kind": "move_file_observation", "guidance": "observation only; source_id/target_id/touched and real write outcomes remain the move authority"}
	checkout, incarnation := mutationCheckoutScope(ctx)
	var ack *editAcknowledgement
	var direct, readAssisted *moveDirectObservation
	ledger := &s.mutationCommits
	ledger.mu.Lock()
	record := ledger.byID[id]
	if record != nil {
		record.mu.RLock()
		if record.kind == moveFileObservationCommit && record.owner == s && record.moveFile != nil && mutationCheckoutScopeMatches(checkout, incarnation, record.checkoutID, record.checkoutIncarnation) {
			data := record.moveFile
			payload["found"], payload["operation_id"], payload["source_id"], payload["target_id"], payload["path"], payload["role"] = true, data.operationID, data.sourceID, data.targetID, record.relPath, data.role
			payload["move_write_verdict"], payload["file_write_status"], payload["writer_return_observed"], payload["new_sha"] = data.writeVerdict, record.disk, !record.committedAt.IsZero(), record.newSHA
			payload["direct_reconcile_state"], payload["requested_background_state"] = data.directState, "not_applicable_direct_reconcile"
			payload["read_assisted_reconcile_state"] = data.readState
			// This handler calls the existing synchronous reconcile, never queue
			// admission/wait. Preserve raw ACK nulls/counts and name applicability
			// explicitly instead of manufacturing measured zero scheduler work.
			payload["acknowledgement_phase_applicability"] = map[string]string{"publish": "not_applicable_direct_reconcile", "reindex_wait": "not_applicable_direct_reconcile"}
			timing := map[string]any{"scope": "move_file_write", "clock": "monotonic", "state": "not_observed", "outcome": record.disk}
			if !record.diskWriteStartedAt.IsZero() && !record.committedAt.IsZero() {
				timing["state"] = "invalid_order"
				if !record.diskWriteStartedAt.Before(record.startedAt) && !record.committedAt.Before(record.diskWriteStartedAt) {
					timing["state"] = "measured"
					timing["started_ns"] = record.diskWriteStartedAt.Sub(record.startedAt).Nanoseconds()
					timing["returned_ns"] = record.committedAt.Sub(record.startedAt).Nanoseconds()
					timing["window_ns"] = record.committedAt.Sub(record.diskWriteStartedAt).Nanoseconds()
				}
			}
			payload["write_timing"] = timing
			if data.direct != nil {
				copy := *data.direct
				direct = &copy
			}
   if data.readAssisted != nil { copy := *data.readAssisted; readAssisted = &copy }
			ack = record.acknowledgement
		}
		record.mu.RUnlock()
	}
	ledger.mu.Unlock()
	if direct != nil { payload["direct_reconcile"] = direct.payload("move_direct_reconcile") }
 if readAssisted != nil { payload["read_assisted_reconcile"] = readAssisted.payload("move_read_assisted_reconcile") }
	var snapshot *editAcknowledgementSnapshot
	if ack != nil {
		copy := ack.snapshot()
		snapshot = &copy
	}
	payload["acknowledgement"] = buildAcknowledgementPayload(snapshot, "unavailable_ledger_entry_or_scope")
	return payload, true
}

func (observed *moveDirectObservation) payload(scope string) map[string]any {
 phases := observed.accounting.Payload()
 phases["scope"] = scope
 causes := make([]mutationReadRepairCause, observed.causeCount)
 copy(causes, observed.causes[:observed.causeCount])
 return map[string]any{"scope": scope, "clock": "monotonic", "owner_generation": observed.generation, "window_ns": observed.windowNS, "accepted_blob_sha": observed.acceptedBlob, "writer_return_to_acceptance_ns": observed.writerToAcceptanceNS, "reconcile_causes": causes, "phase_accounting": phases, "additive_with_acknowledgement": false}
}
