package store_sqlite

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// A connection used only with background contexts retains SQLite's native
// busy handler, whose VFS sleeps remain observable. The Go handler installed
// for cancellable calls is outside this gauge's coverage.
func TestSQLiteSleepGaugeCountsBusyHandlerSleeps(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "busy.sqlite")
	s, err := openPristine(t, path)
	require.NoError(t, err)
	defer func() { _ = s.Close() }()
	require.True(t, sqliteSleepMark().Installed, "the gauge is installed at Open")
	require.True(t, sqliteSleepMark().VFSOnly, "installation must not claim complete busy-wait coverage")

	holder, err := sql.Open("sqlite", sqliteWriterDSN(path))
	require.NoError(t, err)
	defer holder.Close()
	holder.SetMaxOpenConns(1)
	conn, err := holder.Conn(ctx)
	require.NoError(t, err)
	defer conn.Close()
	_, err = conn.ExecContext(ctx, `BEGIN IMMEDIATE`)
	require.NoError(t, err)

	before := s.ReaderWaitMark()
	done := make(chan error, 1)
	go func() {
		s.writeMu.Lock()
		defer s.writeMu.Unlock()
		_, err := s.writerDB.Exec(`CREATE TABLE IF NOT EXISTS busy_probe(x)`)
		done <- err
	}()
	time.Sleep(300 * time.Millisecond)
	_, err = conn.ExecContext(ctx, `COMMIT`)
	require.NoError(t, err)
	require.NoError(t, <-done)
	var n int
	require.NoError(t, s.db.QueryRow(`SELECT count(*) FROM sqlite_schema`).Scan(&n))
	split := s.ReaderWaitMark().Split(before)
	t.Logf("split: %+v", split)
	require.Positive(t, split.BusySleeps)
	require.GreaterOrEqual(t, split.BusySleep, 150*time.Millisecond, "the busy handler's sleep was not counted")
	require.Positive(t, split.ReadTxns)
	// Windows can finish every short transaction in the same wall-clock
	// cell. Completed transaction count above proves real read work; only
	// the deliberately blocked busy-handler interval has a positive floor.
	require.GreaterOrEqual(t, split.ReadTxn, time.Duration(0))
}

// The wrapper classifies whole-millisecond delays as the busy handler's and
// the rest as the WAL read-lock retry loop's.
func TestSQLiteSleepGaugeClassifiesDelays(t *testing.T) {
	installSQLiteSleepGauge()
	before := sqliteSleepMark()
	sqliteSleepWrapper(nil, 0, 1000)
	sqliteSleepWrapper(nil, 0, 39)
	sqliteSleepWrapper(nil, 0, 1)
	after := sqliteSleepMark()
	require.Equal(t, int64(1), after.BusyCalls-before.BusyCalls)
	require.Equal(t, int64(2), after.WALRetryCalls-before.WALRetryCalls)
	require.GreaterOrEqual(t, after.BusyNanos-before.BusyNanos, int64(time.Millisecond))
}

func TestReaderWaitSplitKeepsCompletedReadsAtZeroClockPrecision(t *testing.T) {
	before := ReaderWaitMark{ReadTxns: 7, ReadTxnNanos: 1000}
	after := ReaderWaitMark{ReadTxns: 10, ReadTxnNanos: 1000}
	split := after.Split(before)
	require.EqualValues(t, 3, split.ReadTxns)
	require.Zero(t, split.ReadTxn, "equal clock cells remain measured zero, not fabricated positive time")
}
