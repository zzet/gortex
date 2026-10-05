package mcp

import (
	"context"

	"github.com/zzet/gortex/internal/graph"
)

// Statistics describe the exact selected reader, just like the promoted Stats
// and RepoStats methods. Forwarding them must not bypass the adjacency filter.
func (r *contractCoreEdges) StatsContext(ctx context.Context) (graph.GraphStats, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return graph.GraphStats{}, err
	}
	var stats graph.GraphStats
	if selected, ok := r.Reader.(interface {
		StatsContext(context.Context) (graph.GraphStats, error)
	}); ok {
		var err error
		stats, err = selected.StatsContext(ctx)
		if err != nil {
			return graph.GraphStats{}, err
		}
	} else {
		stats = r.Stats()
	}
	if err := ctx.Err(); err != nil {
		return graph.GraphStats{}, err
	}
	return stats, nil
}

// This private stats-only hook preserves conditional counter support through
// the core's existing capability wrappers. A composed or scoped reader without
// counters must still use its own RepoStats, never an underlying store's totals.
func (r *contractCoreEdges) contractCoreRepoMemoryEstimates() (map[string]graph.RepoMemoryEstimate, bool) {
	selected, ok := r.Reader.(interface {
		AllRepoMemoryEstimates() map[string]graph.RepoMemoryEstimate
	})
	if !ok {
		return nil, false
	}
	return selected.AllRepoMemoryEstimates(), true
}
