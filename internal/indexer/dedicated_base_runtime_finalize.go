package indexer

import (
	"fmt"

	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

// FinalizeDedicatedBaseOwner reclaims a completed local owner slot only after
// the lifecycle has completed durable cleanup. The channel returned by that
// slot's CloseDedicatedBaseOwner is a capability: an owner tuple alone cannot
// distinguish an old cleanup from an explicitly retracked identical owner.
// This method does no SQL, does not wait, and never reopens shutdown admission.
// Missing state is an idempotent no-op, so a later lifecycle finalizer failure
// can be retried without retaining an unbounded finalized-capability registry.
func (r *dedicatedBaseRuntime) FinalizeDedicatedBaseOwner(graphID string, owner store_sqlite.DedicatedBaseOwner, drained <-chan struct{}) error {
	if r == nil || graphID == "" || owner.CheckoutID == "" || owner.Incarnation == "" || drained == nil {
		return fmt.Errorf("%w: finalization requires exact graph/owner and drain capability", errDedicatedBaseRuntimeInput)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	state := r.ownerAdmissions[graphID]
	if state == nil {
		return nil
	}
	if state.owner != owner || state.drained != drained {
		return fmt.Errorf("%w: publisher drain does not identify the current owner slot", store_sqlite.ErrCatalogStaleGuard)
	}
	if !state.closing || state.active != 0 || !state.drainedClosed {
		return fmt.Errorf("%w: publisher owner has not closed and drained", errDedicatedBaseRuntimeClosed)
	}
	delete(r.ownerAdmissions, graphID)
	if len(r.ownerAdmissions) == 0 {
		r.ownerAdmissions = nil
	}
	return nil
}
