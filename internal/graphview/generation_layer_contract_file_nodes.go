package graphview

import (
	"context"

	"github.com/zzet/gortex/internal/graph"
)

var _ graph.OverlayLayerContractFileNodesReader = (*GenerationLayer)(nil)

func (l *GenerationLayer) LayerContractFileNodesContext(ctx context.Context, repo string, paths []string) (map[string][]*graph.Node, error) {
	if err := l.checkContractInputRevision(ctx); err != nil {
		return nil, err
	}
	rows, err := l.handle.LayerContractFileNodesContext(ctx, repo, paths)
	if err != nil {
		return nil, err
	}
	if err := l.checkContractInputRevision(ctx); err != nil {
		return nil, err
	}
	out := make(map[string][]*graph.Node)
	for path, nodes := range rows {
		for _, node := range nodes {
			if l.servesNode(node) {
				out[path] = append(out[path], node)
			}
		}
	}
	return out, nil
}
