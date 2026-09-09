package graphview

import (
	"errors"
	"fmt"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
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
