package indexer

import (
	"context"
	"errors"
	"time"

	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/viewmetrics"
)

func (c *CheckoutCoordinator) retirementInUse(generationID int64) bool {
	if c == nil {
		return false
	}
	if leased := c.inUse(); leased != nil && leased(generationID) {
		return true
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.basePinned == generationID || c.routedDirty == generationID {
		return true
	}
	for _, retained := range c.retained {
		if retained.generationID == generationID {
			return true
		}
	}
	for _, retained := range c.retainedDirty {
		if retained.generationID == generationID {
			return true
		}
	}
	return false
}

func (c *CheckoutCoordinator) pendingRetirementGenerations() []int64 {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	pending := make([]int64, 0, len(c.backlog))
	for generationID := range c.backlog {
		pending = append(pending, generationID)
	}
	c.mu.Unlock()
	return pending
}

// retirePayloadGenerationSlice advances one coordinator-owned retirement while
// retaining the coordinator's full in-use predicate. A budget yield leaves the
// backlog entry in place so a later worker pass resumes the same generation.
func (c *CheckoutCoordinator) retirePayloadGenerationSlice(
	ctx context.Context,
	generationID int64,
	maxElapsed time.Duration,
) (retired bool, pending bool, err error) {
	if c == nil || c.store == nil || generationID <= 0 {
		return false, false, nil
	}
	c.mu.Lock()
	_, pending = c.backlog[generationID]
	c.mu.Unlock()
	if !pending {
		return false, false, nil
	}

	err = c.store.RetirePayloadGenerationSlice(ctx, generationID, c.retirementInUse, maxElapsed)
	switch {
	case err == nil, errors.Is(err, store_sqlite.ErrCatalogNotFound):
		c.mu.Lock()
		delete(c.backlog, generationID)
		c.mu.Unlock()
		viewmetrics.Count(viewmetrics.GenerationSweepCollectedTotal, viewmetrics.SweepCheckout)
		return true, false, nil
	case errors.Is(err, store_sqlite.ErrPayloadSweepBudgetExhausted),
		errors.Is(err, store_sqlite.ErrPayloadGenerationInUse):
		return false, true, nil
	default:
		return false, true, err
	}
}
