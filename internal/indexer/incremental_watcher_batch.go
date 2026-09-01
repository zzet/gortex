package indexer

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/reach"
)

// watcherBatchReindex is the common large-change entry point used by both the
// filesystem storm drainer and the git ref reconciler. Production MultiWatcher
// instances replace the per-indexer fallback with MultiIndexer so the exact
// mutation frontier also receives the shared resolver and derived catch-up.
type watcherBatchReindex func(paths []string) (*IndexResult, error)

// incrementalReindexPathsWithReceipt keeps the mutation receipt boundary tight:
// only the bounded parse/evict pipeline is observed. Resolution and derived
// mutations happen after the receipt has closed and therefore cannot widen the
// next incremental frontier.
func (idx *Indexer) incrementalReindexPathsWithReceipt(
	root string,
	paths []string,
	detectDeletions bool,
) (result *IndexResult, receipt *graph.MutationReceipt, batch *reparsePendingEnrichmentBatch, err error) {
	return idx.incrementalReindexPathsWithReceiptMode(root, paths, incrementalPathMode{
		detectDeletions: detectDeletions,
	})
}

func (idx *Indexer) incrementalReindexPathsWithReceiptMode(
	root string,
	paths []string,
	mode incrementalPathMode,
) (result *IndexResult, receipt *graph.MutationReceipt, batch *reparsePendingEnrichmentBatch, err error) {
	batch = &reparsePendingEnrichmentBatch{deferResolverCatchup: true}
	receiptStore, _ := idx.graph.(graph.MutationReceiptStore)
	if receiptStore == nil {
		result, err = idx.incrementalReindexPathsMode(root, paths, mode, batch)
		return result, nil, batch, err
	}

	token := receiptStore.BeginMutationReceipt()
	defer func() {
		observed := receiptStore.EndMutationReceipt(token)
		receipt = &observed
	}()
	result, err = idx.incrementalReindexPathsMode(root, paths, mode, batch)
	return result, receipt, batch, err
}

// incrementalResolutionFrontier chooses the narrowest quality-safe resolver
// scope. A complete receipt is authoritative. Stores without receipt support
// can still use the successful-file DerivedInvalidationPlan emitted by the
// bounded pipeline. An incomplete receipt takes that conservative scoped
// fallback once; only a complete irrelevant receipt can prove no catch-up work.
func incrementalResolutionFrontier(
	result *IndexResult,
	receipt *graph.MutationReceipt,
) (files []string, needed bool, exact bool) {
	if result == nil {
		return nil, false, true
	}
	if receipt != nil {
		if !receipt.Complete {
			files = appendUniqueSorted(nil, result.DerivedInvalidation.Files...)
			return files, true, false
		}
		if !receipt.ResolutionRelevant {
			return nil, false, true
		}
		files = receipt.ResolutionFiles()
		if len(files) == 0 {
			files = appendUniqueSorted(nil, result.DerivedInvalidation.Files...)
			return files, true, false
		}
		return files, true, true
	}

	files = append([]string(nil), result.DerivedInvalidation.Files...)
	if len(files) == 0 {
		if result.StaleFileCount > 0 || result.DeletedFileCount > 0 {
			return nil, true, false
		}
		return nil, false, true
	}
	sort.Strings(files)
	files = compactSortedStrings(files)
	return files, true, true
}

func compactSortedStrings(values []string) []string {
	if len(values) < 2 {
		return values
	}
	write := 1
	for read := 1; read < len(values); read++ {
		if values[read] == values[write-1] {
			continue
		}
		values[write] = values[read]
		write++
	}
	return values[:write]
}

func (idx *Indexer) observeIncrementalCatchup(kind string, files []string) {
	if idx == nil || idx.incrementalCatchupHook == nil {
		return
	}
	idx.incrementalCatchupHook(kind, append([]string(nil), files...))
}

// runIncrementalResolutionCatchup owns every resolution-dependent tail for a
// deferred incremental mutation. The changed-file resolver, dataflow rewrite,
// affected-by resolver and durable fact refresh each run at most once for the
// complete batch.
func (idx *Indexer) runIncrementalResolutionCatchup(
	files []string,
	batch *reparsePendingEnrichmentBatch,
	resolveFiles func([]string),
) []string {
	files = appendUniqueSorted(nil, files...)
	if len(files) == 0 {
		return nil
	}
	idx.observeIncrementalCatchup("resolve", files)
	resolveFiles(files)

	idx.observeIncrementalCatchup("dataflow", files)
	idx.materializeDataflowParamsForFiles(files)

	affected := batch.deferredAffectedPlan()
	idx.executeAffectedByPlan(affected)

	// Facts are rebuilt once, after both resolver frontiers are current. This
	// avoids one delete/set transaction per structural chunk and also ensures an
	// affected caller that degraded to a stub drops its stale durable fact.
	factFiles := appendUniqueSorted(files, affected.files...)
	idx.observeIncrementalCatchup("ref_facts", factFiles)
	idx.persistRefFactsForFiles(factFiles)
	return factFiles
}

