package daemon

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestMCPDispatchTimingActualGrantAndLeaseReplacement(t *testing.T) {
	ctx := withMCPDispatchSlots(withMCPDispatchSlotLease(t.Context()), newMCPDispatchSlots(1))
	var inside MCPDispatchTiming
	_, err := dispatchMCPWithContext(ctx, func() ([]byte, error) {
		var ok bool
		inside, ok = MCPDispatchTimingForContext(ctx)
		if !ok {
			return nil, context.Canceled // actual handler must see the completed grant
		}
		return []byte("done"), nil
	})
	require.NoError(t, err)
	require.False(t, inside.Granted.Before(inside.Entered))
	lease := mcpDispatchSlotLeaseForContext(ctx)
	first := lease.attach(func() {})
	second := lease.attach(func() {})
	lease.detach(first) // stale detach must not replace the observation either
	require.True(t, ReleaseMCPDispatchSlot(ctx))
	lease.detach(second)
	_, err = dispatchMCPWithContext(ctx, func() ([]byte, error) { return nil, nil })
	require.NoError(t, err)
	after, ok := MCPDispatchTimingForContext(ctx)
	require.True(t, ok)
	require.Equal(t, inside, after, "later admission/release/attach cannot become the original grant")
	after.Entered = time.Time{}
	copyValue, _ := MCPDispatchTimingForContext(ctx)
	require.Equal(t, inside, copyValue)
}

func TestMCPDispatchTimingQueuedGrantIsNotEarlyAdmissionSignal(t *testing.T) {
	parent, cancel := context.WithCancel(t.Context())
	slots := newMCPDispatchSlots(1)
	slots <- struct{}{}
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { <-slots }) }
	admission := newMCPDispatchAdmission()
	ctx := withMCPDispatchQueueWait(withMCPDispatchSlots(withMCPDispatchSlotLease(withMCPDispatchAdmission(parent, admission)), slots), time.Minute)
	done := make(chan error, 1)
	finished := make(chan struct{})
	t.Cleanup(func() {
		release()
		cancel()
		select {
		case <-finished:
		case <-time.After(5 * time.Second):
			t.Error("owned queued dispatcher did not finish")
		}
	})
	go func() {
		defer close(finished)
		_, err := dispatchMCPWithContext(ctx, func() ([]byte, error) { return []byte("done"), nil })
		done <- err
	}()
	select {
	case <-admission.done:
	case <-time.After(5 * time.Second):
		t.Fatal("queue did not release the connection reader")
	}
	queued, available := MCPDispatchTimingForContext(ctx)
	require.False(t, available, "admission.done is not the actual channel grant")
	require.False(t, queued.Entered.IsZero())
	// The held slot proves queue ordering; a real later clock reading also
	// proves the strict measured interval on coarse Windows clocks.
	require.Eventually(t, func() bool { return time.Now().After(queued.Entered) }, 5*time.Second, time.Millisecond,
		"monotonic clock did not advance while dispatch capacity was held")
	release()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("queued handler did not finish after capacity release")
	}
	timing, available := MCPDispatchTimingForContext(ctx)
	require.True(t, available)
	require.True(t, timing.Granted.After(timing.Entered))
}

