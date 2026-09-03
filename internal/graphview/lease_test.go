package graphview

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"
	"testing/synctest"
)

// waitDrainInBackground runs WaitDrain in its own goroutine and reports the
// result on the returned channel. The channel is buffered so the goroutine
// always finishes, whether or not the test reads from it.
func waitDrainInBackground(ctx context.Context, m *LeaseManager, ids ...int64) <-chan error {
	done := make(chan error, 1)
	go func() { done <- m.WaitDrain(ctx, ids...) }()
	return done
}

// assertStillWaiting fails when done has already produced a result. Callers run
// it after synctest.Wait, so "not yet produced" means the waiter is durably
// blocked rather than merely slow.
func assertStillWaiting(t *testing.T, done <-chan error, when string) {
	t.Helper()
	select {
	case err := <-done:
		t.Fatalf("WaitDrain returned (%v) %s", err, when)
	default:
	}
}

// assertWoke fails unless done has already produced a nil result.
func assertWoke(t *testing.T, done <-chan error, when string) {
	t.Helper()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("WaitDrain() = %v %s, want nil", err, when)
		}
	default:
		t.Fatalf("WaitDrain did not wake %s", when)
	}
}

func TestLeaseManagerZeroValueIsUsable(t *testing.T) {
	var m LeaseManager
	if m.InUse(1) {
		t.Error("a fresh manager reports a pinned id")
	}
	l := m.Acquire(1)
	if !m.InUse(1) {
		t.Error("InUse(1) = false after Acquire")
	}
	l.Release()
	if m.InUse(1) {
		t.Error("InUse(1) = true after Release")
	}
	if err := m.WaitDrain(context.Background(), 1); err != nil {
		t.Errorf("WaitDrain() = %v on a drained id", err)
	}
}

func TestLeaseAcquireAndRelease(t *testing.T) {
	m := NewLeaseManager()
	l := m.Acquire(4, 5, 6)
	if got := l.IDs(); !slices.Equal(got, []int64{4, 5, 6}) {
		t.Errorf("IDs() = %v, want [4 5 6]", got)
	}
	for _, id := range []int64{4, 5, 6} {
		if !m.InUse(id) {
			t.Errorf("InUse(%d) = false while the lease is held", id)
		}
	}
	if m.InUse(7) {
		t.Error("InUse(7) = true for an id nobody acquired")
	}
	l.Release()
	for _, id := range []int64{4, 5, 6} {
		if m.InUse(id) {
			t.Errorf("InUse(%d) = true after Release", id)
		}
	}

	// IDs hands out a copy, so a caller cannot rewrite what the lease pins.
	l2 := m.Acquire(9)
	ids := l2.IDs()
	ids[0] = 99
	if !m.InUse(9) || m.InUse(99) {
		t.Error("mutating the IDs() copy changed what the lease pins")
	}
	l2.Release()
}

func TestLeaseEmptyAcquireAndNilRelease(t *testing.T) {
	m := NewLeaseManager()
	l := m.Acquire()
	if got := l.IDs(); len(got) != 0 {
		t.Errorf("IDs() = %v, want none", got)
	}
	l.Release()
	l.Release()

	var nilLease *Lease
	nilLease.Release()
	if got := nilLease.IDs(); got != nil {
		t.Errorf("nil lease IDs() = %v, want nil", got)
	}
}

func TestLeaseRefcountsPerID(t *testing.T) {
	m := NewLeaseManager()
	a := m.Acquire(1, 2)
	b := m.Acquire(2)
	a.Release()
	if m.InUse(1) {
		t.Error("id 1 stayed pinned after its only lease was released")
	}
	if !m.InUse(2) {
		t.Error("id 2 was dropped while a second lease still held it")
	}
	b.Release()
	if m.InUse(2) {
		t.Error("id 2 stayed pinned after every lease was released")
	}
}

func TestLeaseDoubleReleaseIsANoOp(t *testing.T) {
	m := NewLeaseManager()
	a := m.Acquire(3)
	b := m.Acquire(3)
	a.Release()
	a.Release()
	a.Release()
	if !m.InUse(3) {
		t.Fatal("a repeated Release dropped another lease's pin")
	}
	b.Release()
	if m.InUse(3) {
		t.Error("id 3 stayed pinned after every lease was released")
	}
	b.Release()
	if m.InUse(3) {
		t.Error("releasing an already-released lease resurrected the pin")
	}
}

func TestLeaseRepeatedIDInOneAcquire(t *testing.T) {
	m := NewLeaseManager()
	l := m.Acquire(8, 8)
	if !m.InUse(8) {
		t.Fatal("InUse(8) = false while the lease is held")
	}
	l.Release()
	if m.InUse(8) {
		t.Error("a doubly-pinned id survived its lease")
	}
}

