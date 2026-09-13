package graphview

import (
	"errors"
	"fmt"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func repositoryLeaseTestOwner(suffix string) RepositoryOwner {
	return RepositoryOwner{
		GraphID:     "graph-" + suffix,
		CheckoutID:  "checkout-" + suffix,
		Incarnation: "incarnation-" + suffix,
		RepoPrefix:  "repo-" + suffix,
	}
}

func registerRepositoryTestOwner(t testing.TB, m *LeaseManager, owner RepositoryOwner) {
	t.Helper()
	if err := m.RegisterRepositoryOwner(owner); err != nil {
		t.Fatal(err)
	}
}

func acquireRepositoryTestLease(t testing.TB, m *LeaseManager, owners ...RepositoryOwner) *RepositoryReadLease {
	t.Helper()
	lease, err := m.AcquireRepositoryRead(owners...)
	if err != nil {
		t.Fatal(err)
	}
	return lease
}

func closeRepositoryTestOwner(t testing.TB, m *LeaseManager, owner RepositoryOwner) *RepositoryDrain {
	t.Helper()
	drain, err := m.CloseRepositoryAdmission(owner)
	if err != nil {
		t.Fatal(err)
	}
	return drain
}

func assertRepositoryDrain(t testing.TB, done <-chan struct{}, wantClosed bool) {
	t.Helper()
	if done == nil {
		t.Fatal("nil drain channel")
	}
	select {
	case <-done:
		if !wantClosed {
			t.Fatal("drained while a reader is held")
		}
	default:
		if wantClosed {
			t.Fatal("last release did not drain synchronously")
		}
	}
}

func TestRepositoryLeaseExplicitRegistrationAndAtomicScope(t *testing.T) {
	var m LeaseManager
	a, b := repositoryLeaseTestOwner("a"), repositoryLeaseTestOwner("b")
	if _, err := m.AcquireRepositoryRead(a); !errors.Is(err, ErrRepositoryOwnerUnknown) {
		t.Fatalf("unknown owner: %v", err)
	}
	registerRepositoryTestOwner(t, &m, a)
	registerRepositoryTestOwner(t, &m, a)
	for _, scope := range [][]RepositoryOwner{{a, b}, {a, {}}, {a, a, b}} {
		lease, err := m.AcquireRepositoryRead(scope...)
		if err == nil || lease != nil {
			t.Fatalf("partial scope admitted: lease=%v err=%v", lease, err)
		}
		if m.repositories.readers != 0 || m.repositories.byPrefix[a.RepoPrefix].readers != 0 {
			t.Fatal("failed scope left partial pins")
		}
	}
	registerRepositoryTestOwner(t, &m, b)
	lease := acquireRepositoryTestLease(t, &m, b, a, b)
	if got, want := lease.Owners(), []RepositoryOwner{b, a}; !reflect.DeepEqual(got, want) {
		t.Fatalf("scope got %v want %v", got, want)
	}
	if m.repositories.readers != 2 {
		t.Fatalf("duplicate pin count = %d", m.repositories.readers)
	}
	copyOfOwners := lease.Owners()
	copyOfOwners[0].RepoPrefix = "mutated"
	if lease.Owners()[0] != b {
		t.Fatal("Owners returned an alias")
	}
	lease.Release()
	lease.Release()
	if m.repositories.readers != 0 {
		t.Fatal("release was not balanced/idempotent")
	}
	var nilLease *RepositoryReadLease
	nilLease.Release()
	if nilLease.Owners() != nil {
		t.Fatal("nil lease has owners")
	}
	if (*RepositoryDrain)(nil).Done() != nil {
		t.Fatal("nil drain falsely appears drained")
	}
}

func TestRepositoryLeaseGenerationZeroAndAllScope(t *testing.T) {
	var m LeaseManager
	empty := acquireRepositoryTestLease(t, &m)
	if len(empty.Owners()) != 0 {
		t.Fatal("empty explicit scope means all")
	}
	empty.Release()
	all, err := m.AcquireAllRepositoryReads()
	if err != nil || len(all.Owners()) != 0 {
		t.Fatalf("empty all scope: %v, %v", all, err)
	}
	all.Release()
	a, b := repositoryLeaseTestOwner("a"), repositoryLeaseTestOwner("b")
	registerRepositoryTestOwner(t, &m, b)
	registerRepositoryTestOwner(t, &m, a)
	all, err = m.AcquireAllRepositoryReads()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(all.Owners(), []RepositoryOwner{a, b}) {
		t.Fatalf("all scope is not deterministic: %v", all.Owners())
	}
	if m.Held() != 0 || m.InUse(0) {
		t.Fatal("repository pin unexpectedly used the generation lease namespace")
	}
	drain := closeRepositoryTestOwner(t, &m, a)
	assertRepositoryDrain(t, drain.Done(), false)
	if _, err := m.AcquireAllRepositoryReads(); !errors.Is(err, ErrRepositoryAdmissionClosed) {
		t.Fatalf("broad query omitted closing payload: %v", err)
	}
	if m.repositories.readers != 0 || m.repositories.broadReaders != 1 {
		t.Fatal("failed broad query left partial pins")
	}
	other := acquireRepositoryTestLease(t, &m, b)
	other.Release()
	all.Release()
	assertRepositoryDrain(t, drain.Done(), true)
	if _, err := m.AcquireAllRepositoryReads(); !errors.Is(err, ErrRepositoryAdmissionClosed) {
		t.Fatalf("drain removed closing tombstone: %v", err)
	}
	if err := m.FinalizeRepositoryCleanup(drain); err != nil {
		t.Fatal(err)
	}
	all, err = m.AcquireAllRepositoryReads()
	if err != nil || !reflect.DeepEqual(all.Owners(), []RepositoryOwner{b}) {
		t.Fatalf("finalized all scope: %v, %v", all, err)
	}
	all.Release()
}

func TestRepositoryLeaseClosingTombstoneAndHandleABA(t *testing.T) {
	var m LeaseManager
	owner := repositoryLeaseTestOwner("old")
	registerRepositoryTestOwner(t, &m, owner)
	lease := acquireRepositoryTestLease(t, &m, owner)
	drain := closeRepositoryTestOwner(t, &m, owner)
	if again := closeRepositoryTestOwner(t, &m, owner); again != drain {
		t.Fatal("close created a second cleanup identity")
	}
	if err := m.FinalizeRepositoryCleanup(drain); !errors.Is(err, ErrRepositoryLeaseInUse) {
		t.Fatalf("finalized a live lease: %v", err)
	}
	if err := m.RegisterRepositoryOwner(owner); !errors.Is(err, ErrRepositoryAdmissionClosed) {
		t.Fatalf("reopened a closing owner: %v", err)
	}
	replacement := owner
	replacement.Incarnation = "incarnation-new"
	if err := m.RegisterRepositoryOwner(replacement); !errors.Is(err, ErrRepositoryOwnerConflict) {
		t.Fatalf("reused prefix before finalize: %v", err)
	}
	graphAlias := owner
	graphAlias.RepoPrefix = "different-prefix"
	if err := m.RegisterRepositoryOwner(graphAlias); !errors.Is(err, ErrRepositoryOwnerConflict) {
		t.Fatalf("reused graph through another prefix: %v", err)
	}
	if _, err := m.AcquireRepositoryRead(replacement); !errors.Is(err, ErrRepositoryOwnerUnknown) {
		t.Fatalf("new incarnation borrowed old admission: %v", err)
	}
	var otherManager LeaseManager
	if err := otherManager.FinalizeRepositoryCleanup(drain); !errors.Is(err, ErrRepositoryDrainInvalid) {
		t.Fatalf("foreign manager finalized owner: %v", err)
	}
	if err := m.FinalizeRepositoryCleanup(&RepositoryDrain{mgr: &m, state: drain.state}); !errors.Is(err, ErrRepositoryDrainInvalid) {
		t.Fatalf("forged cleanup handle: %v", err)
	}
	lease.Release()
	assertRepositoryDrain(t, drain.Done(), true)
	if err := m.FinalizeRepositoryCleanup(drain); err != nil {
		t.Fatal(err)
	}
	registerRepositoryTestOwner(t, &m, replacement)
	newLease := acquireRepositoryTestLease(t, &m, replacement)
	newDrain := closeRepositoryTestOwner(t, &m, replacement)
	lease.Release()
	if err := m.FinalizeRepositoryCleanup(drain); err != nil {
		t.Fatalf("old handle lost idempotence: %v", err)
	}
	if err := m.FinalizeRepositoryCleanup(&RepositoryDrain{mgr: &m, state: drain.state}); !errors.Is(err, ErrRepositoryDrainInvalid) {
		t.Fatalf("forged finalized handle: %v", err)
	}
	assertRepositoryDrain(t, newDrain.Done(), false)
	if m.repositories.byPrefix[replacement.RepoPrefix].owner != replacement {
		t.Fatal("old finalizer removed replacement")
	}
	if _, err := m.CloseRepositoryAdmission(owner); !errors.Is(err, ErrRepositoryOwnerUnknown) {
		t.Fatalf("old incarnation closed replacement: %v", err)
	}
	newLease.Release()
	if err := m.FinalizeRepositoryCleanup(newDrain); err != nil {
		t.Fatal(err)
	}
	if m.repositories.byPrefix != nil || m.repositories.byGraph != nil || m.repositories.readers != 0 {
		t.Fatal("finalized manager retained registration state")
	}
}

func TestRepositoryLeaseBroadReadCoversLaterRegistrations(t *testing.T) {
	for _, withInitialOwner := range []bool{false, true} {
		t.Run(fmt.Sprintf("initial-owner=%t", withInitialOwner), func(t *testing.T) {
			var m LeaseManager
			a, b := repositoryLeaseTestOwner("a"), repositoryLeaseTestOwner("later-b")
			if withInitialOwner {
				registerRepositoryTestOwner(t, &m, a)
			}
			first, err := m.AcquireAllRepositoryReads()
			if err != nil {
				t.Fatal(err)
			}
			second, err := m.AcquireAllRepositoryReads()
			if err != nil {
				t.Fatal(err)
			}
			initialScope := first.Owners()
			registerRepositoryTestOwner(t, &m, b)
			if !reflect.DeepEqual(first.Owners(), initialScope) {
				t.Fatal("later registration mutated diagnostic scope snapshot")
			}
			var aDrain *RepositoryDrain
			var aErr error
			var closeA sync.WaitGroup
			if withInitialOwner {
				closeA.Add(1)
				go func() { defer closeA.Done(); aDrain, aErr = m.CloseRepositoryAdmission(a) }()
			}
			bDrain := closeRepositoryTestOwner(t, &m, b)
			closeA.Wait()
			if aErr != nil {
				t.Fatal(aErr)
			}
			assertRepositoryDrain(t, bDrain.Done(), false)
			var notified atomic.Int32
			bDrain.Notify(func() { notified.Add(1) })
			if err := m.FinalizeRepositoryCleanup(bDrain); !errors.Is(err, ErrRepositoryLeaseInUse) {
				t.Fatalf("later owner finalized under broad reader: %v", err)
			}
			// Explicit empty is caller-proven control, not a broad payload read.
			control := acquireRepositoryTestLease(t, &m)
			control.Release()
			first.Release()
			assertRepositoryDrain(t, bDrain.Done(), false)
			if notified.Load() != 0 || m.repositories.readers != 0 || m.repositories.broadReaders != 1 {
				t.Fatal("non-final broad release changed explicit counts or woke cleanup")
			}
			if aDrain != nil {
				assertRepositoryDrain(t, aDrain.Done(), false)
			}
			second.Release()
			assertRepositoryDrain(t, bDrain.Done(), true)
			if notified.Load() != 1 {
				t.Fatalf("later owner's targeted wake count=%d", notified.Load())
			}
			if aDrain != nil {
				assertRepositoryDrain(t, aDrain.Done(), true)
				if err := m.FinalizeRepositoryCleanup(aDrain); err != nil {
					t.Fatal(err)
				}
			}
			if err := m.FinalizeRepositoryCleanup(bDrain); err != nil {
				t.Fatal(err)
			}
			if m.repositories.byPrefix != nil || m.repositories.closing != nil || m.repositories.broadReaders != 0 {
				t.Fatal("broad cleanup retained owner state")
			}
		})
	}
}

func TestRepositoryLeaseBroadAndExplicitDrainIndependently(t *testing.T) {
	var m LeaseManager
	a, b := repositoryLeaseTestOwner("a"), repositoryLeaseTestOwner("b")
	registerRepositoryTestOwner(t, &m, a)
	registerRepositoryTestOwner(t, &m, b)
	broad, err := m.AcquireAllRepositoryReads()
	if err != nil {
		t.Fatal(err)
	}
	explicit := acquireRepositoryTestLease(t, &m, a)
	aDrain := closeRepositoryTestOwner(t, &m, a)
	bDrain := closeRepositoryTestOwner(t, &m, b)
	var aCalls, bCalls atomic.Int32
	aDrain.Notify(func() { aCalls.Add(1) })
	bDrain.Notify(func() { bCalls.Add(1) })
	broad.Release()
	assertRepositoryDrain(t, bDrain.Done(), true)
	assertRepositoryDrain(t, aDrain.Done(), false)
	if m.repositories.readers != 1 || m.repositories.broadReaders != 0 || aCalls.Load() != 0 || bCalls.Load() != 1 {
		t.Fatal("broad release consumed explicit pins or notified a non-drained owner")
	}
	explicit.Release()
	assertRepositoryDrain(t, aDrain.Done(), true)
	if aCalls.Load() != 1 || bCalls.Load() != 1 {
		t.Fatal("explicit release repeated another owner's notification")
	}
}

func TestRepositoryLeaseShutdownWaitsForInitiallyEmptyBroadRead(t *testing.T) {
	var m LeaseManager
	broad, err := m.AcquireAllRepositoryReads()
	if err != nil {
		t.Fatal(err)
	}
	control := acquireRepositoryTestLease(t, &m)
	done := m.ShutdownRepositoryAdmissions()
	assertRepositoryDrain(t, done, false)
	control.Release()
	assertRepositoryDrain(t, done, false)
	broad.Release()
	assertRepositoryDrain(t, done, true)
	if _, err := m.AcquireAllRepositoryReads(); !errors.Is(err, ErrRepositoryAdmissionsStopped) {
		t.Fatalf("empty stopped broad domain reopened: %v", err)
	}
	if err := m.RegisterRepositoryOwner(repositoryLeaseTestOwner("late")); !errors.Is(err, ErrRepositoryAdmissionsStopped) {
		t.Fatalf("empty stopped domain registered an owner: %v", err)
	}
}

func TestRepositoryLeaseDrainNotificationTargetingAndReentry(t *testing.T) {
	var m LeaseManager
	a, b := repositoryLeaseTestOwner("a"), repositoryLeaseTestOwner("b")
	registerRepositoryTestOwner(t, &m, a)
	registerRepositoryTestOwner(t, &m, b)
	lease := acquireRepositoryTestLease(t, &m, a)
	second := acquireRepositoryTestLease(t, &m, a)
	drain := closeRepositoryTestOwner(t, &m, a)
	var calls atomic.Int32
	stop := drain.Notify(func() {
		calls.Add(1)
		// Re-enter both the manager and this release-once section. Neither
		// lock may be held while a callback executes.
		other := acquireRepositoryTestLease(t, &m, b)
		other.Release()
		second.Release()
	})
	var canceled atomic.Int32
	cancel := drain.Notify(func() { canceled.Add(1) })
	cancel()
	cancel()
	other := acquireRepositoryTestLease(t, &m, b)
	other.Release()
	lease.Release()
	if calls.Load() != 0 || canceled.Load() != 0 {
		t.Fatal("unrelated/non-final release notified cleanup")
	}
	second.Release()
	stop()
	if calls.Load() != 1 || canceled.Load() != 0 {
		t.Fatalf("notifications: live=%d canceled=%d", calls.Load(), canceled.Load())
	}
	if len(drain.state.notifications) != 0 {
		t.Fatal("drained owner retained callbacks")
	}
	var immediate int
	lateStop := drain.Notify(func() {
		immediate++
		if err := m.FinalizeRepositoryCleanup(drain); err != nil {
			t.Fatal(err)
		}
	})
	lateStop()
	lateStop()
	if immediate != 1 {
		t.Fatalf("late registration was not immediate/once: %d", immediate)
	}
}

func TestRepositoryLeaseAcquireCloseLinearization(t *testing.T) {
	for iteration := 0; iteration < 128; iteration++ {
		var m LeaseManager
		owner := repositoryLeaseTestOwner("race")
		registerRepositoryTestOwner(t, &m, owner)
		start := make(chan struct{})
		type acquired struct {
			lease *RepositoryReadLease
			err   error
		}
		result := make(chan acquired, 1)
		closed := make(chan *RepositoryDrain, 1)
		go func() {
			<-start
			lease, err := m.AcquireRepositoryRead(owner)
			result <- acquired{lease, err}
		}()
		go func() {
			<-start
			drain, err := m.CloseRepositoryAdmission(owner)
			if err != nil {
				closed <- nil
				return
			}
			closed <- drain
		}()
		close(start)
		got, drain := <-result, <-closed
		if drain == nil {
			t.Fatal("close failed")
		}
		if got.err == nil {
			assertRepositoryDrain(t, drain.Done(), false)
			got.lease.Release()
		} else if !errors.Is(got.err, ErrRepositoryAdmissionClosed) || got.lease != nil {
			t.Fatalf("invalid race outcome: %+v", got)
		}
		assertRepositoryDrain(t, drain.Done(), true)
		if _, err := m.AcquireRepositoryRead(owner); !errors.Is(err, ErrRepositoryAdmissionClosed) {
			t.Fatalf("post-close acquire: %v", err)
		}
	}
}

func TestRepositoryLeaseAllScopeCloseLinearization(t *testing.T) {
	for iteration := 0; iteration < 64; iteration++ {
		var m LeaseManager
		a, b := repositoryLeaseTestOwner("a"), repositoryLeaseTestOwner("b")
		registerRepositoryTestOwner(t, &m, a)
		registerRepositoryTestOwner(t, &m, b)
		start := make(chan struct{})
		var lease *RepositoryReadLease
		var acquireErr error
		var drain *RepositoryDrain
		var closeErr error
		var group sync.WaitGroup
		group.Add(2)
		go func() { defer group.Done(); <-start; lease, acquireErr = m.AcquireAllRepositoryReads() }()
		go func() { defer group.Done(); <-start; drain, closeErr = m.CloseRepositoryAdmission(b) }()
		close(start)
		group.Wait()
		if closeErr != nil {
			t.Fatal(closeErr)
		}
		if acquireErr == nil {
			if m.repositories.readers != 0 || m.repositories.broadReaders != 1 || len(lease.Owners()) != 2 {
				t.Fatal("all acquisition was partial")
			}
			assertRepositoryDrain(t, drain.Done(), false)
			lease.Release()
		} else if !errors.Is(acquireErr, ErrRepositoryAdmissionClosed) || lease != nil || m.repositories.readers != 0 || m.repositories.broadReaders != 0 {
			t.Fatalf("failed all acquisition left state: %v", acquireErr)
		}
		assertRepositoryDrain(t, drain.Done(), true)
	}
}

func TestRepositoryLeaseConcurrentReleaseAndNotification(t *testing.T) {
	var m LeaseManager
	owner := repositoryLeaseTestOwner("once")
	registerRepositoryTestOwner(t, &m, owner)
	lease := acquireRepositoryTestLease(t, &m, owner)
	drain := closeRepositoryTestOwner(t, &m, owner)
	var calls atomic.Int32
	drain.Notify(func() { calls.Add(1) })
	var group sync.WaitGroup
	for i := 0; i < 32; i++ {
		group.Add(1)
		go func() { defer group.Done(); lease.Release() }()
	}
	group.Wait()
	assertRepositoryDrain(t, drain.Done(), true)
	if calls.Load() != 1 || m.repositories.readers != 0 {
		t.Fatalf("concurrent release: notifications=%d readers=%d", calls.Load(), m.repositories.readers)
	}
}

func TestRepositoryLeaseNotifyDrainLinearization(t *testing.T) {
	for iteration := 0; iteration < 64; iteration++ {
		var m LeaseManager
		owner := repositoryLeaseTestOwner("notify")
		registerRepositoryTestOwner(t, &m, owner)
		lease := acquireRepositoryTestLease(t, &m, owner)
		drain := closeRepositoryTestOwner(t, &m, owner)
		start := make(chan struct{})
		var calls atomic.Int32
		var cancel func()
		var group sync.WaitGroup
		group.Add(2)
		go func() {
			defer group.Done()
			<-start
			cancel = drain.Notify(func() { calls.Add(1) })
		}()
		go func() { defer group.Done(); <-start; lease.Release() }()
		close(start)
		group.Wait()
		cancel()
		if calls.Load() != 1 {
			t.Fatalf("notify/drain race lost or repeated wake: %d", calls.Load())
		}
		assertRepositoryDrain(t, drain.Done(), true)
		if len(drain.state.notifications) != 0 {
			t.Fatal("notify/drain race retained a callback")
		}
	}
}

func TestRepositoryLeaseShutdownPermanentAndGenerationCompatibility(t *testing.T) {
	var m LeaseManager
	owner := repositoryLeaseTestOwner("shutdown")
	registerRepositoryTestOwner(t, &m, owner)
	lease := acquireRepositoryTestLease(t, &m, owner)
	generationLease := m.Acquire(7, 7)
	done := m.ShutdownRepositoryAdmissions()
	if m.ShutdownRepositoryAdmissions() != done {
		t.Fatal("shutdown returned a different drain channel")
	}
	assertRepositoryDrain(t, done, false)
	if !m.InUse(7) {
		t.Fatal("owner shutdown affected generation leases")
	}
	lateGeneration := m.Acquire(8)
	if !m.InUse(8) {
		t.Fatal("owner shutdown changed generation Acquire compatibility")
	}
	lateGeneration.Release()
	for _, acquire := range []func() (*RepositoryReadLease, error){
		func() (*RepositoryReadLease, error) { return m.AcquireRepositoryRead(owner) },
		func() (*RepositoryReadLease, error) { return m.AcquireRepositoryRead() },
		m.AcquireAllRepositoryReads,
	} {
		if got, err := acquire(); got != nil || !errors.Is(err, ErrRepositoryAdmissionsStopped) {
			t.Fatalf("shutdown allowed acquire: %v, %v", got, err)
		}
	}
	lease.Release()
	assertRepositoryDrain(t, done, true)
	if !m.InUse(7) {
		t.Fatal("owner drain released an independent generation lease")
	}
	generationLease.Release()
	drain := closeRepositoryTestOwner(t, &m, owner)
	if err := m.FinalizeRepositoryCleanup(drain); err != nil {
		t.Fatal(err)
	}
	if err := m.RegisterRepositoryOwner(owner); !errors.Is(err, ErrRepositoryAdmissionsStopped) {
		t.Fatalf("empty stopped manager reopened: %v", err)
	}
	if m.ShutdownRepositoryAdmissions() != done {
		t.Fatal("finalization reset shutdown identity")
	}
}

func TestRepositoryLeaseShutdownRegisterAcquireLinearization(t *testing.T) {
	for iteration := 0; iteration < 64; iteration++ {
		var m LeaseManager
		owner := repositoryLeaseTestOwner("race")
		start := make(chan struct{})
		var registerErr, acquireErr error
		var lease *RepositoryReadLease
		var done <-chan struct{}
		var group sync.WaitGroup
		group.Add(2)
		go func() {
			defer group.Done()
			<-start
			registerErr = m.RegisterRepositoryOwner(owner)
			if registerErr == nil {
				lease, acquireErr = m.AcquireRepositoryRead(owner)
			}
		}()
		go func() { defer group.Done(); <-start; done = m.ShutdownRepositoryAdmissions() }()
		close(start)
		group.Wait()
		if registerErr != nil && !errors.Is(registerErr, ErrRepositoryAdmissionsStopped) {
			t.Fatal(registerErr)
		}
		if acquireErr != nil && !errors.Is(acquireErr, ErrRepositoryAdmissionsStopped) {
			t.Fatal(acquireErr)
		}
		if lease != nil {
			assertRepositoryDrain(t, done, false)
			lease.Release()
		}
		assertRepositoryDrain(t, done, true)
		if err := m.RegisterRepositoryOwner(owner); !errors.Is(err, ErrRepositoryAdmissionsStopped) {
			t.Fatalf("late register: %v", err)
		}
	}
}

func TestRepositoryLeaseFinalizationBoundsManagerState(t *testing.T) {
	var m LeaseManager
	for i := 0; i < 256; i++ {
		owner := repositoryLeaseTestOwner(fmt.Sprint(i))
		registerRepositoryTestOwner(t, &m, owner)
		lease := acquireRepositoryTestLease(t, &m, owner)
		drain := closeRepositoryTestOwner(t, &m, owner)
		stop := drain.Notify(func() {})
		lease.Release()
		stop()
		if err := m.FinalizeRepositoryCleanup(drain); err != nil {
			t.Fatal(err)
		}
		if m.repositories.byPrefix != nil || m.repositories.byGraph != nil || m.repositories.closing != nil || m.repositories.readers != 0 || m.repositories.broadReaders != 0 || drain.state.notifications != nil {
			t.Fatalf("iteration %d retained manager-owned history", i)
		}
	}
}

func BenchmarkRepositoryLeaseAcquire(b *testing.B) {
	for _, count := range []int{0, 1, 20} {
		for _, all := range []bool{false, true} {
			name := fmt.Sprintf("owners=%d/all=%t", count, all)
			b.Run(name, func(b *testing.B) {
				var m LeaseManager
				owners := make([]RepositoryOwner, count)
				for i := range owners {
					owners[i] = repositoryLeaseTestOwner(fmt.Sprintf("%02d", i))
					registerRepositoryTestOwner(b, &m, owners[i])
				}
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					var lease *RepositoryReadLease
					var err error
					if all {
						lease, err = m.AcquireAllRepositoryReads()
					} else {
						lease, err = m.AcquireRepositoryRead(owners...)
					}
					if err != nil || len(lease.states) != count {
						b.Fatalf("scope oracle: lease=%v err=%v count=%d", lease, err, count)
					}
					lease.Release()
				}
				b.StopTimer()
				if m.repositories.readers != 0 || m.repositories.broadReaders != 0 || len(m.repositories.byPrefix) != count || len(m.repositories.byGraph) != count {
					b.Fatal("acquisition accumulated registrations or leaked pins")
				}
			})
		}
	}
}

