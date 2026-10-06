package indexer

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

func agedRetirementFixture(t *testing.T) (*CheckoutLifecycle, *ViewBuildGate, int64) {
	t.Helper()
	store, id, started := failedRetirementFixture(t)
	l := newGenerationRetirementLifecycle(store, started)
	gate := NewViewBuildGate()
	gate.Open()
	l.SetBuildGate(gate)
	l.foregroundWork = func() (string, time.Time) { return "refresh_ticket", time.Now() }
	l.interactiveDemand = func() bool { return true }
	l.deferredRetirementEligibleSince.Store(time.Now().Add(-3 * time.Minute).UnixNano())
	return l, gate, id
}

func TestDeferredRetirementDiscoversAndAgesDebtDuringPendingRequests(t *testing.T) {
	l, _, id := agedRetirementFixture(t)
	l.deferredRetirementEligibleSince.Store(0)
	retired, pending, err := l.SweepDeferredRetirements(t.Context())
	require.NoError(t, err)
	require.Zero(t, retired)
	require.True(t, pending)
	require.Positive(t, l.deferredRetirementEligibleSince.Load())
	require.Zero(t, l.deferredRetirementProgress.Load(), "discovery is not committed retirement progress")
	requireGenerationStateWithPayload(t, l.store, id, store_sqlite.ViewGenerationFailed)
	// This is the same continuing debt, rather than a new idle window for
	// each tiny commit. Pending tickets remain present throughout service.
	l.deferredRetirementEligibleSince.Store(time.Now().Add(-3 * time.Minute).UnixNano())
	core, logs := observer.New(zap.InfoLevel)
	l.logger = zap.New(core)
	for attempt := 0; attempt < 20; attempt++ {
		_, pending, err = l.SweepDeferredRetirements(t.Context())
		require.NoError(t, err)
		if !pending {
			break
		}
	}
	require.False(t, pending)
	requireGenerationRetired(t, l.store, id)
	require.Zero(t, l.deferredRetirementEligibleSince.Load())
	entries := logs.FilterMessage("indexer: deferred retirement committed a bounded burst").All()
	require.NotEmpty(t, entries)
	var chunks int64
	for _, entry := range entries {
		fields := entry.ContextMap()
		chunks += fields["chunks_committed"].(int64)
		t.Logf("bounded_service=%v", fields)
	}
	require.Greater(t, chunks, int64(1), "service must advance more than one quantum per quiet permit")
}

func TestAgedRetirementUsesBoundedBudgetBeforeQueuedEditGetsTheLane(t *testing.T) {
	l, gate, id := agedRetirementFixture(t)
	// Keep a permanent interactive waiter while an aged background request
	// qualifies through the existing fairness rule; no new scheduler path.
	gate.backgroundStarvation = 0
	hold, err := gate.Acquire(t.Context(), ViewBuildInteractive)
	require.NoError(t, err)
	gate.mu.Lock()
	gate.interactiveBurst = maxInteractiveBuildBurst
	gate.mu.Unlock()
	var release sync.Once
	unpark := func() { release.Do(hold) }
	ctx, cancel := context.WithCancel(t.Context())
	sweepDone := make(chan error, 1)
	editDone := make(chan time.Time, 1)
	sweepJoined, editJoined := false, false
	defer func() {
		cancel()
		unpark()
		if !sweepJoined {
			select {
			case <-sweepDone:
			case <-time.After(5 * time.Second):
				t.Error("retirement did not join")
			}
		}
		if !editJoined {
			select {
			case <-editDone:
			case <-time.After(5 * time.Second):
				t.Error("edit admission did not join")
			}
		}
	}()
	var committed store_sqlite.PayloadRetirementProgress
	l.retireQuantum = func(ctx context.Context, generationID int64, inUse func(int64) bool) (store_sqlite.PayloadRetirementProgress, error) {
		if generationID != id {
			return store_sqlite.PayloadRetirementProgress{}, fmt.Errorf("wrong retirement generation: %d", generationID)
		}
		progress, err := l.store.RetirePayloadGenerationQuantum(ctx, generationID, inUse)
		committed.ChunksCommitted += progress.ChunksCommitted
		committed.RowsDeleted += progress.RowsDeleted
		return progress, err
	}
	go func() { _, _, err := l.SweepDeferredRetirements(ctx); sweepDone <- err }()
	require.Eventually(t, func() bool { return gate.Stats().BackgroundQueued == 1 }, 5*time.Second, time.Millisecond)
	go func() {
		permit, err := gate.Acquire(ctx, ViewBuildInteractive)
		if err == nil {
			editDone <- time.Now()
			permit()
		} else {
			editDone <- time.Time{}
		}
	}()
	require.Eventually(t, func() bool { return gate.Stats().InteractiveQueued == 1 }, 5*time.Second, time.Millisecond)
	from := time.Now()
	unpark()
	select {
	case at := <-editDone:
		editJoined = true
		require.False(t, at.IsZero())
		require.LessOrEqual(t, at.Sub(from), 100*time.Millisecond)
	case <-time.After(5 * time.Second):
		t.Fatal("queued edit did not acquire after bounded service")
	}
	select {
	case err := <-sweepDone:
		sweepJoined = true
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("bounded service did not finish")
	}
	require.Positive(t, committed.ChunksCommitted)
	require.LessOrEqual(t, committed.RowsDeleted, int64(64))
	// Completion may clear debt; partial work must keep its original age.
	if l.hasDeferredRetirementWork() {
		require.True(t, l.retirementDebtAged(time.Now()))
	}
}

