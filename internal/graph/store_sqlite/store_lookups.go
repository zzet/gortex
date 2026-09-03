package store_sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"

	"github.com/zzet/gortex/internal/graph"
)

// These methods were added to graph.Store after the sqlite backend was
// first removed; they are restored here so *Store satisfies the current
// interface. All reuse the chunked IN-list / raw-SQL helpers in store.go
// (queryNodesSQL / queryEdgesSQL / lookupChunkSize / minInt). SQLite's
// planner drives every one through the existing secondary indexes.

// lookupNodeCols is the canonical node column list (and scan order) for
// every node-shaped SELECT in the package. It must stay in sync with
// scanNode. The struct columns (start_column/end_column) sit with the line
// range; the promoted meta columns (signature/visibility/doc/external/
// return_type/is_async/is_static/is_abstract/is_exported/updated_at/
// data_class/semantic_type/semantic_source) precede meta.
const lookupNodeColsLightPrefix = `id, kind, name, qual_name, file_path, start_line, end_line, start_column, end_column, language, repo_prefix, workspace_id, project_id, signature, visibility, doc, external, return_type, is_async, is_static, is_abstract, is_exported, updated_at, data_class, semantic_type, semantic_source, clone_sig, entry_point, entry_point_kind`

// The retrieval-payload columns sit AFTER meta deliberately: they are the
// search-index feedstock (formerly ~2/3 of every Meta blob's bytes) and the
// doc-section text, promoted out of the blob so the flat codec never encodes
// or decodes them again — while the light projection below keeps excluding
// them, exactly as it excluded them inside the blob.
const lookupNodeCols = lookupNodeColsLightPrefix + `, meta, search_signature, search_qual_name, search_doc, search_metadata_suppressed, section_text`

// lookupNodeColsLight is the light projection GetRepoNodesLight uses so a
// repo-scoped scan never transfers or decodes a blob — nor the promoted
// retrieval payload. Shared prefix, not hand-duplicated, so it cannot drift
// out of sync with lookupNodeCols / scanNode.
const lookupNodeColsLight = lookupNodeColsLightPrefix

// lookupNodeSummaryCols is the identity/location prefix consumed by
// graph.NodeLightScanner. Unlike lookupNodeColsLight it deliberately excludes
// promoted metadata columns too, so whole-graph algorithms do not allocate a
// Meta map (or retain docs/signatures) for every node.
var lookupNodeSummaryCols = strings.SplitN(lookupNodeCols, ", signature", 2)[0]

const lookupEdgeCols = `from_id, to_id, kind, file_path, line, confidence, confidence_label, origin, tier, cross_repo, meta, resolve_terminal, resolve_terminal_reason, semantic_source`
const lookupQualifiedEdgeCols = `e.from_id, e.to_id, e.kind, e.file_path, e.line, e.confidence, e.confidence_label, e.origin, e.tier, e.cross_repo, e.meta, e.resolve_terminal, e.resolve_terminal_reason, e.semantic_source`

// Compile-time assertion: *Store satisfies graph.NodeNameClassCounter.
var _ graph.NodeNameClassCounter = (*Store)(nil)
var _ graph.ExistingNodeIDFinder = (*Store)(nil)

// ExistingNodeIDs projects only primary keys for the requested nodes. It is
// intentionally separate from GetNodesByIDs: warm attribution passes need to
// suppress repeat writes, not decode full node payloads and Meta blobs.
func (s *Store) ExistingNodeIDs(ids []string) map[string]struct{} {
	uniq := dedupeNonEmpty(ids)
	out := make(map[string]struct{}, len(uniq))
	for i := 0; i < len(uniq); i += lookupChunkSize {
		end := minInt(i+lookupChunkSize, len(uniq))
		chunk := uniq[i:end]
		q := `SELECT id FROM nodes WHERE id IN (` + inPlaceholders(len(chunk)) + `) AND view_gen = ?`
		rows, err := s.db.Query(q, append(toAnyArgs(chunk), s.viewGen)...)
		if err != nil {
			panicOnFatal(err)
			return out
		}
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				_ = rows.Close()
				panicOnFatal(err)
				return out
			}
			out[id] = struct{}{}
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			panicOnFatal(err)
			return out
		}
		_ = rows.Close()
	}
	return out
}

