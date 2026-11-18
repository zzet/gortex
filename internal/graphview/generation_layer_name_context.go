package graphview

import (
	"context"

	"github.com/zzet/gortex/internal/graph"
)

// NodesByNameContext carries cancellation through the generation-bound store
// and applies the same visibility mask as NodesByName.
func (l *GenerationLayer) NodesByNameContext(ctx context.Context, name string) ([]*graph.Node, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if name == "" {
		return nil, nil
	}
	nodes, err := l.handle.FindNodesByNameContext(ctx, name)
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, ctxErr
	}
	if len(l.contextPaths) == 0 {
		return nodes, err
	}
	out := make([]*graph.Node, 0, len(nodes))
	for _, node := range nodes {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		if l.servesNode(node) {
			out = append(out, node)
		}
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, ctxErr
	}
	return out, err
}

// VisitNodesByNameContainingFoldedContext streams matching visible rows and
// returns cancellation or store errors. A false callback is a successful stop.
func (l *GenerationLayer) VisitNodesByNameContainingFoldedContext(ctx context.Context, substr string, yield func(*graph.Node) bool) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if yield == nil {
		return nil
	}
	err := l.handle.VisitNodesByNameContainingFoldedContext(ctx, substr, func(node *graph.Node) bool {
		if ctx.Err() != nil {
			return false
		}
		if !l.servesNode(node) {
			return true
		}
		return yield(node)
	})
	if err != nil {
		return err
	}
	return ctx.Err()
}
