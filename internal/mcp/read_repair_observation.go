package mcp

import (
	"context"
	"time"
	"weak"

	"github.com/zzet/gortex/internal/daemon"
	"github.com/zzet/gortex/internal/indexer"
)

// One immutable, independent coordinator execution. This is neither the
// original receipt's P nor B: read repair can overlap that receipt's quiet
// wait, and adding these durations would count the same time twice. Retention
// belongs only to the existing mutation ledger, independently of the original
// publication recorder's index. That pointer is an incarnation witness only.
type mutationReadRepairObservation struct {
	generation uint64
	windowNS   int64
	causes     [8]mutationReadRepairCause
	causeCount int
	accounting indexer.ReadRepairPhaseAccounting
	// Frozen admission identity survives that existing receipt's shorter TTL;
	// accepted R is retained by the mutation ledger, not by the receipt map.
	requestedReceipt    string
	requestedGeneration uint64
	acceptedBlobSHA     string
}

type mutationReadRepairCause struct {
	Session             string `json:"session,omitempty"`
	RequestIDKey        string `json:"request_id_key,omitempty"`
	AdmissionGeneration uint64 `json:"admission_generation"`
}

func (o mutationReadRepairObservation) payload() map[string]any {
	causes := make([]mutationReadRepairCause, o.causeCount)
	copy(causes, o.causes[:o.causeCount])
	return map[string]any{
		"scope": "read_assisted_reconcile", "clock": "monotonic",
		"owner_generation": o.generation, "window_ns": o.windowNS,
		"reader_causes": causes, "additive_with_requested_background": false,
		"background_relationship": "unavailable",
		"phase_accounting":        o.accounting.Payload(),
		"requested_receipt":       o.requestedReceipt,
		"requested_generation":    o.requestedGeneration,
		"accepted_blob_sha":       o.acceptedBlobSHA,
	}
}

// A witness retains only immutable identities and weak owners during the
// existing work lifetime. It cannot keep an evicted receipt or Server alive.
type mutationReadRepairWitness struct {
	server      weak.Pointer[Server]
	record      weak.Pointer[mutationCommitRecord]
	indexer     weak.Pointer[indexer.Indexer]
	publication weak.Pointer[indexer.PublicationPhaseRecord]
	admission   weak.Pointer[mutationReceipt]
	id          string
	fingerprint string
	absPath     string
	blobSHA     string
	receipt     string
	checkout    string
	incarnation string
	commitSeq   uint64
	generation  uint64
	committedAt time.Time
}

func (s *Server) readRepairObserver(ctx context.Context, absPath string, idx *indexer.Indexer) *indexer.ReadRepairObserver {
	witness, ok := s.captureReadRepairWitness(absPath, idx)
	if !ok {
		return nil
	}
	key, _ := daemon.MCPRequestIDKeyForContext(ctx)
	return &indexer.ReadRepairObserver{Session: SessionIDFromContext(ctx), RequestIDKey: key, Complete: witness.complete}
}

// Bind the actual existing ticket before waiting, without changing public
// pending/result fields. All receipt identity fields read here are immutable.
// No publication/recorder methods or mutable receipt state are read under the
// existing ledger -> record locks.
func (s *Server) bindReadRepairAdmission(absPath string, publication *indexer.PublicationPhaseRecord, admission *mutationReceipt) {
	if publication == nil || admission == nil || admission.generation == 0 || admission.checkoutScoped || admission.path != absPath {
		return
	}
	ledger := &s.mutationCommits
	ledger.mu.Lock()
	defer ledger.mu.Unlock()
	for i := len(ledger.ordered) - 1; i >= 0; i-- {
		record := ledger.ordered[i]
		if !record.isPhysicalCommit() {
			continue
		}
		record.mu.Lock()
		match := record.absPath == absPath
		if match && ledger.byID[record.id] == record && record.owner == s && record.disk == mutationDiskCommitted &&
			record.commitSeq != 0 && record.newSHA != "" && record.publication == publication &&
			record.readRepairAdmission == (weak.Pointer[mutationReceipt]{}) {
			record.readRepairAdmission = weak.Make(admission)
		}
		record.mu.Unlock()
		if match {
			return
		}
	}
}

