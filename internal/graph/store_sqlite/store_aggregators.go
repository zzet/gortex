package store_sqlite

import (
	"iter"
	"sort"
	"strings"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graphpath"
)

// This file implements the trivial SQL aggregator / scanner optional
// capability interfaces from graph.Store. Each method pushes its
// GROUP BY / WHERE / COUNT into SQLite so the planner drives it through
// the schema's secondary indexes, returning only the aggregate rows
// instead of materialising the whole node / edge table Go-side.
//
// Conventions shared across these methods:
//   - Empty / nil input returns nil (parity with the in-memory store).
//   - Input id / kind slices are deduped before they reach the IN-list.
//   - Large IN-lists are chunked by lookupChunkSize.
//   - agg-prefixed helpers are local to this file.
//   - Every query binds the handle's payload view generation. Correlated
//     subqueries bind it INSIDE the subquery, because that is where the rows
//     they count come from; endpoint JOINs pair the two sides' generations so
//     a node from another generation can neither attribute nor authorise an
//     edge here.

var (
	_ graph.InEdgeCounter            = (*Store)(nil)
	_ graph.NodeIDsByKinds           = (*Store)(nil)
	_ graph.EdgeKindCounter          = (*Store)(nil)
	_ graph.NodeDegreeByKinds        = (*Store)(nil)
	_ graph.NodesInFilesByKindFinder = (*Store)(nil)
	_ graph.FileImportAggregator     = (*Store)(nil)
	_ graph.InDegreeForNodes         = (*Store)(nil)
	_ graph.CrossRepoEdgeAggregator  = (*Store)(nil)
	_ graph.FileImporters            = (*Store)(nil)
	_ graph.FileSymbolNamesByPaths   = (*Store)(nil)
	_ graph.EdgesByKindsScanner      = (*Store)(nil)
	_ graph.NodesByKindsScanner      = (*Store)(nil)
	_ graph.EdgeAdjacencyForKinds    = (*Store)(nil)
	_ graph.NodeDegreeAggregator     = (*Store)(nil)
	_ graph.NodeFanAggregator        = (*Store)(nil)
)

// aggDedupeEdgeKinds drops empties and duplicates from an edge-kind
// slice, preserving first-seen order; returns the kinds widened to the
// []any an IN-list binds.
func aggDedupeEdgeKinds(kinds []graph.EdgeKind) (uniq []graph.EdgeKind, args []any) {
	seen := make(map[graph.EdgeKind]struct{}, len(kinds))
	for _, k := range kinds {
		if k == "" {
			continue
		}
		if _, ok := seen[k]; ok {
			continue
		}
		seen[k] = struct{}{}
		uniq = append(uniq, k)
		args = append(args, string(k))
	}
	return uniq, args
}

// aggDedupeNodeKinds is the node-kind twin of aggDedupeEdgeKinds.
func aggDedupeNodeKinds(kinds []graph.NodeKind) (uniq []graph.NodeKind, args []any) {
	seen := make(map[graph.NodeKind]struct{}, len(kinds))
	for _, k := range kinds {
		if k == "" {
			continue
		}
		if _, ok := seen[k]; ok {
			continue
		}
		seen[k] = struct{}{}
		uniq = append(uniq, k)
		args = append(args, string(k))
	}
	return uniq, args
}

// InEdgeCountsByKind returns per-target incoming-edge counts for the
// supplied edge kinds, grouped server-side via edges_by_to.
func (s *Store) InEdgeCountsByKind(kinds []graph.EdgeKind) map[string]int {
	_, args := aggDedupeEdgeKinds(kinds)
	if len(args) == 0 {
		return nil
	}
	q := `SELECT to_id, COUNT(*) FROM edges WHERE kind IN (` + inPlaceholders(len(args)) + `) AND view_gen = ? GROUP BY to_id`
	rows, err := s.db.Query(q, append(args, s.viewGen)...)
	panicOnFatal(err)
	if rows == nil {
		// swallowed teardown-race error: read returns empty (see panicOnFatal)
		return nil
	}
	defer rows.Close()
	out := make(map[string]int)
	for rows.Next() {
		var id string
		var n int
		panicOnFatal(rows.Scan(&id, &n))
		out[id] = n
	}
	panicOnFatal(rows.Err())
	return out
}

