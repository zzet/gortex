package indexer

import (
	"context"
	"errors"
	"sync/atomic"
	"time"
)

// A deferred retirement slice gives the store's writer back to interactive
// work within a few milliseconds, including in the middle of a chunk.
//
// The store already yields between chunks while a writer waits, but one
// chunk — a thousand-row delete from the edges or nodes table of a large
// store — measured two seconds and more, once about thirty, and an edit's
// route withdrawal waited the whole chunk out. So the background slice runs
// under a context that a watcher cancels the moment an interactive writer
// waits: the driver interrupts the running statement, the chunk's
// transaction rolls back, and the gate is free again. Nothing is lost that
// matters: the generation keeps its retiring fence and seal, every sweep step
// is idempotent, and the next pass resumes the same generation.
//
// A slice that cannot finish a chunk between edits must still finish some
// time, so preemption is suspended once the sweep has made no progress for
// deferredRetirementStarvationLimit; that slice keeps the between-chunk
// yield but runs its chunks to completion.

const (
	deferredRetirementPreemptPoll     = 2 * time.Millisecond
	deferredRetirementStarvationLimit = 2 * time.Minute
)

// errRetirementPreempted is the cause a preempted slice's context carries.
var errRetirementPreempted = errors.New("deferred retirement: preempted by an interactive writer")

// deferredRetirementPreemptions counts the slices given back to an
// interactive writer (skipped before starting or cancelled mid-chunk).
var deferredRetirementPreemptions atomic.Int64

// DeferredRetirementPreemptions reports how many deferred retirement slices
// yielded to interactive writers in this process.
func DeferredRetirementPreemptions() int64 { return deferredRetirementPreemptions.Load() }

// writeDemandReporter is a store that can say a writer is waiting for it: a
// caller parked on the write gate or an announced mutation.
type writeDemandReporter interface {
	WriteWanted() bool
}

// interactiveWriteWanted reports interactive work that wants the store's
// writer now or is about to: an edit cycle holding the build lane (the
// predicate the checkpoint and the analysis pass yield to — an admitted
// delta is no longer "pending" anywhere else, and it writes its payload and
// publishes before it lets the lane go), the store's own write demand, an
// interactive build queued on the shared lane, or a checkout mutation between
// its reservation and its ticket's completion (the route withdrawal, the
// receipt and the edit's publication all fall inside that window).
func (l *CheckoutLifecycle) interactiveWriteWanted(coordinators []*CheckoutCoordinator) bool {
	if l == nil {
		return false
	}
	if l.editCycleHoldsBuildLane() {
		return true
	}
	if l.interactiveDemand != nil {
		return l.interactiveDemand()
	}
	if reporter, ok := any(l.store).(writeDemandReporter); ok && l.store != nil && reporter.WriteWanted() {
		return true
	}
	for _, coordinator := range coordinators {
		if coordinator.interactiveWritePending() {
			return true
		}
	}
	return false
}

// interactiveWritePending reports a checkout mutation or edit refresh this
// coordinator has admitted and not yet completed, or an interactive build
// queued on its lane.
func (c *CheckoutCoordinator) interactiveWritePending() bool {
	if c == nil {
		return false
	}
	c.refreshMu.Lock()
	pending := len(c.refreshWaiters)+c.refreshReserved > 0
	c.refreshMu.Unlock()
	if pending {
		return true
	}
	return c.gate != nil && c.gate.Stats().InteractiveQueued > 0
}

// retirementPreemptionArmed reports whether this slice may be preempted: the
// sweep made progress (or started) within the starvation limit.
func (l *CheckoutLifecycle) retirementPreemptionArmed(now time.Time) bool {
	limit := l.retirementStarvationLimit
	if limit == 0 {
		limit = deferredRetirementStarvationLimit
	}
	if limit < 0 {
		return false
	}
	last := l.deferredRetirementProgress.Load()
	if last == 0 {
		l.deferredRetirementProgress.CompareAndSwap(0, now.UnixNano())
		return true
	}
	return now.Sub(time.Unix(0, last)) < limit
}

// noteRetirementProgress restarts the starvation clock.
func (l *CheckoutLifecycle) noteRetirementProgress(now time.Time) {
	l.deferredRetirementProgress.Store(now.UnixNano())
}

// retirementShouldStandDown is the sweep's yield rule: it never runs while an
// edit cycle holds the build lane; while its preemption is armed (it has not
// been starved) it also gives way to every other interactive writer.
func (l *CheckoutLifecycle) retirementShouldStandDown(armed bool, coordinators []*CheckoutCoordinator) bool {
	if l.editCycleHoldsBuildLane() {
		return true
	}
	if l.retirementPacedStandDown(!armed) != "" {
		return true
	}
	return armed && l.interactiveWriteWanted(coordinators)
}

// preemptOnInteractiveWrite returns ctx wrapped so that it is cancelled with
// errRetirementPreempted as soon as interactive work wants the writer. stop
// ends the watcher; it must be called when the slice returns.
func (l *CheckoutLifecycle) preemptOnInteractiveWrite(
	ctx context.Context, coordinators []*CheckoutCoordinator,
) (context.Context, func()) {
	return l.preemptOnInteractiveWriteWhen(ctx, func() bool { return l.interactiveWriteWanted(coordinators) })
}

// preemptOnInteractiveWriteWhen is preemptOnInteractiveWrite with the
// predicate named.
func (l *CheckoutLifecycle) preemptOnInteractiveWriteWhen(ctx context.Context, wanted func() bool) (context.Context, func()) {
	sliceCtx, cancel := context.WithCancelCause(ctx)
	done := make(chan struct{})
	go func() {
		ticker := time.NewTicker(deferredRetirementPreemptPoll)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-sliceCtx.Done():
				return
			case <-ticker.C:
				if wanted() {
					cancel(errRetirementPreempted)
					return
				}
			}
		}
	}()
	return sliceCtx, func() {
		close(done)
		cancel(context.Canceled)
	}
}

// retirementPreempted reports that a slice stopped because it was preempted
// rather than because its caller gave up.
func retirementPreempted(parent, sliceCtx context.Context) bool {
	return parent.Err() == nil && errors.Is(context.Cause(sliceCtx), errRetirementPreempted)
}
