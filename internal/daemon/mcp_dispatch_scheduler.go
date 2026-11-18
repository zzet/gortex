package daemon

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/zzet/gortex/internal/viewmetrics"
)

const (
	mcpDispatchSlowLimit       = 2
	mcpDispatchMutationReserve = 2
	mcpDispatchSessionLimit    = 4
	mcpDispatchDrainLimit      = 4
	mcpDispatchDeadlineReserve = 100 * time.Millisecond
)

// One short mutex owns admission and lease accounting, including the process
// drain bound across pools. Production has one pool; isolated pools retain the
// existing channel injection seam. No handler or I/O runs under this mutex.
type mcpDispatchCoordinator struct {
	mu       sync.Mutex
	pools    map[chan struct{}]*mcpDispatchScheduler
	draining int
	blocked  []*mcpScheduledGrant
}

var mcpDispatchProcess = mcpDispatchCoordinator{pools: make(map[chan struct{}]*mcpDispatchScheduler)}

type mcpDispatchScheduler struct {
	owner              *mcpDispatchCoordinator
	slots              chan struct{}
	reserve, slowLimit int
	active             [3]int
	draining           [3]int
	nonMutating        int
	reservedAdmissions uint64
	protocolWaiting    int
	held               map[string]int
	lanes              [3]mcpDispatchLane
}

type mcpDispatchLane struct {
	sessions      []*mcpDispatchSessionQueue
	next          int
	queued        int
	lastDrain     time.Time
	drainInterval time.Duration
	wait          [3]mcpDispatchWaitStats
	refusals      uint64
}

type mcpDispatchSessionQueue struct {
	session  string
	requests []*mcpDispatchWaiter
}

type mcpDispatchWaiter struct {
	class     MCPDispatchClass
	session   string
	queuedAt  time.Time
	expiresAt time.Time
	ctx       context.Context
	err       error
	done      chan struct{}
	grant     *mcpScheduledGrant
	observed  bool
}

type mcpGrantState uint8

const (
	mcpGrantActive mcpGrantState = iota
	mcpGrantDraining
	mcpGrantReleased
)

type mcpScheduledGrant struct {
	pool      *mcpDispatchScheduler
	class     MCPDispatchClass
	session   string
	state     mcpGrantState
	protocol  bool
	cancelled bool
	// A middleware firewall can return before the handler it abandoned.
	// Keep the physical completion charge until both have returned.
	retainedHandlers int
	dispatchReturned bool
}

func mcpSchedulerFor(slots chan struct{}) *mcpDispatchScheduler {
	return mcpDispatchProcess.pool(slots)
}

func (c *mcpDispatchCoordinator) pool(slots chan struct{}) *mcpDispatchScheduler {
	c.mu.Lock()
	defer c.mu.Unlock()
	if pool := c.pools[slots]; pool != nil {
		return pool
	}
	limit := cap(slots)
	reserve := min(mcpDispatchMutationReserve, max(0, limit-1))
	pool := &mcpDispatchScheduler{owner: c, slots: slots, reserve: reserve, slowLimit: min(mcpDispatchSlowLimit, limit-reserve), held: make(map[string]int)}
	if c.pools == nil {
		c.pools = make(map[chan struct{}]*mcpDispatchScheduler)
	}
	c.pools[slots] = pool
	return pool
}

func mcpEffectiveQueueWait(ctx context.Context, configured time.Duration, now time.Time) time.Duration {
	if deadline, ok := ctx.Deadline(); ok {
		return max(0, min(configured, deadline.Sub(now)-mcpDispatchDeadlineReserve))
	}
	return max(0, configured)
}

func (s *mcpDispatchScheduler) otherSessionWaitingLocked(session string) bool {
	for i := range s.lanes {
		queues := s.lanes[i].sessions
		if len(queues) > 1 || len(queues) == 1 && queues[0].session != session {
			return true
		}
	}
	return false
}

func (s *mcpDispatchScheduler) eligibleLocked(class MCPDispatchClass, session string) bool {
	if len(s.slots) >= cap(s.slots) {
		return false
	}
	if class != MCPDispatchMutating && s.nonMutating >= cap(s.slots)-s.reserve {
		return false
	}
	if class == MCPDispatchSlow && s.active[class] >= s.slowLimit {
		return false
	}
	return s.held[session] < mcpDispatchSessionLimit || !s.otherSessionWaitingLocked(session)
}

