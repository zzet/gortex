package mcp

import (
	"context"

	"github.com/zzet/gortex/internal/graph"
)

var (
	_ graph.BFSCapable         = (*contractCoreEdges)(nil)
	_ graph.BFSAdmissionReader = (*contractCoreEdges)(nil)
)

// Canonical BFS hops are recomputed over visible adjacency, never forwarded to
// a physical traversal that could walk hidden contract or overlay edges.
func (r *contractCoreEdges) BFS(seeds []string, dir graph.Direction, kinds []graph.EdgeKind, depth, limit int) ([]graph.BFSHop, error) {
	return r.BFSContext(r.ctx, seeds, dir, kinds, depth, limit)
}
func (r *contractCoreEdges) BFSContext(ctx context.Context, seeds []string, dir graph.Direction, kinds []graph.EdgeKind, depth, limit int) ([]graph.BFSHop, error) {
	return graph.BFSOf(ctx, r, seeds, dir, kinds, depth, limit)
}

// Engine admission retains its established backedges/caps/boundaries. Only a
// selected physical backend accepting the complete exclusions can prepare a
// one-statement snapshot. Overlays and broad/budget-exhausted walks decline,
// then the engine keeps its original selected-reader Go walk.
func (r *contractCoreEdges) BFSAdmissionContext(ctx context.Context, request graph.BFSAdmissionRequest) (graph.BFSAdmissionSnapshot, bool, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return graph.BFSAdmissionSnapshot{}, false, err
	}
	backend, ok := r.Reader.(graph.FilteredBFSAdmissionReader)
	if !ok {
		return graph.BFSAdmissionSnapshot{}, false, nil
	}
	rows, handled, err := backend.BFSAdmissionFilteredContext(ctx, request, r.deadCodeFilter())
	recordContractCoreReadError(r.ctx, err)
	return rows, handled, err
}
