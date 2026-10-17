package store_sqlite

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// A writer blocked on another connection's write lock sleeps in SQLite's busy
// handler; the gauge counts that sleep as busy time, and a read lap shows its
// read transactions.
func TestSQLiteSleepGaugeCountsBusyHandlerSleeps(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "busy.sqlite")
	s, err := openPristine(t, path)
	require.NoError(t, err)
	defer func() { _ = s.Close() }()
	require.True(t, sqliteSleepMark().Installed, "the gauge is installed at Open")

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
	require.Positive(t, split.ReadTxn)
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
