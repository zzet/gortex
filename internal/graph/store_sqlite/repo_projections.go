package store_sqlite

import (
	"encoding/json"
	"sort"
	"strings"

	"github.com/zzet/gortex/internal/graph"
)

// projectionJSON returns a stable, duplicate-free JSON array suitable for a
// json_each CTE. One bound JSON value keeps every projection to a single SQL
// statement even when a workspace contains hundreds of repositories.
func projectionJSON(values []string) (string, bool) {
	if len(values) == 0 {
		return "", false
	}
	seen := make(map[string]struct{}, len(values))
	unique := make([]string, 0, len(values))
	for _, value := range values {
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		unique = append(unique, value)
	}
	sort.Strings(unique)
	data, err := json.Marshal(unique)
	if err != nil {
		panicOnFatal(err)
		return "", false
	}
	return string(data), true
}

// RepoLanguageFileCounts projects only the flat repository, file, language,
// kind, and data-class columns. Node.Meta, docs, signatures, and edges never
// cross the SQLite boundary.
func (s *Store) RepoLanguageFileCounts(repoPrefixes []string) []graph.RepoLanguageFileCount {
	reposJSON, ok := projectionJSON(repoPrefixes)
	if !ok {
		return nil
	}
	rows, err := s.db.Query(`
WITH requested(repo_prefix) AS (
    SELECT CAST(value AS TEXT) FROM json_each(?)
)
SELECT n.repo_prefix, n.file_path, n.language, COUNT(*)
FROM requested AS r
JOIN nodes AS n ON n.repo_prefix = r.repo_prefix
WHERE n.language <> ''
  AND n.kind <> ?
  AND (n.kind <> ? OR n.data_class IS NOT 'content')
  AND n.view_gen = ?
GROUP BY n.repo_prefix, n.file_path, n.language
ORDER BY n.repo_prefix, n.file_path, n.language`, reposJSON, string(graph.KindModule), string(graph.KindDoc), s.viewGen)
	if err != nil {
		panicOnFatal(err)
		return nil
	}
	defer rows.Close()

	var out []graph.RepoLanguageFileCount
	for rows.Next() {
		var row graph.RepoLanguageFileCount
		if err := rows.Scan(&row.RepoPrefix, &row.FilePath, &row.Language, &row.Count); err != nil {
			panicOnFatal(err)
			return out
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		panicOnFatal(err)
	}
	return out
}

// RepoLanguageCounts returns node-only language counts for all requested repos
// in one query. It deliberately does not touch the edges table (unlike
// RepoStats), and filters content sections using the promoted data_class column.
func (s *Store) RepoLanguageCounts(repoPrefixes []string) map[string]map[string]int {
	out := make(map[string]map[string]int)
	reposJSON, ok := projectionJSON(repoPrefixes)
	if !ok {
		return out
	}
	rows, err := s.db.Query(`
WITH requested(repo_prefix) AS (
    SELECT CAST(value AS TEXT) FROM json_each(?)
)
SELECT n.repo_prefix, n.language, COUNT(*)
FROM requested AS r
JOIN nodes AS n ON n.repo_prefix = r.repo_prefix
WHERE n.language <> ''
  AND (n.kind <> ? OR n.data_class IS NOT 'content')
  AND n.view_gen = ?
GROUP BY n.repo_prefix, n.language
ORDER BY n.repo_prefix, n.language`, reposJSON, string(graph.KindDoc), s.viewGen)
	if err != nil {
		panicOnFatal(err)
		return out
	}
	defer rows.Close()

	for rows.Next() {
		var repoPrefix, language string
		var count int
		if err := rows.Scan(&repoPrefix, &language, &count); err != nil {
			panicOnFatal(err)
			return out
		}
		byLanguage := out[repoPrefix]
		if byLanguage == nil {
			byLanguage = make(map[string]int)
			out[repoPrefix] = byLanguage
		}
		byLanguage[language] = count
	}
	if err := rows.Err(); err != nil {
		panicOnFatal(err)
	}
	return out
}

// RepoNodeIDsByKinds projects only node IDs for the full repository/kind set.
// Both filters are json_each CTEs, so changed-repo inference costs one indexed
// SQLite query instead of one node scan per repository.
func (s *Store) RepoNodeIDsByKinds(repoPrefixes []string, kinds []graph.NodeKind) []string {
	reposJSON, ok := projectionJSON(repoPrefixes)
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
	rows, err := s.db.Query(repoNodeIDsByKindsQuery(), reposJSON, kindsJSON, s.viewGen)
	if err != nil {
		panicOnFatal(err)
		return nil
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			panicOnFatal(err)
			return out
		}
		out = append(out, id)
	}
	if err := rows.Err(); err != nil {
		panicOnFatal(err)
	}
	// The former ORDER BY n.id forced a temp B-tree over every matched row;
	// the ID contract is kept with a Go-side sort instead.
	sort.Strings(out)
	return out
}

