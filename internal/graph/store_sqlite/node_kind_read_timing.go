package store_sqlite

import (
	"context"
	"fmt"
	"time"

	"github.com/zzet/gortex/internal/graph"
)

type nodeKindGateTimingKey struct{}
type nodeKindDriverTiming struct {
	firstEntry time.Time
	gate       time.Duration
	entries    int
}

// observedNodeKindsBatch keeps database/sql QueryContext acquisition and
// ErrBadConn retry semantics unchanged. The context hook observes first driver
// entry and gate admission; it never owns a connection or alters retries.
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
	query := `SELECT id, kind, file_path, repo_prefix FROM nodes WHERE id IN (` + inPlaceholders(len(ids)) + `) AND view_gen = ?`
	driverTiming := nodeKindDriverTiming{}
	queryCtx := context.WithValue(ctx, nodeKindGateTimingKey{}, &driverTiming)
	queryStart := time.Now()
	rows, err := s.db.QueryContext(queryCtx, query, append(toAnyArgs(ids), s.viewGen)...)
	timing.QueryStart = time.Since(queryStart)
	timing.PreDriver = timing.QueryStart
	if !driverTiming.firstEntry.IsZero() {
		timing.PreDriver = driverTiming.firstEntry.Sub(queryStart)
	}
	timing.Gate = driverTiming.gate
	timing.DriverEntries = driverTiming.entries
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

// observedNodePresenceBatch retains the same request-local read timing for the
// ID-only CSR path, including database/sql retries and cursor close errors.
func (s *Store) observedNodePresenceBatch(ctx context.Context, ids []string, observer func(graph.NodeKindReadTiming)) (out map[string]struct{}, err error) {
	timing := graph.NodeKindReadTiming{Batches: 1, InputIDs: len(ids)}
	started := time.Now()
	defer func() {
		timing.Total = time.Since(started)
		if err != nil {
			timing.Errors++
		}
		observer(timing)
	}()
	query := `SELECT id FROM nodes WHERE id IN (` + inPlaceholders(len(ids)) + `) AND view_gen = ?`
	driverTiming := nodeKindDriverTiming{}
	queryCtx := context.WithValue(ctx, nodeKindGateTimingKey{}, &driverTiming)
	queryStart := time.Now()
	rows, err := s.db.QueryContext(queryCtx, query, append(toAnyArgs(ids), s.viewGen)...)
	timing.QueryStart = time.Since(queryStart)
	timing.PreDriver = timing.QueryStart
	if !driverTiming.firstEntry.IsZero() {
		timing.PreDriver = driverTiming.firstEntry.Sub(queryStart)
	}
	timing.Gate = driverTiming.gate
	timing.DriverEntries = driverTiming.entries
	if err != nil {
		return nil, fmt.Errorf("get node presence by ids: %w", err)
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
	out = make(map[string]struct{}, len(ids))
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan node presence by id: %w", err)
		}
		out[id] = struct{}{}
		timing.Rows++
	}
	if err = rows.Err(); err != nil {
		return nil, fmt.Errorf("read node presence by ids: %w", err)
	}
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	return out, nil
}
