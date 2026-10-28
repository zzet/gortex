package indexer

import (
	"context"
	"fmt"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/reach"
	"github.com/zzet/gortex/internal/search"
	"go.uber.org/zap"
)

// UntrackRepoChecked hides and removes one live repository without changing
// its configured intent. It is used by promotion/replacement flows whose
// control-plane state is committed separately.
func (mi *MultiIndexer) UntrackRepoChecked(
	ctx context.Context, repoPrefix string,
) (nodesRemoved, edgesRemoved int, err error) {
	return mi.untrackRepoChecked(ctx, repoPrefix, false, nil)
}

// purgeRepoChecked is the authoritative lifecycle path. Unlike
// UntrackRepoChecked it also runs when no process-local registry entry exists,
// which is how a durable cleanup saga resumes after restart. finalize runs
// while the stable mutation lane is still closed; a failure therefore cannot
// open a retrack window before durable config removal succeeds.
func (mi *MultiIndexer) purgeRepoChecked(
	ctx context.Context,
	repoPrefix string,
	finalize func(*RepoMetadata) error,
) (nodesRemoved, edgesRemoved int, err error) {
	return mi.untrackRepoChecked(ctx, repoPrefix, true, finalize)
}

func (mi *MultiIndexer) untrackRepoChecked(
	ctx context.Context,
	repoPrefix string,
	force bool,
	finalize func(*RepoMetadata) error,
) (nodesRemoved, edgesRemoved int, err error) {
	return mi.untrackRepoCheckedRetainingAdmission(ctx, repoPrefix, force, finalize, false)
}

// untrackRepoCheckedRetainingAdmission is the lifecycle-only continuation path.
// Legacy callers keep the four-argument wrapper and release admission normally.
func (mi *MultiIndexer) untrackRepoCheckedRetainingAdmission(
	ctx context.Context,
	repoPrefix string,
	force bool,
	finalize func(*RepoMetadata) error,
	retainAdmission bool,
) (nodesRemoved, edgesRemoved int, err error) {
	return mi.untrackRepoWithOwner(ctx, repoPrefix, force, finalize, retainAdmission, nil, nil)
}

// purgeRepoOwnedChecked is promotion rollback, not authoritative untrack. It
// may remove only the registration that attempt actually observed. The witness
// is checked again after the existing stable lane drains and before payload or
// registry teardown; a replacement owner is never selected by prefix alone.
func (mi *MultiIndexer) purgeRepoOwnedChecked(ctx context.Context, expected prefixOwnerWitness, stopWatcher func()) (int, int, error) {
	if expected.meta == nil || expected.prefix == "" {
		return 0, 0, errPrefixIdentityUnknown
	}
	return mi.untrackRepoWithOwner(ctx, expected.prefix, true, nil, false, &expected, stopWatcher)
}

