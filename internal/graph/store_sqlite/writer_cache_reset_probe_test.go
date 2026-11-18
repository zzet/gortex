package store_sqlite

import (
	"context"
	"database/sql"
	"testing"

	"github.com/stretchr/testify/require"
)

// An idle reset that completed and then saw its interrupt (a writer queued,
// or the hold ran out, after the log was already reset) is counted as a reset,
// not as a skipped attempt: the log is at its start. The reset connection's
// observer runs the TRUNCATE and then waits out the hold, so the reset's own
// statement returns the hold's error after a real reset.
func TestAnIdleResetInterruptedAfterItCompletedCountsAsAReset(t *testing.T) {
	t.Setenv("GORTEX_SQLITE_WAL_RECLAIM_MB", "0") // drive attempts directly
	prevObserver := walResetCheckpointConnObserver
	walResetCheckpointConnObserver = func(ctx context.Context, conn *sql.Conn) {
		if _, ok := ctx.Deadline(); ok {
			_, _ = checkpointWALOnceOn(context.Background(), conn, "TRUNCATE")
			<-ctx.Done()
		}
	}
	t.Cleanup(func() { walResetCheckpointConnObserver = prevObserver }) // after the store closes
	s, path := openWALReclaimStore(t)
	defer func() { _ = s.Close() }()
	seedWALChurnTable(t, s)
	lane := &fakeBuildLane{}
	lane.install(s)
	growWAL(t, s, 16)
	ckpt, err := openWALReclaimCheckpointDB(s.dbPath)
	require.NoError(t, err)
	defer func() { _ = ckpt.Close() }()
	resetsBefore := s.WALReclaimStats().Resets
	cfg := walReclaimConfig{thresholdBytes: 64 << 20, drainDeadline: defaultWALReclaimDrainDeadline, truncateBudget: walReclaimTruncateBudget, readerWait: defaultWALReclaimReaderWait}
	res := s.reclaimWALOnce(cfg, ckpt, path+"-wal")
	require.True(t, walLogIsReset(s.dbPath, path+"-wal"), "precondition: the log was reset")
	require.Equal(t, walReclaimReset, res.outcome, "counted as %s %q", res.outcome, res.reason)
	require.True(t, res.resetWriterFree)
	require.Equal(t, resetsBefore+1, s.WALReclaimStats().Resets)
}

// An idle reset that completed and then saw its interrupt (a writer queued,
// or the hold ran out, after the log was already reset) is counted as a reset,
// not as a skipped attempt: the log is at its start. The interrupt is injected
// after a real reset (walResetResultHook).
func TestAnIdleResetCompletedWithInjectedInterruptCountsAsAReset(t *testing.T) {
	t.Setenv("GORTEX_SQLITE_WAL_RECLAIM_MB", "0") // drive attempts directly
	s, path := openWALReclaimStore(t)
	defer func() { _ = s.Close() }()
	seedWALChurnTable(t, s)
	lane := &fakeBuildLane{}
	lane.install(s)
	growWAL(t, s, 16)
	ckpt, err := openWALReclaimCheckpointDB(s.dbPath)
	require.NoError(t, err)
	defer func() { _ = ckpt.Close() }()
	walResetResultHook = func(err error) error {
		if err == nil {
			return context.Canceled
		}
		return err
	}
	defer func() { walResetResultHook = nil }()
	resetsBefore := s.WALReclaimStats().Resets
	cfg := walReclaimConfig{thresholdBytes: 64 << 20, drainDeadline: defaultWALReclaimDrainDeadline, truncateBudget: walReclaimTruncateBudget, readerWait: defaultWALReclaimReaderWait}
	res := s.reclaimWALOnce(cfg, ckpt, path+"-wal")
	require.True(t, walLogIsReset(s.dbPath, path+"-wal"), "precondition: the log was reset")
	require.Equal(t, walReclaimReset, res.outcome, "counted as %s %q", res.outcome, res.reason)
	require.True(t, res.resetWriterFree)
	require.Equal(t, resetsBefore+1, s.WALReclaimStats().Resets)
}
