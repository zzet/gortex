package store_sqlite

import (
	"context"
	"fmt"

	"github.com/zzet/gortex/internal/graph"
)

var _ graph.NodePresenceByIDsReader = (*Store)(nil)

// GetNodePresenceByIDsContext reads only identity from this exact physical
// generation. It retains the existing bounded lookup chunks and checked read
// semantics without decoding kinds, locations or node metadata.
func (s *Store) GetNodePresenceByIDsContext(ctx context.Context, ids []string) (map[string]struct{}, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	ids = dedupeNonEmpty(ids)
	out := make(map[string]struct{}, len(ids))
	for start := 0; start < len(ids); start += lookupChunkSize {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		chunk := ids[start:min(start+lookupChunkSize, len(ids))]
		if observer := graph.NodeKindReadObserver(ctx); observer != nil {
			batch, err := s.observedNodePresenceBatch(ctx, chunk, observer)
			if err != nil {
				return nil, err
			}
			for id := range batch {
				out[id] = struct{}{}
			}
			continue
		}
		q := `SELECT id FROM nodes WHERE id IN (` + inPlaceholders(len(chunk)) + `) AND view_gen = ?`
		rows, err := s.db.QueryContext(ctx, q, append(toAnyArgs(chunk), s.viewGen)...)
		if err != nil {
			return nil, fmt.Errorf("get node presence by ids: %w", err)
		}
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				_ = rows.Close()
				return nil, fmt.Errorf("scan node presence by id: %w", err)
			}
			out[id] = struct{}{}
		}
		err = rows.Err()
		closeErr := rows.Close()
		if err != nil {
			return nil, fmt.Errorf("read node presence by ids: %w", err)
		}
		if closeErr != nil {
			return nil, closeErr
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return out, nil
}
