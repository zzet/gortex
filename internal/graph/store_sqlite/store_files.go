package store_sqlite

import (
	"github.com/zzet/gortex/internal/graph"
)

// Compile-time assertions that the SQLite Store satisfies the optional
// per-file metadata persistence capability (the files sidecar feeding
// index_health's per-file parse-error / node-count rollup).
var (
	_ graph.FileMetaWriter       = (*Store)(nil)
	_ graph.FileMetaRepoReplacer = (*Store)(nil)
	_ graph.FileMetaReader       = (*Store)(nil)
	_ graph.FileMetaPathReader   = (*Store)(nil)
)

// fileMetaChunk bounds rows per multi-row INSERT (7 params/row; 80 rows =
// 560 host params, well under SQLite's 999 default).
const fileMetaChunk = 80

// SetFileMetas upserts per-file metadata rows for one repo prefix in a single
// transaction, chunked under the host-parameter limit. Idempotent on the
// (repo_prefix, file_path) primary key. Empty input is a no-op.
func (s *Store) SetFileMetas(repoPrefix string, rows []graph.FileMetaRow) error {
	if len(rows) == 0 {
		return nil
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	tx, err := s.beginWrite()
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // rollback after Commit is a no-op

	for start := 0; start < len(rows); start += fileMetaChunk {
		end := start + fileMetaChunk
		if end > len(rows) {
			end = len(rows)
		}
		batch := rows[start:end]
		args := make([]any, 0, len(batch)*7)
		stmt := make([]byte, 0, 96+len(batch)*24)
		stmt = append(stmt, "INSERT OR REPLACE INTO files (view_gen, repo_prefix, file_path, content_hash, size, node_count, errors) VALUES "...)
		for i, r := range batch {
			if i > 0 {
				stmt = append(stmt, ',')
			}
			stmt = append(stmt, "(?, ?, ?, ?, ?, ?, ?)"...)
			args = append(args, s.viewGen, repoPrefix, r.FilePath, r.ContentHash, r.Size, r.NodeCount, r.Errors)
		}
		if _, err := tx.Exec(string(stmt), args...); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ReplaceFileMetas atomically replaces the authoritative repository
// projection. The repo-wide delete is index-backed and inserts remain bounded
// by fileMetaChunk; empty rows intentionally clear stale metadata.
func (s *Store) ReplaceFileMetas(repoPrefix string, rows []graph.FileMetaRow) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	tx, err := s.beginWrite()
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // rollback after Commit is a no-op
	if _, err := tx.Exec(`DELETE FROM files WHERE view_gen = ? AND repo_prefix = ?`, s.viewGen, repoPrefix); err != nil {
		return err
	}
	for start := 0; start < len(rows); start += fileMetaChunk {
		end := start + fileMetaChunk
		if end > len(rows) {
			end = len(rows)
		}
		batch := rows[start:end]
		args := make([]any, 0, len(batch)*7)
		stmt := make([]byte, 0, 96+len(batch)*24)
		stmt = append(stmt, "INSERT INTO files (view_gen, repo_prefix, file_path, content_hash, size, node_count, errors) VALUES "...)
		for i, row := range batch {
			if i > 0 {
				stmt = append(stmt, ',')
			}
			stmt = append(stmt, "(?, ?, ?, ?, ?, ?, ?)"...)
			args = append(args, s.viewGen, repoPrefix, row.FilePath, row.ContentHash, row.Size, row.NodeCount, row.Errors)
		}
		if _, err := tx.Exec(string(stmt), args...); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// DeleteFileMetasByFiles drops the metadata rows for the supplied files in one
// repo prefix, chunked into `file_path IN (…)` DELETEs. Empty input is a no-op.
func (s *Store) DeleteFileMetasByFiles(repoPrefix string, files []string) error {
	if len(files) == 0 {
		return nil
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	tx, err := s.beginWrite()
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // rollback after Commit is a no-op

	for start := 0; start < len(files); start += fileMetaChunk {
		end := start + fileMetaChunk
		if end > len(files) {
			end = len(files)
		}
		chunk := files[start:end]
		args := make([]any, 0, len(chunk)+2)
		args = append(args, s.viewGen, repoPrefix)
		stmt := make([]byte, 0, 64+len(chunk)*2)
		stmt = append(stmt, "DELETE FROM files WHERE view_gen = ? AND repo_prefix = ? AND file_path IN ("...)
		for i, f := range chunk {
			if i > 0 {
				stmt = append(stmt, ',')
			}
			stmt = append(stmt, '?')
			args = append(args, f)
		}
		stmt = append(stmt, ')')
		if _, err := tx.Exec(string(stmt), args...); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// FileMetasForRepo returns every recorded file row for the repo prefix.
// Always non-nil.
func (s *Store) FileMetasForRepo(repoPrefix string) ([]graph.FileMetaRow, error) {
	rows, err := s.db.Query(
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
	return out, rows.Err()
}

// FileMetasByPaths reads a bounded set of rows through the
// (repo_prefix,file_path) primary key. Chunks stay below SQLite's conservative
// host-parameter limit and avoid both a repository scan and one query per file.
func (s *Store) FileMetasByPaths(repoPrefix string, filePaths []string) (map[string]graph.FileMetaRow, error) {
	out := make(map[string]graph.FileMetaRow, len(filePaths))
	for start := 0; start < len(filePaths); start += fileMetaChunk {
		end := start + fileMetaChunk
		if end > len(filePaths) {
			end = len(filePaths)
		}
		chunk := filePaths[start:end]
		args := make([]any, 0, len(chunk)+2)
		args = append(args, s.viewGen, repoPrefix)
		stmt := make([]byte, 0, 112+len(chunk)*2)
		stmt = append(stmt, "SELECT file_path, content_hash, size, node_count, errors FROM files WHERE view_gen = ? AND repo_prefix = ? AND file_path IN ("...)
		for i, filePath := range chunk {
			if i > 0 {
				stmt = append(stmt, ',')
			}
			stmt = append(stmt, '?')
			args = append(args, filePath)
		}
		stmt = append(stmt, ')')

		rows, err := s.db.Query(string(stmt), args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var row graph.FileMetaRow
			if err := rows.Scan(&row.FilePath, &row.ContentHash, &row.Size, &row.NodeCount, &row.Errors); err != nil {
				rows.Close()
				return nil, err
			}
			out[row.FilePath] = row
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, err
		}
		if err := rows.Close(); err != nil {
			return nil, err
		}
	}
	return out, nil
}
