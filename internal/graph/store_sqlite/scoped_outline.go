package store_sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/zzet/gortex/internal/graph"
)

// outlineScopePredicate is the SQL twin of graph.NodeScope.Allows. GLOB keeps
// the external path check case-sensitive, like strings.HasPrefix in Go.
func outlineScopePredicate(scope graph.NodeScope, alias string) (string, []any) {
	return outlineScopePredicateWithWorkspace(scope, alias, false)
}

// A nonempty exact effective-workspace seek can discharge only the workspace
// clause. Project and repo intersections still use the original scope.
func outlineScopePredicateWithWorkspace(scope graph.NodeScope, alias string, workspaceProven bool) (string, []any) {
	var terms []string
	var args []any
	global := `(` + alias + `.repo_prefix = '' AND ` + alias + `.workspace_id = '' AND (` + alias + `.file_path GLOB 'external::*' OR ` + alias + `.file_path GLOB 'external-call::*'))`
	if scope.WorkspaceID != "" && !workspaceProven {
		terms = append(terms, `(`+global+` OR COALESCE(NULLIF(`+alias+`.workspace_id, ''), `+alias+`.repo_prefix) = ?)`)
		args = append(args, scope.WorkspaceID)
	}
	if scope.ProjectID != "" && (scope.WorkspaceID != "" || len(scope.RepoAllow) > 0) {
		terms = append(terms, `(`+global+` OR COALESCE(NULLIF(`+alias+`.project_id, ''), `+alias+`.repo_prefix) = ?)`)
		args = append(args, scope.ProjectID)
	}
	if len(scope.RepoAllow) > 0 {
		terms = append(terms, alias+`.repo_prefix IN (SELECT CAST(value AS TEXT) FROM json_each(?))`)
		args = append(args, outlineReposJSON(scope))
	}
	if len(terms) == 0 {
		return "1", nil
	}
	return strings.Join(terms, " AND "), args
}

func outlineReposJSON(scope graph.NodeScope) string {
	// The unowned bucket is always eligible under a repo narrow. Workspace
	// and project checks still filter source nodes in that bucket.
	repos := []string{""}
	for repo, allowed := range scope.RepoAllow {
		if allowed && repo != "" {
			repos = append(repos, repo)
		}
	}
	sort.Strings(repos)
	data, _ := json.Marshal(repos)
	return string(data)
}

type outlineQuery struct {
	sql  string
	args []any
}

// outlineAggregateQuery materializes the light scope once for every section.
// Edge membership probes its compact ID set rather than fetching the wide node
// table once per endpoint. Unary + keeps to_id membership a residual lookup if
// the adjacency index is temporarily absent; otherwise SQLite can turn the IN
// set into one identity-index seek per pair of scoped nodes.
func outlineAggregateQuery(scope graph.NodeScope, generation int64, filter graph.OutlineEdgeFilter) outlineQuery {
	predicate, scopeArgs := outlineScopePredicate(scope, "n")
	from := ` FROM nodes AS n`
	if len(scope.RepoAllow) > 0 {
		from += ` INDEXED BY nodes_by_repo`
	}
	query := `WITH scoped AS MATERIALIZED (
		SELECT n.id, n.kind, n.language, n.file_path` + from + `
		WHERE n.view_gen = ? AND ` + predicate + `)
		SELECT 'language', language, COUNT(*) FROM scoped GROUP BY language
		UNION ALL `
	args := append([]any{generation}, scopeArgs...)

	edgeFilter, edgeArgs := outlineEdgePredicate(filter, "sc", "")
	targetFilter := ""
	var targetArgs []any
	if len(filter.ExcludeNodeKinds) > 0 {
		data, _ := json.Marshal(filter.ExcludeNodeKinds)
		targetFilter = ` AND COALESCE(target.kind, '') NOT IN (SELECT CAST(value AS TEXT) FROM json_each(?))`
		targetArgs = append(targetArgs, string(data))
	}
	viaFilter, viaArgs := outlineViaPredicate(filter.ExcludeVia)
	query += `SELECT 'edge', '', COUNT(*) FROM scoped AS sc
		CROSS JOIN edges AS e INDEXED BY edges_by_from ON e.from_id = sc.id AND e.view_gen = ?
		WHERE 1` + edgeFilter + ` AND +e.to_id IN (SELECT target.id FROM scoped AS target WHERE 1` + targetFilter + `)` + viaFilter + `
		UNION ALL `
	args = append(args, generation)
	args = append(args, edgeArgs...)
	args = append(args, targetArgs...)
	args = append(args, viaArgs...)

	importFilter, importArgs := outlineEdgePredicate(filter, "source", "sc")
	query += `SELECT 'import', COALESCE(NULLIF(sc.file_path, ''), sc.id) AS path, COUNT(*) FROM scoped AS sc
		CROSS JOIN edges AS e INDEXED BY edges_by_to ON e.to_id = sc.id AND e.view_gen = ? AND e.kind = ? `
	if len(filter.ExcludeNodeKinds) > 0 {
		query += `LEFT JOIN nodes AS source ON source.id = e.from_id AND source.view_gen = e.view_gen `
	}
	query += `WHERE 1` + importFilter + viaFilter + ` GROUP BY path`
	args = append(args, generation, string(graph.EdgeImports))
	args = append(args, importArgs...)
	args = append(args, viaArgs...)
	return outlineQuery{sql: query, args: args}
}

