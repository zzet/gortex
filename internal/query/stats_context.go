package query

import (
	"context"

	"github.com/zzet/gortex/internal/graph"
)

type statsContextReader interface {
	StatsContext(context.Context) (graph.GraphStats, error)
}

// StatsContext returns graph statistics under the caller's deadline when the
// bound reader offers the additive capability. Legacy readers keep their
// existing Stats behavior, but a caller that expires while they run receives
// the context error instead of a stale or partial response.
func (e *Engine) StatsContext(ctx context.Context) (*graph.GraphStats, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if reader, ok := e.g.(statsContextReader); ok {
		stats, err := reader.StatsContext(ctx)
		if err != nil {
			return nil, err
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return &stats, nil
	}
	stats := e.g.Stats()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return &stats, nil
}
