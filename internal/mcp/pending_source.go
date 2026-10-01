package mcp

import (
	"path/filepath"
	"sort"
	"sync"
)

// pendingSourceRegistry is publication truth, independent of the bounded
// recovery ledger. Each mutation has its own version (commit receipt); an
// older publication cannot clear a newer write to the same checkout/path.
// There is deliberately no age or capacity eviction for unresolved writes.
type pendingSourceRegistry struct {
	mu       sync.Mutex
	versions map[string]*mutationCommitRecord
}

func (p *pendingSourceRegistry) register(record *mutationCommitRecord) {
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
	p := &s.pendingSourceFiles
	p.mu.Lock()
	defer p.mu.Unlock()
	paths := make(map[string]struct{})
	for id, record := range p.versions {
		snap := record.snapshot()
		s.refreshPendingSourceRecord(record)
		snap = record.snapshot()
		record.mu.RLock()
		recovered := record.pendingSourceRecovered
		record.mu.RUnlock()
		if recovered || snap.DiskStatus == mutationDiskNotApplied || snap.DiskStatus == mutationDiskFailed ||
			(snap.DiskStatus == mutationDiskCommitted && snap.GraphStatus == mutationGraphFresh) {
			delete(p.versions, id)
			continue
		}
		if snap.CheckoutID == checkoutID && (incarnation == "" || snap.CheckoutIncarnation == incarnation) {
			paths[filepath.Clean(record.absPath)] = struct{}{}
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
	snap := record.snapshot()
	if snap.GraphStatus == mutationGraphFresh || snap.ReindexReceipt == "" {
		return
	}
	if value, ok := s.mutationReceipts.Load(snap.ReindexReceipt); ok {
		if receipt, ok := value.(*mutationReceipt); ok {
			receipt.mu.RLock()
			recovered := receipt.barrierRecoveredGeneration > 0 && receipt.checkoutID == snap.CheckoutID && receipt.checkoutIncarnation == snap.CheckoutIncarnation
			receipt.mu.RUnlock()
			if recovered {
				record.mu.Lock()
				record.pendingSourceRecovered = true
				record.mu.Unlock()
				return
			}
			outcome := receipt.outcome(true)
			if !outcome.Pending && outcome.Reindexed && outcome.Err == nil && snap.GraphStatus != mutationGraphPending {
				record.mu.Lock()
				record.pendingSourceRecovered = true
				record.mu.Unlock()
				return
			}
			if !outcome.Pending && snap.GraphStatus == mutationGraphPending {
				record.recordGraph(outcome)
			}
		}
	}
}

// Capture only failed versions that existed before recovery sampling. IDs are
// commit versions, so a new edit to the same path cannot be retired by it.
func (s *Server) captureFailedSourceVersions(eligible map[string]struct{}, checkoutID, incarnation, absPath string) {
	p := &s.pendingSourceFiles
	p.mu.Lock()
	defer p.mu.Unlock()
	for id, record := range p.versions {
		snap := record.snapshot()
		if snap.DiskStatus != mutationDiskCommitted || !snap.GraphRecorded || snap.GraphStatus == mutationGraphPending || snap.GraphStatus == mutationGraphFresh {
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
