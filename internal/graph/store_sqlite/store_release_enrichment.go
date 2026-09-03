package store_sqlite

import (
	"database/sql"

	"github.com/zzet/gortex/internal/graph"
)

var (
	_ graph.ReleaseEnrichmentWriter = (*Store)(nil)
	_ graph.ReleaseEnrichmentReader = (*Store)(nil)
)

// releaseChunk bounds rows per multi-row INSERT (4 cols → 4 params/row).
const releaseChunk = 240

const releaseCols = `node_id, repo_prefix, added_in`

// BulkSetReleases persists release rows for one repo prefix, chunked.
func (s *Store) BulkSetReleases(repoPrefix string, rows []graph.ReleaseEnrichment) error {
	if len(rows) == 0 {
		return nil
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	tx, err := s.beginWrite()
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck

	for start := 0; start < len(rows); start += releaseChunk {
		end := start + releaseChunk
		if end > len(rows) {
			end = len(rows)
		}
		batch := rows[start:end]
		args := make([]any, 0, len(batch)*4)
		stmt := make([]byte, 0, 96+len(batch)*12)
		stmt = append(stmt, "INSERT OR REPLACE INTO release_enrichment (view_gen, "...)
		stmt = append(stmt, releaseCols...)
		stmt = append(stmt, ") VALUES "...)
		for i, e := range batch {
			if i > 0 {
				stmt = append(stmt, ',')
			}
			stmt = append(stmt, "(?,?,?,?)"...)
			args = append(args, s.viewGen, e.NodeID, repoPrefix, e.AddedIn)
		}
		if _, err := tx.Exec(string(stmt), args...); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// DeleteReleases drops release rows for the supplied node ids, chunked.
func (s *Store) DeleteReleases(nodeIDs []string) error {
	if len(nodeIDs) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(nodeIDs))
	uniq := make([]string, 0, len(nodeIDs))
	for _, id := range nodeIDs {
		if id == "" {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		uniq = append(uniq, id)
	}
	if len(uniq) == 0 {
		return nil
	}

	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	tx, err := s.beginWrite()
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck

	for start := 0; start < len(uniq); start += releaseChunk {
		end := start + releaseChunk
		if end > len(uniq) {
			end = len(uniq)
		}
		chunk := uniq[start:end]
		args := make([]any, 0, len(chunk)+1)
		args = append(args, s.viewGen)
		stmt := make([]byte, 0, 56+len(chunk)*2)
		stmt = append(stmt, "DELETE FROM release_enrichment WHERE view_gen = ? AND node_id IN ("...)
		for i, id := range chunk {
			if i > 0 {
				stmt = append(stmt, ',')
			}
			stmt = append(stmt, '?')
			args = append(args, id)
		}
		stmt = append(stmt, ')')
		if _, err := tx.Exec(string(stmt), args...); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ReleaseRows returns release rows for repoPrefix; empty → all repos.
func (s *Store) ReleaseRows(repoPrefix string) []graph.ReleaseEnrichment {
	var (
		rows *sql.Rows
		err  error
	)
	if repoPrefix == "" {
		rows, err = s.db.Query(`SELECT `+releaseCols+` FROM release_enrichment WHERE view_gen = ?`, s.viewGen)
	} else {
		rows, err = s.db.Query(`SELECT `+releaseCols+` FROM release_enrichment WHERE view_gen = ? AND repo_prefix = ? AND repo_prefix <> ''`, s.viewGen, repoPrefix)
	}
	if err != nil {
		return nil
	}
	defer rows.Close()

	var out []graph.ReleaseEnrichment
	for rows.Next() {
		var e graph.ReleaseEnrichment
		if err := rows.Scan(&e.NodeID, &e.RepoPrefix, &e.AddedIn); err != nil {
			return out
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return out
	}
	return out
}
