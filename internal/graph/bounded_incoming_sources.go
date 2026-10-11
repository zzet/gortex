package graph

import (
	"context"
	"sort"
)

// BoundedIncomingSourceProjection is a metadata-free incoming-adjacency page.
// Sources contains distinct source IDs for each requested target. Truncated is
// set per target when more than limit distinct sources of the requested kind
// exist; callers must not interpret that target's partial Sources as complete.
type BoundedIncomingSourceProjection struct {
	Sources   map[string][]string
	Truncated map[string]bool
}

// BoundedIncomingSourceReader projects distinct incoming source identities for
// one edge kind under a per-target cap. It is optional so bounded callers can
// fail closed instead of falling back to full incoming-edge materialization.
type BoundedIncomingSourceReader interface {
	FindIncomingSourcesBounded(context.Context, []string, EdgeKind, int) (BoundedIncomingSourceProjection, error)
}

type contextNodesByIDsReader interface {
	GetNodesByIDsContext(context.Context, []string) (map[string]*Node, error)
}

var (
	_ BoundedIncomingSourceReader = (*Graph)(nil)
	_ BoundedIncomingSourceReader = (*OverlaidView)(nil)

	maxBoundedIncomingSourceLimit = int(^uint(0) >> 1)
)

// GetNodesByIDsContext is the cancellable exact-refetch sibling used by
// bounded request paths. It preserves overlay ownership for both ordinary and
// detached legacy identities, never mutates the caller's ID slice, and delegates
// the durable partition to a contextual base when available.
func (v *OverlaidView) GetNodesByIDsContext(ctx context.Context, ids []string) (map[string]*Node, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	out := make(map[string]*Node, len(ids))
	baseIDs := make([]string, 0, len(ids))
	seen := make(map[string]struct{}, len(ids))
	for index, id := range ids {
		if index&127 == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		if id == "" {
			continue
		}
		if _, duplicate := seen[id]; duplicate {
			continue
		}
		seen[id] = struct{}{}
		if v.layer != nil && (v.nodeBelongsToOverlay(id) || v.layer.OwnsNodeIdentity(id)) {
			if node := v.layer.NodeByID(id); node != nil {
				out[id] = node
			}
			continue
		}
		baseIDs = append(baseIDs, id)
	}
	if len(baseIDs) == 0 || v.base == nil {
		return out, nil
	}
	var (
		base map[string]*Node
		err  error
	)
	if contextual, ok := v.base.(contextNodesByIDsReader); ok {
		base, err = contextual.GetNodesByIDsContext(ctx, baseIDs)
	} else {
		base = v.base.GetNodesByIDs(baseIDs)
	}
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	for id, node := range base {
		if node != nil {
			out[id] = node
		}
	}
	return out, nil
}

func boundedIncomingTargetIDs(ids []string) []string {
	seen := make(map[string]struct{}, len(ids))
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if id == "" {
			continue
		}
		if _, duplicate := seen[id]; duplicate {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	return out
}

// FindIncomingSourcesBounded is the in-memory reference implementation. It
// scans immutable edge pointers under one shard read lock per target, retains
// only distinct source identities, and stops at the limit+1 sentinel.
func (g *Graph) FindIncomingSourcesBounded(
	ctx context.Context,
	targetIDs []string,
	kind EdgeKind,
	limit int,
) (BoundedIncomingSourceProjection, error) {
	projection := BoundedIncomingSourceProjection{
		Sources:   make(map[string][]string),
		Truncated: make(map[string]bool),
	}
	if g == nil {
		return projection, nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	ids := boundedIncomingTargetIDs(targetIDs)
	if limit <= 0 {
		for _, id := range ids {
			projection.Truncated[id] = true
		}
		return projection, nil
	}
	if limit >= maxBoundedIncomingSourceLimit {
		return BoundedIncomingSourceProjection{}, &BoundedLocalizationLimitError{
			Resource: "incoming-source sentinel",
			Limit:    maxBoundedIncomingSourceLimit - 1,
		}
	}
	for _, id := range ids {
		if err := ctx.Err(); err != nil {
			return BoundedIncomingSourceProjection{}, err
		}
		shard := g.shardFor(id)
		shard.mu.RLock()
		seen := make(map[string]struct{}, limit+1)
		for index, edge := range shard.inEdges[id] {
			if index&127 == 0 {
				if err := ctx.Err(); err != nil {
					shard.mu.RUnlock()
					return BoundedIncomingSourceProjection{}, err
				}
			}
			if edge == nil || edge.Kind != kind || edge.From == "" {
				continue
			}
			seen[edge.From] = struct{}{}
			if len(seen) > limit {
				projection.Truncated[id] = true
				break
			}
		}
		shard.mu.RUnlock()
		if projection.Truncated[id] {
			continue
		}
		sources := make([]string, 0, len(seen))
		for sourceID := range seen {
			sources = append(sources, sourceID)
		}
		sort.Strings(sources)
		if len(sources) > 0 {
			projection.Sources[id] = sources
		}
	}
	return projection, nil
}

// FindIncomingSourcesBounded applies edge-file and independent outgoing-set
// ownership to checked raw candidates before deduplicating source identities.
// Unrelated nodes/masks are never scanned or charged against the query budget.
func (v *OverlaidView) FindIncomingSourcesBounded(
	ctx context.Context,
	targetIDs []string,
	kind EdgeKind,
	limit int,
) (BoundedIncomingSourceProjection, error) {
	projection := BoundedIncomingSourceProjection{
		Sources:   make(map[string][]string),
		Truncated: make(map[string]bool),
	}
	if v == nil {
		return projection, nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	ids := boundedIncomingTargetIDs(targetIDs)
	if limit <= 0 {
		for _, id := range ids {
			projection.Truncated[id] = true
		}
		return projection, nil
	}
	if err := ctx.Err(); err != nil {
		return BoundedIncomingSourceProjection{}, err
	}
	if limit >= maxBoundedIncomingSourceLimit {
		return BoundedIncomingSourceProjection{}, &BoundedLocalizationLimitError{
			Resource: "overlay incoming-source sentinel",
			Limit:    maxBoundedIncomingSourceLimit - 1,
		}
	}
	if v.layer == nil {
		if v.base == nil {
			return projection, nil
		}
		bounded, ok := v.base.(BoundedIncomingSourceReader)
		if !ok {
			return BoundedIncomingSourceProjection{}, ErrBoundedLocalizationUnavailable
		}
		return bounded.FindIncomingSourcesBounded(ctx, ids, kind, limit)
	}

	return v.FindIncomingSourcesScoped(ctx, ids, kind, limit, IncomingSourceScope{}, nil)
}
