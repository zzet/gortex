package analysis

import (
	"sort"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graphpath"
)

// edgePair identifies a directed edge between two nodes.
type edgePair struct{ from, to string }

// Cycle represents a detected dependency cycle in the graph.
type Cycle struct {
	Path     []string `json:"path"`     // ordered symbol IDs forming the cycle
	Kind     string   `json:"kind"`     // "import-cycle", "call-cycle", "cross-community-cycle"
	Severity int      `json:"severity"` // 3=import, 2=cross-community, 1=call
}

// DetectCycles finds all dependency cycles in the graph using Tarjan's SCC algorithm.
// If scope is non-empty, only nodes whose FilePath starts with scope are considered.
// Cycles are classified by edge type and community membership, then sorted by severity descending.
func DetectCycles(g graph.Reader, communities *CommunityResult, scope string) []Cycle {
	nodes := g.AllNodes()

	// Build set of in-scope node IDs
	inScope := make(map[string]bool, len(nodes))
	for _, n := range nodes {
		if n.Kind == graph.KindFile || n.Kind == graph.KindImport {
			continue
		}
		if !graphpath.HasPrefix(n.FilePath, scope) {
			continue
		}
		inScope[n.ID] = true
	}

	// Build adjacency list and track edge kinds between pairs.
	//
	// Edge collection streams only EdgeImports + EdgeCalls via
	// EdgesByKind (two MATCH (...)-[e:Edge {kind: $kind}]->(...) on
	// disk backends) instead of materialising every edge in the graph
	// just to filter for two kinds -- ~500k edge rows over cgo dropped
	// to the import-and-call subset (a few tens of thousands on the
	// gortex workspace).
	adj := make(map[string][]string)
	edgeKinds := make(map[edgePair][]graph.EdgeKind)

	collect := func(kind graph.EdgeKind) {
		for e := range g.EdgesByKind(kind) {
			if e == nil {
				continue
			}
			if !inScope[e.From] || !inScope[e.To] {
				continue
			}
			pair := edgePair{e.From, e.To}
			// Avoid duplicate adjacency entries
			if _, exists := edgeKinds[pair]; !exists {
				adj[e.From] = append(adj[e.From], e.To)
			}
			edgeKinds[pair] = append(edgeKinds[pair], kind)
		}
	}
	collect(graph.EdgeImports)
	collect(graph.EdgeCalls)

	// Run Tarjan's SCC
	sccs := tarjanSCC(inScope, adj)

	// Convert SCCs to cycles
	var cycles []Cycle
	for _, scc := range sccs {
		if len(scc) < 2 {
			continue
		}

		// Order the cycle path by following edges within the SCC
		path := orderCyclePath(scc, adj)

		// Classify the cycle
		kind, severity := classifyCycle(path, edgeKinds, communities)

		cycles = append(cycles, Cycle{
			Path:     path,
			Kind:     kind,
			Severity: severity,
		})
	}

	// Sort by severity descending
	sort.Slice(cycles, func(i, j int) bool {
		return cycles[i].Severity > cycles[j].Severity
	})

	return cycles
}

// WouldCreateCycle checks if adding an edge from fromID to toID would create a cycle.
// It performs DFS from toID to see if fromID is reachable. If so, adding fromID→toID
// would close a cycle. Returns the cycle path from toID to fromID when found.
func WouldCreateCycle(g graph.Reader, fromID, toID string) (bool, []string) {
	edges := g.AllEdges()

	// Build adjacency from calls and imports edges
	adj := make(map[string][]string)
	seen := make(map[string]map[string]bool)
	for _, e := range edges {
		if e.Kind != graph.EdgeImports && e.Kind != graph.EdgeCalls {
			continue
		}
		if seen[e.From] == nil {
			seen[e.From] = make(map[string]bool)
		}
		if !seen[e.From][e.To] {
			seen[e.From][e.To] = true
			adj[e.From] = append(adj[e.From], e.To)
		}
	}

	// DFS from toID looking for fromID
	visited := make(map[string]bool)
	parent := make(map[string]string)
	found := false

	var dfs func(node string)
	dfs = func(node string) {
		if found {
			return
		}
		if node == fromID {
			found = true
			return
		}
		visited[node] = true
		for _, next := range adj[node] {
			if found {
				return
			}
			if !visited[next] {
				parent[next] = node
				dfs(next)
			}
		}
	}

	parent[toID] = ""
	dfs(toID)

	if !found {
		return false, nil
	}

	// Reconstruct path from toID to fromID
	var path []string
	current := fromID
	for current != toID {
		path = append(path, current)
		current = parent[current]
	}
	path = append(path, toID)

	// Reverse to get toID → ... → fromID order
	for i, j := 0, len(path)-1; i < j; i, j = i+1, j-1 {
		path[i], path[j] = path[j], path[i]
	}

	return true, path
}

