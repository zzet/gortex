package daemon

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

type scheduledReply struct {
	grant *mcpScheduledGrant
	err   error
}

func testDispatchPool(limit int) *mcpDispatchScheduler {
	owner := &mcpDispatchCoordinator{}
	return owner.pool(newMCPDispatchSlots(limit))
}

func takeScheduled(t *testing.T, pool *mcpDispatchScheduler, class MCPDispatchClass, session string) *mcpScheduledGrant {
	t.Helper()
	ctx := withMCPDispatchQueueWait(t.Context(), 0)
	grant, err := pool.acquire(ctx, class, session)
	if err != nil {
		t.Fatalf("immediate %s/%s: %v", class, session, err)
	}
	t.Cleanup(grant.complete)
	return grant
}

func queueScheduled(t *testing.T, pool *mcpDispatchScheduler, class MCPDispatchClass, session string) <-chan scheduledReply {
	t.Helper()
	admission := newMCPDispatchAdmission()
	ctx, cancel := context.WithCancel(withMCPDispatchAdmission(withMCPDispatchQueueWait(t.Context(), 5*time.Second), admission))
	result := make(chan scheduledReply, 1)
	done := make(chan struct{})
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Error("queued acquisition did not stop during cleanup")
			return
		}
		select {
		case reply := <-result:
			if reply.grant != nil {
				reply.grant.complete()
			}
		default:
		}
	})
	go func() {
		defer close(done)
		grant, err := pool.acquire(ctx, class, session)
		result <- scheduledReply{grant, err}
	}()

	select {
	case <-admission.done:
	case reply := <-result:
		if reply.grant != nil {
			reply.grant.complete()
		}
		t.Fatalf("request did not queue: %+v", reply)
	case <-time.After(time.Second):
		t.Fatal("request did not reach queue admission")
	}
	return result
}

func receiveScheduled(t *testing.T, result <-chan scheduledReply) *mcpScheduledGrant {
	t.Helper()
	select {
	case reply := <-result:
		if reply.err != nil || reply.grant == nil {
			t.Fatalf("queued admission: %+v", reply)
		}
		t.Cleanup(reply.grant.complete)
		return reply.grant
	case <-time.After(time.Second):
		t.Fatal("queued dispatch not admitted")
		return nil
	}
}

func notScheduled(t *testing.T, result <-chan scheduledReply) {
	t.Helper()
	select {
	case reply := <-result:
		if reply.grant != nil {
			reply.grant.complete()
		}
		t.Fatalf("unexpected admission: %+v", reply)
	default:
	}
}

func TestDispatchSchedulerSlowCapAndMutationReservation(t *testing.T) {
	pool := testDispatchPool(8)
	slow := []*mcpScheduledGrant{takeScheduled(t, pool, MCPDispatchSlow, "slow1"), takeScheduled(t, pool, MCPDispatchSlow, "slow2")}
	thirdSlow := queueScheduled(t, pool, MCPDispatchSlow, "slow3")
	for i := range 4 {
		takeScheduled(t, pool, MCPDispatchFast, fmt.Sprintf("fast%d", i))
	}
	fast := queueScheduled(t, pool, MCPDispatchFast, "extra-fast")
	takeScheduled(t, pool, MCPDispatchMutating, "edit1")
	takeScheduled(t, pool, MCPDispatchMutating, "edit2")
	pool.owner.mu.Lock()
	if pool.active[MCPDispatchSlow] != 2 || pool.active[MCPDispatchFast] != 4 || pool.active[MCPDispatchMutating] != 2 {
		t.Errorf("active lanes=%v", pool.active)
	}
	pool.owner.mu.Unlock()
	notScheduled(t, thirdSlow)
	notScheduled(t, fast)
	slow[0].complete()
	// Fast waiters have priority over the third slow call.
	receiveScheduled(t, fast).complete()
	receiveScheduled(t, thirdSlow)
}

