package mcp

import (
	"context"

	"github.com/zzet/gortex/internal/graph"
)

var _ graph.CallAncestorReader = (*contractCoreEdges)(nil)

func (r *contractCoreEdges) CallAncestorsContext(ctx context.Context, ids []string) ([]graph.CallAncestorRow, error) {
	if backend, ok := r.Reader.(graph.FilteredCallAncestorReader); ok {
		rows, err := backend.CallAncestorsFilteredContext(ctx, ids, r.deadCodeFilter())
		recordContractCoreReadError(r.ctx, err)
		return rows, err
	}
	return graph.CallAncestorsOf(ctx, r, ids)
}
