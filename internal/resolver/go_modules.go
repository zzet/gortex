package resolver

import (
	"maps"
	"strings"

	"github.com/zzet/gortex/internal/graph"
)

// SetGoModules supplies root go.mod identities keyed by tracked repository
// prefix. Graph paths are relative to these roots, not to the module name.
func (r *Resolver) SetGoModules(modules map[string]string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.goModules = maps.Clone(modules)
	r.goModulePaths = indexGoModulePaths(modules)
}

// SetCrossWorkspaceDepLookup installs the same boundary policy used by the
// cross-repository resolver. A nil lookup retains the legacy unwired behavior.
func (r *Resolver) SetCrossWorkspaceDepLookup(lookup CrossWorkspaceDepLookup) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.crossWorkspaceLookup = lookup
}

// SetGoModuleLookup installs a revision-cached identity lookup for every resolve
// entry point, including watcher batches that never run a per-repo full pass.
func (r *Resolver) SetGoModuleLookup(lookup func() map[string]string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.goModuleLookup = lookup
}

// refreshGoModules runs under the resolver lock before pass workers start.
// The lookup publishes an immutable map; edges read it without shared locks.
func (r *Resolver) refreshGoModules() {
	if r.goModuleReplacementLookup != nil {
		r.goModuleReplacements = r.goModuleReplacementLookup()
	}
	if r.goModuleLookup != nil {
		modules := r.goModuleLookup()
		if !maps.Equal(modules, r.goModules) {
			r.goModulePaths = indexGoModulePaths(modules)
		}
		r.goModules = modules
	}
}

// SetDeferGoModuleMisses preserves symbol-bearing imports only while the
// indexer resolves against a repository-only cold shadow.
func (r *Resolver) SetDeferGoModuleMisses(deferMisses bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.deferredGoMu.Lock()
	defer r.deferredGoMu.Unlock()
	r.deferGoModuleMisses = deferMisses
	if deferMisses {
		r.deferredGoEdges = nil
	}
}

func (r *Resolver) pendingGoModuleExtern(e *graph.Edge) bool {
	if !r.deferGoModuleMisses || !strings.HasSuffix(e.FilePath, ".go") {
		return false
	}
	target := graph.UnresolvedName(e.To)
	if !strings.HasPrefix(target, "extern::") {
		return false
	}
	spec := strings.TrimPrefix(target, "extern::")
	sep := strings.LastIndex(spec, "::")
	if sep < 0 {
		return false
	}
	importPath := spec[:sep]
	caller := r.callerRepoPrefix(e)
	ownModule := r.goModules[caller]
	longest := r.goModulePaths.longest(importPath)
	if len(longest) > len(ownModule) {
		return true
	}
	if ownModule != "" && (importPath == ownModule || strings.HasPrefix(importPath, ownModule+"/")) {
		return false // the cold shadow already contains the caller's whole module
	}
	return longest != ""
}

func (r *Resolver) externWorkspaceEligible(e *graph.Edge, candidate *graph.Node, importPath string) bool {
	return crossWorkspaceEligible(r.crossWorkspaceLookup,
		candidateWorkspaceID(r.cachedGetNode(e.From)), candidateWorkspaceID(candidate), importPath)
}

