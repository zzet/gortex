package indexer

import (
	"context"
	"fmt"
	"sync"
)

// closeAndDrain creates at most one WaitGroup waiter for a closed mutation
// lane. All mutation admissions increment work while holding c.mu and refuse
// closed lanes, so no Add can race the wait begun after this closure.
func (c *repositoryMutationCoordinator) closeAndDrain() <-chan struct{} {
	c.mu.Lock()
	c.closed = true
	if c.closeDrain != nil {
		done := c.closeDrain
		c.mu.Unlock()
		return done
	}
	done := make(chan struct{})
	c.closeDrain = done
	c.mu.Unlock()
	go func() { c.work.Wait(); close(done) }()
	return done
}

type repositoryCleanupLane struct {
	mu          sync.Mutex
	owner       *MultiIndexer
	prefix      string
	coordinator *repositoryMutationCoordinator
	reservation *byte
	done        <-chan struct{}
	finalized   bool
}

// beginRepositoryCleanupLane reserves the exact stable mutation coordinator
// before closing admission. Reservation and ordinary detach use the same lock
// order, so either cleanup owns this coordinator or selection restarts after a
// completed detach; a stale coordinator is never rebound to a later prefix
// generation.
func (mi *MultiIndexer) beginRepositoryCleanupLane(prefix string) (*repositoryCleanupLane, error) {
	if prefix == "" {
		return nil, fmt.Errorf("indexer: cleanup refuses an empty prefix")
	}
	if mi.isClosed() {
		return nil, errMultiIndexerClosed
	}
	for {
		mi.mu.RLock()
		state := mi.pendingRepositoryUntracks[prefix]
		meta, tracked := mi.repos[prefix]
		idx := mi.indexers[prefix]
		mi.mu.RUnlock()

		var coordinator *repositoryMutationCoordinator
		if state != nil {
			coordinator = state.coordinator
		} else if tracked {
			var current bool
			coordinator, current = mi.repositoryMutationCoordinatorForTeardownSnapshot(prefix, meta, idx)
			if !current {
				continue
			}
		} else {
			coordinator = mi.repositoryMutationCoordinator(prefix)
		}
		if coordinator == nil {
			return nil, fmt.Errorf("indexer: cleanup for %s has no mutation lane", prefix)
		}

		mi.mu.RLock()
		mi.repositoryMutationMu.Lock()
		coordinator.mu.Lock()
		currentState := mi.pendingRepositoryUntracks[prefix]
		currentMeta, currentTracked := mi.repos[prefix]
		currentIdx := mi.indexers[prefix]
		current := mi.repositoryMutations[prefix] == coordinator
		switch {
		case currentState != nil:
			current = current && currentState.coordinator == coordinator
		case tracked:
			current = current && currentTracked && currentMeta == meta && currentIdx == idx
		default:
			current = current && !currentTracked
		}
		if !current {
			coordinator.mu.Unlock()
			mi.repositoryMutationMu.Unlock()
			mi.mu.RUnlock()
			continue
		}
		if coordinator.cleanupReservation != nil {
			coordinator.mu.Unlock()
			mi.repositoryMutationMu.Unlock()
			mi.mu.RUnlock()
			return nil, fmt.Errorf("indexer: cleanup lane for %s is already reserved", prefix)
		}
		reservation := new(byte)
		coordinator.cleanupReservation = reservation
		coordinator.mu.Unlock()
		mi.repositoryMutationMu.Unlock()
		mi.mu.RUnlock()

		return &repositoryCleanupLane{
			owner:       mi,
			prefix:      prefix,
			coordinator: coordinator,
			reservation: reservation,
			done:        coordinator.closeAndDrain(),
		}, nil
	}
}

// purgeRepoForCleanup completes payload/config phases but retains the exact
// closed MI continuation. A successful return permits graph deletion, not
// retracking. Full saga completion authorizes finalization separately.
func (mi *MultiIndexer) purgeRepoForCleanup(ctx context.Context, prefix string, finalize func(*RepoMetadata) error) (int, int, error) {
	return mi.untrackRepoCheckedRetainingAdmission(ctx, prefix, true, finalize, true)
}

func (mi *MultiIndexer) finalizeRepositoryCleanupLane(lane *repositoryCleanupLane) error {
	if lane == nil || lane.owner != mi || lane.reservation == nil {
		return fmt.Errorf("indexer: invalid cleanup lane handle")
	}
	lane.mu.Lock()
	defer lane.mu.Unlock()
	if lane.finalized {
		return nil
	}
	mi.mu.RLock()
	state := mi.pendingRepositoryUntracks[lane.prefix]
	mi.mu.RUnlock()
	if state == nil {
		return fmt.Errorf("indexer: cleanup continuation missing for %s", lane.prefix)
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.coordinator != lane.coordinator || !state.completed || !state.retainAdmission {
		return fmt.Errorf("indexer: cleanup continuation for %s is not finalizable", lane.prefix)
	}
	_, detached := mi.detachRepositoryUntrackContinuation(
		lane.prefix, state, lane.coordinator, lane.reservation,
	)
	if !detached {
		return fmt.Errorf("indexer: cleanup continuation changed for %s", lane.prefix)
	}
	lane.finalized = true
	return nil
}