// tarjanSCC runs Tarjan's strongly connected components algorithm.
// Returns a list of SCCs, each being a slice of node IDs.
func tarjanSCC(nodeSet map[string]bool, adj map[string][]string) [][]string {
	index := 0
	stack := make([]string, 0)
	onStack := make(map[string]bool)
	indices := make(map[string]int)
	lowlinks := make(map[string]int)
	defined := make(map[string]bool)
	var sccs [][]string

	var strongConnect func(v string)
	strongConnect = func(v string) {
		indices[v] = index
		lowlinks[v] = index
		index++
		defined[v] = true
		stack = append(stack, v)
		onStack[v] = true

		for _, w := range adj[v] {
			if !nodeSet[w] {
				continue
			}
			if !defined[w] {
				strongConnect(w)
				if lowlinks[w] < lowlinks[v] {
					lowlinks[v] = lowlinks[w]
				}
			} else if onStack[w] {
				if indices[w] < lowlinks[v] {
					lowlinks[v] = indices[w]
				}
			}
		}

		// If v is a root node, pop the SCC
		if lowlinks[v] == indices[v] {
			var scc []string
			for {
				w := stack[len(stack)-1]
				stack = stack[:len(stack)-1]
				onStack[w] = false
				scc = append(scc, w)
				if w == v {
					break
				}
			}
			sccs = append(sccs, scc)
		}
	}

	for id := range nodeSet {
		if !defined[id] {
			strongConnect(id)
		}
	}

	return sccs
}

// orderCyclePath finds a valid cycle within the SCC members by following edges.
// The returned path guarantees that for every consecutive pair (path[i], path[i+1])
// and the closing pair (path[last], path[0]), a directed edge exists in adj.
// Note: the path may not include all SCC members if no Hamiltonian cycle exists.
func orderCyclePath(scc []string, adj map[string][]string) []string {
	sccSet := make(map[string]bool, len(scc))
	for _, id := range scc {
		sccSet[id] = true
	}

	// Find a cycle using DFS from the first node.
	// In an SCC with size >= 2, there is always at least one cycle.
	start := scc[0]
	visited := make(map[string]bool)
	parent := make(map[string]string)

	var cyclePath []string
	var dfs func(node string) bool
	dfs = func(node string) bool {
		visited[node] = true
		for _, next := range adj[node] {
			if !sccSet[next] {
				continue
			}
			if next == start && node != start {
				// Found a cycle back to start — reconstruct
				cyclePath = []string{start}
				cur := node
				var stack []string
				for cur != start {
					stack = append(stack, cur)
					cur = parent[cur]
				}
				// Reverse stack to get start -> ... -> node order
				for i := len(stack) - 1; i >= 0; i-- {
					cyclePath = append(cyclePath, stack[i])
				}
				return true
			}
			if !visited[next] {
				parent[next] = node
				if dfs(next) {
					return true
				}
			}
		}
		return false
	}

	parent[start] = ""
	dfs(start)

	if len(cyclePath) >= 2 {
		return cyclePath
	}

	// Fallback: return the SCC members in original order (should not happen for valid SCC >= 2)
	return scc
}

// classifyCycle determines the kind and severity of a cycle based on edge types
// and community membership.
func classifyCycle(path []string, edgeKinds map[edgePair][]graph.EdgeKind, communities *CommunityResult) (string, int) {
	// Check if any edge in the cycle is an import edge
	hasImport := false
	for i := 0; i < len(path); i++ {
		from := path[i]
		to := path[(i+1)%len(path)]
		pair := edgePair{from, to}
		for _, kind := range edgeKinds[pair] {
			if kind == graph.EdgeImports {
				hasImport = true
				break
			}
		}
		if hasImport {
			break
		}
	}

	if hasImport {
		return "import-cycle", 3
	}

	// Check if cycle spans multiple communities
	if communities != nil && communities.NodeToComm != nil {
		communitySet := make(map[string]bool)
		for _, id := range path {
			if comm, ok := communities.NodeToComm[id]; ok {
				communitySet[comm] = true
			}
		}
		if len(communitySet) > 1 {
			return "cross-community-cycle", 2
		}
	}

	return "call-cycle", 1
}
