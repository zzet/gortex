package store_sqlite

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"log"
	"math/rand"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// captureReclaimLog redirects the standard logger for one case.
func captureReclaimLog(t *testing.T) *syncBuffer {
	t.Helper()
	buf := &syncBuffer{}
	prev := log.Writer()
	log.SetOutput(buf)
	t.Cleanup(func() {
		log.SetOutput(prev)
		// The reclaim's attempt lines, shown with -v and on failure.
		n := 0
		for _, line := range strings.Split(buf.String(), "\n") {
			if strings.Contains(line, "wal reclaim") && n < 200 {
				t.Log(line)
				n++
			}
		}
	})
	return buf
}

// shortenWALReclaimLaneReaderWait bounds the in-lane wait for old readers
// (walReclaimLaneReaderWait, 5 s) for a case whose reader never leaves: the
// attempt ends the same way, only sooner. The case keeps the reclaim loop off
// (GORTEX_SQLITE_WAL_RECLAIM_MB=0), so only its own attempts read the wait.
func shortenWALReclaimLaneReaderWait(t *testing.T, d time.Duration) {
	t.Helper()
	prev := walReclaimLaneReaderWait
	walReclaimLaneReaderWait = d
	t.Cleanup(func() { walReclaimLaneReaderWait = prev })
}

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// longReaderPattern is the live daemon's pattern: pool reads that each hold
// their read transaction for readMin..readMax, staggered so one is always in
// flight, and writes in bursts (an edit's build) separated by gaps: a short
// gap (shorter than any read, so no reset can land in it) and a long gap
// (more than twice the longest read, so one can).
type longReaderPattern struct {
	readers          int
	readMin, readMax time.Duration
	burstWrites      int // churn writes of ~1 MiB each
	shortGap         time.Duration
	longGap          time.Duration
	cycles           int
}

type longReaderGap struct {
	start, end time.Time
	long       bool
	walAtStart int64
}

type longReaderRun struct {
	gaps          []longReaderGap
	resets        []time.Time
	maxWAL        int64
	maxGateWait   time.Duration
	maxStatement  time.Duration
	maxBurstBytes int64
	started       time.Time
}

// firstResetAfter is the first reset at or after t (zero when none).
func (r longReaderRun) firstResetAfter(t time.Time) time.Time {
	for _, at := range r.resets {
		if !at.Before(t) {
			return at
		}
	}
	return time.Time{}
}

func runLongReaderPattern(t *testing.T, s *Store, path string, p longReaderPattern) longReaderRun {
	t.Helper()
	run := longReaderRun{started: time.Now()}
	stop := make(chan struct{})
	var wg sync.WaitGroup
	var failed atomic.Value
	var mu sync.Mutex
	for r := 0; r < p.readers; r++ {
		wg.Add(1)
		go func(seed int64) {
			defer wg.Done()
			rng := rand.New(rand.NewSource(seed))
			// Stagger so some reader is always mid-transaction.
			time.Sleep(time.Duration(seed) * p.readMin / time.Duration(p.readers))
			for {
				select {
				case <-stop:
					return
				default:
				}
				tx, err := s.db.BeginTx(context.Background(), &sql.TxOptions{ReadOnly: true})
				if err != nil {
					failed.Store(err)
					return
				}
				var n int
				queryErr := tx.QueryRow(`SELECT count(*) FROM wal_churn WHERE id % 97 = 0`).Scan(&n)
				if err := queryErr; err != nil {
					_ = tx.Rollback()
					failed.Store(err)
					return
				}
				hold := p.readMin + time.Duration(rng.Int63n(int64(p.readMax-p.readMin)))
				select {
				case <-stop:
				case <-time.After(hold):
				}
				_ = tx.Rollback()
			}
		}(int64(r))
	}
	wg.Add(1)
	go func() { // the log's size and the resets, sampled
		defer wg.Done()
		resets := s.WALReclaimStats().Resets
		for {
			select {
			case <-stop:
				return
			case <-time.After(5 * time.Millisecond):
			}
			size := walFileSize(path + "-wal")
			now := s.WALReclaimStats().Resets
			mu.Lock()
			run.maxWAL = max(run.maxWAL, size)
			for ; resets < now; resets++ {
				run.resets = append(run.resets, time.Now())
			}
			mu.Unlock()
		}
	}()
	// The writer: bursts and gaps, every write's wait for the gate and its
	// own statement timed apart.
	k := 0
	burst := func() {
		before := vfsIOMark()
		for i := 0; i < p.burstWrites; i++ {
			start := time.Now()
			s.writeMu.Lock()
			wait := time.Since(start)
			s.writeMu.Unlock()
			stmt := time.Now()
			if err := churnWriteOnce(s, k); err != nil {
				failed.Store(err)
				return
			}
			k++
			mu.Lock()
			run.maxGateWait = max(run.maxGateWait, wait)
			run.maxStatement = max(run.maxStatement, time.Since(stmt))
			mu.Unlock()
		}
		written := vfsIOMark().Since(before).WriterWALWriteBytes
		mu.Lock()
		run.maxBurstBytes = max(run.maxBurstBytes, written)
		mu.Unlock()
	}
	gap := func(d time.Duration, long bool) {
		g := longReaderGap{start: time.Now(), long: long, walAtStart: walFileSize(path + "-wal")}
		time.Sleep(d)
		g.end = time.Now()
		mu.Lock()
		run.gaps = append(run.gaps, g)
		mu.Unlock()
	}
	for c := 0; c < p.cycles; c++ {
		burst()
		gap(p.shortGap, false)
		burst()
		gap(p.longGap, true)
	}
	close(stop)
	wg.Wait()
	if err, _ := failed.Load().(error); err != nil {
		t.Fatal(err)
	}
	return run
}

