package analysis

import (
	"fmt"
	"math"
	"sort"

	"github.com/zzet/gortex/internal/graph"
)

// Spectral clustering tuning.
const (
	// spectralMinSplitSize — connected node sets at or below this are
	// emitted as a cluster rather than bisected further.
	spectralMinSplitSize = 16
	// spectralPowerIters — shifted power-iteration steps used to
	// approximate the Fiedler vector. The ranking (sign pattern)
	// stabilises well before this bound on real call graphs.
	spectralPowerIters = 150
	// spectralMinCluster — clusters smaller than this are dropped, in
	// step with the Louvain/Leiden detectors' singleton handling.
	spectralMinCluster = 2
)

// SpectralClusters partitions the call / reference graph by recursive
// spectral bisection: each cut splits a connected node set along the
// sign of its Fiedler vector — the eigenvector of the graph
// Laplacian's second-smallest eigenvalue — the classic spectral
// partitioning step. It is offered as an alternative to the
// modularity-driven Louvain / Leiden detectors; spectral cuts pair
// better with embedding-similarity edges, where modularity's
// resolution limit blurs cluster boundaries.
//
// The result has the same shape as DetectCommunities so analyze
// kind=clusters can swap algorithms transparently.
func SpectralClusters(g graph.Reader) *CommunityResult {
	nodes := g.AllNodes()
	edges := g.AllEdges()

	symbolNodes := make(map[string]bool)
	for _, n := range nodes {
		if n.Kind != graph.KindFile && n.Kind != graph.KindImport {
			symbolNodes[n.ID] = true
		}
	}

	// Undirected weighted adjacency — same construction the Louvain
	// detector uses, so the two algorithms cluster the same graph.
	type edgeKey struct{ a, b string }
	weights := make(map[edgeKey]float64)
	for _, e := range edges {
		if !symbolNodes[e.From] || !symbolNodes[e.To] || e.From == e.To {
			continue
		}
		w := edgeWeight(e.Kind)
		if w == 0 {
			continue
		}
		weights[edgeKey{e.From, e.To}] += w
		weights[edgeKey{e.To, e.From}] += w
	}
	neighbors := make(map[string]map[string]float64)
	for k, w := range weights {
		if neighbors[k.a] == nil {
			neighbors[k.a] = make(map[string]float64)
		}
		neighbors[k.a][k.b] = w
	}
	if len(neighbors) == 0 {
		return &CommunityResult{NodeToComm: make(map[string]string)}
	}

	nodeMap := make(map[string]*graph.Node, len(nodes))
	for _, n := range nodes {
		nodeMap[n.ID] = n
	}

	// Recursively bisect every connected component.
	all := make([]string, 0, len(neighbors))
	for id := range neighbors {
		all = append(all, id)
	}
	sort.Strings(all)
	clusters := spectralBisect(all, neighbors)

	// Order clusters deterministically by their smallest member.
	sort.Slice(clusters, func(i, j int) bool {
		return minMember(clusters[i]) < minMember(clusters[j])
	})

	result := &CommunityResult{NodeToComm: make(map[string]string)}
	idx := 0
	for _, members := range clusters {
		if len(members) < spectralMinCluster {
			continue
		}
		sort.Strings(members)
		id := fmt.Sprintf("community-%d", idx)
		idx++

		fileSet := make(map[string]bool)
		for _, mid := range members {
			if n := nodeMap[mid]; n != nil {
				fileSet[n.FilePath] = true
			}
		}
		files := make([]string, 0, len(fileSet))
		for f := range fileSet {
			files = append(files, f)
		}
		sort.Strings(files)

		for _, mid := range members {
			result.NodeToComm[mid] = id
		}
		result.Communities = append(result.Communities, Community{
			ID:       id,
			Label:    inferCommunityLabel(members, nodeMap, files),
			Members:  members,
			Files:    files,
			Size:     len(members),
			Cohesion: computeCohesion(members, neighbors),
			Hub:      findHub(members, nodeMap, neighbors),
		})
	}

	disambiguateLabels(result.Communities)
	assignDirectoryParents(result.Communities)
	sort.Slice(result.Communities, func(i, j int) bool {
		if result.Communities[i].Size != result.Communities[j].Size {
			return result.Communities[i].Size > result.Communities[j].Size
		}
		return result.Communities[i].ID < result.Communities[j].ID
	})
	result.Modularity = graphModularity(neighbors, result.NodeToComm)
	return result
}

// spectralBisect recursively partitions a node set. A set that splits
// into multiple connected components is divided along them first;
// a single connected component larger than the floor is cut by its
// Fiedler vector; everything else is emitted as a cluster.
func spectralBisect(members []string, neighbors map[string]map[string]float64) [][]string {
	comps := connectedComponentsWithin(members, neighbors)
	if len(comps) > 1 {
		var out [][]string
		for _, c := range comps {
			out = append(out, spectralBisect(c, neighbors)...)
		}
		return out
	}
	if len(members) <= spectralMinSplitSize {
		return [][]string{members}
	}
	left, right := fiedlerSplit(members, neighbors)
	if len(left) == 0 || len(right) == 0 {
		// The Fiedler vector did not separate the set — emit as-is
		// rather than recursing forever.
		return [][]string{members}
	}
	out := spectralBisect(left, neighbors)
	return append(out, spectralBisect(right, neighbors)...)
}

