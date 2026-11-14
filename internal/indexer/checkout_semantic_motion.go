package indexer

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/zzet/gortex/internal/gitstate"
)

// semanticMotionAttempt is private to one dirty build, not its refresh ticket.
// Watcher events only request a verifier; motion requires a successful sample
// different from the build's exact before sample. Live same-checkout contract
// demand has a separate owned cancellation cause and never certifies motion.
// One verifier per attempt bounds work even when the watcher keeps firing.
// A missed change is still caught by the unchanged final publication fence.
type semanticMotionAttempt struct {
	mu            sync.Mutex
	ctx           context.Context
	cancel        context.CancelCauseFunc
	before, after string
	sample        func(context.Context) (gitstate.DirtySnapshot, error)
	verifyCtx     context.Context
	stopVerify    context.CancelFunc
	done          chan struct{}
	stopped       bool
	providerErr   error
}

func (*semanticMotionAttempt) Error() string {
	return "indexer: verified movement during working-copy enrichment"
}

type contractSemanticDemand struct{ attempt *semanticMotionAttempt }

func (*contractSemanticDemand) Error() string {
	return "indexer: working-copy enrichment deferred for live contract demand"
}

func (a *semanticMotionAttempt) deferForContractDemand() {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.stopped && a.ctx.Err() == nil {
		a.cancel(&contractSemanticDemand{attempt: a})
	}
}

// A joined cancellation can also carry an independent provider/storage error.
// Only cancellation itself, possibly wrapped, authorizes owned demand retry.
func contractDemandCancellation(err, ownedCause error) bool {
	for err != nil {
		if err == context.Canceled || ownedCause != nil && err == ownedCause {
			return true
		}
		if joined, ok := err.(interface{ Unwrap() []error }); ok {
			children := joined.Unwrap()
			if len(children) == 0 {
				return false
			}
			for _, child := range children {
				if !contractDemandCancellation(child, ownedCause) {
					return false
				}
			}
			return true
		}
		wrapped, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = wrapped.Unwrap()
	}
	return false
}

func (a *semanticMotionAttempt) ownsContractDemand(err error) bool {
	demand, ok := err.(*contractSemanticDemand)
	return ok && demand != nil && demand.attempt == a && context.Cause(a.ctx) == demand
}

func (c *CheckoutCoordinator) armSemanticMotion(a *semanticMotionAttempt, before gitstate.DirtySnapshot, started time.Time) func(error) {
	a.before = before.Fingerprint
	a.sample = func(ctx context.Context) (gitstate.DirtySnapshot, error) {
		if c.sampler != nil {
			return c.sampler.Sample(ctx)
		}
		return gitstate.SampleDirty(ctx, c.root)
	}
	a.verifyCtx, a.stopVerify = context.WithCancel(a.ctx)
	m := &c.motion
	m.mu.Lock()
	m.semantic = a
	hinted := m.lastEvent.After(started)
	m.mu.Unlock()
	if hinted {
		a.verify()
	}
	if c.pendingContractWork() {
		a.deferForContractDemand()
	}
	return func(providerErr error) {
		// Closing the arm and canceling/joining its verifier precede the return
		// from runEnrichment. Publication cannot race a live verifier.
		a.mu.Lock()
		a.stopped = true
		a.providerErr = providerErr
		a.stopVerify()
		done := a.done
		a.mu.Unlock()
		m.mu.Lock()
		if m.semantic == a {
			m.semantic = nil
		}
		m.mu.Unlock()
		if done != nil {
			<-done
		}
	}
}

func (a *semanticMotionAttempt) verify() {
	a.mu.Lock()
	if a.stopped || a.done != nil || a.ctx.Err() != nil || a.before == "" {
		a.mu.Unlock()
		return
	}
	a.done = make(chan struct{})
	a.mu.Unlock()
	go func() {
		defer close(a.done)
		after, err := a.sample(a.verifyCtx)
		a.mu.Lock()
		defer a.mu.Unlock()
		if a.stopped || err != nil || a.verifyCtx.Err() != nil || after.Fingerprint == "" || after.Fingerprint == a.before {
			return
		}
		a.after = after.Fingerprint
		a.cancel(a)
	}()
}

// The builder has already drained the provider and abandoned the private
// generation before this translation. Never translate an external cancel,
// deadline or unrelated failure into another build. The existing two-attempt
// loop records motion, retires the failed payload and defers enrichment.
func (a *semanticMotionAttempt) buildError(parent context.Context, generationID int64, root string, err error) error {
	cause := context.Cause(a.ctx)
	demand, isDemand := cause.(*contractSemanticDemand)
	ownedDemand := isDemand && demand != nil && demand.attempt == a
	if ownedDemand && !contractDemandCancellation(err, cause) || !ownedDemand && (cause != a || !errors.Is(err, context.Canceled)) {
		return err
	}
	if parentErr := parent.Err(); parentErr != nil {
		return parentErr
	}
	if a.providerErr != nil {
		canceled := errors.Is(a.providerErr, context.Canceled)
		if ownedDemand {
			canceled = contractDemandCancellation(a.providerErr, cause)
		}
		if !canceled {
			return a.providerErr
		}
	}
	if ownedDemand {
		return demand
	}
	return &DirtySnapshotChangedError{CheckoutRoot: root, GenerationID: generationID, Before: a.before, After: a.after}
}