// NodeIDsByKinds returns the deduplicated IDs of every node whose kind
// is in the supplied set.
func (s *Store) NodeIDsByKinds(kinds []graph.NodeKind) []string {
	_, args := aggDedupeNodeKinds(kinds)
	if len(args) == 0 {
		return nil
	}
	q := `SELECT id FROM nodes WHERE kind IN (` + inPlaceholders(len(args)) + `) AND view_gen = ? ORDER BY id`
	rows, err := s.db.Query(q, append(args, s.viewGen)...)
	panicOnFatal(err)
	if rows == nil {
		// swallowed teardown-race error: read returns empty (see panicOnFatal)
		return nil
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		panicOnFatal(rows.Scan(&id))
		out = append(out, id)
	}
	panicOnFatal(rows.Err())
	return out
}

// EdgeKindCounts returns one entry per distinct edge kind with its
// occurrence count across the whole graph.
func (s *Store) EdgeKindCounts() map[graph.EdgeKind]int {
	rows, err := s.db.Query(`SELECT kind, COUNT(*) FROM edges WHERE view_gen = ? GROUP BY kind`, s.viewGen)
	panicOnFatal(err)
	if rows == nil {
		// swallowed teardown-race error: read returns empty (see panicOnFatal)
		return nil
	}
	defer rows.Close()
	out := make(map[graph.EdgeKind]int)
	for rows.Next() {
		var kind string
		var n int
		panicOnFatal(rows.Scan(&kind, &n))
		out[graph.EdgeKind(kind)] = n
	}
	panicOnFatal(rows.Err())
	return out
}

// NodeDegreeByKinds returns total in/out degree for every node whose
// kind is in the set (optionally under pathPrefix); UsageInCount is
// always 0 for this capability.
func (s *Store) NodeDegreeByKinds(kinds []graph.NodeKind, pathPrefix string) []graph.NodeDegreeRow {
	_, kindArgs := aggDedupeNodeKinds(kinds)
	if len(kindArgs) == 0 {
		return nil
	}
	// Bind order follows placeholder order: the two degree subqueries, then
	// the kind IN-list, then the node-side generation, then the path prefix.
	args := []any{s.viewGen, s.viewGen}
	args = append(args, kindArgs...)
	args = append(args, s.viewGen)
	q := `SELECT n.id,
		(SELECT COUNT(*) FROM edges e WHERE e.to_id = n.id AND e.view_gen = ?) AS in_count,
		(SELECT COUNT(*) FROM edges e WHERE e.from_id = n.id AND e.view_gen = ?) AS out_count
	FROM nodes n
	WHERE n.kind IN (` + inPlaceholders(len(kindArgs)) + `) AND n.view_gen = ?`
	if pathPrefix != "" {
		pred, pargs := pathPrefixPredicate("n.file_path", pathPrefix)
		q += ` AND ` + pred
		args = append(args, pargs...)
	}
	q += ` ORDER BY n.id`
	rows, err := s.db.Query(q, args...)
	panicOnFatal(err)
	if rows == nil {
		// swallowed teardown-race error: read returns empty (see panicOnFatal)
		return nil
	}
	defer rows.Close()
	var out []graph.NodeDegreeRow
	for rows.Next() {
		var r graph.NodeDegreeRow
		panicOnFatal(rows.Scan(&r.NodeID, &r.InCount, &r.OutCount))
		out = append(out, r)
	}
	panicOnFatal(rows.Err())
	return out
}

