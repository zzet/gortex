package analysis

import (
	"context"
	"math/rand"
	"sort"

	"github.com/zzet/gortex/internal/graph"
)

// BetweennessResult holds per-node betweenness-centrality scores.
//
// Betweenness measures how often a node lies on a shortest path
// between two other nodes — a high score marks a structural
// bottleneck that load flows through. It complements PageRank
// (which measures being depended-on) by measuring being a conduit.
type BetweennessResult struct {
	// Scores maps node ID to its betweenness value. Larger means the
	// node sits on more shortest paths; values are best read relative
	// to Max.
	Scores map[string]float64
	// Max is the largest score in Scores — the normaliser callers use
	// to project centrality onto a 0..1 / 0..100 scale.
	Max float64
	// Sampled reports whether the sampled-pivot fast path was used.
	// False means every node was a source (exact Brandes').
	Sampled bool
	// Pivots is the number of source nodes the accumulation ran from.
	// Equals the node count on the exact path.
	Pivots int
}

// ScoreOf returns the betweenness score for a node, or 0 when absent.
func (r *BetweennessResult) ScoreOf(id string) float64 {
	if r == nil {
		return 0
	}
	return r.Scores[id]
}

// Betweenness tuning.
//
// betweennessExactThreshold is the node-count cutoff between the two
// paths: at or below it every node is a source (exact Brandes',
// O(V*E)); above it the sampled fast path runs from a bounded number
// of pivots (O(k*E)) and rescales by V/k.
//
// betweennessPivots bounds k on the sampled path. ~256 source pivots
// give a stable ranking on graphs into the hundreds of thousands of
// nodes — betweenness is needed for ordering hotspots, not for an
// exact path count, and the sampling error shrinks with graph size.
//
// betweennessSeed fixes the pivot-sampling RNG so a given graph
// yields the same scores on every run.
const (
	betweennessExactThreshold = 2000
	betweennessPivots         = 256
	betweennessSeed           = 0x6f7274
)

// ComputeBetweenness runs betweenness centrality over the call /
// reference graph. It adapts to graph size: small graphs get exact
// Brandes' (every node a source); large graphs switch to the
// sampled-pivot fast path — accumulate from k randomly chosen
// sources and scale the result by V/k. Both paths share the same
// single-source accumulation kernel, so the only difference is which
// sources feed it.
//
// Only EdgeCalls and EdgeReferences participate, matching
// ComputePageRank: structural edges (defines, member_of, imports)
// would swamp the dependency signal. Edges are treated as unweighted
// and directed — shortest paths are hop counts found by BFS.
//
// Pivot sampling is seeded with a fixed seed, so results are
// reproducible run to run.
func ComputeBetweenness(g graph.Reader) *BetweennessResult {
	result, _ := ComputeBetweennessContext(context.Background(), g)
	return result
}

