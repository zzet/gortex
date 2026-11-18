package store_sqlite

import (
	"context"
	"fmt"
	"time"
)

// RetirePayloadGenerationSlice runs one resumable retirement pass with a soft
// elapsed budget. The budget is checked between committed chunks, so a pass
// always preserves completed work and may overrun by at most its current chunk.
func (s *Store) RetirePayloadGenerationSlice(
	ctx context.Context,
	generationID int64,
	inUse func(int64) bool,
	maxElapsed time.Duration,
) error {
	if maxElapsed <= 0 {
		return fmt.Errorf("%w: retirement slice duration %s", ErrCatalogInvalidValue, maxElapsed)
	}
	budget := defaultPayloadSweepBudget()
	budget.maxElapsed = maxElapsed
	return s.retirePayloadGeneration(ctx, generationID, inUse, budget)
}
