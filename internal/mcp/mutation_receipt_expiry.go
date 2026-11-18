package mcp

import "time"

func (s *Server) scheduleMutationReceiptExpiry(receipt *mutationReceipt, retention time.Duration) {
	expire := func() { s.expireMutationReceipt(receipt) }
	if s.mutationReceiptAfterFunc != nil {
		s.mutationReceiptAfterFunc(retention, expire)
		return
	}
	time.AfterFunc(retention, expire)
}

func (s *Server) expireMutationReceipt(receipt *mutationReceipt) {
	if !s.mutationReceipts.CompareAndDelete(receipt.id, receipt) {
		return
	}
	// The ledger is bounded; registry versions can outlive it. Settle both
	// owners without holding their locks while refreshing publication state.
	records := s.mutationCommits.recent(maxMutationCommits)
	p := &s.pendingSourceFiles
	p.mu.Lock()
	for _, record := range p.versions {
		records = append(records, record)
	}
	p.mu.Unlock()
	for _, record := range records {
		record.mu.RLock()
		matches := record.reindexReceipt == receipt.id
		record.mu.RUnlock()
		if matches {
			s.refreshPendingSourceRecord(record)
		}
	}
}
