package indexer

import (
	"encoding/json"
	"path/filepath"
	"sort"
	"strings"

	"github.com/zzet/gortex/internal/contracts"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/modules"
)

// refreshContractsForFiles re-extracts only the exact changed-file frontier.
// It returns whether the effective contract set changed and whether a
// conservative full-repo fallback was required.
const contractFrontierReadBatchSize = 128

type contractRefreshResult struct {
	Changed        bool
	LegacyFallback bool
	Groups         []ContractGroupFrontier
	SymbolIDs      []string
}

func (result *contractRefreshResult) addFrontier(contractsToAdd ...[]contracts.Contract) {
	if result == nil {
		return
	}
	for _, records := range contractsToAdd {
		for _, contract := range records {
			if contract.ID == "" {
				continue
			}
			result.Groups = append(result.Groups, ContractGroupFrontier{
				WorkspaceID: contract.EffectiveWorkspace(),
				ProjectID:   contract.EffectiveProject(),
				ContractID:  contract.ID,
			})
			if contract.SymbolID != "" {
				result.SymbolIDs = append(result.SymbolIDs, contract.SymbolID)
			}
		}
	}
}

func (idx *Indexer) refreshContractsForFiles(files []string) contractRefreshResult {
	files = appendUniqueSorted(nil, files...)
	if len(files) == 0 {
		return contractRefreshResult{}
	}

	reg := idx.ensureIncrementalContractRegistry()
	files = idx.expandIncrementalContractFrontier(files, reg)
	_, byLang := idx.buildPerFileContractExtractors()
	result := contractRefreshResult{}
	var changedFiles []string
	priorIDs := make(map[string]struct{})
	for start := 0; start < len(files); start += contractFrontierReadBatchSize {
		end := start + contractFrontierReadBatchSize
		if end > len(files) {
			end = len(files)
		}
		chunk := files[start:end]
		nodesByFile, edgesByNode := idx.contractGraphFrontier(chunk)
		for _, graphPath := range chunk {
			prior := reg.ByFile(graphPath)
			fresh, mtimeNano, exists, preservePrior := idx.extractContractsForGraphFileFromBatch(
				graphPath, byLang, nodesByFile[graphPath], edgesByNode,
			)
			if preservePrior {
				continue
			}
			if !contractSetsEqual(prior, fresh) {
				result.addFrontier(prior, fresh)
				for _, contract := range prior {
					if contract.ID != "" {
						priorIDs[contract.ID] = struct{}{}
					}
				}
				reg.ReplaceFile(graphPath, fresh)
				changedFiles = append(changedFiles, graphPath)
				result.Changed = true
			}

			idx.contractCacheMu.Lock()
			if exists {
				idx.contractCache[graphPath] = &contractCacheEntry{mtimeNano: mtimeNano, contracts: fresh}
			} else {
				delete(idx.contractCache, graphPath)
			}
			idx.contractCacheMu.Unlock()
		}
	}
	result.Groups = mergeContractGroups(nil, result.Groups...)
	result.SymbolIDs = appendUniqueSorted(nil, result.SymbolIDs...)
	if result.Changed {
		idx.commitIncrementalContractFiles(reg, changedFiles, priorIDs)
	}
	return result
}

func (idx *Indexer) isIncrementalContractManifest(absPath string) bool {
	relPath, err := filepath.Rel(idx.rootPath, absPath)
	if err != nil || relPath == ".." || strings.HasPrefix(relPath, ".."+string(filepath.Separator)) {
		return false
	}
	relPath = filepath.ToSlash(relPath)
	return relPath == "go.mod" || relPath == "go.work"
}

func splitIncrementalContractManifests(idx *Indexer, files []string) (sources, manifests []string) {
	for _, filePath := range files {
		if idx.isIncrementalContractManifest(filePath) {
			manifests = append(manifests, filePath)
		} else {
			sources = append(sources, filePath)
		}
	}
	return sources, manifests
}

