package store_sqlite

import (
	"context"
	"database/sql"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// Existing controls use real pools with a shorter SQLite Busy wait. A true
// policy argument constructs the production defaults; a functional caller may
// then explicitly override only its private writer credit, as documented below.
func pressureReaderDrainFixture(t *testing.T) (*Store, chan struct{}, func() error) {
	t.Helper()
	return pressureReaderDrainFixturePolicy(t, false)
}

func pressureReaderDrainFixturePolicy(t *testing.T, productionPolicy bool) (*Store, chan struct{}, func() error) {
	t.Helper()
	t.Setenv("GORTEX_SQLITE_WAL_RECLAIM_MB", "0")
	t.Setenv("GORTEX_SQLITE_LAZY_INDEXES", "off")
	s, _ := openWALReclaimStore(t)
	t.Cleanup(func() { _ = s.Close() })
	s.stopCheckpointLoop()
	manualStop := make(chan struct{})
	s.stopCheckpoint = manualStop
	t.Cleanup(func() { close(manualStop) })
	// Twice the production writer step. These tests are about reader drain
	// and gate ownership, not about how long one hold may last; a busy CI
	// runner gets headroom, and missing even double the budget means an
	// unacceptable slowdown. Set after the checkpoint loop stopped, so no
	// background reclaim reads it concurrently.
	if !productionPolicy {
		s.walPressureHoldOverride = 2 * walReclaimPressureHold
		oldBusy := walResetBusyMillis
		walResetBusyMillis = 10
		t.Cleanup(func() { walResetBusyMillis = oldBusy })
	}
	nodes, items := ftsDocs("drain", 2, func(int) string { return "rare" })
	s.AddBatch(nodes, nil)
	require.NoError(t, s.BatchUpsertSymbolFTS(items))
	require.NoError(t, s.EnsureRowCounters(t.Context()))
	ctx, cancel := context.WithCancel(t.Context())
	release := make(chan struct{})
	entered := make(chan struct{})
	done := make(chan error, 1)
	readerJoined := false
	joinReader := func() error {
		select {
		case err := <-done:
			readerJoined = true
			return err
		case <-time.After(5 * time.Second):
			return context.DeadlineExceeded
		}
	}
	prior := symbolFTSPrefixWalkObserver
	var once sync.Once
	symbolFTSPrefixWalkObserver = func() {
		once.Do(func() { close(entered) })
		select {
		case <-release:
		case <-ctx.Done():
		}
	}
	t.Cleanup(func() {
		cancel()
		select {
		case <-release:
		default:
			close(release)
		}
		if !readerJoined {
			_ = joinReader()
			if !readerJoined {
				t.Error("held scoring reader did not join")
			}
		}
		symbolFTSPrefixWalkObserver = prior
	})
	go func() {
		_, hits, err := s.SymbolFTSScoringSnapshot(ctx, []string{"rare"})
		if err == nil && hits["rare"] != 2 {
			err = errSQLiteCheckpointIncomplete
		}
		done <- err
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("known scoring snapshot not established")
	}
	ckpt, err := openWALReclaimCheckpointDB(s.dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = ckpt.Close() })
	result, err := checkpointWALOnceOn(t.Context(), ckpt, "PASSIVE")
	require.NoError(t, err)
	require.Positive(t, result.WALFrames)
	require.Equal(t, result.WALFrames, result.CheckpointedFrames)
	return s, release, joinReader
}

func TestPressureBusyResetDrainsOutsideWriterBeforeNewReaders(t *testing.T) {
	testPressureReaderDrain(t, false)
}
func TestPressureAdaptiveBusyResetDrainsWithoutRenewingCredit(t *testing.T) {
	testPressureReaderDrain(t, true)
}

// The default nonblocking Busy policy is observed on the actual reset
// connection. The real Busy/drain mechanism has explicit fixture-only writer
// credit, so this correctness oracle does not require setup and SQLite work
// to complete within the production 50 ms credit on a CI host. Default credit
// exhaustion is exercised independently below.
func TestPressureDefaultBusyResetWitnessDrainsOutsideWriter(t *testing.T) {
	for _, inPlace := range []bool{false, true} {
		name := "truncate"
		if inPlace {
			name = "restart"
		}
		t.Run(name, func(t *testing.T) {
			if inPlace {
				// Scale only the file-size branch. The helper explicitly controls
				// fixture credit while checking the actual default Busy policy.
				prior := walShrinkInPlaceBytes
				walShrinkInPlaceBytes = 1
				t.Cleanup(func() { walShrinkInPlaceBytes = prior })
			}
			testPressureReaderDrainPolicy(t, false, true, inPlace)
		})
	}
}

