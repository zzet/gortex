package tstypes

import (
	"context"
	"sort"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/semantic"
)

// preloadBounded prepares only the current fact page. Cross-file name
// candidates are fetched in one scoped batch and graph adjacency is expanded
// only from those candidates and the page's files. A full-repository language
// projection is never retained.
func (a *applier) preloadBounded(all []*fileFacts) {
	files := make([]string, 0, len(all))
	fileSet := make(map[string]struct{}, len(all))
	repoNames := make(map[string]map[string]struct{})
	maxRounds := extendsWalkDepth + 3
	for _, facts := range all {
		if facts == nil {
			continue
		}
		if _, ok := repoNames[facts.repoPrefix]; !ok {
			repoNames[facts.repoPrefix] = make(map[string]struct{})
		}
		collectFactNames(a.spec, facts, repoNames[facts.repoPrefix])
		if depth := factCallChainDepth(facts); depth+extendsWalkDepth+3 > maxRounds {
			maxRounds = depth + extendsWalkDepth + 3
		}
		if facts.file != "" {
			if _, duplicate := fileSet[facts.file]; !duplicate {
				fileSet[facts.file] = struct{}{}
				files = append(files, facts.file)
			}
		}
	}
	sort.Strings(files)
	a.loadPageFileNodes(files, fileSet, repoNames)
	for _, file := range files {
		a.fileLoaded[file] = true
	}

	for repoPrefix, names := range repoNames {
		a.preloadNames(repoPrefix, names)
	}
	// Each round seeds the frontier walk with only the nodes added since the
	// previous round: adjacency and node loads are gated by their loaded-sets,
	// so re-walking an old seed can never discover anything its first walk
	// did not, and re-passing the full accumulated set each round was pure
	// re-sort/re-scan churn.
	seededFrontier := make(map[string]struct{}, len(a.nodesByID))
	for round := 0; round < maxRounds; round++ {
		ids := make([]string, 0, len(a.nodesByID)-len(seededFrontier))
		for id := range a.nodesByID {
			if _, done := seededFrontier[id]; done {
				continue
			}
			seededFrontier[id] = struct{}{}
			ids = append(ids, id)
		}
		a.preloadApplicationFrontier(ids)

		addedName := false
		for _, node := range a.allNodes {
			if node == nil || node.Meta == nil {
				continue
			}
			for _, key := range []string{"return_type", "extension_receiver"} {
				value, _ := node.Meta[key].(string)
				value = a.spec.normalize(value)
				if value == "" {
					continue
				}
				names := repoNames[node.RepoPrefix]
				if names == nil {
					continue
				}
				if _, exists := names[value]; !exists {
					names[value] = struct{}{}
					addedName = true
				}
			}
		}
		if addedName {
			for repoPrefix, names := range repoNames {
				a.preloadNames(repoPrefix, names)
			}
		}
		if !addedName {
			break
		}
	}
}

// loadPageFileNodes hydrates the page's per-file node groups through the
// pass hot cache. This projection was the one store read the cache's
// node/name/adjacency funnels never covered: every page applier of all four
// phases re-fetched near-identical file sets straight from the store
// (measured at 48–62% of whole-process CPU before the cache existed, and
// still one full store round-trip per page × phase after it). Groups are
// shared node pointers under the same safety model as the nodes funnel, and
// files the store yields nothing for are cached as empty groups.
func (a *applier) loadPageFileNodes(files []string, fileSet map[string]struct{}, repoNames map[string]map[string]struct{}) {
	missing := files
	if a.hot != nil {
		missing = make([]string, 0, len(files))
		for _, file := range files {
			if group, ok := a.hot.getFiles(file); ok {
				for _, node := range group {
					a.rememberNode(node)
				}
				continue
			}
			missing = append(missing, file)
		}
	}
	if len(missing) == 0 {
		return
	}
	if streamer, ok := a.g.(graph.NodesInFilesByKindStreamer); ok {
		yielded := make(map[string]struct{}, len(missing))
		for file, group := range streamer.NodesInFilesByKindSeq(missing, tstypesFileNodeKinds) {
			yielded[file] = struct{}{}
			a.hot.putFiles(file, group)
			for _, node := range group {
				a.rememberNode(node)
			}
		}
		for _, file := range missing {
			if _, ok := yielded[file]; !ok {
				a.hot.putFiles(file, []*graph.Node{})
			}
		}
		return
	}
	if finder, ok := a.g.(graph.NodesInFilesByKindFinder); ok {
		byFile := make(map[string][]*graph.Node, len(missing))
		for _, node := range finder.NodesInFilesByKind(missing, tstypesFileNodeKinds) {
			a.rememberNode(node)
			if node.FilePath != "" {
				byFile[node.FilePath] = append(byFile[node.FilePath], node)
			}
		}
		for _, file := range missing {
			group := byFile[file]
			if group == nil {
				group = []*graph.Node{}
			}
			a.hot.putFiles(file, group)
		}
		return
	}
	// Compatibility-only stores get one scoped projection. Production
	// Graph and SQLite stores implement NodesInFilesByKindFinder.
	for repoPrefix := range repoNames {
		for _, node := range a.g.GetRepoNodes(repoPrefix) {
			if _, wanted := fileSet[node.FilePath]; wanted {
				a.rememberNode(node)
			}
		}
	}
}

