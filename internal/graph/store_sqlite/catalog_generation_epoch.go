package store_sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// CommitCheckoutStackRequest atomically publishes both sparse slots of one
// checkout, but only while the dedicated graph still names the base generation
// those slots were built over. It closes the read-base/build/publish race with
// a concurrent dedicated-base replacement.
type CommitCheckoutStackRequest struct {
	CheckoutID               string
	GraphID                  string
	ExpectedBaseGenerationID int64
	CommitGenerationID       int64
	DirtyGenerationID        int64
	RouteExists              bool
	ExpectedRouteEpoch       int64
	State                    RouteState
}

// InstallCheckoutRouteForBaseRequest installs the empty pending route a
// coordinator uses before its first sparse build. ExpectedBaseGenerationID is
// the base epoch the coordinator observed before deciding the route was absent.
type InstallCheckoutRouteForBaseRequest struct {
	CheckoutID               string
	GraphID                  string
	ExpectedBaseGenerationID int64
}

// InstallCheckoutRouteForBase creates an absent pending route only while its
// dedicated graph is ready and still names the captured base. A concurrent
// refresh therefore cannot be crossed by an old coordinator which observed no
// route before refresh admission.
func (c *Catalog) InstallCheckoutRouteForBase(
	ctx context.Context,
	req InstallCheckoutRouteForBaseRequest,
) error {
	if err := requireCatalogID("checkout_id", req.CheckoutID); err != nil {
		return err
	}
	if err := requireCatalogID("graph_id", req.GraphID); err != nil {
		return err
	}
	if req.ExpectedBaseGenerationID <= 0 {
		return fmt.Errorf("expected base generation id must be positive")
	}

	return c.withTx(ctx, func(tx *sql.Tx) error {
		if err := checkoutRouteGraphAuthorizedTx(ctx, tx, req.CheckoutID, req.GraphID); err != nil {
			return err
		}
		active, err := graphBaseGenerationIsActiveTx(
			ctx, tx, req.GraphID, req.ExpectedBaseGenerationID)
		if err != nil {
			return err
		}
		if !active {
			return fmt.Errorf("%w: dedicated graph %s base moved", ErrCatalogStaleGuard, req.GraphID)
		}
		_, err = tx.ExecContext(ctx, `
INSERT INTO checkout_routes
  (checkout_id, graph_id, commit_generation_id, dirty_generation_id, route_epoch, state)
VALUES (?, ?, NULL, NULL, 0, ?)`, req.CheckoutID, req.GraphID, string(RoutePending))
		if isSQLiteUniqueViolation(err) {
			return fmt.Errorf("%w: checkout %s route appeared", ErrCatalogStaleGuard, req.CheckoutID)
		}
		return err
	})
}

