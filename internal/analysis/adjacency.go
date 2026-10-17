package analysis

import (
	"sort"

	"github.com/zzet/gortex/internal/graph"
)

// AdjacencySnapshot is a compact, immutable CSR-style view of the
// call / reference graph, built once per analysis pass and reused by
// seeded random-walk queries so they never re-scan AllNodes / AllEdges.
//
// Only EdgeCalls and EdgeReferences participate — the same edge set
// ComputePageRank walks — and each edge rides its graph.ProvenanceWeight
// so a seeded walk attenuates over-represented LSP-dispatch fan-outs
// identically to the global PageRank.
//
// Layout (forward adjacency, From -> To):
//
//   - ids[i]            the node ID at dense index i
//   - offsets[i]..[i+1] the slice of out-neighbours of node i
//   - neighbors[k]      dense index of the k-th out-neighbour
//   - weights[k]        provenance weight of the edge to neighbors[k]
//   - outWeight[i]      sum of weights of node i's out-edges
//
// The snapshot is read-only after construction; PersonalizedPageRank
// allocates only its own score vectors, so concurrent walks over one
// snapshot are safe without locking.
type AdjacencySnapshot struct {
	ids       []string
	index     map[string]int
	offsets   []int32
	neighbors []int32
	weights   []float64
	outWeight []float64

	// pkgRoots maps a package directory (the dir of a node's file path,
	// ≈ a Go package) to a content hash of that package's contribution
	// to the walk: every member node's stable ID plus its out-edges
	// (neighbour IDs + weights). The hash is index-shift invariant — it
	// uses string IDs, not dense indices — so a package whose subgraph
	// did not change keeps the same root even when nodes are added or
	// removed in OTHER packages. This is the per-package Merkle root the
	// walk cache keys on, so only walks touching a changed package miss.
	// Empty when the snapshot is empty.
	pkgRoots map[string]uint64
}

// NodeCount returns the number of nodes in the snapshot.
func (a *AdjacencySnapshot) NodeCount() int {
	if a == nil {
		return 0
	}
	return len(a.ids)
}

// EdgeCount returns the number of directed call / reference edges
// captured in the snapshot.
func (a *AdjacencySnapshot) EdgeCount() int {
	if a == nil {
		return 0
	}
	return len(a.neighbors)
}

// BuildAdjacencySnapshot constructs the CSR adjacency over the call /
// reference graph. Nodes are densely indexed in sorted ID order so the
// snapshot — and therefore every seeded walk over it — is deterministic
// regardless of the backend's node / edge enumeration order. An edge
// whose endpoint is not a real graph node (an unresolved or dangling
// target) is skipped so the dense index stays consistent.
func BuildAdjacencySnapshot(g graph.Store) *AdjacencySnapshot {
	return BuildAdjacencySnapshotPaced(g, nil)
}

// BuildAdjacencySnapshotPaced is BuildAdjacencySnapshot with a cooperative
// scheduling point in every hot loop (see Pace). A nil Pace never parks.
func BuildAdjacencySnapshotPaced(g graph.Store, pace *Pace) *AdjacencySnapshot {
	snap := &AdjacencySnapshot{index: map[string]int{}}
	if g == nil {
		return snap
	}

	ids := make([]string, 0, g.NodeCount())
	for n := range graph.NodesLightSeq(g) {
		pace.Tick()
		if n == nil || n.ID == "" {
			continue
		}
		ids = append(ids, n.ID)
	}
	if len(ids) == 0 {
		return snap
	}
	sort.Strings(ids)
	index := make(map[string]int, len(ids))
	for i, id := range ids {
		index[id] = i
	}

	// First pass: bucket out-edges per source so the CSR offsets can be
	// laid out contiguously. Only call / reference edges with both
	// endpoints in the dense index participate.
	type link struct {
		to int
		w  float64
	}
	adj := make([][]link, len(ids))
	// Meta-less kind-scoped scan (see LightEdgeScanner): the CSR build reads only
	// e.Kind, endpoints, and graph.ProvenanceWeight.
	for e := range graph.EdgesLightSeq(g, graph.EdgeCalls, graph.EdgeReferences) {
		pace.Tick()
		if e == nil {
			continue
		}
		if e.Kind != graph.EdgeCalls && e.Kind != graph.EdgeReferences {
			continue
		}
		from, ok := index[e.From]
		if !ok {
			continue
		}
		to, ok := index[e.To]
		if !ok {
			continue
		}
		adj[from] = append(adj[from], link{to: to, w: graph.ProvenanceWeight(e)})
	}

	offsets := make([]int32, len(ids)+1)
	var total int
	for i := range adj {
		offsets[i] = int32(total)
		total += len(adj[i])
	}
	offsets[len(ids)] = int32(total)

	neighbors := make([]int32, 0, total)
	weights := make([]float64, 0, total)
	outWeight := make([]float64, len(ids))
	for i := range adj {
		// Sort each node's out-neighbours by dense index so the CSR row
		// order is deterministic (AllEdges order is backend-specific).
		pace.Tick()
		row := adj[i]
		sort.Slice(row, func(a, b int) bool { return row[a].to < row[b].to })
		for _, l := range row {
			neighbors = append(neighbors, int32(l.to))
			weights = append(weights, l.w)
			outWeight[i] += l.w
		}
	}

	snap.ids = ids
	snap.index = index
	snap.offsets = offsets
	snap.neighbors = neighbors
	snap.weights = weights
	snap.outWeight = outWeight
	snap.pkgRoots = computePackageRoots(ids, offsets, neighbors, weights)
	return snap
}

// pprDefaultRestart is the restart probability a seeded walk uses when
// the caller passes a non-positive value. 0.15 mirrors the canonical
// 0.85 PageRank damping (restart = 1 - damping).
const pprDefaultRestart = 0.15

