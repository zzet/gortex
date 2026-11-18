package indexer

import (
	"context"
	"errors"
	"sync"
)

// byteMutationAttempt belongs to one coordinator cycle, including its preflight
// and gate queue. The first payload publication commits the entire cycle, even
// when it subsequently builds a dirty layer. It never cancels a refresh ticket.
type byteMutationAttempt struct {
	mu        sync.Mutex
	parent    context.Context
	ctx       context.Context
	cancel    context.CancelCauseFunc
	committed bool
	stopped   bool
	done      chan struct{}
}

type byteMutationAttemptKey struct{}

var errByteMutationHandoff = errors.New("indexer: unpublished checkout attempt handed to a byte mutation")

func (a *byteMutationAttempt) interrupt() {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.committed && !a.stopped {
		a.cancel(errByteMutationHandoff)
	}
}
func (a *byteMutationAttempt) commit() {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.ctx.Err() == nil {
		a.committed = true
	}
}
func (a *byteMutationAttempt) outcome(out CheckoutCycle) CheckoutCycle {
	if errors.Is(out.Err, context.Canceled) && context.Cause(a.ctx) == errByteMutationHandoff {
		if err := a.parent.Err(); err != nil {
			out.Err = err
			return out
		}
		out.Err = nil
		out.Rescheduled = true
		out.YieldedTo = "byte_mutation"
	}
	return out
}

func (c *CheckoutCoordinator) beginByteMutationAttempt(ctx context.Context) (context.Context, func(), bool) {
	c.byteMu.Lock()
	defer c.byteMu.Unlock()
	if c.byteDemand > 0 {
		return ctx, func() {}, false
	}
	child, cancel := context.WithCancelCause(ctx)
	a := &byteMutationAttempt{parent: ctx, ctx: child, cancel: cancel, done: make(chan struct{})}
	c.byteAttempt = a
	return context.WithValue(child, byteMutationAttemptKey{}, a), func() {
		c.byteMu.Lock()
		a.mu.Lock()
		a.stopped = true
		a.mu.Unlock()
		if c.byteAttempt == a {
			c.byteAttempt = nil
		}
		c.byteMu.Unlock()
		cancel(nil)
		close(a.done)
	}, true
}

// reserveByteMutation prevents the drained attempt from restarting ahead of
// admission. Its caller has already authenticated the accepted positive anchor
// and joined the lifecycle's ordinary source-mutation admission.
func (c *CheckoutCoordinator) reserveByteMutation() (func(), <-chan struct{}) {
	c.byteMu.Lock()
	c.byteDemand++
	var done <-chan struct{}
	if c.byteAttempt != nil {
		c.byteAttempt.interrupt()
		done = c.byteAttempt.done
	}
	c.byteMu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			c.byteMu.Lock()
			c.byteDemand--
			c.byteMu.Unlock()
			// Also resume old work when the edit fails before Prepare or writes nothing.
			c.Signal("byte mutation handoff released; reconcile outstanding source work")
		})
	}, done
}

func byteMutationOwnCancellation(ctx context.Context, err error) bool {
	a, _ := ctx.Value(byteMutationAttemptKey{}).(*byteMutationAttempt)
	return a != nil && errors.Is(err, context.Canceled) && context.Cause(a.ctx) == errByteMutationHandoff
}
func byteMutationCycleOutcome(ctx context.Context, out CheckoutCycle) CheckoutCycle {
	if a, _ := ctx.Value(byteMutationAttemptKey{}).(*byteMutationAttempt); a != nil {
		return a.outcome(out)
	}
	return out
}