func (a *applier) preloadNames(repoPrefix string, wanted map[string]struct{}) {
	missing := make([]string, 0, len(wanted))
	for name := range wanted {
		key := typeCandidateKey{repoPrefix: repoPrefix, name: name}
		if name != "" && !a.nameLoaded[key] {
			missing = append(missing, name)
		}
	}
	if len(missing) == 0 {
		return
	}
	sort.Strings(missing)
	// Serve pass-cached name groups first (including cached NEGATIVE groups
	// — common inferred names that bind nothing are re-asked by almost every
	// page). The cache stores the raw store result; the residency filter
	// below runs identically on both paths.
	remember := func(name string, group []*graph.Node) {
		for _, node := range group {
			if node != nil && node.RepoPrefix == repoPrefix && a.spec.allowsCandidateLanguage(node.Language) {
				a.rememberNode(node)
			}
		}
		a.nameLoaded[typeCandidateKey{repoPrefix: repoPrefix, name: name}] = true
	}
	residue := missing[:0]
	for _, name := range missing {
		if group, ok := a.hot.getNames(typeCandidateKey{repoPrefix: repoPrefix, name: name}); ok {
			remember(name, group)
			continue
		}
		residue = append(residue, name)
	}
	if len(residue) == 0 {
		return
	}
	// Language-scoped candidate hydration: a type name inferred from this
	// spec's grammars can only bind nodes of the spec's languages (plus
	// language-neutral definitions). Repo-only scoping let a mixed repo's
	// host language flood every common name — the SQL path pushes the
	// language predicate into nodes_by_repo_language_name, and the
	// compatibility paths apply the same filter in memory.
	matches := graph.FindNodesByNamesInRepoLanguages(a.g, residue, repoPrefix, a.spec.candidateLanguages())
	for _, name := range residue {
		remember(name, matches[name])
		a.hot.putNames(typeCandidateKey{repoPrefix: repoPrefix, name: name}, matches[name])
	}
}

func collectFactNames(spec *LangSpec, facts *fileFacts, out map[string]struct{}) {
	add := func(name string) {
		if spec != nil {
			name = spec.normalize(name)
		}
		if name != "" {
			out[name] = struct{}{}
		}
	}
	for _, imp := range facts.imports {
		add(imp.Local)
	}
	for _, fact := range facts.supers {
		add(fact.typeName)
		add(fact.superName)
	}
	for _, fact := range facts.metas {
		add(fact.owner)
		add(fact.name)
		if fact.key == "return_type" || fact.key == "semantic_type" {
			add(fact.value)
		}
	}
	for _, fact := range facts.aliases {
		add(fact.typeName)
		add(fact.trait)
		add(fact.alias)
		add(fact.method)
	}
	for i := range facts.calls {
		collectCallNames(spec, &facts.calls[i], out)
	}
}

func collectCallNames(spec *LangSpec, fact *callFact, out map[string]struct{}) {
	if fact == nil {
		return
	}
	for _, name := range []string{fact.method, fact.recvType, fact.recvPendingCallee, fact.recvCallTypeArg, fact.recvIdent} {
		if spec != nil {
			name = spec.normalize(name)
		}
		if name != "" {
			out[name] = struct{}{}
		}
	}
	collectCallNames(spec, fact.recvChain, out)
}

func factCallChainDepth(facts *fileFacts) int {
	maxDepth := 0
	var depth func(*callFact) int
	depth = func(fact *callFact) int {
		if fact == nil {
			return 0
		}
		return 1 + depth(fact.recvChain)
	}
	for i := range facts.calls {
		if d := depth(&facts.calls[i]); d > maxDepth {
			maxDepth = d
		}
	}
	return maxDepth
}

