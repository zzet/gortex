package resolver

import (
	"context"
	"maps"
	"slices"
	"strings"
	"sync"

	"github.com/zzet/gortex/internal/graph"
)

func (cr *CrossRepoResolver) SetGoModules(modules map[string]string) {
	cr.goModules = maps.Clone(modules)
	cr.goModulePaths = indexGoModulePaths(modules)
}

func (cr *CrossRepoResolver) resolveGoExtern(e *graph.Edge, spec string, stats *CrossRepoStats) {
	sep := strings.LastIndex(spec, "::")
	if sep < 0 || !strings.HasSuffix(e.FilePath, ".go") {
		stats.Unresolved++
		return
	}
	match := cr.goModuleCandidate(spec[:sep], cr.cachedFindNodesByName(spec[sep+2:]), cr.callerRepoPrefix(e), cr.callerWorkspaceID(e))
	if match == nil {
		if isStdlibLike(spec[:sep]) && !trackedGoImport(cr.goModulePaths, spec[:sep]) {
			e.To = graph.StubID(cr.callerRepoPrefix(e), graph.StubKindStdlib, spec[:sep], spec[sep+2:])
		} else {
			e.To = "dep::" + spec
			cr.noteDemotedGoModuleCall(e)
		}
		stats.Unresolved++
		return
	}
	bindGoModuleEdge(e, match, cr.callerRepoPrefix(e), cr.goModules[cr.callerRepoPrefix(e)], cr.goModuleReplacements)
	stats.Resolved++
	if e.CrossRepo {
		stats.CrossRepoEdges++
		stats.ByRepo[match.RepoPrefix]++
	}
}

// GoModuleDepRetry remembers the last completed legacy-terminal sweep for a
// store's module/config revision. MultiIndexer shares it across pass objects.
type GoModuleDepRetry struct {
	mu           sync.Mutex
	modules      map[string]string
	replacements map[string][]GoModuleReplacement
	revision     uint64
	complete     bool
	pending      map[graph.EdgeIdentity]*graph.Edge
}

func (cr *CrossRepoResolver) SetGoModuleDepRetry(state *GoModuleDepRetry, revision uint64) {
	cr.goModuleRetry, cr.goModuleRevision = state, revision
}

