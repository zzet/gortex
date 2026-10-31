package analysis

import (
	"iter"
	"slices"

	"github.com/zzet/gortex/internal/graph"
)

func deadCodeRepoSet(prefixes []string) map[string]bool {
	if prefixes == nil {
		return nil
	}
	allowed := make(map[string]bool, len(prefixes))
	for _, prefix := range prefixes {
		allowed[prefix] = true
	}
	return allowed
}

func deadCodeRepoAllows(node *graph.Node, allowed map[string]bool) bool {
	return node != nil && (allowed == nil || allowed[node.RepoPrefix])
}

func filterDeadCodeRepos(nodes []*graph.Node, prefixes []string) []*graph.Node {
	if prefixes == nil {
		return nodes
	}
	allowed := deadCodeRepoSet(prefixes)
	var out []*graph.Node
	for _, node := range nodes {
		if deadCodeRepoAllows(node, allowed) {
			out = append(out, node)
		}
	}
	return out
}

func filterDeadCodeNodeScope(nodes []*graph.Node, scope *graph.NodeScope) []*graph.Node {
	if scope == nil {
		return nodes
	}
	var out []*graph.Node
	for _, n := range nodes {
		if n != nil && scope.Allows(n) {
			out = append(out, n)
		}
	}
	return out
}

func deadCodeFileNodes(g graph.Reader, prefixes []string, scope *graph.NodeScope) iter.Seq[*graph.Node] {
	if scope != nil {
		if scoped, ok := g.(graph.NodeScopedDeadCodeReader); ok {
			return scoped.NodesByKindInScope(graph.KindFile, prefixes, *scope)
		}
	}
	if scoped, ok := g.(graph.ScopedDeadCodeReader); ok {
		return func(yield func(*graph.Node) bool) {
			for n := range scoped.NodesByKindInRepos(graph.KindFile, prefixes) {
				if n != nil && (scope == nil || scope.Allows(n)) && !yield(n) {
					return
				}
			}
		}
	}
	allowed := deadCodeRepoSet(prefixes)
	return func(yield func(*graph.Node) bool) {
		for node := range g.NodesByKind(graph.KindFile) {
			if deadCodeRepoAllows(node, allowed) && (scope == nil || scope.Allows(node)) && !yield(node) {
				return
			}
		}
	}
}

// deadCodeMethodEvidence is request-local and bounded by method candidates,
// their receivers and the interfaces those receivers actually implement.
// Every adjacency read stays on g, preserving overlay/authority masks and
// global interface evidence without a whole-scope implements projection.
func deadCodeMethodEvidence(g graph.Reader, candidates []*graph.Node, incoming map[string][]*graph.Edge, opt FindDeadCodeOptions) (map[string]map[string]bool, map[string][]*graph.Edge) {
	var methodIDs []string
	usage := incomingUsageKinds(graph.KindMethod)
	for _, n := range candidates {
		if n == nil || n.Kind != graph.KindMethod || graph.IsStub(n.ID) {
			continue
		}
		if external, _ := n.Meta["external"].(bool); external {
			continue
		}
		if generated, _ := n.Meta["generated"].(bool); generated {
			continue
		}
		used := false
		for _, edge := range incoming[n.ID] {
			if edge != nil && slices.Contains(usage, edge.Kind) {
				used = true
				break
			}
		}
		if !used {
			methodIDs = append(methodIDs, n.ID)
		}
	}
	if len(methodIDs) == 0 {
		return nil, nil
	}
	methodEdges := g.GetOutEdgesByNodeIDs(methodIDs)
	var owners []string
	seen := make(map[string]bool)
	for _, id := range methodIDs {
		for _, edge := range methodEdges[id] {
			if edge != nil && edge.Kind == graph.EdgeMemberOf && !seen[edge.To] {
				seen[edge.To] = true
				owners = append(owners, edge.To)
			}
		}
	}
	if len(owners) == 0 {
		return nil, methodEdges
	}
	if opt.NodeScope != nil || opt.RepoPrefixes != nil {
		nodes := g.GetNodesByIDs(owners)
		allowed := deadCodeRepoSet(opt.RepoPrefixes)
		kept := owners[:0]
		for _, id := range owners {
			n := nodes[id]
			if n != nil && deadCodeRepoAllows(n, allowed) && (opt.NodeScope == nil || opt.NodeScope.Allows(n)) {
				kept = append(kept, id)
			}
		}
		owners = kept
	}
	// Unbound legacy reads intentionally admit a dangling implementing type:
	// its actual member_of/implements edges can still satisfy a method. A
	// scoped read requires concrete ownership metadata above, just as SQL did.
	if len(owners) == 0 {
		return nil, methodEdges
	}
	implements := g.GetOutEdgesByNodeIDs(owners)
	var interfaces []string
	seen = make(map[string]bool)
	for _, id := range owners {
		for _, edge := range implements[id] {
			if edge != nil && edge.Kind == graph.EdgeImplements && !seen[edge.To] {
				seen[edge.To] = true
				interfaces = append(interfaces, edge.To)
			}
		}
	}
	if len(interfaces) == 0 {
		return nil, methodEdges
	}
	nodes := g.GetNodesByIDs(interfaces)
	var rows []graph.IfaceImplementsRow
	for _, id := range owners {
		for _, edge := range implements[id] {
			if edge == nil || edge.Kind != graph.EdgeImplements {
				continue
			}
			iface := nodes[edge.To]
			if iface != nil && iface.Kind == graph.KindInterface && iface.Meta != nil {
				rows = append(rows, graph.IfaceImplementsRow{TypeID: id, IfaceID: iface.ID, IfaceMeta: iface.Meta})
			}
		}
	}
	return buildIfaceRequiredMethodsFromRows(rows), methodEdges
}
