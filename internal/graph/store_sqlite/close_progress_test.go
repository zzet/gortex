package store_sqlite

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// A close reports its progress for the stop path and ends with its phase;
// a close that copied enough frames records its per-frame cost, and the next
// estimate uses the measured cost instead of the default.
func TestCloseReportsProgressAndLearnsItsRate(t *testing.T) {
	t.Setenv("GORTEX_SQLITE_WAL_RECLAIM_MB", "0")
	prevMin, prevInterval := closeRateMinFrames, closeProgressInterval
	closeRateMinFrames, closeProgressInterval = 1000, 20*time.Millisecond
	t.Cleanup(func() { closeRateMinFrames, closeProgressInterval = prevMin, prevInterval })

	s, path := openWALReclaimStore(t)
	seedWALChurnTable(t, s)
	// A pinned reader keeps the periodic PASSIVE from backfilling, so the
	// close has a real backlog to copy.
	pin, err := s.db.BeginTx(context.Background(), &sql.TxOptions{ReadOnly: true})
	require.NoError(t, err)
	var n int
	require.NoError(t, pin.QueryRow(`SELECT count(*) FROM wal_churn`).Scan(&n))
	growWAL(t, s, 40)
	require.NoError(t, pin.Rollback())
	estimateBefore, pending := s.CloseCheckpointEstimate()
	require.Greater(t, pending, int64(1000))
	require.Equal(t, closeCheckpointEstimateAt(pending, closeCheckpointPerFrame), estimateBefore, "no close measured yet: the default cost")

	started := time.Now()
	require.NoError(t, s.Close())
	took := time.Since(started)

	p, ok := ReadCloseProgress(path)
	require.True(t, ok, "the close wrote no progress report")
	t.Logf("progress: %+v (close took %s)", p, took)
	require.Equal(t, "done", p.Phase)
	require.Equal(t, pending, p.PendingFrames)
	require.Equal(t, pending, p.FramesCopiedEstimate)
	require.Positive(t, p.LastProgressUnixNano)

	learned := closePerFrame(path)
	t.Logf("learned per-frame cost %s (default %s)", learned, closeCheckpointPerFrame)
	require.NotEqual(t, closeCheckpointPerFrame, learned, "the measured close was not recorded")
	require.LessOrEqual(t, learned, max(took/time.Duration(pending), closeRateFloor)+time.Microsecond)
	require.Equal(t, closeCheckpointEstimateAt(pending, learned), closeCheckpointEstimateAt(pending, closePerFrame(path)))
}

// The recalibrated default: the live 08:16:07 close (264,431 pending frames
// in 50.8 s) is now estimated within 20 % of what it took; the old 10 µs
// estimate was 17.6 s.
func TestCloseEstimateMatchesTheMeasuredLiveClose(t *testing.T) {
	est := closeCheckpointEstimate(264_431) - closeCheckpointBaseBudget
	t.Logf("copy estimate for the live close: %s (measured 50.8 s)", est)
	require.InDelta(t, 50.8, est.Seconds(), 0.2*50.8)
}
