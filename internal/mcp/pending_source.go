package mcp

import (
	"path/filepath"
	"sort"
	"sync"
)

// pendingSourceRegistry keeps per-commit publication evidence, independently
// of the bounded recovery ledger. Canonical records have no source-request
// reader: retire failed/fresh graph outcomes (including receipt_lost) and
// unapplied/failed disk writes promptly. An explicitly recorded terminal stale
// history is distinct from an unrecorded stale seed and retains its existing
// bookkeeping semantics. Checkout failures stay pending until a successful
// newer commit, a captured recovery, or authoritative incarnation retirement.
// Neither lane becomes fresh by time; unresolved versions have no age/cap eviction.
type pendingSourceRegistry struct {
	mu       sync.Mutex
	versions map[string]*mutationCommitRecord
	// freshCommits is the highest commitSeq per checkout/incarnation/path whose
	// bytes reached a fresh publication. It outlives the fresh version only
	// while another version of that key is registered, so it is bounded by
	// the registry.
	freshCommits map[pendingSourceKey]uint64
}

type pendingSourceKey struct {
	checkoutID, incarnation, path string
}

func (p *pendingSourceRegistry) register(record *mutationCommitRecord) {
	if !record.isPhysicalCommit() {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.versions == nil {
		p.versions = make(map[string]*mutationCommitRecord)
	}
	p.versions[record.id] = record
}

// pendingSourcePaths reconciles exact ticket outcomes, even after the recovery
// ledger evicted the commit. A failed publication remains pending: failure is
// not evidence that the graph contains the committed bytes.
func (s *Server) pendingSourcePaths(checkoutID, incarnation string) []string {
	s.retireDeadPendingSourceIncarnations()
	p := &s.pendingSourceFiles
	// Refresh may update a commit and retire settled versions. Never invoke
	// that callback while holding the registry lock.
	p.mu.Lock()
	records := make([]*mutationCommitRecord, 0, len(p.versions))
	for _, record := range p.versions {
		records = append(records, record)
	}
	p.mu.Unlock()
	for _, record := range records {
		s.refreshPendingSourceRecord(record)
	}
	return p.paths(checkoutID, incarnation)
}

func (p *pendingSourceRegistry) paths(checkoutID, incarnation string) []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.freshCommits == nil {
		p.freshCommits = make(map[pendingSourceKey]uint64)
	}
	survivors := make(map[string]mutationCommitSnapshot, len(p.versions))
	for id, record := range p.versions {
		snap := record.snapshot()
		record.mu.RLock()
		recovered := record.pendingSourceRecovered
		record.mu.RUnlock()
		if snap.DiskStatus == mutationDiskCommitted && snap.GraphStatus == mutationGraphFresh {
			key := pendingSourceKey{snap.CheckoutID, snap.CheckoutIncarnation, filepath.Clean(record.absPath)}
			if snap.CommitSeq > p.freshCommits[key] {
				p.freshCommits[key] = snap.CommitSeq
			}
		}
		if recovered || snap.DiskStatus == mutationDiskNotApplied || snap.DiskStatus == mutationDiskFailed ||
			(snap.CheckoutID == "" && snap.GraphRecorded && snap.GraphStatus == mutationGraphFailed) ||
			(snap.DiskStatus == mutationDiskCommitted && snap.GraphStatus == mutationGraphFresh) {
			delete(p.versions, id)
			continue
		}
		survivors[id] = snap
	}
	paths := make(map[string]struct{})
	live := make(map[pendingSourceKey]struct{}, len(survivors))
	for id, snap := range survivors {
		path := filepath.Clean(p.versions[id].absPath)
		key := pendingSourceKey{snap.CheckoutID, snap.CheckoutIncarnation, path}
		// A fresh publication of a later commit to the same checkout
		// incarnation and path read bytes that replaced this version's, so
		// this version's own ticket outcome (e.g. a superseded refresh) no
		// longer decides whether the path is pending.
		if snap.DiskStatus == mutationDiskCommitted && snap.CommitSeq != 0 && snap.CommitSeq < p.freshCommits[key] {
			delete(p.versions, id)
			continue
		}
		live[key] = struct{}{}
		if snap.CheckoutID == checkoutID && (incarnation == "" || snap.CheckoutIncarnation == incarnation) {
			paths[path] = struct{}{}
		}
	}
	// Uncommitted versions take a later commitSeq, so a watermark only matters
	// while an older version of its key is registered.
	for key := range p.freshCommits {
		if _, ok := live[key]; !ok {
			delete(p.freshCommits, key)
		}
	}
	result := make([]string, 0, len(paths))
	for path := range paths {
		result = append(result, path)
	}
	sort.Strings(result)
	return result
}