// ---------------------------------------------------------------------------
// W5.4 — the serving request's own repository admission, and the joined
// consumers that outlive it.
// ---------------------------------------------------------------------------

// TestServingRepositoryReadAdmitsEveryOpenOwner pins the scope rule: a serving
// request is admitted to every registered, open owner at once, and to no
// closing one. Admitting it to a closing owner would cross a boundary no new
// reader may cross; refusing the whole acquisition because of that owner —
// which is what the broad path does — would instead make an unrelated
// repository's untrack deny service to every request in the process.
func TestServingRepositoryReadAdmitsEveryOpenOwner(t *testing.T) {
	var m LeaseManager
	if lease := m.AcquireServingRepositoryRead(); lease != nil {
		t.Fatal("a manager with no registered owner admitted a serving request to something")
	}
	open, closing := repositoryLeaseTestOwner("open"), repositoryLeaseTestOwner("closing")
	registerRepositoryTestOwner(t, &m, open)
	registerRepositoryTestOwner(t, &m, closing)
	drain := closeRepositoryTestOwner(t, &m, closing)
	assertRepositoryDrain(t, drain.Done(), true)

	lease := m.AcquireServingRepositoryRead()
	if lease == nil {
		t.Fatal("a request found no open owner to be admitted to")
	}
	if got := lease.Owners(); !reflect.DeepEqual(got, []RepositoryOwner{open}) {
		t.Fatalf("serving scope = %v, want exactly the open owner %v", got, open)
	}
	if lease.broad {
		t.Fatal("the serving scope is broad; one request would then block every owner's drain")
	}
	// The open owner's own drain now waits for the request, and only for it.
	openDrain := closeRepositoryTestOwner(t, &m, open)
	assertRepositoryDrain(t, openDrain.Done(), false)
	lease.Release()
	assertRepositoryDrain(t, openDrain.Done(), true)
}

