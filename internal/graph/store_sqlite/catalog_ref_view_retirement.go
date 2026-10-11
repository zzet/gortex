package store_sqlite

import (
	"context"
	"database/sql"
	"fmt"
)

// ReleaseRefViewGeneration atomically releases a ref view only while it still
// points at generationID and makes that ready generation discoverable by the
// deferred retirement worker. A stale retention candidate changes nothing.
func (c *Catalog) ReleaseRefViewGeneration(
	ctx context.Context,
	refViewID string,
	generationID int64,
) (bool, error) {
	if err := requireCatalogID("ref_view_id", refViewID); err != nil {
		return false, err
	}
	if generationID <= 0 {
		return false, fmt.Errorf("%w: generation_id %d", ErrCatalogInvalidValue, generationID)
	}
	var released bool
	err := c.withTx(ctx, func(tx *sql.Tx) error {
		result, err := tx.ExecContext(ctx,
			`DELETE FROM ref_views WHERE ref_view_id = ? AND active_generation_id = ?`,
			refViewID, generationID,
		)
		if err != nil {
			return err
		}
		deleted, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if deleted == 0 {
			return nil
		}

		result, err = tx.ExecContext(ctx,
			`UPDATE view_generations SET state = ? WHERE generation_id = ? AND state = ?`,
			string(ViewGenerationSuperseded), generationID, string(ViewGenerationReady),
		)
		if err != nil {
			return err
		}
		marked, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if marked != 1 {
			return fmt.Errorf("ref view %s generation %d is not ready", refViewID, generationID)
		}
		released = true
		return nil
	})
	if released && err == nil {
		noteGenerationReferenceReleased(generationID)
	}
	return released && err == nil, err
}

// MarkUnreferencedRefViewGenerationSuperseded makes an unpointed ready
// generation durable retirement work. A ref view attached after candidate
// selection wins the compare-and-set and leaves the generation unchanged.
func (c *Catalog) MarkUnreferencedRefViewGenerationSuperseded(
	ctx context.Context,
	generationID int64,
) (bool, error) {
	if generationID <= 0 {
		return false, fmt.Errorf("%w: generation_id %d", ErrCatalogInvalidValue, generationID)
	}
	var marked bool
	err := c.withTx(ctx, func(tx *sql.Tx) error {
		result, err := tx.ExecContext(ctx, `
UPDATE view_generations
SET state = ?
WHERE generation_id = ?
  AND state = ?
  AND NOT EXISTS (
    SELECT 1 FROM ref_views WHERE active_generation_id = ?
  )`,
			string(ViewGenerationSuperseded), generationID,
			string(ViewGenerationReady), generationID,
		)
		if err != nil {
			return err
		}
		changed, err := result.RowsAffected()
		if err != nil {
			return err
		}
		marked = changed == 1
		return nil
	})
	return marked && err == nil, err
}
