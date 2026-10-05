package store_sqlite

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/zzet/gortex/internal/graph"
)

var _ graph.CallReferenceOutgoingReader = (*Store)(nil)

// GetCallReferenceOutEdgesContext retains the full-row outgoing order. The
// unforced kind query uses a different index and can reorder same-anchor rows,
// changing which edges survive bounded CSR caps. Index-deferred stores instead
// use the original full query and filter after decoding.
func (s *Store) GetCallReferenceOutEdgesContext(ctx context.Context, ids []string) (map[string][]*graph.Edge, error) {
	if ctx == nil {
		return nil, fmt.Errorf("call/reference outgoing edges: nil context")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	uniq := dedupeNonEmpty(ids)
	if len(uniq) == 0 {
		return nil, nil
	}
	// Pin catalog and edge rows together; close this transaction before the
	// caller classifies endpoints through another selected-reader query.
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var indexed bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type='index' AND name='edges_by_from_line_kind')`).Scan(&indexed); err != nil {
		return nil, err
	}
	// The same-index plan was qualified for base0. Positive layers retain
	// their original planner/order and filter the complete decoded rows.
	indexed = indexed && s.viewGen == 0
	out := make(map[string][]*graph.Edge, len(uniq))
	for start := 0; start < len(uniq); start += lookupChunkSize {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		chunk := uniq[start:min(start+lookupChunkSize, len(uniq))]
		original := callReferenceOutgoingSQL(len(chunk), false)
		q := original
		args := append(toAnyArgs(chunk), s.viewGen)
		if indexed {
			q = callReferenceOutgoingSQL(len(chunk), true)
			args = append(args, string(graph.EdgeCalls), string(graph.EdgeReferences))
		}
		rows, err := tx.QueryContext(ctx, q, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			edge, err := s.scanEdgeCursor(rows)
			if err != nil {
				_ = rows.Close()
				return nil, err
			}
			if edge != nil && (edge.Kind == graph.EdgeCalls || edge.Kind == graph.EdgeReferences) {
				out[edge.From] = append(out[edge.From], edge)
			}
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return nil, err
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

func callReferenceOutgoingSQL(count int, indexed bool) string {
	if indexed {
		return `SELECT ` + lookupEdgeCols + ` FROM edges INDEXED BY edges_by_from_line_kind WHERE from_id IN (` + inPlaceholders(count) + `) AND view_gen = ? AND kind IN (?, ?)`
	}
	return `SELECT ` + lookupEdgeCols + ` FROM edges WHERE from_id IN (` + inPlaceholders(count) + `) AND view_gen = ?`
}
