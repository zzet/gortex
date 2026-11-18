package mcp

import "github.com/zzet/gortex/internal/graph"

// scopedOutlineCountsFor preserves the exact reader's adjacency semantics.
// A contract-core wrapper supplies its exclusions to a selected backend that
// can aggregate them; composed and overlay readers retain the legacy fallback.
// Neither raw aggregate capability is exposed through the wrapper itself.
func scopedOutlineCountsFor(reader graph.Reader, scope graph.NodeScope) (graph.OutlineCounts, bool, error) {
	if wrapped, ok := reader.(contractCoreWrapped); ok {
		core := wrapped.contractCore()
		ag, ok := graph.BindReadContext(core.Reader, core.ctx).(graph.FilteredScopedOutlineAggregator)
		if !ok {
			return graph.OutlineCounts{}, false, nil
		}
		filter := graph.OutlineEdgeFilter{
			ExcludeKinds:   []graph.EdgeKind{graph.EdgeMatches, graph.EdgeBridges, graph.EdgeHandlesRoute, graph.EdgeReadsConfig},
			ExcludeNodeIDs: core.contractIDs,
			ExcludeVia:     "spring.Bean",
		}
		if core.contractIDs == nil {
			filter.ExcludeNodeKinds = contractCoreOwnedKinds
		}
		counts, err := ag.ScopedOutlineCountsFiltered(scope, filter)
		return counts, true, err
	}
	ag, ok := reader.(graph.ScopedOutlineAggregator)
	if !ok {
		return graph.OutlineCounts{}, false, nil
	}
	counts, err := ag.ScopedOutlineCounts(scope)
	return counts, true, err
}
