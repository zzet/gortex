package graph

import "context"

func memoryContractRepoSeeds(ctx context.Context, repo string, visitNodes func(func(*Node) bool), visitEdges func(func(*Edge, *Node) bool)) (ContractFileProjection, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return ContractFileProjection{}, err
	}
	p := newContractFileProjection()
	budget := ContractProjectionRowLimit
	var failure error
	retain := func() bool {
		if err := ctx.Err(); err != nil {
			failure = err
			return false
		}
		budget--
		if budget < 0 {
			failure = ErrContractProjectionLimit
			return false
		}
		return true
	}
	visitNodes(func(n *Node) bool {
		if err := ctx.Err(); err != nil {
			failure = err
			return false
		}
		if n != nil && n.RepoPrefix == repo && n.Kind == KindContract {
			if !retain() {
				return false
			}
			p.ScalarNodes = append(p.ScalarNodes, n)
		}
		return true
	})
	if failure != nil {
		return ContractFileProjection{}, failure
	}
	visitEdges(func(e *Edge, source *Node) bool {
		if err := ctx.Err(); err != nil {
			failure = err
			return false
		}
		if !isContractOwnerEdge(e) {
			return true
		}
		ownerRepo := ""
		known := source != nil
		if known {
			ownerRepo = source.RepoPrefix
		}
		if value, ok := e.Meta["contract_owner_repo_prefix"].(string); ok {
			ownerRepo = value
			known = true
		}
		// Edge-only physical layers may inherit their source from a lower
		// selected layer. Keep unattributed seeds for checked composition;
		// CompleteContractIDProjection resolves or refuses missing sources.
		if !known || ownerRepo == repo {
			if !retain() {
				return false
			}
			p.OwnerRows = append(p.OwnerRows, RepoEdgeRow{RepoPrefix: ownerRepo, Edge: e})
		}
		return true
	})
	if failure != nil {
		return ContractFileProjection{}, failure
	}
	if err := ctx.Err(); err != nil {
		return ContractFileProjection{}, err
	}
	return p, nil
}

func (g *Graph) LayerContractRepoProjectionContext(ctx context.Context, repo string) (ContractFileProjection, error) {
	g.lockAllRead()
	defer g.unlockAllRead()
	visit := func(yield func(*Node) bool) {
		for _, s := range g.shards {
			if repo == "" {
				for _, n := range s.nodes {
					if n.RepoPrefix == repo && !yield(n) {
						return
					}
				}
			} else {
				for _, n := range s.byRepo[repo] {
					if !yield(n) {
						return
					}
				}
			}
		}
	}
	edges := func(yield func(*Edge, *Node) bool) {
		visit(func(n *Node) bool {
			for _, e := range g.shardFor(n.ID).outEdges[n.ID] {
				if !yield(e, n) {
					return false
				}
			}
			return true
		})
	}
	return memoryContractRepoSeeds(ctx, repo, visit, edges)
}
func (g *Graph) LoadContractRepoProjectionContext(ctx context.Context, repo string) (ContractFileProjection, error) {
	return CompleteContractRepoProjection(ctx, g, repo)
}

func (l *OverlayLayer) LayerContractRepoProjectionContext(ctx context.Context, repo string) (ContractFileProjection, error) {
	// An editor layer has immutable per-file nodes; this is bounded to that
	// physical layer, never the backing graph or any other checkout.
	visit := func(yield func(*Node) bool) {
		for _, n := range l.nodeByID {
			if !yield(n) {
				return
			}
		}
	}
	edges := func(yield func(*Edge, *Node) bool) {
		for id, edges := range l.outEdges {
			for _, e := range edges {
				if !yield(e, l.nodeByID[id]) {
					return
				}
			}
		}
	}
	return memoryContractRepoSeeds(ctx, repo, visit, edges)
}