func testPressureReaderDrain(t *testing.T, adaptive bool) {
	testPressureReaderDrainPolicy(t, adaptive, false, false)
}

func testPressureReaderDrainPolicy(t *testing.T, adaptive, productionPolicy, inPlace bool) {
	s, release, joinReader := pressureReaderDrainFixturePolicy(t, productionPolicy)
	busyWitness := make(chan walCheckpointResult, 1)
	checkBusyPolicy := func() {}
	if productionPolicy {
		require.Zero(t, s.walPressureHoldOverride)
		require.EqualValues(t, sqliteCheckpointBusyTimeoutMillis, walResetBusyMillis)
		require.Equal(t, 50*time.Millisecond, s.walPressureHold())
		// Functional fixture credit only, not a change to the production
		// policy. Keep the existing aggregate writer bound assertion below.
		s.walPressureHoldOverride = time.Second
		type busyPolicy struct {
			millis int
			err    error
		}
		var policyMu sync.Mutex
		var policies []busyPolicy
		priorPolicyObserver := walResetCheckpointConnObserver
		walResetCheckpointConnObserver = func(ctx context.Context, conn *sql.Conn) {
			var millis int
			err := conn.QueryRowContext(ctx, `PRAGMA busy_timeout`).Scan(&millis)
			policyMu.Lock()
			policies = append(policies, busyPolicy{millis, err})
			policyMu.Unlock()
		}
		t.Cleanup(func() { walResetCheckpointConnObserver = priorPolicyObserver })
		checkBusyPolicy = func() {
			// Called only after the reset goroutine joined; no assertion or
			// retained connection escapes the background observer callback.
			policyMu.Lock()
			observed := append([]busyPolicy(nil), policies...)
			policyMu.Unlock()
			require.NotEmpty(t, observed, "actual reset connection policy was not observed")
			for _, policy := range observed {
				require.NoError(t, policy.err)
				require.Zero(t, policy.millis, "the actual pressure checkpoint must use nonblocking Busy policy")
			}
		}
		priorObserver := walCheckpointResultObserver
		expectedMode := "TRUNCATE"
		if inPlace {
			expectedMode = "RESTART"
		}
		walCheckpointResultObserver = func(mode string, _ time.Time, _ time.Duration, result walCheckpointResult, scanErr error) {
			if mode == expectedMode && scanErr == nil && result.Busy > 0 {
				select {
				case busyWitness <- result:
				default:
				}
			}
		}
		t.Cleanup(func() { walCheckpointResultObserver = priorObserver })
	}
	ckpt, err := openWALReclaimCheckpointDB(s.dbPath)
	require.NoError(t, err)
	defer ckpt.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	var res walReclaimResult
	if adaptive {
		frontier, ok := readWALReclaimFrontier(s.dbPath)
		require.True(t, ok)
		// Dispatch-only eligible-credit seam; both later Busy and reset are realSQLite.
		res.urgent = true
		res.slowTail = &walReclaimSlowTail{salt: frontier.salt, covered: frontier.backfill, copyElapsed: time.Second}
		priorHook := walPressureResetHook
		var calls atomic.Int64
		walPressureResetHook = func(heldCtx context.Context) {
			if calls.Add(1) == 1 {
				<-heldCtx.Done()
			}
		}
		t.Cleanup(func() { walPressureResetHook = priorHook })
	}
	done := make(chan error, 1)
	go func() { done <- s.reclaimWALPressureReset(ctx, ckpt, &res) }()
	joined := false
	t.Cleanup(func() {
		cancel()
		select {
		case <-release:
		default:
			close(release)
		}
		if !joined {
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Error("reset did not join")
			}
		}
	})
	deadline := time.Now().Add(time.Second)
	for !s.readGate.closed.Load() && time.Now().Before(deadline) {
		select {
		case e := <-done:
			joined = true
			checkBusyPolicy()
			t.Fatalf("reset returned before bounded reader drain: %v", e)
		case <-time.After(time.Millisecond):
		}
	}
	require.True(t, s.readGate.closed.Load(), "old pressure source never closes admission after realBusy")
	if productionPolicy {
		select {
		case witness := <-busyWitness:
			require.Positive(t, witness.Busy)
		default:
			t.Fatal("reader drain was not admitted by an actual SQLite Busy result")
		}
	}
	// Drain waits without holding the writer.
	require.NoError(t, s.writeMu.LockContext(ctx))
	s.writeMu.Unlock()
	beforeWait := s.ReaderWaitMark()
	readCtx, readCancel := context.WithTimeout(ctx, walReclaimPressureHold)
	var parked int
	parkedErr := s.db.QueryRowContext(readCtx, `SELECT count(*) FROM nodes`).Scan(&parked)
	readCancel()
	require.ErrorIs(t, parkedErr, context.DeadlineExceeded)
	require.Greater(t, s.ReaderWaitMark().GateWaits, beforeWait.GateWaits, "actualSQL did not park at closed gate")
	close(release)
	require.NoError(t, joinReader())
	select {
	case e := <-done:
		joined = true
		checkBusyPolicy()
		require.NoError(t, e)
	case <-time.After(5 * time.Second):
		t.Fatal("reset did not finish")
	}
	require.False(t, s.readGate.closed.Load())
	require.True(t, res.pauseClosed)
	require.Positive(t, res.pause)
	require.LessOrEqual(t, res.writerSpent, walReclaimMaxWriterHold)
	var after int
	require.NoError(t, s.db.QueryRowContext(ctx, `SELECT count(*) FROM nodes`).Scan(&after))
	require.Equal(t, 2, after)
	if adaptive {
		require.True(t, res.adaptiveUsed)
		require.LessOrEqual(t, res.adaptiveBudget, walReclaimMaxWriterHold)
	}
	snap, ok := readWALIndexSnapshot(s.dbPath)
	require.True(t, ok)
	if inPlace {
		require.LessOrEqual(t, snap.MxFrame, uint32(walShrinkMaxRestartFrames))
	} else {
		require.Zero(t, snap.MxFrame)
	}
	if productionPolicy {
		var restoredBusy int
		require.NoError(t, s.writerDB.QueryRowContext(ctx, `PRAGMA busy_timeout`).Scan(&restoredBusy))
		require.Equal(t, sqliteBusyTimeoutMillis, restoredBusy, "pressure probe must restore the writer's ordinary timeout")
	}
}

