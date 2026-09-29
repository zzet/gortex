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
// build lane. Interactive requests may jump ahead of background reconciliation,
// but a bounded burst keeps background work from starving.
type ViewBuildPriority uint8

const (
	ViewBuildBackground ViewBuildPriority = iota
	ViewBuildInteractive

	maxInteractiveBuildBurst              = 4
	defaultInteractiveViewBuildQueueLimit = 128
	defaultBackgroundViewBuildQueueLimit  = 1024

	// viewBuildBackgroundStarvation is how long the oldest background waiter
	// must have waited before a burst of interactive grants gives way to it.
	// Below it, an interactive waiter never waits behind queued background
	// work; above it, background work still runs at least once per
	// maxInteractiveBuildBurst interactive grants.
	viewBuildBackgroundStarvation = 5 * time.Second
	// viewBuildInteractiveStarvation is how long an interactive waiter may be
	// passed over by later demand (AcquireRanked) before it is granted in
	// arrival order again.
	viewBuildInteractiveStarvation = 2 * time.Second
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
	// demand and promotionRequested are guarded by the gate mutex.
	demand             <-chan struct{}
	promotionRequested bool
	// rank reports when the waiter's newest demand arrived (Unix
	// nanoseconds; 0 for none); nil for a waiter that ranks by arrival only.
	rank func() int64
}

// ViewBuildGateStats is a fixed-cardinality process-local snapshot. Queue
// depths exclude the active build. No repository, checkout, ref, or path is
// retained in these statistics.
type ViewBuildGateStats struct {
	Open   bool
	Active bool

	InteractiveLimit int
	BackgroundLimit  int

	InteractiveQueued int
	BackgroundQueued  int

	InteractiveHighWater int
	BackgroundHighWater  int

	AdmittedInteractive uint64
	AdmittedBackground  uint64
	RejectedInteractive uint64
	RejectedBackground  uint64
	CanceledInteractive uint64
	CanceledBackground  uint64

	WaitSamples uint64
	TotalWait   time.Duration
	MaxWait     time.Duration

	// YieldRequests counts background holders asked to give the lane up to
	// interactive demand (NoteYieldable); YieldRefusals counts holders that
	// had already yielded maxViewBuildYields times and were left to finish.
	YieldRequests uint64
	YieldRefusals uint64

	// ActiveSince is when the active build was granted the lane (zero when
	// idle), and Holder what it declared itself to be (NoteHolder); an active
	// lane with no Holder is held by a builder that declares nothing.
	ActiveSince time.Time
	Holder      *ViewBuildLaneHolder
}

// ViewBuildLaneHolder names the build holding the lane, so a wait for it can
// be attributed. It carries a checkout id and a generation at most — the same
// bounded identity a cycle report does.
type ViewBuildLaneHolder struct {
	// Kind is what the holder is doing: checkout_cycle,
	// dirty_chain_compaction, checkout_mutation, checkout_transition, ...
	Kind       string
	CheckoutID string
	Priority   string
	// Generation is the generation the holder is building over or for, when
	// it has one (a compaction's chain top, say).
	Generation int64
	// Since is when the lane was granted to it.
	Since time.Time
}

// ViewBuildGate serializes physical derived-view builds after daemon warmup.
// Its independently bounded queues provide overload backpressure without
// imposing a semantic limit on worktrees, refs, or overlays.
type ViewBuildGate struct {
	mu sync.Mutex

	open   bool
	opened chan struct{}
	active bool

	interactive []*viewBuildWaiter
	background  []*viewBuildWaiter
	// Avoid scanning ordinary Acquire queues which contain no demand signals.
	promotableQueued int

	interactiveBurst int
	interactiveLimit int
	backgroundLimit  int

	// backgroundStarvation and interactiveStarvation are the starvation
	// bounds (viewBuildBackgroundStarvation, viewBuildInteractiveStarvation).
	backgroundStarvation  time.Duration
	interactiveStarvation time.Duration

	interactiveHighWater int
	backgroundHighWater  int

	admittedInteractive uint64
	admittedBackground  uint64
	rejectedInteractive uint64
	rejectedBackground  uint64
	canceledInteractive uint64
	canceledBackground  uint64

	waitSamples uint64
	totalWait   time.Duration
	maxWait     time.Duration

	// activeSince and holder describe the active build (Stats).
	activeSince time.Time
	holder      *ViewBuildLaneHolder

	// activePriority is the priority the active build was granted at.
	// yield is the cooperative-preemption channel a background holder armed
	// (NoteYieldable); yieldClosed records that it was closed. Both are
	// reset whenever the lane changes hands.
	activePriority ViewBuildPriority
	yield          chan struct{}
	yieldClosed    bool
	yieldRequests  uint64
	yieldRefusals  uint64
}

