package store_sqlite

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// The mark accumulates a reader held at the read gate by a quiesce and a
// reader blocked on the read pool, and never goes back.
func TestReaderWaitMarkCountsGateAndPoolWaits(t *testing.T) {
	ctx := context.Background()
	s, err := openPristine(t, filepath.Join(t.TempDir(), "wait.sqlite"))
	require.NoError(t, err)
	defer func() { _ = s.Close() }()
	if s.readGate == nil {
		t.Skip("read gate disabled")
	}
	start := s.ReaderWaitMark()

	// Gate: close it, start a read, reopen after 60 ms.
	reopen, _, err := s.readGate.quiesce(ctx, time.Now().Add(time.Second))
	require.NoError(t, err)
	done := make(chan error, 1)
	go func() {
		var n int
		done <- s.db.QueryRowContext(ctx, `SELECT 1`).Scan(&n)
	}()
	time.Sleep(60 * time.Millisecond)
	reopen()
	require.NoError(t, <-done)
	afterGate := s.ReaderWaitMark()
	require.GreaterOrEqual(t, afterGate.Since(start), 50*time.Millisecond, "gate wait not counted: %+v", afterGate)
	require.Greater(t, afterGate.GateWaits, start.GateWaits)

	// Pool: hold every connection, one more reader waits 60 ms.
	limit := s.db.Stats().MaxOpenConnections
	require.Positive(t, limit)
	var held []*sql.Conn
	for i := 0; i < limit; i++ {
		c, err := s.db.Conn(ctx)
		require.NoError(t, err)
		held = append(held, c)
	}
	go func() {
		var n int
		done <- s.db.QueryRowContext(ctx, `SELECT 1`).Scan(&n)
	}()
	time.Sleep(60 * time.Millisecond)
	for _, c := range held {
		require.NoError(t, c.Close())
	}
	require.NoError(t, <-done)
	afterPool := s.ReaderWaitMark()
	require.GreaterOrEqual(t, afterPool.Since(afterGate), 50*time.Millisecond, "pool wait not counted: %+v", afterPool)
	require.Greater(t, afterPool.PoolWaits, afterGate.PoolWaits)
}
