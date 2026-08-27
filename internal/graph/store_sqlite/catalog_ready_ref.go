package store_sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// BindReadyGenerationLeaseToRefViewRequest atomically publishes a claimed
// canonical generation into one still-current ref-view build attempt.
type BindReadyGenerationLeaseToRefViewRequest struct {
	Key          ReadyGenerationCacheKey
	LeaseToken   string
	GenerationID int64

	RefViewID                       string
	ExpectedRouteEpoch              int64
	ExpectedDesiredTree             string
	ExpectedDesiredBuildFingerprint string

	BuildID    string
	BuildToken string

	ActiveRef              string
	ActiveCommit           string
	ActiveTree             string
	ActiveBuildFingerprint string
	ExactView              bool
}

// BindReadyGenerationLeaseToRefView consumes a DB-clocked handoff lease in the
// same transaction that closes the exact build attempt and advances the exact
// ref route it captured. A moved route, reclaimed build, expired lease,
// withdrawn source snapshot, or changed canonical identity leaves all three
// rows untouched and reports ErrCatalogStaleGuard.
func (c *Catalog) BindReadyGenerationLeaseToRefView(
	ctx context.Context,
	req BindReadyGenerationLeaseToRefViewRequest,
) error {
	if err := validateReadyGenerationCacheKey(req.Key); err != nil {
		return err
	}
	if req.LeaseToken == "" {
		return fmt.Errorf("ready generation lease token must not be empty")
	}
	if err := requireCatalogID("ref_view_id", req.RefViewID); err != nil {
		return err
	}
	if err := requireCatalogID("build_id", req.BuildID); err != nil {
		return err
	}
	if err := requireCatalogID("build_token", req.BuildToken); err != nil {
		return err
	}
	if req.GenerationID <= 0 {
		return fmt.Errorf("generation id must be positive")
	}
	if req.ExpectedRouteEpoch < 0 {
		return fmt.Errorf("expected route epoch must not be negative")
	}
	if req.ExpectedDesiredTree == "" || req.ExpectedDesiredBuildFingerprint == "" ||
		req.ActiveCommit == "" || req.ActiveTree == "" || req.ActiveBuildFingerprint == "" {
		return fmt.Errorf("ref-view adoption identity must be complete")
	}
	if req.ExpectedDesiredTree != req.Key.TreeOID || req.ActiveTree != req.Key.TreeOID ||
		req.ActiveBuildFingerprint != req.ExpectedDesiredBuildFingerprint {
		return fmt.Errorf("ref-view adoption identity does not match the ready generation key")
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
	compatible, err := candidateMatchesReadyGenerationKey(ctx, tx, req.GenerationID, req.Key,
		[]string{readyGenerationSourceSnapshotCapability})
	if err != nil {
		return err
	}
	if !compatible {
		return fmt.Errorf("%w: ready generation is no longer compatible or source-complete", ErrCatalogStaleGuard)
	}

	result, err := tx.ExecContext(ctx, `
		UPDATE ref_view_builds
		SET state = ?, generation_id = ?, last_progress = ?, error = ''
		WHERE build_id = ? AND build_token = ? AND ref_view_id = ? AND state = ?
		  AND desired_tree = ? AND base_generation_id = ?
		  AND build_fingerprint = ? AND captured_route_epoch = ?
	`, string(ViewGenerationReady), req.GenerationID, now,
		req.BuildID, req.BuildToken, req.RefViewID, string(ViewGenerationBuilding),
		req.ExpectedDesiredTree, req.Key.BaseGenerationID,
		req.ExpectedDesiredBuildFingerprint, req.ExpectedRouteEpoch)
	if err != nil {
		return fmt.Errorf("complete ref-view cache build: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rows != 1 {
		return fmt.Errorf("%w: ref-view build attempt moved", ErrCatalogStaleGuard)
	}

	result, err = tx.ExecContext(ctx, `
		UPDATE ref_views
		SET active_generation_id = ?, active_ref = ?, active_commit = ?, active_tree = ?,
		    active_build_fingerprint = ?, state = ?, exact_view = ?,
		    last_resolved = ?, last_selected = ?, last_error = '',
		    route_epoch = route_epoch + 1
		WHERE ref_view_id = ? AND graph_id = ? AND route_epoch = ?
		  AND desired_tree = ? AND desired_build_fingerprint = ?
	`, req.GenerationID, catalogNullString(req.ActiveRef), catalogNullString(req.ActiveCommit),
		catalogNullString(req.ActiveTree), catalogNullString(req.ActiveBuildFingerprint),
		string(RefViewReady), catalogBoolInt(req.ExactView), now, now,
		req.RefViewID, req.Key.GraphID, req.ExpectedRouteEpoch,
		req.ExpectedDesiredTree, req.ExpectedDesiredBuildFingerprint)
	if err != nil {
		return fmt.Errorf("bind ready generation to ref view: %w", err)
	}
	rows, err = result.RowsAffected()
	if err != nil {
		return err
	}
	if rows != 1 {
		return fmt.Errorf("%w: ref-view route moved", ErrCatalogStaleGuard)
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
