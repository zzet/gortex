package store_sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// CommitAuthorizedPromotionRequest is the complete publication guard for one
// dedicated checkout. The base marker, both prepared overlay generations, the
// route, and the effective mode are joined by one catalog transaction.
type CommitAuthorizedPromotionRequest struct {
	CheckoutID         string
	Incarnation        string
	FamilyID           string
	TransitionID       string
	RequiredCause      string
	GraphID            string
	RequiredGraphState string

	BaseGenerationID   int64
	BaseTreeOID         string
	CommitGenerationID int64
	DirtyGenerationID  int64

	RouteExists        bool
	ExpectedRouteEpoch int64
	State               CheckoutState
	LastSeen            int64
}

// CommitAuthorizedPromotion atomically makes a prepared dedicated stack
// visible. Until this transaction commits, both the checkout's old route and
// its effective automatic mode remain unchanged. The transition deliberately
// remains standing so the caller can durably finish external watcher/config
// cleanup before completing it.
func (c *Catalog) CommitAuthorizedPromotion(ctx context.Context, req CommitAuthorizedPromotionRequest) error {
	if err := validateCommitAuthorizedPromotionRequest(req); err != nil {
		return err
	}
	return c.withTx(ctx, func(tx *sql.Tx) error {
		checkout, err := authorizationCheckoutTx(ctx, tx, req.CheckoutID, req.Incarnation)
		if err != nil {
			return err
		}
		if checkout.familyID != req.FamilyID || checkout.activeTransition != req.TransitionID {
			return fmt.Errorf("%w: checkout %s promotion authorization moved", ErrCatalogStaleGuard, req.CheckoutID)
		}
		standing, found, err := intentTransitionTx(ctx, tx, req.CheckoutID)
		if err != nil {
			return err
		}
		if !found || standing.TransitionID != req.TransitionID ||
			standing.Cause != req.RequiredCause ||
			standing.RequestedMode != CheckoutModeDedicated {
			return fmt.Errorf("%w: transition %s on checkout %s", ErrCatalogStaleGuard,
				req.TransitionID, req.CheckoutID)
		}

		graph, err := promotionGraphTx(ctx, tx, req.GraphID)
		if err != nil {
			return err
		}
		if graph.ownerCheckoutID != req.CheckoutID || graph.familyID != req.FamilyID ||
			graph.state != req.RequiredGraphState {
			return fmt.Errorf("%w: graph %s cannot serve checkout %s", ErrCatalogStaleGuard,
				req.GraphID, req.CheckoutID)
		}
		if err := verifyPromotionGenerationsTx(ctx, tx, req); err != nil {
			return err
		}
		route, routed, err := checkoutRouteTx(ctx, tx, req.CheckoutID)
		if err != nil {
			return err
		}

		// Retrying after the transaction committed is deliberately idempotent.
		// External cleanup can therefore crash and resume without republishing
		// the route or advancing its epoch a second time.
		if checkout.effectiveMode == CheckoutModeDedicated {
			if checkout.desiredMode == CheckoutModeDedicated &&
				graph.activeGenerationID == req.BaseGenerationID && routed &&
				route.GraphID == req.GraphID &&
				route.CommitGenerationID == req.CommitGenerationID &&
				route.DirtyGenerationID == req.DirtyGenerationID &&
				route.State == RouteActive {
				return nil
			}
			return fmt.Errorf("%w: checkout %s promotion publication moved",
				ErrCatalogStaleGuard, req.CheckoutID)
		}
		if checkout.state != standing.PriorCheckoutState ||
			checkout.desiredMode != standing.PriorDesiredMode ||
			checkout.effectiveMode != standing.PriorEffectiveMode {
			return fmt.Errorf("%w: checkout %s is not awaiting promotion",
				ErrCatalogStaleGuard, req.CheckoutID)
		}
		if routed != req.RouteExists || (routed && route.RouteEpoch != req.ExpectedRouteEpoch) {
			return fmt.Errorf("%w: checkout %s route moved", ErrCatalogStaleGuard, req.CheckoutID)
		}

		result, err := tx.ExecContext(ctx, `
UPDATE dedicated_graphs
   SET active_generation_id = ?
 WHERE graph_id = ? AND owner_checkout_id = ? AND family_id = ?
   AND state = ? AND COALESCE(active_generation_id, 0) IN (0, ?)`,
			req.BaseGenerationID, req.GraphID, req.CheckoutID, req.FamilyID,
			req.RequiredGraphState, req.BaseGenerationID)
		if err != nil {
			return err
		}
		if changed, changedErr := result.RowsAffected(); changedErr != nil {
			return changedErr
		} else if changed != 1 {
			return fmt.Errorf("%w: graph %s base publication moved", ErrCatalogStaleGuard, req.GraphID)
		}

		if routed {
			result, err = tx.ExecContext(ctx, `
UPDATE checkout_routes
   SET graph_id = ?, commit_generation_id = ?, dirty_generation_id = ?,
       route_epoch = route_epoch + 1, state = ?
 WHERE checkout_id = ? AND route_epoch = ?`,
				req.GraphID, req.CommitGenerationID, req.DirtyGenerationID,
				string(RouteActive), req.CheckoutID, req.ExpectedRouteEpoch)
			if err != nil {
				return err
			}
			if changed, changedErr := result.RowsAffected(); changedErr != nil {
				return changedErr
			} else if changed != 1 {
				return fmt.Errorf("%w: checkout %s route moved", ErrCatalogStaleGuard, req.CheckoutID)
			}
		} else {
			_, err = tx.ExecContext(ctx, `
INSERT INTO checkout_routes
  (checkout_id, graph_id, commit_generation_id, dirty_generation_id, route_epoch, state)
VALUES (?, ?, ?, ?, 0, ?)`, req.CheckoutID, req.GraphID,
				req.CommitGenerationID, req.DirtyGenerationID, string(RouteActive))
			if err != nil {
				if isSQLiteUniqueViolation(err) {
					return fmt.Errorf("%w: checkout %s route appeared", ErrCatalogStaleGuard, req.CheckoutID)
				}
				return err
			}
		}

		result, err = tx.ExecContext(ctx, `
UPDATE checkouts
   SET state = ?, desired_mode = ?, effective_mode = ?, last_seen = ?, last_error = ''
 WHERE checkout_id = ? AND incarnation = ? AND active_intent_transition_id = ?
   AND effective_mode = ?`, string(req.State), string(CheckoutModeDedicated),
			string(CheckoutModeDedicated), req.LastSeen, req.CheckoutID, req.Incarnation,
			req.TransitionID, string(CheckoutModeAutomatic))
		if err != nil {
			return err
		}
		if changed, changedErr := result.RowsAffected(); changedErr != nil {
			return changedErr
		} else if changed != 1 {
			return fmt.Errorf("%w: checkout %s promotion transition %s",
				ErrCatalogStaleGuard, req.CheckoutID, req.TransitionID)
		}
		return nil
	})
}

