package store_sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
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

// walChurn drives the incident's workload: one continuous writer, a rotating
// population of pool readers that each hold a read transaction for
// holdMin..holdMax, and the pressure loop's PASSIVE backfill every 20 ms.
type walChurn struct {
	s        *Store
	stop     chan struct{}
	wg       sync.WaitGroup
	writes   atomic.Int64
	errMu    sync.Mutex
	err      error
	latMu    sync.Mutex
	readLats []time.Duration
	maxWAL   atomic.Int64
	resets   atomic.Int64 // observed log resets (mxFrame moved backwards)
	caughtUp atomic.Int64 // samples with nBackfill == mxFrame > 0
}

func (c *walChurn) fail(err error) {
	c.errMu.Lock()
	if c.err == nil {
		c.err = err
	}
	c.errMu.Unlock()
}

func startWALChurn(t *testing.T, s *Store, path string, readers int, holdMin, holdMax time.Duration) *walChurn {
	t.Helper()
	c := &walChurn{s: s, stop: make(chan struct{})}
	stopped := func() bool {
		select {
		case <-c.stop:
			return true
		default:
			return false
		}
	}
	c.wg.Add(1)
	go func() { // writer
		defer c.wg.Done()
		for k := 0; !stopped(); k++ {
			if err := churnWriteOnce(s, k); err != nil {
				c.fail(fmt.Errorf("writer: %w", err))
				return
			}
			c.writes.Add(1)
		}
	}()
	for r := 0; r < readers; r++ {
		c.wg.Add(1)
		go func(seed int64) { // rotating reader
			defer c.wg.Done()
			rng := rand.New(rand.NewSource(seed))
			for !stopped() {
				start := time.Now()
				tx, err := s.db.BeginTx(context.Background(), &sql.TxOptions{ReadOnly: true})
				if err != nil {
					c.fail(fmt.Errorf("reader begin: %w", err))
					return
				}
				var n int
				if err := tx.QueryRow(`SELECT count(*) FROM wal_churn WHERE id % 97 = 0`).Scan(&n); err != nil {
					_ = tx.Rollback()
					c.fail(fmt.Errorf("reader query: %w", err))
					return
				}
				lat := time.Since(start)
				c.latMu.Lock()
				c.readLats = append(c.readLats, lat)
				c.latMu.Unlock()
				hold := holdMin + time.Duration(rng.Int63n(int64(holdMax-holdMin)+1))
				select {
				case <-c.stop:
				case <-time.After(hold):
				}
				_ = tx.Rollback()
			}
		}(int64(r + 1))
	}
	c.wg.Add(1)
	go func() { // the pressure loop's PASSIVE backfill, at a test cadence
		defer c.wg.Done()
		ckpt, err := openWALReclaimCheckpointDB(s.dbPath)
		if err != nil {
			c.fail(err)
			return
		}
		defer ckpt.Close()
		for !stopped() {
			s.runBackgroundCheckpointAttempt(func(ctx context.Context) (bool, bool) {
				_, _ = checkpointWALOnceOn(ctx, ckpt, "PASSIVE")
				return true, false
			})
			select {
			case <-c.stop:
			case <-time.After(20 * time.Millisecond):
			}
		}
	}()
	c.wg.Add(1)
	go func() { // sampler
		defer c.wg.Done()
		var lastMx uint32
		for !stopped() {
			if size := walFileSize(path + "-wal"); size > c.maxWAL.Load() {
				c.maxWAL.Store(size)
			}
			if snap, ok := readWALIndexSnapshot(path); ok {
				// A reset restarts the log at frame 1, so mxFrame going
				// backwards is the connection-independent reset signal (the
				// WAL header's sequence is written from the WRITER's own
				// counter and does not move when another connection resets).
				if snap.MxFrame < lastMx {
					c.resets.Add(1)
				}
				lastMx = snap.MxFrame
				if snap.MxFrame > 0 && snap.NBackfill == snap.MxFrame {
					c.caughtUp.Add(1)
				}
			}
			time.Sleep(5 * time.Millisecond)
		}
	}()
	return c
}

func (c *walChurn) halt(t *testing.T) {
	t.Helper()
	close(c.stop)
	c.wg.Wait()
	c.errMu.Lock()
	defer c.errMu.Unlock()
	require.NoError(t, c.err)
}

func (c *walChurn) latencies() (p50, p99, maxLat time.Duration, n int) {
	c.latMu.Lock()
	defer c.latMu.Unlock()
	lats := append([]time.Duration(nil), c.readLats...)
	if len(lats) == 0 {
		return 0, 0, 0, 0
	}
	sort.Slice(lats, func(i, j int) bool { return lats[i] < lats[j] })
	return lats[len(lats)/2], lats[len(lats)*99/100], lats[len(lats)-1], len(lats)
}

