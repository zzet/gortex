package mcp

import (
	"context"
	"time"

	"github.com/zzet/gortex/internal/graph"
)

// contractCoreEditingContext serves the get_editing_context fast path through
// a contract-core wrapper. The selected reader's own FileEditingContext reads
// raw imports and calls, so the projection is recomputed over the wrapper's
// filtered adjacency instead: the same batched reads, with contract, bridge
// and config-key endpoints and every other edge the filter hides left out.
type contractCoreEditingContext struct{ reader graph.Reader }

func (c contractCoreEditingContext) FileEditingContext(filePath string, kinds []graph.NodeKind) *graph.FileEditingContextResult {
	return graph.FileEditingContextOf(c.reader, filePath, kinds)
}

// fileEditingContextFor resolves the editing-context fast path for a request
// reader. Through a contract-core wrapper it is offered only when the selected
// reader has one, so each request shape keeps the path it takes without the
// runtime.
func fileEditingContextFor(reader graph.Reader) (graph.FileEditingContext, bool) {
	if wrapped, ok := reader.(contractCoreWrapped); ok {
		if _, fast := wrapped.contractCore().Reader.(graph.FileEditingContext); !fast {
			return nil, false
		}
		return contractCoreEditingContext{reader: reader}, true
	}
	fc, ok := reader.(graph.FileEditingContext)
	return fc, ok
}

// fileEditingContextContext performs the entire bundle inside the selected
// reader's checked scope. A refused or canceled scope never falls through to
// errorless per-symbol reads. The RepoView lease remains owned by the request.
func fileEditingContextContext(ctx context.Context, reader graph.Reader, path string, kinds []graph.NodeKind) (*graph.FileEditingContextResult, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Millisecond)
	defer cancel()
	selected := reader
	var core *contractCoreEdges
	if wrapped, ok := reader.(contractCoreWrapped); ok {
		core = wrapped.contractCore()
		selected = core.Reader
	}
	var result *graph.FileEditingContextResult
	err := graph.WithFileEditingContextReader(ctx, selected, func(_ context.Context, rows graph.FileEditingContextRows) error {
		if core != nil {
			rows = &editingContextCoreRows{FileEditingContextRows: rows, core: core}
		}
		result = graph.FileEditingContextOf(rows, path, kinds)
		return nil
	})
	if err != nil {
		if deadlineErr := ctx.Err(); deadlineErr != nil {
			return nil, deadlineErr
		}
		return nil, err
	}
	return result, nil
}

type editingContextCoreRows struct {
	graph.FileEditingContextRows
	core *contractCoreEdges
}

func (r *editingContextCoreRows) filter(rows []*graph.Edge) []*graph.Edge {
	var ids []string
	for _, edge := range rows {
		if r.core.visible(edge) {
			ids = append(ids, edge.From, edge.To)
		}
	}
	var nodes map[string]*graph.Node
	if r.core.contractIDs == nil {
		nodes = r.GetNodesByIDs(ids)
	}
	owned := func(id string) bool {
		n := nodes[id]
		return n != nil && (n.Kind == graph.KindContract || n.Kind == graph.KindContractBridge || n.Kind == graph.KindConfigKey)
	}
	var out []*graph.Edge
	for _, edge := range rows {
		if r.core.visible(edge) && !owned(edge.From) && !owned(edge.To) {
			out = append(out, edge)
		}
	}
	return out
}
func (r *editingContextCoreRows) GetOutEdges(id string) []*graph.Edge {
	return r.filter(r.FileEditingContextRows.GetOutEdges(id))
}
func (r *editingContextCoreRows) filterBatch(rows map[string][]*graph.Edge) map[string][]*graph.Edge {
	var all []*graph.Edge
	for _, edges := range rows {
		all = append(all, edges...)
	}
	visible := make(map[*graph.Edge]bool)
	for _, edge := range r.filter(all) {
		visible[edge] = true
	}
	out := make(map[string][]*graph.Edge, len(rows))
	for id, edges := range rows {
		for _, edge := range edges {
			if visible[edge] {
				out[id] = append(out[id], edge)
			}
		}
	}
	return out
}

func (r *editingContextCoreRows) GetOutEdgesByNodeIDs(ids []string) map[string][]*graph.Edge {
	return r.filterBatch(r.FileEditingContextRows.GetOutEdgesByNodeIDs(ids))
}
func (r *editingContextCoreRows) GetInEdgesByNodeIDs(ids []string) map[string][]*graph.Edge {
	return r.filterBatch(r.FileEditingContextRows.GetInEdgesByNodeIDs(ids))
}

// requestLocalEditingContext borrows only the request's already captured
// in-memory file layer. It never refetches the selected store, parses bytes,
// or turns unavailable cross-file relations into an apparently complete set.
func requestLocalEditingContext(ctx context.Context, path string) *graph.FileEditingContextResult {
	var selected *graph.OverlayLayer
	for _, candidate := range requestProjectionLayers(ctx) {
		if layer, ok := candidate.(*graph.OverlayLayer); ok && layer.HasFile(path) {
			selected = layer
		}
	}
	if editor := OverlayViewFromContext(ctx); editor != nil {
		if layer := editor.Layer(); layer != nil && layer.HasFile(path) {
			selected = layer
		}
	}
	result := &graph.FileEditingContextResult{}
	if selected == nil {
		return result
	}
	for _, node := range selected.FileNodes(path) {
		if node.Kind == graph.KindFile {
			result.FileNode = node
		} else {
			result.Defines = append(result.Defines, node)
		}
	}
	if result.FileNode != nil {
		for _, edge := range selected.OutEdges(result.FileNode.ID) {
			if edge != nil && edge.Kind == graph.EdgeImports {
				result.Imports = append(result.Imports, edge)
			}
		}
	}
	return result
}
