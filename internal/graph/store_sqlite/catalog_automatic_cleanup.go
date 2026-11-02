package store_sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// DeleteAutomaticCheckout is the final automatic-retirement fence. An intent
// or dedicated owner acquired after the pass sampled the checkout wins inside
// the same transaction as deletion; no check-then-delete window loses it.
func (c *Catalog) DeleteAutomaticCheckout(ctx context.Context, checkoutID, incarnation string) error {
	if err := requireCatalogID("checkout_id", checkoutID); err != nil {
		return err
	}
	if err := requireCatalogID("incarnation", incarnation); err != nil {
		return err
	}
	return c.withTx(ctx, func(tx *sql.Tx) error {
		var present int
		if err := tx.QueryRowContext(ctx, `SELECT 1 FROM checkouts WHERE checkout_id=?`, checkoutID).Scan(&present); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("%w: checkout %s", ErrCatalogNotFound, checkoutID)
			}
			return err
		}
		return execGuardedTx(ctx, tx, "automatic checkout "+checkoutID, `DELETE FROM checkouts
WHERE checkout_id=? AND incarnation=?
AND NOT EXISTS(SELECT 1 FROM tracking_intents WHERE checkout_id=? AND active=1)
AND NOT EXISTS(SELECT 1 FROM dedicated_graphs WHERE owner_checkout_id=?)`, checkoutID, incarnation, checkoutID, checkoutID)
	})
}
