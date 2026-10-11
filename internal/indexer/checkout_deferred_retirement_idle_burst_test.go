package indexer

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

// idleRetirementLifecycle is the daemon's gated shape over a backlog of
// failed generations, with no foreground work at all.
func idleRetirementLifecycle(t *testing.T, generations, nodes int) (*CheckoutLifecycle, *ViewBuildGate, []int64) {
	t.Helper()
	fixture := newSparseBuildFlightFixture(t)
	template := payloadRequestForBuild(fixture.request)
	l := newGenerationRetirementLifecycle(fixture.store, time.Now())
	gate := NewViewBuildGate()
	gate.Open()
	l.SetBuildGate(gate)
	l.foregroundWork = func() (string, time.Time) { return "", time.Time{} }
	var ids []int64
	for i := 0; i < generations; i++ {
		id := seedRetirementPayload(t, fixture.store, template, fmt.Sprintf("idle-%d", i), nodes, 0)
		ids = append(ids, id)
		l.owed[id] = struct{}{}
	}
	return l, gate, ids
}

// With nobody waiting, the worker's cadence keeps the writer busy: it
// re-polls at once after a productive burst, so over the drain the bursts
// hold the lane at least half the wall time. One 50 ms burst per one-second
// pass held it about five percent. The burst is shortened on purpose, so this
// pins the cadence between bursts, not the idle burst's length or its wide
// quanta (TestIdleRetirementBurstDeliversWideQuantaPastTheBoundedBudget).
func TestIdleRetirementWorkerCadenceHoldsTheLaneAtLeastHalfTheTime(t *testing.T) {
	generations, nodes, burst := 8, 2000, deferredRetirementBurstDuration+10*time.Millisecond
	if indexerRaceDetectorOn {
		// The race detector instruments the pure-Go SQLite: a statement runs
		// tens of times slower, so a burst must be long enough to commit.
		generations, nodes, burst = 4, 1000, 10*deferredRetirementBurstDuration
	}
	l, _, ids := idleRetirementLifecycle(t, generations, nodes)
	// Short idle bursts make the fixture's drain many passes long, so the
	// cadence between passes is what the duty measures.
	l.retirementIdleBurst = burst
	core, logs := observer.New(zap.InfoLevel)
	l.logger = zap.New(core)
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	started := time.Now()
	DeferredRetirementLoop{
		Sweep:     l.SweepDeferredRetirements,
		Logger:    zap.NewNop(),
		BasePause: time.Second, MaxBackoff: time.Minute,
		Progress: DeferredRetirementProgress,
	}.Run(ctx)
	wall := time.Since(started)
	require.NoError(t, ctx.Err(), "the drain did not finish")
	for _, id := range ids {
		requireGenerationRetired(t, l.store, id)
	}
	var held time.Duration
	var rows int64
	bursts := logs.FilterMessage("indexer: deferred retirement committed a bounded burst").All()
	require.GreaterOrEqual(t, len(bursts), 3, "the drain must span several passes for the cadence to matter")
	for _, entry := range bursts {
		fields := entry.ContextMap()
		held += fields["lane_hold"].(time.Duration)
		rows += fields["rows_deleted"].(int64)
	}
	duty := held.Seconds() / wall.Seconds()
	t.Logf("idle drain: rows=%d wall=%s lane_hold=%s duty=%.2f rows_per_s=%.0f", rows, wall, held, duty, float64(rows)/wall.Seconds())
	require.GreaterOrEqual(t, duty, 0.5, "the idle drain held the lane %.0f%% of the time", 100*duty)
}

// One burst nobody waits on, at the production idle budget, runs past the
// bounded 50 ms and deletes in wide quanta — more than one 16-row chunk per
// transaction. The cadence pin above shortens the burst to measure the time
// between bursts; this one measures what a production idle burst delivers.
func TestIdleRetirementBurstDeliversWideQuantaPastTheBoundedBudget(t *testing.T) {
	nodes := 40000
	if indexerRaceDetectorOn {
		// The instrumented pure-Go SQLite deletes tens of times slower.
		nodes = 4000
	}
	l, _, _ := idleRetirementLifecycle(t, 1, nodes)
	l.deferredRetirementEligibleSince.Store(time.Now().Add(-3 * time.Minute).UnixNano())
	core, logs := observer.New(zap.InfoLevel)
	l.logger = zap.New(core)
	_, _, err := l.SweepDeferredRetirements(t.Context())
	require.NoError(t, err)
	bursts := logs.FilterMessage("indexer: deferred retirement committed a bounded burst").All()
	require.Len(t, bursts, 1)
	fields := bursts[0].ContextMap()
	hold := fields["lane_hold"].(time.Duration)
	rows, quanta := fields["rows_deleted"].(int64), fields["chunks_committed"].(int64)
	retired := fields["retired_generations"].(int64)
	t.Logf("idle burst: hold=%s rows=%d quanta=%d rows_per_quantum=%.1f rows_per_s=%.0f retired=%d",
		hold, rows, quanta, float64(rows)/float64(max(quanta, 1)), float64(rows)/hold.Seconds(), retired)
	if retired == 0 {
		require.Greater(t, hold, 4*deferredRetirementBurstDuration, "a burst nobody waits on ended at its bounded budget")
	}
	// A narrow quantum deletes at most one 16-row chunk.
	require.Greater(t, rows, 16*quanta, "the idle burst ran narrow quanta")
}

