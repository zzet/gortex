package reach

import (
	"context"
	"sync"
	"time"
)

// CoordinationWait describes one actual lookup admission, separately from
// traversal time. Detached readers report Attempted=false for the mutex.
type CoordinationWait struct {
	Phase     string
	Duration  time.Duration
	Attempted bool
	Acquired  bool
}

type coordinationWaitKey struct{}

// WithCoordinationWaitObserver observes only lookups descended from ctx; it
// retains no process-wide history and changes no gate or locking policy.
func WithCoordinationWaitObserver(ctx context.Context, observer func(CoordinationWait)) context.Context {
	if observer == nil {
		return ctx
	}
	return context.WithValue(ctx, coordinationWaitKey{}, observer)
}

func coordinationWaitObserver(ctx context.Context) func(CoordinationWait) {
	observer, _ := ctx.Value(coordinationWaitKey{}).(func(CoordinationWait))
	return observer
}

func observedTopologyRead(ctx context.Context) (func(), bool) {
	observer := coordinationWaitObserver(ctx)
	if observer == nil {
		return beginTopologyReadContext(ctx)
	}
	started := time.Now()
	release, ok := beginTopologyReadContext(ctx)
	observer(CoordinationWait{Phase: "gate_wait", Duration: time.Since(started), Attempted: true, Acquired: ok})
	return release, ok
}

// observedResolveLock reports the existing mutex admission without changing
// which readers acquire it or the cancellation policy.
func observedResolveLock(ctx context.Context, mu *sync.Mutex) bool {
	observer := coordinationWaitObserver(ctx)
	if observer == nil {
		return lockContext(ctx, mu)
	}
	started := time.Now()
	ok := lockContext(ctx, mu)
	observer(CoordinationWait{Phase: "resolve_mutex_wait", Duration: time.Since(started), Attempted: true, Acquired: ok})
	return ok
}