func TestMCPDispatchTimingRefusalDoesNotBorrowLaterAttempt(t *testing.T) {
	slots := newMCPDispatchSlots(1)
	slots <- struct{}{}
	ctx := withMCPDispatchQueueWait(withMCPDispatchSlots(withMCPDispatchSlotLease(t.Context()), slots), 0)
	_, err := dispatchMCPWithContext(ctx, func() ([]byte, error) {
		t.Error("saturated handler must not run")
		return nil, nil
	})
	require.ErrorIs(t, err, errMCPDispatchSaturated)
	_, ok := MCPDispatchTimingForContext(ctx)
	require.False(t, ok)
	<-slots
	_, err = dispatchMCPWithContext(ctx, func() ([]byte, error) { return nil, nil })
	require.NoError(t, err)
	_, ok = MCPDispatchTimingForContext(ctx)
	require.False(t, ok, "a later grant cannot complete a refused attempt's timeline")

	slots <- struct{}{}
	parent, cancel := context.WithCancel(t.Context())
	cancel()
	cancelled := withMCPDispatchSlots(withMCPDispatchSlotLease(parent), slots)
	_, err = dispatchMCPWithContext(cancelled, func() ([]byte, error) {
		t.Error("cancelled queued handler must not run")
		return nil, nil
	})
	require.ErrorIs(t, err, context.Canceled)
	_, ok = MCPDispatchTimingForContext(cancelled)
	require.False(t, ok, "a cancelled queue has no invented grant")
	<-slots
}

func TestMCPDispatchTimingReplayAndUnboundedRemainUnavailable(t *testing.T) {
	sess := &Session{LogicalSessionID: "timing-session"}
	request := []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"write_file"}}`)
	first := withMCPDispatchSlots(withMCPDispatchSlotLease(t.Context()), newMCPDispatchSlots(1))
	_, replayed, err := sess.dispatchMCPOnceContext(first, request, func() ([]byte, error) {
		return []byte(`{"jsonrpc":"2.0","id":1,"result":{}}`), nil
	})
	require.NoError(t, err)
	require.False(t, replayed)
	_, ok := MCPDispatchTimingForContext(first)
	require.True(t, ok)
	replay := withMCPDispatchSlotLease(t.Context())
	_, replayed, err = sess.dispatchMCPOnceContext(replay, request, func() ([]byte, error) {
		t.Error("cached response must not execute")
		return nil, nil
	})
	require.NoError(t, err)
	require.True(t, replayed)
	_, ok = MCPDispatchTimingForContext(replay)
	require.False(t, ok)
	unbounded := withMCPDispatchUnbounded(withMCPDispatchSlotLease(t.Context()))
	_, err = dispatchMCPWithContext(unbounded, func() ([]byte, error) { return nil, nil })
	require.NoError(t, err)
	_, ok = MCPDispatchTimingForContext(unbounded)
	require.False(t, ok)
	_, ok = MCPDispatchTimingForContext(withMCPDispatchUnbounded(first))
	require.False(t, ok, "an unbounded descendant cannot advertise its parent's bounded grant")
}

func TestMCPDispatchTimingNilPrimitiveDoesNotAllocate(t *testing.T) {
	var lease *mcpDispatchSlotLease
	ctx := t.Context()
	allocations := testing.AllocsPerRun(100, func() {
		lease.beginTiming().granted()
		_, _ = MCPDispatchTimingForContext(ctx)
	})
	require.Zero(t, allocations)
	_, ok := MCPDispatchTimingForContext(nil) //nolint:staticcheck // Intentional nil-context abstention control.
	require.False(t, ok)
}

// Primitive lower bound only: this isolates the two clock samples and existing
// lease mutexes, not JSON dispatch, queueing, syscall or end-to-end read cost.
func BenchmarkMCPDispatchTimingLease(b *testing.B) {
	b.Run("nil-lease", func(b *testing.B) {
		var lease *mcpDispatchSlotLease
		b.ReportAllocs()
		for b.Loop() {
			lease.beginTiming().granted()
		}
	})
	b.Run("lease-mutex-control", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			lease := mcpDispatchSlotLease{}
			lease.mu.Lock()
			lease.mu.Unlock() //nolint:staticcheck // Intentional empty lock baseline isolates existing lease mutex costs.
			lease.mu.Lock()
			lease.mu.Unlock() //nolint:staticcheck // Intentional empty lock baseline isolates existing lease mutex costs.
		}
	})
	b.Run("observed-lease", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			lease := mcpDispatchSlotLease{}
			lease.beginTiming().granted()
		}
	})
}
