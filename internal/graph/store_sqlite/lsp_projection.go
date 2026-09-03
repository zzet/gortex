package store_sqlite

import (
	"strings"

	"github.com/zzet/gortex/internal/graph"
)

// LSPRepoFileCounts returns symbol and unstamped-symbol counts grouped by file
// for the requested repository and languages. Both maps contain only symbol
// nodes (file/import nodes are excluded), and the query reads promoted columns
// only; Node.Meta never crosses the SQLite boundary.
func (s *Store) LSPRepoFileCounts(repoPrefix string, languages []string) (map[string]int, map[string]int) {
	totals := make(map[string]int)
	unstamped := make(map[string]int)
	languagesJSON, ok := projectionJSON(languages)
	if !ok {
		return totals, unstamped
	}
	rows, err := s.db.Query(`
WITH requested_languages(language) AS (
    SELECT CAST(value AS TEXT) FROM json_each(?)
)
SELECT n.file_path,
       COUNT(*),
       SUM(CASE WHEN n.semantic_type IS NULL OR n.semantic_type = '' THEN 1 ELSE 0 END)
FROM requested_languages AS l
JOIN nodes AS n ON n.language = l.language
WHERE n.repo_prefix = ?
  AND n.kind NOT IN (?, ?)
  AND n.view_gen = ?
GROUP BY n.file_path
ORDER BY n.file_path`, languagesJSON, repoPrefix, string(graph.KindFile), string(graph.KindImport), s.viewGen)
	if err != nil {
		panicOnFatal(err)
		return totals, unstamped
	}
	defer rows.Close()
	for rows.Next() {
		var filePath string
		var total, pending int
		if err := rows.Scan(&filePath, &total, &pending); err != nil {
			panicOnFatal(err)
			return totals, unstamped
		}
		totals[filePath] = total
		unstamped[filePath] = pending
	}
	if err := rows.Err(); err != nil {
		panicOnFatal(err)
	}
	return totals, unstamped
}

// LSPRepoNodesByFiles returns the language-scoped symbol projection for a
// bounded file frontier. When unstampedOnly is true, SQLite filters already
// enriched nodes through the promoted semantic_type column before decoding.
// The light scanner reconstructs promoted metadata but never reads the opaque
// meta blob, so callers may use these nodes for structural matching only.
func (s *Store) LSPRepoNodesByFiles(repoPrefix string, languages, filePaths []string, unstampedOnly bool) []*graph.Node {
	languagesJSON, ok := projectionJSON(languages)
	if !ok {
		return nil
	}
	filesJSON, ok := projectionJSON(filePaths)
	if !ok {
		return nil
	}
	predicate := ""
	if unstampedOnly {
		predicate = " AND (n.semantic_type IS NULL OR n.semantic_type = '')"
	}
	rows, err := s.db.Query(`
WITH requested_languages(language) AS (
    SELECT CAST(value AS TEXT) FROM json_each(?)
), requested_files(file_path) AS (
    SELECT CAST(value AS TEXT) FROM json_each(?)
)
SELECT `+qualifiedNodeColumns("n", lookupNodeColsLight)+`
FROM requested_files AS f
CROSS JOIN nodes AS n
JOIN requested_languages AS l ON l.language = n.language
WHERE n.file_path = f.file_path
  AND +n.repo_prefix = ?
  AND n.kind NOT IN (?, ?)`+predicate+`
  AND n.view_gen = ?
ORDER BY n.file_path, n.id`, languagesJSON, filesJSON, repoPrefix, string(graph.KindFile), string(graph.KindImport), s.viewGen)
	if err != nil {
		panicOnFatal(err)
		return nil
	}
	defer rows.Close()
	var out []*graph.Node
	for rows.Next() {
		node, err := scanNodeLight(rows)
		if err != nil {
			panicOnFatal(err)
			return out
		}
		out = append(out, node)
	}
	if err := rows.Err(); err != nil {
		panicOnFatal(err)
	}
	return out
}

