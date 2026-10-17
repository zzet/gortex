//go:build performance

package store_sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"math/rand"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

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
	maxWALAt atomic.Int64 // existing peak sample timestamp; no extra WAL read
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
				c.maxWALAt.Store(time.Now().UnixNano())
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

// Continuous edits look like an announcement that is always active. The WAL
// hard cap (4x the ceiling) must still start the reclaim then, bounded by the
// writer-hold cap, or the log grows without end (22 GB and 57.6 GB were seen
// when background work could not run).
func TestPerformanceHardCapStartsTheReclaimUnderAPermanentAnnouncement(t *testing.T) {
	t.Setenv("GORTEX_SQLITE_WAL_RECLAIM_MB", "4")
	t.Setenv("GORTEX_SQLITE_WAL_RECLAIM_CEILING_MB", "8")
	prevFloor, prevSpacing, prevLeak := walReclaimCeilingFloor, walReclaimHardCapSpacing, editIntentStandDownMax
	walReclaimCeilingFloor, walReclaimHardCapSpacing, editIntentStandDownMax = 0, 200*time.Millisecond, time.Hour
	t.Cleanup(func() {
		walReclaimCeilingFloor, walReclaimHardCapSpacing, editIntentStandDownMax = prevFloor, prevSpacing, prevLeak
	})
	setWALReclaimCadence(t, 50*time.Millisecond, 50*time.Millisecond, 400*time.Millisecond)
	logs := captureReclaimLog(t)
	s, path := openWALReclaimStore(t)
	defer func() { _ = s.Close() }()
	seedWALChurnTable(t, s)
	lane := &fakeBuildLane{} // installed, never held: only the announcement
	lane.install(s)
	release := s.AnnounceWrite()
	defer release()
	if !s.buildLaneBusy() {
		t.Fatal("precondition: the announcement stands background work down")
	}
	churn := startWALChurn(t, s, path, 2, 100*time.Millisecond, 200*time.Millisecond)
	churn.runUntil(t, 140, 120*time.Second)
	churn.halt(t)
	const hardCap = 4 * (8 << 20)
	st := s.WALReclaimStats()
	t.Logf("permanent announcement: wal_max=%.1fMiB hard_cap_runs=%d resets=%d refusals=%d writer_hold_max=%s",
		float64(churn.maxWAL.Load())/(1<<20), st.CycleCeilingRuns, st.Resets, st.CycleRefusals, st.WriterHoldMax)
	if st.CycleCeilingRuns == 0 || st.Resets == 0 {
		t.Fatalf("the hard cap never started the reclaim under a permanent announcement: %+v", st)
	}
	if churn.maxWAL.Load() > 2*hardCap {
		t.Fatalf("the WAL reached %.1f MiB, past twice its hard cap", float64(churn.maxWAL.Load())/(1<<20))
	}
	if st.WriterHoldMax > walReclaimMaxWriterHold+raceSlack(250*time.Millisecond) {
		t.Fatalf("a hard-cap run held the writer %s", st.WriterHoldMax)
	}
	if !strings.Contains(logs.String(), "reason=wal_hard_cap") {
		t.Fatal("the hard-cap run was not logged")
	}
}

