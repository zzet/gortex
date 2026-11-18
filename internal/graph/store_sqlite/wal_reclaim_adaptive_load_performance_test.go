//go:build performance

package store_sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// Original measured-load cases remain exact apart from their TestPerformance
// names. Default CI retains the independent credit/cancellation/invalid-copy
// controls and the barrier-driven pressure crossing; cadence, throughput and
// foreground latency measurements run only in the opt-in performance flow.

func TestPerformanceVariableSyncSingleAdaptiveAttempt(t *testing.T) {
	for _, kind := range []string{"short", "pressure"} {
		t.Run(kind, func(t *testing.T) {
			s, db := finalBackfillFixture(t)
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			var writes, syncs, worstGate, worstSQL atomic.Int64
			joined := make(chan error, 1)
			go func() {
				for {
					select {
					case <-ctx.Done():
						joined <- nil
						return
					case <-time.After(20 * time.Millisecond):
					}
					gateStart := time.Now()
					if err := s.writeMu.LockContext(ctx); err != nil {
						joined <- nil
						return
					}
					gate := int64(time.Since(gateStart))
					for prev := worstGate.Load(); gate > prev && !worstGate.CompareAndSwap(prev, gate); prev = worstGate.Load() {
					}
					sqlStart := time.Now()
					_, err := s.writerDB.ExecContext(ctx, `UPDATE wal_churn SET payload = ? WHERE id = 1`, fmt.Sprintf("variable-%d", writes.Load()+1))
					sqlTime := int64(time.Since(sqlStart))
					for prev := worstSQL.Load(); sqlTime > prev && !worstSQL.CompareAndSwap(prev, sqlTime); prev = worstSQL.Load() {
					}
					s.writeMu.Unlock()
					if err != nil {
						if ctx.Err() != nil {
							joined <- nil
						} else {
							joined <- err
						}
						return
					}
					writes.Add(1)
				}
			}()
			var once sync.Once
			var producerErr error
			stop := func() {
				once.Do(func() {
					cancel()
					select {
					case producerErr = <-joined:
					case <-time.After(3 * time.Second):
						producerErr = fmt.Errorf("producer did not join")
					}
				})
			}
			defer stop()
			var durations []time.Duration
			observeDelayedReclaimSync(t, db, func(file int) {
				if file != vfsFileWAL {
					return
				}
				n := syncs.Add(1)
				delay := 600 * time.Millisecond
				if n == 1 {
					delay = 120 * time.Millisecond
				}
				started := time.Now()
				time.Sleep(delay)
				durations = append(durations, time.Since(started))
			})
			var passiveResults []string
			walCheckpointCallObserver = func(mode string, _ time.Time, took time.Duration) {
				frontier, ok := readWALReclaimFrontier(s.dbPath)
				passiveResults = append(passiveResults, fmt.Sprintf("mode=%s elapsed=%s held=%t writes=%d frontier=%+v valid=%t", mode, took, s.writeMu.held(), writes.Load(), frontier, ok))
			}
			defer func() { walCheckpointCallObserver = nil }()
			res := &walReclaimResult{urgent: true}
			before := time.Now()
			done := false
			var err error
			if kind == "short" {
				done, err = s.reclaimWALResetHold(ctx, walReclaimConfig{thresholdBytes: 1}, db, res, false, false)
			} else {
				err = s.reclaimWALPressureReset(ctx, db, res)
				done = err == nil
			}
			active := ctx.Err() == nil
			writesAtReturn := writes.Load()
			if done {
				deadline := time.Now().Add(time.Second)
				for writes.Load() == writesAtReturn && time.Now().Before(deadline) {
					time.Sleep(time.Millisecond)
				}
			}
			stop()
			frontier, ok := readWALReclaimFrontier(s.dbPath)
			t.Logf("kind=%s done=%t err=%v active=%t elapsed=%s writes_at_return=%d final_writes=%d syncs=%d sync_durations=%v adaptive_used=%t adaptive_budget=%s adaptive_hold=%s aggregate_hold=%s final_frontier=%+v valid=%t checkpoint_results=%v worst_gate=%s worst_SQL=%s producer_error=%v", kind, done, err, active, time.Since(before), writesAtReturn, writes.Load(), syncs.Load(), durations, res.adaptiveUsed, res.adaptiveBudget, res.adaptiveHold, res.writerSpent, frontier, ok, passiveResults, time.Duration(worstGate.Load()), time.Duration(worstSQL.Load()), producerErr)
			require.NoError(t, producerErr)
			require.True(t, active)
			require.True(t, done, "the single witnessed adaptive completion could not reset a reader-free growing tail")
			require.Greater(t, writes.Load(), writesAtReturn, "the producer did not resume after the reset")
			require.True(t, res.adaptiveUsed)
			require.GreaterOrEqual(t, syncs.Load(), int64(2))
			require.LessOrEqual(t, res.writerSpent, walReclaimMaxWriterHold)
			require.Less(t, time.Duration(worstGate.Load()), walReclaimMaxWriterHold)
			require.Zero(t, res.pause)
			require.False(t, s.readGate.closed.Load())
			var payload string
			require.NoError(t, s.db.QueryRow(`SELECT payload FROM wal_churn WHERE id=1`).Scan(&payload))
			require.Equal(t, fmt.Sprintf("variable-%d", writes.Load()), payload)
		})
	}
}

