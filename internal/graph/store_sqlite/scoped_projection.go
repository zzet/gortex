package store_sqlite

import (
	"iter"
	"strings"

	"github.com/zzet/gortex/internal/graph"
)

// scopedProjectionPage is deliberately small: framework partial passes may
// inspect a very large changed repository, but neither the SQLite cursor nor a
// Go slice is allowed to turn that repository into the memory unit.
const scopedProjectionPage = 256

// Scoped projections bind the kind as a literal predicate — one streaming
// cursor per kind — never through a json_each CTE join. With a kinds CTE the
// planner cannot satisfy `ORDER BY id LIMIT n` from nodes_by_kind /
// edges_by_kind and falls back to a temp b-tree that re-sorts every remaining
// matching row on EVERY page, turning a linear walk quadratic (measured 0.93s
// vs 0.009s per 256-row page on a 2.7M-edge store). A literal kind rides the
// (kind, id) shape both indexes already have — edges_by_kind(kind) on a rowid
// table and nodes_by_kind(kind) on the WITHOUT ROWID nodes table — so each
// page is a pure index seek. Rows stream in ascending id order within a kind;
// kinds yield in argument order.

// scopedKindValues normalises a kind list into per-cursor literals, deduped,
// argument order preserved. ok is false when kinds were requested but all
// empty — an intentionally empty projection, mirroring the json_each guard it
// replaces. An empty input yields one unfiltered cursor sentinel.
func scopedKindValues[T ~string](kinds []T) ([]string, bool) {
	if len(kinds) == 0 {
		return []string{""}, true
	}
	seen := make(map[string]struct{}, len(kinds))
	values := make([]string, 0, len(kinds))
	for _, kind := range kinds {
		if kind == "" {
			continue
		}
		if _, dup := seen[string(kind)]; dup {
			continue
		}
		seen[string(kind)] = struct{}{}
		values = append(values, string(kind))
	}
	if len(values) == 0 {
		return nil, false
	}
	return values, true
}

// NodesInScopeSeq keyset-pages full node rows owned by the requested file or
// repository frontier. Each cursor is closed before rows are yielded, so a
// synthesizer may safely write after (or even during) iteration.
func (s *Store) NodesInScopeSeq(repoPrefixes, filePaths []string, kinds ...graph.NodeKind) iter.Seq[*graph.Node] {
	kindValues, ok := scopedKindValues(kinds)
	if !ok {
		return func(func(*graph.Node) bool) {}
	}
	return func(yield func(*graph.Node) bool) {
		for _, kind := range kindValues {
			query, args, ok := scopedNodeProjectionQuery(repoPrefixes, filePaths, kind, lookupNodeCols, s.viewGen)
			if !ok {
				return
			}
			if !s.streamScopedNodes(query, args, false, yield) {
				return
			}
		}
	}
}

// NodesLightInScopeSeq is the candidate-census projection. It uses the same
// keyset paging as NodesInScopeSeq but never reads Meta, docs, or signatures.
func (s *Store) NodesLightInScopeSeq(repoPrefixes, filePaths []string) iter.Seq[*graph.Node] {
	return func(yield func(*graph.Node) bool) {
		query, args, ok := scopedNodeProjectionQuery(repoPrefixes, filePaths, "", lookupNodeSummaryCols, s.viewGen)
		if !ok {
			return
		}
		s.streamScopedNodes(query, args, true, yield)
	}
}

// streamScopedNodes drains one node cursor page by page. It reports false when
// the consumer stopped the sequence, true when the cursor is exhausted.
func (s *Store) streamScopedNodes(query string, args []any, summary bool, yield func(*graph.Node) bool) bool {
	lastID := ""
	for {
		pageArgs := append(append([]any(nil), args...), lastID, scopedProjectionPage)
		rows, err := s.db.Query(query, pageArgs...)
		if err != nil {
			panicOnFatal(err)
			return false
		}
		page := make([]*graph.Node, 0, scopedProjectionPage)
		for rows.Next() {
			var node *graph.Node
			var scanErr error
			if summary {
				node, scanErr = scanNodeSummary(rows)
			} else {
				node, scanErr = scanNodeCursor(rows)
			}
			if scanErr != nil {
				_ = rows.Close()
				panicOnFatal(scanErr)
				return false
			}
			if node != nil {
				page = append(page, node)
				lastID = node.ID
			}
		}
		if rowsErr := rows.Err(); rowsErr != nil {
			_ = rows.Close()
			panicOnFatal(rowsErr)
			return false
		}
		_ = rows.Close()
		for _, node := range page {
			if !yield(node) {
				return false
			}
		}
		if len(page) < scopedProjectionPage {
			return true
		}
	}
}