// CountNodesByNameClass implements graph.NodeNameClassCounter: for each
// distinct name, it tallies how many nodes.name matches are Real (is_stub =
// 0 and kind IN definitionKinds) vs Stub (is_stub = 1), server-side via
// nodes_by_name — one aggregate query per chunk instead of one
// FindNodesByName round trip per name. A name absent from the returned map
// has no matching node at all (Real == Stub == 0 either way).
func (s *Store) CountNodesByNameClass(names []string, definitionKinds []graph.NodeKind) map[string]graph.NodeNameClassCount {
	_, kindArgs := aggDedupeNodeKinds(definitionKinds)
	if len(kindArgs) == 0 {
		return nil
	}
	uniq := dedupeNonEmpty(names)
	if len(uniq) == 0 {
		return nil
	}
	out := make(map[string]graph.NodeNameClassCount, len(uniq))
	kindPlaceholders := inPlaceholders(len(kindArgs))
	for i := 0; i < len(uniq); i += lookupChunkSize {
		end := minInt(i+lookupChunkSize, len(uniq))
		chunk := uniq[i:end]
		q := `SELECT name,
		             SUM(CASE WHEN is_stub = 0 AND kind IN (` + kindPlaceholders + `) THEN 1 ELSE 0 END),
		             SUM(CASE WHEN is_stub = 1 THEN 1 ELSE 0 END)
		        FROM nodes
		       WHERE name IN (` + inPlaceholders(len(chunk)) + `) AND view_gen = ?
		       GROUP BY name`
		args := make([]any, 0, len(kindArgs)+len(chunk)+1)
		args = append(args, kindArgs...)
		args = append(args, toAnyArgs(chunk)...)
		args = append(args, s.viewGen)
		rows, err := s.db.Query(q, args...)
		if err != nil {
			panicOnFatal(err)
			return out
		}
		for rows.Next() {
			var name string
			var c graph.NodeNameClassCount
			if err := rows.Scan(&name, &c.Real, &c.Stub); err != nil {
				_ = rows.Close()
				panicOnFatal(err)
				return out
			}
			out[name] = c
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			panicOnFatal(err)
			return out
		}
		_ = rows.Close()
	}
	return out
}

// FindNodesByNameContaining returns nodes whose Name contains substr,
// case-insensitively (SQLite's LIKE is ASCII case-insensitive). An empty
// substring matches nothing (parity with the in-memory store); a limit > 0
// caps the result set. The leading-wildcard LIKE is a deliberate full scan —
// no index accelerates an unanchored substring — matching the in-memory
// strings.Contains fallback. % and _ in substr are escaped so they match
// literally.
func (s *Store) FindNodesByNameContaining(substr string, limit int) []*graph.Node {
	if substr == "" {
		return nil
	}
	pattern := "%" + escapeLikePattern(substr) + "%"
	q := `SELECT ` + lookupNodeCols + ` FROM nodes WHERE name LIKE ? ESCAPE '\' AND view_gen = ? ORDER BY id`
	if limit > 0 {
		return s.queryNodesSQL(q+` LIMIT ?`, pattern, s.viewGen, limit)
	}
	return s.queryNodesSQL(q, pattern, s.viewGen)
}

// GetNodesByQualNames returns every candidate for each requested qualified
// name. The query orders by qual_name then ID, so each candidate slice is
// deterministic and repository/workspace-aware callers can disambiguate it.
func (s *Store) GetNodesByQualNames(qualNames []string) map[string][]*graph.Node {
	uniq := dedupeNonEmpty(qualNames)
	if len(uniq) == 0 {
		return nil
	}

	out := make(map[string][]*graph.Node, len(uniq))
	for _, n := range s.queryNodesSQL(nodesByQualNameLookupSQL, qualNameLookupPayload(uniq), s.viewGen) {
		if n != nil {
			out[n.QualName] = append(out[n.QualName], n)
		}
	}
	return out
}

// GetOutEdgesByNodeIDs batches per-node out-edge fan-out into one query per
// chunk. Missing IDs are simply absent from the returned map.
func (s *Store) GetOutEdgesByNodeIDs(ids []string) map[string][]*graph.Edge {
	return s.edgesByNodeIDs(ids, "from_id", func(e *graph.Edge) string { return e.From })
}

