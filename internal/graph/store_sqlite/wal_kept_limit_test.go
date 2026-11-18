package store_sqlite

import (
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// Use the real read-mark refusal and a small WAL; reaching the cleanup path
// must not depend on the production 512MiB file-size heuristic.
func keptWALAfterExternalTruncate(t *testing.T) (*Store, *sql.DB) {
	t.Helper()
	previous := walShrinkInPlaceBytes
	walShrinkInPlaceBytes = 1
	t.Cleanup(func() { walShrinkInPlaceBytes = previous })
	s, checkpoint := finalBackfillFixture(t)
	reader, err := s.db.BeginTx(t.Context(), &sql.TxOptions{ReadOnly: true})
	require.NoError(t, err)
	t.Cleanup(func() { _ = reader.Rollback() })
	var payload string
	require.NoError(t, reader.QueryRow(`SELECT payload FROM wal_churn WHERE id = 1`).Scan(&payload))
	_, err = checkpointWALOnceOn(t.Context(), checkpoint, "PASSIVE")
	require.NoError(t, err)
	s.writeMu.Lock()
	_, err = s.resetWALForPressure(t.Context())
	s.writeMu.Unlock()
	require.ErrorIs(t, err, errSQLiteCheckpointIncomplete)
	require.NotNil(t, s.walLimitKept.Load())
	require.False(t, s.walShrinkPending.Load())
	require.EqualValues(t, -1, activeWriterJournalLimit(t, s))
	require.NoError(t, reader.Rollback())
	// An independent SQLite connection empties the file without the marker
	// write our in-place reset normally uses to signal its pending shrink.
	_, err = checkpointWALOnceOn(t.Context(), checkpoint, "TRUNCATE")
	require.NoError(t, err)
	info, err := os.Stat(s.dbPath + "-wal")
	require.NoError(t, err)
	require.Zero(t, info.Size())
	require.True(t, s.walShrinkNeeded(), "an emptied kept WAL still owes limit restoration")
	return s, checkpoint
}

func TestWALKeptLimitRestoredAfterExternalTruncate(t *testing.T) {
	s, checkpoint := keptWALAfterExternalTruncate(t)
	_, _, err := s.shrinkWAL(t.Context(), checkpoint)
	require.NoError(t, err)
	require.EqualValues(t, walShrinkKeepBytes, activeWriterJournalLimit(t, s))
	require.Nil(t, s.walLimitKept.Load())
	require.False(t, s.walShrinkPending.Load())
	require.False(t, s.walShrinkNeeded())
}

func TestWALKeptLimitRestorationFailureRetainsCleanup(t *testing.T) {
	s, checkpoint := keptWALAfterExternalTruncate(t)
	kept := s.walLimitKept.Load()
	// A closed pinned connection fails through database/sql without changing
	// its underlying pooled connection's pager limit. Cleanup must be retryable.
	closed, err := s.writerDB.Conn(t.Context())
	require.NoError(t, err)
	require.NoError(t, closed.Close())
	func() {
		s.writeMu.Lock()
		previous := s.bulkConn
		s.bulkConn = closed
		s.writeMu.Unlock()
		defer func() {
			s.writeMu.Lock()
			s.bulkConn = previous
			s.writeMu.Unlock()
		}()
		_, _, err = s.shrinkWAL(t.Context(), checkpoint)
	}()
	require.ErrorIs(t, err, sql.ErrConnDone)
	require.Same(t, kept, s.walLimitKept.Load(), "the failed setting lost its incarnation receipt")
	require.True(t, s.walShrinkPending.Load())
	require.True(t, s.walShrinkNeeded())
	require.EqualValues(t, -1, activeWriterJournalLimit(t, s))
	_, _, err = s.shrinkWAL(t.Context(), checkpoint)
	require.NoError(t, err)
	require.EqualValues(t, walShrinkKeepBytes, activeWriterJournalLimit(t, s))
	require.Nil(t, s.walLimitKept.Load())
	require.False(t, s.walShrinkPending.Load())
	require.False(t, s.walShrinkNeeded())
}

func TestWALKeptLogUnreadableFileIsNotEmpty(t *testing.T) {
	root := t.TempDir()
	s := &Store{storeCore: &storeCore{dbPath: filepath.Join(root, "store.sqlite")}}
	kept := &walReclaimFrontier{salt: [8]byte{1}}
	walPath := s.dbPath + "-wal"
	// A real symlink-loop error also works when tests run as root (chmod
	// cannot reliably produce EACCES there). Windows may lack symlink rights.
	t.Run("unexpected_stat_error", func(t *testing.T) {
		if err := os.Symlink(filepath.Base(walPath), walPath); err != nil {
			t.Skipf("symlink loop unavailable: %v", err)
		}
		defer func() { require.NoError(t, os.Remove(walPath)) }()
		_, err := os.Stat(walPath)
		require.Error(t, err)
		if os.IsNotExist(err) {
			t.Skip("platform classifies the loop as absence; no unexpected-stat fixture")
		}
		require.False(t, s.walKeptLogRestarted(kept), "a stat error was mistaken for an emptied WAL")
	})
	require.True(t, s.walKeptLogRestarted(kept), "an actually absent WAL has no incarnation to protect")
	require.NoError(t, os.WriteFile(walPath, []byte("unreadable header"), 0o600))
	require.False(t, s.walKeptLogRestarted(kept), "a non-empty, unreadable frontier is not proof of restart")
	require.NoError(t, os.Truncate(walPath, 0))
	require.True(t, s.walKeptLogRestarted(kept), "an actually empty WAL permits restoration")
}

func TestWALShrinkInterveningResetRetainsNewCleanup(t *testing.T) {
	previous := walShrinkInPlaceBytes
	walShrinkInPlaceBytes = 1
	t.Cleanup(func() { walShrinkInPlaceBytes = previous; walShrinkBeforeRestore = nil })
	s, checkpoint := finalBackfillFixture(t)
	walPath := s.dbPath + "-wal"
	const largeBytes = walShrinkKeepBytes + walShrinkMinSlice
	require.NoError(t, os.Truncate(walPath, largeBytes))
	s.writeMu.Lock()
	_, err := s.resetWALForReclaim(t.Context())
	s.writeMu.Unlock()
	require.NoError(t, err)
	intervened := false
	walShrinkBeforeRestore = func() {
		walShrinkBeforeRestore = nil
		intervened = true
		before, ok := readWALReclaimFrontier(s.dbPath)
		require.True(t, ok)
		// The last slice finished. A distinct real reset now restarts an
		// enlarged sparse file before the old shrink can restore its limit.
		s.writeMu.Lock()
		require.NoError(t, os.Truncate(walPath, largeBytes))
		_, err = s.resetWALForReclaim(t.Context())
		s.writeMu.Unlock()
		require.NoError(t, err)
		after, ok := readWALReclaimFrontier(s.dbPath)
		require.True(t, ok)
		require.NotEqual(t, before.salt, after.salt)
	}
	_, _, err = s.shrinkWAL(t.Context(), checkpoint)
	require.NoError(t, err)
	require.True(t, intervened)
	require.True(t, s.walShrinkPending.Load(), "the old shrink erased the newer reset's cleanup")
	require.EqualValues(t, -1, activeWriterJournalLimit(t, s))
	_, err = checkpointWALOnceOn(t.Context(), checkpoint, "PASSIVE")
	require.NoError(t, err)
	require.NoError(t, churnWriteOnce(s, 0))
	require.EqualValues(t, largeBytes, walFileSize(walPath), "a later commit truncated the new reset's file")
	_, _, err = s.shrinkWAL(t.Context(), checkpoint)
	require.NoError(t, err)
	require.EqualValues(t, walShrinkKeepBytes, activeWriterJournalLimit(t, s))
	require.False(t, s.walShrinkPending.Load())
	require.False(t, s.walShrinkNeeded())
}

// A driver error after the real marker commit must keep the error visible
// without discarding the physical reset's cleanup obligation.
func TestWALInPlaceResetNonContextErrorRetainsCleanup(t *testing.T) {
	previous := walShrinkInPlaceBytes
	walShrinkInPlaceBytes = 1
	t.Cleanup(func() { walShrinkInPlaceBytes = previous; walResetResultHook = nil })
	s, checkpoint := finalBackfillFixture(t)
	walPath := s.dbPath + "-wal"
	const largeBytes = walShrinkKeepBytes + walShrinkMinSlice
	require.NoError(t, os.Truncate(walPath, largeBytes))
	before := readWALWriteMark(s.dbPath)
	require.True(t, before.Valid)
	injected := errors.New("driver error after marker commit")
	observed := false
	walResetResultHook = func(err error) error {
		observed = true
		require.NoError(t, err, "the real reset must complete before error injection")
		return injected
	}
	s.writeMu.Lock()
	_, err := s.resetWALForReclaim(t.Context())
	s.writeMu.Unlock()
	walResetResultHook = nil
	require.True(t, observed)
	require.ErrorIs(t, err, injected, "a non-context error must remain observable")
	require.True(t, walInPlaceResetCompleted(before, readWALWriteMark(s.dbPath)))
	require.Zero(t, s.WALReclaimStats().ShrinkInPlaceResets, "the error must not become a success count")
	require.True(t, s.walShrinkPending.Load())
	require.EqualValues(t, -1, activeWriterJournalLimit(t, s))
	_, err = checkpointWALOnceOn(t.Context(), checkpoint, "PASSIVE")
	require.NoError(t, err)
	require.NoError(t, churnWriteOnce(s, 0))
	require.EqualValues(t, largeBytes, walFileSize(walPath), "a later commit truncated the restarted file")
	_, _, err = s.shrinkWAL(t.Context(), checkpoint)
	require.NoError(t, err)
	require.EqualValues(t, walShrinkKeepBytes, activeWriterJournalLimit(t, s))
	require.False(t, s.walShrinkPending.Load())
}