// retryGoModuleDeps visits only external terminals, filters by tracked module
// identity, and bounds mutation batches. Symbol lookup is memoized per batch.
func (cr *CrossRepoResolver) retryGoModuleDeps(ctx context.Context, stats *CrossRepoStats) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(cr.goModules) == 0 && cr.goModuleRetry == nil {
		return nil
	}
	if cr.goModuleRetry == nil {
		cr.goModuleRetry = &GoModuleDepRetry{}
	}
	state := cr.goModuleRetry
	state.mu.Lock()
	previous := maps.Clone(state.modules)
	seedBindings := !state.complete
	if seedBindings && len(cr.goModules) == 0 {
		seedBindings = false
		for _, repo := range cr.graph.RepoPrefixes() {
			if len(graph.ReadRepoFilePaths(cr.graph, repo, "", []string{"go"}, nil)) > 0 {
				seedBindings = true // persisted bindings survive removal of every go.mod
				break
			}
		}
	}
	changedReplacements := !maps.EqualFunc(state.replacements, cr.goModuleReplacements, slices.Equal[[]GoModuleReplacement])
	state.mu.Unlock()
	if seedBindings || (changedReplacements && len(cr.goModules) > 0) || !maps.Equal(previous, cr.goModules) {
		if seedBindings || changedReplacements {
			previous = nil
		}
		if err := cr.recheckGoModuleBindings(ctx, previous, stats); err != nil {
			return err
		}
	}
	state.mu.Lock()
	pending := state.pending
	state.pending = nil
	state.mu.Unlock()
	var deferred []*graph.Edge
	for _, edge := range pending {
		deferred = append(deferred, edge)
		if len(deferred) == externalCallMutationChunk {
			cr.resolveGoModuleEdgesLocked(deferred, stats)
			deferred = deferred[:0]
		}
	}
	cr.resolveGoModuleEdgesLocked(deferred, stats)
	state.mu.Lock()
	defer state.mu.Unlock()
	if len(cr.goModules) == 0 {
		state.modules, state.revision, state.complete = maps.Clone(cr.goModules), cr.goModuleRevision, true
		state.replacements = maps.Clone(cr.goModuleReplacements)
		return nil
	}
	if state.complete && state.revision == cr.goModuleRevision && maps.Equal(state.modules, cr.goModules) && !changedReplacements {
		return nil
	}
	var batch []graph.EdgeReindex
	originals := make(map[graph.EdgeIdentity]*graph.Edge)
	names := make(map[string][]*graph.Node)
	imports := make(map[string]bool)
	flush := func() {
		if len(batch) > 0 {
			if !cr.applyGoModuleReindexes(batch) {
				cr.logger.Warn("Go module terminal batch dropped; queued for a later module pass")
				if state.pending == nil {
					state.pending = make(map[graph.EdgeIdentity]*graph.Edge)
				}
				maps.Copy(state.pending, originals)
			}
			batch = nil
			clear(originals)
		}
		clear(names)
		clear(imports)
	}
	collect := externalCallCandidateEdges(cr.graph)
	collect(func(row externalCallCandidate) bool {
		if ctx.Err() != nil {
			return false
		}
		e := row.edge
		if resolutionExempt(e) || !strings.HasSuffix(e.FilePath, ".go") {
			return true
		}
		spec := goModuleEdgeSpec(e)
		sep := strings.LastIndex(spec, "::")
		if sep < 0 {
			return true
		}
		importPath := spec[:sep]
		tracked, known := imports[importPath]
		if !known {
			tracked = trackedGoImport(cr.goModulePaths, importPath)
			imports[importPath] = tracked
		}
		if len(imports) >= externalCallMutationChunk {
			flush()
		}
		if !tracked {
			return true
		}
		name := spec[sep+2:]
		candidates, ok := names[name]
		if !ok {
			candidates = cr.graph.FindNodesByName(name)
			names[name] = candidates
		}
		source := row.source
		if source == nil {
			source = cr.graph.GetNode(e.From)
		}
		caller := ""
		if source != nil {
			caller = source.RepoPrefix
		}
		match := cr.goModuleCandidate(spec[:sep], candidates, caller, candidateWorkspaceID(source))
		if match != nil {
			original := *e
			original.Meta = maps.Clone(e.Meta)
			originals[graph.EdgeIdentityFor(e)] = &original
			oldTo := e.To
			bindGoModuleEdge(e, match, caller, cr.goModules[caller], cr.goModuleReplacements)
			delete(e.Meta, "external_call")
			delete(e.Meta, "go_extern_target")
			batch = append(batch, graph.EdgeReindex{Edge: e, OldTo: oldTo})
			stats.Resolved++
			if e.CrossRepo {
				stats.CrossRepoEdges++
				stats.ByRepo[match.RepoPrefix]++
			}
		}
		if len(batch) >= externalCallMutationChunk || len(names) >= externalCallMutationChunk {
			flush()
		}
		return true
	})
	flush()
	if err := ctx.Err(); err != nil {
		return err
	}
	state.modules, state.revision, state.complete = maps.Clone(cr.goModules), cr.goModuleRevision, true
	state.replacements = maps.Clone(cr.goModuleReplacements)
	return nil
}

// goModuleIncomingEdges probes only package-qualified placeholders owned by the
// changed Go definitions. Bare-name frontier keys cannot find these imports.
func (cr *CrossRepoResolver) goModuleIncomingEdges(nodesByFile map[string][]*graph.Node, definitionFiles []string) []*graph.Edge {
	edges, fact, err := cr.goModuleIncomingEdgesBounded(nodesByFile, definitionFiles, nil)
	logIncomingAdmissionRefusal(cr.logger, "go_module_incoming", fact.Keys, fact, err)
	return edges
}

