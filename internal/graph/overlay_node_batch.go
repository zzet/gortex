package graph

import "context"

// The existing adjacency indexes also name the highest immutable node owner:
// latestFile covers the ID's file, latestOut covers detached node identities.
// No rows or new lifetime authority are cached here.
func (b *overlayAdjacencyBatch) nodeLayer(id string) *OverlayLayer {
	if id == "" {
		return nil
	}
	owner := -1
	if index, ok := b.latestFile[overlayIDPath(id)]; ok {
		owner = index
	}
	if index, ok := b.latestOut[id]; ok && index > owner {
		owner = index
	}
	if owner < 0 {
		return nil
	}
	return b.layers[owner]
}

func (b *overlayAdjacencyBatch) partitionNodes(ctx context.Context, ids []string) (map[string]*Node, []string, error) {
	own := make(map[string]*Node, len(ids))
	baseIDs := make([]string, 0, len(ids))
	seen := make(map[string]struct{}, len(ids))
	for index, id := range ids {
		if index&127 == 0 {
			if err := ctx.Err(); err != nil {
				return nil, nil, err
			}
		}
		if id == "" {
			continue
		}
		if _, duplicate := seen[id]; duplicate {
			continue
		}
		seen[id] = struct{}{}
		if layer := b.nodeLayer(id); layer != nil {
			if node := layer.NodeByID(id); node != nil {
				own[id] = node
			}
		} else {
			baseIDs = append(baseIDs, id)
		}
	}
	return own, baseIDs, nil
}

func (b *overlayAdjacencyBatch) nodes(ctx context.Context, ids []string) (map[string]*Node, error) {
	own, baseIDs, err := b.partitionNodes(ctx, ids)
	if err != nil {
		return nil, err
	}
	if len(baseIDs) == 0 || b.base == nil {
		return own, nil
	}
	var base map[string]*Node
	if contextual, ok := b.base.(contextNodesByIDsReader); ok {
		base, err = contextual.GetNodesByIDsContext(ctx, baseIDs)
	} else {
		base = b.base.GetNodesByIDs(baseIDs)
	}
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	for id, node := range base {
		if node != nil {
			own[id] = node
		}
	}
	return own, nil
}

func (b *overlayAdjacencyBatch) nodeKinds(ctx context.Context, ids []string) (map[string]NodeKindRow, error) {
	own, baseIDs, err := b.partitionNodes(ctx, ids)
	if err != nil {
		return nil, err
	}
	// Keep the contextual base call even for an empty partition. An unsupported
	// or invalid selected capability must not turn into an authoritative empty.
	base, err := GetNodeKindsByIDsContext(ctx, b.base, baseIDs)
	if err != nil {
		return nil, err
	}
	rows := make(map[string]NodeKindRow, len(own)+len(base))
	for id, node := range own {
		rows[id] = nodeKindRow(node)
	}
	for id, row := range base {
		rows[id] = row
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return rows, nil
}