// The twin of the case above with the daemon's defaults: the default
// threshold (256 MiB), cadence and reader wait, no scaling. The bursts are
// scaled to the threshold (~100 MiB each) and the log reaches ~0.5 GiB, so it
// is gated (GORTEX_STORE_DEFAULTS_READERS=1). It reports the log's peak, the
// longest wait for the gate and the time to the first reset.
func TestWALReclaimResetsUnderLongOverlappingReadersWithTheDaemonDefaults(t *testing.T) {
	if os.Getenv("GORTEX_STORE_DEFAULTS_READERS") != "1" {
		t.Skip("set GORTEX_STORE_DEFAULTS_READERS=1 (writes a ~0.5 GiB log)")
	}
	logs := captureReclaimLog(t)
	s, path := openWALReclaimStore(t)
	defer func() { _ = s.Close() }()
	seedWALChurnTable(t, s)
	prepareSteadyStateLongReaderStore(t, s)
	cfg := resolveWALReclaimConfig()
	p := longReaderPattern{
		readers: 4, readMin: 2500 * time.Millisecond, readMax: 4 * time.Second,
		burstWrites: 100, shortGap: 1500 * time.Millisecond, longGap: 15 * time.Second, cycles: 3,
	}
	run := runLongReaderPattern(t, s, path, p)
	checkLongReaderContract(t, s, run, cfg.thresholdBytes, logs.String())
}

func prepareSteadyStateLongReaderStore(t *testing.T, s *Store) {
	t.Helper()
	// Measure steady-state reclamation after the real one-time maintenance.
	// Otherwise its metadata writes can add WAL frames inside a writer gap.
	setupCtx, cancelSetup := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancelSetup()
	if err := s.EnsureRowCounters(setupCtx); err != nil {
		t.Fatalf("initialize row counters before steady-state workload: %v", err)
	}
	if err := s.buildLazyIndexOnce(setupCtx); err != nil {
		t.Fatalf("build lazy index before steady-state workload: %v", err)
	}
	installed, err := s.rowCountersInstalled(setupCtx)
	if err != nil || !installed || !s.rowCountersReady.Load() || !s.fileGenerationIndexPresent() {
		t.Fatalf("steady-state maintenance readiness: counters_installed=%t counters_ready=%t index_present=%t error=%v",
			installed, s.rowCountersReady.Load(), s.fileGenerationIndexPresent(), err)
	}
	t.Log("steady-state maintenance ready: row counters installed and seeded; lazy file-generation index present; background workers remain enabled")
}

