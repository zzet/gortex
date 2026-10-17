package store_sqlite

import (
	"github.com/zzet/gortex/internal/graph"
)

// The graph-traversal and subgraph-reader optional capabilities for the
// SQLite backend. Each method mirrors the in-memory *graph.Graph
// reference implementation exactly so both satisfy the same conformance
// suite (internal/graph/storetest). The walks use the same per-node /
// batched edge readers the in-memory store uses (GetOutEdges /
// GetInEdges / GetFileNodes / GetNodesByIDs / GetIn|OutEdgesByNodeIDs),
// which on SQLite hit the (from_id,kind) / (to_id,kind) / file_path
// indexes — no new prepared statements needed.

var (
	_ graph.ReachableForwardByKinds = (*Store)(nil)
	_ graph.ClassHierarchyTraverser = (*Store)(nil)
	_ graph.FrontierExpander        = (*Store)(nil)
	_ graph.FileEditingContext      = (*Store)(nil)
	_ graph.FileSubGraphReader      = (*Store)(nil)
	_ graph.FileSubGraphCountReader = (*Store)(nil)
)

// ReachableForwardByKinds computes the set of node IDs reachable from
// the seed frontier via outgoing edges whose Kind is in kinds, via a
// layer-by-layer forward BFS. Empty seeds returns nil; empty kinds
// returns the seed set unchanged. The returned map keys are the
// reachable IDs (seeds included); every value is true.
func (s *Store) ReachableForwardByKinds(seeds []string, kinds []graph.EdgeKind) map[string]bool {
	if len(seeds) == 0 {
		return nil
	}
	covered := make(map[string]bool, len(seeds))
	frontier := make([]string, 0, len(seeds))
	for _, id := range seeds {
		if id == "" || covered[id] {
			continue
		}
		covered[id] = true
		frontier = append(frontier, id)
	}
	if len(kinds) == 0 {
		return covered
	}
	allowed := make(map[graph.EdgeKind]struct{}, len(kinds))
	for _, k := range kinds {
		allowed[k] = struct{}{}
	}
	for len(frontier) > 0 {
		next := frontier[:0:0]
		for _, id := range frontier {
			for _, e := range s.GetOutEdges(id) {
				if e == nil {
					continue
				}
				if _, ok := allowed[e.Kind]; !ok {
					continue
				}
				if !covered[e.To] {
					covered[e.To] = true
					next = append(next, e.To)
				}
			}
		}
		frontier = next
	}
	return covered
}

// ClassHierarchyTraverse walks the inheritance subgraph rooted at
// seedID, following only edges whose Kind is in kinds, up to depth hops.
// direction "up" follows outgoing edges; "down" follows incoming. Empty
// kinds, depth <= 0, an unknown direction, or an unknown seed return
// nil. Each returned row carries the full Path (node IDs from the seed,
// exclusive) and per-hop EdgeKinds for one terminal node.
func (s *Store) ClassHierarchyTraverse(
	seedID string,
	direction string,
	kinds []graph.EdgeKind,
	depth int,
) []graph.ClassHierarchyRow {
	if seedID == "" || depth <= 0 || len(kinds) == 0 {
		return nil
	}
	kset := make(map[graph.EdgeKind]struct{}, len(kinds))
	for _, k := range kinds {
		if k == "" {
			continue
		}
		kset[k] = struct{}{}
	}
	if len(kset) == 0 {
		return nil
	}
	if s.GetNode(seedID) == nil {
		return nil
	}
	walkUp := direction == "up"
	walkDown := direction == "down"
	if !walkUp && !walkDown {
		return nil
	}
	type travQueued struct {
		id        string
		path      []string
		edgeKinds []graph.EdgeKind
		hops      int
	}
	visited := map[string]struct{}{seedID: {}}
	queue := []travQueued{{id: seedID, path: nil, edgeKinds: nil, hops: 0}}
	var out []graph.ClassHierarchyRow
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		if cur.hops >= depth {
			continue
		}
		var edges []*graph.Edge
		if walkUp {
			edges = s.GetOutEdges(cur.id)
		} else {
			edges = s.GetInEdges(cur.id)
		}
		for _, e := range edges {
			if e == nil {
				continue
			}
			if _, ok := kset[e.Kind]; !ok {
				continue
			}
			var nb string
			if walkUp {
				nb = e.To
			} else {
				nb = e.From
			}
			if nb == "" {
				continue
			}
			if _, ok := visited[nb]; ok {
				continue
			}
			visited[nb] = struct{}{}
			newPath := append([]string(nil), cur.path...)
			newPath = append(newPath, nb)
			newKinds := append([]graph.EdgeKind(nil), cur.edgeKinds...)
			newKinds = append(newKinds, e.Kind)
			out = append(out, graph.ClassHierarchyRow{
				Path:      newPath,
				EdgeKinds: newKinds,
			})
			queue = append(queue, travQueued{id: nb, path: newPath, edgeKinds: newKinds, hops: cur.hops + 1})
		}
	}
	return out
}

