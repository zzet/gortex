package store_sqlite

import (
	"context"
	"encoding/json"
	"iter"
	"slices"
	"sort"

	"github.com/zzet/gortex/internal/graph"
)

var _ graph.ScopedDeadCodeReader = (*Store)(nil)
var _ graph.FilteredScopedDeadCodeReader = (*Store)(nil)
var _ graph.NodeScopedDeadCodeReader = (*Store)(nil)
var _ graph.FilteredNodeScopedDeadCodeReader = (*Store)(nil)

const effectiveWorkspaceSQL = `COALESCE(NULLIF(workspace_id, ''), repo_prefix)`
const nodesByEffectiveWorkspaceDDL = `CREATE INDEX IF NOT EXISTS nodes_by_effective_workspace ON nodes(` + effectiveWorkspaceSQL + `, view_gen, kind, id)`
const nodesDeadCodeEligibleWorkspaceDDL = `CREATE INDEX IF NOT EXISTS nodes_deadcode_eligible_workspace ON nodes(` + effectiveWorkspaceSQL + `, view_gen, kind, id, external, workspace_id, repo_prefix) WHERE external IS NOT 1`

// Ordinary workspace nodes and the unowned global-external exception occupy
// disjoint effective-workspace buckets. The complete NodeScope predicate still
// checks every row, including project/repo policy and the external path proof.
func deadCodeWorkspaceBuckets(scope *graph.NodeScope, prefixes []string) []string {
	if prefixes == nil && scope != nil && scope.WorkspaceID != "" {
		return []string{scope.WorkspaceID, ""}
	}
	return nil
}

func (s *Store) DeadCodeCandidatesInScope(kinds []graph.NodeKind, incoming map[graph.NodeKind][]graph.EdgeKind, prefixes []string, scope graph.NodeScope, exclude graph.DeadCodeCandidateExclusions) []*graph.Node {
	nodes, _ := s.DeadCodeCandidatesInScopeFiltered(kinds, incoming, prefixes, scope, graph.OutlineEdgeFilter{}, exclude)
	return nodes
}

func sortedDeadCodeRepos(prefixes []string) []string {
	if prefixes == nil {
		return nil
	}
	out := slices.Clone(prefixes)
	slices.Sort(out)
	return slices.Compact(out)
}

// nodes is WITHOUT ROWID: nodes_by_repo(repo_prefix, view_gen) also carries
// the remaining primary-key id. Repo/generation equality makes id a keyset
// seek and supplies ORDER BY without repeatedly sorting the ownership bucket.
func deadCodeScopedVerdictPageSQL(allowed int) string {
	q, _ := deadCodeFilteredVerdictPageSQL(allowed, true, graph.OutlineEdgeFilter{})
	return q
}

func deadCodeFilteredVerdictPageSQL(allowed int, scoped bool, filter graph.OutlineEdgeFilter) (string, []any) {
	return deadCodeNodeScopeVerdictPageSQL(allowed, scoped, false, nil, filter)
}

func deadCodeNodeScopeVerdictPageSQL(allowed int, scoped, workspaceSeek bool, scope *graph.NodeScope, filter graph.OutlineEdgeFilter, exclusions ...graph.DeadCodeCandidateExclusions) (string, []any) {
	return deadCodeNodeScopeVerdictPageSQLInWorkspace(allowed, scoped, workspaceSeek, false, scope, filter, exclusions...)
}