func TestPerformanceCompletedSlowConvergenceRecoversContinuousWrites(t *testing.T) {
	for _, delay := range []time.Duration{0, 4660 * time.Millisecond} {
		t.Run(fmt.Sprintf("DB_sync_delay_%s", delay), func(t *testing.T) {
			s, db := finalBackfillFixture(t)
			lane := &fakeBuildLane{}
			lane.install(s)
			lane.held.Store(true)
			t.Cleanup(func() { lane.held.Store(false) })
			previousRate := walHoldCopyRate.Swap(0)
			t.Cleanup(func() { walHoldCopyRate.Store(previousRate) })
			// This scenario includes multiple writer-free copies and admitted
			// reset attempts. Its finite progress guard is separate from each
			// production lane operation's unchanged ten-second budget.
			const scenarioProgressBudget = 30 * time.Second
			ctx, cancel := context.WithTimeout(t.Context(), scenarioProgressBudget)
			defer cancel()
			cancelJoined := make(chan struct{})
			stopCancellation := context.AfterFunc(ctx, func() {
				defer close(cancelJoined)
				s.backgroundCheckpoint.mu.Lock()
				attempt := s.backgroundCheckpoint.active
				s.backgroundCheckpoint.mu.Unlock()
				if attempt != nil {
					attempt.cancel(context.Cause(ctx))
				}
			})
			t.Cleanup(func() {
				if !stopCancellation() {
					select {
					case <-cancelJoined:
					case <-time.After(time.Second):
						t.Error("owned attempt cancellation did not join")
					}
				}
			})
			var writes, worstGate, worstSQL atomic.Int64
			committed := make(chan struct{}, 1)
			producer := make(chan error, 1)
			go func() {
				for {
					select {
					case <-ctx.Done():
						producer <- nil
						return
					case <-time.After(150 * time.Millisecond):
					}
					wctx, done := context.WithTimeout(ctx, 2*time.Second)
					started := time.Now()
					err := s.writeMu.LockContext(wctx)
					gate := time.Since(started)
					var sqlTime time.Duration
					if err == nil {
						started = time.Now()
						_, err = s.writerDB.ExecContext(wctx, `UPDATE wal_churn SET payload = ? WHERE id % 8 = 1`, fmt.Sprintf("live-%d", writes.Load()+1))
						sqlTime = time.Since(started)
						s.writeMu.Unlock()
					}
					done()
					for previous := worstGate.Load(); int64(gate) > previous && !worstGate.CompareAndSwap(previous, int64(gate)); previous = worstGate.Load() {
					}
					for previous := worstSQL.Load(); int64(sqlTime) > previous && !worstSQL.CompareAndSwap(previous, int64(sqlTime)); previous = worstSQL.Load() {
					}
					if err != nil {
						if ctx.Err() != nil {
							producer <- nil
						} else {
							producer <- fmt.Errorf("foreground gate=%s SQL=%s: %w", gate, sqlTime, err)
						}
						return
					}
					writes.Add(1)
					select {
					case committed <- struct{}{}:
					default:
					}
				}
			}()
			var joinOnce sync.Once
			var producerErr error
			join := func() {
				joinOnce.Do(func() {
					cancel()
					select {
					case producerErr = <-producer:
					case <-time.After(3 * time.Second):
						producerErr = fmt.Errorf("foreground producer did not join")
					}
				})
			}
			t.Cleanup(join)
			var heldSQL, armed atomic.Bool
			var injected, commitsDuringSync, injectionNs, actualDBSyncNs, effectiveDBSyncNs, mainSyncWrites, walSyncs atomic.Int64
			previousQueryer := walReclaimCreditPassiveQueryerObserver
			walReclaimCreditPassiveQueryerObserver = func(sqlCtx context.Context, target *Store, db *sql.DB) (walCheckpointQueryer, func()) {
				if target != s {
					return db, func() {}
				}
				heldSQL.Store(true)
				return db, func() { heldSQL.Store(false); armed.Store(false) }
			}
			t.Cleanup(func() { walReclaimCreditPassiveQueryerObserver = previousQueryer })
			previousPressureHook := walPressureResetHook
			walPressureResetHook = func(heldCtx context.Context) {
				if deadline, ok := heldCtx.Deadline(); ok && time.Until(deadline) > walReclaimPressureHold {
					armed.Store(true)
				}
			}
			t.Cleanup(func() { walPressureResetHook = previousPressureHook })
			observeAdaptiveMultiFrameSync(t, db, func(file int) {
				if file == vfsFileWAL {
					walSyncs.Add(1)
					time.Sleep(300 * time.Millisecond)
				}
				if file == vfsFileMain && heldSQL.Load() && armed.Load() {
					mainSyncWrites.Store(writes.Load())
				}
			}, func(file int, from, to time.Time) {
				if file != vfsFileMain {
					return
				}
				actualDBSyncNs.Store(max(actualDBSyncNs.Load(), int64(to.Sub(from))))
				if delay > 0 && heldSQL.Load() && armed.CompareAndSwap(true, false) && injected.CompareAndSwap(0, 1) {
					// The real OS sync ran. Keep the checkpoint at its return boundary
					// only until the total interval reaches the observed delay, so
					// slow hosts do not pay both real and artificial DB-sync latency.
					if remaining := time.Until(from.Add(delay)); remaining > 0 {
						paddingStarted := time.Now()
						time.Sleep(remaining)
						injectionNs.Store(int64(time.Since(paddingStarted)))
					}
					commitsDuringSync.Store(writes.Load() - mainSyncWrites.Load())
				}
				effectiveDBSyncNs.Store(max(effectiveDBSyncNs.Load(), int64(time.Since(from))))
			})
			t.Logf("scenario_progress_budget=%s lane_operation_budget=%s", scenarioProgressBudget, walReclaimLaneBudget)
			started := time.Now()
			resets, adaptiveAttempts, plateauPasses := 0, 0, 0
			var aggregateMax time.Duration
			cfg := walReclaimConfig{thresholdBytes: 1, ceilingBytes: 64 << 20, readerWait: time.Second, truncateBudget: walReclaimTruncateBudget}
			for ctx.Err() == nil && resets == 0 {
				result := s.reclaimWALOnce(cfg, db, s.dbPath+"-wal")
				t.Logf("actual outer outcome=%s reason=%s%s", result.outcome.String(), result.reason, result.stampSuffix())
				if result.convergence != nil && result.convergence.stop == "slow_tail_plateau" {
					plateauPasses++
				}
				if result.adaptiveUsed {
					adaptiveAttempts++
				}
				aggregateMax = max(aggregateMax, result.writerSpent)
				if result.outcome == walReclaimReset {
					resets++
					before := writes.Load()
					for writes.Load() == before && ctx.Err() == nil {
						select {
						case <-committed:
						case <-ctx.Done():
						}
					}
				}
			}
			active := ctx.Err() == nil
			join()
			t.Logf("minimum_DB_sync_requested=%s padding_added=%s actual_DB_sync=%s effective_DB_sync=%s writes_during_sync=%d resets=%d writes=%d WAL_syncs=%d adaptive_attempts=%d plateau_passes=%d aggregate_max=%s gate_max=%s SQL_max=%s elapsed=%s producer_error=%v", delay, time.Duration(injectionNs.Load()), time.Duration(actualDBSyncNs.Load()), time.Duration(effectiveDBSyncNs.Load()), commitsDuringSync.Load(), resets, writes.Load(), walSyncs.Load(), adaptiveAttempts, plateauPasses, aggregateMax, time.Duration(worstGate.Load()), time.Duration(worstSQL.Load()), time.Since(started), producerErr)
			require.NoError(t, producerErr)
			require.True(t, active, "recovery did not complete within the scenario progress guard")
			require.Equal(t, 1, resets)
			require.Positive(t, adaptiveAttempts)
			require.Positive(t, plateauPasses)
			require.GreaterOrEqual(t, writes.Load(), int64(2))
			require.GreaterOrEqual(t, walSyncs.Load(), int64(2))
			require.LessOrEqual(t, aggregateMax, walReclaimMaxWriterHold)
			require.Less(t, time.Duration(worstGate.Load()), 2*time.Second)
			if delay > 0 {
				require.Equal(t, int64(1), injected.Load(), "the actual adaptive DB-sync premise was not reached")
				require.GreaterOrEqual(t, time.Duration(effectiveDBSyncNs.Load()), delay)
				require.Positive(t, commitsDuringSync.Load(), "foreground SQL did not resume while DB sync outlived credit")
			}
			var payload string
			require.NoError(t, s.db.QueryRow(`SELECT payload FROM wal_churn WHERE id = 1`).Scan(&payload))
			require.Equal(t, fmt.Sprintf("live-%d", writes.Load()), payload)
			require.False(t, s.readGate.closed.Load(), "bounded completion must keep reader admission open")
		})
	}
}

