package store_sqlite

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// One ordinary final copy starts with a small tail, then the slow sync lets
// real writes leave more than256frames. Its adaptive completion must use the
// bounded time credit. Repeated pipeline convergence is covered by load tests.
func TestUrgentAdaptiveCopyAdmitsMultiFrameTail(t *testing.T) {
	for _, kind := range []string{"short", "pressure"} {
		t.Run(kind, func(t *testing.T) {
			s, db := finalBackfillFixture(t)
			priorRate := walHoldCopyRate.Swap(0)
			t.Cleanup(func() { walHoldCopyRate.Store(priorRate) })
			// The operation may join an uninterruptible sync after its writer credit
			// expires. Use its existing finite lifetime; held credit remains <=2 s.
			ctx, cancel := context.WithTimeout(t.Context(), walReclaimLaneBudget)
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
			started := time.Now()
			resets := 0
			adaptiveAttempts := 0
			var aggregateMax time.Duration
			var maxAdaptiveEntryTail uint32
			t.Cleanup(func() { walIdleResetHook = nil })
			for ctx.Err() == nil && resets < 1 {
				observeDelayedReclaimSync(t, db, func(file int) {
					if file == vfsFileWAL {
						syncs.Add(1)
						time.Sleep(300 * time.Millisecond)
					}
				})
				res := &walReclaimResult{urgent: true}
				walIdleResetHook = func() {
					if res.slowTail != nil {
						if snap, ok := readWALIndexSnapshot(s.dbPath); ok && snap.MxFrame > snap.NBackfill {
							maxAdaptiveEntryTail = max(maxAdaptiveEntryTail, snap.MxFrame-snap.NBackfill)
						}
					}
				}
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
			t.Logf("resets=%d writes=%d WAL_syncs=%d adaptive_attempts=%d aggregate_hold_max=%s worst_gate=%s worst_SQL=%s elapsed=%s producer_error=%v adaptive_entry_tail_frames=%d", resets, writes.Load(), syncs.Load(), adaptiveAttempts, aggregateMax, time.Duration(worstGate.Load()), time.Duration(worstSQL.Load()), time.Since(started), producerErr, maxAdaptiveEntryTail)
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
