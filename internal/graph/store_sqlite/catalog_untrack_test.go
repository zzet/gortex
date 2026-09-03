package store_sqlite

import (
	"context"
	"errors"
	"testing"
)

type catalogUntrackFixture struct {
	t       *testing.T
	catalog *Catalog

	familyID           string
	primaryCheckoutID  string
	primaryIncarnation string
	primaryGraphID     string
	targetCheckoutID   string
	targetIncarnation  string
	targetGraphID      string
}

func newCatalogUntrackFixture(t *testing.T) *catalogUntrackFixture {
	t.Helper()
	catalog := openCatalogStore(t).Catalog()
	f := &catalogUntrackFixture{
		t:                  t,
		catalog:            catalog,
		familyID:           "family-untrack",
		primaryCheckoutID:  "checkout-primary",
		primaryIncarnation: "incarnation-primary",
		primaryGraphID:     "graph-primary",
		targetCheckoutID:   "checkout-target",
		targetIncarnation:  "incarnation-target",
		targetGraphID:      "graph-target",
	}
	seedFamilyAndCheckout(t, catalog, f.familyID, f.primaryCheckoutID, f.primaryIncarnation)
	seedFamilyAndCheckout(t, catalog, f.familyID, f.targetCheckoutID, f.targetIncarnation)
	ctx := context.Background()
	for _, checkout := range []struct {
		id          string
		incarnation string
	}{
		{f.primaryCheckoutID, f.primaryIncarnation},
		{f.targetCheckoutID, f.targetIncarnation},
	} {
		if err := catalog.UpdateCheckoutState(ctx, UpdateCheckoutStateRequest{
			CheckoutID:    checkout.id,
			Incarnation:   checkout.incarnation,
			State:         CheckoutStateReady,
			DesiredMode:   CheckoutModeDedicated,
			EffectiveMode: CheckoutModeDedicated,
			LastSeen:      101,
		}); err != nil {
			t.Fatalf("make %s dedicated: %v", checkout.id, err)
		}
	}
	for _, graph := range []DedicatedGraph{
		{
			GraphID: f.primaryGraphID, OwnerCheckoutID: f.primaryCheckoutID,
			RepoPrefix: "primary", FamilyID: f.familyID, IsPrimaryBase: true,
			State: "graph_ready",
		},
		{
			GraphID: f.targetGraphID, OwnerCheckoutID: f.targetCheckoutID,
			RepoPrefix: "target", FamilyID: f.familyID, State: "graph_ready",
		},
	} {
		if err := catalog.UpsertDedicatedGraph(ctx, graph); err != nil {
			t.Fatalf("seed graph %s: %v", graph.GraphID, err)
		}
	}
	return f
}

func (f *catalogUntrackFixture) seedIntent(id, checkoutID string, kind IntentSourceKind) {
	f.t.Helper()
	if err := f.catalog.UpsertTrackingIntent(context.Background(), TrackingIntent{
		IntentID:      id,
		CheckoutID:    checkoutID,
		SourceKind:    kind,
		SourceLocator: "test:" + id,
		Active:        true,
		CreatedAt:     10,
	}); err != nil {
		f.t.Fatalf("seed intent %s: %v", id, err)
	}
}

func (f *catalogUntrackFixture) assertIntentsActive(checkoutID string, want bool) {
	f.t.Helper()
	intents, err := f.catalog.ListTrackingIntents(context.Background(), checkoutID)
	if err != nil {
		f.t.Fatalf("ListTrackingIntents: %v", err)
	}
	if len(intents) == 0 {
		f.t.Fatal("checkout has no intents to assert")
	}
	for _, intent := range intents {
		if intent.Active != want {
			f.t.Fatalf("intent %s active = %v, want %v", intent.IntentID, intent.Active, want)
		}
	}
}

func pendingCleanup(id, reason string, epoch int64) CleanupEntry {
	return CleanupEntry{
		CleanupID:       id,
		OpaqueTargetIDs: `{"kind":"test"}`,
		Reason:          reason,
		Phase:           CleanupPhasePending,
		PrimaryEpoch:    epoch,
		LastProgress:    20,
	}
}