// LSPRepoConfirmableEdgesByFiles projects only use-site edges whose source is
// in the requested repository/language/file frontier. Structural-containment
// edges cannot be adjudicated by references/definition and are excluded in
// SQLite, preventing the LSP view from materializing the repository's complete
// edge set. ambiguousOnly additionally restricts the result to confidence < 1.
func (s *Store) LSPRepoConfirmableEdgesByFiles(repoPrefix string, languages, filePaths []string, ambiguousOnly bool) []*graph.Edge {
	languagesJSON, ok := projectionJSON(languages)
	if !ok {
		return nil
	}
	filesJSON, ok := projectionJSON(filePaths)
	if !ok {
		return nil
	}
	confidencePredicate := ""
	if ambiguousOnly {
		confidencePredicate = " AND e.confidence < 1.0"
	}
	rows, err := s.db.Query(`
WITH requested_languages(language) AS (
    SELECT CAST(value AS TEXT) FROM json_each(?)
), requested_files(file_path) AS (
    SELECT CAST(value AS TEXT) FROM json_each(?)
)
SELECT `+lookupQualifiedEdgeCols+`
FROM requested_files AS f
CROSS JOIN nodes AS n
JOIN requested_languages AS l ON l.language = n.language
CROSS JOIN edges AS e
WHERE n.file_path = f.file_path
  AND e.from_id = n.id
  AND +n.repo_prefix = ?
  AND e.kind NOT IN (?, ?, ?, ?, ?, ?)`+confidencePredicate+`
  AND n.view_gen = ? AND e.view_gen = n.view_gen
ORDER BY e.from_id, e.to_id, e.kind, e.file_path, e.line`, languagesJSON, filesJSON, repoPrefix,
		string(graph.EdgeMemberOf), string(graph.EdgeDefines), string(graph.EdgeContains),
		string(graph.EdgeParamOf), string(graph.EdgeImports), string(graph.EdgeCaptures), s.viewGen)
	if err != nil {
		panicOnFatal(err)
		return nil
	}
	defer rows.Close()
	var out []*graph.Edge
	for rows.Next() {
		edge, err := s.scanEdgeCursor(rows)
		if err != nil {
			panicOnFatal(err)
			return out
		}
		if edge == nil {
			continue
		}
		out = append(out, edge)
	}
	if err := rows.Err(); err != nil {
		panicOnFatal(err)
	}
	return out
}

// LSPRepoEdgesByFilesAndKinds projects only requested structural edge kinds
// whose source is in the repository/language/file frontier. The kind set is a
// JSON CTE, keeping the operation to one statement regardless of frontier size.
func (s *Store) LSPRepoEdgesByFilesAndKinds(repoPrefix string, languages, filePaths []string, kinds []graph.EdgeKind) []*graph.Edge {
	languagesJSON, ok := projectionJSON(languages)
	if !ok {
		return nil
	}
	filesJSON, ok := projectionJSON(filePaths)
	if !ok || len(kinds) == 0 {
		return nil
	}
	kindValues := make([]string, 0, len(kinds))
	for _, kind := range kinds {
		kindValues = append(kindValues, string(kind))
	}
	kindsJSON, ok := projectionJSON(kindValues)
	if !ok {
		return nil
	}
	rows, err := s.db.Query(`
WITH requested_languages(language) AS (
    SELECT CAST(value AS TEXT) FROM json_each(?)
), requested_files(file_path) AS (
    SELECT CAST(value AS TEXT) FROM json_each(?)
), requested_kinds(kind) AS (
    SELECT CAST(value AS TEXT) FROM json_each(?)
)
SELECT `+lookupQualifiedEdgeCols+`
FROM requested_files AS f
CROSS JOIN nodes AS n
JOIN requested_languages AS l ON l.language = n.language
CROSS JOIN edges AS e
JOIN requested_kinds AS k ON k.kind = e.kind
WHERE n.file_path = f.file_path
  AND e.from_id = n.id
  AND +n.repo_prefix = ?
  AND n.view_gen = ? AND e.view_gen = n.view_gen
ORDER BY e.from_id, e.to_id, e.kind, e.file_path, e.line`, languagesJSON, filesJSON, kindsJSON, repoPrefix, s.viewGen)
	if err != nil {
		panicOnFatal(err)
		return nil
	}
	defer rows.Close()
	var out []*graph.Edge
	for rows.Next() {
		edge, err := s.scanEdgeCursor(rows)
		if err != nil {
			panicOnFatal(err)
			return out
		}
		if edge == nil {
			continue
		}
		out = append(out, edge)
	}
	if err := rows.Err(); err != nil {
		panicOnFatal(err)
	}
	return out
}

