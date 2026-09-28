package indexer

import (
	"context"
	"errors"
	"time"
)

const deferredRetirementPreemptPoll = 2 * time.Millisecond

// errRetirementPreempted is the cause a preempted slice's context carries.
var errRetirementPreempted = errors.New("deferred retirement: preempted by an interactive writer")

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
