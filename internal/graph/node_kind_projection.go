package graph

import (
	"context"
)

// NodeKindRow is a read-only structural projection. Location/namespace are
// retained solely for selected layer and repository visibility, never Meta.
type NodeKindRow struct {
	Kind       NodeKind
	FilePath   string
	RepoPrefix string
}

type NodeKindsByIDsReader interface {
	GetNodeKindsByIDsContext(context.Context, []string) (map[string]NodeKindRow, error)
}

type OverlayLayerNodeKindsByIDsReader interface {
	LayerNodeKindsByIDsContext(context.Context, []string) (map[string]NodeKindRow, error)
}

// GetNodeKindsByIDsContext reads only requested identities. Checked selected
// capabilities preserve their selected reader; legacy readers retain bounded
// point-read compatibility, with cancellation checks and no whole-store scan.
func GetNodeKindsByIDsContext(ctx context.Context, reader Reader, ids []string) (map[string]NodeKindRow, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	ids = uniqueNonEmpty(ids)
	if reader == nil {
		return nil, nil
	}
	if checked, ok := reader.(NodeKindsByIDsReader); ok {
		rows, err := checked.GetNodeKindsByIDsContext(ctx, ids)
		if err != nil {
			return nil, err
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return rows, nil
	}
	var nodes map[string]*Node
	if checked, ok := reader.(contextNodesByIDsReader); ok {
		var err error
		nodes, err = checked.GetNodesByIDsContext(ctx, ids)
		if err != nil {
			return nil, err
		}
	} else {
		nodes = reader.GetNodesByIDs(ids)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	out := make(map[string]NodeKindRow, len(nodes))
	for id, node := range nodes {
		if node != nil {
			out[id] = nodeKindRow(node)
		}
	}
	return out, nil
}

func nodeKindRow(node *Node) NodeKindRow {
	return NodeKindRow{Kind: node.Kind, FilePath: node.FilePath, RepoPrefix: node.RepoPrefix}
}

func (g *Graph) GetNodeKindsByIDsContext(ctx context.Context, ids []string) (map[string]NodeKindRow, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	out := make(map[string]NodeKindRow)
	for _, id := range uniqueNonEmpty(ids) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		shard := g.shardFor(id)
		shard.mu.RLock()
		if node := shard.nodes[id]; node != nil {
			out[id] = nodeKindRow(node)
		}
		shard.mu.RUnlock()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

func (v *OverlaidView) GetNodeKindsByIDsContext(ctx context.Context, ids []string) (map[string]NodeKindRow, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var ownIDs, baseIDs []string
	for _, id := range uniqueNonEmpty(ids) {
		if v.overlayOwnsIdentity(id) {
			ownIDs = append(ownIDs, id)
		} else {
			baseIDs = append(baseIDs, id)
		}
	}
	own := make(map[string]NodeKindRow)
	if v.layer != nil {
		var err error
		// Even an empty partition validates checked physical ownership.
		own, err = layerNodeKinds(ctx, v.layer, ownIDs)
		if err != nil {
			return nil, err
		}
	}
	base, err := GetNodeKindsByIDsContext(ctx, v.base, baseIDs)
	if err != nil {
		return nil, err
	}
	if v.layer != nil {
		if _, err := layerNodeKinds(ctx, v.layer, nil); err != nil {
			return nil, err
		}
	}
	if own == nil {
		own = make(map[string]NodeKindRow)
	}
	for id, row := range base {
		own[id] = row
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return own, nil
}

func (dw *DeltaWriter) GetNodeKindsByIDsContext(ctx context.Context, ids []string) (map[string]NodeKindRow, error) {
	return dw.view.GetNodeKindsByIDsContext(ctx, ids)
}

// layerNodeKinds keeps legacy layer point-read semantics without pretending it
// has checked physical provenance. GenerationLayer supplies the checked trait;
// existing memory/test layers remain bounded and cancellation-aware.
func layerNodeKinds(ctx context.Context, layer OverlayLayerReader, ids []string) (map[string]NodeKindRow, error) {
	if checked, ok := layer.(OverlayLayerNodeKindsByIDsReader); ok {
		return checked.LayerNodeKindsByIDsContext(ctx, ids)
	}
	return memoryLayerNodeKinds(ctx, layer, ids)
}

func memoryLayerNodeKinds(ctx context.Context, layer OverlayLayerReader, ids []string) (map[string]NodeKindRow, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	out := make(map[string]NodeKindRow)
	for _, id := range uniqueNonEmpty(ids) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if node := layer.NodeByID(id); node != nil {
			out[id] = nodeKindRow(node)
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

func (l *OverlayLayer) LayerNodeKindsByIDsContext(ctx context.Context, ids []string) (map[string]NodeKindRow, error) {
	return memoryLayerNodeKinds(ctx, l, ids)
}
func (l *deltaLayer) LayerNodeKindsByIDsContext(ctx context.Context, ids []string) (map[string]NodeKindRow, error) {
	return memoryLayerNodeKinds(ctx, l, ids)
}