// TestServingRepositoryReadNeverRefusesAServingRequest: once admissions are
// stopped there is nothing to admit to, and the answer is "no lifetime held",
// never an error the request surface would have to turn into a refusal.
func TestServingRepositoryReadNeverRefusesAServingRequest(t *testing.T) {
	var m LeaseManager
	owner := repositoryLeaseTestOwner("stopped")
	registerRepositoryTestOwner(t, &m, owner)
	<-m.ShutdownRepositoryAdmissions()
	if lease := m.AcquireServingRepositoryRead(); lease != nil {
		t.Fatal("a stopped manager still admitted a serving request")
	}
	// Nil-safe on every method, so no call site branches on the result.
	var none *RepositoryReadLease
	none.Release()
	if none.Handoff() != nil || none.Owners() != nil || none.Holders() != 0 {
		t.Fatal("a nil serving scope is not nil-safe")
	}
}

// TestRepositoryReadHandoffOutlivesTheAcquirer is the lifetime half: work the
// request leaves running keeps the repository un-finalizable until it actually
// finishes, exactly as a joined generation lease keeps payload unretirable.
func TestRepositoryReadHandoffOutlivesTheAcquirer(t *testing.T) {
	var m LeaseManager
	owner := repositoryLeaseTestOwner("joined")
	registerRepositoryTestOwner(t, &m, owner)
	lease := acquireRepositoryTestLease(t, &m, owner)
	joined := lease.Handoff()
	if joined == nil {
		t.Fatal("a live lease refused a joined consumer")
	}
	if got := lease.Holders(); got != 2 {
		t.Fatalf("holders after one handoff = %d, want 2", got)
	}
	if got := joined.Owners(); !reflect.DeepEqual(got, []RepositoryOwner{owner}) {
		t.Fatalf("joined scope = %v, want %v", got, []RepositoryOwner{owner})
	}
	drain := closeRepositoryTestOwner(t, &m, owner)
	assertRepositoryDrain(t, drain.Done(), false)

	// The request returns; only the detached worker's hold is left.
	lease.Release()
	lease.Release()
	assertRepositoryDrain(t, drain.Done(), false)
	if got := lease.Holders(); got != 1 {
		t.Fatalf("holders after the acquirer released = %d, want 1", got)
	}

	joined.Release()
	assertRepositoryDrain(t, drain.Done(), true)
	joined.Release()
	if got := m.repositories.readers; got != 0 {
		t.Fatalf("readers after every holder released = %d, want 0", got)
	}
	if lease.Handoff() != nil {
		t.Fatal("a fully released lease handed out a pin that pins nothing")
	}
}