// ExpandFrontier returns, for the given source IDs, their adjacent edges
// of the requested kinds plus the neighbour node at each edge's far end.
// forward=true follows outgoing edges (neighbour = edge target);
// forward=false follows incoming (neighbour = edge source). Empty ids or
// empty kinds return nil; limit > 0 caps the total number of hops.
func (s *Store) ExpandFrontier(ids []string, forward bool, kinds []graph.EdgeKind, limit int) []graph.FrontierHop {
	if len(ids) == 0 || len(kinds) == 0 {
		return nil
	}
	kset := make(map[graph.EdgeKind]struct{}, len(kinds))
	for _, k := range kinds {
		kset[k] = struct{}{}
	}
	var out []graph.FrontierHop
	for _, id := range ids {
		var edges []*graph.Edge
		if forward {
			edges = s.GetOutEdges(id)
		} else {
			edges = s.GetInEdges(id)
		}
		for _, e := range edges {
			if e == nil {
				continue
			}
			if _, ok := kset[e.Kind]; !ok {
				continue
			}
			var nbID string
			if forward {
				nbID = e.To
			} else {
				nbID = e.From
			}
			nb := s.GetNode(nbID)
			if nb == nil {
				continue
			}
			out = append(out, graph.FrontierHop{Edge: e, Neighbor: nb})
			if limit > 0 && len(out) >= limit {
				return out
			}
		}
	}
	return out
}

// FileEditingContext returns the get_editing_context payload for
// filePath: the file node, the symbols defined in it, the file node's
// import out-edges, and the 1-hop callers / callees (via EdgeCalls) of
// the defined call-target symbols, filtered to symbols outside the file.
// kinds is the set of node kinds treated as call targets (function +
// method). Empty path or a file with no nodes returns nil.
func (s *Store) FileEditingContext(filePath string, kinds []graph.NodeKind) *graph.FileEditingContextResult {
	return graph.FileEditingContextOf(s, filePath, kinds)
}

// GetFileSubGraph returns every node anchored to filePath plus every
// edge adjacent to one of those nodes, deduplicated by (from, to, kind).
// A missing / empty file returns (nil, nil).
func (s *Store) GetFileSubGraph(filePath string) ([]*graph.Node, []*graph.Edge) {
	if filePath == "" {
		return nil, nil
	}
	nodes := s.GetFileNodes(filePath)
	if len(nodes) == 0 {
		return nil, nil
	}
	ids := make([]string, 0, len(nodes))
	for _, n := range nodes {
		if n != nil && n.ID != "" {
			ids = append(ids, n.ID)
		}
	}
	outByID := s.GetOutEdgesByNodeIDs(ids)
	inByID := s.GetInEdgesByNodeIDs(ids)
	type travEdgeKey struct {
		from string
		to   string
		kind graph.EdgeKind
	}
	seen := make(map[travEdgeKey]struct{}, 2*len(ids))
	edges := make([]*graph.Edge, 0, 2*len(ids))
	add := func(e *graph.Edge) {
		if e == nil {
			return
		}
		k := travEdgeKey{from: e.From, to: e.To, kind: e.Kind}
		if _, ok := seen[k]; ok {
			return
		}
		seen[k] = struct{}{}
		edges = append(edges, e)
	}
	for _, id := range ids {
		for _, e := range outByID[id] {
			add(e)
		}
		for _, e := range inByID[id] {
			add(e)
		}
	}
	return nodes, edges
}

// GetFileSubGraphCounts is the count-only sibling of GetFileSubGraph:
// it returns the file's nodes plus the number of distinct adjacent
// edges, without materialising the edge slice for the caller.
func (s *Store) GetFileSubGraphCounts(filePath string) ([]*graph.Node, int) {
	nodes, edges := s.GetFileSubGraph(filePath)
	return nodes, len(edges)
}
