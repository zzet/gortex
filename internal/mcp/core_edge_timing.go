package mcp

import (
	"context"

	"github.com/zzet/gortex/internal/search/rerank"
)

type coreEdgeTimingKey struct{}

// Only the observed centrality call installs this hook. Context ancestry keeps
// its checked-read/error holder and selected view; no global state is changed.
func withCoreEdgeTiming(ctx context.Context, timing *rerank.CoreEdgeTiming) context.Context {
	if ctx == nil || timing == nil {
		return ctx
	}
	return context.WithValue(ctx, coreEdgeTimingKey{}, timing)
}

func coreEdgeTimingFromContext(ctx context.Context) *rerank.CoreEdgeTiming {
	if ctx == nil {
		return nil
	}
	timing, _ := ctx.Value(coreEdgeTimingKey{}).(*rerank.CoreEdgeTiming)
	return timing
}
