package indexer

import (
	"context"
	"errors"
	"sort"
	"time"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/viewmetrics"
)

// Ref-view retention.
//
// A ref view's generation is not owned by a checkout, so nothing takes it away
// when a working copy goes: selecting a hundred branches leaves a hundred
// payloads that only this pass will ever collect. Three bounds decide what
// survives:
//
//   - Recency. The generation a view selected inside the retention window is
//     serving is kept, however far over the other two bounds the graph is: it
//     is the answer the next selection of that view gives. Everything else is
//     a candidate — a generation nothing points at any more, and the one a
//     view nobody has selected since the window opened is still holding.
//   - Count. Past the per-graph cap, candidates go least recently selected
//     first, which is the order a cache evicts in.
//   - Bytes. The publish step records each generation's payload size, so the
//     per-graph budget is a sum over rows rather than a guess; the same
//     oldest-first order decides who pays for it.
//
// Nothing here decides that a generation may go. Every candidate is offered to
// the same guarded retire the coordinators use, so a routed, based-upon or
// leased generation is refused there and simply offered again next sweep. The
// one thing that guard cannot catch is a generation still being built —
// nothing references it yet and nothing leases it — so an in-flight build is
// kept out of the candidate scan here instead.

// RefViewRetention bounds the ref-view payload one graph keeps.
type RefViewRetention struct {
	// RetainInactive is how long a generation survives after the last
	// selection of the view serving it.
	RetainInactive time.Duration
	// MaxCachedGenerations caps how many ref-view generations one graph keeps.
	MaxCachedGenerations int
	// MaxBytesPerGraph caps the total recorded payload size of one graph's
	// ref-view generations.
	MaxBytesPerGraph int64
}

// DefaultRefViewRetention returns the shipped bounds.
func DefaultRefViewRetention() RefViewRetention {
	return RefViewRetention{
		RetainInactive:       7 * 24 * time.Hour,
		MaxCachedGenerations: 32,
		MaxBytesPerGraph:     5 << 30,
	}
}

// withDefaults fills every unset bound from the shipped defaults. A zero bound
// is "not configured" rather than "collect everything": a cap of zero would
// evict a generation the moment it was published.
func (r RefViewRetention) withDefaults() RefViewRetention {
	out := DefaultRefViewRetention()
	if r.RetainInactive > 0 {
		out.RetainInactive = r.RetainInactive
	}
	if r.MaxCachedGenerations > 0 {
		out.MaxCachedGenerations = r.MaxCachedGenerations
	}
	if r.MaxBytesPerGraph > 0 {
		out.MaxBytesPerGraph = r.MaxBytesPerGraph
	}
	return out
}

// refViewCandidate is one ref-view generation the sweep is deciding about.
type refViewCandidate struct {
	generationID int64
	// selected is the clock the eviction order runs on: the last selection of
	// the view this generation serves, or — for a generation no view points at
	// any more — when it was published.
	selected int64
	bytes    int64
	// refViewID is the view still pointing at this generation, empty when
	// nothing does. The pointer is a reference the guarded retire refuses on,
	// so evicting the payload means forgetting the view first — which is
	// exactly what eviction means: the next selection of that selector
	// resolves it again and rebuilds.
	refViewID string
	// reason is which of the three bounds decided this candidate: it aged out
	// of the retention window, or it was over the count or byte budget. It is
	// the eviction's class, and the only thing about it a metric may carry.
	reason string
}

