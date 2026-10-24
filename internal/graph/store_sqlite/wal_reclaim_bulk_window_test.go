package store_sqlite

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// A log that is backfilled but for a few frames — one seen live: 3,625 of
// 4.2 M frames left — resets with a short writer hold: the writer step copies
// only the remainder and the reset gets the rest of the hold cap. An attempt
// that fails on it (a reader outliving the cap) after advancing the backfill
// retries in seconds, not after the minutes of backoff earlier failures built
// up (live: "next_attempt_in=5m0s" at 11–17 GB).
func TestWALReclaimResetsANearlyBackfilledLogWithAShortHold(t *testing.T) {
	t.Setenv("GORTEX_SQLITE_WAL_RECLAIM_MB", "0")
	// This WAL fixture does not exercise the unrelated lazy index worker.
	t.Setenv("GORTEX_SQLITE_LAZY_INDEXES", "off")
	s, path := openWALReclaimStore(t)
	defer func() { _ = s.Close() }()
	// Own the checkpoint frontier throughout setup and the measured attempts.
	// A periodic PASSIVE could otherwise copy the pending prefix after late
	// rolls back but before reclaim captures its entry snapshot, making this
	// attempt correctly report no progress. Joining the loop closes its stop
	// channel; manual reclaim watchers need a live, fixture-owned one.
	s.stopCheckpointLoop()
	manualStop := make(chan struct{})
	s.stopCheckpoint = manualStop
	defer close(manualStop)
	seedWALChurnTable(t, s)
	ckpt, err := openWALReclaimCheckpointDB(s.dbPath)
	require.NoError(t, err)
	defer ckpt.Close()

	// A big log (~150 MiB, ten times the threshold below), then a reader
	// pinned near its end, a few more frames (~500 of ~39k: under the fiftieth
	// the precondition asks), and a PASSIVE that backfills everything up to
	// the reader's mark.
	early, err := s.db.BeginTx(context.Background(), &sql.TxOptions{ReadOnly: true})
	require.NoError(t, err)
	var n int
	require.NoError(t, early.QueryRow(`SELECT count(*) FROM wal_churn`).Scan(&n))
	growWAL(t, s, 150)
	require.NoError(t, early.Rollback())
	late, err := s.db.BeginTx(context.Background(), &sql.TxOptions{ReadOnly: true})
	require.NoError(t, err)
	require.NoError(t, late.QueryRow(`SELECT count(*) FROM wal_churn`).Scan(&n))
	growWAL(t, s, 2)
	// Keep setup bounded and verify the actual backfill frontier.
	var snap walIndexSnapshot
	var ok bool
	for i := 0; i < 50; i++ {
		_, _ = checkpointWALOnceOn(context.Background(), ckpt, "PASSIVE")
		if snap, ok = readWALIndexSnapshot(path); ok && snap.MxFrame-snap.NBackfill < snap.MxFrame/50 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	require.NoError(t, late.Rollback())
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
	// The blocker outlives every wait of the attempt (30 s writer-free, 5 s
	// in the lane by default); shorter waits end it the same way, sooner.
	blockedCfg := cfg
	blockedCfg.readerWait = 500 * time.Millisecond
	shortenWALReclaimLaneReaderWait(t, 500*time.Millisecond)
	failed := s.reclaimWALOnce(blockedCfg, ckpt, path+"-wal")
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
