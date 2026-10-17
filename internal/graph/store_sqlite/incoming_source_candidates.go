package store_sqlite

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/zzet/gortex/internal/graph"
)

var _ graph.BoundedIncomingSourceCandidateReader = (*Store)(nil)

// Same checked indexed incoming shape as boundedIncomingAdjacencySQL, but
// retain only the two columns needed for post-cursor ownership and dedup.
// No DISTINCT: a raw row is charged even when its source repeats.
const readIncomingSourceCandidatesSQL = `SELECT from_id, file_path
FROM edges INDEXED BY edges_by_to
WHERE to_id = ? AND kind = ? AND from_id <> '' AND view_gen = ?
LIMIT ?`

func (s *Store) ReadIncomingSourceCandidates(ctx context.Context, targetIDs []string, kind graph.EdgeKind) (map[string][]graph.IncomingSourceCandidate, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	ids := dedupeNonEmpty(targetIDs)
	if len(ids) > graph.MaxBoundedAdjacencyKeys {
		return nil, &graph.BoundedLocalizationLimitError{Resource: "incoming-source candidate targets", Limit: graph.MaxBoundedAdjacencyKeys}
	}
	out := make(map[string][]graph.IncomingSourceCandidate)
	if len(ids) == 0 {
		return out, nil
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	inspected := 0
	for _, target := range ids {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		rows, err := tx.QueryContext(ctx, readIncomingSourceCandidatesSQL, target, kind, s.viewGen, graph.MaxIncomingSourceCandidateRows-inspected+1)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			inspected++
			if inspected > graph.MaxIncomingSourceCandidateRows {
				_ = rows.Close()
				return nil, &graph.BoundedLocalizationLimitError{Resource: "incoming-source candidate inspections", Limit: graph.MaxIncomingSourceCandidateRows}
			}
			if inspected&127 == 0 {
				if err := ctx.Err(); err != nil {
					_ = rows.Close()
					return nil, err
				}
			}
			var row graph.IncomingSourceCandidate
			if err := rows.Scan(&row.From, &row.FilePath); err != nil {
				_ = rows.Close()
				return nil, err
			}
			out[target] = append(out[target], row)
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("incoming-source candidates: %w", err)
		}
		if err := rows.Close(); err != nil {
			return nil, err
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return out, nil
}
