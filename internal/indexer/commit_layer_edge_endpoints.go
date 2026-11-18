package indexer

import "github.com/zzet/gortex/internal/graph"

// EdgeEndpoints serves the endpoint-only projections through the composed
// view the base wraps, when every reader in it can (see graph.EdgeEndpointsOf).
// The live incremental path builds over a commitLayerBase, so without this the
// closure's name accounting, the declared-context seed and the edge-source
// masks would always fall back to full-row reads there.
func (b commitLayerBase) EdgeEndpoints() (graph.EdgeEndpointReader, bool) {
	return graph.EdgeEndpointsOf(b.Reader)
}

var _ graph.EdgeEndpointProvider = commitLayerBase{}

// RecordedEdges serves full edge rows by recording file through the composed
// view the base wraps, when every reader in it can (graph.RecordedEdgesOf).
func (b commitLayerBase) RecordedEdges() (graph.RecordedEdgeReader, bool) {
	return graph.RecordedEdgesOf(b.Reader)
}

var _ graph.RecordedEdgeProvider = commitLayerBase{}
