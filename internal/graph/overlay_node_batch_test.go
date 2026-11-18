package graph

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
)

type nodeBatchBase struct {
	*Graph
	nodes, kinds int
	err          error
	cancel       context.CancelFunc
}

func (b *nodeBatchBase) GetNodesByIDsContext(ctx context.Context, ids []string) (map[string]*Node, error) {
	b.nodes++
	if b.cancel != nil {
		b.cancel()
	}
	if b.err != nil {
		return map[string]*Node{"partial": {ID: "partial"}}, b.err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return b.GetNodesByIDs(ids), nil
}
func (b *nodeBatchBase) GetNodeKindsByIDsContext(ctx context.Context, ids []string) (map[string]NodeKindRow, error) {
	b.kinds++
	if b.cancel != nil {
		b.cancel()
	}
	if b.err != nil {
		return map[string]NodeKindRow{"partial": {Kind: KindFunction}}, b.err
	}
	return b.Graph.GetNodeKindsByIDsContext(ctx, ids)
}

func TestImmutableNodeBatchDoesNotCopyPartitionsAtEveryLayer(t *testing.T) {
	base := &nodeBatchBase{Graph: New()}
	var ids []string
	for i := range 32 {
		id := fmt.Sprintf("canonical.go::Base%d", i)
		base.AddNode(&Node{ID: id, FilePath: "canonical.go", Kind: KindFunction, StartLine: i + 1})
		ids = append(ids, id)
	}
	var layers []OverlayLayerReader
	for i := range 128 {
		path := fmt.Sprintf("pending-%d.go", i)
		layer := NewOverlayLayer()
		layer.MarkFile(path, false)
		layer.AddNode(path, &Node{ID: path + "::Pending", FilePath: path, Kind: KindFunction, StartLine: 3})
		layers = append(layers, layer)
	}
	ids = append(ids, "pending-0.go::Pending", "pending-127.go::Pending")
	view := NewOverlaidViewWithLayers(base, layers)
	require.NotNil(t, view.adjacencyBatch)
	var nodes map[string]*Node
	var kinds map[string]NodeKindRow
	var readErr error
	for _, kind := range []string{"nodes", "kinds"} {
		t.Run(kind, func(t *testing.T) {
			base.nodes, base.kinds = 0, 0
			allocations := testing.AllocsPerRun(3, func() {
				if kind == "nodes" {
					nodes, readErr = view.GetNodesByIDsContext(context.Background(), ids)
				} else {
					kinds, readErr = view.GetNodeKindsByIDsContext(context.Background(), ids)
				}
			})
			require.NoError(t, readErr)
			if kind == "nodes" {
				require.Len(t, nodes, len(ids))
				require.Equal(t, 4, base.nodes)
			} else {
				require.Len(t, kinds, len(ids))
				require.Equal(t, 4, base.kinds)
			}
			require.Less(t, allocations, float64(128), "one immutable request batch must not allocate/copy per layer")
		})
	}
}

func TestImmutableNodeBatchPreservesContextAndBaseRefusals(t *testing.T) {
	for _, mode := range []string{"before", "during", "error"} {
		for _, kind := range []string{"nodes", "kinds"} {
			t.Run(mode+"/"+kind, func(t *testing.T) {
				base := &nodeBatchBase{Graph: New()}
				base.AddNode(&Node{ID: "base.go::Current", FilePath: "base.go", Kind: KindFunction})
				layer := NewOverlayLayer()
				layer.MarkFile("own.go", false)
				layer.AddNode("own.go", &Node{ID: "own.go::Own", FilePath: "own.go", Kind: KindFunction})
				view := NewOverlaidViewWithLayers(base, []OverlayLayerReader{layer, NewOverlayLayer()})
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				want := error(context.Canceled)
				switch mode {
				case "before":
					cancel()
				case "during":
					base.cancel = cancel
				case "error":
					want = errors.New("actual selected base refuses")
					base.err = want
				}
				ids := []string{"base.go::Current", "own.go::Own"}
				if kind == "nodes" {
					rows, err := view.GetNodesByIDsContext(ctx, ids)
					require.ErrorIs(t, err, want)
					require.Nil(t, rows)
				} else {
					rows, err := view.GetNodeKindsByIDsContext(ctx, ids)
					require.ErrorIs(t, err, want)
					require.Nil(t, rows)
				}
			})
		}
	}
	// Empty node-kind selection still asks the actual selected base capability.
	sentinel := errors.New("empty selected base refuses")
	base := &nodeBatchBase{Graph: New(), err: sentinel}
	view := NewOverlaidViewWithLayers(base, []OverlayLayerReader{NewOverlayLayer(), NewOverlayLayer()})
	rows, err := view.GetNodeKindsByIDsContext(context.Background(), nil)
	require.ErrorIs(t, err, sentinel)
	require.Nil(t, rows)
	require.Equal(t, 1, base.kinds)
}
