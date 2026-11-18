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

// ContractExtractionEdgeReader supplies full metadata for calls/provides/consumes
// only. Ordinary per-file contract extraction consumes no other edge kinds.
// A reader without this optional projection keeps the checked full-read fallback.
type ContractExtractionEdgeReader interface {
	ContractExtractionEdgesContext(context.Context, []string, int) (map[string][]*Edge, bool, error)
}
type OverlayLayerContractExtractionEdgeReader interface {
	LayerContractExtractionEdgesContext(context.Context, []string, int) (map[string][]*Edge, bool, error)
}

func contractExtractionEdgeKind(kind EdgeKind) bool {
	return kind == EdgeCalls || kind == EdgeProvides || kind == EdgeConsumes
}
func contractExtractionEdgeRows(rows map[string][]*Edge) map[string][]*Edge {
	out := make(map[string][]*Edge, len(rows))
	for id, edges := range rows {
		for _, edge := range edges {
			if contractExtractionEdgeKind(edge.Kind) {
				out[id] = append(out[id], edge)
			}
		}
	}
	return out
}

// ContractExtractionEdgesContext retains complete metadata for the edge kinds
// consumed by per-file extraction. It uses the same selected recording/endpoint
// ownership and checked revision fences as full outgoing evidence.
func ContractExtractionEdgesContext(ctx context.Context, reader Reader, ids []string, limit int) (map[string][]*Edge, bool, error) {
	return outgoingMetadataContext(ctx, reader, ids, limit, true)
}

// GetOutEdgesByNodeIDsWithMetadataContext preserves recording-file ownership
// and endpoint masks while retaining all metadata. Unsupported and partial
// physical readers never become an apparently complete empty adjacency.
func GetOutEdgesByNodeIDsWithMetadataContext(ctx context.Context, reader Reader, ids []string, limit int) (map[string][]*Edge, bool, error) {
	return outgoingMetadataContext(ctx, reader, ids, limit, false)
}

func outgoingMetadataContext(ctx context.Context, reader Reader, ids []string, limit int, extraction bool) (map[string][]*Edge, bool, error) {
	if ctx == nil {
		return nil, false, errors.New("outgoing metadata: nil context")
	}
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	ids = uniqueNonEmpty(ids)
	switch r := reader.(type) {
	case *DeltaWriter:
		return outgoingMetadataContext(ctx, r.view, ids, limit, extraction)
	case *OverlaidView:
		return r.outgoingMetadataSelectionContext(ctx, ids, limit, extraction)
	case Unwrapper:
		if next := r.Unwrap(); next != nil {
			return outgoingMetadataContext(ctx, next, ids, limit, extraction)
		}
	}
	if extraction {
		if selected, ok := reader.(ContractExtractionEdgeReader); ok {
			return selected.ContractExtractionEdgesContext(ctx, ids, limit)
		}
	}
	checked, ok := reader.(OutgoingMetadataReader)
	if !ok {
		return nil, false, ErrOutgoingMetadataUnsupported
	}
	rows, truncated, err := checked.GetOutEdgesByNodeIDsWithMetadataContext(ctx, ids, limit)
	if extraction && err == nil && !truncated {
		rows = contractExtractionEdgeRows(rows)
	}
	return rows, truncated, err
}

func (v *OverlaidView) outgoingMetadataSelectionContext(ctx context.Context, ids []string, limit int, extraction bool) (map[string][]*Edge, bool, error) {
	base := make(map[string][]*Edge)
	if v.base != nil {
		rows, truncated, err := outgoingMetadataContext(ctx, v.base, ids, ContractProjectionRowLimit, extraction)
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
	var own map[string][]*Edge
	var truncated bool
	var err error
	if selected, ok := v.layer.(OverlayLayerContractExtractionEdgeReader); extraction && ok {
		own, truncated, err = selected.LayerContractExtractionEdgesContext(ctx, ids, ContractProjectionRowLimit)
	} else {
		own, truncated, err = checked.LayerOutEdgesWithMetadataContext(ctx, ids, ContractProjectionRowLimit)
		if extraction && err == nil && !truncated {
			own = contractExtractionEdgeRows(own)
		}
	}
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
	// Coarse source/recording claims already decide these rows. Discard them
	// before hydrating endpoint identities that cannot affect the answer.
	// Keep the checked projection below even when no base rows survive.
	remaining := make(map[string][]*Edge, len(base))
	for id, rows := range base {
		for _, edge := range rows {
			if !v.overlayOwnsBaseEdge(edge.From, edge.FilePath) && !v.overlayClaimsBaseEdge(edge) {
				remaining[id] = append(remaining[id], edge)
			}
		}
	}
	base = remaining
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
