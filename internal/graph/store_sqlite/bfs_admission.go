package store_sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"sort"
	"strconv"
	"strings"

	"github.com/zzet/gortex/internal/graph"
)

var _ graph.FilteredBFSAdmissionReader = (*Store)(nil)

func bfsAdmissionColumns(dir graph.Direction) (anchor, next, index string) {
	if dir == graph.DirectionBackward {
		return "to_id", "from_id", "edges_by_to"
	}
	return "from_id", "to_id", "edges_by_from"
}

// A missing far-end node must remain a missing identity, while its edge still
// carries unresolved/dangling evidence. Only the 13 non-null identity fields
// need defaults for a LEFT JOIN; promoted nullable payloads keep their values.
func bfsAdmissionNeighborColumns(alias string) string {
	columns := strings.Split(lookupNodeCols, ", ")
	for i, column := range columns {
		qualified := alias + "." + column
		if i < 13 {
			fallback := "''"
			switch column {
			case "start_line", "end_line", "start_column", "end_column":
				fallback = "0"
			}
			qualified = `COALESCE(` + qualified + `, ` + fallback + `)`
		}
		columns[i] = qualified
	}
	return strings.Join(columns, ", ")
}

// bfsAdmissionQuery walks, counts and hydrates one neighborhood. Every
// far-end test is a covering-index probe: a WITHOUT ROWID index entry ends in
// the node id, so a probe never reads the wide nodes row, whose payload can
// spill to overflow pages. hydrated is the only full-row read, one seek per
// distinct walked node, apart from a workspace gate's: the seed's row and the
// row of each neighbor outside the seed's repository.
func bfsAdmissionQuery(generation int64, request graph.BFSAdmissionRequest, filter graph.OutlineEdgeFilter) outlineQuery {
	states, edges, _ := graph.BFSAdmissionBudgets(request)
	anchor, next, index := bfsAdmissionColumns(request.Direction)
	kindJSON, _ := json.Marshal(request.Kinds)
	kindPredicate := `e.kind IN (SELECT CAST(value AS TEXT) FROM json_each(?))`
	limit := strconv.Itoa(request.Limit)
	var q strings.Builder
	var args []any
	add := func(fragment string, values ...any) {
		q.WriteString(fragment)
		args = append(args, values...)
	}
	// The seed's kind is checked once in the base case; every other anchor
	// passed the same probe as a walked neighbor. Edge-local terms need no
	// node row at all.
	unmasked := func(string, ...any) {}
	if len(filter.ExcludeNodeKinds) > 0 {
		masked, _ := json.Marshal(filter.ExcludeNodeKinds)
		unmasked = func(id string, idArgs ...any) {
			add(` AND NOT EXISTS (SELECT 1 FROM nodes AS n INDEXED BY nodes_by_kind
			WHERE n.kind IN (SELECT CAST(value AS TEXT) FROM json_each(?)) AND n.view_gen = ? AND n.id = `+id+`)`,
				append([]any{string(masked), generation}, idArgs...)...)
		}
	}
	present := func(id string) {
		add(`EXISTS (SELECT 1 FROM nodes AS n INDEXED BY nodes_by_generation WHERE n.view_gen = ? AND n.id = `+id+`)`, generation)
	}
	// A repository narrow, or else a workspace scope, gates the walk with a
	// superset of the scope. Go admit checks the exact workspace and project
	// scope on every neighbor it reads, before recording its edge.
	inScope := present
	workspaceGate := false
	switch {
	case len(request.Scope.RepoAllow) > 0:
		repos := outlineReposJSON(request.Scope)
		inScope = func(id string) {
			add(`EXISTS (SELECT 1 FROM nodes AS n INDEXED BY nodes_by_repo
			WHERE n.repo_prefix IN (SELECT CAST(value AS TEXT) FROM json_each(?)) AND n.view_gen = ? AND n.id = `+id+`)`, repos, generation)
		}
	case request.Scope.WorkspaceID != "":
		// A neighbor in the seed's repository passes on a covering probe. Any
		// other is checked against the scope on its own row, so the walk
		// stops at the workspace boundary instead of following edges into
		// every repository they reach.
		workspaceGate = true
		scope, scopeArgs := outlineScopePredicate(request.Scope, "n")
		inScope = func(id string) {
			add(`(EXISTS (SELECT 1 FROM nodes AS n INDEXED BY nodes_by_repo
			WHERE n.repo_prefix = (SELECT repo FROM seed) AND n.view_gen = ? AND n.id = `+id+`)
			OR EXISTS (SELECT 1 FROM nodes AS n WHERE n.id = `+id+` AND n.view_gen = ? AND `+scope+`))`,
				append([]any{generation, generation}, scopeArgs...)...)
		}
	}
	edgeMask, edgeMaskArgs := outlineEdgePredicate(graph.OutlineEdgeFilter{ExcludeKinds: filter.ExcludeKinds, ExcludeNodeIDs: filter.ExcludeNodeIDs}, "", "")
	via, viaArgs := outlineViaPredicate(filter.ExcludeVia)
	structural := strings.NewReplacer("kind", "e.kind", "to_id", "e.to_id").Replace(structuralTargetInvalidSQL)
	visible := func() {
		add(` AND NOT `+structural+edgeMask+via, append(append([]any(nil), edgeMaskArgs...), viaArgs...)...)
	}
	// Reject neighbors before adding recursive states, exactly as Go admit.
	// Unresolved edges remain in final adjacency for boundary reconstruction.
	resolved := `e.` + next + ` NOT GLOB 'unresolved::*' AND instr(e.` + next + `, '::unresolved::') = 0 AND e.` + next + ` NOT GLOB 'external::*'`

	add(`WITH RECURSIVE `)
	if workspaceGate {
		add(`seed(repo) AS MATERIALIZED (SELECT n.repo_prefix FROM nodes AS n WHERE n.id = ? AND n.view_gen = ?),
		`, request.Seed, generation)
	}
	// A seed reached again adds no shorter path, only repeated states.
	add(`walk(id, depth) AS (
		SELECT ?, 0 WHERE 1`, request.Seed)
	unmasked(`?`, request.Seed)
	add(`
		UNION
		SELECT e.`+next+`, b.depth + 1 FROM walk AS b
		CROSS JOIN edges AS e INDEXED BY `+index+` ON e.view_gen = ? AND e.`+anchor+` = b.id AND `+kindPredicate+`
		WHERE b.depth < ? AND e.`+next+` <> ? AND `+resolved, generation, string(kindJSON), request.Depth, request.Seed)
	visible()
	add(` AND `)
	inScope(`e.` + next)
	unmasked(`e.` + next)
	// room is the cap left after every walked identity. No admission over
	// this snapshot holds more nodes, so a count below room cannot reach the
	// engine's sort threshold whichever upper bound reports it.
	add(`
		LIMIT `+strconv.Itoa(states+1)+`),
		ids AS MATERIALIZED (SELECT id, MIN(depth) AS depth FROM walk GROUP BY id),
		ok AS MATERIALIZED (SELECT (SELECT COUNT(*) FROM walk) <= `+strconv.Itoa(states)+` AS fits, `+limit+` - COUNT(*) AS room FROM ids),
		hydrated AS MATERIALIZED (SELECT `+qualifiedNodeColumns("n", lookupNodeCols)+`
		FROM ok CROSS JOIN ids AS i CROSS JOIN nodes AS n ON n.id = i.id AND n.view_gen = ?
		WHERE ok.fits AND i.id <> ?),
		raw AS MATERIALIZED (SELECT i.id, (SELECT COUNT(*) FROM (SELECT 1 FROM edges AS e INDEXED BY `+index+`
		WHERE e.view_gen = ? AND e.`+anchor+` = i.id LIMIT `+limit+`)) AS n
		FROM ids AS i WHERE i.depth < `+strconv.Itoa(request.Depth)+`),
		totals AS MATERIALIZED (SELECT r.id, CASE WHEN NOT ok.fits OR r.n < ok.room THEN r.n
		ELSE (SELECT COUNT(*) FROM (SELECT 1 FROM edges AS e INDEXED BY `+index+`
		WHERE e.view_gen = ? AND e.`+anchor+` = r.id`, generation, request.Seed, generation, generation)
	visible()
	unmasked(`e.` + next)
	// A row survives when its far end was walked, is the seed, is an unmasked
	// unresolved boundary, or is missing; a resolved node outside the gate or
	// with a masked kind is dropped. Overflow keeps rows for the sentinel.
	add(` LIMIT `+limit+`)) END AS n
		FROM ok CROSS JOIN raw AS r)
		SELECT NOT ok.fits, e.id, t.n, `+lookupQualifiedEdgeCols+`, `+bfsAdmissionNeighborColumns("h")+`
		FROM ok CROSS JOIN totals AS t CROSS JOIN edges AS e INDEXED BY `+index+` ON e.view_gen = ? AND e.`+anchor+` = t.id AND `+kindPredicate+`
		LEFT JOIN hydrated AS h ON h.id = e.`+next+`
		WHERE 1`, generation, string(kindJSON))
	visible()
	add(` AND (NOT ok.fits OR h.id IS NOT NULL OR e.`+next+` = ? OR (NOT (`+resolved+`)`, request.Seed)
	unmasked(`e.` + next)
	add(`) OR NOT `)
	present(`e.` + next)
	add(`) LIMIT ` + strconv.Itoa(edges+1))
	return outlineQuery{sql: q.String(), args: args}
}

