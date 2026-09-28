package indexer

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/graphview"
)

func TestIdleCheckoutDecision(t *testing.T) {
	now := time.Now()
	week := 7 * 24 * time.Hour
	automatic := store_sqlite.Checkout{CheckoutID: "c", EffectiveMode: store_sqlite.CheckoutModeAutomatic}
	idle := idleCheckoutInput{Checkout: automatic, LastUse: now.Add(-8 * 24 * time.Hour)}
	cases := []struct {
		name    string
		mutate  func(*idleCheckoutInput)
		after   time.Duration
		release bool
		reason  string
	}{
		{"idle past the limit", nil, week, true, idleReleaseIdle},
		{"disabled", nil, 0, false, idleKeepDisabled},
		{"primary", func(in *idleCheckoutInput) { in.Primary = true }, week, false, idleKeepPrimary},
		{"dedicated", func(in *idleCheckoutInput) { in.Checkout.EffectiveMode = store_sqlite.CheckoutModeDedicated }, week, false, idleKeepNotAutomatic},
		{"activating", func(in *idleCheckoutInput) { in.Busy = true }, week, false, idleKeepBusy},
		{"uncommitted edit within the limit", func(in *idleCheckoutInput) { in.NewestUncommitted = now.Add(-6 * 24 * time.Hour) }, week, false, idleKeepRecentEdits},
		{"uncommitted edit older than the limit", func(in *idleCheckoutInput) { in.NewestUncommitted = now.Add(-9 * 24 * time.Hour) }, week, true, idleReleaseIdle},
		{"used within the limit", func(in *idleCheckoutInput) { in.LastUse = now.Add(-6 * 24 * time.Hour) }, week, false, idleKeepInUse},
	}
	for _, tc := range cases {
		in := idle
		if tc.mutate != nil {
			tc.mutate(&in)
		}
		release, reason := decideIdleCheckout(in, tc.after, now)
		if release != tc.release || reason != tc.reason {
			t.Errorf("%s: release=%t reason=%s, want %t %s", tc.name, release, reason, tc.release, tc.reason)
		}
	}
}

// idleLifecycle is a lifecycle over f whose clock stands `ahead` in the
// future, with c as the checkout's live coordinator.
func idleLifecycle(t *testing.T, f *coordinatorFixture, c *CheckoutCoordinator, ahead time.Duration) *CheckoutLifecycle {
	t.Helper()
	l := newGenerationRetirementLifecycle(f.store, time.Now())
	l.leases = f.leases
	future := time.Now().Add(ahead)
	l.now = func() time.Time { return future }
	l.coordinators = map[string]*CheckoutCoordinator{f.checkoutID: c}
	return l
}

// An automatic checkout idle past the limit loses its layers but keeps its
// registration and its route record; a request gets view_building (an
// honest answer, never the released layers); opening it rebuilds the layers
// over the same registration.
func TestIdleCheckoutReleaseKeepsTheRouteAndRebuildsOnOpen(t *testing.T) {
	f := newCoordinatorFixture(t)
	c := chainCoordinatorOn(t, f, CheckoutCoordinatorConfig{})
	builderWriteFile(t, f.worktree, "helper.go", "package fixture\n\nfunc Helper() {\n\t_ = 1\n}\n")
	before := coordinatorReconcile(t, c)
	if before.CommitGenerationID <= 0 {
		t.Fatalf("fixture: no commit layer: %+v", before)
	}
	// The uncommitted edit is older than the limit.
	old := time.Now().Add(-9 * 24 * time.Hour)
	if err := os.Chtimes(filepath.Join(f.worktree, "helper.go"), old, old); err != nil {
		t.Fatal(err)
	}
	l := idleLifecycle(t, f, c, 8*24*time.Hour)
	released, err := l.ReleaseIdleCheckouts(context.Background())
	if err != nil || !slices.Equal(released, []string{f.checkoutID}) {
		t.Fatalf("released %v (%v), want the idle automatic checkout only", released, err)
	}
	route := f.route()
	if route.State != store_sqlite.RoutePending || route.CommitGenerationID != 0 || route.DirtyGenerationID != 0 {
		t.Fatalf("the released route is %+v, want pending with both slots cleared", route)
	}
	if _, found, _ := f.catalog.GetCheckout(context.Background(), f.checkoutID); !found {
		t.Fatal("the release dropped the checkout's registration")
	}
	if !slices.Contains(l.owedGenerations(), before.CommitGenerationID) {
		t.Fatalf("the released commit layer %d is not owed to the sweep", before.CommitGenerationID)
	}
	materializer := &graphview.Materializer{Store: f.store, Catalog: f.catalog, Leases: f.leases}
	if _, err := materializer.MaterializeCheckout(context.Background(), f.checkoutID); graphview.CodeOf(err) != graphview.CodeViewBuilding {
		t.Fatalf("a request of the released checkout: %v, want view_building", err)
	}
	// Opening it: a new coordinator over the same registration rebuilds.
	reopened := chainCoordinatorOn(t, f, CheckoutCoordinatorConfig{})
	after := coordinatorReconcile(t, reopened)
	if after.CommitGenerationID <= 0 || f.route().State != store_sqlite.RouteActive {
		t.Fatalf("the reopened checkout did not rebuild its layers: %+v route %+v", after, f.route())
	}
	chainAssertFlat(t, f, "idle-reopened")
}

// A checkout with an uncommitted file modified within the limit is kept,
// whatever its last selection; the primary is never released.
func TestIdleCheckoutReleaseKeepsRecentEditsAndThePrimary(t *testing.T) {
	f := newCoordinatorFixture(t)
	c := chainCoordinatorOn(t, f, CheckoutCoordinatorConfig{})
	builderWriteFile(t, f.worktree, "helper.go", "package fixture\n\nfunc Helper() {\n\t_ = 1\n}\n")
	coordinatorReconcile(t, c)
	ahead := 8 * 24 * time.Hour
	recent := time.Now().Add(ahead - 24*time.Hour) // one day before the lifecycle's now
	if err := os.Chtimes(filepath.Join(f.worktree, "helper.go"), recent, recent); err != nil {
		t.Fatal(err)
	}
	l := idleLifecycle(t, f, c, ahead)
	released, err := l.ReleaseIdleCheckouts(context.Background())
	if err != nil || len(released) != 0 {
		t.Fatalf("released %v (%v), want nothing", released, err)
	}
	if route := f.route(); route.State != store_sqlite.RouteActive || route.CommitGenerationID <= 0 {
		t.Fatalf("the route was touched: %+v", route)
	}
}

// owedGenerations lists what the lifecycle owes the sweep.
func (l *CheckoutLifecycle) owedGenerations() []int64 {
	l.coordMu.Lock()
	defer l.coordMu.Unlock()
	out := make([]int64, 0, len(l.owed))
	for id := range l.owed {
		out = append(out, id)
	}
	return out
}
