package store_sqlite

// This file implements the moderate-SQL analysis capability interfaces
// for the SQLite graph.Store backend. Each method mirrors the in-memory
// reference implementation in internal/graph/graph.go and is verified
// against the same conformance suite (internal/graph/storetest).
//
// Shape: push the structural filter into one indexed SELECT via the raw-
// SQL helpers (queryNodesSQL / s.db.Query), then do any Meta-dependent
// (JSON-decoded) or distinct-counting filtering in Go. No new prepared
// statements are added — every query rides the secondary indexes already
// created in schema.go (edges_by_from / edges_by_to / nodes_by_kind).

import (
	"database/sql"
	"fmt"
	"strings"

	"github.com/zzet/gortex/internal/graph"
)

// Compile-time assertions: *Store satisfies each analysis capability.
var _ graph.DeadCodeCandidator = (*Store)(nil)
var _ graph.IfaceImplementsScanner = (*Store)(nil)
var _ graph.MemberMethodsByType = (*Store)(nil)
var _ graph.StructuralParentEdges = (*Store)(nil)
var _ graph.ExtractCandidatesScanner = (*Store)(nil)
var _ graph.CrossRepoCandidates = (*Store)(nil)
var _ graph.ScopedCrossRepoCandidates = (*Store)(nil)
var _ graph.MutationScopedCrossRepoCandidates = (*Store)(nil)
var _ graph.ThrowerErrorSurfacer = (*Store)(nil)

// anaDedupeEdgeKinds drops empty / duplicate edge kinds, preserving
// first-seen order — the EdgeKind twin of dedupeNonEmpty.
func anaDedupeEdgeKinds(in []graph.EdgeKind) []graph.EdgeKind {
	seen := make(map[graph.EdgeKind]struct{}, len(in))
	out := make([]graph.EdgeKind, 0, len(in))
	for _, k := range in {
		if k == "" {
			continue
		}
		if _, ok := seen[k]; ok {
			continue
		}
		seen[k] = struct{}{}
		out = append(out, k)
	}
	return out
}

// --- DeadCodeCandidator -------------------------------------------------

// DeadCodeCandidates returns nodes of the allowed kinds that have no
// incoming edge of the corresponding allowed in-edge kinds. An empty
// per-kind allowlist (or one that dedupes to nothing) means "any incoming
// edge counts as usage". Mirrors graph.(*Graph).DeadCodeCandidates: the
// candidate set is purely structural (the analysis layer applies the
// exported / test / entry-point / synthetic post-filters in Go), so no
// node-id exclusion happens here. The NOT-EXISTS filter runs server-side
// per node kind.
func (s *Store) DeadCodeCandidates(allowedNodeKinds []graph.NodeKind, allowedInEdgeKinds map[graph.NodeKind][]graph.EdgeKind) []*graph.Node {
	if len(allowedNodeKinds) == 0 {
		return nil
	}
	var out []*graph.Node
	for _, nk := range allowedNodeKinds {
		allowed := anaDedupeEdgeKinds(allowedInEdgeKinds[nk])
		anyKindCounts := len(allowed) == 0

		var q string
		var args []any
		// The reachability probe pairs generations with the node it is
		// testing. Without the pairing an incoming edge from any other
		// generation answers "used", and every node reads as reachable.
		if anyKindCounts {
			// Any incoming edge disqualifies the node.
			q = `SELECT ` + lookupNodeCols + ` FROM nodes n
WHERE n.kind = ?
  AND NOT EXISTS (SELECT 1 FROM edges e WHERE e.to_id = n.id AND e.view_gen = n.view_gen)
  AND n.view_gen = ?
ORDER BY n.id`
			args = []any{string(nk), s.viewGen}
		} else {
			// Only an incoming edge of one of the allowed kinds counts.
			q = `SELECT ` + lookupNodeCols + ` FROM nodes n
WHERE n.kind = ?
  AND NOT EXISTS (SELECT 1 FROM edges e WHERE e.to_id = n.id AND e.kind IN (` + inPlaceholders(len(allowed)) + `) AND e.view_gen = n.view_gen)
  AND n.view_gen = ?
ORDER BY n.id`
			args = make([]any, 0, 2+len(allowed))
			args = append(args, string(nk))
			for _, ek := range allowed {
				args = append(args, string(ek))
			}
			args = append(args, s.viewGen)
		}

		for _, n := range s.queryNodesSQL(q, args...) {
			if n != nil {
				out = append(out, n)
			}
		}
	}
	return out
}

