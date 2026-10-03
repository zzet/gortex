package store_sqlite

import (
	"context"
	"database/sql"
	"modernc.org/libc"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unsafe"

	"github.com/stretchr/testify/require"
)

func runFinalReclaimStep(ctx context.Context, kind string, s *Store, db *sql.DB, res *walReclaimResult) error {
	switch kind {
	case "pressure":
		return s.reclaimWALPressureReset(ctx, db, res)
	case "short":
		res.urgent = true // exercise credit expiry, not the early writer-yield path
		_, err := s.reclaimWALResetHold(ctx, walReclaimConfig{}, db, res, true, false)
		return err
	default:
		res.urgent, res.lastResort = true, true
		return s.reclaimWALInLane(ctx, walReclaimConfig{thresholdBytes: 1, ceilingBytes: 1 << 20, readerWait: time.Second, truncateBudget: walReclaimTruncateBudget}, db, res)
	}
}

func finalBackfillFixture(t *testing.T) (*Store, *sql.DB) {
	t.Helper()
	t.Setenv("GORTEX_SQLITE_WAL_RECLAIM_MB", "0")
	s, _ := openWALReclaimStore(t)
	t.Cleanup(func() { _ = s.Close() })
	seedWALChurnTable(t, s)
	db, err := openWALReclaimCheckpointDB(s.dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	_, err = checkpointWALOnceOn(t.Context(), db, "PASSIVE")
	require.NoError(t, err)
	s.writeMu.Lock()
	_, err = s.writerDB.Exec(`UPDATE wal_churn SET payload = 'before' WHERE id = 1`)
	s.writeMu.Unlock()
	require.NoError(t, err)
	return s, db
}

// Stall at the actual VFS xSync entry, before its underlying OS sync. SQLite
// still owns its checkpoint locks. A bounded credit must let foreground SQL
// commit while that call remains stalled, without omitting the real sync.
func TestReclaimFinalBackfillBoundsWriterCredit(t *testing.T) {
	for _, kind := range []string{"pressure", "short", "last_resort"} {
		t.Run(kind, func(t *testing.T) {
			s, db := finalBackfillFixture(t)
			entered, release := stallReclaimCheckpointSync(t, db)
			credit := walReclaimResetHold / 2
			if kind == "last_resort" {
				credit = walReclaimMaxWriterHold
			}
			done := make(chan error, 1)
			res := &walReclaimResult{}
			go func() { done <- runFinalReclaimStep(t.Context(), kind, s, db, res) }()
			select {
			case <-entered:
			case <-time.After(5 * time.Second):
				close(release)
				t.Fatal("final backfill never reached the driver boundary")
			}
			ctx, cancel := context.WithTimeout(t.Context(), credit+100*time.Millisecond)
			started := time.Now()
			err := s.writeMu.LockContext(ctx)
			wait := time.Since(started)
			var writeErr error
			if err == nil {
				_, writeErr = s.writerDB.ExecContext(ctx, `UPDATE wal_churn SET payload = 'foreground' WHERE id = 1`)
				s.writeMu.Unlock()
			}
			elapsed := time.Since(started)
			cancel()
			select {
			case <-done:
				close(release)
				t.Fatal("checkpoint returned while its VFS sync was still parked")
			default:
			}
			close(release)
			<-done
			require.NoError(t, err, "the final checkpoint exceeded its foreground writer credit")
			require.NoError(t, writeErr, "foreground SQL could not commit during the slow checkpoint call")
			require.Less(t, wait, credit+100*time.Millisecond)
			require.Less(t, elapsed, credit+100*time.Millisecond, "foreground gate plus SQL commit exceeded the writer credit")
			t.Logf("kind=%s credit=%s foreground_gate=%s foreground_gate_and_sql=%s", kind, credit, wait, elapsed)
			require.False(t, res.openGate, "the foreground commit must invalidate this reset admission")
			res = &walReclaimResult{}
			require.NoError(t, runFinalReclaimStep(t.Context(), kind, s, db, res))
			require.True(t, res.openGate, "the next complete backfill did not reset")
			var payload string
			require.NoError(t, s.db.QueryRow(`SELECT payload FROM wal_churn WHERE id = 1`).Scan(&payload))
			require.Equal(t, "foreground", payload)
		})
	}
}

func TestReclaimFinalBackfillRefusesAWriteAfterCopy(t *testing.T) {
	for _, kind := range []string{"pressure", "short", "last_resort"} {
		t.Run(kind, func(t *testing.T) {
			s, db := finalBackfillFixture(t)
			var copied sync.Once
			var resets atomic.Int64
			walCheckpointCallObserver = func(mode string, _ time.Time, _ time.Duration) {
				if mode == "PASSIVE" {
					copied.Do(func() {
						// The checkpoint completed, but this write wins admission
						// before reset. Its frames are outside the saved counts.
						credit := walReclaimResetHold / 2
						if kind == "last_resort" {
							credit = walReclaimMaxWriterHold
						}
						ctx, cancel := context.WithTimeout(t.Context(), credit+100*time.Millisecond)
						err := s.writeMu.LockContext(ctx)
						cancel()
						if err != nil {
							return
						} // fail-before reaches the assertions below
						_, err = s.writerDB.Exec(`UPDATE wal_churn SET payload = 'after' WHERE id = 1`)
						s.writeMu.Unlock()
						require.NoError(t, err)
					})
				} else if mode == "TRUNCATE" || mode == "RESTART" {
					resets.Add(1)
				}
			}
			t.Cleanup(func() { walCheckpointCallObserver = nil })
			res := &walReclaimResult{}
			_ = runFinalReclaimStep(t.Context(), kind, s, db, res)
			require.False(t, res.openGate, "stale checkpoint counts admitted a reset")
			require.Zero(t, resets.Load(), "the changed backfill reached a reset statement")
			snap, ok := readWALIndexSnapshot(s.dbPath)
			require.True(t, ok)
			require.Greater(t, snap.MxFrame, snap.NBackfill)
			// The next attempt copies outside the writer, then resets the
			// current fully backfilled state without losing the later write.
			res = &walReclaimResult{}
			require.NoError(t, runFinalReclaimStep(t.Context(), kind, s, db, res))
			require.True(t, walLogIsReset(s.dbPath, s.dbPath+"-wal"))
			var payload string
			require.NoError(t, s.db.QueryRow(`SELECT payload FROM wal_churn WHERE id = 1`).Scan(&payload))
			require.Equal(t, "after", payload)
		})
	}
}

func TestReclaimFinalBackfillRetriesAfterAnOldReaderEnds(t *testing.T) {
	for _, closed := range []bool{false, true} {
		name := "last_resort"
		if closed {
			name = "closed_gate"
		}
		t.Run(name, func(t *testing.T) {
			s, db := finalBackfillFixture(t)
			reader, err := s.db.BeginTx(t.Context(), &sql.TxOptions{ReadOnly: true})
			require.NoError(t, err)
			defer func() { _ = reader.Rollback() }()
			var before string
			require.NoError(t, reader.QueryRow(`SELECT payload FROM wal_churn WHERE id = 1`).Scan(&before))
			s.writeMu.Lock()
			_, err = s.writerDB.Exec(`UPDATE wal_churn SET payload = 'after-reader' WHERE id = 1`)
			s.writeMu.Unlock()
			require.NoError(t, err)
			cfg := walReclaimConfig{thresholdBytes: 1, ceilingBytes: 1 << 20, truncateBudget: walReclaimTruncateBudget}
			if !closed {
				cfg.readerWait = time.Second
			}
			var resets atomic.Int64
			walCheckpointCallObserver = func(mode string, _ time.Time, _ time.Duration) {
				if mode == "TRUNCATE" || mode == "RESTART" {
					resets.Add(1)
				}
			}
			t.Cleanup(func() { walCheckpointCallObserver = nil })
			res := &walReclaimResult{urgent: true, lastResort: true}
			err = s.reclaimWALInLane(t.Context(), cfg, db, res)
			require.Error(t, err, "a pinned reader must refuse incomplete reset admission")
			require.False(t, res.openGate)
			require.Zero(t, resets.Load())
			require.True(t, s.writeMu.TryLock(), "the refused attempt left the writer held")
			s.writeMu.Unlock()
			require.NoError(t, reader.Rollback())
			// This is the bounded outer retry: no background checkpoint lease
			// or writer is retained by the refused attempt across reader expiry.
			res = &walReclaimResult{urgent: true, lastResort: true}
			ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
			defer cancel()
			require.NoError(t, s.reclaimWALInLane(ctx, cfg, db, res))
			require.True(t, walLogIsReset(s.dbPath, s.dbPath+"-wal"))
			require.Positive(t, resets.Load())
			var after string
			require.NoError(t, s.db.QueryRow(`SELECT payload FROM wal_churn WHERE id = 1`).Scan(&after))
			require.Equal(t, "after-reader", after)
		})
	}
}

// The bulk window starts only after the checkpoint credit releases the gate.
func TestReclaimFinalBackfillRefusesBulkWindowAtReadmission(t *testing.T) {
	s, db := finalBackfillFixture(t)
	var bulkStarted bool
	var resets int
	walCheckpointCallObserver = func(mode string, _ time.Time, _ time.Duration) {
		if mode == "TRUNCATE" || mode == "RESTART" {
			resets++
			return
		}
		if mode != "PASSIVE" || bulkStarted {
			return
		}
		var err error
		bulkStarted, err = s.BeginGenerationBulkLoad(77)
		require.NoError(t, err)
		require.True(t, bulkStarted)
	}
	t.Cleanup(func() { walCheckpointCallObserver = nil })
	defer func() {
		if bulkStarted {
			require.NoError(t, s.EndGenerationBulkLoadFor(77))
		}
	}()
	require.NoError(t, s.writeMu.LockContext(t.Context()))
	writer := newWALReclaimWriterCredit(s, time.Now())
	defer writer.release()
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	_, err := writer.passive(ctx, db, 25*time.Millisecond, false)
	require.ErrorIs(t, err, errWALCheckpointDeferredBulk)
	require.Zero(t, resets)
	require.True(t, bulkStarted)
	require.True(t, writer.held, "refused readmission must have reacquired ownership before inspecting bulkConn")
	// Let the bulk fixture's deferred close acquire the writer.
	writer.release()
}

type reclaimSyncStall struct {
	tls              uintptr
	entered, release chan struct{}
	once             sync.Once
}

var reclaimSyncStallState atomic.Pointer[reclaimSyncStall]
var reclaimSyncOriginals sync.Map // io_methods table -> its original xSync
var reclaimSyncWrapper = func(tls *libc.TLS, pFile uintptr, flags int32) int32 {
	methods := fileAt(pFile).FpMethods
	original, ok := reclaimSyncOriginals.Load(methods)
	if !ok {
		panic("missing original VFS xSync")
	}
	if state := reclaimSyncStallState.Load(); state != nil && uintptr(unsafe.Pointer(tls)) == state.tls {
		state.once.Do(func() { close(state.entered); <-state.release })
	}
	fp := original.(uintptr)
	return (*(*func(*libc.TLS, uintptr, int32) int32)(unsafe.Pointer(&struct{ uintptr }{fp})))(tls, pFile, flags)
}

// TestMain calls this once, before m.Run. installWALCopyPause closes its probe
// connection before returning; no live store can dispatch xSync while the
// shared tables are changed. Fixtures only install TLS-specific stall state.
func installReclaimSyncWrapper() {
	installWALCopyPause()
	seen := make(map[uintptr]bool)
	for _, methods := range []uintptr{walCopyMethods.Load(), vfsWALMethods.Load()} {
		if methods == 0 || seen[methods] {
			continue
		}
		seen[methods] = true
		m := ioMethodsAt(methods)
		if m.FxSync != 0 {
			reclaimSyncOriginals.Store(methods, m.FxSync)
			m.FxSync = funcPointer(reclaimSyncWrapper)
		}
	}
}

func stallReclaimCheckpointSync(t *testing.T, db *sql.DB) (chan struct{}, chan struct{}) {
	t.Helper()
	conn, err := db.Conn(t.Context())
	require.NoError(t, err)
	key, ok := pausableConn(conn)
	require.NoError(t, conn.Close())
	require.True(t, ok, "checkpoint VFS connection is not observable")
	require.NotZero(t, walCopyMethods.Load(), "test VFS wrapper was not installed before the stores opened")
	state := &reclaimSyncStall{tls: key, entered: make(chan struct{}), release: make(chan struct{})}
	// Keep wrapper/original mappings permanent: a driver may already have
	// resolved the wrapper when a fixture ends. Only its TLS-specific stall
	// is removed, so unrelated connections always retain their real xSync.
	reclaimSyncStallState.Store(state)
	t.Cleanup(func() { reclaimSyncStallState.Store(nil) })
	return state.entered, state.release
}

func TestReclaimFinalBackfillPanicJoinsCreditTimer(t *testing.T) {
	s, db := finalBackfillFixture(t)
	walCheckpointCallObserver = func(mode string, _ time.Time, _ time.Duration) {
		if mode == "PASSIVE" {
			panic("credit seam panic")
		}
	}
	t.Cleanup(func() { walCheckpointCallObserver = nil })
	require.Panics(t, func() {
		s.writeMu.Lock()
		writer := newWALReclaimWriterCredit(s, time.Now())
		defer writer.release()
		_, _ = writer.passive(t.Context(), db, 25*time.Millisecond, false)
	})
	time.Sleep(50 * time.Millisecond) // the original timer must not unlock again
	require.True(t, s.writeMu.TryLock())
	s.writeMu.Unlock()
}
