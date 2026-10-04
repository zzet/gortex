package graph

import (
	"context"
	"errors"
)

var ErrOutgoingMetadataUnsupported = errors.New("checked outgoing metadata projection unsupported")

type OutgoingMetadataReader interface {
	GetOutEdgesByNodeIDsWithMetadataContext(context.Context, []string, int) (map[string][]*Edge, bool, error)
}
type OverlayLayerOutgoingMetadataReader interface {
	LayerOutEdgesWithMetadataContext(context.Context, []string, int) (map[string][]*Edge, bool, error)
}

// GetOutEdgesByNodeIDsWithMetadataContext preserves recording-file ownership
// and endpoint masks while retaining all metadata. Unsupported and partial
// physical readers never become an apparently complete empty adjacency.
func GetOutEdgesByNodeIDsWithMetadataContext(ctx context.Context, reader Reader, ids []string, limit int) (map[string][]*Edge, bool, error) {
	if ctx == nil {
		return nil, false, errors.New("outgoing metadata: nil context")
	}
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	ids = uniqueNonEmpty(ids)
	switch r := reader.(type) {
	case *DeltaWriter:
		return GetOutEdgesByNodeIDsWithMetadataContext(ctx, r.view, ids, limit)
	case *OverlaidView:
		return r.outgoingMetadataContext(ctx, ids, limit)
	case Unwrapper:
		if next := r.Unwrap(); next != nil {
			return GetOutEdgesByNodeIDsWithMetadataContext(ctx, next, ids, limit)
		}
	}
	checked, ok := reader.(OutgoingMetadataReader)
	if !ok {
		return nil, false, ErrOutgoingMetadataUnsupported
	}
	return checked.GetOutEdgesByNodeIDsWithMetadataContext(ctx, ids, limit)
}

func (v *OverlaidView) outgoingMetadataContext(ctx context.Context, ids []string, limit int) (map[string][]*Edge, bool, error) {
	base := make(map[string][]*Edge)
	if v.base != nil {
		rows, truncated, err := GetOutEdgesByNodeIDsWithMetadataContext(ctx, v.base, ids, ContractProjectionRowLimit)
		if err != nil {
			return nil, false, err
		}
		if truncated {
			return nil, true, nil
		}
		base = rows
	}
	if v.layer == nil {
		return boundOutgoingMetadata(base, ids, limit)
	}
	checked, ok := v.layer.(OverlayLayerOutgoingMetadataReader)
	if !ok {
		return nil, false, ErrOutgoingMetadataUnsupported
	}
	own, truncated, err := checked.LayerOutEdgesWithMetadataContext(ctx, ids, ContractProjectionRowLimit)
	if err != nil {
		return nil, false, err
	}
	if truncated {
		return nil, true, nil
	}
	projection, ok := v.layer.(OverlayLayerContractProjectionReader)
	if !ok {
		return nil, false, ErrContractProjectionUnsupported
	}
	var endpoints []string
	for _, rows := range base {
		for _, e := range rows {
			for _, id := range []string{e.From, e.To} {
				if v.overlayOwnsIdentity(id) {
					endpoints = append(endpoints, id)
				}
			}
		}
	}
	evidence, err := projection.LayerContractIDProjectionContext(ctx, endpoints)
	if err != nil {
		return nil, false, err
	}
	out := make(map[string][]*Edge, len(ids))
	for _, id := range ids {
		for _, e := range base[id] {
			if v.overlayOwnsBaseEdge(e.From, e.FilePath) || v.overlayClaimsBaseEdge(e) {
				continue
			}
			if (v.overlayOwnsIdentity(e.From) && evidence.SourceNodes[e.From] == nil) || (v.overlayOwnsIdentity(e.To) && evidence.SourceNodes[e.To] == nil) {
				continue
			}
			out[id] = append(out[id], e)
		}
		out[id] = append(out[id], own[id]...)
	}
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	return boundOutgoingMetadata(out, ids, limit)
}
func boundOutgoingMetadata(rows map[string][]*Edge, ids []string, limit int) (map[string][]*Edge, bool, error) {
	if limit <= 0 {
		limit = ContractProjectionRowLimit
	}
	total := 0
	seen := make(map[string]bool, len(ids))
	for _, id := range ids {
		if seen[id] {
			continue
		}
		seen[id] = true
		total += len(rows[id])
		if total > limit {
			return nil, true, nil
		}
	}
	return rows, false, nil
}
func (g *Graph) GetOutEdgesByNodeIDsWithMetadataContext(ctx context.Context, ids []string, limit int) (map[string][]*Edge, bool, error) {
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	g.ResolveMutex().Lock()
	defer g.ResolveMutex().Unlock()
	out := make(map[string][]*Edge, len(ids))
	for _, id := range ids {
		if err := ctx.Err(); err != nil {
			return nil, false, err
		}
		out[id] = g.GetOutEdges(id)
	}
	return boundOutgoingMetadata(out, ids, limit)
}
func (l *OverlayLayer) LayerOutEdgesWithMetadataContext(ctx context.Context, ids []string, limit int) (map[string][]*Edge, bool, error) {
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	out := make(map[string][]*Edge, len(ids))
	for _, id := range ids {
		out[id] = l.OutEdges(id)
	}
	return boundOutgoingMetadata(out, ids, limit)
}
func (l *deltaLayer) LayerOutEdgesWithMetadataContext(ctx context.Context, ids []string, limit int) (map[string][]*Edge, bool, error) {
	return l.work.GetOutEdgesByNodeIDsWithMetadataContext(ctx, ids, limit)
}
