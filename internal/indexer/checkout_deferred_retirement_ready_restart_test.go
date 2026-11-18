package indexer

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

// Reopen the physical catalog with a served coordinator whose process-local
// caches/backlog have been lost, exactly the state durable discovery must heal.
func reopenedServedReadyRetirementFixture(t *testing.T) (*CheckoutLifecycle, *ViewBuildGate, *CheckoutCoordinator, map[string]int64) {
	t.Helper()
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "store.sqlite")
	store, err := store_sqlite.Open(path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	catalog := store.Catalog()
	const checkout = "served-ready-restart"
	const family = "served-ready-family"
	const graphID = "served-ready-graph"
	now := time.Now().Unix()
	require.NoError(t, catalog.UpsertRepositoryFamily(ctx, store_sqlite.RepositoryFamily{FamilyID: family, CommonDirIdentity: "served-ready-common", State: "ready", CreatedAt: now, LastSeen: now}))
	require.NoError(t, catalog.UpsertCheckout(ctx, store_sqlite.Checkout{CheckoutID: checkout, Incarnation: "served-ready-incarnation", FamilyID: family, State: store_sqlite.CheckoutStateReady, DesiredMode: store_sqlite.CheckoutModeAutomatic, EffectiveMode: store_sqlite.CheckoutModeAutomatic, LastSeen: now}))
	require.NoError(t, catalog.UpsertDedicatedGraph(ctx, store_sqlite.DedicatedGraph{GraphID: graphID, OwnerCheckoutID: checkout, RepoPrefix: "repo", FamilyID: family, State: "ready"}))
	ids := map[string]int64{}
	publish := func(label, kind string, base int64) int64 {
		t.Helper()
		id, h, err := store.BeginPayloadGeneration(ctx, store_sqlite.PayloadGenerationRequest{OwnerKind: checkoutLayerOwnerKind, GraphID: graphID, CheckoutID: checkout, LayerID: label, GenerationKind: kind, BaseGenerationID: base, TreeOID: "tree-" + label, CreatedAt: now})
		require.NoError(t, err)
		require.NoError(t, h.AddBatchChecked([]*graph.Node{{ID: "repo::" + label, Name: label, Kind: graph.KindFunction, RepoPrefix: "repo", FilePath: "repo/fixture.go"}}, nil))
		require.NoError(t, h.BatchUpsertSymbolFTS([]graph.SymbolFTSItem{{NodeID: "repo::" + label, Tokens: "restart ready " + label}}))
		require.NoError(t, h.Close())
		require.NoError(t, store.PublishPayloadGeneration(ctx, id, now))
		ids[label] = id
		return id
	}
	commit := publish("commit", CommitLayerGenerationKind, 0)
	publish("orphan", DirtyLayerGenerationKind, commit)
	publish("retained", DirtyLayerGenerationKind, commit)
	publish("leased", DirtyLayerGenerationKind, commit)
	routed := publish("routed", DirtyLayerGenerationKind, commit)
	require.NoError(t, catalog.UpsertCheckoutRoute(ctx, store_sqlite.CheckoutRoute{CheckoutID: checkout, GraphID: graphID, CommitGenerationID: commit, DirtyGenerationID: routed, State: store_sqlite.RouteActive}))
	require.NoError(t, store.Close())
	store, err = store_sqlite.Open(path)
	require.NoError(t, err)
	l := newGenerationRetirementLifecycle(store, time.Now())
	gate := NewViewBuildGate()
	gate.Open()
	l.SetBuildGate(gate)
	c := &CheckoutCoordinator{store: store, gate: gate, checkoutID: checkout, basePinned: commit, routedDirty: routed, retainedDirty: []retainedDirtyLayer{{key: "retained", generationID: ids["retained"]}}, backlog: map[int64]struct{}{}}
	l.coordinators = map[string]*CheckoutCoordinator{checkout: c}
	lease := l.leases.Acquire(ids["leased"])
	t.Cleanup(lease.Release)
	require.Empty(t, c.pendingRetirementGenerations(), "no RAM debt survives reconstruction")
	return l, gate, c, ids
}

func sweepUntilReadyOrphanRetired(t *testing.T, l *CheckoutLifecycle, id int64) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	for attempt := 0; attempt < 64 && ctx.Err() == nil; attempt++ {
		_, _, err := l.SweepDeferredRetirements(ctx)
		require.NoError(t, err)
		_, found, err := l.catalog.GetViewGeneration(ctx, id)
		require.NoError(t, err)
		if !found {
			return
		}
	}
	t.Fatalf("served Ready orphan %d was not rediscovered and retired", id)
}

