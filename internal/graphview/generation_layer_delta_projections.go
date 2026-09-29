package graphview

import (
	"iter"

	"github.com/zzet/gortex/internal/graph"
)

// Generation-scoped projections for a per-file delta composing over this
// layer. Each answers from the generation's own rows through the handle's
// indexed, generation-scoped queries, filtered by the layer's serving rule,
// and never materializes the whole layer (Nodes / Edges / NamedNodes do). A
// working tree's layers below a delta can be whole commit layers of a
// diverged branch; loading one per edit is a whole-generation read.

var _ graph.OverlayLayerProjectionReader = (*GenerationLayer)(nil)

// LayerNodesInScope implements graph.OverlayLayerProjectionReader.
func (l *GenerationLayer) LayerNodesInScope(repoPrefixes, filePaths []string, light bool, kinds ...graph.NodeKind) []*graph.Node {
	if l.noNodeRows() {
		return nil
	}
	var seq iter.Seq[*graph.Node]
	if light {
		seq = l.handle.NodesLightInScopeSeq(repoPrefixes, filePaths)
	} else {
		seq = l.handle.NodesInScopeSeq(repoPrefixes, filePaths, kinds...)
	}
	var out []*graph.Node
	for n := range seq {
		if l.servesNode(n) {
			out = append(out, n)
		}
	}
	return out
}

// LayerEdgesByKinds implements graph.OverlayLayerProjectionReader.
func (l *GenerationLayer) LayerEdgesByKinds(kinds []graph.EdgeKind) []*graph.Edge {
	if l.noEdgeRows() {
		return nil
	}
	var out []*graph.Edge
	for _, kind := range kinds {
		for e := range l.handle.EdgesByKind(kind) {
			if l.servesEdge(e) {
				out = append(out, e)
			}
		}
	}
	return out
}

// LayerInEdgesByNodeIDs implements graph.OverlayLayerProjectionReader.
func (l *GenerationLayer) LayerInEdgesByNodeIDs(ids []string) map[string][]*graph.Edge {
	if len(ids) == 0 || l.noEdgeRows() {
		return nil
	}
	batch := l.handle.GetInEdgesByNodeIDs(ids)
	out := make(map[string][]*graph.Edge, len(batch))
	for id, edges := range batch {
		if served := l.serveEdges(edges); len(served) > 0 {
			out[id] = served
		}
	}
	return out
}

// LayerOutEdgesByNodeIDs implements graph.OverlayLayerProjectionReader.
func (l *GenerationLayer) LayerOutEdgesByNodeIDs(ids []string) map[string][]*graph.Edge {
	return l.GetOutEdgesByNodeIDs(ids)
}

// LayerNodesByNames implements graph.OverlayLayerProjectionReader.
func (l *GenerationLayer) LayerNodesByNames(names []string) map[string][]*graph.Node {
	if len(names) == 0 || l.noNodeRows() {
		return nil
	}
	batch := l.handle.FindNodesByNames(names)
	out := make(map[string][]*graph.Node, len(batch))
	for name, nodes := range batch {
		if served := l.serveNodes(nodes); len(served) > 0 {
			out[name] = served
		}
	}
	return out
}

// LayerRepoEdgesByKinds implements graph.OverlayLayerProjectionReader.
func (l *GenerationLayer) LayerRepoEdgesByKinds(repoPrefixes []string, kinds []graph.EdgeKind) []graph.RepoEdgeRow {
	if l.noEdgeRows() {
		return nil
	}
	var out []graph.RepoEdgeRow
	for _, row := range l.handle.RepoEdgesByKinds(repoPrefixes, kinds) {
		if l.servesEdge(row.Edge) {
			out = append(out, row)
		}
	}
	return out
}