func deadCodeNodeScopeVerdictPageSQLInWorkspace(allowed int, scoped, workspaceSeek, workspaceProven bool, scope *graph.NodeScope, filter graph.OutlineEdgeFilter, exclusions ...graph.DeadCodeCandidateExclusions) (string, []any) {
	probe := `NOT EXISTS (SELECT 1 FROM edges e `
	probe += `WHERE e.to_id = n.id AND e.view_gen = n.view_gen`
	if allowed > 0 {
		probe += ` AND e.kind IN (` + inPlaceholders(allowed) + `)`
	}
	edgeSQL, edgeArgs := outlineEdgePredicate(filter, "", "n")
	viaSQL, viaArgs := outlineViaPredicate(filter.ExcludeVia)
	probe += edgeSQL
	if len(filter.ExcludeNodeKinds) > 0 {
		// nodes_by_kind carries the WITHOUT ROWID identity. Looking for an
		// excluded kind avoids decoding/seeking the wide source node per edge.
		// Missing/NULL kinds still count as incoming usage, as with LEFT JOIN.
		data, _ := json.Marshal(filter.ExcludeNodeKinds)
		probe += ` AND NOT EXISTS (SELECT 1 FROM nodes hidden_source INDEXED BY nodes_by_kind WHERE hidden_source.view_gen=e.view_gen AND hidden_source.kind IN (SELECT CAST(value AS TEXT) FROM json_each(?)) AND hidden_source.id=e.from_id)`
		edgeArgs = append(edgeArgs, string(data))
	}
	probe += viaSQL + `)`
	q := `SELECT n.id, ` + probe + ` FROM nodes AS n`
	if scoped {
		q += ` INDEXED BY nodes_by_repo WHERE n.view_gen = ? AND n.repo_prefix = ?`
	} else if workspaceSeek {
		index := "nodes_by_effective_workspace"
		if len(exclusions) > 0 && exclusions[0].External {
			// Only explicit eligible-candidate reads imply this partial key.
			// Raw scope reads still include marked external declarations.
			index = "nodes_deadcode_eligible_workspace"
		}
		q += ` INDEXED BY ` + index + ` WHERE n.view_gen = ? AND ` + effectiveWorkspaceSQL + ` = ?`
	} else {
		q += ` WHERE n.view_gen = ?`
	}
	args := append(edgeArgs, viaArgs...)
	if len(exclusions) > 0 {
		q += deadCodeCandidateExclusionSQL(exclusions[0])
	}
	if scope != nil {
		predicate, scopeArgs := outlineScopePredicateWithWorkspace(*scope, "n", workspaceProven)
		q += ` AND ` + predicate
		args = append(args, scopeArgs...)
	}
	q += ` AND n.id > ? AND n.kind = ? ORDER BY n.id LIMIT ?`
	return q, args
}

func deadCodeCandidateExclusionSQL(exclude graph.DeadCodeCandidateExclusions) string {
	var q string
	if exclude.External {
		// NULL includes legacy blob-only metadata. Keep those rows for the
		// decoded analyzer check instead of assuming absent promoted data is false.
		q += ` AND n.external IS NOT 1`
	}
	if exclude.Stubs {
		// Match graph.StubKind's first two segments, case sensitively. The
		// generated is_stub column uses LIKE and cannot distinguish MODULE.
		for _, kind := range []string{graph.StubKindStdlib, graph.StubKindBuiltin, graph.StubKindExternalCall, graph.StubKindModule} {
			q += ` AND NOT (n.id GLOB '` + kind + `::*' OR (instr(n.id, '::') > 0 AND substr(n.id, instr(n.id, '::') + 2) GLOB '` + kind + `::*'))`
		}
	}
	return q
}

const deadCodeScopedFilesPageSQL = `SELECT ` + lookupNodeCols + ` FROM nodes INDEXED BY nodes_by_repo
WHERE view_gen = ? AND repo_prefix = ? AND id > ? AND kind = ? ORDER BY id LIMIT ?`

func (s *Store) DeadCodeCandidatesInRepos(kinds []graph.NodeKind, incoming map[graph.NodeKind][]graph.EdgeKind, prefixes []string) []*graph.Node {
	out, _ := s.DeadCodeCandidatesInReposContext(s.readContext(), kinds, incoming, prefixes)
	return out
}

// DeadCodeCandidatesInReposContext seeks only the requested ownership buckets.
// The same-generation incoming-usage probe deliberately has no repo predicate:
// a caller in another repo keeps the candidate alive.
func (s *Store) DeadCodeCandidatesInReposContext(ctx context.Context, kinds []graph.NodeKind, incoming map[graph.NodeKind][]graph.EdgeKind, prefixes []string) ([]*graph.Node, error) {
	if prefixes == nil {
		return s.DeadCodeCandidatesContext(ctx, kinds, incoming)
	}
	return s.deadCodeCandidatesInReposFiltered(ctx, kinds, incoming, prefixes, graph.OutlineEdgeFilter{})
}

func (s *Store) DeadCodeCandidatesInReposFiltered(kinds []graph.NodeKind, incoming map[graph.NodeKind][]graph.EdgeKind, prefixes []string, filter graph.OutlineEdgeFilter) ([]*graph.Node, error) {
	return s.deadCodeCandidatesInReposFiltered(s.readContext(), kinds, incoming, prefixes, filter)
}

func (s *Store) deadCodeCandidatesInReposFiltered(ctx context.Context, kinds []graph.NodeKind, incoming map[graph.NodeKind][]graph.EdgeKind, prefixes []string, filter graph.OutlineEdgeFilter) ([]*graph.Node, error) {
	return s.deadCodeCandidatesInScopeFiltered(ctx, kinds, incoming, prefixes, nil, filter)
}

