package store_sqlite

import (
	"sort"
	"strings"

	"github.com/zzet/gortex/internal/graph"
)

var _ graph.BFSCapable = (*Store)(nil)

// BFS runs a bounded breadth-first traversal in a single round-trip via a
// recursive CTE — the disk-backed sibling of the in-memory
// (*graph.Graph).BFS reference. See graph.BFSCapable for the contract;
// the two are shadow-tested for identical hop-sets in the conformance
// suite (storetest), including a cycle fixture.
//
// The recursive term joins edges on the direction's indexed column —
// edges_by_from(from_id, kind) for a forward walk, edges_by_to(to_id,
// kind) for a backward walk — and the nodes primary key, so it stays
// index-driven instead of scanning the edges table (confirmed via
// EXPLAIN QUERY PLAN in store_bfs_test.go). The nodes join also enforces
// the "node-backed targets only" rule: an edge to an unresolved /
// external stub with no node row is not followed. A cycle terminates on
// the depth bound; the outer ROW_NUMBER picks each node's minimum-depth,
// (parent, kind)-smallest discovery edge so the result is deterministic
// and matches the in-memory walk's bfsHopLess tie-break.
//
// Reads run lock-free, like the store's other read paths (SQLite WAL
// serves readers concurrently with the single serialized writer).
func (s *Store) BFS(seeds []string, dir graph.Direction, kinds []graph.EdgeKind, maxDepth, limit int) ([]graph.BFSHop, error) {
	seen := make(map[string]struct{}, len(seeds))
	uniqSeeds := make([]string, 0, len(seeds))
	for _, sd := range seeds {
		if sd == "" {
			continue
		}
		if _, ok := seen[sd]; ok {
			continue
		}
		seen[sd] = struct{}{}
		uniqSeeds = append(uniqSeeds, sd)
	}
	if len(uniqSeeds) == 0 {
		return nil, nil
	}

	uniqKinds := anaDedupeEdgeKinds(kinds)

	// Seed-only fast path: with no edge kinds to follow or a non-positive
	// depth bound the result is exactly the seeds at depth 0. Seeds enter
	// unconditionally (no node-backed gate), matching the in-memory
	// reference, which adds them before any traversal.
	if len(uniqKinds) == 0 || maxDepth <= 0 {
		hops := make([]graph.BFSHop, 0, len(uniqSeeds))
		for _, sd := range uniqSeeds {
			hops = append(hops, graph.BFSHop{NodeID: sd, Depth: 0})
		}
		sortBFSHops(hops)
		if limit > 0 && len(hops) > limit {
			hops = hops[:limit]
		}
		return hops, nil
	}

	query := buildBFSQuery(dir, len(uniqSeeds), len(uniqKinds), limit > 0)

	args := make([]any, 0, len(uniqSeeds)+len(uniqKinds)+3)
	for _, sd := range uniqSeeds {
		args = append(args, sd)
	}
	args = append(args, maxDepth)
	for _, k := range uniqKinds {
		args = append(args, string(k))
	}
	args = append(args, s.viewGen)
	if limit > 0 {
		args = append(args, limit)
	}

	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []graph.BFSHop
	for rows.Next() {
		var (
			nodeID, parentID, edgeKind string
			depth                      int
		)
		if err := rows.Scan(&nodeID, &depth, &parentID, &edgeKind); err != nil {
			return nil, err
		}
		out = append(out, graph.BFSHop{
			NodeID:   nodeID,
			Depth:    depth,
			ParentID: parentID,
			EdgeKind: graph.EdgeKind(edgeKind),
		})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// buildBFSQuery assembles the recursive-CTE BFS statement for the given
// direction, seed count, kind count, and whether a LIMIT is applied. It is
// a pure string builder (no I/O) so a test can EXPLAIN QUERY PLAN the exact
// statement and assert the recursive join stays index-driven.
//
// Direction selects the join columns: forward follows from_id -> to_id (the
// discovered neighbour is the edge target), backward follows to_id ->
// from_id (the neighbour is the edge source). The recursive term joins
// edges on the walked node's id, so a forward walk leads with from_id (the
// edges_by_from(from_id, kind) index) and a backward walk leads with to_id
// (edges_by_to(to_id, kind)); the nodes join uses the nodes primary key.
func buildBFSQuery(dir graph.Direction, nSeeds, nKinds int, withLimit bool) string {
	joinCol, nextCol := "e.from_id", "e.to_id"
	edgeIdx := "edges_by_from"
	if dir == graph.DirectionBackward {
		joinCol, nextCol = "e.to_id", "e.from_id"
		edgeIdx = "edges_by_to"
	}

	var b strings.Builder
	b.WriteString("WITH RECURSIVE seeds(node_id) AS (VALUES ")
	for i := 0; i < nSeeds; i++ {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString("(?)")
	}
	b.WriteString("),\n")
	b.WriteString("bfs(node_id, depth, parent_id, edge_kind) AS (\n")
	b.WriteString("  SELECT node_id, 0, '', '' FROM seeds\n")
	b.WriteString("  UNION\n")
	b.WriteString("  SELECT " + nextCol + ", b.depth + 1, b.node_id, e.kind\n")
	b.WriteString("  FROM bfs b\n")
	// INDEXED BY forces the frontier-node seek (from_id / to_id leading)
	// instead of the planner's stats-free preference for edges_by_kind,
	// which on a hot kind would scan every edge of that kind per frontier
	// node. If the index is ever absent (a bulk-load window drops it) the
	// query errors and the engine falls back to the in-memory walk.
	b.WriteString("  JOIN edges e INDEXED BY " + edgeIdx + " ON " + joinCol + " = b.node_id\n")
	// The node-backed gate pairs generations: a target that exists only in
	// another generation must read as absent, not as a followable hop.
	b.WriteString("  JOIN nodes n ON n.id = " + nextCol + " AND n.view_gen = e.view_gen\n")
	b.WriteString("  WHERE b.depth < ? AND e.kind IN (" + inPlaceholders(nKinds) + ") AND e.view_gen = ?\n")
	b.WriteString("),\n")
	b.WriteString("ranked AS (\n")
	b.WriteString("  SELECT node_id, depth, parent_id, edge_kind,\n")
	b.WriteString("         ROW_NUMBER() OVER (PARTITION BY node_id ORDER BY depth, parent_id, edge_kind) AS rn\n")
	b.WriteString("  FROM bfs\n")
	b.WriteString(")\n")
	b.WriteString("SELECT node_id, depth, parent_id, edge_kind FROM ranked WHERE rn = 1\n")
	b.WriteString("ORDER BY depth, node_id")
	if withLimit {
		b.WriteString("\nLIMIT ?")
	}
	return b.String()
}

// sortBFSHops orders hops by (depth, node_id) — the same final ordering
// the recursive-CTE query applies, used by the seed-only fast path.
func sortBFSHops(hops []graph.BFSHop) {
	sort.Slice(hops, func(i, j int) bool {
		if hops[i].Depth != hops[j].Depth {
			return hops[i].Depth < hops[j].Depth
		}
		return hops[i].NodeID < hops[j].NodeID
	})
}