func (s *mcpDispatchScheduler) grantLocked(class MCPDispatchClass, session string) *mcpScheduledGrant {
	select {
	case s.slots <- struct{}{}:
	default:
		return nil // Protocol admission or an injected holder took capacity.
	}
	s.active[class]++
	viewmetrics.AddGauge(viewmetrics.MCPDispatchInFlight, 1, class.String())
	if class == MCPDispatchMutating && s.active[class] <= s.reserve {
		s.reservedAdmissions++
		viewmetrics.Count(viewmetrics.MCPDispatchReservedAdmissionsTotal)
		viewmetrics.AddGauge(viewmetrics.MCPDispatchReservedSlotsUsed, 1)
	}
	if class != MCPDispatchMutating {
		s.nonMutating++
	}
	s.held[session]++
	return &mcpScheduledGrant{pool: s, class: class, session: session}
}

// scheduleLocked honors class priority and rotates whole sessions within each
// class. A temporarily ineligible session cannot block another class/session.
func (s *mcpDispatchScheduler) scheduleLocked() {
	for {
		admitted := false
		for _, class := range [...]MCPDispatchClass{MCPDispatchMutating, MCPDispatchFast, MCPDispatchSlow} {
			lane := &s.lanes[class]
			for offset := 0; offset < len(lane.sessions); offset++ {
				index := (lane.next + offset) % len(lane.sessions)
				q := lane.sessions[index]
				waiter := q.requests[0]
				now := time.Now()
				if waiter.ctx != nil && waiter.ctx.Err() != nil {
					waiter = s.popLocked(class, index)
					s.observeQueuedWaitLocked(waiter, mcpWaitCancelled, now)
					waiter.err = fmt.Errorf("%w: %w", errMCPDispatchNotStarted, waiter.ctx.Err())
					close(waiter.done)
					admitted = true
					break
				}
				if !waiter.expiresAt.IsZero() && !now.Before(waiter.expiresAt) {
					waiter = s.popLocked(class, index)
					s.observeQueuedWaitLocked(waiter, mcpWaitRefused, now)
					waiter.err = &mcpDispatchBusyError{waited: now.Sub(waiter.queuedAt), retryAfter: s.retryAfterLocked(class), lane: class}
					close(waiter.done)
					admitted = true
					break
				}
				if !s.eligibleLocked(class, q.session) {
					continue
				}
				grant := s.grantLocked(class, q.session)
				if grant == nil {
					return
				}
				waiter = s.popLocked(class, index)
				s.observeQueuedWaitLocked(waiter, mcpWaitAdmitted, now)
				waiter.grant = grant
				close(waiter.done)
				admitted = true
				break
			}
			if admitted {
				break
			}
		}
		if !admitted {
			return
		}
	}
}

// popLocked advances the round-robin cursor for both grants and terminal
// queued outcomes. Empty session queues never remain in the ring.
func (s *mcpDispatchScheduler) popLocked(class MCPDispatchClass, index int) *mcpDispatchWaiter {
	lane := &s.lanes[class]
	q := lane.sessions[index]
	waiter := q.requests[0]
	q.requests[0] = nil
	q.requests = q.requests[1:]
	lane.queued--
	viewmetrics.AddGauge(viewmetrics.MCPDispatchQueueDepth, -1, class.String())
	if len(q.requests) == 0 {
		copy(lane.sessions[index:], lane.sessions[index+1:])
		lane.sessions[len(lane.sessions)-1] = nil
		lane.sessions = lane.sessions[:len(lane.sessions)-1]
		lane.next = index
	} else {
		lane.next = index + 1
	}
	if len(lane.sessions) > 0 {
		lane.next %= len(lane.sessions)
	} else {
		lane.next = 0
	}
	return waiter
}

