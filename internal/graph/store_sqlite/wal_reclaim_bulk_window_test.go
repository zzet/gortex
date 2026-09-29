package store_sqlite

import (
	"context"
	"database/sql"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// A generation bulk window holds the checkpoint lease for as long as it is
// open — live, 14 minutes per closure build while the log grew to 22 GB. Over
// the ceiling the reclaim runs inside the window anyway (bounded by the
// writer-hold cap, its writer step taking the write gate between the window's
// own transactions), so the log stays near the ceiling while the window stays
// open and keeps writing.
func TestWALReclaimRunsInsideALongBulkWindowOverTheCeiling(t *testing.T) {
	t.Setenv("GORTEX_SQLITE_WAL_RECLAIM_MB", "8")
	t.Setenv("GORTEX_SQLITE_WAL_RECLAIM_CEILING_MB", "48")
	prevFloor, prevSpacing := walReclaimCeilingFloor, walReclaimLeaseOverrideSpacing
	walReclaimCeilingFloor, walReclaimLeaseOverrideSpacing = 0, 2*time.Second
	t.Cleanup(func() { walReclaimCeilingFloor, walReclaimLeaseOverrideSpacing = prevFloor, prevSpacing })
	setWALReclaimCadence(t, 500*time.Millisecond, 500*time.Millisecond, 4*time.Second)
	logs := captureReclaimLog(t)
	s, path := openWALReclaimStore(t)
	defer func() { _ = s.Close() }()
	seedWALChurnTable(t, s)

	engaged, err := s.BeginGenerationBulkLoad(77)
	require.NoError(t, err)
	require.True(t, engaged, "the bulk window must open")
	defer func() { _ = s.EndGenerationBulkLoadFor(77) }()

	stop := make(chan struct{})
	var wg sync.WaitGroup
	var writes atomic.Int64
	var writeErr atomic.Value
	wg.Add(1)
	go func() { // the window's writes: ~1 MiB of page rewrites every 60 ms
		defer wg.Done()
		for k := 0; ; k++ {
			select {
			case <-stop:
				return
			case <-time.After(60 * time.Millisecond):
			}
			s.writeMu.Lock()
			tx, err := s.beginWriteContext(context.Background())
			if err == nil {
				_, err = tx.Exec(`UPDATE wal_churn SET payload = randomblob(1024) WHERE id % 16 = ?`, k%16)
				if err == nil {
					err = tx.Commit()
				} else {
					_ = tx.Rollback()
				}
			}
			s.writeMu.Unlock()
			if err != nil {
				writeErr.Store(err)
				return
			}
			writes.Add(1)
		}
	}()
	for r := 0; r < 2; r++ {
		wg.Add(1)
		go func() { // short readers
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				tx, err := s.db.BeginTx(context.Background(), &sql.TxOptions{ReadOnly: true})
				if err != nil {
					return
				}
				var n int
				_ = tx.QueryRow(`SELECT count(*) FROM wal_churn WHERE id % 97 = 0`).Scan(&n)
				time.Sleep(150 * time.Millisecond)
				_ = tx.Rollback()
			}
		}()
	}
	var maxWAL atomic.Int64
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			case <-time.After(5 * time.Millisecond):
			}
			if size := walFileSize(path + "-wal"); size > maxWAL.Load() {
				maxWAL.Store(size)
			}
		}
	}()
	time.Sleep(25 * time.Second)
	close(stop)
	wg.Wait()
	if err, _ := writeErr.Load().(error); err != nil {
		t.Fatal(err)
	}
	st := s.WALReclaimStats()
	out := logs.String()
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "wal reclaim") {
			t.Log(line)
		}
	}
	t.Logf("bulk window 25 s: writes=%d wal_max=%.1fMiB ceiling=48MiB lease_overrides=%d resets=%d writer_hold_max=%s",
		writes.Load(), float64(maxWAL.Load())/(1<<20), st.LeaseOverrides, st.Resets, st.WriterHoldMax)
	require.Positive(t, st.LeaseOverrides, "no reclaim ran inside the bulk window")
	require.Positive(t, st.Resets, "the log was never reset while the window stayed open")
	require.LessOrEqual(t, maxWAL.Load(), int64(2*48)<<20, "the log passed twice its ceiling inside the bulk window")
	require.LessOrEqual(t, st.WriterHoldMax, walReclaimMaxWriterHold+250*time.Millisecond)
	require.Contains(t, out, "wal reclaim running inside a bulk window reason=wal_ceiling")
}