// BFSAdmissionFilteredContext reads an indexed neighborhood in one statement,
// preserving Go's admission rather than substituting a discovery-hop-only
// graph. The repository narrow, or without one the workspace, gates recursive
// neighbors and Go admit applies the exact workspace and project scope;
// state/edge sentinels decline broad projections. A count that could reach
// Go's pre-cap sort threshold is exact and saturates at the caller's cap;
// below it, the raw adjacency size decides that sort the same way without
// reading edge rows.
func (s *Store) BFSAdmissionFilteredContext(ctx context.Context, request graph.BFSAdmissionRequest, filter graph.OutlineEdgeFilter) (graph.BFSAdmissionSnapshot, bool, error) {
	ctx = liveNameContext(ctx)
	if err := ctx.Err(); err != nil {
		return graph.BFSAdmissionSnapshot{}, false, err
	}
	if _, _, supported := graph.BFSAdmissionBudgets(request); !supported || request.Direction != graph.DirectionForward && request.Direction != graph.DirectionBackward {
		return graph.BFSAdmissionSnapshot{}, false, nil
	}
	request.Kinds = anaDedupeEdgeKinds(request.Kinds)
	if len(request.Kinds) == 0 {
		return graph.BFSAdmissionSnapshot{}, false, nil
	}
	return s.readBFSAdmission(ctx, request, bfsAdmissionQuery(s.viewGen, request, filter))
}

