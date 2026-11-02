package mcp

import (
	"context"
	"sort"
	"time"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/query"
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
	var result *graph.FileEditingContextResult
	err := withFileStoreRows(ctx, reader, func(rows graph.FileEditingContextRows) error {
		result = graph.FileEditingContextOf(rows, path, kinds)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

// withFileStoreRows gives every section of one file response the SAME 30ms
// cancellable, coherent selected-store scope. No legacy API is a fallback.
func withFileStoreRows(ctx context.Context, reader graph.Reader, consume func(graph.FileEditingContextRows) error) error {
	deadline := time.Now().Add(30 * time.Millisecond)
	if original, ok := ctx.Value(projectedSourceDeadlineKey{}).(time.Time); ok && original.Before(deadline) {
		deadline = original
	}
	ctx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	selected := reader
	var core *contractCoreEdges
	if wrapped, ok := reader.(contractCoreWrapped); ok {
		core = wrapped.contractCore()
		selected = core.Reader
	}
	err := graph.WithFileEditingContextReader(ctx, selected, func(_ context.Context, rows graph.FileEditingContextRows) error {
		if core != nil {
			rows = &editingContextCoreRows{FileEditingContextRows: rows, core: core}
		}
		return consume(rows)
	})
	if err != nil && ctx.Err() != nil {
		return ctx.Err()
	}
	return err
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
func requestLocalFileLayer(ctx context.Context, path string) *graph.OverlayLayer {
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
	return selected
}

func requestLocalEditingContext(ctx context.Context, path string) *graph.FileEditingContextResult {
	selected := requestLocalFileLayer(ctx, path)
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

// fileSummaryContext preserves the full legacy symbols/subgraph/count shape
// while all physical rows, endpoint hydration and importer facts share one
// checked transaction and budget. Counts never use an errorless scalar API.
func fileSummaryContext(ctx context.Context, reader graph.Reader, path string, countsOnly, includeDependents bool) (*query.SubGraph, []string, error) {
	var result *query.SubGraph
	var dependents []string
	err := withFileStoreRows(ctx, reader, func(rows graph.FileEditingContextRows) error {
		result, dependents = fileSummaryRows(rows, path, countsOnly, includeDependents)
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	return result, dependents, nil
}

func fileSummaryRows(rows graph.FileEditingContextRows, path string, countsOnly, includeDependents bool) (*query.SubGraph, []string) {
	nodes := rows.GetFileNodes(path)
	ids := make([]string, 0, len(nodes))
	for _, node := range nodes {
		if node != nil && node.ID != "" {
			ids = append(ids, node.ID)
		}
	}
	outgoing := rows.GetOutEdgesByNodeIDs(ids)
	incoming := rows.GetInEdgesByNodeIDs(ids)
	type edgeKey struct {
		from, to string
		kind     graph.EdgeKind
	}
	seen := make(map[edgeKey]bool)
	var edges []*graph.Edge
	for _, id := range ids {
		for _, batch := range [][]*graph.Edge{outgoing[id], incoming[id]} {
			for _, edge := range batch {
				if edge == nil {
					continue
				}
				key := edgeKey{edge.From, edge.To, edge.Kind}
				if !seen[key] {
					seen[key] = true
					edges = append(edges, edge)
				}
			}
		}
	}
	result := &query.SubGraph{Nodes: nodes, Edges: edges, TotalNodes: len(nodes), TotalEdges: len(edges)}
	if countsOnly {
		result.Edges = nil
	}
	var dependents []string
	if includeDependents {
		fileIncoming, loaded := incoming[path]
		if !loaded {
			fileIncoming = rows.GetInEdgesByNodeIDs([]string{path})[path]
		}
		var sources []string
		for _, edge := range fileIncoming {
			if edge != nil && edge.Kind == graph.EdgeImports {
				sources = append(sources, edge.From)
			}
		}
		sourceNodes := rows.GetNodesByIDs(sources)
		known := make(map[string]bool)
		for _, id := range sources {
			file := id
			if node := sourceNodes[id]; node != nil {
				if node.Kind == graph.KindFile {
					file = node.ID
				} else if node.FilePath != "" {
					file = node.FilePath
				}
			}
			if file != "" && file != path && !known[file] {
				known[file] = true
				dependents = append(dependents, file)
			}
		}
		sort.Strings(dependents)
	}
	return result, dependents
}

func requestLocalFileSummary(ctx context.Context, path string, countsOnly bool) *query.SubGraph {
	layer := requestLocalFileLayer(ctx, path)
	if layer == nil {
		return &query.SubGraph{}
	}
	result, _ := fileSummaryRows(graph.NewOverlaidViewWithLayer(nil, layer), path, countsOnly, false)
	return result
}