// A log that is backfilled but for a few frames — one seen live: 3,625 of
// 4.2 M frames left — resets with a short writer hold: the writer step copies
// only the remainder and the reset gets the rest of the hold cap. An attempt
// that fails on it (a reader outliving the cap) after advancing the backfill
// retries in seconds, not after the minutes of backoff earlier failures built
// up (live: "next_attempt_in=5m0s" at 11–17 GB).
func TestWALReclaimResetsANearlyBackfilledLogWithAShortHold(t *testing.T) {
	t.Setenv("GORTEX_SQLITE_WAL_RECLAIM_MB", "0")
	s, path := openWALReclaimStore(t)
	defer func() { _ = s.Close() }()
	seedWALChurnTable(t, s)
	ckpt, err := openWALReclaimCheckpointDB(s.dbPath)
	require.NoError(t, err)
	defer ckpt.Close()

	// A big log, then a reader pinned near its end, a few more frames, and a
	// PASSIVE that backfills everything up to the reader's mark.
	early, err := s.db.BeginTx(context.Background(), &sql.TxOptions{ReadOnly: true})
	require.NoError(t, err)
	var n int
	require.NoError(t, early.QueryRow(`SELECT count(*) FROM wal_churn`).Scan(&n))
	growWAL(t, s, 300)
	require.NoError(t, early.Rollback())
	late, err := s.db.BeginTx(context.Background(), &sql.TxOptions{ReadOnly: true})
	require.NoError(t, err)
	require.NoError(t, late.QueryRow(`SELECT count(*) FROM wal_churn`).Scan(&n))
	growWAL(t, s, 2)
	_, _ = checkpointWALOnceOn(context.Background(), ckpt, "PASSIVE")
	require.NoError(t, late.Rollback())
	snap, ok := readWALIndexSnapshot(path)
	require.True(t, ok)
	remainder := snap.MxFrame - snap.NBackfill
	t.Logf("log: wal=%.0fMiB frames=%d backfilled=%d remainder=%d", float64(walFileSize(path+"-wal"))/(1<<20), snap.MxFrame, snap.NBackfill, remainder)
	require.Positive(t, remainder)
	require.Less(t, remainder, snap.MxFrame/50, "precondition: nearly backfilled")

	cfg := walReclaimConfig{thresholdBytes: 16 << 20, drainDeadline: defaultWALReclaimDrainDeadline, truncateBudget: walReclaimTruncateBudget, readerWait: defaultWALReclaimReaderWait}

	// The live sequence first: an attempt meets a reader that outlives the
	// writer-hold cap and fails after advancing the backfill, on a schedule
	// that earlier no-progress failures had pushed to minutes. The next
	// attempt must be seconds away, not the live "next_attempt_in=5m0s".
	blocker, err := s.db.BeginTx(context.Background(), &sql.TxOptions{ReadOnly: true})
	require.NoError(t, err)
	require.NoError(t, blocker.QueryRow(`SELECT count(*) FROM wal_churn`).Scan(&n))
	growWAL(t, s, 1)
	sched := newWALReclaimSchedule(walReclaimBackoffInitial, min(walReclaimBackoffMax, walReclaimPressureBackoffMax))
	for i := 0; i < 6; i++ {
		sched.observeProgress(time.Now(), walReclaimDeferred, false)
	}
	failed := s.reclaimWALOnce(cfg, ckpt, path+"-wal")
	now := time.Now()
	sched.observeProgress(now, failed.outcome, failed.progressed)
	t.Logf("blocked attempt: outcome=%s reason=%q progressed=%t writer_hold=%s next_attempt_in=%s",
		failed.outcome, failed.reason, failed.progressed, failed.writerHold, sched.nextAt.Sub(now).Round(time.Second))
	require.NoError(t, blocker.Rollback())
	require.Equal(t, walReclaimDeferred, failed.outcome, "reason=%q", failed.reason)
	require.True(t, failed.progressed, "the blocked attempt still advanced the backfill")
	require.LessOrEqual(t, sched.nextAt.Sub(now), walReclaimBackoffInitial, "a failure that made progress was pushed minutes away")

	started := time.Now()
	res := s.reclaimWALOnce(cfg, ckpt, path+"-wal")
	t.Logf("reclaim: outcome=%s reason=%q writer_hold=%s elapsed=%s%s", res.outcome, res.reason, res.writerHold, time.Since(started), res.convergenceSuffix())
	require.Equal(t, walReclaimReset, res.outcome, "reason=%q", res.reason)
	require.Less(t, res.writerHold, walReclaimMaxWriterHold, "the reset held the writer to the cap")
	require.Zero(t, walFileSize(path+"-wal"))
}