func (mi *MultiIndexer) untrackRepoWithOwner(
	ctx context.Context,
	repoPrefix string,
	force bool,
	finalize func(*RepoMetadata) error,
	retainAdmission bool,
	expected *prefixOwnerWitness,
	stopOwnedWatcher func(),
) (nodesRemoved, edgesRemoved int, err error) {
	if repoPrefix == "" {
		return 0, 0, fmt.Errorf("indexer: repository teardown refuses an empty prefix")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if mi.isClosed() {
		return 0, 0, errMultiIndexerClosed
	}

	// Reuse the continuation first. The lane it owns stayed closed when the
	// earlier attempt failed, so no new repository generation can have crossed
	// the teardown boundary.
	mi.mu.RLock()
	state := mi.pendingRepositoryUntracks[repoPrefix]
	metaSnapshot, tracked := mi.repos[repoPrefix]
	idxSnapshot := mi.indexers[repoPrefix]
	ownerErr := mi.validatePrefixCleanupOwnerLocked(repoPrefix, expected)
	mi.mu.RUnlock()
	if ownerErr != nil {
		return 0, 0, ownerErr
	}

	var coordinator *repositoryMutationCoordinator
	if state != nil {
		coordinator = state.coordinator
	} else if tracked {
		var current bool
		coordinator, current = mi.repositoryMutationCoordinatorForTeardownSnapshot(
			repoPrefix, metaSnapshot, idxSnapshot,
		)
		if !current {
			mi.mu.RLock()
			state = mi.pendingRepositoryUntracks[repoPrefix]
			mi.mu.RUnlock()
			if state == nil {
				return 0, 0, fmt.Errorf("indexer: repository teardown for %s was superseded", repoPrefix)
			}
			coordinator = state.coordinator
		}
	} else {
		if !force {
			return 0, 0, nil
		}
		coordinator = mi.repositoryMutationCoordinator(repoPrefix)
	}
	if coordinator == nil {
		return 0, 0, fmt.Errorf("indexer: repository teardown for %s has no mutation lane", repoPrefix)
	}

	// Admission closes before the registry is hidden. Waiting outside mi.mu
	// lets the in-flight mutation tail finish without lock inversion.
	if err := coordinator.closeAndWait(ctx); err != nil {
		return 0, 0, fmt.Errorf("indexer: drain repository %s before teardown: %w", repoPrefix, err)
	}
	mi.mu.RLock()
	ownerErr = mi.validatePrefixCleanupOwnerLocked(repoPrefix, expected)
	mi.mu.RUnlock()
	if ownerErr != nil {
		return 0, 0, ownerErr
	}

	mi.batchMutationGate.RLock()
	defer mi.batchMutationGate.RUnlock()
	finishTopologyMutation := reach.BeginTopologyMutation(mi.graph)
	topologyChanged := false
	defer func() { finishTopologyMutation(topologyChanged) }()

	mi.goModuleMu.Lock()
	stamp := mi.goModuleFiles[repoPrefix]
	hadGoModule := mi.goModuleCache[repoPrefix] != "" || stamp.modified != 0
	mi.goModuleMu.Unlock()
	hadRules := mi.configMgr != nil && len(mi.configMgr.GetRepoConfig(repoPrefix).CrossWorkspaceDeps) > 0
	goModuleAffected := hadGoModule || hadRules || len(graph.ReadRepoFilePaths(mi.graph, repoPrefix, "", []string{"go"}, nil)) > 0
	mi.mu.Lock()
	if err := mi.validatePrefixCleanupOwnerLocked(repoPrefix, expected); err != nil {
		mi.mu.Unlock()
		return 0, 0, err
	}
	state = mi.pendingRepositoryUntracks[repoPrefix]
	if state == nil {
		meta := mi.repos[repoPrefix]
		idx := mi.indexers[repoPrefix]
		if meta == nil && !force {
			mi.mu.Unlock()
			return 0, 0, nil
		}
		if mi.existingRepositoryMutationCoordinator(repoPrefix) != coordinator ||
			(idx != nil && !idx.hasRepositoryMutationCoordinator(coordinator)) {
			mi.mu.Unlock()
			return 0, 0, fmt.Errorf("indexer: repository teardown for %s lost its mutation lane", repoPrefix)
		}
		state = &repositoryUntrackState{
			metadata:         meta,
			indexer:          idx,
			coordinator:      coordinator,
			contract:         mi.contractInvalidationPlanForRepo(idx),
			finalize:         finalize,
			goModuleAffected: goModuleAffected,
		}
		if mi.pendingRepositoryUntracks == nil {
			mi.pendingRepositoryUntracks = make(map[string]*repositoryUntrackState)
		}
		mi.pendingRepositoryUntracks[repoPrefix] = state
		if meta != nil {
			delete(mi.repos, repoPrefix)
			delete(mi.indexers, repoPrefix)
			topologyChanged = true
		}
	}
	mi.mu.Unlock()
	if state.metadata != nil {
		mi.forgetRepoLoad(state.metadata.RootPath)
	}
	if expected != nil && stopOwnedWatcher != nil {
		// The witness was accepted inside the drained stable lane and the
		// registry is hidden. Stop only that owner's watcher, outside mi.mu
		// and the cleanup continuation lock, before any payload is removed.
		stopOwnedWatcher()
	}

	state.mu.Lock()
	defer state.mu.Unlock()
	state.retainAdmission = state.retainAdmission || retainAdmission
	if state.finalize == nil && finalize != nil {
		state.finalize = finalize
	}
	finalizeConfig := func() error {
		if state.configFinalized || state.finalize == nil {
			return nil
		}
		if err := state.finalize(state.metadata); err != nil {
			return fmt.Errorf("indexer: finalize repository cleanup for %s: %w", repoPrefix, err)
		}
		state.configFinalized = true
		return nil
	}
	if state.completed {
		if err := mi.finalizeCompletedRepositoryUntrackConfig(repoPrefix, state, coordinator); err != nil {
			return state.nodesRemoved, state.edgesRemoved, err
		}
		return state.nodesRemoved, state.edgesRemoved, nil
	}

	if !state.indexerClosed {
		if state.indexer != nil {
			state.indexer.Close()
			state.indexer.releaseTrigramSearcher()
			state.indexer.trigramBudget().forget(state.indexer)
			state.indexer = nil
		}
		state.indexerClosed = true
	}

	// Purge and aggregate-vector publication share the vector update lane with
	// sibling installs. Successful phases are explicit so a disk-full/config
	// retry does not repeat a completed destructive scan.
	refresh := func(sw *search.Swappable) error {
		if !state.payloadPurged {
			removedNodes, removedEdges, purgeErr := mi.purgeRepositoryPayload(repoPrefix, state.metadata)
			if purgeErr != nil {
				return purgeErr
			}
			state.nodesRemoved = removedNodes
			state.edgesRemoved = removedEdges
			state.payloadPurged = true
			topologyChanged = true
		}
		if !state.vectorPublished {
			if publishErr := mi.publishVectorCorpusAfterRepoRemoval(ctx, repoPrefix, sw); publishErr != nil {
				return publishErr
			}
			state.vectorPublished = true
		}
		return nil
	}
	if sw, ok := mi.search.(*search.Swappable); ok {
		err = sw.SerializeVectorUpdate(func() error { return refresh(sw) })
	} else {
		err = refresh(nil)
	}
	if err != nil {
		return state.nodesRemoved, state.edgesRemoved,
			fmt.Errorf("indexer: authoritative cleanup for %s: %w", repoPrefix, err)
	}

	if err := finalizeConfig(); err != nil {
		return state.nodesRemoved, state.edgesRemoved, err
	}
	if !state.contract.Empty() {
		mi.ReconcileContractEdgesForFrontier(state.contract)
	}
	if state.goModuleAffected {
		mi.goModuleRetry.Invalidate()
		cr := mi.newCrossRepoResolver()
		if err := cr.RetryGoModuleDeps(ctx); err != nil {
			// Removal and config finalization already committed. The invalidated
			// retry state lets the next module pass finish this optional tail.
			mi.logger.Warn("settle Go module callers after untrack deferred", zap.String("repo", repoPrefix), zap.Error(err))
		}
		// A canceled sweep can already have changed a bounded batch. Finish
		// the source/parallel tails it recorded before dropping this pass.
		mi.finishGoModuleCalls(cr)
		mi.reconcileRetargetedTestCalls(cr.TakeRetargetedTestCallFiles())
	}

	// A cleanup reservation wins atomically over continuation deletion and
	// keeps this exact closed lane for lifecycle finalization.
	state.completed = true
	if state.retainAdmission {
		return state.nodesRemoved, state.edgesRemoved, nil
	}
	retained, detached := mi.detachRepositoryUntrackContinuation(repoPrefix, state, coordinator, nil)
	if retained {
		state.retainAdmission = true
		return state.nodesRemoved, state.edgesRemoved, nil
	}
	if !detached {
		return state.nodesRemoved, state.edgesRemoved,
			fmt.Errorf("%w for %s", errRepositoryUntrackContinuationChanged, repoPrefix)
	}
	return state.nodesRemoved, state.edgesRemoved, nil
}

var errRepositoryUntrackContinuationChanged = fmt.Errorf("indexer: repository teardown continuation changed")

// finalizeCompletedRepositoryUntrackConfig runs a callback supplied after the
// payload phases completed only while the same continuation and mutation lane
// still own the prefix. The caller owns state.mu through the callback.
func (mi *MultiIndexer) finalizeCompletedRepositoryUntrackConfig(
	repoPrefix string,
	state *repositoryUntrackState,
	coordinator *repositoryMutationCoordinator,
) error {
	if state == nil || coordinator == nil || state.coordinator != coordinator {
		return fmt.Errorf("%w for %s", errRepositoryUntrackContinuationChanged, repoPrefix)
	}
	if state.configFinalized || state.finalize == nil {
		return nil
	}
	mi.mu.RLock()
	mi.repositoryMutationMu.Lock()
	current := mi.pendingRepositoryUntracks[repoPrefix] == state &&
		mi.repositoryMutations[repoPrefix] == coordinator
	mi.repositoryMutationMu.Unlock()
	mi.mu.RUnlock()
	if !current {
		return fmt.Errorf("%w for %s", errRepositoryUntrackContinuationChanged, repoPrefix)
	}
	if err := state.finalize(state.metadata); err != nil {
		return fmt.Errorf("indexer: finalize repository cleanup for %s: %w", repoPrefix, err)
	}
	state.configFinalized = true
	return nil
}

func (mi *MultiIndexer) purgeRepositoryPayload(
	repoPrefix string, meta *RepoMetadata,
) (nodesRemoved, edgesRemoved int, err error) {
	restubTrackedNpmRepoIncoming(mi.graph, repoPrefix)
	parked := restubGoModuleRepoIncoming(mi.graph, repoPrefix)
	if purger, ok := mi.graph.(interface{ PurgeRepo(string) error }); ok {
		if err := purger.PurgeRepo(repoPrefix); err != nil {
			return 0, 0, fmt.Errorf("purge repository payload: %w", err)
		}
		if err := mi.finalizeUntrackedGoModuleCalls(parked); err != nil {
			return 0, 0, err
		}
		if meta != nil {
			return meta.NodeCount, meta.EdgeCount, nil
		}
		return 0, 0, nil
	}
	nodesRemoved, edgesRemoved, err = evictRepoAllGenerations(mi.graph, repoPrefix)
	if err == nil {
		err = mi.finalizeUntrackedGoModuleCalls(parked)
	}
	return nodesRemoved, edgesRemoved, err
}

func (mi *MultiIndexer) finalizeUntrackedGoModuleCalls(parked []graph.EdgeIdentity) error {
	if len(parked) == 0 {
		return nil
	}
	cr := mi.newCrossRepoResolver()
	if err := cr.ResolveDeferredGoModuleEdgeIdentities(parked); err != nil {
		return err
	}
	mi.finishGoModuleCalls(cr)
	mi.reconcileRetargetedTestCalls(cr.TakeRetargetedTestCallFiles())
	return nil
}
