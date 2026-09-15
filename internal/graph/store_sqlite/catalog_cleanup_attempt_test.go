package store_sqlite

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"sync"
	"testing"
)

func cleanupAttemptCatalog(t *testing.T) *Catalog {
	t.Helper()
	store, err := openPristine(t, filepath.Join(t.TempDir(), "attempt.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if store != nil {
			_ = store.Close()
		}
	})
	return store.Catalog()
}

func cleanupAttemptFixture(token string) CleanupEntry {
	return CleanupEntry{CleanupID: "forget-checkout:owner:incarnation", OpaqueTargetIDs: `{"kind":"forget_checkout","checkout_id":"owner","incarnation":"incarnation","cleanup_attempt_id":"` + token + `"}`, Reason: "forget_checkout", Phase: CleanupPhasePending, LastProgress: 1}
}

func TestCleanupAttemptCannotResurrectCompletedJournal(t *testing.T) {
	c := cleanupAttemptCatalog(t)
	ctx := context.Background()
	entry := cleanupAttemptFixture("old")
	if err := c.CreateCleanupAttempt(ctx, entry, "old"); err != nil {
		t.Fatal(err)
	}
	if err := c.DeleteCleanupAttempt(ctx, entry.CleanupID, "old"); err != nil {
		t.Fatal(err)
	}
	if err := c.AdvanceCleanupAttempt(ctx, entry, "old"); !errors.Is(err, ErrCatalogStaleGuard) {
		t.Fatalf("old update recreated journal: %v", err)
	}
	if err := c.DeleteCleanupAttempt(ctx, entry.CleanupID, "old"); !errors.Is(err, ErrCatalogStaleGuard) {
		t.Fatalf("missing journal counted as this actor's completion: %v", err)
	}
	if _, found, err := c.GetCleanupEntry(ctx, entry.CleanupID); err != nil || found {
		t.Fatalf("journal resurrected: found=%v err=%v", found, err)
	}
}

func TestCleanupAttemptCASPreservesSuccessorAtSameID(t *testing.T) {
	c := cleanupAttemptCatalog(t)
	ctx := context.Background()
	old := cleanupAttemptFixture("old")
	if err := c.CreateCleanupAttempt(ctx, old, "old"); err != nil {
		t.Fatal(err)
	}
	if err := c.DeleteCleanupAttempt(ctx, old.CleanupID, "old"); err != nil {
		t.Fatal(err)
	}
	next := cleanupAttemptFixture("next")
	next.LastProgress = 2
	if err := c.CreateCleanupAttempt(ctx, next, "next"); err != nil {
		t.Fatal(err)
	}
	for name, run := range map[string]func() error{
		"advance": func() error { return c.AdvanceCleanupAttempt(ctx, old, "old") },
		"delete":  func() error { return c.DeleteCleanupAttempt(ctx, old.CleanupID, "old") },
		"create":  func() error { return c.CreateCleanupAttempt(ctx, old, "old") },
		"load":    func() error { _, err := c.GetCleanupAttempt(ctx, old.CleanupID, "old"); return err },
	} {
		t.Run(name, func(t *testing.T) {
			if err := run(); !errors.Is(err, ErrCatalogStaleGuard) {
				t.Fatalf("stale attempt accepted: %v", err)
			}
			got, err := c.GetCleanupAttempt(ctx, next.CleanupID, "next")
			if err != nil || !sameCleanupSnapshot(got, next) {
				t.Fatalf("successor changed: got=%+v err=%v", got, err)
			}
		})
	}
}

func TestCleanupAttemptAdvancesOnlyMatchingPayloadToken(t *testing.T) {
	c := cleanupAttemptCatalog(t)
	ctx := context.Background()
	entry := cleanupAttemptFixture("current")
	if err := c.CreateCleanupAttempt(ctx, entry, "current"); err != nil {
		t.Fatal(err)
	}
	entry.Phase, entry.LastProgress, entry.LastError = CleanupPhaseFailed, 10, "retry"
	if err := c.AdvanceCleanupAttempt(ctx, entry, "current"); err != nil {
		t.Fatal(err)
	}
	got, err := c.GetCleanupAttempt(ctx, entry.CleanupID, "current")
	if err != nil || !sameCleanupSnapshot(got, entry) {
		t.Fatalf("progress lost: %+v %v", got, err)
	}
	if err := c.AdvanceCleanupAttempt(ctx, cleanupAttemptFixture("forged"), "current"); !errors.Is(err, ErrCatalogStaleGuard) {
		t.Fatalf("write replaced its own expected token: %v", err)
	}
}

