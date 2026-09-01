package store_sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// BindReadyGenerationLeaseToCheckoutRequest atomically installs a claimed
// canonical commit generation in an existing checkout route and consumes the
// handoff lease. The dirty slot is cleared because a commit transition changes
// the lower layer it was built against.
type BindReadyGenerationLeaseToCheckoutRequest struct {
	Key                ReadyGenerationCacheKey
	LeaseToken         string
	CheckoutID         string
	ExpectedRouteEpoch int64
	GenerationID       int64
	State              RouteState
	// RequireActiveGraphBase rejects publication when the dedicated graph no
	// longer names Key.BaseGenerationID. Zero preserves catalog-only callers
	// that intentionally have no dedicated-graph row.
	RequireActiveGraphBase bool
}

// BindReadyGenerationLeaseToCheckout validates a ready-generation cache lease
// and advances the checkout route in one transaction. A stale route, expired
// lease, moved cache winner, or no-longer-live generation changes nothing and
// reports ErrCatalogStaleGuard.
func (c *Catalog) BindReadyGenerationLeaseToCheckout(
	ctx context.Context,
	req BindReadyGenerationLeaseToCheckoutRequest,
) error {
	if err := validateReadyGenerationCacheKey(req.Key); err != nil {
		return err
	}
	if req.LeaseToken == "" {
		return fmt.Errorf("ready generation lease token must not be empty")
	}
	if err := requireCatalogID("checkout_id", req.CheckoutID); err != nil {
		return err
	}
	if req.GenerationID <= 0 {
		return fmt.Errorf("generation id must be positive")
	}
	if err := requireCatalogValue("state", req.State, routeStates); err != nil {
		return err
	}

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
		return fmt.Errorf("read ready generation lease clock: %w", err)
	}
	var leasedGeneration int64
	var leasedGraph string
	var expiresAt int64
	err = tx.QueryRowContext(ctx, `
		SELECT generation_id, graph_id, expires_at
		FROM ready_generation_leases
		WHERE lease_token = ?
	`, req.LeaseToken).Scan(&leasedGeneration, &leasedGraph, &expiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: ready generation lease is missing", ErrCatalogStaleGuard)
	}
	if err != nil {
		return err
	}
	if leasedGeneration != req.GenerationID || leasedGraph != req.Key.GraphID || expiresAt <= now {
		return fmt.Errorf("%w: ready generation lease is stale", ErrCatalogStaleGuard)
	}
	compatible, err := candidateMatchesReadyGenerationKey(ctx, tx, req.GenerationID, req.Key, []string{readyGenerationSourceSnapshotCapability})
	if err != nil {
		return err
	}
	if !compatible {
		return fmt.Errorf("%w: ready generation is no longer compatible", ErrCatalogStaleGuard)
	}
	if req.RequireActiveGraphBase {
		active, err := graphBaseGenerationIsActiveTx(ctx, tx, req.Key.GraphID, req.Key.BaseGenerationID)
		if err != nil {
			return err
		}
		if !active {
			return fmt.Errorf("%w: dedicated graph %s base moved", ErrCatalogStaleGuard, req.Key.GraphID)
		}
	}
	previousRoute, routed, err := checkoutRouteTx(ctx, tx, req.CheckoutID)
	if err != nil {
		return err
	}
	if !routed || previousRoute.RouteEpoch != req.ExpectedRouteEpoch ||
		previousRoute.GraphID != req.Key.GraphID {
		return fmt.Errorf("%w: checkout route moved", ErrCatalogStaleGuard)
	}

	result, err := tx.ExecContext(ctx, `
		UPDATE checkout_routes
		SET commit_generation_id = ?,
		    dirty_generation_id = NULL,
		    state = ?,
		    route_epoch = route_epoch + 1
		WHERE checkout_id = ?
		  AND route_epoch = ?
		  AND graph_id = ?
	`, req.GenerationID, string(req.State), req.CheckoutID, req.ExpectedRouteEpoch, req.Key.GraphID)
	if err != nil {
		return fmt.Errorf("bind ready generation to checkout route: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rows != 1 {
		return fmt.Errorf("%w: checkout route moved", ErrCatalogStaleGuard)
	}
	// Both sides of a successful A -> B switch become durable cache entries.
	// Restamping A here starts its inactivity window at switch-away, while B's
	// pin bridges daemon restart even before another switch occurs. These writes
	// share the route CAS transaction, so a stale publication touches neither.
	if err := upsertCheckoutCommitCachePinTx(ctx, tx, req.CheckoutID, req.Key.GraphID,
		previousRoute.CommitGenerationID, now); err != nil {
		return fmt.Errorf("retain previous checkout commit generation: %w", err)
	}
	if err := upsertCheckoutCommitCachePinTx(ctx, tx, req.CheckoutID, req.Key.GraphID,
		req.GenerationID, now); err != nil {
		return fmt.Errorf("retain selected checkout commit generation: %w", err)
	}
	result, err = tx.ExecContext(ctx, `
		DELETE FROM ready_generation_leases
		WHERE lease_token = ? AND generation_id = ?
	`, req.LeaseToken, req.GenerationID)
	if err != nil {
		return fmt.Errorf("consume ready generation lease: %w", err)
	}
	rows, err = result.RowsAffected()
	if err != nil {
		return err
	}
	if rows != 1 {
		return fmt.Errorf("%w: ready generation lease moved", ErrCatalogStaleGuard)
	}
	return tx.Commit()
}
