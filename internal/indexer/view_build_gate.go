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

// ErrFamilyNotAdmitted is returned by AcquireAdmittedForFamily when the
// family is not eligible for the lane: the warmup latch is closed and the
// family's readiness is absent or was revoked while the caller queued.
var ErrFamilyNotAdmitted = errors.New("indexer: family not admitted to the view build lane")

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
	ready    chan struct{}
	priority ViewBuildPriority
	// family names the repository family this build belongs to; empty for a
	// family-less build. A family waiter is eligible for the lane once the
	// latch is open OR its family's base is published (familyReady).
	family     string
	enqueuedAt time.Time
	// backgroundAgeCredit affects scheduling only; wait metrics use enqueuedAt.
	backgroundAgeCredit time.Duration
	granted             bool
	canceled            bool
	// admittedOnly waiters are dropped with revoked set, instead of parked,
	// when their family's readiness is revoked while they queue.
	admittedOnly bool
	revoked      bool
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
	// Root is the holder checkout's working tree and Reason why its cycle
	// runs (a poll, a refresh ticket, a signal): what a waiter's slow
	// admission names as the lane's owner.
	Root   string
	Reason string
}

// ViewBuildGate serializes physical derived-view builds after daemon warmup.
// Its independently bounded queues provide overload backpressure without
// imposing a semantic limit on worktrees, refs, or overlays.
type ViewBuildGate struct {
	mu sync.Mutex
	// importPreparation bounds unpublished one-file work outside the lane.
	importPreparation chan struct{}

	open   bool
	opened chan struct{}
	active bool

	// familyReady names families whose own committed base is published, so a
	// build for one of them is eligible for the lane even while the global
	// warmup latch (open) is still closed. familyWake holds the per-family
	// eligibility channels a deferred waiter blocks on; each is closed when
	// its family becomes ready or the latch opens. Both are guarded by mu.
	familyReady map[string]struct{}
	familyWake  map[string]chan struct{}
	// familyEpoch counts readiness resets per family. A mark carries the epoch
	// read before the catalog evidence that earned it, so evidence observed
	// before a primary replacement can never re-admit the family afterwards.
	familyEpoch map[string]uint64
	// familyTurnover counts primary moves in progress per family. While one is,
	// MarkFamilyReady refuses: the catalog may still name the incumbent.
	familyTurnover map[string]int

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
	for family, ch := range g.familyWake {
		close(ch)
		delete(g.familyWake, family)
	}
	g.grantNextLocked()
	// Parked interactive waiters are now eligible; preempt an active background
	// build for them just as an interactive enqueue would.
	g.requestYieldLocked()
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
	return g.acquireRanked(ctx, priority, demand, rank, 0, "", false)
}

// AcquireForFamily is Acquire for a build that belongs to a repository family.
// It is eligible for the lane once the global latch is open OR the family's own
// committed base is published (MarkFamilyReady), so a worktree whose base is
// ready is served during startup without waiting for unrelated families to
// finish warming. A empty family keeps the global-only rule.
func (g *ViewBuildGate) AcquireForFamily(ctx context.Context, priority ViewBuildPriority, family string) (func(), error) {
	return g.AcquireRankedForFamily(ctx, priority, nil, nil, family)
}

// AcquireRankedForFamily is AcquireRanked with a family eligibility bypass.
func (g *ViewBuildGate) AcquireRankedForFamily(ctx context.Context, priority ViewBuildPriority, demand <-chan struct{}, rank func() int64, family string) (func(), error) {
	return g.acquireRanked(ctx, priority, demand, rank, 0, family, false)
}

// AcquireAdmittedForFamily is AcquireRankedForFamily for a caller that must
// not park on an ineligible family: it fails with ErrFamilyNotAdmitted when the
// family is not eligible now, or when its readiness is revoked while it queues
// behind another build.
func (g *ViewBuildGate) AcquireAdmittedForFamily(ctx context.Context, priority ViewBuildPriority, demand <-chan struct{}, rank func() int64, family string) (func(), error) {
	return g.acquireRanked(ctx, priority, demand, rank, 0, family, true)
}

// acquireRetirement carries offered-debt age across bounded service turns.
// It changes neither background FIFO nor the foreground burst guarantee.
func (g *ViewBuildGate) acquireRetirement(ctx context.Context, debtAge time.Duration) (func(), error) {
	return g.acquireRanked(ctx, ViewBuildBackground, nil, nil, debtAge, "", false)
}