// Exhaustion is a valid outcome of the unchanged production credit. It must
// not be promoted to a Busy/readers witness or close read admission.
func TestPressureDefaultCreditExhaustionRefusesWithoutBusyEvidence(t *testing.T) {
	s, _, _ := pressureReaderDrainFixturePolicy(t, true)
	require.Zero(t, s.walPressureHoldOverride)
	require.Equal(t, 50*time.Millisecond, s.walPressureHold())
	priorHook := walPressureResetHook
	exhausted := false
	walPressureResetHook = func(ctx context.Context) { <-ctx.Done(); exhausted = true }
	t.Cleanup(func() { walPressureResetHook = priorHook })
	priorObserver := walCheckpointResultObserver
	resetCalls, busyResults := 0, 0
	walCheckpointResultObserver = func(mode string, _ time.Time, _ time.Duration, result walCheckpointResult, err error) {
		if mode == "TRUNCATE" || mode == "RESTART" {
			resetCalls++
		}
		if err == nil && result.Busy > 0 {
			busyResults++
		}
	}
	t.Cleanup(func() { walCheckpointResultObserver = priorObserver })
	ckpt, err := openWALReclaimCheckpointDB(s.dbPath)
	require.NoError(t, err)
	defer ckpt.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	var res walReclaimResult
	err = s.reclaimWALPressureReset(ctx, ckpt, &res)
	require.True(t, exhausted, "the hook must consume the actual default credit before any reset")
	require.ErrorIs(t, err, errWALPressureHold)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.Zero(t, resetCalls, "expired credit cannot dispatch a reset")
	require.Zero(t, busyResults, "deadline refusal is not a genuine SQLite Busy tuple")
	require.False(t, res.pressureResetBusy)
	require.False(t, res.pauseClosed)
	require.False(t, s.readGate.closed.Load())
	require.NoError(t, s.writeMu.LockContext(ctx), "a refused reset releases the writer")
	s.writeMu.Unlock()
}

