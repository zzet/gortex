package store_sqlite

import (
	"context"
	"database/sql"
	"github.com/zzet/gortex/internal/graph"
)

var _ graph.ConstantValueProjectionReader = (*Store)(nil)
var _ graph.ConstantValueContextReader = (*Store)(nil)

// ReadConstantValueProjectionContext reads values, node presence and accepted
// file inventory in one physical read transaction. All predicates are bounded
// primary-key batches; an empty request still checks the handle lifetime.
func (s *Store) ReadConstantValueProjectionContext(ctx context.Context, ids []string, files []graph.ConstantFileKey) (graph.ConstantValueProjection, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return graph.ConstantValueProjection{}, err
	}
	if s.coreless() || s.db == nil {
		return graph.ConstantValueProjection{}, sql.ErrConnDone
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return graph.ConstantValueProjection{}, err
	}
	defer func() { _ = tx.Rollback() }()
	p := graph.ConstantValueProjection{Rows: make(map[string]graph.ScopedConstantValueRow), Nodes: make(map[string]string), Files: make(map[graph.ConstantFileKey]graph.FileMetaRow)}
	ids = dedupeNonEmpty(ids)
	for start := 0; start < len(ids); start += lookupChunkSize {
		chunk := ids[start:min(start+lookupChunkSize, len(ids))]
		args := append([]any{s.viewGen}, toAnyArgs(chunk)...)
		query := `SELECT node_id,repo_prefix,file_path,value FROM constant_values WHERE view_gen=? AND node_id IN (` + inPlaceholders(len(chunk)) + `)`
		rows, err := tx.QueryContext(ctx, query, args...)
		if err != nil {
			return graph.ConstantValueProjection{}, err
		}
		for rows.Next() {
			var row graph.ScopedConstantValueRow
			if err := rows.Scan(&row.NodeID, &row.RepoPrefix, &row.FilePath, &row.Value); err != nil {
				_ = rows.Close()
				return graph.ConstantValueProjection{}, err
			}
			p.Rows[row.NodeID] = row
			files = append(files, graph.ConstantFileKey{RepoPrefix: row.RepoPrefix, FilePath: row.FilePath})
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return graph.ConstantValueProjection{}, err
		}
		if err := rows.Close(); err != nil {
			return graph.ConstantValueProjection{}, err
		}
		rows, err = tx.QueryContext(ctx, `SELECT id,file_path FROM nodes WHERE view_gen=? AND id IN (`+inPlaceholders(len(chunk))+`)`, args...)
		if err != nil {
			return graph.ConstantValueProjection{}, err
		}
		for rows.Next() {
			var id, path string
			if err := rows.Scan(&id, &path); err != nil {
				_ = rows.Close()
				return graph.ConstantValueProjection{}, err
			}
			p.Nodes[id] = path
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return graph.ConstantValueProjection{}, err
		}
		if err := rows.Close(); err != nil {
			return graph.ConstantValueProjection{}, err
		}
	}
	byRepo := make(map[string][]string)
	seen := make(map[graph.ConstantFileKey]bool)
	for _, key := range files {
		if !seen[key] {
			seen[key] = true
			byRepo[key.RepoPrefix] = append(byRepo[key.RepoPrefix], key.FilePath)
		}
	}
	for repo, paths := range byRepo {
		for start := 0; start < len(paths); start += lookupChunkSize {
			chunk := paths[start:min(start+lookupChunkSize, len(paths))]
			args := append([]any{s.viewGen, repo}, toAnyArgs(chunk)...)
			rows, err := tx.QueryContext(ctx, `SELECT file_path,content_hash,size,node_count,errors FROM files WHERE view_gen=? AND repo_prefix=? AND file_path IN (`+inPlaceholders(len(chunk))+`)`, args...)
			if err != nil {
				return graph.ConstantValueProjection{}, err
			}
			for rows.Next() {
				var row graph.FileMetaRow
				if err := rows.Scan(&row.FilePath, &row.ContentHash, &row.Size, &row.NodeCount, &row.Errors); err != nil {
					_ = rows.Close()
					return graph.ConstantValueProjection{}, err
				}
				p.Files[graph.ConstantFileKey{RepoPrefix: repo, FilePath: row.FilePath}] = row
			}
			if err := rows.Err(); err != nil {
				_ = rows.Close()
				return graph.ConstantValueProjection{}, err
			}
			if err := rows.Close(); err != nil {
				return graph.ConstantValueProjection{}, err
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return graph.ConstantValueProjection{}, err
	}
	return p, nil
}
func (s *Store) ConstantValuesByNodeIDsContext(ctx context.Context, ids []string) (map[string]string, error) {
	return graph.ConstantValuesByNodeIDsContext(ctx, s, ids)
}