func TestAgedRetirementCannotEnterAnActualHeldEditCycle(t *testing.T) {
	l, gate, id := agedRetirementFixture(t)
	permit, err := gate.Acquire(t.Context(), ViewBuildInteractive)
	require.NoError(t, err)
	clear := gate.NoteHolder(ViewBuildLaneHolder{Kind: "checkout_mutation", Priority: "interactive"})
	var release sync.Once
	unpark := func() { release.Do(func() { clear(); permit() }) }
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	joined := false
	defer func() {
		cancel()
		unpark()
		if !joined {
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Error("held-cycle cleanup did not join")
			}
		}
	}()
	go func() { _, _, err := l.SweepDeferredRetirements(ctx); done <- err }()
	require.Eventually(t, func() bool { return gate.Stats().BackgroundQueued == 1 }, 5*time.Second, time.Millisecond)
	requireGenerationStateWithPayload(t, l.store, id, store_sqlite.ViewGenerationFailed)
	select {
	case err := <-done:
		joined = true
		t.Fatalf("cleanup entered held edit: %v", err)
	default:
	}
	// Queued service does not retain the sweep mutex needed by explicit work.
	require.True(t, l.retirementSweepMu.TryLock())
	l.retirementSweepMu.Unlock()
	unpark()
	select {
	case err := <-done:
		joined = true
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("cleanup did not resume")
	}
}

func TestAgedRetirementRefusalsDoNotInventProgressOrRefreshDebtAge(t *testing.T) {
	for _, kind := range []string{"lease", "wal", "build_flight", "sweep_mutex"} {
		t.Run(kind, func(t *testing.T) {
			l, gate, id := agedRetirementFixture(t)
			l.owed[id] = struct{}{}
			since := l.deferredRetirementEligibleSince.Load()
			before := l.deferredRetirementProgress.Load()
			switch kind {
			case "lease":
				lease := l.leases.Acquire(id)
				defer lease.Release()
			case "wal":
				l.walBytes = func() int64 { return deferredRetirementWALPause + 1 }
			case "build_flight":
				// Admit the physical owner before failing its output, as in
				// the abandonment handoff that retirement must not interrupt.
				require.NoError(t, l.store.Catalog().SetViewGenerationState(t.Context(), id, store_sqlite.ViewGenerationBuilding, store_sqlite.ViewGenerationFailed))
				flight, leader, _, err := l.store.JoinPayloadBuildFlight(t.Context(), id, false)
				require.NoError(t, err)
				require.True(t, leader)
				defer flight.Complete(nil)
				require.NoError(t, l.store.Catalog().SetViewGenerationState(t.Context(), id, store_sqlite.ViewGenerationFailed, store_sqlite.ViewGenerationBuilding))
			case "sweep_mutex":
				l.retirementSweepMu.Lock()
				defer l.retirementSweepMu.Unlock()
			}
			// Direct service permits the explicit-mutex refusal to be observed
			// without blocking on the inventory phase's existing mutex.
			_, pending, err := l.serveDeferredRetirementBurst(t.Context(), gate, []int64{id}, nil, nil)
			require.NoError(t, err)
			require.True(t, pending)
			require.Equal(t, before, l.deferredRetirementProgress.Load())
			require.Equal(t, since, l.deferredRetirementEligibleSince.Load())
			requireGenerationStateWithPayload(t, l.store, id, store_sqlite.ViewGenerationFailed)
			require.False(t, gate.Stats().Active)
		})
	}
}

