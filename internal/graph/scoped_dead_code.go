package graph

import "iter"

// DeadCodeCandidateExclusions removes only declarations already ineligible for
// dead-code reporting. This is a conservative accelerator: backends may retain
// legacy metadata they cannot prove ineligible without hydration. Callers must
// still check decoded nodes. It never restricts incoming usage evidence.
type DeadCodeCandidateExclusions struct {
	Stubs    bool
	External bool
}

func (e DeadCodeCandidateExclusions) Allows(n *Node) bool {
	if n == nil || (e.Stubs && IsStub(n.ID)) {
		return false
	}
	if e.External {
		external, _ := n.Meta["external"].(bool)
		return !external
	}
	return true
}

// NodeScopedDeadCodeReader intersects exact node metadata scope with explicit
// repository ownership. Usage evidence remains global and reader-visible.
type NodeScopedDeadCodeReader interface {
	DeadCodeCandidatesInScope([]NodeKind, map[NodeKind][]EdgeKind, []string, NodeScope, DeadCodeCandidateExclusions) []*Node
	NodesByKindInScope(NodeKind, []string, NodeScope) iter.Seq[*Node]
}

// FilteredNodeScopedDeadCodeReader lets an adjacency wrapper supply its mask
// without losing the exact workspace/project boundary before usage probes.
type FilteredNodeScopedDeadCodeReader interface {
	DeadCodeCandidatesInScopeFiltered([]NodeKind, map[NodeKind][]EdgeKind, []string, NodeScope, OutlineEdgeFilter, DeadCodeCandidateExclusions) ([]*Node, error)
}

// ScopedDeadCodeReader restricts candidate ownership, never evidence of use:
// incoming edges and implemented interfaces can belong to any repository.
// Nil prefixes mean unscoped; a non-nil empty set matches no nodes. An empty
// ownership prefix is included only when explicitly present in the set.
// Composed readers must preserve their masks instead of forwarding this
// capability to a physical base store.
type ScopedDeadCodeReader interface {
	DeadCodeCandidatesInRepos(kinds []NodeKind, incoming map[NodeKind][]EdgeKind, prefixes []string) []*Node
	IfaceImplementsRowsInRepos(prefixes []string) []IfaceImplementsRow
	NodesByKindInRepos(kind NodeKind, prefixes []string) iter.Seq[*Node]
}

// FilteredScopedDeadCodeReader applies the existing adjacency-exclusion
// contract to scoped candidate and implements reads. It lets an adjacency
// wrapper use indexed queries without exposing physical, unfiltered verdicts.
type FilteredScopedDeadCodeReader interface {
	DeadCodeCandidatesInReposFiltered(kinds []NodeKind, incoming map[NodeKind][]EdgeKind, prefixes []string, filter OutlineEdgeFilter) ([]*Node, error)
	IfaceImplementsRowsInReposFiltered(prefixes []string, filter OutlineEdgeFilter) ([]IfaceImplementsRow, error)
}

var _ ScopedDeadCodeReader = (*Graph)(nil)
var _ FilteredScopedDeadCodeReader = (*Graph)(nil)

func repoPrefixSet(prefixes []string) map[string]bool {
	if prefixes == nil {
		return nil
	}
	allowed := make(map[string]bool, len(prefixes))
	for _, prefix := range prefixes {
		allowed[prefix] = true
	}
	return allowed
}

func (g *Graph) NodesByKindInRepos(kind NodeKind, prefixes []string) iter.Seq[*Node] {
	allowed := repoPrefixSet(prefixes)
	return func(yield func(*Node) bool) {
		for n := range g.NodesByKind(kind) {
			if n != nil && (allowed == nil || allowed[n.RepoPrefix]) && !yield(n) {
				return
			}
		}
	}
}

func (g *Graph) DeadCodeCandidatesInRepos(kinds []NodeKind, incoming map[NodeKind][]EdgeKind, prefixes []string) []*Node {
	return g.deadCodeCandidates(kinds, incoming, prefixes, OutlineEdgeFilter{})
}

func (g *Graph) DeadCodeCandidatesInReposFiltered(kinds []NodeKind, incoming map[NodeKind][]EdgeKind, prefixes []string, filter OutlineEdgeFilter) ([]*Node, error) {
	return g.deadCodeCandidates(kinds, incoming, prefixes, filter), nil
}

func (g *Graph) IfaceImplementsRowsInRepos(prefixes []string) []IfaceImplementsRow {
	if prefixes == nil {
		return g.IfaceImplementsRows()
	}
	return g.ifaceImplementsRowsInRepos(prefixes, OutlineEdgeFilter{})
}

func (g *Graph) IfaceImplementsRowsInReposFiltered(prefixes []string, filter OutlineEdgeFilter) ([]IfaceImplementsRow, error) {
	return g.ifaceImplementsRowsInRepos(prefixes, filter), nil
}

func (g *Graph) ifaceImplementsRowsInRepos(prefixes []string, filter OutlineEdgeFilter) []IfaceImplementsRow {
	allowed := repoPrefixSet(prefixes)
	if allowed != nil && len(allowed) == 0 {
		return nil
	}
	var out []IfaceImplementsRow
	for edge := range g.EdgesByKind(EdgeImplements) {
		if edge == nil {
			continue
		}
		typ := g.GetNode(edge.From)
		if allowed != nil && (typ == nil || !allowed[typ.RepoPrefix]) {
			continue
		}
		iface := g.GetNode(edge.To)
		if iface == nil || iface.Kind != KindInterface || iface.Meta == nil || !filter.Allows(edge, typ, iface) {
			continue
		}
		if _, ok := iface.Meta["methods"]; ok {
			out = append(out, IfaceImplementsRow{TypeID: edge.From, IfaceID: iface.ID, IfaceMeta: iface.Meta})
		}
	}
	return out
}