// LSPNodeFanInCounts returns exact inbound edge counts for a node frontier as
// one SQLite aggregate. Reference-add ordering needs the counts, not the edge
// payloads; keeping them scalar avoids retaining every inbound edge.
func (s *Store) LSPNodeFanInCounts(nodeIDs []string) map[string]int {
	out := make(map[string]int)
	idsJSON, ok := projectionJSON(nodeIDs)
	if !ok {
		return out
	}
	rows, err := s.db.Query(`
WITH requested_nodes(id) AS (
    SELECT CAST(value AS TEXT) FROM json_each(?)
)
SELECT r.id, COUNT(e.to_id)
FROM requested_nodes AS r
LEFT JOIN edges AS e ON e.to_id = r.id AND e.view_gen = ?
GROUP BY r.id
ORDER BY r.id`, idsJSON, s.viewGen)
	if err != nil {
		panicOnFatal(err)
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var count int
		if err := rows.Scan(&id, &count); err != nil {
			panicOnFatal(err)
			return out
		}
		out[id] = count
	}
	if err := rows.Err(); err != nil {
		panicOnFatal(err)
	}
	return out
}

// LSPInEdgesByNodeIDsAndKinds projects only dispatch-relevant inbound edges
// for a node frontier. Cross-repository overrides/implements/extends remain
// visible without materializing unrelated calls, references, or dataflow.
func (s *Store) LSPInEdgesByNodeIDsAndKinds(nodeIDs []string, kinds []graph.EdgeKind) []*graph.Edge {
	idsJSON, ok := projectionJSON(nodeIDs)
	if !ok || len(kinds) == 0 {
		return nil
	}
	kindValues := make([]string, 0, len(kinds))
	for _, kind := range kinds {
		kindValues = append(kindValues, string(kind))
	}
	kindsJSON, ok := projectionJSON(kindValues)
	if !ok {
		return nil
	}
	rows, err := s.db.Query(`
WITH requested_nodes(id) AS (
    SELECT CAST(value AS TEXT) FROM json_each(?)
), requested_kinds(kind) AS (
    SELECT CAST(value AS TEXT) FROM json_each(?)
)
SELECT `+lookupQualifiedEdgeCols+`
FROM requested_nodes AS r
JOIN edges AS e ON e.to_id = r.id AND e.view_gen = ?
JOIN requested_kinds AS k ON k.kind = e.kind
ORDER BY e.from_id, e.to_id, e.kind, e.file_path, e.line`, idsJSON, kindsJSON, s.viewGen)
	if err != nil {
		panicOnFatal(err)
		return nil
	}
	defer rows.Close()
	var out []*graph.Edge
	for rows.Next() {
		edge, err := s.scanEdgeCursor(rows)
		if err != nil {
			panicOnFatal(err)
			return out
		}
		if edge == nil {
			continue
		}
		out = append(out, edge)
	}
	if err := rows.Err(); err != nil {
		panicOnFatal(err)
	}
	return out
}

// qualifiedNodeColumns qualifies the canonical comma-separated node column
// list for joins while preserving scan order.
func qualifiedNodeColumns(alias, columns string) string {
	return alias + "." + strings.ReplaceAll(columns, ", ", ", "+alias+".")
}