// The reproduction: continuous writes, rotating readers and PASSIVE backfill.
// Backfill catches up, yet the log never resets, so the file grows past
// journal_size_limit and stays there.
func TestPerformanceWALGrowsWithoutResetUnderReaderChurn(t *testing.T) {
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

// The fix under the same churn (a writer that never pauses): the log is
// bounded by the last resort (walReclaimLastResortBytes, here 4 × a 12 MiB
// ceiling) plus one attempt's growth, the reader-visible pause stays inside
// drain deadline + TRUNCATE budget, and once the writes stop the next attempt
// brings the log back within journal_size_limit with a short hold.
//
// It replaced a contract under which the reclaim held the writer up to 2 s
// at every urgent attempt and the log never outgrew journal_size_limit.
func TestPerformanceWALReclaimBoundsWALUnderReaderChurn(t *testing.T) {
	t.Setenv("GORTEX_SQLITE_WAL_RECLAIM_MB", "16")
	t.Setenv("GORTEX_SQLITE_WAL_RECLAIM_CEILING_MB", "12")
	prevFloor := walReclaimCeilingFloor
	walReclaimCeilingFloor = 0
	t.Cleanup(func() { walReclaimCeilingFloor = prevFloor })
	setWALReclaimCadence(t, 50*time.Millisecond, 50*time.Millisecond, 400*time.Millisecond)
	s, path := openWALReclaimStore(t)
	defer func() { _ = s.Close() }()
	seedWALChurnTable(t, s)

	churn := startWALChurn(t, s, path, 4, 150*time.Millisecond, 300*time.Millisecond)
	churn.runUntil(t, 240, 90*time.Second)
	churn.halt(t)

	// Physical truncation precedes completion-stat publication. Observe both
	// the required completed resets and the final WAL bound before sampling.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && (s.WALReclaimStats().Resets < 2 || walFileSize(path+"-wal") > walReclaimJournalLimit) {
		time.Sleep(20 * time.Millisecond)
	}
	stats := s.WALReclaimStats()
	final := walFileSize(path + "-wal")
	p50, p99, maxLat, n := churn.latencies()
	t.Logf("reader churn sampled peak at=%s wal_bytes=%d; existing5ms sampler, timestamp after size read, not continuous exact peak", time.Unix(0, churn.maxWALAt.Load()).UTC().Format(time.RFC3339Nano), churn.maxWAL.Load())
	t.Logf("fix: writes=%d wal_final=%.1fMiB wal_max=%.1fMiB resets=%d deferrals=%d skips=%d attempts=%d frames=%d bytes=%.1fMiB pause_n=%d pause_max=%s pause_avg=%s reader_waits=%d reader_wait_max=%s read_lat p50=%s p99=%s max=%s n=%d observed_resets=%d",
		churn.writes.Load(), float64(final)/(1<<20), float64(churn.maxWAL.Load())/(1<<20),
		stats.Resets, stats.Deferrals, stats.Skips, stats.Attempts, stats.FramesReclaimed, float64(stats.BytesReclaimed)/(1<<20),
		stats.PauseCount, stats.PauseMax, avgPause(stats), stats.ReaderWaits, stats.ReaderWaitMax, p50, p99, maxLat, n, churn.resets.Load())

	mark := int64(walReclaimLastResortFactor * (12 << 20))
	require.GreaterOrEqual(t, stats.Resets, int64(2), "the reclaim must reset the log repeatedly under churn")
	require.LessOrEqual(t, final, int64(walReclaimJournalLimit), "after reclaim the WAL is back within journal_size_limit")
	require.LessOrEqual(t, churn.maxWAL.Load(), 2*mark,
		"under the same churn the WAL stays within the last-resort mark plus one attempt's growth")
	require.LessOrEqual(t, stats.WriterHoldMax, walReclaimMaxWriterHold+raceSlack(100*time.Millisecond))
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

// A generation bulk window holds the checkpoint lease for as long as it is
// open — live, 14 minutes per closure build while the log grew to 22 GB. Over
// the ceiling the reclaim runs inside the window anyway (bounded by the
// writer-hold cap, its writer step taking the write gate between the window's
// own transactions), so the log stays near the ceiling while the window stays
// open and keeps writing.
func TestPerformanceWALReclaimRunsInsideALongBulkWindowOverTheCeiling(t *testing.T) {
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

	initial := s.WALReclaimStats()
	started := time.Now()
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
	var stopOnce sync.Once
	stopWorkers := func() {
		stopOnce.Do(func() { close(stop) })
		wg.Wait()
	}
	defer stopWorkers()

	// Filling the log is setup for this oracle: give the first ceiling
	// override a bounded activation window, then observe a full 25 seconds
	// of pressure with the bulk window, writers, and readers still active.
	// The maximum WAL sampler spans both setup and observation.
	activationDeadline := time.NewTimer(25 * time.Second)
	activationPoll := time.NewTicker(5 * time.Millisecond)
	activated := false
	var activation WALReclaimStats
activationLoop:
	for {
		activation = s.WALReclaimStats()
		if activation.LeaseOverrides > initial.LeaseOverrides {
			activated = true
			break
		}
		select {
		case <-activationPoll.C:
		case <-activationDeadline.C:
			break activationLoop
		}
	}
	activationPoll.Stop()
	activationDeadline.Stop()
	activatedAt := time.Now()
	activationWrites := writes.Load()
	activationWAL := walFileSize(path + "-wal")
	if activated {
		time.Sleep(25 * time.Second)
	}
	// Capture progress before withdrawing the active producers. Shutdown or
	// their final drained transactions cannot supply the required reset.
	observation := s.WALReclaimStats()
	stopWorkers()
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
	t.Logf("bulk pressure activation: reached=%t at=%s elapsed=%s writes=%d wal=%.1fMiB lease_overrides=%d resets=%d",
		activated, activatedAt.Format("15:04:05.000"), activatedAt.Sub(started), activationWrites, float64(activationWAL)/(1<<20), activation.LeaseOverrides, activation.Resets)
	t.Logf("bulk pressure observation 25 s: writes=%d wal_max=%.1fMiB ceiling=48MiB lease_overrides=%d resets=%d active_resets=%d writer_hold_max=%s",
		writes.Load(), float64(maxWAL.Load())/(1<<20), st.LeaseOverrides, st.Resets, observation.Resets-activation.Resets, st.WriterHoldMax)
	require.True(t, activated, "no ceiling override activated within the bounded setup window")
	require.Positive(t, st.LeaseOverrides, "no reclaim ran inside the bulk window")
	require.Greater(t, observation.Resets, activation.Resets, "the log was never reset during the active pressure observation")
	require.LessOrEqual(t, maxWAL.Load(), int64(2*48)<<20, "the log passed twice its ceiling inside the bulk window")
	require.LessOrEqual(t, st.WriterHoldMax, walReclaimMaxWriterHold+250*time.Millisecond)
	require.Contains(t, out, "wal reclaim running inside a bulk window reason=wal_ceiling")
}

// The long-reader pattern at a scaled threshold (16 MiB). The contract:
//   - no write waits for the gate longer than one short hold
//     (walReclaimResetHold, 50 ms): the reclaim waits for the readers
//     without the writer and takes it only for the reset;
//   - a long gap that starts with the log over the threshold ends with the
//     log reset (the first gap that allows a reset gets it);
//   - the log stays under the threshold plus the two bursts between such
//     gaps (a burst can start just under the threshold, the short gap cannot
//     reset, the next burst lands before the long gap);
//   - no reader is paused.
//
// It replaced a contract under which the writer was held up to 2 s while
// the reclaim waited for older readers: writes waited up to 2 s, and the log
// stayed within three times the threshold.
func TestPerformanceWALReclaimResetsUnderLongOverlappingReadersAndBurstyWrites(t *testing.T) {
	t.Setenv("GORTEX_SQLITE_WAL_RECLAIM_MB", "16")
	// A production-like cadence (the daemon polls every 5 s).
	setWALReclaimCadence(t, time.Second, time.Second, 4*time.Second)
	logs := captureReclaimLog(t)
	s, path := openWALReclaimStore(t)
	defer func() { _ = s.Close() }()
	seedWALChurnTable(t, s)
	prepareSteadyStateLongReaderStore(t, s)
	p := longReaderPattern{
		readers: 4, readMin: 2500 * time.Millisecond, readMax: 4 * time.Second,
		burstWrites: 12, shortGap: 1500 * time.Millisecond, longGap: 12 * time.Second, cycles: 3,
	}
	run := runLongReaderPattern(t, s, path, p)
	checkLongReaderContract(t, s, run, 16<<20, logs.String())
}

// Edit cycles take the build lane for a few hundred milliseconds with gaps of
// a few hundred between them, while admitted writes take the log over its threshold
// but stay under the pressure mark. The reclaim loop's checkpoint work may run only in the
// gaps: no checkpoint starts inside a cycle, and one running when a cycle
// starts stops active work within the lane poll (20 ms) plus scheduling slack.
// Positively observed same-call VFS sync entered before the edit is reported
// separately from active work; unknown or ambiguous spans retain the wall bound.
// Raw overlap and late returns remain visible alongside the active-work share.
func TestPerformanceWALReclaimUsesNoTimeInsideEditCycles(t *testing.T) {
	t.Setenv("GORTEX_SQLITE_WAL_RECLAIM_MB", "8")
	// Ceiling 64 MiB: the pressure mark (4 x the threshold) is 32 MiB, above
	// the log in this test, so every attempt waits for the gaps.
	t.Setenv("GORTEX_SQLITE_WAL_RECLAIM_CEILING_MB", "64")
	prevFloor := walReclaimCeilingFloor
	walReclaimCeilingFloor = 0
	t.Cleanup(func() { walReclaimCeilingFloor = prevFloor })
	setWALReclaimCadence(t, 25*time.Millisecond, 25*time.Millisecond, 200*time.Millisecond)

	type span = reclaimCheckpointSpan
	var mu sync.Mutex
	var calls []span
	var cycles []span
	walCheckpointCallObserver = func(_ string, start time.Time, took time.Duration) {
		mu.Lock()
		calls = append(calls, span{start, start.Add(took)})
		mu.Unlock()
	}
	t.Cleanup(func() { walCheckpointCallObserver = nil })
	// A paced pass that meets an edit waits in its page writes instead of
	// being interrupted (wal_copy_pause.go): its span may cover the edit, but
	// it writes nothing there. Those passes are judged by the pages they
	// wrote inside the edits, not by their span.
	var pausedPasses []span
	var checkpointSyncs []reclaimCheckpointSyncInterval
	walCopyPassObserver = func(start, end time.Time, pauses int) {
		if pauses > 0 {
			mu.Lock()
			pausedPasses = append(pausedPasses, span{start, end})
			mu.Unlock()
		}
	}
	t.Cleanup(func() { walCopyPassObserver = nil })

	installCreditCheckpointSyncObservation(t)
	s, path := openWALReclaimStore(t)
	// Register Close before lease acquisition so any failed setup still joins
	// active SQL. A successful lease cancels and joins the previous attempt,
	// then prevents another background attempt during observer/policy setup.
	var phaseProbeInstalled bool
	defer func() {
		if phaseProbeInstalled {
			reclaimCheckpointSyncProbeState.Store(nil)
		}
	}()
	defer func() { _ = s.Close() }()
	lease, leaseErr := s.acquireGenerationBulkCheckpointLease()
	require.NoError(t, leaseErr)
	defer s.releaseGenerationBulkCheckpointLease(lease)
	s.backgroundCheckpoint.mu.Lock()
	activeAtInstallation := s.backgroundCheckpoint.active
	s.backgroundCheckpoint.mu.Unlock()
	require.Nil(t, activeAtInstallation, "setup lease must join the prior background attempt")
	reclaimCheckpointSyncProbeState.Store(&reclaimCheckpointSyncProbe{store: s, observe: func(p reclaimCheckpointSyncInterval) {
		mu.Lock()
		checkpointSyncs = append(checkpointSyncs, p)
		mu.Unlock()
	}})
	phaseProbeInstalled = true
	seedWALChurnTable(t, s)
	growWAL(t, s, 12) // start over the threshold, below the pressure mark
	lane := &fakeBuildLane{}
	lane.install(s)
	require.True(t, s.releaseGenerationBulkCheckpointLease(lease))

	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { // the edit cycles
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			case <-time.After(250 * time.Millisecond): // gap
			}
			start := time.Now()
			lane.held.Store(true)
			time.Sleep(400 * time.Millisecond)
			lane.held.Store(false)
			mu.Lock()
			cycles = append(cycles, span{start, time.Now()})
			mu.Unlock()
		}
	}()
	// Qualify the ordinary nonpressure policy: stop admitting new fixture
	// writes at 24 MiB until the store itself resets. This preserves headroom
	// below the 32 MiB pressure exception; it is not a sustained-throughput
	// workload (the separate pressure and bulk cases exercise that policy).
	// No checkpoint of the test's own supplies progress.
	initialResets := s.WALReclaimStats().Resets
	var committedWrites []span
	var writesAfterReset, admissionPauses int
	var admissionWait time.Duration
	var maxWAL int64
	var writeErr error
	wg.Add(2)
	go func() {
		defer wg.Done()
		for k := 0; ; k++ {
			select {
			case <-stop:
				return
			case <-time.After(20 * time.Millisecond):
			}
			// Read the reset counter first: a reset racing the size sample
			// must release this wait rather than require another reset.
			resets := s.WALReclaimStats().Resets
			size := walFileSize(path + "-wal")
			maxWAL = max(maxWAL, size)
			if size >= 24<<20 {
				admissionPauses++
				started := time.Now()
				for s.WALReclaimStats().Resets <= resets {
					maxWAL = max(maxWAL, walFileSize(path+"-wal"))
					select {
					case <-stop:
						admissionWait += time.Since(started)
						return
					case <-time.After(20 * time.Millisecond):
					}
				}
				admissionWait += time.Since(started)
			}
			wasReset := s.WALReclaimStats().Resets > initialResets
			started := time.Now()
			if err := churnWriteOnce(s, k); err != nil {
				mu.Lock()
				writeErr = err
				mu.Unlock()
				return
			}
			committedWrites = append(committedWrites, span{started, time.Now()})
			if wasReset {
				writesAfterReset++
			}
			if w := walFileSize(path + "-wal"); w > maxWAL {
				maxWAL = w
			}
		}
	}()
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			case <-time.After(30 * time.Millisecond):
			}
			var n int
			_ = s.db.QueryRow(`SELECT count(*) FROM wal_churn WHERE id < 100`).Scan(&n)
		}
	}()
	time.Sleep(8 * time.Second)
	close(stop)
	wg.Wait()
	// Freeze progress before cleanup. Join the owned checkpoint loop before
	// matching calls: an unfinished concurrent call must not be invisible to
	// the ambiguity check, and shutdown must not supply observation resets.
	stats := s.WALReclaimStats()
	s.stopCheckpointLoop()
	s.backgroundCheckpoint.mu.Lock()
	activeAtClassification := s.backgroundCheckpoint.active
	s.backgroundCheckpoint.mu.Unlock()
	require.Nil(t, activeAtClassification, "classification requires all owned checkpoint calls joined")

	mu.Lock()
	defer mu.Unlock()
	require.NoError(t, writeErr)
	// Scheduling slack past the 20 ms lane poll; the race detector slows the
	// checkpoint's interrupt checks several-fold, so its bound is wider.
	slack := 60 * time.Millisecond
	if raceDetectorOn {
		slack = 250 * time.Millisecond
	}
	paused := func(k span) bool {
		for _, p := range pausedPasses {
			if !k.start.Before(p.start) && !k.end.After(p.end.Add(time.Millisecond)) {
				return true
			}
		}
		return false
	}
	// A same-TLS/Store span must belong wholly to exactly this call and overlap
	// no other call. Ambiguous, post-edit and unknown spans waive nothing.
	knownSync := func(callIndex int, editStart, end time.Time) time.Duration {
		return knownPreEditCheckpointSync(s, calls, callIndex, checkpointSyncs, editStart, end)
	}
	var inside, rawInside, allRawInside, cycleTime time.Duration
	startedInside, lateStops, rawLateStops := 0, 0, 0
	for _, c := range cycles {
		cycleTime += c.end.Sub(c.start)
		for callIndex, k := range calls {
			allLo, allHi := maxTime(c.start, k.start), minTime(c.end, k.end)
			if allHi.After(allLo) {
				allRawInside += allHi.Sub(allLo)
			}
			if paused(k) {
				continue
			}
			lo, hi := maxTime(c.start, k.start), minTime(c.end, k.end)
			if hi.After(lo) {
				rawInside += hi.Sub(lo)
				inside += max(time.Duration(0), hi.Sub(lo)-knownSync(callIndex, c.start, hi))
			}
			if k.start.After(c.start.Add(slack)) && k.start.Before(c.end) {
				startedInside++
			}
			if k.start.Before(c.start) && k.end.After(c.start.Add(walCheckpointCycleYieldPoll+slack)) {
				rawLateStops++
				activeStop := k.end.Sub(c.start) - knownSync(callIndex, c.start, k.end)
				if activeStop > walCheckpointCycleYieldPoll+slack {
					lateStops++
				}
			}
		}
	}
	writesInsideCycles := 0
	for _, w := range committedWrites {
		for _, c := range cycles {
			if !w.start.Before(c.start) && !w.end.After(c.end) {
				writesInsideCycles++
				break
			}
		}
	}
	t.Logf("nonpressure producer: commits=%d entirely_inside_cycles=%d commits_after_reset=%d actual_observation_resets=%d admission_pauses=%d admission_wait=%s high_water=24MiB", len(committedWrites), writesInsideCycles, writesAfterReset, stats.Resets-initialResets, admissionPauses, admissionWait)
	require.GreaterOrEqual(t, len(committedWrites), 12, "the nonpressure fixture must commit meaningful real writes")
	require.Positive(t, writesInsideCycles, "the fixture must include actual committed writes during edit cycles")
	require.Positive(t, stats.Resets-initialResets, "setup or shutdown must not supply the observation's reset progress")
	require.Positive(t, writesAfterReset, "real writes must resume after an authoritative reset")
	var worstForeground time.Duration
	for _, w := range committedWrites {
		worstForeground = max(worstForeground, w.end.Sub(w.start))
	}
	for i, p := range checkpointSyncs {
		t.Logf("checkpoint_sync i=%d TLS=%x store=%p pacer=%p credit=%p start=%s end=%s duration=%s", i, p.tls, p.store, p.pacer, p.credit, p.span.start.Format(time.RFC3339Nano), p.span.end.Format(time.RFC3339Nano), p.span.end.Sub(p.span.start))
	}
	t.Logf("all_raw_checkpoint_overlap=%s original_nonpaused_raw_overlap=%s raw_late_stops=%d observed_sync_intervals=%d foreground_gate_SQL_max=%s", allRawInside, rawInside, rawLateStops, len(checkpointSyncs), worstForeground)
	require.LessOrEqual(t, worstForeground, 500*time.Millisecond, "actual foreground SQL exceeded existing mutation latency contract")
	share := float64(inside) / float64(max(cycleTime, 1))
	t.Logf("cycles=%d checkpoint_calls=%d time_inside_cycles=%s of %s (%.2f%%) started_inside=%d late_stops=%d resets=%d wal_max=%.1fMiB",
		len(cycles), len(calls), inside.Round(time.Millisecond), cycleTime.Round(time.Millisecond), 100*share,
		startedInside, lateStops, stats.Resets, float64(maxWAL)/(1<<20))
	require.GreaterOrEqual(t, len(cycles), 5)
	require.Greater(t, maxWAL, int64(8<<20), "precondition: the log passed its threshold during the cycles")
	require.Less(t, maxWAL, int64(32<<20), "precondition: the log stayed under the pressure mark")
	require.Positive(t, stats.Resets, "the reclaim must still reset the log in the gaps")
	require.Zero(t, s.WALCopyStats().WrittenWhileBusyBytes, "a paused pass wrote pages inside an edit cycle")
	require.Zero(t, startedInside, "a checkpoint started inside an edit cycle")
	require.Zero(t, lateStops, "a checkpoint ran on past the start of an edit cycle")
	maxShare := 0.05
	if raceDetectorOn {
		maxShare = 0.10
	}
	require.Less(t, share, maxShare, "the reclaim exceeded active-work share outside positively observed sync inside edit cycles")
}