// CommitCheckoutStack installs a complete base -> commit -> dirty chain under
// one route epoch. No catalog state changes when the base, generations, or
// route snapshot moved.
func (c *Catalog) CommitCheckoutStack(ctx context.Context, req CommitCheckoutStackRequest) error {
	if err := requireCatalogID("checkout_id", req.CheckoutID); err != nil {
		return err
	}
	if err := requireCatalogID("graph_id", req.GraphID); err != nil {
		return err
	}
	if req.ExpectedBaseGenerationID <= 0 || req.CommitGenerationID <= 0 || req.DirtyGenerationID <= 0 {
		return fmt.Errorf("checkout stack generation ids must be positive")
	}
	if req.ExpectedRouteEpoch < 0 {
		return fmt.Errorf("expected route epoch must not be negative")
	}
	if err := requireCatalogValue("state", req.State, routeStates); err != nil {
		return err
	}

	return c.withTx(ctx, func(tx *sql.Tx) error {
		if err := checkoutRouteGraphAuthorizedTx(ctx, tx, req.CheckoutID, req.GraphID); err != nil {
			return err
		}
		active, err := graphBaseGenerationIsActiveTx(ctx, tx, req.GraphID, req.ExpectedBaseGenerationID)
		if err != nil {
			return err
		}
		if !active {
			return fmt.Errorf("%w: dedicated graph %s base moved", ErrCatalogStaleGuard, req.GraphID)
		}

		commit, err := promotionGenerationTx(ctx, tx, req.CommitGenerationID)
		if err != nil {
			return err
		}
		dirty, err := promotionGenerationTx(ctx, tx, req.DirtyGenerationID)
		if err != nil {
			return err
		}
		if commit.graphID != req.GraphID ||
			commit.baseGenerationID != req.ExpectedBaseGenerationID ||
			commit.state != ViewGenerationReady {
			return fmt.Errorf("%w: checkout commit generation %d moved",
				ErrCatalogStaleGuard, req.CommitGenerationID)
		}
		if dirty.graphID != req.GraphID || dirty.checkoutID != req.CheckoutID ||
			dirty.baseGenerationID != req.CommitGenerationID ||
			dirty.state != ViewGenerationReady {
			return fmt.Errorf("%w: checkout dirty generation %d moved",
				ErrCatalogStaleGuard, req.DirtyGenerationID)
		}

		route, routed, err := checkoutRouteTx(ctx, tx, req.CheckoutID)
		if err != nil {
			return err
		}
		if routed != req.RouteExists || (routed && route.RouteEpoch != req.ExpectedRouteEpoch) {
			return fmt.Errorf("%w: checkout %s route moved", ErrCatalogStaleGuard, req.CheckoutID)
		}
		if routed {
			result, err := tx.ExecContext(ctx, `
UPDATE checkout_routes
   SET graph_id = ?, commit_generation_id = ?, dirty_generation_id = ?,
       route_epoch = route_epoch + 1, state = ?
 WHERE checkout_id = ? AND route_epoch = ?`,
				req.GraphID, req.CommitGenerationID, req.DirtyGenerationID,
				string(req.State), req.CheckoutID, req.ExpectedRouteEpoch)
			if err != nil {
				return err
			}
			changed, err := result.RowsAffected()
			if err != nil {
				return err
			}
			if changed != 1 {
				return fmt.Errorf("%w: checkout %s route moved", ErrCatalogStaleGuard, req.CheckoutID)
			}
			return nil
		}

		_, err = tx.ExecContext(ctx, `
INSERT INTO checkout_routes
  (checkout_id, graph_id, commit_generation_id, dirty_generation_id, route_epoch, state)
VALUES (?, ?, ?, ?, 0, ?)`, req.CheckoutID, req.GraphID,
			req.CommitGenerationID, req.DirtyGenerationID, string(req.State))
		if isSQLiteUniqueViolation(err) {
			return fmt.Errorf("%w: checkout %s route appeared", ErrCatalogStaleGuard, req.CheckoutID)
		}
		return err
	})
}

// checkoutRouteGraphAuthorizedTx prevents a coordinator left behind by a mode
// transition from moving an automatic checkout back onto its retired private
// graph. Dedicated checkouts may still route to their own graph; automatic
// checkouts may only name the family's designated primary.
func checkoutRouteGraphAuthorizedTx(
	ctx context.Context,
	tx *sql.Tx,
	checkoutID, graphID string,
) error {
	var effectiveMode, familyID string
	err := tx.QueryRowContext(ctx, `
SELECT effective_mode, family_id FROM checkouts WHERE checkout_id = ?`, checkoutID).Scan(
		&effectiveMode, &familyID,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: checkout %s cannot be routed", ErrCatalogStaleGuard, checkoutID)
	}
	if err != nil {
		return err
	}
	if CheckoutMode(effectiveMode) != CheckoutModeAutomatic {
		return nil
	}

	var primaryGraphID string
	err = tx.QueryRowContext(ctx, `
SELECT graph_id
  FROM dedicated_graphs
	 WHERE family_id = ? AND is_primary_base = 1`, familyID).Scan(&primaryGraphID)
	if errors.Is(err, sql.ErrNoRows) {
		// Legacy/unscoped catalog fixtures can predate family-primary rows. Once
		// a family has a designation, every automatic writer below is fenced to
		// it; without one there is no primary identity to compare against.
		return nil
	}
	if err != nil {
		return err
	}
	if graphID != primaryGraphID {
		return fmt.Errorf("%w: automatic checkout %s cannot route to non-primary graph %s",
			ErrCatalogStaleGuard, checkoutID, graphID)
	}
	return nil
}

func graphBaseGenerationIsActiveTx(
	ctx context.Context, tx *sql.Tx, graphID string, generationID int64,
) (bool, error) {
	var active int
	err := tx.QueryRowContext(ctx, `
SELECT 1
  FROM dedicated_graphs
 WHERE graph_id = ? AND active_generation_id = ? AND state = ?`,
		graphID, generationID, DedicatedGraphStateReady).Scan(&active)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil && active == 1, err
}

func checkoutRouteBaseGenerationIsActiveTx(
	ctx context.Context, tx *sql.Tx, checkoutID string, generationID int64,
) (bool, error) {
	var active int
	err := tx.QueryRowContext(ctx, `
SELECT 1
  FROM checkout_routes AS route
  JOIN dedicated_graphs AS graph ON graph.graph_id = route.graph_id
 WHERE route.checkout_id = ?
   AND graph.active_generation_id = ?
   AND graph.state = ?`, checkoutID, generationID, DedicatedGraphStateReady).Scan(&active)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil && active == 1, err
}
