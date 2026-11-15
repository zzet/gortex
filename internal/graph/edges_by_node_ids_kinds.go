package graph

// EdgesByNodeIDsKindsReader is an optional batched adjacency projection
// restricted to a fixed set of edge kinds. It is the kind-filtered sibling of
// GetOutEdgesByNodeIDs / GetInEdgesByNodeIDs for callers that only ever read
// a few kinds of a node's adjacency: on a disk backend a hub node's unrelated
// rows (calls, references, typed_as, ...) are never decoded, so neither their
// text columns nor their meta maps are allocated.
//
// Contract, per requested ID:
//   - the result holds exactly the edges GetOut/InEdgesByNodeIDs would return
//     whose Kind is in kinds, with the same payload (full Meta and promoted
//     columns, so a caller may persist a returned edge back unchanged);
//   - within one kind, edges appear in the order the unfiltered batch read
//     yields them. On the SQLite store outgoing edges are ordered by source
//     line, kind, then insertion (within a kind, GetOutEdges' declared order)
//     and incoming edges by kind then insertion (GetInEdges' declared order);
//     the in-memory backend keeps its insertion order;
//   - across kinds the order is backend-specific: the in-memory backend
//     keeps insertion order, while SQLite orders same-line outgoing edges of
//     different kinds by kind name, where the unfiltered read's cross-kind
//     order is plan-dependent. A caller whose first-match loop spans kinds
//     must not rely on matching the unfiltered read across kinds;
//   - empty or nil kinds requests every kind (the unfiltered read);
//   - missing IDs are absent or map to an empty slice.
//
// Optional capability: OutEdgesByNodeIDsOfKinds / InEdgesByNodeIDsOfKinds
// fall back to the unfiltered batch read plus an order-preserving filter, so
// wrapping stores (overlays, delta writers) keep their masks.
type EdgesByNodeIDsKindsReader interface {
	GetOutEdgesByNodeIDsKinds(ids []string, kinds []EdgeKind) map[string][]*Edge
	GetInEdgesByNodeIDsKinds(ids []string, kinds []EdgeKind) map[string][]*Edge
}

// OutEdgesByNodeIDsOfKinds returns each node's outgoing edges of the given
// kinds through the store's EdgesByNodeIDsKindsReader capability, or through
// GetOutEdgesByNodeIDs filtered in order when the store does not offer it.
func OutEdgesByNodeIDsOfKinds(s Store, ids []string, kinds []EdgeKind) map[string][]*Edge {
	if len(kinds) == 0 {
		return s.GetOutEdgesByNodeIDs(ids)
	}
	if reader, ok := s.(EdgesByNodeIDsKindsReader); ok {
		return reader.GetOutEdgesByNodeIDsKinds(ids, kinds)
	}
	return filterEdgeMapKinds(s.GetOutEdgesByNodeIDs(ids), kinds)
}

// InEdgesByNodeIDsOfKinds is the incoming twin of OutEdgesByNodeIDsOfKinds.
func InEdgesByNodeIDsOfKinds(s Store, ids []string, kinds []EdgeKind) map[string][]*Edge {
	if len(kinds) == 0 {
		return s.GetInEdgesByNodeIDs(ids)
	}
	if reader, ok := s.(EdgesByNodeIDsKindsReader); ok {
		return reader.GetInEdgesByNodeIDsKinds(ids, kinds)
	}
	return filterEdgeMapKinds(s.GetInEdgesByNodeIDs(ids), kinds)
}

// GetOutEdgesByNodeIDsKinds filters the in-memory per-node append slice in
// place order. See EdgesByNodeIDsKindsReader for the contract.
func (g *Graph) GetOutEdgesByNodeIDsKinds(ids []string, kinds []EdgeKind) map[string][]*Edge {
	if len(kinds) == 0 {
		return g.GetOutEdgesByNodeIDs(ids)
	}
	return g.edgesByNodeIDsKinds(ids, kinds, true)
}

// GetInEdgesByNodeIDsKinds is the incoming twin of GetOutEdgesByNodeIDsKinds.
func (g *Graph) GetInEdgesByNodeIDsKinds(ids []string, kinds []EdgeKind) map[string][]*Edge {
	if len(kinds) == 0 {
		return g.GetInEdgesByNodeIDs(ids)
	}
	return g.edgesByNodeIDsKinds(ids, kinds, false)
}

func (g *Graph) edgesByNodeIDsKinds(ids []string, kinds []EdgeKind, outgoing bool) map[string][]*Edge {
	if len(ids) == 0 {
		return nil
	}
	want := edgeKindSet(kinds)
	out := make(map[string][]*Edge, len(ids))
	for _, id := range ids {
		if id == "" {
			continue
		}
		if _, ok := out[id]; ok {
			continue
		}
		s := g.shardFor(id)
		s.mu.RLock()
		src := s.inEdges[id]
		if outgoing {
			src = s.outEdges[id]
		}
		var kept []*Edge
		for _, e := range src {
			if e != nil {
				if _, ok := want[e.Kind]; ok {
					kept = append(kept, e)
				}
			}
		}
		s.mu.RUnlock()
		out[id] = kept
	}
	return out
}

func edgeKindSet(kinds []EdgeKind) map[EdgeKind]struct{} {
	set := make(map[EdgeKind]struct{}, len(kinds))
	for _, kind := range kinds {
		set[kind] = struct{}{}
	}
	return set
}

// filterEdgeMapKinds keeps, per ID, the edges of the requested kinds in their
// original order. IDs whose adjacency filters to nothing stay present with a
// nil slice so a caller's per-ID presence checks see the same keys.
func filterEdgeMapKinds(in map[string][]*Edge, kinds []EdgeKind) map[string][]*Edge {
	if in == nil {
		return nil
	}
	want := edgeKindSet(kinds)
	out := make(map[string][]*Edge, len(in))
	for id, edges := range in {
		var kept []*Edge
		for _, e := range edges {
			if e != nil {
				if _, ok := want[e.Kind]; ok {
					kept = append(kept, e)
				}
			}
		}
		out[id] = kept
	}
	return out
}