func (g *ViewBuildGate) acquireRanked(ctx context.Context, priority ViewBuildPriority, demand <-chan struct{}, rank func() int64, backgroundAgeCredit time.Duration, family string, admittedOnly bool) (func(), error) {
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
	if priority != ViewBuildBackground || backgroundAgeCredit < 0 {
		backgroundAgeCredit = 0
	}
	if backgroundAgeCredit > g.backgroundStarvation {
		backgroundAgeCredit = g.backgroundStarvation
	}
	// Restore the invariant before evaluating the immediate path. Normally all
	// state transitions already call grantNextLocked.
	g.grantNextLocked()
	// An eligible caller — the latch open, or its family's committed base
	// published — takes an idle lane directly. grantNextLocked just gave any
	// eligible queued waiter first claim; if the lane is still idle, parked
	// ineligible waiters cannot run, so they neither consume the idle lane nor
	// reject this caller at the queue limit. A family-less caller while closed
	// is ineligible and still queues exactly as before.
	eligible := g.familyReadyLocked(family)
	if admittedOnly && !eligible {
		g.mu.Unlock()
		return nil, ErrFamilyNotAdmitted
	}
	idle := !g.active && eligible
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
		ready:               make(chan struct{}),
		priority:            priority,
		family:              family,
		enqueuedAt:          time.Now(),
		backgroundAgeCredit: backgroundAgeCredit,
		demand:              demand,
		promotionRequested:  promotionRequested,
		rank:                rank,
		admittedOnly:        admittedOnly,
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
		if waiter.revoked {
			return nil, ErrFamilyNotAdmitted
		}
		return g.releaseFunc(), nil
	case <-ctx.Done():
		g.mu.Lock()
		if waiter.granted {
			g.mu.Unlock()
			g.release()
		} else if waiter.revoked {
			g.mu.Unlock()
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
	if g.eligibleCountLocked(g.interactive) > 0 {
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

// requestYieldLocked asks an armed background holder to give the lane up to an
// interactive waiter that can actually run. A parked ineligible waiter (a
// family-less ref build behind the closed latch, say) must not preempt a ready
// family's background build: it cannot take the lane, so yielding to it would
// only churn the ready build until the latch opens. Caller holds g.mu.
func (g *ViewBuildGate) requestYieldLocked() {
	if !g.active || g.activePriority != ViewBuildBackground || g.yield == nil || g.yieldClosed {
		return
	}
	if g.eligibleCountLocked(g.interactive) == 0 {
		return
	}
	close(g.yield)
	g.yieldClosed = true
	g.yieldRequests++
}

func (g *ViewBuildGate) grantNextLocked() {
	g.promoteDemandedLocked()
	if g.active {
		return
	}
	// Closed latch: a family-less waiter is ineligible until Open and a family
	// waiter only once its base is published. With no ready family nothing
	// queued is eligible, so the latch holds every waiter exactly as before.
	if !g.open && len(g.familyReady) == 0 {
		return
	}
	now := time.Now()
	for {
		var waiter *viewBuildWaiter
		interactive := g.eligibleCountLocked(g.interactive)
		background := g.eligibleCountLocked(g.background)
		switch {
		case interactive > 0 && (background == 0 || g.interactiveBurst < maxInteractiveBuildBurst ||
			g.backgroundHeadWithinStarvationLocked(now)):
			// Interactive first. Queued background work overtakes only once
			// a burst of interactive grants has passed AND it has waited
			// past the starvation bound. Only eligible waiters compete; an
			// ineligible one keeps its queue place until its family is ready.
			waiter = g.takeInteractiveLocked(now)
		case background > 0:
			waiter = g.takeBackgroundEligibleLocked()
		case interactive > 0:
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
	pick := g.firstEligibleLocked(g.interactive)
	head := g.interactive[pick]
	if !head.canceled && now.Sub(head.enqueuedAt) < g.interactiveStarvation {
		newest := head.enqueuedAt.UnixNano()
		for i, waiter := range g.interactive {
			if waiter.canceled || waiter.rank == nil || !g.eligibleLocked(waiter) {
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

// eligibleLocked reports whether a queued waiter may be granted the lane: the
// global latch is open, or the waiter's family has a published committed base.
// Caller holds g.mu.
func (g *ViewBuildGate) eligibleLocked(waiter *viewBuildWaiter) bool {
	if g.open {
		return true
	}
	if waiter.family == "" {
		return false
	}
	_, ok := g.familyReady[waiter.family]
	return ok
}

// eligibleCountLocked counts the eligible waiters in a queue. An open latch
// admits every waiter, so the count is the queue length and the scheduler's
// fairness order is exactly the original. Caller holds g.mu.
func (g *ViewBuildGate) eligibleCountLocked(queue []*viewBuildWaiter) int {
	if g.open {
		return len(queue)
	}
	n := 0
	for _, waiter := range queue {
		if g.eligibleLocked(waiter) {
			n++
		}
	}
	return n
}

// firstEligibleLocked returns the index of the first eligible waiter in a
// queue (0 when the latch is open). The caller guarantees one exists. Caller
// holds g.mu.
func (g *ViewBuildGate) firstEligibleLocked(queue []*viewBuildWaiter) int {
	if g.open {
		return 0
	}
	for i, waiter := range queue {
		if g.eligibleLocked(waiter) {
			return i
		}
	}
	return 0
}

// backgroundHeadWithinStarvationLocked reports whether the oldest eligible
// background waiter has waited less than the starvation bound, so interactive
// demand still goes first. An open latch reads the queue head as before.
// Caller holds g.mu and guarantees an eligible background waiter exists.
func (g *ViewBuildGate) backgroundHeadWithinStarvationLocked(now time.Time) bool {
	head := g.background[g.firstEligibleLocked(g.background)]
	return now.Sub(head.enqueuedAt)+head.backgroundAgeCredit < g.backgroundStarvation
}

// takeBackgroundEligibleLocked removes and returns the oldest eligible
// background waiter, preserving the order of every waiter left behind.
// Caller holds g.mu and guarantees one exists.
func (g *ViewBuildGate) takeBackgroundEligibleLocked() *viewBuildWaiter {
	// Open latch: every waiter is eligible, so advance the head in constant
	// time exactly as the original scheduler did. The copy below is only for
	// the closed latch, where an ineligible head must keep its queue place.
	if g.open {
		waiter := g.background[0]
		g.background = g.background[1:]
		return waiter
	}
	i := g.firstEligibleLocked(g.background)
	waiter := g.background[i]
	copy(g.background[i:], g.background[i+1:])
	g.background[len(g.background)-1] = nil
	g.background = g.background[:len(g.background)-1]
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

// FamilyReadinessEpoch returns the family's readiness epoch. Read it BEFORE
// reading the catalog evidence that a family's primary base is published, and
// pass it to MarkFamilyReady.
func (g *ViewBuildGate) FamilyReadinessEpoch(family string) uint64 {
	if g == nil {
		return 0
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.familyEpoch[family]
}

// MarkFamilyReady records that a family's own committed base is published, so
// builds for that family become eligible for the lane even while the global
// warmup latch is closed. It wakes any deferred waiter for the family and
// grants the lane if it is idle. The mark is refused (false) when the family's
// readiness was reset after epoch was read: the evidence may describe a primary
// that has since been replaced. Idempotent; a no-op once the latch is open.
func (g *ViewBuildGate) MarkFamilyReady(family string, epoch uint64) bool {
	if g == nil || family == "" {
		return false
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.open {
		return true
	}
	if g.familyEpoch[family] != epoch || g.familyTurnover[family] > 0 {
		return false
	}
	if _, ok := g.familyReady[family]; ok {
		return true
	}
	if g.familyReady == nil {
		g.familyReady = make(map[string]struct{})
	}
	g.familyReady[family] = struct{}{}
	if ch, ok := g.familyWake[family]; ok {
		close(ch)
		delete(g.familyWake, family)
	}
	g.grantNextLocked()
	// A queued interactive waiter for this family just became eligible; if a
	// background build holds the lane, ask it to yield so the interactive work
	// runs (the yield path only fires for an eligible interactive waiter).
	g.requestYieldLocked()
	return true
}

// ResetFamilyReadiness revokes a family's early-admission bit when its primary
// graph changes: the replacement primary must publish its own committed base
// before builds for the family may bypass the global warmup latch again. Any
// coordinator already parked on the family's wake channel is woken so it
// re-checks eligibility (admissionWait re-seeds from the catalog); a coordinator
// created after the switch waits for the new primary instead of inheriting the
// old one's readiness.
func (g *ViewBuildGate) ResetFamilyReadiness(family string) {
	if g == nil || family == "" {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.familyReady, family)
	// Bump the epoch so a mark whose evidence was read before this reset is
	// refused rather than resurrecting the replaced primary's readiness.
	if g.familyEpoch == nil {
		g.familyEpoch = make(map[string]uint64)
	}
	g.familyEpoch[family]++
	// Close the wake channel before dropping it. Deleting without closing would
	// strand a pre-reset waiter: MarkFamilyReady and Open only close channels
	// still in the map, so it would recover only on its next poll. A spurious
	// wake is harmless because eligibility is re-checked on every wake.
	if ch, ok := g.familyWake[family]; ok {
		close(ch)
		delete(g.familyWake, family)
	}
	if !g.open {
		g.revokeFamilyWaitersLocked(family)
	}
}

// BeginFamilyTurnover revokes a family's readiness for a primary move and
// fences it: no mark is accepted until the matching EndFamilyTurnover, because
// the catalog may still name the incumbent while the move is uncommitted.
func (g *ViewBuildGate) BeginFamilyTurnover(family string) {
	if g == nil || family == "" {
		return
	}
	// Fence before the reset, so no seed can mark between the two.
	g.mu.Lock()
	if g.familyTurnover == nil {
		g.familyTurnover = make(map[string]int)
	}
	g.familyTurnover[family]++
	g.mu.Unlock()
	g.ResetFamilyReadiness(family)
}

// EndFamilyTurnover lifts the fence BeginFamilyTurnover set, once the move has
// committed or failed. It bumps the epoch again, so evidence read during the
// move is refused, and wakes parked waiters to re-seed from the catalog.
func (g *ViewBuildGate) EndFamilyTurnover(family string) {
	if g == nil || family == "" {
		return
	}
	g.mu.Lock()
	if g.familyTurnover[family]--; g.familyTurnover[family] <= 0 {
		delete(g.familyTurnover, family)
	}
	g.mu.Unlock()
	g.ResetFamilyReadiness(family)
}

// revokeFamilyWaitersLocked drops a family's queued AcquireAdmittedForFamily
// waiters, which fail with ErrFamilyNotAdmitted instead of parking on the
// closed latch. Caller holds g.mu.
func (g *ViewBuildGate) revokeFamilyWaitersLocked(family string) {
	revoke := func(queue []*viewBuildWaiter) []*viewBuildWaiter {
		kept := queue[:0]
		for _, waiter := range queue {
			if waiter.admittedOnly && waiter.family == family && !waiter.canceled {
				g.recordDequeuedLocked(waiter)
				g.recordCanceledLocked(waiter.priority)
				waiter.revoked = true
				close(waiter.ready)
				continue
			}
			kept = append(kept, waiter)
		}
		for i := len(kept); i < len(queue); i++ {
			queue[i] = nil
		}
		return kept
	}
	g.interactive = revoke(g.interactive)
	g.background = revoke(g.background)
}

// AdmittedForFamily reports whether a build for the family may run: the global
// latch is open, or the family's own committed base is published.
func (g *ViewBuildGate) AdmittedForFamily(family string) bool {
	if g == nil {
		return true
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.familyReadyLocked(family)
}

// FamilyOpened is the channel a deferred family waiter blocks on: it is closed
// when the global latch opens OR the family's base becomes ready. An already
// eligible family gets an already-closed channel.
func (g *ViewBuildGate) FamilyOpened(family string) <-chan struct{} {
	if g == nil {
		return admittedChannel
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.familyReadyLocked(family) {
		return admittedChannel
	}
	return g.familyWakeChanLocked(family)
}

// familyReadyLocked reports eligibility under the mutex.
func (g *ViewBuildGate) familyReadyLocked(family string) bool {
	if g.open {
		return true
	}
	if family == "" {
		return false
	}
	_, ok := g.familyReady[family]
	return ok
}

// familyWakeChanLocked returns the channel that fires when the family becomes
// eligible, creating it on first use. A family-less waiter waits on the global
// opened channel. Caller holds g.mu.
func (g *ViewBuildGate) familyWakeChanLocked(family string) <-chan struct{} {
	if family == "" {
		return g.opened
	}
	if g.familyWake == nil {
		g.familyWake = make(map[string]chan struct{})
	}
	ch, ok := g.familyWake[family]
	if !ok {
		ch = make(chan struct{})
		g.familyWake[family] = ch
	}
	return ch
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
