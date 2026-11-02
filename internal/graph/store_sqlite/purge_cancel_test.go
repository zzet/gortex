package store_sqlite

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestPurgeRepoCancellationReleasesQueuedWriter(t *testing.T) {
	s := openPurgeStore(t)
	seedRepoRows(t, s.writerDB, "gone")
	s.writeMu.Lock()
	locked := true
	unlock := func() {
		if locked {
			s.writeMu.Unlock()
			locked = false
		}
	}
	defer unlock()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- s.PurgeRepoContext(ctx, "gone") }()
	// Observe actual writer admission rather than canceling before the call.
	deadline := time.Now().Add(time.Second)
	for s.writeMu.waiting() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	require.Positive(t, s.writeMu.waiting())
	cancel()
	var result error
	var joined bool
	select {
	case result = <-done:
		joined = true
	case <-time.After(200 * time.Millisecond):
	}
	// Cleanup always joins actual work before Close, even on the expected RED.
	unlock()
	if !joined {
		select {
		case result = <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("tiny purge failed to join after releasing its owned gate")
		}
	}
	require.True(t, joined, "canceled purge remained queued until an unrelated writer released the gate")
	require.ErrorIs(t, result, context.Canceled)
	require.Equal(t, 1, countByPrefix(t, s.db, "nodes", "gone"), "canceled writer admission must preserve the orphan row")
}

func TestPurgeRepoCancellationReleasesPreflightConnectionWait(t *testing.T) {
	s := openPurgeStore(t)
	seedRepoRows(t, s.writerDB, "gone")
	s.writerDB.SetMaxOpenConns(1)
	conn, err := s.writerDB.Conn(context.Background())
	require.NoError(t, err)
	defer conn.Close()
	before := s.writerDB.Stats().WaitCount
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- s.PurgeRepoContext(ctx, "gone") }()
	require.Eventually(t, func() bool { return s.writerDB.Stats().WaitCount > before }, time.Second, time.Millisecond, "purge must actually wait for its owned preflight connection")
	cancel()
	select {
	case err = <-done:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(time.Second):
		t.Fatal("canceled purge did not release its preflight connection wait")
	}
	require.NoError(t, conn.Close())
	require.False(t, s.writeMu.held(), "canceled maintenance must release the writer before returning")
	require.Equal(t, 1, countByPrefix(t, s.db, "nodes", "gone"))
}

func TestPurgeRepoCancellationRollsBackSQLBeforeWriterRelease(t *testing.T) {
	installShadowDeleteFunction(t)
	s := openPurgeStore(t)
	seedRepoRows(t, s.writerDB, "gone")
	shadowDeleteTrigger(t, s, "nodes", "OLD.repo_prefix")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var reached atomic.Bool
	// The trigger runs inside the actual final DELETE, after edges and all
	// sidecar deletes. Cancellation must roll back that real partial transaction.
	shadowDeleteObserver.Store(&shadowDeleteProbe{fire: func(prefix string) {
		if prefix == "gone" {
			reached.Store(true)
			cancel()
		}
	}})
	err := s.PurgeRepoContext(ctx, "gone")
	require.True(t, reached.Load(), "test must cancel during actual purge SQL, not before admission")
	require.ErrorIs(t, err, context.Canceled)
	require.False(t, s.writeMu.held())
	require.Equal(t, 1, countByPrefix(t, s.db, "nodes", "gone"))
	for _, table := range prefixKeyedTables {
		require.Positive(t, countByPrefix(t, s.db, table, "gone"), "canceled purge must restore %s", table)
	}
	var edges int
	require.NoError(t, s.db.QueryRow(`SELECT COUNT(*) FROM edges WHERE from_id = 'gone::a.go::X'`).Scan(&edges))
	require.Equal(t, 1, edges)
	_, err = s.writerDB.Exec(`DROP TRIGGER test_shadow_delete`)
	require.NoError(t, err)
	require.NoError(t, s.PurgeRepo("gone"), "joined rollback must leave the writer usable")
	require.Zero(t, countByPrefix(t, s.db, "nodes", "gone"))
}