func (s *mcpDispatchScheduler) enqueueLocked(waiter *mcpDispatchWaiter) {
	viewmetrics.AddGauge(viewmetrics.MCPDispatchQueueDepth, 1, waiter.class.String())
	lane := &s.lanes[waiter.class]
	for _, q := range lane.sessions {
		if q.session == waiter.session {
			q.requests = append(q.requests, waiter)
			lane.queued++
			return
		}
	}
	lane.sessions = append(lane.sessions, &mcpDispatchSessionQueue{session: waiter.session, requests: []*mcpDispatchWaiter{waiter}})
	lane.queued++
}

func (s *mcpDispatchScheduler) removeLocked(waiter *mcpDispatchWaiter) {
	lane := &s.lanes[waiter.class]
	for index, q := range lane.sessions {
		if q.session != waiter.session {
			continue
		}
		for at, request := range q.requests {
			if request != waiter {
				continue
			}
			copy(q.requests[at:], q.requests[at+1:])
			q.requests[len(q.requests)-1] = nil
			q.requests = q.requests[:len(q.requests)-1]
			lane.queued--
			viewmetrics.AddGauge(viewmetrics.MCPDispatchQueueDepth, -1, waiter.class.String())
			if len(q.requests) == 0 {
				copy(lane.sessions[index:], lane.sessions[index+1:])
				lane.sessions[len(lane.sessions)-1] = nil
				lane.sessions = lane.sessions[:len(lane.sessions)-1]
				if index < lane.next {
					lane.next--
				}
				if len(lane.sessions) > 0 {
					lane.next %= len(lane.sessions)
				} else {
					lane.next = 0
				}
			}
			return
		}
	}
}

func (s *mcpDispatchScheduler) retryAfterLocked(class MCPDispatchClass) time.Duration {
	lane := &s.lanes[class]
	estimate := mcpDispatchBusyRetryAfter
	if lane.drainInterval > 0 {
		// A drain releases one admission opportunity; include the waiting frontier
		// without allowing multiplication to overflow the bounded estimate.
		ahead := time.Duration(max(1, lane.queued+1))
		estimate = min(5*time.Second, lane.drainInterval) * ahead
	}
	return max(100*time.Millisecond, min(5*time.Second, estimate))
}

func (s *mcpDispatchScheduler) acquire(ctx context.Context, class MCPDispatchClass, session string) (*mcpScheduledGrant, error) {
	if class != MCPDispatchFast && class != MCPDispatchMutating && class != MCPDispatchSlow {
		class = MCPDispatchFast
	}
	now := time.Now()
	s.owner.mu.Lock()
	if err := ctx.Err(); err != nil {
		s.observeWaitLocked(class, mcpWaitCancelled, 0)
		s.owner.mu.Unlock()
		signalMCPDispatchAdmission(ctx)
		return nil, fmt.Errorf("%w: %w", errMCPDispatchNotStarted, err)
	}
	hasQueue := false
	for i := range s.lanes {
		hasQueue = hasQueue || s.lanes[i].queued > 0
	}
	if !hasQueue && s.eligibleLocked(class, session) {
		if grant := s.grantLocked(class, session); grant != nil {
			s.observeWaitLocked(class, mcpWaitAdmitted, time.Since(now))
			s.owner.mu.Unlock()
			return grant, nil
		}
	}
	wait := mcpEffectiveQueueWait(ctx, mcpDispatchQueueWaitForContext(ctx), now)
	if s.lanes[class].queued >= mcpDispatchQueueFactor*cap(s.slots) {
		s.observeWaitLocked(class, mcpWaitRefused, 0)
		retry := s.retryAfterLocked(class)
		s.owner.mu.Unlock()
		signalMCPDispatchAdmission(ctx)
		return nil, &mcpDispatchBusyError{retryAfter: retry, lane: class}
	}
	waiter := &mcpDispatchWaiter{class: class, session: session, queuedAt: now, done: make(chan struct{})}
	waiter.ctx = ctx
	if wait > 0 {
		waiter.expiresAt = now.Add(wait)
	}
	s.enqueueLocked(waiter)
	s.scheduleLocked()
	// Even with queuing off, a request may use capacity unavailable to the
	// other queued classes (notably the mutation reservation).
	if waiter.grant != nil {
		s.owner.mu.Unlock()
		return waiter.grant, nil
	}
	if waiter.err != nil {
		s.owner.mu.Unlock()
		signalMCPDispatchAdmission(ctx)
		return nil, waiter.err
	}
	if wait <= 0 {
		s.removeLocked(waiter)
		s.observeQueuedWaitLocked(waiter, mcpWaitRefused, now)
		retry := s.retryAfterLocked(class)
		s.scheduleLocked()
		s.owner.mu.Unlock()
		signalMCPDispatchAdmission(ctx)
		return nil, &mcpDispatchBusyError{retryAfter: retry, lane: class}
	}
	s.owner.mu.Unlock()
	signalMCPDispatchAdmission(ctx)
	timer := time.NewTimer(max(0, time.Until(now.Add(wait))))
	defer timer.Stop()
	select {
	case <-waiter.done:
	case <-timer.C:
	case <-ctx.Done():
	}
	s.owner.mu.Lock()
	if err := ctx.Err(); err != nil {
		if waiter.grant != nil {
			waiter.grant.completeLocked(time.Now())
		} else {
			s.removeLocked(waiter)
			s.observeQueuedWaitLocked(waiter, mcpWaitCancelled, time.Now())
			s.scheduleLocked()
		}
		s.owner.mu.Unlock()
		return nil, fmt.Errorf("%w: %w", errMCPDispatchNotStarted, err)
	}
	if waiter.grant != nil {
		s.owner.mu.Unlock()
		return waiter.grant, nil
	}
	if waiter.err != nil {
		s.owner.mu.Unlock()
		return nil, waiter.err
	}
	s.removeLocked(waiter)
	s.observeQueuedWaitLocked(waiter, mcpWaitRefused, time.Now())
	retry := s.retryAfterLocked(class)
	s.scheduleLocked()
	s.owner.mu.Unlock()
	return nil, &mcpDispatchBusyError{waited: time.Since(now), retryAfter: retry, lane: class}
}