// maxViewBuildYields bounds how many times one piece of background work may
// give the lane up to interactive demand. Past it, NoteYieldable arms
// nothing and the work runs to completion, so sustained interactive load
// cannot livelock a background build that must eventually publish.
const maxViewBuildYields = 3

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

func NewViewBuildGate() *ViewBuildGate {
	return newViewBuildGateWithLimits(
		defaultInteractiveViewBuildQueueLimit,
		defaultBackgroundViewBuildQueueLimit,
	)
}

func newViewBuildGateWithLimits(interactiveLimit, backgroundLimit int) *ViewBuildGate {
	if interactiveLimit < 0 || backgroundLimit < 0 {
		panic("indexer: view build queue limits must be non-negative")
	}
	return &ViewBuildGate{
		opened:                make(chan struct{}),
		interactiveLimit:      interactiveLimit,
		backgroundLimit:       backgroundLimit,
		backgroundStarvation:  viewBuildBackgroundStarvation,
		interactiveStarvation: viewBuildInteractiveStarvation,
	}
}

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

// Acquire waits for the one physical build lane. Capacity applies only while a
// caller must wait: even a zero-capacity gate admits an idle open lane.
func (g *ViewBuildGate) Acquire(ctx context.Context, priority ViewBuildPriority) (func(), error) {
	return g.AcquirePromotable(ctx, priority, nil)
}

// AcquirePromotable is Acquire with a coalesced demand signal. A signal promotes
// a queued background waiter to the interactive tail; it never preempts an
// active build. If the interactive queue is full, the waiter retains its
// background place and promotion is retried as capacity becomes available.
// Demand is consumed under the gate mutex before admission decisions; queue
// statistics may still classify it as background until that scheduling point.
// The caller owns demand; a buffered channel of capacity one coalesces signals.
func (g *ViewBuildGate) AcquirePromotable(ctx context.Context, priority ViewBuildPriority, demand <-chan struct{}) (func(), error) {
	return g.AcquireRanked(ctx, priority, demand, nil)
}

