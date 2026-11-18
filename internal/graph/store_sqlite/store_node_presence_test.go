package store_sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

func TestNodePresenceSQLChunksGenerationAndNoMetadataDecode(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "presence.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	var nodes []*graph.Node
	var ids []string
	for i := 0; i < lookupChunkSize+3; i++ {
		id := fmt.Sprintf("repo/file.go::N%d", i)
		kind := graph.KindFunction
		switch i {
		case 0:
			kind = ""
		case 1:
			kind = "future-kind"
		}
		nodes = append(nodes, &graph.Node{ID: id, Kind: kind, FilePath: "repo/file.go", RepoPrefix: "repo"})
		ids = append(ids, id)
	}
	s.AddBatch(nodes, nil)
	positive := s.AtGeneration(7)
	positive.AddNode(&graph.Node{ID: ids[0], Kind: graph.KindContract, FilePath: "other/f.go", RepoPrefix: "other"})
	positive.AddNode(&graph.Node{ID: "only-positive", Kind: ""})
	// Neither metadata decoding nor a kind predicate may participate in
	// existence: corrupt metadata, empty kinds and future kinds remain present.
	_, err = s.writerDB.Exec(`UPDATE nodes SET meta = ?`, []byte{0xff, 0xfe})
	require.NoError(t, err)
	input := append(append([]string(nil), ids...), "", ids[0], ids[0], "missing", "only-positive")
	rows, err := s.GetNodePresenceByIDsContext(t.Context(), input)
	require.NoError(t, err)
	require.Len(t, rows, len(ids))
	for _, id := range ids {
		require.Contains(t, rows, id)
	}
	require.NotContains(t, rows, "only-positive")
	rows, err = positive.GetNodePresenceByIDsContext(t.Context(), input)
	require.NoError(t, err)
	require.Equal(t, map[string]struct{}{ids[0]: {}, "only-positive": {}}, rows)
	rows, err = s.GetNodePresenceByIDsContext(t.Context(), []string{"", ""})
	require.NoError(t, err)
	require.NotNil(t, rows)
	require.Empty(t, rows)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	rows, err = s.GetNodePresenceByIDsContext(ctx, input)
	require.ErrorIs(t, err, context.Canceled)
	require.Nil(t, rows)

	// Cancellation after the first completed chunk must not expose that
	// chunk as a usable answer to the entire request.
	ctx, cancel = context.WithCancel(t.Context())
	defer cancel()
	var timing graph.NodeKindReadTiming
	ctx = graph.WithNodeKindReadObserver(ctx, func(v graph.NodeKindReadTiming) {
		timing.Add(v)
		cancel()
	})
	rows, err = s.GetNodePresenceByIDsContext(ctx, input)
	require.ErrorIs(t, err, context.Canceled)
	require.Nil(t, rows)
	require.Equal(t, 1, timing.Batches)
	require.Equal(t, lookupChunkSize, timing.Rows)
	require.NoError(t, s.Close())
	rows, err = s.GetNodePresenceByIDsContext(t.Context(), input)
	require.Error(t, err)
	require.Nil(t, rows)
}

func TestNodePresenceReadTimingParityAndErrors(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "timing.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	s.AddNode(&graph.Node{ID: "repo/f.go::N", Kind: ""})
	ids := []string{"repo/f.go::N", "missing", "repo/f.go::N", ""}
	plain, err := s.GetNodePresenceByIDsContext(t.Context(), ids)
	require.NoError(t, err)
	var timing graph.NodeKindReadTiming
	ctx := graph.WithNodeKindReadObserver(t.Context(), timing.Add)
	observed, err := s.GetNodePresenceByIDsContext(ctx, ids)
	require.NoError(t, err)
	require.Equal(t, plain, observed)
	require.Equal(t, 1, timing.Batches)
	require.Equal(t, 2, timing.InputIDs)
	require.Equal(t, 1, timing.Rows)
	require.GreaterOrEqual(t, timing.QueryStart, timing.Gate)
	require.GreaterOrEqual(t, timing.Total, timing.QueryStart+timing.Drain)
	require.NoError(t, s.Close())
	observed, err = s.GetNodePresenceByIDsContext(ctx, ids)
	require.Error(t, err)
	require.Nil(t, observed)
	require.Equal(t, 1, timing.Errors)
}

func TestNodePresenceReadTimingPoolCancellationReturnsNoPartialRows(t *testing.T) {
	connector := &nodeKindRetryConnector{}
	connector.calls.Store(1) // This control exercises pool cancellation, not retry.
	db := sql.OpenDB(gatedConnector{inner: connector, gate: newSQLiteReadGate()})
	t.Cleanup(func() { _ = db.Close() })
	s := &Store{storeCore: &storeCore{db: db}}
	s.db.SetMaxOpenConns(1)
	held, err := s.db.Conn(t.Context())
	require.NoError(t, err)
	t.Cleanup(func() { _ = held.Close() })
	var timing graph.NodeKindReadTiming
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	ctx = graph.WithNodeKindReadObserver(ctx, timing.Add)
	rows, err := s.GetNodePresenceByIDsContext(ctx, []string{"repo/f.go::N"})
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.Nil(t, rows)
	require.Positive(t, timing.PreDriver)
	require.Equal(t, timing.QueryStart, timing.PreDriver)
	require.Equal(t, 1, timing.Errors)
	require.NoError(t, held.Close())
	require.Zero(t, s.db.Stats().InUse)
	rows, err = s.GetNodePresenceByIDsContext(t.Context(), []string{"repo/f.go::N"})
	require.NoError(t, err)
	require.Equal(t, map[string]struct{}{"repo/f.go::N": {}}, rows)
}
