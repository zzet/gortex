package indexer

import (
	"context"
	"iter"
	"strings"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/resolver"
)

// Park only npm-linked incoming imports and their calls before replacing or
// removing a provider. Other references keep their existing eviction rules.
func restubTrackedNpmRepoIncoming(g graph.Store, repo string) {
	if repo == "" || len(graph.ReadRepoFilePaths(g, repo, "", nil, []string{
		"js", "jsx", "ts", "tsx", "mjs", "cjs", "mts", "cts", "vue", "svelte", "astro",
	})) == 0 {
		return
	}
	var ids []string
	flush := func() {
		identities := graph.InEdgeIdentitiesByNodeIDs(g, ids)
		var sites []graph.EdgeSite
		for _, edges := range identities {
			for _, edge := range edges {
				if (edge.Kind == graph.EdgeImports || edge.Kind == graph.EdgeCalls) && graph.RepoPrefixOfID(edge.From) != repo {
					sites = append(sites, graph.EdgeSite{From: edge.From, Line: edge.Line, Kind: edge.Kind})
				}
			}
		}
		if len(sites) == 0 {
			ids = ids[:0]
			return
		}
		hydrated := g.GetEdgeCandidates(nil, sites)
		var targets []string
		for target, edges := range identities {
			marked := false
			for _, edge := range edges {
				if (edge.Kind != graph.EdgeImports && edge.Kind != graph.EdgeCalls) || graph.RepoPrefixOfID(edge.From) == repo {
					continue
				}
				for _, candidate := range hydrated.Site(edge.From, edge.Line, edge.Kind) {
					if candidate.To != target {
						continue
					}
					if edge.Kind == graph.EdgeImports {
						if spec, _ := candidate.Meta[resolver.NpmImportSpecifierMeta].(string); spec != "" {
							marked = true
						}
					} else if candidate.Meta[graph.JSLexicalBindingMeta] == graph.JSLexicalRequire {
						if spec, _ := candidate.Meta[resolver.NpmImportSpecifierMeta].(string); spec != "" {
							marked = true
						}
					}
				}
			}
			if marked {
				targets = append(targets, target)
			}
		}
		if len(targets) > 0 {
			incoming := g.GetInEdgesByNodeIDs(targets)
			evicted := make(map[string]struct{})
			for _, edges := range incoming {
				for _, edge := range edges {
					if edge != nil && graph.RepoPrefixOfID(edge.From) == repo {
						evicted[edge.From] = struct{}{}
					}
				}
			}
			restubTrackedNpmImportsFromView(g, evicted, incoming)
		}
		ids = ids[:0]
	}
	for node := range graph.RepoNodeIdentitiesSeq(g, []string{repo},
		graph.KindFile, graph.KindFunction, graph.KindMethod, graph.KindType,
		graph.KindInterface, graph.KindVariable, graph.KindConstant) {
		ids = append(ids, node.ID)
		if len(ids) == 512 {
			flush()
		}
	}
	if len(ids) > 0 {
		flush()
	}
}

