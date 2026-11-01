package mcp

import "github.com/zzet/gortex/internal/graph"

// scopedEdgeCountBatch bounds one adjacency read while counting a bound
// session's edges.
const scopedEdgeCountBatch = 1024

// countEdgesWithin counts the edges whose endpoints both lie in inScope, or
// every edge when inScope is nil. A bound session reads only its own nodes'
// outgoing adjacency, a batch at a time, instead of every edge in the store.
func countEdgesWithin(g graph.Reader, inScope map[string]bool) int {
	if inScope == nil {
		return g.EdgeCount()
	}
	ids := make([]string, 0, len(inScope))
	for id, in := range inScope {
		if in {
			ids = append(ids, id)
		}
	}
	total := 0
	for start := 0; start < len(ids); start += scopedEdgeCountBatch {
		batch := ids[start:min(start+scopedEdgeCountBatch, len(ids))]
		for _, edges := range g.GetOutEdgesByNodeIDs(batch) {
			for _, edge := range edges {
				if edge != nil && inScope[edge.To] {
					total++
				}
			}
		}
	}
	return total
}