func (cr *CrossRepoResolver) goModuleIncomingEdgesBounded(nodesByFile map[string][]*graph.Node, definitionFiles []string, budget *graph.IncomingSourceBudget) ([]*graph.Edge, graph.IncomingSourceAdmission, error) {
	if len(cr.goModules) == 0 {
		return nil, graph.IncomingSourceAdmission{}, nil
	}
	keys := make(map[string]struct{})
	providers := make(map[string][]*graph.Node)
	synthetic := make(map[string]struct{})
	for _, file := range definitionFiles {
		for _, n := range nodesByFile[file] {
			if n == nil || n.Language != "go" || n.Name == "" || (n.Kind != graph.KindFunction && n.Kind != graph.KindType && n.Kind != graph.KindInterface) {
				continue
			}
			module := cr.goModules[n.RepoPrefix]
			dir := filePathDir(n.FilePath)
			if module == "" || (dir != n.RepoPrefix && !strings.HasPrefix(dir, n.RepoPrefix+"/")) {
				continue
			}
			spec := module + strings.TrimPrefix(dir, n.RepoPrefix) + "::" + n.Name
			providers[spec] = append(providers[spec], n)
			keys[graph.UnresolvedMarker+"extern::"+spec] = struct{}{}
			keys["dep::"+spec] = struct{}{}
			synthetic[externalCallPrefix+"dep::"+module+strings.TrimPrefix(dir, n.RepoPrefix)] = struct{}{}
		}
	}
	var ids []string
	for key := range keys {
		ids = append(ids, key)
	}
	for key := range synthetic {
		ids = append(ids, key)
	}
	var edges []*graph.Edge
	sources := make(map[string]*graph.Node)
	var incomingByID map[string][]*graph.Edge
	var fact graph.IncomingSourceAdmission
	if budget == nil {
		incomingByID = cr.graph.GetInEdgesByNodeIDs(ids)
	} else {
		var err error
		incomingByID, fact, err = graph.AdmitIncomingRowsBounded(context.Background(), cr.graph.GetInEdgesByNodeIDs, ids, budget)
		if err != nil {
			return nil, fact, err
		}
	}
	for _, incoming := range incomingByID {
		for _, e := range incoming {
			if strings.HasPrefix(e.To, externalCallPrefix) {
				target, _ := e.Meta["go_extern_target"].(string)
				if _, wanted := keys[target]; !wanted {
					continue
				}
			}
			spec := goModuleEdgeSpec(e)
			sep := strings.LastIndex(spec, "::")
			if sep < 0 {
				continue
			}
			source, known := sources[e.From]
			if !known {
				source = cr.cachedGetNode(e.From)
				sources[e.From] = source
			}
			for _, provider := range providers[spec] {
				if crossWorkspaceEligible(cr.crossWorkspaceLookup, candidateWorkspaceID(source), candidateWorkspaceID(provider), spec[:sep]) {
					edges = append(edges, e)
					break
				}
			}
		}
	}
	return edges, fact, nil
}

// applyGoModuleReindexes never publishes derived work for a dropped batch.
func (cr *CrossRepoResolver) applyGoModuleReindexes(batch []graph.EdgeReindex) bool {
	if len(batch) == 0 {
		return true
	}
	if writer, ok := cr.graph.(interface {
		ReindexEdgesApplied([]graph.EdgeReindex) bool
	}); ok {
		if !writer.ReindexEdgesApplied(batch) {
			return false
		}
	} else {
		cr.graph.ReindexEdges(batch)
	}
	cr.removeRetargetedGoModuleParallels(batch)
	DetectCrossRepoEdgesForReindexes(cr.graph, batch)
	// Provider restubs park returned-value sources on the bare symbol,
	// while the call retains its package-qualified import, even as a terminal.
	sourceReindexes := slices.Clone(batch)
	for _, row := range batch {
		if row.Edge.Kind != graph.EdgeCalls || isSyntheticResolveTarget(row.Edge.To) {
			continue
		}
		spec := goModuleEdgeSpec(row.Edge)
		if sep := strings.LastIndex(spec, "::"); sep >= 0 {
			sourceReindexes = append(sourceReindexes, graph.EdgeReindex{
				Edge: row.Edge, OldTo: graph.UnresolvedMarker + spec[sep+2:],
			})
		}
	}
	reconcilePlaceholderSources(cr.graph, nil, sourceReindexes)
	for _, row := range batch {
		cr.noteRetargetedCall(row.Edge)
		cr.noteDemotedGoModuleCall(row.Edge)
	}
	return true
}