// pathPrefixPredicate returns a WHERE predicate (and its args) matching
// column against every store spelling of a path prefix — the '/'-joined
// form and the repo-prefixed native form (see graphpath.PrefixForms).
// The column itself cannot be normalized without defeating its index.
func pathPrefixPredicate(column, pathPrefix string) (string, []any) {
	forms := graphpath.PrefixForms(pathPrefix)
	ors := make([]string, len(forms))
	args := make([]any, len(forms))
	for i, form := range forms {
		ors[i] = column + ` LIKE ? ESCAPE '\'`
		args[i] = escapeLikePattern(form) + "%"
	}
	return "(" + strings.Join(ors, " OR ") + ")", args
}

// nodesInFilesByKindQuery builds the per-chunk projection SQL. Pure string
// assembly (no I/O) so the plan-lock test can EXPLAIN the exact query the
// store executes (store_bfs.go precedent).
//
// The kind predicate is written +kind to disqualify nodes_by_kind. A
// stats-blind planner (any store before its first ANALYZE) costs the two
// candidate indexes by IN-probe count alone, and the kind list is always
// shorter than the file list — measured on a 480k-node workspace it served
// this projection by walking whole kind ranges (~117k index entries, one
// main-table B-tree seek each) per call instead of ~50-row file probes. The
// unary + keeps nodes_by_file the only eligible index without INDEXED BY's
// hard error while a bulk-load window has the droppable indexes off. The
// former ORDER BY id forced a temp B-tree per chunk; callers get a Go-side
// sort instead.
func nodesInFilesByKindQuery(files, kinds int) string {
	return `SELECT ` + lookupNodeCols + ` FROM nodes WHERE file_path IN (` +
		inPlaceholders(files) + `) AND +kind IN (` + inPlaceholders(kinds) + `) AND view_gen = ?`
}