// TestBasePinHandoffCarriesBothHalves is the defect this closes on the read
// path: a request's base pin holds generation zero AND the owner that speaks
// for it, and both used to die when the request returned even though the work
// that borrowed the view was still running.
func TestBasePinHandoffCarriesBothHalves(t *testing.T) {
	var m LeaseManager
	owner := repositoryLeaseTestOwner("base")
	registerRepositoryTestOwner(t, &m, owner)
	pin := m.AcquireBaseCorpus(owner.RepoPrefix)
	if !pin.OwnerPinned() {
		t.Fatal("the base pin took no owner half to hand off")
	}
	joined := pin.Handoff()
	if joined == nil || !joined.OwnerPinned() {
		t.Fatal("the handoff dropped the owner half")
	}
	if got := joined.Generations(); !reflect.DeepEqual(got, []int64{BaseCorpusGeneration}) {
		t.Fatalf("joined generations = %v, want [%d]", got, BaseCorpusGeneration)
	}
	drain := closeRepositoryTestOwner(t, &m, owner)
	assertRepositoryDrain(t, drain.Done(), false)

	// The request ends. The detached worker still reads the corpus, so neither
	// half may be released yet.
	pin.Release()
	if !m.InUse(BaseCorpusGeneration) {
		t.Fatal("generation zero was released under a detached worker")
	}
	assertRepositoryDrain(t, drain.Done(), false)

	joined.Release()
	joined.Release()
	if m.InUse(BaseCorpusGeneration) {
		t.Fatal("generation zero stayed pinned after the last holder released")
	}
	assertRepositoryDrain(t, drain.Done(), true)
}