// EdgesInScopeSeq keyset-pages source-owned edges. Source nodes are hydrated
// once per page, after the edge cursor is closed; callers therefore get an
// edge+source pair without an N+1 GetNode loop or a second open SQLite cursor.
// maxID freezes the scan boundary once for all kinds so edges synthesized
// during this pass cannot be re-consumed by a later page of the same iterator.
func (s *Store) EdgesInScopeSeq(repoPrefixes, filePaths []string, kinds ...graph.EdgeKind) iter.Seq[graph.ScopedEdgeRow] {
	if len(kinds) == 0 {
		return func(func(graph.ScopedEdgeRow) bool) {}
	}
	kindValues, ok := scopedKindValues(kinds)
	if !ok {
		return func(func(graph.ScopedEdgeRow) bool) {}
	}
	return func(yield func(graph.ScopedEdgeRow) bool) {
		var maxID int64
		if err := s.db.QueryRow(`SELECT COALESCE(MAX(id), 0) FROM edges WHERE view_gen = ?`, s.viewGen).Scan(&maxID); err != nil {
			panicOnFatal(err)
			return
		}
		// edge.file_path is the cheapest exact-frontier key, but imported or
		// legacy rows can carry blank/mismatched provenance. Validate only the
		// frozen edge generation before selecting the covering file index; a
		// malformed frontier keeps the source-node semantics through the
		// nodes_by_file -> edges_by_from fallback below.
		fileIndexed := len(filePaths) > 0 && s.scopedEdgeFileProvenanceCanonical(
			repoPrefixes, filePaths, kindValues, maxID,
		)
		for _, kind := range kindValues {
			var query string
			var args []any
			var ok bool
			if fileIndexed {
				query, args, ok = scopedEdgeProjectionQuery(repoPrefixes, filePaths, kind, s.viewGen)
			} else {
				query, args, ok = scopedEdgeSourceProjectionQuery(repoPrefixes, filePaths, kind, s.viewGen)
			}
			if !ok {
				return
			}
			if !s.streamScopedEdges(query, args, maxID, yield) {
				return
			}
		}
	}
}

// streamScopedEdges drains one edge-kind cursor page by page, hydrating both
// endpoints per page. It reports false when the consumer stopped the sequence,
// true when the cursor is exhausted.
func (s *Store) streamScopedEdges(query string, args []any, maxID int64, yield func(graph.ScopedEdgeRow) bool) bool {
	lastID := int64(0)
	for lastID < maxID {
		pageArgs := append(append([]any(nil), args...), lastID, maxID, scopedProjectionPage)
		rows, err := s.db.Query(query, pageArgs...)
		if err != nil {
			panicOnFatal(err)
			return false
		}
		page := make([]*graph.Edge, 0, scopedProjectionPage)
		for rows.Next() {
			var edgeID int64
			edge, scanErr := s.scanEdgeCursor(edgeIDScanner{scanner: rows, id: &edgeID})
			if scanErr != nil {
				_ = rows.Close()
				panicOnFatal(scanErr)
				return false
			}
			lastID = edgeID
			if edge != nil {
				page = append(page, edge)
			}
		}
		if rowsErr := rows.Err(); rowsErr != nil {
			_ = rows.Close()
			panicOnFatal(rowsErr)
			return false
		}
		_ = rows.Close()
		if len(page) == 0 {
			return true
		}
		endpointIDs := make([]string, 0, len(page)*2)
		for _, edge := range page {
			endpointIDs = append(endpointIDs, edge.From)
			if edge.To != "" && !graph.IsUnresolvedTarget(edge.To) {
				endpointIDs = append(endpointIDs, edge.To)
			}
		}
		endpoints := s.GetNodesByIDs(endpointIDs)
		for _, edge := range page {
			if !yield(graph.ScopedEdgeRow{
				Edge: edge, Source: endpoints[edge.From], Target: endpoints[edge.To],
			}) {
				return false
			}
		}
		if len(page) < scopedProjectionPage {
			return true
		}
	}
	return true
}

type edgeIDScanner struct {
	scanner interface{ Scan(...any) error }
	id      *int64
}

func (s edgeIDScanner) Scan(dest ...any) error {
	all := make([]any, 0, len(dest)+1)
	all = append(all, s.id)
	all = append(all, dest...)
	return s.scanner.Scan(all...)
}

