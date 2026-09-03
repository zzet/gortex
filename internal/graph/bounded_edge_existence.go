package graph

import (
	"context"
	"sort"
)

// MaxBoundedEdgeExistencePredicates is the hard request envelope for exact
// edge-existence projections. It is intentionally sized to the qualified-leaf
// localization frontier: ten candidate declarations times at most thirty-two
// admitted owner declarations.
const MaxBoundedEdgeExistencePredicates = 320

// TypedEdgeEndpoint identifies an exact directed edge independent of source
// location and payload. It is comparable and can be used directly as a map key.
type TypedEdgeEndpoint struct {
	From string
	To   string
	Kind EdgeKind
}

// BoundedEdgeExistenceReader answers a finite set of exact edge predicates
// without materializing adjacency or Edge payloads. Implementations must either
// return the complete set of existing predicates or an error; partial evidence
// is never valid. limit applies to unique, non-empty predicates and may not
// exceed MaxBoundedEdgeExistencePredicates.
type BoundedEdgeExistenceReader interface {
	FindExistingEdgeEndpoints(context.Context, []TypedEdgeEndpoint, int) (map[TypedEdgeEndpoint]struct{}, error)
}

var (
	_ BoundedEdgeExistenceReader = (*Graph)(nil)
	_ BoundedEdgeExistenceReader = (*OverlaidView)(nil)
)

type typedEdgeTarget struct {
	to   string
	kind EdgeKind
}

func canonicalBoundedEdgeEndpoints(
	ctx context.Context,
	endpoints []TypedEdgeEndpoint,
	limit int,
) ([]TypedEdgeEndpoint, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if limit < 1 || limit > MaxBoundedEdgeExistencePredicates {
		return nil, &BoundedLocalizationLimitError{
			Resource: "edge-existence predicates",
			Limit:    MaxBoundedEdgeExistencePredicates,
		}
	}
	if len(endpoints) == 0 {
		return nil, nil
	}
	capacity := len(endpoints)
	if capacity > limit+1 {
		capacity = limit + 1
	}
	seen := make(map[TypedEdgeEndpoint]struct{}, capacity)
	out := make([]TypedEdgeEndpoint, 0, capacity)
	for index, endpoint := range endpoints {
		if index&127 == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		if endpoint.From == "" || endpoint.To == "" || endpoint.Kind == "" {
			continue
		}
		if _, duplicate := seen[endpoint]; duplicate {
			continue
		}
		seen[endpoint] = struct{}{}
		if len(seen) > limit {
			return nil, &BoundedLocalizationLimitError{
				Resource: "edge-existence predicates",
				Limit:    limit,
			}
		}
		out = append(out, endpoint)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].From != out[j].From {
			return out[i].From < out[j].From
		}
		if out[i].To != out[j].To {
			return out[i].To < out[j].To
		}
		return out[i].Kind < out[j].Kind
	})
	return out, nil
}

// FindExistingEdgeEndpoints is the in-memory reference implementation. It
// groups predicates by source, takes one shard read lock per source, and stops
// scanning that adjacency bucket as soon as every requested predicate is found.
func (g *Graph) FindExistingEdgeEndpoints(
	ctx context.Context,
	endpoints []TypedEdgeEndpoint,
	limit int,
) (map[TypedEdgeEndpoint]struct{}, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	keys, err := canonicalBoundedEdgeEndpoints(ctx, endpoints, limit)
	if err != nil {
		return nil, err
	}
	found := make(map[TypedEdgeEndpoint]struct{}, len(keys))
	if g == nil || len(keys) == 0 {
		return found, nil
	}
	for start := 0; start < len(keys); {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		end := start + 1
		for end < len(keys) && keys[end].From == keys[start].From {
			end++
		}
		from := keys[start].From
		wanted := make(map[typedEdgeTarget]TypedEdgeEndpoint, end-start)
		for _, endpoint := range keys[start:end] {
			wanted[typedEdgeTarget{to: endpoint.To, kind: endpoint.Kind}] = endpoint
		}
		shard := g.shardFor(from)
		shard.mu.RLock()
		for index, edge := range shard.outEdges[from] {
			if index&127 == 0 {
				if err := ctx.Err(); err != nil {
					shard.mu.RUnlock()
					return nil, err
				}
			}
			if edge == nil {
				continue
			}
			target := typedEdgeTarget{to: edge.To, kind: edge.Kind}
			endpoint, ok := wanted[target]
			if !ok {
				continue
			}
			found[endpoint] = struct{}{}
			delete(wanted, target)
			if len(wanted) == 0 {
				break
			}
		}
		shard.mu.RUnlock()
		start = end
	}
	return found, nil
}