// churnUntil runs the churn until the writer has committed `writes`
// transactions (≈1 MiB of frames each) or the timeout passes.
func (c *walChurn) runUntil(t *testing.T, writes int64, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for c.writes.Load() < writes && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
}

// The reproduction: continuous writes, rotating readers and PASSIVE backfill.
// Backfill catches up, yet the log never resets, so the file grows past
// journal_size_limit and stays there.
func TestWALGrowsWithoutResetUnderReaderChurn(t *testing.T) {
	t.Setenv("GORTEX_SQLITE_WAL_RECLAIM_MB", "0")
	s, path := openWALReclaimStore(t)
	defer func() { _ = s.Close() }()
	seedWALChurnTable(t, s)

	churn := startWALChurn(t, s, path, 4, 150*time.Millisecond, 300*time.Millisecond)
	churn.runUntil(t, 110, 90*time.Second)
	churn.halt(t)

	final := walFileSize(path + "-wal")
	snap, _ := readWALIndexSnapshot(path)
	p50, p99, maxLat, n := churn.latencies()
	t.Logf("reproduction: writes=%d wal_final=%.1fMiB wal_max=%.1fMiB mxFrame=%d nBackfill=%d observed_resets=%d caught_up_samples=%d read_lat p50=%s p99=%s max=%s n=%d",
		churn.writes.Load(), float64(final)/(1<<20), float64(churn.maxWAL.Load())/(1<<20),
		snap.MxFrame, snap.NBackfill, churn.resets.Load(), churn.caughtUp.Load(), p50, p99, maxLat, n)
	require.Greater(t, snap.NBackfill, uint32(0), "PASSIVE backfill must have progressed")
	require.Greater(t, final, int64(walReclaimJournalLimit),
		"without a reset the WAL must outgrow journal_size_limit (it is applied only at a reset)")
	require.Zero(t, s.WALReclaimStats().Resets)
}

// The fix under the same churn: the reclaim keeps the log bounded, and the
// reader-visible pause stays inside drain deadline + TRUNCATE budget.
func TestWALReclaimBoundsWALUnderReaderChurn(t *testing.T) {
	t.Setenv("GORTEX_SQLITE_WAL_RECLAIM_MB", "16")
	setWALReclaimCadence(t, 50*time.Millisecond, 50*time.Millisecond, 400*time.Millisecond)
	s, path := openWALReclaimStore(t)
	defer func() { _ = s.Close() }()
	seedWALChurnTable(t, s)

	churn := startWALChurn(t, s, path, 4, 150*time.Millisecond, 300*time.Millisecond)
	churn.runUntil(t, 110, 90*time.Second)
	churn.halt(t)

	// Let the poller act on the residue the last writes left, then read.
	deadline := time.Now().Add(5 * time.Second)
	for walFileSize(path+"-wal") > walReclaimJournalLimit && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	stats := s.WALReclaimStats()
	final := walFileSize(path + "-wal")
	p50, p99, maxLat, n := churn.latencies()
	t.Logf("fix: writes=%d wal_final=%.1fMiB wal_max=%.1fMiB resets=%d deferrals=%d skips=%d attempts=%d frames=%d bytes=%.1fMiB pause_n=%d pause_max=%s pause_avg=%s reader_waits=%d reader_wait_max=%s read_lat p50=%s p99=%s max=%s n=%d observed_resets=%d",
		churn.writes.Load(), float64(final)/(1<<20), float64(churn.maxWAL.Load())/(1<<20),
		stats.Resets, stats.Deferrals, stats.Skips, stats.Attempts, stats.FramesReclaimed, float64(stats.BytesReclaimed)/(1<<20),
		stats.PauseCount, stats.PauseMax, avgPause(stats), stats.ReaderWaits, stats.ReaderWaitMax, p50, p99, maxLat, n, churn.resets.Load())

	require.GreaterOrEqual(t, stats.Resets, int64(2), "the reclaim must reset the log repeatedly under churn")
	require.LessOrEqual(t, final, int64(walReclaimJournalLimit), "after reclaim the WAL is back within journal_size_limit")
	require.LessOrEqual(t, churn.maxWAL.Load(), int64(walReclaimJournalLimit),
		"under the same churn the WAL never outgrows journal_size_limit")
	cfg := resolveWALReclaimConfig()
	bound := cfg.drainDeadline + cfg.truncateBudget + 100*time.Millisecond
	require.LessOrEqual(t, stats.PauseMax, bound, "gate closed longer than drain deadline + TRUNCATE budget")
	require.LessOrEqual(t, stats.ReaderWaitMax, bound, "a reader waited at the gate longer than the bound")
}

func avgPause(st WALReclaimStats) time.Duration {
	if st.PauseCount == 0 {
		return 0
	}
	return st.PauseTotal / time.Duration(st.PauseCount)
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