// ComputeBetweennessContext retains the same deterministic pivots and scores,
// but never returns a partial ranking when its owner is cancelled.
func ComputeBetweennessContext(ctx context.Context, g graph.Reader) (*BetweennessResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if g == nil {
		return &BetweennessResult{Scores: map[string]float64{}}, nil
	}
	if ctx.Done() != nil {
		g = graph.BindReadContext(g, ctx)
	}
	// Betweenness measures shortest-path centrality across the
	// call / reference subgraph; only function and method nodes carry
	// those edges. The scoring kernel only ever touches node IDs, so
	// the unfiltered AllNodes() pull was wasted on the other 90% of
	// the node table AND on the 9 unused columns of every retained
	// row. NodeIDsByKinds returns just the id column from a single
	// query; NodesByKindsScanner is the legacy fallback for
	// backends that haven't shipped the id projection yet.
	betweennessKinds := []graph.EdgeKind{graph.EdgeCalls, graph.EdgeReferences}
	bcNodeKinds := []graph.NodeKind{graph.KindFunction, graph.KindMethod}
	var ids []string
	if scan, ok := g.(graph.NodeIDsByKinds); ok {
		ids = scan.NodeIDsByKinds(bcNodeKinds)
	} else if scan, ok := g.(graph.NodesByKindsScanner); ok {
		ns := scan.NodesByKinds(bcNodeKinds)
		ids = make([]string, 0, len(ns))
		for _, nd := range ns {
			ids = append(ids, nd.ID)
		}
	} else {
		all := g.AllNodes()
		ids = make([]string, 0, len(all))
		for _, nd := range all {
			if nd.Kind == graph.KindFunction || nd.Kind == graph.KindMethod {
				ids = append(ids, nd.ID)
			}
		}
	}
	// Federation proxy nodes carry real kinds, so the kind filter above
	// keeps them — drop them here so a remote stub is never a pivot.
	ids = excludeProxyIDs(ids)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	n := len(ids)
	if n == 0 {
		return &BetweennessResult{Scores: map[string]float64{}}, nil
	}

	// Stable node ordering: betweenness itself is order-independent,
	// but a deterministic order makes the sampled pivot pick
	// reproducible regardless of the iteration order
	// NodeIDsByKinds happens to return.
	sort.Strings(ids)
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	// Forward adjacency over the call / reference subgraph.
	// EdgeAdjacencyForKinds returns only the (from, to) projection of
	// function/method endpoints — the disk path collapses to one
	// join with both endpoint kinds enforced in the store, so
	// neither the cross-kind edges nor the ~10 unused columns are
	// ever materialized. Falls back to EdgesByKinds (and then
	// EdgesByKind per kind) on backends that don't implement the
	// adjacency capability.
	// Convert stable string IDs to compact integer slots once. Brandes runs one
	// BFS per pivot; keeping strings in its inner loop previously rebuilt four
	// large string-keyed maps per pivot and allocated 4.5 GiB during one daemon
	// wakeup on the production graph.
	idIndex := make(map[string]int, n)
	for i, id := range ids {
		if i&1023 == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		idIndex[id] = i
	}
	adjacency := make([][]int, n)
	scanned := 0
	appendPair := func(from, to string) bool {
		scanned++
		if scanned&1023 == 0 && ctx.Err() != nil {
			return false
		}
		fromIdx, fromOK := idIndex[from]
		toIdx, toOK := idIndex[to]
		if !fromOK || !toOK {
			return true
		}
		adjacency[fromIdx] = append(adjacency[fromIdx], toIdx)
		return true
	}
	if adjScan, ok := g.(graph.EdgeAdjacencyForKinds); ok {
		for pair := range adjScan.EdgeAdjacencyForKinds(betweennessKinds, bcNodeKinds) {
			if !appendPair(pair[0], pair[1]) {
				return nil, ctx.Err()
			}
		}
	} else if es, ok := g.(graph.EdgesByKindsScanner); ok {
		for e := range es.EdgesByKinds(betweennessKinds) {
			if e != nil {
				if !appendPair(e.From, e.To) {
					return nil, ctx.Err()
				}
			}
		}
	} else {
		for _, kind := range betweennessKinds {
			for e := range g.EdgesByKind(kind) {
				if e != nil {
					if !appendPair(e.From, e.To) {
						return nil, ctx.Err()
					}
				}
			}
		}
	}

	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// Adaptive fast path: exact for small graphs, sampled otherwise.
	sources := ids
	sampled := false
	if n > betweennessExactThreshold {
		sampled = true
		sources = samplePivots(ids, betweennessPivots)
	}

	if err := ctx.Err(); err != nil {
		return nil, err
	}
	scoreDense := make([]float64, n)
	scratch := newBrandesScratch(n)
	for _, src := range sources {
		if err := brandesAccumulateContext(ctx, idIndex[src], adjacency, scoreDense, &scratch); err != nil {
			return nil, err
		}
	}

	// Rescale the sampled estimate up to a full-source equivalent and convert
	// back to the public string-keyed result only once, after every pivot.
	scale := 1.0
	if sampled && len(sources) > 0 {
		scale = float64(n) / float64(len(sources))
	}
	score := make(map[string]float64, n)
	var max float64
	for i, id := range ids {
		if i&1023 == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		v := scoreDense[i] * scale
		score[id] = v
		if v > max {
			max = v
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return &BetweennessResult{
		Scores:  score,
		Max:     max,
		Sampled: sampled,
		Pivots:  len(sources),
	}, nil
}

// samplePivots picks k distinct source nodes from ids using a
// fixed-seed RNG, so the chosen pivots — and therefore the resulting
// scores — are identical on every run. When k is at or above the
// node count the whole set is returned (the caller would not be on
// the sampled path in that case, but the guard keeps samplePivots
// total).
func samplePivots(ids []string, k int) []string {
	if k >= len(ids) {
		out := make([]string, len(ids))
		copy(out, ids)
		return out
	}
	rng := rand.New(rand.NewSource(betweennessSeed))
	perm := rng.Perm(len(ids))
	out := make([]string, k)
	for i := range k {
		out[i] = ids[perm[i]]
	}
	return out
}

// brandesAccumulate runs one single-source pass of Brandes'
// algorithm: a BFS from src counts shortest paths, then a reverse
// dependency-accumulation sweep folds this source's contribution
// into score. Intermediate vertices (everything but src and the
// target) collect the credit, which is exactly betweenness.
//
// brandesScratch owns the dense per-source state. Its backing arrays and
// predecessor lists are reused across pivots, so sampled analysis allocates in
// proportion to the graph once rather than once per pivot.
type brandesScratch struct {
	sigma []float64
	dist  []int
	delta []float64
	preds [][]int
	queue []int
	order []int
}

func newBrandesScratch(nodes int) brandesScratch {
	return brandesScratch{
		sigma: make([]float64, nodes),
		dist:  make([]int, nodes),
		delta: make([]float64, nodes),
		preds: make([][]int, nodes),
		queue: make([]int, 0, nodes),
		order: make([]int, 0, nodes),
	}
}

func (s *brandesScratch) resetContext(ctx context.Context) error {
	for i := range s.dist {
		if i&1023 == 0 {
			if err := ctx.Err(); err != nil {
				return err
			}
		}
		s.sigma[i] = 0
		s.dist[i] = -1
		s.delta[i] = 0
		s.preds[i] = s.preds[i][:0]
	}
	s.queue = s.queue[:0]
	s.order = s.order[:0]
	return ctx.Err()
}

// The graph is unweighted, so a plain BFS yields shortest paths. Node IDs have
// already been lowered to dense integer slots by ComputeBetweenness; no maps
// are allocated in the per-pivot loop.
func brandesAccumulateContext(ctx context.Context, src int, adjacency [][]int, score []float64, scratch *brandesScratch) error {
	if err := scratch.resetContext(ctx); err != nil {
		return err
	}
	scratch.sigma[src] = 1
	scratch.dist[src] = 0
	scratch.queue = append(scratch.queue, src)

	for head := 0; head < len(scratch.queue); head++ {
		if head&255 == 0 {
			if err := ctx.Err(); err != nil {
				return err
			}
		}
		v := scratch.queue[head]
		scratch.order = append(scratch.order, v)
		for edge, w := range adjacency[v] {
			if edge&1023 == 1023 {
				if err := ctx.Err(); err != nil {
					return err
				}
			}
			if scratch.dist[w] < 0 {
				scratch.dist[w] = scratch.dist[v] + 1
				scratch.queue = append(scratch.queue, w)
			}
			if scratch.dist[w] == scratch.dist[v]+1 {
				scratch.sigma[w] += scratch.sigma[v]
				scratch.preds[w] = append(scratch.preds[w], v)
			}
		}
	}

	for i := len(scratch.order) - 1; i >= 0; i-- {
		if i&255 == 0 {
			if err := ctx.Err(); err != nil {
				return err
			}
		}
		w := scratch.order[i]
		if scratch.sigma[w] != 0 {
			for edge, v := range scratch.preds[w] {
				if edge&1023 == 1023 {
					if err := ctx.Err(); err != nil {
						return err
					}
				}
				scratch.delta[v] += (scratch.sigma[v] / scratch.sigma[w]) * (1 + scratch.delta[w])
			}
		}
		if w != src {
			score[w] += scratch.delta[w]
		}
	}
	return ctx.Err()
}
