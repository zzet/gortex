package indexer

import (
	"context"
	"sync"
)

// laneYieldedToInteractive is the CheckoutCycle.YieldedTo value of a
// background cycle that gave the build lane up to an interactive build.
const laneYieldedToInteractive = "interactive_build"

// backgroundLaneYield is one background lane holder's side of cooperative
// preemption (ViewBuildGate.NoteYieldable): it cancels the holder's work
// context when an interactive build starts waiting for the lane, unless the
// work has already reached its commit point.
//
// The commit point is the instant a build is about to publish. Publication is
// milliseconds, while everything before it is the build, so abandoning there
// wastes the whole build to save almost nothing; and once a generation is
// published the route flip that follows must not be torn by a cancel either.
// Past the commit point the holder runs to the end of its cycle.
//
// fire and commit exclude each other under mu, and fire cancels while holding
// it, so a build that passed commit never sees a cancel from here, and a build
// whose commit lost the race finds its context already canceled.
type backgroundLaneYield struct {
	mu        sync.Mutex
	cancel    context.CancelFunc
	withdraw  func()
	committed bool
	yielded   bool

	stop     chan struct{}
	stopOnce sync.Once
}

type buildCommitPointKey struct{}

// armBackgroundLaneYield arms cooperative preemption for the background work
// holding gate's lane. It returns ctx unchanged and a nil yield (whose methods
// are no-ops) when the gate arms nothing: the holder is interactive, the work
// has already yielded maxViewBuildYields times, or the caller does not hold
// the lane. Otherwise the returned context is canceled when an interactive
// build starts waiting, and it carries the commit point a build reaches
// through reachBuildCommitPoint. The caller must call close when its cycle
// ends.
func armBackgroundLaneYield(ctx context.Context, gate *ViewBuildGate, yields int) (context.Context, *backgroundLaneYield) {
	yield, withdraw := gate.NoteYieldable(yields)
	if yield == nil {
		withdraw()
		return ctx, nil
	}
	ctx, cancel := context.WithCancel(ctx)
	y := &backgroundLaneYield{cancel: cancel, withdraw: withdraw, stop: make(chan struct{})}
	go func() {
		select {
		case <-yield:
			y.fire()
		case <-y.stop:
		case <-ctx.Done():
		}
	}()
	return context.WithValue(ctx, buildCommitPointKey{}, y), y
}

// fire cancels the holder's work, unless it has passed its commit point.
func (y *backgroundLaneYield) fire() {
	y.mu.Lock()
	defer y.mu.Unlock()
	if y.committed || y.yielded {
		return
	}
	y.yielded = true
	y.cancel()
}

// commit marks the commit point: from here the work is not canceled for an
// interactive build. It reports false when the work had already yielded (its
// context is then canceled).
func (y *backgroundLaneYield) commit() bool {
	if y == nil {
		return true
	}
	y.mu.Lock()
	if y.yielded {
		y.mu.Unlock()
		return false
	}
	already := y.committed
	y.committed = true
	y.mu.Unlock()
	if !already {
		y.withdraw()
	}
	return true
}

// Yielded reports whether the work was canceled to give the lane up.
func (y *backgroundLaneYield) Yielded() bool {
	if y == nil {
		return false
	}
	y.mu.Lock()
	defer y.mu.Unlock()
	return y.yielded
}

// close disarms the yield and releases the derived context. Safe on nil and
// more than once.
func (y *backgroundLaneYield) close() {
	if y == nil {
		return
	}
	y.stopOnce.Do(func() {
		close(y.stop)
		y.withdraw()
		y.cancel()
	})
}

// reachBuildCommitPoint is called by a build right before it publishes. For a
// build running under an armed background yield it withdraws the yield, so an
// interactive build arriving from here on waits for the (milliseconds-long)
// publication instead of discarding the build. The caller re-checks ctx after
// it: a yield that won the race has already canceled it.
func reachBuildCommitPoint(ctx context.Context) {
	if ctx == nil {
		return
	}
	if y, _ := ctx.Value(buildCommitPointKey{}).(*backgroundLaneYield); y != nil {
		y.commit()
	}
	// The movement abort of a background build (checkout_motion.go) shares
	// the commit point.
	commitTreeMoveAbort(ctx)
}
