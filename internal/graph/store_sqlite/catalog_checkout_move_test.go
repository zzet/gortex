package store_sqlite

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func checkoutMoveObservation(checkout Checkout, root string) UpdateCheckoutObservationRequest {
	return UpdateCheckoutObservationRequest{
		CheckoutID:           checkout.CheckoutID,
		Incarnation:          checkout.Incarnation,
		ExpectedRootPath:     checkout.RootPath,
		State:                checkout.State,
		RootPath:             root,
		GitDir:               root + "/.git",
		Locked:               checkout.Locked,
		Prunable:             checkout.Prunable,
		HeadRef:              checkout.HeadRef,
		HeadCommit:           checkout.HeadCommit,
		HeadTree:             checkout.HeadTree,
		LastAccessible:       checkout.LastAccessible,
		UnavailableSince:     checkout.UnavailableSince,
		AvailabilityDeadline: checkout.AvailabilityDeadline,
		RemovalDetectedAt:    checkout.RemovalDetectedAt,
		RemovalDeadline:      checkout.RemovalDeadline,
		RemovalEvidence:      checkout.RemovalEvidence,
		LastSeen:             checkout.LastSeen + 1,
		LastError:            checkout.LastError,
	}
}

func TestCheckoutObservationRootGuardRejectsStaleMove(t *testing.T) {
	store := openCatalogStore(t)
	ctx := context.Background()
	catalog := store.Catalog()
	seedFamilyAndCheckout(t, catalog, "fam", "wt", "inc")

	atA, found, err := catalog.GetCheckout(ctx, "wt")
	if err != nil || !found {
		t.Fatalf("GetCheckout(A) = found %t, err %v", found, err)
	}
	staleAToB := checkoutMoveObservation(atA, "/tmp/wt-b")
	if err := catalog.UpdateCheckoutObservation(ctx, staleAToB); err != nil {
		t.Fatalf("A -> B: %v", err)
	}
	moveB, found, err := catalog.GetCheckoutRootMove(ctx, "wt")
	if err != nil || !found {
		t.Fatalf("GetCheckoutRootMove(B) = found %t, err %v", found, err)
	}
	if moveB.PreviousRootPath != "/tmp/wt" ||
		moveB.LatestPreviousRootPath != "/tmp/wt" ||
		moveB.ConfigRootPath != "/tmp/wt" ||
		moveB.CurrentRootPath != "/tmp/wt-b" {
		t.Fatalf("A -> B journal = %+v", moveB)
	}
	if err := catalog.PrepareCheckoutRootMoveConfig(
		ctx, "wt", "inc", "/tmp/wt", "/tmp/wt-b", "before-a-b", "after-a-b",
	); err != nil {
		t.Fatalf("prepare config A -> B: %v", err)
	}
	atB, _, err := catalog.GetCheckout(ctx, "wt")
	if err != nil {
		t.Fatalf("GetCheckout(B): %v", err)
	}
	if err := catalog.UpdateCheckoutObservation(ctx, checkoutMoveObservation(atB, "/tmp/wt-c")); err != nil {
		t.Fatalf("B -> C: %v", err)
	}
	moveC, found, err := catalog.GetCheckoutRootMove(ctx, "wt")
	if err != nil || !found {
		t.Fatalf("GetCheckoutRootMove(C) = found %t, err %v", found, err)
	}
	if moveC.PreviousRootPath != "/tmp/wt" ||
		moveC.LatestPreviousRootPath != "/tmp/wt-b" ||
		moveC.ConfigRootPath != "/tmp/wt" ||
		moveC.ConfigPreparedFromPath != "/tmp/wt" ||
		moveC.ConfigPreparedToPath != "/tmp/wt-b" ||
		moveC.ConfigPreparedBeforeHash != "before-a-b" ||
		moveC.ConfigPreparedAfterHash != "after-a-b" ||
		moveC.CurrentRootPath != "/tmp/wt-c" {
		t.Fatalf("B -> C journal = %+v", moveC)
	}
	if err := catalog.CompleteCheckoutRootMove(ctx, moveB); !errors.Is(err, ErrCatalogStaleGuard) {
		t.Fatalf("stale B completion = %v, want ErrCatalogStaleGuard", err)
	}

	if err := catalog.UpdateCheckoutObservation(ctx, staleAToB); !errors.Is(err, ErrCatalogStaleGuard) {
		t.Fatalf("stale A -> B = %v, want ErrCatalogStaleGuard", err)
	}
	after, _, err := catalog.GetCheckout(ctx, "wt")
	if err != nil {
		t.Fatalf("GetCheckout(C): %v", err)
	}
	if after.RootPath != "/tmp/wt-c" {
		t.Fatalf("stale observation rewound root to %q", after.RootPath)
	}
	if err := catalog.AcknowledgeCheckoutRootMoveConfig(
		ctx, "wt", "inc", "/tmp/wt", "/tmp/wt-b", "before-a-b", "after-a-b",
	); err != nil {
		t.Fatalf("acknowledge config at B: %v", err)
	}
	if err := catalog.AcknowledgeCheckoutRootMoveConfig(
		ctx, "wt", "inc", "/tmp/wt", "/tmp/wt-c", "before-a-c", "after-a-c",
	); !errors.Is(err, ErrCatalogStaleGuard) {
		t.Fatalf("stale config acknowledgement = %v, want ErrCatalogStaleGuard", err)
	}
	if err := catalog.CompleteCheckoutRootMove(ctx, moveC); err == nil {
		t.Fatal("completion with prepared config unexpectedly succeeded")
	}
	moveC, found, err = catalog.GetCheckoutRootMove(ctx, "wt")
	if err != nil || !found || moveC.ConfigRootPath != "/tmp/wt-b" ||
		moveC.ConfigPreparedFromPath != "" || moveC.ConfigPreparedToPath != "" ||
		moveC.ConfigPreparedBeforeHash != "" || moveC.ConfigPreparedAfterHash != "" {
		t.Fatalf("acknowledged C journal = found %t, move %+v, err %v", found, moveC, err)
	}
	if err := catalog.CompleteCheckoutRootMove(ctx, moveC); err != nil {
		t.Fatalf("complete C move: %v", err)
	}
	if _, found, err := catalog.GetCheckoutRootMove(ctx, "wt"); err != nil || found {
		t.Fatalf("completed journal = found %t, err %v", found, err)
	}
}

