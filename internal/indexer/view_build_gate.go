package indexer

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/zzet/gortex/internal/viewmetrics"
)

// ViewBuildPriority determines how a queued build competes for the one physical
// build lane. Required lifecycle publication always precedes client-requested
// and background work. Interactive requests may jump ahead of background
// reconciliation, but a bounded burst keeps background work from starving.
type ViewBuildPriority uint8

const (
	ViewBuildBackground ViewBuildPriority = iota
	ViewBuildInteractive
	ViewBuildRequired

	maxInteractiveBuildBurst = 4
	// Required publication is the frozen startup/lifecycle cohort. Zero means
	// unbounded: rejecting one of these owners because other required owners
	// woke first would turn healthy startup work into a terminal degraded
	// verdict. The cohort is already bounded by cataloged lifecycle state.
	defaultRequiredViewBuildQueueLimit    = 0
	defaultInteractiveViewBuildQueueLimit = 128
	defaultBackgroundViewBuildQueueLimit  = 1024
)

// ErrViewBuildQueueFull is a retryable overload signal. It limits queued
// callers, not tracked worktrees, refs, or the number of views that may exist.
var ErrViewBuildQueueFull = errors.New("indexer: view build queue full")

// ViewBuildQueueFullError identifies which independently bounded priority
// queue rejected an admission request.
type ViewBuildQueueFullError struct {
	Priority ViewBuildPriority
	Limit    int
}

func (e *ViewBuildQueueFullError) Error() string {
	return fmt.Sprintf("indexer: view build %s queue is full (limit %d)", viewBuildPriorityLabel(e.Priority), e.Limit)
}

func (e *ViewBuildQueueFullError) Unwrap() error { return ErrViewBuildQueueFull }

type viewBuildWaiter struct {
	ready      chan struct{}
	priority   ViewBuildPriority
	enqueuedAt time.Time
	granted    bool
	canceled   bool
}

// ViewBuildGateStats is a fixed-cardinality process-local snapshot. Queue
// depths exclude the active build. No repository, checkout, ref, or path is
// retained in these statistics.
type ViewBuildGateStats struct {
	Open         bool
	RequiredOpen bool
	Active       bool

	RequiredLimit    int
	InteractiveLimit int
	BackgroundLimit  int

	RequiredQueued    int
	InteractiveQueued int
	BackgroundQueued  int

	RequiredHighWater    int
	InteractiveHighWater int
	BackgroundHighWater  int

	AdmittedRequired    uint64
	AdmittedInteractive uint64
	AdmittedBackground  uint64
	RejectedRequired    uint64
	RejectedInteractive uint64
	RejectedBackground  uint64
	CanceledRequired    uint64
	CanceledInteractive uint64
	CanceledBackground  uint64

	WaitSamples uint64
	TotalWait   time.Duration
	MaxWait     time.Duration
}

// ViewBuildGate serializes physical derived-view builds after daemon warmup.
// Its independently bounded queues provide overload backpressure without
// imposing a semantic limit on worktrees, refs, or overlays.
type ViewBuildGate struct {
	mu sync.Mutex

	open           bool
	opened         chan struct{}
	requiredOpen   bool
	requiredOpened chan struct{}
	active         bool

	required    []*viewBuildWaiter
	interactive []*viewBuildWaiter
	background  []*viewBuildWaiter

	interactiveBurst int
	requiredLimit    int
	interactiveLimit int
	backgroundLimit  int

	requiredHighWater    int
	interactiveHighWater int
	backgroundHighWater  int

	admittedRequired    uint64
	admittedInteractive uint64
	admittedBackground  uint64
	rejectedRequired    uint64
	rejectedInteractive uint64
	rejectedBackground  uint64
	canceledRequired    uint64
	canceledInteractive uint64
	canceledBackground  uint64

	waitSamples uint64
	totalWait   time.Duration
	maxWait     time.Duration
}