// --- IfaceImplementsScanner ---------------------------------------------

// IfaceImplementsRows returns one row per EdgeImplements edge whose
// target is a KindInterface carrying Meta["methods"]. The interface's
// decoded Meta rides on the row (callers pull the "methods" field, which
// round-trips as []string or []any). Interfaces with no Meta or no
// "methods" key are elided server-side.
func (s *Store) IfaceImplementsRows() []graph.IfaceImplementsRow {
	q := `SELECT e.from_id, n.id, n.meta
FROM edges e
JOIN nodes n ON n.id = e.to_id AND n.view_gen = e.view_gen
WHERE e.kind = ? AND n.kind = ? AND n.meta IS NOT NULL AND e.view_gen = ?`
	rows, err := s.db.Query(q, string(graph.EdgeImplements), string(graph.KindInterface), s.viewGen)
	if err != nil {
		return nil
	}
	defer rows.Close()

	var out []graph.IfaceImplementsRow
	for rows.Next() {
		var fromID, ifaceID string
		var metaBlob sql.RawBytes
		if err := rows.Scan(&fromID, &ifaceID, &metaBlob); err != nil {
			continue
		}
		meta, derr := decodeMeta(metaBlob)
		if derr != nil || meta == nil {
			continue
		}
		if _, ok := meta["methods"]; !ok {
			continue
		}
		out = append(out, graph.IfaceImplementsRow{
			TypeID:    fromID,
			IfaceID:   ifaceID,
			IfaceMeta: meta,
		})
	}
	// Individual undecodable rows are skipped above by design; a failed
	// iteration is different in kind — it truncates the projection — so it
	// fails the whole call the way a failed Query does.
	if rows.Err() != nil {
		return nil
	}
	return out
}

// --- MemberMethodsByType ------------------------------------------------

// MemberMethodsByType returns typeID → []MemberMethodInfo for every
// EdgeMemberOf edge whose source is a KindMethod. The columns come from
// the METHOD NODE (FilePath / StartLine / RepoPrefix), matching the
// in-memory reference. Per-type lists are deduplicated by MethodID; the
// scan is ordered by the edge PK so the first-seen winner is stable. An
// empty graph (no qualifying rows) returns nil.
const memberMethodsByTypeSQL = `SELECT e.to_id, n.id, n.name, n.file_path, n.start_line, n.repo_prefix
FROM edges e
JOIN nodes n ON n.id = e.from_id AND n.view_gen = e.view_gen
WHERE e.kind = ? AND n.kind = ? AND e.view_gen = ?
ORDER BY e.id`

const generationMemberMethodsByTypeSQL = `SELECT e.to_id, n.id, n.name, n.file_path, n.start_line, n.repo_prefix
FROM edges AS e INDEXED BY edges_by_generation
JOIN nodes n ON n.id = e.from_id AND n.view_gen = e.view_gen
WHERE e.view_gen > 0 AND e.view_gen = ? AND e.kind = ? AND n.kind = ?
ORDER BY e.id`

func (s *Store) MemberMethodsByType() map[string][]graph.MemberMethodInfo {
	q := memberMethodsByTypeSQL
	args := []any{string(graph.EdgeMemberOf), string(graph.KindMethod), s.viewGen}
	if s.viewGen > baseViewGeneration {
		q = generationMemberMethodsByTypeSQL
		args = []any{s.viewGen, string(graph.EdgeMemberOf), string(graph.KindMethod)}
	}
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil
	}
	defer rows.Close()

	out := make(map[string][]graph.MemberMethodInfo)
	seen := make(map[string]map[string]struct{})
	for rows.Next() {
		var typeID, methodID, name, filePath, repoPrefix string
		var startLine int
		if err := rows.Scan(&typeID, &methodID, &name, &filePath, &startLine, &repoPrefix); err != nil {
			continue
		}
		if seen[typeID] == nil {
			seen[typeID] = make(map[string]struct{})
		}
		if _, ok := seen[typeID][methodID]; ok {
			continue
		}
		seen[typeID][methodID] = struct{}{}
		out[typeID] = append(out[typeID], graph.MemberMethodInfo{
			MethodID:   methodID,
			Name:       name,
			FilePath:   filePath,
			StartLine:  startLine,
			RepoPrefix: repoPrefix,
		})
	}
	if rows.Err() != nil {
		return nil
	}
	if len(out) == 0 {
		// Match the in-memory reference: empty graph returns nil.
		return nil
	}
	return out
}

