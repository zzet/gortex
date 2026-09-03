package analysis

import (
	"sort"

	"github.com/zzet/gortex/internal/graph"
)

// KCoreHit is one row of the k-core decomposition output: a node
// plus its k-degree (the largest k for which it stays in the
// k-core after iterative degree-< k pruning). High k-degree
// signals a node sits inside a densely connected core; a chain of
// leaves all have k-degree 1, a triangle has k-degree 2, a
// 4-clique has k-degree 3.
type KCoreHit struct {
	NodeID  string
	KDegree int
}

// KCoreOptions filters the working set. Empty NodeKinds /
// EdgeKinds means "all kinds". Edges are treated as undirected
// (k-core is defined on undirected graphs).
type KCoreOptions struct {
	NodeKinds []graph.NodeKind
	EdgeKinds []graph.EdgeKind
}

// ComputeKCore returns the k-core decomposition of g. Classic
// algorithm — Batagelj & Zaversnik 2003, O(V + E):
//
//  1. compute every node's undirected degree
//  2. process nodes in degree-ascending order
//  3. when a node is removed, decrement its still-present
//     neighbours' degrees so they can be picked up at the right
//     level
//
// Used as the fallback when the backing graph.Store does not
// implement graph.KCorer.
func ComputeKCore(g graph.Reader, opts KCoreOptions) []KCoreHit {
	if g == nil {
		return nil
	}
	nodeAllow := makeComponentKindAllow(opts.NodeKinds)
	edgeAllow := makeComponentEdgeAllow(opts.EdgeKinds)

	// Dense index over allowed nodes.
	nodes := g.AllNodes()
	idx := make(map[string]int, len(nodes))
	dense := make([]string, 0, len(nodes))
	for _, n := range nodes {
		if n == nil || !nodeAllow(n.Kind) {
			continue
		}
		idx[n.ID] = len(dense)
		dense = append(dense, n.ID)
	}
	if len(dense) == 0 {
		return nil
	}

	// Undirected adjacency; dedupe self-loops + parallel edges.
	type edge struct{ a, b int }
	seenEdge := make(map[edge]bool)
	adj := make([][]int, len(dense))
	for _, e := range g.AllEdges() {
		if e == nil || !edgeAllow(e.Kind) {
			continue
		}
		i, ok1 := idx[e.From]
		j, ok2 := idx[e.To]
		if !ok1 || !ok2 || i == j {
			continue
		}
		key := edge{i, j}
		if i > j {
			key = edge{j, i}
		}
		if seenEdge[key] {
			continue
		}
		seenEdge[key] = true
		adj[i] = append(adj[i], j)
		adj[j] = append(adj[j], i)
	}

	n := len(dense)
	degree := make([]int, n)
	maxDeg := 0
	for i := range dense {
		degree[i] = len(adj[i])
		if degree[i] > maxDeg {
			maxDeg = degree[i]
		}
	}

	// Bucket sort by degree (Batagelj & Zaversnik). bucket[d]
	// holds dense-indices currently at degree d; pos[v] is v's
	// position in its bucket; vertOrder is the global processing
	// order populated as we drain the buckets.
	bucket := make([][]int, maxDeg+1)
	pos := make([]int, n)
	for v, d := range degree {
		pos[v] = len(bucket[d])
		bucket[d] = append(bucket[d], v)
	}

	kdeg := make([]int, n)
	processed := make([]bool, n)
	for d := 0; d <= maxDeg; d++ {
		for len(bucket[d]) > 0 {
			// Pop the back of bucket[d] (O(1)).
			v := bucket[d][len(bucket[d])-1]
			bucket[d] = bucket[d][:len(bucket[d])-1]
			if processed[v] {
				continue
			}
			processed[v] = true
			kdeg[v] = d
			for _, w := range adj[v] {
				if processed[w] {
					continue
				}
				if degree[w] > d {
					// Move w one bucket down.
					old := degree[w]
					// O(1) removal: swap with the back element
					// of the old bucket and adjust its pos.
					i := pos[w]
					last := len(bucket[old]) - 1
					if i != last {
						other := bucket[old][last]
						bucket[old][i] = other
						pos[other] = i
					}
					bucket[old] = bucket[old][:last]
					degree[w] = old - 1
					pos[w] = len(bucket[degree[w]])
					bucket[degree[w]] = append(bucket[degree[w]], w)
				}
			}
		}
	}

	out := make([]KCoreHit, 0, n)
	for v, id := range dense {
		out = append(out, KCoreHit{NodeID: id, KDegree: kdeg[v]})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].KDegree != out[j].KDegree {
			return out[i].KDegree > out[j].KDegree
		}
		return out[i].NodeID < out[j].NodeID
	})
	return out
}
