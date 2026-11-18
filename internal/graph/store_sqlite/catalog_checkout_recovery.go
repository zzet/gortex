package store_sqlite

import (
	"context"
	"fmt"
)

// UpdateCheckoutRecoveryState changes only serving state, under the same
// incarnation and state that the recovery read. It never writes mode columns
// or races a newly authorized mode transition back to the old mode.
func (c *Catalog) UpdateCheckoutRecoveryState(ctx context.Context, checkoutID, incarnation string, expected, next CheckoutState, detail string, now int64) error {
	if err := requireCatalogValue("state", next, checkoutStates); err != nil {
		return err
	}
	return c.execGuarded(ctx, fmt.Sprintf("checkout %s recovery", checkoutID), `
UPDATE checkouts SET state=?,last_error=?,last_seen=MAX(last_seen,?)
WHERE checkout_id=? AND incarnation=? AND state=? AND active_intent_transition_id IS NULL`, next, detail, now, checkoutID, incarnation, expected)
}

// DeleteCompletedCleanupForRecovery only clears an old layer-purge marker
// while the same incarnation is still recovering. A newer outage's purge or
// mode transition cannot be erased by a stale recovery.
func (c *Catalog) DeleteCompletedCleanupForRecovery(ctx context.Context, checkoutID, incarnation, cleanupID string) error {
	_, err := c.exec(ctx, `DELETE FROM cleanup_journal WHERE cleanup_id=? AND phase=?
 AND EXISTS(SELECT 1 FROM checkouts WHERE checkout_id=? AND incarnation=? AND state=? AND active_intent_transition_id IS NULL)`, cleanupID, CleanupPhaseDone, checkoutID, incarnation, CheckoutStateReconciling)
	return err
}