// sweepRefViewRetention collects the ref-view generations the bounds no longer
// keep, and reports how many went.
func (l *CheckoutLifecycle) sweepRefViewRetention(ctx context.Context) int {
	if l == nil || l.store == nil || l.catalog == nil {
		return 0
	}
	rows, err := l.catalog.ListViewGenerations(ctx, store_sqlite.ViewGenerationFilter{
		OwnerKind: refViewOwnerKind,
	})
	if err != nil {
		l.logger.Debug("checkout lifecycle: could not scan ref view generations", zap.Error(err))
		return 0
	}
	byGraph := map[string][]store_sqlite.ViewGeneration{}
	for _, row := range rows {
		if row.GraphID == "" || row.GenerationID <= 0 {
			continue
		}
		byGraph[row.GraphID] = append(byGraph[row.GraphID], row)
	}

	retired := 0
	for graphID, generations := range byGraph {
		for _, candidate := range l.refViewEvictions(ctx, graphID, generations) {
			if candidate.refViewID != "" {
				if err := l.catalog.DeleteRefView(ctx, candidate.refViewID); err != nil {
					l.logger.Debug("checkout lifecycle: could not release an evicted ref view",
						zap.String("ref_view", candidate.refViewID), zap.Error(err))
					continue
				}
			}
			err := l.store.RetirePayloadGeneration(ctx, candidate.generationID, l.leases.InUse)
			switch {
			case err == nil, errors.Is(err, store_sqlite.ErrCatalogNotFound):
				retired++
				viewmetrics.Count(viewmetrics.RefViewEvictedTotal, candidate.reason)
				viewmetrics.Count(viewmetrics.GenerationSweepCollectedTotal, viewmetrics.SweepRefView)
			default:
				// Leased, based-upon, or re-adopted between the two writes. The
				// generation stays and the next sweep asks again.
				l.logger.Debug("checkout lifecycle: ref view generation kept",
					zap.String("graph", graphID),
					zap.Int64("generation", candidate.generationID), zap.Error(err))
			}
		}
	}
	return retired
}

// refViewEvictions decides which of one graph's ref-view generations the
// bounds no longer keep, oldest selection first.
func (l *CheckoutLifecycle) refViewEvictions(
	ctx context.Context,
	graphID string,
	generations []store_sqlite.ViewGeneration,
) []refViewCandidate {
	views, err := l.catalog.ListRefViews(ctx, graphID)
	if err != nil {
		l.logger.Debug("checkout lifecycle: could not list ref views",
			zap.String("graph", graphID), zap.Error(err))
		return nil
	}
	type selection struct {
		refViewID string
		selected  int64
	}
	active := make(map[int64]selection, len(views))
	for _, view := range views {
		if view.ActiveGenerationID > 0 {
			active[view.ActiveGenerationID] = selection{view.RefViewID, view.LastSelected}
		}
	}

	cutoff := l.now().Add(-l.refViewRetention.RetainInactive).Unix()
	inFlightSince := l.now().Add(-refViewBuildLiveness).Unix()
	var (
		candidates []refViewCandidate
		total      int64
		kept       int
	)
	for _, row := range generations {
		if row.State == store_sqlite.ViewGenerationBuilding && row.CreatedAt >= inFlightSince {
			// A build is writing this payload right now. Nothing references it
			// and nothing leases it, so the guarded retire would accept it and
			// seal it under its own builder — the janitor failing a build
			// rather than bounding anything. It is not cached payload until it
			// publishes, so it is not counted either. A building row past the
			// liveness window has no builder left behind it and stays a
			// candidate: that is the only way a crashed build's payload is
			// ever collected.
			continue
		}
		total += row.StorageBytes
		serving, served := active[row.GenerationID]
		if served && serving.selected >= cutoff {
			// The view serving it was selected inside the window. It is what
			// the next selection of that view answers with.
			kept++
			continue
		}
		candidate := refViewCandidate{
			generationID: row.GenerationID,
			selected:     serving.selected,
			bytes:        row.StorageBytes,
			refViewID:    serving.refViewID,
		}
		if !served {
			candidate.selected = row.PublishedAt
			if candidate.selected == 0 {
				candidate.selected = row.CreatedAt
			}
		}
		candidates = append(candidates, candidate)
	}
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].selected != candidates[j].selected {
			return candidates[i].selected < candidates[j].selected
		}
		return candidates[i].generationID < candidates[j].generationID
	})

	overCount := kept + len(candidates) - l.refViewRetention.MaxCachedGenerations
	overBytes := total - l.refViewRetention.MaxBytesPerGraph
	var out []refViewCandidate
	for _, candidate := range candidates {
		stale := candidate.selected < cutoff
		switch {
		case stale:
			candidate.reason = viewmetrics.EvictedStale
		case overCount > 0:
			candidate.reason = viewmetrics.EvictedOverCount
		case overBytes > 0:
			candidate.reason = viewmetrics.EvictedOverBytes
		default:
			return out
		}
		out = append(out, candidate)
		overCount--
		overBytes -= candidate.bytes
	}
	return out
}