// connectedComponentsWithin returns the connected components of the
// subgraph induced by members (edges to nodes outside the set are
// ignored).
func connectedComponentsWithin(members []string, neighbors map[string]map[string]float64) [][]string {
	inSet := make(map[string]bool, len(members))
	for _, m := range members {
		inSet[m] = true
	}
	visited := make(map[string]bool, len(members))
	var comps [][]string
	for _, start := range members {
		if visited[start] {
			continue
		}
		var comp []string
		queue := []string{start}
		visited[start] = true
		for len(queue) > 0 {
			cur := queue[0]
			queue = queue[1:]
			comp = append(comp, cur)
			for nb := range neighbors[cur] {
				if inSet[nb] && !visited[nb] {
					visited[nb] = true
					queue = append(queue, nb)
				}
			}
		}
		comps = append(comps, comp)
	}
	return comps
}

// fiedlerSplit approximates the Fiedler vector of the subgraph induced
// by members via shifted power iteration on (c·I − L), deflating the
// constant eigenvector each step, then splits the set by the vector's
// sign. The members slice must be a single connected component.
func fiedlerSplit(members []string, neighbors map[string]map[string]float64) (left, right []string) {
	n := len(members)
	index := make(map[string]int, n)
	for i, id := range members {
		index[id] = i
	}

	// Local degree and the Laplacian shift c = maxDegree·2 + 1, which
	// keeps c·I − L positive so the dominant eigenvector of the
	// shifted matrix is the Fiedler vector of L.
	degree := make([]float64, n)
	var maxDeg float64
	for i, id := range members {
		for nb, w := range neighbors[id] {
			if _, ok := index[nb]; ok {
				degree[i] += w
			}
		}
		if degree[i] > maxDeg {
			maxDeg = degree[i]
		}
	}
	shift := maxDeg*2 + 1

	// Deterministic, non-constant seed vector.
	v := make([]float64, n)
	for i := range v {
		v[i] = math.Sin(float64(i + 1))
	}
	deflateAndNormalize(v)

	next := make([]float64, n)
	for iter := 0; iter < spectralPowerIters; iter++ {
		for i, id := range members {
			// (L v)[i] = degree[i]·v[i] − Σ_j A_ij v[j]
			lv := degree[i] * v[i]
			for nb, w := range neighbors[id] {
				if j, ok := index[nb]; ok {
					lv -= w * v[j]
				}
			}
			// w = (c·I − L) v
			next[i] = shift*v[i] - lv
		}
		copy(v, next)
		deflateAndNormalize(v)
	}

	// Sign split. A degenerate all-one-sign vector falls back to a
	// median split so the recursion still makes progress.
	threshold := 0.0
	if allSameSign(v) {
		threshold = median(v)
	}
	for i, id := range members {
		if v[i] >= threshold {
			left = append(left, id)
		} else {
			right = append(right, id)
		}
	}
	return left, right
}

// deflateAndNormalize projects v onto the subspace orthogonal to the
// all-ones vector (removing L's trivial zero-eigenvalue component),
// then scales it to unit length.
func deflateAndNormalize(v []float64) {
	if len(v) == 0 {
		return
	}
	var mean float64
	for _, x := range v {
		mean += x
	}
	mean /= float64(len(v))
	var norm float64
	for i := range v {
		v[i] -= mean
		norm += v[i] * v[i]
	}
	norm = math.Sqrt(norm)
	if norm < 1e-12 {
		// Collapsed to the constant vector — reseed.
		for i := range v {
			v[i] = math.Sin(float64(i)*2 + 1)
		}
		deflateAndNormalize(v)
		return
	}
	for i := range v {
		v[i] /= norm
	}
}

func allSameSign(v []float64) bool {
	pos, neg := false, false
	for _, x := range v {
		if x >= 0 {
			pos = true
		} else {
			neg = true
		}
	}
	return !pos || !neg
}

func median(v []float64) float64 {
	if len(v) == 0 {
		return 0
	}
	cp := append([]float64(nil), v...)
	sort.Float64s(cp)
	return cp[len(cp)/2]
}

func minMember(ids []string) string {
	if len(ids) == 0 {
		return ""
	}
	m := ids[0]
	for _, id := range ids[1:] {
		if id < m {
			m = id
		}
	}
	return m
}

// graphModularity scores a partition's modularity on the undirected
// weighted adjacency — Q = (1/2m) Σ_ij [A_ij − k_i k_j/2m] δ(c_i,c_j).
func graphModularity(neighbors map[string]map[string]float64, nodeToComm map[string]string) float64 {
	degree := make(map[string]float64, len(neighbors))
	var m2 float64
	for id, nbrs := range neighbors {
		for _, w := range nbrs {
			degree[id] += w
			m2 += w
		}
	}
	if m2 == 0 {
		return 0
	}
	var q float64
	for id, nbrs := range neighbors {
		ci, ok := nodeToComm[id]
		if !ok {
			continue
		}
		for j, w := range nbrs {
			if nodeToComm[j] == ci {
				q += w - degree[id]*degree[j]/m2
			}
		}
	}
	return q / m2
}
