package graphview

import (
	"context"

	"github.com/zzet/gortex/internal/graph"
)

var _ graph.BoundedIncomingSourceCandidateReader = (*GenerationLayer)(nil)

// ReadIncomingSourceCandidates reads only the bounded metadata-free source
// projection. It never populates nodesOnce/edgesOnce or caches full adjacency.
func (l *GenerationLayer) ReadIncomingSourceCandidates(ctx context.Context, ids []string, kind graph.EdgeKind) (map[string][]graph.IncomingSourceCandidate, error) {
	return l.handle.ReadIncomingSourceCandidates(ctx, ids, kind)
}