func restubTrackedNpmImportsFromView(g graph.Store, evicted map[string]struct{}, incoming map[string][]*graph.Edge) []string {
	var consumerFiles []string
	consumers := make(map[string]map[string]string)
	var batch []graph.EdgeReindex
	for _, edges := range incoming {
		for _, edge := range edges {
			if edge == nil || edge.Kind != graph.EdgeImports {
				continue
			}
			spec, _ := edge.Meta[resolver.NpmImportSpecifierMeta].(string)
			if _, sourceEvicted := evicted[edge.From]; spec == "" || sourceEvicted {
				continue
			}
			consumerFiles = appendUniqueSorted(consumerFiles, edge.From)
			oldTo := edge.To
			if _, name, symbol := strings.Cut(oldTo, "::"); symbol {
				if alias, _ := edge.Meta["import_alias"].(string); alias != "" {
					name = alias
				}
				if consumers[oldTo] == nil {
					consumers[oldTo] = make(map[string]string)
				}
				consumers[oldTo][edge.From] = name
			}
			edge.To = "external::" + spec
			edge.CrossRepo = false
			// A parked import keeps its identity in its target; the marker
			// returns when the package links again, as in a cold index.
			delete(edge.Meta, resolver.NpmImportSpecifierMeta)
			batch = append(batch, graph.EdgeReindex{Edge: edge, OldTo: oldTo})
		}
	}
	var obsolete []*graph.Edge
	restubbed := make(map[graph.EdgeIdentity]struct{})
	resetNpmCall := func(edge *graph.Edge, name string) {
		delete(edge.Meta, resolver.NpmImportSpecifierMeta)
		delete(edge.Meta, "resolution")
		edge.To = graph.UnresolvedMarker + name
		edge.CrossRepo = false
		edge.Origin, edge.Tier, edge.Confidence, edge.ConfidenceLabel = "", "", 0, ""
	}
	for target, files := range consumers {
		for _, edge := range incoming[target] {
			if edge == nil || edge.Kind != graph.EdgeCalls || !edge.CrossRepo {
				continue
			}
			// Local require() calls carry their own symbol name and must not
			// inherit an ES import alias from a sibling import statement.
			if edge.Meta[graph.JSLexicalBindingMeta] == graph.JSLexicalRequire {
				continue
			}
			if _, sourceEvicted := evicted[edge.From]; sourceEvicted {
				continue
			}
			name, imported := files[edge.FilePath]
			if !imported {
				continue
			}
			restubbed[graph.EdgeIdentityFor(edge)] = struct{}{}
			parallel := *edge
			parallel.Kind = graph.EdgeCrossRepoCalls
			obsolete = append(obsolete, &parallel)
			resetNpmCall(edge, name)
			batch = append(batch, graph.EdgeReindex{Edge: edge, OldTo: target})
		}
	}
	for target, edges := range incoming {
		for _, edge := range edges {
			if edge == nil || edge.Kind != graph.EdgeCalls || !edge.CrossRepo || edge.Meta[graph.JSLexicalBindingMeta] != graph.JSLexicalRequire {
				continue
			}
			spec, _ := edge.Meta[resolver.NpmImportSpecifierMeta].(string)
			if spec == "" {
				continue
			}
			if _, sourceEvicted := evicted[edge.From]; sourceEvicted {
				continue
			}
			id := graph.EdgeIdentityFor(edge)
			if _, done := restubbed[id]; done {
				continue
			}
			_, name, hasSym := strings.Cut(target, "::")
			if !hasSym || name == "" {
				continue
			}
			restubbed[id] = struct{}{}
			if edge.FilePath != "" {
				consumerFiles = appendUniqueSorted(consumerFiles, edge.FilePath)
			}
			parallel := *edge
			parallel.Kind = graph.EdgeCrossRepoCalls
			obsolete = append(obsolete, &parallel)
			resetNpmCall(edge, name)
			batch = append(batch, graph.EdgeReindex{Edge: edge, OldTo: target})
		}
	}

	if len(obsolete) > 0 {
		graph.RemoveEdgesExact(g, obsolete)
	}
	if len(batch) > 0 {
		g.ReindexEdges(batch)
	}
	return consumerFiles
}

func isJSTSNpmPath(path string) bool {
	dot := strings.LastIndexByte(path, '.')
	if dot < 0 {
		return false
	}
	switch strings.ToLower(path[dot+1:]) {
	case "js", "jsx", "ts", "tsx", "mjs", "cjs", "mts", "cts", "vue", "svelte", "astro":
		return true
	}
	return false
}

