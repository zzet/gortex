package store_sqlite

import (
	"context"
	"database/sql"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// The reader outlives the aggregate two-second writer credit. Previously the
// pinned bulk connection kept its five-second busy timeout and waited for this
// reader's three-second departure while edits queued behind the writer.
func TestWALBulkCompletionResetCapsPinnedConnectionBusyTimeout(t *testing.T) {
	t.Setenv("GORTEX_SQLITE_WAL_AUTOCHECKPOINT_PAGES", "0")
	s, checkpoint := finalBackfillFixture(t)
	engaged, err := s.BeginGenerationBulkLoad(77)
	require.NoError(t, err)
	require.True(t, engaged)
	defer func() { require.NoError(t, s.EndGenerationBulkLoadFor(77)) }()
	reader, err := s.db.BeginTx(t.Context(), &sql.TxOptions{ReadOnly: true})
	require.NoError(t, err)
	var payload string
	require.NoError(t, reader.QueryRow(`SELECT payload FROM wal_churn WHERE id = 1`).Scan(&payload))
	var previous int64
	require.NoError(t, s.bulkConn.QueryRowContext(t.Context(), `PRAGMA busy_timeout`).Scan(&previous))
	require.Greater(t, previous, walReclaimMaxWriterHold.Milliseconds())
	entered := make(chan struct{})
	stop := make(chan struct{})
	released := make(chan error, 1)
	var once sync.Once
	var observedBusy int64
	walResetCheckpointConnObserver = func(ctx context.Context, conn *sql.Conn) {
		require.Same(t, s.bulkConn, conn)
		require.NoError(t, conn.QueryRowContext(ctx, `PRAGMA busy_timeout`).Scan(&observedBusy))
		once.Do(func() { close(entered) })
	}
	defer func() { walResetCheckpointConnObserver = nil }()
	go func() {
		select {
		case <-entered:
			select {
			case <-time.After(3 * time.Second):
			case <-stop:
			}
		case <-stop:
		}
		released <- reader.Rollback()
	}()
	joined := false
	defer func() {
		close(stop)
		if !joined {
			<-released
		}
	}()
	res := &walReclaimResult{urgent: true, bulkCompletion: true, leaseOverride: true}
	cfg := walReclaimConfig{thresholdBytes: 1, readerWait: time.Second, truncateBudget: walReclaimTruncateBudget}
	err = s.reclaimWALInLane(context.Background(), cfg, checkpoint, res)
	require.Error(t, err, "the reader must still pin the reset at the hold deadline")
	select {
	case <-entered:
	default:
		t.Fatal("bulk completion did not reach the reset connection")
	}
	require.False(t, res.openGate)
	require.Less(t, res.writerHold, walReclaimMaxWriterHold+500*time.Millisecond)
	require.LessOrEqual(t, observedBusy, walReclaimMaxWriterHold.Milliseconds())
	require.True(t, s.writeMu.TryLock(), "the refused reset retained the writer")
	s.writeMu.Unlock()
	var restored int64
	require.NoError(t, s.bulkConn.QueryRowContext(t.Context(), `PRAGMA busy_timeout`).Scan(&restored))
	require.Equal(t, previous, restored)
	require.NoError(t, <-released)
	joined = true
}