// NodesInFilesByKind returns every node living in one of the supplied
// files whose kind is in the supplied set, ID-sorted.
func (s *Store) NodesInFilesByKind(files []string, kinds []graph.NodeKind) []*graph.Node {
	var out []*graph.Node
	for _, nodes := range s.NodesInFilesByKindSeq(files, kinds) {
		out = append(out, nodes...)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// NodesInFilesByKindSeq streams the same projection grouped per file, in
// the caller's (deduped) file order, without materialising the cross-file
// slice. Each yielded group is ID-sorted; files with no matching node yield
// nothing, so callers that cache negative results track the yielded set
// against their request themselves.
func (s *Store) NodesInFilesByKindSeq(files []string, kinds []graph.NodeKind) iter.Seq2[string, []*graph.Node] {
	uniqFiles := dedupeNonEmpty(files)
	_, kindArgs := aggDedupeNodeKinds(kinds)
	return func(yield func(string, []*graph.Node) bool) {
		if len(uniqFiles) == 0 || len(kindArgs) == 0 {
			return
		}
		for i := 0; i < len(uniqFiles); i += lookupChunkSize {
			end := minInt(i+lookupChunkSize, len(uniqFiles))
			chunk := uniqFiles[i:end]
			args := append(toAnyArgs(chunk), kindArgs...)
			args = append(args, s.viewGen)
			byFile := make(map[string][]*graph.Node, len(chunk))
			for _, n := range s.queryNodesSQL(nodesInFilesByKindQuery(len(chunk), len(kindArgs)), args...) {
				byFile[n.FilePath] = append(byFile[n.FilePath], n)
			}
			for _, file := range chunk {
				nodes := byFile[file]
				if len(nodes) == 0 {
					continue
				}
				sort.Slice(nodes, func(a, b int) bool { return nodes[a].ID < nodes[b].ID })
				if !yield(file, nodes) {
					return
				}
			}
		}
	}
}

// FileImportCounts returns per-target-file incoming-import counts. A
// nil scope counts every import edge; a non-nil scope bounds counts to
// edges whose target node ID lies in the slice (empty non-nil => nil).
func (s *Store) FileImportCounts(scope []string) []graph.FileImportCountRow {
	if scope != nil && len(scope) == 0 {
		return nil
	}
	base := `SELECT COALESCE(NULLIF(n.file_path, ''), n.id) AS path, COUNT(*) AS cnt
		FROM edges e JOIN nodes n ON e.to_id = n.id AND n.view_gen = e.view_gen
		WHERE e.kind = ? AND e.view_gen = ?`
	args := []any{string(graph.EdgeImports), s.viewGen}
	fileToCount := make(map[string]int)
	if scope == nil {
		q := base + ` GROUP BY path`
		aggScanImportCounts(s, q, args, fileToCount)
	} else {
		uniq := dedupeNonEmpty(scope)
		if len(uniq) == 0 {
			return nil
		}
		for i := 0; i < len(uniq); i += lookupChunkSize {
			end := minInt(i+lookupChunkSize, len(uniq))
			chunk := uniq[i:end]
			q := base + ` AND e.to_id IN (` + inPlaceholders(len(chunk)) + `) GROUP BY path`
			aggScanImportCounts(s, q, append(append([]any(nil), args...), toAnyArgs(chunk)...), fileToCount)
		}
	}
	if len(fileToCount) == 0 {
		return nil
	}
	out := make([]graph.FileImportCountRow, 0, len(fileToCount))
	for path, cnt := range fileToCount {
		out = append(out, graph.FileImportCountRow{FilePath: path, Count: cnt})
	}
	return out
}

// aggScanImportCounts runs an import-count query and folds the (path,
// count) rows into the accumulator (chunked scopes can revisit a path).
func aggScanImportCounts(s *Store, q string, args []any, acc map[string]int) {
	rows, err := s.db.Query(q, args...)
	panicOnFatal(err)
	if rows == nil {
		// swallowed teardown-race error: read returns empty (see panicOnFatal)
		return
	}
	defer rows.Close()
	for rows.Next() {
		var path string
		var cnt int
		panicOnFatal(rows.Scan(&path, &cnt))
		acc[path] += cnt
	}
	panicOnFatal(rows.Err())
}

// InDegreeForNodes returns total incoming-edge counts (any kind) for
// the supplied node id set.
func (s *Store) InDegreeForNodes(ids []string) map[string]int {
	uniq := dedupeNonEmpty(ids)
	if len(uniq) == 0 {
		return nil
	}
	out := make(map[string]int)
	for i := 0; i < len(uniq); i += lookupChunkSize {
		end := minInt(i+lookupChunkSize, len(uniq))
		chunk := uniq[i:end]
		q := `SELECT to_id, COUNT(*) FROM edges WHERE to_id IN (` +
			inPlaceholders(len(chunk)) + `) AND view_gen = ? GROUP BY to_id`
		rows, err := s.db.Query(q, append(toAnyArgs(chunk), s.viewGen)...)
		panicOnFatal(err)
		if rows == nil {
			// swallowed teardown-race error: read returns empty (see panicOnFatal)
			return out
		}
		for rows.Next() {
			var id string
			var n int
			panicOnFatal(rows.Scan(&id, &n))
			out[id] = n
		}
		panicOnFatal(rows.Err())
		_ = rows.Close()
	}
	return out
}

// CrossRepoEdgeCounts returns pre-grouped cross-repo edge counts keyed
// by (base kind, from-repo, to-repo). Cross-repo kinds are those
// graph.BaseKindForCrossRepo recognises; the count is reported under
// the base kind.
func (s *Store) CrossRepoEdgeCounts() []graph.CrossRepoEdgeRow {
	q := `SELECT e.kind, nf.repo_prefix, nt.repo_prefix, COUNT(*)
		FROM edges e
		JOIN nodes nf ON e.from_id = nf.id AND nf.view_gen = e.view_gen
		JOIN nodes nt ON e.to_id = nt.id AND nt.view_gen = e.view_gen
		WHERE nf.repo_prefix <> nt.repo_prefix AND e.view_gen = ?
		GROUP BY e.kind, nf.repo_prefix, nt.repo_prefix`
	rows, err := s.db.Query(q, s.viewGen)
	panicOnFatal(err)
	if rows == nil {
		// swallowed teardown-race error: read returns empty (see panicOnFatal)
		return nil
	}
	defer rows.Close()
	// Aggregate keyed by the edge's OWN kind (cross_repo_*), NOT the base.
	// BaseKindForCrossRepo is used only as the recogniser that decides
	// whether an edge participates — parity with the in-memory store.
	type key struct {
		kind graph.EdgeKind
		from string
		to   string
	}
	acc := make(map[key]int)
	for rows.Next() {
		var kind, from, to string
		var n int
		panicOnFatal(rows.Scan(&kind, &from, &to, &n))
		ek := graph.EdgeKind(kind)
		if _, ok := graph.BaseKindForCrossRepo(ek); !ok {
			continue
		}
		acc[key{kind: ek, from: from, to: to}] += n
	}
	panicOnFatal(rows.Err())
	if len(acc) == 0 {
		return nil
	}
	out := make([]graph.CrossRepoEdgeRow, 0, len(acc))
	for k, n := range acc {
		out = append(out, graph.CrossRepoEdgeRow{Kind: k.kind, FromRepo: k.from, ToRepo: k.to, Count: n})
	}
	return out
}

// FileImporters returns the importing-node rows for every EdgeImports
// edge whose target's FilePath OR ID equals filePath.
func (s *Store) FileImporters(filePath string) []graph.FileImporterRow {
	if filePath == "" {
		return nil
	}
	q := `SELECT nf.file_path, nf.id, nf.name, nf.kind
		FROM edges e
		JOIN nodes nt ON e.to_id = nt.id AND nt.view_gen = e.view_gen
		JOIN nodes nf ON e.from_id = nf.id AND nf.view_gen = e.view_gen
		WHERE e.kind = ? AND (nt.file_path = ? OR nt.id = ?) AND e.view_gen = ?
		ORDER BY nf.file_path`
	rows, err := s.db.Query(q, string(graph.EdgeImports), filePath, filePath, s.viewGen)
	panicOnFatal(err)
	if rows == nil {
		// swallowed teardown-race error: read returns empty (see panicOnFatal)
		return nil
	}
	defer rows.Close()
	var out []graph.FileImporterRow
	for rows.Next() {
		var r graph.FileImporterRow
		var kind string
		panicOnFatal(rows.Scan(&r.FromFile, &r.FromID, &r.FromName, &kind))
		r.FromKind = graph.NodeKind(kind)
		out = append(out, r)
	}
	panicOnFatal(rows.Err())
	return out
}

// FileSymbolNamesByPaths returns the distinct (file, name) pairs for
// nodes in the supplied paths whose kind is in the set, sorted by
// (file, name).
func (s *Store) FileSymbolNamesByPaths(paths []string, kinds []graph.NodeKind) []graph.FileSymbolNameRow {
	uniqPaths := dedupeNonEmpty(paths)
	_, kindArgs := aggDedupeNodeKinds(kinds)
	if len(uniqPaths) == 0 || len(kindArgs) == 0 {
		return nil
	}
	var out []graph.FileSymbolNameRow
	for i := 0; i < len(uniqPaths); i += lookupChunkSize {
		end := minInt(i+lookupChunkSize, len(uniqPaths))
		chunk := uniqPaths[i:end]
		args := append(toAnyArgs(chunk), kindArgs...)
		args = append(args, s.viewGen)
		q := `SELECT DISTINCT file_path, name FROM nodes WHERE file_path IN (` +
			inPlaceholders(len(chunk)) + `) AND kind IN (` + inPlaceholders(len(kindArgs)) + `) AND view_gen = ?`
		rows, err := s.db.Query(q, args...)
		panicOnFatal(err)
		if rows == nil {
			// swallowed teardown-race error: read returns empty (see panicOnFatal)
			return out
		}
		for rows.Next() {
			var r graph.FileSymbolNameRow
			panicOnFatal(rows.Scan(&r.FilePath, &r.Name))
			out = append(out, r)
		}
		panicOnFatal(rows.Err())
		_ = rows.Close()
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].FilePath != out[j].FilePath {
			return out[i].FilePath < out[j].FilePath
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// EdgesByKinds streams every edge whose kind is in the supplied set;
// honours early-stop. Empty kinds yields nothing.
func (s *Store) EdgesByKinds(kinds []graph.EdgeKind) iter.Seq[*graph.Edge] {
	_, args := aggDedupeEdgeKinds(kinds)
	return func(yield func(*graph.Edge) bool) {
		if len(args) == 0 {
			return
		}
		q := `SELECT ` + lookupEdgeCols + ` FROM edges WHERE kind IN (` +
			inPlaceholders(len(args)) + `) AND view_gen = ? ORDER BY id`
		for _, e := range s.queryEdgesSQL(q, append(args, s.viewGen)...) {
			if e == nil {
				continue
			}
			if !yield(e) {
				return
			}
		}
	}
}

// externalCallTargetPredicate selects edges whose target is an
// external-package terminal (dep:: / stdlib:: / external::, including the
// per-repo-prefixed stdlib form) or an already-materialised
// external-call:: node. Shared verbatim by ExternalCallCandidateEdges and
// the edges_external partial index (schema.go) so SQLite matches the
// partial index for the query — keep the two identical.
const externalCallTargetPredicate = `(to_id GLOB 'dep::*' OR to_id GLOB 'external::*' OR to_id GLOB 'stdlib::*' OR to_id GLOB '*::stdlib::*' OR to_id GLOB 'external-call::*')`

// ExternalCallCandidateEdges implements graph.ExternalCallCandidates: it
// returns only the call / reference edges the external-call synthesizer
// might act on, selected server-side so the whole call-edge table never
// crosses into Go just to be prefix-filtered. The GLOB predicate is
// served by the edges_external partial index.
func (s *Store) ExternalCallCandidateEdges() []*graph.Edge {
	q := `SELECT ` + lookupEdgeCols + ` FROM edges
		WHERE kind IN ('calls','references') AND ` + externalCallTargetPredicate + `
		  AND view_gen = ?
		ORDER BY id`
	return s.queryEdgesSQL(q, s.viewGen)
}

// DistinctExternalTargets performs the Go external-attribution discovery as
// one SQLite aggregate. Only distinct destination IDs cross the driver; full
// edge payloads (especially Meta) stay disk-resident.
func (s *Store) DistinctExternalTargets(kinds []graph.EdgeKind) []string {
	_, args := aggDedupeEdgeKinds(kinds)
	if len(args) == 0 {
		return nil
	}
	q := `SELECT DISTINCT to_id FROM edges
		WHERE kind IN (` + inPlaceholders(len(args)) + `) AND ` + externalCallTargetPredicate + `
		  AND view_gen = ?
		ORDER BY to_id`
	rows, err := s.db.Query(q, append(args, s.viewGen)...)
	panicOnFatal(err)
	if rows == nil {
		return nil
	}
	defer rows.Close()

	var targets []string
	for rows.Next() {
		var target string
		panicOnFatal(rows.Scan(&target))
		targets = append(targets, target)
	}
	panicOnFatal(rows.Err())
	return targets
}

// NodesByKinds returns every node whose kind is in the supplied set.
func (s *Store) NodesByKinds(kinds []graph.NodeKind) []*graph.Node {
	_, args := aggDedupeNodeKinds(kinds)
	if len(args) == 0 {
		return nil
	}
	q := `SELECT ` + lookupNodeCols + ` FROM nodes WHERE kind IN (` +
		inPlaceholders(len(args)) + `) AND view_gen = ? ORDER BY id`
	return s.queryNodesSQL(q, append(args, s.viewGen)...)
}

// EdgeAdjacencyForKinds streams (from, to) id pairs for edges whose
// kind is in edgeKinds and whose endpoints both have a kind in
// nodeKinds; honours early-stop. Empty kinds yields nothing.
func (s *Store) EdgeAdjacencyForKinds(edgeKinds []graph.EdgeKind, nodeKinds []graph.NodeKind) iter.Seq[[2]string] {
	_, eArgs := aggDedupeEdgeKinds(edgeKinds)
	_, nArgs := aggDedupeNodeKinds(nodeKinds)
	return func(yield func([2]string) bool) {
		if len(eArgs) == 0 || len(nArgs) == 0 {
			return
		}
		args := append([]any(nil), eArgs...)
		args = append(args, nArgs...)
		args = append(args, nArgs...)
		args = append(args, s.viewGen)
		q := `SELECT e.from_id, e.to_id
			FROM edges e
			JOIN nodes nf ON e.from_id = nf.id AND nf.view_gen = e.view_gen
			JOIN nodes nt ON e.to_id = nt.id AND nt.view_gen = e.view_gen
			WHERE e.kind IN (` + inPlaceholders(len(eArgs)) + `)
			AND nf.kind IN (` + inPlaceholders(len(nArgs)) + `)
			AND nt.kind IN (` + inPlaceholders(len(nArgs)) + `)
			AND e.view_gen = ?`
		rows, err := s.db.Query(q, args...)
		panicOnFatal(err)
		if rows == nil {
			// swallowed teardown-race error: read returns empty (see panicOnFatal)
			return
		}
		defer rows.Close()
		for rows.Next() {
			var from, to string
			panicOnFatal(rows.Scan(&from, &to))
			if !yield([2]string{from, to}) {
				return
			}
		}
		panicOnFatal(rows.Err())
	}
}

// NodeDegreeCounts returns per-node in/out/usage-in edge counts for the
// supplied id set. Unknown ids produce no row; duplicates collapse.
func (s *Store) NodeDegreeCounts(ids []string, usageKinds []graph.EdgeKind) []graph.NodeDegreeRow {
	uniq := dedupeNonEmpty(ids)
	if len(uniq) == 0 {
		return nil
	}
	_, usageArgs := aggDedupeEdgeKinds(usageKinds)
	out := make([]graph.NodeDegreeRow, 0, len(uniq))
	for i := 0; i < len(uniq); i += lookupChunkSize {
		end := minInt(i+lookupChunkSize, len(uniq))
		chunk := uniq[i:end]
		// Usage-in subquery: a literal 0 when no usage kinds are given.
		usageExpr := `0`
		var usageInline []any
		if len(usageArgs) > 0 {
			usageExpr = `(SELECT COUNT(*) FROM edges e WHERE e.to_id = n.id AND e.kind IN (` +
				inPlaceholders(len(usageArgs)) + `) AND e.view_gen = ?)`
			usageInline = append(append([]any(nil), usageArgs...), s.viewGen)
		}
		q := `SELECT n.id,
			(SELECT COUNT(*) FROM edges e WHERE e.to_id = n.id AND e.view_gen = ?) AS in_count,
			(SELECT COUNT(*) FROM edges e WHERE e.from_id = n.id AND e.view_gen = ?) AS out_count,
			` + usageExpr + ` AS usage_in
		FROM nodes n
		WHERE n.id IN (` + inPlaceholders(len(chunk)) + `) AND n.view_gen = ?`
		// Bind order matches placeholder order: the two degree subqueries,
		// the usage subquery, the id IN-list, then the node-side generation.
		args := []any{s.viewGen, s.viewGen}
		args = append(args, usageInline...)
		args = append(args, toAnyArgs(chunk)...)
		args = append(args, s.viewGen)
		rows, err := s.db.Query(q, args...)
		panicOnFatal(err)
		if rows == nil {
			// swallowed teardown-race error: read returns empty (see panicOnFatal)
			return out
		}
		for rows.Next() {
			var r graph.NodeDegreeRow
			panicOnFatal(rows.Scan(&r.NodeID, &r.InCount, &r.OutCount, &r.UsageInCount))
			out = append(out, r)
		}
		panicOnFatal(rows.Err())
		_ = rows.Close()
	}
	return out
}

// NodeFanCounts returns per-node fan-in (incoming edges in fanInKinds)
// and fan-out (outgoing edges in fanOutKinds) for the supplied id set.
// Unknown ids produce no row; duplicates collapse.
func (s *Store) NodeFanCounts(ids []string, fanInKinds, fanOutKinds []graph.EdgeKind) []graph.NodeFanRow {
	uniq := dedupeNonEmpty(ids)
	if len(uniq) == 0 {
		return nil
	}
	_, inArgs := aggDedupeEdgeKinds(fanInKinds)
	_, outArgs := aggDedupeEdgeKinds(fanOutKinds)
	out := make([]graph.NodeFanRow, 0, len(uniq))
	for i := 0; i < len(uniq); i += lookupChunkSize {
		end := minInt(i+lookupChunkSize, len(uniq))
		chunk := uniq[i:end]

		fanInExpr := `0`
		var inInline []any
		if len(inArgs) > 0 {
			fanInExpr = `(SELECT COUNT(*) FROM edges e WHERE e.to_id = n.id AND e.kind IN (` +
				inPlaceholders(len(inArgs)) + `) AND e.view_gen = ?)`
			inInline = append(append([]any(nil), inArgs...), s.viewGen)
		}
		fanOutExpr := `0`
		var outInline []any
		if len(outArgs) > 0 {
			fanOutExpr = `(SELECT COUNT(*) FROM edges e WHERE e.from_id = n.id AND e.kind IN (` +
				inPlaceholders(len(outArgs)) + `) AND e.view_gen = ?)`
			outInline = append(append([]any(nil), outArgs...), s.viewGen)
		}
		q := `SELECT n.id, ` + fanInExpr + ` AS fan_in, ` + fanOutExpr + ` AS fan_out
		FROM nodes n
		WHERE n.id IN (` + inPlaceholders(len(chunk)) + `) AND n.view_gen = ?`
		// Bind order matches placeholder order in the SELECT list: fan-in
		// subquery, fan-out subquery, the id IN-list, then the node generation.
		args := append([]any(nil), inInline...)
		args = append(args, outInline...)
		args = append(args, toAnyArgs(chunk)...)
		args = append(args, s.viewGen)
		rows, err := s.db.Query(q, args...)
		panicOnFatal(err)
		if rows == nil {
			// swallowed teardown-race error: read returns empty (see panicOnFatal)
			return out
		}
		for rows.Next() {
			var r graph.NodeFanRow
			panicOnFatal(rows.Scan(&r.NodeID, &r.FanIn, &r.FanOut))
			out = append(out, r)
		}
		panicOnFatal(rows.Err())
		_ = rows.Close()
	}
	return out
}

// CommunityCrossingsByKind returns per-source crossing counts for edges
// whose kind is in the supplied set, given a node→community map. A
// crossing is an edge whose source community differs from its target
// community; zero-count sources are dropped. Empty kinds or empty
// community map returns nil. The community comparison runs Go-side
// because community membership is not a node column.
func (s *Store) CommunityCrossingsByKind(kinds []graph.EdgeKind, nodeToComm map[string]string) map[string]int {
	_, args := aggDedupeEdgeKinds(kinds)
	if len(args) == 0 || len(nodeToComm) == 0 {
		return nil
	}
	q := `SELECT from_id, to_id FROM edges WHERE kind IN (` + inPlaceholders(len(args)) + `) AND view_gen = ?`
	rows, err := s.db.Query(q, append(args, s.viewGen)...)
	panicOnFatal(err)
	if rows == nil {
		// swallowed teardown-race error: read returns empty (see panicOnFatal)
		return nil
	}
	defer rows.Close()
	out := make(map[string]int)
	for rows.Next() {
		var from, to string
		panicOnFatal(rows.Scan(&from, &to))
		fromComm, ok := nodeToComm[from]
		if !ok {
			continue
		}
		toComm, ok := nodeToComm[to]
		if !ok {
			continue
		}
		if fromComm != toComm {
			out[from]++
		}
	}
	panicOnFatal(rows.Err())
	if len(out) == 0 {
		return nil
	}
	return out
}
