package graphview

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"
	"testing/synctest"
	"time"
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

// --- W5.3: the base corpus pin ------------------------------------------

// TestBasePinHoldsGenerationZeroForTheRequest is the first half of the pin's
// contract: generation zero is in the same refcount every derived generation
// is leased through, so a mutator or a sweep consulting InUse/WaitDrain sees a
// reader on the bottom of the stack.
func TestBasePinHoldsGenerationZeroForTheRequest(t *testing.T) {
	m := NewLeaseManager()
	if m.InUse(BaseCorpusGeneration) {
		t.Fatal("generation zero is pinned before any request took it")
	}
	pin := m.AcquireBaseCorpus("")
	if pin == nil {
		t.Fatal("AcquireBaseCorpus returned nil")
	}
	if !m.InUse(BaseCorpusGeneration) {
		t.Fatal("generation zero is not pinned while a base pin is live")
	}
	if got := pin.Generations(); len(got) != 1 || got[0] != BaseCorpusGeneration {
		t.Fatalf("pin.Generations() = %v, want [%d]", got, BaseCorpusGeneration)
	}
	pin.Release()
	pin.Release()
	if m.InUse(BaseCorpusGeneration) {
		t.Fatal("generation zero is still pinned after the request released")
	}
}

// TestBasePinKeepsOwnerCleanupWaitingForTheRequest is the lifetime half: an
// owner whose admission is closed while a request is reading its corpus does
// not drain until that request lets go.
func TestBasePinKeepsOwnerCleanupWaitingForTheRequest(t *testing.T) {
	m := NewLeaseManager()
	owner := RepositoryOwner{GraphID: "g1", CheckoutID: "c1", Incarnation: "i1", RepoPrefix: "repo"}
	if err := m.RegisterRepositoryOwner(owner); err != nil {
		t.Fatal(err)
	}
	pin := m.AcquireBaseCorpus("repo")
	if !pin.OwnerPinned() {
		t.Fatal("a registered owner was not pinned by the base pin")
	}
	drain, err := m.CloseRepositoryAdmission(owner)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-drain.Done():
		t.Fatal("the owner drained while a request still held its base corpus")
	default:
	}
	pin.Release()
	select {
	case <-drain.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("the owner never drained after the request released its base pin")
	}
}

// TestBasePinWithoutASourceAuthorityNeverReportsAChange is the honesty half.
// A dedicated owner carries no source revision authority today, so the pin can
// neither confirm nor deny a mutation — and "unknown" must never be rendered
// as "changed", which would make every routed answer inexact on no evidence.
func TestBasePinWithoutASourceAuthorityNeverReportsAChange(t *testing.T) {
	m := NewLeaseManager()
	owner := RepositoryOwner{GraphID: "g1", CheckoutID: "c1", Incarnation: "i1", RepoPrefix: "repo"}
	if err := m.RegisterRepositoryOwner(owner); err != nil {
		t.Fatal(err)
	}
	pin := m.AcquireBaseCorpus("repo")
	defer pin.Release()
	if pin.Witnessed() {
		t.Fatal("a dedicated owner with no source authority reported a witness")
	}
	if err := pin.ValidateCurrent(); !errors.Is(err, ErrBaseCorpusUnwitnessed) {
		t.Fatalf("ValidateCurrent() = %v, want %v", err, ErrBaseCorpusUnwitnessed)
	}
}

// TestBasePinDetectsASourceMutationUnderTheRequest is the detection half, and
// the one the rider depends on: the pin holds lifetime, the source moves
// anyway, and the holder finds out.
func TestBasePinDetectsASourceMutationUnderTheRequest(t *testing.T) {
	m := NewLeaseManager()
	reg := privateRawRegistration(t, m, "repo", "one")
	if _, err := m.CaptureInitialRawRepositorySource(context.Background(), reg, "source-a"); err != nil {
		t.Fatal(err)
	}
	pin := m.AcquireBaseCorpus("repo")
	defer pin.Release()
	if !pin.Witnessed() {
		t.Fatal("a captured raw source was not witnessed by the base pin")
	}
	if err := pin.ValidateCurrent(); err != nil {
		t.Fatalf("ValidateCurrent() before any mutation = %v, want nil", err)
	}
	write, err := m.AcquireRawRepositoryMutation(context.Background(), reg)
	if err != nil {
		t.Fatal(err)
	}
	if err := pin.ValidateCurrent(); !errors.Is(err, ErrBaseCorpusChanged) {
		t.Fatalf("ValidateCurrent() during a source mutation = %v, want %v", err, ErrBaseCorpusChanged)
	}
	if err := write.Complete("source-b"); err != nil {
		t.Fatal(err)
	}
	write.Release()
	if err := pin.ValidateCurrent(); !errors.Is(err, ErrBaseCorpusChanged) {
		t.Fatalf("ValidateCurrent() after a completed source mutation = %v, want %v", err, ErrBaseCorpusChanged)
	}
}

// TestBasePinDetectsTheFirstMutationOfANeverCapturedOwner covers the gap the
// initial capture exists for from the other side: a pin taken before any
// authority existed still sees the authority appear, rather than reading the
// absence as "unchanged" forever.
func TestBasePinDetectsTheFirstMutationOfANeverCapturedOwner(t *testing.T) {
	m := NewLeaseManager()
	reg := privateRawRegistration(t, m, "repo", "one")
	pin := m.AcquireBaseCorpus("repo")
	defer pin.Release()
	if pin.Witnessed() {
		t.Fatal("an owner with no data authority reported a witness")
	}
	if err := pin.ValidateCurrent(); !errors.Is(err, ErrBaseCorpusUnwitnessed) {
		t.Fatalf("ValidateCurrent() = %v, want %v", err, ErrBaseCorpusUnwitnessed)
	}
	privateRawSource(t, m, reg, "source-a")
	if err := pin.ValidateCurrent(); !errors.Is(err, ErrBaseCorpusChanged) {
		t.Fatalf("ValidateCurrent() after the first mutation = %v, want %v", err, ErrBaseCorpusChanged)
	}
}

// TestBasePinSurvivesAnUnregisteredPrefix: the generation is shared, so
// reading it is what needs pinning whether or not an owner speaks for it.
func TestBasePinSurvivesAnUnregisteredPrefix(t *testing.T) {
	m := NewLeaseManager()
	pin := m.AcquireBaseCorpus("nobody")
	if pin == nil || !m.InUse(BaseCorpusGeneration) {
		t.Fatal("an unregistered prefix left the base corpus unpinned")
	}
	if pin.OwnerPinned() {
		t.Fatal("an unregistered prefix reported an owner pin")
	}
	if err := pin.ValidateCurrent(); !errors.Is(err, ErrBaseCorpusUnwitnessed) {
		t.Fatalf("ValidateCurrent() = %v, want %v", err, ErrBaseCorpusUnwitnessed)
	}
	pin.Release()
	if m.InUse(BaseCorpusGeneration) {
		t.Fatal("the base corpus stayed pinned after release")
	}
}