// resolveGoModuleExtern prefers module identity over graph-directory suffixes.
// A tracked checkout is a source hint, not proof of the required module version.
func (r *Resolver) resolveGoModuleExtern(e *graph.Edge, importPath, callerRepo string, candidates []*graph.Node, stats *ResolveStats) bool {
	if !strings.HasSuffix(e.FilePath, ".go") {
		return false
	}
	replacement, replaced := goModuleReplacementFor(r.goModuleReplacements, callerRepo, importPath)
	match := goModulePreciseLocal(r.goModules, importPath, candidates, callerRepo, replacement.Directory)
	if match == nil && replaced {
		return false
	}
	if match == nil && r.pendingGoModuleExtern(e) {
		var remote []*graph.Node
		for _, candidate := range candidates {
			if candidate.RepoPrefix != callerRepo {
				remote = append(remote, candidate)
			}
		}
		match = goModuleCandidate(r.goModules, importPath, remote,
			callerRepo, candidateWorkspaceID(r.cachedGetNode(e.From)), r.crossWorkspaceLookup)
	} else if match == nil {
		match = goModuleCandidate(r.goModules, importPath, candidates,
			callerRepo, candidateWorkspaceID(r.cachedGetNode(e.From)), r.crossWorkspaceLookup)
	}
	if match == nil {
		return false
	}
	bindGoModuleEdge(e, match, callerRepo, r.goModules[callerRepo], r.goModuleReplacements)
	stats.Resolved++
	return true
}

func goModulePreciseLocal(modules map[string]string, importPath string, candidates []*graph.Node, callerRepo, replacementDir string) *graph.Node {
	ownModule := modules[callerRepo]
	ownDir := ""
	if ownModule != "" && (importPath == ownModule || strings.HasPrefix(importPath, ownModule+"/")) {
		ownDir = callerRepo + strings.TrimPrefix(importPath, ownModule)
	}
	// Only precise same-repo forms outrank a tracked module identity.
	for _, c := range candidates {
		if c.RepoPrefix != callerRepo || (c.Language != "" && c.Language != "go") ||
			(c.Kind != graph.KindFunction && c.Kind != graph.KindMethod && c.Kind != graph.KindType && c.Kind != graph.KindInterface) {
			continue
		}
		dir := filePathDir(c.FilePath)
		if replacementDir != "" {
			if dir == replacementDir {
				return c
			}
			continue
		}
		if dir == ownDir || dir == importPath || strings.HasSuffix(dir, "/"+importPath) {
			return c
		}
	}
	return nil
}

func goModuleCandidate(modules map[string]string, importPath string, candidates []*graph.Node, callerRepo, sourceWS string, lookup CrossWorkspaceDepLookup) *graph.Node {
	if match := goModulePreciseLocal(modules, importPath, candidates, callerRepo, ""); match != nil {
		return match
	}
	var match *graph.Node
	longest := 0
	ambiguous := false
	priority := -1
	for _, c := range candidates {
		if (c.Language != "" && c.Language != "go") || (c.Kind != graph.KindFunction && c.Kind != graph.KindType && c.Kind != graph.KindInterface) {
			continue // a package selector does not name a receiver method
		}
		module := modules[c.RepoPrefix]
		if module == "" || (importPath != module && !strings.HasPrefix(importPath, module+"/")) {
			continue
		}
		wantDir := c.RepoPrefix + strings.TrimPrefix(importPath, module)
		if filePathDir(c.FilePath) != wantDir || !crossWorkspaceEligible(lookup, sourceWS, candidateWorkspaceID(c), importPath) {
			continue
		}
		// Dedicated worktrees use <primary>@<admin>. Prefer a primary
		// checkout, but still refuse ties between independent primaries.
		rank := 0
		if !strings.Contains(c.RepoPrefix, "@") {
			rank = 1
		}
		if c.RepoPrefix == callerRepo {
			rank = 2
		}
		switch {
		case match == nil || len(module) > longest || len(module) == longest && rank > priority:
			match, longest, priority, ambiguous = c, len(module), rank, false
		case len(module) == longest && rank == priority:
			if c.RepoPrefix != match.RepoPrefix || filePathDir(c.FilePath) != filePathDir(match.FilePath) {
				ambiguous = true
			} else if c.ID < match.ID {
				match = c
			}
		}
	}
	if ambiguous {
		return nil
	}
	return match
}