// AcquireRanked is AcquirePromotable for a waiter that can say when its newest
// demand arrived: rank reports it (Unix nanoseconds, 0 for none) and is read
// under the gate mutex at every grant, so demand that arrives while the
// caller waits counts. Among interactive waiters, one whose demand arrived
// after the oldest interactive waiter began waiting — a checkout the user
// has just edited or queried — is granted before it, newest demand first;
// otherwise, and for any waiter passed over for interactiveStarvation, the
// order is arrival. rank must not block or take a lock that is held while
// calling into the gate.
func (g *ViewBuildGate) AcquireRanked(ctx context.Context, priority ViewBuildPriority, demand <-chan struct{}, rank func() int64) (func(), error) {
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
	promotionRequested := false

	g.mu.Lock()
	// Restore the invariant before evaluating the immediate path. Normally all
	// state transitions already call grantNextLocked.
	g.grantNextLocked()
	idle := g.open && !g.active && len(g.interactive) == 0 && len(g.background) == 0
	canWait := len(g.interactive) < g.interactiveLimit
	if priority == ViewBuildBackground {
		canWait = canWait || len(g.background) < g.backgroundLimit
	}
	// Leave demand buffered when admission must reject; a caller can retry
	// without losing the selection which gave the request priority.
	if idle || canWait {
		select {
		case <-demand:
			promotionRequested = true
			demand = nil
		default:
		}
	}
	if idle {
		if promotionRequested {
			priority = ViewBuildInteractive
		}
		g.active = true
		g.activeSince, g.holder = time.Now(), nil
		g.activePriority, g.yield, g.yieldClosed = priority, nil, false
		g.recordPriorityLocked(priority)
		g.recordAdmittedLocked(priority)
		g.mu.Unlock()
		return g.releaseFunc(), nil
	}
	if err := ctx.Err(); err != nil {
		g.mu.Unlock()
		return nil, err
	}

	if promotionRequested && priority == ViewBuildBackground && len(g.interactive) < g.interactiveLimit {
		priority = ViewBuildInteractive
	}
	if priority == ViewBuildInteractive {
		demand = nil
		promotionRequested = false
	}

	limit, queued := g.backgroundLimit, len(g.background)
	if priority == ViewBuildInteractive {
		limit, queued = g.interactiveLimit, len(g.interactive)
	}
	if queued >= limit {
		g.recordRejectedLocked(priority)
		g.mu.Unlock()
		return nil, &ViewBuildQueueFullError{Priority: priority, Limit: limit}
	}

	waiter := &viewBuildWaiter{
		ready:              make(chan struct{}),
		priority:           priority,
		enqueuedAt:         time.Now(),
		demand:             demand,
		promotionRequested: promotionRequested,
		rank:               rank,
	}
	if priority == ViewBuildInteractive {
		g.interactive = append(g.interactive, waiter)
		if len(g.interactive) > g.interactiveHighWater {
			g.interactiveHighWater = len(g.interactive)
		}
		g.requestYieldLocked()
	} else {
		g.background = append(g.background, waiter)
		if waiter.demand != nil || waiter.promotionRequested {
			g.promotableQueued++
		}
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
			if waiter.priority == ViewBuildInteractive {
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
	g.activeSince, g.holder = time.Time{}, nil
	g.yield, g.yieldClosed = nil, false
	g.grantNextLocked()
}

// NoteHolder declares what the caller holding the lane is doing; it shows in
// Stats until the lane is released or the returned func withdraws it. A
// caller that does not hold the lane declares nothing.
func (g *ViewBuildGate) NoteHolder(holder ViewBuildLaneHolder) func() {
	if g == nil {
		return func() {}
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.active {
		return func() {}
	}
	holder.Since = g.activeSince
	noted := &holder
	g.holder = noted
	return func() {
		g.mu.Lock()
		if g.holder == noted {
			g.holder = nil
		}
		g.mu.Unlock()
	}
}

// NoteYieldable arms cooperative preemption for the background build holding
// the lane: the returned channel is closed as soon as an interactive build
// starts waiting (immediately, if one already is). The holder is expected to
// check it at its safe points — phase boundaries, between files — abandon
// its unpublished work, release the lane, and queue again; the gate never
// takes the lane back by itself, so a holder that ignores the channel only
// keeps the old run-to-completion behaviour.
//
// yields is how many times this piece of work has already given the lane up.
// At maxViewBuildYields, for an interactive holder, or for a caller that does
// not hold the lane, NoteYieldable arms nothing and returns a nil channel
// (which never fires). withdraw disarms it (at the holder's commit point,
// say, after which giving up would waste more than it saves).
func (g *ViewBuildGate) NoteYieldable(yields int) (yield <-chan struct{}, withdraw func()) {
	if g == nil {
		return nil, func() {}
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.active || g.activePriority != ViewBuildBackground {
		return nil, func() {}
	}
	if yields >= maxViewBuildYields {
		g.yieldRefusals++
		return nil, func() {}
	}
	if g.yield == nil {
		g.yield, g.yieldClosed = make(chan struct{}), false
	}
	armed := g.yield
	if len(g.interactive) > 0 {
		g.requestYieldLocked()
	}
	return armed, func() {
		g.mu.Lock()
		if g.yield == armed {
			g.yield, g.yieldClosed = nil, false
		}
		g.mu.Unlock()
	}
}

// requestYieldLocked asks an armed background holder to give the lane up.
// Caller holds g.mu.
func (g *ViewBuildGate) requestYieldLocked() {
	if !g.active || g.activePriority != ViewBuildBackground || g.yield == nil || g.yieldClosed {
		return
	}
	close(g.yield)
	g.yieldClosed = true
	g.yieldRequests++
}

func (g *ViewBuildGate) grantNextLocked() {
	g.promoteDemandedLocked()
	if !g.open || g.active {
		return
	}
	now := time.Now()
	for {
		var waiter *viewBuildWaiter
		switch {
		case len(g.interactive) > 0 && (len(g.background) == 0 || g.interactiveBurst < maxInteractiveBuildBurst ||
			now.Sub(g.background[0].enqueuedAt) < g.backgroundStarvation):
			// Interactive first. Queued background work overtakes only once
			// a burst of interactive grants has passed AND it has waited
			// past the starvation bound.
			waiter = g.takeInteractiveLocked(now)
		case len(g.background) > 0:
			waiter = g.background[0]
			g.background = g.background[1:]
		case len(g.interactive) > 0:
			waiter = g.takeInteractiveLocked(now)
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
		g.activeSince, g.holder = time.Now(), nil
		g.activePriority, g.yield, g.yieldClosed = waiter.priority, nil, false
		waiter.granted = true
		g.recordAdmittedLocked(waiter.priority)
		close(waiter.ready)
		return
	}
}

// takeInteractiveLocked removes and returns the interactive waiter to grant
// next: the oldest, unless a ranked waiter's newest demand arrived after the
// oldest began waiting, in which case the ranked waiter with the newest
// demand goes first. A head passed over for interactiveStarvation is granted
// in arrival order regardless.
func (g *ViewBuildGate) takeInteractiveLocked(now time.Time) *viewBuildWaiter {
	pick := 0
	head := g.interactive[0]
	if !head.canceled && now.Sub(head.enqueuedAt) < g.interactiveStarvation {
		newest := head.enqueuedAt.UnixNano()
		for i, waiter := range g.interactive {
			if waiter.canceled || waiter.rank == nil {
				continue
			}
			if demanded := waiter.rank(); demanded > newest {
				pick, newest = i, demanded
			}
		}
	}
	waiter := g.interactive[pick]
	copy(g.interactive[pick:], g.interactive[pick+1:])
	g.interactive[len(g.interactive)-1] = nil
	g.interactive = g.interactive[:len(g.interactive)-1]
	return waiter
}

// promoteDemandedLocked also samples signals in the granting goroutine, so
// a release cannot overlook buffered demand merely because the waiting
// goroutine has not been scheduled yet. Compaction preserves background FIFO;
// promotions join the interactive tail and keep their original wait start.
func (g *ViewBuildGate) promoteDemandedLocked() {
	if g.promotableQueued == 0 {
		return
	}
	kept := g.background[:0]
	for _, waiter := range g.background {
		if waiter.demand != nil {
			select {
			case <-waiter.demand:
				waiter.demand = nil
				waiter.promotionRequested = true
			default:
			}
		}
		if waiter.promotionRequested && len(g.interactive) < g.interactiveLimit {
			g.promotableQueued--
			waiter.promotionRequested = false
			waiter.priority = ViewBuildInteractive
			g.interactive = append(g.interactive, waiter)
			if len(g.interactive) > g.interactiveHighWater {
				g.interactiveHighWater = len(g.interactive)
			}
			g.requestYieldLocked()
			viewmetrics.AddGauge(viewmetrics.ViewBuildQueue, -1, viewBuildPriorityLabel(ViewBuildBackground))
			viewmetrics.AddGauge(viewmetrics.ViewBuildQueue, 1, viewBuildPriorityLabel(ViewBuildInteractive))
			continue
		}
		kept = append(kept, waiter)
	}
	clear(g.background[len(kept):])
	g.background = kept
}

func (g *ViewBuildGate) recordPriorityLocked(priority ViewBuildPriority) {
	if priority == ViewBuildInteractive {
		if g.interactiveBurst < maxInteractiveBuildBurst {
			g.interactiveBurst++
		}
		return
	}
	g.interactiveBurst = 0
}

func (g *ViewBuildGate) recordDequeuedLocked(waiter *viewBuildWaiter) {
	if waiter.priority == ViewBuildBackground && (waiter.demand != nil || waiter.promotionRequested) {
		g.promotableQueued--
	}
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
	if priority == ViewBuildInteractive {
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
	if priority == ViewBuildInteractive {
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
	if priority == ViewBuildInteractive {
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
	if priority == ViewBuildInteractive {
		return priority
	}
	return ViewBuildBackground
}

func viewBuildPriorityLabel(priority ViewBuildPriority) string {
	if priority == ViewBuildInteractive {
		return viewmetrics.BuildPriorityInteractive
	}
	return viewmetrics.BuildPriorityBackground
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
		return ViewBuildGateStats{Open: true}
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return ViewBuildGateStats{
		Open:                 g.open,
		Active:               g.active,
		InteractiveLimit:     g.interactiveLimit,
		BackgroundLimit:      g.backgroundLimit,
		InteractiveQueued:    len(g.interactive),
		BackgroundQueued:     len(g.background),
		InteractiveHighWater: g.interactiveHighWater,
		BackgroundHighWater:  g.backgroundHighWater,
		AdmittedInteractive:  g.admittedInteractive,
		AdmittedBackground:   g.admittedBackground,
		RejectedInteractive:  g.rejectedInteractive,
		RejectedBackground:   g.rejectedBackground,
		CanceledInteractive:  g.canceledInteractive,
		CanceledBackground:   g.canceledBackground,
		WaitSamples:          g.waitSamples,
		TotalWait:            g.totalWait,
		MaxWait:              g.maxWait,
		ActiveSince:          g.activeSince,
		Holder:               copyViewBuildLaneHolder(g.holder),
		YieldRequests:        g.yieldRequests,
		YieldRefusals:        g.yieldRefusals,
	}
}

func copyViewBuildLaneHolder(holder *ViewBuildLaneHolder) *ViewBuildLaneHolder {
	if holder == nil {
		return nil
	}
	copied := *holder
	return &copied
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

var admittedChannel = func() chan struct{} {
	ch := make(chan struct{})
	close(ch)
	return ch
}()
