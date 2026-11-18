package graphview

import (
	"context"
	"github.com/zzet/gortex/internal/graph"
)

var _ graph.OverlayLayerConstantValueReader = (*GenerationLayer)(nil)

func (l *GenerationLayer) LayerConstantValueProjectionContext(ctx context.Context, ids []string, files []graph.ConstantFileKey) (graph.ConstantValueProjection, error) {
	if l.inputRevision != l.handle.PayloadInputRevision() {
		return graph.ConstantValueProjection{}, graph.ErrConstantProjectionStale
	}
	p, err := l.handle.ReadConstantValueProjectionContext(ctx, ids, files)
	if err != nil {
		return graph.ConstantValueProjection{}, err
	}
	for id, path := range p.Nodes {
		if l.isContextPath(path) {
			delete(p.Nodes, id)
		}
	}
	for id, row := range p.Rows {
		if l.isContextPath(row.FilePath) {
			delete(p.Rows, id)
		}
	}
	for key := range p.Files {
		if l.isContextPath(key.FilePath) {
			delete(p.Files, key)
		}
	}
	if l.inputRevision != l.handle.PayloadInputRevision() {
		return graph.ConstantValueProjection{}, graph.ErrConstantProjectionStale
	}
	return p, nil
}
