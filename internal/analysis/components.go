package analysis

import (
	"sort"

	"github.com/zzet/gortex/internal/graph"
)

// ComponentResult is one connected component returned by
// ComputeWCC / ComputeSCC. Members are sorted ascending so the
// output is deterministic across runs.
type ComponentResult struct {
	ID      int      `json:"id"`
	Members []string `json:"members"`
	Size    int      `json:"size"`
}

// ComponentOptions filters the working set the algorithm runs
// against. Empty NodeKinds / EdgeKinds means "all kinds".
type ComponentOptions struct {
	NodeKinds []graph.NodeKind
	EdgeKinds []graph.EdgeKind
	// MinSize trims trivial singleton components from the
	// response — common for SCC where every non-cyclic symbol
	// is its own 1-element SCC.
	MinSize int
}

// ComputeWCC returns the weakly connected components of g — pairs
// of nodes reachable from each other when every edge is treated
// as undirected. Components are sorted by size descending; ties
// broken by member ID for determinism.
//
// O(V + E). Used as the fallback when the backing graph.Store
// does not implement graph.ComponentFinder.
func ComputeWCC(g graph.Reader, opts ComponentOptions) []ComponentResult {
	if g == nil {
		return nil
	}
	nodeAllow := makeComponentKindAllow(opts.NodeKinds)
	edgeAllow := makeComponentEdgeAllow(opts.EdgeKinds)

	// Build a dense int index over allowed nodes.
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

	// Undirected adjacency over allowed edges.
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
		adj[i] = append(adj[i], j)
		adj[j] = append(adj[j], i)
	}

	// Union-find equivalence: BFS from each unseen node, mark
	// every reachable node with the same component label.
	comp := make([]int, len(dense))
	for i := range comp {
		comp[i] = -1
	}
	next := 0
	queue := make([]int, 0, 64)
	for i := range dense {
		if comp[i] != -1 {
			continue
		}
		label := next
		next++
		comp[i] = label
		queue = append(queue[:0], i)
		for len(queue) > 0 {
			cur := queue[0]
			queue = queue[1:]
			for _, nb := range adj[cur] {
				if comp[nb] == -1 {
					comp[nb] = label
					queue = append(queue, nb)
				}
			}
		}
	}

	return collectComponents(dense, comp, opts.MinSize)
}

// ComputeSCC returns the strongly connected components of g —
// pairs of nodes mutually reachable along directed edges. Uses
// an iterative Tarjan's algorithm to avoid blowing the recursion
// stack on a deep call graph. O(V + E).
func ComputeSCC(g graph.Reader, opts ComponentOptions) []ComponentResult {
	if g == nil {
		return nil
	}
	nodeAllow := makeComponentKindAllow(opts.NodeKinds)
	edgeAllow := makeComponentEdgeAllow(opts.EdgeKinds)

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

	// Directed adjacency. Only out-edges — SCC walks one way.
	adj := make([][]int, len(dense))
	for _, e := range g.AllEdges() {
		if e == nil || !edgeAllow(e.Kind) {
			continue
		}
		i, ok1 := idx[e.From]
		j, ok2 := idx[e.To]
		if !ok1 || !ok2 {
			continue
		}
		adj[i] = append(adj[i], j)
	}

	// Iterative Tarjan. State arrays sized to the dense node
	// count; the call stack is replaced by an explicit (node,
	// neighbour-iteration-index) stack.
	n := len(dense)
	const undefined = -1
	idxArr := make([]int, n)
	lowlink := make([]int, n)
	onStack := make([]bool, n)
	for i := range idxArr {
		idxArr[i] = undefined
	}
	stack := make([]int, 0, n)
	type frame struct {
		v  int
		ni int // next-neighbour index to visit
	}
	work := make([]frame, 0, n)

	var index int
	comp := make([]int, n)
	for i := range comp {
		comp[i] = -1
	}
	nextComp := 0

	for start := 0; start < n; start++ {
		if idxArr[start] != undefined {
			continue
		}
		// Initialise the explicit DFS for this root.
		idxArr[start] = index
		lowlink[start] = index
		index++
		stack = append(stack, start)
		onStack[start] = true
		work = append(work, frame{v: start, ni: 0})

		for len(work) > 0 {
			top := &work[len(work)-1]
			v := top.v
			neighbors := adj[v]
			if top.ni < len(neighbors) {
				w := neighbors[top.ni]
				top.ni++
				if idxArr[w] == undefined {
					// Descend into w.
					idxArr[w] = index
					lowlink[w] = index
					index++
					stack = append(stack, w)
					onStack[w] = true
					work = append(work, frame{v: w, ni: 0})
				} else if onStack[w] {
					if idxArr[w] < lowlink[v] {
						lowlink[v] = idxArr[w]
					}
				}
				continue
			}
			// All neighbours consumed; pop the frame and propagate
			// the lowlink upward.
			work = work[:len(work)-1]
			if len(work) > 0 {
				parent := &work[len(work)-1]
				if lowlink[v] < lowlink[parent.v] {
					lowlink[parent.v] = lowlink[v]
				}
			}
			// Emit an SCC if v is its lowlink root.
			if lowlink[v] == idxArr[v] {
				label := nextComp
				nextComp++
				for {
					w := stack[len(stack)-1]
					stack = stack[:len(stack)-1]
					onStack[w] = false
					comp[w] = label
					if w == v {
						break
					}
				}
			}
		}
	}

	return collectComponents(dense, comp, opts.MinSize)
}

// collectComponents groups dense node IDs by component label,
// applies MinSize, sorts members for determinism, and returns
// the slice ordered by size descending.
func collectComponents(dense []string, comp []int, minSize int) []ComponentResult {
	groups := make(map[int][]string)
	for i, id := range dense {
		c := comp[i]
		if c < 0 {
			continue
		}
		groups[c] = append(groups[c], id)
	}
	out := make([]ComponentResult, 0, len(groups))
	for c, members := range groups {
		if minSize > 0 && len(members) < minSize {
			continue
		}
		sort.Strings(members)
		out = append(out, ComponentResult{ID: c, Members: members, Size: len(members)})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Size != out[j].Size {
			return out[i].Size > out[j].Size
		}
		if len(out[i].Members) > 0 && len(out[j].Members) > 0 {
			return out[i].Members[0] < out[j].Members[0]
		}
		return out[i].ID < out[j].ID
	})
	// Renumber sequentially so the output IDs are 0..N-1 in
	// size-descending order. Stable for snapshot tests.
	for i := range out {
		out[i].ID = i
	}
	return out
}

func makeComponentKindAllow(kinds []graph.NodeKind) func(graph.NodeKind) bool {
	if len(kinds) == 0 {
		return func(graph.NodeKind) bool { return true }
	}
	set := make(map[graph.NodeKind]struct{}, len(kinds))
	for _, k := range kinds {
		set[k] = struct{}{}
	}
	return func(k graph.NodeKind) bool {
		_, ok := set[k]
		return ok
	}
}

func makeComponentEdgeAllow(kinds []graph.EdgeKind) func(graph.EdgeKind) bool {
	if len(kinds) == 0 {
		return func(graph.EdgeKind) bool { return true }
	}
	set := make(map[graph.EdgeKind]struct{}, len(kinds))
	for _, k := range kinds {
		set[k] = struct{}{}
	}
	return func(k graph.EdgeKind) bool {
		_, ok := set[k]
		return ok
	}
}
