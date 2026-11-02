package mcp

import (
	"context"
	"testing"

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
