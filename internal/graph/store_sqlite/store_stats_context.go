package store_sqlite

import (
	"context"
	"database/sql"

	"github.com/zzet/gortex/internal/graph"
)

func (s *Store) nodeCountContext(ctx context.Context) (int, error) {
	stmt := s.stmtNodeCount
	if s.viewGen > baseViewGeneration {
		stmt = s.stmtGenerationNodeCount
	}
	var n int
	if err := stmt.QueryRowContext(ctx, s.viewGen).Scan(&n); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return 0, ctxErr
		}
		return 0, err
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	return n, nil
}

func (s *Store) edgeCountContext(ctx context.Context) (int, error) {
	stmt := s.stmtEdgeCount
	if s.viewGen > baseViewGeneration {
		stmt = s.stmtGenerationEdgeCount
	}
	var n int
	if err := stmt.QueryRowContext(ctx, s.viewGen).Scan(&n); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return 0, ctxErr
		}
		return 0, err
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	return n, nil
}

func (s *Store) countsFromIndexStateContext(ctx context.Context) (nodes, edges int, ok bool, err error) {
	var rows int
	if err := s.stmtIndexStateTotals.QueryRowContext(ctx, s.viewGen).Scan(&rows, &nodes, &edges); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return 0, 0, false, ctxErr
		}
		return 0, 0, false, err
	}
	if err := ctx.Err(); err != nil {
		return 0, 0, false, err
	}
	return nodes, edges, rows != 0, nil
}

func (s *Store) scanStatsRowsContext(ctx context.Context, stmt *sql.Stmt, put func(string, int)) error {
	rows, err := stmt.QueryContext(ctx, s.viewGen)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		return err
	}
	defer rows.Close()
	for rows.Next() {
		if err := ctx.Err(); err != nil {
			return err
		}
		var key string
		var n int
		if err := rows.Scan(&key, &n); err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return ctxErr
			}
			return err
		}
		put(key, n)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := rows.Err(); err != nil {
		return err
	}
	return nil
}

// StatsContext returns an all-or-nothing histogram and totals under ctx.
// It never returns a partial GraphStats: a deadline or row error discards all
// accumulated fields so callers cannot render a short histogram as a real one.
func (s *Store) StatsContext(ctx context.Context) (graph.GraphStats, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return graph.GraphStats{}, err
	}
	st := graph.GraphStats{ByKind: map[string]int{}, ByLanguage: map[string]int{}}
	nodes, edges, ok, err := s.countsFromIndexStateContext(ctx)
	if err != nil {
		return graph.GraphStats{}, err
	}
	if !ok {
		if nodes, err = s.nodeCountContext(ctx); err != nil {
			return graph.GraphStats{}, err
		}
		if edges, err = s.edgeCountContext(ctx); err != nil {
			return graph.GraphStats{}, err
		}
	}
	st.TotalNodes, st.TotalEdges = nodes, edges
	if err := s.scanStatsRowsContext(ctx, s.stmtStatsByKind, func(key string, n int) { st.ByKind[key] = n }); err != nil {
		return graph.GraphStats{}, err
	}
	if err := s.scanStatsRowsContext(ctx, s.stmtStatsByLanguage, func(key string, n int) { st.ByLanguage[key] = n }); err != nil {
		return graph.GraphStats{}, err
	}
	if err := ctx.Err(); err != nil {
		return graph.GraphStats{}, err
	}
	return st, nil
}