// OwnsActiveDedicatedRoute reports whether repoPrefix is protected by a
// dedicated checkout publication. The intent arm closes the promotion window:
// once a checkout asks to become dedicated, legacy filesystem mutation stops
// before the immutable HEAD corpus is captured. The active-route arm keeps the
// protection durable after the transition journal is completed and across
// daemon restarts.
func (c *Catalog) OwnsActiveDedicatedRoute(ctx context.Context, graphID, repoPrefix string) (bool, error) {
	if err := requireCatalogID("graph_id", graphID); err != nil {
		return false, err
	}
	if err := requireCatalogID("repo_prefix", repoPrefix); err != nil {
		return false, err
	}
	var owned int
	err := c.store.db.QueryRowContext(ctx, `
SELECT EXISTS (
  SELECT 1
    FROM dedicated_graphs AS graph
    JOIN checkouts AS checkout
      ON checkout.checkout_id = graph.owner_checkout_id
    LEFT JOIN checkout_routes AS route
      ON route.checkout_id = checkout.checkout_id
    LEFT JOIN intent_transitions AS transition
      ON transition.checkout_id = checkout.checkout_id
     AND transition.transition_id = checkout.active_intent_transition_id
   WHERE graph.graph_id = ?
     AND graph.repo_prefix = ?
     AND (
       (transition.requested_mode = ? AND transition.state IN (?, ?, ?))
       OR (
         checkout.effective_mode = ?
         AND COALESCE(graph.active_generation_id, 0) > 0
         AND route.graph_id = graph.graph_id
         AND COALESCE(route.commit_generation_id, 0) > 0
         AND COALESCE(route.dirty_generation_id, 0) > 0
         AND route.state = ?
       )
     )
)`, graphID, repoPrefix, string(CheckoutModeDedicated),
		string(IntentTransitionPending), string(IntentTransitionRunning), string(IntentTransitionFailed),
		string(CheckoutModeDedicated), string(RouteActive)).Scan(&owned)
	if err != nil {
		return false, err
	}
	return owned != 0, nil
}

type promotionGraph struct {
	ownerCheckoutID    string
	familyID           string
	state              string
	activeGenerationID int64
}

func promotionGraphTx(ctx context.Context, tx *sql.Tx, graphID string) (promotionGraph, error) {
	var out promotionGraph
	err := tx.QueryRowContext(ctx, `
SELECT owner_checkout_id, family_id, state, COALESCE(active_generation_id, 0)
  FROM dedicated_graphs WHERE graph_id = ?`, graphID).Scan(
		&out.ownerCheckoutID, &out.familyID, &out.state, &out.activeGenerationID)
	if errors.Is(err, sql.ErrNoRows) {
		return promotionGraph{}, fmt.Errorf("%w: dedicated graph %s", ErrCatalogStaleGuard, graphID)
	}
	return out, err
}

