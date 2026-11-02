package reach

import (
	"context"
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

func observedTopologyRead(ctx context.Context, scope []string) (func(), bool) {
	observer := coordinationWaitObserver(ctx)
	if observer == nil {
		return beginTopologyRead(ctx, scope)
	}
	started := time.Now()
	release, ok := beginTopologyRead(ctx, scope)
	observer(CoordinationWait{Phase: "gate_wait", Duration: time.Since(started), Attempted: true, Acquired: ok})
	return release, ok
}