func TestCheckoutObservationAliasSpellingDoesNotPublishMove(t *testing.T) {
	store := openCatalogStore(t)
	ctx := context.Background()
	catalog := store.Catalog()
	seedFamilyAndCheckout(t, catalog, "fam", "wt", "inc")

	root := t.TempDir()
	realRoot := filepath.Join(root, "real")
	aliasRoot := filepath.Join(root, "alias")
	if err := os.Mkdir(realRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(realRoot, aliasRoot); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	checkout, _, err := catalog.GetCheckout(ctx, "wt")
	if err != nil {
		t.Fatal(err)
	}
	if err := catalog.UpdateCheckoutObservation(ctx, checkoutMoveObservation(checkout, realRoot)); err != nil {
		t.Fatal(err)
	}
	move, found, err := catalog.GetCheckoutRootMove(ctx, "wt")
	if err != nil || !found {
		t.Fatalf("initial physical move = found %t, err %v", found, err)
	}
	checkout, _, err = catalog.GetCheckout(ctx, "wt")
	if err != nil {
		t.Fatal(err)
	}
	if err := catalog.UpdateCheckoutObservation(ctx, checkoutMoveObservation(checkout, aliasRoot)); err != nil {
		t.Fatal(err)
	}
	checkout, _, err = catalog.GetCheckout(ctx, "wt")
	if err != nil {
		t.Fatal(err)
	}
	if checkout.RootPath != realRoot {
		t.Fatalf("alias observation changed stored root spelling to %q", checkout.RootPath)
	}
	afterAlias, found, err := catalog.GetCheckoutRootMove(ctx, "wt")
	if err != nil || !found || afterAlias != move {
		t.Fatalf("alias observation journal = found %t, move %+v, err %v", found, afterAlias, err)
	}
	if err := catalog.CompleteCheckoutRootMove(ctx, move); err != nil {
		t.Fatalf("complete pending move after alias observation: %v", err)
	}
}

func TestCompleteCheckoutRootMoveRequiresDedicatedConfigAckAndNoPrepare(t *testing.T) {
	store := openCatalogStore(t)
	ctx := context.Background()
	catalog := store.Catalog()
	seedFamilyAndCheckout(t, catalog, "fam", "wt", "inc")
	checkout, found, err := catalog.GetCheckout(ctx, "wt")
	if err != nil || !found {
		t.Fatalf("GetCheckout = found %t, err %v", found, err)
	}
	checkout.DesiredMode = CheckoutModeDedicated
	checkout.EffectiveMode = CheckoutModeDedicated
	if err := catalog.UpsertCheckout(ctx, checkout); err != nil {
		t.Fatal(err)
	}
	if err := catalog.UpdateCheckoutObservation(
		ctx, checkoutMoveObservation(checkout, "/tmp/wt-new"),
	); err != nil {
		t.Fatal(err)
	}
	move, found, err := catalog.GetCheckoutRootMove(ctx, "wt")
	if err != nil || !found {
		t.Fatalf("GetCheckoutRootMove = found %t, err %v", found, err)
	}
	if err := catalog.CompleteCheckoutRootMove(ctx, move); !errors.Is(err, ErrCatalogStaleGuard) {
		t.Fatalf("unacknowledged dedicated completion = %v, want stale guard", err)
	}
	if err := catalog.PrepareCheckoutRootMoveConfig(
		ctx, "wt", "inc", "/tmp/wt", "/tmp/wt-new", "before", "after",
	); err != nil {
		t.Fatal(err)
	}
	prepared, _, err := catalog.GetCheckoutRootMove(ctx, "wt")
	if err != nil {
		t.Fatal(err)
	}
	if err := catalog.CompleteCheckoutRootMove(ctx, prepared); !errors.Is(err, ErrCatalogInvalidValue) {
		t.Fatalf("prepared completion = %v, want invalid value", err)
	}
	if err := catalog.AcknowledgeCheckoutRootMoveConfig(
		ctx, "wt", "inc", "/tmp/wt", "/tmp/wt-new", "before", "after",
	); err != nil {
		t.Fatal(err)
	}
	move, _, err = catalog.GetCheckoutRootMove(ctx, "wt")
	if err != nil {
		t.Fatal(err)
	}
	if err := catalog.CompleteCheckoutRootMove(ctx, move); err != nil {
		t.Fatalf("acknowledged dedicated completion: %v", err)
	}
}

func TestRelocateActiveTrackingIntentLocatorsIsGuardedAndMerges(t *testing.T) {
	store := openCatalogStore(t)
	ctx := context.Background()
	catalog := store.Catalog()
	seedFamilyAndCheckout(t, catalog, "fam", "wt", "inc")
	checkout, _, err := catalog.GetCheckout(ctx, "wt")
	if err != nil {
		t.Fatal(err)
	}
	if err := catalog.UpdateCheckoutObservation(ctx, checkoutMoveObservation(checkout, "/tmp/wt-new")); err != nil {
		t.Fatal(err)
	}

	for _, intent := range []TrackingIntent{
		{IntentID: "cli-old", CheckoutID: "wt", SourceKind: IntentSourceCLITrack, SourceLocator: "/tmp/wt", Active: true},
		// A revoked historical target must be reactivated before cli-old is
		// removed, otherwise the relocation silently loses CLI ownership.
		{IntentID: "cli-new", CheckoutID: "wt", SourceKind: IntentSourceCLITrack, SourceLocator: "/tmp/wt-new", Active: false, RevokedAt: 42, LastError: "old failure"},
		{IntentID: "mcp-old", CheckoutID: "wt", SourceKind: IntentSourceMCPTrack, SourceLocator: "/tmp/wt", Active: true},
		{IntentID: "config-old", CheckoutID: "wt", SourceKind: IntentSourceManualConfig, SourceLocator: "/tmp/wt", Active: true},
		{IntentID: "project", CheckoutID: "wt", SourceKind: IntentSourceProjectMembership, SourceLocator: "project:web", Active: true},
	} {
		if err := catalog.UpsertTrackingIntent(ctx, intent); err != nil {
			t.Fatalf("seed intent %s: %v", intent.IntentID, err)
		}
	}

	if _, err := catalog.RelocateActiveTrackingIntentLocators(ctx, "wt", "inc", "/tmp/not-current"); !errors.Is(err, ErrCatalogStaleGuard) {
		t.Fatalf("stale root relocation = %v, want ErrCatalogStaleGuard", err)
	}
	moved, err := catalog.RelocateActiveTrackingIntentLocators(ctx, "wt", "inc", "/tmp/wt-new")
	if err != nil {
		t.Fatalf("RelocateActiveTrackingIntentLocators: %v", err)
	}
	if moved != 3 {
		t.Fatalf("moved locators = %d, want 3", moved)
	}

	intents, err := catalog.ListTrackingIntents(ctx, "wt")
	if err != nil {
		t.Fatal(err)
	}
	byKind := map[IntentSourceKind][]string{}
	var mergedCLI *TrackingIntent
	for _, intent := range intents {
		if intent.IntentID == "cli-new" {
			copy := intent
			mergedCLI = &copy
		}
		if intent.Active {
			byKind[intent.SourceKind] = append(byKind[intent.SourceKind], intent.SourceLocator)
		}
	}
	for _, kind := range []IntentSourceKind{IntentSourceCLITrack, IntentSourceMCPTrack, IntentSourceManualConfig} {
		if got := byKind[kind]; len(got) != 1 || got[0] != "/tmp/wt-new" {
			t.Fatalf("%s locators = %v, want only current root", kind, got)
		}
	}
	if got := byKind[IntentSourceProjectMembership]; len(got) != 1 || got[0] != "project:web" {
		t.Fatalf("project locator changed: %v", got)
	}
	if mergedCLI == nil || !mergedCLI.Active || mergedCLI.RevokedAt != 0 || mergedCLI.LastError != "" {
		t.Fatalf("inactive target was not reactivated cleanly: %+v", mergedCLI)
	}
}

func BenchmarkCheckoutRootMoveObservationCAS(b *testing.B) {
	store, err := Open(filepath.Join(b.TempDir(), "move-observation.sqlite"))
	if err != nil {
		b.Fatal(err)
	}
	defer store.Close()
	catalog := store.Catalog()
	ctx := context.Background()
	if err := catalog.UpsertRepositoryFamily(ctx, RepositoryFamily{
		FamilyID: "family", CommonDirIdentity: "/tmp/family.git", State: "ready",
	}); err != nil {
		b.Fatal(err)
	}
	checkout := Checkout{
		CheckoutID: "checkout", Incarnation: "inc", FamilyID: "family",
		RootPath: "/tmp/move-a", GitDir: "/tmp/family.git/worktrees/checkout",
		AdminName: "checkout", State: CheckoutStateReady,
		DesiredMode: CheckoutModeAutomatic, EffectiveMode: CheckoutModeAutomatic,
	}
	if err := catalog.UpsertCheckout(ctx, checkout); err != nil {
		b.Fatal(err)
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		next := "/tmp/move-b"
		if i%2 != 0 {
			next = "/tmp/move-a"
		}
		request := checkoutMoveObservation(checkout, next)
		if err := catalog.UpdateCheckoutObservation(ctx, request); err != nil {
			b.Fatal(err)
		}
		checkout.RootPath = next
		checkout.LastSeen = request.LastSeen
	}
}
