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

type coordinationDeadlineKey struct{}

// WithCoordinationDeadline bounds the coordination waits of lookups descended
// from ctx, the topology gate and the resolver mutex, by deadline. A wait that
// reaches it ends like a cancelled one: the lookup returns its bounded
// lower-bound result, and an observer sees the wait unacquired. A gate or mutex
// that is free admits even past deadline, and the traversal after admission
// keeps the lookup's own budget, so the deadline only shortens waiting.
func WithCoordinationDeadline(ctx context.Context, deadline time.Time) context.Context {
	return context.WithValue(ctx, coordinationDeadlineKey{}, deadline)
}

// coordinationWaitContext is ctx for one coordination wait: bounded by the
// coordination deadline when ctx carries one.
func coordinationWaitContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if deadline, ok := ctx.Value(coordinationDeadlineKey{}).(time.Time); ok {
		return context.WithDeadline(ctx, deadline)
	}
	return ctx, func() {}
}

func observedTopologyRead(ctx context.Context, scope []string) (func(), bool) {
	waitCtx, cancel := coordinationWaitContext(ctx)
	defer cancel()
	observer := coordinationWaitObserver(ctx)
	if observer == nil {
		return beginTopologyRead(waitCtx, scope)
	}
	started := time.Now()
	release, ok := beginTopologyRead(waitCtx, scope)
	observer(CoordinationWait{Phase: "gate_wait", Duration: time.Since(started), Attempted: true, Acquired: ok})
	return release, ok
}