// readBFSAdmission decodes one admission statement's rows. Any overflow
// sentinel or budget breach declines the whole snapshot.
func (s *Store) readBFSAdmission(ctx context.Context, request graph.BFSAdmissionRequest, query outlineQuery) (graph.BFSAdmissionSnapshot, bool, error) {
	_, edgeBudget, _ := graph.BFSAdmissionBudgets(request)
	rows, err := outlineRows(ctx, s.db, query)
	if err != nil {
		return graph.BFSAdmissionSnapshot{}, false, err
	}
	defer rows.Close()
	out := graph.BFSAdmissionSnapshot{Nodes: make(map[string]*graph.Node), Adjacency: make(map[string]graph.KindAdjacency)}
	type orderedEdge struct {
		edge *graph.Edge
		id   int64
	}
	ordered := make(map[string][]orderedEdge)
	count := 0
	for rows.Next() {
		count++
		if count > edgeBudget {
			return graph.BFSAdmissionSnapshot{}, false, nil
		}
		var overflow bool
		var orderID int64
		var total int
		var neighbor *graph.Node
		edge, err := s.scanEdgeCursor(bfsAdmissionEdgeScanner{rows: rows, overflow: &overflow, orderID: &orderID, total: &total, neighbor: &neighbor})
		if err != nil {
			return graph.BFSAdmissionSnapshot{}, false, err
		}
		if overflow {
			return graph.BFSAdmissionSnapshot{}, false, nil
		}
		if edge == nil {
			continue
		}
		anchor := edge.From
		if request.Direction == graph.DirectionBackward {
			anchor = edge.To
		}
		ordered[anchor] = append(ordered[anchor], orderedEdge{edge: edge, id: orderID})
		out.Adjacency[anchor] = graph.KindAdjacency{Total: total}
		if neighbor != nil && neighbor.ID != "" {
			out.Nodes[neighbor.ID] = neighbor
		}
	}
	if err := rows.Err(); err != nil {
		return graph.BFSAdmissionSnapshot{}, false, err
	}
	for anchor, values := range ordered {
		// Match ordinary point reads before Go's optional evidence sort. No
		// global SQL ORDER BY can force an unbounded sort before a sentinel.
		sort.SliceStable(values, func(i, j int) bool {
			if request.Direction == graph.DirectionBackward {
				if values[i].edge.Kind != values[j].edge.Kind {
					return values[i].edge.Kind < values[j].edge.Kind
				}
			} else if values[i].edge.Line != values[j].edge.Line {
				return values[i].edge.Line < values[j].edge.Line
			}
			return values[i].id < values[j].id
		})
		adjacency := out.Adjacency[anchor]
		for _, value := range values {
			adjacency.Edges = append(adjacency.Edges, value.edge)
		}
		out.Adjacency[anchor] = adjacency
	}
	return out, true, ctx.Err()
}

// Nest the existing node/edge scanners around one Rows.Scan so both payloads
// use their established decoders and borrowed metadata bytes remain cursor-local.
type bfsAdmissionEdgeScanner struct {
	rows     *sql.Rows
	overflow *bool
	orderID  *int64
	total    *int
	neighbor **graph.Node
}

func (s bfsAdmissionEdgeScanner) Scan(edgeDest ...any) error {
	node, err := scanNodeCursor(bfsAdmissionNodeScanner{edge: s, edgeDest: edgeDest})
	*s.neighbor = node
	return err
}

type bfsAdmissionNodeScanner struct {
	edge     bfsAdmissionEdgeScanner
	edgeDest []any
}

func (s bfsAdmissionNodeScanner) Scan(nodeDest ...any) error {
	dest := []any{s.edge.overflow, s.edge.orderID, s.edge.total}
	dest = append(dest, s.edgeDest...)
	dest = append(dest, nodeDest...)
	return s.edge.rows.Scan(dest...)
}
