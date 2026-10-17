package graphview

import (
	"context"

	"github.com/zzet/gortex/internal/graph"
)

// VisitNodesByNameContext streams exact-name rows through this generation's
// context-path mask. Returning false stops the underlying Store cursor.
func (l *GenerationLayer) VisitNodesByNameContext(ctx context.Context, name string, yield func(*graph.Node) bool) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if yield == nil {
		return nil
	}
	return l.handle.VisitNodesByNameContext(ctx, name, func(node *graph.Node) bool {
		if !l.servesNode(node) {
			return true
		}
		return yield(node)
	})
}

// VisitNodesByNamesContext applies the generation's context-path mask to every
// row of a checked batch exact-name lookup.
func (l *GenerationLayer) VisitNodesByNamesContext(ctx context.Context, names []string, yield func(*graph.Node) bool) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if yield == nil {
		return nil
	}
	return l.handle.VisitNodesByNamesContext(ctx, names, func(node *graph.Node) bool {
		if !l.servesNode(node) {
			return true
		}
		return yield(node)
	})
}