// refreshIncrementalContractManifests updates root manifest graph artifacts in
// one bounded pass. go.mod module edges are rebuilt from the manifest and the
// repo's KindImport projection; go.work has no contract artifacts of its own.
func (idx *Indexer) refreshIncrementalContractManifests(files []string) (DerivedInvalidationPlan, []string) {
	var plan DerivedInvalidationPlan
	files = appendUniqueSorted(nil, files...)
	receipts := make([]fileReadReceipt, 0, len(files))
	failed := make([]string, 0)
	for _, absPath := range files {
		relPath := idx.graphRelKey(absPath)
		graphPath := idx.prefixPath(relPath)
		src, readVersion, err := idx.readFileWithVersion(absPath)
		if err != nil || !readVersion.valid {
			failed = append(failed, absPath)
			continue
		}
		switch filepath.ToSlash(relPath) {
		case "go.mod":
			if idx.config.Coverage.IsEnabled("modules") {
				idx.graph.EvictFile(graphPath)
				idx.extractOneModuleManifestSource("go.mod", src, modules.ParseGoMod, readGoModModulePath)
			}
		case "go.work":
			// The Go semantic loader consumes go.work from disk when an affected
			// source frontier runs; no synthetic module-contract rows are emitted.
		default:
			continue
		}
		receipts = append(receipts, fileReadReceipt{
			absPath: absPath, mtimeKey: idx.relKey(absPath), readVersion: readVersion,
		})
		plan.Flags |= DerivedInvalidatesImports | DerivedInvalidatesContracts
		plan.Files = append(plan.Files, graphPath)
	}
	_, stale := idx.recordFileReadVersionsBatched(receipts)
	failed = append(failed, stale...)
	plan.Files = appendUniqueSorted(nil, plan.Files...)
	return plan, appendUniqueSorted(nil, failed...)
}

