package indexer

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// A reftable directory prevents the stat-based HEAD proof. Git still uses
// this fixture's configured files ref store, so its full sample stays valid.
func forceSampledMutationAdmission(t *testing.T, f *coordinatorFixture, c *CheckoutCoordinator) {
	t.Helper()
	if err := os.Mkdir(filepath.Join(f.primary, ".git", "reftable"), 0o755); err != nil {
		t.Fatal(err)
	}
	if c.sampler.CaptureHeadEvidence().Usable() {
		t.Fatal("fixture did not disable the stat-based HEAD proof")
	}
	if _, err := c.sampler.Sample(t.Context()); err != nil {
		t.Fatalf("the fallback git sample must remain usable: %v", err)
	}
}

func TestCheckoutMutationSampledAdmissionNamesPublishedBaseMotion(t *testing.T) {
	f := newUnpublishedCommittedBaseFixture(t)
	gate := NewViewBuildGate()
	gate.Open()
	c := f.coordinator(t, CheckoutCoordinatorConfig{Gate: gate, Debounce: time.Hour, debounceDemand: true})
	if out := c.reconcile(t.Context()); out.Err != nil {
		t.Fatal(out.Err)
	}
	route := f.route()
	forceSampledMutationAdmission(t, f.coordinatorFixture, c)
	f.publishBase(t)
	l := &CheckoutLifecycle{catalog: f.catalog, store: f.store, coordinators: map[string]*CheckoutCoordinator{f.checkoutID: c}}
	m, err := l.BeginCheckoutMutation(t.Context(), f.checkoutID, f.worktree, route.RouteEpoch)
	if m != nil {
		m.Close()
		t.Fatal("admitted a route built before committed-base publication")
	}
	if !errors.Is(err, ErrCheckoutMutationRouteMoved) || !errors.Is(err, ErrCheckoutMutationStale) {
		t.Fatalf("published base motion = %v, want retryable route motion", err)
	}
	if f.route() != route {
		t.Fatal("refused admission changed the route")
	}
}

func TestCheckoutMutationHeldCycleNamesBaseMotion(t *testing.T) {
	f := newPinnedBaseMutationFixture(t)
	if !f.coordinator.RequestBaseRelease(f.baseGeneration, "test release") {
		t.Fatal("release request was not accepted")
	}
	f.coordinator.cycleMu.Lock()
	defer f.coordinator.cycleMu.Unlock()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	m, err := f.lifecycle.BeginCheckoutMutation(ctx, f.family.checkoutID, f.family.worktree, f.route.RouteEpoch)
	if m != nil {
		m.Close()
		t.Fatal("admitted a mutation while its cycle lock was held")
	}
	if !errors.Is(err, ErrCheckoutMutationRouteMoved) || !errors.Is(err, ErrCheckoutMutationStale) {
		t.Fatalf("held-cycle base motion = %v, want retryable route motion", err)
	}
}

func TestCheckoutMutationSampledAdmissionKeepsHEADAndDiskMotionStale(t *testing.T) {
	for _, change := range []string{"HEAD before admission", "disk before admission", "HEAD after admission"} {
		t.Run(change, func(t *testing.T) {
			f, c, l := newCheckoutMutationFixture(t)
			route := f.route()
			forceSampledMutationAdmission(t, f, c)
			move := func() {
				builderWriteFile(t, f.worktree, "helper.go", "package fixture\n\nfunc ExternalChange() {}\n")
				if change != "disk before admission" {
					builderGit(t, f.worktree, "add", "-A")
					builderGit(t, f.worktree, "commit", "-m", "external HEAD motion")
				}
			}
			if change != "HEAD after admission" {
				move()
			}
			m, err := l.BeginCheckoutMutation(t.Context(), f.checkoutID, f.worktree, route.RouteEpoch)
			if change == "HEAD after admission" {
				if err != nil {
					t.Fatal(err)
				}
				move()
				err = m.Prepare(context.Background())
			}
			if m != nil {
				m.Close()
			}
			if !errors.Is(err, ErrCheckoutMutationStale) || errors.Is(err, ErrCheckoutMutationRouteMoved) {
				t.Fatalf("%s = %v, want non-retryable snapshot staleness", change, err)
			}
			if f.route() != route {
				t.Fatal("external change refusal changed the route")
			}
		})
	}
}