// FindExistingEdgeEndpoints composes exact base evidence with one immutable
// overlay. A source owned by the overlay uses only current overlay edges. For an
// untouched source, base evidence survives unless the exact target identity is
// tombstoned by the overlay; a same-ID replacement preserves that base edge.
//
// Ownership here is decided per SOURCE, not per edge as everywhere else,
// because a {from,to,kind} key names no recorded file to ask the layer about.
// The two readings a file would separate — a call the overlay deleted from the
// covered buffer, and a base edge out of a covered symbol that some other file
// recorded — are the same key at this surface, so the reader keeps the answer
// that never authenticates a deleted declaration. An existence probe that must
// see the second kind reads the outgoing identity projection, whose keys carry
// the path.
func (v *OverlaidView) FindExistingEdgeEndpoints(
	ctx context.Context,
	endpoints []TypedEdgeEndpoint,
	limit int,
) (map[TypedEdgeEndpoint]struct{}, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	keys, err := canonicalBoundedEdgeEndpoints(ctx, endpoints, limit)
	if err != nil {
		return nil, err
	}
	found := make(map[TypedEdgeEndpoint]struct{}, len(keys))
	if v == nil || len(keys) == 0 {
		return found, nil
	}
	if v.layer == nil {
		if v.base == nil {
			return found, nil
		}
		bounded, ok := v.base.(BoundedEdgeExistenceReader)
		if !ok {
			return nil, ErrBoundedLocalizationUnavailable
		}
		return bounded.FindExistingEdgeEndpoints(ctx, keys, limit)
	}

	baseKeys := make([]TypedEdgeEndpoint, 0, len(keys))
	for start := 0; start < len(keys); {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		end := start + 1
		for end < len(keys) && keys[end].From == keys[start].From {
			end++
		}
		from := keys[start].From
		if v.overlayOwnsSourceAdjacency(from) {
			// A covered or detached tombstone owns the adjacency but carries no
			// current declaration. Never authenticate a stray staged edge for a
			// source declaration the overlay removed.
			if !v.overlayIdentityVisible(from) {
				start = end
				continue
			}
			wanted := make(map[typedEdgeTarget]TypedEdgeEndpoint, end-start)
			for _, endpoint := range keys[start:end] {
				if !v.overlayIdentityVisible(endpoint.To) {
					continue
				}
				wanted[typedEdgeTarget{to: endpoint.To, kind: endpoint.Kind}] = endpoint
			}
			if len(wanted) == 0 {
				start = end
				continue
			}
			for index, edge := range v.layer.OutEdges(from) {
				if index&127 == 0 {
					if err := ctx.Err(); err != nil {
						return nil, err
					}
				}
				if edge == nil {
					continue
				}
				target := typedEdgeTarget{to: edge.To, kind: edge.Kind}
				endpoint, ok := wanted[target]
				if !ok {
					continue
				}
				found[endpoint] = struct{}{}
				delete(wanted, target)
				if len(wanted) == 0 {
					break
				}
			}
			start = end
			continue
		}
		for _, endpoint := range keys[start:end] {
			if !v.overlayIdentityVisible(endpoint.To) {
				continue
			}
			baseKeys = append(baseKeys, endpoint)
		}
		start = end
	}

	if len(baseKeys) == 0 || v.base == nil {
		return found, nil
	}
	bounded, ok := v.base.(BoundedEdgeExistenceReader)
	if !ok {
		return nil, ErrBoundedLocalizationUnavailable
	}
	baseFound, err := bounded.FindExistingEdgeEndpoints(ctx, baseKeys, limit)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	for endpoint := range baseFound {
		found[endpoint] = struct{}{}
	}
	return found, nil
}