// A copy budget below the writers' rate, while a checkout is being edited:
// under the pressure mark the copy is paced and falls behind, so the log
// grows to the mark; over it the copy is not paced, and the log is reset and
// stays bounded. Scaled: threshold 4 MiB, pressure mark 16 MiB (the fold's
// mark stays 1 GiB, so only the pressure mark can end the pacing here), a
// budget of 0.5 MiB/s against a writer of ~4 MiB/s, and edits of 300 ms
// every 2.2 s. When only the fold's mark ended the pacing, no attempt
// converged: the log reached 61 MiB in 15 s with no reset.
func TestPerformanceTheCopyIsNotPacedOverThePressureMark(t *testing.T) {
	t.Setenv("GORTEX_SQLITE_WAL_RECLAIM_MB", "4")
	t.Setenv("GORTEX_SQLITE_WAL_RECLAIM_CEILING_MB", "64")
	setWALReclaimCadence(t, 25*time.Millisecond, 25*time.Millisecond, 200*time.Millisecond)
	s, path := openWALReclaimStore(t)
	defer func() { _ = s.Close() }()
	if walCopyMethods.Load() == 0 {
		t.Skip("the copy pause is not installed in this process")
	}
	seedWALChurnTable(t, s)
	lane := &fakeBuildLane{}
	lane.install(s)
	slowCopy(t, s, 30<<20) // 0.5 MiB/s while editing

	stop := make(chan struct{})
	var wg sync.WaitGroup
	var mu sync.Mutex
	var maxWAL int64
	var writeErr error
	wg.Add(2)
	go func() { // the edits
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			case <-time.After(2200 * time.Millisecond):
			}
			lane.held.Store(true)
			time.Sleep(300 * time.Millisecond)
			lane.held.Store(false)
		}
	}()
	go func() { // about 4 MiB/s of page rewrites
		defer wg.Done()
		for k := 0; ; k++ {
			select {
			case <-stop:
				return
			case <-time.After(250 * time.Millisecond):
			}
			if err := churnWriteOnce(s, k); err != nil {
				mu.Lock()
				writeErr = err
				mu.Unlock()
				return
			}
			mu.Lock()
			if w := walFileSize(path + "-wal"); w > maxWAL {
				maxWAL = w
			}
			mu.Unlock()
		}
	}()
	time.Sleep(15 * time.Second)
	close(stop)
	wg.Wait()
	st, cp := s.WALReclaimStats(), s.WALCopyStats()
	t.Logf("wal_max=%.1fMiB resets=%d pressure_runs=%d pressure_resets=%d budget_wait=%s written=%.1fMiB",
		float64(maxWAL)/(1<<20), st.Resets, cp.PressureRuns, cp.PressureResets, cp.BudgetWait.Round(time.Millisecond), float64(cp.WrittenBytes)/(1<<20))
	require.NoError(t, writeErr)
	require.Positive(t, st.Resets, "the log was never reset with the budget below the writers' rate")
	require.Less(t, maxWAL, int64(32<<20), "the log outgrew twice its pressure mark")
}

