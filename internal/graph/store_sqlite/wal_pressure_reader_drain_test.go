package store_sqlite

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// All readers/writers use actual pools; the only timing seam shortens SQLite's
// Busy wait so its positive result precedes the unchanged50ms outer credit.
func pressureReaderDrainFixture(t *testing.T) (*Store, chan struct{}, func() error) {
	t.Helper()
	t.Setenv("GORTEX_SQLITE_WAL_RECLAIM_MB", "0")
	t.Setenv("GORTEX_SQLITE_LAZY_INDEXES", "off")
	s, _ := openWALReclaimStore(t)
	t.Cleanup(func() { _ = s.Close() })
	s.stopCheckpointLoop()
	manualStop := make(chan struct{})
	s.stopCheckpoint = manualStop
	t.Cleanup(func() { close(manualStop) })
	oldBusy := walResetBusyMillis
	walResetBusyMillis = 10
	t.Cleanup(func() { walResetBusyMillis = oldBusy })
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
func testPressureReaderDrain(t *testing.T, adaptive bool) {
	s, release, joinReader := pressureReaderDrainFixture(t)
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
			t.Fatalf("reset returned before bounded reader drain: %v", e)
		case <-time.After(time.Millisecond):
		}
	}
	require.True(t, s.readGate.closed.Load(), "old pressure source never closes admission after realBusy")
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
	require.Zero(t, snap.MxFrame)
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
			done := make(chan error, 1)
			var res walReclaimResult
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
