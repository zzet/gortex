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

// A causal control: a short completed sync is not a bound on the next
// durable sync. No readers are involved, so failures cannot be a pinned cohort.
func TestVariableSyncSingleAdaptiveAttempt(t *testing.T) {
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
