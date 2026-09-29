package indexer

import (
	"context"
	"time"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/entrypoints"
	"github.com/zzet/gortex/internal/reach"
	"github.com/zzet/gortex/internal/resolver"
)

// IncrementalDerivedReport exposes the work selected by the changed-file
// invalidation plans. It is intentionally count-only so telemetry remains
// bounded even for a large workspace.
type IncrementalDerivedReport struct {
	Repos          int
	Files          int
	TypeIDs        int
	LegacyFallback bool
	Implements     int
	Overrides      int
	TestSymbols    int
	TestEdges      int
	Capability     int
	// EntryPointHierarchy counts entry-point stamps (aspnet:controller
	// today) propagated down resolved extends chains on declaration-shape
	// changes.
	EntryPointHierarchy int
	// Framework preserves the legacy attempted/landed count. It is not an
	// inserted-edge delta; FrameworkPer and the phase timings expose the actual
	// owner of a slow incremental settle without changing that compatibility.
	Framework              int
	FrameworkPer           []resolver.SynthCount
	FrameworkGated         int
	FrameworkReceiverGated int
	FrameworkCensusMs      int64
	FrameworkScopeMs       int64
	FrameworkGateMs        int64
	FrameworkClaimMs       int64
	FrameworkDemoteMs      int64
	FrameworkScopeRows     int
	FrameworkScopeBytes    int
	ExternalCalls          int
	CrossRepo              int
	Contracts              int
	DurationMs             int64
	// Per-family wall times of the passes above, so a slow settle names its
	// owner: hierarchy (implements/overrides/entry-point propagation), test
	// projection, capability synthesis, framework dispatch, external-call and
	// cross-repository synthesis, and contract reconciliation.
	HierarchyMs     int64
	TestsMs         int64
	CapabilityMs    int64
	FrameworkMs     int64
	ExternalCallsMs int64
	CrossRepoMs     int64
	ContractsMs     int64
}

// runStandaloneIncrementalDerivedPasses reuses the exact MultiIndexer derived
// coordinator for a direct Indexer. The synthetic coordinator intentionally
// contains only this repository; real shared-graph callers must use their owning
// MultiIndexer so contract and cross-repository passes see every sibling.
func (idx *Indexer) runStandaloneIncrementalDerivedPasses(plan DerivedInvalidationPlan) IncrementalDerivedReport {
	return idx.runStandaloneIncrementalDerivedPassesWithPrior(plan, nil)
}

// runStandaloneIncrementalDerivedPassesWithPrior is the form an incremental
// mutation uses for its own catch-up: prior is the capability state its reparse
// read before eviction (nil runs the full capability frontier).
func (idx *Indexer) runStandaloneIncrementalDerivedPassesWithPrior(
	plan DerivedInvalidationPlan,
	prior *capabilityPrior,
) IncrementalDerivedReport {
	if idx == nil || idx.graph == nil || idx.deferGlobalPasses.Load() || plan.Empty() {
		return IncrementalDerivedReport{}
	}
	logger := idx.logger
	if logger == nil {
		logger = zap.NewNop()
	}
	prefix := idx.RepoPrefix()
	mi := NewMultiIndexer(idx.graph, idx.registry, idx.search, nil, logger)
	mi.indexers[prefix] = idx
	mi.repos[prefix] = &RepoMetadata{
		RepoPrefix: prefix,
		RootPath:   idx.RootPath(),
	}
	return mi.runIncrementalDerivedPassesWithPriorTopologyHeld(context.Background(), map[string]DerivedInvalidationPlan{
		prefix: plan,
	}, prior)
}

// drainRetargetedTestCallFiles collects, from each named repo's resolver,
// the caller files of test-classified calls the resolution step just bound
// (see resolver.Resolver.TakeRetargetedTestCallFiles). Destructive: the
// per-resolver frontier is cleared.
func (mi *MultiIndexer) drainRetargetedTestCallFiles(prefixSet map[string]struct{}) []string {
	mi.mu.RLock()
	resolvers := make([]*resolver.Resolver, 0, len(prefixSet))
	for prefix := range prefixSet {
		if idx := mi.indexers[prefix]; idx != nil && idx.resolver != nil {
			resolvers = append(resolvers, idx.resolver)
		}
	}
	mi.mu.RUnlock()
	var files []string
	for _, r := range resolvers {
		files = append(files, r.TakeRetargetedTestCallFiles()...)
	}
	if len(files) == 0 {
		return nil
	}
	return appendUniqueSorted(nil, files...)
}