func TestWaitDrainReturnsImmediatelyWhenNothingIsHeld(t *testing.T) {
	m := NewLeaseManager()
	if err := m.WaitDrain(context.Background()); err != nil {
		t.Errorf("WaitDrain() with no ids = %v", err)
	}
	if err := m.WaitDrain(context.Background(), 1, 2, 3); err != nil {
		t.Errorf("WaitDrain() on drained ids = %v", err)
	}

	// Nothing to wait for beats a cancelled context: the caller's invariant
	// already holds.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := m.WaitDrain(ctx, 1); err != nil {
		t.Errorf("WaitDrain() on a drained id with a cancelled context = %v", err)
	}
}

func TestWaitDrainWakesOnTheLastRelease(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m := NewLeaseManager()
		first := m.Acquire(1)
		second := m.Acquire(1, 2)

		done := waitDrainInBackground(context.Background(), m, 1, 2)
		synctest.Wait()
		assertStillWaiting(t, done, "while both leases were held")

		first.Release()
		synctest.Wait()
		assertStillWaiting(t, done, "while the second lease was still held")

		second.Release()
		synctest.Wait()
		assertWoke(t, done, "after the last release")
	})
}

func TestWaitDrainIgnoresUnwatchedIDs(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m := NewLeaseManager()
		watched := m.Acquire(1)
		unwatched := m.Acquire(2)
		defer unwatched.Release()

		done := waitDrainInBackground(context.Background(), m, 1)
		synctest.Wait()
		assertStillWaiting(t, done, "while the watched id was held")

		watched.Release()
		synctest.Wait()
		assertWoke(t, done, "once the watched id drained")
	})
}

func TestWaitDrainStaysBlockedWhenANewLeaseArrives(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m := NewLeaseManager()
		first := m.Acquire(1)

		done := waitDrainInBackground(context.Background(), m, 1)
		synctest.Wait()
		assertStillWaiting(t, done, "while the first lease was held")

		// A reader that arrives mid-wait re-pins the generation; the waiter
		// must not be woken by the first lease going away.
		second := m.Acquire(1)
		first.Release()
		synctest.Wait()
		assertStillWaiting(t, done, "while a lease acquired mid-wait was held")

		second.Release()
		synctest.Wait()
		assertWoke(t, done, "after the mid-wait lease released")
	})
}

func TestWaitDrainHonorsContextCancellation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m := NewLeaseManager()
		held := m.Acquire(1)
		defer held.Release()

		ctx, cancel := context.WithCancel(context.Background())
		done := waitDrainInBackground(ctx, m, 1)
		synctest.Wait()
		assertStillWaiting(t, done, "before cancellation")

		cancel()
		synctest.Wait()
		select {
		case err := <-done:
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("WaitDrain() = %v, want context.Canceled", err)
			}
		default:
			t.Fatal("WaitDrain did not return after its context was cancelled")
		}

		// Cancelling one waiter must not disturb the manager's accounting.
		if !m.InUse(1) {
			t.Error("the lease was dropped when the waiter gave up")
		}
	})
}

func TestWaitDrainWakesEveryWaiter(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m := NewLeaseManager()
		held := m.Acquire(1)

		waiters := make([]<-chan error, 4)
		for i := range waiters {
			waiters[i] = waitDrainInBackground(context.Background(), m, 1)
		}
		synctest.Wait()
		for i, done := range waiters {
			assertStillWaiting(t, done, fmt.Sprintf("before the release (waiter %d)", i))
		}

		held.Release()
		synctest.Wait()
		for i, done := range waiters {
			assertWoke(t, done, fmt.Sprintf("after the release (waiter %d)", i))
		}
	})
}

func TestLeaseManagerConcurrentAcquireRelease(t *testing.T) {
	m := NewLeaseManager()
	const (
		workers = 16
		rounds  = 200
		ids     = 4
	)

	var wg sync.WaitGroup
	for w := range workers {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			for r := range rounds {
				id := int64((worker + r) % ids)
				l := m.Acquire(id, id+ids)
				if !m.InUse(id) {
					t.Errorf("InUse(%d) = false while worker %d held it", id, worker)
					l.Release()
					return
				}
				l.Release()
				l.Release()
			}
		}(w)
	}

	// Drain waiters run against the same ids the workers churn on: they must
	// never wake early, and they must never miss the drain either.
	drained := make(chan error, 4)
	for range cap(drained) {
		go func() {
			drained <- m.WaitDrain(context.Background(), 0, 1, 2, 3, 4, 5, 6, 7)
		}()
	}

	wg.Wait()
	for range cap(drained) {
		if err := <-drained; err != nil {
			t.Errorf("WaitDrain() = %v, want nil", err)
		}
	}
	for id := int64(0); id < 2*ids; id++ {
		if m.InUse(id) {
			t.Errorf("InUse(%d) = true after every lease was released", id)
		}
	}
}
