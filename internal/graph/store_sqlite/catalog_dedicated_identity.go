package store_sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// upsertDedicatedGraphIdentity maintains graph identity without acquiring
// ownership of a publication-managed active generation pointer.
//
// Before publication authority exists, preserve legacy upsert behavior. Once
// authority exists, this operation owns identity/state only: a supplied zero or
// conflicting positive ActiveGenerationID is deliberately ignored, not adopted.
// Existing owner, namespace and family cannot be rebound through an upsert.
func (c *Catalog) upsertDedicatedGraphIdentity(ctx context.Context, dedicated DedicatedGraph) error {
	if err := dedicated.validate(); err != nil {
		return err
	}
	return c.withTx(ctx, func(tx *sql.Tx) error {
		existing := DedicatedGraph{GraphID: dedicated.GraphID}
		var owner sql.NullString
		var active sql.NullInt64
		var primary int
		err := tx.QueryRowContext(ctx, `SELECT owner_checkout_id, repo_prefix, family_id,
			is_primary_base, active_generation_id, state FROM dedicated_graphs WHERE graph_id=?`,
			dedicated.GraphID).Scan(&owner, &existing.RepoPrefix, &existing.FamilyID,
			&primary, &active, &existing.State)
		if errors.Is(err, sql.ErrNoRows) {
			_, err = tx.ExecContext(ctx, `INSERT INTO dedicated_graphs
				(graph_id, owner_checkout_id, repo_prefix, family_id, is_primary_base, active_generation_id, state)
				VALUES (?, ?, ?, ?, ?, ?, ?)`, dedicated.GraphID, catalogNullString(dedicated.OwnerCheckoutID),
				dedicated.RepoPrefix, dedicated.FamilyID, catalogBoolInt(dedicated.IsPrimaryBase),
				catalogNullInt(dedicated.ActiveGenerationID), dedicated.State)
			if err != nil {
				return err
			}
			return validateViewGenerationAdmissionTx(ctx, tx, dedicated.ActiveGenerationID)
		}
		if err != nil {
			return err
		}
		existing.OwnerCheckoutID, existing.ActiveGenerationID = owner.String, active.Int64
		existing.IsPrimaryBase = primary != 0

		var guarded bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS (
			SELECT 1 FROM dedicated_base_publications WHERE graph_id=?)`, dedicated.GraphID).Scan(&guarded); err != nil {
			return err
		}
		if guarded {
			if dedicated.OwnerCheckoutID != existing.OwnerCheckoutID || dedicated.RepoPrefix != existing.RepoPrefix || dedicated.FamilyID != existing.FamilyID {
				return fmt.Errorf("%w: dedicated graph %s publication owns its checkout, namespace and family", ErrCatalogStaleGuard, dedicated.GraphID)
			}
			dedicated.ActiveGenerationID = existing.ActiveGenerationID
		}
		// Normalize the active pointer before comparing, so ordinary binding
		// refreshes with active=0 are genuinely read-only after publication.
		if dedicated == existing {
			return nil
		}
		if err := execGuardedTx(ctx, tx, "dedicated graph identity", `UPDATE dedicated_graphs SET
			owner_checkout_id=?, repo_prefix=?, family_id=?, is_primary_base=?, active_generation_id=?, state=?
			WHERE graph_id=?`, catalogNullString(dedicated.OwnerCheckoutID), dedicated.RepoPrefix,
			dedicated.FamilyID, catalogBoolInt(dedicated.IsPrimaryBase), catalogNullInt(dedicated.ActiveGenerationID),
			dedicated.State, dedicated.GraphID); err != nil {
			return err
		}
		if dedicated.ActiveGenerationID != existing.ActiveGenerationID {
			return validateViewGenerationAdmissionTx(ctx, tx, dedicated.ActiveGenerationID)
		}
		return nil
	})
}
