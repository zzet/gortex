package indexer

import (
	"context"

	"github.com/zzet/gortex/internal/graph"
)

// FindNodesByNameContext preserves the optional request-context capability of
// the composed commit-layer reader wrapped by commitLayerBase.
func (b commitLayerBase) FindNodesByNameContext(ctx context.Context, name string) ([]*graph.Node, error) {
	return graph.FindNodesByNameContext(ctx, b.Reader, name)
}

// VisitNodesByNameContext preserves the optional streaming exact-name
// capability of the composed commit-layer reader wrapped by commitLayerBase.
func (b commitLayerBase) VisitNodesByNameContext(ctx context.Context, name string, yield func(*graph.Node) bool) error {
	return graph.VisitNodesByNameContext(ctx, b.Reader, name, yield)
}

// VisitNodesByNamesContext preserves the optional batched exact-name visitor
// through the base wrapper used by incremental builds.
func (b commitLayerBase) VisitNodesByNamesContext(ctx context.Context, names []string, yield func(*graph.Node) bool) error {
	return graph.VisitNodesByNamesContext(ctx, b.Reader, names, yield)
}
