package mcp

import (
	"context"
	"sync"
	"time"
)

// Cancelled preparation owns no scoring/materialization work while another
// lazy caller holds the lock. Legacy callers retain ordinary mutex coalescing.
// The caller owns mu only on success; no worker is detached from its owner.
func lockAnalysisPreparation(ctx context.Context, mu *sync.Mutex) bool {
	if ctx.Done() == nil {
		mu.Lock()
		return true
	}
	if ctx.Err() != nil {
		return false
	}
	if mu.TryLock() {
		if ctx.Err() != nil {
			mu.Unlock()
			return false
		}
		return true
	}
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return false
		case <-ticker.C:
			if mu.TryLock() {
				if ctx.Err() != nil {
					mu.Unlock()
					return false
				}
				return true
			}
		}
	}
}
