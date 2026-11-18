package store_sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// RebaseViewGenerationRequest moves one generation from one base to another.
type RebaseViewGenerationRequest struct {
	GenerationID int64
	FromBase     int64
	ToBase       int64
}

// RebaseViewGeneration re-parents a layer onto a content-equal base: the fold
// of the chain it was written over. One guarded transaction:
//
//   - the layer is ready or superseded and its base is FromBase;
//   - ToBase is ready (not building, retiring or failed) and does not have
//     the layer in its own ancestry (no cycle);
//   - base_generation_id := ToBase; nothing else changes. The layer's masks
//     and lower_view_fingerprint stay: they are relative to a base the caller
//     has verified content-equal to the one they were written over.
//
// The "based" reference moves with the column (ViewGenerationReferences reads
// it live), so FromBase stops being referenced by this layer. The chain walk
// reads the base from the catalog at every materialization, so the next view
// composes the layer over ToBase while existing leases keep their stacks.
// A guard miss is ErrCatalogStaleGuard.
func (c *Catalog) RebaseViewGeneration(ctx context.Context, req RebaseViewGenerationRequest) error {
	if req.GenerationID <= 0 || req.FromBase <= 0 || req.ToBase <= 0 {
		return fmt.Errorf("%w: rebase %d from %d to %d", ErrCatalogInvalidValue, req.GenerationID, req.FromBase, req.ToBase)
	}
	if req.FromBase == req.ToBase || req.GenerationID == req.ToBase || req.GenerationID == req.FromBase {
		return fmt.Errorf("%w: rebase %d from %d to %d", ErrCatalogInvalidValue, req.GenerationID, req.FromBase, req.ToBase)
	}
	return c.withTx(ctx, func(tx *sql.Tx) error {
		var state string
		err := tx.QueryRowContext(ctx, `SELECT state FROM view_generations WHERE generation_id = ?`, req.ToBase).Scan(&state)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: base %d is missing", ErrCatalogStaleGuard, req.ToBase)
		}
		if err != nil {
			return err
		}
		if ViewGenerationState(state) != ViewGenerationReady {
			return fmt.Errorf("%w: base %d is %s", ErrCatalogStaleGuard, req.ToBase, state)
		}
		// No cycle: the new base's ancestry must not reach the layer.
		seen := map[int64]struct{}{}
		for id := req.ToBase; id > 0; {
			if id == req.GenerationID {
				return fmt.Errorf("%w: base %d descends from %d", ErrCatalogInvalidValue, req.ToBase, req.GenerationID)
			}
			if _, looped := seen[id]; looped || len(seen) >= maxDedicatedBaseAncestry {
				return fmt.Errorf("%w: the ancestry of %d does not end", ErrCatalogInvalidValue, req.ToBase)
			}
			seen[id] = struct{}{}
			var base int64
			if err := tx.QueryRowContext(ctx,
				`SELECT COALESCE(base_generation_id, 0) FROM view_generations WHERE generation_id = ?`, id).Scan(&base); err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					break
				}
				return err
			}
			id = base
		}
		result, err := tx.ExecContext(ctx, `
UPDATE view_generations SET base_generation_id = ?
 WHERE generation_id = ? AND IFNULL(base_generation_id, 0) = ? AND state IN (?, ?)`,
			req.ToBase, req.GenerationID, req.FromBase, string(ViewGenerationReady), string(ViewGenerationSuperseded))
		if err != nil {
			return err
		}
		changed, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if changed != 1 {
			return fmt.Errorf("%w: generation %d is not a ready layer over %d", ErrCatalogStaleGuard, req.GenerationID, req.FromBase)
		}
		return nil
	})
}
