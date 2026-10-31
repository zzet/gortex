package mcp

import (
	"iter"

	"github.com/zzet/gortex/internal/graph"
)

var _ graph.ScopedDeadCodeReader = (*contractCoreEdges)(nil)

// This is the same adjacency contract as filterBatch, supplied explicitly to
// backend queries. Raw candidate/interface capabilities cannot cross the mask.
func (r *contractCoreEdges) deadCodeFilter() graph.OutlineEdgeFilter {
	filter := graph.OutlineEdgeFilter{
		ExcludeKinds:   []graph.EdgeKind{graph.EdgeMatches, graph.EdgeBridges, graph.EdgeHandlesRoute, graph.EdgeReadsConfig},
		ExcludeNodeIDs: r.contractIDs,
		ExcludeVia:     "spring.Bean",
	}
	if r.contractIDs == nil {
		filter.ExcludeNodeKinds = contractCoreOwnedKinds
	}
	return filter
}

func (r *contractCoreEdges) NodesByKindInRepos(kind graph.NodeKind, prefixes []string) iter.Seq[*graph.Node] {
	if scoped, ok := r.Reader.(graph.ScopedDeadCodeReader); ok {
		return scoped.NodesByKindInRepos(kind, prefixes)
	}
	return func(yield func(*graph.Node) bool) {
		if prefixes != nil && len(prefixes) == 0 {
			return
		}
		for n := range r.NodesByKind(kind) {
			if r.ctx != nil && r.ctx.Err() != nil {
				return
			}
			if n != nil && deadCodePrefixAllowed(n.RepoPrefix, prefixes) && !yield(n) {
				return
			}
		}
	}
}

func deadCodePrefixAllowed(prefix string, prefixes []string) bool {
	if prefixes == nil {
		return true
	}
	for _, allowed := range prefixes {
		if allowed == prefix {
			return true
		}
	}
	return false
}

func (r *contractCoreEdges) DeadCodeCandidatesInRepos(kinds []graph.NodeKind, incoming map[graph.NodeKind][]graph.EdgeKind, prefixes []string) []*graph.Node {
	if scoped, ok := r.Reader.(graph.FilteredScopedDeadCodeReader); ok {
		nodes, err := scoped.DeadCodeCandidatesInReposFiltered(kinds, incoming, prefixes, r.deadCodeFilter())
		recordContractCoreReadError(r.ctx, err)
		return nodes
	}
	// Composed readers without the filtered query keep their own nodes and
	// expand usage through this mask. Bound each adjacency read to one batch.
	var out []*graph.Node
	for _, kind := range kinds {
		var batch []*graph.Node
		flush := func() {
			ids := make([]string, 0, len(batch))
			for _, n := range batch {
				ids = append(ids, n.ID)
			}
			edges := r.GetInEdgesByNodeIDs(ids)
			for _, n := range batch {
				used := false
				for _, edge := range edges[n.ID] {
					if edge == nil {
						continue
					}
					if len(incoming[kind]) == 0 {
						used = true
						break
					}
					for _, allowed := range incoming[kind] {
						if edge.Kind == allowed {
							used = true
							break
						}
					}
					if used {
						break
					}
				}
				if !used {
					out = append(out, n)
				}
			}
			batch = batch[:0]
		}
		for n := range r.NodesByKindInRepos(kind, prefixes) {
			if r.ctx != nil && r.ctx.Err() != nil {
				return out
			}
			batch = append(batch, n)
			if len(batch) == 256 {
				flush()
			}
		}
		if len(batch) > 0 {
			flush()
		}
	}
	return out
}

func (r *contractCoreEdges) IfaceImplementsRowsInRepos(prefixes []string) []graph.IfaceImplementsRow {
	if scoped, ok := r.Reader.(graph.FilteredScopedDeadCodeReader); ok {
		rows, err := scoped.IfaceImplementsRowsInReposFiltered(prefixes, r.deadCodeFilter())
		recordContractCoreReadError(r.ctx, err)
		return rows
	}
	if prefixes != nil && len(prefixes) == 0 {
		return nil
	}
	// This compatibility path is used only when the exact selected view cannot
	// supply the filtered query. Both evidence and endpoints come from that view.
	var out []graph.IfaceImplementsRow
	var batch []*graph.Edge
	flush := func() {
		ids := make([]string, 0, len(batch)*2)
		for _, edge := range batch {
			ids = append(ids, edge.From, edge.To)
		}
		nodes := r.GetNodesByIDs(ids)
		for _, edge := range batch {
			typ, iface := nodes[edge.From], nodes[edge.To]
			if prefixes != nil && (typ == nil || !deadCodePrefixAllowed(typ.RepoPrefix, prefixes)) {
				continue
			}
			if iface == nil || iface.Kind != graph.KindInterface {
				continue
			}
			if _, ok := iface.Meta["methods"]; ok {
				out = append(out, graph.IfaceImplementsRow{TypeID: edge.From, IfaceID: edge.To, IfaceMeta: iface.Meta})
			}
		}
		batch = batch[:0]
	}
	for edge := range r.EdgesByKind(graph.EdgeImplements) {
		batch = append(batch, edge)
		if len(batch) == 256 {
			flush()
		}
	}
	if len(batch) > 0 {
		flush()
	}
	return out
}