func bindGoModuleEdge(e *graph.Edge, match *graph.Node, callerRepo, ownModule string, replacements map[string][]GoModuleReplacement) {
	e.CrossRepo = callerRepo != "" && match.RepoPrefix != "" && match.RepoPrefix != callerRepo
	// Only foreign or replacement bindings depend on a module-map recheck.
	spec := goModuleEdgeSpec(e)
	if sep := strings.LastIndex(spec, "::"); sep >= 0 {
		e.Meta = maps.Clone(e.Meta)
		importPath := spec[:sep]
		_, replaced := goModuleReplacementFor(replacements, callerRepo, importPath)
		own := ownModule != "" && (importPath == ownModule || strings.HasPrefix(importPath, ownModule+"/"))
		if e.CrossRepo || replaced || !own {
			if e.Meta == nil {
				e.Meta = make(map[string]any)
			}
			e.Meta["go_import"] = spec
		}
	}
	e.To = match.ID
	if e.CrossRepo {
		e.Confidence = 0.6
		e.ConfidenceLabel = graph.ConfidenceLabelFor(e.Kind, e.Confidence)
		e.Origin = graph.OriginASTInferred
	} else {
		e.Confidence = 0
		e.ConfidenceLabel = ""
		e.Origin = ""
	}
	clearEdgeTerminal(e)
}

// Index module paths once per snapshot; import lookups depend only on path depth.
type goModulePathIndex map[string]struct{}

func indexGoModulePaths(modules map[string]string) goModulePathIndex {
	paths := make(goModulePathIndex, len(modules))
	for _, module := range modules {
		if module != "" {
			paths[module] = struct{}{}
		}
	}
	return paths
}

func (paths goModulePathIndex) longest(importPath string) string {
	for path := importPath; path != ""; {
		if _, ok := paths[path]; ok {
			return path
		}
		sep := strings.LastIndexByte(path, '/')
		if sep < 0 {
			break
		}
		path = path[:sep]
	}
	return ""
}

func trackedGoImport(paths goModulePathIndex, importPath string) bool {
	return paths.longest(importPath) != ""
}

// TakeDeferredGoModuleEdges drains only imports the cold shadow left pending.
func (r *Resolver) TakeDeferredGoModuleEdges() []*graph.Edge {
	r.deferredGoMu.Lock()
	defer r.deferredGoMu.Unlock()
	if r.deferGoModuleMisses {
		return nil // a global pass must not drain a shadow that has not published yet
	}
	edges := make([]*graph.Edge, 0, len(r.deferredGoEdges))
	for _, e := range r.deferredGoEdges {
		edges = append(edges, e)
	}
	r.deferredGoEdges = nil
	return edges
}

func (r *Resolver) noteDeferredGoModuleEdge(e *graph.Edge) {
	r.deferredGoMu.Lock()
	defer r.deferredGoMu.Unlock()
	if r.deferredGoEdges == nil {
		r.deferredGoEdges = make(map[graph.EdgeIdentity]*graph.Edge)
	}
	copy := *e
	copy.Meta = maps.Clone(e.Meta)
	r.deferredGoEdges[graph.EdgeIdentityFor(e)] = &copy
}

// SetGoModuleReplacementLookup supplies immutable caller-local replacement restrictions.
func (r *Resolver) SetGoModuleReplacementLookup(lookup func() map[string][]GoModuleReplacement) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.goModuleReplacementLookup = lookup
}

// GoModuleReplacement restricts a caller's import to an exact in-checkout
// graph directory. An empty Directory refuses remote module-path binding.
type GoModuleReplacement struct {
	Module    string
	Directory string
}

func goModuleReplacementFor(replacements map[string][]GoModuleReplacement, caller, importPath string) (GoModuleReplacement, bool) {
	var selected GoModuleReplacement
	found := false
	for _, replacement := range replacements[caller] {
		module := replacement.Module
		if (module == "" || importPath == module || strings.HasPrefix(importPath, module+"/")) &&
			(!found || len(module) > len(selected.Module)) {
			selected, found = replacement, true
		}
	}
	if selected.Directory != "" {
		selected.Directory += strings.TrimPrefix(importPath, selected.Module)
	}
	return selected, found
}
