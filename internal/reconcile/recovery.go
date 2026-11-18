package reconcile

import "context"

// ResetRecoveredLayers lets a later outage purge the layers being rebuilt now.
// The catalog atomically checks this incarnation's recovery state and only
// deletes a completed purge, never another active cleanup attempt.
func (r *Reconciler) ResetRecoveredLayers(ctx context.Context, checkoutID, incarnation string) error {
	target := sagaTarget{Kind: sagaPurgeLayers, CheckoutID: checkoutID, Incarnation: incarnation}
	return r.catalog.DeleteCompletedCleanupForRecovery(ctx, checkoutID, incarnation, target.cleanupID())
}
