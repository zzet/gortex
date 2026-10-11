package indexer

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

func TestDedicatedBaseRuntimeFinalizeRequiresExactClosedDrain(t *testing.T) {
	r := &dedicatedBaseRuntime{}
	owner := dedicatedDrainOwner()
	if err := r.RegisterDedicatedBaseOwner("graph", owner); err != nil {
		t.Fatal(err)
	}
	release, slot, err := r.admitOwnerState(t.Context(), "graph", owner, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if err := r.FinalizeDedicatedBaseOwner("graph", owner, slot.drained); !errors.Is(err, errDedicatedBaseRuntimeClosed) {
		t.Fatalf("open owner was finalized: %v", err)
	}
	drained, err := r.CloseDedicatedBaseOwner("graph", owner)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.FinalizeDedicatedBaseOwner("graph", owner, drained); !errors.Is(err, errDedicatedBaseRuntimeClosed) {
		t.Fatalf("active owner was finalized: %v", err)
	}
	release()
	awaitDedicatedDrain(t, drained)
	wrongOwner := owner
	wrongOwner.Incarnation = "other"
	if err := r.FinalizeDedicatedBaseOwner("graph", wrongOwner, drained); !errors.Is(err, store_sqlite.ErrCatalogStaleGuard) {
		t.Fatalf("wrong owner=%v", err)
	}
	forged := make(chan struct{})
	close(forged)
	if err := r.FinalizeDedicatedBaseOwner("graph", owner, forged); !errors.Is(err, store_sqlite.ErrCatalogStaleGuard) {
		t.Fatalf("wrong drain=%v", err)
	}
	if r.ownerAdmissions["graph"] != slot {
		t.Fatal("refused finalization removed the slot")
	}
	if err := r.FinalizeDedicatedBaseOwner("graph", owner, nil); !errors.Is(err, errDedicatedBaseRuntimeInput) {
		t.Fatalf("nil drain=%v", err)
	}
	if err := r.FinalizeDedicatedBaseOwner("graph", owner, drained); err != nil {
		t.Fatal(err)
	}
	if err := r.FinalizeDedicatedBaseOwner("graph", owner, drained); err != nil {
		t.Fatalf("retry=%v", err)
	}
	if len(r.ownerAdmissions) != 0 || r.admittedActors != 0 {
		t.Fatal("completed owner was retained")
	}
}

func TestDedicatedBaseRuntimeFinalizeCannotDeleteSameOwnerReplacement(t *testing.T) {
	r := &dedicatedBaseRuntime{}
	owner := dedicatedDrainOwner()
	if err := r.RegisterDedicatedBaseOwner("graph", owner); err != nil {
		t.Fatal(err)
	}
	old, err := r.CloseDedicatedBaseOwner("graph", owner)
	if err != nil {
		t.Fatal(err)
	}
	awaitDedicatedDrain(t, old)
	if err := r.FinalizeDedicatedBaseOwner("graph", owner, old); err != nil {
		t.Fatal(err)
	}
	if err := r.RegisterDedicatedBaseOwner("graph", owner); err != nil {
		t.Fatal(err)
	}
	current := r.ownerAdmissions["graph"]
	next, err := r.CloseDedicatedBaseOwner("graph", owner)
	if err != nil {
		t.Fatal(err)
	}
	awaitDedicatedDrain(t, next)
	if next == old {
		t.Fatal("replacement reused the old drain capability")
	}
	if err := r.FinalizeDedicatedBaseOwner("graph", owner, old); !errors.Is(err, store_sqlite.ErrCatalogStaleGuard) {
		t.Fatalf("old finalizer acted on new closing owner: %v", err)
	}
	if r.ownerAdmissions["graph"] != current {
		t.Fatal("new closing slot was removed")
	}
	if err := r.FinalizeDedicatedBaseOwner("graph", owner, next); err != nil {
		t.Fatal(err)
	}
}

func TestDedicatedBaseRuntimeFinalizeKeepsHeldPublisherRevoked(t *testing.T) {
	b, request, claim := privateClaimedDedicatedFixture(t)
	r := &dedicatedBaseRuntime{store: b.Store}
	graphID, owner := claim.Desire.Authority.GraphID, claim.Desire.Authority.Owner
	if err := r.RegisterDedicatedBaseOwner(graphID, owner); err != nil {
		t.Fatal(err)
	}
	release, old, err := r.admitOwnerState(t.Context(), graphID, owner, nil)
	if err != nil {
		t.Fatal(err)
	}
	release()
	held := &dedicatedBasePublisher{runtime: r, authority: claim.Desire.Authority, admission: old}
	drained, err := r.CloseDedicatedBaseOwner(graphID, owner)
	if err != nil {
		t.Fatal(err)
	}
	awaitDedicatedDrain(t, drained)
	if err := r.FinalizeDedicatedBaseOwner(graphID, owner, drained); err != nil {
		t.Fatal(err)
	}
	check, err := installDedicatedWriteAudit(t.Context(), b.Store, request.StorePath)
	if err != nil {
		t.Fatal(err)
	}
	assertRevoked := func(label string) {
		t.Helper()
		_, err := held.ensureInitial(t.Context(), func(context.Context) (dedicatedBaseObservation, error) {
			t.Fatal("revoked publisher invoked observer")
			return dedicatedBaseObservation{}, nil
		})
		if !errors.Is(err, errDedicatedBaseRuntimeClosed) {
			t.Fatalf("%s: %v", label, err)
		}
	}
	assertRevoked("missing slot")
	if len(r.ownerAdmissions) != 0 {
		t.Fatal("held publisher recreated a missing slot")
	}
	if err := r.RegisterDedicatedBaseOwner(graphID, owner); err != nil {
		t.Fatal(err)
	}
	assertRevoked("same-owner retrack")
	current := r.ownerAdmissions[graphID]
	if current == old {
		t.Fatal("old publisher slot was revived")
	}
	if err := check(); err != nil {
		t.Fatalf("revoked publisher changed catalog: %v", err)
	}
	// A fresh capability can still enter the replacement owner, with no SQL.
	allowed, got, err := r.admitOwnerState(t.Context(), graphID, owner, current)
	if err != nil || got != current {
		t.Fatalf("replacement admission: same=%v err=%v", got == current, err)
	}
	allowed()
}

func TestDedicatedBaseRuntimeFinalizeReclaimsSlotsWithoutReopeningShutdown(t *testing.T) {
	r := &dedicatedBaseRuntime{}
	owner := dedicatedDrainOwner()
	for i := range 128 {
		id := fmt.Sprintf("graph-%d", i)
		if err := r.RegisterDedicatedBaseOwner(id, owner); err != nil {
			t.Fatal(err)
		}
		drained, err := r.CloseDedicatedBaseOwner(id, owner)
		if err != nil {
			t.Fatal(err)
		}
		awaitDedicatedDrain(t, drained)
		if err := r.FinalizeDedicatedBaseOwner(id, owner, drained); err != nil {
			t.Fatal(err)
		}
		if len(r.ownerAdmissions) != 0 {
			t.Fatalf("retained slot at %d", i)
		}
	}
	if err := r.RegisterDedicatedBaseOwner("last", owner); err != nil {
		t.Fatal(err)
	}
	all := r.CloseDedicatedBaseAdmission()
	awaitDedicatedDrain(t, all)
	last, err := r.CloseDedicatedBaseOwner("last", owner)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.FinalizeDedicatedBaseOwner("last", owner, last); err != nil {
		t.Fatal(err)
	}
	if !r.admissionClosed || r.CloseDedicatedBaseAdmission() != all {
		t.Fatal("slot cleanup reset shutdown")
	}
	if err := r.RegisterDedicatedBaseOwner("last", owner); !errors.Is(err, errDedicatedBaseRuntimeClosed) {
		t.Fatalf("registration after shutdown=%v", err)
	}
	if _, err := r.admitOwner(t.Context(), "new", owner); !errors.Is(err, errDedicatedBaseRuntimeClosed) {
		t.Fatalf("admission after shutdown=%v", err)
	}
	if len(r.ownerAdmissions) != 0 {
		t.Fatal("shutdown refusal created a slot")
	}
}

func TestDedicatedBaseRuntimeFinalizeRacesSameOwnerRegistration(t *testing.T) {
	owner := dedicatedDrainOwner()
	for range 64 {
		r := &dedicatedBaseRuntime{}
		if err := r.RegisterDedicatedBaseOwner("graph", owner); err != nil {
			t.Fatal(err)
		}
		old, err := r.CloseDedicatedBaseOwner("graph", owner)
		if err != nil {
			t.Fatal(err)
		}
		awaitDedicatedDrain(t, old)
		start := make(chan struct{})
		var done sync.WaitGroup
		var finishErr, registerErr error
		done.Add(2)
		go func() { defer done.Done(); <-start; finishErr = r.FinalizeDedicatedBaseOwner("graph", owner, old) }()
		go func() { defer done.Done(); <-start; registerErr = r.RegisterDedicatedBaseOwner("graph", owner) }()
		close(start)
		done.Wait()
		if registerErr != nil || (finishErr != nil && !errors.Is(finishErr, store_sqlite.ErrCatalogStaleGuard)) {
			t.Fatalf("register=%v finalize=%v", registerErr, finishErr)
		}
		current := r.ownerAdmissions["graph"]
		if current == nil || current.closing || current.drained == old {
			t.Fatal("finalizer erased or reused replacement slot")
		}
	}
}

func BenchmarkDedicatedBaseRuntimeFinalizeOwnerCycle(b *testing.B) {
	r := &dedicatedBaseRuntime{}
	owner := dedicatedDrainOwner()
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if err := r.RegisterDedicatedBaseOwner("graph", owner); err != nil {
			b.Fatal(err)
		}
		drained, err := r.CloseDedicatedBaseOwner("graph", owner)
		if err != nil {
			b.Fatal(err)
		}
		if err := r.FinalizeDedicatedBaseOwner("graph", owner, drained); err != nil {
			b.Fatal(err)
		}
	}
	b.StopTimer()
	if len(r.ownerAdmissions) != 0 || r.admittedActors != 0 {
		b.Fatal("owner cycle retained admission state")
	}
}
