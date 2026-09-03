package graph

import "context"

var (
	_ BoundedOutgoingEdgeIdentityReader = (*OverlaidView)(nil)
	_ BoundedIncomingEdgeIdentityReader = (*OverlaidView)(nil)
)

func overlayAdjacencyCompensatedLimit(layer OverlayLayerReader, limit int) int {
	if layer == nil {
		return limit
	}
	// One shadow identity may hide arbitrarily many location-distinct edges.
	// The only honest bounded compensation is the full per-key envelope; the
	// caller-visible limit is re-applied after overlay filtering.
	return MaxBoundedAdjacencyRowsPerKey
}

// overlayOwnsIdentity reports whether the layer speaks for a node
// identity: it covers the file the ID belongs to, carries a node under
// the ID, or marked the ID removed.
func (v *OverlaidView) overlayOwnsIdentity(id string) bool {
	return v != nil && v.layer != nil &&
		(v.nodeBelongsToOverlay(id) || v.layer.OwnsNodeIdentity(id))
}

// overlayOwnsBaseEdge reports whether the layer supersedes one base
// edge. Ownership follows the file the edge was RECORDED in, because
// that is the file whose re-derivation produced it: covering a file
// replaces the edges written in it and nothing else. A base edge out of
// a covered file's symbol that base recorded somewhere else — the
// reverse value flow a caller's own file holds, say — belongs to that
// other file, and hiding it would drop a row nothing in the layer
// re-creates.
//
// filePath is the edge's recorded path, empty when base wrote none.
// Without a path there is no file to ask the question with, so the
// coarse source-side claim answers instead. Either way the source's own
// OwnsOutEdges claim still applies: it is the claim no file list can
// express, reaching edges wherever they were recorded — a layer that
// only retargets what an untouched node points at speaks for that
// node's adjacency while the node itself keeps coming from base.
func (v *OverlaidView) overlayOwnsBaseEdge(from, filePath string) bool {
	if v == nil || v.layer == nil {
		return false
	}
	if filePath == "" {
		return v.overlayOwnsSourceAdjacency(from)
	}
	return v.layer.HasFile(filePath) || v.layer.OwnsOutEdges(from)
}

// overlayOwnsSourceAdjacency reports whether the layer speaks for a
// source's edges at all — because it covers the file the source lives
// in, or because it replaced that source's adjacency outright. It is the
// coarse claim, and it is what the readers that hold no recorded path
// have to decide on: the layer's own staged edges, whose provenance is
// the source they were staged for, and the existence probe, whose key
// carries no file.
func (v *OverlaidView) overlayOwnsSourceAdjacency(id string) bool {
	return v != nil && v.layer != nil &&
		(v.layer.CoversNodeID(id) || v.layer.OwnsOutEdges(id))
}

// overlayIdentityVisible reports whether a node identity is still
// visible through the view: either the layer does not speak for it, or
// it does and kept a node under that ID. Both endpoints of an edge are
// tested with it, so a source and a target disappear under the same
// rule.
func (v *OverlaidView) overlayIdentityVisible(id string) bool {
	return !v.overlayOwnsIdentity(id) || v.layer.NodeByID(id) != nil
}

func appendBoundedBaseIdentities(
	ctx context.Context,
	identities []EdgeIdentity,
	limit int,
	budget *boundedAdjacencyBudget,
	accept func(EdgeIdentity) bool,
) ([]EdgeIdentity, bool, error) {
	var out []EdgeIdentity
	seen := make(map[EdgeIdentity]struct{})
	for index, identity := range identities {
		if index&127 == 0 {
			if err := ctx.Err(); err != nil {
				return nil, false, err
			}
		}
		if accept != nil && !accept(identity) {
			continue
		}
		if _, duplicate := seen[identity]; duplicate {
			continue
		}
		seen[identity] = struct{}{}
		if err := budget.retain(); err != nil {
			return nil, false, err
		}
		if len(out) == limit {
			return nil, true, nil
		}
		out = append(out, identity)
	}
	sortEdgeIdentities(out)
	return out, false, nil
}

func mergeBoundedIdentitySlices(left, right []EdgeIdentity, limit int) ([]EdgeIdentity, bool) {
	out := make([]EdgeIdentity, 0, len(left)+len(right))
	seen := make(map[EdgeIdentity]struct{}, len(left)+len(right))
	for _, identities := range [][]EdgeIdentity{left, right} {
		for _, identity := range identities {
			if _, duplicate := seen[identity]; duplicate {
				continue
			}
			seen[identity] = struct{}{}
			if len(out) == limit {
				return nil, true
			}
			out = append(out, identity)
		}
	}
	sortEdgeIdentities(out)
	return out, false
}