// ResolveDeferredGoModuleEdges needs no directory, dependency or whole-repo frontier.
// Each bounded batch releases the shared resolve lock before the next one starts.
func (cr *CrossRepoResolver) ResolveDeferredGoModuleEdges(edges []*graph.Edge) error {
	for start := 0; start < len(edges); start += externalCallMutationChunk {
		end := min(start+externalCallMutationChunk, len(edges))
		cr.mu.Lock()
		cr.resolveGoModuleEdgesLocked(edges[start:end], nil)
		cr.mu.Unlock()
	}
	return nil
}

func goModuleEdgeSpec(e *graph.Edge) string {
	if spec, ok := e.Meta["go_import"].(string); ok && spec != "" {
		return spec
	}
	target := e.To
	if strings.HasPrefix(target, externalCallPrefix) {
		target, _ = e.Meta["go_extern_target"].(string)
	}
	switch {
	case strings.HasPrefix(graph.UnresolvedName(target), "extern::"):
		return strings.TrimPrefix(graph.UnresolvedName(target), "extern::")
	case strings.HasPrefix(target, "dep::"):
		return strings.TrimPrefix(target, "dep::")
	case graph.IsStdlibStub(target):
		return graph.StubRest(target)
	default:
		return ""
	}
}

func (cr *CrossRepoResolver) resolveGoModuleEdgesLocked(edges []*graph.Edge, stats *CrossRepoStats) {
	var batch []graph.EdgeReindex
	names := make(map[[2]string][]*graph.Node)
	sources := make(map[string]*graph.Node)
	targetRepos := make(map[string]string)
	originals := make(map[graph.EdgeIdentity]*graph.Edge)
	for _, e := range edges {
		if e == nil || resolutionExempt(e) || !strings.HasSuffix(e.FilePath, ".go") {
			continue
		}
		spec := goModuleEdgeSpec(e)
		sep := strings.LastIndex(spec, "::")
		if sep < 0 {
			continue
		}
		source, known := sources[e.From]
		if !known {
			source = cr.cachedGetNode(e.From)
			sources[e.From] = source
		}
		caller := ""
		if source != nil {
			caller = source.RepoPrefix
		}
		key := [2]string{spec[sep+2:], caller}
		candidates, known := names[key]
		if !known {
			candidates = cr.cachedFindNodesByName(key[0])
			names[key] = candidates
		}
		match := cr.goModuleCandidate(spec[:sep], candidates, caller, candidateWorkspaceID(source))
		copy := *e
		copy.Meta = maps.Clone(e.Meta)
		if match != nil {
			targetRepos[match.ID] = match.RepoPrefix
			bindGoModuleEdge(&copy, match, caller, cr.goModules[caller], cr.goModuleReplacements)
		} else {
			if !graph.IsUnresolvedTarget(e.To) && e.Meta["go_import"] == nil {
				continue
			}
			copy.CrossRepo, copy.Confidence, copy.ConfidenceLabel, copy.Origin, copy.Tier = false, 0, "", "", ""
			if isStdlibLike(spec[:sep]) && !trackedGoImport(cr.goModulePaths, spec[:sep]) {
				copy.To = graph.StubID(caller, graph.StubKindStdlib, spec[:sep], spec[sep+2:])
			} else {
				copy.To = "dep::" + spec
			}
		}
		if copy.To == e.To {
			continue
		}
		original := *e
		original.Meta = maps.Clone(e.Meta)
		originals[graph.EdgeIdentityFor(e)] = &original
		delete(copy.Meta, "external_call")
		delete(copy.Meta, "go_extern_target")
		batch = append(batch, graph.EdgeReindex{Edge: &copy, OldTo: e.To, RefreshIdentity: true, OldFilePath: e.FilePath, OldLine: e.Line})
	}
	batch = cr.filterLiveReindex(batch)
	if !cr.applyGoModuleReindexes(batch) {
		cr.logger.Warn("Go module rebind batch dropped; queued for a later module pass")
		if cr.goModuleRetry == nil {
			cr.goModuleRetry = &GoModuleDepRetry{}
		}
		cr.goModuleRetry.mu.Lock()
		if cr.goModuleRetry.pending == nil {
			cr.goModuleRetry.pending = make(map[graph.EdgeIdentity]*graph.Edge)
		}
		maps.Copy(cr.goModuleRetry.pending, originals)
		cr.goModuleRetry.mu.Unlock()
		return
	}
	if stats == nil {
		return
	}
	for _, row := range batch {
		if isSyntheticResolveTarget(row.Edge.To) {
			stats.Unresolved++
			continue
		}
		stats.Resolved++
		if row.Edge.CrossRepo {
			stats.CrossRepoEdges++
			stats.ByRepo[targetRepos[row.Edge.To]]++
		}
	}
}

