package indexer

import (
	"errors"
	"testing"
)

// An admission against a route that moved after the caller selected it — a
// newer epoch, or layers that no longer compose over the current base — is
// refused as a moved route, which the caller may select again for. A HEAD
// change is not one: admission samples nothing, and Prepare refuses it.
func TestCheckoutMutationAdmissionNamesAMovedRoute(t *testing.T) {
	f, _, l := newCheckoutMutationFixture(t)
	route := f.route()
	if m, err := l.BeginCheckoutMutation(t.Context(), f.checkoutID, f.worktree, route.RouteEpoch+1); !errors.Is(err, ErrCheckoutMutationRouteMoved) || !errors.Is(err, ErrCheckoutMutationStale) {
		if m != nil {
			m.Close()
		}
		t.Fatalf("an admission against an older epoch = %v, want a moved route", err)
	}
	fixture := newPinnedBaseMutationFixture(t)
	if !fixture.coordinator.RequestBaseRelease(fixture.baseGeneration, "test release") {
		t.Fatal("release request was not accepted")
	}
	if m, err := fixture.lifecycle.BeginCheckoutMutation(t.Context(), fixture.family.checkoutID, fixture.family.worktree, fixture.route.RouteEpoch); !errors.Is(err, ErrCheckoutMutationRouteMoved) {
		if m != nil {
			m.Close()
		}
		t.Fatalf("an admission whose base moved = %v, want a moved route", err)
	}
}
