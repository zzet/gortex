package mcp

import (
	"context"

	"github.com/zzet/gortex/internal/graph"
)

var _ graph.KindAdjacencyReader = (*contractCoreEdges)(nil)

// Preserve the complete selected adjacency's masks and ordering, including
// Total: counting raw hidden rows would change a capped walk's sort decision.
func (r *contractCoreEdges) ReadKindAdjacency(ctx context.Context, id string, dir graph.Direction, kinds []graph.EdgeKind) (graph.KindAdjacency, error) {
	if selected, ok := r.Reader.(graph.FilteredKindAdjacencyReader); ok {
		rows, err := selected.ReadKindAdjacencyFiltered(ctx, id, dir, kinds, r.deadCodeFilter())
		recordContractCoreReadError(r.ctx, err)
		return rows, err
	}
	// A composed/overlay reader owns its endpoint and recording-file masks.
	// Keep its ordinary visible lookup rather than peeling off that view.
	return graph.ReadKindAdjacency(ctx, kindAdjacencyFallback{r}, id, dir, kinds)
}

// Embedding Reader intentionally drops this optional capability, so the
// fallback helper reads filtered adjacency instead of recursing into itself.
type kindAdjacencyFallback struct{ graph.Reader }
