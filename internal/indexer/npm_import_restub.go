package indexer

import (
	"context"
	"iter"
	"sort"
	"strings"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/resolver"
)

func jstsNodeFilePath(id string) string {
	before, _, _ := strings.Cut(id, "::")
	before, _, _ = strings.Cut(before, "#")
	return before
}

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
		// Stage 1: hydrate only cross-repo EdgeImports sites; any target
		// already marked via a tracked-npm EdgeImports does not need its
		// EdgeCalls sites hydrated separately because restubTrackedNpmImportsFromView
		// inspects all incoming edges (both ES-import and require calls) of every
		// marked target.
		var importSites []graph.EdgeSite
		importSiteSeen := make(map[graph.EdgeSite]struct{})
		for _, edges := range identities {
			for _, edge := range edges {
				if edge.Kind == graph.EdgeImports && graph.RepoPrefixOfID(edge.From) != repo {
					site := graph.EdgeSite{From: edge.From, Line: edge.Line, Kind: edge.Kind}
					if _, dup := importSiteSeen[site]; !dup {
						importSiteSeen[site] = struct{}{}
						importSites = append(importSites, site)
					}
				}
			}
		}
		markedTargets := make(map[string]struct{})
		if len(importSites) > 0 {
			hydrated := g.GetEdgeCandidates(nil, importSites)
			for target, edges := range identities {
				for _, edge := range edges {
					if edge.Kind != graph.EdgeImports || graph.RepoPrefixOfID(edge.From) == repo {
						continue
					}
					for _, candidate := range hydrated.Site(edge.From, edge.Line, edge.Kind) {
						if candidate.To != target {
							continue
						}
						if spec, _ := candidate.Meta[resolver.NpmImportSpecifierMeta].(string); spec != "" {
							markedTargets[target] = struct{}{}
							break
						}
					}
					if _, marked := markedTargets[target]; marked {
						break
					}
				}
			}
		}
		// Stage 2: only for unmarked JS/TS symbol targets (e.g. symbols reached
		// solely via function-scoped require() with no incoming EdgeImports),
		// hydrate cross-repo JS/TS EdgeCalls sites.
		var callSites []graph.EdgeSite
		callSiteSeen := make(map[graph.EdgeSite]struct{})
		var unmarkedJSTSTargets []string
		for target, edges := range identities {
			if _, marked := markedTargets[target]; marked {
				continue
			}
			if !strings.Contains(target, "::") || !isJSTSNpmPath(jstsNodeFilePath(target)) {
				continue
			}
			addedTarget := false
			for _, edge := range edges {
				if edge.Kind != graph.EdgeCalls || graph.RepoPrefixOfID(edge.From) == repo {
					continue
				}
				callerPath := jstsNodeFilePath(edge.FilePath)
				if callerPath == "" {
					callerPath = jstsNodeFilePath(edge.From)
				}
				if !isJSTSNpmPath(callerPath) {
					continue
				}
				if !addedTarget {
					unmarkedJSTSTargets = append(unmarkedJSTSTargets, target)
					addedTarget = true
				}
				site := graph.EdgeSite{From: edge.From, Line: edge.Line, Kind: edge.Kind}
				if _, dup := callSiteSeen[site]; !dup {
					callSiteSeen[site] = struct{}{}
					callSites = append(callSites, site)
				}
			}
		}
		if len(callSites) > 0 {
			hydrated := g.GetEdgeCandidates(nil, callSites)
			for _, target := range unmarkedJSTSTargets {
				for _, edge := range identities[target] {
					if edge.Kind != graph.EdgeCalls || graph.RepoPrefixOfID(edge.From) == repo {
						continue
					}
					for _, candidate := range hydrated.Site(edge.From, edge.Line, edge.Kind) {
						if candidate.To != target {
							continue
						}
						if candidate.Meta[graph.JSLexicalBindingMeta] == graph.JSLexicalRequire {
							if spec, _ := candidate.Meta[resolver.NpmImportSpecifierMeta].(string); spec != "" {
								markedTargets[target] = struct{}{}
								break
							}
						}
					}
					if _, marked := markedTargets[target]; marked {
						break
					}
				}
			}
		}
		if len(markedTargets) > 0 {
			targets := make([]string, 0, len(markedTargets))
			for target := range markedTargets {
				targets = append(targets, target)
			}
			sort.Strings(targets)
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
		delete(edge.Meta, graph.EdgeMetaResolveTerminal)
		delete(edge.Meta, graph.EdgeMetaResolveTerminalReason)
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
			// Preserve restub provenance for ES-import calls so unchanged
			// consumer files do not enter receipt.UnresolvedFiles on provider save.
			graph.StashRestubProvenance(edge)
			edge.To = graph.UnresolvedMarker + name
			edge.CrossRepo, edge.ConfidenceLabel = false, ""
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
			name, _ := edge.Meta[graph.JSLexicalRequireLocalMeta].(string)
			if name == "" {
				var hasSym bool
				_, name, hasSym = strings.Cut(target, "::")
				if !hasSym || name == "" {
					continue
				}
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
	return mi.npmConsumerFilesForRepoContext(context.Background(), repo)
}

func (mi *MultiIndexer) npmConsumerFilesForRepoContext(ctx context.Context, repo string) []string {
	if ctx == nil {
		ctx = context.Background()
	}
	snap := mi.loadNpmPackageSnapshot()
	if snap == nil || snap.lookup == nil || len(snap.repoPackages) == 0 {
		return nil
	}
	lookup := snap.lookup
	declaredByRepo := snap.repoPackages[repo]
	isProvider := len(declaredByRepo) > 0
	var competingProviderRepos map[string]bool
	if isProvider {
		for pkgName := range declaredByRepo {
			for dir := range lookup(pkgName) {
				otherRepo, _, _ := strings.Cut(dir, "/")
				if otherRepo != "" && otherRepo != repo {
					if competingProviderRepos == nil {
						competingProviderRepos = make(map[string]bool)
					}
					competingProviderRepos[otherRepo] = true
				}
			}
		}
	}
	files := make(map[string]struct{})
	claims := make(map[string]map[string]resolver.NpmPackageTarget)
	lookupCached := func(spec string) map[string]resolver.NpmPackageTarget {
		if spec == "" {
			return nil
		}
		providers, known := claims[spec]
		if !known {
			providers = lookup(spec)
			claims[spec] = providers
		}
		return providers
	}
	checkSpec := func(spec, ownerFile string) bool {
		if spec == "" || ownerFile == "" {
			return false
		}
		ownerInRepo := graph.RepoPrefixOfID(ownerFile) == repo
		if !ownerInRepo {
			if !isProvider {
				return false
			}
			pkgName, _ := splitPackageSpecifier(spec)
			if _, owned := declaredByRepo[pkgName]; !owned {
				return false
			}
		}
		providers := lookupCached(spec)
		for dir := range providers {
			if dir == repo || strings.HasPrefix(dir, repo+"/") || ownerInRepo {
				files[ownerFile] = struct{}{}
				return true
			}
		}
		return false
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
			targetRepo := graph.RepoPrefixOfID(edge.To)
			if targetRepo == repo || competingProviderRepos[targetRepo] {
				site := graph.EdgeSite{From: edge.From, Line: edge.Line, Kind: graph.EdgeImports}
				if _, dup := importSiteSeen[site]; !dup {
					importSiteSeen[site] = struct{}{}
					resolvedImportSites = append(resolvedImportSites, site)
				}
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
			if edge.Origin != graph.OriginASTResolved && edge.Origin != graph.OriginLSPResolved {
				continue
			}
			targetRepo := graph.RepoPrefixOfID(edge.To)
			if targetRepo != repo && !competingProviderRepos[targetRepo] {
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

	// For non-provider repos, collect this repo's unresolved require() calls
	// first via index-pinned SourceOnly + MetaContains scan (0 rows on non-JS/TS
	// repos) so barrel discovery only walks foreign barrels when needed.
	var repoReqEdges []*graph.Edge
	if !isProvider {
		for edge := range unresolvedRequireCalls(ctx, mi.graph, repo, mi.logger) {
			repoReqEdges = append(repoReqEdges, edge)
		}
	}

	// Discover local, cross-repo, and chained tracked-npm barrel files across
	// all repository track orders.
	type trackedExtReExport struct {
		ownerFile string
		spec      string
	}
	npmBarrels := make(map[string]struct{})
	var trackedExtReExports []trackedExtReExport
	for edge := range graph.EdgesLightSeq(mi.graph, graph.EdgeReExports) {
		ownerFile := edge.FilePath
		if ownerFile == "" {
			ownerFile, _, _ = strings.Cut(edge.From, "::")
		}
		if ownerFile == "" {
			continue
		}
		ownerInRepo := graph.RepoPrefixOfID(ownerFile) == repo
		var rawSpec string
		switch {
		case strings.HasPrefix(edge.To, "external::"):
			rawSpec = strings.TrimPrefix(edge.To, "external::")
		case graph.IsUnresolvedTarget(edge.To):
			u := graph.UnresolvedName(edge.To)
			if !strings.HasPrefix(u, "import::") {
				continue
			}
			rawSpec = strings.TrimPrefix(u, "import::")
		default:
			targetRepo := graph.RepoPrefixOfID(edge.To)
			if isProvider {
				if edge.CrossRepo && (targetRepo == repo || competingProviderRepos[targetRepo]) {
					npmBarrels[ownerFile] = struct{}{}
					files[ownerFile] = struct{}{}
				}
			} else if len(repoReqEdges) > 0 && len(snap.repoPackages[targetRepo]) > 0 {
				npmBarrels[ownerFile] = struct{}{}
			}
			continue
		}
		spec, _, _ := strings.Cut(rawSpec, "::")
		if spec == "" {
			continue
		}
		seeded := false
		if isProvider || ownerInRepo {
			if checkSpec(spec, ownerFile) {
				npmBarrels[ownerFile] = struct{}{}
				seeded = true
			}
		} else if len(repoReqEdges) > 0 && len(lookupCached(spec)) > 0 {
			npmBarrels[ownerFile] = struct{}{}
			seeded = true
		}
		if !seeded && len(lookupCached(spec)) > 0 {
			trackedExtReExports = append(trackedExtReExports, trackedExtReExport{
				ownerFile: ownerFile,
				spec:      spec,
			})
		}
	}

	var transitiveNpmBarrels map[string]struct{}
	if len(npmBarrels) > 0 {
		transitiveNpmBarrels = make(map[string]struct{}, len(npmBarrels))
		newFiles := make([]string, 0, len(npmBarrels))
		for f := range npmBarrels {
			transitiveNpmBarrels[f] = struct{}{}
			newFiles = append(newFiles, f)
		}
		sort.Strings(newFiles)
		for len(newFiles) > 0 {
			newFileSet := make(map[string]struct{}, len(newFiles))
			for _, f := range newFiles {
				newFileSet[f] = struct{}{}
			}
			nodesByFile := mi.graph.GetFileNodesByPaths(newFiles)
			frontier := make([]string, 0, len(newFiles)*2)
			for _, f := range newFiles {
				frontier = append(frontier, f)
				for _, n := range nodesByFile[f] {
					if n != nil && n.ID != "" && n.ID != f {
						frontier = append(frontier, n.ID)
					}
				}
			}
			nextSet := make(map[string]struct{})
			for _, edges := range graph.InEdgeIdentitiesByNodeIDs(mi.graph, frontier) {
				for _, id := range edges {
					if id.Kind != graph.EdgeReExports && id.Kind != graph.EdgeImports {
						continue
					}
					ownerFile := id.FilePath
					if ownerFile == "" {
						ownerFile, _, _ = strings.Cut(id.From, "::")
					}
					if ownerFile == "" {
						continue
					}
					if isProvider || graph.RepoPrefixOfID(ownerFile) == repo {
						files[ownerFile] = struct{}{}
					}
					if id.Kind == graph.EdgeReExports {
						if _, seen := transitiveNpmBarrels[ownerFile]; !seen {
							transitiveNpmBarrels[ownerFile] = struct{}{}
							nextSet[ownerFile] = struct{}{}
						}
					}
				}
			}
			for _, ext := range trackedExtReExports {
				if _, seen := transitiveNpmBarrels[ext.ownerFile]; seen {
					continue
				}
				matched := false
				for _, target := range lookupCached(ext.spec) {
					for _, stem := range target.Entries {
						if stem != "" && resolver.ProbeJSTSFileInSet(newFileSet, stem) != "" {
							matched = true
							break
						}
					}
					if matched {
						break
					}
				}
				if !matched {
					continue
				}
				if isProvider || graph.RepoPrefixOfID(ext.ownerFile) == repo {
					files[ext.ownerFile] = struct{}{}
				}
				transitiveNpmBarrels[ext.ownerFile] = struct{}{}
				nextSet[ext.ownerFile] = struct{}{}
			}
			newFiles = newFiles[:0]
			for f := range nextSet {
				newFiles = append(newFiles, f)
			}
			sort.Strings(newFiles)
		}
	}

	var (
		aliasInit bool
		pathAlias resolver.PathAliasResolver
		npmAlias  resolver.NpmAliasResolver
	)
	matchReq := func(edge *graph.Edge) {
		spec, _ := edge.Meta[graph.JSLexicalRequireSpecMeta].(string)
		mod, _, _ := strings.Cut(spec, "::")
		if checkSpec(mod, edge.FilePath) {
			return
		}
		if len(transitiveNpmBarrels) == 0 || mod == "" || edge.FilePath == "" {
			return
		}
		if !aliasInit {
			pathAlias = mi.pathAliasResolver()
			npmAlias = mi.npmAliasResolver()
			aliasInit = true
		}
		if resolver.MatchesJSTSFileSet(transitiveNpmBarrels, pathAlias, npmAlias, edge.FilePath, mod) {
			files[edge.FilePath] = struct{}{}
			return
		}
		for _, target := range lookupCached(mod) {
			for _, stem := range target.Entries {
				if stem != "" && resolver.ProbeJSTSFileInSet(transitiveNpmBarrels, stem) != "" {
					files[edge.FilePath] = struct{}{}
					return
				}
			}
		}
	}
	if !isProvider {
		for _, edge := range repoReqEdges {
			matchReq(edge)
		}
	} else {
		for edge := range unresolvedRequireCalls(ctx, mi.graph, "", mi.logger) {
			matchReq(edge)
		}
	}
	return sortedStringKeys(files)
}

// unresolvedRequireCalls streams unresolved call edges whose callee is a local
// bound by a static require(). A non-empty sourceRepo keeps only calls whose
// file lives in that repository. A paging store filters on kind, source
// repository, and raw MetaContains substring before it hydrates a row, so
// tracking a repository does not load every unresolved edge of the workspace;
// the exact checks still run here.
func unresolvedRequireCalls(ctx context.Context, store graph.Store, sourceRepo string, logger *zap.Logger) iter.Seq[*graph.Edge] {
	if ctx == nil {
		ctx = context.Background()
	}
	keep := func(edge *graph.Edge) bool {
		return edge != nil && edge.Kind == graph.EdgeCalls &&
			edge.Meta[graph.JSLexicalBindingMeta] == graph.JSLexicalRequire &&
			(sourceRepo == "" || graph.RepoPrefixOfID(edge.FilePath) == sourceRepo)
	}
	return func(yield func(*graph.Edge) bool) {
		if ctx.Err() != nil {
			return
		}
		pager, ok := store.(graph.UnresolvedEdgePager)
		if !ok {
			for edge := range store.EdgesWithUnresolvedTarget() {
				if ctx.Err() != nil {
					return
				}
				if keep(edge) && !yield(edge) {
					return
				}
			}
			return
		}
		scan, err := pager.BeginUnresolvedEdgeScan(ctx)
		if err != nil {
			if logger != nil {
				logger.Debug("npm consumer scan: begin unresolved edge scan failed",
					zap.String("source_repo", sourceRepo), zap.Error(err))
			}
			return
		}
		scan.Kinds = []graph.EdgeKind{graph.EdgeCalls}
		scan.MetaContains = graph.JSLexicalRequireSpecMeta
		if sourceRepo != "" {
			scan.ScopeAnchors = []string{sourceRepo}
			scan.SourceOnly = true
		}
		var afterID int64
		for {
			if ctx.Err() != nil {
				return
			}
			page, err := pager.ReadUnresolvedEdgePage(ctx, scan, afterID, 2048, 16<<20)
			if err != nil {
				if logger != nil {
					logger.Debug("npm consumer scan: read unresolved edge page failed",
						zap.String("source_repo", sourceRepo), zap.Int64("after_id", afterID), zap.Error(err))
				}
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