// discardRetargetedTestCallFiles drops the per-repo retarget frontiers a
// graph-wide (or repo-scoped) test projection has just superseded. A nil
// prefix set discards every tracked repo's frontier. Destructive by
// design: the callers noted there are already covered by the projection
// that just ran, so draining them later would only re-project them.
func (mi *MultiIndexer) discardRetargetedTestCallFiles(prefixes map[string]bool) {
	mi.mu.RLock()
	defer mi.mu.RUnlock()
	for prefix, idx := range mi.indexers {
		if idx == nil || idx.resolver == nil {
			continue
		}
		if prefixes != nil && !prefixes[prefix] {
			continue
		}
		idx.resolver.TakeRetargetedTestCallFiles()
	}
}

// RunIncrementalDerivedPasses executes only the derived families invalidated
// by the exact per-file plans. A legacy database without persisted fingerprints
// takes the old scoped-global path once; ordinary body/metadata edits never do.
func (mi *MultiIndexer) RunIncrementalDerivedPasses(
	ctx context.Context,
	plans map[string]DerivedInvalidationPlan,
) IncrementalDerivedReport {
	if mi == nil || mi.graph == nil || len(plans) == 0 {
		return IncrementalDerivedReport{}
	}
	if ctx != nil {
		select {
		case <-ctx.Done():
			return IncrementalDerivedReport{}
		default:
		}
	}
	hasWork := false
	for _, plan := range plans {
		if !plan.Empty() {
			hasWork = true
			break
		}
	}
	if !hasWork {
		return IncrementalDerivedReport{}
	}
	mi.batchMutationGate.Lock()
	defer mi.batchMutationGate.Unlock()
	if ctx != nil {
		select {
		case <-ctx.Done():
			return IncrementalDerivedReport{}
		default:
		}
	}
	finishTopologyMutation := reach.BeginTopologyMutation(mi.graph)
	defer finishTopologyMutation(true)
	return mi.runIncrementalDerivedPassesTopologyHeld(ctx, plans)
}

// runIncrementalDerivedPassesTopologyHeld is the non-reentrant implementation
// for mutation pipelines that already own the global topology writer.
func (mi *MultiIndexer) runIncrementalDerivedPassesTopologyHeld(
	ctx context.Context,
	plans map[string]DerivedInvalidationPlan,
) IncrementalDerivedReport {
	return mi.runIncrementalDerivedPassesWithPriorTopologyHeld(ctx, plans, nil)
}