// npmConsumerFilesForRepo finds parked and resolved tracked-npm consumers
// affected by this provider and this repo's own imports claimed by any tracked
// provider. Used after a full index or track; incremental saves carry exact
// consumer files from the restub step.
func (mi *MultiIndexer) npmConsumerFilesForRepo(repo string) []string {
	snap := mi.loadNpmPackageSnapshot()
	if snap == nil || snap.lookup == nil || len(snap.repoPackages) == 0 {
		return nil
	}
	lookup := snap.lookup
	declaredByRepo := snap.repoPackages[repo]
	isProvider := len(declaredByRepo) > 0
	files := make(map[string]struct{})
	claims := make(map[string]map[string]resolver.NpmPackageTarget)
	checkSpec := func(spec, ownerFile string) {
		if spec == "" || ownerFile == "" {
			return
		}
		ownerInRepo := graph.RepoPrefixOfID(ownerFile) == repo
		if !ownerInRepo {
			if !isProvider {
				return
			}
			pkgName, _ := splitPackageSpecifier(spec)
			if _, owned := declaredByRepo[pkgName]; !owned {
				return
			}
		}
		providers, known := claims[spec]
		if !known {
			providers = lookup(spec)
			claims[spec] = providers
		}
		for dir := range providers {
			if dir == repo || strings.HasPrefix(dir, repo+"/") || ownerInRepo {
				files[ownerFile] = struct{}{}
				break
			}
		}
	}
	var resolvedImportSites []graph.EdgeSite
	importSiteSeen := make(map[graph.EdgeSite]struct{})
	for edge := range graph.EdgesLightSeq(mi.graph, graph.EdgeImports) {
		if strings.HasPrefix(edge.To, "external::") {
			if !isProvider && graph.RepoPrefixOfID(edge.From) != repo {
				continue
			}
			spec, _, _ := strings.Cut(strings.TrimPrefix(edge.To, "external::"), "::")
			checkSpec(spec, edge.From)
			continue
		}
		if isProvider && edge.CrossRepo && isJSTSNpmPath(edge.From) {
			site := graph.EdgeSite{From: edge.From, Line: edge.Line, Kind: graph.EdgeImports}
			if _, dup := importSiteSeen[site]; !dup {
				importSiteSeen[site] = struct{}{}
				resolvedImportSites = append(resolvedImportSites, site)
			}
		}
	}
	const hydrationPage = 2048
	for start := 0; start < len(resolvedImportSites); start += hydrationPage {
		page := resolvedImportSites[start:min(start+hydrationPage, len(resolvedImportSites))]
		full := mi.graph.GetEdgeCandidates(nil, page)
		for _, site := range page {
			for _, edge := range full.Site(site.From, site.Line, graph.EdgeImports) {
				if edge == nil || !edge.CrossRepo {
					continue
				}
				marked, _ := edge.Meta[resolver.NpmImportSpecifierMeta].(string)
				if marked == "" {
					continue
				}
				mod, _, _ := strings.Cut(marked, "::")
				checkSpec(mod, edge.From)
			}
		}
	}
	if isProvider {
		var resolvedCallSites []graph.EdgeSite
		callSiteSeen := make(map[graph.EdgeSite]struct{})
		for edge := range graph.EdgesLightSeq(mi.graph, graph.EdgeCrossRepoCalls) {
			if !isJSTSNpmPath(edge.FilePath) {
				continue
			}
			site := graph.EdgeSite{From: edge.From, Line: edge.Line, Kind: graph.EdgeCalls}
			if _, dup := callSiteSeen[site]; !dup {
				callSiteSeen[site] = struct{}{}
				resolvedCallSites = append(resolvedCallSites, site)
			}
		}
		for start := 0; start < len(resolvedCallSites); start += hydrationPage {
			page := resolvedCallSites[start:min(start+hydrationPage, len(resolvedCallSites))]
			full := mi.graph.GetEdgeCandidates(nil, page)
			for _, site := range page {
				for _, edge := range full.Site(site.From, site.Line, graph.EdgeCalls) {
					if edge == nil || !edge.CrossRepo || edge.Meta[graph.JSLexicalBindingMeta] != graph.JSLexicalRequire {
						continue
					}
					marked, _ := edge.Meta[resolver.NpmImportSpecifierMeta].(string)
					if marked == "" {
						continue
					}
					mod, _, _ := strings.Cut(marked, "::")
					checkSpec(mod, edge.FilePath)
				}
			}
		}
	}
	sourceRepo := repo
	if isProvider {
		sourceRepo = ""
	}
	for edge := range unresolvedRequireCalls(mi.graph, sourceRepo) {
		spec, _ := edge.Meta[graph.JSLexicalRequireSpecMeta].(string)
		mod, _, _ := strings.Cut(spec, "::")
		checkSpec(mod, edge.FilePath)
	}
	return sortedStringKeys(files)
}

// unresolvedRequireCalls streams unresolved call edges whose callee is a local
// bound by a static require(). A non-empty sourceRepo keeps only calls whose
// file lives in that repository. A paging store filters on kind and source
// repository before it hydrates a row, so tracking a repository does not load
// every unresolved edge of the workspace; the exact checks still run here.
func unresolvedRequireCalls(store graph.Store, sourceRepo string) iter.Seq[*graph.Edge] {
	keep := func(edge *graph.Edge) bool {
		return edge != nil && edge.Kind == graph.EdgeCalls &&
			edge.Meta[graph.JSLexicalBindingMeta] == graph.JSLexicalRequire &&
			(sourceRepo == "" || graph.RepoPrefixOfID(edge.FilePath) == sourceRepo)
	}
	return func(yield func(*graph.Edge) bool) {
		pager, ok := store.(graph.UnresolvedEdgePager)
		if !ok {
			for edge := range store.EdgesWithUnresolvedTarget() {
				if keep(edge) && !yield(edge) {
					return
				}
			}
			return
		}
		ctx := context.Background()
		scan, err := pager.BeginUnresolvedEdgeScan(ctx)
		if err != nil {
			return
		}
		scan.Kinds = []graph.EdgeKind{graph.EdgeCalls}
		if sourceRepo != "" {
			scan.ScopeAnchors = []string{sourceRepo}
			scan.SourceOnly = true
		}
		var afterID int64
		for {
			page, err := pager.ReadUnresolvedEdgePage(ctx, scan, afterID, 2048, 16<<20)
			if err != nil {
				return
			}
			for _, edge := range page.Edges {
				if keep(edge) && !yield(edge) {
					return
				}
			}
			if page.Exhausted || page.NextID <= afterID {
				return
			}
			afterID = page.NextID
		}
	}
}