func TestEligibleRetirementAgeSurvivesPartialDiscoveryAndSmallProgress(t *testing.T) {
	l := &CheckoutLifecycle{}
	now := time.Now()
	l.noteEligibleRetirementDebt(now, true, true)
	l.noteRetirementProgress(now.Add(time.Minute))
	l.noteEligibleRetirementDebt(now.Add(time.Minute), false, false)
	require.Equal(t, now.UnixNano(), l.deferredRetirementEligibleSince.Load())
	require.True(t, l.retirementDebtAged(now.Add(3*time.Minute)))
	l.noteEligibleRetirementDebt(now.Add(3*time.Minute), false, true)
	require.Zero(t, l.deferredRetirementEligibleSince.Load())
}

func TestAgedRetirementDrainsLeavesBeforeReferencedParents(t *testing.T) {
	f := newDedicatedChainFixture(t)
	parent := f.base(store_sqlite.ViewGenerationSuperseded, 0)
	child := f.base(store_sqlite.ViewGenerationSuperseded, parent)
	live := f.base(store_sqlite.ViewGenerationReady, 0)
	f.activate(live)
	l := f.lifecycle(-1)
	gate := NewViewBuildGate()
	gate.Open()
	l.SetBuildGate(gate)
	l.foregroundWork = func() (string, time.Time) { return "refresh_ticket", time.Now() }
	l.deferredRetirementEligibleSince.Store(time.Now().Add(-3 * time.Minute).UnixNano())
	var retired int
	for attempt := 0; attempt < 40; attempt++ {
		n, pending, err := l.SweepDeferredRetirements(t.Context())
		require.NoError(t, err)
		retired += n
		if !pending {
			break
		}
	}
	require.Equal(t, 2, retired)
	requireGenerationRetired(t, f.store, child)
	requireGenerationRetired(t, f.store, parent)
	requireCatalogGenerationPresent(t, f.store, live)
}

func TestAgedRetirementUsesOneDeadlineAcrossAllQuanta(t *testing.T) {
	l, gate, _ := agedRetirementFixture(t)
	var deadline time.Time
	var calls int
	l.retireQuantum = func(ctx context.Context, id int64, inUse func(int64) bool) (store_sqlite.PayloadRetirementProgress, error) {
		got, ok := ctx.Deadline()
		require.True(t, ok)
		if deadline.IsZero() {
			deadline = got
		} else {
			require.Equal(t, deadline, got)
		}
		calls++
		progress, err := l.store.RetirePayloadGenerationQuantum(ctx, id, inUse)
		if calls == 2 {
			<-ctx.Done()
			return progress, errors.Join(err, ctx.Err())
		}
		return progress, err
	}
	_, pending, err := l.SweepDeferredRetirements(t.Context())
	require.NoError(t, err)
	require.True(t, pending)
	require.Equal(t, 2, calls)
	require.False(t, gate.Stats().Active)
	require.True(t, l.retirementDebtAged(time.Now()))
}

func TestRetirementBurstDistinguishesAutomaticRollbackFromLiveSQLFailure(t *testing.T) {
	for _, kind := range []string{"expired", "live", "unrelated", "parent_canceled"} {
		t.Run(kind, func(t *testing.T) {
			l, gate, id := agedRetirementFixture(t)
			db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "rollback.sqlite"))
			require.NoError(t, err)
			db.SetMaxOpenConns(1)
			t.Cleanup(func() { require.NoError(t, db.Close()) })
			_, err = db.Exec(`CREATE TABLE retained(id INTEGER PRIMARY KEY); INSERT INTO retained VALUES(1)`)
			require.NoError(t, err)
			core, logs := observer.New(zap.InfoLevel)
			l.logger = zap.New(core)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			var first store_sqlite.PayloadRetirementProgress
			calls := 0
			sentinel := errors.New("unrelated SQL failure after expiration")
			l.retireQuantum = func(burstCtx context.Context, generationID int64, inUse func(int64) bool) (store_sqlite.PayloadRetirementProgress, error) {
				calls++
				if calls == 1 {
					var err error
					first, err = l.store.RetirePayloadGenerationQuantum(burstCtx, generationID, inUse)
					return first, err
				}
				tx, err := db.BeginTx(burstCtx, nil)
				require.NoError(t, err)
				_, err = tx.ExecContext(burstCtx, `DELETE FROM retained`)
				require.NoError(t, err)
				if kind == "live" {
					require.NoError(t, tx.Rollback())
					require.NoError(t, burstCtx.Err())
				} else {
					if kind == "parent_canceled" {
						cancel()
					}
					<-burstCtx.Done()
					// Witness database/sql's own context rollback before Commit;
					// no manual Rollback generates the expired-path outcome.
					require.Eventually(t, func() bool {
						_, err := tx.ExecContext(context.Background(), `SELECT 1`)
						return errors.Is(err, sql.ErrTxDone)
					}, time.Second, time.Millisecond)
				}
				err = tx.Commit()
				require.ErrorIs(t, err, sql.ErrTxDone)
				if kind == "unrelated" {
					err = sentinel
				}
				return store_sqlite.PayloadRetirementProgress{}, fmt.Errorf("quantum commit: %w", err)
			}
			_, pending, err := l.SweepDeferredRetirements(ctx)
			switch kind {
			case "expired":
				require.NoError(t, err)
			case "live":
				require.ErrorIs(t, err, sql.ErrTxDone)
			case "unrelated":
				require.ErrorIs(t, err, sentinel)
			case "parent_canceled":
				require.ErrorIs(t, err, context.Canceled)
			}
			require.True(t, pending)
			require.Equal(t, 2, calls)
			require.EqualValues(t, 1, first.ChunksCommitted)
			var remaining int
			require.NoError(t, db.QueryRow(`SELECT count(*) FROM retained`).Scan(&remaining))
			require.Equal(t, 1, remaining, "automatic rollback cannot count or retain deleted rows")
			entries := logs.FilterMessage("indexer: deferred retirement committed a bounded burst").All()
			require.Len(t, entries, 1)
			fields := entries[0].ContextMap()
			require.Equal(t, first.ChunksCommitted, fields["chunks_committed"])
			require.Equal(t, first.RowsDeleted, fields["rows_deleted"])
			require.EqualValues(t, first.StepsAdvanced, fields["steps_advanced"])
			require.Zero(t, fields["retired_generations"])
			requireGenerationStateWithPayload(t, l.store, id, store_sqlite.ViewGenerationRetiring)
			require.False(t, gate.Stats().Active)
		})
	}
}