// GetInEdgesByNodeIDs is the incoming-edge twin of GetOutEdgesByNodeIDs.
// GetNodeContext returns one node while allowing callers to cancel a blocked
// SQLite read. It intentionally remains an optional extension of graph.Store
// so in-memory and third-party stores keep their existing contract.
func (s *Store) GetNodeContext(ctx context.Context, id string) (*graph.Node, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	row := s.stmtGetNode.QueryRowContext(ctx, id, s.viewGen)
	n, err := scanNode(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get node %q: %w", id, err)
	}
	return n, nil
}

// GetNodesByIDsContext batch-loads nodes with cancellable SQLite queries.
// Partial results are returned with an error so safety-sensitive callers can
// retain evidence while marking their result incomplete.
func (s *Store) GetNodesByIDsContext(ctx context.Context, ids []string) (map[string]*graph.Node, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	uniq := dedupeNonEmpty(ids)
	out := make(map[string]*graph.Node, len(uniq))
	for i := 0; i < len(uniq); i += lookupChunkSize {
		if err := ctx.Err(); err != nil {
			return out, err
		}
		end := minInt(i+lookupChunkSize, len(uniq))
		chunk := uniq[i:end]
		q := `SELECT ` + lookupNodeCols + ` FROM nodes WHERE id IN (` + inPlaceholders(len(chunk)) + `) AND view_gen = ?`
		rows, err := s.db.QueryContext(ctx, q, append(toAnyArgs(chunk), s.viewGen)...)
		if err != nil {
			return out, fmt.Errorf("get nodes by ids: %w", err)
		}
		for rows.Next() {
			n, scanErr := scanNodeCursor(rows)
			if scanErr != nil {
				_ = rows.Close()
				return out, fmt.Errorf("scan node by id: %w", scanErr)
			}
			if n != nil {
				out[n.ID] = n
			}
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return out, fmt.Errorf("get nodes by ids: %w", err)
		}
		if err := rows.Close(); err != nil {
			return out, err
		}
	}
	return out, nil
}

func (s *Store) GetInEdgesByNodeIDs(ids []string) map[string][]*graph.Edge {
	return s.edgesByNodeIDs(ids, "to_id", func(e *graph.Edge) string { return e.To })
}

// GetInEdgesByNodeIDsContext is the bounded, cancellable incoming-edge read
// used by reachability analysis. The ordinary Store interface intentionally
// stays unchanged; reach detects this optional capability and falls back to
// the in-memory batch read on backends that do not implement it.
//
// limit is a total row budget across every IN-list chunk. One extra row is
// requested only to prove truncation, and QueryContext lets an expired impact
// request interrupt SQLite instead of monopolising its single connection.
// GetOutEdgesByNodeIDsContext returns at most limit outgoing edges and reports
// whether additional rows exist. Every SQLite query observes ctx cancellation.
func (s *Store) GetOutEdgesByNodeIDsContext(ctx context.Context, ids []string, limit int) (map[string][]*graph.Edge, bool, error) {
	uniq := dedupeNonEmpty(ids)
	if len(uniq) == 0 {
		return nil, false, nil
	}
	if limit <= 0 {
		return nil, true, nil
	}

	out := make(map[string][]*graph.Edge, len(uniq))
	total := 0
	for i := 0; i < len(uniq); i += lookupChunkSize {
		if err := ctx.Err(); err != nil {
			return out, true, err
		}
		end := minInt(i+lookupChunkSize, len(uniq))
		chunk := uniq[i:end]
		remaining := limit - total
		queryLimit := remaining + 1
		q := `SELECT ` + edgeColsLight + ` FROM edges WHERE from_id IN (` + inPlaceholders(len(chunk)) + `) AND view_gen = ? LIMIT ?`
		args := toAnyArgs(chunk)
		args = append(args, s.viewGen, queryLimit)
		rows, err := s.db.QueryContext(ctx, q, args...)
		if err != nil {
			return out, true, err
		}
		for rows.Next() {
			e, scanErr := s.scanEdgeLight(rows)
			if scanErr != nil {
				_ = rows.Close()
				return out, true, scanErr
			}
			if total >= limit {
				_ = rows.Close()
				return out, true, nil
			}
			if e != nil {
				out[e.From] = append(out[e.From], e)
				total++
			}
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return out, true, fmt.Errorf("bounded outgoing-edge query: %w", err)
		}
		if err := rows.Close(); err != nil {
			return out, true, err
		}
	}
	return out, false, nil
}

