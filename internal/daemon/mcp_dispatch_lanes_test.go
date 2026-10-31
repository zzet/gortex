package daemon

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

func scheduledContext(t *testing.T, slots chan struct{}, class MCPDispatchClass, session string) context.Context {
	t.Helper()
	return withMCPDispatchSlotLease(withMCPDispatchRequest(withMCPDispatchSlots(t.Context(), slots), mcpDispatchRequestInfo{class: class, session: session}))
}

func TestDispatchProtocolCompletionWakesToolLane(t *testing.T) {
	slots := newMCPDispatchSlots(1)
	ctx := withMCPDispatchRequest(withMCPDispatchSlots(t.Context(), slots), mcpDispatchRequestInfo{protocol: true})
	started, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	unblock := sync.OnceFunc(func() { close(release) })
	t.Cleanup(unblock)
	go func() {
		defer close(done)
		_, _ = dispatchMCPWithContext(ctx, func() ([]byte, error) { close(started); <-release; return nil, nil })
	}()
	awaitMCPTestSignal(t, started, "protocol handler not admitted")
	queued := queueScheduled(t, mcpSchedulerFor(slots), MCPDispatchFast, "reader")
	unblock()
	receiveScheduled(t, queued).complete()
	awaitMCPTestSignal(t, done, "protocol dispatch did not finish")
}

func TestDispatchCancellationReturnsCapacityWithin100ms(t *testing.T) {
	slots := newMCPDispatchSlots(1)
	ctx, cancel := context.WithCancel(scheduledContext(t, slots, MCPDispatchFast, "reader"))
	defer cancel()
	started, done := make(chan struct{}), make(chan error, 1)
	go func() {
		_, err := dispatchMCPWithContext(ctx, func() ([]byte, error) { close(started); <-ctx.Done(); return nil, ctx.Err() })
		done <- err
	}()
	awaitMCPTestSignal(t, started, "context-aware handler not admitted")
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancel error=%v", err)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("cancellation did not return within100ms")
	}
	if len(slots) != 0 {
		t.Fatal("canceled context-aware handler still occupies dispatch capacity")
	}
}

func TestDispatchUncooperativeCancellationCannotExceedFourDrains(t *testing.T) {
	slots := newMCPDispatchSlots(1)
	pool := mcpSchedulerFor(slots)
	release := make(chan struct{})
	unblock := sync.OnceFunc(func() { close(release) })
	var bodies sync.WaitGroup
	t.Cleanup(func() { unblock(); bodies.Wait() })
	for i := range mcpDispatchDrainLimit + 1 {
		ctx, cancel := context.WithCancel(scheduledContext(t, slots, MCPDispatchMutating, fmt.Sprintf("session%d", i)))
		started, done := make(chan struct{}), make(chan error, 1)
		bodies.Add(1)
		bodyDone := sync.OnceFunc(bodies.Done)
		go func() {
			_, err := dispatchMCPWithContext(ctx, func() ([]byte, error) { defer bodyDone(); close(started); <-release; return nil, nil })
			if mcpDispatchNotStarted(err) {
				bodyDone()
			}
			done <- err
		}()
		awaitMCPTestSignal(t, started, "uncooperative handler did not start")
		cancel()
		select {
		case err := <-done:
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("cancel error=%v", err)
			}
		case <-time.After(100 * time.Millisecond):
			t.Fatal("canceled caller still waiting")
		}
	}
	pool.owner.mu.Lock()
	if pool.owner.draining != 4 || pool.draining[MCPDispatchMutating] != 4 || pool.active[MCPDispatchMutating] != 1 || len(slots) != 1 {
		t.Errorf("draining=%d pooldrain=%v active=%v held=%d", pool.owner.draining, pool.draining, pool.active, len(slots))
	}
	pool.owner.mu.Unlock()
	ctx := withMCPDispatchQueueWait(scheduledContext(t, slots, MCPDispatchMutating, "overflow"), 0)
	ran := false
	_, err := dispatchMCPWithContext(ctx, func() ([]byte, error) { ran = true; return nil, nil })
	if ran || !errors.Is(err, errMCPDispatchSaturated) {
		t.Fatalf("full draining allowance ran=%v error=%v", ran, err)
	}
	unblock()
}

