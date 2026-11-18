package store_sqlite

import (
	"context"
	"fmt"

	"github.com/zzet/gortex/internal/graph"
)

const contractAnalysisNodesSQL = `SELECT ` + lookupNodeCols + ` FROM nodes INDEXED BY nodes_by_generation WHERE view_gen = ? ORDER BY id LIMIT ?`

// ContractAnalysisNodesContext reads bounded full node evidence from this
// physical analysis payload only. It does not compose or inspect core layers.
// Callers must refuse truncation; errors never return partial usable evidence.
func (s *Store) ContractAnalysisNodesContext(ctx context.Context, limit int) ([]*graph.Node, bool, error) {
	if ctx == nil {
		return nil, false, fmt.Errorf("contract analysis nodes: nil context")
	}
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	if limit <= 0 || limit > 65536 {
		return nil, false, fmt.Errorf("contract analysis nodes: invalid limit %d", limit)
	}
	rows, err := s.db.QueryContext(ctx, contractAnalysisNodesSQL, s.viewGen, limit+1)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	out := make([]*graph.Node, 0)
	for rows.Next() {
		n, err := scanNodeCursor(rows)
		if err != nil {
			return nil, false, err
		}
		out = append(out, n)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	if err := rows.Close(); err != nil {
		return nil, false, err
	}
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	if len(out) > limit {
		return out[:limit], true, nil
	}
	return out, false, nil
}