// --- StructuralParentEdges ----------------------------------------------

// StructuralParentEdges returns every Extends / Implements / Composes
// edge whose endpoints are both Type / Interface, projected as (FromID,
// ToID, FromKind, ToKind, Origin). Endpoints that aren't both type /
// interface are filtered server-side. Empty graph or no matching edges
// returns nil.
func (s *Store) StructuralParentEdges() []graph.StructuralParentEdgeRow {
	q := `SELECT e.from_id, e.to_id, nf.kind, nt.kind, e.origin
FROM edges e
JOIN nodes nf ON nf.id = e.from_id AND nf.view_gen = e.view_gen
JOIN nodes nt ON nt.id = e.to_id AND nt.view_gen = e.view_gen
WHERE e.kind IN (?,?,?)
  AND nf.kind IN (?,?) AND nt.kind IN (?,?)
  AND e.view_gen = ?
ORDER BY e.id`
	rows, err := s.db.Query(q,
		string(graph.EdgeExtends), string(graph.EdgeImplements), string(graph.EdgeComposes),
		string(graph.KindType), string(graph.KindInterface),
		string(graph.KindType), string(graph.KindInterface),
		s.viewGen,
	)
	if err != nil {
		return nil
	}
	defer rows.Close()

	var out []graph.StructuralParentEdgeRow
	for rows.Next() {
		var fromID, toID, fromKind, toKind, origin string
		if err := rows.Scan(&fromID, &toID, &fromKind, &toKind, &origin); err != nil {
			continue
		}
		out = append(out, graph.StructuralParentEdgeRow{
			FromID:   fromID,
			ToID:     toID,
			FromKind: graph.NodeKind(fromKind),
			ToKind:   graph.NodeKind(toKind),
			Origin:   origin,
		})
	}
	if rows.Err() != nil {
		return nil
	}
	return out
}

// --- ExtractCandidatesScanner -------------------------------------------

// ExtractCandidates ranks function / method nodes by extractability: line
// span (EndLine - StartLine + 1), distinct caller fan-in, and distinct
// callee fan-out, counting only edges whose kind is in the supplied set.
// Rows must clear all three thresholds. Nodes with a zero StartLine /
// EndLine are dropped; pathPrefix narrows by file-path prefix. Mirrors
// graph.(*Graph).ExtractCandidates exactly: only KindFunction +
// KindMethod nodes are considered, and the distinct-by-endpoint counting
// runs Go-side over GetInEdges / GetOutEdges.
func (s *Store) ExtractCandidates(kinds []graph.EdgeKind, minLines, minCallers, minFanOut int, pathPrefix string) []graph.ExtractCandidateRow {
	if len(kinds) == 0 {
		return nil
	}
	kindSet := make(map[graph.EdgeKind]struct{}, len(kinds))
	for _, k := range kinds {
		if k == "" {
			continue
		}
		kindSet[k] = struct{}{}
	}
	if len(kindSet) == 0 {
		return nil
	}

	// Candidate nodes: function / method only, non-zero line span,
	// optional path-prefix gate.
	q := `SELECT ` + lookupNodeCols + ` FROM nodes
WHERE kind IN (?,?) AND start_line > 0 AND end_line > 0`
	args := []any{string(graph.KindFunction), string(graph.KindMethod)}
	if pathPrefix != "" {
		pred, pargs := pathPrefixPredicate("file_path", pathPrefix)
		q += ` AND ` + pred
		args = append(args, pargs...)
	}
	q += ` AND view_gen = ? ORDER BY id`
	args = append(args, s.viewGen)
	nodes := s.queryNodesSQL(q, args...)

	var out []graph.ExtractCandidateRow
	for _, n := range nodes {
		if n == nil {
			continue
		}
		lineCount := n.EndLine - n.StartLine + 1
		if lineCount < minLines {
			continue
		}

		callerSet := make(map[string]struct{})
		for _, e := range s.GetInEdges(n.ID) {
			if e == nil {
				continue
			}
			if _, ok := kindSet[e.Kind]; !ok {
				continue
			}
			callerSet[e.From] = struct{}{}
		}
		if len(callerSet) < minCallers {
			continue
		}

		calleeSet := make(map[string]struct{})
		for _, e := range s.GetOutEdges(n.ID) {
			if e == nil {
				continue
			}
			if _, ok := kindSet[e.Kind]; !ok {
				continue
			}
			calleeSet[e.To] = struct{}{}
		}
		if len(calleeSet) < minFanOut {
			continue
		}

		out = append(out, graph.ExtractCandidateRow{
			NodeID:      n.ID,
			Name:        n.Name,
			FilePath:    n.FilePath,
			StartLine:   n.StartLine,
			EndLine:     n.EndLine,
			LineCount:   lineCount,
			CallerCount: len(callerSet),
			FanOut:      len(calleeSet),
		})
	}
	return out
}