func checkLongReaderContract(t *testing.T, s *Store, run longReaderRun, threshold int64, logs string) {
	t.Helper()
	stats := s.WALReclaimStats()
	bound := threshold + 2*run.maxBurstBytes
	firstReset := time.Duration(0)
	if len(run.resets) > 0 {
		firstReset = run.resets[0].Sub(run.started)
	}
	t.Logf("resets=%d deferrals=%d skips=%d pause_max=%s writer_hold_max=%s wal_max=%.1fMiB bound=%.1fMiB burst=%.1fMiB max_gate_wait=%s max_statement=%s first_reset_after=%s",
		stats.Resets, stats.Deferrals, stats.Skips, stats.PauseMax, stats.WriterHoldMax,
		float64(run.maxWAL)/(1<<20), float64(bound)/(1<<20), float64(run.maxBurstBytes)/(1<<20),
		run.maxGateWait, run.maxStatement, firstReset.Round(time.Millisecond))
	longGaps := 0
	for i, g := range run.gaps {
		at := run.firstResetAfter(g.start)
		inGap := !at.IsZero() && at.Before(g.end)
		t.Logf("gap %d long=%v wal_at_start=%.1fMiB length=%s reset_in_gap=%v", i, g.long, float64(g.walAtStart)/(1<<20), g.end.Sub(g.start).Round(time.Millisecond), inGap)
		if g.long && g.walAtStart > threshold {
			longGaps++
			require.True(t, inGap, "gap %d: a long gap over the threshold ended without a reset", i)
		}
	}
	require.Positive(t, longGaps, "no long gap started over the threshold: the pattern did not exercise the reset")
	require.LessOrEqual(t, run.maxGateWait, walReclaimResetHold+testTimingSlack(10*time.Millisecond),
		"a write waited for the gate longer than one short hold")
	require.LessOrEqual(t, stats.WriterHoldMax, walReclaimResetHold+testTimingSlack(10*time.Millisecond),
		"the reclaim held the writer longer than one short hold")
	require.LessOrEqual(t, run.maxWAL, bound, "the log outgrew the threshold plus two bursts")
	require.Zero(t, stats.PauseMax, "no reader may be paused")
	require.Contains(t, logs, "wal reclaimed stage=open_gate")
}

// Reclaim keeps the writer free while its pinned-reader wait is parked.
// Both urgency classes admit and commit mutations before the reader retires.
// The default TestAReclaimHoldHandsTheWriterBackWhenTheLogIsNotReady checks
// writer-hold and gate-wait caps. checkLongReaderContract adds opt-in coverage.
func TestWALReclaimKeepsWriterFreeDuringParkedReaderWait(t *testing.T) {
	for _, tc := range []struct {
		name      string
		threshold int64
		urgent    bool
	}{
		// The ordinary threshold leaves the attempt non-urgent.
		{name: "not_urgent", threshold: 1 << 30},
		// Urgency changes reset admission, not the writer-free reader wait.
		{name: "urgent", threshold: 1 << 20, urgent: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("GORTEX_SQLITE_WAL_RECLAIM_MB", "0")
			s, path := openWALReclaimStore(t)
			defer func() { _ = s.Close() }()
			seedWALChurnTable(t, s)
			growWAL(t, s, 4)
			pinned, err := s.db.BeginTx(context.Background(), &sql.TxOptions{ReadOnly: true})
			require.NoError(t, err)
			defer func() { _ = pinned.Rollback() }()
			var n int
			require.NoError(t, pinned.QueryRow(`SELECT count(*) FROM wal_churn`).Scan(&n))
			growWAL(t, s, 4)

			ckpt, err := openWALReclaimCheckpointDB(s.dbPath)
			require.NoError(t, err)
			defer ckpt.Close()
			cfg := walReclaimConfig{thresholdBytes: tc.threshold, drainDeadline: 250 * time.Millisecond, truncateBudget: walReclaimTruncateBudget, readerWait: time.Second}
			shortenWALReclaimLaneReaderWait(t, 2*time.Second)
			type waitingRound struct {
				epoch uint64
				older int
			}
			waiting := make(chan waitingRound, 1)
			continueWait := make(chan struct{})
			var releaseOnce, observeOnce sync.Once
			releaseWait := func() { releaseOnce.Do(func() { close(continueWait) }) }
			previous := walReclaimRoundObserver
			walReclaimRoundObserver = func(stage string, epoch uint64, older int, err error) {
				if stage == "full_wait_start" || stage == "partial_wait_start" {
					observeOnce.Do(func() {
						waiting <- waitingRound{epoch: epoch, older: older}
						<-continueWait
					})
				}
				if previous != nil {
					previous(stage, epoch, older, err)
				}
			}
			done := make(chan walReclaimResult, 1)
			joined := false
			go func() { done <- s.reclaimWALOnce(cfg, ckpt, path+"-wal") }()
			defer func() {
				_ = pinned.Rollback()
				releaseWait()
				if !joined {
					select {
					case <-done:
					case <-time.After(8 * time.Second):
						t.Error("reclaim did not stop after its reader and observer were released")
					}
				}
				walReclaimRoundObserver = previous
			}()
			var round waitingRound
			select {
			case round = <-waiting:
			case <-done:
				joined = true
				t.Fatal("reclaim returned without reaching its pinned-reader wait")
			case <-time.After(8 * time.Second):
				t.Fatal("reclaim never reached its pinned-reader wait")
			}
			require.Positive(t, round.older, "the wait must retain the pinned SQLite reader")
			// Commit a finite mutation stream while that wait remains parked.
			// This cannot pass if reclaim carries the writer into its reader wait.
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			for k := 0; k < 12; k++ {
				release, err := s.HoldWriteGate(ctx)
				require.NoError(t, err, "mutation queued behind a reader wait")
				_, err = s.writerDB.ExecContext(ctx, `UPDATE wal_churn SET payload = ? WHERE id = 1`, fmt.Sprintf("foreground-%d", k))
				release()
				require.NoError(t, err)
			}
			require.Positive(t, s.readGate.olderCount(round.epoch), "mutations must commit before the pinned reader retires")
			var payload string
			require.NoError(t, s.db.QueryRowContext(ctx, `SELECT payload FROM wal_churn WHERE id = 1`).Scan(&payload))
			require.Equal(t, "foreground-11", payload)
			// Retire the pin rather than exhaust the observation wait.
			require.NoError(t, pinned.Rollback())
			releaseWait()
			select {
			case res := <-done:
				joined = true
				require.Equal(t, tc.urgent, res.urgent)
				require.Equal(t, walReclaimReset, res.outcome)
				require.True(t, walLogIsReset(s.dbPath, path+"-wal"))
			case <-time.After(8 * time.Second):
				t.Fatal("reclaim did not finish after the reader retired")
			}
		})
	}
}

