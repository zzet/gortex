package mcp

import (
	"context"
	"fmt"
	"iter"
	"time"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/search/rerank"
)

// A contract analysis owns identities and derived edges, never source files.
// Thus a fresh route cannot hide the handler's current core declarations.
type contractIdentityLayer struct{ *graph.OverlayLayer }

func (*contractIdentityLayer) HasFile(string) bool      { return false }
func (*contractIdentityLayer) IsTombstone(string) bool  { return false }
func (*contractIdentityLayer) FilePaths() []string      { return nil }
func (*contractIdentityLayer) CoversNodeID(string) bool { return false }

// contractCoreEdges retains core adjacency except the component replaced by
// the selected analysis. Provides/Consumes are removed only for contract
// endpoints; SQL/table dataflow using those same kinds remains core data.
type contractCoreEdges struct {
	graph.Reader
	ctx         context.Context
	contractIDs map[string]bool
	edgeTiming  *rerank.CoreEdgeTiming
}

func (r *contractCoreEdges) visible(edge *graph.Edge) bool {
	if edge == nil || r.contractIDs[edge.From] || r.contractIDs[edge.To] {
		return false
	}
	switch edge.Kind {
	case graph.EdgeMatches, graph.EdgeBridges, graph.EdgeHandlesRoute, graph.EdgeReadsConfig:
		return false
	}
	return edge.Meta["via"] != "spring.Bean"
}

func (r *contractCoreEdges) filter(rows []*graph.Edge) []*graph.Edge {
	// Classify existing endpoint metadata in one local batch; this never
	// captures contract inputs or opens an analysis attachment.
	var ids []string
	for _, edge := range rows {
		if r.visible(edge) {
			ids = append(ids, edge.From, edge.To)
		}
	}
	var kinds map[string]graph.NodeKindRow
	var excluded map[string]struct{}
	if r.contractIDs == nil && len(ids) > 0 {
		var started time.Time
		if t := r.edgeTiming; t != nil {
			t.EndpointReads++
			t.EndpointIDs += len(ids)
			unique := make(map[string]struct{}, len(ids))
			for _, id := range ids {
				if id != "" {
					unique[id] = struct{}{}
				}
			}
			t.EndpointDistinctIDs += len(unique)
		}
		if hasContractCoreReadErrors(r.ctx) {
			if r.edgeTiming != nil {
				r.edgeTiming.CheckedClassifiers++
				started = time.Now()
			}
			var err error
			excluded, err = graph.GetNodeIDsByKindsContext(r.ctx, r.Reader, ids, []graph.NodeKind{graph.KindContract, graph.KindContractBridge, graph.KindConfigKey})
			if r.edgeTiming != nil {
				r.edgeTiming.EndpointLookup += time.Since(started)
			}
			if err != nil {
				recordContractCoreReadError(r.ctx, err)
				return nil
			}
		} else {
			// Potential writers keep the previous errorless classifier. A late
			// read refusal must never discard their committed mutation receipt.
			if r.edgeTiming != nil {
				r.edgeTiming.LegacyClassifiers++
				started = time.Now()
			}
			nodes := r.GetNodesByIDs(ids)
			if r.edgeTiming != nil {
				r.edgeTiming.EndpointLookup += time.Since(started)
			}
			kinds = make(map[string]graph.NodeKindRow, len(nodes))
			for id, node := range nodes {
				if node != nil {
					kinds[id] = graph.NodeKindRow{Kind: node.Kind}
				}
			}
		}
	}
	owned := func(id string) bool {
		if excluded != nil {
			_, found := excluded[id]
			return found
		}
		row, found := kinds[id]
		return found && (row.Kind == graph.KindContract || row.Kind == graph.KindContractBridge || row.Kind == graph.KindConfigKey)
	}
	result := make([]*graph.Edge, 0, len(rows))
	for _, edge := range rows {
		if r.visible(edge) && !owned(edge.From) && !owned(edge.To) {
			result = append(result, edge)
		}
	}
	return result
}