func (s *Store) GetInEdgesByNodeIDsContext(ctx context.Context, ids []string, limit int) (map[string][]*graph.Edge, bool, error) {
	uniq := dedupeNonEmpty(ids)
	if len(uniq) == 0 {
		return nil, false, nil
	}
	if limit <= 0 {
		return nil, true, nil
	}

	out := make(map[string][]*graph.Edge, len(uniq))
	total := 0
	for i := 0; i < len(uniq); i += lookupChunkSize {
		if err := ctx.Err(); err != nil {
			return out, true, err
		}
		end := minInt(i+lookupChunkSize, len(uniq))
		chunk := uniq[i:end]
		remaining := limit - total
		// remaining may be zero: fetch one proof row from later chunks so
		// exactly-limit and greater-than-limit are distinguishable.
		queryLimit := remaining + 1
		q := `SELECT ` + edgeColsLight + ` FROM edges WHERE to_id IN (` + inPlaceholders(len(chunk)) + `) AND view_gen = ? LIMIT ?`
		args := toAnyArgs(chunk)
		args = append(args, s.viewGen, queryLimit)
		rows, err := s.db.QueryContext(ctx, q, args...)
		if err != nil {
			return out, true, err
		}
		for rows.Next() {
			e, scanErr := s.scanEdgeLight(rows)
			if scanErr != nil {
				_ = rows.Close()
				return out, true, scanErr
			}
			if total >= limit {
				_ = rows.Close()
				return out, true, nil
			}
			if e != nil {
				out[e.To] = append(out[e.To], e)
				total++
			}
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return out, true, fmt.Errorf("bounded incoming-edge query: %w", err)
		}
		if err := rows.Close(); err != nil {
			return out, true, err
		}
	}
	return out, false, nil
}