// coverFiles hydrates the page's per-file node groups (through the pass hot
// cache — warming them for every later phase) and counts covered symbols.
// It replaces the coverage side effect the supers phase carried when every
// file appeared in its walk; the walk itself decodes no facts.
func (a *applier) coverFiles(page []*fileFacts) int {
	files := make([]string, 0, len(page))
	fileSet := make(map[string]struct{}, len(page))
	repoNames := make(map[string]map[string]struct{})
	for _, facts := range page {
		if facts == nil || facts.file == "" {
			continue
		}
		if _, duplicate := fileSet[facts.file]; duplicate {
			continue
		}
		fileSet[facts.file] = struct{}{}
		files = append(files, facts.file)
		if _, ok := repoNames[facts.repoPrefix]; !ok {
			repoNames[facts.repoPrefix] = make(map[string]struct{})
		}
	}
	sort.Strings(files)
	a.loadPageFileNodes(files, fileSet, repoNames)
	return a.coveredSymbols(page)
}

func (a *applier) preparePage(all []*fileFacts) []*fileIndex {
	sort.Slice(all, func(i, j int) bool { return all[i].file < all[j].file })
	a.preload(all)
	a.loadCallerCalls(all)
	a.preparedAdjacency()
	indexes := make([]*fileIndex, len(all))
	for i, facts := range all {
		indexes[i] = a.buildIndex(facts)
	}
	return indexes
}

func (a *applier) applySupersPage(ctx context.Context, all []*fileFacts, res *semantic.EnrichResult) error {
	indexes := a.preparePage(all)
	for i, facts := range all {
		for _, fact := range facts.supers {
			if err := ctx.Err(); err != nil {
				return err
			}
			a.applySuper(indexes[i], fact, res)
		}
	}
	return nil
}

func (a *applier) applyMetasPage(ctx context.Context, all []*fileFacts, res *semantic.EnrichResult) error {
	indexes := a.preparePage(all)
	for i, facts := range all {
		for _, fact := range facts.metas {
			if err := ctx.Err(); err != nil {
				return err
			}
			a.applyMeta(indexes[i], fact, res)
		}
	}
	return nil
}

func (a *applier) resolveAliasesPage(ctx context.Context, all []*fileFacts) ([]stagedResolvedAlias, error) {
	indexes := a.preparePage(all)
	var staged []stagedResolvedAlias
	for i, facts := range all {
		for _, fact := range facts.aliases {
			if err := ctx.Err(); err != nil {
				return staged, err
			}
			typeNode := indexes[i].types[fact.typeName]
			if typeNode == nil {
				continue
			}
			var traitID string
			if fact.trait != "" {
				trait := a.resolveSuperNode(indexes[i], fact.trait)
				if trait == nil {
					continue
				}
				traitID = trait.ID
			}
			staged = append(staged, stagedResolvedAlias{
				typeID: typeNode.ID, alias: fact.alias, traitID: traitID, method: fact.method,
			})
		}
	}
	return staged, nil
}

func (a *applier) typeNodeIDs() []string {
	ids := make([]string, 0)
	for id, node := range a.nodesByID {
		if node != nil && receiverTypeKinds[node.Kind] {
			ids = append(ids, id)
		}
	}
	return uniqueSortedIDs(ids)
}

func (a *applier) installAliases(records []stagedResolvedAlias) {
	traitIDs := make([]string, 0, len(records))
	for _, record := range records {
		if record.traitID != "" {
			traitIDs = append(traitIDs, record.traitID)
		}
	}
	traits := a.nodes(traitIDs)
	for _, record := range records {
		var trait *graph.Node
		if record.traitID != "" {
			trait = traits[record.traitID]
			if trait == nil {
				continue
			}
		}
		a.aliases[record.typeID] = append(a.aliases[record.typeID], resolvedAlias{
			alias: record.alias, trait: trait, method: record.method,
		})
	}
}

func (a *applier) applyCallsPage(ctx context.Context, all []*fileFacts, aliases []stagedResolvedAlias, res *semantic.EnrichResult) error {
	indexes := a.preparePage(all)
	a.installAliases(aliases)
	for i, facts := range all {
		for _, fact := range facts.calls {
			if err := ctx.Err(); err != nil {
				return err
			}
			a.applyCall(indexes[i], fact, res)
		}
	}
	return nil
}

func (a *applier) pageStats(base factPageStats) factPageStats {
	base.CacheNodes = len(a.nodesByID)
	base.CacheNames = len(a.nodesByName)
	for _, edges := range a.outByID {
		base.CacheEdges += len(edges)
	}
	for _, edges := range a.inByID {
		base.CacheEdges += len(edges)
	}
	for _, edges := range a.callsByID {
		base.CacheEdges += len(edges)
	}
	return base
}

func (a *applier) coveredSymbols(all []*fileFacts) int {
	count := 0
	for _, facts := range all {
		for _, node := range a.nodesByFile[facts.file] {
			if node != nil && a.languages[node.Language] &&
				node.Kind != graph.KindFile && node.Kind != graph.KindImport {
				count++
			}
		}
	}
	return count
}
