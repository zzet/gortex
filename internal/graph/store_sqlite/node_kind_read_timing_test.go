package store_sqlite

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

func TestNodeKindReadTimingParityAndConnectionReturn(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "timing.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	s.AddNode(&graph.Node{ID: "repo/f.go::N", Kind: graph.KindFunction, FilePath: "repo/f.go", RepoPrefix: "repo"})
	ids := []string{"repo/f.go::N", "missing", "repo/f.go::N", ""}
	expected, err := s.GetNodeKindsByIDsContext(t.Context(), ids)
	require.NoError(t, err)
	var timing graph.NodeKindReadTiming
	ctx := graph.WithNodeKindReadObserver(t.Context(), timing.Add)
	got, err := s.GetNodeKindsByIDsContext(ctx, ids)
	require.NoError(t, err)
	require.Equal(t, expected, got)
	require.Equal(t, 1, timing.Batches)
	require.Equal(t, 2, timing.InputIDs)
	require.Equal(t, 1, timing.Rows)
	require.GreaterOrEqual(t, timing.QueryStart, timing.Gate)
	require.GreaterOrEqual(t, timing.Total, timing.Pool+timing.QueryStart+timing.Drain)
	require.Zero(t, s.db.Stats().InUse)
	require.NoError(t, s.Close())
	got, err = s.GetNodeKindsByIDsContext(ctx, ids)
	require.Error(t, err)
	require.Nil(t, got)
	require.Equal(t, 1, timing.Errors)
}

func TestNodeKindReadTimingPoolCancellationReturnsNoPartialRows(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "pool.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	s.db.SetMaxOpenConns(1)
	held, err := s.db.Conn(t.Context())
	require.NoError(t, err)
	var timing graph.NodeKindReadTiming
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	ctx = graph.WithNodeKindReadObserver(ctx, timing.Add)
	rows, err := s.GetNodeKindsByIDsContext(ctx, []string{"missing"})
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.Nil(t, rows)
	require.Positive(t, timing.Pool)
	require.Zero(t, timing.QueryStart)
	require.Equal(t, 1, timing.Errors)
	require.NoError(t, held.Close())
	require.Zero(t, s.db.Stats().InUse)
	_, err = s.GetNodeKindsByIDsContext(t.Context(), []string{"missing"})
	require.NoError(t, err)
}

func TestNodeKindReadTimingGateCancellationIsRequestLocal(t *testing.T) {
	gate := &sqliteReadGate{reopen: make(chan struct{})}
	gate.closed.Store(true)
	conn := &gatedConn{gate: gate}
	var measured time.Duration
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	ctx = context.WithValue(ctx, nodeKindGateTimingKey{}, &measured)
	require.ErrorIs(t, conn.enter(ctx), context.DeadlineExceeded)
	require.Positive(t, measured)
	require.False(t, conn.entered.Load())
	// A request without the hook must not add to this request's counter.
	before := measured
	other, stop := context.WithCancel(t.Context())
	stop()
	require.ErrorIs(t, conn.enter(other), context.Canceled)
	require.Equal(t, before, measured)
}
