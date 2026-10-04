package indexer

import (
	"encoding/json"
	"path/filepath"
	"sort"
	"strings"
	"time"

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
	if !idx.contractDependenciesChanged {
		remaining := files[:0]
		for _, path := range files {
			if idx.incrementalUnchangedContracts[path] == nil {
				remaining = append(remaining, path)
			}
		}
		files = remaining
	}
	if len(files) == 0 {
		return contractRefreshResult{}
	}

	reg := idx.ensureIncrementalContractRegistry()
	files = idx.expandIncrementalContractFrontier(files, reg)
	_, byLang := idx.buildPerFileContractExtractors()
	result := contractRefreshResult{}
	var changedFiles, restated []string
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
			fresh = idx.normalizeFreshContractRecords(fresh)
			if idx.contractInputError() != nil {
				return result
			}
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
			} else if !idx.contractRowsPresent(fresh) {
				// The records did not change, but the graph no longer holds
				// them: the file's rows were re-derived (a delta that covers
				// the path restates all of its rows, and the contract rows
				// are not part of the parse). They are written again, with
				// no frontier change: nothing a referrer binds to moved.
				restated = append(restated, graphPath)
				for _, contract := range fresh {
					if contract.ID != "" {
						priorIDs[contract.ID] = struct{}{}
					}
				}
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
	if idx.contractSharedInputsChanged {
		// Raw file extraction does not include cross-file mount prefixes.
		// Shared-input changes already retained the full registry/frontier;
		// join before persisting owner payloads, as the cold pass does.
		if scanFiles := idx.routerPrefixScanFiles(reg); len(scanFiles) > 0 {
			contracts.JoinRouterPrefixes(reg, scanFiles, idx.contractFileSrc)
			for _, path := range changedFiles {
				result.addFrontier(reg.ByFile(path))
			}
		}
	}
	result.Groups = mergeContractGroups(nil, result.Groups...)
	result.SymbolIDs = appendUniqueSorted(nil, result.SymbolIDs...)
	if result.Changed || len(restated) > 0 {
		idx.commitIncrementalContractFiles(reg, append(changedFiles, restated...), priorIDs)
	}
	return result
}

