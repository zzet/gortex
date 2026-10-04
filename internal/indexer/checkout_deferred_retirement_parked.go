package indexer

import (
	"errors"
	"sync/atomic"
	"time"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

// A generation the catalog still references — a route slot, a ref view, an
// active graph pointer, a generation built on it — cannot be retired, and
// nothing about it changes until one of those references goes. The deferred
// sweep offered such generations again on every pass (one in the measured run
// was refused thirteen times, each refusal an error, a backoff and a Warn),
// and while any of them was pending the worker rescanned the catalog every
// second. A refused generation is now parked: it is skipped until a reference
// may have been released — a route slot flipped or cleared, or a generation
// retired (the child a "based" reference names) — or, as a floor for
// references released where no hint is raised (a ref view dropped), until
// retirementParkRecheck has passed. When only parked generations remain, the
// sweep reports nothing pending and the worker waits on its idle pause.

// retirementParkRecheck is how long a parked generation waits without a hint.
const retirementParkRecheck = 5 * time.Minute

// retirementReleaseHints counts the events that may release a generation
// reference.
var retirementReleaseHints atomic.Int64

// deferredRetirementParked is the number of generations parked now, for the
// daemon's progress line.
var deferredRetirementParked atomic.Int64

// DeferredRetirementParked reports how many generations wait, parked, for a
// reference to be released.
func DeferredRetirementParked() int64 { return deferredRetirementParked.Load() }

// noteRetirementReferenceReleased records one such event.
func noteRetirementReferenceReleased() { retirementReleaseHints.Add(1) }

// retirementPark is one parked generation: the hint count and time it was
// parked at.
type retirementPark struct {
	hint  int64
	since time.Time
}

// retirementStillReferenced reports a refusal for a reference the catalog
// still holds.
func retirementStillReferenced(err error) bool {
	return errors.Is(err, store_sqlite.ErrCatalogGenerationReferenced)
}

// parkReferencedRetirement parks a generation the catalog still references.
func (l *CheckoutLifecycle) parkReferencedRetirement(generationID int64, now time.Time) {
	l.coordMu.Lock()
	if l.retirementParked == nil {
		l.retirementParked = make(map[int64]retirementPark)
	}
	_, again := l.retirementParked[generationID]
	l.retirementParked[generationID] = retirementPark{hint: retirementReleaseHints.Load(), since: now}
	parked := len(l.retirementParked)
	deferredRetirementParked.Store(int64(parked))
	l.coordMu.Unlock()
	if l.logger != nil && !again {
		l.logger.Info("indexer: retirement parked a still-referenced generation",
			zap.Int64("generation", generationID), zap.Int("parked", parked))
	}
}

// retirementParkedNow reports whether a generation is still parked: no
// release hint since it was parked, and the recheck floor not reached. A
// generation whose park lapsed is unparked (it is offered again).
func (l *CheckoutLifecycle) retirementParkedNow(generationID int64, now time.Time) bool {
	l.coordMu.Lock()
	defer l.coordMu.Unlock()
	park, ok := l.retirementParked[generationID]
	if !ok {
		return false
	}
	if retirementReleaseHints.Load() != park.hint || now.Sub(park.since) >= retirementParkRecheck {
		delete(l.retirementParked, generationID)
		deferredRetirementParked.Store(int64(len(l.retirementParked)))
		return false
	}
	return true
}

// unparkRetirement forgets a generation (retired, or gone).
func (l *CheckoutLifecycle) unparkRetirement(generationID int64) {
	l.coordMu.Lock()
	delete(l.retirementParked, generationID)
	deferredRetirementParked.Store(int64(len(l.retirementParked)))
	l.coordMu.Unlock()
}