func TestPerformanceUrgentReclaimCompletesSlowSyncWithContinuousWrites(t *testing.T) {
	for _, kind := range []string{"short", "pressure"} {
		t.Run(kind, func(t *testing.T) {
			s, db := finalBackfillFixture(t)
			ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
			var writes, syncs, worstGate, worstSQL atomic.Int64
			committed := make(chan struct{}, 1)
			producer := make(chan error, 1)
			go func() {
				for {
					select {
					case <-ctx.Done():
						producer <- nil
						return
					case <-time.After(150 * time.Millisecond):
					}
					wctx, stop := context.WithTimeout(ctx, 2*time.Second)
					before := time.Now()
					err := s.writeMu.LockContext(wctx)
					gate := time.Since(before)
					stage := "gate"
					var sqlTime time.Duration
					if err == nil {
						stage = "SQL"
						before = time.Now()
						_, err = s.writerDB.ExecContext(wctx, `UPDATE wal_churn SET payload = ? WHERE id = 1`, fmt.Sprintf("urgent-%d", writes.Load()+1))
						sqlTime = time.Since(before)
						s.writeMu.Unlock()
					}
					stop()
					for prev := worstGate.Load(); int64(gate) > prev && !worstGate.CompareAndSwap(prev, int64(gate)); prev = worstGate.Load() {
					}
					for prev := worstSQL.Load(); int64(sqlTime) > prev && !worstSQL.CompareAndSwap(prev, int64(sqlTime)); prev = worstSQL.Load() {
					}
					if err != nil {
						if ctx.Err() != nil {
							producer <- nil
						} else {
							producer <- fmt.Errorf("foreground %s (gate=%s SQL=%s): %w", stage, gate, sqlTime, err)
						}
						return
					}
					writes.Add(1)
					select {
					case committed <- struct{}{}:
					default:
					}
				}
			}()
			var joinOnce sync.Once
			var producerErr error
			join := func() {
				joinOnce.Do(func() {
					cancel()
					select {
					case producerErr = <-producer:
					case <-time.After(3 * time.Second):
						producerErr = fmt.Errorf("foreground producer did not join")
					}
				})
			}
			t.Cleanup(join)
			started := time.Now()
			resets := 0
			adaptiveAttempts := 0
			var aggregateMax time.Duration
			for ctx.Err() == nil && resets < 2 {
				observeDelayedReclaimSync(t, db, func(file int) {
					if file == vfsFileWAL {
						syncs.Add(1)
						time.Sleep(300 * time.Millisecond)
					}
				})
				res := &walReclaimResult{urgent: true}
				done := false
				if kind == "pressure" {
					done = s.reclaimWALPressureReset(ctx, db, res) == nil
				} else {
					done, _ = s.reclaimWALResetHold(ctx, walReclaimConfig{thresholdBytes: 1}, db, res, false, false)
				}
				if res.adaptiveUsed {
					adaptiveAttempts++
					aggregateMax = max(aggregateMax, res.writerSpent)
					require.LessOrEqual(t, res.writerSpent, walReclaimMaxWriterHold)
				}
				if !done {
					continue
				}
				resets++
				before := writes.Load()
				for writes.Load() == before && ctx.Err() == nil {
					select {
					case <-committed:
					case <-ctx.Done():
					}
				}
			}
			active := ctx.Err() == nil
			join()
			t.Logf("resets=%d writes=%d WAL_syncs=%d adaptive_attempts=%d aggregate_hold_max=%s worst_gate=%s worst_SQL=%s elapsed=%s producer_error=%v", resets, writes.Load(), syncs.Load(), adaptiveAttempts, aggregateMax, time.Duration(worstGate.Load()), time.Duration(worstSQL.Load()), time.Since(started), producerErr)
			require.NoError(t, producerErr)
			require.True(t, active, "urgent reset never caught the growing tail before its finite deadline")
			require.Equal(t, 2, resets)
			require.Positive(t, adaptiveAttempts)
			require.GreaterOrEqual(t, writes.Load(), int64(3))
			require.GreaterOrEqual(t, syncs.Load(), int64(2))
			require.Less(t, time.Duration(worstGate.Load()), 2*time.Second)
			var payload string
			require.NoError(t, s.db.QueryRow(`SELECT payload FROM wal_churn WHERE id = 1`).Scan(&payload))
			require.Equal(t, fmt.Sprintf("urgent-%d", writes.Load()), payload)
		})
	}
}