func (s *Store) DeadCodeCandidatesInScopeFiltered(kinds []graph.NodeKind, incoming map[graph.NodeKind][]graph.EdgeKind, prefixes []string, scope graph.NodeScope, filter graph.OutlineEdgeFilter, exclude graph.DeadCodeCandidateExclusions) ([]*graph.Node, error) {
	return s.deadCodeCandidatesInScopeFiltered(s.readContext(), kinds, incoming, prefixes, &scope, filter, exclude)
}

func (s *Store) deadCodeCandidatesInScopeFiltered(ctx context.Context, kinds []graph.NodeKind, incoming map[graph.NodeKind][]graph.EdgeKind, prefixes []string, scope *graph.NodeScope, filter graph.OutlineEdgeFilter, exclusions ...graph.DeadCodeCandidateExclusions) ([]*graph.Node, error) {
	repos := sortedDeadCodeRepos(prefixes)
	if prefixes == nil {
		repos = []string{""} // One global pass; no ownership predicate or argument.
	}
	workspaceBuckets := deadCodeWorkspaceBuckets(scope, prefixes)
	if workspaceBuckets != nil {
		repos = workspaceBuckets
	}

	var out []*graph.Node
	for _, kind := range kinds {
		var candidates []*graph.Node
		allowed := anaDedupeEdgeKinds(incoming[kind])
		for _, prefix := range repos {
			workspaceProven := workspaceBuckets != nil && prefix != "" && scope != nil && prefix == scope.WorkspaceID
			q, filterArgs := deadCodeNodeScopeVerdictPageSQLInWorkspace(len(allowed), prefixes != nil, workspaceBuckets != nil, workspaceProven, scope, filter, exclusions...)
			// SELECT-list exclusions precede generation/ownership and scope.
			var scopeArgs []any
			if scope != nil {
				_, scopeArgs = outlineScopePredicateWithWorkspace(*scope, "n", workspaceProven)
				filterArgs = filterArgs[:len(filterArgs)-len(scopeArgs)]
			}
			after := ""
			for {
				if err := ctx.Err(); err != nil {
					return append(out, candidates...), err
				}
				args := make([]any, 0, len(allowed)+5)
				for _, edgeKind := range allowed {
					args = append(args, string(edgeKind))
				}
				args = append(args, filterArgs...)
				args = append(args, s.viewGen)
				if prefixes != nil || workspaceBuckets != nil {
					args = append(args, prefix)
				}
				args = append(args, scopeArgs...)
				args = append(args, after, string(kind), deadCodePageSize)
				dead, scanned, lastID, err := s.scanDeadCodeVerdictPage(ctx, q, args...)
				if err != nil {
					return append(out, candidates...), err
				}
				if len(dead) > 0 {
					nodes, err := s.nodesByIDsOrdered(ctx, dead)
					candidates = append(candidates, nodes...)
					if err != nil {
						return append(out, candidates...), err
					}
				}
				if scanned < deadCodePageSize {
					break
				}
				after = lastID
			}
		}
		// Preserve the unscoped kind/id order even when prefix spelling and
		// node ids do not share an ordering convention.
		sort.Slice(candidates, func(i, j int) bool { return candidates[i].ID < candidates[j].ID })
		out = append(out, candidates...)
	}
	return out, nil
}

func (s *Store) IfaceImplementsRowsInRepos(prefixes []string) []graph.IfaceImplementsRow {
	out, _ := s.IfaceImplementsRowsInReposContext(s.readContext(), prefixes)
	return out
}

func (s *Store) IfaceImplementsRowsInReposContext(ctx context.Context, prefixes []string) ([]graph.IfaceImplementsRow, error) {
	return s.ifaceImplementsRowsInReposFiltered(ctx, prefixes, graph.OutlineEdgeFilter{})
}

func (s *Store) IfaceImplementsRowsInReposFiltered(prefixes []string, filter graph.OutlineEdgeFilter) ([]graph.IfaceImplementsRow, error) {
	return s.ifaceImplementsRowsInReposFiltered(s.readContext(), prefixes, filter)
}

