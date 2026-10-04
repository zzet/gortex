package mcp

import (
	"context"

	"github.com/zzet/gortex/internal/graph"
)

// Node and name reads retain the selected reader's checked, bounded lookup
// capabilities. Only adjacency is filtered here; forwarding traversal engines
// would allow their paths to bypass the contract-derived edge filter.
func (r *contractCoreEdges) FindNodesByNameContext(ctx context.Context, name string) ([]*graph.Node, error) {
	return graph.FindNodesByNameContext(ctx, r.Reader, name)
}

func (r *contractCoreEdges) FindNodesByNameContainingContext(ctx context.Context, substr string, limit int) ([]*graph.Node, error) {
	return graph.FindNodesByNameContainingContext(ctx, r.Reader, substr, limit)
}

// Expose compact filtered lookup only when the selected reader has it. An
// outer overlay otherwise retains its adaptive bounded legacy fetch policy.
type contractCoreFilteredNames struct{ *contractCoreEdges }

func newContractCoreEdges(reader graph.Reader, ctx context.Context, ids map[string]bool) graph.Reader {
	core := &contractCoreEdges{Reader: reader, ctx: ctx, contractIDs: ids}
	if _, ok := reader.(graph.FilteredContainingNameReader); ok {
		return &contractCoreFilteredNames{core}
	}
	return core
}

func (r *contractCoreFilteredNames) FindNodesByNameContainingFilteredContext(ctx context.Context, substr string, limit int, filter graph.NameSearchFilter) ([]*graph.Node, error) {
	return graph.FindNodesByNameContainingFilteredContext(ctx, r.Reader, substr, limit, filter)
}

func (r *contractCoreEdges) GetNodeContext(ctx context.Context, id string) (*graph.Node, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if contextual, ok := r.Reader.(interface {
		GetNodeContext(context.Context, string) (*graph.Node, error)
	}); ok {
		node, err := contextual.GetNodeContext(ctx, id)
		if err != nil {
			return nil, err
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return node, nil
	}
	node := r.Reader.GetNode(id)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return node, nil
}

func (r *contractCoreEdges) GetNodesByIDsContext(ctx context.Context, ids []string) (map[string]*graph.Node, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if contextual, ok := r.Reader.(interface {
		GetNodesByIDsContext(context.Context, []string) (map[string]*graph.Node, error)
	}); ok {
		nodes, err := contextual.GetNodesByIDsContext(ctx, ids)
		if err != nil {
			return nil, err
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return nodes, nil
	}
	nodes := r.Reader.GetNodesByIDs(ids)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return nodes, nil
}

// Preserve compact identity scans through the exact selected reader. The graph
// helpers retain legacy fallbacks without widening the selected node set, and
// the scanner owns its borrowed page and callback lock-release contract.
func (r *contractCoreEdges) ScanNodeSearchKeys(ctx context.Context, pageSize int, yield func([]graph.NodeSearchKey) bool) error {
	return graph.ScanNodeSearchKeys(ctx, r.Reader, pageSize, yield)
}

func (r *contractCoreEdges) AllNodesLight() []*graph.Node {
	return graph.AllNodesLight(r.Reader)
}