// Edits every 2.5 s (the measured 25 s cadence at a tenth of the scale: 0.3 s
// refresh, 2.2 s gap) against a copy slower than one gap: with the pause the
// ordinary passes pause across edits, while pressure may resume copying
// inside edits once the producer outpaces that budget. The combined policy
// must keep the log under 32 MiB and keep resetting it over the intended 60
// completed mutations, with at least 15 s of live observation. The 250 ms
// cadence follows each SQL completion, so 15 s alone does not prove that work
// was delivered on slower storage. The 30 s delivery deadline bounds this
// fixed-work fixture without changing production or request budgets.
// Ordinary no-busy-copy
// behavior is checked separately by TestReclaimCopyPausesForAnEditAndKeepsItsProgress.
func TestPerformanceReclaimKeepsTheLogBoundedWithEditsEveryFewSeconds(t *testing.T) {
	t.Setenv("GORTEX_SQLITE_WAL_RECLAIM_MB", "4")
	t.Setenv("GORTEX_SQLITE_WAL_RECLAIM_CEILING_MB", "64") // hard cap 256 MiB: out of reach
	setWALReclaimCadence(t, 25*time.Millisecond, 25*time.Millisecond, 200*time.Millisecond)
	s, path := openWALReclaimStore(t)
	defer func() { _ = s.Close() }()
	if walCopyMethods.Load() == 0 {
		t.Skip("the copy pause is not installed in this process")
	}
	// Configure the predicate and slow copy before an observed attempt starts.
	// A startup pass begun before installation would correctly yield instead
	// of using the paused-copy policy this workload is meant to exercise.
	lease, err := s.acquireGenerationBulkCheckpointLease()
	require.NoError(t, err)
	defer func() { s.releaseGenerationBulkCheckpointLease(lease) }()
	seedWALChurnTable(t, s)
	growWAL(t, s, 16)
	lane := &fakeBuildLane{}
	lane.install(s)
	// 2 MiB/s: a pass over the table's ~6 MiB of pages (about 2.8 s) takes
	// longer than a gap between edits (2.2 s).
	slowCopy(t, s, 120<<20)
	maxWAL := walFileSize(path + "-wal") // include the seed before a reset can clear it
	require.True(t, s.releaseGenerationBulkCheckpointLease(lease))

	stop := make(chan struct{})
	var wg sync.WaitGroup
	var mu sync.Mutex
	var writeErr error
	var completedWrites int
	var firstWriteAt, workDeliveredAt, worstWriteStarted, worstWriteFinished time.Time
	var maxWriteDuration time.Duration
	started := time.Now()
	deliveryDeadline := started.Add(30 * time.Second)
	var stopOnce sync.Once
	stopWorkers := func() {
		stopOnce.Do(func() { close(stop) })
		wg.Wait()
	}
	defer stopWorkers()
	wg.Add(2)
	go func() { // the edits
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			case <-time.After(2200 * time.Millisecond):
			}
			lane.held.Store(true)
			time.Sleep(300 * time.Millisecond)
			lane.held.Store(false)
		}
	}()
	go func() { // about 4 MiB/s of page rewrites
		defer wg.Done()
		for k := 0; ; k++ {
			select {
			case <-stop:
				return
			case <-time.After(250 * time.Millisecond):
			}
			writeStarted := time.Now()
			err := churnWriteOnce(s, k)
			writeFinished := time.Now()
			mu.Lock()
			if duration := writeFinished.Sub(writeStarted); duration > maxWriteDuration {
				maxWriteDuration = duration
				worstWriteStarted, worstWriteFinished = writeStarted, writeFinished
			}
			if err != nil {
				writeErr = err
				mu.Unlock()
				return
			}
			completedWrites++
			if completedWrites == 1 {
				firstWriteAt = writeFinished
			}
			if completedWrites == 60 {
				workDeliveredAt = writeFinished
			}
			if w := walFileSize(path + "-wal"); w > maxWAL {
				maxWAL = w
			}
			mu.Unlock()
		}
	}()
	deadline := time.NewTimer(time.Until(deliveryDeadline))
	defer deadline.Stop()
	poll := time.NewTicker(20 * time.Millisecond)
	defer poll.Stop()
