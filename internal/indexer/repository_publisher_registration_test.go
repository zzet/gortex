package indexer

import (
	"context"
	"errors"
	"testing"

	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/graphview"
)

func TestLifecycleRegistrationRequiresReadyBeforePublisherBinding(t *testing.T) {
	lifecycle, identity := repositoryAdmissionFixture(t)
	runtime := &repositoryAdmissionPublisher{}
	if err := lifecycle.SetDedicatedBaseCleanupRuntime(runtime); err != nil {
		t.Fatal(err)
	}
	graph, found, err := lifecycle.catalog.GetDedicatedGraph(context.Background(), identity.GraphID)
	if err != nil || !found {
		t.Fatalf("graph=%v %v", found, err)
	}
	graph.State = "unavailable"
	if err := lifecycle.catalog.UpsertDedicatedGraph(context.Background(), graph); err != nil {
		t.Fatal(err)
	}
	if err := lifecycle.RegisterRepositoryOwner(context.Background(), identity.GraphID); !errors.Is(err, graphview.ErrRepositoryOwnerUnknown) {
		t.Fatalf("nonready graph registered: %v", err)
	}
	if len(runtime.registered) != 0 {
		t.Fatal("nonready catalog bound publisher slot")
	}
}

func TestLifecycleLeaseConflictDoesNotReopenPublisherSlot(t *testing.T) {
	lifecycle, identity := repositoryAdmissionFixture(t)
	runtime := &dedicatedBaseRuntime{}
	if err := lifecycle.SetDedicatedBaseCleanupRuntime(runtime); err != nil {
		t.Fatal(err)
	}
	owner := store_sqlite.DedicatedBaseOwner{CheckoutID: identity.CheckoutID, Incarnation: identity.Incarnation}
	if err := runtime.RegisterDedicatedBaseOwner(identity.GraphID, owner); err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.CloseDedicatedBaseOwner(identity.GraphID, owner); err != nil {
		t.Fatal(err)
	}
	oldSlot := runtime.ownerAdmissions[identity.GraphID]
	conflict := repositoryCleanupOwner(identity)
	conflict.Incarnation = "conflicting-lease-owner"
	if err := lifecycle.leases.RegisterRepositoryOwner(conflict); err != nil {
		t.Fatal(err)
	}
	if err := lifecycle.RegisterRepositoryOwner(context.Background(), identity.GraphID); !errors.Is(err, graphview.ErrRepositoryOwnerConflict) {
		t.Fatalf("lease conflict=%v", err)
	}
	if runtime.ownerAdmissions[identity.GraphID] != oldSlot || !oldSlot.closing {
		t.Fatal("lease registration failure already reopened publisher capability")
	}
}

func TestLifecycleOldCleanupHandleCannotCloseSameOwnerReplacementPublisher(t *testing.T) {
	lifecycle, identity := repositoryAdmissionFixture(t)
	ctx := context.Background()
	runtime := &dedicatedBaseRuntime{}
	if err := lifecycle.SetDedicatedBaseCleanupRuntime(runtime); err != nil {
		t.Fatal(err)
	}
	if err := lifecycle.RegisterRepositoryOwner(ctx, identity.GraphID); err != nil {
		t.Fatal(err)
	}
	_, oldDrain, found, err := lifecycle.closeRepositoryAdmission(ctx, identity.GraphID)
	if err != nil || !found {
		t.Fatalf("close=%v %v", found, err)
	}
	if _, err := lifecycle.closeRepositoryPublisher(identity, oldDrain); err != nil {
		t.Fatal(err)
	}
	oldSlot := runtime.ownerAdmissions[identity.GraphID]
	// Complete the isolated registry boundary with no MI or payload: this test
	// targets a retained cleanup callback, not the full public saga pipeline.
	if err := lifecycle.catalog.DeleteDedicatedGraph(ctx, identity.GraphID); err != nil {
		t.Fatal(err)
	}
	lifecycle.repositoryAdmissionMu.Lock()
	if err := lifecycle.leases.FinalizeRepositoryCleanup(oldDrain); err != nil {
		lifecycle.repositoryAdmissionMu.Unlock()
		t.Fatal(err)
	}
	delete(lifecycle.repositoryOwners, identity.GraphID)
	delete(lifecycle.repositoryClosing, identity.GraphID)
	lifecycle.repositoryAdmissionMu.Unlock()
	graph := store_sqlite.DedicatedGraph{GraphID: identity.GraphID, OwnerCheckoutID: identity.CheckoutID, RepoPrefix: identity.RepoPrefix, FamilyID: identity.FamilyID, State: "ready"}
	if err := lifecycle.catalog.UpsertDedicatedGraph(ctx, graph); err != nil {
		t.Fatal(err)
	}
	if err := lifecycle.RegisterRepositoryOwner(ctx, identity.GraphID); err != nil {
		t.Fatal(err)
	}
	newSlot := runtime.ownerAdmissions[identity.GraphID]
	if newSlot == oldSlot || newSlot.closing {
		t.Fatal("authorized same-owner registration did not replace drained slot")
	}
	if _, err := lifecycle.closeRepositoryPublisher(identity, oldDrain); !errors.Is(err, graphview.ErrRepositoryDrainInvalid) {
		t.Fatalf("stale cleanup callback accepted: %v", err)
	}
	if newSlot.closing {
		t.Fatal("old same-owner cleanup closed replacement publisher")
	}
	owner := store_sqlite.DedicatedBaseOwner{CheckoutID: identity.CheckoutID, Incarnation: identity.Incarnation}
	release, _, err := runtime.admitOwnerState(ctx, identity.GraphID, owner, newSlot)
	if err != nil {
		t.Fatalf("replacement publisher not usable: %v", err)
	}
	release()
}