func (s *Server) captureReadRepairWitness(absPath string, idx *indexer.Indexer) (mutationReadRepairWitness, bool) {
	if idx == nil {
		return mutationReadRepairWitness{}, false
	}
	ledger := &s.mutationCommits
	ledger.mu.Lock()
	var witness mutationReadRepairWitness
	for i := len(ledger.ordered) - 1; i >= 0; i-- {
		record := ledger.ordered[i]
		if !record.isPhysicalCommit() {
			continue
		}
		record.mu.RLock()
		if record.absPath != absPath {
			record.mu.RUnlock()
			continue
		}
		// The newest physical entry is authoritative even if it is not yet
		// eligible. Never fall back to an older edit of the same path.
		admission := record.readRepairAdmission.Value()
		if ledger.byID[record.id] == record && record.owner == s && record.disk == mutationDiskCommitted &&
			record.commitSeq != 0 && record.newSHA != "" && record.publication != nil &&
			admission != nil && admission.generation != 0 && admission.path == absPath && !admission.checkoutScoped && record.readRepair == nil &&
			(!record.graphRecorded || record.graph == mutationGraphPending) {
			witness = mutationReadRepairWitness{
				server: weak.Make(s), record: weak.Make(record), indexer: weak.Make(idx),
				publication: weak.Make(record.publication), id: record.id, fingerprint: record.fingerprint,
				admission: weak.Make(admission), absPath: absPath, blobSHA: record.newSHA, receipt: admission.id,
				checkout: record.checkoutID, incarnation: record.checkoutIncarnation,
				commitSeq: record.commitSeq, generation: admission.generation, committedAt: record.committedAt,
			}
		}
		record.mu.RUnlock()
		break
	}
	ledger.mu.Unlock()
	return witness, witness.id != ""
}

func (w mutationReadRepairWitness) complete(observation indexer.ReadRepairObservation) {
	s, record, idx := w.server.Value(), w.record.Value(), w.indexer.Value()
	if s == nil || record == nil || idx == nil || !observation.AcceptedFor(idx, w.blobSHA) {
		return
	}
	started, finished := observation.Bounds()
	if w.committedAt.IsZero() || started.Before(w.committedAt) {
		return
	}
	accepted := mutationReadRepairObservation{generation: observation.Generation(), windowNS: finished.Sub(started).Nanoseconds(), accounting: observation.PhaseAccounting()}
	for i := range accepted.causes {
		session, key, generation, ok := observation.Cause(i)
		if !ok {
			break
		}
		accepted.causes[i] = mutationReadRepairCause{Session: session, RequestIDKey: key, AdmissionGeneration: generation}
		accepted.causeCount++
	}
	if _, _, _, more := observation.Cause(len(accepted.causes)); more {
		return // Never silently truncate a future larger producer bound.
	}
	w.bind(accepted)
}

// bind is the acceptance fence after the execution's content/owner proof. It
// performs no recorder callbacks or registry lookups under ledger locks.
func (w mutationReadRepairWitness) bind(accepted mutationReadRepairObservation) {
	s, record := w.server.Value(), w.record.Value()
	if s == nil || record == nil {
		return
	}
	ledger := &s.mutationCommits
	ledger.mu.Lock()
	defer ledger.mu.Unlock()
	if ledger.byID[w.id] != record {
		return
	}
	// Supersession before acceptance refuses the link. Once accepted, later
	// edits do not erase this historical execution or mutate earlier snapshots.
	for i := len(ledger.ordered) - 1; i >= 0; i-- {
		candidate := ledger.ordered[i]
		if !candidate.isPhysicalCommit() {
			continue
		}
		candidate.mu.RLock()
		match := candidate.absPath == w.absPath
		candidate.mu.RUnlock()
		if match {
			if candidate != record {
				return
			}
			break
		}
	}
	record.mu.Lock()
	defer record.mu.Unlock()
	if record.owner != s || record.id != w.id || record.commitSeq != w.commitSeq || record.newSHA != w.blobSHA ||
		record.absPath != w.absPath || record.fingerprint != w.fingerprint || record.disk != mutationDiskCommitted ||
		(record.reindexReceipt != "" && record.reindexReceipt != w.receipt) ||
		(record.reindexGeneration != 0 && record.reindexGeneration != w.generation) ||
		w.admission.Value() == nil || record.readRepairAdmission != w.admission ||
		record.checkoutID != w.checkout || record.checkoutIncarnation != w.incarnation ||
		record.publication == nil || record.publication != w.publication.Value() || record.readRepair != nil {
		return
	}
	accepted.requestedReceipt, accepted.requestedGeneration = w.receipt, w.generation
	accepted.acceptedBlobSHA = w.blobSHA
	record.readRepair = &accepted
}

// Only the already-needed multi-repository read repair is observed. Embedded
// and unobserved callers preserve their existing fallback and nil behavior.
func (s *Server) reindexFileForRead(ctx context.Context, absPath string, idx *indexer.Indexer) bool {
	if s.multiIndexer != nil {
		if prefix := s.multiIndexer.RepoForFile(absPath); prefix != "" {
			result, err := s.multiIndexer.IncrementalReindexRepoObserved(prefix, []string{absPath}, s.readRepairObserver(ctx, absPath, idx))
			if incrementalReindexSucceeded(result, err) {
				return true
			}
		}
	}
	// Match reindexFile's existing standalone fallback without submitting the
	// multi-repository reconcile twice after a failure.
	if s.indexer != nil {
		if root := s.indexer.RootPath(); root != "" {
			if _, ok := relativeWithinRoot(root, absPath); ok {
				result, err := s.indexer.IncrementalReindexPaths(root, []string{absPath})
				return incrementalReindexSucceeded(result, err)
			}
		}
	}
	return false
}