// GetEdgeCandidates executes predicate-shaped, chunked joins against the
// existing endpoint and source indexes. Only matching edge rows cross the
// SQLite boundary and decode Meta; callers never materialize full adjacency
// for every go/types use.
func (s *Store) GetEdgeCandidates(endpoints []graph.EdgeEndpoint, sites []graph.EdgeSite) graph.EdgeCandidateSet {
	out := graph.NewEdgeCandidateSet()
	canonical := make(map[edgeAttributeKey]*graph.Edge, len(endpoints)+len(sites))
	endpointAdded := make(map[edgeAttributeKey]struct{}, len(endpoints))
	siteAdded := make(map[edgeAttributeKey]struct{}, len(sites))

	canonicalize := func(edge *graph.Edge) (edgeAttributeKey, *graph.Edge) {
		key := edgeCandidateIdentity(edge)
		if existing := canonical[key]; existing != nil {
			return key, existing
		}
		canonical[key] = edge
		return key, edge
	}
	addEndpoint := func(edge *graph.Edge) {
		key, edge := canonicalize(edge)
		if _, exists := endpointAdded[key]; exists {
			return
		}
		endpointAdded[key] = struct{}{}
		out.AddEndpoint(edge)
	}
	addSite := func(edge *graph.Edge) {
		key, edge := canonicalize(edge)
		if _, exists := siteAdded[key]; exists {
			return
		}
		siteAdded[key] = struct{}{}
		out.AddSite(edge)
	}
	collect := func(shape, query string, args []any, add func(*graph.Edge)) bool {
		edges, err := s.queryEdgeCandidatesSQL(query, args...)
		if err != nil {
			panicOnFatal(fmt.Errorf("query %s edge candidates: %w", shape, err))
			return false
		}
		for _, edge := range edges {
			add(edge)
		}
		return true
	}

	endpointSeen := make(map[graph.EdgeEndpoint]struct{}, len(endpoints))
	endpointKeys := make([]graph.EdgeEndpoint, 0, len(endpoints))
	for _, key := range endpoints {
		if key.From == "" || key.To == "" {
			continue
		}
		if _, ok := endpointSeen[key]; ok {
			continue
		}
		endpointSeen[key] = struct{}{}
		endpointKeys = append(endpointKeys, key)
	}
	// Probe the edges index in key order: enrichment passes hand in tens of
	// thousands of keys in discovery order, and random-order point probes
	// page-fault their way across the mmap'd B-tree (measured: 74% of this
	// path's samples under the b-tree seek syscalls). Monotonic probes touch
	// each index page once. Per-key result buckets are order-insensitive to
	// the inter-key ordering, so consumers see identical candidate sets.
	sort.Slice(endpointKeys, func(i, j int) bool {
		if endpointKeys[i].From != endpointKeys[j].From {
			return endpointKeys[i].From < endpointKeys[j].From
		}
		return endpointKeys[i].To < endpointKeys[j].To
	})
	for i := 0; i < len(endpointKeys); i += lookupChunkSize {
		end := minInt(i+lookupChunkSize, len(endpointKeys))
		chunk := endpointKeys[i:end]
		args := make([]any, 0, len(chunk)*2+1)
		for _, key := range chunk {
			args = append(args, key.From, key.To)
		}
		args = append(args, s.viewGen)
		if !collect("endpoint", edgeCandidatesEndpointQuery(len(chunk)), args, addEndpoint) {
			return out
		}
	}

	siteSeen := make(map[graph.EdgeSite]struct{}, len(sites))
	exactSites := make([]graph.EdgeSite, 0, len(sites))
	anySites := make([]graph.EdgeSite, 0)
	for _, key := range sites {
		if key.From == "" {
			continue
		}
		if _, ok := siteSeen[key]; ok {
			continue
		}
		siteSeen[key] = struct{}{}
		if key.Kind == "" {
			anySites = append(anySites, key)
		} else {
			exactSites = append(exactSites, key)
		}
	}
	// Same monotonic-probe ordering as the endpoint keys above.
	sortSites := func(keys []graph.EdgeSite) {
		sort.Slice(keys, func(i, j int) bool {
			if keys[i].From != keys[j].From {
				return keys[i].From < keys[j].From
			}
			if keys[i].Line != keys[j].Line {
				return keys[i].Line < keys[j].Line
			}
			return keys[i].Kind < keys[j].Kind
		})
	}
	sortSites(exactSites)
	sortSites(anySites)
	for i := 0; i < len(exactSites); i += lookupChunkSize {
		end := minInt(i+lookupChunkSize, len(exactSites))
		chunk := exactSites[i:end]
		args := make([]any, 0, len(chunk)*3+1)
		for _, key := range chunk {
			args = append(args, key.From, key.Line, string(key.Kind))
		}
		args = append(args, s.viewGen)
		if !collect("exact-site", edgeCandidatesExactSiteQuery(len(chunk)), args, addSite) {
			return out
		}
	}
	for i := 0; i < len(anySites); i += lookupChunkSize {
		end := minInt(i+lookupChunkSize, len(anySites))
		chunk := anySites[i:end]
		args := make([]any, 0, len(chunk)*2+1)
		for _, key := range chunk {
			args = append(args, key.From, key.Line)
		}
		args = append(args, s.viewGen)
		if !collect("any-kind site", edgeCandidatesAnySiteQuery(len(chunk)), args, addSite) {
			return out
		}
	}
	return out
}

// The three candidate-query builders are pure string assembly (no I/O) so
// the plan-lock test can EXPLAIN the exact SQL GetEdgeCandidates executes
// (store_bfs.go precedent). Every shape drives the edges side of the VALUES
// join through edges_by_from(from_id, kind); the wanted CTE is the scanned
// side by construction.

func edgeCandidatesValues(rows int, row string) string {
	return strings.TrimSuffix(strings.Repeat(row+",", rows), ",")
}

// The wanted CTE is a constant VALUES relation with no generation of its own,
// so the generation predicate rides one trailing bind on the edges side. It
// stays out of the JOIN's ON clause so the probe columns the plan locks name
// are still the ones the planner sees first.

func edgeCandidatesEndpointQuery(pairs int) string {
	return `WITH wanted(from_id, to_id) AS (VALUES ` + edgeCandidatesValues(pairs, "(?, ?)") + `)
	      SELECT ` + lookupQualifiedEdgeCols + `
	        FROM wanted AS w
	        JOIN edges AS e ON e.from_id = w.from_id AND e.to_id = w.to_id
	       WHERE e.view_gen = ?`
}

