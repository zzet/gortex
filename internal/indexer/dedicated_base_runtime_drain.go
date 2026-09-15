package indexer

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

var errDedicatedBaseRuntimeClosed = errors.New("dedicated base publisher admission is closed")

// Protected by dedicatedBaseRuntime.mu. A publisher captures its exact slot;
// authorized re-registration may replace a drained closing slot, never reopen
// that old slot. Held publishers therefore cannot resurrect a forgotten owner.
type dedicatedBaseOwnerAdmission struct {
	owner         store_sqlite.DedicatedBaseOwner
	active        int
	closing       bool
	drained       chan struct{}
	drainedClosed bool
}

// admitOwner precedes both observation-gate waits and authority/allocation SQL.
// It covers the complete public runtime call, including physical work after the
// observation gate is released. Physical-flight drain remains a separate fence.
func (r *dedicatedBaseRuntime) admitOwner(ctx context.Context, graphID string, owner store_sqlite.DedicatedBaseOwner) (func(), error) {
	release, _, err := r.admitOwnerState(ctx, graphID, owner, nil)
	return release, err
}

func (r *dedicatedBaseRuntime) admitOwnerState(ctx context.Context, graphID string, owner store_sqlite.DedicatedBaseOwner, expected *dedicatedBaseOwnerAdmission) (func(), *dedicatedBaseOwnerAdmission, error) {
	if r == nil || ctx == nil || graphID == "" || owner.CheckoutID == "" || owner.Incarnation == "" {
		return nil, nil, fmt.Errorf("%w: owner admission requires context and exact graph/owner", errDedicatedBaseRuntimeInput)
	}
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	r.mu.Lock()
	if r.admissionClosed {
		r.mu.Unlock()
		return nil, nil, errDedicatedBaseRuntimeClosed
	}
	if expected != nil && r.ownerAdmissions[graphID] != expected {
		r.mu.Unlock()
		return nil, nil, errDedicatedBaseRuntimeClosed
	}
	state, err := r.ownerAdmissionLocked(graphID, owner)
	if err != nil {
		r.mu.Unlock()
		return nil, nil, err
	}
	if state.closing {
		r.mu.Unlock()
		return nil, nil, errDedicatedBaseRuntimeClosed
	}
	state.active++
	r.admittedActors++
	r.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			r.mu.Lock()
			state.active--
			r.admittedActors--
			r.finishOwnerDrainLocked(state)
			// Failed validation never issued a publisher capability. Release an
			// unconfirmed idle slot, but retain confirmed owners and closing
			// tombstones; a different admitted actor may still confirm this slot.
			if state.active == 0 && !state.closing && state.owner == (store_sqlite.DedicatedBaseOwner{}) && r.ownerAdmissions[graphID] == state {
				delete(r.ownerAdmissions, graphID)
				if len(r.ownerAdmissions) == 0 {
					r.ownerAdmissions = nil
				}
			}
			if r.admissionClosed && r.admittedActors == 0 && !r.admissionDrainClosed {
				close(r.admissionDrain)
				r.admissionDrainClosed = true
			}
			r.mu.Unlock()
		})
	}, state, nil
}

