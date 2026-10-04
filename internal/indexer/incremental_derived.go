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
	Framework                 int
	FrameworkPer              []resolver.SynthCount
	FrameworkGated            int
	FrameworkReceiverGated    int
	FrameworkCandidateGated   []string
	FrameworkCandidateWitness map[string]string
	// FactoryChainPriorSkipped counts the chains the factory-chain pass did
	// not walk again (resolver.FactoryChainPrior).
	FactoryChainPriorSkipped  int64
	FrameworkCandidateCheckMs int64
	// FnValueReused / FnValueResolved count the fn-value candidates the
	// framework pass republished from the prior and resolved;
	// FnValuePriorUsed reports that the prior was handed to it.
	FnValueReused       int64
	FnValueResolved     int64
	FnValuePriorUsed    bool
	FrameworkCensusMs   int64
	FrameworkScopeMs    int64
	FrameworkGateMs     int64
	FrameworkClaimMs    int64
	FrameworkDemoteMs   int64
	FrameworkScopeRows  int
	FrameworkScopeBytes int
	ExternalCalls       int
	CrossRepo           int
	Contracts           int
	DurationMs          int64
	// Per-family wall times of the passes above, so a slow settle names its
	// owner: hierarchy (implements/overrides/entry-point propagation), test
	// projection, capability synthesis, framework dispatch, external-call and
	// cross-repository synthesis, and contract reconciliation.
	HierarchyMs  int64
	TestsMs      int64
	CapabilityMs int64
	// CapabilitySourcesReused / CapabilityRowsReused / CapabilityRoots /
	// CapabilityExpanded are the capability pass's reuse counters for this
	// run (capability_edges_scoped.go): methods not evaluated as roots,
	// indirect rows taken from the prior, roots evaluated, and roots whose
	// mutated-field set changed.
	CapabilitySourcesReused int64
	CapabilityRowsReused    int64
	CapabilityRoots         int64
	CapabilityExpanded      int64
	// HierarchyRepublished counts the implements/overrides rows republished
	// from the prior instead of re-inferred.
	HierarchyRepublished int
	// PassFaults is the major page faults per derived pass, keyed like the
	// *Ms fields (hierarchy, tests, capability, framework, …).
	PassFaults map[string]int64
	// PassCPUMs is the process CPU per derived pass, keyed like PassFaults:
	// set beside the pass's wall time, it says whether a slow pass worked or
	// waited.
	PassCPUMs       map[string]int64
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
	if idx.contractCoreInputs != nil {
		plan.Flags &^= DerivedInvalidatesContracts
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
	// Major page faults per pass (the store's pages read from disk), keyed
	// like the *_ms fields.
	report.PassFaults = map[string]int64{}
	faultsAt, cpuAt := editDeltaProcessIO(), processCPUTime()
	notePassFaults := func(pass string) {
		now, cpu := editDeltaProcessIO(), processCPUTime()
		if f := now.since(faultsAt).majorFaults; f > 0 {
			report.PassFaults[pass] += f
		}
		if report.PassCPUMs == nil {
			report.PassCPUMs = make(map[string]int64)
		}
		report.PassCPUMs[pass] += (cpu - cpuAt).Milliseconds()
		faultsAt, cpuAt = now, cpu
	}

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

	republishHierarchy := merged.hierarchyUnchanged() && prior.covers(merged.Files)
	// A save the plan does not flag for declarations still re-derived its
	// files (the eviction took the rows recorded at them): their hierarchy,
	// unchanged, is republished as well, or a delta that covers the path
	// masks the lower copy and the rows are lost.
	if !merged.Flags.Has(DerivedInvalidatesDeclarations) && republishHierarchy && len(prior.hierarchy) > 0 {
		phase := time.Now()
		mi.graph.AddBatch(nil, prior.hierarchy)
		report.HierarchyRepublished = len(prior.hierarchy)
		report.HierarchyMs += time.Since(phase).Milliseconds()
	} else if merged.Flags.Has(DerivedInvalidatesDeclarations) && len(merged.TypeIDs) > 0 && republishHierarchy {
		// The files' hierarchy is as it was, so the inference would land
		// exactly the rows it landed before the eviction took them: they are
		// republished instead of re-inferred over the repository's
		// interfaces.
		phase := time.Now()
		if len(prior.hierarchy) > 0 {
			mi.graph.AddBatch(nil, prior.hierarchy)
		}
		report.HierarchyRepublished = len(prior.hierarchy)
		report.HierarchyMs += time.Since(phase).Milliseconds()
	} else if merged.Flags.Has(DerivedInvalidatesDeclarations) && len(merged.TypeIDs) > 0 {
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
		notePassFaults("hierarchy")
	}

	// The resolution step of this same apply may have bound pending calls
	// whose TEST callers live outside merged.Files (the definition-side
	// plan never names the caller file, and its flags may be declaration-
	// only). Drain that retargeted frontier and reconcile those callers
	// regardless of plan flags — without it a call that resolves later
	// than its projection pass never gains its EdgeTests.
	notePassFaults("prelude")
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
	notePassFaults("tests")
	if merged.Flags.Has(DerivedInvalidatesDeclarations) && len(merged.TypeIDs) > 0 {
		phase = time.Now()
		// A save that adds/re-parents a type can hang a new derived type
		// off a stamped hierarchy (or stamp a new base); the changed-type
		// frontier drives seed discovery, like the scoped passes above.
		report.EntryPointHierarchy = entrypoints.PropagateEntryPointsDownHierarchyScoped(mi.graph, merged.TypeIDs)
		report.HierarchyMs += time.Since(phase).Milliseconds()
		notePassFaults("hierarchy")
	}
	capabilityAt := capabilityReuseCounts()
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
		notePassFaults("capability")
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
		notePassFaults("capability")
	}
	frameworkRuns := merged.Flags.Has(DerivedInvalidatesDeclarations) ||
		merged.Flags.Has(DerivedInvalidatesImports) ||
		merged.Flags.Has(DerivedInvalidatesRuntime)
	if !frameworkRuns && prior.covers(merged.Files) && prior.fnValue != nil {
		// The files were re-parsed, so their fn-value registrations went with
		// the eviction; no framework pass runs to land them again.
		phase = time.Now()
		report.Framework = resolver.ResolveFnValueCallbacksForFiles(mi.graph, merged.Files, prior.fnValue)
		report.FrameworkMs = time.Since(phase).Milliseconds()
		notePassFaults("framework")
	}
	if merged.Flags.Has(DerivedInvalidatesDeclarations) ||
		merged.Flags.Has(DerivedInvalidatesImports) ||
		merged.Flags.Has(DerivedInvalidatesRuntime) {
		phase = time.Now()
		frameworkSelection := mi.frameworkSynthesizerSelection(scopedPrefixes)
		if prior.covers(merged.Files) && prior.fnValue != nil {
			// The fn-value gate republishes what the save cannot have moved
			// (resolver.FnValuePrior).
			frameworkSelection = frameworkSelection.WithFnValuePrior(prior.fnValue)
		}
		if prior.covers(merged.Files) && prior.factoryChain != nil {
			frameworkSelection = frameworkSelection.WithFactoryChainPrior(prior.factoryChain)
		}
		factoryChainSkippedAt := resolver.FactoryChainPriorSkipped()
		fnReusedAt, fnResolvedAt := resolver.FnValueGateCounts()
		framework := resolver.RunFrameworkSynthesizersScopedForFilesWithSelection(
			mi.graph,
			scopedPrefixes,
			merged.Files,
			merged.CSharpHierarchyChanged,
			frameworkSelection,
		)
		report.Framework = framework.Total
		report.FrameworkPer = framework.Per
		report.FrameworkGated = framework.Gated
		report.FrameworkReceiverGated = framework.ReceiverGated
		report.FrameworkCandidateGated = framework.CandidateGated
		report.FrameworkCandidateWitness = framework.CandidateWitness
		report.FactoryChainPriorSkipped = resolver.FactoryChainPriorSkipped() - factoryChainSkippedAt
		report.FrameworkCandidateCheckMs = framework.CandidateCheckMillis
		fnReused, fnResolved := resolver.FnValueGateCounts()
		report.FnValueReused, report.FnValueResolved = fnReused-fnReusedAt, fnResolved-fnResolvedAt
		report.FnValuePriorUsed = prior.covers(merged.Files) && prior.fnValue != nil
		report.FrameworkCensusMs = framework.CensusMillis
		report.FrameworkScopeMs = framework.ScopeMillis
		report.FrameworkGateMs = framework.GateMillis
		report.FrameworkClaimMs = framework.ClaimMillis
		report.FrameworkDemoteMs = framework.DemoteMillis
		report.FrameworkScopeRows = framework.ScopeRows
		report.FrameworkScopeBytes = framework.ScopeBytes
		report.FrameworkMs = time.Since(phase).Milliseconds()
		notePassFaults("framework")
		phase = time.Now()
		report.ExternalCalls = resolver.SynthesizeExternalCallsForFiles(
			mi.graph, mi.externalCallSynthesisEnabled(), merged.Files,
		)
		report.ExternalCallsMs = time.Since(phase).Milliseconds()
		notePassFaults("external_calls")
		phase = time.Now()
		report.CrossRepo = resolver.DetectCrossRepoEdgesForFiles(mi.graph, merged.Files)
		report.CrossRepoMs = time.Since(phase).Milliseconds()
		notePassFaults("cross_repo")
	}
	if merged.Flags.Has(DerivedInvalidatesContracts) {
		phase = time.Now()
		report.Contracts = mi.ReconcileContractEdgesForFrontier(merged)
		report.ContractsMs = time.Since(phase).Milliseconds()
		notePassFaults("contracts")
	}

	capabilityNow := capabilityReuseCounts()
	report.CapabilitySourcesReused = capabilityNow[0] - capabilityAt[0]
	report.CapabilityRowsReused = capabilityNow[1] - capabilityAt[1]
	report.CapabilityRoots = capabilityNow[2] - capabilityAt[2]
	report.CapabilityExpanded = capabilityNow[3] - capabilityAt[3]
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
		zap.Strings("framework_candidate_gated", report.FrameworkCandidateGated),
		zap.Any("framework_candidate_witness", report.FrameworkCandidateWitness),
		zap.Int64("factory_chain_prior_skipped", report.FactoryChainPriorSkipped),
		zap.Int64("framework_candidate_check_ms", report.FrameworkCandidateCheckMs),
		zap.Bool("fn_value_prior_used", report.FnValuePriorUsed),
		zap.Int64("fn_value_reused", report.FnValueReused),
		zap.Int64("fn_value_resolved", report.FnValueResolved),
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
		zap.Int64("capability_sources_reused", report.CapabilitySourcesReused),
		zap.Int64("capability_rows_reused", report.CapabilityRowsReused),
		zap.Int64("capability_roots", report.CapabilityRoots),
		zap.Int64("capability_expanded", report.CapabilityExpanded),
		zap.Int64("framework_ms", report.FrameworkMs),
		zap.Int64("external_calls_ms", report.ExternalCallsMs),
		zap.Int64("cross_repo_ms", report.CrossRepoMs),
		zap.Int64("contracts_ms", report.ContractsMs),
		zap.Any("derived_faults", report.PassFaults),
		zap.Any("derived_cpu_ms", report.PassCPUMs),
		zap.Int("hierarchy_republished", report.HierarchyRepublished),
		zap.Int64("duration_ms", report.DurationMs))
}
