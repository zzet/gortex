package reconcile

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"

	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

// seedIntentOf gives a checkout one intent from a named source.
func seedIntentOf(f *fixture, checkoutID string, kind store_sqlite.IntentSourceKind, locator string) {
	f.t.Helper()
	err := f.catalog.UpsertTrackingIntent(context.Background(), store_sqlite.TrackingIntent{
		IntentID:      string(kind) + ":" + checkoutID,
		CheckoutID:    checkoutID,
		SourceKind:    kind,
		SourceLocator: locator,
		Active:        true,
	})
	if err != nil {
		f.t.Fatalf("seed %s intent: %v", kind, err)
	}
}

// TestRevokeTrackingIntentsWithdrawsWhatItMay covers the forget preflight:
// every explicit intent is withdrawn and stamped with the time it happened.
func TestRevokeTrackingIntentsWithdrawsWhatItMay(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFixture(t, Default())
		f.seedCheckout("co-1", "inc-1", "wt", store_sqlite.CheckoutModeDedicated)
		seedIntentOf(f, "co-1", store_sqlite.IntentSourceCLITrack, "gortex track /repo/wt")
		seedIntentOf(f, "co-1", store_sqlite.IntentSourceManualConfig, "/config.yaml")

		revocation, err := f.rec.RevokeTrackingIntents(context.Background(), "co-1")
		if err != nil {
			t.Fatalf("RevokeTrackingIntents: %v", err)
		}
		if revocation.IsBlocked() || len(revocation.Revoked) != 2 {
			t.Fatalf("revocation = %+v, want both intents withdrawn", revocation)
		}

		intents, err := f.catalog.ListTrackingIntents(context.Background(), "co-1")
		if err != nil {
			t.Fatalf("ListTrackingIntents: %v", err)
		}
		for _, intent := range intents {
			if intent.Active {
				t.Fatalf("intent %s is still active", intent.SourceKind)
			}
			if intent.RevokedAt != time.Now().Unix() {
				t.Fatalf("intent %s revoked at %d, want %d",
					intent.SourceKind, intent.RevokedAt, time.Now().Unix())
			}
		}
	})
}

// TestRevokeTrackingIntentsRefusesNonRevocable proves the preflight is
// all-or-nothing: an intent nobody may withdraw here blocks the forget, and
// the intents that could have been withdrawn are left untouched so the
// caller sees exactly the state it decided against.
func TestRevokeTrackingIntentsRefusesNonRevocable(t *testing.T) {
	f := newFixture(t, Default())
	f.seedCheckout("co-1", "inc-1", "wt", store_sqlite.CheckoutModeDedicated)
	seedIntentOf(f, "co-1", store_sqlite.IntentSourceCLITrack, "gortex track /repo/wt")
	seedIntentOf(f, "co-1", store_sqlite.IntentSourceProjectMembership, "project:web")

	revocation, err := f.rec.RevokeTrackingIntents(context.Background(), "co-1")
	if !errors.Is(err, ErrIntentNotRevocable) {
		t.Fatalf("RevokeTrackingIntents error = %v, want ErrIntentNotRevocable", err)
	}
	if len(revocation.Blocked) != 1 ||
		revocation.Blocked[0].SourceKind != store_sqlite.IntentSourceProjectMembership {
		t.Fatalf("blocked = %+v, want the project membership", revocation.Blocked)
	}

	intents, err := f.catalog.ListTrackingIntents(context.Background(), "co-1")
	if err != nil {
		t.Fatalf("ListTrackingIntents: %v", err)
	}
	for _, intent := range intents {
		if !intent.Active {
			t.Fatalf("a refused preflight withdrew %s anyway", intent.SourceKind)
		}
	}
}

// TestRetireCheckoutForgetsWhatTheFamilyCanStillServe covers the demotable
// shape: a reachable non-primary checkout of a family whose primary is ready
// stops being served. Today that reduces to forgetting it.
func TestRetireCheckoutForgetsWhatTheFamilyCanStillServe(t *testing.T) {
	f := newFixture(t, Default())
	f.seedCheckout("co-main", "inc-main", "@main", store_sqlite.CheckoutModeDedicated)
	f.seedOwnedGraph("graph-main", "co-main")
	f.promoteToPrimary("graph-main")
	f.seedCheckout("co-wt", "inc-wt", "wt", store_sqlite.CheckoutModeDedicated)
	f.seedOwnedGraph("graph-wt", "co-wt")
	f.seedIntent("co-wt")

	outcome, err := f.rec.RetireCheckout(context.Background(), "co-wt", "inc-wt", "reload")
	if err != nil {
		t.Fatalf("RetireCheckout: %v", err)
	}
	if outcome != OutcomeForgotten {
		t.Fatalf("outcome = %q, want %q", outcome, OutcomeForgotten)
	}
	f.assertNoCheckoutRows("co-wt")
	if f.graphExists("graph-wt") {
		t.Fatal("the retired checkout's graph row survives")
	}
	if !f.graphExists("graph-main") {
		t.Fatal("the family's primary must be untouched")
	}
}

