package graph

import "context"

func newContractFileProjection() ContractFileProjection {
	return ContractFileProjection{FileNodes: make(map[string][]*Node), Targets: make(map[string]*Node), SourceNodes: make(map[string]*Node)}
}
func isContractOwnerEdge(e *Edge) bool {
	return e != nil && (e.Kind == EdgeProvides || e.Kind == EdgeConsumes || e.Kind == EdgeHandlesRoute)
}

// memoryContractProjection is used only by the in-memory physical graph/layer.
// SQL-backed generations implement the same reads using file/adjacency indexes.
func memoryContractProjection(ctx context.Context, paths, ids []string, fileNodes func(string) []*Node, nodeByID func(string) *Node, inEdges, outEdges func(string) []*Edge, recorded func(func(*Edge) bool)) (ContractFileProjection, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return ContractFileProjection{}, err
	}
	p := newContractFileProjection()
	budget := ContractProjectionRowLimit
	retain := func() bool { budget--; return budget >= 0 }
	pathSet := make(map[string]bool, len(paths))
	for _, path := range paths {
		if pathSet[path] {
			continue
		}
		pathSet[path] = true
		for _, n := range fileNodes(path) {
			if !retain() {
				return ContractFileProjection{}, ErrContractProjectionLimit
			}
			p.FileNodes[path] = append(p.FileNodes[path], n)
			if n.Kind == KindContract {
				p.ScalarNodes = append(p.ScalarNodes, n)
			}
		}
	}
	if len(paths) > 0 {
		recorded(func(e *Edge) bool {
			if ctx.Err() != nil {
				return false
			}
			if pathSet[e.FilePath] && isContractOwnerEdge(e) {
				if !retain() {
					return false
				}
				p.OwnerRows = append(p.OwnerRows, RepoEdgeRow{Edge: e})
			}
			return true
		})
	}
	seen := make(map[string]bool, len(ids))
	for _, id := range ids {
		if seen[id] || id == "" {
			continue
		}
		seen[id] = true
		if n := nodeByID(id); n != nil {
			if !retain() {
				return ContractFileProjection{}, ErrContractProjectionLimit
			}
			p.SourceNodes[id] = n
			if n.Kind == KindContract {
				p.Targets[id] = n
			}
		}
		for _, bucket := range []struct {
			edges    []*Edge
			outgoing bool
		}{{inEdges(id), false}, {outEdges(id), true}} {
			for _, e := range bucket.edges {
				if !isContractOwnerEdge(e) {
					continue
				}
				if !retain() {
					return ContractFileProjection{}, ErrContractProjectionLimit
				}
				row := RepoEdgeRow{Edge: e}
				if bucket.outgoing {
					p.OutgoingOwnerRows = append(p.OutgoingOwnerRows, row)
				} else {
					p.OwnerRows = append(p.OwnerRows, row)
				}
			}
		}
	}
	if err := ctx.Err(); err != nil {
		return ContractFileProjection{}, err
	}
	if budget < 0 {
		return ContractFileProjection{}, ErrContractProjectionLimit
	}
	return p, nil
}

func (g *Graph) memoryContractProjection(ctx context.Context, paths, ids []string) (ContractFileProjection, error) {
	g.lockAllRead()
	defer g.unlockAllRead()
	node := func(id string) *Node { return g.shardFor(id).nodes[id] }
	file := func(path string) []*Node {
		var out []*Node
		for _, s := range g.shards {
			out = append(out, s.byFile[path]...)
		}
		return out
	}
	incoming := func(id string) []*Edge { return g.shardFor(id).inEdges[id] }
	outgoing := func(id string) []*Edge { return g.shardFor(id).outEdges[id] }
	recorded := func(yield func(*Edge) bool) {
		for _, s := range g.shards {
			for _, edges := range s.outEdges {
				for _, e := range edges {
					if !yield(e) {
						return
					}
				}
			}
		}
	}
	return memoryContractProjection(ctx, paths, ids, file, node, incoming, outgoing, recorded)
}
func (g *Graph) LayerContractFileProjectionContext(ctx context.Context, _ string, paths []string) (ContractFileProjection, error) {
	return g.memoryContractProjection(ctx, paths, nil)
}
func (g *Graph) LayerContractIDProjectionContext(ctx context.Context, ids []string) (ContractFileProjection, error) {
	return g.memoryContractProjection(ctx, nil, ids)
}
func (g *Graph) LoadContractFileProjectionContext(ctx context.Context, repo string, paths []string) (ContractFileProjection, error) {
	return CompleteContractFileProjection(ctx, g, repo, paths)
}
func (g *Graph) LoadContractIDProjectionContext(ctx context.Context, ids []string) (ContractFileProjection, error) {
	return CompleteContractIDProjection(ctx, g, ids)
}
func (l *OverlayLayer) LayerContractFileProjectionContext(ctx context.Context, _ string, paths []string) (ContractFileProjection, error) {
	return memoryContractProjection(ctx, paths, nil, l.FileNodes, l.NodeByID, l.InEdges, l.OutEdges, func(yield func(*Edge) bool) {
		for _, edges := range l.outEdges {
			for _, e := range edges {
				if !yield(e) {
					return
				}
			}
		}
	})
}
func (l *OverlayLayer) LayerContractIDProjectionContext(ctx context.Context, ids []string) (ContractFileProjection, error) {
	return memoryContractProjection(ctx, nil, ids, l.FileNodes, l.NodeByID, l.InEdges, l.OutEdges, nil)
}