func (s *mcpDispatchScheduler) noteDrainLocked(class MCPDispatchClass, now time.Time) {
	lane := &s.lanes[class]
	if !lane.lastDrain.IsZero() {
		interval := max(time.Millisecond, now.Sub(lane.lastDrain))
		if lane.drainInterval == 0 {
			lane.drainInterval = interval
		} else {
			lane.drainInterval = (lane.drainInterval*7 + interval) / 8
		}
	}
	lane.lastDrain = now
}

func (g *mcpScheduledGrant) dropActiveLocked(now time.Time) {
	s := g.pool
	select {
	case <-s.slots:
	default:
		panic("MCP dispatch grant lost its capacity token")
	}
	s.active[g.class]--
	viewmetrics.AddGauge(viewmetrics.MCPDispatchInFlight, -1, g.class.String())
	if !g.protocol && g.class == MCPDispatchMutating && s.active[g.class] < s.reserve {
		viewmetrics.AddGauge(viewmetrics.MCPDispatchReservedSlotsUsed, -1)
	}
	if !g.protocol {
		if g.class != MCPDispatchMutating {
			s.nonMutating--
		}
		s.held[g.session]--
		if s.held[g.session] == 0 {
			delete(s.held, g.session)
		}
	}
	s.noteDrainLocked(g.class, now)
}

func (c *mcpDispatchCoordinator) promoteCancelledLocked(now time.Time) {
	for c.draining < mcpDispatchDrainLimit && len(c.blocked) > 0 {
		grant := c.blocked[0]
		c.blocked[0] = nil
		c.blocked = c.blocked[1:]
		if grant.state != mcpGrantActive || !grant.cancelled {
			continue
		}
		grant.dropActiveLocked(now)
		grant.state = mcpGrantDraining
		grant.pool.draining[grant.class]++
		viewmetrics.AddGauge(viewmetrics.MCPDispatchCancelledHeld, -1, grant.class.String())
		viewmetrics.AddGauge(viewmetrics.MCPDispatchDraining, 1, grant.class.String())
		c.draining++
		grant.pool.scheduleLocked()
	}
}

