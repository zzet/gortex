package query

import (
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/search"
)

// WithLayers binds the ownership claims of the reader already selected on
// this clone. Reader wrappers need not expose concrete overlay implementations.
// The slice is copied; callers retain ownership of their slice storage.
func (e *Engine) WithLayers(layers []graph.OverlayLayerReader) *Engine {
	if e == nil {
		return nil
	}
	clone := *e
	clone.layers = append([]graph.OverlayLayerReader(nil), layers...)
	return &clone
}

func (e *Engine) bundleNodeNeedsReader(node *graph.Node) bool {
	if e.viewLayersActive() && len(e.layers) == 0 {
		return true
	}
	for _, layer := range e.layers {
		if layer != nil && (layer.HasFile(node.FilePath) || layer.CoversNodeID(node.ID) || layer.OwnsNodeIdentity(node.ID)) {
			return true
		}
	}
	return false
}

func (e *Engine) bundleEdgesNeedReader(bundle search.SymbolBundle) bool {
	node := bundle.Node
	if e.bundleNodeNeedsReader(node) {
		return true
	}
	for _, layer := range e.layers {
		if layer == nil {
			continue
		}
		// Base bundles cannot carry adjacency introduced by an upper layer.
		if layer.OwnsOutEdges(node.ID) || len(layer.OutEdges(node.ID)) > 0 || len(layer.InEdges(node.ID)) > 0 {
			return true
		}
		for _, edges := range [][]*graph.Edge{bundle.InEdges, bundle.OutEdges} {
			for _, edge := range edges {
				if edge == nil {
					continue
				}
				if layer.HasFile(edge.FilePath) || layer.CoversNodeID(edge.From) || layer.CoversNodeID(edge.To) || layer.OwnsNodeIdentity(edge.From) || layer.OwnsNodeIdentity(edge.To) || layer.OwnsOutEdges(edge.From) {
					return true
				}
			}
		}
	}
	return false
}
