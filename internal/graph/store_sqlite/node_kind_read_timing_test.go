package store_sqlite

import (
	"context"
	"database/sql"
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
	require.GreaterOrEqual(t, timing.Total, timing.QueryStart+timing.Drain)
	require.Zero(t, s.db.Stats().InUse)
	require.NoError(t, s.Close())
	got, err = s.GetNodeKindsByIDsContext(ctx, ids)
	require.Error(t, err)
	require.Nil(t, got)
	require.Equal(t, 1, timing.Errors)
}

func TestNodeKindReadTimingPoolCancellationReturnsNoPartialRows(t *testing.T) {
	connector := &nodeKindRetryConnector{}
	connector.calls.Store(1) // This control does not inject a retry.
	db := sql.OpenDB(gatedConnector{inner: connector, gate: newSQLiteReadGate()})
	t.Cleanup(func() { _ = db.Close() })
	s := &Store{storeCore: &storeCore{db: db}}
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
	require.Positive(t, timing.PreDriver)
	require.Equal(t, timing.QueryStart, timing.PreDriver)
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
	var measured nodeKindDriverTiming
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	ctx = context.WithValue(ctx, nodeKindGateTimingKey{}, &measured)
	require.ErrorIs(t, conn.enter(ctx), context.DeadlineExceeded)
	require.Positive(t, measured.gate)
	require.False(t, conn.entered.Load())
	// A request without the hook must not add to this request's counter.
	before := measured.gate
	other, stop := context.WithCancel(t.Context())
	stop()
	require.ErrorIs(t, conn.enter(other), context.Canceled)
	require.Equal(t, before, measured.gate)
}
