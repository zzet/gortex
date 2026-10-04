package reconcile

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

type cleanupAttemptActorKey struct{}

type cleanupAttemptHooks struct {
	purgeEntered, allowPurge   chan struct{}
	finishEntered, allowFinish chan struct{}
	purgeOnce, finishOnce      sync.Once
	releaseCalls               atomic.Int64
	failRelease                atomic.Bool
}

var errCleanupAttemptRetry = errors.New("retry cleanup owner later")

func (h *cleanupAttemptHooks) PurgeCheckoutLayers(ctx context.Context, _, _ string) error {
	if ctx.Value(cleanupAttemptActorKey{}) != "first" || h.purgeEntered == nil {
		return nil
	}
	h.purgeOnce.Do(func() { close(h.purgeEntered) })
	select {
	case <-h.allowPurge:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (h *cleanupAttemptHooks) ReleaseGraph(context.Context, string) error {
	h.releaseCalls.Add(1)
	if h.failRelease.Load() {
		return errCleanupAttemptRetry
	}
	return nil
}

func (h *cleanupAttemptHooks) RepositoryCleanupAttemptFinished([]string) {
	if h.finishEntered == nil {
		return
	}
	h.finishOnce.Do(func() { close(h.finishEntered); <-h.allowFinish })
}

type cleanupAttemptIntegrationFixture struct {
	store   *store_sqlite.Store
	catalog *store_sqlite.Catalog
	owner   store_sqlite.Checkout
	graph   store_sqlite.DedicatedGraph
}

func newCleanupAttemptIntegrationFixture(t *testing.T) *cleanupAttemptIntegrationFixture {
	t.Helper()
	root := t.TempDir()
	store, err := store_sqlite.Open(filepath.Join(root, "cleanup.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	f := &cleanupAttemptIntegrationFixture{store: store, catalog: store.Catalog()}
	family := store_sqlite.RepositoryFamily{FamilyID: "attempt-family", CommonDirIdentity: filepath.Join(root, ".git"), State: "active"}
	if err := f.catalog.UpsertRepositoryFamily(context.Background(), family); err != nil {
		t.Fatal(err)
	}
	f.owner = store_sqlite.Checkout{CheckoutID: "attempt-owner", Incarnation: "same-incarnation", FamilyID: family.FamilyID, RootPath: root, GitDir: family.CommonDirIdentity, AdminName: "main", State: store_sqlite.CheckoutStateReady, DesiredMode: store_sqlite.CheckoutModeDedicated, EffectiveMode: store_sqlite.CheckoutModeDedicated}
	f.graph = store_sqlite.DedicatedGraph{GraphID: "attempt-graph", OwnerCheckoutID: f.owner.CheckoutID, RepoPrefix: "attempt-repo", FamilyID: family.FamilyID, State: "ready"}
	f.install(t)
	return f
}

func (f *cleanupAttemptIntegrationFixture) install(t *testing.T) {
	t.Helper()
	if err := f.catalog.UpsertCheckout(context.Background(), f.owner); err != nil {
		t.Fatal(err)
	}
	if err := f.catalog.UpsertDedicatedGraph(context.Background(), f.graph); err != nil {
		t.Fatal(err)
	}
}

func (f *cleanupAttemptIntegrationFixture) target() sagaTarget {
	return sagaTarget{Kind: sagaForgetCheckout, CheckoutID: f.owner.CheckoutID, Incarnation: f.owner.Incarnation, FamilyID: f.owner.FamilyID, GraphID: f.graph.GraphID}
}

func TestAuthorizedCleanupAttemptSerializesHooksAndFencesRetrack(t *testing.T) {
	for _, handles := range []string{"same_reconciler", "separate_reconcilers", "separate_catalog_handles"} {
		t.Run(handles, func(t *testing.T) {
			f := newCleanupAttemptIntegrationFixture(t)
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			hooks := &cleanupAttemptHooks{purgeEntered: make(chan struct{}), allowPurge: make(chan struct{}), finishEntered: make(chan struct{}), allowFinish: make(chan struct{})}
			var purgeOnce, finishOnce sync.Once
			allowPurge := func() { purgeOnce.Do(func() { close(hooks.allowPurge) }) }
			allowFinish := func() { finishOnce.Do(func() { close(hooks.allowFinish) }) }
			first := &Reconciler{catalog: f.catalog, hooks: hooks, now: time.Now}
			second := first
			if handles != "same_reconciler" {
				second = &Reconciler{catalog: f.catalog, hooks: hooks, now: time.Now}
			}
			if handles == "separate_catalog_handles" {
				second.catalog = f.store.Catalog()
			}
			firstResult := make(chan error, 1)
			firstDone := make(chan struct{})
			go func() {
				defer close(firstDone)
				_, err := first.ForgetCheckoutExplicit(context.WithValue(ctx, cleanupAttemptActorKey{}, "first"), f.owner.CheckoutID, f.owner.Incarnation, f.owner.FamilyID, f.graph.GraphID)
				firstResult <- err
			}()
			t.Cleanup(func() { cancel(); allowPurge(); allowFinish(); awaitCleanupSignal(t, firstDone) })
			awaitCleanupSignal(t, hooks.purgeEntered)
			// Split only the second wrapper at its exact authorize/run boundary so
			// its context.Done barrier cannot be triggered by authorization SQL.
			_, admitted, err := second.authorizeExplicitCleanup(ctx, f.target(), store_sqlite.UntrackAuthorizationForget)
			if err != nil {
				t.Fatal(err)
			}
			if admitted.AttemptID == "" {
				t.Fatal("authorization did not return durable attempt identity")
			}
			waiting := &cleanupExecutionWaitContext{Context: ctx, queued: make(chan struct{})}
			secondResult := make(chan error, 1)
			secondDone := make(chan struct{})
			go func() { defer close(secondDone); secondResult <- second.runSaga(waiting, admitted) }()
			t.Cleanup(func() { cancel(); allowPurge(); allowFinish(); awaitCleanupSignal(t, secondDone) })
			awaitCleanupSignal(t, waiting.queued)
			if hooks.releaseCalls.Load() != 0 {
				t.Fatal("second actor released graph while first was in purge hook")
			}
			if _, found, err := f.catalog.GetDedicatedGraph(ctx, f.graph.GraphID); err != nil || !found {
				t.Fatalf("graph vanished during first hook: found=%v err=%v", found, err)
			}
			allowPurge()
			awaitCleanupSignal(t, hooks.finishEntered)
			// The first actor has deleted its journal, but still owns its finish
			// callback. Gate release before this callback is an ABA ordering bug.
			cleanupExecutions.mu.Lock()
			entry := cleanupExecutions.entries[admitted.cleanupID()]
			refs, active := 0, false
			if entry != nil {
				refs, active = entry.refs, entry.active
			}
			cleanupExecutions.mu.Unlock()
			if refs != 2 || !active {
				t.Fatalf("execution released before final notification: refs=%d active=%v", refs, active)
			}
			allowFinish()
			awaitCleanupSignal(t, firstDone)
			awaitCleanupSignal(t, secondDone)
			if err := <-firstResult; err != nil {
				t.Fatalf("first completion: %v", err)
			}
			if err := <-secondResult; !errors.Is(err, store_sqlite.ErrCatalogStaleGuard) {
				t.Fatalf("queued old actor was not refused: %v", err)
			}
			if hooks.releaseCalls.Load() != 1 {
				t.Fatalf("joined actor duplicated release: %d", hooks.releaseCalls.Load())
			}
			f.install(t) // exact same checkout ID, incarnation, graph ID and prefix
			if err := second.runSaga(ctx, admitted); !errors.Is(err, store_sqlite.ErrCatalogStaleGuard) {
				t.Fatalf("old target replayed after retrack: %v", err)
			}
			if got, found, err := f.catalog.GetDedicatedGraph(ctx, f.graph.GraphID); err != nil || !found || got.State != "ready" {
				t.Fatalf("healthy retrack lost: %+v %v %v", got, found, err)
			}
			_, next, err := first.authorizeExplicitCleanup(ctx, f.target(), store_sqlite.UntrackAuthorizationForget)
			if err != nil {
				t.Fatal(err)
			}
			if next.AttemptID == admitted.AttemptID || next.AttemptID == "" {
				t.Fatal("same-ID retrack reused old cleanup attempt")
			}
			if err := second.runSaga(ctx, admitted); !errors.Is(err, store_sqlite.ErrCatalogStaleGuard) {
				t.Fatalf("old actor adopted successor journal: %v", err)
			}
			if _, err := f.catalog.GetCleanupAttempt(ctx, next.cleanupID(), next.AttemptID); err != nil {
				t.Fatal(err)
			}
			if err := first.runSaga(ctx, next); err != nil {
				t.Fatalf("successor's own authorized cleanup failed: %v", err)
			}
			if hooks.releaseCalls.Load() != 2 {
				t.Fatalf("unexpected graph release count: %d", hooks.releaseCalls.Load())
			}
			cleanupExecutions.mu.Lock()
			remaining := len(cleanupExecutions.entries)
			cleanupExecutions.mu.Unlock()
			if remaining != 0 {
				t.Fatalf("completed attempts retained gate state: %d", remaining)
			}
		})
	}
}

func TestCleanupAttemptLegacyRecoveryRetainsIdentityAcrossRetry(t *testing.T) {
	f := newCleanupAttemptIntegrationFixture(t)
	ctx := context.Background()
	hooks := &cleanupAttemptHooks{}
	hooks.failRelease.Store(true)
	r := &Reconciler{catalog: f.catalog, hooks: hooks, now: time.Now}
	target := f.target()
	payload, err := json.Marshal(target)
	if err != nil {
		t.Fatal(err)
	}
	legacy := store_sqlite.CleanupEntry{CleanupID: target.cleanupID(), OpaqueTargetIDs: string(payload), Reason: string(target.Kind), Phase: store_sqlite.CleanupPhasePending, LastProgress: 1}
	if err := f.catalog.UpsertCleanupEntry(ctx, legacy); err != nil {
		t.Fatal(err)
	}
	if err := r.Resume(ctx); !errors.Is(err, errCleanupAttemptRetry) {
		t.Fatalf("expected durable retry: %v", err)
	}
	standing, found, err := f.catalog.GetCleanupEntry(ctx, target.cleanupID())
	if err != nil || !found {
		t.Fatalf("failed cleanup lost journal: %v %v", found, err)
	}
	admitted, err := decodeSagaTarget(standing)
	if err != nil || admitted.AttemptID == "" {
		t.Fatalf("legacy retry has no identity: %+v %v", admitted, err)
	}
	hooks.failRelease.Store(false)
	next := &Reconciler{catalog: f.store.Catalog(), hooks: hooks, now: time.Now}
	if err := next.Resume(ctx); err != nil {
		t.Fatal(err)
	}
	if err := r.runSaga(ctx, admitted); !errors.Is(err, store_sqlite.ErrCatalogStaleGuard) {
		t.Fatalf("old resumed actor recreated completed journal: %v", err)
	}
}

func TestCleanupAttemptLeavesLayerPurgeJournalCompatibility(t *testing.T) {
	f := newCleanupAttemptIntegrationFixture(t)
	r := &Reconciler{catalog: f.catalog, hooks: &cleanupAttemptHooks{}, now: time.Now}
	target := sagaTarget{Kind: sagaPurgeLayers, CheckoutID: f.owner.CheckoutID, Incarnation: f.owner.Incarnation}
	if err := r.runSaga(context.Background(), target); err != nil {
		t.Fatal(err)
	}
	entry, found, err := f.catalog.GetCleanupEntry(context.Background(), target.cleanupID())
	if err != nil || !found || entry.Phase != store_sqlite.CleanupPhaseDone {
		t.Fatalf("purge done marker changed: %+v %v %v", entry, found, err)
	}
	decoded, err := decodeSagaTarget(entry)
	if err != nil || decoded.AttemptID != "" {
		t.Fatalf("unrelated purge gained execution protocol: %+v %v", decoded, err)
	}
}
