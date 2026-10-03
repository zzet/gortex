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

// Exercise production convergence and admission, without an independent
// background PASSIVE pass rescuing a direct helper's growing tail.
func TestCompletedSlowConvergenceRecoversContinuousWrites(t *testing.T) {
	for _, delay := range []time.Duration{0, 4660 * time.Millisecond} {
		t.Run(fmt.Sprintf("DB_sync_delay_%s", delay), func(t *testing.T) {
			s, db := finalBackfillFixture(t)
			lane := &fakeBuildLane{}
			lane.install(s)
			lane.held.Store(true)
			t.Cleanup(func() { lane.held.Store(false) })
			previousRate := walHoldCopyRate.Swap(0)
			t.Cleanup(func() { walHoldCopyRate.Store(previousRate) })
			ctx, cancel := context.WithTimeout(t.Context(), walReclaimLaneBudget)
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
			require.True(t, active, "actual outer attempt did not recover within its original lifetime")
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

func TestSlowConvergenceProofRefusesInvalidCopies(t *testing.T) {
	for _, kind := range []string{"cancelled", "new_WAL", "incomplete", "no_actual_copy"} {
		t.Run(kind, func(t *testing.T) {
			s, db := finalBackfillFixture(t)
			lane := &fakeBuildLane{}
			lane.install(s)
			lane.held.Store(true)
			t.Cleanup(func() { lane.held.Store(false) })
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			if kind == "incomplete" {
				reader, err := s.db.BeginTx(ctx, nil)
				require.NoError(t, err)
				t.Cleanup(func() { _ = reader.Rollback() })
				var payload string
				require.NoError(t, reader.QueryRowContext(ctx, `SELECT payload FROM wal_churn WHERE id = 1`).Scan(&payload))
			}
			if kind == "no_actual_copy" {
				_, err := checkpointWALOnceOn(ctx, db, "PASSIVE")
				require.NoError(t, err)
			} else {
				s.writeMu.Lock()
				_, err := s.writerDB.ExecContext(ctx, `UPDATE wal_churn SET payload = 'needs-copy' WHERE id % 8 = 1`)
				s.writeMu.Unlock()
				require.NoError(t, err)
			}
			beforeFrontier, frontierKnown := readWALReclaimFrontier(s.dbPath)
			require.True(t, frontierKnown)
			var observed atomic.Bool
			var first walCheckpointResult
			previousObserver := walCheckpointResultObserver
			walCheckpointResultObserver = func(mode string, _ time.Time, _ time.Duration, result walCheckpointResult, _ error) {
				if mode != "PASSIVE" || !observed.CompareAndSwap(false, true) {
					return
				}
				first = result
				switch kind {
				case "cancelled":
					cancel()
				case "new_WAL":
					_, err := checkpointWALOnceOn(t.Context(), s.writerDB, "TRUNCATE")
					require.NoError(t, err)
				}
				if kind == "cancelled" || kind == "new_WAL" {
					s.writeMu.Lock()
					_, err := s.writerDB.ExecContext(t.Context(), `UPDATE wal_churn SET payload = 'new-tail' WHERE id % 8 = 1`)
					s.writeMu.Unlock()
					require.NoError(t, err)
					if kind == "new_WAL" {
						after, known := readWALReclaimFrontier(s.dbPath)
						require.True(t, known)
						require.NotEqual(t, beforeFrontier.salt, after.salt, "real TRUNCATE did not replace the WAL identity")
					}
				}
			}
			t.Cleanup(func() { walCheckpointResultObserver = previousObserver })
			observeAdaptiveMultiFrameSync(t, db, func(file int) {
				if file == vfsFileWAL {
					time.Sleep(75 * time.Millisecond)
				}
			}, nil)
			attempt := &backgroundCheckpointAttempt{copy: &walCopyAttempt{pressure: true}}
			result := s.convergeBackfillPacedWithSmallRemainder(ctx, db, attempt, walReclaimPressureSmallFrames)
			t.Logf("kind=%s actual_first_tuple=%+v observed=%v stop=%s passes=%d context=%v", kind, first, observed.Load(), result.stop, result.passes, ctx.Err())
			require.Nil(t, result.slowTail, "invalid copy earned adaptive entitlement")
			require.NotEqual(t, "slow_tail_plateau", result.stop)
			if kind == "no_actual_copy" {
				require.False(t, observed.Load())
				require.Zero(t, result.passes)
			} else {
				require.True(t, observed.Load(), "the actual convergence copy premise was not reached")
				if kind == "incomplete" {
					require.Less(t, first.CheckpointedFrames, first.WALFrames)
				} else {
					require.Zero(t, first.Busy)
					require.Positive(t, first.WALFrames)
					require.Equal(t, first.WALFrames, first.CheckpointedFrames)
				}
			}
		})
	}
}