// FindOutgoingEdgeIdentitiesBounded preserves GetOutEdges replacement and
// tombstone semantics without materializing Edge payloads. Durable rows the
// layer supersedes are dropped by the same per-edge rule — the identity key
// carries the recorded path, so the projection can ask it — and the layer's
// own adjacency is merged in; rows touching a removed overlay identity are
// filtered before the caller-visible cap. A source the layer hid contributes
// nothing at all.
func (v *OverlaidView) FindOutgoingEdgeIdentitiesBounded(
	ctx context.Context,
	sourceIDs []string,
	kinds []EdgeKind,
	limit int,
) (BoundedEdgeIdentityProjection, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return BoundedEdgeIdentityProjection{}, err
	}
	if err := validateBoundedAdjacencyLimit(limit); err != nil {
		return BoundedEdgeIdentityProjection{}, err
	}
	kindSet, err := canonicalBoundedAdjacencyKinds(ctx, kinds)
	if err != nil {
		return BoundedEdgeIdentityProjection{}, err
	}
	ids, err := canonicalBoundedAdjacencyEndpoints(ctx, sourceIDs)
	if err != nil {
		return BoundedEdgeIdentityProjection{}, err
	}
	projection := emptyBoundedEdgeIdentityProjection()
	if v == nil || len(ids) == 0 || len(kindSet) == 0 {
		return projection, nil
	}
	if v.layer == nil {
		if v.base == nil {
			return projection, nil
		}
		bounded, ok := v.base.(BoundedOutgoingEdgeIdentityReader)
		if !ok {
			return BoundedEdgeIdentityProjection{}, ErrBoundedLocalizationUnavailable
		}
		return bounded.FindOutgoingEdgeIdentitiesBounded(ctx, ids, kinds, limit)
	}

	baseIDs := make([]string, 0, len(ids))
	hiddenSources := make(map[string]bool)
	for _, id := range ids {
		if v.overlayOwnsIdentity(id) && v.layer.NodeByID(id) == nil {
			hiddenSources[id] = true
			continue
		}
		baseIDs = append(baseIDs, id)
	}
	baseProjection := emptyBoundedEdgeIdentityProjection()
	if len(baseIDs) > 0 && v.base != nil {
		bounded, ok := v.base.(BoundedOutgoingEdgeIdentityReader)
		if !ok {
			return BoundedEdgeIdentityProjection{}, ErrBoundedLocalizationUnavailable
		}
		baseProjection, err = bounded.FindOutgoingEdgeIdentitiesBounded(
			ctx, baseIDs, kinds, overlayAdjacencyCompensatedLimit(v.layer, limit),
		)
		if err != nil {
			return BoundedEdgeIdentityProjection{}, err
		}
	}

	budget := &boundedAdjacencyBudget{}
	for _, sourceID := range ids {
		if err := ctx.Err(); err != nil {
			return BoundedEdgeIdentityProjection{}, err
		}
		if hiddenSources[sourceID] {
			continue
		}
		if baseProjection.Truncated[sourceID] {
			projection.Truncated[sourceID] = true
			continue
		}
		baseIdentities, baseTruncated, mergeErr := appendBoundedBaseIdentities(
			ctx, baseProjection.ByEndpoint[sourceID], limit, budget,
			func(identity EdgeIdentity) bool {
				return identity.From == sourceID &&
					!v.overlayOwnsBaseEdge(identity.From, identity.FilePath) &&
					v.overlayIdentityVisible(identity.To) && kindRequested(kindSet, identity.Kind)
			},
		)
		if mergeErr != nil {
			return BoundedEdgeIdentityProjection{}, mergeErr
		}
		if baseTruncated {
			projection.Truncated[sourceID] = true
			continue
		}
		overlayIdentities, overlayTruncated, scanErr := scanBoundedEdgeIdentities(
			ctx, v.layer.OutEdges(sourceID), kindSet, limit, nil, budget,
			func(identity EdgeIdentity) bool { return v.overlayIdentityVisible(identity.To) },
		)
		if scanErr != nil {
			return BoundedEdgeIdentityProjection{}, scanErr
		}
		if overlayTruncated {
			projection.Truncated[sourceID] = true
			continue
		}
		identities, truncated := mergeBoundedIdentitySlices(baseIdentities, overlayIdentities, limit)
		if truncated {
			projection.Truncated[sourceID] = true
			continue
		}
		if len(identities) > 0 {
			projection.ByEndpoint[sourceID] = identities
		}
	}
	return projection, nil
}

