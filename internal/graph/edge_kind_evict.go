package graph

// EdgeKindEvicter removes every edge of the requested kinds in one backend
// operation. It is intentionally optional so adapter stores remain source
// compatible while production Graph and SQLite stores avoid one RemoveEdge
// mutation per reconciled relationship.
type EdgeKindEvicter interface {
	EvictEdgesByKinds(kinds []EdgeKind) int
}

// EvictEdgesByKinds dispatches to the set-oriented capability. The fallback is
// for lightweight adapter stores only: it deduplicates logical edge keys before
// calling the legacy mutator, avoiding repeated deletes when multiple source
// locations share the same endpoints.
//
// Callers must hold Store.ResolveMutex(). Scanning a kind reads From / To /
// Kind off edge values a resolve pass rewrites in place under that lock.
func EvictEdgesByKinds(s Store, kinds []EdgeKind) int {
	if s == nil || len(kinds) == 0 {
		return 0
	}
	if evicter, ok := s.(EdgeKindEvicter); ok {
		return evicter.EvictEdgesByKinds(kinds)
	}
	type edgeKey struct {
		from string
		to   string
		kind EdgeKind
	}
	keys := make(map[edgeKey]struct{})
	for _, kind := range kinds {
		for edge := range s.EdgesByKind(kind) {
			if edge != nil {
				keys[edgeKey{from: edge.From, to: edge.To, kind: edge.Kind}] = struct{}{}
			}
		}
	}
	removed := 0
	for key := range keys {
		if s.RemoveEdge(key.from, key.to, key.kind) {
			removed++
		}
	}
	return removed
}

// EvictEdgesByKinds implements EdgeKindEvicter for the in-memory graph. The
// graph's mutator is an in-process indexed adjacency update (not a backend
// round-trip); the SQLite implementation performs one DELETE statement.
func (g *Graph) EvictEdgesByKinds(kinds []EdgeKind) int {
	if g == nil || len(kinds) == 0 {
		return 0
	}
	type edgeKey struct {
		from string
		to   string
		kind EdgeKind
	}
	keys := make(map[edgeKey]struct{})
	for _, kind := range kinds {
		for edge := range g.EdgesByKind(kind) {
			if edge != nil {
				keys[edgeKey{from: edge.From, to: edge.To, kind: edge.Kind}] = struct{}{}
			}
		}
	}
	removed := 0
	for key := range keys {
		if g.RemoveEdge(key.from, key.to, key.kind) {
			removed++
		}
	}
	return removed
}

var _ EdgeKindEvicter = (*Graph)(nil)
