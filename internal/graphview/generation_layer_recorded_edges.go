package graphview

import "github.com/zzet/gortex/internal/graph"

var _ graph.OverlayLayerRecordedEdgeReader = (*GenerationLayer)(nil)

// LayerRecordedEdgesAt is Edges() filtered to FilePath ∈ paths: the
// generation's own full rows by recording file, through the same context-path
// filter its full-row readers apply.
func (l *GenerationLayer) LayerRecordedEdgesAt(paths []string) []*graph.Edge {
	if l.noEdgeRows() {
		return nil
	}
	return l.serveEdges(l.handle.RecordedEdgesAt(paths))
}
