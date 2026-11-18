package graph

import "context"

type overlayStatsContextReader interface {
	StatsContext(context.Context) (GraphStats, error)
}

// statsLegacy preserves Stats' contextless base-reader behavior while sharing
// the first complete result with StatsContext.
func (v *OverlaidView) statsLegacy() GraphStats {
	if stats, ok := v.cachedStats(); ok {
		return stats
	}
	stats, _ := v.computeStats(context.Background(), false)
	return v.cacheStats(stats)
}

// StatsContext returns the overlay's statistics under the caller's deadline.
// Failed or canceled work is computed only in locals and never enters the
// request-local cache; the next caller can retry normally.
func (v *OverlaidView) StatsContext(ctx context.Context) (GraphStats, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return GraphStats{}, err
	}
	if stats, ok := v.cachedStats(); ok {
		if err := ctx.Err(); err != nil {
			return GraphStats{}, err
		}
		return stats, nil
	}
	stats, err := v.computeStats(ctx, true)
	if err != nil {
		return GraphStats{}, err
	}
	if err := ctx.Err(); err != nil {
		return GraphStats{}, err
	}
	stats = v.cacheStats(stats)
	if err := ctx.Err(); err != nil {
		return GraphStats{}, err
	}
	return stats, nil
}

func (v *OverlaidView) cachedStats() (GraphStats, bool) {
	v.statsMu.Lock()
	defer v.statsMu.Unlock()
	if v.stats == nil {
		return GraphStats{}, false
	}
	return *v.stats, true
}

func (v *OverlaidView) cacheStats(stats GraphStats) GraphStats {
	v.statsMu.Lock()
	defer v.statsMu.Unlock()
	if v.stats == nil {
		cached := stats
		v.stats = &cached
	}
	return *v.stats
}

// computeStats keeps the captured Stats composition rules byte-for-byte in
// meaning. Context checks around the layer deltas prevent a canceled partial
// result from being cached, but the legacy Reader delta methods themselves are
// contextless and therefore cannot be interrupted in the middle of one call.
func (v *OverlaidView) computeStats(ctx context.Context, contextual bool) (GraphStats, error) {
	if v.base == nil {
		return GraphStats{}, nil
	}
	if err := ctx.Err(); err != nil {
		return GraphStats{}, err
	}

	var stats GraphStats
	if contextual {
		if reader, ok := v.base.(overlayStatsContextReader); ok {
			var err error
			stats, err = reader.StatsContext(ctx)
			if err != nil {
				return GraphStats{}, err
			}
		} else {
			stats = v.base.Stats()
		}
	} else {
		stats = v.base.Stats()
	}
	if err := ctx.Err(); err != nil {
		return GraphStats{}, err
	}
	if v.layer == nil {
		return stats, nil
	}

	nodeDelta := v.nodeCountDelta()
	if err := ctx.Err(); err != nil {
		return GraphStats{}, err
	}
	edgeCount := v.EdgeCount()
	if err := ctx.Err(); err != nil {
		return GraphStats{}, err
	}
	baseEdgeCount := v.base.EdgeCount()
	if err := ctx.Err(); err != nil {
		return GraphStats{}, err
	}
	stats.TotalNodes += nodeDelta
	stats.TotalEdges += edgeCount - baseEdgeCount
	return stats, nil
}
