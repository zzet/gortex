package store_sqlite

import (
	"context"
	"database/sql"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func rowCounterReaderReservationFixture(t *testing.T) *Store {
	t.Helper()
	s, err := openPristine(t, filepath.Join(t.TempDir(), "reader-reservation.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	s.stopCheckpointLoop()
	s.db.SetMaxOpenConns(4)
	nodes, edges := rowCounterFixture("repo/a.go", 20)
	s.AddBatch(nodes, edges)
	return s
}

func startRowCounterReaderAdmission(t *testing.T, s *Store, ctx context.Context, cancel context.CancelFunc) (<-chan struct{}, func(), <-chan error) {
	t.Helper()
	at, proceed, joined := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var entered, released sync.Once
	release := func() { released.Do(func() { close(proceed) }) }
	rowCountersBeforeReadConnHook = func() { entered.Do(func() { close(at) }); <-proceed }
	done := make(chan error, 1)
	go func() { defer close(joined); done <- s.EnsureRowCounters(ctx) }()
	t.Cleanup(func() {
		release()
		cancel()
		select {
		case <-joined:
			rowCountersBeforeReadConnHook = nil
		case <-time.After(5 * time.Second):
			t.Error("cancelled row-counter admission did not join")
		}
	})
	return at, release, done
}

func pinCounterReaderPool(t *testing.T, s *Store) []*sql.Conn {
	t.Helper()
	conns := make([]*sql.Conn, 0, s.db.Stats().MaxOpenConnections)
	for range s.db.Stats().MaxOpenConnections {
		conn, err := s.db.Conn(t.Context())
		require.NoError(t, err)
		t.Cleanup(func() { _ = conn.Close() })
		conns = append(conns, conn)
	}
	return conns
}

func releaseCounterReaderPool(conns []*sql.Conn) {
	for _, conn := range conns {
		_ = conn.Close()
	}
}

func TestRowCountersReserveSeedReaderBeforeWriter(t *testing.T) {
	s := rowCounterReaderReservationFixture(t)
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	at, releaseBarrier, done := startRowCounterReaderAdmission(t, s, ctx, cancel)
	select {
	case <-at:
	case <-ctx.Done():
		t.Fatal("seed reader admission never reached the barrier")
	}
	pins := pinCounterReaderPool(t, s)
	releaseBarrier()
	// Pinning the pool must not monopolize the writer. Require an actual
	// foreground SQL commit, not only admission of an empty gate probe.
	writeCtx, stop := context.WithTimeout(t.Context(), 100*time.Millisecond)
	started := time.Now()
	gateErr := s.writeMu.LockContext(writeCtx)
	var writeErr error
	if gateErr == nil {
		_, writeErr = s.writerDB.ExecContext(writeCtx, `UPDATE nodes SET name = 'during_seed_admission' WHERE view_gen = 0`)
		s.writeMu.Unlock()
	}
	elapsed := time.Since(started)
	stop()
	releaseCounterReaderPool(pins)
	require.NoError(t, <-done)
	require.NoError(t, gateErr, "seed reader admission waited on the read pool while holding the writer")
	require.NoError(t, writeErr)
	require.Less(t, elapsed, 100*time.Millisecond)
	requireCountersExact(t, s, 0)
	t.Logf("foreground_gate_and_sql=%s", elapsed)
}

func TestRowCountersCancelQueuedSeedReaderReleasesResources(t *testing.T) {
	s := rowCounterReaderReservationFixture(t)
	ctx, cancel := context.WithCancel(t.Context())
	at, releaseBarrier, done := startRowCounterReaderAdmission(t, s, ctx, cancel)
	select {
	case <-at:
	case <-time.After(time.Second):
		t.Fatal("seed reader admission never reached the barrier")
	}
	pins := pinCounterReaderPool(t, s)
	before := s.db.Stats().WaitCount
	releaseBarrier()
	until := time.Now().Add(time.Second)
	for s.db.Stats().WaitCount == before && time.Now().Before(until) {
		time.Sleep(time.Millisecond)
	}
	queued := s.db.Stats().WaitCount > before
	gateFree := s.writeMu.TryLock()
	if gateFree {
		s.writeMu.Unlock()
	}
	cancel()
	err := <-done
	rowCountersBeforeReadConnHook = nil // the owned operation has joined
	releaseCounterReaderPool(pins)
	require.True(t, queued, "seed reader admission never entered the real pool wait")
	require.True(t, gateFree, "queued seed reader held the writer")
	require.ErrorIs(t, err, context.Canceled)
	require.Zero(t, s.db.Stats().InUse)
	// Cancellation leaves the same store usable and the counters retryable.
	require.NoError(t, s.EnsureRowCounters(t.Context()))
	requireCountersExact(t, s, 0)
}
