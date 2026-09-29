package graphview

import "github.com/zzet/gortex/internal/graph"

// The layer side of the composed view's endpoint projections: the
// generation's own rows, reduced to endpoints, with the same context-path
// filter its full-row readers apply (servesEdge). They read the handle's own
// generation through the store's projections, so a composed view over a
// generation chain serves graph.EdgeEndpointsOf without decoding full rows.
var _ graph.OverlayLayerEdgeEndpointReader = (*GenerationLayer)(nil)

// LayerEdgeEndpointsRecordedAt is Edges() filtered to FilePath ∈ paths.
func (l *GenerationLayer) LayerEdgeEndpointsRecordedAt(paths []string) []graph.EdgeEndpointRow {
	return l.serveEndpointRows(l.handle.EdgeEndpointsRecordedAt(paths))
}

// LayerEdgeEndpointsFrom is OutEdges(id) per id, reduced to endpoints and
// restricted to kinds when kinds is non-empty.
func (l *GenerationLayer) LayerEdgeEndpointsFrom(ids []string, kinds []graph.EdgeKind) []graph.EdgeEndpointRow {
	return l.serveEndpointRows(l.handle.EdgeEndpointsFrom(ids, kinds))
}

func (l *GenerationLayer) serveEndpointRows(rows []graph.EdgeEndpointRow) []graph.EdgeEndpointRow {
	if len(l.contextPaths) == 0 || len(rows) == 0 {
		return rows
	}
	out := make([]graph.EdgeEndpointRow, 0, len(rows))
	for _, row := range rows {
		if !l.isContextPath(row.FilePath) {
			out = append(out, row)
		}
	}
	return out
}
