package reconcile

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

func TestLegacyJournalCauseInference(t *testing.T) {
	for _, revoked := range []bool{false, true} {
		name := "active-intent"
		if revoked {
			name = "revoked-intent"
		}
		t.Run(name, func(t *testing.T) {
			f := newFixture(t, Default())
			f.seedCheckout("owner", "inc", "work", store_sqlite.CheckoutModeDedicated)
			f.seedOwnedGraph("owned", "owner")
			epoch := f.promoteToPrimary("owned")
			intent := store_sqlite.TrackingIntent{IntentID: "explicit", CheckoutID: "owner", SourceKind: store_sqlite.IntentSourceCLITrack, SourceLocator: "cli", Active: !revoked}
			if revoked {
				intent.RevokedAt = time.Now().Unix()
			}
			if err := f.catalog.UpsertTrackingIntent(context.Background(), intent); err != nil {
				t.Fatal(err)
			}
			payload, err := json.Marshal(map[string]any{"kind": "retire_primary_closure", "graph_id": "owned", "family_id": f.familyID, "checkout_id": "owner", "primary_epoch": epoch})
			if err != nil {
				t.Fatal(err)
			}
			if err := f.catalog.UpsertCleanupEntry(context.Background(), store_sqlite.CleanupEntry{CleanupID: "retire-primary-closure:owned", OpaqueTargetIDs: string(payload), Reason: "retire_primary_closure", Phase: store_sqlite.CleanupPhasePending}); err != nil {
				t.Fatal(err)
			}
			if err := f.rec.Resume(context.Background()); err != nil {
				t.Fatal(err)
			}
			_, exists, err := f.catalog.GetCheckout(context.Background(), "owner")
			if err != nil || exists == revoked {
				t.Fatalf("legacy revoked=%v: owner exists=%v err=%v cleanup=%v", revoked, exists, err, f.hooks.snapshot())
			}
		})
	}
}

func TestCleanupCausePersistsAcrossFailedAttempt(t *testing.T) {
	f := newFixture(t, Default())
	f.seedCheckout("owner", "inc", "work", store_sqlite.CheckoutModeDedicated)
	f.seedOwnedGraph("owned", "owner")
	f.hooks.failRelease = 1
	if err := f.rec.ForgetCheckout(context.Background(), "owner", "inc"); err == nil {
		t.Fatal("expected failed graph release")
	}
	entry, exists, err := f.catalog.GetCleanupEntry(context.Background(), "forget-checkout:owner:inc")
	if err != nil || !exists {
		t.Fatalf("cleanup journal missing: %v %v", exists, err)
	}
	var payload struct {
		Cause string `json:"cause"`
	}
	if err := json.Unmarshal([]byte(entry.OpaqueTargetIDs), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Cause != "forget" {
		t.Fatalf("durable attempt cause=%q want forget", payload.Cause)
	}
}