func TestDispatchSchedulerPriorityAndSessionRoundRobin(t *testing.T) {
	t.Run("class priority", func(t *testing.T) {
		pool := testDispatchPool(8)
		var holders []*mcpScheduledGrant
		for range 8 {
			holders = append(holders, takeScheduled(t, pool, MCPDispatchMutating, "holder"))
		}
		slow := queueScheduled(t, pool, MCPDispatchSlow, "slow")
		fast := queueScheduled(t, pool, MCPDispatchFast, "fast")
		edit := queueScheduled(t, pool, MCPDispatchMutating, "edit")
		holders[0].complete()
		receiveScheduled(t, edit)
		notScheduled(t, fast)
		notScheduled(t, slow)
		holders[1].complete()
		receiveScheduled(t, fast)
		notScheduled(t, slow)
		holders[2].complete()
		receiveScheduled(t, slow)
	})
	t.Run("round robin", func(t *testing.T) {
		pool := testDispatchPool(1)
		holder := takeScheduled(t, pool, MCPDispatchMutating, "holder")
		a1 := queueScheduled(t, pool, MCPDispatchFast, "A")
		a2 := queueScheduled(t, pool, MCPDispatchFast, "A")
		a3 := queueScheduled(t, pool, MCPDispatchFast, "A")
		b1 := queueScheduled(t, pool, MCPDispatchFast, "B")
		b2 := queueScheduled(t, pool, MCPDispatchFast, "B")
		holder.complete()
		receiveScheduled(t, a1).complete()
		notScheduled(t, a2)
		receiveScheduled(t, b1).complete()
		receiveScheduled(t, a2).complete()
		notScheduled(t, a3)
		receiveScheduled(t, b2).complete()
		receiveScheduled(t, a3)
	})
}

func TestDispatchSchedulerFourHeldLimitOnlyWhileOtherSessionsWait(t *testing.T) {
	pool := testDispatchPool(8)
	for range 4 {
		takeScheduled(t, pool, MCPDispatchMutating, "A")
	}
	var holders []*mcpScheduledGrant
	for range 4 {
		holders = append(holders, takeScheduled(t, pool, MCPDispatchMutating, "holder"))
	}
	a := queueScheduled(t, pool, MCPDispatchMutating, "A")
	b := queueScheduled(t, pool, MCPDispatchMutating, "B")
	holders[0].complete()
	receiveScheduled(t, b)
	notScheduled(t, a)
	// B has no backlog now, so A may borrow beyond four again.
	holders[1].complete()
	receiveScheduled(t, a)
}

func TestDispatchSchedulerDeadlineBudgetAndSmallOverrides(t *testing.T) {
	now := time.Now()
	ctx, cancel := context.WithDeadline(t.Context(), now.Add(250*time.Millisecond))
	defer cancel()
	if got := mcpEffectiveQueueWait(ctx, 5*time.Second, now); got != 150*time.Millisecond {
		t.Errorf("deadline queue budget=%s", got)
	}
	if got := mcpEffectiveQueueWait(ctx, 50*time.Millisecond, now); got != 50*time.Millisecond {
		t.Errorf("configured queue budget=%s", got)
	}
	if got := mcpEffectiveQueueWait(ctx, 5*time.Second, now.Add(200*time.Millisecond)); got != 0 {
		t.Errorf("exhausted queue budget=%s", got)
	}
	for _, test := range []struct{ cap, reserve, slow int }{{1, 0, 1}, {2, 1, 1}, {3, 2, 1}, {8, 2, 2}, {64, 2, 2}} {
		pool := testDispatchPool(test.cap)
		if pool.reserve != test.reserve || pool.slowLimit != test.slow {
			t.Errorf("cap%d reserve/slow=%d/%d", test.cap, pool.reserve, pool.slowLimit)
		}
		takeScheduled(t, pool, MCPDispatchFast, "reader")
	}
}