func TestPerformanceUrgentAdaptiveCopyAdmitsMultiFrameTail(t *testing.T) {
	for _, kind := range []string{"short", "pressure"} {
		t.Run(kind, func(t *testing.T) {
			s, db := finalBackfillFixture(t)
			lane := &fakeBuildLane{}
			lane.install(s)
			lane.held.Store(true)
			t.Cleanup(func() { lane.held.Store(false) })
			priorRate := walHoldCopyRate.Swap(0)
			t.Cleanup(func() { walHoldCopyRate.Store(priorRate) })
			// The operation may join an uninterruptible sync after its writer credit
			// expires. Use its existing finite lifetime; held credit remains <=2 s.
			ctx, cancel := context.WithTimeout(t.Context(), walReclaimLaneBudget)
			cancelJoined := make(chan struct{})
			stopCancellation := context.AfterFunc(ctx, func() {
				defer close(cancelJoined)
				s.backgroundCheckpoint.mu.Lock()
				attempt := s.backgroundCheckpoint.active
				s.backgroundCheckpoint.mu.Unlock()
				if attempt != nil {
					attempt.cancel(context.Cause(ctx))
				}
			})
			t.Cleanup(func() {
				if !stopCancellation() {
					select {
					case <-cancelJoined:
					case <-time.After(time.Second):
						t.Error("owned attempt cancellation did not join")
					}
				}
			})
			var writes, syncs, worstGate, worstSQL atomic.Int64
			committed := make(chan struct{}, 1)
			producer := make(chan error, 1)
			go func() {
				for {
					select {
					case <-ctx.Done():
						producer <- nil
						return
					case <-time.After(150 * time.Millisecond):
					}
					wctx, stop := context.WithTimeout(ctx, 2*time.Second)
					before := time.Now()
					err := s.writeMu.LockContext(wctx)
					gate := time.Since(before)
					stage := "gate"
					var sqlTime time.Duration
					if err == nil {
						stage = "SQL"
						before = time.Now()
						_, err = s.writerDB.ExecContext(wctx, `UPDATE wal_churn SET payload = ? WHERE id % 8 = 1`, fmt.Sprintf("urgent-%d", writes.Load()+1))
						sqlTime = time.Since(before)
						s.writeMu.Unlock()
					}
					stop()
					for prev := worstGate.Load(); int64(gate) > prev && !worstGate.CompareAndSwap(prev, int64(gate)); prev = worstGate.Load() {
					}
					for prev := worstSQL.Load(); int64(sqlTime) > prev && !worstSQL.CompareAndSwap(prev, int64(sqlTime)); prev = worstSQL.Load() {
					}
					if err != nil {
						if ctx.Err() != nil {
							producer <- nil
						} else {
							producer <- fmt.Errorf("foreground %s (gate=%s SQL=%s): %w", stage, gate, sqlTime, err)
						}
						return
					}
					writes.Add(1)
					select {
					case committed <- struct{}{}:
					default:
					}
				}
			}()
			var joinOnce sync.Once
			var producerErr error
			join := func() {
				joinOnce.Do(func() {
					cancel()
					select {
					case producerErr = <-producer:
					case <-time.After(3 * time.Second):
						producerErr = fmt.Errorf("foreground producer did not join")
					}
				})
			}
			t.Cleanup(join)
			var eventsMu sync.Mutex
			var events, syncEvents []string
			record := func(format string, args ...any) {
				eventsMu.Lock()
				defer eventsMu.Unlock()
				if len(events) < 16 {
					events = append(events, fmt.Sprintf(format, args...))
				}
			}
			// Record the exact checkpoint-connection TLS sync interval. Its span
			// includes the existing artificial delay and the underlying VFS sync.
			observeAdaptiveMultiFrameSync(t, db, func(file int) {
				if file != vfsFileWAL {
					return
				}
				syncs.Add(1)
				time.Sleep(300 * time.Millisecond)
			}, func(file int, from, to time.Time) {
				eventsMu.Lock()
				defer eventsMu.Unlock()
				if len(syncEvents) < 16 {
					syncEvents = append(syncEvents, fmt.Sprintf("TLS sync file=%d start=%s end=%s span_including_test_delay=%s", file, from.Format(time.RFC3339Nano), to.Format(time.RFC3339Nano), to.Sub(from)))
				}
			})
			started := time.Now()
			resets := 0
			adaptiveAttempts := 0
			var aggregateMax time.Duration
			var maxAdaptiveEntryTail uint32
			t.Cleanup(func() { walIdleResetHook = nil; walPressureResetHook = nil })
			iteration := 0
			recoverThroughPipeline := false
			var lastResult walReclaimResult
			var lastError error
			var lastDone, lastSnapshotOK bool
			var lastSnapshot walIndexSnapshot
			for ctx.Err() == nil && resets < 1 {
				iteration++
				res := &walReclaimResult{urgent: true}
				entry := func(phase string) {
					snap, ok := readWALIndexSnapshot(s.dbPath)
					if res.slowTail != nil && ok && snap.MxFrame > snap.NBackfill {
						maxAdaptiveEntryTail = max(maxAdaptiveEntryTail, snap.MxFrame-snap.NBackfill)
					}
					record("entry iteration=%d phase=%s adaptive_witness=%v snapshot_ok=%v post_or_held_frontier=%+v", iteration, phase, res.slowTail != nil, ok, snap)
				}
				walIdleResetHook = func() { entry("short") }
				walPressureResetHook = func(heldCtx context.Context) {
					entry("pressure")
					if deadline, ok := heldCtx.Deadline(); ok && time.Until(deadline) > walReclaimPressureHold {
						if snap, known := readWALIndexSnapshot(s.dbPath); known && snap.MxFrame > snap.NBackfill {
							maxAdaptiveEntryTail = max(maxAdaptiveEntryTail, snap.MxFrame-snap.NBackfill)
						}
					}
				}
				done := false
				var helperErr error
				if recoverThroughPipeline {
					cfg := walReclaimConfig{thresholdBytes: 1, ceilingBytes: 64 << 20, readerWait: time.Second, truncateBudget: walReclaimTruncateBudget}
					*res = s.reclaimWALOnce(cfg, db, s.dbPath+"-wal")
					done = res.outcome == walReclaimReset
					record("production recovery iteration=%d outcome=%s reason=%s%s", iteration, res.outcome.String(), res.reason, res.stampSuffix())
				} else if kind == "pressure" {
					helperErr = s.reclaimWALPressureReset(ctx, db, res)
					done = helperErr == nil
				} else {
					done, helperErr = s.reclaimWALResetHold(ctx, walReclaimConfig{thresholdBytes: 1}, db, res, false, false)
				}
				snap, snapshotOK := readWALIndexSnapshot(s.dbPath)
				lastResult, lastError, lastDone, lastSnapshotOK, lastSnapshot = *res, helperErr, done, snapshotOK, snap
				if iteration <= 16 {
					record("helper iteration=%d done=%v error=%v reason=%s adaptive=%v budget=%s spent=%s context=%v snapshot_ok=%v post_return_frontier=%+v", iteration, done, helperErr, res.reason, res.adaptiveUsed, res.adaptiveBudget, res.writerSpent, ctx.Err(), snapshotOK, snap)
				}
				if res.adaptiveUsed {
					adaptiveAttempts++
					aggregateMax = max(aggregateMax, res.writerSpent)
					require.LessOrEqual(t, res.writerSpent, walReclaimMaxWriterHold)
				}
				if !done {
					recoverThroughPipeline = true
					continue
				}
				resets++
				before := writes.Load()
				for writes.Load() == before && ctx.Err() == nil {
					select {
					case <-committed:
					case <-ctx.Done():
					}
				}
			}
			active := ctx.Err() == nil
			join()
			t.Logf("resets=%d writes=%d WAL_syncs=%d adaptive_attempts=%d aggregate_hold_max=%s worst_gate=%s worst_SQL=%s elapsed=%s producer_error=%v adaptive_entry_tail_frames=%d", resets, writes.Load(), syncs.Load(), adaptiveAttempts, aggregateMax, time.Duration(worstGate.Load()), time.Duration(worstSQL.Load()), time.Since(started), producerErr, maxAdaptiveEntryTail)
			for _, event := range events {
				t.Log(event)
			}
			for _, event := range syncEvents {
				t.Log(event)
			}
			t.Logf("last helper iteration=%d done=%v error=%v reason=%s adaptive=%v budget=%s spent=%s snapshot_ok=%v post_return_frontier=%+v", iteration, lastDone, lastError, lastResult.reason, lastResult.adaptiveUsed, lastResult.adaptiveBudget, lastResult.writerSpent, lastSnapshotOK, lastSnapshot)
			require.NoError(t, producerErr)
			if kind == "short" {
				require.Greater(t, maxAdaptiveEntryTail, walReclaimPressureSmallFrames, "causal precondition: actual adaptive entry tail exceeds the ordinary frame allowance")
			}
			require.True(t, active, "urgent reset never caught the growing tail before its finite deadline")
			require.Equal(t, 1, resets)
			require.Positive(t, adaptiveAttempts)
			require.GreaterOrEqual(t, writes.Load(), int64(2))
			require.GreaterOrEqual(t, syncs.Load(), int64(2))
			require.Less(t, time.Duration(worstGate.Load()), 2*time.Second)
			var payload string
			require.NoError(t, s.db.QueryRow(`SELECT payload FROM wal_churn WHERE id = 1`).Scan(&payload))
			require.Equal(t, fmt.Sprintf("urgent-%d", writes.Load()), payload)
		})
	}
}

