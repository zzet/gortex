package graphview

import (
	"context"

	"github.com/zzet/gortex/internal/graph"
)

func (l *GenerationLayer) LayerNodeKindsByIDsContext(ctx context.Context, ids []string) (map[string]graph.NodeKindRow, error) {
	if err := l.checkContractInputRevision(ctx); err != nil {
		return nil, err
	}
	rows, err := l.handle.GetNodeKindsByIDsContext(ctx, ids)
	if err != nil {
		return nil, err
	}
	for id, row := range rows {
		if l.isContextPath(row.FilePath) {
			delete(rows, id)
		}
	}
	if err := l.checkContractInputRevision(ctx); err != nil {
		return nil, err
	}
	return rows, nil
}
