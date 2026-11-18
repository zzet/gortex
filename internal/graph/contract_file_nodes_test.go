package graph

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

type checkedFileNodesLayer struct {
	*OverlayLayer
	rows  map[string][]*Node
	err   error
	calls int
}

func (l *checkedFileNodesLayer) LayerContractFileNodesContext(context.Context, string, []string) (map[string][]*Node, error) {
	l.calls++
	return l.rows, l.err
}
func (l *checkedFileNodesLayer) LayerContractFileProjectionContext(context.Context, string, []string) (ContractFileProjection, error) {
	panic("node-only read hydrated file projection")
}
func (l *checkedFileNodesLayer) LayerContractIDProjectionContext(context.Context, []string) (ContractFileProjection, error) {
	panic("node-only read hydrated canonical groups")
}

type checkedFileProjectionOnly struct {
	Reader
	rows ContractFileProjection
	err  error
}

func (r checkedFileProjectionOnly) LayerContractFileProjectionContext(context.Context, string, []string) (ContractFileProjection, error) {
	return r.rows, r.err
}
func (r checkedFileProjectionOnly) LayerContractIDProjectionContext(context.Context, []string) (ContractFileProjection, error) {
	panic("node-only fallback hydrated IDs")
}

func TestContractFileNodesComposesMasksWithoutGroupHydration(t *testing.T) {
	base := New()
	old := &Node{ID: "a.go::f", Name: "f", Kind: KindFunction, FilePath: "a.go", Meta: map[string]any{"signature": "old"}}
	gone := &Node{ID: "gone.go::f", Name: "f", Kind: KindFunction, FilePath: "gone.go"}
	base.AddBatch([]*Node{old, gone}, nil)
	current := *old
	current.Meta = map[string]any{"signature": "current", "nested": map[string]any{"value": "kept"}}
	layer := &checkedFileNodesLayer{OverlayLayer: NewOverlayLayer(), rows: map[string][]*Node{"a.go": {&current}}}
	layer.MarkFile("a.go", false)
	layer.AddNode("a.go", &current)
	layer.MarkFile("gone.go", true)
	view := NewOverlaidViewWithLayer(base, layer)
	for _, reader := range []Reader{view, NewDeltaWriter(view, nil)} {
		rows, err := ContractFileNodesContext(t.Context(), reader, "", []string{"a.go", "gone.go", "missing.go"})
		require.NoError(t, err)
		require.Equal(t, []*Node{&current}, rows["a.go"])
		require.Empty(t, rows["gone.go"])
		require.Empty(t, rows["missing.go"])
	}
	require.Equal(t, 2, layer.calls)
	removed := NewOverlayLayer()
	removed.MarkRemoved(current.Name, current.ID)
	rows, err := ContractFileNodesContext(t.Context(), NewOverlaidView(view, removed), "", []string{"a.go"})
	require.NoError(t, err)
	require.Empty(t, rows["a.go"])
}

func TestContractFileNodesCheckedCompatibilityAndFailures(t *testing.T) {
	node := &Node{ID: "a.go", Kind: KindFile, FilePath: "a.go"}
	projection := ContractFileProjection{FileNodes: map[string][]*Node{"a.go": {node}}}
	fallback := checkedFileProjectionOnly{Reader: New(), rows: projection}
	rows, err := ContractFileNodesContext(t.Context(), fallback, "", []string{"a.go"})
	require.NoError(t, err)
	require.Equal(t, projection.FileNodes, rows)
	cause := errors.New("checked file nodes failed")
	for _, reader := range []Reader{
		checkedFileProjectionOnly{Reader: New(), rows: projection, err: cause},
		NewOverlaidViewWithLayer(New(), &checkedFileNodesLayer{OverlayLayer: NewOverlayLayer(), rows: projection.FileNodes, err: cause}),
	} {
		rows, err = ContractFileNodesContext(t.Context(), reader, "", []string{"a.go"})
		require.ErrorIs(t, err, cause)
		require.Nil(t, rows, "a partial checked result cannot certify presence or absence")
	}
	rows, err = ContractFileNodesContext(t.Context(), struct{ Reader }{New()}, "", []string{"a.go"})
	require.ErrorIs(t, err, ErrContractProjectionUnsupported)
	require.Nil(t, rows)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	rows, err = ContractFileNodesContext(ctx, fallback, "", nil)
	require.ErrorIs(t, err, context.Canceled)
	require.Nil(t, rows)
}

func TestContractFileNodesBoundsTheComposedAnswer(t *testing.T) {
	nodes := make([]*Node, ContractProjectionRowLimit)
	for i := range nodes {
		nodes[i] = &Node{ID: "a.go::f", FilePath: "a.go"}
	}
	base := checkedFileProjectionOnly{Reader: New(), rows: ContractFileProjection{FileNodes: map[string][]*Node{"a.go": nodes}}}
	own := &checkedFileNodesLayer{OverlayLayer: NewOverlayLayer(), rows: map[string][]*Node{"b.go": {{ID: "b.go", FilePath: "b.go"}}}}
	// Use a physical reader for the base: the bound is across both selected layers.
	rows, err := ContractFileNodesContext(t.Context(), NewOverlaidViewWithLayer(base, own), "", []string{"a.go", "b.go"})
	require.ErrorIs(t, err, ErrContractProjectionLimit)
	require.Nil(t, rows)
}
