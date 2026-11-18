package store_sqlite

import (
	"context"
	"database/sql"
	"os"
	"testing"

	"github.com/stretchr/testify/require"
)

func activeWriterJournalLimit(t *testing.T, s *Store) int64 {
	t.Helper()
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	conn, release, err := s.activeWriteConnLocked(t.Context())
	require.NoError(t, err)
	defer release()
	var limit int64
	require.NoError(t, conn.QueryRowContext(t.Context(), `PRAGMA journal_size_limit`).Scan(&limit))
	return limit
}

func TestWALResetExpiredContextCannotChangeJournalLimit(t *testing.T) {
	for _, bulk := range []bool{false, true} {
		t.Run(map[bool]string{false: "writer", true: "bulk"}[bulk], func(t *testing.T) {
			t.Setenv("GORTEX_SQLITE_WAL_AUTOCHECKPOINT_PAGES", "0")
			s, _ := finalBackfillFixture(t)
			if bulk {
				engaged, err := s.BeginGenerationBulkLoad(77)
				require.NoError(t, err)
				require.True(t, engaged)
				defer func() { require.NoError(t, s.EndGenerationBulkLoadFor(77)) }()
			}
			before := activeWriterJournalLimit(t, s)
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			s.writeMu.Lock()
			err := s.setWriterJournalLimitLocked(ctx, -1)
			s.writeMu.Unlock()
			require.ErrorIs(t, err, context.Canceled)
			require.Equal(t, before, activeWriterJournalLimit(t, s), "modernc must not prepare a mutating PRAGMA after cancellation")
		})
	}
}

// RESTART can copy every frame and still be refused by a reader's read mark.
// The next commit then naturally restarts the log. It must retain the large
// file for the slice shrink rather than applying a whole-file truncate.
func TestWALResetRefusedAfterFullCopyKeepsJournalLimitLifted(t *testing.T) {
	t.Setenv("GORTEX_SQLITE_WAL_AUTOCHECKPOINT_PAGES", "0")
	s, checkpoint := finalBackfillFixture(t)
	reader, err := s.db.BeginTx(t.Context(), &sql.TxOptions{ReadOnly: true})
	require.NoError(t, err)
	defer func() { _ = reader.Rollback() }()
	var payload string
	require.NoError(t, reader.QueryRow(`SELECT payload FROM wal_churn WHERE id = 1`).Scan(&payload))
	_, err = checkpointWALOnceOn(t.Context(), checkpoint, "PASSIVE")
	require.NoError(t, err)
	snap, ok := readWALIndexSnapshot(s.dbPath)
	require.True(t, ok)
	require.Positive(t, snap.MxFrame)
	require.Equal(t, snap.MxFrame, snap.NBackfill)
	// Sparse unused tail: exercise the real large-file policy without writing
	// hundreds of megabytes. SQLite's valid frames remain described by -shm.
	walPath := s.dbPath + "-wal"
	const largeBytes = 513 << 20
	require.NoError(t, os.Truncate(walPath, largeBytes))
	var result walCheckpointResult
	func() {
		s.writeMu.Lock()
		defer s.writeMu.Unlock()
		result, err = s.resetWALForPressure(t.Context()) // no lock wait under a writer credit
	}()
	require.ErrorIs(t, err, errSQLiteCheckpointIncomplete)
	require.Positive(t, result.Busy)
	require.Equal(t, result.WALFrames, result.CheckpointedFrames)
	require.False(t, s.walShrinkPending.Load(), "a refused reset must not be counted as completed")
	require.Zero(t, s.WALReclaimStats().ShrinkInPlaceResets)
	require.EqualValues(t, -1, activeWriterJournalLimit(t, s))
	require.NoError(t, reader.Rollback())
	require.NoError(t, churnWriteOnce(s, 0))
	require.EqualValues(t, largeBytes, walFileSize(walPath), "the next restart truncated the large file in one call")
	require.True(t, s.walShrinkNeeded())
}

func TestWALResetInterruptedBeforeRestartRestoresOnlyUncopiedLimit(t *testing.T) {
	for _, copied := range []bool{false, true} {
		t.Run(map[bool]string{false: "uncopied", true: "fully_copied"}[copied], func(t *testing.T) {
			t.Setenv("GORTEX_SQLITE_WAL_AUTOCHECKPOINT_PAGES", "0")
			s, checkpoint := finalBackfillFixture(t)
			if copied {
				_, err := checkpointWALOnceOn(t.Context(), checkpoint, "PASSIVE")
				require.NoError(t, err)
			}
			previous := walShrinkInPlaceBytes
			walShrinkInPlaceBytes = 1
			t.Cleanup(func() { walShrinkInPlaceBytes = previous; walResetCheckpointConnObserver = nil })
			before := readWALWriteMark(s.dbPath)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			observed := false
			walResetCheckpointConnObserver = func(context.Context, *sql.Conn) { observed = true; cancel() }
			var err error
			func() {
				s.writeMu.Lock()
				defer s.writeMu.Unlock()
				_, err = s.resetWALForReclaim(ctx)
			}()
			require.True(t, observed)
			require.ErrorIs(t, err, context.Canceled)
			require.Equal(t, before, readWALWriteMark(s.dbPath), "a canceled attempt must not restart the log")
			require.False(t, s.walShrinkPending.Load())
			require.Zero(t, s.WALReclaimStats().ShrinkInPlaceResets)
			want := int64(walShrinkKeepBytes)
			if copied {
				want = -1
			}
			require.Equal(t, want, activeWriterJournalLimit(t, s))
		})
	}
}
