package store_sqlite

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

type editingContextScope interface {
	WithFileEditingContextReader(context.Context, func(context.Context, graph.FileEditingContextRows) error) error
}

func TestEditingContextSnapshotFullParityAndCancellation(t *testing.T) {
	s := newTestProjectionStore(t)
	nodes := []*graph.Node{
		{ID: "a.go", Kind: graph.KindFile, FilePath: "a.go", Meta: map[string]any{"payload": "file"}},
		{ID: "a.go::f", Kind: graph.KindFunction, FilePath: "a.go", Meta: map[string]any{"signature": "full"}},
		{ID: "b.go::g", Kind: graph.KindFunction, FilePath: "b.go", Meta: map[string]any{"payload": "neighbor"}},
	}
	s.AddBatch(nodes, []*graph.Edge{
		{From: "a.go", To: "external", Kind: graph.EdgeImports, FilePath: "a.go", Meta: map[string]any{"external": true}},
		{From: "a.go::f", To: "b.go::g", Kind: graph.EdgeCalls, FilePath: "a.go", Meta: map[string]any{"payload": "out"}},
		{From: "b.go::g", To: "a.go::f", Kind: graph.EdgeCalls, FilePath: "b.go", Meta: map[string]any{"payload": "in"}},
	})
	scope, ok := any(s).(editingContextScope)
	require.True(t, ok, "physical editing bundle must supply a cancellable coherent read scope")
	want := s.FileEditingContext("a.go", []graph.NodeKind{graph.KindFunction})
	require.NoError(t, scope.WithFileEditingContextReader(context.Background(), func(_ context.Context, r graph.FileEditingContextRows) error {
		require.Equal(t, want, graph.FileEditingContextOf(r, "a.go", []graph.NodeKind{graph.KindFunction}))
		return nil
	}))
	// Occupy the sole physical reader connection. The bundle must time out at
	// transaction admission rather than wait in an errorless legacy lookup.
	s.db.SetMaxOpenConns(1)
	conn, err := s.db.Conn(context.Background())
	require.NoError(t, err)
	defer func() { require.NoError(t, conn.Close()) }()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	called := false
	err = scope.WithFileEditingContextReader(ctx, func(context.Context, graph.FileEditingContextRows) error { called = true; return nil })
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.False(t, called)
}

func TestEditingContextSnapshotRefusesPartialAndReusesCore(t *testing.T) {
	s := newTestProjectionStore(t)
	s.AddBatch([]*graph.Node{{ID: "a.go::f", Kind: graph.KindFunction, FilePath: "a.go", Meta: map[string]any{"signature": "full"}}}, nil)
	scope, ok := any(s).(editingContextScope)
	require.True(t, ok)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	require.NoError(t, scope.WithFileEditingContextReader(ctx, func(ctx context.Context, _ graph.FileEditingContextRows) error {
		return scope.WithFileEditingContextReader(ctx, func(_ context.Context, r graph.FileEditingContextRows) error {
			require.NotNil(t, graph.FileEditingContextOf(r, "a.go", nil))
			return nil
		})
	}))
	_, err := s.writerDB.Exec(`UPDATE nodes SET meta=? WHERE id=?`, []byte("invalid metadata"), "a.go::f")
	require.NoError(t, err)
	var got *graph.FileEditingContextResult
	err = scope.WithFileEditingContextReader(context.Background(), func(_ context.Context, r graph.FileEditingContextRows) error {
		got = graph.FileEditingContextOf(r, "a.go", nil)
		return nil
	})
	require.Error(t, err, "an invalid full row cannot certify an empty or partial bundle")
	require.Nil(t, got)
}

func TestEditingContextSnapshotLimitAndRevisionRefusal(t *testing.T) {
	s := newTestProjectionStore(t)
	node := &graph.Node{ID: "a.go::f", Kind: graph.KindFunction, FilePath: "a.go"}
	neighbor := &graph.Node{ID: "b.go::g", Kind: graph.KindFunction, FilePath: "b.go", Meta: map[string]any{"signature": "before snapshot"}}
	require.NoError(t, s.AddBatchChecked([]*graph.Node{node, neighbor}, nil))
	err := s.WithFileEditingContextReader(t.Context(), func(ctx context.Context, rows graph.FileEditingContextRows) error {
		scope := ctx.Value(editingContextScopeKey{}).(*editingContextTransaction)
		scope.budget = 0
		require.Nil(t, rows.GetFileNodes("a.go"))
		return nil
	})
	require.ErrorIs(t, err, graph.ErrContractProjectionLimit)
	err = s.WithFileEditingContextReader(t.Context(), func(_ context.Context, rows graph.FileEditingContextRows) error {
		require.Len(t, rows.GetFileNodes("a.go"), 1)
		updated := *neighbor
		updated.Meta = map[string]any{"signature": "after snapshot"}
		require.NoError(t, s.AddBatchChecked([]*graph.Node{&updated}, nil))
		// This identity has not been hydrated or cached yet: observing the old
		// full payload proves the later query shares the established read tx.
		require.Equal(t, neighbor.Meta, rows.GetNodesByIDs([]string{neighbor.ID})[neighbor.ID].Meta)
		return nil
	})
	require.ErrorIs(t, err, graph.ErrContractProjectionStale, "a mutation across the snapshot must refuse publication")
}