// A non-urgent attempt whose pinned reader outlasts both reader waits gives
// up without a reset and reports why.
func TestWALReclaimReportsAReaderThatOutlastsTheWait(t *testing.T) {
	t.Setenv("GORTEX_SQLITE_WAL_RECLAIM_MB", "0")
	s, path := openWALReclaimStore(t)
	defer func() { _ = s.Close() }()
	seedWALChurnTable(t, s)
	growWAL(t, s, 4)
	pinned, err := s.db.BeginTx(context.Background(), &sql.TxOptions{ReadOnly: true})
	require.NoError(t, err)
	defer func() { _ = pinned.Rollback() }()
	var n int
	require.NoError(t, pinned.QueryRow(`SELECT count(*) FROM wal_churn`).Scan(&n))
	growWAL(t, s, 4)

	ckpt, err := openWALReclaimCheckpointDB(s.dbPath)
	require.NoError(t, err)
	defer ckpt.Close()
	cfg := walReclaimConfig{thresholdBytes: 1 << 30, drainDeadline: 250 * time.Millisecond, truncateBudget: walReclaimTruncateBudget, readerWait: 100 * time.Millisecond}
	shortenWALReclaimLaneReaderWait(t, 100*time.Millisecond)

	res := s.reclaimWALOnce(cfg, ckpt, path+"-wal")
	require.False(t, res.urgent)
	require.NotEqual(t, walReclaimReset, res.outcome, "the pinned reader keeps the log from a reset")
	require.Contains(t, res.openGateReport, "older_readers_outlasted_wait")
}

// An announced mutation (AnnounceWrite, set before it waits on any lock of
// its own) makes the reclaim leave the writer alone: an attempt that finds one
// never takes the writer, and a hold in progress ends within the yield poll.
func TestWALReclaimYieldsToAnAnnouncedMutation(t *testing.T) {
	t.Setenv("GORTEX_SQLITE_WAL_RECLAIM_MB", "0")
	s, path := openWALReclaimStore(t)
	defer func() { _ = s.Close() }()
	seedWALChurnTable(t, s)
	growWAL(t, s, 4)
	ckpt, err := openWALReclaimCheckpointDB(s.dbPath)
	require.NoError(t, err)
	defer ckpt.Close()
	// A threshold far above the log: not urgent, so the attempt yields.
	cfg := walReclaimConfig{thresholdBytes: 1 << 30, drainDeadline: 250 * time.Millisecond, truncateBudget: walReclaimTruncateBudget, readerWait: time.Second}

	release := s.AnnounceWrite()
	res := s.reclaimWALOnce(cfg, ckpt, path+"-wal")
	require.Equal(t, walReclaimSkipped, res.outcome)
	require.Equal(t, "writer_waiting", res.reason)
	require.Zero(t, res.writerHold, "the attempt took the writer despite an announced mutation")
	release()
	release() // idempotent

	ctx, stopYield := s.yieldToWriters(context.Background())
	defer stopYield()
	announced := time.Now()
	release = s.AnnounceWrite()
	defer release()
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("the hold did not yield to an announced mutation")
	}
	require.ErrorIs(t, context.Cause(ctx), errWALReclaimWriterWaiting)
	require.Less(t, time.Since(announced), testTimingSlack(100*time.Millisecond))
}

