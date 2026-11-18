package store_sqlite

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
)

func TestCheckoutRecoveryCannotOverwriteNewOutageOrIncarnation(t *testing.T) {
	for _, replacement := range []string{"new-outage", "new-incarnation"} {
		t.Run(replacement, func(t *testing.T) {
			store, err := Open(filepath.Join(t.TempDir(), "recovery.sqlite"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = store.Close() })
			catalog := store.Catalog()
			ctx := context.Background()
			if err := catalog.UpsertRepositoryFamily(ctx, RepositoryFamily{FamilyID: "family", CommonDirIdentity: "/fixture/.git", State: "family_ready"}); err != nil {
				t.Fatal(err)
			}
			checkout := Checkout{CheckoutID: "owner", Incarnation: "inc", FamilyID: "family", RootPath: "/fixture", GitDir: "/fixture/.git", AdminName: "@main", State: CheckoutStateReconciling, DesiredMode: CheckoutModeDedicated, EffectiveMode: CheckoutModeDedicated}
			if replacement == "new-outage" {
				checkout.State = CheckoutStateUnavailable
			} else {
				checkout.Incarnation = "next-inc"
				checkout.DesiredMode = CheckoutModeAutomatic
				checkout.EffectiveMode = CheckoutModeAutomatic
			}
			if err := catalog.UpsertCheckout(ctx, checkout); err != nil {
				t.Fatal(err)
			}
			const cleanupID = "purge-layers:owner:inc"
			if err := catalog.UpsertCleanupEntry(ctx, CleanupEntry{CleanupID: cleanupID, OpaqueTargetIDs: `{"kind":"purge_checkout_layers"}`, Reason: "purge_checkout_layers", Phase: CleanupPhaseDone}); err != nil {
				t.Fatal(err)
			}
			if err := catalog.UpdateCheckoutRecoveryState(ctx, "owner", "inc", CheckoutStateReconciling, CheckoutStateReady, "", 2); !errors.Is(err, ErrCatalogStaleGuard) {
				t.Fatalf("stale recovery=%v", err)
			}
			if err := catalog.DeleteCompletedCleanupForRecovery(ctx, "owner", "inc", cleanupID); err != nil {
				t.Fatal(err)
			}
			if _, exists, err := catalog.GetCleanupEntry(ctx, cleanupID); err != nil || !exists {
				t.Fatalf("stale recovery erased purge evidence: %v %v", exists, err)
			}
			current, exists, err := catalog.GetCheckout(ctx, "owner")
			if err != nil || !exists || current.State != checkout.State || current.Incarnation != checkout.Incarnation || current.DesiredMode != checkout.DesiredMode || current.EffectiveMode != checkout.EffectiveMode {
				t.Fatalf("stale recovery changed new owner: %+v err=%v", current, err)
			}
		})
	}
}