// TestBasePinHandoffIsRefusedOnceTheRequestEnded: a worker that asks after its
// request is over gets nil — never a handle over a corpus whose owner may
// already have finalized.
func TestBasePinHandoffIsRefusedOnceTheRequestEnded(t *testing.T) {
	var m LeaseManager
	owner := repositoryLeaseTestOwner("late")
	registerRepositoryTestOwner(t, &m, owner)
	pin := m.AcquireBaseCorpus(owner.RepoPrefix)
	pin.Release()
	if pin.Handoff() != nil {
		t.Fatal("a released base pin handed out a joined consumer")
	}
	var absent *BasePin
	if absent.Handoff() != nil {
		t.Fatal("a nil base pin handed out a joined consumer")
	}
	// An unregistered prefix holds the generation half only, and joining it is
	// a no-op on the owner half rather than a refusal.
	unowned := m.AcquireBaseCorpus("nobody")
	joined := unowned.Handoff()
	if joined == nil {
		t.Fatal("an owner-less base pin refused a joined consumer")
	}
	if joined.OwnerPinned() {
		t.Fatal("an owner-less base pin invented an owner half")
	}
	unowned.Release()
	if !m.InUse(BaseCorpusGeneration) {
		t.Fatal("the joined consumer lost the generation half")
	}
	joined.Release()
	if m.InUse(BaseCorpusGeneration) {
		t.Fatal("generation zero stayed pinned after every holder released")
	}
}

