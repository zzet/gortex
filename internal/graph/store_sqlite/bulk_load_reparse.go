package store_sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// BeginManagedDedicatedReparseBulkLoad gives an unpublished initial dedicated
// reparse the existing generation cache shape, even when a prior attempt left
// payload behind. It neither deletes that payload nor authorizes its writes.
// The caller still owns the claimed build; every managed write rechecks Building.
// BeginGenerationBulkLoad retains its separate empty-only contract.
func (s *Store) BeginManagedDedicatedReparseBulkLoad(ctx context.Context, generationID int64) (bool, error) {
	if ctx == nil {
		return false, fmt.Errorf("%w: nil reparse context", ErrCatalogInvalidValue)
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if s == nil || s.coreless() || !s.managedPayloadGeneration || s.seal == nil || s.viewGen != generationID || generationID <= baseViewGeneration {
		return false, fmt.Errorf("%w: reparse window needs its managed positive generation handle", ErrCatalogInvalidValue)
	}
	// Validate before the writer gate; the common helper rechecks through its
	// held writer connection before changing any connection-local shape.
	if err := checkDedicatedReparseWindow(ctx, s.db, generationID); err != nil {
		return false, err
	}
	// Reuse the proven Building verdict only to resolve an unknown shared
	// seal. This CAS cannot reopen a seal that publication or retirement closed.
	if err := s.openPayloadSeal(s.seal); err != nil {
		return false, err
	}
	return s.beginGenerationBulkLoad(ctx, generationID, true)
}

func checkDedicatedReparseWindow(ctx context.Context, q dedicatedBaseQuerier, generationID int64) error {
	var state, owner, kind, graphID, lower string
	var layerID sql.NullString
	var base sql.NullInt64
	err := q.QueryRowContext(ctx, `SELECT state,owner_kind,generation_kind,graph_id,base_generation_id,layer_id,lower_view_fingerprint
FROM view_generations WHERE generation_id=?`, generationID).Scan(&state, &owner, &kind, &graphID, &base, &layerID, &lower)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: reparse generation %d has no catalog row", ErrDedicatedBaseCandidate, generationID)
	}
	if err != nil {
		return err
	}
	if ViewGenerationState(state) != ViewGenerationBuilding || owner != "dedicated_graph" || kind != "dedicated" || graphID == "" || base.Int64 != 0 || layerID.String != "" || lower != "" {
		return fmt.Errorf("%w: generation %d is not an initial Building dedicated payload", ErrDedicatedBaseCandidate, generationID)
	}
	return ctx.Err()
}

// Empty-only callers keep their existing gate behavior; the new reparse
// admission can stop while waiting for either acquisition of that gate.
func (s *Store) lockGenerationBulkWriter(ctx context.Context, reparse bool) error {
	if reparse {
		return s.writeMu.LockContext(ctx)
	}
	s.writeMu.Lock()
	return nil
}