func TestPerformanceOrdinaryWALAttemptHandsOverAfterPressureCrossing(t *testing.T) {
	previousRate := walHoldCopyRate.Swap(0)
	t.Cleanup(func() { walHoldCopyRate.Store(previousRate) })
	s, db := finalBackfillFixture(t)
	lane := &fakeBuildLane{}
	lane.install(s)
	path := s.dbPath + "-wal"
	cfg := walReclaimConfig{thresholdBytes: 3 << 20, ceilingBytes: 64 << 20, readerWait: defaultWALReclaimReaderWait, truncateBudget: walReclaimTruncateBudget}
	before, ok := readWALReclaimFrontier(s.dbPath)
	require.True(t, ok)
	require.Less(t, walFileSize(path), walPressureMark(cfg))
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	var inject atomic.Bool
	inject.Store(true)
	type batch struct {
		count int
		done  chan error
	}
	requests := make(chan batch)
	producerDone := make(chan struct{})
	var commits, worstWrite atomic.Int64
	go func() {
		defer close(producerDone)
		for {
			select {
			case <-ctx.Done():
				return
			case request := <-requests:
				var err error
				for range request.count {
					wctx, finish := context.WithTimeout(ctx, 500*time.Millisecond)
					started := time.Now()
					err = s.writeMu.LockContext(wctx)
					if err == nil {
						_, err = s.writerDB.ExecContext(wctx, `UPDATE wal_churn SET payload = ? WHERE id % 16 = 2`, fmt.Sprintf("%01024d", commits.Load()+1))
						s.writeMu.Unlock()
					}
					elapsed := int64(time.Since(started))
					worstWrite.Store(max(worstWrite.Load(), elapsed))
					finish()
					if err != nil {
						break
					}
					commits.Add(1)
				}
				request.done <- err
			}
		}
	}()
	stopCancelDone := make(chan struct{})
	stopCancel := context.AfterFunc(ctx, func() {
		defer close(stopCancelDone)
		s.backgroundCheckpoint.mu.Lock()
		active := s.backgroundCheckpoint.active
		s.backgroundCheckpoint.mu.Unlock()
		if active != nil {
			active.cancel(context.Cause(ctx))
		}
	})
	previous := walCheckpointResultObserver
	var passes int
	var firstPassBytes int64
	var crossing walReclaimFrontier
	var crossingKnown bool
	var producerError error
	walCheckpointResultObserver = func(mode string, start time.Time, took time.Duration, result walCheckpointResult, scanErr error) {
		if previous != nil {
			previous(mode, start, took, result, scanErr)
		}
		if mode != "PASSIVE" || scanErr != nil || result.incomplete() || !inject.Load() || ctx.Err() != nil || s.writeMu.held() {
			return
		}
		passes++
		if passes == 1 {
			firstPassBytes = walFileSize(path)
		}
		if walFileSize(path) >= walPressureMark(cfg) && !crossingKnown {
			crossing, crossingKnown = readWALReclaimFrontier(s.dbPath)
		}
	}
	var cleanupOnce sync.Once
	cleanup := func() {
		cleanupOnce.Do(func() {
			cancel()
			if !stopCancel() {
				<-stopCancelDone
			}
			<-producerDone
			walCheckpointResultObserver = previous
			lane.held.Store(false)
		})
	}
	t.Cleanup(cleanup)
	observeAdaptiveMultiFrameSync(t, db, func(file int) {
		if file != vfsFileWAL || !inject.Load() || ctx.Err() != nil || s.writeMu.held() {
			return
		}
		request := batch{count: 2, done: make(chan error, 1)}
		select {
		case requests <- request:
		case <-ctx.Done():
			return
		}
		select {
		case producerError = <-request.done:
		case <-ctx.Done():
			return
		}
		if producerError != nil {
			return
		}
		if walFileSize(path) >= walPressureMark(cfg) && !crossingKnown {
			crossing, crossingKnown = readWALReclaimFrontier(s.dbPath)
		}
		time.Sleep(100 * time.Millisecond)
	}, nil)
	started := time.Now()
	result := s.reclaimWALOnce(cfg, db, path)
	elapsed := time.Since(started)
	inject.Store(false)
	t.Logf("ordinary outcome=%s reason=%s passes=%d foreground_commits=%d elapsed=%s writer_hold=%s crossed=%v tail=%d", result.outcome.String(), result.reason, passes, commits.Load(), elapsed, result.writerSpent, crossingKnown, crossing.mx-crossing.backfill)
	require.NoError(t, producerError)
	require.True(t, crossingKnown, "the primary-round pressure crossing must be real")
	require.Equal(t, before.salt, crossing.salt, "the mark crossed within the original WAL")
	require.Greater(t, crossing.mx-crossing.backfill, walReclaimPressureSmallFrames)
	require.Less(t, firstPassBytes, walPressureMark(cfg), "initial pass must remain below pressure")
	require.GreaterOrEqual(t, passes, 2, "the initial pass alone must not cross the mark")
	require.Equal(t, walReclaimSkipped, result.outcome)
	require.Equal(t, "pressure_mark", result.reason, "ordinary rounds must yield to fresh pressure admission")
	require.Zero(t, result.writerSpent, "handoff must not grant/reset under an ordinary writer hold")
	require.True(t, ctx.Err() == nil, "handoff used the entire bounded observation window")
	// Fresh admission, with the edit held, must apply the existing pressure
	// policy and authoritative reset, not reinterpret the previous SQL result.
	lane.held.Store(true)
	fresh := s.reclaimWALOnce(cfg, db, path)
	require.True(t, fresh.pressure)
	require.Equal(t, walReclaimReset, fresh.outcome, "%s", fresh.reason)
	require.True(t, walLogIsReset(s.dbPath, path))
	require.LessOrEqual(t, fresh.writerSpent, walReclaimMaxWriterHold)
	require.Less(t, time.Duration(worstWrite.Load()), 500*time.Millisecond)
	var payload string
	require.NoError(t, s.db.QueryRow(`SELECT payload FROM wal_churn WHERE id=2`).Scan(&payload))
	require.Equal(t, fmt.Sprintf("%01024d", commits.Load()), payload)
	require.False(t, s.readGate.closed.Load())
	t.Logf("fresh pressure reset=%v writer_hold=%s foreground_gate_SQL_max=%s", fresh.outcome == walReclaimReset, fresh.writerSpent, time.Duration(worstWrite.Load()))
	cleanup()
}