// runIncrementalDerivedPassesWithPriorTopologyHeld also takes the capability
// state the mutation's own reparse captured before eviction. When it covers
// every file of the frontier, the capability pass re-derives only the changed
// files, the sources whose edges into them the eviction deleted, and the
// receiver callers of methods whose mutated-field set changed; otherwise (nil
// or partial) it runs the full frontier.
func (mi *MultiIndexer) runIncrementalDerivedPassesWithPriorTopologyHeld(
	ctx context.Context,
	plans map[string]DerivedInvalidationPlan,
	prior *capabilityPrior,
) IncrementalDerivedReport {
	started := time.Now()
	report := IncrementalDerivedReport{}
	if mi == nil || mi.graph == nil || len(plans) == 0 {
		return report
	}

	var merged DerivedInvalidationPlan
	prefixSet := make(map[string]struct{}, len(plans))
	prefixScope := make(map[string]bool, len(plans))
	for prefix, plan := range plans {
		if plan.Empty() {
			continue
		}
		merged.Merge(plan)
		prefixSet[prefix] = struct{}{}
		prefixScope[prefix] = true
	}
	if len(prefixSet) == 0 {
		return report
	}
	report.Repos = len(prefixSet)
	report.Files = len(merged.Files)
	report.TypeIDs = len(merged.TypeIDs)
	report.LegacyFallback = merged.LegacyFallback

	// A single unprefixed repo cannot use repo-scoped readers. Falling back to
	// nil here preserves the old whole-graph behavior, which is still bounded
	// to that one repository.
	scopedPrefixes := prefixScope
	if prefixScope[""] {
		scopedPrefixes = nil
	}

	// A pre-fingerprint row cannot classify which derived family changed, so
	// its producer conservatively sets every relevant flag. The file/type/
	// contract frontiers are still exact: escalating that first edit to a
	// repository-wide global pass made an upgraded warm daemon spend minutes
	// rebuilding unrelated framework edges. Run the exact coordinator below
	// with the conservative flags and retain LegacyFallback as telemetry.
	if ctx != nil {
		select {
		case <-ctx.Done():
			report.DurationMs = time.Since(started).Milliseconds()
			return report
		default:
		}
	}

	// Batched IndexFile calls deliberately do not update the clone index because
	// its old function signatures are gone by the time this coordinator runs.
	// Mark incompleteness rather than rebuilding a whole repo on an edit; the
	// explicit clone/global consumer owns the eventual bounded rebuild.
	mi.mu.RLock()
	for prefix := range prefixSet {
		if idx := mi.indexers[prefix]; idx != nil && idx.cloneIndex != nil {
			idx.cloneIndex.MarkPending()
		}
	}
	mi.mu.RUnlock()

	if merged.Flags.Has(DerivedInvalidatesDeclarations) && len(merged.TypeIDs) > 0 {
		typeFrontier := make(map[string]bool, len(merged.TypeIDs))
		for _, id := range merged.TypeIDs {
			typeFrontier[id] = true
		}
		phase := time.Now()
		// Same edges as InferImplementsScoped / InferOverridesScoped over the
		// frontier, read from the frontier's own adjacency rather than the
		// repository's member_of and parent-edge streams (4-12 s per save).
		r := resolver.New(mi.graph)
		report.Implements = r.InferImplementsForFrontier(typeFrontier)
		report.Overrides = r.InferOverridesForFrontier(typeFrontier)
		report.HierarchyMs += time.Since(phase).Milliseconds()
	}

	// The resolution step of this same apply may have bound pending calls
	// whose TEST callers live outside merged.Files (the definition-side
	// plan never names the caller file, and its flags may be declaration-
	// only). Drain that retargeted frontier and reconcile those callers
	// regardless of plan flags — without it a call that resolves later
	// than its projection pass never gains its EdgeTests.
	phase := time.Now()
	retargeted := mi.drainRetargetedTestCallFiles(prefixSet)
	if merged.Flags.Has(DerivedInvalidatesRuntime) || merged.Flags.Has(DerivedInvalidatesTests) {
		files := merged.Files
		if len(files) > 0 && len(retargeted) > 0 {
			files = appendUniqueSorted(append([]string(nil), files...), retargeted...)
		}
		report.TestSymbols, report.TestEdges = markTestSymbolsAndEmitEdgesScoped(mi.graph, scopedPrefixes, files...)
	} else if len(retargeted) > 0 {
		report.TestSymbols, report.TestEdges = markTestSymbolsAndEmitEdgesScoped(mi.graph, scopedPrefixes, retargeted...)
	}
	report.TestsMs = time.Since(phase).Milliseconds()
	if merged.Flags.Has(DerivedInvalidatesDeclarations) && len(merged.TypeIDs) > 0 {
		phase = time.Now()
		// A save that adds/re-parents a type can hang a new derived type
		// off a stamped hierarchy (or stamp a new base); the changed-type
		// frontier drives seed discovery, like the scoped passes above.
		report.EntryPointHierarchy = entrypoints.PropagateEntryPointsDownHierarchyScoped(mi.graph, merged.TypeIDs)
		report.HierarchyMs += time.Since(phase).Milliseconds()
	}
	if merged.Flags.Has(DerivedInvalidatesRuntime) {
		phase = time.Now()
		var readsEnv, execProc, fields int
		if prior.covers(merged.Files) {
			readsEnv, execProc, fields = synthesizeCapabilityEdgesForFilesWithPrior(mi.graph, prior, merged.Files)
		} else {
			readsEnv, execProc, fields = synthesizeCapabilityEdgesScoped(mi.graph, scopedPrefixes, merged.Files...)
		}
		report.Capability = readsEnv + execProc + fields
		report.CapabilityMs = time.Since(phase).Milliseconds()
	} else if prior.covers(merged.Files) {
		// The runtime fingerprint is unchanged, but every file here was
		// structurally re-parsed: the eviction deleted the capability rows its
		// nodes source (and the ones pointing at its re-keyed definitions), and
		// nothing else re-derives them. The prior bounds that to the files and
		// the sources whose rows the eviction deleted.
		phase = time.Now()
		readsEnv, execProc, fields := synthesizeCapabilityEdgesForFilesWithPrior(mi.graph, prior, merged.Files)
		report.Capability = readsEnv + execProc + fields
		report.CapabilityMs = time.Since(phase).Milliseconds()
	}
	if merged.Flags.Has(DerivedInvalidatesDeclarations) ||
		merged.Flags.Has(DerivedInvalidatesImports) ||
		merged.Flags.Has(DerivedInvalidatesRuntime) {
		phase = time.Now()
		framework := resolver.RunFrameworkSynthesizersScopedForFilesWithSelection(
			mi.graph,
			scopedPrefixes,
			merged.Files,
			merged.CSharpHierarchyChanged,
			mi.frameworkSynthesizerSelection(scopedPrefixes),
		)
		report.Framework = framework.Total
		report.FrameworkPer = framework.Per
		report.FrameworkGated = framework.Gated
		report.FrameworkReceiverGated = framework.ReceiverGated
		report.FrameworkCensusMs = framework.CensusMillis
		report.FrameworkScopeMs = framework.ScopeMillis
		report.FrameworkGateMs = framework.GateMillis
		report.FrameworkClaimMs = framework.ClaimMillis
		report.FrameworkDemoteMs = framework.DemoteMillis
		report.FrameworkScopeRows = framework.ScopeRows
		report.FrameworkScopeBytes = framework.ScopeBytes
		report.FrameworkMs = time.Since(phase).Milliseconds()
		phase = time.Now()
		report.ExternalCalls = resolver.SynthesizeExternalCallsForFiles(
			mi.graph, mi.externalCallSynthesisEnabled(), merged.Files,
		)
		report.ExternalCallsMs = time.Since(phase).Milliseconds()
		phase = time.Now()
		report.CrossRepo = resolver.DetectCrossRepoEdgesForFiles(mi.graph, merged.Files)
		report.CrossRepoMs = time.Since(phase).Milliseconds()
	}
	if merged.Flags.Has(DerivedInvalidatesContracts) {
		phase = time.Now()
		report.Contracts = mi.ReconcileContractEdgesForFrontier(merged)
		report.ContractsMs = time.Since(phase).Milliseconds()
	}

	report.DurationMs = time.Since(started).Milliseconds()
	mi.logIncrementalDerived(report, merged)
	return report
}

