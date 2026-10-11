package mcp

import (
	"context"

	"github.com/zzet/gortex/internal/graph"
)

// Node and name reads retain the selected reader's checked, bounded lookup
// capabilities. Only adjacency is filtered here; forwarding traversal engines
// would allow their paths to bypass the contract-derived edge filter.
func (r *contractCoreEdges) FindNodesByNameContext(ctx context.Context, name string) ([]*graph.Node, error) {
	return graph.FindNodesByNameContext(ctx, r.Reader, name)
}

func (r *contractCoreEdges) FindNodesByNameContainingContext(ctx context.Context, substr string, limit int) ([]*graph.Node, error) {
	return graph.FindNodesByNameContainingContext(ctx, r.Reader, substr, limit)
}

// Expose compact filtered lookup only when the selected reader has it. An
// outer overlay otherwise retains its adaptive bounded legacy fetch policy.
type contractCoreFilteredNames struct{ *contractCoreEdges }

type contractCoreBoundedFiles struct {
	*contractCoreEdges
	files graph.BoundedFileNodeReader
}

type contractCoreFilteredNamesBoundedFiles struct{ *contractCoreBoundedFiles }

func newContractCoreEdges(reader graph.Reader, ctx context.Context, ids map[string]bool) graph.Reader {
	return wrapContractCoreEdges(&contractCoreEdges{Reader: reader, ctx: ctx, contractIDs: ids, edgeTiming: coreEdgeTimingFromContext(ctx)})
}

// wrapContractCoreEdges selects the capability variant for core's reader.
func wrapContractCoreEdges(core *contractCoreEdges) graph.Reader {
	reader := core.Reader
	_, filtered := reader.(graph.FilteredContainingNameReader)
	if files, ok := reader.(graph.BoundedFileNodeReader); ok {
		bounded := &contractCoreBoundedFiles{contractCoreEdges: core, files: files}
		if filtered {
			return preserveContractCoreScopedProjection(&contractCoreFilteredNamesBoundedFiles{bounded}, reader, core)
		}
		return preserveContractCoreScopedProjection(bounded, reader, core)
	}
	if filtered {
		return preserveContractCoreScopedProjection(&contractCoreFilteredNames{core}, reader, core)
	}
	return preserveContractCoreScopedProjection(core, reader, core)
}

func (r *contractCoreFilteredNamesBoundedFiles) FindNodesByNameContainingFilteredContext(ctx context.Context, substr string, limit int, filter graph.NameSearchFilter) ([]*graph.Node, error) {
	return graph.FindNodesByNameContainingFilteredContext(ctx, r.Reader, substr, limit, filter)
}

// Keep localization optional and bounded: a selected reader without this
// capability must still fail closed instead of hydrating legacy file rows.
func (r *contractCoreBoundedFiles) FindFileNodesBounded(ctx context.Context, path string, scope graph.LocalizationNodeScope, limit int) (graph.BoundedNodeProjection, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return graph.BoundedNodeProjection{}, err
	}
	page, err := r.files.FindFileNodesBounded(ctx, path, scope, limit)
	if err != nil {
		return graph.BoundedNodeProjection{}, err
	}
	if err := ctx.Err(); err != nil {
		return graph.BoundedNodeProjection{}, err
	}
	return page, nil
}

func (r *contractCoreFilteredNames) FindNodesByNameContainingFilteredContext(ctx context.Context, substr string, limit int, filter graph.NameSearchFilter) ([]*graph.Node, error) {
	return graph.FindNodesByNameContainingFilteredContext(ctx, r.Reader, substr, limit, filter)
}

func (r *contractCoreEdges) GetNodeContext(ctx context.Context, id string) (*graph.Node, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if contextual, ok := r.Reader.(interface {
		GetNodeContext(context.Context, string) (*graph.Node, error)
	}); ok {
		node, err := contextual.GetNodeContext(ctx, id)
		if err != nil {
			return nil, err
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return node, nil
	}
	node := r.GetNode(id)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return node, nil
}

// GetFileNodesContext keeps the selected reader's deadline-aware file lookup
// for diff joins in internal/analysis, which cannot reach the accessor. File
// nodes read no edges, and a selected reader without the lookup answers as
// those callers fall back: GetFileNodes.
func (r *contractCoreEdges) GetFileNodesContext(ctx context.Context, path string) []*graph.Node {
	if ctx == nil {
		ctx = context.Background()
	}
	if contextual, ok := r.Reader.(interface {
		GetFileNodesContext(context.Context, string) []*graph.Node
	}); ok {
		return contextual.GetFileNodesContext(ctx, path)
	}
	return r.GetFileNodes(path)
}

func (r *contractCoreEdges) GetNodesByIDsContext(ctx context.Context, ids []string) (map[string]*graph.Node, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if contextual, ok := r.Reader.(interface {
		GetNodesByIDsContext(context.Context, []string) (map[string]*graph.Node, error)
	}); ok {
		nodes, err := contextual.GetNodesByIDsContext(ctx, ids)
		if err != nil {
			return nil, err
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return nodes, nil
	}
	nodes := r.GetNodesByIDs(ids)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return nodes, nil
}

// Preserve compact identity scans through the exact selected reader. The graph
// helpers retain legacy fallbacks without widening the selected node set, and
// the scanner owns its borrowed page and callback lock-release contract.
func (r *contractCoreEdges) ScanNodeSearchKeys(ctx context.Context, pageSize int, yield func([]graph.NodeSearchKey) bool) error {
	return graph.ScanNodeSearchKeys(ctx, r.Reader, pageSize, yield)
}

func (r *contractCoreEdges) AllNodesLight() []*graph.Node {
	return graph.AllNodesLight(r.Reader)
}

func (r *contractCoreEdges) GetNodeKindsByIDsContext(ctx context.Context, ids []string) (map[string]graph.NodeKindRow, error) {
	return graph.GetNodeKindsByIDsContext(ctx, r.Reader, ids)
}

// NodeIDsByKinds keeps ID-only kind scans (betweenness, hotspot candidates)
// off AllNodes. It reads no edges, and a selected reader without the
// projection answers from its own kind iterators, so every shape honours it.
func (r *contractCoreEdges) NodeIDsByKinds(kinds []graph.NodeKind) []string {
	if scan, ok := r.Reader.(graph.NodeIDsByKinds); ok {
		return scan.NodeIDsByKinds(kinds)
	}
	seen := make(map[graph.NodeKind]bool, len(kinds))
	var ids []string
	for _, kind := range kinds {
		if kind == "" || seen[kind] {
			continue
		}
		seen[kind] = true
		for node := range r.NodesByKind(kind) {
			if node != nil && node.ID != "" {
				ids = append(ids, node.ID)
			}
		}
	}
	return ids
}

func (r *baseGraphReader) GetNodeKindsByIDsContext(ctx context.Context, ids []string) (map[string]graph.NodeKindRow, error) {
	rows, err := graph.GetNodeKindsByIDsContext(ctx, r.base, ids)
	if err != nil {
		return nil, err
	}
	for id, row := range rows {
		if !r.inScope(&graph.Node{Kind: row.Kind, FilePath: row.FilePath, RepoPrefix: row.RepoPrefix}) {
			delete(rows, id)
		}
	}
	return rows, nil
}