func (f *catalogUntrackFixture) primaryAuthorization(cleanup CleanupEntry, epoch int64) AuthorizeUntrackRequest {
	return AuthorizeUntrackRequest{
		Plan:                 UntrackAuthorizationPrimaryClosure,
		CheckoutID:           f.primaryCheckoutID,
		Incarnation:          f.primaryIncarnation,
		FamilyID:             f.familyID,
		OwnedGraphID:         f.primaryGraphID,
		PrimaryGraphID:       f.primaryGraphID,
		ExpectedPrimaryEpoch: epoch,
		RevokedAt:            30,
		RevocableKinds:       []IntentSourceKind{IntentSourceCLITrack},
		Cleanup:              &cleanup,
	}
}

func (f *catalogUntrackFixture) forgetAuthorization(cleanup CleanupEntry) AuthorizeUntrackRequest {
	return AuthorizeUntrackRequest{
		Plan:           UntrackAuthorizationForget,
		CheckoutID:     f.targetCheckoutID,
		Incarnation:    f.targetIncarnation,
		FamilyID:       f.familyID,
		OwnedGraphID:   f.targetGraphID,
		RevokedAt:      30,
		RevocableKinds: []IntentSourceKind{IntentSourceCLITrack, IntentSourceMCPTrack},
		Cleanup:        &cleanup,
	}
}

func (f *catalogUntrackFixture) demotionAuthorization() AuthorizeUntrackRequest {
	transition := IntentTransition{
		TransitionID:       "transition-demote",
		CheckoutID:         f.targetCheckoutID,
		Cause:              "explicit_untrack_demote",
		PriorDesiredMode:   CheckoutModeDedicated,
		PriorEffectiveMode: CheckoutModeDedicated,
		RequestedMode:      CheckoutModeAutomatic,
		PriorCheckoutState: CheckoutStateReady,
		SourceSnapshotHash: f.targetGraphID + ":" + f.primaryGraphID + ":0",
		State:              IntentTransitionRunning,
		CreatedAt:          30,
		LastProgress:       30,
	}
	return AuthorizeUntrackRequest{
		Plan:                 UntrackAuthorizationDemote,
		CheckoutID:           f.targetCheckoutID,
		Incarnation:          f.targetIncarnation,
		FamilyID:             f.familyID,
		OwnedGraphID:         f.targetGraphID,
		PrimaryGraphID:       f.primaryGraphID,
		ExpectedPrimaryEpoch: 0,
		RequiredPrimaryState: "graph_ready",
		RevokedAt:            30,
		RevocableKinds:       []IntentSourceKind{IntentSourceCLITrack, IntentSourceMCPTrack},
		Transition:           &transition,
	}
}

func (f *catalogUntrackFixture) demotionCommit(
	transitionID string, epoch int64, cleanup CleanupEntry,
) CommitAuthorizedDemotionRequest {
	return CommitAuthorizedDemotionRequest{
		CheckoutID:           f.targetCheckoutID,
		Incarnation:          f.targetIncarnation,
		FamilyID:             f.familyID,
		TransitionID:         transitionID,
		OwnedGraphID:         f.targetGraphID,
		PrimaryGraphID:       f.primaryGraphID,
		ExpectedPrimaryEpoch: epoch,
		RequiredPrimaryState: "graph_ready",
		State:                CheckoutStateReady,
		LastSeen:             40,
		Cleanup:              &cleanup,
	}
}