func (r *contractCoreEdges) GetOutEdges(id string) []*graph.Edge {
	return r.filter(r.Reader.GetOutEdges(id))
}
func (r *contractCoreEdges) GetInEdges(id string) []*graph.Edge {
	return r.filter(r.Reader.GetInEdges(id))
}
func (r *contractCoreEdges) GetOutEdgesByNodeIDs(ids []string) map[string][]*graph.Edge {
	if r.edgeTiming == nil {
		return r.filterBatch(r.Reader.GetOutEdgesByNodeIDs(ids))
	}
	t := r.edgeTiming
	if t.RawReads == 0 {
		t.ReaderType = fmt.Sprintf("%T", r.Reader)
	}
	started := time.Now()
	rows := r.Reader.GetOutEdgesByNodeIDs(ids)
	t.RawRead += time.Since(started)
	t.RawReads++
	for _, edges := range rows {
		t.RawRows += len(edges)
	}
	started = time.Now()
	filtered := r.filterBatch(rows)
	t.Filter += time.Since(started)
	for _, edges := range filtered {
		t.KeptRows += len(edges)
	}
	return filtered
}
func (r *contractCoreEdges) GetInEdgesByNodeIDs(ids []string) map[string][]*graph.Edge {
	return r.filterBatch(r.Reader.GetInEdgesByNodeIDs(ids))
}
func (r *contractCoreEdges) filterBatch(rows map[string][]*graph.Edge) map[string][]*graph.Edge {
	var all []*graph.Edge
	for _, edges := range rows {
		all = append(all, edges...)
	}
	visible := make(map[*graph.Edge]bool)
	for _, edge := range r.filter(all) {
		visible[edge] = true
	}
	result := make(map[string][]*graph.Edge, len(rows))
	for id, edges := range rows {
		for _, edge := range edges {
			if visible[edge] {
				result[id] = append(result[id], edge)
			}
		}
	}
	return result
}
func (r *contractCoreEdges) AllEdges() []*graph.Edge { return r.filter(r.Reader.AllEdges()) }
func (r *contractCoreEdges) EdgesByKind(kind graph.EdgeKind) iter.Seq[*graph.Edge] {
	return func(yield func(*graph.Edge) bool) {
		if r.ctx != nil && r.ctx.Err() != nil {
			return
		}
		batch := make([]*graph.Edge, 0, 256)
		flush := func() bool {
			if r.ctx != nil && r.ctx.Err() != nil {
				return false
			}
			for _, edge := range r.filter(batch) {
				if !yield(edge) {
					return false
				}
			}
			batch = batch[:0]
			return true
		}
		for edge := range r.Reader.EdgesByKind(kind) {
			batch = append(batch, edge)
			if len(batch) == cap(batch) && !flush() {
				return
			}
		}
		flush()
	}
}

func (binding *contractAnalysisContext) composedReader(ctx context.Context, core graph.Reader) (graph.Reader, error) {
	binding.composedOnce.Do(func() {
		analysis, err := binding.analysisReader(ctx)
		if err != nil {
			binding.composedErr = err
			return
		}
		layer := &contractIdentityLayer{graph.NewOverlayLayer()}
		ids := make(map[string]bool)
		for _, kind := range []graph.NodeKind{graph.KindContract, graph.KindContractBridge, graph.KindConfigKey} {
			for node := range core.NodesByKind(kind) {
				if err := ctx.Err(); err != nil {
					binding.composedErr = err
					return
				}
				ids[node.ID] = true
				if analysis.GetNode(node.ID) == nil {
					layer.MarkRemoved(node.Name, node.ID)
				}
			}
			for node := range analysis.NodesByKind(kind) {
				ids[node.ID] = true
				layer.AddNode(node.FilePath, node)
			}
		}
		for _, edge := range analysis.AllEdges() {
			layer.AddEdge(edge)
		}
		binding.composedLayer = layer
		binding.composed = graph.NewOverlaidViewWithLayer(newContractCoreEdges(core, ctx, ids), layer)
	})
	if binding.composedErr != nil {
		binding.recordReadError(binding.composedErr)
		return nil, binding.composedErr
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return binding.composed, nil
}
