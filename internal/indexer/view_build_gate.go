package indexer

import (
	"context"
	"sync"
)

// ViewBuildPriority orders work waiting for the one derived-view build lane.
// An interactive ref selection goes ahead of automatic refreshes that have not
// started yet; an active build is never preempted.
type ViewBuildPriority uint8

const (
	ViewBuildBackground ViewBuildPriority = iota
	ViewBuildInteractive

	// maxInteractiveBuildBurst bounds how many user-requested builds may pass a
	// background build that is already waiting. Interactive work stays
	// responsive without allowing a continuous ref-view stream to starve
	// checkout reconciliation forever.
	maxInteractiveBuildBurst = 4
)

type viewBuildWaiter struct {
	ready    chan struct{}
	granted  bool
	canceled bool
}

// ViewBuildGate is both the daemon warmup latch and the shared admission lane
// for derived generations. SQLite has one physical writer, so waking every
// checkout and ref build at once only creates a queue inside writeMu while
// starving control/catalog writes. Keeping that queue here makes it bounded,
// cancellation-aware, and able to prefer user-requested ref views.
type ViewBuildGate struct {
	mu     sync.Mutex
	open   bool
	opened chan struct{}
	active bool

	interactive      []*viewBuildWaiter
	background       []*viewBuildWaiter
	interactiveBurst int
}

// IsOpen reports whether builds may proceed. The gate is monotonic: once open,
// it never closes, so callers may safely decide whether waiting would violate
// an asynchronous API contract.
func (g *ViewBuildGate) IsOpen() bool {
	if g == nil {
		return true
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.open
}

// WaitUntilOpen waits for daemon warmup without entering the derived-build
// queue or consuming its single active slot.
func (g *ViewBuildGate) WaitUntilOpen(ctx context.Context) error {
	if g == nil {
		return nil
	}

	g.mu.Lock()
	if g.open {
		g.mu.Unlock()
		return nil
	}
	opened := g.opened
	g.mu.Unlock()

	select {
	case <-opened:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// NewViewBuildGate returns a closed, single-build gate. Open releases warmup;
// Acquire then serializes derived builds for the lifetime of the daemon.
func NewViewBuildGate() *ViewBuildGate {
	return &ViewBuildGate{opened: make(chan struct{})}
}

// Open admits build work and wakes everything waiting on warmup. It is
// idempotent. Only one queued build is granted; subsequent work advances when
// its predecessor releases the admission lease.
func (g *ViewBuildGate) Open() {
	if g == nil {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.open {
		return
	}
	g.open = true
	close(g.opened)
	g.grantNextLocked()
}

// Acquire waits for warmup and for the shared derived-build lane. The returned
// release function is idempotent and must be called. A nil gate admits work
// immediately, preserving embedded and test callers that have no daemon gate.
func (g *ViewBuildGate) Acquire(ctx context.Context, priority ViewBuildPriority) (func(), error) {
	if g == nil {
		return func() {}, nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	waiter := &viewBuildWaiter{ready: make(chan struct{})}

	g.mu.Lock()
	if priority == ViewBuildInteractive {
		g.interactive = append(g.interactive, waiter)
	} else {
		g.background = append(g.background, waiter)
	}
	g.grantNextLocked()
	g.mu.Unlock()

	select {
	case <-waiter.ready:
		var once sync.Once
		return func() { once.Do(g.release) }, nil
	case <-ctx.Done():
		g.mu.Lock()
		if waiter.granted {
			g.mu.Unlock()
			g.release()
		} else {
			waiter.canceled = true
			g.interactive = removeViewBuildWaiter(g.interactive, waiter)
			g.background = removeViewBuildWaiter(g.background, waiter)
			g.grantNextLocked()
			g.mu.Unlock()
		}
		return nil, ctx.Err()
	}
}

func (g *ViewBuildGate) release() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.active {
		return
	}
	g.active = false
	g.grantNextLocked()
}

func (g *ViewBuildGate) grantNextLocked() {
	if !g.open || g.active {
		return
	}
	for {
		var waiter *viewBuildWaiter
		interactive := false
		switch {
		case len(g.interactive) > 0 &&
			(len(g.background) == 0 || g.interactiveBurst < maxInteractiveBuildBurst):
			waiter = g.interactive[0]
			g.interactive = g.interactive[1:]
			interactive = true
		case len(g.background) > 0:
			waiter = g.background[0]
			g.background = g.background[1:]
		case len(g.interactive) > 0:
			waiter = g.interactive[0]
			g.interactive = g.interactive[1:]
			interactive = true
		default:
			return
		}
		if waiter.canceled {
			continue
		}
		if interactive {
			g.interactiveBurst++
		} else {
			g.interactiveBurst = 0
		}
		g.active = true
		waiter.granted = true
		close(waiter.ready)
		return
	}
}

func removeViewBuildWaiter(queue []*viewBuildWaiter, target *viewBuildWaiter) []*viewBuildWaiter {
	for i, waiter := range queue {
		if waiter != target {
			continue
		}
		copy(queue[i:], queue[i+1:])
		queue[len(queue)-1] = nil
		return queue[:len(queue)-1]
	}
	return queue
}

// Admitted reports only whether warmup has opened. Capacity is obtained with
// Acquire; callers use this method to decide whether to return a labeled
// deferred/building response without waiting for warmup.
func (g *ViewBuildGate) Admitted() bool {
	if g == nil {
		return true
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.open
}

// Opened is closed once warmup admits builds. It remains for coordinator loop
// wakeups; actual build admission is always obtained through Acquire.
func (g *ViewBuildGate) Opened() <-chan struct{} {
	if g == nil {
		return admittedChannel
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.opened
}

var admittedChannel = func() chan struct{} {
	ch := make(chan struct{})
	close(ch)
	return ch
}()