// --- CrossRepoCandidates ------------------------------------------------

// CrossRepoCandidates returns every edge whose kind is in baseKinds and
// whose endpoints carry two different non-empty RepoPrefix values. The
// edge is returned verbatim (callers rewrite Edge.CrossRepo); FromRepo /
// ToRepo are the endpoint prefixes. Empty baseKinds returns nil; single-
// repo graphs (or graphs whose nodes carry no RepoPrefix) yield nothing.
func (s *Store) CrossRepoCandidates(baseKinds []graph.EdgeKind) []graph.CrossRepoCandidateRow {
	return s.crossRepoCandidates(baseKinds, nil, nil, nil)
}

// CrossRepoCandidatesForRepos applies an incident repository frontier inside
// the endpoint join. Both sides are included so indexing a new target repo also
// materializes edges arriving from an unchanged source repo.
func (s *Store) CrossRepoCandidatesForRepos(baseKinds []graph.EdgeKind, repoPrefixes []string) []graph.CrossRepoCandidateRow {
	repos := dedupeNonEmpty(repoPrefixes)
	if len(repos) == 0 {
		return nil
	}
	return s.crossRepoCandidates(baseKinds, repos, nil, nil)
}

// CrossRepoCandidatesForFiles applies the watcher frontier in SQL. The edge's
// source site and both endpoint file owners are considered, matching the
// in-memory incident-frontier semantics without one adjacency query per node.
func (s *Store) CrossRepoCandidatesForFiles(baseKinds []graph.EdgeKind, filePaths []string) []graph.CrossRepoCandidateRow {
	return s.CrossRepoCandidatesForMutation(baseKinds, filePaths, filePaths)
}

// CrossRepoCandidatesForMutation applies only the query arms required by each
// mutation role. The two JSON scopes remain bounded to one host parameter each.
func (s *Store) CrossRepoCandidatesForMutation(baseKinds []graph.EdgeKind, edgeSourceFiles, incidentNodeFiles []string) []graph.CrossRepoCandidateRow {
	edgeFiles := dedupeNonEmpty(edgeSourceFiles)
	incidentFiles := dedupeNonEmpty(incidentNodeFiles)
	if len(edgeFiles) == 0 && len(incidentFiles) == 0 {
		return nil
	}
	return s.crossRepoCandidates(baseKinds, nil, edgeFiles, incidentFiles)
}

