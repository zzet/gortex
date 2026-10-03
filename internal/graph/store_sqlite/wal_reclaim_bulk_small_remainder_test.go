package store_sqlite

import (
	"context"
	"fmt"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// Plant a near 4 MiB tail during the first actual sync, then make its next WAL
// sync outlive the unchanged 2 s credit. The old unmeasured 1024-frame shortcut
// sends that copy into a held pass; bulk completion must first converge it
// writer-free. This is a VFS boundary delay, not an actual OS latency claim.
func TestBulkCompletionConvergesUnmeasuredTailBeforeWriterCredit(t *testing.T) {
	var walSyncs atomic.Int32
	var secondMu sync.Mutex
	var secondBefore, secondAfter walIndexSnapshot
	var secondAfterOK bool
	var secondResult walCheckpointResult
	var secondScanErr error
	var secondObserved bool
	var checkpointPath string
	priorResultObserver := walCheckpointResultObserver
	// Install before Open; restore after fixture Close and all owned SQL joins.
	// The counter identifies this scoped connection's second actual WAL sync.
	walCheckpointResultObserver = func(mode string, started time.Time, took time.Duration, result walCheckpointResult, err error) {
		if priorResultObserver != nil {
			priorResultObserver(mode, started, took, result, err)
		}
		if mode != "PASSIVE" || walSyncs.Load() != 2 {
			return
		}
		secondMu.Lock()
		if secondObserved {
			secondMu.Unlock()
			return
		}
		secondAfter, secondAfterOK = readWALIndexSnapshot(checkpointPath)
		secondResult, secondScanErr, secondObserved = result, err, true
		secondMu.Unlock()
	}
	t.Cleanup(func() { walCheckpointResultObserver = priorResultObserver })
	s, db := finalBackfillFixture(t)
	checkpointPath = s.dbPath
	growWAL(t, s, 6)
	engaged, err := s.BeginGenerationBulkLoad(77)
	require.NoError(t, err)
	require.True(t, engaged)
	t.Cleanup(func() { require.NoError(t, s.EndGenerationBulkLoadFor(77)) })
	cfg := walReclaimConfig{thresholdBytes: 1 << 20, ceilingBytes: 4 << 20, readerWait: time.Second, truncateBudget: walReclaimTruncateBudget}
	path := s.dbPath + "-wal"
	require.GreaterOrEqual(t, walFileSize(path), cfg.ceilingBytes)
	require.Less(t, walFileSize(path), walReclaimHardCapFactor*cfg.ceilingBytes)
	entered, release := stallReclaimCheckpointSync(t, db)
	state := reclaimSyncStallState.Load()
	require.NotNil(t, state)
	var secondStage string
	var secondTail uint32
	state.beforeSync = func(kind int) {
		if kind != vfsFileWAL || walSyncs.Add(1) != 2 {
			return
		}
		if snap, ok := readWALIndexSnapshot(s.dbPath); ok && snap.MxFrame > snap.NBackfill {
			secondBefore = snap
			secondTail = snap.MxFrame - snap.NBackfill
		}
		pcs := make([]uintptr, 64)
		frames := runtime.CallersFrames(pcs[:runtime.Callers(2, pcs)])
		for {
			frame, more := frames.Next()
			if strings.Contains(frame.Function, "convergeBackfillPacedWithSmallRemainder") {
				secondStage = "writer_free_convergence"
			}
			if strings.Contains(frame.Function, "walReclaimWriterCredit).passive") {
				secondStage = "held_final_copy"
			}
			if !more {
				break
			}
		}
		time.Sleep(walReclaimMaxWriterHold + 500*time.Millisecond)
	}
	var releaseOnce sync.Once
	unpark := func() { releaseOnce.Do(func() { close(release) }) }
	done := make(chan walReclaimResult, 1)
	joined := make(chan struct{})
	producerJoined := make(chan struct{})
	producerCtx, stopProducer := context.WithCancel(t.Context())
	var producerStarted bool
	var owned *backgroundCheckpointAttempt
	t.Cleanup(func() {
		stopProducer()
		if owned != nil {
			owned.cancel(context.Canceled)
		}
		unpark()
		if producerStarted {
			select {
			case <-producerJoined:
			case <-time.After(5 * time.Second):
				t.Error("producer failed to join")
			}
		}
		select {
		case <-joined:
		case <-time.After(5 * time.Second):
			t.Error("bulk attempt failed to join")
		}
	})
	go func() { done <- s.reclaimWALAttempt(cfg, db, path); close(joined) }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("no actual initial checkpoint sync")
	}
	s.backgroundCheckpoint.mu.Lock()
	owned = s.backgroundCheckpoint.active
	s.backgroundCheckpoint.mu.Unlock()
	require.NotNil(t, owned)
	// Use the pinned bulk writer rather than opening the exhausted writer pool.
	for k := 0; k < 4; k++ {
		ctx, cancel := context.WithTimeout(t.Context(), walReclaimWriterWait)
		err := s.writeMu.LockContext(ctx)
		if err == nil {
			tx, beginErr := s.beginWriteContext(ctx)
			err = beginErr
			if err == nil {
				_, err = tx.ExecContext(ctx, `UPDATE wal_churn SET payload=randomblob(1024) WHERE id % 16=?`, k)
				if err == nil {
					err = tx.Commit()
				} else {
					_ = tx.Rollback()
				}
			}
			s.writeMu.Unlock()
		}
		cancel()
		require.NoError(t, err)
	}
	var writes atomic.Int64
	var worstForeground atomic.Int64
	var producerErr error
	producerStarted = true
	go func() {
		defer close(producerJoined)
		for {
			select {
			case <-producerCtx.Done():
				return
			case <-time.After(150 * time.Millisecond):
			}
			ctx, cancel := context.WithTimeout(producerCtx, walReclaimWriterWait)
			start := time.Now()
			err := s.writeMu.LockContext(ctx)
			if err == nil {
				tx, beginErr := s.beginWriteContext(ctx)
				err = beginErr
				if err == nil {
					seq := writes.Load() + 1
					_, err = tx.ExecContext(ctx, `UPDATE wal_churn SET payload=randomblob(1024) WHERE id % 16=? AND id<>1`, seq%16)
					if err == nil {
						_, err = tx.ExecContext(ctx, `UPDATE wal_churn SET payload=? WHERE id=1`, fmt.Sprintf("bulk-live-%d", seq))
					}
					if err == nil {
						err = tx.Commit()
					} else {
						_ = tx.Rollback()
					}
					if err == nil {
						writes.Add(1)
					}
				}
				s.writeMu.Unlock()
			}
			elapsed := time.Since(start)
			for prev := worstForeground.Load(); int64(elapsed) > prev && !worstForeground.CompareAndSwap(prev, int64(elapsed)); prev = worstForeground.Load() {
			}
			cancel()
			if err != nil {
				if producerCtx.Err() == nil {
					producerErr = err
				}
				return
			}
		}
	}()
	unpark()
	var result walReclaimResult
	select {
	case result = <-done:
	case <-time.After(12 * time.Second):
		t.Fatal("bulk attempt did not finish within its existing operation scope")
	}
	writesAtReturn := writes.Load()
	until := time.Now().Add(time.Second)
	for writes.Load() <= writesAtReturn && time.Now().Before(until) {
		time.Sleep(time.Millisecond)
	}
	stopProducer()
	select {
	case <-producerJoined:
	case <-time.After(5 * time.Second):
		t.Fatal("producer did not join")
	}
	producerStarted = false
	secondMu.Lock()
	frontier, frontierOK := secondAfter, secondAfterOK
	copied, scanErr, observed := secondResult, secondScanErr, secondObserved
	secondMu.Unlock()
	t.Logf("bulk=%v pressure=%v outcome=%v reason=%s second_sync_stage=%s planted_tail=%d second_checkpoint_post_return_frontier=%+v valid=%v actual_scan_tuple=%+v scan_error=%v observed=%v convergence=%v actual_writer_hold=%s writes_at_return=%d final_writes=%d foreground_gate_SQL_max=%s producer_error=%v", result.bulkCompletion, result.pressure, result.outcome, result.reason, secondStage, secondTail, frontier, frontierOK, copied, scanErr, observed, result.convergence, result.writerHold, writesAtReturn, writes.Load(), time.Duration(worstForeground.Load()), producerErr)
	require.NoError(t, producerErr)
	require.True(t, result.bulkCompletion)
	require.False(t, result.pressure, "bulk lease override retains its own policy")
	require.Greater(t, secondTail, walReclaimPressureSmallFrames)
	require.LessOrEqual(t, secondTail, walReclaimConvergeSmallFrames, "counterfactual must exercise old unmeasured-small fallback")
	require.Equal(t, "writer_free_convergence", secondStage)
	require.True(t, observed, "the second copy must produce an actual checkpoint Scan result")
	require.NoError(t, scanErr)
	require.True(t, frontierOK)
	require.Greater(t, frontier.NBackfill, secondBefore.NBackfill, "the completed writer-free copy must advance its post-return frontier")
	require.NotNil(t, result.convergence)
	require.Positive(t, result.convergence.passes)
	require.Equal(t, walReclaimReset, result.outcome)
	require.Greater(t, writes.Load(), writesAtReturn, "actual writes must continue after authoritative reset")
	require.LessOrEqual(t, result.writerHold, walReclaimMaxWriterHold+250*time.Millisecond)
	require.LessOrEqual(t, time.Duration(worstForeground.Load()), walReclaimMaxWriterHold+250*time.Millisecond)
	require.False(t, s.readGate.closed.Load())
	var payload string
	require.NoError(t, s.db.QueryRow(`SELECT payload FROM wal_churn WHERE id=1`).Scan(&payload))
	require.Equal(t, fmt.Sprintf("bulk-live-%d", writes.Load()), payload)
}