// TestServingRepositoryReadConcurrentHandoffAndClose is the race-detector arm:
// admission, handoff, release and closure all touch the same owner state, and
// the drain must fire exactly once, after the last holder.
func TestServingRepositoryReadConcurrentHandoffAndClose(t *testing.T) {
	var m LeaseManager
	owner := repositoryLeaseTestOwner("racing")
	registerRepositoryTestOwner(t, &m, owner)

	// One deterministic in-flight request with a detached worker, so the close
	// below is guaranteed to have something to wait for however the racing
	// goroutines are scheduled.
	held := m.AcquireServingRepositoryRead()
	if held == nil {
		t.Fatal("the first request was admitted to nothing")
	}
	detached := held.Handoff()
	if detached == nil {
		t.Fatal("a live serving scope refused a joined consumer")
	}

	const readers = 32
	var wg sync.WaitGroup
	var joins atomic.Int64
	start := make(chan struct{})
	for i := 0; i < readers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			lease := m.AcquireServingRepositoryRead()
			if lease == nil {
				return
			}
			joined := lease.Handoff()
			lease.Release()
			if joined != nil {
				joins.Add(1)
				joined.Release()
			}
		}()
	}
	close(start)
	drain := closeRepositoryTestOwner(t, &m, owner)
	wg.Wait()
	assertRepositoryDrain(t, drain.Done(), false)
	held.Release()
	assertRepositoryDrain(t, drain.Done(), false)
	detached.Release()
	<-drain.Done()
	if joins.Load() == 0 {
		t.Log("no racing request won admission before the close; the deterministic arm still ran")
	}
	if m.repositories.readers != 0 || m.repositories.broadReaders != 0 {
		t.Fatalf("leaked pins: readers=%d broad=%d", m.repositories.readers, m.repositories.broadReaders)
	}
}