// FindIncomingEdgeIdentitiesBounded preserves GetInEdges semantics: durable
// edges from overlay-owned sources are replaced, then current overlay edges are
// merged. A removed target yields an empty complete key; same-ID replacement
// retains durable callers from untouched sources.
func (v *OverlaidView) FindIncomingEdgeIdentitiesBounded(
	ctx context.Context,
	targetIDs []string,
	kinds []EdgeKind,
	limit int,
) (BoundedEdgeIdentityProjection, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return BoundedEdgeIdentityProjection{}, err
	}
	if err := validateBoundedAdjacencyLimit(limit); err != nil {
		return BoundedEdgeIdentityProjection{}, err
	}
	kindSet, err := canonicalBoundedAdjacencyKinds(ctx, kinds)
	if err != nil {
		return BoundedEdgeIdentityProjection{}, err
	}
	ids, err := canonicalBoundedAdjacencyEndpoints(ctx, targetIDs)
	if err != nil {
		return BoundedEdgeIdentityProjection{}, err
	}
	projection := emptyBoundedEdgeIdentityProjection()
	if v == nil || len(ids) == 0 || len(kindSet) == 0 {
		return projection, nil
	}
	if v.layer == nil {
		if v.base == nil {
			return projection, nil
		}
		bounded, ok := v.base.(BoundedIncomingEdgeIdentityReader)
		if !ok {
			return BoundedEdgeIdentityProjection{}, ErrBoundedLocalizationUnavailable
		}
		return bounded.FindIncomingEdgeIdentitiesBounded(ctx, ids, kinds, limit)
	}

	baseIDs := make([]string, 0, len(ids))
	removedTargets := make(map[string]bool)
	for _, id := range ids {
		if v.overlayOwnsIdentity(id) && v.layer.NodeByID(id) == nil {
			removedTargets[id] = true
			continue
		}
		baseIDs = append(baseIDs, id)
	}
	baseProjection := emptyBoundedEdgeIdentityProjection()
	if len(baseIDs) > 0 && v.base != nil {
		bounded, ok := v.base.(BoundedIncomingEdgeIdentityReader)
		if !ok {
			return BoundedEdgeIdentityProjection{}, ErrBoundedLocalizationUnavailable
		}
		baseProjection, err = bounded.FindIncomingEdgeIdentitiesBounded(
			ctx, baseIDs, kinds, overlayAdjacencyCompensatedLimit(v.layer, limit),
		)
		if err != nil {
			return BoundedEdgeIdentityProjection{}, err
		}
	}

	budget := &boundedAdjacencyBudget{}
	for _, targetID := range ids {
		if err := ctx.Err(); err != nil {
			return BoundedEdgeIdentityProjection{}, err
		}
		if removedTargets[targetID] {
			continue
		}
		if baseProjection.Truncated[targetID] {
			projection.Truncated[targetID] = true
			continue
		}
		baseIdentities, baseTruncated, mergeErr := appendBoundedBaseIdentities(
			ctx, baseProjection.ByEndpoint[targetID], limit, budget,
			func(identity EdgeIdentity) bool {
				return identity.To == targetID &&
					!v.overlayOwnsBaseEdge(identity.From, identity.FilePath) &&
					v.overlayIdentityVisible(identity.From) &&
					v.overlayIdentityVisible(identity.To) && kindRequested(kindSet, identity.Kind)
			},
		)
		if mergeErr != nil {
			return BoundedEdgeIdentityProjection{}, mergeErr
		}
		if baseTruncated {
			projection.Truncated[targetID] = true
			continue
		}
		overlayIdentities, overlayTruncated, scanErr := scanBoundedEdgeIdentities(
			ctx, v.layer.InEdges(targetID), kindSet, limit, nil, budget,
			func(identity EdgeIdentity) bool {
				return identity.To == targetID && v.overlayIdentityVisible(identity.To) &&
					v.overlayOwnsSourceAdjacency(identity.From) && v.overlayIdentityVisible(identity.From)
			},
		)
		if scanErr != nil {
			return BoundedEdgeIdentityProjection{}, scanErr
		}
		if overlayTruncated {
			projection.Truncated[targetID] = true
			continue
		}
		identities, truncated := mergeBoundedIdentitySlices(baseIdentities, overlayIdentities, limit)
		if truncated {
			projection.Truncated[targetID] = true
			continue
		}
		if len(identities) > 0 {
			projection.ByEndpoint[targetID] = identities
		}
	}
	return projection, nil
}

func kindRequested(kinds map[EdgeKind]struct{}, kind EdgeKind) bool {
	_, ok := kinds[kind]
	return ok
}
