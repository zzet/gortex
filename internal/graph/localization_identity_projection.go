package graph

import (
	"context"
	"iter"
	"sort"
)

// OverlayDetachedFileSummaryReader enumerates only the requested file's
// captured marker summaries. It must not scan all upper rows or all markers.
type OverlayDetachedFileSummaryReader interface {
	DetachedFileNodeSummaries(string) iter.Seq[*Node]
}

type OverlayLocalizationIdentityReader interface {
	LocalizationIdentityNodesContext(context.Context, []string, bool) ([]*Node, error)
}

func (v *OverlaidView) findUncoveredFileNodesBounded(ctx context.Context, filePath string, scope LocalizationNodeScope, limit int) (BoundedNodeProjection, error) {
	var baseReader BoundedFileNodeReader
	if v.base != nil {
		var ok bool
		baseReader, ok = v.base.(BoundedFileNodeReader)
		if !ok {
			return BoundedNodeProjection{}, ErrBoundedLocalizationUnavailable
		}
	}
	if v.layer == nil {
		if baseReader == nil {
			return BoundedNodeProjection{}, nil
		}
		return baseReader.FindFileNodesBounded(ctx, filePath, scope, limit)
	}
	pageSize := limit + 1
	kept := make([]*Node, 0, pageSize)
	visible := 0
	var ids []string
	preScope := scope
	preScope.ExcludeTests = false
	if reader, ok := v.layer.(OverlayDetachedFileSummaryReader); ok {
		inspected := 0
		for node := range reader.DetachedFileNodeSummaries(filePath) {
			inspected++
			if inspected > overlayExactNameInspectionLimit {
				return BoundedNodeProjection{}, &BoundedLocalizationLimitError{Resource: "carried file identity entries", Limit: overlayExactNameInspectionLimit}
			}
			if inspected&127 == 0 {
				if err := ctx.Err(); err != nil {
					return BoundedNodeProjection{}, err
				}
			}
			if err := scope.CheckInspection(inspected); err != nil {
				return BoundedNodeProjection{}, err
			}
			if node == nil || node.FilePath != filePath || !preScope.Allows(node) {
				continue
			}
			if scope.ExcludeTests {
				ids = append(ids, node.ID)
			} else {
				visible++
				kept = insertBoundedLocalizationNode(kept, node, pageSize)
			}
		}
	} else if _, hasDetached := v.layer.(OverlayDetachedNodeReader); hasDetached {
		// Do not fall back to a global marker scan or whole-file hydration.
		return BoundedNodeProjection{}, ErrBoundedLocalizationUnavailable
	}
	if len(ids) > 0 {
		reader, ok := v.layer.(OverlayLocalizationIdentityReader)
		if !ok {
			return BoundedNodeProjection{}, ErrBoundedLocalizationUnavailable
		}
		sort.Strings(ids)
		// This is a per-query budget, not a 256-row availability gate. Each
		// storage request stays small, and only is_test survives decoding.
		for start := 0; start < len(ids); start += overlayDetachedShadowLimit {
			if err := ctx.Err(); err != nil {
				return BoundedNodeProjection{}, err
			}
			end := min(start+overlayDetachedShadowLimit, len(ids))
			rows, err := reader.LocalizationIdentityNodesContext(ctx, ids[start:end], true)
			if err != nil {
				return BoundedNodeProjection{}, err
			}
			for _, node := range rows {
				if node == nil || node.FilePath != filePath || !scope.Allows(node) {
					continue
				}
				visible++
				kept = insertBoundedLocalizationNode(kept, node, pageSize)
			}
			// IDs and each checked result are ascending, so later batches
			// cannot displace a retained lower-ID sentinel.
			if visible >= pageSize {
				break
			}
		}
	}
	if baseReader != nil {
		baseScope := scope.withIdentityExcluder(v.layer.OwnsNodeIdentity)
		basePage, err := baseReader.FindFileNodesBounded(ctx, filePath, baseScope, limit)
		if err != nil {
			return BoundedNodeProjection{}, err
		}
		for _, node := range basePage.Nodes {
			if err := ctx.Err(); err != nil {
				return BoundedNodeProjection{}, err
			}
			if node == nil {
				continue
			}
			if v.layer.OwnsNodeIdentity(node.ID) {
				return BoundedNodeProjection{}, ErrBoundedLocalizationUnavailable
			}
			visible++
			kept = insertBoundedLocalizationNode(kept, node, pageSize)
		}
		if basePage.Truncated && visible <= limit {
			visible = pageSize
		}
	}
	if err := ctx.Err(); err != nil {
		return BoundedNodeProjection{}, err
	}
	if visible > pageSize {
		visible = pageSize
	}
	if len(kept) > limit {
		kept = kept[:limit]
	}
	return BoundedNodeProjection{Nodes: localizationNodeSummaries(kept), Total: visible, Truncated: visible > limit}, nil
}
