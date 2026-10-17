package indexer

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

type pinnedBaseMutationFixture struct {
	family         *committedBaseFixture
	coordinator    *CheckoutCoordinator
	lifecycle      *CheckoutLifecycle
	route          store_sqlite.CheckoutRoute
	baseGeneration int64
}

func newPinnedBaseMutationFixture(t *testing.T) pinnedBaseMutationFixture {
	t.Helper()
	family := newCommittedBaseFixture(t)
	gate := NewViewBuildGate()
	gate.Open()
	coordinator := family.coordinator(t, CheckoutCoordinatorConfig{
		Gate:     gate,
		Debounce: time.Hour,
	})
	ctx := context.Background()
	out := coordinator.reconcile(ctx)
	if out.Err != nil || out.CommitGenerationID == 0 || out.DirtyGenerationID == 0 {
		t.Fatalf("initial dependent reconcile: %+v", out)
	}
	route := family.route()
	commit, found := family.generation(route.CommitGenerationID)
	if !found || commit.BaseGenerationID <= 0 {
		t.Fatalf("routed commit does not name a committed base: %+v", commit)
	}
	baseGeneration := commit.BaseGenerationID
	advanced, _ := family.advanceCommittedBase(t, "advanced.go", "Advanced")
	current, err := coordinator.primaryBase(ctx)
	if err != nil {
		t.Fatalf("primaryBase after advance: %v", err)
	}
	if current.generationID != advanced || current.generationID == baseGeneration {
		t.Fatalf("primary base did not advance: old=%d published=%d current=%d", baseGeneration, advanced, current.generationID)
	}
	if after := family.route(); after != route {
		t.Fatalf("primary advance moved the dependent route: before=%+v after=%+v", route, after)
	}
	lifecycle := &CheckoutLifecycle{
		catalog:      family.catalog,
		store:        family.store,
		coordinators: map[string]*CheckoutCoordinator{family.checkoutID: coordinator},
	}
	return pinnedBaseMutationFixture{
		family:         family,
		coordinator:    coordinator,
		lifecycle:      lifecycle,
		route:          route,
		baseGeneration: baseGeneration,
	}
}

func TestCheckoutMutationAcceptsRoutedPinnedBaseAfterPrimaryAdvance(t *testing.T) {
	fixture := newPinnedBaseMutationFixture(t)
	mutation, err := fixture.lifecycle.BeginCheckoutMutation(
		t.Context(), fixture.family.checkoutID, fixture.family.worktree, fixture.route.RouteEpoch,
	)
	if err != nil {
		t.Fatalf("admit mutation over routed pinned base: %v", err)
	}
	defer mutation.Close()
	if err := mutation.Prepare(t.Context()); err != nil {
		t.Fatalf("prepare mutation over routed pinned base: %v", err)
	}
}

func TestCheckoutMutationRejectsReleasedOrUnservablePinnedBase(t *testing.T) {
	t.Run("released", func(t *testing.T) {
		fixture := newPinnedBaseMutationFixture(t)
		if !fixture.coordinator.RequestBaseRelease(fixture.baseGeneration, "test release") {
			t.Fatal("release request was not accepted")
		}
		mutation, err := fixture.lifecycle.BeginCheckoutMutation(
			t.Context(), fixture.family.checkoutID, fixture.family.worktree, fixture.route.RouteEpoch,
		)
		if mutation != nil {
			mutation.Close()
		}
		if !errors.Is(err, ErrCheckoutMutationStale) {
			t.Fatalf("released pin admission error = %v, want ErrCheckoutMutationStale", err)
		}
	})

	t.Run("unservable", func(t *testing.T) {
		fixture := newPinnedBaseMutationFixture(t)
		undo := doctorGenerationColumn(
			t, fixture.family.storePath, fixture.baseGeneration,
			"state", string(store_sqlite.ViewGenerationFailed),
		)
		defer undo()
		mutation, err := fixture.lifecycle.BeginCheckoutMutation(
			t.Context(), fixture.family.checkoutID, fixture.family.worktree, fixture.route.RouteEpoch,
		)
		if mutation != nil {
			mutation.Close()
		}
		if !errors.Is(err, ErrCheckoutMutationStale) {
			t.Fatalf("unservable pin admission error = %v, want ErrCheckoutMutationStale", err)
		}
	})
}