func TestPressureReaderDrainReopensForCancellationAndForegroundWrite(t *testing.T) {
	for _, mode := range []string{"cancel", "write"} {
		t.Run(mode, func(t *testing.T) {
			s, release, joinReader := pressureReaderDrainFixture(t)
			ckpt, err := openWALReclaimCheckpointDB(s.dbPath)
			require.NoError(t, err)
			defer ckpt.Close()
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			// This control tests cancellation after a witnessed Busy reset. An
			// expired initial 50 ms writer credit correctly refuses before drain;
			// requiring Busy inside that credit would make fixture setup depend
			// on scheduling. Keep the same pinned reader and actual writer, and
			// establish the Busy precondition within the existing fixture budget.
			require.NoError(t, s.writeMu.LockContext(ctx))
			busyResult, busyErr := s.resetWALForReclaim(ctx)
			s.writeMu.Unlock()
			require.ErrorIs(t, busyErr, errSQLiteCheckpointIncomplete)
			require.Positive(t, busyResult.Busy)
			done := make(chan error, 1)
			res := walReclaimResult{pressureResetBusy: true}
			go func() {
				done <- s.reclaimWALPressureResetAfterReaderDrain(ctx, ckpt, &res, defaultWALReclaimDrainDeadline)
			}()
			joined := false
			t.Cleanup(func() {
				cancel()
				select {
				case <-release:
				default:
					close(release)
				}
				if !joined {
					select {
					case <-done:
					case <-time.After(5 * time.Second):
						t.Error("canceled reset did not join")
					}
				}
			})
			deadline := time.Now().Add(time.Second)
			for !s.readGate.closed.Load() && time.Now().Before(deadline) {
				select {
				case e := <-done:
					joined = true
					t.Fatalf("no Busy reader-drain fallback: %v", e)
				case <-time.After(time.Millisecond):
				}
			}
			require.True(t, s.readGate.closed.Load())
			if mode == "cancel" {
				cancel()
			} else {
				unannounce := s.AnnounceWrite()
				defer unannounce()
				require.NoError(t, s.writeMu.LockContext(ctx))
				changed, writeErr := s.writerDB.ExecContext(ctx, `UPDATE nodes SET name=name||'edit' WHERE view_gen=0`)
				s.writeMu.Unlock()
				require.NoError(t, writeErr)
				n, writeErr := changed.RowsAffected()
				require.NoError(t, writeErr)
				require.Equal(t, int64(2), n)
			}
			select {
			case e := <-done:
				joined = true
				if mode == "cancel" {
					require.ErrorIs(t, e, context.Canceled)
				} else {
					require.ErrorIs(t, e, errWALReclaimWriterWaiting)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("foreground cancellation failed to stop drain")
			}
			require.False(t, s.readGate.closed.Load())
			close(release)
			require.NoError(t, joinReader())
		})
	}
}

func TestPressureReaderDrainDoesNotReopenForeignGate(t *testing.T) {
	s, release, joinReader := pressureReaderDrainFixture(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	foreignDone := make(chan error, 1)
	go func() {
		reopen, _, err := s.readGate.quiesce(ctx, time.Now().Add(5*time.Second))
		if reopen != nil {
			reopen()
		}
		foreignDone <- err
	}()
	joined := false
	t.Cleanup(func() {
		cancel()
		if !joined {
			select {
			case <-foreignDone:
			case <-time.After(5 * time.Second):
				t.Error("foreign gate owner did not join")
			}
		}
	})
	until := time.Now().Add(time.Second)
	for !s.readGate.closed.Load() && time.Now().Before(until) {
		time.Sleep(time.Millisecond)
	}
	require.True(t, s.readGate.closed.Load())
	var res walReclaimResult
	ckpt, err := openWALReclaimCheckpointDB(s.dbPath)
	require.NoError(t, err)
	defer ckpt.Close()
	err = s.reclaimWALPressureReset(t.Context(), ckpt, &res)
	require.ErrorIs(t, err, errReadGateBusy)
	require.True(t, s.readGate.closed.Load())
	require.False(t, res.pauseClosed)
	require.Zero(t, res.pause)
	cancel()
	select {
	case e := <-foreignDone:
		joined = true
		require.ErrorIs(t, e, context.Canceled)
	case <-time.After(5 * time.Second):
		t.Fatal("foreign drain did not cancel")
	}
	require.False(t, s.readGate.closed.Load())
	close(release)
	require.NoError(t, joinReader())
}