// With nothing in the log and nothing to shrink, an attempt never takes the
// writer (the live `backfilled=0/0 writer_hold=31s` case).
func TestWALReclaimSkipsWhenThereIsNothingToReclaim(t *testing.T) {
	t.Setenv("GORTEX_SQLITE_WAL_RECLAIM_MB", "0")
	s, path := openWALReclaimStore(t)
	defer func() { _ = s.Close() }()
	seedWALChurnTable(t, s)
	ckpt, err := openWALReclaimCheckpointDB(s.dbPath)
	require.NoError(t, err)
	defer ckpt.Close()
	_, err = checkpointWALOnceOn(context.Background(), ckpt, "TRUNCATE")
	require.NoError(t, err)
	cfg := walReclaimConfig{thresholdBytes: 1 << 30, drainDeadline: 250 * time.Millisecond, truncateBudget: walReclaimTruncateBudget, readerWait: 20 * time.Second}
	start := time.Now()
	res := s.reclaimWALOnce(cfg, ckpt, path+"-wal")
	t.Logf("outcome=%s reason=%q writer_hold=%s elapsed=%s", res.outcome, res.reason, res.writerHold, time.Since(start))
	require.Equal(t, walReclaimSkipped, res.outcome)
	require.Equal(t, "nothing_to_reclaim", res.reason)
	require.Zero(t, res.writerHold)
	require.Less(t, time.Since(start), testTimingSlack(time.Second))
}

// A residue drain that finds a reader holding the log gives up on its first
// busy TRUNCATE, holds the writer for at most its TRUNCATE budget, and hands
// the residue to the reclaim (nudge), instead of spinning seconds per attempt
// with every write queued behind it.
func TestResidueDrainHandsABusyTruncateToTheReclaim(t *testing.T) {
	t.Setenv("GORTEX_SQLITE_WAL_RECLAIM_MB", "0")
	logs := captureReclaimLog(t)
	s, _ := openWALReclaimStore(t)
	defer func() { _ = s.Close() }()
	seedWALChurnTable(t, s)
	growWAL(t, s, 4)
	pinned, err := s.db.BeginTx(context.Background(), &sql.TxOptions{ReadOnly: true})
	require.NoError(t, err)
	defer func() { _ = pinned.Rollback() }()
	var n int
	require.NoError(t, pinned.QueryRow(`SELECT count(*) FROM wal_churn`).Scan(&n))
	growWAL(t, s, 4)

	var maxWait atomic.Int64
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { // an edit stream measuring its wait for the writer
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			start := time.Now()
			s.writeMu.Lock()
			if d := int64(time.Since(start)); d > maxWait.Load() {
				maxWait.Store(d)
			}
			s.writeMu.Unlock()
			time.Sleep(5 * time.Millisecond)
		}
	}()
	started := time.Now()
	s.scheduleWALDrain("test")
	waitForCondition(t, "drain finished", func() bool {
		s.maintenanceSched.Lock()
		defer s.maintenanceSched.Unlock()
		return !s.maintenanceDrainRunning && !s.maintenanceDrainOwed
	})
	elapsed := time.Since(started)
	close(stop)
	wg.Wait()
	t.Logf("drain elapsed=%s handoffs=%d drains=%d max_write_wait=%s nudged=%v", elapsed, s.walDrainHandoffs.Load(), s.walDrains.Load(), time.Duration(maxWait.Load()), s.walReclaimNudged.Load())
	require.Equal(t, int64(1), s.walDrainHandoffs.Load(), "the busy TRUNCATE must be handed to the reclaim")
	require.Zero(t, s.walDrains.Load())
	require.True(t, s.walReclaimNudged.Load(), "the reclaim must be nudged")
	require.Less(t, time.Duration(maxWait.Load()), walResidueTruncateBudget+200*time.Millisecond, "a write waited on the drain longer than its TRUNCATE budget")
	require.Less(t, elapsed, 5*time.Second, "the drain spun")
	require.NotContains(t, logs.String(), "busy exhausted")
	require.Contains(t, logs.String(), "wal residue drain handed to reclaim")
}