func TestAuthorizePrimaryClosureIsAtomic(t *testing.T) {
	t.Run("stale epoch writes nothing", func(t *testing.T) {
		f := newCatalogUntrackFixture(t)
		f.seedIntent("intent-cli", f.primaryCheckoutID, IntentSourceCLITrack)
		ctx := context.Background()
		if err := f.catalog.SetPrimaryDedicatedGraph(ctx, SetPrimaryDedicatedGraphRequest{
			FamilyID:             f.familyID,
			GraphID:              f.primaryGraphID,
			ExpectedPrimaryEpoch: 0,
			LastSeen:             31,
		}); err != nil {
			t.Fatalf("advance primary epoch: %v", err)
		}
		cleanup := pendingCleanup("cleanup-primary-stale", "retire_primary_closure", 0)

		_, err := f.catalog.AuthorizeUntrack(ctx, f.primaryAuthorization(cleanup, 0))
		if !errors.Is(err, ErrCatalogStaleGuard) {
			t.Fatalf("AuthorizeUntrack = %v, want ErrCatalogStaleGuard", err)
		}
		f.assertIntentsActive(f.primaryCheckoutID, true)
		if _, found, err := f.catalog.GetCleanupEntry(context.Background(), cleanup.CleanupID); err != nil || found {
			t.Fatalf("stale authorization cleanup = found %v, err %v", found, err)
		}
	})

	t.Run("success revokes and journals together", func(t *testing.T) {
		f := newCatalogUntrackFixture(t)
		f.seedIntent("intent-cli", f.primaryCheckoutID, IntentSourceCLITrack)
		cleanup := pendingCleanup("cleanup-primary-success", "retire_primary_closure", 0)

		result, err := f.catalog.AuthorizeUntrack(context.Background(), f.primaryAuthorization(cleanup, 0))
		if err != nil {
			t.Fatalf("AuthorizeUntrack: %v", err)
		}
		if len(result.Revoked) != 1 || result.Cleanup == nil || result.Existing {
			t.Fatalf("authorization result = %+v", result)
		}
		f.assertIntentsActive(f.primaryCheckoutID, false)
		stored, found, err := f.catalog.GetCleanupEntry(context.Background(), cleanup.CleanupID)
		if err != nil || !found {
			t.Fatalf("GetCleanupEntry = %+v, %v, %v", stored, found, err)
		}
		if stored.PrimaryEpoch != 0 || stored.Reason != cleanup.Reason {
			t.Fatalf("stored cleanup = %+v", stored)
		}
	})

	t.Run("existing cleanup must match the preview epoch", func(t *testing.T) {
		f := newCatalogUntrackFixture(t)
		f.seedIntent("intent-cli", f.primaryCheckoutID, IntentSourceCLITrack)
		stored := pendingCleanup("cleanup-primary-existing", "retire_primary_closure", 9)
		if err := f.catalog.UpsertCleanupEntry(context.Background(), stored); err != nil {
			t.Fatalf("seed cleanup: %v", err)
		}
		requested := pendingCleanup(stored.CleanupID, stored.Reason, 0)

		_, err := f.catalog.AuthorizeUntrack(context.Background(), f.primaryAuthorization(requested, 0))
		if !errors.Is(err, ErrCatalogStaleGuard) {
			t.Fatalf("AuthorizeUntrack = %v, want ErrCatalogStaleGuard", err)
		}
		f.assertIntentsActive(f.primaryCheckoutID, true)
	})
}

func TestAuthorizeUntrackBlockedAndCancelledWriteNothing(t *testing.T) {
	t.Run("one non-revocable intent blocks the whole set", func(t *testing.T) {
		f := newCatalogUntrackFixture(t)
		f.seedIntent("intent-cli", f.targetCheckoutID, IntentSourceCLITrack)
		f.seedIntent("intent-project", f.targetCheckoutID, IntentSourceProjectMembership)
		cleanup := pendingCleanup("cleanup-forget-blocked", "forget_checkout", 0)

		result, err := f.catalog.AuthorizeUntrack(context.Background(), f.forgetAuthorization(cleanup))
		if err != nil {
			t.Fatalf("AuthorizeUntrack: %v", err)
		}
		if len(result.Revoked) != 0 || len(result.Blocked) != 1 ||
			result.Blocked[0].SourceKind != IntentSourceProjectMembership {
			t.Fatalf("blocked authorization = %+v", result)
		}
		f.assertIntentsActive(f.targetCheckoutID, true)
		if _, found, err := f.catalog.GetCleanupEntry(context.Background(), cleanup.CleanupID); err != nil || found {
			t.Fatalf("blocked authorization cleanup = found %v, err %v", found, err)
		}
	})

	t.Run("cancelled transaction writes nothing", func(t *testing.T) {
		f := newCatalogUntrackFixture(t)
		f.seedIntent("intent-cli", f.targetCheckoutID, IntentSourceCLITrack)
		cleanup := pendingCleanup("cleanup-forget-cancelled", "forget_checkout", 0)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		if _, err := f.catalog.AuthorizeUntrack(ctx, f.forgetAuthorization(cleanup)); err == nil {
			t.Fatal("cancelled AuthorizeUntrack unexpectedly succeeded")
		}
		f.assertIntentsActive(f.targetCheckoutID, true)
		if _, found, err := f.catalog.GetCleanupEntry(context.Background(), cleanup.CleanupID); err != nil || found {
			t.Fatalf("cancelled authorization cleanup = found %v, err %v", found, err)
		}
	})
}

