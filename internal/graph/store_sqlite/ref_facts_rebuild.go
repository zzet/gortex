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

// ReplaceRefFactsForFiles atomically delete-then-refills the exact changed-file
// frontier. The delete is deliberately scoped by repo even though graph paths
// are normally prefixed: stale facts for a now-empty/removed file still need
// deletion, and a same-named file in another repository must survive.
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

	if _, err := tx.Exec(`DELETE FROM ref_facts
WHERE view_gen = ?
  AND repo_prefix = ?
  AND file_path IN (SELECT CAST(value AS TEXT) FROM json_each(?))`, s.viewGen, repoPrefix, string(filesJSON)); err != nil {
		return statements, err
	}
	statements++
	insert := refFactInsertPrefix + `    FROM json_each(?) AS requested
    JOIN nodes AS n
      ON n.repo_prefix = ? AND n.file_path = CAST(requested.value AS TEXT)
    JOIN edges AS e INDEXED BY edges_by_from ON e.from_id = n.id AND e.view_gen = n.view_gen` + refFactInsertSuffix
	if _, err := tx.Exec(insert, string(filesJSON), repoPrefix, s.viewGen, s.viewGen); err != nil {
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
