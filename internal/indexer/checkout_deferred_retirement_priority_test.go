package indexer

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

func TestNextDeferredRetirementContinuesBelowCompletedCursor(t *testing.T) {
	l := &CheckoutLifecycle{}
	l.deferredRetirementCursor = 115
	if got := l.nextDeferredRetirement([]int64{130, 120, 110, 100}); got != 110 {
		t.Fatalf("next retirement = %d, want 110", got)
	}
	// A new newest generation must not reset progress toward the old tail.
	if got := l.nextDeferredRetirement([]int64{140, 130, 120, 100}); got != 100 {
		t.Fatalf("next retirement after new arrival = %d, want 100", got)
	}
	if got := l.nextDeferredRetirement([]int64{140, 130, 120}); got != 140 {
		t.Fatalf("wrapped retirement = %d, want 140", got)
	}
}

func TestHasDeferredRetirementWorkRechecksOwedAndCoordinatorBacklog(t *testing.T) {
	l := &CheckoutLifecycle{
		coordinators: map[string]*CheckoutCoordinator{},
		owed:         map[int64]struct{}{},
	}
	if l.hasDeferredRetirementWork() {
		t.Fatal("empty lifecycle reported pending retirement")
	}
	l.owed[41] = struct{}{}
	if !l.hasDeferredRetirementWork() {
		t.Fatal("owed retirement was not observed")
	}
	delete(l.owed, 41)
	l.coordinators["checkout"] = &CheckoutCoordinator{backlog: map[int64]struct{}{42: {}}}
	if !l.hasDeferredRetirementWork() {
		t.Fatal("coordinator backlog was not observed")
	}
}

func TestReconcileFamilyDeferredRetirementDoesNotEnterPhysicalSweep(t *testing.T) {
	f := newLifecycleFixture(t)
	defer f.close()
	ctx := context.Background()
	root := f.gitRepo("deferred-topology-nudge")
	tracked, err := f.lc.Register(ctx, config.RepoEntry{Path: root, Name: "deferred-topology-nudge"}, TrackSourceCLI)
	require.NoError(t, err)
	require.NoError(t, tracked.CatalogErr)

	const owedGeneration = int64(987654321)
	f.lc.coordMu.Lock()
	f.lc.owed[owedGeneration] = struct{}{}
	f.lc.coordMu.Unlock()

	// Holding the physical-sweep lock makes accidental use of the synchronous
	// admin path deterministic: the call cannot return until this test unlocks.
	f.lc.retirementSweepMu.Lock()
	locked := true
	t.Cleanup(func() {
		if locked {
			f.lc.retirementSweepMu.Unlock()
		}
	})
	done := make(chan error, 1)
	go func() {
		_, err := f.lc.ReconcileFamilyDeferredRetirement(ctx, tracked.FamilyID)
		done <- err
	}()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		f.lc.retirementSweepMu.Unlock()
		locked = false
		t.Fatal("topology reconciliation waited for the physical retirement sweep")
	}
	f.lc.retirementSweepMu.Unlock()
	locked = false

	f.lc.coordMu.Lock()
	_, retained := f.lc.owed[owedGeneration]
	delete(f.lc.owed, owedGeneration)
	f.lc.coordMu.Unlock()
	require.True(t, retained, "topology reconciliation must leave retirement work for the worker")
}

func TestDeferredDiscoveryReoffersServedCoordinatorBacklogAfterRestart(t *testing.T) {
	l := &CheckoutLifecycle{now: time.Now}
	list := func(_ context.Context, filter store_sqlite.ViewGenerationFilter) ([]store_sqlite.ViewGeneration, error) {
		if len(filter.States) != 2 {
			return nil, nil
		}
		return []store_sqlite.ViewGeneration{
			{GenerationID: 52, CheckoutID: "served", State: store_sqlite.ViewGenerationRetiring},
			{GenerationID: 51, CheckoutID: "served", State: store_sqlite.ViewGenerationSuperseded},
		}, nil
	}
	got, err := l.discoverDeferredRetirementsWith(
		context.Background(), map[string]struct{}{"served": {}}, nil, list, nil,
	)
	require.NoError(t, err)
	require.Equal(t, []int64{52, 51}, got)
}

