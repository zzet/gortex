package graphview

import (
	"errors"
	"sync"
	"testing"
	"time"
)

func closeIdentityOwner() RepositoryOwner {
	return RepositoryOwner{GraphID: "graph", CheckoutID: "checkout", Incarnation: "incarnation", RepoPrefix: "repo"}
}

func closeIdentityRegister(t *testing.T, m *LeaseManager, owner RepositoryOwner) *RepositoryRegistration {
	t.Helper()
	registration, err := m.RegisterRepositoryOwnerHandle(owner, nil)
	if err != nil {
		t.Fatalf("RegisterRepositoryOwnerHandle(%+v): %v", owner, err)
	}
	if registration.Owner() != owner {
		t.Fatalf("registration owner=%+v want %+v", registration.Owner(), owner)
	}
	return registration
}

func closeIdentityDrained(t *testing.T, drain *RepositoryDrain) {
	t.Helper()
	select {
	case <-drain.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("closed registration never drained")
	}
}

// TestDedicatedRepositoryCloseDoesNotCrossSameTupleRetrack is the dedicated
// half of TestRawRepositoryCapabilityDoesNotCrossSameTupleRetrack: after a
// registration is finalized and an identical owner tuple is registered again,
// the retained handle addresses only the object it captured. Gate 7 — a
// delayed callback must not damage a replacement owner at a reused identity.
func TestDedicatedRepositoryCloseDoesNotCrossSameTupleRetrack(t *testing.T) {
	m := NewLeaseManager()
	owner := closeIdentityOwner()
	old := closeIdentityRegister(t, m, owner)
	drain, err := m.CloseRepositoryRegistration(old)
	if err != nil {
		t.Fatal(err)
	}
	closeIdentityDrained(t, drain)
	if err := m.FinalizeRepositoryCleanup(drain); err != nil {
		t.Fatal(err)
	}
	current := closeIdentityRegister(t, m, owner)
	if current == old || current.state == old.state {
		t.Fatal("retrack reused the finalized registration object")
	}
	if _, err := m.CloseRepositoryRegistration(old); !errors.Is(err, ErrRepositoryOwnerUnknown) {
		t.Fatalf("stale handle closed the replacement: %v", err)
	}
	// The stale cleanup capability is equally powerless, and re-finalizing it
	// must not evict the replacement from the registry either.
	if _, err := m.CloseRepositoryRegistration(drain.Registration()); !errors.Is(err, ErrRepositoryOwnerUnknown) {
		t.Fatalf("stale drain handle closed the replacement: %v", err)
	}
	if err := m.FinalizeRepositoryCleanup(drain); err != nil {
		t.Fatalf("idempotent finalize of the captured registration: %v", err)
	}
	read, err := m.AcquireRepositoryRead(owner)
	if err != nil {
		t.Fatalf("replacement registration did not survive the stale close: %v", err)
	}
	read.Release()
	replacementDrain, err := m.CloseRepositoryRegistration(current)
	if err != nil {
		t.Fatalf("replacement handle could not close its own registration: %v", err)
	}
	if replacementDrain == drain {
		t.Fatal("replacement close returned the stale cleanup capability")
	}
	closeIdentityDrained(t, replacementDrain)
}

// TestDedicatedRepositoryCloseDrainsAndFinalizesOnlyWhatItCaptured pins the
// lifetime half: the captured registration keeps its tombstone until its own
// readers let go, a replacement cannot be registered over that tombstone, and
// finalization releases exactly the captured object.
func TestDedicatedRepositoryCloseDrainsAndFinalizesOnlyWhatItCaptured(t *testing.T) {
	m := NewLeaseManager()
	owner := closeIdentityOwner()
	registration := closeIdentityRegister(t, m, owner)
	read, err := m.AcquireRepositoryRead(owner)
	if err != nil {
		t.Fatal(err)
	}
	drain, err := m.CloseRepositoryRegistration(registration)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-drain.Done():
		t.Fatal("captured registration drained while its reader was live")
	default:
	}
	if _, err := m.RegisterRepositoryOwnerHandle(owner, nil); !errors.Is(err, ErrRepositoryAdmissionClosed) {
		t.Fatalf("closing tombstone was reopened before finalization: %v", err)
	}
	if err := m.FinalizeRepositoryCleanup(drain); !errors.Is(err, ErrRepositoryLeaseInUse) {
		t.Fatalf("finalize passed a pinned registration: %v", err)
	}
	read.Release()
	closeIdentityDrained(t, drain)
	if err := m.FinalizeRepositoryCleanup(drain); err != nil {
		t.Fatal(err)
	}
	if _, err := m.CloseRepositoryRegistration(registration); !errors.Is(err, ErrRepositoryOwnerUnknown) {
		t.Fatalf("finalized registration still closeable: %v", err)
	}
}

