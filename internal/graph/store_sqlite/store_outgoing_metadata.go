package store_sqlite

import (
	"context"
	"fmt"
	"github.com/zzet/gortex/internal/graph"
)

// GetOutEdgesByNodeIDsWithMetadataContext reads bounded exact-generation
// outgoing evidence including metadata and promoted edge fields. Traversal's
// light projection cannot certify DI or contract-local syntax enrichment.
// Errors return no usable rows; callers must reject Truncated=true.
func (s *Store) GetOutEdgesByNodeIDsWithMetadataContext(ctx context.Context, ids []string, limit int) (map[string][]*graph.Edge, bool, error) {
	if ctx == nil {
		return nil, false, fmt.Errorf("bounded metadata outgoing edges: nil context")
	}
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	uniq := dedupeNonEmpty(ids)
	if len(uniq) == 0 {
		return nil, false, nil
	}
	if limit <= 0 {
		return nil, true, nil
	}

	out := make(map[string][]*graph.Edge, len(uniq))
	total := 0
	for i := 0; i < len(uniq); i += lookupChunkSize {
		if err := ctx.Err(); err != nil {
			return nil, true, err
		}
		end := minInt(i+lookupChunkSize, len(uniq))
		chunk := uniq[i:end]
		remaining := limit - total
		queryLimit := remaining + 1
		q := `SELECT ` + lookupEdgeCols + ` FROM edges WHERE from_id IN (` + inPlaceholders(len(chunk)) + `) AND view_gen = ? LIMIT ?`
		args := toAnyArgs(chunk)
		args = append(args, s.viewGen, queryLimit)
		rows, err := s.db.QueryContext(ctx, q, args...)
		if err != nil {
			return nil, true, err
		}
		for rows.Next() {
			e, scanErr := s.scanEdgeCursor(rows)
			if scanErr != nil {
				_ = rows.Close()
				return nil, true, scanErr
			}
			if total >= limit {
				_ = rows.Close()
				return out, true, nil
			}
			if e != nil {
				out[e.From] = append(out[e.From], e)
				total++
			}
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return nil, true, fmt.Errorf("bounded outgoing-edge query: %w", err)
		}
		if err := rows.Close(); err != nil {
			return nil, true, err
		}
	}
	return out, false, nil
}