func (idx *Indexer) runIncrementalWatcherSemantic(graphPaths []string) []string {
	if idx == nil || idx.semanticMgr == nil || !idx.semanticMgr.Enabled() ||
		!idx.semanticMgr.HasProviders() || !idx.semanticMgr.EnrichesOnWatch() {
		return nil
	}
	pendingFiles, _, _ := idx.deferredEnrichScope()
	if len(pendingFiles) == 0 {
		pendingFiles = appendUniqueSorted(nil, graphPaths...)
	}
	idx.observeIncrementalCatchup("semantic", pendingFiles)
	idx.runDeferredEnrich()
	if idx.pendingEnrich.Load() {
		return pendingFiles
	}
	pending := make(map[string]bool, len(graphPaths))
	for _, graphPath := range graphPaths {
		if graphPath != "" {
			pending[graphPath] = false
		}
	}
	idx.setReparsePendingEnrichments(pending)
	return pendingFiles
}

// runIncrementalPointSemantic preserves IndexFile's exact single-save semantic
// contract. Storm and reconciliation batches use runIncrementalWatcherSemantic
// to amortize provider work; one explicit point mutation calls EnrichFile so
// in-process type providers can confirm the new edges before IndexFile returns.
func (idx *Indexer) runIncrementalPointSemantic(graphPaths []string) []string {
	if idx == nil || idx.semanticMgr == nil || !idx.semanticMgr.Enabled() ||
		!idx.semanticMgr.HasProviders() || !idx.semanticMgr.EnrichesOnWatch() {
		return nil
	}
	paths := appendUniqueSorted(nil, graphPaths...)
	if len(paths) == 0 {
		return nil
	}
	idx.observeIncrementalCatchup("semantic", paths)
	pending := make(map[string]bool, len(paths))
	for _, graphPath := range paths {
		pending[graphPath] = true
		if _, err := idx.semanticMgr.EnrichFile(idx.graph, idx.rootPath, graphPath); err != nil {
			idx.logger.Debug("indexer: exact point semantic enrichment failed",
				zap.String("file", graphPath), zap.Error(err))
			continue
		}
		pending[graphPath] = false
	}
	idx.setReparsePendingEnrichments(pending)
	return paths
}

// incrementalReindexWatcherPaths is the complete direct-Indexer coordinator used
// outside a MultiIndexer. It performs one bounded parse/evict batch, one exact
// resolver/reference-fact catch-up, semantic refresh, and the precise standalone
// derived passes. Shared-graph callers use MultiIndexer so cross-repository work
// sees every sibling registry and indexer.
func incrementalTopologyChanged(result *IndexResult) bool {
	if result == nil {
		return false
	}
	if result.DeletedFileCount > 0 || result.FullRetrack {
		return true
	}
	plan := result.DerivedInvalidation
	nonTopologyFiles := plan.InertFiles
	// Metadata refreshes preserve node and edge identity. Files is populated for
	// their exact derived frontier, so only the topology-bearing plan fields
	// below disqualify them from the no-invalidation path.
	metadataTopologyFree := plan.Flags == 0 &&
		len(plan.TypeIDs) == 0 &&
		len(plan.ContractGroups) == 0 &&
		len(plan.ContractSymbolIDs) == 0 &&
		len(plan.ContractBridgeNodeIDs) == 0 &&
		!plan.LegacyFallback
	if metadataTopologyFree {
		nonTopologyFiles += plan.MetadataOnlyFiles
	}
	return result.StaleFileCount > nonTopologyFiles
}

func (idx *Indexer) incrementalReindexWatcherPaths(root string, paths []string) (*IndexResult, error) {
	return idx.incrementalWatcherPaths(root, paths, incrementalPathMode{detectDeletions: true})
}

func (idx *Indexer) incrementalDiscoverWatcherPaths(root string, paths []string) (*IndexResult, error) {
	return idx.incrementalWatcherPaths(root, paths, incrementalPathMode{})
}

