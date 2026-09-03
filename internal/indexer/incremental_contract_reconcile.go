package indexer

import (
	"sort"
	"strings"

	"github.com/zzet/gortex/internal/contracts"
	"github.com/zzet/gortex/internal/graph"
)

// ReconcileContractEdgesForFrontier is the precise incremental sibling of the
// existing full cold reconciliation path.
func (mi *MultiIndexer) ReconcileContractEdgesForFrontier(plan DerivedInvalidationPlan) int {
	if len(plan.ContractGroups) == 0 && len(plan.ContractSymbolIDs) == 0 {
		return mi.ReconcileContractEdges()
	}

	mi.reconcileMu.Lock()
	defer mi.reconcileMu.Unlock()
	g := mi.Graph()
	if g == nil {
		return 0
	}
	// Same resolver-lane exclusion as the full pass: the incident-edge scan
	// below dereferences live store edges a concurrent resolve rewrites in
	// place. See ReconcileContractEdges.
	g.ResolveMutex().Lock()
	defer g.ResolveMutex().Unlock()
	merged := mi.MergedContractRegistry()
	if merged == nil {
		return 0
	}

	inlined := contracts.InlineWrappersForFiles(merged, g, mi.wrapperSourceReader(), plan.Files)
	mi.persistScopedInlinedContracts(inlined)
	contracts.BindProviderSymbols(merged, g)
	matchResult := contracts.Match(merged)

	groups := make(map[string]struct{}, len(plan.ContractGroups))
	groupsByContract := make(map[string][]ContractGroupFrontier)
	contractIDSet := make(map[string]struct{}, len(plan.ContractGroups))
	for _, group := range plan.ContractGroups {
		if group.ContractID == "" {
			continue
		}
		groups[contractGroupKey(group)] = struct{}{}
		groupsByContract[group.ContractID] = append(groupsByContract[group.ContractID], group)
		contractIDSet[group.ContractID] = struct{}{}
	}
	seeds := make(map[string]struct{}, len(plan.ContractSymbolIDs))
	for _, id := range plan.ContractSymbolIDs {
		if id != "" {
			seeds[id] = struct{}{}
		}
	}
	for _, contract := range merged.All() {
		key := ContractGroupFrontier{
			WorkspaceID: contract.EffectiveWorkspace(),
			ProjectID:   contract.EffectiveProject(),
			ContractID:  contract.ID,
		}
		if _, affected := groups[contractGroupKey(key)]; affected && contract.SymbolID != "" {
			seeds[contract.SymbolID] = struct{}{}
		}
	}

	affectedBridges := make(map[bridgeGroupKey]struct{})
	var selected []contracts.CrossLink
	for _, link := range matchResult.Matched {
		if !contractLinkTouchesFrontier(link, groups, seeds) {
			continue
		}
		affectedBridges[contractLinkBridgeGroup(link)] = struct{}{}
		if link.Provider.SymbolID != "" && link.Consumer.SymbolID != "" &&
			link.Provider.SymbolID != link.Consumer.SymbolID {
			selected = append(selected, link)
		}
	}
	removeEdges := collectIncidentContractMatchEdges(g, sortedStringKeys(seeds))

	removeBridgeSet := make(map[string]struct{}, len(plan.ContractBridgeNodeIDs)+len(plan.ContractGroups))
	for _, id := range plan.ContractBridgeNodeIDs {
		if id != "" {
			removeBridgeSet[id] = struct{}{}
		}
	}
	if len(plan.ContractBridgeNodeIDs) > 0 {
		for _, node := range g.GetNodesByIDs(plan.ContractBridgeNodeIDs) {
			if key, ok := bridgeGroupFromNode(node); ok {
				affectedBridges[key] = struct{}{}
			}
		}
	}
	for _, group := range plan.ContractGroups {
		removeBridgeSet[bridgeNodeID(bridgeGroupKey{
			workspace: group.WorkspaceID, project: group.ProjectID, contractID: group.ContractID,
		})] = struct{}{}
	}
	contractIDs := sortedStringKeys(contractIDSet)
	if len(contractIDs) > 0 {
		incoming := g.GetInEdgesByNodeIDs(contractIDs)
		targetsByBridge := make(map[string]map[string]struct{})
		for contractID, adjacent := range incoming {
			for _, edge := range adjacent {
				if edge == nil || edge.Kind != graph.EdgeBridges {
					continue
				}
				if targetsByBridge[edge.From] == nil {
					targetsByBridge[edge.From] = make(map[string]struct{})
				}
				targetsByBridge[edge.From][contractID] = struct{}{}
			}
		}
		candidateIDs := make([]string, 0, len(targetsByBridge))
		for id := range targetsByBridge {
			candidateIDs = append(candidateIDs, id)
		}
		bridgeNodes := g.GetNodesByIDs(candidateIDs)
		for id, targets := range targetsByBridge {
			key, ok := bridgeGroupFromNode(bridgeNodes[id])
			if !ok || !bridgeBoundaryTouchesFrontier(key, targets, groupsByContract) {
				continue
			}
			removeBridgeSet[id] = struct{}{}
			affectedBridges[key] = struct{}{}
		}
	}
	for group := range affectedBridges {
		removeBridgeSet[bridgeNodeID(group)] = struct{}{}
	}

	var bridgeMatches []contracts.CrossLink
	topicSet := make(map[string]struct{})
	for _, group := range plan.ContractGroups {
		if _, _, ok := parseTopicContractID(group.ContractID); ok {
			topicSet[group.ContractID] = struct{}{}
		}
	}
	for _, link := range matchResult.Matched {
		if _, affected := affectedBridges[contractLinkBridgeGroup(link)]; !affected {
			continue
		}
		bridgeMatches = append(bridgeMatches, link)
		if link.Provider.Type == contracts.ContractTopic {
			topicSet[link.Provider.ID] = struct{}{}
		}
	}
	topicIDs := sortedStringKeys(topicSet)
	if len(topicIDs) > 0 {
		for _, adjacent := range g.GetInEdgesByNodeIDs(topicIDs) {
			for _, edge := range adjacent {
				if edge != nil && (edge.Kind == graph.EdgeProducesTopic || edge.Kind == graph.EdgeConsumesTopic) {
					removeEdges = append(removeEdges, edge)
				}
			}
		}
	}

	var nodes []*graph.Node
	var edges []*graph.Edge
	for _, link := range selected {
		edges = append(edges, contractMatchEdge(link))
	}
	topicNodes := make(map[string]struct{})
	for _, link := range matchResult.Matched {
		_, affected := topicSet[link.Provider.ID]
		if !affected || link.Provider.Type != contracts.ContractTopic ||
			link.Provider.SymbolID == "" || link.Consumer.SymbolID == "" ||
			link.Provider.SymbolID == link.Consumer.SymbolID {
			continue
		}
		appendTopicEdges(link, topicNodes, &nodes, &edges)
	}
	bridgeNodes, bridgeEdges := buildContractBridgeBatch(bridgeMatches)
	nodes = append(nodes, bridgeNodes...)
	edges = append(edges, bridgeEdges...)

	if _, err := graph.ReplaceDerivedContracts(g, graph.DerivedContractReplacement{
		RemoveEdges:         removeEdges,
		RemoveBridgeNodeIDs: sortedStringKeys(removeBridgeSet),
		Nodes:               nodes,
		Edges:               edges,
		TouchedTopicNodeIDs: topicIDs,
	}); err != nil {
		mi.logger.Warn("incremental contract reconciliation failed: " + err.Error())
		return 0
	}
	return len(selected)
}

