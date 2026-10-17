package graph

import "context"

// OverlayLayerContractFileNodesReader reads complete physical file nodes only.
// It must reject partial results and preserve every node field. Callers still
// validate the selected-view witness; this read does not certify source bytes.
type OverlayLayerContractFileNodesReader interface {
	LayerContractFileNodesContext(context.Context, string, []string) (map[string][]*Node, error)
}

// ContractFileNodesContext composes checked file nodes under the same identity
// and file masks as full contract projections, without hydrating any edges or
// canonical contract groups. Errorless file readers cannot certify absence.
func ContractFileNodesContext(ctx context.Context, reader Reader, repo string, paths []string) (map[string][]*Node, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var rows map[string][]*Node
	var err error
	switch r := reader.(type) {
	case *OverlaidView:
		rows, err = r.contractFileNodes(ctx, repo, paths)
	case *DeltaWriter:
		rows, err = r.view.contractFileNodes(ctx, repo, paths)
	case OverlayLayerContractFileNodesReader:
		rows, err = r.LayerContractFileNodesContext(ctx, repo, paths)
	case OverlayLayerContractProjectionReader:
		// Preserve checked compatibility for readers without the narrower
		// capability. Never fall through to an errorless file lookup.
		var projection ContractFileProjection
		projection, err = r.LayerContractFileProjectionContext(ctx, repo, paths)
		rows = projection.FileNodes
	default:
		if wrapper, ok := reader.(Unwrapper); ok && wrapper.Unwrap() != nil {
			return ContractFileNodesContext(ctx, wrapper.Unwrap(), repo, paths)
		}
		return nil, ErrContractProjectionUnsupported
	}
	if err != nil {
		return nil, err
	}
	count := 0
	for _, nodes := range rows {
		count += len(nodes)
		if count > ContractProjectionRowLimit {
			return nil, ErrContractProjectionLimit
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return rows, nil
}

func (v *OverlaidView) contractFileNodes(ctx context.Context, repo string, paths []string) (map[string][]*Node, error) {
	out := make(map[string][]*Node)
	if v.base != nil {
		base, err := ContractFileNodesContext(ctx, v.base, repo, paths)
		if err != nil {
			return nil, err
		}
		for path, nodes := range base {
			for _, node := range nodes {
				if v.baseNodeVisible(node) {
					out[path] = append(out[path], node)
				}
			}
		}
	}
	if v.layer != nil {
		// Physical layers must expose checked evidence even for an empty
		// request, so a closed or changed layer cannot certify absence.
		var own map[string][]*Node
		var err error
		if reader, ok := v.layer.(OverlayLayerContractFileNodesReader); ok {
			own, err = reader.LayerContractFileNodesContext(ctx, repo, paths)
		} else if reader, ok := v.layer.(OverlayLayerContractProjectionReader); ok {
			var projection ContractFileProjection
			projection, err = reader.LayerContractFileProjectionContext(ctx, repo, paths)
			own = projection.FileNodes
		} else {
			return nil, ErrContractProjectionUnsupported
		}
		if err != nil {
			return nil, err
		}
		for path, nodes := range own {
			out[path] = append(out[path], nodes...)
		}
	}
	return out, nil
}

func memoryContractFileNodes(ctx context.Context, paths []string, fileNodes func(string) []*Node) (map[string][]*Node, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	out := make(map[string][]*Node)
	count := 0
	for _, path := range uniqueNonEmpty(paths) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		rows := fileNodes(path)
		count += len(rows)
		if count > ContractProjectionRowLimit {
			return nil, ErrContractProjectionLimit
		}
		out[path] = rows
	}
	return out, ctx.Err()
}

func (g *Graph) LayerContractFileNodesContext(ctx context.Context, _ string, paths []string) (map[string][]*Node, error) {
	return memoryContractFileNodes(ctx, paths, g.GetFileNodes)
}
func (l *OverlayLayer) LayerContractFileNodesContext(ctx context.Context, _ string, paths []string) (map[string][]*Node, error) {
	return memoryContractFileNodes(ctx, paths, l.FileNodes)
}
func (l *deltaLayer) LayerContractFileNodesContext(ctx context.Context, repo string, paths []string) (map[string][]*Node, error) {
	return l.work.LayerContractFileNodesContext(ctx, repo, paths)
}