func TestCleanupAttemptLegacyUpgradePreservesOpaqueMembersAndProgress(t *testing.T) {
	c := cleanupAttemptCatalog(t)
	ctx := context.Background()
	legacy := cleanupAttemptFixture("")
	legacy.OpaqueTargetIDs = `{"kind":"forget_checkout","future_field":{"nested":[1,2]},"phase":"release_graph"}`
	legacy.Phase, legacy.LastProgress, legacy.LastError, legacy.GraceDeadline = CleanupPhaseFailed, 9, "pending", 17
	if err := c.UpsertCleanupEntry(ctx, legacy); err != nil {
		t.Fatal(err)
	}
	upgraded, err := c.UpgradeCleanupAttempt(ctx, legacy, "upgraded")
	if err != nil {
		t.Fatal(err)
	}
	if err := requireCleanupAttempt(upgraded, "upgraded"); err != nil {
		t.Fatal(err)
	}
	var payload map[string]json.RawMessage
	if err := json.Unmarshal([]byte(upgraded.OpaqueTargetIDs), &payload); err != nil {
		t.Fatal(err)
	}
	if string(payload["future_field"]) != `{"nested":[1,2]}` {
		t.Fatalf("opaque field lost: %s", payload["future_field"])
	}
	upgraded.OpaqueTargetIDs = legacy.OpaqueTargetIDs
	if !sameCleanupSnapshot(upgraded, legacy) {
		t.Fatalf("upgrade reset progress: %+v", upgraded)
	}
	if _, err := c.UpgradeCleanupAttempt(ctx, legacy, "second"); !errors.Is(err, ErrCatalogStaleGuard) {
		t.Fatalf("old snapshot rebound attempt: %v", err)
	}
}

func TestCleanupAttemptLegacyUpgradeCannotInsertOrAdoptReplacement(t *testing.T) {
	c := cleanupAttemptCatalog(t)
	ctx := context.Background()
	legacy := cleanupAttemptFixture("")
	if _, err := c.UpgradeCleanupAttempt(ctx, legacy, "old"); !errors.Is(err, ErrCatalogStaleGuard) {
		t.Fatalf("missing legacy row inserted: %v", err)
	}
	next := cleanupAttemptFixture("next")
	if err := c.CreateCleanupAttempt(ctx, next, "next"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.UpgradeCleanupAttempt(ctx, legacy, "old"); !errors.Is(err, ErrCatalogStaleGuard) {
		t.Fatalf("legacy snapshot adopted successor: %v", err)
	}
	got, err := c.GetCleanupAttempt(ctx, next.CleanupID, "next")
	if err != nil || !sameCleanupSnapshot(got, next) {
		t.Fatalf("successor changed: %+v %v", got, err)
	}
}

func TestCleanupAttemptConcurrentLegacyUpgradeHasOneWinner(t *testing.T) {
	c := cleanupAttemptCatalog(t)
	ctx := context.Background()
	legacy := cleanupAttemptFixture("")
	if err := c.UpsertCleanupEntry(ctx, legacy); err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	results := make(chan error, 2)
	var wg sync.WaitGroup
	for _, token := range []string{"one", "two"} {
		wg.Add(1)
		go func(token string) {
			defer wg.Done()
			<-start
			_, err := c.UpgradeCleanupAttempt(ctx, legacy, token)
			results <- err
		}(token)
	}
	close(start)
	wg.Wait()
	close(results)
	wins, stale := 0, 0
	for err := range results {
		if err == nil {
			wins++
		} else if errors.Is(err, ErrCatalogStaleGuard) {
			stale++
		} else {
			t.Fatal(err)
		}
	}
	if wins != 1 || stale != 1 {
		t.Fatalf("upgrade winner count: wins=%d stale=%d", wins, stale)
	}
}

func TestCleanupAttemptIdentitySurvivesStoreReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "attempt.sqlite")
	store, err := openPristine(t, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if store != nil {
			_ = store.Close()
		}
	})
	entry := cleanupAttemptFixture("durable")
	ctx := context.Background()
	if err := store.Catalog().CreateCleanupAttempt(ctx, entry, "durable"); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	got, err := store.Catalog().GetCleanupAttempt(ctx, entry.CleanupID, "durable")
	if err != nil || !sameCleanupSnapshot(got, entry) {
		t.Fatalf("durable identity lost: %+v %v", got, err)
	}
}
