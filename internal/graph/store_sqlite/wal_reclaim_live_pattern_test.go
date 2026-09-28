package store_sqlite

import (
	"bytes"
	"context"
	"database/sql"
	"log"
	"math/rand"
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

// The live daemon's pattern: pool reads that each hold their read
// transaction for 2.5–4 s (longer than the former 2 s open-gate wait and far
// longer than the 250 ms closed-gate drain), always overlapping, and writes
// that arrive in bursts (an edit's build) with idle writer time in between.
// The reclaim must reset the log from the open-gate stage, log one line per
// reset naming the stage, and never keep a queued write waiting longer than
// the minimum hold.
func TestWALReclaimResetsUnderLongOverlappingReadersAndBurstyWrites(t *testing.T) {
	t.Setenv("GORTEX_SQLITE_WAL_RECLAIM_MB", "16")
	// A production-like cadence (the daemon polls every 5 s): a 50 ms poll
	// would retry a yielded attempt straight back into the same write burst
	// and hold the writer for the minimum hold again at every write.
	setWALReclaimCadence(t, time.Second, time.Second, 4*time.Second)
	logs := captureReclaimLog(t)
	s, path := openWALReclaimStore(t)
	defer func() { _ = s.Close() }()
	seedWALChurnTable(t, s)

	stop := make(chan struct{})
	var wg sync.WaitGroup
	var maxWAL, maxWriteWait atomic.Int64
	var failed atomic.Value
	for r := 0; r < 4; r++ {
		wg.Add(1)
		go func(seed int64) {
			defer wg.Done()
			rng := rand.New(rand.NewSource(seed))
			// Stagger so some reader is always mid-transaction.
			time.Sleep(time.Duration(seed) * 700 * time.Millisecond)
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
				if err := tx.QueryRow(`SELECT count(*) FROM wal_churn WHERE id % 97 = 0`).Scan(&n); err != nil {
					_ = tx.Rollback()
					failed.Store(err)
					return
				}
				hold := 2500*time.Millisecond + time.Duration(rng.Int63n(int64(1500*time.Millisecond)))
				select {
				case <-stop:
				case <-time.After(hold):
				}
				_ = tx.Rollback()
			}
		}(int64(r))
	}
	wg.Add(1)
	go func() { // bursty writer: ~12 MiB per burst, then idle
		defer wg.Done()
		k := 0
		for {
			for i := 0; i < 12; i++ {
				start := time.Now()
				s.writeMu.Lock()
				wait := time.Since(start)
				s.writeMu.Unlock()
				if int64(wait) > maxWriteWait.Load() {
					maxWriteWait.Store(int64(wait))
				}
				if err := churnWriteOnce(s, k); err != nil {
					failed.Store(err)
					return
				}
				k++
			}
			select {
			case <-stop:
				return
			case <-time.After(6 * time.Second):
			}
		}
	}()
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
	// Run until the open-gate stage has reset the log twice, or a generous
	// deadline: the count is wall-clock sensitive on a loaded host (an attempt
	// that yields to a queued write is a skip and retries later), the
	// mechanism is not.
	deadline := time.Now().Add(120 * time.Second)
	for s.WALReclaimStats().OpenGateResets < 2 && time.Now().Before(deadline) {
		time.Sleep(200 * time.Millisecond)
	}
	close(stop)
	wg.Wait()
	if err, _ := failed.Load().(error); err != nil {
		t.Fatal(err)
	}
	stats := s.WALReclaimStats()
	out := logs.String()
	t.Logf("resets=%d open_gate=%d deferrals=%d skips=%d pause_max=%s writer_hold_max=%s wal_max=%.1fMiB max_write_wait=%s",
		stats.Resets, stats.OpenGateResets, stats.Deferrals, stats.Skips, stats.PauseMax, stats.WriterHoldMax,
		float64(maxWAL.Load())/(1<<20), time.Duration(maxWriteWait.Load()))
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "wal reclaim") {
			t.Log(line)
		}
	}
	require.GreaterOrEqual(t, stats.OpenGateResets, int64(1), "the open-gate stage must reset the log under long overlapping readers")
	if !raceDetectorOn {
		require.Less(t, stats.Deferrals, stats.OpenGateResets, "attempts must mostly reset, not defer")
	}
	require.Contains(t, out, "wal reclaimed stage=open_gate")
	require.Zero(t, stats.PauseMax, "no reader may be paused: the resets come from the open-gate stage")
	// Attempts start above the threshold; one that yields to a queued write
	// lets that burst (12 MiB) and possibly the next land before a retry
	// resets, so the log stays below threshold + two bursts.
	walBound := int64(3 * 16 << 20)
	if raceDetectorOn {
		walBound = 5 * 16 << 20
	}
	require.LessOrEqual(t, maxWAL.Load(), walBound, "the WAL must stay within three times the threshold")
	require.LessOrEqual(t, time.Duration(maxWriteWait.Load()), walReclaimMaxWriterHold+raceSlack(100*time.Millisecond),
		"a queued write waited on the reclaim longer than its minimum hold")
}