func TestDeferredRetirementRediscoversReadyOrphanAfterServedRestart(t *testing.T) {
	l, _, _, ids := reopenedServedReadyRetirementFixture(t)
	beforeFTS, err := l.store.SymbolFTSStats(t.Context())
	require.NoError(t, err)
	require.Equal(t, int64(5), beforeFTS.Rows, "each fixture generation owns one physical FTS document")
	sweepUntilReadyOrphanRetired(t, l, ids["orphan"])
	afterFTS, err := l.store.SymbolFTSStats(t.Context())
	require.NoError(t, err)
	require.Equal(t, beforeFTS.Rows-1, afterFTS.Rows, "retirement must remove the orphan's physical shared-FTS document")
	require.Nil(t, l.store.AtGeneration(ids["orphan"]).GetNode("repo::orphan"), "physical node must be retired with its catalog row")
	for _, label := range []string{"commit", "routed", "retained", "leased"} {
		row, found, err := l.catalog.GetViewGeneration(t.Context(), ids[label])
		require.NoError(t, err)
		require.True(t, found, label)
		require.Equal(t, store_sqlite.ViewGenerationReady, row.State, label)
		require.NotNil(t, l.store.AtGeneration(ids[label]).GetNode("repo::"+label), label)
	}
	route, found, err := l.catalog.GetCheckoutRoute(t.Context(), "served-ready-restart")
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, ids["commit"], route.CommitGenerationID)
	require.Equal(t, ids["routed"], route.DirtyGenerationID)
	hits, err := l.store.AtGeneration(ids["routed"]).SearchSymbolsRepoScopedContext(t.Context(), "restart ready", []string{"repo"}, 10)
	require.NoError(t, err)
	require.NotEmpty(t, hits)
}

func TestDeferredReadyRediscoveryPreservesUngatedAndExplicitSweep(t *testing.T) {
	l, _, _, ids := reopenedServedReadyRetirementFixture(t)
	l.SetBuildGate(nil)
	_, _, err := l.SweepDeferredRetirements(t.Context())
	require.NoError(t, err)
	requireCatalogGenerationPresent(t, l.store, ids["orphan"])
	require.Empty(t, l.owed, "ungated discovery must retain its served-checkout exclusion")
	// Legacy explicit sweeping uses its original, conservative helper even when
	// this lifecycle later has a managed worker gate.
	gate := NewViewBuildGate()
	gate.Open()
	l.SetBuildGate(gate)
	candidates := l.orphanedGenerations(t.Context(), map[string]struct{}{"served-ready-restart": {}}, nil)
	require.NotContains(t, candidates, ids["orphan"])
}

func TestDeferredReadyRediscoveryWaitsForPublishRetainLane(t *testing.T) {
	l, gate, c, ids := reopenedServedReadyRetirementFixture(t)
	// The real coordinator holds this permit through publication and retention.
	// Do not depend on its optional holder/foreground hint: the lane itself must
	// exclude retirement while a newly Ready layer is not in the retained set.
	permit, err := gate.Acquire(t.Context(), ViewBuildInteractive)
	require.NoError(t, err)
	var once sync.Once
	release := func() { once.Do(permit) }
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	joined := false
	defer func() {
		cancel()
		release()
		if !joined {
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Error("retirement failed to join")
			}
		}
	}()
	go func() { _, _, err := l.SweepDeferredRetirements(ctx); done <- err }()
	require.Eventually(t, func() bool { return gate.Stats().BackgroundQueued == 1 }, 5*time.Second, time.Millisecond)
	requireCatalogGenerationPresent(t, l.store, ids["orphan"])
	require.NotNil(t, l.store.AtGeneration(ids["orphan"]).GetNode("repo::orphan"))
	select {
	case err := <-done:
		joined = true
		t.Fatalf("retirement crossed held publication lane: %v", err)
	default:
	}
	c.mu.Lock()
	c.retainedDirty = append(c.retainedDirty, retainedDirtyLayer{key: "just-published", generationID: ids["orphan"]})
	c.mu.Unlock()
	release()
	select {
	case err := <-done:
		joined = true
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("retirement did not resume after publication retained its layer")
	}
	require.True(t, l.deferredRetirementInUse(ids["orphan"]))
	requireCatalogGenerationPresent(t, l.store, ids["orphan"])
}
