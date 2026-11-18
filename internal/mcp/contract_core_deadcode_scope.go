package mcp

import (
	"iter"

	"github.com/zzet/gortex/internal/graph"
)

var _ graph.NodeScopedDeadCodeReader = (*contractCoreEdges)(nil)

func (r *contractCoreEdges) DeadCodeCandidatesInScope(kinds []graph.NodeKind, incoming map[graph.NodeKind][]graph.EdgeKind, prefixes []string, scope graph.NodeScope, exclude graph.DeadCodeCandidateExclusions) []*graph.Node {
	if backend, ok := r.Reader.(graph.FilteredNodeScopedDeadCodeReader); ok {
		nodes, err := backend.DeadCodeCandidatesInScopeFiltered(kinds, incoming, prefixes, scope, r.deadCodeFilter(), exclude)
		recordContractCoreReadError(r.ctx, err)
		return nodes
	}
	// The exact selected reader keeps its own adjacency mask. A composed view
	// without SQL support remains correct without tunnelling to its base store.
	var nodes []*graph.Node
	for _, n := range r.DeadCodeCandidatesInRepos(kinds, incoming, prefixes) {
		if n != nil && scope.Allows(n) && exclude.Allows(n) {
			nodes = append(nodes, n)
		}
	}
	return nodes
}

func (r *contractCoreEdges) NodesByKindInScope(kind graph.NodeKind, prefixes []string, scope graph.NodeScope) iter.Seq[*graph.Node] {
	if backend, ok := r.Reader.(graph.NodeScopedDeadCodeReader); ok {
		return backend.NodesByKindInScope(kind, prefixes, scope)
	}
	return func(yield func(*graph.Node) bool) {
		for n := range r.NodesByKindInRepos(kind, prefixes) {
			if n != nil && scope.Allows(n) && !yield(n) {
				return
			}
		}
	}
}
