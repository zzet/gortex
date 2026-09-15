package indexer

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/graphview"
	"github.com/zzet/gortex/internal/reconcile"
)

// This is the completed producer/payload boundary, not a physical MI purge
// fixture. The Store, Catalog, public Reconciler retirement, ref/owner drains,
// local lifecycle finalizer, publisher runtime and same-owner retrack are real.
// No cleanup worker goroutine is started; the finalizer is driven explicitly.
type publisherFinalizationBoundaryHooks struct{}

func (*publisherFinalizationBoundaryHooks) PurgeCheckoutLayers(context.Context, string, string) error {
	return nil
}
func (*publisherFinalizationBoundaryHooks) ReleaseGraph(context.Context, string) error { return nil }

func publisherFinalizationFixture(t *testing.T) (*CheckoutLifecycle, *dedicatedBaseRuntime, store_sqlite.RepositoryCleanupIdentity, store_sqlite.DedicatedGraph) {
	t.Helper()
	l, identity := repositoryAdmissionFixture(t)
	r := &dedicatedBaseRuntime{}
	if err := l.SetDedicatedBaseCleanupRuntime(r); err != nil {
		t.Fatal(err)
	}
	l.mi = &MultiIndexer{}
	rec, err := reconcile.New(l.catalog, &publisherFinalizationBoundaryHooks{}, reconcile.Config{AvailabilityGrace: time.Second, RemovalGrace: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	l.rec = rec
	graph, found, err := l.catalog.GetDedicatedGraph(t.Context(), identity.GraphID)
	if err != nil || !found {
		t.Fatalf("healthy graph found=%v err=%v", found, err)
	}
	return l, r, identity, graph
}

func publisherFinalizationPrepare(t *testing.T, l *CheckoutLifecycle, identity store_sqlite.RepositoryCleanupIdentity) *repositoryCleanupState {
	t.Helper()
	got, owner, found, err := l.closeRepositoryAdmission(t.Context(), identity.GraphID)
	if err != nil || !found || got != identity {
		t.Fatalf("close identity=%+v found=%v err=%v", got, found, err)
	}
	publisher, err := l.closeRepositoryPublisher(identity, owner)
	if err != nil {
		t.Fatal(err)
	}
	awaitDedicatedDrain(t, publisher)
	drained := make(chan struct{})
	close(drained)
	state := &repositoryCleanupState{
		graphID: identity.GraphID, identity: identity, owner: owner,
		refs:      l.closeRepositoryRefViews(identity.RepoPrefix),
		lane:      &repositoryCleanupLane{owner: l.mi, prefix: identity.RepoPrefix, done: drained, finalized: true},
		publisher: publisher, initialized: true, producersDone: drained,
		attemptDone: drained, waitReady: drained, payloadPurged: true,
	}
	if err := l.rec.RetireDedicatedGraph(t.Context(), identity.GraphID); err != nil {
		t.Fatal(err)
	}
	if _, found, err := l.catalog.GetDedicatedGraph(t.Context(), identity.GraphID); err != nil || found {
		t.Fatalf("durable graph retirement found=%v err=%v", found, err)
	}
	if pending, err := l.rec.RepositoryCleanupPending(t.Context(), identity.GraphID, identity.CheckoutID, identity.Incarnation, identity.FamilyID); err != nil || pending {
		t.Fatalf("durable cleanup pending=%v err=%v", pending, err)
	}
	return state
}

func TestRepositoryFinalizationReclaimsPublisherAcrossSameOwnerRetracks(t *testing.T) {
	l, r, identity, graph := publisherFinalizationFixture(t)
	owner := store_sqlite.DedicatedBaseOwner{CheckoutID: identity.CheckoutID, Incarnation: identity.Incarnation}
	var prior *repositoryCleanupState
	for i := range 16 {
		if i != 0 {
			if err := l.catalog.UpsertDedicatedGraph(t.Context(), graph); err != nil {
				t.Fatal(err)
			}
		}
		if err := l.RegisterRepositoryOwner(t.Context(), identity.GraphID); err != nil {
			t.Fatal(err)
		}
		current := r.ownerAdmissions[identity.GraphID]
		if current == nil {
			t.Fatal("lifecycle did not register publisher owner")
		}
		state := publisherFinalizationPrepare(t, l, identity)
		if prior != nil {
			if done, err := l.finalizeRepositoryCleanupState(t.Context(), prior); err != nil || !done {
				t.Fatalf("old lifecycle retry done=%v err=%v", done, err)
			}
			if err := r.FinalizeDedicatedBaseOwner(identity.GraphID, owner, prior.publisher); !errors.Is(err, store_sqlite.ErrCatalogStaleGuard) {
				t.Fatalf("old runtime callback=%v", err)
			}
			if r.ownerAdmissions[identity.GraphID] != current {
				t.Fatal("old finalizer removed replacement closing slot")
			}
		}
		if done, err := l.finalizeRepositoryCleanupState(t.Context(), state); err != nil || !done {
			t.Fatalf("finalization %d done=%v err=%v", i, done, err)
		}
		if !state.finalized || len(r.ownerAdmissions) != 0 || len(l.repositoryOwners) != 0 || len(l.repositoryClosing) != 0 {
			t.Fatalf("completed cleanup retained owner state at %d", i)
		}
		prior = state
	}
}

func TestRepositoryFinalizationPreservesDirectPublisherReplacement(t *testing.T) {
	l, r, identity, _ := publisherFinalizationFixture(t)
	if err := l.RegisterRepositoryOwner(t.Context(), identity.GraphID); err != nil {
		t.Fatal(err)
	}
	state := publisherFinalizationPrepare(t, l, identity)
	owner := store_sqlite.DedicatedBaseOwner{CheckoutID: identity.CheckoutID, Incarnation: identity.Incarnation}
	// Simulate a separate privileged runtime caller replacing the local slot.
	// Normal lifecycle registration remains closed until its own finalization.
	if err := r.RegisterDedicatedBaseOwner(identity.GraphID, owner); err != nil {
		t.Fatal(err)
	}
	current := r.ownerAdmissions[identity.GraphID]
	next, err := r.CloseDedicatedBaseOwner(identity.GraphID, owner)
	if err != nil {
		t.Fatal(err)
	}
	awaitDedicatedDrain(t, next)
	if done, err := l.finalizeRepositoryCleanupState(t.Context(), state); done || !errors.Is(err, store_sqlite.ErrCatalogStaleGuard) {
		t.Fatalf("stale lifecycle capability done=%v err=%v", done, err)
	}
	if r.ownerAdmissions[identity.GraphID] != current || state.finalized || l.repositoryClosing[identity.GraphID] != state.owner || l.repositoryOwners[identity.GraphID] != repositoryCleanupOwner(identity) {
		t.Fatal("stale finalization discarded new slot or local tombstones")
	}
	if _, err := l.AcquireRepositoryRead(identity.GraphID); !errors.Is(err, graphview.ErrRepositoryAdmissionClosed) {
		t.Fatalf("failed finalizer reopened read admission: %v", err)
	}
}

func TestRepositoryFinalizationRetriesAfterPublisherSlotWasReclaimed(t *testing.T) {
	l, r, identity, _ := publisherFinalizationFixture(t)
	if err := l.RegisterRepositoryOwner(t.Context(), identity.GraphID); err != nil {
		t.Fatal(err)
	}
	read, err := l.AcquireRepositoryRead(identity.GraphID)
	if err != nil {
		t.Fatal(err)
	}
	defer read.Release()
	state := publisherFinalizationPrepare(t, l, identity)
	// Force a later finalization guard to refuse after the publisher's local
	// reclamation. This is a terminal-retry test, not permission to purge while
	// holding readers; the fixture contains no payload and starts after purge.
	if done, err := l.finalizeRepositoryCleanupState(t.Context(), state); done || !errors.Is(err, graphview.ErrRepositoryLeaseInUse) {
		t.Fatalf("held owner lease finalization done=%v err=%v", done, err)
	}
	if len(r.ownerAdmissions) != 0 || state.finalized || l.repositoryClosing[identity.GraphID] != state.owner {
		t.Fatal("partial finalization did not retain the retry boundary")
	}
	read.Release()
	if done, err := l.finalizeRepositoryCleanupState(t.Context(), state); err != nil || !done {
		t.Fatalf("finalizer retry done=%v err=%v", done, err)
	}
	if len(r.ownerAdmissions) != 0 || len(l.repositoryOwners) != 0 || len(l.repositoryClosing) != 0 {
		t.Fatal("retry retained completed owner state")
	}
}