observe:
	for {
		mu.Lock()
		complete := completedWrites >= 60 && !workDeliveredAt.After(deliveryDeadline)
		failed := writeErr != nil
		mu.Unlock()
		if complete && time.Since(started) >= 15*time.Second {
			break
		}
		if failed {
			break
		}
		select {
		case <-deadline.C:
			break observe
		case <-poll.C:
		}
	}
	observationFinished := time.Now()
	stopWorkers()
	// A deadline notification can race the final pre-deadline completion. The
	// joined completion timestamp decides delivery, not which select case won.
	delivered := completedWrites >= 60 && !workDeliveredAt.IsZero() &&
		!workDeliveredAt.After(deliveryDeadline) && observationFinished.Sub(started) >= 15*time.Second
	st, cp := s.WALReclaimStats(), s.WALCopyStats()
	t.Logf("periodic workload: completed_writes=%d required_writes=60 delivered=%t observation=%s delivery_deadline=30s first_write_at=%s work_delivered_at=%s write_gate_and_sql_max=%s worst_write_start=%s worst_write_end=%s",
		completedWrites, delivered, observationFinished.Sub(started), firstWriteAt.Format(time.RFC3339Nano), workDeliveredAt.Format(time.RFC3339Nano),
		maxWriteDuration, worstWriteStarted.Format(time.RFC3339Nano), worstWriteFinished.Format(time.RFC3339Nano))
	t.Logf("wal_max=%.1fMiB resets=%d copy: passes=%d paused_passes=%d paused=%s budget_wait=%s written=%.1fMiB written_while_busy=%d attempts_with_a_discarded_pass=%d paced_cut_short=%d pressure_runs=%d pressure_resets=%d pressure_lane_resets=%d pause_mark_or_cap_overruns=%d",
		float64(maxWAL)/(1<<20), st.Resets, cp.Passes, cp.PausedPasses, cp.Paused.Round(time.Millisecond), cp.BudgetWait.Round(time.Millisecond),
		float64(cp.WrittenBytes)/(1<<20), cp.WrittenWhileBusyBytes, cp.DiscardedPasses, cp.PacedPassesCutShort,
		cp.PressureRuns, cp.PressureResets, cp.PressureLaneResets, cp.PauseCapOverruns)
	require.NoError(t, writeErr)
	require.True(t, delivered, "60 mutations were not delivered within the 30 s fixture deadline")
	require.GreaterOrEqual(t, completedWrites, 60)
	require.GreaterOrEqual(t, observationFinished.Sub(started), 15*time.Second)
	require.Less(t, maxWAL, int64(32<<20), "the log outgrew its bound under edits every 2.5 s")
	require.Zero(t, cp.PacedPassesCutShort, "a paced pass was cut short")
	// Copying through a busy lane is permitted once pressure is reached.
	// Keep it visible above; the dedicated ordinary-copy case forbids it.
	require.GreaterOrEqual(t, st.Resets, int64(3), "the log was reset too rarely")
	require.Positive(t, cp.PausedPasses, "precondition: passes spanned edits")
}

