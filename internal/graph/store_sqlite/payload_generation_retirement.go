package store_sqlite

import (
	"context"
	"database/sql"
	"fmt"
)

// beginPayloadGenerationRetirement is the durable publication boundary for
// retirement. Ready-generation claim/bind and this transition share writeMu,
// and the guarded update observes every durable owner plus active handoff
// leases in the same transaction. Once the row is retiring, later claims and
// binds reject it before payload deletion begins.
func (c *Catalog) beginPayloadGenerationRetirement(ctx context.Context, generationID int64) error {
	core := c.store.storeCore
	if err := core.writeMu.LockContext(ctx); err != nil {
		return err
	}
	defer core.writeMu.Unlock()
	if err := ensureReadyGenerationCacheSchema(ctx, core); err != nil {
		return err
	}

	tx, err := core.writerDB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	var now int64
	if err := tx.QueryRowContext(ctx, `SELECT unixepoch()`).Scan(&now); err != nil {
		return fmt.Errorf("read retirement clock: %w", err)
	}
	result, err := tx.ExecContext(ctx, `
UPDATE view_generations
SET state = ?
WHERE generation_id = ?
  AND NOT EXISTS (
    SELECT 1 FROM checkout_routes
    WHERE commit_generation_id = ? OR dirty_generation_id = ?
  )
  AND NOT EXISTS (
    SELECT 1 FROM ref_views WHERE active_generation_id = ?
  )
  AND NOT EXISTS (
    SELECT 1 FROM view_generations WHERE base_generation_id = ?
  )
  AND NOT EXISTS (
    SELECT 1 FROM dedicated_graphs WHERE active_generation_id = ?
  )
  AND NOT EXISTS (
    SELECT 1 FROM ready_generation_leases
    WHERE generation_id = ? AND expires_at > ?
  )`,
		string(ViewGenerationRetiring), generationID,
		generationID, generationID, generationID, generationID, generationID,
		generationID, now,
	)
	if err != nil {
		return fmt.Errorf("begin retirement for generation %d: %w", generationID, err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("read retirement result for generation %d: %w", generationID, err)
	}
	if affected == 1 {
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit retirement for generation %d: %w", generationID, err)
		}
		return nil
	}

	var state string
	if err := tx.QueryRowContext(ctx,
		`SELECT state FROM view_generations WHERE generation_id = ?`, generationID,
	).Scan(&state); err != nil {
		if err == sql.ErrNoRows {
			return fmt.Errorf("%w: generation %d", ErrCatalogNotFound, generationID)
		}
		return fmt.Errorf("classify retirement for generation %d: %w", generationID, err)
	}
	var checkoutRef, refViewRef, childRef, dedicatedRef, liveLease int
	if err := tx.QueryRowContext(ctx, `
SELECT
  EXISTS(SELECT 1 FROM checkout_routes
         WHERE commit_generation_id = ? OR dirty_generation_id = ?),
  EXISTS(SELECT 1 FROM ref_views WHERE active_generation_id = ?),
  EXISTS(SELECT 1 FROM view_generations WHERE base_generation_id = ?),
  EXISTS(SELECT 1 FROM dedicated_graphs WHERE active_generation_id = ?),
  EXISTS(SELECT 1 FROM ready_generation_leases
         WHERE generation_id = ? AND expires_at > ?)`,
		generationID, generationID, generationID, generationID, generationID,
		generationID, now,
	).Scan(&checkoutRef, &refViewRef, &childRef, &dedicatedRef, &liveLease); err != nil {
		return fmt.Errorf("classify retirement references for generation %d: %w", generationID, err)
	}
	if liveLease != 0 {
		return fmt.Errorf("%w: generation %d", ErrPayloadGenerationInUse, generationID)
	}
	if checkoutRef != 0 || refViewRef != 0 || childRef != 0 || dedicatedRef != 0 {
		return fmt.Errorf("%w: generation %d", ErrCatalogGenerationReferenced, generationID)
	}
	return fmt.Errorf("%w: generation %d cannot retire from state %s", ErrCatalogStaleGuard, generationID, state)
}
