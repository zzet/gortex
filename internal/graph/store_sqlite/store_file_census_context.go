package store_sqlite

import (
	"context"
	"fmt"
	"github.com/zzet/gortex/internal/graph"
)

// FileMetasForRepoContext is the cancellable exact-generation accepted-file
// inventory. It returns no partial census on read, scan or cancellation error.
// An empty successful inventory remains non-nil, matching the legacy method.
func (s *Store) FileMetasForRepoContext(ctx context.Context, repoPrefix string) ([]graph.FileMetaRow, error) {
	if ctx == nil {
		return nil, fmt.Errorf("file metadata census: nil context")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT file_path, content_hash, size, node_count, errors FROM files WHERE view_gen = ? AND repo_prefix = ? ORDER BY file_path`,
		s.viewGen, repoPrefix,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []graph.FileMetaRow{}
	for rows.Next() {
		var r graph.FileMetaRow
		if err := rows.Scan(&r.FilePath, &r.ContentHash, &r.Size, &r.NodeCount, &r.Errors); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	return out, nil
}
