package store_sqlite

import (
	"github.com/zzet/gortex/internal/graph"
)

// Compile-time assertions that the SQLite Store satisfies the optional
// constant-value persistence capability. A KindConstant node's literal
// value lives in this queryable sidecar (not the JSON Meta blob)
// so the resolver can dereference a const-identifier dispatch name across
// files without an unindexable per-node blob decode.
var (
	_ graph.ConstantValueWriter       = (*Store)(nil)
	_ graph.ConstantValueRepoReplacer = (*Store)(nil)
	_ graph.ConstantValueReader       = (*Store)(nil)
)

// constValueChunk bounds rows per multi-row INSERT (5 params/row; 80 rows
// = 400 host params, well under SQLite's 999 default).
const constValueChunk = 80

// BulkSetConstantValues persists constant values for one repo prefix in a
// single transaction, chunked under the host-parameter limit. Idempotent
// on the node_id primary key. Empty input is a no-op.
func (s *Store) BulkSetConstantValues(repoPrefix string, rows []graph.ConstantValueRow) error {
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

	for start := 0; start < len(rows); start += constValueChunk {
		end := start + constValueChunk
		if end > len(rows) {
			end = len(rows)
		}
		batch := rows[start:end]
		args := make([]any, 0, len(batch)*5)
		stmt := make([]byte, 0, 96+len(batch)*16)
		stmt = append(stmt, "INSERT OR REPLACE INTO constant_values (view_gen, node_id, repo_prefix, file_path, value) VALUES "...)
		for i, r := range batch {
			if i > 0 {
				stmt = append(stmt, ',')
			}
			stmt = append(stmt, "(?, ?, ?, ?, ?)"...)
			args = append(args, s.viewGen, r.NodeID, repoPrefix, r.FilePath, r.Value)
		}
		if _, err := tx.Exec(string(stmt), args...); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ReplaceConstantValues atomically replaces the authoritative repository
// projection. The repo-wide delete is index-backed and the insert remains
// bounded by constValueChunk; empty rows intentionally clear stale values.
func (s *Store) ReplaceConstantValues(repoPrefix string, rows []graph.ConstantValueRow) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	tx, err := s.beginWrite()
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // rollback after Commit is a no-op
	if _, err := tx.Exec(`DELETE FROM constant_values WHERE view_gen = ? AND repo_prefix = ?`, s.viewGen, repoPrefix); err != nil {
		return err
	}
	for start := 0; start < len(rows); start += constValueChunk {
		end := start + constValueChunk
		if end > len(rows) {
			end = len(rows)
		}
		batch := rows[start:end]
		args := make([]any, 0, len(batch)*5)
		stmt := make([]byte, 0, 96+len(batch)*16)
		stmt = append(stmt, "INSERT INTO constant_values (view_gen, node_id, repo_prefix, file_path, value) VALUES "...)
		for i, row := range batch {
			if i > 0 {
				stmt = append(stmt, ',')
			}
			stmt = append(stmt, "(?, ?, ?, ?, ?)"...)
			args = append(args, s.viewGen, row.NodeID, repoPrefix, row.FilePath, row.Value)
		}
		if _, err := tx.Exec(string(stmt), args...); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// DeleteConstantValuesByFiles drops all constant values sourced in the
// supplied files for one repo prefix, chunked into `file_path IN (…)`
// DELETEs. Empty input is a no-op.
func (s *Store) DeleteConstantValuesByFiles(repoPrefix string, files []string) error {
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

	for start := 0; start < len(files); start += constValueChunk {
		end := start + constValueChunk
		if end > len(files) {
			end = len(files)
		}
		chunk := files[start:end]
		args := make([]any, 0, len(chunk)+2)
		args = append(args, s.viewGen, repoPrefix)
		stmt := make([]byte, 0, 64+len(chunk)*2)
		stmt = append(stmt, "DELETE FROM constant_values WHERE view_gen = ? AND repo_prefix = ? AND file_path IN ("...)
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

// ConstantValuesByNodeIDs returns the persisted values for the supplied
// node ids (omitting ids with no recorded value). Always non-nil.
func (s *Store) ConstantValuesByNodeIDs(nodeIDs []string) (map[string]string, error) {
	out := make(map[string]string, len(nodeIDs))
	if len(nodeIDs) == 0 {
		return out, nil
	}
	for start := 0; start < len(nodeIDs); start += constValueChunk {
		end := start + constValueChunk
		if end > len(nodeIDs) {
			end = len(nodeIDs)
		}
		chunk := nodeIDs[start:end]
		args := make([]any, 0, len(chunk)+1)
		args = append(args, s.viewGen)
		stmt := make([]byte, 0, 64+len(chunk)*2)
		stmt = append(stmt, "SELECT node_id, value FROM constant_values WHERE view_gen = ? AND node_id IN ("...)
		for i, id := range chunk {
			if i > 0 {
				stmt = append(stmt, ',')
			}
			stmt = append(stmt, '?')
			args = append(args, id)
		}
		stmt = append(stmt, ')')
		rows, err := s.db.Query(string(stmt), args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var id, val string
			if err := rows.Scan(&id, &val); err != nil {
				_ = rows.Close()
				return nil, err
			}
			out[id] = val
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return nil, err
		}
		_ = rows.Close()
	}
	return out, nil
}