func (mi *MultiIndexer) logIncrementalDerived(report IncrementalDerivedReport, plan DerivedInvalidationPlan) {
	mi.logger.Info("incremental derived passes complete",
		zap.Int("repos", report.Repos),
		zap.Int("files", report.Files),
		zap.Int("type_ids", report.TypeIDs),
		zap.Uint32("flags", uint32(plan.Flags)),
		zap.Bool("legacy_fallback", report.LegacyFallback),
		zap.Int("implements", report.Implements),
		zap.Int("overrides", report.Overrides),
		zap.Int("test_edges", report.TestEdges),
		zap.Int("entrypoint_hierarchy", report.EntryPointHierarchy),
		zap.Int("capability_edges", report.Capability),
		// Keep framework_edges for log consumers, and add the accurate label:
		// synthesizers report attempted/landed candidates, including idempotent
		// rows that may already be durable.
		zap.Int("framework_edges", report.Framework),
		zap.Int("framework_attempted", report.Framework),
		zap.Any("framework_per_synth", report.FrameworkPer),
		zap.Int("framework_gated", report.FrameworkGated),
		zap.Int("framework_receiver_gated", report.FrameworkReceiverGated),
		zap.Int64("framework_census_ms", report.FrameworkCensusMs),
		zap.Int64("framework_scope_ms", report.FrameworkScopeMs),
		zap.Int64("framework_gate_ms", report.FrameworkGateMs),
		zap.Int64("framework_claim_ms", report.FrameworkClaimMs),
		zap.Int64("framework_demote_ms", report.FrameworkDemoteMs),
		zap.Int("framework_scope_rows", report.FrameworkScopeRows),
		zap.Int("framework_scope_bytes", report.FrameworkScopeBytes),
		zap.Int("external_calls", report.ExternalCalls),
		zap.Int("cross_repo_edges", report.CrossRepo),
		zap.Int("contract_edges", report.Contracts),
		zap.Int64("hierarchy_ms", report.HierarchyMs),
		zap.Int64("tests_ms", report.TestsMs),
		zap.Int64("capability_ms", report.CapabilityMs),
		zap.Int64("framework_ms", report.FrameworkMs),
		zap.Int64("external_calls_ms", report.ExternalCallsMs),
		zap.Int64("cross_repo_ms", report.CrossRepoMs),
		zap.Int64("contracts_ms", report.ContractsMs),
		zap.Int64("duration_ms", report.DurationMs))
}