func checkoutRouteTx(ctx context.Context, tx *sql.Tx, checkoutID string) (CheckoutRoute, bool, error) {
	var route CheckoutRoute
	var state string
	err := tx.QueryRowContext(ctx, `
SELECT graph_id, COALESCE(commit_generation_id, 0), COALESCE(dirty_generation_id, 0),
       route_epoch, state
  FROM checkout_routes WHERE checkout_id = ?`, checkoutID).Scan(
		&route.GraphID, &route.CommitGenerationID, &route.DirtyGenerationID,
		&route.RouteEpoch, &state)
	if errors.Is(err, sql.ErrNoRows) {
		return CheckoutRoute{}, false, nil
	}
	if err != nil {
		return CheckoutRoute{}, false, err
	}
	route.CheckoutID = checkoutID
	route.State = RouteState(state)
	return route, true, nil
}

type promotionGeneration struct {
	graphID          string
	checkoutID       string
	baseGenerationID int64
	treeOID          string
	state            ViewGenerationState
}

func promotionGenerationTx(ctx context.Context, tx *sql.Tx, generationID int64) (promotionGeneration, error) {
	var out promotionGeneration
	var state string
	err := tx.QueryRowContext(ctx, `
SELECT COALESCE(graph_id, ''), COALESCE(checkout_id, ''),
       COALESCE(base_generation_id, 0), COALESCE(tree_oid, ''), state
  FROM view_generations WHERE generation_id = ?`, generationID).Scan(
		&out.graphID, &out.checkoutID, &out.baseGenerationID, &out.treeOID, &state)
	if errors.Is(err, sql.ErrNoRows) {
		return promotionGeneration{}, fmt.Errorf("%w: view generation %d", ErrCatalogStaleGuard, generationID)
	}
	if err != nil {
		return promotionGeneration{}, err
	}
	out.state = ViewGenerationState(state)
	return out, nil
}

func verifyPromotionGenerationsTx(ctx context.Context, tx *sql.Tx, req CommitAuthorizedPromotionRequest) error {
	base, err := promotionGenerationTx(ctx, tx, req.BaseGenerationID)
	if err != nil {
		return err
	}
	commit, err := promotionGenerationTx(ctx, tx, req.CommitGenerationID)
	if err != nil {
		return err
	}
	dirty, err := promotionGenerationTx(ctx, tx, req.DirtyGenerationID)
	if err != nil {
		return err
	}
	if base.graphID != req.GraphID || base.checkoutID != req.CheckoutID ||
		base.baseGenerationID != 0 || base.treeOID != req.BaseTreeOID ||
		base.state != ViewGenerationReady {
		return fmt.Errorf("%w: dedicated base generation %d moved",
			ErrCatalogStaleGuard, req.BaseGenerationID)
	}
	if commit.graphID != req.GraphID || commit.checkoutID != req.CheckoutID ||
		commit.baseGenerationID != req.BaseGenerationID || commit.treeOID != req.BaseTreeOID ||
		commit.state != ViewGenerationReady {
		return fmt.Errorf("%w: dedicated commit generation %d moved",
			ErrCatalogStaleGuard, req.CommitGenerationID)
	}
	if dirty.graphID != req.GraphID || dirty.checkoutID != req.CheckoutID ||
		dirty.baseGenerationID != req.CommitGenerationID || dirty.state != ViewGenerationReady {
		return fmt.Errorf("%w: dedicated dirty generation %d moved",
			ErrCatalogStaleGuard, req.DirtyGenerationID)
	}
	return nil
}

func validateCommitAuthorizedPromotionRequest(req CommitAuthorizedPromotionRequest) error {
	for name, value := range map[string]string{
		"checkout_id":          req.CheckoutID,
		"incarnation":          req.Incarnation,
		"family_id":            req.FamilyID,
		"transition_id":        req.TransitionID,
		"required_cause":       req.RequiredCause,
		"graph_id":             req.GraphID,
		"required_graph_state": req.RequiredGraphState,
		"base_tree_oid":        req.BaseTreeOID,
	} {
		if err := requireCatalogID(name, value); err != nil {
			return err
		}
	}
	if req.BaseGenerationID <= 0 || req.CommitGenerationID <= 0 || req.DirtyGenerationID <= 0 {
		return fmt.Errorf("%w: promotion generations must be positive", ErrCatalogInvalidValue)
	}
	if req.ExpectedRouteEpoch < 0 || (!req.RouteExists && req.ExpectedRouteEpoch != 0) {
		return fmt.Errorf("%w: invalid expected route epoch %d", ErrCatalogInvalidValue,
			req.ExpectedRouteEpoch)
	}
	return requireCatalogValue("state", req.State, checkoutStates)
}