// RegisterDedicatedBaseOwner is called only after the lifecycle has validated a
// ready catalog binding and completed any previous cleanup for this graph. It
// is not an automatic retry of a closed admission and does no catalog writes.
// A repeated live registration is idempotent; replacing a closed, drained slot
// deliberately permits deterministic GraphID reuse, including explicit retrack
// of the same checkout incarnation. Old publisher capabilities stay revoked.
func (r *dedicatedBaseRuntime) RegisterDedicatedBaseOwner(graphID string, owner store_sqlite.DedicatedBaseOwner) error {
	if r == nil || graphID == "" || owner.CheckoutID == "" || owner.Incarnation == "" {
		return fmt.Errorf("%w: registration requires exact graph/owner", errDedicatedBaseRuntimeInput)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.admissionClosed {
		return errDedicatedBaseRuntimeClosed
	}
	if r.ownerAdmissions == nil {
		r.ownerAdmissions = make(map[string]*dedicatedBaseOwnerAdmission)
	}
	old := r.ownerAdmissions[graphID]
	if old != nil && !old.closing {
		if old.owner.CheckoutID != "" && old.owner != owner {
			return fmt.Errorf("%w: cannot replace a live publisher owner", store_sqlite.ErrCatalogStaleGuard)
		}
		old.owner = owner
		return nil
	}
	if old != nil && (old.active != 0 || !old.drainedClosed) {
		return fmt.Errorf("%w: previous publisher owner has not drained", errDedicatedBaseRuntimeClosed)
	}
	r.ownerAdmissions[graphID] = &dedicatedBaseOwnerAdmission{owner: owner, drained: make(chan struct{})}
	return nil
}

func (r *dedicatedBaseRuntime) ownerAdmissionLocked(graphID string, owner store_sqlite.DedicatedBaseOwner) (*dedicatedBaseOwnerAdmission, error) {
	if r.ownerAdmissions == nil {
		r.ownerAdmissions = make(map[string]*dedicatedBaseOwnerAdmission)
	}
	state := r.ownerAdmissions[graphID]
	if state == nil {
		// Admission precedes catalog validation. Do not let an invalid/stale
		// installation request permanently bind this graph to an alleged owner.
		state = &dedicatedBaseOwnerAdmission{drained: make(chan struct{})}
		r.ownerAdmissions[graphID] = state
	} else if state.owner.CheckoutID != "" && state.owner != owner {
		return nil, fmt.Errorf("%w: publisher owner differs for graph %s", store_sqlite.ErrCatalogStaleGuard, graphID)
	}
	return state, nil
}

// confirmOwner follows successful catalog authority validation inside an
// admitted actor. The caller still owns the graph observation gate. Cleanup may
// have closed admission in between, but cannot delete the graph until this
// already-counted actor drains; no outer precheck replaces catalog write guards.
func (r *dedicatedBaseRuntime) confirmOwner(graphID string, owner store_sqlite.DedicatedBaseOwner) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	state, err := r.ownerAdmissionLocked(graphID, owner)
	if err != nil {
		return err
	}
	state.owner = owner
	return nil
}

func (r *dedicatedBaseRuntime) finishOwnerDrainLocked(state *dedicatedBaseOwnerAdmission) {
	if state.closing && state.active == 0 && !state.drainedClosed {
		close(state.drained)
		state.drainedClosed = true
	}
}

// CloseDedicatedBaseOwner is nonblocking and idempotent for the same exact
// owner. Durable catalog closing authorization must precede this local fence.
// New requests fail immediately; the returned channel joins calls already
// admitted, including actors waiting to observe or allocate. It does no SQL,
// does not delete data, and does not start a waiter goroutine.
func (r *dedicatedBaseRuntime) CloseDedicatedBaseOwner(graphID string, owner store_sqlite.DedicatedBaseOwner) (<-chan struct{}, error) {
	if r == nil || graphID == "" || owner.CheckoutID == "" || owner.Incarnation == "" {
		return nil, fmt.Errorf("%w: closing requires exact graph/owner", errDedicatedBaseRuntimeInput)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	state, err := r.ownerAdmissionLocked(graphID, owner)
	if err != nil {
		return nil, err
	}
	state.owner = owner // caller carries durable closing authorization
	state.closing = true
	r.finishOwnerDrainLocked(state)
	return state.drained, nil
}

// CloseDedicatedBaseAdmission is the runtime shutdown counterpart. Its channel
// joins admitted runtime calls, not every physical flight or catalog writer;
// shutdown must also join those existing owners before closing the Store.
func (r *dedicatedBaseRuntime) CloseDedicatedBaseAdmission() <-chan struct{} {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.admissionDrain == nil {
		r.admissionDrain = make(chan struct{})
	}
	r.admissionClosed = true
	for _, state := range r.ownerAdmissions {
		state.closing = true
		r.finishOwnerDrainLocked(state)
	}
	if r.admittedActors == 0 && !r.admissionDrainClosed {
		close(r.admissionDrain)
		r.admissionDrainClosed = true
	}
	return r.admissionDrain
}
