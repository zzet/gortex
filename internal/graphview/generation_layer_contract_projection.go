package graphview

import (
	"context"
	"github.com/zzet/gortex/internal/graph"
)

var _ graph.OverlayLayerContractProjectionReader = (*GenerationLayer)(nil)

func (l *GenerationLayer) LayerContractFileProjectionContext(ctx context.Context, repo string, paths []string) (graph.ContractFileProjection, error) {
	if err := l.checkContractInputRevision(ctx); err != nil {
		return graph.ContractFileProjection{}, err
	}
	p, err := l.handle.LayerContractFileProjectionContext(ctx, repo, paths)
	if err != nil {
		return graph.ContractFileProjection{}, err
	}
	if err := l.checkContractInputRevision(ctx); err != nil {
		return graph.ContractFileProjection{}, err
	}
	return graph.FilterContractFileProjection(p, l.servesNode, l.servesEdge), nil
}
func (l *GenerationLayer) LayerContractIDProjectionContext(ctx context.Context, ids []string) (graph.ContractFileProjection, error) {
	if err := l.checkContractInputRevision(ctx); err != nil {
		return graph.ContractFileProjection{}, err
	}
	p, err := l.handle.LayerContractIDProjectionContext(ctx, ids)
	if err != nil {
		return graph.ContractFileProjection{}, err
	}
	if err := l.checkContractInputRevision(ctx); err != nil {
		return graph.ContractFileProjection{}, err
	}
	return graph.FilterContractFileProjection(p, l.servesNode, l.servesEdge), nil
}

func (l *GenerationLayer) checkContractInputRevision(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	revision, err := l.handle.PayloadInputRevisionContext(ctx)
	if err != nil {
		return err
	}
	if l.inputRevision != revision {
		return graph.ErrContractProjectionStale
	}
	return nil
}
