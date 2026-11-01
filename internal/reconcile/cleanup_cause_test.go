package reconcile

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
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

func TestCauseReachesReleaseGraphAcrossRestart(t *testing.T) {
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
	if err := f.store.Close(); err != nil {
		t.Fatal(err)
	}
	copyPath := filepath.Join(t.TempDir(), "restart.sqlite")
	for _, suffix := range []string{"", "-wal"} {
		data, err := os.ReadFile(f.dbPath + suffix)
		if os.IsNotExist(err) && suffix != "" {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(copyPath+suffix, data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	migrations := 0
	reopened, err := store_sqlite.Open(copyPath, store_sqlite.WithUpgradeBackup(), store_sqlite.WithMigrationObserver(func(store_sqlite.MigrationProgress) { migrations++ }))
	if migrations != 0 {
		t.Fatalf("cause recovery unexpectedly migrated unchanged schema: %d callbacks", migrations)
	}
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	hooks := &recordingHooks{}
	rec, err := New(reopened.Catalog(), hooks, Default())
	if err != nil {
		t.Fatal(err)
	}
	if err := rec.Resume(context.Background()); err != nil {
		t.Fatal(err)
	}
	hooks.mu.Lock()
	defer hooks.mu.Unlock()
	if len(hooks.causes) != 1 || hooks.causes[0] != CauseForget {
		t.Fatalf("restarted release causes=%v", hooks.causes)
	}
}

func TestLegacyExplicitGraphCleanupInfersDemotion(t *testing.T) {
	f := newFixture(t, Default())
	f.seedCheckout("owner", "inc", "work", store_sqlite.CheckoutModeDedicated)
	f.seedOwnedGraph("owned", "owner")
	target := sagaTarget{Kind: sagaRetireGraph, GraphID: "owned", CheckoutID: "owner", FamilyID: f.familyID}
	payload, err := json.Marshal(target)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.catalog.UpsertCleanupEntry(context.Background(), store_sqlite.CleanupEntry{CleanupID: target.cleanupID(), OpaqueTargetIDs: string(payload), Reason: string(target.Kind), Phase: store_sqlite.CleanupPhasePending}); err != nil {
		t.Fatal(err)
	}
	if err := f.rec.Resume(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.hooks.mu.Lock()
	defer f.hooks.mu.Unlock()
	if len(f.hooks.causes) != 1 || f.hooks.causes[0] != CauseDemotion {
		t.Fatalf("legacy explicit-only cleanup causes=%v", f.hooks.causes)
	}
}
