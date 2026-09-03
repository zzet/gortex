package store_sqlite

import "github.com/zzet/gortex/internal/graph"

var _ graph.SemanticBindingTypeStore = (*Store)(nil)

// semanticBindingChunk keeps VALUES/INSERT statements below SQLite's
// conservative 999-host-parameter limit (6 parameters per persisted row,
// 4 per lookup row).
const semanticBindingChunk = 150

// ReplaceSemanticBindingTypes atomically replaces the compact compiler-binding
// index for one repository. An empty row set deliberately clears the repo.
func (s *Store) ReplaceSemanticBindingTypes(repoPrefix string, rows []graph.SemanticBindingType) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	tx, err := s.beginWrite()
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // rollback after Commit is a no-op

	if _, err := tx.Exec("DELETE FROM semantic_binding_types WHERE view_gen = ? AND repo_prefix = ?", s.viewGen, repoPrefix); err != nil {
		return err
	}
	for start := 0; start < len(rows); start += semanticBindingChunk {
		end := start + semanticBindingChunk
		if end > len(rows) {
			end = len(rows)
		}
		batch := rows[start:end]
		args := make([]any, 0, len(batch)*6)
		stmt := make([]byte, 0, 128+len(batch)*17)
		stmt = append(stmt, "INSERT OR REPLACE INTO semantic_binding_types (view_gen, repo_prefix, file_path, line, name, type_name) VALUES "...)
		for i, row := range batch {
			if i > 0 {
				stmt = append(stmt, ',')
			}
			stmt = append(stmt, "(?, ?, ?, ?, ?, ?)"...)
			args = append(args, s.viewGen, repoPrefix, row.Site.FilePath, row.Site.Line, row.Site.Name, row.TypeName)
		}
		if _, err := tx.Exec(string(stmt), args...); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ReplaceSemanticBindingTypesForFiles atomically drops and replaces bindings
// for the supplied files. Rows for sibling files and repositories survive.
func (s *Store) ReplaceSemanticBindingTypesForFiles(repoPrefix string, files []string, rows []graph.SemanticBindingType) error {
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

	for start := 0; start < len(files); start += semanticBindingChunk {
		end := start + semanticBindingChunk
		if end > len(files) {
			end = len(files)
		}
		chunk := files[start:end]
		args := make([]any, 0, len(chunk)+2)
		args = append(args, s.viewGen, repoPrefix)
		stmt := make([]byte, 0, 96+len(chunk)*2)
		stmt = append(stmt, "DELETE FROM semantic_binding_types WHERE view_gen = ? AND repo_prefix = ? AND file_path IN ("...)
		for i, filePath := range chunk {
			if i > 0 {
				stmt = append(stmt, ',')
			}
			stmt = append(stmt, '?')
			args = append(args, filePath)
		}
		stmt = append(stmt, ')')
		if _, err := tx.Exec(string(stmt), args...); err != nil {
			return err
		}
	}
	for start := 0; start < len(rows); start += semanticBindingChunk {
		end := start + semanticBindingChunk
		if end > len(rows) {
			end = len(rows)
		}
		batch := rows[start:end]
		args := make([]any, 0, len(batch)*6)
		stmt := make([]byte, 0, 128+len(batch)*17)
		stmt = append(stmt, "INSERT OR REPLACE INTO semantic_binding_types (view_gen, repo_prefix, file_path, line, name, type_name) VALUES "...)
		for i, row := range batch {
			if i > 0 {
				stmt = append(stmt, ',')
			}
			stmt = append(stmt, "(?, ?, ?, ?, ?, ?)"...)
			args = append(args, s.viewGen, repoPrefix, row.Site.FilePath, row.Site.Line, row.Site.Name, row.TypeName)
		}
		if _, err := tx.Exec(string(stmt), args...); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// DeleteSemanticBindingTypesByFiles invalidates compiler bindings for changed
// files without touching sibling files or repositories.
func (s *Store) DeleteSemanticBindingTypesByFiles(repoPrefix string, files []string) error {
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

	for start := 0; start < len(files); start += semanticBindingChunk {
		end := start + semanticBindingChunk
		if end > len(files) {
			end = len(files)
		}
		chunk := files[start:end]
		args := make([]any, 0, len(chunk)+2)
		args = append(args, s.viewGen, repoPrefix)
		stmt := make([]byte, 0, 96+len(chunk)*2)
		stmt = append(stmt, "DELETE FROM semantic_binding_types WHERE view_gen = ? AND repo_prefix = ? AND file_path IN ("...)
		for i, filePath := range chunk {
			if i > 0 {
				stmt = append(stmt, ',')
			}
			stmt = append(stmt, '?')
			args = append(args, filePath)
		}
		stmt = append(stmt, ')')
		if _, err := tx.Exec(string(stmt), args...); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// SemanticBindingTypes performs one predicate-shaped VALUES join per chunk.
// The table's WITHOUT ROWID primary key backs the complete join key, so neither
// the number of persisted bindings nor unrelated repositories affect lookup
// work.
func (s *Store) SemanticBindingTypes(sites []graph.SemanticBindingSite) (map[graph.SemanticBindingSite]string, error) {
	out := make(map[graph.SemanticBindingSite]string, len(sites))
	if len(sites) == 0 {
		return out, nil
	}

	seen := make(map[graph.SemanticBindingSite]struct{}, len(sites))
	unique := make([]graph.SemanticBindingSite, 0, len(sites))
	for _, site := range sites {
		if _, ok := seen[site]; ok {
			continue
		}
		seen[site] = struct{}{}
		unique = append(unique, site)
	}

	for start := 0; start < len(unique); start += semanticBindingChunk {
		end := start + semanticBindingChunk
		if end > len(unique) {
			end = len(unique)
		}
		chunk := unique[start:end]
		args := make([]any, 0, len(chunk)*4)
		stmt := make([]byte, 0, 256+len(chunk)*14)
		stmt = append(stmt, "WITH wanted(repo_prefix, file_path, line, name) AS (VALUES "...)
		for i, site := range chunk {
			if i > 0 {
				stmt = append(stmt, ',')
			}
			stmt = append(stmt, "(?, ?, ?, ?)"...)
			args = append(args, site.RepoPrefix, site.FilePath, site.Line, site.Name)
		}
		stmt = append(stmt, `) SELECT b.repo_prefix, b.file_path, b.line, b.name, b.type_name
FROM wanted AS w
JOIN semantic_binding_types AS b
  ON b.view_gen = ?
 AND b.repo_prefix = w.repo_prefix
 AND b.file_path = w.file_path
 AND b.line = w.line
 AND b.name = w.name`...)
		args = append(args, s.viewGen)

		rows, err := s.db.Query(string(stmt), args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var site graph.SemanticBindingSite
			var typeName string
			if err := rows.Scan(&site.RepoPrefix, &site.FilePath, &site.Line, &site.Name, &typeName); err != nil {
				_ = rows.Close()
				return nil, err
			}
			out[site] = typeName
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return nil, err
		}
		_ = rows.Close()
	}
	return out, nil
}
