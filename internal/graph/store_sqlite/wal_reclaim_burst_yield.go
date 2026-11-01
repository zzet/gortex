package store_sqlite

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
)

var (
	errWALReclaimEditBurst       = errors.New("store_sqlite: wal reclaim deferred: edit burst")
	errWALReclaimInteractiveRead = errors.New("store_sqlite: wal reclaim deferred: interactive read")
)

type walInteractiveReads struct {
	mu     sync.Mutex // orders activation with writer and read-gate admission
	active atomic.Int32
}

// AnnounceInteractiveRead marks only an in-flight edit acknowledgement or the
// same session's edited-file read-back. The caller releases it when that call
// ends, including cancellation. It does not retain an idle session marker.
func (s *Store) AnnounceInteractiveRead() func() {
	if s.coreless() {
		return func() {}
	}
	state := &s.walReclaim.interactive
	state.mu.Lock()
	state.active.Add(1)
	state.mu.Unlock()
	var once sync.Once
	return func() { once.Do(func() { state.active.Add(-1) }) }
}

func (s *Store) editBurstActive() bool {
	return s.buildLaneBusy() || s.editIntentActive() || s.writeWanted()
}

func (r *walReclaimResult) burstException() bool {
	return r.bulkCompletion || r.hardCap || r.lastResort
}

// Writer demand keeps its existing first-cause precedence. Edit bursts and
// interactive reads remain independent, so removing the former policy never
// removes the latter, including from hard-cap and bulk-completion attempts.
func (s *Store) reclaimYieldCause(ctx context.Context, r *walReclaimResult) error {
	if !r.burstException() {
		if demand := s.writeDemandCause(); demand != nil {
			noteWALReclaimDemand(ctx, demand)
			r.deferredEditBurst = true
			return demand
		}
	}
	if s.walReclaim.interactive.active.Load() > 0 {
		r.deferredInteractiveRead = true
		r.reason = "interactive_read"
		return errWALReclaimInteractiveRead
	}
	if !r.burstException() && s.editBurstActive() {
		r.deferredEditBurst = true
		r.reason = "edit_burst"
		return errWALReclaimEditBurst
	}
	return nil
}

// Waiting for the ordinary semaphore does not reserve reclaim admission.
// Recheck under the same mutex as marker activation before recording an actual
// reclaim hold; an interactive read arriving while it queued wins admission.
func (s *Store) lockWALReclaimWriter(ctx context.Context, r *walReclaimResult) error {
	if err := s.reclaimYieldCause(ctx, r); err != nil {
		return err
	}
	if err := s.writeMu.LockContext(ctx); err != nil {
		return err
	}
	state := &s.walReclaim.interactive
	state.mu.Lock()
	defer state.mu.Unlock()
	if err := s.reclaimYieldCause(ctx, r); err != nil {
		s.writeMu.Unlock()
		return err
	}
	s.writeMu.beginReclaim(s.editBurstActive())
	check := s.editBurstActive
	s.writeMu.reclaimBurstCheck.Store(&check)
	if r.bulkCompletion {
		s.writeMu.reclaimBulkExceptions.Add(1)
	} else if r.hardCap || r.lastResort {
		s.writeMu.reclaimHardExceptions.Add(1)
	}
	return nil
}