// TestServingRepositoryHandoffForNarrowsToTheNamedRepositories is the bound on
// what a detached worker may block.
//
// A serving request is admitted to every open owner because it cannot know
// which one its handler will read. Handing THAT scope to work of unbounded
// duration would make one repository's background job block every other
// repository's drain — and the physical payload purge behind an untrack — for
// as long as it ran. A worker names what it reads, so it is joined to exactly
// those owners and its handle's lifetime is independent of the acquirer's.
func TestServingRepositoryHandoffForNarrowsToTheNamedRepositories(t *testing.T) {
	var m LeaseManager
	read, unread := repositoryLeaseTestOwner("read"), repositoryLeaseTestOwner("unread")
	registerRepositoryTestOwner(t, &m, read)
	registerRepositoryTestOwner(t, &m, unread)

	lease := m.AcquireServingRepositoryRead()
	if got := len(lease.Owners()); got != 2 {
		t.Fatalf("the request itself is admitted to %d owners, want both", got)
	}
	worker := lease.HandoffFor(read.RepoPrefix)
	if worker == nil {
		t.Fatal("a live serving scope refused a narrowed handoff for a repository it holds")
	}
	if got := worker.Owners(); !reflect.DeepEqual(got, []RepositoryOwner{read}) {
		t.Fatalf("narrowed handoff scope = %v, want exactly %v", got, read)
	}
	// The request ends; only the worker's named hold is left.
	lease.Release()
	unreadDrain := closeRepositoryTestOwner(t, &m, unread)
	assertRepositoryDrain(t, unreadDrain.Done(), true)
	readDrain := closeRepositoryTestOwner(t, &m, read)
	assertRepositoryDrain(t, readDrain.Done(), false)

	worker.Release()
	worker.Release()
	assertRepositoryDrain(t, readDrain.Done(), true)
	if m.repositories.readers != 0 || m.repositories.broadReaders != 0 {
		t.Fatalf("leaked pins: readers=%d broad=%d", m.repositories.readers, m.repositories.broadReaders)
	}
}

