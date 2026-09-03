package reconcile

import (
	"context"
	"errors"
	"testing"

	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

func assertTrackingIntentsActive(t *testing.T, catalog *store_sqlite.Catalog, checkoutID string, want bool) {
	t.Helper()
	intents, err := catalog.ListTrackingIntents(context.Background(), checkoutID)
	if err != nil {
		t.Fatalf("ListTrackingIntents: %v", err)
	}
	if len(intents) == 0 {
		t.Fatal("checkout has no tracking intents to assert")
	}
	for _, intent := range intents {
		if intent.Active != want {
			t.Fatalf("intent %s active = %v, want %v", intent.IntentID, intent.Active, want)
		}
	}
}

// TestExplicitForgetAuthorizationSurvivesRestart proves the authorization
// transaction is the durable point of no return. A hook failure happens only
// after intent revocation and the cleanup journal committed; a fresh reconciler
// can finish that exact operation without a second preview or authorization.
func TestExplicitForgetAuthorizationSurvivesRestart(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, Default())
	seedForgettableCheckout(f, "co-1", "inc-1", "wt", "graph-1")
	f.hooks.failPurge = 1

	revocation, err := f.rec.ForgetCheckoutExplicit(
		ctx, "co-1", "inc-1", f.familyID, "graph-1")
	if !errors.Is(err, errHookFailed) {
		t.Fatalf("ForgetCheckoutExplicit = %v, want hook failure", err)
	}
	if len(revocation.Revoked) != 1 || len(revocation.Blocked) != 0 {
		t.Fatalf("revocation = %+v", revocation)
	}
	assertTrackingIntentsActive(t, f.catalog, "co-1", false)
	entries := f.journal()
	if len(entries) != 1 || entries[0].Phase != store_sqlite.CleanupPhaseFailed {
		t.Fatalf("durable cleanup after failure = %+v", entries)
	}

	fresh, err := New(f.catalog, f.hooks, Default())
	if err != nil {
		t.Fatalf("New fresh reconciler: %v", err)
	}
	if err := fresh.Resume(ctx); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	f.assertNoCheckoutRows("co-1")
	if f.graphExists("graph-1") {
		t.Fatal("recovered explicit forget left its graph")
	}
	if entries := f.journal(); len(entries) != 0 {
		t.Fatalf("recovered explicit forget left journal entries: %+v", entries)
	}
	if got := f.hooks.countPrefix("purge:"); got != 2 {
		t.Fatalf("purge attempts = %d, want the failed call and recovered call", got)
	}
	if got := f.hooks.countPrefix("release:"); got != 1 {
		t.Fatalf("release calls = %d, want 1", got)
	}
}

// TestExplicitForgetBlockedIntentWritesNothing pins the preflight boundary: a
// project-membership intent blocks the revocable CLI intent, the cleanup
// journal, and every teardown side effect as one set.
func TestExplicitForgetBlockedIntentWritesNothing(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, Default())
	seedForgettableCheckout(f, "co-1", "inc-1", "wt", "graph-1")
	if err := f.catalog.UpsertTrackingIntent(ctx, store_sqlite.TrackingIntent{
		IntentID:      "intent-project",
		CheckoutID:    "co-1",
		SourceKind:    store_sqlite.IntentSourceProjectMembership,
		SourceLocator: "project:test",
		Active:        true,
		CreatedAt:     101,
	}); err != nil {
		t.Fatalf("seed project intent: %v", err)
	}

	revocation, err := f.rec.ForgetCheckoutExplicit(
		ctx, "co-1", "inc-1", f.familyID, "graph-1")
	if !errors.Is(err, ErrIntentNotRevocable) {
		t.Fatalf("ForgetCheckoutExplicit = %v, want ErrIntentNotRevocable", err)
	}
	if len(revocation.Revoked) != 0 || len(revocation.Blocked) != 1 ||
		revocation.Blocked[0].SourceKind != store_sqlite.IntentSourceProjectMembership {
		t.Fatalf("blocked revocation = %+v", revocation)
	}
	assertTrackingIntentsActive(t, f.catalog, "co-1", true)
	f.checkout("co-1")
	if !f.graphExists("graph-1") {
		t.Fatal("blocked explicit forget removed its graph")
	}
	if entries := f.journal(); len(entries) != 0 {
		t.Fatalf("blocked explicit forget wrote journal entries: %+v", entries)
	}
	if calls := f.hooks.snapshot(); len(calls) != 0 {
		t.Fatalf("blocked explicit forget called hooks: %v", calls)
	}
}

// TestExplicitPrimaryStaleEpochKeepsIntentAndGraph covers the original split-
// transaction failure: losing the primary CAS must not leave an explicitly
// tracked checkout with its intent revoked.
func TestExplicitPrimaryStaleEpochKeepsIntentAndGraph(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, Default())
	seedForgettableCheckout(f, "co-primary", "inc-p", "primary", "graph-primary")
	epoch := f.promoteToPrimary("graph-primary")

	revocation, err := f.rec.RetirePrimaryClosureExplicit(
		ctx, "graph-primary", "co-primary", "inc-p", f.familyID, epoch-1)
	if !errors.Is(err, store_sqlite.ErrCatalogStaleGuard) {
		t.Fatalf("RetirePrimaryClosureExplicit = %v, want ErrCatalogStaleGuard", err)
	}
	if len(revocation.Revoked) != 0 || len(revocation.Blocked) != 0 {
		t.Fatalf("stale revocation = %+v", revocation)
	}
	assertTrackingIntentsActive(t, f.catalog, "co-primary", true)
	f.checkout("co-primary")
	if !f.graphExists("graph-primary") {
		t.Fatal("stale explicit retirement removed the primary graph")
	}
	if entries := f.journal(); len(entries) != 0 {
		t.Fatalf("stale explicit retirement wrote journal entries: %+v", entries)
	}
}

// TestExplicitForgetCancellationWritesNothing proves a cancellation before the
// authorization transaction cannot create the partial state restart recovery
// is intended to handle.
func TestExplicitForgetCancellationWritesNothing(t *testing.T) {
	f := newFixture(t, Default())
	seedForgettableCheckout(f, "co-1", "inc-1", "wt", "graph-1")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := f.rec.ForgetCheckoutExplicit(ctx, "co-1", "inc-1", f.familyID, "graph-1")
	if err == nil {
		t.Fatal("cancelled ForgetCheckoutExplicit unexpectedly succeeded")
	}
	assertTrackingIntentsActive(t, f.catalog, "co-1", true)
	f.checkout("co-1")
	if entries := f.journal(); len(entries) != 0 {
		t.Fatalf("cancelled explicit forget wrote journal entries: %+v", entries)
	}
	if calls := f.hooks.snapshot(); len(calls) != 0 {
		t.Fatalf("cancelled explicit forget called hooks: %v", calls)
	}
}
