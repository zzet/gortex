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

// A quiet checkout selects wide quanta; the gaps between foreground saves
// retain narrow quanta. Assert committed row policy without a host-time gate.
func TestRetirementBurstQuantumPolicy(t *testing.T) {
	for _, recentForeground := range []bool{false, true} {
		t.Run(fmt.Sprintf("recent_foreground=%t", recentForeground), func(t *testing.T) {
			// Four initial wide chunks (128 rows), or 32 narrow chunks
			// (16 rows), exercise row-width policy without the large
			// workload needed by the opt-in wall-time measurements.
			const nodes = 4 * 128
			l, _, _ := idleRetirementLifecycle(t, 1, nodes)
			l.deferredRetirementEligibleSince.Store(time.Now().Add(-3 * time.Minute).UnixNano())
			if recentForeground {
				l.foregroundWork = func() (string, time.Time) { return "", time.Now().Add(-500 * time.Millisecond) }
			}
			core, logs := observer.New(zap.InfoLevel)
			l.logger = zap.New(core)
			var rows, quanta int64
			// An interrupted quantum may only advance an empty-table cursor.
			// Resume until actual rows are observed; do not demand that the
			// first timed quantum commits payload work on a loaded host.
			for pass := 0; pass < 64 && rows == 0; pass++ {
				logs.TakeAll()
				_, _, err := l.SweepDeferredRetirements(t.Context())
				require.NoError(t, err)
				bursts := logs.FilterMessage("indexer: deferred retirement committed a bounded burst").All()
				for _, entry := range bursts {
					fields := entry.ContextMap()
					if deleted := fields["rows_deleted"].(int64); deleted > 0 {
						rows += deleted
						quanta += fields["chunks_committed"].(int64)
					}
				}
			}
			require.Positive(t, rows, "bounded resumptions must reach actual payload rows")
			if recentForeground {
				require.LessOrEqual(t, rows, 16*quanta, "a burst between saves ran wide quanta")
			} else {
				require.Greater(t, rows, 16*quanta, "the idle burst ran narrow quanta")
			}
		})
	}
}

// A burst nobody waits on gives the lane to a queued edit at the next quantum
// boundary once its bounded budget has elapsed.
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
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	quantumStarted, releaseQuantum := make(chan struct{}), make(chan struct{})
	var release sync.Once
	unpark := func() { release.Do(func() { close(releaseQuantum) }) }
	var wg sync.WaitGroup
	defer func() { cancel(); unpark(); wg.Wait() }()
	// Exercise boundary ordering, not SQLite or host scheduling latency. The
	// extended burst still has its normal idle budget; only the bounded-budget
	// comparison is shortened through the existing fixture seam.
	l.retirementBurst = time.Nanosecond
	quantaSeen := 0
	var committed store_sqlite.PayloadRetirementProgress
	l.retireQuantum = func(ctx context.Context, id int64, inUse func(int64) bool) (store_sqlite.PayloadRetirementProgress, error) {
		quantaSeen++
		if quantaSeen != 1 {
			return store_sqlite.PayloadRetirementProgress{}, fmt.Errorf("retirement began another quantum while a build was queued")
		}
		close(quantumStarted)
		select {
		case <-releaseQuantum:
		case <-ctx.Done():
			return store_sqlite.PayloadRetirementProgress{}, ctx.Err()
		}
		progress, err := l.store.RetirePayloadGenerationQuantum(ctx, id, inUse)
		committed = progress
		return progress, err
	}
	sweepDone := make(chan error, 1)
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, _, err := l.SweepDeferredRetirements(ctx)
		sweepDone <- err
	}()
	bursting := func() bool {
		st := gate.Stats()
		return st.Active && st.Holder != nil && st.Holder.Kind == "payload_retirement"
	}
	select {
	case <-quantumStarted:
	case <-ctx.Done():
		t.Fatal("retirement did not enter its first quantum")
	}
	require.True(t, bursting())
	buildDone := make(chan error, 1)
	wg.Add(1)
	go func() {
		defer wg.Done()
		permit, err := gate.Acquire(ctx, priority)
		if err == nil {
			permit()
		}
		buildDone <- err
	}()
	require.Eventually(t, func() bool {
		stats := gate.Stats()
		return stats.InteractiveQueued+stats.BackgroundQueued == 1
	}, 30*time.Second, time.Millisecond)
	unpark()
	select {
	case err := <-buildDone:
		require.NoError(t, err)
	case <-ctx.Done():
		t.Fatal("queued build did not acquire at the quantum boundary")
	}
	select {
	case err := <-sweepDone:
		require.NoError(t, err)
	case <-ctx.Done():
		t.Fatal("retirement did not finish after yielding")
	}
	wg.Wait()
	require.Equal(t, 1, quantaSeen, "retirement must not begin another quantum before yielding")
	require.Positive(t, committed.ChunksCommitted, "the real in-flight quantum must commit before yielding")
	// Empty-table cursor proofs are committed progress too: a bounded
	// quantum may yield before reaching its first table with payload rows.
	require.Positive(t, committed.RowsDeleted+int64(committed.StepsAdvanced),
		"the committed quantum must delete payload or advance its resume cursor")
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
