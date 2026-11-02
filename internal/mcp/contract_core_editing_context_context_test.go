package mcp

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

type editingContextStalledScope struct{ graph.Reader }

func (s editingContextStalledScope) WithFileEditingContextReader(ctx context.Context, _ func(context.Context, graph.FileEditingContextRows) error) error {
	<-ctx.Done()
	return ctx.Err()
}

type editingContextPartialScope struct{ *graph.Graph }

func (s editingContextPartialScope) WithFileEditingContextReader(ctx context.Context, fn func(context.Context, graph.FileEditingContextRows) error) error {
	if err := fn(ctx, s.Graph); err != nil {
		return err
	}
	return graph.ErrContractProjectionLimit
}
func TestEditingContextBundleDeadlineAndPartialRefusal(t *testing.T) {
	got, err := fileEditingContextContext(t.Context(), editingContextStalledScope{}, "a.go", nil)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.Nil(t, got)
	g := graph.New()
	g.AddNode(&graph.Node{ID: "a.go::f", Kind: graph.KindFunction, FilePath: "a.go"})
	got, err = fileEditingContextContext(t.Context(), editingContextPartialScope{g}, "a.go", nil)
	require.ErrorIs(t, err, graph.ErrContractProjectionLimit)
	require.Nil(t, got, "refused complete-looking rows must not escape")
	got, err = fileEditingContextContext(t.Context(), struct{ graph.Reader }{g}, "a.go", nil)
	require.ErrorIs(t, err, graph.ErrFileEditingContextUnsupported)
	require.Nil(t, got, "no legacy bundle or engine fallback")
}
func TestEditingContextBundleContractFilterFullParity(t *testing.T) {
	store := newContractCoreEditingStore(t)
	wrapped := newContractCoreEdges(store, t.Context(), nil)
	kinds := []graph.NodeKind{graph.KindFunction, graph.KindMethod}
	want := graph.FileEditingContextOf(wrapped, "repo/a.go", kinds)
	got, err := fileEditingContextContext(t.Context(), wrapped, "repo/a.go", kinds)
	require.NoError(t, err)
	require.Equal(t, want, got)
}

// A projection deadline limits work that may earn current-file freshness. It
// cannot erase useful canonical rows from a separately labelled pending reply.
func TestPendingFileStoreScopeDoesNotReviveProjectionAllowance(t *testing.T) {
	g := graph.New()
	g.AddNode(&graph.Node{ID: "a.go::f", Kind: graph.KindFunction, FilePath: "a.go", StartLine: 3, EndLine: 4})
	expired := time.Now().Add(-time.Second)
	allowance := &projectedSourceAllowance{tool: "get_file_summary"}
	allowance.attempts.Store(2)
	ctx := context.WithValue(t.Context(), projectedSourceDeadlineKey{}, expired)
	ctx = context.WithValue(ctx, projectedSourceAllowanceKey{}, allowance)
	calls := 0
	err := withFileStoreRows(ctx, g, func(rows graph.FileEditingContextRows) error {
		calls++
		require.Len(t, rows.GetFileNodes("a.go"), 1)
		require.Equal(t, expired, ctx.Value(projectedSourceDeadlineKey{}))
		require.False(t, allowance.claim(), "canonical rows cannot authorize a third projection attempt")
		return nil
	})
	require.NoError(t, err)
	require.Equal(t, 1, calls)
	require.EqualValues(t, 2, allowance.attempts.Load())
	for _, cancelled := range []bool{false, true} {
		var caller context.Context
		var cancel context.CancelFunc
		want := context.DeadlineExceeded
		if cancelled {
			caller, cancel = context.WithCancel(ctx)
			cancel()
			want = context.Canceled
		} else {
			caller, cancel = context.WithDeadline(ctx, time.Now().Add(-time.Millisecond))
		}
		err := withFileStoreRows(caller, g, func(graph.FileEditingContextRows) error {
			t.Error("a finished caller must not read canonical rows")
			return nil
		})
		require.ErrorIs(t, err, want)
		cancel()
	}
}
