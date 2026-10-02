package indexer

import (
	"errors"
	"testing"
	"time"
)

// The lease's pre-write sample is the edit's own: it asks for the sampler as
// an urgent caller, so a background sample holding the lease gives it up at
// its next git boundary instead of making the edit wait for the whole of it.
func TestCheckoutMutationPrepareSamplesAsAnUrgentCaller(t *testing.T) {
	f, c, l := newCheckoutMutationFixture(t)
	m, err := l.BeginCheckoutMutation(t.Context(), f.checkoutID, f.worktree, f.route().RouteEpoch)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	release, err := c.sampler.Hold(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- m.Prepare(t.Context()) }()
	deadline := time.Now().Add(10 * time.Second)
	for c.sampler.UrgentWaiting() == 0 && time.Now().Before(deadline) {
		select {
		case err := <-done:
			release()
			t.Fatalf("Prepare returned while the lease was held: %v", err)
		default:
		}
		time.Sleep(time.Millisecond)
	}
	urgent := c.sampler.UrgentWaiting()
	release()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if urgent == 0 {
		t.Fatal("Prepare waited for the sampler as a background caller")
	}
}

// An admission against a route that moved after the caller selected it — a
// newer epoch, or layers that no longer compose over the current base — is
// refused as a moved route, which the caller may select again for. A HEAD
// change is not one: with usable HEAD file evidence admission samples
// nothing, and Prepare refuses it. Without that proof admission samples
// and may only establish a conservative stale HEAD-or-base refusal.
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
	wantErr := ErrCheckoutMutationRouteMoved
	if !fixture.coordinator.sampler.CaptureHeadEvidence().Usable() {
		wantErr = ErrCheckoutMutationStale
	}
	if m, err := fixture.lifecycle.BeginCheckoutMutation(t.Context(), fixture.family.checkoutID, fixture.family.worktree, fixture.route.RouteEpoch); m != nil || !errors.Is(err, wantErr) {
		if m != nil {
			m.Close()
		}
		t.Fatalf("an admission whose base moved = lease %v, error %v, want no lease and %v", m, err, wantErr)
	}
}
