package graphview

import (
	"context"
	"iter"

	"github.com/zzet/gortex/internal/graph"
)

// The view lease still owns this layer. The borrowed adapter changes only
// physical row reads; immutable masks and identity claims remain this layer's.
func (l *GenerationLayer) WithFileEditingContextLayer(ctx context.Context, fn func(context.Context, graph.OverlayLayerReader) error) error {
	if err := l.checkContractInputRevision(ctx); err != nil {
		return err
	}
	err := l.handle.WithFileEditingContextReader(ctx, func(ctx context.Context, r graph.FileEditingContextRows) error {
		return fn(ctx, &editingContextLayer{OverlayLayerReader: l, layer: l, reader: r})
	})
	if err != nil {
		return err
	}
	return l.checkContractInputRevision(ctx)
}

type editingContextLayer struct {
	graph.OverlayLayerReader
	layer  *GenerationLayer
	reader graph.FileEditingContextRows
}

func (l *editingContextLayer) NodeByID(id string) *graph.Node {
	n := l.reader.GetNode(id)
	if !l.layer.servesNode(n) {
		return nil
	}
	return n
}
func (l *editingContextLayer) FileNodes(path string) []*graph.Node {
	return l.layer.serveNodes(l.reader.GetFileNodes(path))
}
func (l *editingContextLayer) filter(rows []*graph.Edge) []*graph.Edge {
	var out []*graph.Edge
	for _, e := range rows {
		if l.layer.servesEdge(e) {
			out = append(out, e)
		}
	}
	return out
}
func (l *editingContextLayer) OutEdges(id string) []*graph.Edge {
	return l.filter(l.reader.GetOutEdges(id))
}
func (l *editingContextLayer) InEdges(id string) []*graph.Edge {
	return l.filter(l.reader.GetInEdgesByNodeIDs([]string{id})[id])
}

// OwnsNodeIdentity must not delegate to GenerationLayer's cached legacy
// method: its covered-row membership probe calls contextless NodeByID.
func (l *editingContextLayer) OwnsNodeIdentity(id string) bool {
	if id == "" {
		return false
	}
	if l.layer.IsRemovedID(id) {
		return true
	}
	if _, ok := l.layer.detachedIDs[id]; ok {
		return true
	}
	if _, ok := l.layer.claimedIDs[id]; ok {
		return true
	}
	return l.layer.CoversNodeID(id) && l.NodeByID(id) != nil
}
func (l *editingContextLayer) DetachedNodeSummaries() iter.Seq[*graph.Node] {
	return l.layer.DetachedNodeSummaries()
}
func (l *editingContextLayer) detached(match func(graph.Node) bool) []*graph.Node {
	var ids []string
	for _, n := range l.layer.detachedNodes {
		if match(n) {
			ids = append(ids, n.ID)
		}
	}
	rows := l.reader.GetNodesByIDs(ids)
	var out []*graph.Node
	for _, id := range ids {
		if n := rows[id]; l.layer.servesNode(n) {
			out = append(out, n)
		}
	}
	return out
}
func (l *editingContextLayer) DetachedFileNodes(path string) []*graph.Node {
	return l.detached(func(n graph.Node) bool { return n.FilePath == path })
}
func (l *editingContextLayer) DetachedRepoNodes(repo string) []*graph.Node {
	return l.detached(func(n graph.Node) bool { return n.RepoPrefix == repo })
}

func (l *editingContextLayer) filterBatch(rows map[string][]*graph.Edge) map[string][]*graph.Edge {
	out := make(map[string][]*graph.Edge, len(rows))
	for id, edges := range rows {
		out[id] = l.filter(edges)
	}
	return out
}
func (l *editingContextLayer) GetOutEdgesByNodeIDs(ids []string) map[string][]*graph.Edge {
	return l.filterBatch(l.reader.GetOutEdgesByNodeIDs(ids))
}
func (l *editingContextLayer) FileEditingContextInEdgesByNodeIDs(ids []string) map[string][]*graph.Edge {
	return l.filterBatch(l.reader.GetInEdgesByNodeIDs(ids))
}