func TestDispatchSchedulerDrainingBoundAcrossPools(t *testing.T) {
	owner := &mcpDispatchCoordinator{}
	a, b := owner.pool(newMCPDispatchSlots(8)), owner.pool(newMCPDispatchSlots(8))
	var old []*mcpScheduledGrant
	for range 4 {
		old = append(old, takeScheduled(t, a, MCPDispatchMutating, "old"))
	}
	for _, grant := range old {
		grant.cancel()
	}
	next := takeScheduled(t, b, MCPDispatchMutating, "next")
	next.cancel()
	owner.mu.Lock()
	if owner.draining != 4 || len(b.slots) != 1 || next.state != mcpGrantActive || !next.cancelled {
		t.Errorf("full drains=%d held=%d next=%+v", owner.draining, len(b.slots), next)
	}
	owner.mu.Unlock()
	old[0].complete()
	owner.mu.Lock()
	if owner.draining != 4 || len(b.slots) != 0 || next.state != mcpGrantDraining {
		t.Errorf("drain promotion: count=%d held=%d state=%d", owner.draining, len(b.slots), next.state)
	}
	owner.mu.Unlock()
	next.complete()
	for _, grant := range old {
		grant.complete()
	}
	owner.mu.Lock()
	if owner.draining != 0 || len(owner.blocked) != 0 {
		t.Errorf("drains leaked=%d blocked=%d", owner.draining, len(owner.blocked))
	}
	owner.mu.Unlock()
}

func TestDispatchSchedulerRetryFollowsBoundedDrainRate(t *testing.T) {
	pool := testDispatchPool(8)
	pool.owner.mu.Lock()
	defer pool.owner.mu.Unlock()
	now := time.Now()
	if got := pool.retryAfterLocked(MCPDispatchFast); got != time.Second {
		t.Errorf("initial retry=%s", got)
	}
	pool.noteDrainLocked(MCPDispatchFast, now)
	pool.noteDrainLocked(MCPDispatchFast, now.Add(20*time.Millisecond))
	if got := pool.retryAfterLocked(MCPDispatchFast); got != 100*time.Millisecond {
		t.Errorf("fast drain retry=%s", got)
	}
	lane := &pool.lanes[MCPDispatchFast]
	lane.drainInterval = 2 * time.Second
	lane.queued = 4
	if got := pool.retryAfterLocked(MCPDispatchFast); got != 5*time.Second {
		t.Errorf("slow frontier retry=%s", got)
	}
}

func TestDispatchSchedulerCannotGrantExpiredQueueBudgetAfterTimerWins(t *testing.T) {
	pool := testDispatchPool(1)
	holder := takeScheduled(t, pool, MCPDispatchMutating, "holder")
	admission := newMCPDispatchAdmission()
	ctx, cancel := context.WithCancel(withMCPDispatchAdmission(withMCPDispatchQueueWait(t.Context(), 20*time.Millisecond), admission))
	defer cancel()
	result := make(chan scheduledReply, 1)
	go func() {
		grant, err := pool.acquire(ctx, MCPDispatchFast, "reader")
		result <- scheduledReply{grant, err}
	}()
	select {
	case <-admission.done:
	case <-time.After(time.Second):
		t.Fatal("reader did not queue")
	}
	func() {
		pool.owner.mu.Lock()
		defer pool.owner.mu.Unlock()
		lane := &pool.lanes[MCPDispatchFast]
		if len(lane.sessions) != 1 {
			t.Fatalf("queued sessions=%d", len(lane.sessions))
		}
		expires := lane.sessions[0].requests[0].expiresAt
		// Hold the admission mutex across the actual queue deadline. This lets
		// the acquisition timer fire but prevents its caller from removing the
		// waiter, reproducing a late capacity-release race without a sleep oracle.
		timer := time.NewTimer(max(0, time.Until(expires)))
		defer timer.Stop()
		<-timer.C
		holder.completeLocked(time.Now())
	}()
	select {
	case reply := <-result:
		if reply.grant != nil {
			reply.grant.complete()
			t.Fatal("expired queued request acquired late capacity")
		}
		if !errors.Is(reply.err, errMCPDispatchSaturated) {
			t.Fatalf("expired queue error=%v", reply.err)
		}
	case <-time.After(time.Second):
		t.Fatal("expired queue did not terminate")
	}
	if len(pool.slots) != 0 {
		t.Fatalf("expired queue leaked %d capacity tokens", len(pool.slots))
	}
}