// RetryGoModuleDeps is the revision-keyed migration used after publishing a module.
func (cr *CrossRepoResolver) RetryGoModuleDeps(ctx context.Context) error {
	cr.mu.Lock()
	defer cr.mu.Unlock()
	return cr.retryGoModuleDeps(ctx, &CrossRepoStats{ByRepo: make(map[string]int)})
}

// ResolveGoModuleIncomingForRepo probes exact targets for the provider's definitions.
// Reindexing has no changed-symbol receipt, so stream only its definition IDs and
// hydrate at most one batch; never load the repository's nodes or edges as a set.
func (cr *CrossRepoResolver) ResolveGoModuleIncomingForRepo(repo string) error {
	if cr.goModules[repo] == "" {
		return nil
	}
	var ids []string
	flush := func() error {
		nodes := cr.graph.GetNodesByIDs(ids)
		byFile := make(map[string][]*graph.Node)
		var files []string
		for _, n := range nodes {
			if n == nil {
				continue
			}
			if _, exists := byFile[n.FilePath]; !exists {
				files = append(files, n.FilePath)
			}
			byFile[n.FilePath] = append(byFile[n.FilePath], n)
		}
		edges := cr.goModuleIncomingEdges(byFile, files)
		ids = ids[:0]
		return cr.ResolveDeferredGoModuleEdges(edges)
	}
	for n := range graph.RepoNodeIdentitiesSeq(cr.graph, []string{repo}, graph.KindFunction, graph.KindType, graph.KindInterface) {
		ids = append(ids, n.ID)
		if len(ids) == externalCallMutationChunk {
			if err := flush(); err != nil {
				return err
			}
		}
	}
	if len(ids) > 0 {
		return flush()
	}
	return nil
}

// Re-check only marked incoming calls into providers affected by identity edits.
// The compact definition stream avoids loading a whole repo's nodes or edges.
func (cr *CrossRepoResolver) recheckGoModuleBindings(ctx context.Context, previous map[string]string, stats *CrossRepoStats) error {
	changed := make(map[string]bool)
	for repo, module := range previous {
		if cr.goModules[repo] != module {
			changed[module] = true
			changed[cr.goModules[repo]] = true
		}
	}
	for repo, module := range cr.goModules {
		if previous[repo] != module {
			changed[module] = true
			changed[previous[repo]] = true
		}
	}
	var repos []string
	if previous == nil {
		repos = cr.graph.RepoPrefixes()
	}
	for repo, module := range previous {
		for path := range changed {
			if path != "" && (module == path || strings.HasPrefix(path, module+"/") || strings.HasPrefix(module, path+"/")) {
				repos = append(repos, repo)
				break
			}
		}
	}
	if len(repos) == 0 {
		return ctx.Err()
	}
	var ids []string
	flush := func() {
		var edges []*graph.Edge
		for _, incoming := range cr.graph.GetInEdgesByNodeIDs(ids) {
			for _, e := range incoming {
				if e.Meta["go_import"] == nil || resolutionExempt(e) {
					continue
				}
				edges = append(edges, e)
				if len(edges) == externalCallMutationChunk {
					cr.resolveGoModuleEdgesLocked(edges, stats)
					edges = edges[:0]
				}
			}
		}
		cr.resolveGoModuleEdgesLocked(edges, stats)
		ids = ids[:0]
	}
	for n := range graph.RepoNodeIdentitiesSeq(cr.graph, repos, graph.KindFunction, graph.KindType, graph.KindInterface) {
		if err := ctx.Err(); err != nil {
			return err
		}
		ids = append(ids, n.ID)
		if len(ids) == externalCallMutationChunk {
			flush()
		}
	}
	if len(ids) > 0 {
		flush()
	}
	return ctx.Err()
}

