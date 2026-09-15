package store_sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// AtManagedGeneration derives a borrowed positive-generation handle whose
// writes must pass lifecycle admission. Construction checks only an initialized
// Store and a positive identity; it grants neither DB liveness nor write
// authority and does not reopen a shared payload seal.
func (s *Store) AtManagedGeneration(generationID int64) (*Store, error) {
	if s == nil || s.storeCore == nil || generationID <= baseViewGeneration {
		return nil, fmt.Errorf("%w: managed generation needs an initialized store and positive id", ErrCatalogInvalidValue)
	}
	handle := s.AtGeneration(generationID)
	handle.managedPayloadGeneration = true
	return handle, nil
}

// checkManagedPayloadWriteTx validates a lifecycle-qualified write through the
// actual payload transaction. It never opens another connection or changes the
// shared publication seal. The caller rolls back on refusal before payload SQL.
func checkManagedPayloadWriteTx(ctx context.Context, tx *sql.Tx, generationID int64) error {
	if ctx == nil {
		return fmt.Errorf("%w: nil context", ErrCatalogInvalidValue)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if tx == nil || generationID <= baseViewGeneration {
		return fmt.Errorf("%w: managed admission needs a transaction and positive generation", ErrCatalogInvalidValue)
	}
	var state string
	err := tx.QueryRowContext(ctx,
		`SELECT state FROM view_generations WHERE generation_id = ?`, generationID).Scan(&state)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: generation %d has no catalog row", ErrPayloadGenerationSealed, generationID)
	}
	if err != nil {
		return err
	}
	if ViewGenerationState(state) != ViewGenerationBuilding {
		return fmt.Errorf("%w: generation %d is %s", ErrPayloadGenerationSealed, generationID, state)
	}
	return nil
}

// execManagedPayloadWriteLocked uses the caller-held writer gate and the same
// selected transaction for lifecycle admission and payload SQL. The centralized
// begin seam performs the predicate once; this helper must not repeat it.
func (s *Store) execManagedPayloadWriteLocked(ctx context.Context, query string, args ...any) (sql.Result, error) {
	tx, err := s.beginWriteContext(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	result, err := tx.ExecContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return result, nil
}
