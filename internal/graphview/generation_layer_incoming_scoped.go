package graphview

import (
	"context"

	"github.com/zzet/gortex/internal/graph"
)

func (l *GenerationLayer) FindIncomingSourcesScoped(ctx context.Context, targetIDs []string, kind graph.EdgeKind, limit int, scope graph.IncomingSourceScope, budget *graph.IncomingSourceBudget) (graph.BoundedIncomingSourceProjection, error) {
	return l.handle.FindIncomingSourcesScoped(ctx, targetIDs, kind, limit, scope, budget)
}

func (l *GenerationLayer) IncomingSourceNodeExists(ctx context.Context, id string, query graph.IncomingSourceNodeQuery) (bool, error) {
	return l.handle.IncomingSourceNodeExists(ctx, id, query)
}

var (
	_ graph.ScopedIncomingSourceReader = (*GenerationLayer)(nil)
	_ graph.IncomingSourceNodeChecker  = (*GenerationLayer)(nil)
)
