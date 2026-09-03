package graph

import "context"

var _ BoundedOutgoingSiteEdgeIdentityReader = (*OverlaidView)(nil)

// FindOutgoingSiteEdgeIdentitiesBounded is the exact {source,line} overlay
// projection. Each site merges the base rows the layer does not supersede with
// the layer's own, under the same per-edge rule the plain readers apply. Sites
// are grouped by source, so every site of one source shares a single raw scan
// of the layer's adjacency.
func (v *OverlaidView) FindOutgoingSiteEdgeIdentitiesBounded(
	ctx context.Context,
	sites []EdgeSourceSite,
	kinds []EdgeKind,
	limit int,
) (BoundedSiteEdgeIdentityProjection, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return BoundedSiteEdgeIdentityProjection{}, err
	}
	if err := validateBoundedAdjacencyLimit(limit); err != nil {
		return BoundedSiteEdgeIdentityProjection{}, err
	}
	kindSet, err := canonicalBoundedAdjacencyKinds(ctx, kinds)
	if err != nil {
		return BoundedSiteEdgeIdentityProjection{}, err
	}
	canonical, err := canonicalBoundedAdjacencySites(ctx, sites)
	if err != nil {
		return BoundedSiteEdgeIdentityProjection{}, err
	}
	projection := emptyBoundedSiteEdgeIdentityProjection()
	if v == nil || len(canonical) == 0 || len(kindSet) == 0 {
		return projection, nil
	}
	if v.layer == nil {
		if v.base == nil {
			return projection, nil
		}
		bounded, ok := v.base.(BoundedOutgoingSiteEdgeIdentityReader)
		if !ok {
			return BoundedSiteEdgeIdentityProjection{}, ErrBoundedLocalizationUnavailable
		}
		return bounded.FindOutgoingSiteEdgeIdentitiesBounded(ctx, canonical, kinds, limit)
	}

	baseSites := make([]EdgeSourceSite, 0, len(canonical))
	hiddenSources := make(map[string]bool)
	for _, site := range canonical {
		if v.overlayOwnsIdentity(site.From) && v.layer.NodeByID(site.From) == nil {
			hiddenSources[site.From] = true
			continue
		}
		baseSites = append(baseSites, site)
	}
	baseProjection := emptyBoundedSiteEdgeIdentityProjection()
	if len(baseSites) > 0 && v.base != nil {
		bounded, ok := v.base.(BoundedOutgoingSiteEdgeIdentityReader)
		if !ok {
			return BoundedSiteEdgeIdentityProjection{}, ErrBoundedLocalizationUnavailable
		}
		baseProjection, err = bounded.FindOutgoingSiteEdgeIdentitiesBounded(
			ctx, baseSites, kinds, overlayAdjacencyCompensatedLimit(v.layer, limit),
		)
		if err != nil {
			return BoundedSiteEdgeIdentityProjection{}, err
		}
	}

	budget := &boundedAdjacencyBudget{}
	for start := 0; start < len(canonical); {
		if err := ctx.Err(); err != nil {
			return BoundedSiteEdgeIdentityProjection{}, err
		}
		end := start + 1
		for end < len(canonical) && canonical[end].From == canonical[start].From {
			end++
		}
		from := canonical[start].From
		if hiddenSources[from] {
			start = end
			continue
		}
		overlayBySite, overlayTruncated, scanErr := scanBoundedSiteEdgeIdentities(
			ctx, v.layer.OutEdges(from), canonical[start:end], kindSet, limit, budget,
			func(identity EdgeIdentity) bool { return v.overlayIdentityVisible(identity.To) },
		)
		if scanErr != nil {
			return BoundedSiteEdgeIdentityProjection{}, scanErr
		}
		for _, site := range canonical[start:end] {
			if baseProjection.Truncated[site] || overlayTruncated[site] {
				projection.Truncated[site] = true
				continue
			}
			baseIdentities, truncated, mergeErr := appendBoundedBaseIdentities(
				ctx, baseProjection.BySite[site], limit, budget,
				func(identity EdgeIdentity) bool {
					return identity.From == site.From && identity.Line == site.Line &&
						!v.overlayOwnsBaseEdge(identity.From, identity.FilePath) &&
						v.overlayIdentityVisible(identity.To) && kindRequested(kindSet, identity.Kind)
				},
			)
			if mergeErr != nil {
				return BoundedSiteEdgeIdentityProjection{}, mergeErr
			}
			if truncated {
				projection.Truncated[site] = true
				continue
			}
			identities, capped := mergeBoundedIdentitySlices(baseIdentities, overlayBySite[site], limit)
			if capped {
				projection.Truncated[site] = true
				continue
			}
			if len(identities) > 0 {
				projection.BySite[site] = identities
			}
		}
		start = end
	}
	return projection, nil
}