// Resolve a late ticket immediately when the commit binds its receipt. The
// receipt can finish before the mutation handler records its pending outcome.
func (s *Server) refreshPendingSourceRecord(record *mutationCommitRecord) {
	if !record.isPhysicalCommit() {
		return
	}
	defer s.retireSettledPendingSource(record)
	snap := record.snapshot()
	if snap.GraphStatus == mutationGraphFresh || snap.ReindexReceipt == "" {
		return
	}
	if value, ok := s.mutationReceipts.Load(snap.ReindexReceipt); ok {
		if receipt, ok := value.(*mutationReceipt); ok {
			s.applyPendingSourceReceipt(record, snap, receipt)
		}
	} else {
		record.markReceiptLost(snap.ReindexReceipt)
	}
}

func (s *Server) applyPendingSourceReceipt(record *mutationCommitRecord, snap mutationCommitSnapshot, receipt *mutationReceipt) {
	receipt.mu.RLock()
	recovered := receipt.barrierRecoveredGeneration > 0 && receipt.checkoutID == snap.CheckoutID && receipt.checkoutIncarnation == snap.CheckoutIncarnation
	receipt.mu.RUnlock()
	if recovered {
		record.markSourceRecoveredForReceipt(snap.ReindexReceipt)
		return
	}
	outcome := receipt.outcome(true)
	unsettled := snap.GraphStatus == mutationGraphPending || (snap.GraphStatus == mutationGraphStale && !snap.GraphRecorded)
	if !outcome.Pending && outcome.Reindexed && outcome.Err == nil && !unsettled {
		record.markSourceRecoveredForReceipt(snap.ReindexReceipt)
		return
	}
	if !outcome.Pending && unsettled {
		record.recordGraphForReceipt(snap.ReindexReceipt, outcome)
	}
}

func (s *Server) retireSettledPendingSource(record *mutationCommitRecord) {
	if !record.isPhysicalCommit() {
		return
	}
	record.mu.RLock()
	retire := record.disk == mutationDiskNotApplied || record.disk == mutationDiskFailed ||
		(record.graphRecorded && (record.graph == mutationGraphFresh || (record.checkoutID == "" && record.graph == mutationGraphFailed)))
	record.mu.RUnlock()
	if retire {
		// Reuse the commit-order watermark rules when retiring a fresh commit;
		// removing it alone would lose evidence for older pending versions.
		s.pendingSourceFiles.paths("", "")
	}
}

// Capture only failed versions that existed before recovery sampling. IDs are
// commit versions, so a new edit to the same path cannot be retired by it.
func (s *Server) captureFailedSourceVersions(eligible map[string]struct{}, checkoutID, incarnation, absPath string, receiptOnly bool) {
	p := &s.pendingSourceFiles
	p.mu.Lock()
	defer p.mu.Unlock()
	for id, record := range p.versions {
		snap := record.snapshot()
		_, receiptCaptured := eligible[snap.ReindexReceipt]
		if receiptOnly && !receiptCaptured {
			continue
		}
		// The receipt witness can be terminal before its disk handler has
		// recorded that outcome. A recovery must retire this exact version
		// immediately too, rather than waiting for a later registry sweep.
		if snap.DiskStatus != mutationDiskCommitted || snap.GraphStatus == mutationGraphFresh ||
			(!receiptCaptured && (!snap.GraphRecorded || snap.GraphStatus == mutationGraphPending)) {
			continue
		}
		if snap.CheckoutID != checkoutID || snap.CheckoutIncarnation != incarnation {
			continue
		}
		if absPath != "" && filepath.Clean(record.absPath) != filepath.Clean(absPath) {
			continue
		}
		eligible[id] = struct{}{}
	}
}

func (s *Server) resolveCapturedSourceVersions(eligible map[string]struct{}, checkoutID, incarnation, absPath string) {
	p := &s.pendingSourceFiles
	p.mu.Lock()
	defer p.mu.Unlock()
	for id := range eligible {
		record := p.versions[id]
		if record == nil {
			continue
		}
		snap := record.snapshot()
		if snap.CheckoutID != checkoutID || snap.CheckoutIncarnation != incarnation {
			continue
		}
		if absPath != "" && filepath.Clean(record.absPath) != filepath.Clean(absPath) {
			continue
		}
		// The eligible set is a pre-sampling witness; versions are never reused.
		record.mu.Lock()
		record.pendingSourceRecovered = true
		record.mu.Unlock()
		delete(p.versions, id)
	}
}
