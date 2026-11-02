package indexer

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

type deltaContextScopeBase struct {
	graph.Reader
	called bool
}

func (b *deltaContextScopeBase) WithFileEditingContextReader(ctx context.Context, fn func(context.Context, graph.FileEditingContextRows) error) error {
	b.called = true
	return fn(ctx, graph.New())
}

func TestDeltaWriterContextScopeCannotBorrowBasePastDelta(t *testing.T) {
	base := &deltaContextScopeBase{Reader: graph.New()}
	delta := graph.NewDeltaWriter(base, nil)
	delta.AddNode(&graph.Node{ID: "file.go::Current", FilePath: "file.go", Kind: graph.KindFunction})
	called := false
	err := graph.WithFileEditingContextReader(t.Context(), delta, func(context.Context, graph.FileEditingContextRows) error {
		called = true
		return nil
	})
	require.ErrorIs(t, err, graph.ErrFileEditingContextUnsupported)
	require.False(t, base.called, "handler-only scope cannot discard staged delta ownership")
	require.False(t, called)
	require.NotNil(t, delta.GetNode("file.go::Current"), "ordinary composed reads retain the delta")
}
