package graphview

import (
	"errors"
	"fmt"
	"testing"
)

func TestRawProvisionalOwnerIsHiddenUntilExactInstallationCommits(t *testing.T) {
	m := NewLeaseManager()
	owner := RawRepositoryOwner{RepoPrefix: "raw", RootIdentity: "canonical/raw", Incarnation: "installation-one"}
	reg, err := m.PrepareRawRepositoryOwner(owner)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.LookupRawRepositoryRegistration(owner.RepoPrefix, owner.RootIdentity); !errors.Is(err, ErrRawRepositoryNotReady) {
		t.Fatalf("provisional lookup=%v", err)
	}
	if _, err := m.AcquireMixedRepositoryRead(nil, []*RawRepositoryRegistration{reg}); !errors.Is(err, ErrRawRepositoryNotReady) {
		t.Fatalf("provisional explicit read=%v", err)
	}
	if _, err := m.AcquireRepositoryRoster(); !errors.Is(err, ErrRawRepositoryNotReady) {
		t.Fatalf("roster silently omitted pending owner=%v", err)
	}
	if _, err := m.AcquireAllRepositoryReads(); !errors.Is(err, ErrRawRepositoryNotReady) {
		t.Fatalf("broad read exposed partial owner=%v", err)
	}
	if _, err := m.PrepareRawRepositoryOwner(owner); !errors.Is(err, ErrRepositoryOwnerConflict) {
		t.Fatalf("second actor borrowed reservation=%v", err)
	}
	if _, err := m.RegisterRawRepositoryOwnerPrepared(owner, nil); !errors.Is(err, ErrRepositoryOwnerConflict) {
		t.Fatalf("legacy ready path committed someone else's reservation=%v", err)
	}
	if err := m.CommitRawRepositoryOwner(reg); err != nil {
		t.Fatal(err)
	}
	selected, err := m.LookupRawRepositoryRegistration(owner.RepoPrefix, owner.RootIdentity)
	if err != nil || selected.state != reg.state {
		t.Fatalf("selected exact state=%v %v", selected, err)
	}
	lease, err := m.AcquireMixedRepositoryRead(nil, []*RawRepositoryRegistration{selected})
	if err != nil {
		t.Fatal(err)
	}
	lease.Release()
	if _, err := m.LookupRawRepositoryRegistration(owner.RepoPrefix, "other-root"); !errors.Is(err, ErrRepositoryOwnerUnknown) {
		t.Fatalf("prefix-only selection=%v", err)
	}
}

func TestRawProvisionalFailureDoesNotDeleteOrCommitReplacement(t *testing.T) {
	m := NewLeaseManager()
	owner := RawRepositoryOwner{RepoPrefix: "raw", RootIdentity: "canonical/raw", Incarnation: "same-tuple-control"}
	old, err := m.PrepareRawRepositoryOwner(owner)
	if err != nil {
		t.Fatal(err)
	}
	drain, err := m.CloseRawRepositoryAdmission(old)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.CommitRawRepositoryOwner(old); !errors.Is(err, ErrRepositoryAdmissionClosed) {
		t.Fatalf("commit after failure close=%v", err)
	}
	if err := m.FinalizeRepositoryCleanup(drain); err != nil {
		t.Fatal(err)
	}
	next, err := m.PrepareRawRepositoryOwner(owner)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.CommitRawRepositoryOwner(old); !errors.Is(err, ErrRepositoryOwnerUnknown) {
		t.Fatalf("old actor committed replacement=%v", err)
	}
	if err := m.FinalizeRepositoryCleanup(drain); err != nil {
		t.Fatal(err)
	}
	if err := m.CommitRawRepositoryOwner(next); err != nil {
		t.Fatal(err)
	}
	if _, err := m.LookupRawRepositoryRegistration(owner.RepoPrefix, owner.RootIdentity); err != nil {
		t.Fatal(err)
	}
}

func BenchmarkMixedOwnerSelection(b *testing.B) {
	for _, count := range []int{1, 10, 30} {
		b.Run(fmt.Sprint(count), func(b *testing.B) {
			m := NewLeaseManager()
			selected := make([]*RawRepositoryRegistration, count)
			for i := range selected {
				selected[i] = privateRawRegistration(b, m, fmt.Sprint("raw-", i), "current")
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				lease, err := m.AcquireMixedRepositoryRead(nil, selected)
				if err != nil {
					b.Fatal(err)
				}
				lease.Release()
			}
		})
	}
}