// repoNodeIDsByKindsQuery and repoEdgesByKindsQuery are pure string builders
// (no I/O) so the plan-lock test can EXPLAIN the exact production SQL.
//
// Both drive REPO-FIRST through nodes_by_repo_kind: the flat kind index
// invites whole-kind-range scans that the repo filter then discards. The
// requested_repos CTE stays the driving (scanned) side by construction; the
// kind predicate is a plain IN so SQLite multi-seeks (repo_prefix, kind) —
// and, for edges, (from_id, kind) — per kind value. No SQL ORDER BY: both
// callers restore their ordering contract with a Go-side sort.
func repoNodeIDsByKindsQuery() string {
	return `
WITH requested_repos(repo_prefix) AS (
    SELECT CAST(value AS TEXT) FROM json_each(?)
), requested_kinds(kind) AS (
    SELECT CAST(value AS TEXT) FROM json_each(?)
)
SELECT n.id
FROM requested_repos AS r
CROSS JOIN requested_kinds AS k
CROSS JOIN nodes AS n ON n.repo_prefix = r.repo_prefix AND n.kind = k.kind AND n.view_gen = ?`
	// CROSS JOIN (not INDEXED BY): SQLite never reorders CROSS JOIN, which is
	// the whole fix — n can only be the probed side. INDEXED BY would be a
	// hard runtime error whenever the partial index cannot serve the query
	// (solo-repo '' prefixes; bulk-load windows with droppable indexes off),
	// and this path panics on query errors.
}

// RepoFilePaths projects only paths for file nodes in one repository/workspace.
// Language and extension predicates are evaluated by SQLite, so callers never
// materialize a workspace-wide KindFile snapshot merely to filter it in Go.
func (s *Store) RepoFilePaths(repoPrefix, workspaceID string, languages, extensions []string) []string {
	const pathExpr = `COALESCE(NULLIF(n.file_path, ''), n.id)`
	var query strings.Builder
	query.WriteString(`SELECT DISTINCT ` + pathExpr + ` FROM nodes AS n WHERE n.repo_prefix = ? AND n.kind = 'file'`)
	args := []any{repoPrefix}
	if workspaceID != "" {
		query.WriteString(` AND n.workspace_id = ?`)
		args = append(args, workspaceID)
	}
	var predicates []string
	if values, ok := projectionJSON(languages); ok {
		predicates = append(predicates, `EXISTS (SELECT 1 FROM json_each(?) AS lang WHERE lower(n.language) = lower(CAST(lang.value AS TEXT)))`)
		args = append(args, values)
	}
	if values, ok := projectionJSON(normalizeProjectionExtensions(extensions)); ok {
		predicates = append(predicates, `EXISTS (SELECT 1 FROM json_each(?) AS ext WHERE lower(`+pathExpr+`) LIKE '%' || lower(CAST(ext.value AS TEXT)))`)
		args = append(args, values)
	}
	if len(predicates) > 0 {
		query.WriteString(` AND (` + strings.Join(predicates, ` OR `) + `)`)
	}
	query.WriteString(` AND n.view_gen = ?`)
	args = append(args, s.viewGen)
	query.WriteString(` ORDER BY ` + pathExpr)

	rows, err := s.db.Query(query.String(), args...)
	if err != nil {
		panicOnFatal(err)
		return nil
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var path string
		if err := rows.Scan(&path); err != nil {
			panicOnFatal(err)
			return out
		}
		out = append(out, path)
	}
	if err := rows.Err(); err != nil {
		panicOnFatal(err)
	}
	return out
}

func normalizeProjectionExtensions(extensions []string) []string {
	out := make([]string, 0, len(extensions))
	for _, extension := range extensions {
		extension = strings.ToLower(strings.TrimSpace(extension))
		if extension == "" {
			continue
		}
		if !strings.HasPrefix(extension, ".") {
			extension = "." + extension
		}
		out = append(out, extension)
	}
	return out
}