// contractRowsPresent reports whether the graph holds every record of list:
// its contract node and, for a record with a symbol, an owner edge from that
// symbol into it.
func (idx *Indexer) contractRowsPresent(list []contracts.Contract) bool {
	if len(list) == 0 {
		return true
	}
	ids := make([]string, 0, len(list))
	for _, c := range list {
		if c.ID != "" {
			ids = append(ids, c.ID)
		}
	}
	ids = appendUniqueSorted(nil, ids...)
	nodes := idx.graph.GetNodesByIDs(ids)
	incoming := idx.graph.GetInEdgesByNodeIDs(ids)
	for _, c := range list {
		if c.ID == "" {
			continue
		}
		if n := nodes[c.ID]; n == nil || n.Kind != graph.KindContract {
			return false
		}
		if c.SymbolID == "" {
			continue
		}
		owned := false
		for _, e := range incoming[c.ID] {
			if e != nil && e.From == c.SymbolID &&
				(e.Kind == graph.EdgeProvides || e.Kind == graph.EdgeConsumes || e.Kind == graph.EdgeHandlesRoute) {
				owned = true
				break
			}
		}
		if !owned {
			return false
		}
	}
	return true
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
		relPath := idx.relKey(absPath)
		graphPath := idx.prefixPath(relPath)
		src, readVersion, err := idx.readFileWithVersion(absPath)
		if err != nil || !readVersion.valid {
			if err == nil {
				err = errFileVersionChanged
			}
			idx.noteFileIndexFailure(absPath, err)
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

type contractRegistryLoadPhases struct {
	contracts.RegistryLoadStats
	CopyMS        float64 `json:"copy_ms"`
	CopiedRecords int     `json:"copied_records"`
}

func (idx *Indexer) ensureIncrementalContractRegistry() *contracts.Registry {
	if idx.contractRegistry != nil {
		return idx.contractRegistry
	}
	if seed := idx.contractRegistrySeed; seed != nil {
		idx.contractRegistrySeedCalls++
		idx.contractRegistrySeed = nil
		seed()
		if idx.contractRegistry != nil {
			return idx.contractRegistry
		}
	}
	started := time.Now()
	idx.contractRegistryLoads++
	reg := contracts.NewRegistry()
	restored, stats := contracts.LoadRegistryFromGraphWithScopeAndStats(idx.graph, idx.repoPrefix, idx.workspaceID, idx.projectID)
	phases := &contractRegistryLoadPhases{RegistryLoadStats: stats}
	copyStarted := time.Now()
	if restored != nil {
		for _, c := range restored.ByRepo(idx.repoPrefix) {
			reg.Add(c)
			phases.CopiedRecords++
		}
	}
	phases.CopyMS = float64(time.Since(copyStarted).Nanoseconds()) / 1e6
	idx.contractRegistryLoadPhases = phases
	idx.contractRegistry = reg
	idx.contractRegistryLoad += time.Since(started)
	return reg
}

func contractOwnerEdgeMeta(c contracts.Contract) map[string]any {
	return map[string]any{
		"contract_owner_repo_prefix": c.RepoPrefix,
		"contract_owner_workspace":   c.EffectiveWorkspace(),
		"contract_owner_project":     c.EffectiveProject(),
		"contract_owner_type":        string(c.Type),
		"contract_owner_confidence":  c.Confidence,
		"contract_owner_meta":        c.Meta,
		"contract_owner_symbol_id":   c.SymbolID,
	}
}

type contractFileOwnerKey struct{ repo, file string }

type contractSymbolOwnerKey struct{ repo, symbol string }

// contractSymbolOwners validates source existence and repository ownership in
// bounded ID batches. Raw AddBatch accepts dangling and wrong-repo owner rows,
// but repo-scoped reconstruction cannot discover those rows for this record.
// Retain only the identity result, not the hydrated source metadata.
func contractSymbolOwners(store graph.Store, all []contracts.Contract) map[contractSymbolOwnerKey]string {
	wanted := make(map[contractSymbolOwnerKey]struct{})
	idsSet := make(map[string]struct{})
	for _, c := range all {
		if c.SymbolID == "" {
			continue
		}
		wanted[contractSymbolOwnerKey{c.RepoPrefix, c.SymbolID}] = struct{}{}
		idsSet[c.SymbolID] = struct{}{}
	}
	ids := make([]string, 0, len(idsSet))
	for id := range idsSet {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	owners := make(map[contractSymbolOwnerKey]string, len(wanted))
	for start := 0; start < len(ids); start += contractFrontierReadBatchSize {
		for _, node := range store.GetNodesByIDs(ids[start:min(start+contractFrontierReadBatchSize, len(ids))]) {
			if node == nil || node.ID == "" {
				continue
			}
			key := contractSymbolOwnerKey{node.RepoPrefix, node.ID}
			if _, keep := wanted[key]; keep {
				owners[key] = node.ID
			}
		}
	}
	return owners
}

// contractFileOwners queries only files of symbol-less records. An admitted
// genuine KindFile node is required: do not fabricate source nodes or use a
// contract canonical node as an ownership endpoint.
func contractFileOwners(store graph.Store, all []contracts.Contract) map[contractFileOwnerKey]string {
	files := make([]string, 0)
	wanted := make(map[contractFileOwnerKey]struct{})
	for _, c := range all {
		if c.SymbolID != "" || c.FilePath == "" {
			continue
		}
		key := contractFileOwnerKey{c.RepoPrefix, c.FilePath}
		if _, duplicate := wanted[key]; duplicate {
			continue
		}
		wanted[key] = struct{}{}
		files = append(files, c.FilePath)
	}
	owners := make(map[contractFileOwnerKey]string, len(wanted))
	if len(files) == 0 {
		return owners
	}
	for node := range graph.NodesInScopeSeq(store, nil, files, graph.KindFile) {
		if node == nil || node.Kind != graph.KindFile || node.ID == "" {
			continue
		}
		key := contractFileOwnerKey{node.RepoPrefix, node.FilePath}
		if _, keep := wanted[key]; !keep {
			continue
		}
		if current := owners[key]; current == "" || node.ID < current {
			owners[key] = node.ID
		}
	}
	return owners
}

func contractOwnerEndpoint(c contracts.Contract, files map[contractFileOwnerKey]string, symbols map[contractSymbolOwnerKey]string) string {
	if c.SymbolID != "" {
		return symbols[contractSymbolOwnerKey{c.RepoPrefix, c.SymbolID}]
	}
	return files[contractFileOwnerKey{c.RepoPrefix, c.FilePath}]
}

// contractGraphRows is the common persistence emitter. Full passes leave
// dependency nodes to their pre-resolution single writer; incremental refresh
// includes dependencies as before. No extraction/enrichment work moves here.
func contractGraphRows(store graph.Store, all []contracts.Contract, includeDependencies bool) (nodes []*graph.Node, edges []*graph.Edge, missingSourceOwners int) {
	if !includeDependencies {
		eligible := make([]contracts.Contract, 0, len(all))
		for _, c := range all {
			if c.Type != contracts.ContractDependency {
				eligible = append(eligible, c)
			}
		}
		all = eligible
	}
	// A contract ID shared by several records (an environment variable read in
	// two files, a route with a provider and consumers) is ONE node. Its row is
	// the record that sorts first by contractNodeRecordLess — the smallest file
	// — so the node a whole index writes
	// and the one a per-file refresh re-emits (which re-emits every sibling of
	// a touched ID) are the same whatever order the registry — a map, filled by
	// parallel extraction — listed the records in. Every record keeps its own
	// ownership edges.
	all = append([]contracts.Contract(nil), all...)
	sort.SliceStable(all, func(i, j int) bool { return contractNodeRecordLess(all[i], all[j]) })
	fileOwners := contractFileOwners(store, all)
	symbolOwners := contractSymbolOwners(store, all)
	nodes = make([]*graph.Node, 0, len(all))
	edges = make([]*graph.Edge, 0, len(all)*2)
	for i, c := range all {
		ownerID := contractOwnerEndpoint(c, fileOwners, symbolOwners)
		if ownerID == "" {
			missingSourceOwners++
		}
		// Only the ID's first record writes its node.
		if i == 0 || all[i-1].ID != c.ID {
			nodes = append(nodes, &graph.Node{
				ID: c.ID, Kind: graph.KindContract, Name: c.ID, FilePath: c.FilePath, Language: "contract",
				RepoPrefix: c.RepoPrefix, WorkspaceID: c.EffectiveWorkspace(), ProjectID: c.EffectiveProject(),
				Meta: map[string]any{
					"type": string(c.Type), "role": string(c.Role), "symbol_id": c.SymbolID,
					"line": c.Line, "confidence": c.Confidence, "contract_meta": c.Meta,
					"contract_owner_record": ownerID != "",
				},
			})
		}
		if ownerID == "" {
			continue
		}
		kind := graph.EdgeProvides
		if c.Role == contracts.RoleConsumer {
			kind = graph.EdgeConsumes
		}
		edges = append(edges, &graph.Edge{
			From: ownerID, To: c.ID, Kind: kind, FilePath: c.FilePath, Line: c.Line, Meta: contractOwnerEdgeMeta(c),
		})
		// File ownership does not make a symbol-less provider a route handler.
		if c.SymbolID != "" && c.Role == contracts.RoleProvider && isRouteContractType(c.Type) {
			meta := contractOwnerEdgeMeta(c)
			meta["contract_type"] = string(c.Type)
			edges = append(edges, &graph.Edge{
				From: c.SymbolID, To: c.ID, Kind: graph.EdgeHandlesRoute,
				FilePath: c.FilePath, Line: c.Line, Meta: meta,
			})
		}
	}
	return nodes, edges, missingSourceOwners
}

// Cross-file constructs promote the existing contract-file dependency set.
// Changed shared inputs additionally include previous-empty consumers, which
// have no registry record to discover. Ordinary edits remain file-local.
func (idx *Indexer) expandIncrementalContractFrontier(files []string, reg *contracts.Registry) []string {
	needsDependencies := idx.contractDependenciesChanged
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
	if idx.contractSharedInputsChanged {
		for node := range graph.NodesInScopeSeq(idx.graph, []string{idx.repoPrefix}, nil, graph.KindFile) {
			if node != nil && node.FilePath != "" {
				files = append(files, node.FilePath)
			}
		}
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
	// The same holds for an ID the changed files introduce: a record another
	// file already holds for it may be the one that writes its node
	// (contractGraphRows), so every record of every touched ID is re-emitted.
	changedIDs := make(map[string]struct{}, len(current))
	for _, contract := range current {
		changedIDs[contract.ID] = struct{}{}
	}
	for _, id := range ids {
		delete(changedIDs, id)
	}
	newIDs := make([]string, 0, len(changedIDs))
	for id := range changedIDs {
		newIDs = append(newIDs, id)
	}
	sort.Strings(newIDs)
	for _, id := range newIDs {
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

	nodes, edges, missingOwners := contractGraphRows(idx.graph, unique, true)
	touchedIDs := append([]string(nil), ids...)
	for _, contract := range unique {
		touchedIDs = append(touchedIDs, contract.ID)
	}
	idx.warnMissingContractOwners(missingOwners)
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

// contractNodeRecordLess orders the records of one contract ID for the choice
// of the record its node carries: by ID, then file, symbol, role and line —
// the registry key's order, completed by the line.
func contractNodeRecordLess(a, b contracts.Contract) bool {
	switch {
	case a.ID != b.ID:
		return a.ID < b.ID
	case a.FilePath != b.FilePath:
		return a.FilePath < b.FilePath
	case a.SymbolID != b.SymbolID:
		return a.SymbolID < b.SymbolID
	case a.Role != b.Role:
		return a.Role < b.Role
	default:
		return a.Line < b.Line
	}
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
	if language == "python" && (strings.Contains(lowerSource, "include_router") ||
		strings.Contains(lowerSource, "register_blueprint") || strings.Contains(lowerSource, "apirouter") || strings.Contains(lowerSource, "blueprint") || strings.Contains(lowerSource, "include")) {
		return true
	}
	if (language == "typescript" || language == "javascript") &&
		(strings.Contains(lowerSource, ".use") || strings.Contains(lowerSource, "@controller") || strings.Contains(lowerSource, "routermodule")) {
		return true
	}
	if language == "rust" && (strings.Contains(lowerSource, ".nest") || strings.Contains(lowerSource, ".merge") ||
		strings.Contains(lowerSource, ".configure") || strings.Contains(lowerSource, ".service")) {
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

// Apply the same record normalization as cold extraction before owner payloads
// are persisted. A later canonical-node patch cannot repair stale owner Meta.
func (idx *Indexer) normalizeFreshContractRecords(records []contracts.Contract) []contracts.Contract {
	if len(records) == 0 {
		return records
	}
	local := contracts.NewRegistry()
	for _, record := range records {
		record.WorkspaceID, record.ProjectID = record.EffectiveWorkspace(), record.EffectiveProject()
		local.Add(record)
	}
	idx.upgradeContractBareTypeRefs(local)
	idx.resolveProviderHandlers(local)
	idx.resolveCallReturnTypes(local)
	idx.snapshotContractShapes(local)
	idx.inlineEnvelopeShapes(local)
	return local.All()
}