func (s *Store) crossRepoCandidates(baseKinds []graph.EdgeKind, repoPrefixes, edgeSourceFiles, incidentNodeFiles []string) []graph.CrossRepoCandidateRow {
	uniq := anaDedupeEdgeKinds(baseKinds)
	if len(uniq) == 0 {
		return nil
	}
	// The projection is the authoritative generation filter: both endpoint
	// joins pair with the edge's generation and the edge itself is bound, so
	// a candidate id the frontier CTE produced for another generation cannot
	// survive here. The CTE arms pair their own node joins for the same
	// reason, which costs no extra bind.
	const projection = `SELECT e.from_id, e.to_id, e.kind, e.file_path, e.line,
       e.confidence, e.confidence_label, e.origin, e.tier, e.cross_repo,
       nf.repo_prefix, nt.repo_prefix
FROM %s
JOIN nodes nf ON nf.id = e.from_id AND nf.view_gen = e.view_gen
JOIN nodes nt ON nt.id = e.to_id AND nt.view_gen = e.view_gen
	WHERE nf.repo_prefix <> '' AND nt.repo_prefix <> ''
  AND nf.repo_prefix <> nt.repo_prefix
  AND e.view_gen = ?`

	appendKinds := func(args []any) []any {
		for _, kind := range uniq {
			args = append(args, string(kind))
		}
		return args
	}
	var q string
	var args []any
	if len(repoPrefixes) > 0 {
		scopeJSON, ok := projectionJSON(repoPrefixes)
		if !ok {
			return nil
		}
		q = `WITH candidate_edges(id) AS (
  SELECT e.id
  FROM nodes n
  JOIN edges e ON e.from_id = n.id AND e.view_gen = n.view_gen
  WHERE n.repo_prefix IN (SELECT CAST(value AS TEXT) FROM json_each(?))
    AND e.kind IN (` + inPlaceholders(len(uniq)) + `)
  UNION
  SELECT e.id
  FROM nodes n
  JOIN edges e ON e.to_id = n.id AND e.view_gen = n.view_gen
  WHERE n.repo_prefix IN (SELECT CAST(value AS TEXT) FROM json_each(?))
    AND e.kind IN (` + inPlaceholders(len(uniq)) + `)
)
` + fmt.Sprintf(projection, `candidate_edges ce JOIN edges e ON e.id = ce.id`)
		args = append(args, scopeJSON)
		args = appendKinds(args)
		args = append(args, scopeJSON)
		args = appendKinds(args)
		args = append(args, s.viewGen)
	} else if len(edgeSourceFiles) > 0 || len(incidentNodeFiles) > 0 {
		candidateQueries := make([]string, 0, 3)
		if len(edgeSourceFiles) > 0 {
			scopeJSON, ok := projectionJSON(edgeSourceFiles)
			if !ok {
				return nil
			}
			candidateQueries = append(candidateQueries, `SELECT e.id
  FROM edges e
  WHERE e.file_path IN (SELECT CAST(value AS TEXT) FROM json_each(?))
    AND e.kind IN (`+inPlaceholders(len(uniq))+`)`)
			args = append(args, scopeJSON)
			args = appendKinds(args)
		}
		if len(incidentNodeFiles) > 0 {
			scopeJSON, ok := projectionJSON(incidentNodeFiles)
			if !ok {
				return nil
			}
			candidateQueries = append(candidateQueries, `SELECT e.id
  FROM nodes n
  JOIN edges e ON e.from_id = n.id AND e.view_gen = n.view_gen
  WHERE n.file_path IN (SELECT CAST(value AS TEXT) FROM json_each(?))
    AND e.kind IN (`+inPlaceholders(len(uniq))+`)`)
			args = append(args, scopeJSON)
			args = appendKinds(args)
			candidateQueries = append(candidateQueries, `SELECT e.id
  FROM nodes n
  JOIN edges e ON e.to_id = n.id AND e.view_gen = n.view_gen
  WHERE n.file_path IN (SELECT CAST(value AS TEXT) FROM json_each(?))
    AND e.kind IN (`+inPlaceholders(len(uniq))+`)`)
			args = append(args, scopeJSON)
			args = appendKinds(args)
		}
		q = `WITH candidate_edges(id) AS (
` + strings.Join(candidateQueries, "\n  UNION\n  ") + `
)
` + fmt.Sprintf(projection, `candidate_edges ce JOIN edges e ON e.id = ce.id`)
		args = append(args, s.viewGen)
	} else {
		q = fmt.Sprintf(projection, `edges e`) + ` AND e.kind IN (` + inPlaceholders(len(uniq)) + `)`
		// The projection's generation bind precedes the kind list in the text.
		args = append(args, s.viewGen)
		args = appendKinds(args)
	}
	rows, err := s.db.Query(q, args...)
	if err != nil {
		panicOnFatal(err)
		return nil
	}
	defer rows.Close()

	var out []graph.CrossRepoCandidateRow
	for rows.Next() {
		var (
			fromRepo, toRepo string
			e                graph.Edge
			crossRepo        int64
		)
		if err := rows.Scan(
			&e.From, &e.To, &e.Kind, &e.FilePath, &e.Line,
			&e.Confidence, &e.ConfidenceLabel, &e.Origin, &e.Tier,
			&crossRepo,
			&fromRepo, &toRepo,
		); err != nil {
			panicOnFatal(err)
			return nil
		}
		e.CrossRepo = crossRepo != 0
		edge := e
		out = append(out, graph.CrossRepoCandidateRow{
			Edge:     &edge,
			FromRepo: fromRepo,
			ToRepo:   toRepo,
		})
	}
	if err := rows.Err(); err != nil {
		panicOnFatal(err)
		return nil
	}
	return out
}

// --- ThrowerErrorSurfacer -----------------------------------------------

