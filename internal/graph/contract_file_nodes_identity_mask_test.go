package graph

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestContractFileNodesRetainsUnanchoredIdentitiesUnderFileMasks(t *testing.T) {
	for _, kind := range []NodeKind{KindType, KindContract} {
		for _, deleted := range []bool{false, true} {
			t.Run(string(kind)+map[bool]string{false: "_replace", true: "_delete"}[deleted], func(t *testing.T) {
				base := New()
				anchored := &Node{ID: "a.go::F", Name: "F", Kind: KindFunction, FilePath: "a.go"}
				global := &Node{ID: "canonical:" + string(kind), Name: "shared", Kind: kind, FilePath: "a.go", Meta: map[string]any{"nested": map[string]any{"full": "retained"}}}
				base.AddBatch([]*Node{anchored, global}, nil)
				layer := NewOverlayLayer()
				layer.MarkFile("a.go", deleted)
				current := *anchored
				current.Meta = map[string]any{"signature": "current"}
				if !deleted {
					layer.AddNode("a.go", &current)
				}
				view := NewOverlaidViewWithLayer(base, layer)
				require.Equal(t, global, view.GetNode(global.ID), "the file claim does not own this canonical identity")
				rows, err := ContractFileNodesContext(t.Context(), view, "", []string{"a.go"})
				require.NoError(t, err)
				want := []*Node{global}
				if !deleted {
					want = append(want, &current)
				}
				require.ElementsMatch(t, want, rows["a.go"], "checked file reads must preserve the same selected identity set")
				// An explicit identity tombstone still removes the otherwise surviving
				// global row; build it before handing the immutable layer to the view.
				removed := NewOverlayLayer()
				removed.MarkFile("a.go", true)
				removed.MarkRemoved(global.Name, global.ID)
				rows, err = ContractFileNodesContext(t.Context(), NewOverlaidViewWithLayer(base, removed), "", []string{"a.go"})
				require.NoError(t, err)
				require.Empty(t, rows["a.go"])
			})
		}
	}
}

func TestContractFileNodesFileMaskRetainsCheckedBaseErrors(t *testing.T) {
	cause := errors.New("checked base unavailable")
	base := checkedFileProjectionOnly{Reader: New(), err: cause}
	layer := NewOverlayLayer()
	layer.MarkFile("a.go", true)
	view := NewOverlaidViewWithLayer(base, layer)
	rows, err := ContractFileNodesContext(t.Context(), view, "", []string{"a.go"})
	require.ErrorIs(t, err, cause)
	require.Nil(t, rows)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	rows, err = ContractFileNodesContext(ctx, view, "", []string{"a.go"})
	require.ErrorIs(t, err, context.Canceled)
	require.Nil(t, rows)
}