func scopedNodeProjectionQuery(
	repoPrefixes, filePaths []string,
	kind string,
	columns string,
	viewGen int64,
) (string, []any, bool) {
	reposJSON, haveRepos := projectionJSON(repoPrefixes)
	filesJSON, haveFiles := projectionJSON(filePaths)
	if !haveRepos && !haveFiles {
		return "", nil, false
	}
	ctes := make([]string, 0, 2)
	joins := make([]string, 0, 2)
	args := make([]any, 0, 3)
	if haveFiles {
		ctes = append(ctes, `requested_files(file_path) AS (SELECT CAST(value AS TEXT) FROM json_each(?))`)
		joins = append(joins, `JOIN requested_files AS f ON f.file_path = n.file_path`)
		args = append(args, filesJSON)
	}
	if haveRepos {
		ctes = append(ctes, `requested_repos(repo_prefix) AS (SELECT CAST(value AS TEXT) FROM json_each(?))`)
		// +n.repo_prefix: keep repo-leading indexes OUT of this keyset query.
		// Its streaming contract rides on an id-ordered access path —
		// nodes_by_kind's (kind, id) entries, or the id-ordered PK scan —
		// and nodes_by_repo_kind's (repo, kind, id) order interleaves ids
		// across the requested repos, forcing the temp B-tree the round-5
		// lock forbids. The repo CTE stays a per-row filter here.
		joins = append(joins, `JOIN requested_repos AS r ON r.repo_prefix = +n.repo_prefix`)
		args = append(args, reposJSON)
	}
	kindPredicate := ""
	if kind != "" {
		kindPredicate = `n.kind = ? AND `
		args = append(args, kind)
	}
	// The generation binds ahead of the keyset arguments the pager appends.
	args = append(args, viewGen)
	query := `WITH ` + strings.Join(ctes, ", ") +
		` SELECT ` + qualifiedNodeColumns("n", columns) +
		` FROM nodes AS n ` + strings.Join(joins, " ") +
		` WHERE ` + kindPredicate + `n.view_gen = ? AND n.id > ? ORDER BY n.id LIMIT ?`
	return query, args, true
}

// scopedEdgeProjectionQuery is the canonical exact-file path. The requested
// file CTE is deliberately the outer loop and edges_by_file is mandatory: an
// id-ordered edges_by_kind walk examines a whole corpus kind range merely to
// discover the handful of rows owned by the requested files.
func scopedEdgeProjectionQuery(
	repoPrefixes, filePaths []string,
	kind string,
	viewGen int64,
) (string, []any, bool) {
	if kind == "" {
		return "", nil, false
	}
	filesJSON, haveFiles := projectionJSON(filePaths)
	if !haveFiles {
		return scopedEdgeSourceProjectionQuery(repoPrefixes, filePaths, kind, viewGen)
	}
	reposJSON, haveRepos := projectionJSON(repoPrefixes)
	ctes := []string{`requested_files(file_path) AS (SELECT CAST(value AS TEXT) FROM json_each(?))`}
	args := []any{filesJSON}
	repoPredicate := ""
	if haveRepos {
		ctes = append(ctes, `requested_repos(repo_prefix) AS (SELECT CAST(value AS TEXT) FROM json_each(?))`)
		args = append(args, reposJSON)
		repoPredicate = ` AND n.repo_prefix IN (SELECT repo_prefix FROM requested_repos)`
	}
	args = append(args, kind, viewGen)
	query := `WITH ` + strings.Join(ctes, ", ") +
		` SELECT e.id, ` + lookupQualifiedEdgeCols +
		` FROM requested_files AS f` +
		` CROSS JOIN edges AS e INDEXED BY edges_by_file` +
		` CROSS JOIN nodes AS n` +
		` WHERE e.file_path = f.file_path AND e.kind = ?` +
		` AND n.id = e.from_id AND n.file_path = e.file_path` + repoPredicate +
		` AND e.view_gen = ? AND n.view_gen = e.view_gen` +
		` AND e.id > ? AND e.id <= ? ORDER BY e.id LIMIT ?`
	return query, args, true
}

