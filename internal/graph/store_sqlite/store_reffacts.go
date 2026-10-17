package store_sqlite

import (
	"strings"

	"github.com/zzet/gortex/internal/graph"
)

// Compile-time assertions that the SQLite Store satisfies the optional
// reference-facts persistence capability. Persisting resolved-reference facts
// in the same backend the graph lives in makes a reference's resolution an
// auditable, diffable record and a warm-restart seed.
var (
	_ graph.RefFactsWriter = (*Store)(nil)
	_ graph.RefFactsReader = (*Store)(nil)
)

// refFactChunk bounds rows per multi-row INSERT. 12 params/row; 80 rows = 960
// host params, under SQLite's 999 default.
const refFactChunk = 80

// candidate-list separator (unit separator — never appears in identifiers).
const refFactCandSep = "\x1f"

func encodeCandidates(c []string) string { return strings.Join(c, refFactCandSep) }

func decodeCandidates(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(s, refFactCandSep)
}

// BulkSetRefFacts persists resolved-reference facts for one repo prefix in a
// single transaction, chunked under the host-parameter limit. Idempotent on
// (repo_prefix, from_id, to_id, kind, line). Empty input is a no-op.
func (s *Store) BulkSetRefFacts(repoPrefix string, facts []graph.RefFact) error {
	if len(facts) == 0 {
		return nil
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	tx, err := s.beginWrite()
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // rollback after Commit is a no-op

	for start := 0; start < len(facts); start += refFactChunk {
		end := start + refFactChunk
		if end > len(facts) {
			end = len(facts)
		}
		batch := facts[start:end]
		args := make([]any, 0, len(batch)*12)
		stmt := make([]byte, 0, 96+len(batch)*24)
		stmt = append(stmt, "INSERT OR REPLACE INTO ref_facts (view_gen, repo_prefix, from_id, to_id, kind, ref_name, line, origin, tier, candidates, file_path, lang) VALUES "...)
		for i, f := range batch {
			if i > 0 {
				stmt = append(stmt, ',')
			}
			stmt = append(stmt, "(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)"...)
			args = append(args, s.viewGen, repoPrefix, f.FromID, f.ToID, f.Kind, f.RefName, f.Line, f.Origin, f.Tier, encodeCandidates(f.Candidates), f.FilePath, f.Lang)
		}
		if _, err := tx.Exec(string(stmt), args...); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// DeleteRefFactsByFiles drops all facts sourced in the supplied files for one
// repo prefix, chunked into `file_path IN (…)` DELETEs. Empty input is a no-op.
func (s *Store) DeleteRefFactsByFiles(repoPrefix string, files []string) error {
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

	byFile := false
	if err := tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type = 'index' AND name = ?)`,
		refFactsByFileIndexName).Scan(&byFile); err != nil {
		byFile = false
	}
	for start := 0; start < len(files); start += refFactChunk {
		end := start + refFactChunk
		if end > len(files) {
			end = len(files)
		}
		chunk := files[start:end]
		args := make([]any, 0, len(chunk)+2)
		args = append(args, s.viewGen, repoPrefix)
		stmt := make([]byte, 0, 96+len(chunk)*2)
		stmt = append(stmt, refFactsDeleteByFilesPrefix(byFile)...)
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

// LoadRefFactsByFiles returns the persisted facts for one repo prefix, scoped
// to the given files (all files when files is empty). Always non-nil.
func (s *Store) LoadRefFactsByFiles(repoPrefix string, files []string) ([]graph.RefFact, error) {
	out := []graph.RefFact{}
	scan := func(query string, args ...any) error {
		rows, err := s.db.Query(query, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var f graph.RefFact
			var cand string
			if err := rows.Scan(&f.FromID, &f.ToID, &f.Kind, &f.RefName, &f.Line, &f.Origin, &f.Tier, &cand, &f.FilePath, &f.Lang); err != nil {
				return err
			}
			f.RepoPrefix = repoPrefix
			f.Candidates = decodeCandidates(cand)
			out = append(out, f)
		}
		return rows.Err()
	}
	const cols = `from_id, to_id, kind, ref_name, line, origin, tier, candidates, file_path, lang`
	if len(files) == 0 {
		if err := scan(`SELECT `+cols+` FROM ref_facts WHERE view_gen = ? AND repo_prefix = ?`, s.viewGen, repoPrefix); err != nil {
			return nil, err
		}
		return out, nil
	}
	indexed := s.refFactsIndexPresent(refFactsByFileIndexName)
	for start := 0; start < len(files); start += refFactChunk {
		end := start + refFactChunk
		if end > len(files) {
			end = len(files)
		}
		filesJSON, ok := projectionJSON(files[start:end])
		if !ok {
			continue
		}
		if err := scan(refFactsByFilesSQL(indexed), filesJSON, s.viewGen, repoPrefix); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// refFactsByFilesSQL reads one chunk of files' facts. The file list drives
// (CROSS JOIN: SQLite never reorders its left operand) and, with the index
// present, the right side is pinned to ref_facts_by_file, so each file is one
// (view_gen, repo_prefix, file_path) seek; without statistics for ref_facts
// the planner otherwise still prefers the primary key's prefix. The plain
// `file_path IN (…)` form let the planner take the WITHOUT ROWID primary
// key's (view_gen, repo_prefix) prefix instead — every fact of the repository
// in the generation per chunk (161k rows, 2.1 s cold on the live store against
// 17 ms for the seek). ORDER BY restores the primary-key order the prefix scan
// returned, so callers see the same rows in the same order.
func refFactsByFilesSQL(indexed bool) string {
	pin := ""
	if indexed {
		pin = " INDEXED BY " + refFactsByFileIndexName
	}
	return `SELECT r.from_id, r.to_id, r.kind, r.ref_name, r.line, r.origin, r.tier, r.candidates, r.file_path, r.lang
FROM json_each(?) AS f
CROSS JOIN ref_facts AS r` + pin + ` ON r.view_gen = ? AND r.repo_prefix = ? AND r.file_path = CAST(f.value AS TEXT)
ORDER BY r.from_id, r.to_id, r.kind, r.line`
}

// refFactsByTargetsSQL is refFactsByFilesSQL's reverse lookup: the chunk's
// target ids drive, each one ref_facts_by_target seek.
func refFactsByTargetsSQL(indexed bool) string {
	pin := ""
	if indexed {
		pin = " INDEXED BY " + refFactsByTargetIndexName
	}
	return `SELECT r.from_id, r.to_id, r.kind, r.ref_name, r.line, r.origin, r.tier, r.candidates, r.file_path, r.lang
FROM json_each(?) AS t
CROSS JOIN ref_facts AS r` + pin + ` ON r.view_gen = ? AND r.repo_prefix = ? AND r.to_id = CAST(t.value AS TEXT)
ORDER BY r.from_id, r.to_id, r.kind, r.line`
}

// refFactsByTargetIndexName is the (view_gen, repo_prefix, to_id) index the
// reverse lookup is pinned to.
const refFactsByTargetIndexName = "ref_facts_by_target"

// refFactsIndexPresent reports whether a ref_facts index exists. It is probed
// per call (one catalog lookup): the indexes are optional, and an INDEXED BY
// naming a missing index fails the statement.
func (s *Store) refFactsIndexPresent(name string) bool {
	var present bool
	if err := s.db.QueryRow(`SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type = 'index' AND name = ?)`, name).Scan(&present); err != nil {
		return false
	}
	return present
}

// refFactsByFileIndexName is the (view_gen, repo_prefix, file_path) index a
// per-file delete is pinned to.
const refFactsByFileIndexName = "ref_facts_by_file"

// refFactsDeleteByFilesPrefix is the per-file delete up to its IN list. With
// ref_facts_by_file present it is pinned there (INDEXED BY): unpinned, the
// WITHOUT ROWID primary key's (view_gen, repo_prefix) prefix wins and a
// one-file delete visits every fact of the repository in the generation. The
// index is optional, so without it the unpinned form keeps working.
func refFactsDeleteByFilesPrefix(indexed bool) string {
	if indexed {
		return "DELETE FROM ref_facts INDEXED BY " + refFactsByFileIndexName + " WHERE view_gen = ? AND repo_prefix = ? AND file_path IN ("
	}
	return "DELETE FROM ref_facts WHERE view_gen = ? AND repo_prefix = ? AND file_path IN ("
}

// LoadRefFactsByTargets returns the persisted facts that resolve TO any of
// the given node IDs for one repo prefix, grouped by source file path — the
// reverse lookup incremental re-resolution uses to find the files that
// referenced a changed symbol after its live in-edges were evicted. Served by
// the ref_facts_by_target index, chunked under the host-parameter limit.
// Always non-nil; empty input is a no-op.
func (s *Store) LoadRefFactsByTargets(repoPrefix string, targetIDs []string) (map[string][]graph.RefFact, error) {
	out := map[string][]graph.RefFact{}
	if len(targetIDs) == 0 {
		return out, nil
	}
	indexed := s.refFactsIndexPresent(refFactsByTargetIndexName)
	for start := 0; start < len(targetIDs); start += refFactChunk {
		end := start + refFactChunk
		if end > len(targetIDs) {
			end = len(targetIDs)
		}
		targetsJSON, ok := projectionJSON(targetIDs[start:end])
		if !ok {
			continue
		}
		rows, err := s.db.Query(refFactsByTargetsSQL(indexed), targetsJSON, s.viewGen, repoPrefix)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var f graph.RefFact
			var cand string
			if err := rows.Scan(&f.FromID, &f.ToID, &f.Kind, &f.RefName, &f.Line, &f.Origin, &f.Tier, &cand, &f.FilePath, &f.Lang); err != nil {
				_ = rows.Close()
				return nil, err
			}
			f.RepoPrefix = repoPrefix
			f.Candidates = decodeCandidates(cand)
			out[f.FilePath] = append(out[f.FilePath], f)
		}
		err = rows.Err()
		_ = rows.Close()
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}