// incrementalEvictWatcherPath is the standalone already-held-lane forced
// deletion executor. Unlike incrementalPointWatcherPath it never consults disk
// presence to decide between reparse and deletion.
func (idx *Indexer) incrementalEvictWatcherPath(path string) (nodesRemoved, edgesRemoved int, err error) {
	finishTopologyMutation := reach.BeginTopologyMutation(idx.graph)
	topologyChanged := true
	defer func() { finishTopologyMutation(topologyChanged) }()

	// Captured BEFORE eviction: deleting a global-usings file (or a
	// csproj, which draws the unit boundaries) removes visibility every
	// dependent's extension bind was narrowed under.
	graphPath := idx.prefixPath(filepath.FromSlash(path))
	evictedGlobals := csharpVisibilityStampForNodes(idx.graph.GetFileNodes(graphPath)).globals != "" ||
		strings.HasSuffix(strings.ToLower(graphPath), ".csproj")

	eviction := idx.evictFileIncrementalRaw(path)
	result := eviction.result
	files, needed, exact := incrementalResolutionFrontier(result, nil)
	if needed && len(files) > 0 {
		idx.runIncrementalResolutionCatchup(files, eviction.batch, func(frontier []string) {
			if idx.incrementalResolveFilesHook != nil {
				idx.incrementalResolveFilesHook(frontier)
				return
			}
			idx.resolver.ResolveFilesAndIncoming(frontier)
		})
	} else if needed && !exact {
		idx.observeIncrementalCatchup("resolve", nil)
		idx.ResolveAll()
	}
	if evictedGlobals {
		idx.reresolveCSharpGlobalUsingDependents(
			[]string{graphPath}, map[string]struct{}{graphPath: {}})
	}
	if result != nil && result.DeletedFileCount > 0 {
		idx.runIncrementalWatcherSemantic(result.DerivedInvalidation.Files)
		idx.observeIncrementalCatchup("derived", result.DerivedInvalidation.Files)
		idx.runStandaloneIncrementalDerivedPasses(result.DerivedInvalidation)
	}
	topologyChanged = incrementalTopologyChanged(result)
	return eviction.nodesRemoved, eviction.edgesRemoved, nil
}

// incrementalPointWatcherPath is the standalone, already-held-lane point
// executor. It forces the explicit path through content classification and the
// complete resolver/semantic/derived tail without re-entering a public
// coordinated API.
func (idx *Indexer) incrementalPointWatcherPath(root, path string) (*IndexResult, error) {
	return idx.incrementalWatcherPaths(root, []string{path}, incrementalPathMode{
		detectDeletions:    true,
		forceExplicitFiles: true,
	})
}

func (idx *Indexer) incrementalWatcherPaths(root string, paths []string, mode incrementalPathMode) (*IndexResult, error) {
	// The coordinator owns the repository lane before invoking this executor.
	// Take the reachability topology gate second and retain it through resolver
	// and derived catch-up so readers observe the complete old or new graph.
	finishTopologyMutation := reach.BeginTopologyMutation(idx.graph)
	topologyChanged := true
	defer func() { finishTopologyMutation(topologyChanged) }()

	result, receipt, batch, err := idx.incrementalReindexPathsWithReceiptMode(root, paths, mode)
	if err != nil {
		return result, err
	}
	files, needed, exact := incrementalResolutionFrontier(result, receipt)
	if needed && len(files) > 0 {
		idx.runIncrementalResolutionCatchup(files, batch, func(frontier []string) {
			if idx.incrementalResolveFilesHook != nil {
				idx.incrementalResolveFilesHook(frontier)
				return
			}
			idx.resolver.ResolveFilesAndIncoming(frontier)
		})
		idx.resolveReceiptNamePendings(receipt)
	} else if needed && !exact {
		idx.observeIncrementalCatchup("resolve", nil)
		idx.ResolveAll()
	}
	if mode.forceExplicitFiles || !idx.deferGlobalPasses.Load() {
		if mode.exactPointSemantic {
			idx.runIncrementalPointSemantic(result.DerivedInvalidation.Files)
		} else {
			idx.runIncrementalWatcherSemantic(result.DerivedInvalidation.Files)
		}
		idx.observeIncrementalCatchup("derived", result.DerivedInvalidation.Files)
		idx.runStandaloneIncrementalDerivedPasses(result.DerivedInvalidation)
	}
	topologyChanged = incrementalTopologyChanged(result)
	return result, nil
}

// resolveReceiptNamePendings rebinds pending references parked under the
// unresolved stubs of names an exact receipt recorded (evicted definitions):
// those references are reachable by name only, never by file frontier — the
// definition's file no longer declares the name, so the file-scoped incoming
// pass above cannot enumerate their stub.
func (idx *Indexer) resolveReceiptNamePendings(receipt *graph.MutationReceipt) {
	if receipt == nil || !receipt.Complete || len(receipt.EvictedNames) == 0 || idx.resolver == nil {
		return
	}
	idx.resolver.ResolveIncomingForNames(receipt.EvictedNames, []string{idx.repoPrefix})
}

