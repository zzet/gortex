package store_sqlite

import (
	"context"
	"fmt"

	"github.com/zzet/gortex/internal/graph"
)

var _ graph.NodeKindsByIDsReader = (*Store)(nil)

// GetNodeKindsByIDsContext projects only structural columns for requested
// identities in this exact physical generation. Each query has at most the
// existing lookup chunk size; no node body or metadata codec is touched.
func (s *Store) GetNodeKindsByIDsContext(ctx context.Context, ids []string) (map[string]graph.NodeKindRow, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	ids = dedupeNonEmpty(ids)
	out := make(map[string]graph.NodeKindRow, len(ids))
	for start := 0; start < len(ids); start += lookupChunkSize {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		chunk := ids[start:min(start+lookupChunkSize, len(ids))]
		if observer := graph.NodeKindReadObserver(ctx); observer != nil {
			batch, err := s.observedNodeKindsBatch(ctx, chunk, observer)
			if err != nil {
				return nil, err
			}
			for id, row := range batch {
				out[id] = row
			}
			continue
		}
		q := `SELECT id, kind, file_path, repo_prefix FROM nodes WHERE id IN (` + inPlaceholders(len(chunk)) + `) AND view_gen = ?`
		rows, err := s.db.QueryContext(ctx, q, append(toAnyArgs(chunk), s.viewGen)...)
		if err != nil {
			return nil, fmt.Errorf("get node kinds by ids: %w", err)
		}
		for rows.Next() {
			var id string
			var row graph.NodeKindRow
			if err := rows.Scan(&id, &row.Kind, &row.FilePath, &row.RepoPrefix); err != nil {
				_ = rows.Close()
				return nil, fmt.Errorf("scan node kind by id: %w", err)
			}
			out[id] = row
		}
		err = rows.Err()
		closeErr := rows.Close()
		if err != nil {
			return nil, fmt.Errorf("read node kinds by ids: %w", err)
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
