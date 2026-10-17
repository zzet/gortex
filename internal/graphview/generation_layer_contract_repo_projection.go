package graphview

import (
	"context"

	"github.com/zzet/gortex/internal/graph"
)

var _ graph.OverlayLayerContractRepoProjectionReader = (*GenerationLayer)(nil)

// LayerContractRepoProjectionContext keeps repository contract hydration on
// the selected physical generation and its original ownership snapshot.
func (l *GenerationLayer) LayerContractRepoProjectionContext(ctx context.Context, repo string) (graph.ContractFileProjection, error) {
	if err := l.checkContractInputRevision(ctx); err != nil {
		return graph.ContractFileProjection{}, err
	}
	p, err := l.handle.LayerContractRepoProjectionContext(ctx, repo)
	if err != nil {
		return graph.ContractFileProjection{}, err
	}
	if err := l.checkContractInputRevision(ctx); err != nil {
		return graph.ContractFileProjection{}, err
	}
	return graph.FilterContractFileProjection(p, l.servesNode, l.servesEdge), nil
}