func (w *Watcher) reindexStormPaths(paths []string) (*IndexResult, error) {
	if w.batchReindex != nil {
		return w.batchReindex(paths)
	}
	if w.indexer == nil || w.indexer.rootPath == "" {
		return nil, fmt.Errorf("watcher: index root is not initialized")
	}
	return w.indexer.IncrementalReindexPaths(w.indexer.rootPath, paths)
}

func (gw *GitWatcher) reindexChangedPaths(paths []string) (*IndexResult, error) {
	if gw.batchReindex != nil {
		return gw.batchReindex(paths)
	}
	if gw.indexer == nil {
		return nil, fmt.Errorf("git-watcher: indexer is not initialized")
	}
	return gw.indexer.IncrementalReindexPaths(gw.repoPath, paths)
}

// resolveIncrementalRepoMutation runs one same-repo pass and one coordinated
// cross-repo outgoing-plus-incoming pass for a complete receipt frontier or,
// when the store cannot certify its eviction shape, the indexer's conservative
// successful-file frontier. A whole-graph fallback is reserved for the
// exceptional case where neither source yields any safe scope.
func (mi *MultiIndexer) resolveIncrementalRepoMutation(
	repoPrefix string,
	result *IndexResult,
	receipt *graph.MutationReceipt,
	batch *reparsePendingEnrichmentBatch,
) {
	mi.resolveIncrementalRepoMutationMode(repoPrefix, result, receipt, batch, false)
}

func (mi *MultiIndexer) resolveIncrementalRepoMutationMode(
	repoPrefix string,
	result *IndexResult,
	receipt *graph.MutationReceipt,
	batch *reparsePendingEnrichmentBatch,
	exactPointSemantic bool,
) {
	files, needed, _ := incrementalResolutionFrontier(result, receipt)
	crossRepoFiles := appendUniqueSorted(nil, files...)
	if result != nil {
		crossRepoFiles = appendUniqueSorted(crossRepoFiles, result.DerivedInvalidation.Files...)
	}
	idx := mi.GetIndexer(repoPrefix)
	if needed && len(files) > 0 && idx != nil {
		resolvedFiles := idx.runIncrementalResolutionCatchup(files, batch, func(frontier []string) {
			if idx.incrementalResolveFilesHook != nil {
				idx.incrementalResolveFilesHook(frontier)
				return
			}
			mi.runMasterResolveFiles(frontier, false)
		})
		crossRepoFiles = appendUniqueSorted(crossRepoFiles, resolvedFiles...)
		if receipt != nil && receipt.Complete {
			mi.runMasterResolveNames(receipt.EvictedNames)
		}
	} else if needed && len(files) > 0 {
		mi.runMasterResolveFiles(files, false)
		if receipt != nil && receipt.Complete {
			mi.runMasterResolveNames(receipt.EvictedNames)
		}
	} else if needed {
		scope := map[string]struct{}{repoPrefix: {}}
		if repoPrefix == "" {
			scope = nil
		}
		mi.runMasterResolve(scope, false)
		// No scoped frontier survived. Refresh any known files without adding a
		// second graph-wide dataflow or durable-fact scan.
		if idx != nil && result != nil {
			known := appendUniqueSorted(nil, result.DerivedInvalidation.Files...)
			crossRepoFiles = appendUniqueSorted(crossRepoFiles, known...)
			idx.observeIncrementalCatchup("dataflow", known)
			idx.materializeDataflowParamsForFiles(known)
			idx.observeIncrementalCatchup("ref_facts", known)
			idx.persistRefFactsForFiles(known)
		}
	}

	var semanticFiles []string
	if idx != nil && result != nil {
		if exactPointSemantic {
			semanticFiles = idx.runIncrementalPointSemantic(result.DerivedInvalidation.Files)
		} else {
			semanticFiles = idx.runIncrementalWatcherSemantic(result.DerivedInvalidation.Files)
		}
		crossRepoFiles = appendUniqueSorted(crossRepoFiles, semanticFiles...)
	}
	if needed || len(semanticFiles) > 0 {
		if idx != nil {
			idx.observeIncrementalCatchup("cross_repo", crossRepoFiles)
		}
		if len(crossRepoFiles) > 0 {
			mi.runCrossRepoResolveFiles(crossRepoFiles)
		} else {
			mi.runCrossRepoResolve(false)
		}
	}
}