func TestDeferredRetirementInUsePreservesLiveCoordinatorAncestry(t *testing.T) {
	const (
		retainedCommit = int64(61)
		retainedDirty  = int64(62)
		pinnedBase     = int64(63)
		routedDirty    = int64(64)
	)
	coordinator := &CheckoutCoordinator{
		retained:      []retainedCommitLayer{{generationID: retainedCommit}},
		retainedDirty: []retainedDirtyLayer{{generationID: retainedDirty}},
		basePinned:    pinnedBase,
		routedDirty:   routedDirty,
	}
	l := &CheckoutLifecycle{
		coordinators: map[string]*CheckoutCoordinator{"served": coordinator},
	}
	for _, generationID := range []int64{retainedCommit, retainedDirty, pinnedBase, routedDirty} {
		require.True(t, l.deferredRetirementInUse(generationID),
			"live coordinator ancestry generation %d was not protected", generationID)
	}
	require.False(t, l.deferredRetirementInUse(65),
		"unrelated generation was reported in use")
}

func TestSweepDeferredRetirementLeavesPhysicalBacklogForWorker(t *testing.T) {
	f := newLifecycleFixture(t)
	defer f.close()
	ctx := context.Background()
	root := f.gitRepo("deferred-janitor")
	tracked, err := f.lc.Register(ctx, config.RepoEntry{Path: root, Name: "deferred-janitor"}, TrackSourceCLI)
	require.NoError(t, err)
	require.NoError(t, tracked.CatalogErr)

	const owedGeneration = int64(987654322)
	f.lc.coordMu.Lock()
	f.lc.owed[owedGeneration] = struct{}{}
	f.lc.coordMu.Unlock()

	f.lc.retirementSweepMu.Lock()
	locked := true
	t.Cleanup(func() {
		if locked {
			f.lc.retirementSweepMu.Unlock()
		}
	})
	done := make(chan error, 1)
	go func() {
		_, err := f.lc.SweepDeferredRetirement(ctx)
		done <- err
	}()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		f.lc.retirementSweepMu.Unlock()
		locked = false
		t.Fatal("deferred janitor waited for the physical retirement sweep")
	}
	f.lc.retirementSweepMu.Unlock()
	locked = false

	f.lc.coordMu.Lock()
	_, retained := f.lc.owed[owedGeneration]
	delete(f.lc.owed, owedGeneration)
	f.lc.coordMu.Unlock()
	require.True(t, retained, "deferred janitor must leave retirement work for the worker")
}

func TestDeferredRetirementInUseIsPendingAndEligibleSiblingAdvances(t *testing.T) {
	f := newLifecycleFixture(t)
	defer f.close()
	ctx := context.Background()

	publish := func(layer string) int64 {
		t.Helper()
		generationID, _, err := f.lc.store.BeginPayloadGeneration(ctx, store_sqlite.PayloadGenerationRequest{
			OwnerKind:      refViewOwnerKind,
			GraphID:        "graph-retirement-in-use",
			LayerID:        layer,
			GenerationKind: CommitLayerGenerationKind,
			TreeOID:        "tree-" + layer,
			CreatedAt:      time.Now().Unix(),
		})
		require.NoError(t, err)
		require.NoError(t, f.lc.store.PublishPayloadGeneration(ctx, generationID, time.Now().Unix()))
		require.NoError(t, f.lc.store.MarkPayloadGenerationSuperseded(ctx, generationID))
		return generationID
	}

	eligible := publish("eligible")
	protected := publish("protected") // newer: selected first by the retirement cursor
	guard := &CheckoutCoordinator{
		retained: []retainedCommitLayer{{generationID: protected}},
	}
	f.lc.coordMu.Lock()
	f.lc.coordinators["retained-ancestry-guard"] = guard
	f.lc.owed[protected] = struct{}{}
	f.lc.owed[eligible] = struct{}{}
	f.lc.coordMu.Unlock()

	retired, pending, err := f.lc.SweepDeferredRetirements(ctx)
	require.NoError(t, err, "an expected in-use refusal is pending work, not an operational failure")
	require.Zero(t, retired)
	require.True(t, pending)
	_, found, err := f.lc.catalog.GetViewGeneration(ctx, protected)
	require.NoError(t, err)
	require.True(t, found, "protected generation must not be reported or removed as retired")

	retired, pending, err = f.lc.SweepDeferredRetirements(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, retired, "cursor must advance to the eligible sibling")
	require.True(t, pending, "protected generation remains queued")
	_, found, err = f.lc.catalog.GetViewGeneration(ctx, eligible)
	require.NoError(t, err)
	require.False(t, found, "eligible sibling was not physically retired")

	f.lc.coordMu.Lock()
	_, protectedOwed := f.lc.owed[protected]
	_, eligibleOwed := f.lc.owed[eligible]
	delete(f.lc.owed, protected)
	delete(f.lc.owed, eligible)
	delete(f.lc.coordinators, "retained-ancestry-guard")
	f.lc.coordMu.Unlock()
	require.True(t, protectedOwed)
	require.False(t, eligibleOwed)
}