// TestDedicatedRepositoryCloseRegistrationIsIdempotentAndRefusesForeignHandles
// covers the double-close and invalid-handle contract the cleanup saga relies
// on: re-closing the same registration returns the same cleanup capability, and
// a nil, foreign-manager or raw handle is refused rather than resolved.
func TestDedicatedRepositoryCloseRegistrationIsIdempotentAndRefusesForeignHandles(t *testing.T) {
	m := NewLeaseManager()
	owner := closeIdentityOwner()
	registration := closeIdentityRegister(t, m, owner)
	first, err := m.CloseRepositoryRegistration(registration)
	if err != nil {
		t.Fatal(err)
	}
	second, err := m.CloseRepositoryRegistration(registration)
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatalf("double close minted a second cleanup capability: %p %p", first, second)
	}
	if third, err := m.CloseRepositoryRegistration(first.Registration()); err != nil || third != first {
		t.Fatalf("drain-addressed re-close: drain=%p err=%v", third, err)
	}
	closeIdentityDrained(t, first)
	if _, err := m.CloseRepositoryRegistration(nil); !errors.Is(err, ErrRepositoryOwnerInvalid) {
		t.Fatalf("nil handle: %v", err)
	}
	other := NewLeaseManager()
	if _, err := other.CloseRepositoryRegistration(registration); !errors.Is(err, ErrRepositoryOwnerInvalid) {
		t.Fatalf("foreign manager accepted a handle it never minted: %v", err)
	}
	raw, err := m.RegisterRawRepositoryOwnerPrepared(RawRepositoryOwner{RepoPrefix: "raw", RootIdentity: "root", Incarnation: "raw-incarnation"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	rawDrain, err := m.CloseRawRepositoryAdmission(raw)
	if err != nil {
		t.Fatal(err)
	}
	if handle := rawDrain.Registration(); handle != nil {
		t.Fatalf("raw cleanup capability minted a dedicated handle: %+v", handle.Owner())
	}
	if _, err := m.CloseRepositoryRegistration(rawDrain.Registration()); !errors.Is(err, ErrRepositoryOwnerInvalid) {
		t.Fatalf("raw registration closed through the dedicated path: %v", err)
	}
}

// TestDedicatedRepositoryCloseRegistrationUnderConcurrentReaders is the race
// arm: concurrent closes of one handle, concurrent explicit and broad readers,
// and drain notification delivery must agree on a single cleanup capability.
func TestDedicatedRepositoryCloseRegistrationUnderConcurrentReaders(t *testing.T) {
	m := NewLeaseManager()
	owner := closeIdentityOwner()
	registration := closeIdentityRegister(t, m, owner)
	const workers = 8
	drains := make([]*RepositoryDrain, workers)
	errs := make([]error, workers)
	var group sync.WaitGroup
	start := make(chan struct{})
	for i := range workers {
		group.Add(1)
		go func() {
			defer group.Done()
			<-start
			drains[i], errs[i] = m.CloseRepositoryRegistration(registration)
		}()
		group.Add(1)
		go func() {
			defer group.Done()
			<-start
			if read, err := m.AcquireRepositoryRead(owner); err == nil {
				read.Release()
			} else if !errors.Is(err, ErrRepositoryAdmissionClosed) {
				t.Errorf("explicit acquisition: %v", err)
			}
			if broad, err := m.AcquireAllRepositoryReads(); err == nil {
				broad.Release()
			} else if !errors.Is(err, ErrRepositoryAdmissionClosed) {
				t.Errorf("broad acquisition: %v", err)
			}
		}()
	}
	close(start)
	group.Wait()
	for i := range workers {
		if errs[i] != nil {
			t.Fatalf("close %d: %v", i, errs[i])
		}
		if drains[i] != drains[0] {
			t.Fatalf("close %d returned a different cleanup capability", i)
		}
	}
	closeIdentityDrained(t, drains[0])
	if err := m.FinalizeRepositoryCleanup(drains[0]); err != nil {
		t.Fatal(err)
	}
}
