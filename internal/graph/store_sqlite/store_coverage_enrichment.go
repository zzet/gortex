package store_sqlite

import (
	"database/sql"

	"github.com/zzet/gortex/internal/graph"
)

var (
	_ graph.CoverageEnrichmentWriter = (*Store)(nil)
	_ graph.CoverageEnrichmentReader = (*Store)(nil)
)

// coverageChunk bounds rows per multi-row INSERT (6 cols → 6 params/row;
// 999/6 ≈ 166 max, 160 leaves headroom).
const coverageChunk = 160

const coverageCols = `node_id, repo_prefix, coverage_pct, num_stmt, hit`

// BulkSetCoverage persists coverage rows for one repo prefix in a single
// chunked transaction. Idempotent on node_id. Empty input is a no-op.
func (s *Store) BulkSetCoverage(repoPrefix string, rows []graph.CoverageEnrichment) error {
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

	for start := 0; start < len(rows); start += coverageChunk {
		end := start + coverageChunk
		if end > len(rows) {
			end = len(rows)
		}
		batch := rows[start:end]
		args := make([]any, 0, len(batch)*6)
		stmt := make([]byte, 0, 96+len(batch)*16)
		stmt = append(stmt, "INSERT OR REPLACE INTO coverage_enrichment (view_gen, "...)
		stmt = append(stmt, coverageCols...)
		stmt = append(stmt, ") VALUES "...)
		for i, e := range batch {
			if i > 0 {
				stmt = append(stmt, ',')
			}
			stmt = append(stmt, "(?,?,?,?,?,?)"...)
			args = append(args, s.viewGen, e.NodeID, repoPrefix, e.CoveragePct, e.NumStmt, e.Hit)
		}
		if _, err := tx.Exec(string(stmt), args...); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// DeleteCoverage drops coverage rows for the supplied node ids, chunked.
func (s *Store) DeleteCoverage(nodeIDs []string) error {
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

	for start := 0; start < len(uniq); start += coverageChunk {
		end := start + coverageChunk
		if end > len(uniq) {
			end = len(uniq)
		}
		chunk := uniq[start:end]
		args := make([]any, 0, len(chunk)+1)
		args = append(args, s.viewGen)
		stmt := make([]byte, 0, 56+len(chunk)*2)
		stmt = append(stmt, "DELETE FROM coverage_enrichment WHERE view_gen = ? AND node_id IN ("...)
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

// CoverageRows returns coverage rows for repoPrefix; empty repoPrefix
// returns ALL rows across repos. Index-only read over the enriched set.
func (s *Store) CoverageRows(repoPrefix string) []graph.CoverageEnrichment {
	var (
		rows *sql.Rows
		err  error
	)
	if repoPrefix == "" {
		rows, err = s.db.Query(`SELECT `+coverageCols+` FROM coverage_enrichment WHERE view_gen = ?`, s.viewGen)
	} else {
		rows, err = s.db.Query(`SELECT `+coverageCols+` FROM coverage_enrichment WHERE view_gen = ? AND repo_prefix = ? AND repo_prefix <> ''`, s.viewGen, repoPrefix)
	}
	if err != nil {
		return nil
	}
	defer rows.Close()

	var out []graph.CoverageEnrichment
	for rows.Next() {
		var e graph.CoverageEnrichment
		if err := rows.Scan(&e.NodeID, &e.RepoPrefix, &e.CoveragePct, &e.NumStmt, &e.Hit); err != nil {
			return out
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return out
	}
	return out
}