func (mi *MultiIndexer) persistScopedInlinedContracts(inlined []contracts.Contract) {
	if len(inlined) == 0 {
		return
	}
	mi.mu.RLock()
	defer mi.mu.RUnlock()

	registrySet := make(map[*contracts.Registry]struct{})
	bareTypeNames := make(map[string]struct{})
	for _, contract := range inlined {
		idx := mi.indexers[contract.RepoPrefix]
		if contract.RepoPrefix == "" || idx == nil || idx.ContractRegistry() == nil {
			continue
		}
		registry := idx.ContractRegistry()
		registrySet[registry] = struct{}{}
		duplicate := false
		for _, existing := range registry.ByID(contract.ID) {
			if existing.SymbolID == contract.SymbolID && existing.FilePath == contract.FilePath && existing.Role == contract.Role {
				duplicate = true
				break
			}
		}
		if !duplicate {
			registry.Add(contract)
		}
		for _, key := range []string{"request_type", "response_type"} {
			name, _ := contract.Meta[key].(string)
			if name != "" && !strings.Contains(name, "::") {
				bareTypeNames[name] = struct{}{}
			}
		}
	}

	registries := make([]*contracts.Registry, 0, len(registrySet))
	for registry := range registrySet {
		registries = append(registries, registry)
	}
	if len(registries) == 0 {
		return
	}
	names := sortedStringKeys(bareTypeNames)
	matchesByName := mi.graph.FindNodesByNames(names)
	lookup := func(name, repoHint string) []string {
		matches := matchesByName[name]
		ids := make([]string, 0, len(matches))
		for _, node := range matches {
			if node.Kind == graph.KindType || node.Kind == graph.KindInterface {
				ids = append(ids, node.ID)
			}
		}
		if len(ids) <= 1 || repoHint == "" {
			return ids
		}
		var sameRepo []string
		for _, id := range ids {
			if strings.HasPrefix(id, repoHint+"/") {
				sameRepo = append(sameRepo, id)
			}
		}
		if len(sameRepo) > 0 {
			return sameRepo
		}
		return ids
	}
	for _, registry := range registries {
		registry.UpgradeBareTypeRefs(lookup)
	}
	mi.disambiguateBareTypesViaImportsBatch(registries, mi.graph)
	for _, registry := range registries {
		mi.attachInlinedShapes(registry, mi.graph)
	}
}

