package store_sqlite

import (
	"encoding/json"
	"fmt"

	"github.com/zzet/gortex/internal/graph"
)

var _ graph.RefFactsRebuilder = (*Store)(nil)

const refFactColumns = `view_gen, repo_prefix, from_id, to_id, kind, ref_name, line, origin, tier, candidates, file_path, lang`

// refFactEligiblePredicate is the SQL spelling of graph.IsResolvableRefEdge,
// graph.IsUnresolvedTarget, and graph.IsStub. Keep the string predicates
// case-sensitive: graph IDs and the Go helpers are case-sensitive too.
const refFactEligiblePredicate = `
e.kind IN ('calls', 'references', 'reads', 'writes', 'typed_as', 'returns', 'instantiates', 'implements', 'extends', 'composes')
AND e.to_id <> ''
AND substr(e.to_id, 1, 12) <> 'unresolved::'
AND instr(e.to_id, '::unresolved::') = 0
AND substr(e.to_id, 1, 8) <> 'stdlib::'
AND substr(e.to_id, 1, 15) <> 'external_call::'
AND substr(e.to_id, 1, 9) <> 'builtin::'
AND substr(e.to_id, 1, 8) <> 'module::'
AND NOT (
    instr(e.to_id, '::') > 0
    AND (
        substr(e.to_id, instr(e.to_id, '::') + 2, 8) = 'stdlib::'
        OR substr(e.to_id, instr(e.to_id, '::') + 2, 15) = 'external_call::'
        OR substr(e.to_id, instr(e.to_id, '::') + 2, 9) = 'builtin::'
        OR substr(e.to_id, instr(e.to_id, '::') + 2, 8) = 'module::'
    )
)`

// refFactOriginExpr mirrors graph.DefaultOriginFor for the resolvable edge
// kinds. Explicit Origin always wins; semantic_source supplies the LSP tier;
// the remaining structural/confidence rules are identical to the Go helper.
const refFactOriginExpr = `CASE
    WHEN e.origin <> '' THEN e.origin
    WHEN COALESCE(e.semantic_source, '') <> '' THEN
        CASE WHEN e.kind = 'implements' THEN 'lsp_dispatch' ELSE 'lsp_resolved' END
    WHEN e.kind IN ('implements', 'extends', 'composes') THEN 'ast_resolved'
    WHEN e.confidence >= 0.9 THEN 'ast_resolved'
    WHEN e.confidence >= 0.5 THEN 'ast_inferred'
    ELSE 'text_matched'
END`

const refFactInsertPrefix = `INSERT OR REPLACE INTO ref_facts (` + refFactColumns + `)
WITH selected AS (
    SELECT n.repo_prefix, e.from_id, e.to_id, e.kind,
           COALESCE(t.name, '') AS ref_name, e.line,
           ` + refFactOriginExpr + ` AS effective_origin,
           n.file_path, n.language
`

// Two view_gen placeholders trail every caller's own: SQLite numbers unnamed
// parameters by their position in the statement text, and every placeholder a
// caller supplies lives in the CTE's FROM clause, ahead of these. The first
// scopes the source corpus the facts are derived from, the second stamps the
// generation the rows are written at; the joins pair the two endpoint sides
// with the edge so a foreign-generation node cannot name a fact here.
const refFactInsertSuffix = `
    LEFT JOIN nodes AS t ON t.id = e.to_id AND t.view_gen = e.view_gen
    WHERE ` + refFactEligiblePredicate + `
      AND n.view_gen = ?
)
SELECT ?, repo_prefix, from_id, to_id, kind, ref_name, line, effective_origin,
       CASE effective_origin
           WHEN 'lsp_resolved' THEN 'lsp'
           WHEN 'lsp_dispatch' THEN 'lsp'
           WHEN 'ast_resolved' THEN 'ast'
           ELSE 'heuristic'
       END,
       '', file_path, language
FROM selected`

