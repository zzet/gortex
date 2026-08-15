package indexer

import "github.com/zzet/gortex/internal/graph"

// contractInvalidationPlanForRepo snapshots the derived contract frontier
// before the repository registry and graph nodes are removed. Cross-repository
// bridge edges may be owned by a surviving repository, so the removed contract
// IDs alone are not enough; include every bridge that points at them.
func (mi *MultiIndexer) contractInvalidationPlanForRepo(idx *Indexer) DerivedInvalidationPlan {
	var plan DerivedInvalidationPlan
	if idx == nil {
		return plan
	}

	registry := idx.ContractRegistry()
	if registry == nil {
		return plan
	}

	contractIDs := make(map[string]struct{})
	for _, contract := range registry.All() {
		if contract.ID == "" {
			continue
		}
		plan.ContractGroups = append(plan.ContractGroups, ContractGroupFrontier{
			WorkspaceID: contract.EffectiveWorkspace(),
			ProjectID:   contract.EffectiveProject(),
			ContractID:  contract.ID,
		})
		if contract.SymbolID != "" {
			plan.ContractSymbolIDs = append(plan.ContractSymbolIDs, contract.SymbolID)
		}
		contractIDs[contract.ID] = struct{}{}
	}
	if len(contractIDs) == 0 {
		return plan
	}

	incoming := mi.graph.GetInEdgesByNodeIDs(sortedStringKeys(contractIDs))
	bridgeIDs := make(map[string]struct{})
	for _, edges := range incoming {
		for _, edge := range edges {
			if edge != nil && edge.Kind == graph.EdgeBridges && edge.From != "" {
				bridgeIDs[edge.From] = struct{}{}
			}
		}
	}
	plan.ContractBridgeNodeIDs = sortedStringKeys(bridgeIDs)
	return plan
}