func edgeCandidatesExactSiteQuery(triples int) string {
	return `WITH wanted(from_id, line, kind) AS (VALUES ` + edgeCandidatesValues(triples, "(?, ?, ?)") + `)
	      SELECT DISTINCT ` + lookupQualifiedEdgeCols + `
	        FROM wanted AS w
	        JOIN edges AS e ON e.from_id = w.from_id AND e.kind = w.kind AND e.line = w.line
	       WHERE e.view_gen = ?`
}

func edgeCandidatesAnySiteQuery(pairs int) string {
	return `WITH wanted(from_id, line) AS (VALUES ` + edgeCandidatesValues(pairs, "(?, ?)") + `)
	      SELECT DISTINCT ` + lookupQualifiedEdgeCols + `
	        FROM wanted AS w
	        JOIN edges AS e ON e.from_id = w.from_id AND e.line = w.line
	       WHERE e.view_gen = ?`
}

// edgeCandidateIdentity mirrors the edges table's logical UNIQUE key. A row
// may match both endpoint and site query shapes; retaining one pointer keeps
// an in-pass reindex visible through every candidate bucket and prevents the
// same persisted edge from being claimed twice.
func edgeCandidateIdentity(edge *graph.Edge) edgeAttributeKey {
	return edgeAttributeKey{
		from:     edge.From,
		to:       edge.To,
		kind:     string(edge.Kind),
		filePath: edge.FilePath,
		line:     edge.Line,
	}
}

// queryEdgeCandidatesSQL is the strict edge-shaped reader used by semantic
// candidate lookups. Unlike the legacy predicate iterator helper, it returns
// query, decode, iteration, and close failures to the caller; GetEdgeCandidates
// then applies the Store contract through panicOnFatal instead of silently
// turning a corrupt or failed read into an apparently empty candidate set.
func (s *Store) queryEdgeCandidatesSQL(query string, args ...any) ([]*graph.Edge, error) {
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	var out []*graph.Edge
	for rows.Next() {
		edge, scanErr := s.scanEdgeCursor(rows)
		if scanErr != nil {
			_ = rows.Close()
			return nil, scanErr
		}
		if edge != nil {
			out = append(out, edge)
		}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	return out, nil
}

// edgesByNodeIDs runs the chunked IN-list edge fetch keyed on the given
// column (from_id or to_id), grouping results by the supplied key extractor.
func (s *Store) edgesByNodeIDs(ids []string, col string, key func(*graph.Edge) string) map[string][]*graph.Edge {
	uniq := dedupeNonEmpty(ids)
	if len(uniq) == 0 {
		return nil
	}
	out := make(map[string][]*graph.Edge, len(uniq))
	for i := 0; i < len(uniq); i += lookupChunkSize {
		end := minInt(i+lookupChunkSize, len(uniq))
		chunk := uniq[i:end]
		q := `SELECT ` + lookupEdgeCols + ` FROM edges WHERE ` + col + ` IN (` + inPlaceholders(len(chunk)) + `) AND view_gen = ?`
		for _, e := range s.queryEdgesSQL(q, append(toAnyArgs(chunk), s.viewGen)...) {
			if e == nil {
				continue
			}
			k := key(e)
			out[k] = append(out[k], e)
		}
	}
	return out
}

// dedupeNonEmpty drops empties and duplicates, preserving first-seen order.
func dedupeNonEmpty(in []string) []string {
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, v := range in {
		if v == "" {
			continue
		}
		if _, ok := seen[v]; ok {
			continue
		}
		seen[v] = struct{}{}
		out = append(out, v)
	}
	return out
}

// inPlaceholders returns "?,?,?" for n bound parameters.
func inPlaceholders(n int) string {
	if n <= 0 {
		return ""
	}
	return strings.Repeat(",?", n)[1:]
}

// toAnyArgs widens a string slice for variadic Query/Exec args.
func toAnyArgs(ss []string) []any {
	args := make([]any, len(ss))
	for i, v := range ss {
		args[i] = v
	}
	return args
}

// escapeLikePattern escapes the LIKE metacharacters so the substring matches
// literally under `... LIKE ? ESCAPE '\'`.
func escapeLikePattern(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}