// scopedEdgeSourceProjectionQuery preserves source-node ownership when edge
// provenance is not canonical. Explicit loop order prevents SQLite from
// satisfying ORDER BY through edges_by_kind and filtering the whole kind
// range after the fact.
func scopedEdgeSourceProjectionQuery(
	repoPrefixes, filePaths []string,
	kind string,
	viewGen int64,
) (string, []any, bool) {
	if kind == "" {
		return "", nil, false
	}
	reposJSON, haveRepos := projectionJSON(repoPrefixes)
	filesJSON, haveFiles := projectionJSON(filePaths)
	if !haveRepos && !haveFiles {
		return "", nil, false
	}
	if !haveFiles {
		// Preserve the linear id-keyset repository walk. Forcing repository
		// nodes first would require a temp B-tree to restore e.id order on
		// every page, recreating the quadratic plan this projection replaced.
		query := `WITH requested_repos(repo_prefix) AS (` +
			`SELECT CAST(value AS TEXT) FROM json_each(?))` +
			` SELECT e.id, ` + lookupQualifiedEdgeCols +
			` FROM nodes AS n JOIN edges AS e ON e.from_id = n.id AND e.view_gen = n.view_gen` +
			` JOIN requested_repos AS r ON r.repo_prefix = n.repo_prefix` +
			` WHERE e.kind = ? AND e.view_gen = ? AND e.id > ? AND e.id <= ? ORDER BY e.id LIMIT ?`
		return query, []any{reposJSON, kind, viewGen}, true
	}

	ctes := []string{`requested_files(file_path) AS (SELECT CAST(value AS TEXT) FROM json_each(?))`}
	args := []any{filesJSON}
	scopePredicate := `n.file_path = f.file_path`
	if haveRepos {
		ctes = append(ctes, `requested_repos(repo_prefix) AS (SELECT CAST(value AS TEXT) FROM json_each(?))`)
		args = append(args, reposJSON)
		scopePredicate += ` AND n.repo_prefix IN (SELECT repo_prefix FROM requested_repos)`
	}
	args = append(args, kind, viewGen)
	query := `WITH ` + strings.Join(ctes, ", ") +
		` SELECT e.id, ` + lookupQualifiedEdgeCols +
		` FROM requested_files AS f` +
		` CROSS JOIN nodes AS n INDEXED BY nodes_by_file` +
		` CROSS JOIN edges AS e INDEXED BY edges_by_from` +
		` WHERE ` + scopePredicate + ` AND e.from_id = n.id AND e.kind = ?` +
		` AND n.view_gen = ? AND e.view_gen = n.view_gen` +
		` AND e.id > ? AND e.id <= ? ORDER BY e.id LIMIT ?`
	return query, args, true
}

func (s *Store) scopedEdgeFileProvenanceCanonical(
	repoPrefixes, filePaths, kinds []string,
	maxID int64,
) bool {
	filesJSON, haveFiles := projectionJSON(filePaths)
	kindsJSON, haveKinds := projectionJSON(kinds)
	if !haveFiles || !haveKinds {
		return false
	}
	reposJSON, haveRepos := projectionJSON(repoPrefixes)
	ctes := []string{
		`requested_files(file_path) AS (SELECT CAST(value AS TEXT) FROM json_each(?))`,
		`requested_kinds(kind) AS (SELECT CAST(value AS TEXT) FROM json_each(?))`,
	}
	args := []any{filesJSON, kindsJSON}
	repoPredicate := ""
	if haveRepos {
		ctes = append(ctes, `requested_repos(repo_prefix) AS (SELECT CAST(value AS TEXT) FROM json_each(?))`)
		args = append(args, reposJSON)
		repoPredicate = ` AND n.repo_prefix IN (SELECT repo_prefix FROM requested_repos)`
	}
	args = append(args, s.viewGen, maxID)
	query := `WITH ` + strings.Join(ctes, ", ") +
		` SELECT NOT EXISTS (` +
		`SELECT 1 FROM requested_files AS f` +
		` CROSS JOIN nodes AS n INDEXED BY nodes_by_file` +
		` CROSS JOIN edges AS e INDEXED BY edges_by_from` +
		` WHERE n.file_path = f.file_path AND e.from_id = n.id` +
		` AND e.kind IN (SELECT kind FROM requested_kinds)` + repoPredicate +
		` AND n.view_gen = ? AND e.view_gen = n.view_gen` +
		` AND e.id <= ? AND e.file_path <> n.file_path LIMIT 1)`
	var canonical bool
	if err := s.db.QueryRow(query, args...).Scan(&canonical); err != nil {
		panicOnFatal(err)
		return false
	}
	return canonical
}

var _ graph.ScopedProjectionSequencer = (*Store)(nil)
