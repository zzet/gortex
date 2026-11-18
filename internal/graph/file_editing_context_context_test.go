package graph

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

// The old contextual node capability must not be mistaken for a safe bundle
// layer: OverlaidView's legacy batch refetch still calls layer.NodeByID.
type editingContextUnsafeLayer struct{ OverlayLayerReader }

func (l *editingContextUnsafeLayer) NodeByID(string) *Node { panic("contextless physical node lookup") }
func (l *editingContextUnsafeLayer) GetNodesByIDsContext(context.Context, []string) (map[string]*Node, error) {
	return nil, nil
}

func TestEditingContextScopeRejectsContextualLookingLegacyLayer(t *testing.T) {
	layer := NewOverlayLayer()
	layer.MarkFile("a.go", false)
	view := NewOverlaidViewWithLayer(New(), &editingContextUnsafeLayer{layer})
	called := false
	err := WithFileEditingContextReader(t.Context(), view, func(context.Context, FileEditingContextRows) error { called = true; return nil })
	require.ErrorIs(t, err, ErrFileEditingContextUnsupported)
	require.False(t, called)
}

func TestEditingContextScopeMemoryOwnershipParity(t *testing.T) {
	base := New()
	base.AddNode(&Node{ID: "a.go::f", Kind: KindFunction, FilePath: "a.go", Meta: map[string]any{"signature": "base"}})
	base.AddNode(&Node{ID: "b.go::g", Kind: KindFunction, FilePath: "b.go", Meta: map[string]any{"full": "neighbor"}})
	base.AddEdge(&Edge{From: "b.go::g", To: "a.go::f", Kind: EdgeCalls, FilePath: "b.go"})
	layer := NewOverlayLayer()
	layer.MarkFile("a.go", false)
	layer.AddNode("a.go", &Node{ID: "a.go::f", Kind: KindFunction, FilePath: "a.go", Meta: map[string]any{"signature": "selected"}})
	view := NewOverlaidView(base, layer)
	want := FileEditingContextOf(view, "a.go", []NodeKind{KindFunction})
	require.NoError(t, WithFileEditingContextReader(t.Context(), view, func(_ context.Context, rows FileEditingContextRows) error {
		require.Equal(t, want, FileEditingContextOf(rows, "a.go", []NodeKind{KindFunction}))
		return nil
	}))
}