// Aged debt between the saves of a steady stream — foreground work seen less
// than deferredRetirementStarvedIdle ago, nothing queued at this instant —
// keeps the bounded burst and narrow quanta: the long slice is for a quiet
// checkout, not for the gaps of an edit stream.
func TestAgedRetirementBetweenSavesKeepsTheBoundedBurst(t *testing.T) {
	nodes := 40000
	if indexerRaceDetectorOn {
		// The instrumented pure-Go SQLite deletes tens of times slower.
		nodes = 4000
	}
	l, _, _ := idleRetirementLifecycle(t, 1, nodes)
	l.deferredRetirementEligibleSince.Store(time.Now().Add(-3 * time.Minute).UnixNano())
	l.foregroundWork = func() (string, time.Time) { return "", time.Now().Add(-500 * time.Millisecond) }
	core, logs := observer.New(zap.InfoLevel)
	l.logger = zap.New(core)
	_, _, err := l.SweepDeferredRetirements(t.Context())
	require.NoError(t, err)
	bursts := logs.FilterMessage("indexer: deferred retirement committed a bounded burst").All()
	require.Len(t, bursts, 1)
	fields := bursts[0].ContextMap()
	hold := fields["lane_hold"].(time.Duration)
	rows, quanta := fields["rows_deleted"].(int64), fields["chunks_committed"].(int64)
	t.Logf("burst between saves: hold=%s rows=%d quanta=%d", hold, rows, quanta)
	require.LessOrEqual(t, rows, 16*quanta, "a burst between saves ran wide quanta")
	if !indexerRaceDetectorOn {
		require.Less(t, hold, 4*deferredRetirementBurstDuration, "a burst between saves ran past its bounded budget")
	}
}

// A burst nobody waits on runs past its bounded budget, and still gives the
// lane to an edit that queues within 100 ms.
func TestExtendedRetirementBurstGivesTheLaneToAQueuedEdit(t *testing.T) {
	extendedBurstYieldsTo(t, ViewBuildInteractive)
}

// The same holds for a build that queues at background priority: a
// watcher-driven edit cycle with no ticket, or a chain fold.
func TestExtendedRetirementBurstGivesTheLaneToAQueuedBackgroundBuild(t *testing.T) {
	extendedBurstYieldsTo(t, ViewBuildBackground)
}

func extendedBurstYieldsTo(t *testing.T, priority ViewBuildPriority) {
	t.Helper()
	nodes := 40000
	if indexerRaceDetectorOn {
		// The instrumented pure-Go SQLite deletes tens of times slower.
		nodes = 4000
	}
	l, gate, ids := idleRetirementLifecycle(t, 1, nodes)
	l.deferredRetirementEligibleSince.Store(time.Now().Add(-3 * time.Minute).UnixNano())
	core, logs := observer.New(zap.InfoLevel)
	l.logger = zap.New(core)
	defer func() {
		if t.Failed() {
			for _, entry := range logs.All() {
				t.Logf("%s %v", entry.Message, entry.ContextMap())
			}
		}
	}()
	ctx, cancel := context.WithCancel(t.Context())
	var wg sync.WaitGroup
	defer func() { cancel(); wg.Wait() }()
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, _, _ = l.SweepDeferredRetirements(ctx)
	}()
	bursting := func() bool {
		st := gate.Stats()
		return st.Active && st.Holder != nil && st.Holder.Kind == "payload_retirement"
	}
	require.Eventually(t, bursting, 30*time.Second, time.Millisecond)
	burstSeen := time.Now()
	time.Sleep(4 * deferredRetirementBurstDuration)
	require.True(t, bursting(), "a burst nobody waits on ended at its bounded budget")
	queued := time.Now()
	permit, err := gate.Acquire(ctx, priority)
	require.NoError(t, err)
	waited := time.Since(queued)
	permit()
	t.Logf("%v build waited %s after a burst running %s", priority, waited, queued.Sub(burstSeen))
	// The bound the bounded burst keeps for a queued edit.
	require.LessOrEqual(t, waited, 100*time.Millisecond)
	cancel()
	wg.Wait()
	requireCatalogGenerationPresentOrRetiring(t, l.store, ids[0])
}

// A burst runs the retirement preamble (reference check, fence transaction,
// writer drain) once per generation it serves, however many quanta follow.
// A burst is 50 ms of 16-row quanta here; a loaded race-detector run may
// commit one quantum in it, which proves nothing, so passes repeat until a
// burst has run several, and every burst is held to the bound.
func TestRetirementBurstFencesEachGenerationOnce(t *testing.T) {
	l, _, _ := idleRetirementLifecycle(t, 1, 2000)
	l.deferredRetirementEligibleSince.Store(time.Now().Add(-3 * time.Minute).UnixNano())
	// A waiting writer keeps the burst to its bounded budget of 16-row quanta.
	l.interactiveDemand = func() bool { return true }
	core, logs := observer.New(zap.InfoLevel)
	l.logger = zap.New(core)
	several := false
	for pass := 0; pass < 20 && !several; pass++ {
		seen := logs.Len()
		preambles := l.store.RetirementPreambles()
		_, _, err := l.SweepDeferredRetirements(t.Context())
		require.NoError(t, err)
		bursts := logs.FilterMessage("indexer: deferred retirement committed a bounded burst").All()
		if logs.Len() == seen || len(bursts) == 0 {
			continue
		}
		fields := bursts[len(bursts)-1].ContextMap()
		require.EqualValues(t, fields["generations_touched"], l.store.RetirementPreambles()-preambles,
			"a burst re-ran the preamble for a generation it had already fenced")
		several = fields["chunks_committed"].(int64) > 1
	}
	require.True(t, several, "no burst ran more than one quantum")
}

func requireCatalogGenerationPresentOrRetiring(t *testing.T, store *store_sqlite.Store, id int64) {
	t.Helper()
	row, found, err := store.Catalog().GetViewGeneration(context.Background(), id)
	require.NoError(t, err)
	if found {
		require.Equal(t, store_sqlite.ViewGenerationRetiring, row.State)
	}
}
