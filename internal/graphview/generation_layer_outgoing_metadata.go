package graphview

import (
	"context"

	"github.com/zzet/gortex/internal/graph"
)

// LayerOutEdgesWithMetadataContext is physical-layer evidence for checked
// selected-view composition. Context-path rows never claim outgoing evidence.
func (l *GenerationLayer) LayerOutEdgesWithMetadataContext(ctx context.Context, ids []string, limit int) (map[string][]*graph.Edge, bool, error) {
	if err := l.checkContractInputRevision(ctx); err != nil {
		return nil, false, err
	}
	rows, truncated, err := l.handle.GetOutEdgesByNodeIDsWithMetadataContext(ctx, ids, limit)
	if err != nil {
		return nil, false, err
	}
	if err := l.checkContractInputRevision(ctx); err != nil {
		return nil, false, err
	}
	result := make(map[string][]*graph.Edge, len(rows))
	for id, edges := range rows {
		for _, edge := range edges {
			if l.servesEdge(edge) {
				result[id] = append(result[id], edge)
			}
		}
	}
	return result, truncated, nil
}
