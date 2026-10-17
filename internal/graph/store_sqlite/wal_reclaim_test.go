package store_sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

const walReclaimJournalLimit = 64 << 20 // journal_size_limit on every writer DSN

// openWALReclaimStore opens a private on-disk store WITHOUT registering a
// Close cleanup, so a case can close it itself and observe the result.
func openWALReclaimStore(t *testing.T) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "wal-reclaim.sqlite")
	s, err := openPristine(t, path)
	require.NoError(t, err)
	return s, path
}

func setWALReclaimCadence(t *testing.T, poll, initial, maximum time.Duration) {
	t.Helper()
	prevPoll, prevInitial, prevMax := walReclaimPollInterval, walReclaimBackoffInitial, walReclaimBackoffMax
	walReclaimPollInterval, walReclaimBackoffInitial, walReclaimBackoffMax = poll, initial, maximum
	t.Cleanup(func() {
		walReclaimPollInterval, walReclaimBackoffInitial, walReclaimBackoffMax = prevPoll, prevInitial, prevMax
	})
}

// seedWALChurnTable creates a scratch table whose rows spread across ~1,400
// pages, so each update transaction below rewrites ~250 pages (~1 MiB of WAL
// frames) — page rewrites, as in the observed incident.
func seedWALChurnTable(t *testing.T, s *Store) {
	t.Helper()
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	_, err := s.writerDB.Exec(`CREATE TABLE IF NOT EXISTS wal_churn(id INTEGER PRIMARY KEY, payload BLOB)`)
	require.NoError(t, err)
	_, err = s.writerDB.Exec(`WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM n WHERE i < 4000)
		INSERT INTO wal_churn(id, payload) SELECT i, randomblob(1024) FROM n`)
	require.NoError(t, err)
}