// A burst: the lane busy 70% of the time (0.7 s held, 0.3 s free) and writes
// faster than the gaps let a paused copy follow. Over the pressure mark the
// copy runs through the edits and the reset takes its short hold inside the
// busy lane, so the log is reset during the burst and stays near the mark plus
// one pass; without the pressure mode it grows through the whole burst.
func TestPerformanceReclaimResetsDuringABurstOverThePressureMark(t *testing.T) {
	s, path, lane := pressureStore(t, 240<<20) // 4 MiB/s while editing: above the distinct write rate
	if walCopyMethods.Load() == 0 {
		t.Skip("the copy pause is not installed in this process")
	}
	s.walCopy.sawBusy(time.Now())
	growWAL(t, s, 24) // start over the 16 MiB mark
	stop := make(chan struct{})
	var wg sync.WaitGroup
	var mu sync.Mutex
	var maxWAL int64
	var writeErr error
	wg.Add(2)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			lane.held.Store(true)
			time.Sleep(700 * time.Millisecond)
			lane.held.Store(false)
			time.Sleep(300 * time.Millisecond)
		}
	}()
	go func() { // ~7 MiB/s of page rewrites
		defer wg.Done()
		for k := 0; ; k++ {
			select {
			case <-stop:
				return
			case <-time.After(150 * time.Millisecond):
			}
			if err := churnWriteOnce(s, k); err != nil {
				mu.Lock()
				writeErr = err
				mu.Unlock()
				return
			}
			mu.Lock()
			if w := walFileSize(path + "-wal"); w > maxWAL {
				maxWAL = w
			}
			mu.Unlock()
		}
	}()
	time.Sleep(15 * time.Second)
	close(stop)
	wg.Wait()
	cp, st := s.WALCopyStats(), s.WALReclaimStats()
	t.Logf("burst: wal_max=%.1fMiB resets=%d pressure_runs=%d pressure_resets=%d (inside the lane %d) pressure_give_ups=%d writer_hold_max=%s adaptive_attempts=%d adaptive_hold_max=%s adaptive_budget_max=%s",
		float64(maxWAL)/(1<<20), st.Resets, cp.PressureRuns, cp.PressureResets, cp.PressureLaneResets, cp.PressureGiveUps, st.WriterHoldMax, st.AdaptiveWriterAttempts, st.AdaptiveWriterHoldMax, st.AdaptiveWriterBudgetMax)
	require.NoError(t, writeErr)
	// Plain runs pin at least two; the race detector slows every attempt past
	// the test's window, so there one is enough to prove the path.
	minResets := int64(2)
	if raceDetectorOn {
		minResets = 1
	}
	require.GreaterOrEqual(t, cp.PressureResets, minResets, "attempts begun inside the busy lane did not reset the log")
	// The mark (16 MiB) plus what the writes add while one attempt converges
	// (~5 s at 6.7 MiB/s); without the pressure mode the log grows past 90 MiB.
	require.Less(t, maxWAL, int64(64<<20), "the log outgrew the pressure mark plus one attempt")
	// The fast path retains its original short cap. A measured adaptive
	// urgent completion uses the separately bounded slow-storage policy;
	// WAL size, reset cadence and ordinary fast controls stay unchanged.
	// Durations are integral nanoseconds: preserve the original strict
	// short bound while allowing equality with the measured adaptive max.
	holdBound := walReclaimPressureHold + 100*time.Millisecond - time.Nanosecond
	if st.AdaptiveWriterHoldMax > 0 {
		require.Positive(t, st.AdaptiveWriterAttempts)
		require.Less(t, st.AdaptiveWriterHoldMax, walReclaimMaxWriterHold+100*time.Millisecond)
		holdBound = max(holdBound, st.AdaptiveWriterHoldMax)
	}
	require.LessOrEqual(t, st.WriterHoldMax, holdBound, "a hold exceeded both the ordinary bound and the measured adaptive hold")
}
