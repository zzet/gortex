package store_sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"sort"

	"github.com/zzet/gortex/internal/graph"
)

var _ graph.BoundedIncomingSourceReader = (*Store)(nil)

const findIncomingSourcesBoundedSQL = `SELECT DISTINCT from_id
	FROM edges
	WHERE to_id = ? AND kind = ? AND from_id <> '' AND view_gen = ?
	LIMIT ?`

// FindIncomingSourcesBounded projects distinct incoming source identities for
// one edge kind. One read-only transaction freezes the batch snapshot; each
// target cursor is closed before the next query so callers can safely perform
// later store reads without cursor re-entry.
func (s *Store) FindIncomingSourcesBounded(
	ctx context.Context,
	targetIDs []string,
	kind graph.EdgeKind,
	limit int,
) (graph.BoundedIncomingSourceProjection, error) {
	projection := graph.BoundedIncomingSourceProjection{
		Sources:   make(map[string][]string),
		Truncated: make(map[string]bool),
	}
	ids := dedupeNonEmpty(targetIDs)
	if len(ids) == 0 {
		return projection, nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return graph.BoundedIncomingSourceProjection{}, err
	}
	if limit <= 0 {
		for _, id := range ids {
			projection.Truncated[id] = true
		}
		return projection, nil
	}
	maxInt := int(^uint(0) >> 1)
	if limit >= maxInt {
		return graph.BoundedIncomingSourceProjection{}, &graph.BoundedLocalizationLimitError{
			Resource: "SQLite incoming-source sentinel",
			Limit:    maxInt - 1,
		}
	}

	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return graph.BoundedIncomingSourceProjection{}, err
	}
	defer func() { _ = tx.Rollback() }()

	for _, targetID := range ids {
		if err := ctx.Err(); err != nil {
			return graph.BoundedIncomingSourceProjection{}, err
		}
		rows, queryErr := tx.QueryContext(ctx, findIncomingSourcesBoundedSQL, targetID, kind, s.viewGen, limit+1)
		if queryErr != nil {
			return graph.BoundedIncomingSourceProjection{}, queryErr
		}
		sources := make([]string, 0, limit)
		truncated := false
		for rows.Next() {
			var sourceID string
			if scanErr := rows.Scan(&sourceID); scanErr != nil {
				_ = rows.Close()
				return graph.BoundedIncomingSourceProjection{}, scanErr
			}
			if len(sources) >= limit {
				truncated = true
				break
			}
			sources = append(sources, sourceID)
		}
		if rowsErr := rows.Err(); rowsErr != nil {
			_ = rows.Close()
			return graph.BoundedIncomingSourceProjection{}, fmt.Errorf("bounded incoming-source query: %w", rowsErr)
		}
		if closeErr := rows.Close(); closeErr != nil {
			return graph.BoundedIncomingSourceProjection{}, closeErr
		}
		if truncated {
			projection.Truncated[targetID] = true
			continue
		}
		if len(sources) > 0 {
			sort.Strings(sources)
			projection.Sources[targetID] = sources
		}
	}
	if err := tx.Commit(); err != nil {
		return graph.BoundedIncomingSourceProjection{}, err
	}
	return projection, nil
}