// RebuildRefFactsForRepos atomically replaces facts for exact repository
// prefixes. nil means the whole graph; an allocated empty slice is a no-op.
// Repository ownership and every fact field are projected inside SQLite, so
// no node/edge/Meta corpus crosses into Go.
func (s *Store) RebuildRefFactsForRepos(repoPrefixes []string) error {
	_, err := s.rebuildRefFactsForRepos(repoPrefixes)
	return err
}

func (s *Store) rebuildRefFactsForRepos(repoPrefixes []string) (statements int, err error) {
	if repoPrefixes != nil && len(repoPrefixes) == 0 {
		return 0, nil
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	tx, err := s.beginWrite()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback() //nolint:errcheck // no-op after Commit

	var insert string
	if repoPrefixes == nil {
		if _, err := tx.Exec(`DELETE FROM ref_facts WHERE view_gen = ?`, s.viewGen); err != nil {
			return statements, err
		}
		statements++
		insert = refFactInsertPrefix + `    FROM nodes AS n
    JOIN edges AS e INDEXED BY edges_by_from ON e.from_id = n.id AND e.view_gen = n.view_gen` + refFactInsertSuffix
		if _, err := tx.Exec(insert, s.viewGen, s.viewGen); err != nil {
			return statements, err
		}
		statements++
	} else {
		reposJSON, err := json.Marshal(dedupeNonEmptyKeepingBlank(repoPrefixes))
		if err != nil {
			return statements, err
		}
		if string(reposJSON) == "[]" {
			return 0, nil
		}
		if _, err := tx.Exec(`DELETE FROM ref_facts
WHERE view_gen = ?
  AND repo_prefix IN (SELECT CAST(value AS TEXT) FROM json_each(?))`, s.viewGen, string(reposJSON)); err != nil {
			return statements, err
		}
		statements++
		insert = refFactInsertPrefix + `    FROM json_each(?) AS requested
    JOIN nodes AS n ON n.repo_prefix = CAST(requested.value AS TEXT)
    JOIN edges AS e INDEXED BY edges_by_from ON e.from_id = n.id AND e.view_gen = n.view_gen` + refFactInsertSuffix
		if _, err := tx.Exec(insert, string(reposJSON), s.viewGen, s.viewGen); err != nil {
			return statements, err
		}
		statements++
	}
	if err := tx.Commit(); err != nil {
		return statements, err
	}
	return statements, nil
}

// refFactFileProjection starts with the requested files, not the whole repo.
// Fact identity omits edge.file_path: collapse collisions before comparing
// payloads, with highest edge rowid as a deterministic winner matching the
// existing adjacency traversal. Otherwise colliding facts can oscillate even
// when the graph is unchanged. Each statement materializes its own desired
// set within the same transaction, avoiding a projection per old fact.
const refFactFileProjection = `WITH selected AS (
    SELECT n.repo_prefix, e.from_id, e.to_id, e.kind,
           COALESCE(t.name, '') AS ref_name, e.line,
           ` + refFactOriginExpr + ` AS effective_origin,
           n.file_path, n.language, e.id AS edge_id
    FROM json_each(?) AS requested
    CROSS JOIN nodes AS n
      ON n.repo_prefix = ? AND n.file_path = CAST(requested.value AS TEXT)
    CROSS JOIN edges AS e INDEXED BY edges_by_from
      ON e.from_id = n.id AND e.view_gen = n.view_gen
    LEFT JOIN nodes AS t ON t.id = e.to_id AND t.view_gen = e.view_gen
    WHERE ` + refFactEligiblePredicate + ` AND n.view_gen = ?
), ranked AS (
    SELECT ? AS view_gen, repo_prefix, from_id, to_id, kind, ref_name, line,
           effective_origin AS origin,
           CASE effective_origin
               WHEN 'lsp_resolved' THEN 'lsp'
               WHEN 'lsp_dispatch' THEN 'lsp'
               WHEN 'ast_resolved' THEN 'ast'
               ELSE 'heuristic'
           END AS tier,
           '' AS candidates, file_path, language AS lang,
           ROW_NUMBER() OVER (
               PARTITION BY repo_prefix, from_id, to_id, kind, line
               ORDER BY edge_id DESC
           ) AS fact_rank
    FROM selected
), desired AS MATERIALIZED (
    SELECT ` + refFactColumns + ` FROM ranked WHERE fact_rank = 1
)
`

const refFactDeleteObsoleteSuffix = `WHERE view_gen = ? AND repo_prefix = ?
  AND file_path IN (SELECT CAST(value AS TEXT) FROM json_each(?))
  AND NOT EXISTS (
      SELECT 1 FROM desired AS d
      WHERE d.view_gen = ref_facts.view_gen AND d.repo_prefix = ref_facts.repo_prefix
        AND d.from_id = ref_facts.from_id AND d.to_id = ref_facts.to_id
        AND d.kind = ref_facts.kind AND d.line = ref_facts.line
  )`

const refFactDeleteObsolete = refFactFileProjection + "DELETE FROM ref_facts\n" + refFactDeleteObsoleteSuffix

// The outer delete must seek the requested files just like DeleteRefFactsByFiles.
// Without a pin, sparse statistics select the repository-wide primary-key prefix.
func refFactDeleteObsoleteSQL(indexed bool) string {
	if indexed {
		return refFactFileProjection + "DELETE FROM ref_facts INDEXED BY " + refFactsByFileIndexName + "\n" + refFactDeleteObsoleteSuffix
	}
	return refFactDeleteObsolete
}

const refFactUpsertChanged = refFactFileProjection + `INSERT INTO ref_facts (` + refFactColumns + `)
SELECT ` + refFactColumns + ` FROM desired WHERE true
ON CONFLICT (view_gen, repo_prefix, from_id, to_id, kind, line) DO UPDATE SET
    ref_name = excluded.ref_name, origin = excluded.origin, tier = excluded.tier,
    candidates = excluded.candidates, file_path = excluded.file_path, lang = excluded.lang
WHERE ref_facts.ref_name IS NOT excluded.ref_name
   OR ref_facts.origin IS NOT excluded.origin
   OR ref_facts.tier IS NOT excluded.tier
   OR ref_facts.candidates IS NOT excluded.candidates
   OR ref_facts.file_path IS NOT excluded.file_path
   OR ref_facts.lang IS NOT excluded.lang`

// ReplaceRefFactsForFiles atomically applies only changed reference facts in
// the exact file frontier. Unchanged payloads perform no persistent writes.
// Deletion remains repo-scoped so removed/empty files lose stale facts without
// disturbing a same-named file in another repository or generation.
func (s *Store) ReplaceRefFactsForFiles(repoPrefix string, files []string) error {
	_, err := s.replaceRefFactsForFiles(repoPrefix, files)
	return err
}

func (s *Store) replaceRefFactsForFiles(repoPrefix string, files []string) (statements int, err error) {
	files = dedupeNonEmpty(files)
	if len(files) == 0 {
		return 0, nil
	}
	filesJSON, err := json.Marshal(files)
	if err != nil {
		return 0, err
	}

	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	tx, err := s.beginWrite()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback() //nolint:errcheck // no-op after Commit

	byFile := false
	if err := tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type = 'index' AND name = ?)`,
		refFactsByFileIndexName).Scan(&byFile); err != nil {
		byFile = false
	}
	if _, err := tx.Exec(refFactDeleteObsoleteSQL(byFile),
		string(filesJSON), repoPrefix, s.viewGen, s.viewGen,
		s.viewGen, repoPrefix, string(filesJSON)); err != nil {
		return statements, err
	}
	statements++
	if _, err := tx.Exec(refFactUpsertChanged, string(filesJSON), repoPrefix, s.viewGen, s.viewGen); err != nil {
		return statements, fmt.Errorf("ref-facts refill: %w", err)
	}
	statements++
	if err := tx.Commit(); err != nil {
		return statements, err
	}
	return statements, nil
}

func dedupeNonEmptyKeepingBlank(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	return out
}
