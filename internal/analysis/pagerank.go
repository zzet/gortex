package analysis

import "github.com/zzet/gortex/internal/graph"

// weightedLink is one adjacency entry carrying the provenance weight of
// the edge it represents. Shared by ComputeHITS and ComputePageRank so
// both centrality measures attenuate over-represented LSP-dispatch
// edges the same way.
type weightedLink struct {
	id string
	w  float64
}

// PageRankResult holds per-node PageRank centrality scores.
type PageRankResult struct {
	// Scores maps node ID to its PageRank value. The values sum to
	// ~1 across all nodes; individual scores are small and best read
	// relative to Max.
	Scores map[string]float64
	// Max is the largest score in Scores — the normaliser callers use
	// to project centrality onto a 0..1 / 0..100 scale.
	Max float64
}

// ScoreOf returns the PageRank score for a node, or 0 when absent.
func (r *PageRankResult) ScoreOf(id string) float64 {
	if r == nil {
		return 0
	}
	return r.Scores[id]
}

// PageRank tuning. Damping 0.85 is the canonical web-graph value;
// iterations are fixed rather than convergence-tested because the
// graph is small enough that 40 power-iteration steps are well past
// the point the ranking order stabilises.
const (
	pageRankDamping    = 0.85
	pageRankIterations = 40
)

// ComputePageRank runs PageRank centrality over the call / reference
// graph. Rank flows backwards along call edges: a function is central
// when central functions call it, so a heavily-depended-on symbol
// accumulates score. Only EdgeCalls and EdgeReferences participate —
// structural edges (defines, member_of, imports) would drown the
// dependency signal.
//
// Dangling nodes (no outgoing call/reference edge — leaf utilities)
// redistribute their mass uniformly each iteration so the scores stay
// a proper probability distribution.
func ComputePageRank(g graph.Store) *PageRankResult { return ComputePageRankPaced(g, nil) }

// ComputePageRankPaced is ComputePageRank with a cooperative scheduling point
// in every hot loop (see Pace). A nil Pace never parks.
func ComputePageRankPaced(g graph.Store, pace *Pace) *PageRankResult {
	if g == nil {
		return &PageRankResult{Scores: map[string]float64{}}
	}
	ids := make([]string, 0, g.NodeCount())
	for node := range graph.NodesLightSeq(g) {
		pace.Tick()
		if node != nil && node.ID != "" && !graph.IsProxyNode(node) {
			ids = append(ids, node.ID)
		}
	}
	n := len(ids)
	if n == 0 {
		return &PageRankResult{Scores: map[string]float64{}}
	}

	// Provenance-weighted adjacency: each edge contributes its
	// graph.ProvenanceWeight to the source's out-weight and rides that
	// weight on the in-link. Score then flows along an edge in
	// proportion to w/outWeight, so the transition matrix columns
	// still sum to 1 (mass is conserved) but an abundant LSP-dispatch
	// fan-out no longer hands a leaf utility outsized centrality. With
	// uniform weights the w/outWeight ratio reduces to 1/outDegree —
	// identical to the unweighted PageRank.
	outWeight := make(map[string]float64, n)
	inLinks := make(map[string][]weightedLink)
	// Meta-less kind-scoped scan: this pass reads only e.Kind, endpoints, and
	// graph.ProvenanceWeight — never arbitrary Meta — so it must not pay to decode
	// every edge's meta blob on a warm-restart whole-graph run.
	for e := range graph.EdgesLightSeq(g, graph.EdgeCalls, graph.EdgeReferences) {
		pace.Tick()
		if e.Kind != graph.EdgeCalls && e.Kind != graph.EdgeReferences {
			continue
		}
		if edgeTouchesProxy(e) {
			continue
		}
		w := graph.ProvenanceWeight(e)
		outWeight[e.From] += w
		inLinks[e.To] = append(inLinks[e.To], weightedLink{e.From, w})
	}

	score := make(map[string]float64, n)
	initial := 1.0 / float64(n)
	for _, id := range ids {
		score[id] = initial
	}

	base := (1 - pageRankDamping) / float64(n)
	for iter := 0; iter < pageRankIterations; iter++ {
		// Dangling nodes have nowhere to send their score; pool it
		// and spread it across every node so no mass leaks.
		var dangling float64
		for _, id := range ids {
			pace.Tick()
			if outWeight[id] == 0 {
				dangling += score[id]
			}
		}
		danglingShare := pageRankDamping * dangling / float64(n)

		next := make(map[string]float64, n)
		for _, id := range ids {
			pace.Tick()
			var sum float64
			for _, src := range inLinks[id] {
				if d := outWeight[src.id]; d > 0 {
					sum += score[src.id] * src.w / d
				}
			}
			next[id] = base + danglingShare + pageRankDamping*sum
		}
		score = next
	}

	var max float64
	for _, v := range score {
		if v > max {
			max = v
		}
	}
	return &PageRankResult{Scores: score, Max: max}
}
