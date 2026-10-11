package indexer

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/viewmetrics"
)

// gatedYoungDebtLifecycle is the daemon's shape — a build gate installed, so
// the sweep takes the gated branch — over one failed generation whose debt is
// already known and younger than the starvation limit.
func gatedYoungDebtLifecycle(t *testing.T) (*CheckoutLifecycle, int64) {
	t.Helper()
	store, id, started := failedRetirementFixture(t)
	l := newGenerationRetirementLifecycle(store, started)
	gate := NewViewBuildGate()
	gate.Open()
	l.SetBuildGate(gate)
	l.interactiveDemand = func() bool { return false }
	l.deferredRetirementEligibleSince.Store(time.Now().UnixNano())
	return l, id
}

func standDownDelta(before, after viewmetrics.Snapshot, reason string) int64 {
	key := viewmetrics.RetirementStandDownTotal + "{reason=" + reason + "}"
	return after.Counters[key] - before.Counters[key]
}

// With a build gate installed, a pass over known, young debt stands down
// before the catalog scan for candidates while an edit cycle holds the lane;
// the first sighting of debt still scans, to start its age.
func TestGatedDeferredRetirementDoesNotScanWhileAnEditCycleHoldsTheLane(t *testing.T) {
	l, _ := gatedYoungDebtLifecycle(t)
	l.analysisEditCycle = func() bool { return true }
	l.retireQuantum = func(context.Context, int64, func(int64) bool) (store_sqlite.PayloadRetirementProgress, error) {
		t.Fatal("a quantum started while an edit cycle held the lane")
		return store_sqlite.PayloadRetirementProgress{}, nil
	}
	scans, passes := deferredRetirementScans.Load(), deferredRetirementPassStandDowns.Load()
	before := viewmetrics.Read()
	for i := 0; i < 3; i++ {
		retired, pending, err := l.SweepDeferredRetirements(t.Context())
		require.NoError(t, err)
		require.Zero(t, retired)
		require.True(t, pending)
	}
	require.Zero(t, deferredRetirementScans.Load()-scans, "the gated sweep scanned the catalog while standing down")
	require.EqualValues(t, 3, deferredRetirementPassStandDowns.Load()-passes)
	require.EqualValues(t, 3, standDownDelta(before, viewmetrics.Read(), "edit_cycle"))

	l.deferredRetirementEligibleSince.Store(0)
	_, pending, err := l.SweepDeferredRetirements(t.Context())
	require.NoError(t, err)
	require.True(t, pending)
	require.EqualValues(t, 1, deferredRetirementScans.Load()-scans, "the first sighting of debt must scan")
	require.Positive(t, l.deferredRetirementEligibleSince.Load())
}

// With a build gate installed, nothing of the sweep — not the scan, not a
// quantum — runs inside the edit idle over known, young debt; after it the
// sweep runs a burst.
func TestGatedDeferredRetirementWaitsForTheEditIdle(t *testing.T) {
	l, id := gatedYoungDebtLifecycle(t)
	last := time.Now().Add(-5 * time.Second)
	l.foregroundWork = func() (string, time.Time) { return "", last }
	quanta := 0
	l.retireQuantum = func(ctx context.Context, generationID int64, inUse func(int64) bool) (store_sqlite.PayloadRetirementProgress, error) {
		quanta++
		return l.store.RetirePayloadGenerationQuantum(ctx, generationID, inUse)
	}
	scans := deferredRetirementScans.Load()
	before := viewmetrics.Read()
	_, pending, err := l.SweepDeferredRetirements(t.Context())
	require.NoError(t, err)
	require.True(t, pending)
	require.Zero(t, quanta)
	require.Zero(t, deferredRetirementScans.Load()-scans, "the gated sweep scanned for candidates inside the edit idle")
	require.EqualValues(t, 1, standDownDelta(before, viewmetrics.Read(), "edit_idle"))

	last = time.Now().Add(-deferredRetirementEditIdle - time.Second)
	_, _, err = l.SweepDeferredRetirements(t.Context())
	require.NoError(t, err)
	require.Positive(t, quanta, "after the edit idle the sweep must serve a burst")
	requireGenerationStateWithPayloadOrRetired(t, l.store, id)
}

// requireGenerationStateWithPayloadOrRetired accepts any progress: the burst
// either fenced the generation or removed it.
func requireGenerationStateWithPayloadOrRetired(t *testing.T, store *store_sqlite.Store, id int64) {
	t.Helper()
	row, found, err := store.Catalog().GetViewGeneration(t.Context(), id)
	require.NoError(t, err)
	if found {
		require.Equal(t, store_sqlite.ViewGenerationRetiring, row.State)
	}
}

// Aged debt skips the foreground stand-down, a chain fold's included, so
// unrelated work still drains beside a long fold — but at one bounded burst
// per second: the fold gives its writer to every waiter, and back-to-back
// bursts would cancel its steps one quantum at a time.
func TestAgedRetirementBesideAChainFoldKeepsOneBurstPerSecond(t *testing.T) {
	l, _ := gatedYoungDebtLifecycle(t)
	l.deferredRetirementEligibleSince.Store(time.Now().Add(-3 * time.Minute).UnixNano())
	l.foldInFlight = func() bool { return true }
	bursts := 0
	l.retireQuantum = func(context.Context, int64, func(int64) bool) (store_sqlite.PayloadRetirementProgress, error) {
		bursts++
		return store_sqlite.PayloadRetirementProgress{ChunksCommitted: 1, RowsDeleted: 16}, store_sqlite.ErrPayloadSweepBudgetExhausted
	}
	before := viewmetrics.Read()
	_, pending, err := l.SweepDeferredRetirements(t.Context())
	require.NoError(t, err)
	require.True(t, pending)
	require.Positive(t, bursts, "aged debt beside a fold got no burst")
	served := bursts
	scans := deferredRetirementScans.Load()
	_, pending, err = l.SweepDeferredRetirements(t.Context())
	require.NoError(t, err)
	require.True(t, pending)
	require.Equal(t, served, bursts, "a second burst ran beside the fold within the second")
	require.Equal(t, scans, deferredRetirementScans.Load(), "the paced pass scanned the catalog")
	require.EqualValues(t, 1, standDownDelta(before, viewmetrics.Read(), "chain_fold"))
	// A second later the fold gets company again.
	l.deferredRetirementFoldBurstAt.Store(time.Now().Add(-deferredRetirementFoldBurstEvery).UnixNano())
	_, _, err = l.SweepDeferredRetirements(t.Context())
	require.NoError(t, err)
	require.Greater(t, bursts, served)
}
