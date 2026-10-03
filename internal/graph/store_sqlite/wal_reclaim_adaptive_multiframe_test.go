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

// One ordinary final copy starts with a small tail, then the slow sync lets
// real writes leave more than256frames. Its adaptive completion must use the
// bounded time credit. If that direct helper leaves a new tail, recovery uses
// production convergence and admission, not repeated helper calls or a
// separate background PASSIVE checkpoint.
func TestUrgentAdaptiveCopyAdmitsMultiFrameTail(t *testing.T) {
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

func TestAdaptiveDeclinedEntryPreservesCopyEntitlement(t *testing.T) {
	for _, kind := range []string{"lane", "bulk", "new_wal"} {
		t.Run(kind, func(t *testing.T) {
			s, db := finalBackfillFixture(t)
			frontier, ok := readWALReclaimFrontier(s.dbPath)
			require.True(t, ok)
			r := &walReclaimResult{urgent: true, slowTail: &walReclaimSlowTail{salt: frontier.salt, covered: frontier.backfill, copyElapsed: 300 * time.Millisecond}}
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			switch kind {
			case "lane":
				lane := &fakeBuildLane{}
				lane.install(s)
				lane.held.Store(true)
				t.Cleanup(func() { lane.held.Store(false) })
			case "bulk":
				engaged, err := s.BeginGenerationBulkLoad(77)
				require.NoError(t, err)
				require.True(t, engaged)
				t.Cleanup(func() { _ = s.EndGenerationBulkLoadFor(77) })
			case "new_wal":
				_, err := checkpointWALOnceOn(ctx, db, "TRUNCATE")
				require.NoError(t, err)
				s.writeMu.Lock()
				_, err = s.writerDB.ExecContext(ctx, `UPDATE wal_churn SET payload = 'new-WAL' WHERE id = 1`)
				s.writeMu.Unlock()
				require.NoError(t, err)
			}
			budget := r.takeAdaptiveWriterBudget()
			require.Positive(t, budget)
			done, _ := s.reclaimWALResetHoldOnce(ctx, walReclaimConfig{}, db, r, false, true, budget)
			require.False(t, done)
			require.False(t, r.adaptiveUsed, "a declined admission consumed the single actual copy entitlement")
			require.Positive(t, r.takeAdaptiveWriterBudget())
			require.Less(t, r.writerSpent, walReclaimResetHold)
		})
	}
}

// This test-only scope publishes a fully initialized TLS observer. It neither
// restores live VFS method tables nor changes SQL/copy contexts or writer credit.
func observeAdaptiveMultiFrameSync(t *testing.T, db *sql.DB, before func(int), after func(int, time.Time, time.Time)) {
	t.Helper()
	conn, err := db.Conn(t.Context())
	require.NoError(t, err)
	key, ok := pausableConn(conn)
	require.NoError(t, conn.Close())
	require.True(t, ok)
	reclaimSyncStallState.Store(&reclaimSyncStall{tls: key, beforeSync: before, afterSync: after})
	t.Cleanup(func() { reclaimSyncStallState.Store(nil) })
}
