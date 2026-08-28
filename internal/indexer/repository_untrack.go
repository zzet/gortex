package indexer

import (
	"context"
	"fmt"

	"github.com/zzet/gortex/internal/reach"
	"github.com/zzet/gortex/internal/search"
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
	mi.mu.RUnlock()

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

	mi.batchMutationGate.RLock()
	defer mi.batchMutationGate.RUnlock()
	finishTopologyMutation := reach.BeginTopologyMutation(mi.graph)
	topologyChanged := false
	defer func() { finishTopologyMutation(topologyChanged) }()

	mi.mu.Lock()
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
			metadata:    meta,
			indexer:     idx,
			coordinator: coordinator,
			contract:    mi.contractInvalidationPlanForRepo(idx),
			finalize:    finalize,
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

	state.mu.Lock()
	defer state.mu.Unlock()
	if state.finalize == nil && finalize != nil {
		state.finalize = finalize
	}
	if state.completed {
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

	if !state.configFinalized && state.finalize != nil {
		if err := state.finalize(state.metadata); err != nil {
			return state.nodesRemoved, state.edgesRemoved,
				fmt.Errorf("indexer: finalize repository cleanup for %s: %w", repoPrefix, err)
		}
		state.configFinalized = true
	}
	if !state.contract.Empty() {
		mi.ReconcileContractEdgesForFrontier(state.contract)
	}

	// Delete the continuation before opening admission. A caller that already
	// holds its pointer observes completed=true; a fresh track cannot enter
	// until the exact closed coordinator is detached immediately afterwards.
	state.completed = true
	mi.mu.Lock()
	if mi.pendingRepositoryUntracks[repoPrefix] == state {
		delete(mi.pendingRepositoryUntracks, repoPrefix)
	}
	mi.mu.Unlock()
	mi.detachRepositoryMutationCoordinator(repoPrefix, coordinator)
	return state.nodesRemoved, state.edgesRemoved, nil
}

func (mi *MultiIndexer) purgeRepositoryPayload(
	repoPrefix string, meta *RepoMetadata,
) (nodesRemoved, edgesRemoved int, err error) {
	if purger, ok := mi.graph.(interface{ PurgeRepo(string) error }); ok {
		if err := purger.PurgeRepo(repoPrefix); err != nil {
			return 0, 0, fmt.Errorf("purge repository payload: %w", err)
		}
		if meta != nil {
			return meta.NodeCount, meta.EdgeCount, nil
		}
		return 0, 0, nil
	}
	return evictRepoAllGenerations(mi.graph, repoPrefix)
}