// A hold that finds the log not ready hands the writer back at once: here a
// reader pins the last frames (it began just before a small write), so the
// hold's backfill stays incomplete although the remainder is small. The
// reclaim retakes the writer only for another short hold after the readers
// it waited for have left; a write never waits for the gate longer than one
// hold, and no hold lasts longer than walReclaimResetHold.
func TestAReclaimHoldHandsTheWriterBackWhenTheLogIsNotReady(t *testing.T) {
	t.Setenv("GORTEX_SQLITE_WAL_RECLAIM_MB", "0")
	s, path := openWALReclaimStore(t)
	defer func() { _ = s.Close() }()
	seedWALChurnTable(t, s)
	growWAL(t, s, 8)
	ckpt, err := openWALReclaimCheckpointDB(s.dbPath)
	require.NoError(t, err)
	defer ckpt.Close()
	_, err = checkpointWALOnceOn(context.Background(), ckpt, "PASSIVE")
	require.NoError(t, err)
	pinned, err := s.db.BeginTx(context.Background(), &sql.TxOptions{ReadOnly: true})
	require.NoError(t, err)
	defer func() { _ = pinned.Rollback() }()
	var n int
	require.NoError(t, pinned.QueryRow(`SELECT count(*) FROM wal_churn`).Scan(&n))
	smallWrite := func(k int) error {
		s.writeMu.Lock()
		defer s.writeMu.Unlock()
		_, err := s.writerDB.Exec(`UPDATE wal_churn SET payload = randomblob(1024) WHERE id = ?`, 1+k%4000)
		return err
	}
	require.NoError(t, smallWrite(0))
	snap, ok := readWALIndexSnapshot(path)
	require.True(t, ok)
	require.LessOrEqual(t, snap.MxFrame-snap.NBackfill, walReclaimPressureSmallFrames, "precondition: a remainder one hold may copy")

	stop := make(chan struct{})
	var maxWait atomic.Int64
	var writes atomic.Int64
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { // a stream of small writes, the gate wait timed
		defer wg.Done()
		for k := 1; ; k++ {
			select {
			case <-stop:
				return
			case <-time.After(20 * time.Millisecond):
			}
			start := time.Now()
			s.writeMu.Lock()
			wait := time.Since(start)
			s.writeMu.Unlock()
			if int64(wait) > maxWait.Load() {
				maxWait.Store(int64(wait))
			}
			_ = smallWrite(k)
			writes.Add(1)
		}
	}()
	// Urgent (no yield to queued writes) and a short writer-free wait, so
	// the attempt reaches its in-lane holds while the reader still pins.
	cfg := walReclaimConfig{thresholdBytes: 1, drainDeadline: 250 * time.Millisecond, truncateBudget: walReclaimTruncateBudget, readerWait: 200 * time.Millisecond}
	// The in-lane holds come first; the lane's wait for the pinned reader then
	// runs out, and 1.5 s of it keeps well over twenty paced writes in flight.
	shortenWALReclaimLaneReaderWait(t, 1500*time.Millisecond)
	res := s.reclaimWALOnce(cfg, ckpt, path+"-wal")
	close(stop)
	wg.Wait()
	t.Logf("outcome=%s reason=%q writer_holds=%d longest_hold=%s max_gate_wait=%s writes=%d", res.outcome, res.reason, res.writerHolds, res.writerHold, time.Duration(maxWait.Load()), writes.Load())
	require.NotEqual(t, walReclaimReset, res.outcome, "precondition: the pinned reader keeps the log from a reset")
	require.GreaterOrEqual(t, res.writerHolds, 1)
	require.LessOrEqual(t, res.writerHold, walReclaimResetHold+testTimingSlack(10*time.Millisecond), "a hold outlasted its cap")
	require.LessOrEqual(t, time.Duration(maxWait.Load()), walReclaimResetHold+testTimingSlack(10*time.Millisecond), "a write waited for the gate past one hold")
	require.Greater(t, writes.Load(), int64(20), "writes kept flowing while the reclaim waited")
}