// ThrowerErrorSurface returns one row per thrower (a node with outgoing
// EdgeThrows edges), aggregating the distinct error targets and the
// distinct literal error-message strings it emits (KindString nodes with
// Meta["context"] == "error_msg", linked by EdgeEmits). pathPrefix gates
// the EdgeThrows rows by their stored FilePath prefix. Throws counts the
// underlying EdgeThrows edges; FilePath / Line seed from the first throws
// edge, falling back to the thrower node's own coordinates when the edge
// carries none — matching the in-memory reference.
func (s *Store) ThrowerErrorSurface(pathPrefix string) []graph.ThrowerErrorRow {
	type rowAccum struct {
		row        graph.ThrowerErrorRow
		targetSeen map[string]struct{}
		msgSeen    map[string]struct{}
	}
	accums := make(map[string]*rowAccum)
	var order []string

	// Pass 1: EdgeThrows aggregation (count + distinct targets), keyed by
	// thrower. The first edge (by PK insertion order) seeds FilePath /
	// Line; an empty edge file/line falls back to the thrower node.
	tq := `SELECT from_id, to_id, file_path, line FROM edges WHERE kind = ? AND view_gen = ?`
	targs := []any{string(graph.EdgeThrows), s.viewGen}
	if pathPrefix != "" {
		pred, pargs := pathPrefixPredicate("file_path", pathPrefix)
		tq += ` AND ` + pred
		targs = append(targs, pargs...)
	}
	tq += ` ORDER BY id`
	trows, err := s.db.Query(tq, targs...)
	if err != nil {
		return nil
	}
	for trows.Next() {
		var from, to, filePath string
		var line int
		if err := trows.Scan(&from, &to, &filePath, &line); err != nil {
			continue
		}
		acc := accums[from]
		if acc == nil {
			file := filePath
			ln := line
			if file == "" || ln == 0 {
				if n := s.GetNode(from); n != nil {
					if file == "" {
						file = n.FilePath
					}
					if ln == 0 {
						ln = n.StartLine
					}
				}
			}
			acc = &rowAccum{
				row: graph.ThrowerErrorRow{
					ThrowerID: from,
					FilePath:  file,
					Line:      ln,
				},
				targetSeen: make(map[string]struct{}),
				msgSeen:    make(map[string]struct{}),
			}
			accums[from] = acc
			order = append(order, from)
		}
		acc.row.Throws++
		if _, ok := acc.targetSeen[to]; !ok {
			acc.targetSeen[to] = struct{}{}
			acc.row.ErrorTargets = append(acc.row.ErrorTargets, to)
		}
	}
	// Pass 1 seeds every accumulator; a truncated read here silently drops
	// throwers from the result rather than reporting a failure.
	if trows.Err() != nil {
		_ = trows.Close()
		return nil
	}
	_ = trows.Close()
	if len(accums) == 0 {
		return nil
	}

	// Pass 2: attach the literal error messages each thrower emits. Join
	// each thrower's EdgeEmits out-edges to KindString targets and filter
	// Meta["context"] == "error_msg" Go-side (the context lives in the
	// JSON Meta blob).
	for _, id := range order {
		acc := accums[id]
		mq := `SELECT n.name, n.meta
FROM edges e
JOIN nodes n ON n.id = e.to_id AND n.view_gen = e.view_gen
WHERE e.from_id = ? AND e.kind = ? AND n.kind = ? AND n.meta IS NOT NULL AND e.view_gen = ?
ORDER BY e.id`
		mrows, err := s.db.Query(mq, id, string(graph.EdgeEmits), string(graph.KindString), s.viewGen)
		if err != nil {
			continue
		}
		for mrows.Next() {
			var name string
			var metaBlob sql.RawBytes
			if err := mrows.Scan(&name, &metaBlob); err != nil {
				continue
			}
			meta, derr := decodeMeta(metaBlob)
			if derr != nil || meta == nil {
				continue
			}
			ctxLabel, _ := meta["context"].(string)
			if ctxLabel != "error_msg" {
				continue
			}
			if _, ok := acc.msgSeen[name]; ok {
				continue
			}
			acc.msgSeen[name] = struct{}{}
			acc.row.ErrorMsgs = append(acc.row.ErrorMsgs, name)
		}
		// Pass 2 only decorates rows pass 1 already produced, so a failed
		// read costs this thrower its message list and nothing else —
		// the same outcome as the Query error handled above.
		_ = mrows.Err()
		_ = mrows.Close()
	}

	out := make([]graph.ThrowerErrorRow, 0, len(order))
	for _, id := range order {
		out = append(out, accums[id].row)
	}
	return out
}