func TestDispatchEditWaitMovesToSlowAfterReturningMutationCapacity(t *testing.T) {
	slots := newMCPDispatchSlots(3)
	pool := mcpSchedulerFor(slots)
	slow := takeScheduled(t, pool, MCPDispatchSlow, "outline")
	ctx := scheduledContext(t, slots, MCPDispatchMutating, "editor")
	entered, slowStarted, finish := make(chan struct{}), make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	unblock := sync.OnceFunc(func() { close(finish) })
	t.Cleanup(unblock)
	go func() {
		_, err := dispatchMCPWithContext(ctx, func() ([]byte, error) {
			close(entered)
			if err := MoveMCPDispatchToSlowLane(ctx, nil); err != nil {
				return nil, err
			}
			if err := MoveMCPDispatchToSlowLane(ctx, nil); err != nil {
				return nil, err
			} // idempotent
			close(slowStarted)
			<-finish
			return []byte(`{"ok":true}`), nil
		})
		done <- err
	}()
	awaitMCPTestSignal(t, entered, "mutation handler did not start")
	awaitMCPDispatchQueueWaiters(t, slots, 1)
	pool.owner.mu.Lock()
	if pool.active[MCPDispatchMutating] != 0 {
		t.Errorf("mutation slot retained during slow admission: %v", pool.active)
	}
	pool.owner.mu.Unlock()
	// Both reserved mutation slots can be used while the old edit waits.
	edit1 := takeScheduled(t, pool, MCPDispatchMutating, "next1")
	edit2 := takeScheduled(t, pool, MCPDispatchMutating, "next2")
	edit1.complete()
	edit2.complete()
	slow.complete()
	awaitMCPTestSignal(t, slowStarted, "post-commit wait did not enter slow lane")
	pool.owner.mu.Lock()
	if pool.active[MCPDispatchSlow] != 1 || pool.active[MCPDispatchMutating] != 0 {
		t.Errorf("edit wait lane=%v", pool.active)
	}
	pool.owner.mu.Unlock()
	unblock()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("slow edit wait did not complete")
	}
	if len(slots) != 0 {
		t.Fatalf("slow wait leaked %d tokens", len(slots))
	}
}

func TestDispatchEditWaitNeedsNoSlowSlotOncePublished(t *testing.T) {
	for _, when := range []string{"while_queued", "before_moving"} {
		t.Run(when, func(t *testing.T) {
			slots := newMCPDispatchSlots(8)
			pool := mcpSchedulerFor(slots)
			takeScheduled(t, pool, MCPDispatchSlow, "outline")
			takeScheduled(t, pool, MCPDispatchSlow, "dead_code")
			ctx := scheduledContext(t, slots, MCPDispatchMutating, "editor")
			published := make(chan struct{})
			if when == "before_moving" {
				close(published)
			}
			moved, finish, done := make(chan error, 1), make(chan struct{}), make(chan error, 1)
			unblock := sync.OnceFunc(func() { close(finish) })
			t.Cleanup(unblock)
			go func() {
				_, err := dispatchMCPWithContext(ctx, func() ([]byte, error) {
					moved <- MoveMCPDispatchToSlowLane(ctx, published)
					<-finish
					return []byte(`{"ok":true}`), nil
				})
				done <- err
			}()
			if when == "while_queued" {
				awaitMCPDispatchQueueWaiters(t, slots, 1)
				close(published)
			}
			// The queue budget is seconds; a finished publication must not spend it.
			select {
			case err := <-moved:
				if err != nil {
					t.Fatalf("published edit wait: %v", err)
				}
			case <-time.After(time.Second):
				t.Fatal("published edit wait stayed queued behind the slow lane")
			}
			pool.owner.mu.Lock()
			mutating := 0
			if when == "before_moving" {
				mutating = 1 // nothing left to wait for, so nothing to move
			}
			if pool.active[MCPDispatchSlow] != 2 || pool.active[MCPDispatchMutating] != mutating || pool.lanes[MCPDispatchSlow].queued != 0 {
				t.Errorf("published edit wait lanes: active=%v slow queued=%d", pool.active, pool.lanes[MCPDispatchSlow].queued)
			}
			pool.owner.mu.Unlock()
			unblock()
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(time.Second):
				t.Fatal("edit dispatch did not complete")
			}
			if len(slots) != 2 {
				t.Fatalf("published edit wait left %d tokens, want the two slow holders", len(slots))
			}
		})
	}
}
