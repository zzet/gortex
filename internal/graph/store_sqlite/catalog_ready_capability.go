package store_sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

const readyGenerationSourceSnapshotCapability = "source.snapshot"

// normalizeReadyGenerationCapabilities validates and deduplicates capability
// requirements while preserving the caller's order. The returned slice never
// aliases the request, so a caller cannot change the meaning of a claim while
// the catalog is evaluating it.
func normalizeReadyGenerationCapabilities(required []string) ([]string, error) {
	if len(required) == 0 {
		return nil, nil
	}
	out := make([]string, 0, len(required))
	seen := make(map[string]struct{}, len(required))
	for _, capability := range required {
		if err := requireCatalogID("required capability", capability); err != nil {
			return nil, err
		}
		if _, duplicate := seen[capability]; duplicate {
			continue
		}
		seen[capability] = struct{}{}
		out = append(out, capability)
	}
	return out, nil
}

// readyGenerationSupportsCapabilities reports whether generation declares
// every required producer complete. Missing, building, incomplete, disabled,
// and withdrawn producers are all cache misses: adoption may only preserve a
// capability the winner can answer now.
func readyGenerationSupportsCapabilities(
	ctx context.Context,
	tx *sql.Tx,
	generationID int64,
	required []string,
) (bool, error) {
	for _, capability := range required {
		var state string
		err := tx.QueryRowContext(ctx, `
			SELECT state
			FROM generation_producer_completeness
			WHERE view_gen = ? AND producer = ?
		`, generationID, capability).Scan(&state)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			return false, nil
		case err != nil:
			return false, fmt.Errorf("read capability %s for generation %d: %w", capability, generationID, err)
		case state != string(ProducerStateComplete):
			return false, nil
		}
	}
	return true, nil
}
