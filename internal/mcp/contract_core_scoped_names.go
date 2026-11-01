package mcp

import (
	"context"

	"github.com/zzet/gortex/internal/graph"
)

var _ graph.ScopedOutlineNodeReader = (*contractCoreEdges)(nil)

// Both projections read nodes only. Their visibility remains the selected
// reader's; no raw adjacency capability crosses the contract-core mask.
func (r *contractCoreEdges) OutlineNodesByName(scope graph.NodeScope, name string) ([]*graph.Node, error) {
	return graph.FindNodesByNameScopedContext(r.ctx, r.Reader, name, scope)
}

func (r *contractCoreEdges) OutlineNodeIDsInScope(scope graph.NodeScope, ids []string) ([]string, error) {
	ctx := r.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if scoped, ok := graph.BindReadContext(r.Reader, ctx).(graph.ScopedOutlineNodeReader); ok {
		out, err := scoped.OutlineNodeIDsInScope(scope, ids)
		if err != nil {
			return nil, err
		}
		return out, ctx.Err()
	}
	nodes, err := r.GetNodesByIDsContext(ctx, ids)
	if err != nil {
		return nil, err
	}
	var out []string
	for id, node := range nodes {
		if node != nil && scope.Allows(node) {
			out = append(out, id)
		}
	}
	return out, ctx.Err()
}