func (idx *Indexer) ensureIncrementalContractRegistry() *contracts.Registry {
	if idx.contractRegistry != nil {
		return idx.contractRegistry
	}

	reg := contracts.NewRegistry()
	ownerRows := graph.ReadRepoEdgesByKinds(
		idx.graph,
		[]string{idx.repoPrefix},
		[]graph.EdgeKind{graph.EdgeProvides, graph.EdgeConsumes},
	)
	ownersByContract := make(map[string][]*graph.Edge)
	contractIDs := make(map[string]struct{})
	for _, row := range ownerRows {
		if row.Edge == nil || row.Edge.To == "" {
			continue
		}
		ownersByContract[row.Edge.To] = append(ownersByContract[row.Edge.To], row.Edge)
		contractIDs[row.Edge.To] = struct{}{}
	}
	// Symbol-less contracts have no ownership edge. Preserve the exact repo node
	// projection for those legacy rows. Ordinary contracts are recovered from
	// repo-owned edges, so a shared canonical node last written by another repo
	// cannot hide this repository on a warm restart.
	legacyNodeIDs := graph.ReadRepoNodeIDsByKinds(
		idx.graph, []string{idx.repoPrefix}, []graph.NodeKind{graph.KindContract},
	)
	for _, id := range legacyNodeIDs {
		if id != "" {
			contractIDs[id] = struct{}{}
		}
	}
	ids := make([]string, 0, len(contractIDs))
	for id := range contractIDs {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	for start := 0; start < len(ids); start += contractFrontierReadBatchSize {
		end := start + contractFrontierReadBatchSize
		if end > len(ids) {
			end = len(ids)
		}
		chunk := ids[start:end]
		nodes := idx.graph.GetNodesByIDs(chunk)
		for _, id := range chunk {
			node := nodes[id]
			if node == nil || node.Kind != graph.KindContract {
				continue
			}
			added := false
			seen := make(map[string]struct{})
			for _, edge := range ownersByContract[id] {
				contract, ok := storedContractFromOwner(
					node, edge, idx.repoPrefix, idx.workspaceID, idx.projectID,
				)
				if !ok {
					continue
				}
				key := contractRegistryKey(contract)
				if _, duplicate := seen[key]; duplicate {
					continue
				}
				seen[key] = struct{}{}
				reg.Add(contract)
				added = true
			}
			if added {
				continue
			}
			base, ok := storedContractFromNode(node)
			if !ok {
				continue
			}
			base.RepoPrefix = idx.repoPrefix
			base.WorkspaceID = idx.workspaceID
			base.ProjectID = idx.projectID
			reg.Add(base)
		}
	}
	idx.contractRegistry = reg
	return reg
}

func storedContractFromNode(node *graph.Node) (contracts.Contract, bool) {
	if node == nil || node.Kind != graph.KindContract || node.ID == "" {
		return contracts.Contract{}, false
	}
	contract := contracts.Contract{
		ID:          node.ID,
		FilePath:    node.FilePath,
		RepoPrefix:  node.RepoPrefix,
		WorkspaceID: node.WorkspaceID,
		ProjectID:   node.ProjectID,
	}
	if node.Meta != nil {
		contract.Type = contracts.ContractType(contractStringMeta(node.Meta, "type"))
		contract.Role = contracts.Role(contractStringMeta(node.Meta, "role"))
		contract.SymbolID = contractStringMeta(node.Meta, "symbol_id")
		contract.Line = contractIntValue(node.Meta["line"])
		contract.Confidence = contractFloatValue(node.Meta["confidence"])
		if meta, ok := node.Meta["contract_meta"].(map[string]any); ok {
			contract.Meta = meta
		}
	}
	return contract, true
}

func storedContractFromOwner(
	node *graph.Node,
	edge *graph.Edge,
	repoPrefix, workspaceID, projectID string,
) (contracts.Contract, bool) {
	contract, ok := storedContractFromNode(node)
	if !ok || edge == nil {
		return contracts.Contract{}, false
	}
	contract.SymbolID = edge.From
	contract.FilePath = edge.FilePath
	contract.Line = edge.Line
	contract.RepoPrefix = repoPrefix
	contract.WorkspaceID = workspaceID
	contract.ProjectID = projectID
	if edge.Kind == graph.EdgeConsumes {
		contract.Role = contracts.RoleConsumer
	} else {
		contract.Role = contracts.RoleProvider
	}
	if edge.Meta == nil {
		return contract, true
	}
	if value, exists := edge.Meta["contract_owner_repo_prefix"].(string); exists {
		contract.RepoPrefix = value
	}
	if value, exists := edge.Meta["contract_owner_workspace"].(string); exists {
		contract.WorkspaceID = value
	}
	if value, exists := edge.Meta["contract_owner_project"].(string); exists {
		contract.ProjectID = value
	}
	if value, exists := edge.Meta["contract_owner_type"].(string); exists {
		contract.Type = contracts.ContractType(value)
	}
	if value, exists := edge.Meta["contract_owner_confidence"]; exists {
		contract.Confidence = contractFloatValue(value)
	}
	if value, exists := edge.Meta["contract_owner_meta"].(map[string]any); exists {
		contract.Meta = value
	}
	return contract, true
}

func contractOwnerEdgeMeta(contract contracts.Contract) map[string]any {
	return map[string]any{
		"contract_owner_repo_prefix": contract.RepoPrefix,
		"contract_owner_workspace":   contract.EffectiveWorkspace(),
		"contract_owner_project":     contract.EffectiveProject(),
		"contract_owner_type":        string(contract.Type),
		"contract_owner_confidence":  contract.Confidence,
		"contract_owner_meta":        contract.Meta,
	}
}

func contractStringMeta(meta map[string]any, key string) string {
	value, _ := meta[key].(string)
	return value
}

func contractIntValue(value any) int {
	switch number := value.(type) {
	case int:
		return number
	case int64:
		return int(number)
	case float64:
		return int(number)
	case json.Number:
		parsed, _ := number.Int64()
		return int(parsed)
	default:
		return 0
	}
}

func contractFloatValue(value any) float64 {
	switch number := value.(type) {
	case float64:
		return number
	case float32:
		return float64(number)
	case int:
		return float64(number)
	case int64:
		return float64(number)
	case json.Number:
		parsed, _ := number.Float64()
		return parsed
	default:
		return 0
	}
}

// expandIncrementalContractFrontier promotes only cross-file contract constructs
// to the existing contract-file dependency set. It never enumerates every source
// file in the repository; go.mod/go.work remain exact single-file refreshes.
func (idx *Indexer) expandIncrementalContractFrontier(files []string, reg *contracts.Registry) []string {
	needsDependencies := false
	for _, graphPath := range files {
		base := strings.ToLower(filepath.Base(graphPath))
		if base == "go.mod" || base == "go.work" {
			continue
		}
		relPath := graphPath
		if idx.repoPrefix != "" {
			prefix := idx.repoPrefix + "/"
			if !strings.HasPrefix(relPath, prefix) {
				continue
			}
			relPath = strings.TrimPrefix(relPath, prefix)
		}
		absPath := filepath.Join(idx.rootPath, filepath.FromSlash(relPath))
		src, err := idx.readFileContent(absPath)
		if err != nil {
			continue
		}
		language, _ := idx.effectiveLanguage(absPath, src)
		if contractSourceNeedsFullRefresh(graphPath, language, src) {
			needsDependencies = true
			break
		}
	}
	if !needsDependencies || reg == nil {
		return files
	}
	for _, contract := range reg.ByRepo(idx.repoPrefix) {
		files = append(files, contract.FilePath)
	}
	return appendUniqueSorted(nil, files...)
}

func (idx *Indexer) commitIncrementalContractFiles(
	reg *contracts.Registry,
	changedFiles []string,
	priorIDs map[string]struct{},
) {
	changedFiles = appendUniqueSorted(nil, changedFiles...)
	ids := make([]string, 0, len(priorIDs))
	for id := range priorIDs {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	var current []contracts.Contract
	for _, graphPath := range changedFiles {
		current = append(current, reg.ByFile(graphPath)...)
	}
	// A contract ID can be shared by several source files. Re-emit surviving
	// siblings while replacing only the changed owner files so another file or
	// repository using the same canonical ID keeps its ownership edges.
	for _, id := range ids {
		current = append(current, reg.ByID(id)...)
	}
	seen := make(map[string]struct{}, len(current))
	unique := current[:0]
	for _, contract := range current {
		key := contractRegistryKey(contract)
		if _, duplicate := seen[key]; duplicate {
			continue
		}
		seen[key] = struct{}{}
		unique = append(unique, contract)
	}
	sort.Slice(unique, func(i, j int) bool {
		return contractRegistryKey(unique[i]) < contractRegistryKey(unique[j])
	})

	nodes := make([]*graph.Node, 0, len(unique))
	edges := make([]*graph.Edge, 0, len(unique)*2)
	touchedIDs := append([]string(nil), ids...)
	for _, contract := range unique {
		nodes = append(nodes, &graph.Node{
			ID:          contract.ID,
			Kind:        graph.KindContract,
			Name:        contract.ID,
			FilePath:    contract.FilePath,
			Language:    "contract",
			RepoPrefix:  contract.RepoPrefix,
			WorkspaceID: contract.EffectiveWorkspace(),
			ProjectID:   contract.EffectiveProject(),
			Meta: map[string]any{
				"type":          string(contract.Type),
				"role":          string(contract.Role),
				"symbol_id":     contract.SymbolID,
				"line":          contract.Line,
				"confidence":    contract.Confidence,
				"contract_meta": contract.Meta,
			},
		})
		touchedIDs = append(touchedIDs, contract.ID)
		if contract.SymbolID == "" {
			continue
		}
		edgeKind := graph.EdgeProvides
		if contract.Role == contracts.RoleConsumer {
			edgeKind = graph.EdgeConsumes
		}
		edges = append(edges, &graph.Edge{
			From: contract.SymbolID, To: contract.ID, Kind: edgeKind,
			FilePath: contract.FilePath, Line: contract.Line,
			Meta: contractOwnerEdgeMeta(contract),
		})
		if contract.Role == contracts.RoleProvider && isRouteContractType(contract.Type) {
			routeMeta := contractOwnerEdgeMeta(contract)
			routeMeta["contract_type"] = string(contract.Type)
			edges = append(edges, &graph.Edge{
				From: contract.SymbolID, To: contract.ID, Kind: graph.EdgeHandlesRoute,
				FilePath: contract.FilePath, Line: contract.Line,
				Meta: routeMeta,
			})
		}
	}
	if _, err := graph.ReplaceContractOwners(idx.graph, graph.ContractOwnerReplacement{
		RepoPrefix:     idx.repoPrefix,
		FilePaths:      changedFiles,
		TouchedNodeIDs: touchedIDs,
		Nodes:          nodes,
		Edges:          edges,
	}); err != nil {
		idx.logger.Warn("incremental contract owner replacement failed: " + err.Error())
	}
	idx.contractRegistry = reg
}

func contractRegistryKey(contract contracts.Contract) string {
	return contract.ID + "|" + contract.FilePath + "|" + contract.SymbolID + "|" + string(contract.Role)
}

func (idx *Indexer) contractGraphFrontier(
	graphPaths []string,
) (map[string][]*graph.Node, map[string][]*graph.Edge) {
	nodesByFile := idx.graph.GetFileNodesByPaths(graphPaths)
	var nodeIDs []string
	seen := make(map[string]struct{})
	for _, graphPath := range graphPaths {
		for _, node := range nodesByFile[graphPath] {
			if node == nil || node.ID == "" {
				continue
			}
			if _, duplicate := seen[node.ID]; duplicate {
				continue
			}
			seen[node.ID] = struct{}{}
			nodeIDs = append(nodeIDs, node.ID)
		}
	}
	return nodesByFile, idx.graph.GetOutEdgesByNodeIDs(nodeIDs)
}

func (idx *Indexer) extractIncrementalManifestContracts(
	graphPath string,
) (fresh []contracts.Contract, mtimeNano int64, exists, preservePrior, handled bool) {
	base := strings.ToLower(filepath.Base(graphPath))
	if base != "go.mod" && base != "go.work" {
		return nil, 0, false, false, false
	}
	relPath := graphPath
	if idx.repoPrefix != "" {
		prefix := idx.repoPrefix + "/"
		if !strings.HasPrefix(relPath, prefix) {
			return nil, 0, true, true, true
		}
		relPath = strings.TrimPrefix(relPath, prefix)
	}
	absPath := filepath.Join(idx.rootPath, filepath.FromSlash(relPath))
	mtimeNano, exists, readable := idx.contentFileVersion(absPath)
	if !readable {
		if !exists {
			return nil, 0, false, false, true
		}
		return nil, 0, true, true, true
	}
	src, err := idx.readFileContent(absPath)
	if err != nil {
		return nil, 0, true, true, true
	}
	if base == "go.work" {
		return nil, mtimeNano, true, false, true
	}
	extractor := &contracts.GoModExtractor{TrackedRepos: idx.trackedRepoModules}
	fresh = extractor.Extract(graphPath, src, nil, nil)
	for i := range fresh {
		fresh[i].RepoPrefix = idx.repoPrefix
		fresh[i].WorkspaceID = idx.workspaceID
		fresh[i].ProjectID = idx.projectID
	}
	return fresh, mtimeNano, true, false, true
}

func (idx *Indexer) extractContractsForGraphFileFromBatch(
	graphPath string,
	byLang map[string][]contracts.Extractor,
	fileNodes []*graph.Node,
	edgesByNode map[string][]*graph.Edge,
) ([]contracts.Contract, int64, bool, bool) {
	if fresh, mtimeNano, exists, preservePrior, handled := idx.extractIncrementalManifestContracts(graphPath); handled {
		return fresh, mtimeNano, exists, preservePrior
	}
	var fileNode *graph.Node
	for _, node := range fileNodes {
		if node != nil && node.Kind == graph.KindFile {
			fileNode = node
			break
		}
	}
	if fileNode == nil {
		// The exact file was deleted and its graph nodes were already evicted.
		return nil, 0, false, false
	}

	relPath := graphPath
	if idx.repoPrefix != "" {
		prefix := idx.repoPrefix + "/"
		if !strings.HasPrefix(relPath, prefix) {
			return nil, 0, true, true
		}
		relPath = strings.TrimPrefix(relPath, prefix)
	}
	absPath := filepath.Join(idx.rootPath, filepath.FromSlash(relPath))
	mtimeNano, _, readable := idx.contentFileVersion(absPath)
	if !readable {
		return nil, 0, true, true
	}
	fileEdges := edgesByNode[fileNode.ID]
	var fresh []contracts.Contract
	if extractors := byLang[fileNode.Language]; len(extractors) > 0 {
		src, err := idx.readFileContent(absPath)
		if err != nil {
			return nil, 0, true, true
		}
		tree := contracts.ParseTreeForLang(fileNode.Language, src)
		fresh = idx.runContractExtractorsForFile(
			graphPath, src, fileNodes, fileEdges, extractors, tree,
		)
		if tree != nil {
			tree.Release()
		}
	}

	// DI contracts are normally appended by the repo-wide post-pass. Rebuild
	// only those whose source edge belongs to this file so ReplaceFile does not
	// discard valid @Inject / provider records on an incremental refresh.
	for _, node := range fileNodes {
		if node == nil {
			continue
		}
		for _, edge := range edgesByNode[node.ID] {
			contract, ok := diContractFromEdge(edge)
			if !ok || contract.FilePath != graphPath {
				continue
			}
			contract.RepoPrefix = idx.repoPrefix
			if idx.workspaceID != "" {
				contract.WorkspaceID = idx.workspaceID
			}
			if idx.projectID != "" {
				contract.ProjectID = idx.projectID
			}
			fresh = append(fresh, contract)
		}
	}
	return fresh, mtimeNano, true, false
}

func contractSourceNeedsFullRefresh(graphPath, language string, src []byte) bool {
	lowerPath := strings.ToLower(graphPath)
	lowerSource := strings.ToLower(string(src))
	// These constructs can rewrite contracts owned by sibling files. They are
	// uncommon, so retain the full pass only when the changed bytes actually
	// contain a cross-file mount or DI declaration.
	if language == "python" && strings.Contains(lowerSource, "include_router") {
		return true
	}
	if (language == "typescript" || language == "javascript") &&
		(strings.Contains(lowerSource, ".use(") || strings.Contains(lowerSource, "@controller(")) {
		return true
	}
	if language == "java" &&
		(strings.Contains(lowerSource, "@bean") || strings.Contains(lowerSource, "@inject") ||
			strings.Contains(lowerSource, "@configuration")) {
		return true
	}
	return strings.HasSuffix(lowerPath, ".properties") || strings.HasSuffix(lowerPath, ".yaml") || strings.HasSuffix(lowerPath, ".yml")
}

func contractSetsEqual(left, right []contracts.Contract) bool {
	rows := func(list []contracts.Contract) ([]string, bool) {
		out := make([]string, 0, len(list))
		for _, contract := range list {
			encoded, err := json.Marshal(contract)
			if err != nil {
				return nil, false
			}
			out = append(out, string(encoded))
		}
		sort.Strings(out)
		return out, true
	}
	leftRows, leftOK := rows(left)
	rightRows, rightOK := rows(right)
	if !leftOK || !rightOK || len(leftRows) != len(rightRows) {
		return false
	}
	for i := range leftRows {
		if leftRows[i] != rightRows[i] {
			return false
		}
	}
	return true
}
