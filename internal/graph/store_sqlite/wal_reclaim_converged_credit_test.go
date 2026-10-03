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
			diagnostic := installWindowsConvergenceDiagnostic(t)
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
					diagnostic.event("producer_return index=%d gate=%s SQL=%s error=%v context_error=%v", writes.Load()+1, gate, sqlTime, err, wctx.Err())
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
				creditStarted := time.Now()
				diagnostic.record("credit_scope_enter", sqlCtx, creditStarted, 0, nil)
				return db, func() {
					diagnostic.record("credit_scope_return", sqlCtx, creditStarted, time.Since(creditStarted), nil)
					heldSQL.Store(false)
					armed.Store(false)
				}
			}
			t.Cleanup(func() { walReclaimCreditPassiveQueryerObserver = previousQueryer })
			previousPressureHook := walPressureResetHook
			walPressureResetHook = func(heldCtx context.Context) {
				diagnostic.record("pressure_hold_admitted", heldCtx, time.Now(), 0, nil)
				if deadline, ok := heldCtx.Deadline(); ok && time.Until(deadline) > walReclaimPressureHold {
					armed.Store(true)
				}
			}
			t.Cleanup(func() { walPressureResetHook = previousPressureHook })
			observeAdaptiveMultiFrameSync(t, db, func(file int) {
				diagnostic.event("xSync_enter file=%d held_credit=%v", file, s.writeMu.held())
				if file == vfsFileWAL {
					walSyncs.Add(1)
					time.Sleep(300 * time.Millisecond)
				}
				if file == vfsFileMain && heldSQL.Load() && armed.Load() {
					mainSyncWrites.Store(writes.Load())
				}
				diagnostic.event("xSync_OS_dispatch file=%d", file)
			}, func(file int, from, to time.Time) {
				diagnostic.event("xSync_return file=%d from_ns=%d to_ns=%d wrapper_sync_includes_before_callback=%s held_credit=%v", file, from.UnixNano(), to.UnixNano(), to.Sub(from), s.writeMu.held())
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
					diagnostic.event("main_sync_padding_return from_ns=%d padding=%s", from.UnixNano(), time.Duration(injectionNs.Load()))
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
			diagnostic.flush()
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
			diagnostic := installWindowsConvergenceDiagnostic(t)
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
			walCheckpointResultObserver = func(mode string, from time.Time, took time.Duration, result walCheckpointResult, scanErr error) {
				diagnostic.event("checkpoint_return mode=%s from_ns=%d took=%s tuple=%+v scan_error=%v", mode, from.UnixNano(), took, result, scanErr)
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
				diagnostic.event("xSync_enter file=%d held_credit=%v", file, s.writeMu.held())
				if file == vfsFileWAL {
					time.Sleep(75 * time.Millisecond)
				}
				diagnostic.event("xSync_OS_dispatch file=%d", file)
			}, func(file int, from, to time.Time) {
				diagnostic.event("xSync_return file=%d from_ns=%d to_ns=%d wrapper_sync_includes_before_callback=%s", file, from.UnixNano(), to.UnixNano(), to.Sub(from))
			})
			attempt := &backgroundCheckpointAttempt{copy: &walCopyAttempt{pressure: true}}
			result := s.convergeBackfillPacedWithSmallRemainder(ctx, db, attempt, walReclaimPressureSmallFrames)
			diagnostic.flush()
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

// Diagnostic events are buffered to avoid synchronous log output while a writer
// credit or VFS sync is active. Every workload and assertion above is unchanged.
type windowsConvergenceDiagnostic struct {
	t       *testing.T
	mu      sync.Mutex
	events  []string
	dropped int
	once    sync.Once
}

func (d *windowsConvergenceDiagnostic) event(format string, args ...any) {
	stamp := time.Now()
	line := fmt.Sprintf("at_ns=%d ", stamp.UnixNano()) + fmt.Sprintf(format, args...)
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.events) >= 512 {
		d.dropped++
		return
	}
	d.events = append(d.events, line)
}

func (d *windowsConvergenceDiagnostic) record(phase string, ctx context.Context, from time.Time, took time.Duration, err error) {
	deadline, hasDeadline := ctx.Deadline()
	d.event("phase=%s from_ns=%d elapsed=%s deadline_ns=%d has_deadline=%v context_error=%v cause=%v result_error=%v", phase, from.UnixNano(), took, deadline.UnixNano(), hasDeadline, ctx.Err(), context.Cause(ctx), err)
}

func (d *windowsConvergenceDiagnostic) flush() {
	d.once.Do(func() {
		d.mu.Lock()
		events := append([]string(nil), d.events...)
		dropped := d.dropped
		d.mu.Unlock()
		for _, event := range events {
			d.t.Logf("WINDOWS_PHASE %s", event)
		}
		d.t.Logf("WINDOWS_PHASE events=%d dropped=%d", len(events), dropped)
	})
}

func installWindowsConvergenceDiagnostic(t *testing.T) *windowsConvergenceDiagnostic {
	d := &windowsConvergenceDiagnostic{t: t}
	previousPhase := walWindowsDiagnosticPhase
	walWindowsDiagnosticPhase = d.record
	previousResult := walCheckpointResultObserver
	walCheckpointResultObserver = func(mode string, from time.Time, took time.Duration, result walCheckpointResult, err error) {
		d.event("checkpoint_return mode=%s from_ns=%d took=%s tuple=%+v scan_error=%v", mode, from.UnixNano(), took, result, err)
		if previousResult != nil {
			previousResult(mode, from, took, result, err)
		}
	}
	t.Cleanup(func() {
		walWindowsDiagnosticPhase = previousPhase
		walCheckpointResultObserver = previousResult
		d.flush()
	})
	return d
}