// SetGoModuleReplacements supplies caller-local replacement restrictions.
func (cr *CrossRepoResolver) SetGoModuleReplacements(replacements map[string][]GoModuleReplacement) {
	cr.goModuleReplacements = replacements
}

func (cr *CrossRepoResolver) goModuleCandidate(importPath string, candidates []*graph.Node, caller, workspace string) *graph.Node {
	replacement, replaced := goModuleReplacementFor(cr.goModuleReplacements, caller, importPath)
	if match := goModulePreciseLocal(cr.goModules, importPath, candidates, caller, replacement.Directory); match != nil {
		return match
	}
	if replaced {
		return nil
	}
	return goModuleCandidate(cr.goModules, importPath, candidates, caller, workspace, cr.crossWorkspaceLookup)
}

// A module-map demotion does not evict either file, so its old parallel survives
// ordinary eviction cleanup. Edge identity includes the caller file and line.
func (cr *CrossRepoResolver) removeRetargetedGoModuleParallels(batch []graph.EdgeReindex) {
	var sites []graph.EdgeSite
	for _, row := range batch {
		if row.Edge.Meta["go_import"] == nil || row.OldTo == row.Edge.To {
			continue
		}
		if kind, ok := graph.CrossRepoKindFor(row.Edge.Kind); ok {
			sites = append(sites, graph.EdgeSite{From: row.Edge.From, Line: row.Edge.Line, Kind: kind})
		}
	}
	if len(sites) == 0 {
		return
	}
	candidates := cr.graph.GetEdgeCandidates(nil, sites)
	var obsolete []*graph.Edge
	for _, row := range batch {
		e := row.Edge
		kind, ok := graph.CrossRepoKindFor(e.Kind)
		if !ok || e.Meta["go_import"] == nil || row.OldTo == e.To {
			continue
		}
		for _, parallel := range candidates.Site(e.From, e.Line, kind) {
			if parallel.To == row.OldTo && parallel.FilePath == e.FilePath {
				obsolete = append(obsolete, parallel)
			}
		}
	}
	graph.RemoveEdgesExact(cr.graph, obsolete)
}

// resolveGoExtern also runs in parallel pass workers; cr.mu alone does not
// serialize their demotion records.
func (cr *CrossRepoResolver) noteDemotedGoModuleCall(e *graph.Edge) {
	if !strings.HasPrefix(e.To, "dep::") || e.Meta["go_import"] == nil || e.FilePath == "" {
		return
	}
	cr.goModuleDemotedMu.Lock()
	defer cr.goModuleDemotedMu.Unlock()
	if cr.goModuleDemotedFiles == nil {
		cr.goModuleDemotedFiles = make(map[string]struct{})
	}
	cr.goModuleDemotedFiles[e.FilePath] = struct{}{}
}

// TakeDemotedGoModuleCallFiles drains caller files after the resolve lock is
// released, so the indexer can synthesize external calls using caller config.
func (cr *CrossRepoResolver) TakeDemotedGoModuleCallFiles() []string {
	cr.mu.Lock()
	defer cr.mu.Unlock()
	cr.goModuleDemotedMu.Lock()
	defer cr.goModuleDemotedMu.Unlock()
	files := make([]string, 0, len(cr.goModuleDemotedFiles))
	for file := range cr.goModuleDemotedFiles {
		files = append(files, file)
	}
	cr.goModuleDemotedFiles = nil
	slices.Sort(files)
	return files
}
