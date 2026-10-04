package store_sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// ErrCatalogGenerationRetiring means a proposed catalog reference targets a
// generation whose retirement fence has already committed.
var ErrCatalogGenerationRetiring = errors.New("store_sqlite: view generation is retiring")

// validateViewGenerationAdmissionTx rejects missing or retiring positive
// generations, and references into an existing closing dedicated graph.
// Existing shape/sentinel rules and stricter dedicated ownership,
// policy, and ancestry checks remain the caller's responsibility. The caller
// must retain this same writer transaction through reference mutation/commit.
func validateViewGenerationAdmissionTx(ctx context.Context, tx *sql.Tx, generationID int64) error {
	if generationID <= 0 {
		return nil
	}
	var state ViewGenerationState
	var graphID sql.NullString
	err := tx.QueryRowContext(ctx, `SELECT state, graph_id FROM view_generations WHERE generation_id=?`, generationID).Scan(&state, &graphID)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: generation %d", ErrCatalogNotFound, generationID)
	}
	if err != nil {
		return err
	}
	if state == ViewGenerationRetiring {
		return fmt.Errorf("%w: generation %d", ErrCatalogGenerationRetiring, generationID)
	}
	return validateDedicatedGraphAdmissionTx(ctx, tx, graphID.String)
}

// BeginViewGenerationRetirement commits the catalog reference fence before any
// payload is sealed or deleted. It does not replace the separate reader/build
// ownership handshake: the caller must recheck those owners after this returns.
func (c *Catalog) BeginViewGenerationRetirement(ctx context.Context, generationID int64) error {
	if ctx == nil {
		return fmt.Errorf("%w: nil context", ErrCatalogInvalidValue)
	}
	if generationID <= 0 {
		return fmt.Errorf("%w: generation ID must be positive", ErrCatalogInvalidValue)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return c.withTx(ctx, func(tx *sql.Tx) error {
		var state ViewGenerationState
		err := tx.QueryRowContext(ctx, `SELECT state FROM view_generations WHERE generation_id=?`, generationID).Scan(&state)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: generation %d", ErrCatalogNotFound, generationID)
		}
		if err != nil {
			return err
		}
		referenced, err := viewGenerationRetirementReferencedTx(ctx, tx, generationID)
		if err != nil {
			return err
		}
		if referenced {
			return fmt.Errorf("%w: generation %d", ErrCatalogGenerationReferenced, generationID)
		}
		// Interrupted chunked sweeping can resume without an idle metadata write.
		if state == ViewGenerationRetiring {
			return nil
		}
		return execGuardedTx(ctx, tx, "generation retirement", `UPDATE view_generations SET state=? WHERE generation_id=? AND state=?`,
			string(ViewGenerationRetiring), generationID, string(state))
	})
}

// viewGenerationRetirementReferencedTx uses the same complete predicate as the
// diagnostic and final catalog deletion paths. The publication term selects the
// single CURRENT graph-owned association, never historical adopted generations.
func viewGenerationRetirementReferencedTx(ctx context.Context, tx *sql.Tx, generationID int64) (bool, error) {
	var referenced bool
	err := tx.QueryRowContext(ctx, viewGenerationReferencedSQL,
		generationID, generationID, generationID, generationID, generationID, generationID).Scan(&referenced)
	return referenced, err
}