func outlineViaPredicate(via string) (string, []any) {
	if via == "" {
		return "", nil
	}
	// New flat blobs contain literal string bytes. Decode only potential
	// matches, while admitting older JSON/gob encodings conservatively so
	// escaped JSON strings retain the same visibility as fully decoded edges.
	return ` AND CASE WHEN e.meta IS NOT NULL
		AND (substr(e.meta, 1, 2) <> ? OR instr(e.meta, ?) > 0)
		THEN gortex_outline_via_is(CAST(e.meta AS BLOB), CAST(? AS BLOB)) ELSE 0 END = 0`,
		[]any{[]byte{metaFlatMagic0, metaFlatVersion}, []byte(via), via}
}

func outlineEdgePredicate(filter graph.OutlineEdgeFilter, from, to string) (string, []any) {
	var predicate string
	var args []any
	if len(filter.ExcludeKinds) > 0 {
		data, _ := json.Marshal(filter.ExcludeKinds)
		predicate += ` AND e.kind NOT IN (SELECT CAST(value AS TEXT) FROM json_each(?))`
		args = append(args, string(data))
	}
	if len(filter.ExcludeNodeKinds) > 0 {
		data, _ := json.Marshal(filter.ExcludeNodeKinds)
		for _, alias := range []string{from, to} {
			if alias == "" {
				continue
			}
			predicate += ` AND (` + alias + `.kind IS NULL OR ` + alias + `.kind NOT IN (SELECT CAST(value AS TEXT) FROM json_each(?)))`
			args = append(args, string(data))
		}
	}
	if len(filter.ExcludeNodeIDs) > 0 {
		ids := make([]string, 0, len(filter.ExcludeNodeIDs))
		for id, excluded := range filter.ExcludeNodeIDs {
			if excluded {
				ids = append(ids, id)
			}
		}
		data, _ := json.Marshal(ids)
		for _, endpoint := range []string{"from_id", "to_id"} {
			predicate += ` AND e.` + endpoint + ` NOT IN (SELECT CAST(value AS TEXT) FROM json_each(?))`
			args = append(args, string(data))
		}
	}
	return predicate, args
}

func (s *Store) ScopedOutlineCounts(scope graph.NodeScope) (graph.OutlineCounts, error) {
	return s.ScopedOutlineCountsFiltered(scope, graph.OutlineEdgeFilter{})
}

func (s *Store) ScopedOutlineCountsFiltered(scope graph.NodeScope, filter graph.OutlineEdgeFilter) (graph.OutlineCounts, error) {
	ctx := s.readContext()
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return graph.OutlineCounts{}, err
	}
	defer func() { _ = tx.Rollback() }()
	out, err := s.outlineCountsFrom(ctx, tx, scope, filter)
	if err != nil {
		return graph.OutlineCounts{}, err
	}
	if err := tx.Commit(); err != nil {
		return graph.OutlineCounts{}, err
	}
	return out, nil
}

type outlineQueryer interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

// Every section shares the transaction's read snapshot and one scoped-node
// materialization; no metadata correction can race a separately counted total.
func (s *Store) outlineCountsFrom(ctx context.Context, tx outlineQueryer, scope graph.NodeScope, filter graph.OutlineEdgeFilter) (graph.OutlineCounts, error) {
	out := graph.OutlineCounts{ByLanguage: make(map[string]int)}
	query := outlineAggregateQuery(scope, s.viewGen, filter)
	if err := scanOutlineQuery(ctx, tx, query, func(rows *sql.Rows) error {
		var section, key string
		var count int
		if err := rows.Scan(&section, &key, &count); err != nil {
			return err
		}
		switch section {
		case "language":
			out.TotalNodes += count
			if key != "" {
				out.ByLanguage[key] = count
			}
		case "edge":
			out.TotalEdges = count
		case "import":
			out.FileImports = append(out.FileImports, graph.FileImportCountRow{FilePath: key, Count: count})
		default:
			return fmt.Errorf("unknown outline aggregate section %q", section)
		}
		return nil
	}); err != nil {
		return graph.OutlineCounts{}, err
	}
	return out, nil
}

// Cold bulk loads may temporarily drop secondary indexes. Retain exact reads
// during that window; steady-state queries pin the repo/adjacency indexes so
// stale planner statistics cannot turn a scoped outline into a global scan.
func outlineRows(ctx context.Context, tx outlineQueryer, query outlineQuery) (*sql.Rows, error) {
	rows, err := tx.QueryContext(ctx, query.sql, query.args...)
	if isNoSuchIndexErr(err) {
		unhinted := strings.NewReplacer(
			" INDEXED BY nodes_by_repo", "",
			" INDEXED BY nodes_by_kind", "",
			" INDEXED BY nodes_by_effective_workspace", "",
			" INDEXED BY nodes_deadcode_eligible_workspace", "",
			" INDEXED BY nodes_by_name", "",
			" INDEXED BY edges_by_from", "",
			" INDEXED BY edges_by_to", "",
		).Replace(query.sql)
		return tx.QueryContext(ctx, unhinted, query.args...)
	}
	return rows, err
}

var _ graph.ScopedOutlineAggregator = (*Store)(nil)

var _ graph.FilteredScopedOutlineAggregator = (*Store)(nil)

func scanOutlineQuery(ctx context.Context, tx outlineQueryer, query outlineQuery, scan func(*sql.Rows) error) error {
	rows, err := outlineRows(ctx, tx, query)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		if err := scan(rows); err != nil {
			return err
		}
	}
	return rows.Err()
}
