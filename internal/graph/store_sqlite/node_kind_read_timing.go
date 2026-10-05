package store_sqlite

import (
	"context"
	"fmt"
	"time"

	"github.com/zzet/gortex/internal/graph"
)

type nodeKindGateTimingKey struct{}

// observedNodeKindsBatch changes only debug-observed connection acquisition.
// The explicit connection owns exactly one rows cursor, as QueryContext does;
// rows and connection close on every return, including scan/cancel failures.
func (s *Store) observedNodeKindsBatch(ctx context.Context, ids []string, observer func(graph.NodeKindReadTiming)) (out map[string]graph.NodeKindRow, err error) {
	timing := graph.NodeKindReadTiming{Batches: 1, InputIDs: len(ids)}
	started := time.Now()
	defer func() {
		timing.Total = time.Since(started)
		if err != nil {
			timing.Errors++
		}
		observer(timing)
	}()
	poolStart := time.Now()
	conn, err := s.db.Conn(ctx)
	timing.Pool = time.Since(poolStart)
	if err != nil {
		return nil, fmt.Errorf("node kinds connection: %w", err)
	}
	defer func() {
		closeErr := conn.Close()
		if err == nil && closeErr != nil {
			out = nil
			err = closeErr
		}
	}()
	query := `SELECT id, kind, file_path, repo_prefix FROM nodes WHERE id IN (` + inPlaceholders(len(ids)) + `) AND view_gen = ?`
	queryCtx := context.WithValue(ctx, nodeKindGateTimingKey{}, &timing.Gate)
	queryStart := time.Now()
	rows, err := conn.QueryContext(queryCtx, query, append(toAnyArgs(ids), s.viewGen)...)
	timing.QueryStart = time.Since(queryStart)
	if err != nil {
		return nil, fmt.Errorf("get node kinds by ids: %w", err)
	}
	drainStart := time.Now()
	defer func() {
		timing.Drain = time.Since(drainStart)
		closeErr := rows.Close()
		if err == nil && closeErr != nil {
			out = nil
			err = closeErr
		}
	}()
	out = make(map[string]graph.NodeKindRow, len(ids))
	for rows.Next() {
		var id string
		var row graph.NodeKindRow
		if err = rows.Scan(&id, &row.Kind, &row.FilePath, &row.RepoPrefix); err != nil {
			return nil, fmt.Errorf("scan node kind by id: %w", err)
		}
		out[id] = row
		timing.Rows++
	}
	if err = rows.Err(); err != nil {
		return nil, fmt.Errorf("read node kinds by ids: %w", err)
	}
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	return out, nil
}
