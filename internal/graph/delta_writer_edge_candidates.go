package graph

// edgeCandidateProvider is the store capability that answers a candidate
// batch by endpoint and site predicates.
type edgeCandidateProvider interface {
	GetEdgeCandidates(endpoints []EdgeEndpoint, sites []EdgeSite) EdgeCandidateSet
}

// GetEdgeCandidates answers a candidate batch through the stack: the store at
// the bottom answers only the requested endpoints and sites (not every row of
// each source — a placeholder source such as an unresolved name keys rows
// across the whole corpus), each layer's rows of the requested sources are
// matched in memory, and every row is kept only when no layer above its level
// hides it — the candidates the composed adjacency holds. The rows are copies:
// callers re-point them before reindexing. A bottom store with no candidate
// or batched adjacency form answers from the composed adjacency.
func (dw *DeltaWriter) GetEdgeCandidates(endpoints []EdgeEndpoint, sites []EdgeSite) EdgeCandidateSet {
	s := dw.stack()
	provider, ok := s.base.(edgeCandidateProvider)
	if _, batched := s.base.(batchedAdjacencyReader); !ok || !batched {
		return dw.getEdgeCandidatesByAdjacency(endpoints, sites)
	}
	out := NewEdgeCandidateSet()
	endpointWanted := make(map[EdgeEndpoint]struct{}, len(endpoints))
	siteWanted := make(map[EdgeSite]struct{}, len(sites))
	var sources []string
	seenSource := make(map[string]struct{})
	addSource := func(id string) {
		if _, dup := seenSource[id]; !dup {
			seenSource[id] = struct{}{}
			sources = append(sources, id)
		}
	}
	for _, key := range endpoints {
		if key.From == "" || key.To == "" {
			continue
		}
		endpointWanted[key] = struct{}{}
		addSource(key.From)
	}
	for _, key := range sites {
		if key.From == "" {
			continue
		}
		siteWanted[key] = struct{}{}
		addSource(key.From)
	}
	if len(sources) == 0 {
		return out
	}
	copies := make(map[*Edge]*Edge)
	copyOf := func(e *Edge) *Edge {
		if c, ok := copies[e]; ok {
			return c
		}
		c := cloneDeltaEdge(e)
		copies[e] = c
		return c
	}
	below := provider.GetEdgeCandidates(endpoints, sites)
	seenEndpoint := make(map[*Edge]struct{})
	for _, edges := range below.byEndpoint {
		for _, e := range edges {
			if _, dup := seenEndpoint[e]; e == nil || dup {
				continue
			}
			seenEndpoint[e] = struct{}{}
			if s.edgeVisibleFrom(e, 0) {
				out.AddEndpoint(copyOf(e))
			}
		}
	}
	seenSite := make(map[*Edge]struct{})
	for _, edges := range below.bySite {
		for _, e := range edges {
			if _, dup := seenSite[e]; e == nil || dup {
				continue
			}
			seenSite[e] = struct{}{}
			if s.edgeVisibleFrom(e, 0) {
				out.AddSite(copyOf(e))
			}
		}
	}
	for i, l := range s.layers {
		for _, edges := range dw.layerAdjacency(l, sources, false) {
			for _, e := range edges {
				if e == nil || !s.edgeVisibleFrom(e, i+1) {
					continue
				}
				if _, ok := endpointWanted[EdgeEndpoint{From: e.From, To: e.To}]; ok {
					out.AddEndpoint(copyOf(e))
				}
				_, exact := siteWanted[EdgeSite{From: e.From, Line: e.Line, Kind: e.Kind}]
				_, anyKind := siteWanted[EdgeSite{From: e.From, Line: e.Line}]
				if exact || anyKind {
					out.AddSite(copyOf(e))
				}
			}
		}
	}
	return out
}