// TestServingRepositoryHandoffForRefusesWhatItDoesNotHold: the narrowing is a
// filter over the admitted scope, never a widening of it. A caller naming a
// repository the acquisition skipped — a closing one, or one registered after
// the request started — is admitted to nothing rather than to a boundary no
// new reader may cross.
func TestServingRepositoryHandoffForRefusesWhatItDoesNotHold(t *testing.T) {
	var m LeaseManager
	held, closing := repositoryLeaseTestOwner("held"), repositoryLeaseTestOwner("skipped")
	registerRepositoryTestOwner(t, &m, held)
	registerRepositoryTestOwner(t, &m, closing)
	drain := closeRepositoryTestOwner(t, &m, closing)
	assertRepositoryDrain(t, drain.Done(), true)

	lease := m.AcquireServingRepositoryRead()
	if lease.HandoffFor(closing.RepoPrefix) != nil {
		t.Fatal("a worker widened its scope to a repository its request was never admitted to")
	}
	if lease.HandoffFor() != nil || lease.HandoffFor("") != nil {
		t.Fatal("a worker that names no repository was admitted to something")
	}
	// Nil-safe and refused once every holder has released, exactly as the
	// unnarrowed Handoff is.
	var none *RepositoryReadLease
	if none.HandoffFor(held.RepoPrefix) != nil {
		t.Fatal("a nil serving scope handed out a narrowed consumer")
	}
	lease.Release()
	if lease.HandoffFor(held.RepoPrefix) != nil {
		t.Fatal("a released serving scope handed out a narrowed consumer")
	}
}

// TestBasePinHandoffIsNeverPartial: a handoff that races the request's own
// release either carries BOTH halves or is refused. Joining the generation
// half first and the owner half second used to allow a third outcome — a
// non-nil handle whose owner half came back nil — which presents as a
// successful handoff while silently having lost the lifetime that keeps the
// payload from being purged. The race is reachable in production: the deadline
// firewall retains on its own goroutine while the handler goroutine may
// already be running `defer view.close()`.
func TestBasePinHandoffIsNeverPartial(t *testing.T) {
	for range 20000 {
		var m LeaseManager
		owner := repositoryLeaseTestOwner("partial")
		registerRepositoryTestOwner(t, &m, owner)
		pin := m.AcquireBaseCorpus(owner.RepoPrefix)
		if !pin.OwnerPinned() {
			t.Fatal("the base pin took no owner half")
		}
		var wg sync.WaitGroup
		start := make(chan struct{})
		var joined *BasePinHandoff
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			joined = pin.Handoff()
		}()
		go func() {
			defer wg.Done()
			<-start
			pin.Release()
		}()
		close(start)
		wg.Wait()
		if joined != nil && !joined.OwnerPinned() {
			t.Fatal("a handoff presented as successful lost its owner half to a concurrent release")
		}
		joined.Release()
		if m.InUse(BaseCorpusGeneration) {
			t.Fatal("generation zero stayed pinned after every holder released")
		}
	}
}

// TestRepositoryOwnerHandleIsOrderedAgainstFinalization pins what makes
// RegisterRepositoryOwnerHandle's two critical sections one registration.
//
// The registration happens under the lease mutex and the handle is resolved
// under a second hold of it, so the pair is ordered by registerMu instead: the
// registration holds it across both sections, and FinalizeRepositoryCleanup —
// the only step that removes a live registration from byPrefix/byGraph, and
// therefore the only one that can free a prefix for a replacement — takes it
// too. Without that order, a finalization plus a replacement registration
// landing between the sections would hand the caller a handle naming the
// replacement, and closing "its own" registration through it would close
// somebody else's.
func TestRepositoryOwnerHandleIsOrderedAgainstFinalization(t *testing.T) {
	var m LeaseManager
	owner := repositoryLeaseTestOwner("ordered")

	// Half one: the registration holds the order across the whole call. The
	// prepare hook runs inside the first critical section, which is exactly
	// where a second section would be entered from.
	prepared := false
	handle, err := m.RegisterRepositoryOwnerHandle(owner, func() error {
		prepared = true
		if m.repositories.registerMu.TryLock() {
			m.repositories.registerMu.Unlock()
			return fmt.Errorf("registration resolved its handle outside the finalization order")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !prepared {
		t.Fatal("the prepare hook never ran; the order was not probed")
	}
	if handle == nil || handle.Owner() != owner {
		t.Fatalf("handle = %v, want the registration this call opened", handle)
	}
	if handle.state != m.repositories.byPrefix[owner.RepoPrefix] {
		t.Fatal("the handle names a registration this call did not open")
	}

	// Half two: finalization takes the same order, so it cannot free a prefix
	// while a registration is between its sections.
	drain, err := m.CloseRepositoryRegistration(handle)
	if err != nil {
		t.Fatal(err)
	}
	assertRepositoryDrain(t, drain.Done(), true)
	m.repositories.registerMu.Lock()
	finalized := make(chan error, 1)
	go func() { finalized <- m.FinalizeRepositoryCleanup(drain) }()
	select {
	case err := <-finalized:
		m.repositories.registerMu.Unlock()
		t.Fatalf("finalization ran outside the registration order: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	m.repositories.registerMu.Unlock()
	select {
	case err := <-finalized:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("finalization never completed after the order was released")
	}
	if m.repositories.byPrefix != nil {
		t.Fatal("finalization left the registration behind")
	}
}