// pprIterations is fixed rather than convergence-tested: the graph is
// small enough that the ranking order stabilises well within this many
// power-iteration steps, and a fixed count keeps the result
// deterministic and the cost bounded at O(iters * edges).
const pprIterations = 40

// PersonalizedPageRank runs a seeded random-walk-with-restart over the
// snapshot. Restart mass returns to the seed set (not uniformly across
// the graph), so the stationary distribution concentrates on nodes that
// are reachable from the seeds along many short, high-provenance paths.
//
// restart is the per-step restart probability; a non-positive value
// uses pprDefaultRestart. Score flows along an edge in proportion to
// its weight / the source's out-weight — identical to ComputePageRank —
// and dangling mass (a node with no out-edges) is returned to the seed
// set so no probability leaks. The result maps node ID to its proximity
// score; an empty map is returned when no seed resolves to a snapshot
// node.
func (a *AdjacencySnapshot) PersonalizedPageRank(seeds []string, restart float64) map[string]float64 {
	return a.PersonalizedPageRankTopK(seeds, restart, 0)
}

// PersonalizedPageRankTopK is PersonalizedPageRank restricted to the k
// nodes with the highest stationary score. The seeded walk concentrates
// almost all of its mass on a small neighbourhood of the seeds, so the
// long tail of near-floor scores carries no usable ranking signal: every
// consumer either looks a candidate's score up by ID (absent → 0, exactly
// as a zero score already is) or max-normalises against the top entry,
// which top-k always retains. Capping the result turns a cached walk from
// a full-graph-sized map (~len(ids) entries) into at most k entries, which
// is what bounds the PPR walk cache's memory. k <= 0 (or k >= the number
// of scored nodes) returns the full dense map, identical to
// PersonalizedPageRank.
func (a *AdjacencySnapshot) PersonalizedPageRankTopK(seeds []string, restart float64, k int) map[string]float64 {
	score := a.personalizedPageRankScores(seeds, restart)
	if score == nil {
		return map[string]float64{}
	}
	if k <= 0 {
		out := make(map[string]float64, len(score))
		for i, v := range score {
			if v != 0 {
				out[a.ids[i]] = v
			}
		}
		return out
	}

	// Gather the non-zero (index, score) pairs, then keep the k largest.
	pairs := make([]pprScore, 0, len(score))
	for i, v := range score {
		if v != 0 {
			pairs = append(pairs, pprScore{idx: i, score: v})
		}
	}
	if k >= len(pairs) {
		out := make(map[string]float64, len(pairs))
		for _, p := range pairs {
			out[a.ids[p.idx]] = p.score
		}
		return out
	}
	// Partial selection by descending score; ties broken by index so the
	// retained set is deterministic across runs.
	sort.Slice(pairs, func(i, j int) bool {
		if pairs[i].score != pairs[j].score {
			return pairs[i].score > pairs[j].score
		}
		return pairs[i].idx < pairs[j].idx
	})
	out := make(map[string]float64, k)
	for _, p := range pairs[:k] {
		out[a.ids[p.idx]] = p.score
	}
	return out
}

// pprScore pairs a snapshot node index with its stationary score for the
// top-k partial selection in PersonalizedPageRankTopK.
type pprScore struct {
	idx   int
	score float64
}

// personalizedPageRankScores runs the seeded random-walk-with-restart and
// returns the raw stationary score array aligned to a.ids (score[i] is the
// proximity of a.ids[i]). It returns nil when the snapshot is empty or no
// seed resolves to a snapshot node — the public wrappers then return an
// empty map.
func (a *AdjacencySnapshot) personalizedPageRankScores(seeds []string, restart float64) []float64 {
	if a == nil || len(a.ids) == 0 {
		return nil
	}
	if restart <= 0 || restart >= 1 {
		restart = pprDefaultRestart
	}

	n := len(a.ids)

	// Restart distribution: uniform over the in-snapshot seeds. A seed
	// absent from the snapshot contributes nothing.
	seedIdx := make([]int, 0, len(seeds))
	seen := make(map[int]bool, len(seeds))
	for _, s := range seeds {
		if i, ok := a.index[s]; ok && !seen[i] {
			seen[i] = true
			seedIdx = append(seedIdx, i)
		}
	}
	if len(seedIdx) == 0 {
		return nil
	}
	restartVec := make([]float64, n)
	seedMass := 1.0 / float64(len(seedIdx))
	for _, i := range seedIdx {
		restartVec[i] = seedMass
	}

	// Initialise the walk at the seed distribution.
	score := make([]float64, n)
	copy(score, restartVec)

	for iter := 0; iter < pprIterations; iter++ {
		next := make([]float64, n)

		// Push each node's score forward along its out-edges, weighted
		// by w/outWeight. Dangling nodes pool their score and return it
		// to the seed set, conserving total mass.
		var dangling float64
		for i := 0; i < n; i++ {
			ow := a.outWeight[i]
			if ow == 0 {
				dangling += score[i]
				continue
			}
			s := score[i]
			if s == 0 {
				continue
			}
			start, end := a.offsets[i], a.offsets[i+1]
			for k := start; k < end; k++ {
				next[a.neighbors[k]] += s * a.weights[k] / ow
			}
		}

		// Combine the walk step, the restart, and the dangling pool.
		// (1-restart) of the walked mass plus restart mass to the seeds;
		// dangling mass also returns to the seeds so it never leaks.
		for i := 0; i < n; i++ {
			next[i] = (1-restart)*next[i] + (restart+(1-restart)*dangling)*restartVec[i]
		}
		score = next
	}

	return score
}