func churnWriteOnce(s *Store, k int) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	tx, err := s.writerDB.Begin()
	if err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE wal_churn SET payload = randomblob(1024) WHERE id % 16 = ?`, k%16); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

func growWAL(t *testing.T, s *Store, writes int) {
	t.Helper()
	for k := 0; k < writes; k++ {
		require.NoError(t, churnWriteOnce(s, k))
	}
}

// A reader that never leaves: the reclaim gives up at the drain deadline,
// reopens the gate, backs off, and never blocks other readers past the bound.
func TestWALReclaimDefersWhenReadersNeverDrain(t *testing.T) {
	t.Setenv("GORTEX_SQLITE_WAL_RECLAIM_MB", "0") // drive attempts directly
	s, path := openWALReclaimStore(t)
	defer func() { _ = s.Close() }()
	seedWALChurnTable(t, s)
	growWAL(t, s, 8)

	pinned, err := s.db.BeginTx(context.Background(), &sql.TxOptions{ReadOnly: true})
	require.NoError(t, err)
	defer func() { _ = pinned.Rollback() }()
	var n int
	require.NoError(t, pinned.QueryRow(`SELECT count(*) FROM wal_churn`).Scan(&n))
	growWAL(t, s, 8)

	ckpt, err := openWALReclaimCheckpointDB(s.dbPath)
	require.NoError(t, err)
	defer ckpt.Close()
	cfg := walReclaimConfig{thresholdBytes: 1 << 20, drainDeadline: 100 * time.Millisecond, truncateBudget: walReclaimTruncateBudget}

	// A probe reader keeps issuing short reads while the attempt runs.
	stop := make(chan struct{})
	var probeMax atomic.Int64
	var probeWG sync.WaitGroup
	probeWG.Add(1)
	go func() {
		defer probeWG.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			start := time.Now()
			var c int
			if err := s.db.QueryRow(`SELECT count(*) FROM wal_churn WHERE id < 10`).Scan(&c); err != nil {
				return
			}
			if d := int64(time.Since(start)); d > probeMax.Load() {
				probeMax.Store(d)
			}
			time.Sleep(2 * time.Millisecond)
		}
	}()

	seqBefore, _ := readWALIndexSnapshot(path)
	started := time.Now()
	res := s.reclaimWALOnce(cfg, ckpt, path+"-wal")
	elapsed := time.Since(started)
	close(stop)
	probeWG.Wait()
	seqAfter, _ := readWALIndexSnapshot(path)
	t.Logf("pinned reader: outcome=%s reason=%q elapsed=%s pause=%s probe_max=%s", res.outcome, res.reason, elapsed, res.pause, time.Duration(probeMax.Load()))

	require.Equal(t, walReclaimDeferred, res.outcome)
	require.Equal(t, "readers_in_flight=1", res.reason)
	require.GreaterOrEqual(t, seqAfter.MxFrame, seqBefore.MxFrame, "no reset while the reader is pinned")
	require.GreaterOrEqual(t, seqAfter.WALBytes, seqBefore.WALBytes, "no truncation while the reader is pinned")
	require.False(t, s.readGate.closed.Load(), "the gate must be reopened after a deferral")
	require.LessOrEqual(t, res.pause, cfg.drainDeadline+50*time.Millisecond)
	require.LessOrEqual(t, time.Duration(probeMax.Load()), cfg.drainDeadline+150*time.Millisecond,
		"a reader was held past the drain deadline")
	require.Less(t, elapsed, 5*time.Second, "a deferral must not block")
	require.Equal(t, int64(1), s.WALReclaimStats().Deferrals)

	// Backoff doubles per deferral and caps.
	sched := newWALReclaimSchedule(5*time.Second, 20*time.Second)
	now := time.Now()
	sched.observe(now, walReclaimDeferred)
	require.Equal(t, now.Add(5*time.Second), sched.nextAt)
	require.False(t, sched.ready(now.Add(4*time.Second)))
	sched.observe(now, walReclaimDeferred)
	require.Equal(t, now.Add(10*time.Second), sched.nextAt)
	sched.observe(now, walReclaimDeferred)
	sched.observe(now, walReclaimDeferred)
	require.Equal(t, 20*time.Second, sched.backoff)
	sched.observe(now, walReclaimSkipped)
	require.Equal(t, 20*time.Second, sched.backoff, "a skip does not back off")
	sched.observe(now, walReclaimReset)
	require.Equal(t, 5*time.Second, sched.backoff)
	require.True(t, sched.ready(now))

	// Once the reader leaves, the next attempt resets and truncates the log.
	require.NoError(t, pinned.Rollback())
	res = s.reclaimWALOnce(cfg, ckpt, path+"-wal")
	require.Equal(t, walReclaimReset, res.outcome, "reason=%q", res.reason)
	require.Zero(t, walFileSize(path+"-wal"))
	require.Greater(t, res.frames, 0)
}

// The reclaim never runs while a generation bulk window holds the checkpoint
// lease, and a window opening mid-reclaim cancels it inside the lease's wait.
func TestWALReclaimRespectsGenerationBulkLease(t *testing.T) {
	t.Setenv("GORTEX_SQLITE_WAL_RECLAIM_MB", "0")
	s, path := openWALReclaimStore(t)
	defer func() { _ = s.Close() }()
	seedWALChurnTable(t, s)
	growWAL(t, s, 8)

	ckpt, err := openWALReclaimCheckpointDB(s.dbPath)
	require.NoError(t, err)
	defer ckpt.Close()
	cfg := walReclaimConfig{thresholdBytes: 1 << 20, drainDeadline: 3 * time.Second, truncateBudget: walReclaimTruncateBudget}

	lease, err := s.acquireGenerationBulkCheckpointLease()
	require.NoError(t, err)
	before, _ := readWALIndexSnapshot(path)
	sizeBefore := walFileSize(path + "-wal")
	res := s.reclaimWALOnce(cfg, ckpt, path+"-wal")
	after, _ := readWALIndexSnapshot(path)
	require.Equal(t, walReclaimSkipped, res.outcome)
	require.Equal(t, "checkpoint_lease", res.reason)
	require.Equal(t, before.MxFrame, after.MxFrame, "no reset under the lease")
	require.Equal(t, before.NBackfill, after.NBackfill, "not even the PASSIVE step may run under the lease")
	require.Equal(t, sizeBefore, walFileSize(path+"-wal"))
	require.True(t, s.releaseGenerationBulkCheckpointLease(lease))

	// Mid-flight: pin a reader so the reclaim parks in the drain wait with
	// the gate closed, then open a bulk window.
	pinned, err := s.db.BeginTx(context.Background(), &sql.TxOptions{ReadOnly: true})
	require.NoError(t, err)
	defer func() { _ = pinned.Rollback() }()
	var n int
	require.NoError(t, pinned.QueryRow(`SELECT count(*) FROM wal_churn`).Scan(&n))
	done := make(chan walReclaimResult, 1)
	go func() { done <- s.reclaimWALOnce(cfg, ckpt, path+"-wal") }()
	require.Eventually(t, func() bool { return s.readGate.closed.Load() }, 5*time.Second, time.Millisecond)
	leaseStart := time.Now()
	lease, err = s.acquireGenerationBulkCheckpointLease()
	require.NoError(t, err, "the bulk window must win against an in-flight reclaim")
	leaseWait := time.Since(leaseStart)
	var mid walReclaimResult
	select {
	case mid = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("reclaim did not stop after the bulk lease cancelled it")
	}
	t.Logf("bulk lease mid-reclaim: lease_wait=%s outcome=%s reason=%q", leaseWait, mid.outcome, mid.reason)
	require.Less(t, leaseWait, s.passiveCheckpointWindow())
	require.Equal(t, walReclaimSkipped, mid.outcome)
	require.Equal(t, "bulk_lease", mid.reason)
	require.False(t, s.readGate.closed.Load(), "the gate must be reopened when the lease cancels the reclaim")
	require.True(t, s.releaseGenerationBulkCheckpointLease(lease))
	require.NoError(t, pinned.Rollback())

	res = s.reclaimWALOnce(cfg, ckpt, path+"-wal")
	require.Equal(t, walReclaimReset, res.outcome, "reason=%q", res.reason)
}

// Close drains a large un-backfilled log: the budget scales with the backlog,
// the final TRUNCATE resets the log, and the file is removed or empty.
func TestCloseCheckpointDrainsLargeBacklog(t *testing.T) {
	t.Setenv("GORTEX_SQLITE_WAL_RECLAIM_MB", "0")
	s, path := openWALReclaimStore(t)
	seedWALChurnTable(t, s)

	// A pinned reader holds backfill at its snapshot, so the log accumulates
	// un-backfilled frames the way a killed shutdown's recovery leaves them.
	pinned, err := s.db.BeginTx(context.Background(), &sql.TxOptions{ReadOnly: true})
	require.NoError(t, err)
	defer func() { _ = pinned.Rollback() }()
	var n int
	require.NoError(t, pinned.QueryRow(`SELECT count(*) FROM wal_churn`).Scan(&n))
	growWAL(t, s, 60)
	require.NoError(t, pinned.Rollback())

	snap, ok := readWALIndexSnapshot(path)
	require.True(t, ok)
	pending := snap.PendingFrames()
	budget, estPending := s.CloseCheckpointEstimate()
	require.Equal(t, pending, estPending)
	walBefore := walFileSize(path + "-wal")
	start := time.Now()
	require.NoError(t, s.Close())
	elapsed := time.Since(start)
	walAfter := walFileSize(path + "-wal")
	_, statErr := os.Stat(path + "-wal")
	t.Logf("close: pending_frames=%d wal_before=%.1fMiB wal_after=%d removed=%t elapsed=%s budget=%s",
		pending, float64(walBefore)/(1<<20), walAfter, os.IsNotExist(statErr), elapsed, budget)
	require.Greater(t, pending, int64(10_000))
	require.Greater(t, walBefore, int64(walReclaimJournalLimit/2))
	require.Zero(t, walAfter, "Close must leave no log behind")
	require.Less(t, elapsed, budget)
}

func TestCloseCheckpointBudgetScales(t *testing.T) {
	require.Equal(t, closeCheckpointBaseBudget, closeCheckpointEstimate(-1))
	require.Equal(t, closeCheckpointBaseBudget, closeCheckpointEstimate(0))
	// The live incident's backlog: 9,045,293 frames.
	require.Equal(t, closeCheckpointBaseBudget+9_045_293*closeCheckpointPerFrame, closeCheckpointEstimate(9_045_293))
	require.Equal(t, closeCheckpointEstimateCap, closeCheckpointEstimate(50_000_000))
	require.Zero(t, closeCheckpointDeadline(9_045_293), "Close imposes no deadline unless asked")
	t.Setenv("GORTEX_SQLITE_CLOSE_CHECKPOINT_MAX_S", "30")
	require.Equal(t, 30*time.Second, closeCheckpointDeadline(9_045_293))
	require.Equal(t, closeCheckpointEstimate(10), closeCheckpointDeadline(10))
	t.Setenv("GORTEX_SQLITE_CLOSE_CHECKPOINT_MAX_S", "0")
	require.Zero(t, closeCheckpointDeadline(9_045_293))
}

// The gate's own contract: new work waits while closed, in-flight work is
// waited for, and a deadline reopens it with the in-flight count.
func TestSQLiteReadGateQuiesce(t *testing.T) {
	g := newSQLiteReadGate()
	require.NoError(t, g.enter(context.Background()))
	require.Equal(t, 1, g.activeCount())

	_, inFlight, err := g.quiesce(context.Background(), time.Now().Add(20*time.Millisecond))
	require.ErrorIs(t, err, errWALReclaimReadersInFlight)
	require.Equal(t, 1, inFlight)
	require.False(t, g.closed.Load())

	go func() {
		time.Sleep(10 * time.Millisecond)
		g.leave()
	}()
	reopen, _, err := g.quiesce(context.Background(), time.Now().Add(time.Second))
	require.NoError(t, err)
	entered := make(chan struct{})
	go func() {
		_ = g.enter(context.Background())
		close(entered)
	}()
	select {
	case <-entered:
		t.Fatal("a new reader passed a closed gate")
	case <-time.After(20 * time.Millisecond):
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Millisecond)
	defer cancel()
	require.ErrorIs(t, g.enter(ctx), context.DeadlineExceeded)
	reopen()
	<-entered
	require.Equal(t, 1, g.activeCount())
	g.leave()
	require.Zero(t, g.activeCount())
}

func TestSQLiteReadGateKillSwitch(t *testing.T) {
	t.Setenv("GORTEX_SQLITE_READ_GATE", "off")
	s, _ := openWALReclaimStore(t)
	defer func() { _ = s.Close() }()
	require.Nil(t, s.readGate)
	seedWALChurnTable(t, s)
	var n int
	require.NoError(t, s.db.QueryRow(`SELECT count(*) FROM wal_churn`).Scan(&n))
	require.Equal(t, 4000, n)

	t.Setenv("GORTEX_SQLITE_READ_GATE", "")
	gated, _ := openWALReclaimStore(t)
	defer func() { _ = gated.Close() }()
	require.NotNil(t, gated.readGate)
	conn, err := gated.db.Conn(context.Background())
	require.NoError(t, err)
	require.NoError(t, conn.Raw(func(dc any) error {
		if _, ok := dc.(*gatedConn); !ok {
			return fmt.Errorf("read pool connection is %T, want the gated wrapper", dc)
		}
		return nil
	}))
	require.NoError(t, conn.Close())
}