func contractLinkTouchesFrontier(link contracts.CrossLink, groups map[string]struct{}, symbols map[string]struct{}) bool {
	if _, ok := symbols[link.Provider.SymbolID]; ok {
		return true
	}
	if _, ok := symbols[link.Consumer.SymbolID]; ok {
		return true
	}
	for _, group := range []ContractGroupFrontier{
		{WorkspaceID: link.Provider.EffectiveWorkspace(), ProjectID: link.Provider.EffectiveProject(), ContractID: link.ContractID},
		{WorkspaceID: link.Provider.EffectiveWorkspace(), ProjectID: link.Provider.EffectiveProject(), ContractID: link.Provider.ID},
		{WorkspaceID: link.Consumer.EffectiveWorkspace(), ProjectID: link.Consumer.EffectiveProject(), ContractID: link.Consumer.ID},
	} {
		if _, ok := groups[contractGroupKey(group)]; ok {
			return true
		}
	}
	return false
}

func contractLinkBridgeGroup(link contracts.CrossLink) bridgeGroupKey {
	return bridgeGroupKey{
		workspace:  link.Provider.EffectiveWorkspace(),
		project:    link.Provider.EffectiveProject(),
		contractID: link.ContractID,
	}
}

func contractMatchEdge(link contracts.CrossLink) *graph.Edge {
	return &graph.Edge{
		From:            link.Consumer.SymbolID,
		To:              link.Provider.SymbolID,
		Kind:            graph.EdgeMatches,
		FilePath:        link.Consumer.FilePath,
		Line:            link.Consumer.Line,
		Confidence:      1,
		ConfidenceLabel: "EXTRACTED",
		Origin:          graph.OriginASTResolved,
		CrossRepo:       link.CrossRepo,
		Meta: map[string]any{
			"contract_id": link.ContractID,
			"workspace":   link.Provider.EffectiveWorkspace(),
			"project":     link.Provider.EffectiveProject(),
		},
	}
}

func collectIncidentContractMatchEdges(g graph.Store, symbolIDs []string) []*graph.Edge {
	if len(symbolIDs) == 0 {
		return nil
	}
	var edges []*graph.Edge
	for _, rows := range []map[string][]*graph.Edge{
		g.GetOutEdgesByNodeIDs(symbolIDs),
		g.GetInEdgesByNodeIDs(symbolIDs),
	} {
		for _, adjacent := range rows {
			for _, edge := range adjacent {
				if edge != nil && edge.Kind == graph.EdgeMatches {
					edges = append(edges, edge)
				}
			}
		}
	}
	return edges
}

func bridgeGroupFromNode(node *graph.Node) (bridgeGroupKey, bool) {
	if node == nil || node.Kind != graph.KindContractBridge || node.Meta == nil {
		return bridgeGroupKey{}, false
	}
	key := bridgeGroupKey{
		workspace:  stringMeta(node.Meta, "workspace"),
		project:    stringMeta(node.Meta, "project"),
		contractID: stringMeta(node.Meta, "contract_id"),
	}
	return key, key.contractID != ""
}

func bridgeBoundaryTouchesFrontier(bridge bridgeGroupKey, targets map[string]struct{}, frontier map[string][]ContractGroupFrontier) bool {
	for target := range targets {
		for _, group := range frontier[target] {
			if group.WorkspaceID == bridge.workspace && group.ProjectID == bridge.project {
				return true
			}
		}
	}
	return false
}

func sortedStringKeys(values map[string]struct{}) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		if key != "" {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	return keys
}
