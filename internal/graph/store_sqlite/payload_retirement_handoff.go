package store_sqlite

import "context"

// FencePayloadGenerationRetirement commits the ordinary reference fence,
// closes payload admission and drains prior writers without deleting rows.
// The retiring catalog row is durable deferred work, including after restart.
// A caller must hand it to the existing deferred retirement owner; successful
// return means fenced, not physically collected. All lease/reference refusals
// are the same as full retirement.
func (s *Store) FencePayloadGenerationRetirement(ctx context.Context, generationID int64, inUse func(int64) bool) error {
	return s.retirePayloadGeneration(ctx, generationID, inUse, payloadSweepBudget{fenceOnly: true})
}