func TestQueuedRetirementContinuesToActualWriterRefusal(t *testing.T) {
	l, gate, id := agedRetirementFixture(t)
	// Keep a permanent interactive waiter while an aged background request
	// qualifies through the existing fairness rule; no new scheduler path.
	gate.backgroundStarvation = 0
	hold, err := gate.Acquire(t.Context(), ViewBuildInteractive)
	require.NoError(t, err)
	gate.mu.Lock()
	gate.interactiveBurst = maxInteractiveBuildBurst
	gate.mu.Unlock()
	var release sync.Once
	unpark := func() { release.Do(hold) }
	ctx, cancel := context.WithCancel(t.Context())
	sweepDone := make(chan error, 1)
	editDone := make(chan time.Time, 1)
	sweepJoined, editJoined := false, false
	defer func() {
		cancel()
		unpark()
		if !sweepJoined {
			select {
			case <-sweepDone:
			case <-time.After(5 * time.Second):
				t.Error("retirement did not join")
			}
		}
		if !editJoined {
			select {
			case <-editDone:
			case <-time.After(5 * time.Second):
				t.Error("edit admission did not join")
			}
		}
	}()
	calls := 0
	l.owed[id] = struct{}{}
	l.retireQuantum = func(ctx context.Context, generationID int64, inUse func(int64) bool) (store_sqlite.PayloadRetirementProgress, error) {
		calls++
		if calls == 1 {
			return store_sqlite.PayloadRetirementProgress{ChunksCommitted: 1, RowsDeleted: 16}, nil
		}
		return store_sqlite.PayloadRetirementProgress{}, store_sqlite.ErrPayloadRetirementWriteWanted
	}
	go func() { _, _, err := l.SweepDeferredRetirements(ctx); sweepDone <- err }()
	require.Eventually(t, func() bool { return gate.Stats().BackgroundQueued == 1 }, 5*time.Second, time.Millisecond)
	go func() {
		permit, err := gate.Acquire(ctx, ViewBuildInteractive)
		if err == nil {
			editDone <- time.Now()
			permit()
		} else {
			editDone <- time.Time{}
		}
	}()
	require.Eventually(t, func() bool { return gate.Stats().InteractiveQueued == 1 }, 5*time.Second, time.Millisecond)
	from := time.Now()
	unpark()
	select {
	case at := <-editDone:
		editJoined = true
		require.False(t, at.IsZero())
		require.LessOrEqual(t, at.Sub(from), 100*time.Millisecond)
	case <-time.After(5 * time.Second):
		t.Fatal("queued edit did not acquire after bounded service")
	}
	select {
	case err := <-sweepDone:
		sweepJoined = true
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("bounded service did not finish")
	}
	require.Equal(t, 2, calls, "queued demand permits the next quantum; actual writer refusal stops it")
	// Completion may clear debt; partial work must keep its original age.
	if l.hasDeferredRetirementWork() {
		require.True(t, l.retirementDebtAged(time.Now()))
	}
}