// TestRetireCheckoutRecordsWhatItMayNotDelete covers the other half of the
// rule: the owner of the family's primary base has nothing to fall back to,
// so the request is recorded and every row stays exactly where it was.
func TestRetireCheckoutRecordsWhatItMayNotDelete(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFixture(t, Default())
		f.seedCheckout("co-main", "inc-main", "@main", store_sqlite.CheckoutModeDedicated)
		f.seedOwnedGraph("graph-main", "co-main")
		f.promoteToPrimary("graph-main")

		outcome, err := f.rec.RetireCheckout(context.Background(), "co-main", "inc-main", "reload")
		if err != nil {
			t.Fatalf("RetireCheckout: %v", err)
		}
		if outcome != OutcomeTransitionPending {
			t.Fatalf("outcome = %q, want %q", outcome, OutcomeTransitionPending)
		}
		if row := f.checkout("co-main"); row.Incarnation != "inc-main" {
			t.Fatalf("the checkout was disturbed: %+v", row)
		}
		if !f.graphExists("graph-main") {
			t.Fatal("the primary graph must survive a refused retirement")
		}

		transition, ok, err := f.catalog.GetIntentTransition(context.Background(), "co-main")
		if err != nil || !ok {
			t.Fatalf("GetIntentTransition = %v %v", ok, err)
		}
		if transition.State != store_sqlite.IntentTransitionPending ||
			transition.RequestedMode != store_sqlite.CheckoutModeAutomatic ||
			transition.CreatedAt != time.Now().Unix() {
			t.Fatalf("transition = %+v", transition)
		}

		// Asking again does not stack a second transition — the slot is
		// unique and the standing one already says what was asked.
		repeat, err := f.rec.RetireCheckout(context.Background(), "co-main", "inc-main", "reload")
		if err != nil || repeat != OutcomeTransitionPending {
			t.Fatalf("second RetireCheckout = %q, %v", repeat, err)
		}
	})
}

// TestRetireCheckoutGuardsOnIncarnation proves a caller holding the identity
// of a path that has since been re-keyed cannot retire what replaced it.
func TestRetireCheckoutGuardsOnIncarnation(t *testing.T) {
	f := newFixture(t, Default())
	f.seedCheckout("co-main", "inc-main", "@main", store_sqlite.CheckoutModeDedicated)
	f.seedOwnedGraph("graph-main", "co-main")
	f.promoteToPrimary("graph-main")
	f.seedCheckout("co-wt", "inc-wt", "wt", store_sqlite.CheckoutModeDedicated)
	f.rekey("co-wt", "inc-wt-2")

	if _, err := f.rec.RetireCheckout(context.Background(), "co-wt", "inc-wt", "reload"); !errors.Is(
		err, store_sqlite.ErrCatalogStaleGuard) {
		t.Fatalf("RetireCheckout error = %v, want a stale guard", err)
	}
	if row := f.checkout("co-wt"); row.Incarnation != "inc-wt-2" {
		t.Fatalf("a refused retirement changed the row: %+v", row)
	}

	// A checkout the catalog never held is not an error — there is simply
	// nothing to retire.
	outcome, err := f.rec.RetireCheckout(context.Background(), "co-gone", "inc-gone", "reload")
	if err != nil || outcome != OutcomeNoIdentity {
		t.Fatalf("RetireCheckout on a missing checkout = %q, %v", outcome, err)
	}
}

// TestDependentsEnumeratesWhatRidesOnAPrimary covers the preview an explicit
// forget shows: the rows that exist only because this checkout does.
func TestDependentsEnumeratesWhatRidesOnAPrimary(t *testing.T) {
	f := newFixture(t, Default())
	f.seedCheckout("co-main", "inc-main", "@main", store_sqlite.CheckoutModeDedicated)
	f.seedOwnedGraph("graph-main", "co-main")
	f.promoteToPrimary("graph-main")
	f.seedRoute("co-main", "graph-main")
	f.seedRefView("view-main", "graph-main")
	f.seedCheckout("co-auto", "inc-auto", "auto", store_sqlite.CheckoutModeAutomatic)

	dependents, err := f.rec.Dependents(context.Background(), "co-main")
	if err != nil {
		t.Fatalf("Dependents: %v", err)
	}
	seen := map[DependentKind]int{}
	for _, dep := range dependents {
		seen[dep.Kind]++
	}
	if seen[DependentCheckout] != 1 || seen[DependentRefView] != 1 || seen[DependentRoute] != 1 {
		t.Fatalf("dependents = %+v", dependents)
	}

	// A checkout with no graph of its own carries nothing.
	if deps, err := f.rec.Dependents(context.Background(), "co-auto"); err != nil || len(deps) != 0 {
		t.Fatalf("Dependents(co-auto) = %+v, %v", deps, err)
	}
}
