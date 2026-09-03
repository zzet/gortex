package store_sqlite

import (
	"context"
	"errors"
	"testing"
	"time"
)

// The catalog's two pools, from the caller's side.
//
// Every catalog write serialises on the store's mutation gate, which a long
// pass holds for as long as its transactions run. Two properties follow, and
// both are load-bearing for anything that has to answer inside a deadline: a
// write with a budget must be able to stop waiting for its turn, and the reads
// that can answer without a turn must not take one.

// TestCatalogWritesGiveUpOnASaturatedWriter pins the first. A deadline that
// bounded only the statement bounded nothing at all: the queue in front of it
// is a whole build, and the gate was taken before the context was ever
// consulted, so a caller that budgeted milliseconds waited minutes.
func TestCatalogWritesGiveUpOnASaturatedWriter(t *testing.T) {
	store := openCatalogStore(t)
	catalog := store.Catalog()
	seedRefView(t, catalog, "rv-1", "graph-1")

	release, err := store.HoldWriteGate(context.Background())
	if err != nil {
		t.Fatalf("HoldWriteGate: %v", err)
	}
	defer release()

	// UpdateRefViewDesire is one statement (exec); GetOrCreateRefView is a
	// transaction (withTx). Both take the gate, so both have to yield.
	budget, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	err = catalog.UpdateRefViewDesire(budget, UpdateRefViewDesireRequest{
		RefViewID: "rv-1", DesiredRef: "refs/heads/rv-1", DesiredCommit: "c1",
		DesiredTree: "t1", DesiredBuildFingerprint: "fp-1",
		State: RefViewBuilding, LastResolved: 100, LastSelected: 100,
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("single-statement write against a held gate = %v, want a deadline", err)
	}

	budget, cancel = context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err = catalog.GetOrCreateRefView(budget, RefView{
		RefViewID: "rv-2", GraphID: "graph-1", SelectorKind: "git_ref",
		SelectorValue: "refs/heads/rv-2", EnrichmentProfile: "default",
		State: RefViewPending, ExactView: true,
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("transactional write against a held gate = %v, want a deadline", err)
	}

	// The gate, not the database, was the obstacle: releasing it lets the same
	// write through.
	release()
	err = catalog.UpdateRefViewDesire(context.Background(), UpdateRefViewDesireRequest{
		RefViewID: "rv-1", DesiredRef: "refs/heads/rv-1", DesiredCommit: "c1",
		DesiredTree: "t1", DesiredBuildFingerprint: "fp-1",
		State: RefViewBuilding, LastResolved: 100, LastSelected: 100,
	})
	if err != nil {
		t.Fatalf("write once the gate was free: %v", err)
	}
}

// TestCatalogInFlightRefViewBuildReadsThroughASaturatedWriter pins the second.
// The claim a running build holds is what every later selection of that tree
// coalesces onto, and asking for it through the claim itself means asking the
// writer the build has saturated. Read off the read pool, the same row answers
// with the gate held.
func TestCatalogInFlightRefViewBuildReadsThroughASaturatedWriter(t *testing.T) {
	store := openCatalogStore(t)
	ctx := context.Background()
	catalog := store.Catalog()
	seedRefView(t, catalog, "rv-1", "graph-1")

	build := RefViewBuild{
		BuildID: "build-1", RefViewID: "rv-1", DesiredRef: "refs/heads/rv-1",
		DesiredCommit: "c1", DesiredTree: "t1", BaseGenerationID: 0,
		EnrichmentProfile: "default", BuildFingerprint: "fp-1",
		CapturedRouteEpoch: 1, State: ViewGenerationBuilding,
		BuildToken: "token-1", CreatedAt: 100, LastProgress: 100,
	}
	if _, err := catalog.ClaimRefViewBuild(ctx, build, 0); err != nil {
		t.Fatalf("ClaimRefViewBuild: %v", err)
	}
	key := RefViewBuildKey{
		RefViewID: "rv-1", DesiredTree: "t1",
		BaseGenerationID: 0, BuildFingerprint: "fp-1",
	}

	release, err := store.HoldWriteGate(context.Background())
	if err != nil {
		t.Fatalf("HoldWriteGate: %v", err)
	}
	defer release()

	held, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	inFlight, found, err := catalog.InFlightRefViewBuild(held, key, 0)
	if err != nil || !found {
		t.Fatalf("InFlightRefViewBuild through a held gate = %+v, %v, %v", inFlight, found, err)
	}
	if inFlight.BuildToken != "token-1" {
		t.Fatalf("read the slot as %+v, want the claimed attempt", inFlight)
	}
	if held.Err() != nil {
		t.Fatalf("the read used its whole context (%v), so it queued on the writer", held.Err())
	}
	release()

	// A slot nobody claims is not in flight, and neither is one whose claim
	// stopped reporting: the liveness cutoff here is the one the claim applies,
	// so the read and the claim cannot disagree about which builds are alive.
	other := key
	other.DesiredTree = "t2"
	if _, found, err := catalog.InFlightRefViewBuild(ctx, other, 0); err != nil || found {
		t.Fatalf("InFlightRefViewBuild on an unclaimed slot = %v, %v, want not found", found, err)
	}
	if _, found, err := catalog.InFlightRefViewBuild(ctx, key, 500); err != nil || found {
		t.Fatalf("InFlightRefViewBuild past the liveness cutoff = %v, %v, want not in flight", found, err)
	}
	if _, found, err := catalog.InFlightRefViewBuild(ctx, key, 100); err != nil || !found {
		t.Fatalf("InFlightRefViewBuild at its own stamp = %v, %v, want in flight", found, err)
	}

	// A finished attempt has released the slot, exactly as the coalescing
	// index sees it.
	err = catalog.CompleteRefViewBuild(ctx, CompleteRefViewBuildRequest{
		BuildID: "build-1", BuildToken: "token-1", State: ViewGenerationReady,
		GenerationID: 7, LastProgress: 200,
	})
	if err != nil {
		t.Fatalf("CompleteRefViewBuild: %v", err)
	}
	if _, found, err := catalog.InFlightRefViewBuild(ctx, key, 0); err != nil || found {
		t.Fatalf("InFlightRefViewBuild on a finished attempt = %v, %v, want not in flight", found, err)
	}
}
