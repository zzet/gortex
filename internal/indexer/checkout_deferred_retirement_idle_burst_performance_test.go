//go:build performance

package indexer

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// With nobody waiting, the worker's cadence keeps the writer busy: it
// re-polls at once after a productive burst, so over the drain the bursts
// hold the lane at least half the wall time. One 50 ms burst per one-second
// pass held it about five percent. The burst is shortened on purpose, so this
// pins the cadence between bursts, not the idle burst's length or its wide
// quanta (TestPerformanceIdleRetirementBurstDeliversWideQuantaPastTheBoundedBudget).
func TestPerformanceIdleRetirementWorkerCadenceHoldsTheLaneAtLeastHalfTheTime(t *testing.T) {
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
func TestPerformanceIdleRetirementBurstDeliversWideQuantaPastTheBoundedBudget(t *testing.T) {
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
func TestPerformanceAgedRetirementBetweenSavesKeepsTheBoundedBurst(t *testing.T) {
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