// A mutation arriving while a reclaim attempt runs is delayed at most
// walReclaimMaxWriterHold, even when an old reader never leaves: the wait for
// old readers holds no writer, and the writer is held at most 2 s. Every write
// succeeds — the store never turns the reclaim into a refused or empty write.
func TestWALReclaimDelaysAMutationAtMostTheWriterHoldCap(t *testing.T) {
	for _, tc := range []struct {
		name      string
		threshold int64
		urgent    bool
	}{
		// Not urgent: the old-reader wait runs writer-free and the lane
		// yields to the mutation stream at once.
		{name: "not_urgent", threshold: 1 << 30},
		// Urgent: the lane keeps the writer despite waiting mutations, but
		// never past the cap.
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
			cfg := walReclaimConfig{thresholdBytes: tc.threshold, drainDeadline: 250 * time.Millisecond, truncateBudget: walReclaimTruncateBudget, readerWait: 3 * time.Second}

			stop := make(chan struct{})
			var maxWait atomic.Int64
			var writes atomic.Int64
			var writeErr atomic.Value
			var wg sync.WaitGroup
			wg.Add(1)
			go func() { // a mutation stream
				defer wg.Done()
				for k := 0; ; k++ {
					select {
					case <-stop:
						return
					default:
					}
					start := time.Now()
					if err := churnWriteOnce(s, k); err != nil {
						writeErr.Store(err)
						return
					}
					if d := int64(time.Since(start)); d > maxWait.Load() {
						maxWait.Store(d)
					}
					writes.Add(1)
					time.Sleep(50 * time.Millisecond)
				}
			}()
			res := s.reclaimWALOnce(cfg, ckpt, path+"-wal")
			close(stop)
			wg.Wait()
			if err, _ := writeErr.Load().(error); err != nil {
				t.Fatalf("a mutation failed during the reclaim: %v", err)
			}
			t.Logf("outcome=%s reason=%q open_gate=%q writer_hold=%s max_mutation=%s writes=%d", res.outcome, res.reason, res.openGateReport, res.writerHold, time.Duration(maxWait.Load()), writes.Load())
			require.LessOrEqual(t, res.writerHold, walReclaimMaxWriterHold+raceSlack(100*time.Millisecond), "the writer was held past the cap")
			require.LessOrEqual(t, time.Duration(maxWait.Load()), walReclaimMaxWriterHold+raceSlack(100*time.Millisecond), "a mutation was delayed past the cap")
			if !tc.urgent {
				require.Greater(t, writes.Load(), int64(10), "mutations kept flowing while the reclaim waited for the old reader")
				require.Contains(t, res.openGateReport, "older_readers_outlasted_wait")
				require.Less(t, time.Duration(maxWait.Load()), raceSlack(500*time.Millisecond), "a non-urgent attempt must not hold a mutation")
			}
		})
	}
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
	require.Less(t, time.Since(announced), 100*time.Millisecond)
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
	require.Less(t, time.Since(start), time.Second)
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