func (s *Store) ifaceImplementsRowsInReposFiltered(ctx context.Context, prefixes []string, filter graph.OutlineEdgeFilter) ([]graph.IfaceImplementsRow, error) {
	repos := sortedDeadCodeRepos(prefixes)
	if prefixes == nil {
		repos = []string{""}
	}
	var out []graph.IfaceImplementsRow
	for _, prefix := range repos {
		var q string
		var args []any
		if prefixes == nil {
			// Preserve legacy dangling implementing-type tuples in the global view.
			q = `SELECT e.from_id, n.id, n.meta FROM edges e
LEFT JOIN nodes implementing ON implementing.id = e.from_id AND implementing.view_gen = e.view_gen
JOIN nodes n ON n.id = e.to_id AND n.view_gen = e.view_gen
WHERE e.kind = ? AND n.kind = ? AND n.meta IS NOT NULL AND e.view_gen = ?`
			args = []any{string(graph.EdgeImplements), string(graph.KindInterface), s.viewGen}
		} else {
			q = `SELECT e.from_id, n.id, n.meta
FROM nodes AS implementing INDEXED BY nodes_by_repo
CROSS JOIN edges e INDEXED BY edges_by_from ON e.from_id = implementing.id AND e.view_gen = implementing.view_gen AND e.kind = ?
JOIN nodes n ON n.id = e.to_id AND n.view_gen = e.view_gen
WHERE implementing.view_gen = ? AND implementing.repo_prefix = ? AND n.kind = ? AND n.meta IS NOT NULL`
			args = []any{string(graph.EdgeImplements), s.viewGen, prefix, string(graph.KindInterface)}
		}
		edgeSQL, edgeArgs := outlineEdgePredicate(filter, "implementing", "n")
		viaSQL, viaArgs := outlineViaPredicate(filter.ExcludeVia)
		args = append(args, edgeArgs...)
		args = append(args, viaArgs...)
		rows, err := outlineRows(ctx, s.db, outlineQuery{sql: q + edgeSQL + viaSQL, args: args})
		if err != nil {
			return nil, err
		}
		part, err := scanIfaceImplementsRows(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, part...)
	}
	return out, nil
}

func (s *Store) NodesByKindInScope(kind graph.NodeKind, prefixes []string, scope graph.NodeScope) iter.Seq[*graph.Node] {
	return func(yield func(*graph.Node) bool) {
		// File metadata is needed for framework-entrypoint exclusions. Filter
		// in SQL before decoding it, retaining the exact selected generation.
		predicate, scopeArgs := outlineScopePredicate(scope, "n")
		buckets := deadCodeWorkspaceBuckets(&scope, prefixes)
		q := `SELECT ` + lookupNodeCols + ` FROM nodes n`
		if buckets != nil {
			q += ` INDEXED BY nodes_by_effective_workspace`
		}
		if prefixes != nil {
			if len(prefixes) == 0 {
				return
			}
			buckets = sortedDeadCodeRepos(prefixes)
			q += ` INDEXED BY nodes_by_repo`
		}
		q += ` WHERE n.view_gen = ? AND ` + predicate
		if prefixes != nil {
			q += ` AND n.repo_prefix = ?`
		} else if buckets != nil {
			q += ` AND ` + effectiveWorkspaceSQL + ` = ?`
		}
		q += ` AND n.id > ? AND n.kind = ? ORDER BY n.id LIMIT ?`
		if buckets == nil {
			buckets = []string{""}
		}
		for _, bucket := range buckets {
			after := ""
			for {
				args := append([]any{s.viewGen}, scopeArgs...)
				if prefixes != nil || scope.WorkspaceID != "" {
					args = append(args, bucket)
				}
				args = append(args, after, string(kind), deadCodePageSize)
				page, scanned, lastID, err := s.scanNodePage(s.readContext(), q, args...)
				if err != nil {
					return
				}
				for _, node := range page {
					if !yield(node) {
						return
					}
				}
				if scanned < deadCodePageSize {
					break
				}
				after = lastID
			}
		}
	}
}

func (s *Store) NodesByKindInRepos(kind graph.NodeKind, prefixes []string) iter.Seq[*graph.Node] {
	if prefixes == nil {
		return s.NodesByKind(kind)
	}
	return func(yield func(*graph.Node) bool) {
		ctx := s.readContext()
		for _, prefix := range sortedDeadCodeRepos(prefixes) {
			after := ""
			for {
				page, scanned, lastID, err := s.scanNodePage(ctx, deadCodeScopedFilesPageSQL, s.viewGen, prefix, after, string(kind), deadCodePageSize)
				if err != nil {
					return
				}
				for _, node := range page {
					if !yield(node) {
						return
					}
				}
				if scanned < deadCodePageSize {
					break
				}
				after = lastID
			}
		}
	}
}
