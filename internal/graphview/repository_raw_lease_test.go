package graphview

import (
	"errors"
	"testing"
)

func privateRawRegistration(t testing.TB, m *LeaseManager, prefix, incarnation string) *RawRepositoryRegistration {
	t.Helper()
	r, err := m.RegisterRawRepositoryOwnerPrepared(RawRepositoryOwner{RepoPrefix: prefix, RootIdentity: "canonical-root/" + prefix, Incarnation: incarnation}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func privateRawStillDraining(t testing.TB, d *RepositoryDrain) {
	t.Helper()
	if d == nil || d.Done() == nil {
		t.Fatal("invalid drain")
	}
	select {
	case <-d.Done():
		t.Fatal("owner drained despite held lease")
	default:
	}
}

func privateRawDrained(t testing.TB, d *RepositoryDrain) {
	t.Helper()
	select {
	case <-d.Done():
	default:
		t.Fatal("owner did not synchronously drain")
	}
}

func TestRawRepositorySharesBroadLifetimeIncludingLateRegistration(t *testing.T) {
	m := NewLeaseManager()
	broad, err := m.AcquireAllRepositoryReads()
	if err != nil {
		t.Fatal(err)
	}
	defer broad.Release()
	raw := privateRawRegistration(t, m, "raw", "first")
	drain, err := m.CloseRawRepositoryAdmission(raw)
	if err != nil {
		t.Fatal(err)
	}
	privateRawStillDraining(t, drain)
	if err := m.FinalizeRepositoryCleanup(drain); !errors.Is(err, ErrRepositoryLeaseInUse) {
		t.Fatalf("premature cleanup=%v", err)
	}
	if _, err := m.AcquireAllRepositoryReads(); !errors.Is(err, ErrRepositoryAdmissionClosed) {
		t.Fatalf("broad read omitted closing raw owner: %v", err)
	}
	broad.Release()
	privateRawDrained(t, drain)
	if err := m.FinalizeRepositoryCleanup(drain); err != nil {
		t.Fatal(err)
	}
}

func TestMixedRepositoryAcquisitionIsAtomicAndReportsTypedOwners(t *testing.T) {
	m := NewLeaseManager()
	dedicated := RepositoryOwner{GraphID: "graph", CheckoutID: "checkout", Incarnation: "dedicated", RepoPrefix: "git"}
	if err := m.RegisterRepositoryOwner(dedicated); err != nil {
		t.Fatal(err)
	}
	raw := privateRawRegistration(t, m, "raw", "raw-incarnation")
	lease, err := m.AcquireMixedRepositoryRead([]RepositoryOwner{dedicated, dedicated}, []*RawRepositoryRegistration{raw, raw})
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	if owners := lease.Owners(); len(owners) != 1 || owners[0] != dedicated {
		t.Fatalf("dedicated owners=%+v", owners)
	}
	if owners := lease.RawOwners(); len(owners) != 1 || owners[0] != raw.Owner() {
		t.Fatalf("raw owners=%+v", owners)
	}
	rawDrain, err := m.CloseRawRepositoryAdmission(raw)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.AcquireMixedRepositoryRead([]RepositoryOwner{dedicated}, []*RawRepositoryRegistration{raw}); !errors.Is(err, ErrRepositoryAdmissionClosed) {
		t.Fatalf("closing raw scope accepted: %v", err)
	}
	dedicatedDrain, err := m.CloseRepositoryAdmission(dedicated)
	if err != nil {
		t.Fatal(err)
	}
	privateRawStillDraining(t, rawDrain)
	privateRawStillDraining(t, dedicatedDrain)
	lease.Release()
	privateRawDrained(t, rawDrain)
	privateRawDrained(t, dedicatedDrain)
	// The failed mixed acquisition must not have leaked a dedicated-only pin.
	if err := m.FinalizeRepositoryCleanup(rawDrain); err != nil {
		t.Fatal(err)
	}
	if err := m.FinalizeRepositoryCleanup(dedicatedDrain); err != nil {
		t.Fatal(err)
	}
}

func TestRawRepositoryCapabilityDoesNotCrossSameTupleRetrack(t *testing.T) {
	m := NewLeaseManager()
	old := privateRawRegistration(t, m, "raw", "same-strings")
	drain, err := m.CloseRawRepositoryAdmission(old)
	if err != nil {
		t.Fatal(err)
	}
	privateRawDrained(t, drain)
	if err := m.FinalizeRepositoryCleanup(drain); err != nil {
		t.Fatal(err)
	}
	current := privateRawRegistration(t, m, "raw", "same-strings")
	if _, err := m.AcquireMixedRepositoryRead(nil, []*RawRepositoryRegistration{old}); !errors.Is(err, ErrRepositoryOwnerUnknown) {
		t.Fatalf("old handle acquired replacement=%v", err)
	}
	if _, err := m.CloseRawRepositoryAdmission(old); !errors.Is(err, ErrRepositoryOwnerUnknown) {
		t.Fatalf("old handle closed replacement=%v", err)
	}
	if err := m.FinalizeRepositoryCleanup(drain); err != nil {
		t.Fatal(err)
	}
	lease, err := m.AcquireMixedRepositoryRead(nil, []*RawRepositoryRegistration{current})
	if err != nil {
		t.Fatal(err)
	}
	lease.Release()
}

func TestRawRepositoryPreparedFailureAndDedicatedConflictPreserveAdmission(t *testing.T) {
	m := NewLeaseManager()
	owner := RawRepositoryOwner{RepoPrefix: "raw", RootIdentity: "root", Incarnation: "incarnation"}
	failure := errors.New("preparation failed")
	if _, err := m.RegisterRawRepositoryOwnerPrepared(owner, func() error { return failure }); !errors.Is(err, failure) {
		t.Fatal(err)
	}
	roster, err := m.AcquireRepositoryRoster()
	if err != nil {
		t.Fatal(err)
	}
	if len(roster.RawRegistrations()) != 0 {
		t.Fatal("failed preparation leaked a registration")
	}
	roster.Release()
	raw, err := m.RegisterRawRepositoryOwnerPrepared(owner, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.RegisterRepositoryOwner(RepositoryOwner{GraphID: "graph", CheckoutID: "checkout", Incarnation: "dedicated", RepoPrefix: "raw"}); !errors.Is(err, ErrRepositoryOwnerConflict) {
		t.Fatalf("namespace collision=%v", err)
	}
	lease, err := m.AcquireMixedRepositoryRead(nil, []*RawRepositoryRegistration{raw})
	if err != nil {
		t.Fatal(err)
	}
	lease.Release()
}

func TestRepositoryRosterLeaseDetectsAdditionCloseAndRelease(t *testing.T) {
	m := NewLeaseManager()
	raw := privateRawRegistration(t, m, "raw", "one")
	roster, err := m.AcquireRepositoryRoster()
	if err != nil {
		t.Fatal(err)
	}
	defer roster.Release()
	if err := roster.ValidateCurrent(); err != nil {
		t.Fatal(err)
	}
	other := privateRawRegistration(t, m, "second", "two")
	if err := roster.ValidateCurrent(); !errors.Is(err, ErrRepositoryOwnerConflict) {
		t.Fatalf("addition not observed=%v", err)
	}
	otherDrain, err := m.CloseRawRepositoryAdmission(other)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.FinalizeRepositoryCleanup(otherDrain); err != nil {
		t.Fatal(err)
	}
	if err := roster.ValidateCurrent(); err != nil {
		t.Fatal("same captured registration set should compare current after an unused add/remove")
	}
	drain, err := m.CloseRawRepositoryAdmission(raw)
	if err != nil {
		t.Fatal(err)
	}
	if err := roster.ValidateCurrent(); !errors.Is(err, ErrRepositoryAdmissionClosed) {
		t.Fatalf("closing raw owner accepted=%v", err)
	}
	privateRawStillDraining(t, drain)
	roster.Release()
	privateRawDrained(t, drain)
	if err := roster.ValidateCurrent(); !errors.Is(err, ErrRepositoryAdmissionClosed) {
		t.Fatalf("released capture still admitted=%v", err)
	}
}

func TestRawRepositoryShutdownPermanentlySharesAdmissionDomain(t *testing.T) {
	m := NewLeaseManager()
	raw := privateRawRegistration(t, m, "raw", "one")
	lease, err := m.AcquireMixedRepositoryRead(nil, []*RawRepositoryRegistration{raw})
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	done := m.ShutdownRepositoryAdmissions()
	select {
	case <-done:
		t.Fatal("shutdown ignored raw pin")
	default:
	}
	if _, err := m.RegisterRawRepositoryOwnerPrepared(RawRepositoryOwner{RepoPrefix: "another", RootIdentity: "another", Incarnation: "new"}, nil); !errors.Is(err, ErrRepositoryAdmissionsStopped) {
		t.Fatalf("shutdown allowed raw registration=%v", err)
	}
	lease.Release()
	select {
	case <-done:
	default:
		t.Fatal("raw release did not finish shutdown")
	}
	drain, err := m.CloseRawRepositoryAdmission(raw)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.FinalizeRepositoryCleanup(drain); err != nil {
		t.Fatal(err)
	}
	if _, err := m.AcquireRepositoryRoster(); !errors.Is(err, ErrRepositoryAdmissionsStopped) {
		t.Fatalf("finalization reopened shutdown=%v", err)
	}
}
