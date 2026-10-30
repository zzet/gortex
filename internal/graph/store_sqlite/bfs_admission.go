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

func bfsAdmissionColumns(dir graph.Direction) (anchor, next, index, neighbor string) {
	if dir == graph.DirectionBackward {
		return "to_id", "from_id", "edges_by_to", "source"
	}
	return "from_id", "to_id", "edges_by_from", "target"
}

func bfsAdmissionJoins(dir graph.Direction, needNeighbor, inner bool, filter graph.OutlineEdgeFilter) string {
	_, _, _, neighbor := bfsAdmissionColumns(dir)
	var out string
	for _, end := range []struct{ alias, column string }{{"source", "from_id"}, {"target", "to_id"}} {
		if end.alias != neighbor || !needNeighbor {
			if len(filter.ExcludeNodeKinds) == 0 {
				continue
			}
		}
		join := " LEFT JOIN "
		if end.alias == neighbor && needNeighbor && inner {
			join = " JOIN "
		}
		out += join + `nodes ` + end.alias + ` ON ` + end.alias + `.id = e.` + end.column + ` AND ` + end.alias + `.view_gen = e.view_gen`
	}
	return out
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

func bfsAdmissionQuery(generation int64, request graph.BFSAdmissionRequest, filter graph.OutlineEdgeFilter) outlineQuery {
	states, edges, _ := graph.BFSAdmissionBudgets(request)
	anchor, next, index, neighbor := bfsAdmissionColumns(request.Direction)
	kindJSON, _ := json.Marshal(request.Kinds)
	kindPredicate := `e.kind IN (SELECT CAST(value AS TEXT) FROM json_each(?))`
	scope, scopeArgs := outlineScopePredicate(request.Scope, neighbor)
	mask, maskArgs := outlineEdgePredicate(filter, "source", "target")
	via, viaArgs := outlineViaPredicate(filter.ExcludeVia)
	structural := strings.NewReplacer("kind", "e.kind", "to_id", "e.to_id").Replace(structuralTargetInvalidSQL)
	visible := ` AND NOT ` + structural + mask + via
	// Reject neighbors before adding recursive states, exactly as Go admit.
	// Unresolved edges remain in final adjacency for boundary reconstruction;
	// resolved foreign neighbors are omitted there with the same scope rule.
	resolved := `e.` + next + ` NOT GLOB 'unresolved::*' AND instr(e.` + next + `, '::unresolved::') = 0 AND e.` + next + ` NOT GLOB 'external::*'`
	q := `WITH RECURSIVE walk(id, depth) AS (
		SELECT ?, 0
		UNION
		SELECT e.` + next + `, b.depth + 1 FROM walk b
		CROSS JOIN edges e INDEXED BY ` + index + ` ON e.view_gen = ? AND e.` + anchor + ` = b.id AND ` + kindPredicate +
		bfsAdmissionJoins(request.Direction, true, true, filter) + `
		WHERE b.depth < ? AND ` + resolved + ` AND ` + scope + visible + `
		LIMIT ` + strconv.Itoa(states+1) + `),
		expanded AS MATERIALIZED (SELECT id FROM walk GROUP BY id HAVING MIN(depth) < ` + strconv.Itoa(request.Depth) + `),
		counts AS MATERIALIZED (SELECT a.id,
		(SELECT COUNT(*) FROM (SELECT 1 FROM edges e INDEXED BY ` + index +
		bfsAdmissionJoins(request.Direction, false, false, filter) + `
		WHERE e.view_gen = ? AND e.` + anchor + ` = a.id` + visible + ` LIMIT ` + strconv.Itoa(request.Limit) + `)) AS n
		FROM expanded a)
		SELECT (SELECT COUNT(*) FROM walk) > ` + strconv.Itoa(states) + `, e.id, counts.n, ` + lookupQualifiedEdgeCols + `, ` + bfsAdmissionNeighborColumns(neighbor) + `
		FROM counts CROSS JOIN edges e INDEXED BY ` + index + ` ON e.view_gen = ? AND e.` + anchor + ` = counts.id AND ` + kindPredicate +
		bfsAdmissionJoins(request.Direction, true, false, filter) + ` WHERE 1` + visible +
		` AND (` + neighbor + `.id IS NULL OR NOT (` + resolved + `) OR (` + scope + `)) LIMIT ` + strconv.Itoa(edges+1)
	args := []any{request.Seed, generation, string(kindJSON), request.Depth}
	args = append(args, scopeArgs...)
	args = append(args, maskArgs...)
	args = append(args, viaArgs...)
	args = append(args, generation)
	args = append(args, maskArgs...)
	args = append(args, viaArgs...)
	args = append(args, generation, string(kindJSON))
	args = append(args, maskArgs...)
	args = append(args, viaArgs...)
	args = append(args, scopeArgs...)
	return outlineQuery{sql: q, args: args}
}

// BFSAdmissionFilteredContext reads an indexed neighborhood in one statement,
// preserving Go's admission rather than substituting a discovery-hop-only
// graph. Scope gates recursive neighbors; state/edge sentinels decline broad
// projections. Counts saturate at the caller's cap, which is sufficient for
// Go's exact pre-cap sort decision and bounds otherwise irrelevant counting.
func (s *Store) BFSAdmissionFilteredContext(ctx context.Context, request graph.BFSAdmissionRequest, filter graph.OutlineEdgeFilter) (graph.BFSAdmissionSnapshot, bool, error) {
	ctx = liveNameContext(ctx)
	if err := ctx.Err(); err != nil {
		return graph.BFSAdmissionSnapshot{}, false, err
	}
	_, edgeBudget, supported := graph.BFSAdmissionBudgets(request)
	if !supported || request.Direction != graph.DirectionForward && request.Direction != graph.DirectionBackward {
		return graph.BFSAdmissionSnapshot{}, false, nil
	}
	request.Kinds = anaDedupeEdgeKinds(request.Kinds)
	if len(request.Kinds) == 0 {
		return graph.BFSAdmissionSnapshot{}, false, nil
	}
	query := bfsAdmissionQuery(s.viewGen, request, filter)
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
