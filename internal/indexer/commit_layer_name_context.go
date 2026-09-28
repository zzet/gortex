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
