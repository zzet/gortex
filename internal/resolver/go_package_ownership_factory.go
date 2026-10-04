package resolver

import (
	"context"
	"iter"
	"sort"
	"strings"

	"github.com/zzet/gortex/internal/graph"
)

// GoPackageOwnershipFactory prepares target-bound lookups before resolver
// workers start. files is the actual resolver graph's rich, repository-scoped
// file inventory for exactly repoPrefixes, not a parser-admission inventory.
// The factory must exhaust that iterator before doing source I/O; neither the
// iterator, source handles nor mutable Nodes may escape in the returned maps.
// Missing/nil lookups mean Unknown, not Different. A factory is called at most
// once per requested repository per pass epoch, and again after interleaving
// invalidates that epoch. It must honor ctx and return no partial map on error.
type GoPackageOwnershipFactory func(context.Context, []string, iter.Seq[*graph.Node]) (map[string]GoPackageOwnershipLookup, error)

// SetGoPackageOwnershipFactory is setup-only, with the same owning lifecycle
// exclusion as SetGoPackageOwnership. It does not read any source. The resolver
// invokes it only for a nonempty Go import/extern frontier, before workers.
func (r *Resolver) SetGoPackageOwnershipFactory(factory GoPackageOwnershipFactory) {
	r.goPackageOwnershipFactory = factory
	r.clearGoPackageOwnership()
}

func (r *Resolver) clearGoPackageOwnership() {
	r.goPackageOwnershipPrepared = nil
}

func goPackagePending(edge *graph.Edge) bool {
	if edge == nil || !graph.IsUnresolvedTarget(edge.To) {
		return false
	}
	name := graph.UnresolvedName(edge.To)
	return strings.HasPrefix(name, "import::") || strings.HasPrefix(name, "extern::")
}

// prepareGoPackageOwnership runs under r.mu before any candidate loop. sources
// follows warmLookupCacheWithSources's authoritative nonnil-map convention.
// Legacy direct paths pass nil and hydrate only the affected source IDs once.
func (r *Resolver) prepareGoPackageOwnership(ctx context.Context, pending []*graph.Edge, sources map[string]*graph.Node) error {
	if r.goPackageOwnershipFactory == nil || len(pending) == 0 {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	ids := make(map[string]struct{})
	for _, edge := range pending {
		if goPackagePending(edge) && edge.From != "" {
			ids[edge.From] = struct{}{}
		}
	}
	if len(ids) == 0 {
		return nil
	}
	if sources == nil {
		keys := make([]string, 0, len(ids))
		for id := range ids {
			keys = append(keys, id)
		}
		sort.Strings(keys)
		sources = r.graph.GetNodesByIDs(keys)
	}
	repos := make(map[string]struct{})
	for id := range ids {
		node := sources[id]
		if node == nil || node.Language != "go" {
			continue
		}
		if _, prepared := r.goPackageOwnershipPrepared[node.RepoPrefix]; !prepared {
			repos[node.RepoPrefix] = struct{}{}
		}
	}
	if len(repos) == 0 {
		return nil
	}
	prefixes := make([]string, 0, len(repos))
	for prefix := range repos {
		prefixes = append(prefixes, prefix)
	}
	sort.Strings(prefixes)
	// The existing scoped rich-node capability preserves Language without
	// re-scanning every repository once per same-repo frontier.
	lookups, err := r.goPackageOwnershipFactory(ctx, prefixes,
		graph.NodesInScopeSeq(r.graph, prefixes, nil, graph.KindFile))
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if r.goPackageOwnershipPrepared == nil {
		r.goPackageOwnershipPrepared = make(map[string]GoPackageOwnershipLookup, len(prefixes))
	}
	for _, prefix := range prefixes {
		r.goPackageOwnershipPrepared[prefix] = lookups[prefix]
	}
	return nil
}

func (r *Resolver) prepareGoPackageFileFrontier(filePaths []string, nodesByFile map[string][]*graph.Node, outByNode map[string][]*graph.Edge) (int, error) {
	if r.goPackageOwnershipFactory == nil {
		return 0, nil
	}
	var pending []*graph.Edge
	sources := make(map[string]*graph.Node)
	for _, filePath := range filePaths {
		for _, node := range nodesByFile[filePath] {
			if node == nil {
				continue
			}
			for _, edge := range outByNode[node.ID] {
				if goPackagePending(edge) && !r.incrementalSkipped(edge) {
					pending = append(pending, edge)
					sources[node.ID] = node
				}
			}
		}
	}
	return len(pending), r.prepareGoPackageOwnership(context.Background(), pending, sources)
}

func (r *Resolver) prepareGoPackageIncomingFrontier(stubKeys []string, inByStub map[string][]*graph.Edge) (int, error) {
	if r.goPackageOwnershipFactory == nil {
		return 0, nil
	}
	var pending []*graph.Edge
	for _, key := range stubKeys {
		for _, edge := range inByStub[key] {
			if goPackagePending(edge) {
				pending = append(pending, edge)
			}
		}
	}
	return len(pending), r.prepareGoPackageOwnership(context.Background(), pending, nil)
}
