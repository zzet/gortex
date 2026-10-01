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
		if snap.DiskStatus == mutationDiskNotApplied || snap.DiskStatus == mutationDiskFailed ||
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
	if snap.GraphStatus != mutationGraphPending || snap.ReindexReceipt == "" {
		return
	}
	if value, ok := s.mutationReceipts.Load(snap.ReindexReceipt); ok {
		if receipt, ok := value.(*mutationReceipt); ok {
			outcome := receipt.outcome(true)
			if !outcome.Pending {
				record.recordGraph(outcome)
			}
		}
	}
}