func (g *mcpScheduledGrant) completeLocked(now time.Time) {
	if g.state == mcpGrantReleased {
		return
	}
	c := g.pool.owner
	if g.state == mcpGrantActive {
		g.dropActiveLocked(now)
	} else {
		c.draining--
		g.pool.draining[g.class]--
		viewmetrics.AddGauge(viewmetrics.MCPDispatchDraining, -1, g.class.String())
	}
	g.state = mcpGrantReleased
	// Remove a completed capacity-charged cancellation before promotion.
	for i, grant := range c.blocked {
		if grant == g {
			viewmetrics.AddGauge(viewmetrics.MCPDispatchCancelledHeld, -1, g.class.String())
			copy(c.blocked[i:], c.blocked[i+1:])
			c.blocked[len(c.blocked)-1] = nil
			c.blocked = c.blocked[:len(c.blocked)-1]
			break
		}
	}
	c.promoteCancelledLocked(now)
	g.pool.scheduleLocked()
}

func (g *mcpScheduledGrant) complete() {
	g.pool.owner.mu.Lock()
	defer g.pool.owner.mu.Unlock()
	g.dispatchReturned = true
	if g.retainedHandlers == 0 {
		g.completeLocked(time.Now())
	}
}

func (g *mcpScheduledGrant) retainCompletion() func() {
	c := g.pool.owner
	c.mu.Lock()
	if g.state == mcpGrantReleased {
		c.mu.Unlock()
		return nil
	}
	g.retainedHandlers++
	c.mu.Unlock()
	// The middleware stopped waiting, just like transport cancellation.
	// Logical capacity returns, while unfinished work stays drain-bounded.
	g.cancel()
	return sync.OnceFunc(func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		g.retainedHandlers--
		if g.retainedHandlers == 0 && g.dispatchReturned {
			g.completeLocked(time.Now())
		}
	})
}

// cancel detaches at most four physically unfinished handlers process-wide.
// Once that allowance is full, canceled handlers stay capacity-charged until
// one drain completes or they return themselves. Thus live handler work is
// bounded by the configured cap plus four, never by the cancellation rate.
func (g *mcpScheduledGrant) cancel() {
	c := g.pool.owner
	c.mu.Lock()
	defer c.mu.Unlock()
	if g.state != mcpGrantActive || g.cancelled {
		return
	}
	g.cancelled = true
	if c.draining >= mcpDispatchDrainLimit {
		c.blocked = append(c.blocked, g)
		viewmetrics.AddGauge(viewmetrics.MCPDispatchCancelledHeld, 1, g.class.String())
		return
	}
	g.dropActiveLocked(time.Now())
	g.state = mcpGrantDraining
	c.draining++
	g.pool.draining[g.class]++
	viewmetrics.AddGauge(viewmetrics.MCPDispatchDraining, 1, g.class.String())
	g.pool.scheduleLocked()
}

func (g *mcpScheduledGrant) releaseEarly() bool {
	c := g.pool.owner
	c.mu.Lock()
	defer c.mu.Unlock()
	if g.state != mcpGrantActive || g.cancelled {
		return false
	}
	g.completeLocked(time.Now())
	return true
}

// Protocol initialization/notifications preserve their historical full-cap,
// unbudgeted wait. They are exempt from tool reservations/session limits, but
// their completion still wakes queued tools and shares the same physical cap.
func (s *mcpDispatchScheduler) acquireProtocol(ctx context.Context) (*mcpScheduledGrant, error) {
	now := time.Now()
	s.owner.mu.Lock()
	s.protocolWaiting++
	s.owner.mu.Unlock()
	select {
	case s.slots <- struct{}{}:
		s.owner.mu.Lock()
		s.protocolWaiting--
		s.active[MCPDispatchFast]++
		viewmetrics.AddGauge(viewmetrics.MCPDispatchInFlight, 1, MCPDispatchFast.String())
		s.observeWaitLocked(MCPDispatchFast, mcpWaitAdmitted, time.Since(now))
		grant := &mcpScheduledGrant{pool: s, class: MCPDispatchFast, state: mcpGrantActive, protocol: true}
		s.owner.mu.Unlock()
		return grant, nil
	case <-ctx.Done():
		s.owner.mu.Lock()
		s.protocolWaiting--
		s.observeWaitLocked(MCPDispatchFast, mcpWaitCancelled, time.Since(now))
		s.owner.mu.Unlock()
		signalMCPDispatchAdmission(ctx)
		return nil, fmt.Errorf("%w: %w", errMCPDispatchNotStarted, ctx.Err())
	}
}