// RepoNodesByKindsWithMetaKey performs one repository/workspace/kind query and
// decodes only nodes carrying the requested metadata key.
func (s *Store) RepoNodesByKindsWithMetaKey(repoPrefix, workspaceID string, kinds []graph.NodeKind, metaKey string) []*graph.Node {
	if len(kinds) == 0 || metaKey == "" {
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
	query := `SELECT ` + qualifiedNodeColumns("n", lookupNodeCols) + `
FROM nodes AS n
JOIN json_each(?) AS requested_kind ON CAST(requested_kind.value AS TEXT) = n.kind
WHERE n.repo_prefix = ?`
	args := []any{kindsJSON, repoPrefix}
	if workspaceID != "" {
		query += ` AND n.workspace_id = ?`
		args = append(args, workspaceID)
	}
	query += ` AND n.view_gen = ? ORDER BY n.id`
	args = append(args, s.viewGen)
	candidates := s.scanNodeQuery(query, args...)
	out := candidates[:0]
	for _, node := range candidates {
		if node == nil || node.Meta == nil {
			continue
		}
		if _, ok := node.Meta[metaKey]; ok {
			out = append(out, node)
		}
	}
	return out
}

type repoPrefixedEdgeScanner struct {
	scanner interface{ Scan(...any) error }
	repo    *string
}

func (s repoPrefixedEdgeScanner) Scan(dest ...any) error {
	all := make([]any, 0, len(dest)+1)
	all = append(all, s.repo)
	all = append(all, dest...)
	return s.scanner.Scan(all...)
}

// RepoEdgesByKinds returns the requested repositories' source-owned edges in a
// single indexed join. Repository and kind sets are bound as JSON arrays, so
// the statement count remains one even for large workspaces and no edge source
// requires a follow-up node lookup.
func (s *Store) RepoEdgesByKinds(repoPrefixes []string, kinds []graph.EdgeKind) []graph.RepoEdgeRow {
	reposJSON, ok := projectionJSON(repoPrefixes)
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
	rows, err := s.db.Query(repoEdgesByKindsQuery(), reposJSON, kindsJSON, s.viewGen)
	if err != nil {
		panicOnFatal(err)
		return nil
	}
	defer rows.Close()

	var out []graph.RepoEdgeRow
	for rows.Next() {
		var repoPrefix string
		edge, err := s.scanEdgeCursor(repoPrefixedEdgeScanner{scanner: rows, repo: &repoPrefix})
		if err != nil {
			panicOnFatal(err)
			return out
		}
		if edge == nil {
			continue
		}
		out = append(out, graph.RepoEdgeRow{RepoPrefix: repoPrefix, Edge: edge})
	}
	if err := rows.Err(); err != nil {
		panicOnFatal(err)
	}
	// The former six-column ORDER BY forced a temp B-tree over the whole
	// result; the ordering contract is restored in Go.
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.RepoPrefix != b.RepoPrefix {
			return a.RepoPrefix < b.RepoPrefix
		}
		if a.Edge.From != b.Edge.From {
			return a.Edge.From < b.Edge.From
		}
		if a.Edge.To != b.Edge.To {
			return a.Edge.To < b.Edge.To
		}
		if a.Edge.Kind != b.Edge.Kind {
			return a.Edge.Kind < b.Edge.Kind
		}
		if a.Edge.FilePath != b.Edge.FilePath {
			return a.Edge.FilePath < b.Edge.FilePath
		}
		return a.Edge.Line < b.Edge.Line
	})
	return out
}

func repoEdgesByKindsQuery() string {
	// r → n → k → e under CROSS JOIN (never reordered): every edge lookup is
	// a full (from_id, kind) seek on edges_by_from — the flat-kind global
	// range scan the old shape invited is structurally unreachable, and the
	// compound seek survives the node-first drive.
	return `
WITH requested_repos(repo_prefix) AS (
    SELECT CAST(value AS TEXT) FROM json_each(?)
), requested_kinds(kind) AS (
    SELECT CAST(value AS TEXT) FROM json_each(?)
)
SELECT n.repo_prefix,
       e.from_id, e.to_id, e.kind, e.file_path, e.line,
	       e.confidence, e.confidence_label, e.origin, e.tier,
	       e.cross_repo, e.meta, e.resolve_terminal, e.resolve_terminal_reason, e.semantic_source
FROM requested_repos AS r
CROSS JOIN nodes AS n ON n.repo_prefix = r.repo_prefix AND n.view_gen = ?
CROSS JOIN requested_kinds AS k
CROSS JOIN edges AS e ON e.from_id = n.id AND e.kind = k.kind AND e.view_gen = n.view_gen`
}

var (
	_ graph.RepoLanguageFileCountReader = (*Store)(nil)
	_ graph.RepoLanguageCountReader     = (*Store)(nil)
	_ graph.RepoNodeKindIDReader        = (*Store)(nil)
	_ graph.RepoEdgeKindReader          = (*Store)(nil)
	_ graph.RepoFilePathReader          = (*Store)(nil)
	_ graph.RepoMetaNodeReader          = (*Store)(nil)
)
