package store_sqlite

import (
	"context"
	"database/sql"
	"fmt"
)

// CommitDedicatedBaseRefreshRequest is the complete publication guard for an
// in-place rebuild of one dedicated graph's immutable base. The replacement
// base and owner stack are already ready but invisible when this transaction
// begins.
type CommitDedicatedBaseRefreshRequest struct {
	CheckoutID         string
	Incarnation        string
	FamilyID           string
	GraphID            string
	RequiredGraphState string

	ExpectedBaseGenerationID int64
	NewBaseGenerationID      int64
	BaseTreeOID              string
	ConfigHash               string
	ExtractorVersions        string
	ResolverVersion          string
	CommitGenerationID       int64
	DirtyGenerationID        int64
	CommitTreeOID            string

	RouteExists        bool
	ExpectedRouteEpoch int64
	LastSeen           int64
}

// CommitDedicatedBaseRefreshResult names every route invalidated by the base
// epoch change and every formerly-routed generation the caller should offer
// for lease-aware retirement.
type CommitDedicatedBaseRefreshResult struct {
	InvalidatedCheckoutIDs []string
	InvalidatedRefViewIDs  []string
	RetiredGenerationIDs   []int64
}

// CommitDedicatedBaseRefresh atomically advances a full dedicated corpus,
// publishes its owner's coherent stack, and makes every dependent route
// visibly building. Old routes keep serving during the off-route build; after
// this commit no sparse generation built over the previous base can be served.
func (c *Catalog) CommitDedicatedBaseRefresh(
	ctx context.Context,
	req CommitDedicatedBaseRefreshRequest,
) (CommitDedicatedBaseRefreshResult, error) {
	var out CommitDedicatedBaseRefreshResult
	if err := validateCommitDedicatedBaseRefreshRequest(req); err != nil {
		return out, err
	}
	err := c.withTx(ctx, func(tx *sql.Tx) error {
		checkout, err := authorizationCheckoutTx(ctx, tx, req.CheckoutID, req.Incarnation)
		if err != nil {
			return err
		}
		if checkout.familyID != req.FamilyID || checkout.state != CheckoutStateReady ||
			checkout.desiredMode != CheckoutModeDedicated ||
			checkout.effectiveMode != CheckoutModeDedicated ||
			checkout.activeTransition != "" {
			return fmt.Errorf("%w: checkout %s cannot refresh its dedicated base",
				ErrCatalogStaleGuard, req.CheckoutID)
		}

		graph, err := promotionGraphTx(ctx, tx, req.GraphID)
		if err != nil {
			return err
		}
		if graph.ownerCheckoutID != req.CheckoutID || graph.familyID != req.FamilyID ||
			graph.state != req.RequiredGraphState ||
			graph.activeGenerationID != req.ExpectedBaseGenerationID {
			return fmt.Errorf("%w: dedicated graph %s base moved",
				ErrCatalogStaleGuard, req.GraphID)
		}
		if err := verifyDedicatedBaseRefreshGenerationsTx(ctx, tx, req); err != nil {
			return err
		}

		route, routed, err := checkoutRouteTx(ctx, tx, req.CheckoutID)
		if err != nil {
			return err
		}
		if routed != req.RouteExists || (routed && route.RouteEpoch != req.ExpectedRouteEpoch) {
			return fmt.Errorf("%w: checkout %s route moved", ErrCatalogStaleGuard, req.CheckoutID)
		}

		rows, err := tx.QueryContext(ctx, `
SELECT checkout_id, COALESCE(commit_generation_id, 0), COALESCE(dirty_generation_id, 0)
  FROM checkout_routes
 WHERE graph_id = ? AND checkout_id <> ?`, req.GraphID, req.CheckoutID)
		if err != nil {
			return err
		}
		for rows.Next() {
			var checkoutID string
			var commitGenerationID, dirtyGenerationID int64
			if err := rows.Scan(&checkoutID, &commitGenerationID, &dirtyGenerationID); err != nil {
				_ = rows.Close()
				return err
			}
			out.InvalidatedCheckoutIDs = append(out.InvalidatedCheckoutIDs, checkoutID)
			appendRetiredGeneration(&out.RetiredGenerationIDs, dirtyGenerationID)
			appendRetiredGeneration(&out.RetiredGenerationIDs, commitGenerationID)
		}
		if err := rows.Close(); err != nil {
			return err
		}
		if err := rows.Err(); err != nil {
			return err
		}

		rows, err = tx.QueryContext(ctx, `
SELECT ref_view_id, COALESCE(active_generation_id, 0)
  FROM ref_views
 WHERE graph_id = ?`, req.GraphID)
		if err != nil {
			return err
		}
		for rows.Next() {
			var refViewID string
			var activeGenerationID int64
			if err := rows.Scan(&refViewID, &activeGenerationID); err != nil {
				_ = rows.Close()
				return err
			}
			out.InvalidatedRefViewIDs = append(out.InvalidatedRefViewIDs, refViewID)
			appendRetiredGeneration(&out.RetiredGenerationIDs, activeGenerationID)
		}
		if err := rows.Close(); err != nil {
			return err
		}
		if err := rows.Err(); err != nil {
			return err
		}

		rows, err = tx.QueryContext(ctx, `
SELECT COALESCE(b.generation_id, 0)
  FROM ref_view_builds AS b
  JOIN ref_views AS v ON v.ref_view_id = b.ref_view_id
 WHERE v.graph_id = ? AND b.base_generation_id = ?`,
			req.GraphID, req.ExpectedBaseGenerationID)
		if err != nil {
			return err
		}
		for rows.Next() {
			var generationID int64
			if err := rows.Scan(&generationID); err != nil {
				_ = rows.Close()
				return err
			}
			appendRetiredGeneration(&out.RetiredGenerationIDs, generationID)
		}
		if err := rows.Close(); err != nil {
			return err
		}
		if err := rows.Err(); err != nil {
			return err
		}
		rows, err = tx.QueryContext(ctx, `
SELECT generation_id
  FROM view_generations
 WHERE graph_id = ? AND base_generation_id = ? AND owner_kind = 'ref_view'`,
			req.GraphID, req.ExpectedBaseGenerationID)
		if err != nil {
			return err
		}
		for rows.Next() {
			var generationID int64
			if err := rows.Scan(&generationID); err != nil {
				_ = rows.Close()
				return err
			}
			appendRetiredGeneration(&out.RetiredGenerationIDs, generationID)
		}
		if err := rows.Close(); err != nil {
			return err
		}
		if err := rows.Err(); err != nil {
			return err
		}
		// Every cached commit in this graph composes over the base epoch being
		// replaced. Revoke those cache owners in the same transaction that makes
		// the new epoch authoritative; none may be migrated by generation id.
		if err := deleteCheckoutCommitCachePinsForGraphTx(ctx, tx, req.GraphID); err != nil {
			return err
		}

		result, err := tx.ExecContext(ctx, `
UPDATE dedicated_graphs
   SET active_generation_id = ?, state = ?
 WHERE graph_id = ? AND owner_checkout_id = ? AND family_id = ?
   AND state = ? AND active_generation_id = ?`,
			req.NewBaseGenerationID, DedicatedGraphStateReady,
			req.GraphID, req.CheckoutID, req.FamilyID,
			req.RequiredGraphState, req.ExpectedBaseGenerationID)
		if err != nil {
			return err
		}
		if changed, err := result.RowsAffected(); err != nil {
			return err
		} else if changed != 1 {
			return fmt.Errorf("%w: dedicated graph %s base moved", ErrCatalogStaleGuard, req.GraphID)
		}

		if routed {
			appendRetiredGeneration(&out.RetiredGenerationIDs, route.DirtyGenerationID)
			appendRetiredGeneration(&out.RetiredGenerationIDs, route.CommitGenerationID)
			result, err = tx.ExecContext(ctx, `
UPDATE checkout_routes
   SET graph_id = ?, commit_generation_id = ?, dirty_generation_id = ?,
       route_epoch = route_epoch + 1, state = ?
 WHERE checkout_id = ? AND route_epoch = ?`, req.GraphID,
				req.CommitGenerationID, req.DirtyGenerationID, string(RouteActive),
				req.CheckoutID, req.ExpectedRouteEpoch)
			if err != nil {
				return err
			}
			if changed, err := result.RowsAffected(); err != nil {
				return err
			} else if changed != 1 {
				return fmt.Errorf("%w: checkout %s route moved", ErrCatalogStaleGuard, req.CheckoutID)
			}
		} else {
			_, err = tx.ExecContext(ctx, `
INSERT INTO checkout_routes
  (checkout_id, graph_id, commit_generation_id, dirty_generation_id, route_epoch, state)
VALUES (?, ?, ?, ?, 0, ?)`, req.CheckoutID, req.GraphID,
				req.CommitGenerationID, req.DirtyGenerationID, string(RouteActive))
			if isSQLiteUniqueViolation(err) {
				return fmt.Errorf("%w: checkout %s route appeared", ErrCatalogStaleGuard, req.CheckoutID)
			}
			if err != nil {
				return err
			}
		}
		if err := upsertCheckoutCommitCachePinTx(ctx, tx, req.CheckoutID, req.GraphID,
			req.CommitGenerationID, req.LastSeen); err != nil {
			return err
		}

		if _, err := tx.ExecContext(ctx, `
UPDATE checkout_routes
   SET commit_generation_id = NULL, dirty_generation_id = NULL,
       route_epoch = route_epoch + 1, state = ?
 WHERE graph_id = ? AND checkout_id <> ?`,
			string(RoutePending), req.GraphID, req.CheckoutID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
UPDATE ref_view_builds
   SET state = ?, last_progress = ?, error = ?
 WHERE ref_view_id IN (SELECT ref_view_id FROM ref_views WHERE graph_id = ?)
   AND base_generation_id = ? AND state = ?`,
			string(ViewGenerationSuperseded), req.LastSeen, "dedicated base epoch refreshed",
			req.GraphID, req.ExpectedBaseGenerationID, string(ViewGenerationBuilding)); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
UPDATE ref_views
   SET active_generation_id = NULL, active_ref = NULL, active_commit = NULL,
       active_tree = NULL, active_build_fingerprint = NULL,
       route_epoch = route_epoch + 1, state = ?, exact_view = 0,
       last_error = ?
 WHERE graph_id = ?`,
			string(RefViewBuilding), "dedicated base epoch refreshed", req.GraphID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
UPDATE checkouts SET last_seen = ?, last_error = ''
 WHERE checkout_id = ? AND incarnation = ?`,
			req.LastSeen, req.CheckoutID, req.Incarnation); err != nil {
			return err
		}
		appendRetiredGeneration(&out.RetiredGenerationIDs, req.ExpectedBaseGenerationID)
		return nil
	})
	return out, err
}

type dedicatedBaseRefreshGeneration struct {
	ownerKind        string
	graphID          string
	layerID          string
	checkoutID       string
	generationKind   string
	baseGenerationID int64
	treeOID          string
	configHash       string
	extractors       string
	resolver         string
	state            ViewGenerationState
}

func dedicatedBaseRefreshGenerationTx(
	ctx context.Context, tx *sql.Tx, generationID int64,
) (dedicatedBaseRefreshGeneration, error) {
	var out dedicatedBaseRefreshGeneration
	var state string
	err := tx.QueryRowContext(ctx, `
SELECT COALESCE(owner_kind, ''), COALESCE(graph_id, ''), COALESCE(layer_id, ''),
       COALESCE(checkout_id, ''), COALESCE(generation_kind, ''),
       COALESCE(base_generation_id, 0), COALESCE(tree_oid, ''),
       COALESCE(config_hash, ''), COALESCE(extractor_versions, ''),
       COALESCE(resolver_version, ''), state
  FROM view_generations WHERE generation_id = ?`, generationID).Scan(
		&out.ownerKind, &out.graphID, &out.layerID, &out.checkoutID,
		&out.generationKind, &out.baseGenerationID, &out.treeOID,
		&out.configHash, &out.extractors, &out.resolver, &state)
	if err == sql.ErrNoRows {
		return out, fmt.Errorf("%w: view generation %d", ErrCatalogStaleGuard, generationID)
	}
	if err != nil {
		return out, err
	}
	out.state = ViewGenerationState(state)
	return out, nil
}

func verifyDedicatedBaseRefreshGenerationsTx(
	ctx context.Context, tx *sql.Tx, req CommitDedicatedBaseRefreshRequest,
) error {
	base, err := dedicatedBaseRefreshGenerationTx(ctx, tx, req.NewBaseGenerationID)
	if err != nil {
		return err
	}
	if base.ownerKind != "dedicated_base" || base.generationKind != "dedicated_base" ||
		base.graphID != req.GraphID || base.layerID != req.GraphID+":base" ||
		base.checkoutID != req.CheckoutID || base.baseGenerationID != 0 ||
		base.treeOID != req.BaseTreeOID || base.configHash != req.ConfigHash ||
		base.extractors != req.ExtractorVersions || base.resolver != req.ResolverVersion ||
		base.state != ViewGenerationReady {
		return fmt.Errorf("%w: replacement dedicated base generation %d moved",
			ErrCatalogStaleGuard, req.NewBaseGenerationID)
	}
	commit, err := promotionGenerationTx(ctx, tx, req.CommitGenerationID)
	if err != nil {
		return err
	}
	dirty, err := promotionGenerationTx(ctx, tx, req.DirtyGenerationID)
	if err != nil {
		return err
	}
	if commit.ownerKind != checkoutGenerationOwnerKind ||
		commit.graphID != req.GraphID || commit.layerID != "commit-"+req.CheckoutID ||
		commit.checkoutID != req.CheckoutID || commit.generationKind != string(RouteSlotCommit) ||
		commit.baseGenerationID != req.NewBaseGenerationID || commit.treeOID != req.CommitTreeOID ||
		commit.configHash != req.ConfigHash || commit.extractors != req.ExtractorVersions ||
		commit.resolver != req.ResolverVersion || commit.state != ViewGenerationReady {
		return fmt.Errorf("%w: refreshed commit generation %d moved",
			ErrCatalogStaleGuard, req.CommitGenerationID)
	}
	if dirty.ownerKind != checkoutGenerationOwnerKind ||
		dirty.graphID != req.GraphID || dirty.layerID != "dirty-"+req.CheckoutID ||
		dirty.checkoutID != req.CheckoutID || dirty.generationKind != string(RouteSlotDirty) ||
		dirty.baseGenerationID != req.CommitGenerationID || dirty.treeOID != req.CommitTreeOID ||
		dirty.configHash != req.ConfigHash || dirty.extractors != req.ExtractorVersions ||
		dirty.resolver != req.ResolverVersion || dirty.state != ViewGenerationReady {
		return fmt.Errorf("%w: refreshed dirty generation %d moved",
			ErrCatalogStaleGuard, req.DirtyGenerationID)
	}
	return nil
}

func validateCommitDedicatedBaseRefreshRequest(req CommitDedicatedBaseRefreshRequest) error {
	for name, value := range map[string]string{
		"checkout_id":          req.CheckoutID,
		"incarnation":          req.Incarnation,
		"family_id":            req.FamilyID,
		"graph_id":             req.GraphID,
		"required_graph_state": req.RequiredGraphState,
		"base_tree_oid":        req.BaseTreeOID,
		"config_hash":          req.ConfigHash,
		"extractor_versions":   req.ExtractorVersions,
		"resolver_version":     req.ResolverVersion,
		"commit_tree_oid":      req.CommitTreeOID,
	} {
		if value == "" {
			return fmt.Errorf("%s must not be empty", name)
		}
	}
	if req.ExpectedBaseGenerationID <= 0 || req.NewBaseGenerationID <= 0 ||
		req.CommitGenerationID <= 0 || req.DirtyGenerationID <= 0 {
		return fmt.Errorf("dedicated base refresh generation ids must be positive")
	}
	if req.ExpectedBaseGenerationID == req.NewBaseGenerationID {
		return fmt.Errorf("replacement dedicated base must differ from the active generation")
	}
	if req.ExpectedRouteEpoch < 0 {
		return fmt.Errorf("expected route epoch must not be negative")
	}
	return nil
}

func appendRetiredGeneration(generations *[]int64, generationID int64) {
	if generationID <= 0 {
		return
	}
	for _, existing := range *generations {
		if existing == generationID {
			return
		}
	}
	*generations = append(*generations, generationID)
}