func TestCommitAuthorizedDemotionRevalidatesBeforePublication(t *testing.T) {
	t.Run("stale primary leaves durable authorization", func(t *testing.T) {
		f := newCatalogUntrackFixture(t)
		f.seedIntent("intent-cli", f.targetCheckoutID, IntentSourceCLITrack)
		ctx := context.Background()
		authorized, err := f.catalog.AuthorizeUntrack(ctx, f.demotionAuthorization())
		if err != nil {
			t.Fatalf("AuthorizeUntrack: %v", err)
		}
		if err := f.catalog.SetPrimaryDedicatedGraph(ctx, SetPrimaryDedicatedGraphRequest{
			FamilyID:             f.familyID,
			GraphID:              f.primaryGraphID,
			ExpectedPrimaryEpoch: 0,
			LastSeen:             31,
		}); err != nil {
			t.Fatalf("advance primary epoch during build: %v", err)
		}
		cleanup := pendingCleanup("cleanup-demote-stale", "retire_graph", 0)

		err = f.catalog.CommitAuthorizedDemotion(ctx,
			f.demotionCommit(authorized.Transition.TransitionID, 0, cleanup))
		if !errors.Is(err, ErrCatalogStaleGuard) {
			t.Fatalf("CommitAuthorizedDemotion = %v, want ErrCatalogStaleGuard", err)
		}
		checkout, found, readErr := f.catalog.GetCheckout(context.Background(), f.targetCheckoutID)
		if readErr != nil || !found {
			t.Fatalf("GetCheckout = %+v, %v, %v", checkout, found, readErr)
		}
		if checkout.EffectiveMode != CheckoutModeDedicated ||
			checkout.ActiveIntentTransitionID != authorized.Transition.TransitionID {
			t.Fatalf("stale commit moved checkout = %+v", checkout)
		}
		if _, found, err := f.catalog.GetCleanupEntry(context.Background(), cleanup.CleanupID); err != nil || found {
			t.Fatalf("stale commit cleanup = found %v, err %v", found, err)
		}

		retry := f.demotionAuthorization()
		retry.ExpectedPrimaryEpoch = 1
		recovered, err := f.catalog.AuthorizeUntrack(ctx, retry)
		if err != nil {
			t.Fatalf("recover demotion authorization at new epoch: %v", err)
		}
		if !recovered.Existing || recovered.Transition == nil ||
			recovered.Transition.TransitionID != authorized.Transition.TransitionID {
			t.Fatalf("recovered authorization = %+v", recovered)
		}
		if err := f.catalog.CommitAuthorizedDemotion(ctx,
			f.demotionCommit(recovered.Transition.TransitionID, 1, cleanup)); err != nil {
			t.Fatalf("retry CommitAuthorizedDemotion: %v", err)
		}
		checkout, found, readErr = f.catalog.GetCheckout(context.Background(), f.targetCheckoutID)
		if readErr != nil || !found {
			t.Fatalf("GetCheckout after retry = %+v, %v, %v", checkout, found, readErr)
		}
		if checkout.EffectiveMode != CheckoutModeAutomatic || checkout.DesiredMode != CheckoutModeAutomatic ||
			checkout.ActiveIntentTransitionID != recovered.Transition.TransitionID {
			t.Fatalf("committed checkout = %+v", checkout)
		}
		standing, found, err := f.catalog.GetIntentTransition(ctx, f.targetCheckoutID)
		if err != nil || !found || standing.TransitionID != recovered.Transition.TransitionID {
			t.Fatalf("committed transition = %+v, found %v, err %v", standing, found, err)
		}
		if err := f.catalog.CompleteIntentTransition(ctx, f.targetCheckoutID, recovered.Transition.TransitionID); err != nil {
			t.Fatalf("CompleteIntentTransition: %v", err)
		}
		checkout, found, readErr = f.catalog.GetCheckout(ctx, f.targetCheckoutID)
		if readErr != nil || !found {
			t.Fatalf("GetCheckout after completion = %+v, %v, %v", checkout, found, readErr)
		}
		if checkout.ActiveIntentTransitionID != "" {
			t.Fatalf("completed checkout = %+v", checkout)
		}
		if _, found, err := f.catalog.GetIntentTransition(ctx, f.targetCheckoutID); err != nil || found {
			t.Fatalf("completed transition = found %v, err %v", found, err)
		}
		if stored, found, err := f.catalog.GetCleanupEntry(context.Background(), cleanup.CleanupID); err != nil || !found || stored.Reason != cleanup.Reason {
			t.Fatalf("committed cleanup = %+v, %v, %v", stored, found, err)
		}
	})

	t.Run("new intent blocks publication", func(t *testing.T) {
		f := newCatalogUntrackFixture(t)
		f.seedIntent("intent-cli", f.targetCheckoutID, IntentSourceCLITrack)
		authorized, err := f.catalog.AuthorizeUntrack(context.Background(), f.demotionAuthorization())
		if err != nil {
			t.Fatalf("AuthorizeUntrack: %v", err)
		}
		f.seedIntent("intent-mcp", f.targetCheckoutID, IntentSourceMCPTrack)
		cleanup := pendingCleanup("cleanup-demote-retracked", "retire_graph", 0)

		err = f.catalog.CommitAuthorizedDemotion(context.Background(),
			f.demotionCommit(authorized.Transition.TransitionID, 0, cleanup))
		if !errors.Is(err, ErrCatalogStaleGuard) {
			t.Fatalf("CommitAuthorizedDemotion = %v, want ErrCatalogStaleGuard", err)
		}
		checkout, _, readErr := f.catalog.GetCheckout(context.Background(), f.targetCheckoutID)
		if readErr != nil {
			t.Fatalf("GetCheckout: %v", readErr)
		}
		if checkout.EffectiveMode != CheckoutModeDedicated ||
			checkout.ActiveIntentTransitionID != authorized.Transition.TransitionID {
			t.Fatalf("retracked checkout moved = %+v", checkout)
		}
		if _, found, err := f.catalog.GetCleanupEntry(context.Background(), cleanup.CleanupID); err != nil || found {
			t.Fatalf("retracked commit cleanup = found %v, err %v", found, err)
		}
	})

	t.Run("existing graph cleanup must match the expected reason", func(t *testing.T) {
		f := newCatalogUntrackFixture(t)
		f.seedIntent("intent-cli", f.targetCheckoutID, IntentSourceCLITrack)
		authorized, err := f.catalog.AuthorizeUntrack(context.Background(), f.demotionAuthorization())
		if err != nil {
			t.Fatalf("AuthorizeUntrack: %v", err)
		}
		foreign := pendingCleanup("cleanup-demote-collision", "foreign_cleanup", 0)
		if err := f.catalog.UpsertCleanupEntry(context.Background(), foreign); err != nil {
			t.Fatalf("seed cleanup collision: %v", err)
		}
		requested := pendingCleanup(foreign.CleanupID, "retire_graph", 0)

		err = f.catalog.CommitAuthorizedDemotion(context.Background(),
			f.demotionCommit(authorized.Transition.TransitionID, 0, requested))
		if !errors.Is(err, ErrCatalogStaleGuard) {
			t.Fatalf("CommitAuthorizedDemotion = %v, want ErrCatalogStaleGuard", err)
		}
		checkout, _, readErr := f.catalog.GetCheckout(context.Background(), f.targetCheckoutID)
		if readErr != nil {
			t.Fatalf("GetCheckout: %v", readErr)
		}
		if checkout.EffectiveMode != CheckoutModeDedicated ||
			checkout.ActiveIntentTransitionID != authorized.Transition.TransitionID {
			t.Fatalf("cleanup collision moved checkout = %+v", checkout)
		}
	})
}