func (g *ViewBuildGate) IsOpen() bool {
	if g == nil {
		return true
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.open
}

// IsRequiredOpen reports whether lifecycle publication work may enter the
// physical build lane. During daemon startup this becomes true before Open:
// configured/dedicated views publish first while ref and background work stay
// queued.
func (g *ViewBuildGate) IsRequiredOpen() bool {
	if g == nil {
		return true
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.requiredOpen
}

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

func (g *ViewBuildGate) WaitUntilRequiredOpen(ctx context.Context) error {
	if g == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	g.mu.Lock()
	if g.requiredOpen {
		g.mu.Unlock()
		return nil
	}
	opened := g.requiredOpened
	g.mu.Unlock()

	select {
	case <-opened:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func NewViewBuildGate() *ViewBuildGate {
	return newViewBuildGateWithAllLimits(
		defaultRequiredViewBuildQueueLimit,
		defaultInteractiveViewBuildQueueLimit,
		defaultBackgroundViewBuildQueueLimit,
	)
}

func newViewBuildGateWithLimits(interactiveLimit, backgroundLimit int) *ViewBuildGate {
	return newViewBuildGateWithAllLimits(
		defaultRequiredViewBuildQueueLimit,
		interactiveLimit,
		backgroundLimit,
	)
}

func newViewBuildGateWithAllLimits(requiredLimit, interactiveLimit, backgroundLimit int) *ViewBuildGate {
	if requiredLimit < 0 || interactiveLimit < 0 || backgroundLimit < 0 {
		panic("indexer: view build queue limits must be non-negative")
	}
	return &ViewBuildGate{
		opened:           make(chan struct{}),
		requiredOpened:   make(chan struct{}),
		requiredLimit:    requiredLimit,
		interactiveLimit: interactiveLimit,
		backgroundLimit:  backgroundLimit,
	}
}

// OpenRequired starts only durable lifecycle publication. It deliberately
// leaves interactive and background work queued until Open is called after the
// startup exact-view cohort reaches a terminal state.
func (g *ViewBuildGate) OpenRequired() {
	if g == nil {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.requiredOpen {
		return
	}
	g.requiredOpen = true
	close(g.requiredOpened)
	g.grantNextLocked()
}

func (g *ViewBuildGate) Open() {
	if g == nil {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.requiredOpen {
		g.requiredOpen = true
		close(g.requiredOpened)
	}
	if g.open {
		return
	}
	g.open = true
	close(g.opened)
	g.grantNextLocked()
}

// Acquire waits for the one physical build lane. Capacity applies only while a
// caller must wait: even a zero-capacity gate admits an idle open lane.
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
	priority = normalizeViewBuildPriority(priority)

	g.mu.Lock()
	// Restore the invariant before evaluating the immediate path. Normally all
	// state transitions already call grantNextLocked.
	g.grantNextLocked()
	if g.canAdmitImmediatelyLocked(priority) {
		g.active = true
		g.recordPriorityLocked(priority)
		g.recordAdmittedLocked(priority)
		g.mu.Unlock()
		return g.releaseFunc(), nil
	}
	if err := ctx.Err(); err != nil {
		g.mu.Unlock()
		return nil, err
	}

	limit, queued := g.backgroundLimit, len(g.background)
	if priority == ViewBuildRequired {
		limit, queued = g.requiredLimit, len(g.required)
	} else if priority == ViewBuildInteractive {
		limit, queued = g.interactiveLimit, len(g.interactive)
	}
	if (priority != ViewBuildRequired || limit > 0) && queued >= limit {
		g.recordRejectedLocked(priority)
		g.mu.Unlock()
		return nil, &ViewBuildQueueFullError{Priority: priority, Limit: limit}
	}

	waiter := &viewBuildWaiter{
		ready:      make(chan struct{}),
		priority:   priority,
		enqueuedAt: time.Now(),
	}
	if priority == ViewBuildRequired {
		g.required = append(g.required, waiter)
		if len(g.required) > g.requiredHighWater {
			g.requiredHighWater = len(g.required)
		}
	} else if priority == ViewBuildInteractive {
		g.interactive = append(g.interactive, waiter)
		if len(g.interactive) > g.interactiveHighWater {
			g.interactiveHighWater = len(g.interactive)
		}
	} else {
		g.background = append(g.background, waiter)
		if len(g.background) > g.backgroundHighWater {
			g.backgroundHighWater = len(g.background)
		}
	}
	viewmetrics.AddGauge(viewmetrics.ViewBuildQueue, 1, viewBuildPriorityLabel(priority))
	g.grantNextLocked()
	g.mu.Unlock()

	select {
	case <-waiter.ready:
		return g.releaseFunc(), nil
	case <-ctx.Done():
		g.mu.Lock()
		if waiter.granted {
			g.mu.Unlock()
			g.release()
		} else {
			waiter.canceled = true
			removed := false
			if waiter.priority == ViewBuildRequired {
				before := len(g.required)
				g.required = removeViewBuildWaiter(g.required, waiter)
				removed = len(g.required) != before
			} else if waiter.priority == ViewBuildInteractive {
				before := len(g.interactive)
				g.interactive = removeViewBuildWaiter(g.interactive, waiter)
				removed = len(g.interactive) != before
			} else {
				before := len(g.background)
				g.background = removeViewBuildWaiter(g.background, waiter)
				removed = len(g.background) != before
			}
			if removed {
				g.recordDequeuedLocked(waiter)
				g.recordCanceledLocked(waiter.priority)
			}
			g.grantNextLocked()
			g.mu.Unlock()
		}
		return nil, ctx.Err()
	}
}

func (g *ViewBuildGate) releaseFunc() func() {
	var once sync.Once
	return func() { once.Do(g.release) }
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
	if g.active {
		return
	}
	for {
		var waiter *viewBuildWaiter
		switch {
		case g.requiredOpen && len(g.required) > 0:
			waiter = g.required[0]
			g.required = g.required[1:]
		case !g.open:
			return
		case len(g.interactive) > 0 && (len(g.background) == 0 || g.interactiveBurst < maxInteractiveBuildBurst):
			waiter = g.interactive[0]
			g.interactive = g.interactive[1:]
		case len(g.background) > 0:
			waiter = g.background[0]
			g.background = g.background[1:]
		case len(g.interactive) > 0:
			waiter = g.interactive[0]
			g.interactive = g.interactive[1:]
		default:
			return
		}

		g.recordDequeuedLocked(waiter)
		if waiter.canceled {
			g.recordCanceledLocked(waiter.priority)
			continue
		}
		g.recordPriorityLocked(waiter.priority)
		g.active = true
		waiter.granted = true
		g.recordAdmittedLocked(waiter.priority)
		close(waiter.ready)
		return
	}
}

func (g *ViewBuildGate) recordPriorityLocked(priority ViewBuildPriority) {
	if priority == ViewBuildRequired {
		return
	}
	if priority == ViewBuildInteractive {
		if g.interactiveBurst < maxInteractiveBuildBurst {
			g.interactiveBurst++
		}
		return
	}
	g.interactiveBurst = 0
}

func (g *ViewBuildGate) recordDequeuedLocked(waiter *viewBuildWaiter) {
	priority := viewBuildPriorityLabel(waiter.priority)
	viewmetrics.AddGauge(viewmetrics.ViewBuildQueue, -1, priority)
	waited := time.Since(waiter.enqueuedAt)
	g.waitSamples++
	g.totalWait += waited
	if waited > g.maxWait {
		g.maxWait = waited
	}
	viewmetrics.Observe(viewmetrics.ViewBuildWaitSeconds, waited, priority)
}

func (g *ViewBuildGate) recordAdmittedLocked(priority ViewBuildPriority) {
	if priority == ViewBuildRequired {
		g.admittedRequired++
	} else if priority == ViewBuildInteractive {
		g.admittedInteractive++
	} else {
		g.admittedBackground++
	}
	viewmetrics.Count(
		viewmetrics.ViewBuildAdmissionTotal,
		viewBuildPriorityLabel(priority),
		viewmetrics.BuildAdmissionAdmitted,
	)
}

func (g *ViewBuildGate) recordRejectedLocked(priority ViewBuildPriority) {
	if priority == ViewBuildRequired {
		g.rejectedRequired++
	} else if priority == ViewBuildInteractive {
		g.rejectedInteractive++
	} else {
		g.rejectedBackground++
	}
	viewmetrics.Count(
		viewmetrics.ViewBuildAdmissionTotal,
		viewBuildPriorityLabel(priority),
		viewmetrics.BuildAdmissionRejected,
	)
}

func (g *ViewBuildGate) recordCanceledLocked(priority ViewBuildPriority) {
	if priority == ViewBuildRequired {
		g.canceledRequired++
	} else if priority == ViewBuildInteractive {
		g.canceledInteractive++
	} else {
		g.canceledBackground++
	}
	viewmetrics.Count(
		viewmetrics.ViewBuildAdmissionTotal,
		viewBuildPriorityLabel(priority),
		viewmetrics.BuildAdmissionCanceled,
	)
}

func normalizeViewBuildPriority(priority ViewBuildPriority) ViewBuildPriority {
	if priority == ViewBuildRequired || priority == ViewBuildInteractive {
		return priority
	}
	return ViewBuildBackground
}

func viewBuildPriorityLabel(priority ViewBuildPriority) string {
	if priority == ViewBuildRequired {
		return viewmetrics.BuildPriorityRequired
	}
	if priority == ViewBuildInteractive {
		return viewmetrics.BuildPriorityInteractive
	}
	return viewmetrics.BuildPriorityBackground
}

func (g *ViewBuildGate) canAdmitImmediatelyLocked(priority ViewBuildPriority) bool {
	if g.active {
		return false
	}
	if priority == ViewBuildRequired {
		return g.requiredOpen && len(g.required) == 0
	}
	return g.open && len(g.required) == 0 && len(g.interactive) == 0 && len(g.background) == 0
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

func (g *ViewBuildGate) Stats() ViewBuildGateStats {
	if g == nil {
		return ViewBuildGateStats{Open: true, RequiredOpen: true}
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return ViewBuildGateStats{
		Open:                 g.open,
		RequiredOpen:         g.requiredOpen,
		Active:               g.active,
		RequiredLimit:        g.requiredLimit,
		InteractiveLimit:     g.interactiveLimit,
		BackgroundLimit:      g.backgroundLimit,
		RequiredQueued:       len(g.required),
		InteractiveQueued:    len(g.interactive),
		BackgroundQueued:     len(g.background),
		RequiredHighWater:    g.requiredHighWater,
		InteractiveHighWater: g.interactiveHighWater,
		BackgroundHighWater:  g.backgroundHighWater,
		AdmittedRequired:     g.admittedRequired,
		AdmittedInteractive:  g.admittedInteractive,
		AdmittedBackground:   g.admittedBackground,
		RejectedRequired:     g.rejectedRequired,
		RejectedInteractive:  g.rejectedInteractive,
		RejectedBackground:   g.rejectedBackground,
		CanceledRequired:     g.canceledRequired,
		CanceledInteractive:  g.canceledInteractive,
		CanceledBackground:   g.canceledBackground,
		WaitSamples:          g.waitSamples,
		TotalWait:            g.totalWait,
		MaxWait:              g.maxWait,
	}
}

// Admitted reports whether daemon warmup has opened the build gate.
func (g *ViewBuildGate) Admitted() bool {
	if g == nil {
		return true
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.open
}

func (g *ViewBuildGate) Opened() <-chan struct{} {
	if g == nil {
		return admittedChannel
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.opened
}

// RequiredOpened is the startup publication edge. Coordinators responsible
// for the frozen exact cohort wait here; ordinary coordinators continue to
// wait on Opened until every required member is terminal.
func (g *ViewBuildGate) RequiredOpened() <-chan struct{} {
	if g == nil {
		return admittedChannel
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.requiredOpened
}

var admittedChannel = func() chan struct{} {
	ch := make(chan struct{})
	close(ch)
	return ch
}()
