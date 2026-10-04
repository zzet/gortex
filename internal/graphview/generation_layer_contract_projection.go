package graphview

import (
	"context"
	"github.com/zzet/gortex/internal/graph"
)

var _ graph.OverlayLayerContractProjectionReader = (*GenerationLayer)(nil)

func (l *GenerationLayer) LayerContractFileProjectionContext(ctx context.Context, repo string, paths []string) (graph.ContractFileProjection, error) {
	p, err := l.handle.LayerContractFileProjectionContext(ctx, repo, paths)
	if err != nil {
		return graph.ContractFileProjection{}, err
	}
	return graph.FilterContractFileProjection(p, l.servesNode, l.servesEdge), nil
}
func (l *GenerationLayer) LayerContractIDProjectionContext(ctx context.Context, ids []string) (graph.ContractFileProjection, error) {
	p, err := l.handle.LayerContractIDProjectionContext(ctx, ids)
	if err != nil {
		return graph.ContractFileProjection{}, err
	}
	return graph.FilterContractFileProjection(p, l.servesNode, l.servesEdge), nil
}
