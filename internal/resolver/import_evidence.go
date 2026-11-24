package resolver

import (
	"path"
	"strings"
	"sync"

	"github.com/zzet/gortex/internal/graph"
)

// Import-evidence disambiguation for bare JS/TS calls.
//
// resolveFunctionCall's generic cascade is locality-driven: same-directory
// candidates win, then the first same-repo match, then (for JS/TS value
// callees) a unique repo-wide match — and any remaining ambiguity refuses.
// That cascade encodes Go package semantics, where a bare name IS visible
// from every file in the directory. The ES module system has no ambient
// directory scope: a bare call's callee is either defined in the caller's
// own file or explicitly imported. Two consequences the cascade gets wrong
// for JS/TS:
//
//   - A same-directory neighbour that defines the name (a test helper
//     shadowing a library export) captures the call even though the caller
//     never imports it — and explicitly imports the real target.
//   - Cross-directory ambiguity (the library export vs. several test-local
//     helpers of the same name) refuses, dropping every call edge to a
//     widely-used export (zustand's `createStore`: 20 refused call edges,
//     ~120 line-level false negatives).
//
// An exact symbol binding wins even if several imported files define the
// name. Without one, a unique callable in the import closure (directly or
// through re-export hops) supplies the evidence; ambiguity is not a pick.
//
// Language gate: the pick only runs for JS/TS caller files. This is a
// correctness requirement, not caution. In Go a bare call can never name a
// symbol from another package (that call is selector-shaped and resolved
// elsewhere), so letting an imported package's file outrank the caller's
// own directory would manufacture impossible edges. In Python a plain
// `import x` puts x's file in the closure without bringing `foo` into
// scope, so file-granularity closure membership is not scope evidence
// there. For the ES module system it is: every import form that binds a
// bare name (named, default, aliased) makes the target module's file the
// only place the name can come from. Go and Python resolution paths are
// bit-for-bit unchanged.
//
// Granularity: the closure is FILE-level, not the directory-level closure
// guardCrossPackageCallEdges uses. The guard's map is seeded with each
// file's own directory (so same-package calls survive the guard), which
// would make every same-dir shadow "import-reachable" and defeat point 3;
// and dir granularity cannot separate two same-named candidates living in
// one imported directory. The expansion machinery is shared, not
// duplicated: raw specifiers go through the exact resolveJSTSImportTarget
// helper resolveImport uses (relative join, tsconfig paths/baseUrl, npm
// alias, extension + index probing), so the closure is identical whether
// an import edge has already been resolved by this pass or not — the pick
// is order-independent inside the parallel worker phase.

func (r *Resolver) resolveJSRequireCallee(e *graph.Edge, funcName string, candidates []*graph.Node) (*graph.Node, string) {
	spec, _ := e.Meta[graph.JSLexicalRequireSpecMeta].(string)
	callerFile := e.FilePath
	if spec == "" || callerFile == "" || !isJSTSPath(callerFile) || len(candidates) == 0 {
		return nil, ""
	}
	target, trackedNpmSpec := r.resolveJSRequireTarget(e, spec)
	if target == "" {
		return nil, ""
	}
	targetFile := r.nodeFilePath(target)
	if pick, ambiguous := pickDirectNamedExportCandidate(r.cachedGetNode, targetFile, funcName, candidates); ambiguous || pick != nil {
		return pick, trackedNpmSpec
	}
	// Pass "" as excludeFile so explicit self-require (`require('./self')`)
	// and circular barrel re-exports back into callerFile still resolve.
	files := r.importClosureFiles("", map[string]struct{}{target: {}})
	return pickRequireClosureCandidate(r.cachedGetNode, files, funcName, candidates), trackedNpmSpec
}

func jsBareDirMatches(file graph.FileNodeIdentity, importPath string) bool {
	if importPath == "" {
		return false
	}
	fullDir := filePathDir(file.FilePath)
	relDir := fullDir
	if file.RepoPrefix != "" {
		if relDir == file.RepoPrefix {
			relDir = ""
		} else {
			relDir = strings.TrimPrefix(relDir, file.RepoPrefix+"/")
		}
	}
	if fullDir == importPath || relDir == importPath {
		return true
	}
	if !strings.Contains(importPath, "/") {
		return lastPathComponent(fullDir) == importPath
	}
	return strings.HasSuffix(fullDir, "/"+importPath) || strings.HasSuffix(relDir, "/"+importPath)
}

func (r *Resolver) resolveJSRequireTarget(e *graph.Edge, spec string) (string, string) {
	if target := r.resolveJSTSSpecTarget(e, spec); target != "" {
		return target, ""
	}
	callerFile := e.FilePath
	rewritten, _ := rewriteNpmAliasImport(r.npmAlias, callerFile, spec)
	for _, imp := range r.callerOutEdges(callerFile) {
		if imp == nil || imp.Kind != graph.EdgeImports {
			continue
		}
		marked, _ := imp.Meta[NpmImportSpecifierMeta].(string)
		if marked == "" {
			continue
		}
		mod, _ := splitImportSpecSymbol(marked)
		if marked != spec && marked != rewritten && mod != spec && mod != rewritten {
			continue
		}
		if target := r.importedTargetOf(imp); target != "" {
			return target, mod
		}
	}
	if !isJSTSBareSpecifier(callerFile, rewritten) || declaresExternalNpmDep(r.npmDep, callerFile, rewritten) {
		return "", ""
	}
	return r.resolveJSBareDirTarget(e, callerFile, rewritten), ""
}

// resolveJSBareDirTarget mirrors resolveImport's same-repo bare JS/TS
// directory entry-point lookup (for pnpm workspace members and local package
// directories reached via `require('pkg')`), while requiring multi-segment
// specifiers to match the full directory suffix rather than just the basename.
func (r *Resolver) resolveJSBareDirTarget(e *graph.Edge, callerFile, importPath string) string {
	callerRepo := r.callerRepoPrefix(e)
	callerLanguage := ""
	if source := r.cachedGetNode(callerFile); source != nil {
		callerLanguage = source.Language
	}
	collectAll := r.workspaceMembers != nil
	const (
		fallback = iota
		suffixMatch
		exactMatch
	)
	var sameRepo graph.FileNodeIdentity
	var sameRepoFound bool
	var sameRepoQuality int
	var sameRepoAll []graph.FileNodeIdentity
	consider := func(file graph.FileNodeIdentity) {
		if !isJSTSDirEntryPoint(file.FilePath) || !jsBareDirMatches(file, importPath) {
			return
		}
		if callerRepo != "" && file.RepoPrefix != callerRepo {
			return
		}
		fullDir := filePathDir(file.FilePath)
		dir := fullDir
		if file.RepoPrefix != "" {
			if dir == file.RepoPrefix {
				dir = ""
			} else {
				dir = strings.TrimPrefix(dir, file.RepoPrefix+"/")
			}
		}
		quality := fallback
		if callerLanguage != "" && dir != "" && file.Language == callerLanguage {
			switch {
			case dir == importPath || fullDir == importPath:
				quality = exactMatch
			case strings.HasSuffix(dir, "/"+importPath) || strings.HasSuffix(fullDir, "/"+importPath):
				quality = suffixMatch
			}
		}
		if quality < sameRepoQuality {
			return
		}
		if quality > sameRepoQuality {
			sameRepoFound = false
			sameRepoAll = nil
			sameRepoQuality = quality
		}
		if !sameRepoFound {
			sameRepo, sameRepoFound = file, true
		}
		if collectAll {
			sameRepoAll = append(sameRepoAll, file)
		}
	}
	stop := func() bool {
		return !collectAll && sameRepoQuality == exactMatch
	}
	if r.dirIndex != nil {
		for _, file := range r.dirIndex[importPath] {
			consider(file)
			if stop() {
				break
			}
		}
		if !stop() {
			for _, file := range r.lastDirIndex[lastPathComponent(importPath)] {
				consider(file)
				if stop() {
					break
				}
			}
		}
	} else {
		for file := range graph.FileNodeIdentitiesSeq(r.graph, nil) {
			dir := filePathDir(file.FilePath)
			if strings.HasSuffix(dir, lastPathComponent(importPath)) || dir == importPath {
				consider(file)
				if stop() {
					break
				}
			}
		}
	}
	if !sameRepoFound {
		return ""
	}
	if workspaceFile, ok := r.preferSameWorkspaceFile(callerFile, sameRepoAll); ok {
		sameRepo = workspaceFile
	}
	return sameRepo.ID
}

func jsFileExportsNamed(getNode func(string) *graph.Node, filePath, exportName, localName string) bool {
	if filePath == "" || exportName == "" || localName == "" {
		return false
	}
	f := getNode(filePath)
	if f == nil {
		return false
	}
	locals, _ := f.Meta[graph.JSLocalExportsMeta].(string)
	return graph.JSBinding(locals, exportName) == localName
}

// pickDirectNamedExportCandidate selects a unique directly named export on
// targetFile. Unexported declarations on targetFile are deliberately not
// preferred here so a private helper in a barrel cannot shadow `export * from`.
func pickDirectNamedExportCandidate(getNode func(string) *graph.Node, targetFile, funcName string, candidates []*graph.Node) (*graph.Node, bool) {
	if targetFile == "" {
		return nil, false
	}
	var pick *graph.Node
	for _, c := range candidates {
		if c.FilePath != targetFile || !isImportableCallee(c, funcName) || !jsFileExportsNamed(getNode, c.FilePath, funcName, c.Name) {
			continue
		}
		if pick != nil && pick.ID != c.ID {
			return nil, true
		}
		pick = c
	}
	return pick, false
}

func jsFileOnlyDefaultExport(getNode func(string) *graph.Node, filePath, localName string) bool {
	if filePath == "" || localName == "" {
		return false
	}
	f := getNode(filePath)
	if f == nil {
		return false
	}
	def, _ := f.Meta[graph.DefaultExportMeta].(string)
	return def == localName
}

// pickRequireClosureCandidate prefers a unique directly named export across
// the required barrel closure, falling back to any unique importable symbol
// only when the closure has no directly named export (e.g. pure CJS modules).
// An ES module's default-only export (`export default function f()`) is never
// a named property on `require()` and is excluded from the CJS fallback.
func pickRequireClosureCandidate(getNode func(string) *graph.Node, files map[string]struct{}, funcName string, candidates []*graph.Node) *graph.Node {
	if len(files) == 0 {
		return nil
	}
	var exported *graph.Node
	for _, c := range candidates {
		if !isImportableCallee(c, funcName) {
			continue
		}
		if _, ok := files[c.FilePath]; !ok {
			continue
		}
		if !jsFileExportsNamed(getNode, c.FilePath, funcName, c.Name) {
			continue
		}
		if exported != nil && exported.ID != c.ID {
			return nil
		}
		exported = c
	}
	if exported != nil {
		return exported
	}
	var pick *graph.Node
	for _, c := range candidates {
		if !isImportableCallee(c, funcName) {
			continue
		}
		if _, ok := files[c.FilePath]; !ok {
			continue
		}
		if jsFileOnlyDefaultExport(getNode, c.FilePath, c.Name) {
			continue
		}
		if pick != nil && pick.ID != c.ID {
			return nil
		}
		pick = c
	}
	return pick
}

type jsRequireTarget struct {
	target         string
	trackedNpmSpec string
}

func (cr *CrossRepoResolver) resolveJSRequireCall(e *graph.Edge, funcName string, stats *CrossRepoStats) {
	spec, _ := e.Meta[graph.JSLexicalRequireSpecMeta].(string)
	callerFile := e.FilePath
	if spec == "" || callerFile == "" || !isJSTSPath(callerFile) {
		stats.Unresolved++
		return
	}
	callerRepo := cr.callerRepoPrefix(e)
	callerWS := cr.callerWorkspaceID(e)
	resolved := cr.resolveJSRequireTarget(e, callerFile, callerRepo, callerWS, spec)
	if resolved.target == "" {
		stats.Unresolved++
		return
	}
	// Use cachedFindNodesByName rather than scopedCandidates: tracked npm
	// providers are intentionally omitted from reachableReposByFile, while
	// target + barrel closure + workspace checks below enforce exact reachability.
	rawCandidates := withoutReExportForwarders(cr.cachedFindNodesByName(funcName))
	if len(rawCandidates) == 0 {
		stats.Unresolved++
		return
	}
	candidates := make([]*graph.Node, 0, len(rawCandidates))
	for _, c := range rawCandidates {
		if isCrossRepoHop(callerRepo, c.RepoPrefix) && !cr.crossWorkspaceEligible(callerWS, candidateWorkspaceID(c), "") {
			continue
		}
		candidates = append(candidates, c)
	}
	if len(candidates) == 0 {
		stats.Unresolved++
		return
	}
	pick, ambiguous := pickDirectNamedExportCandidate(cr.cachedGetNode, cr.nodeFilePath(resolved.target), funcName, candidates)
	if ambiguous {
		stats.Unresolved++
		return
	}
	if pick == nil {
		files := cr.importClosureFiles(resolved.target)
		pick = pickRequireClosureCandidate(cr.cachedGetNode, files, funcName, candidates)
	}
	if pick == nil {
		stats.Unresolved++
		return
	}
	e.To = pick.ID
	e.Origin = graph.OriginASTResolved
	e.Confidence = 0.9
	if e.Meta == nil {
		e.Meta = map[string]any{}
	}
	e.Meta["resolution"] = "import_closure"
	if resolved.trackedNpmSpec != "" {
		e.Meta[NpmImportSpecifierMeta] = resolved.trackedNpmSpec
	} else {
		delete(e.Meta, NpmImportSpecifierMeta)
	}
	stats.Resolved++
	if isCrossRepoHop(callerRepo, pick.RepoPrefix) {
		e.CrossRepo = true
		stats.CrossRepoEdges++
		stats.ByRepo[pick.RepoPrefix]++
	}
}

func (cr *CrossRepoResolver) cachedOutEdges(file string) []*graph.Edge {
	cr.requireCacheMu.RLock()
	edges, ok := cr.outEdgesByFile[file]
	cr.requireCacheMu.RUnlock()
	if ok {
		return edges
	}
	loaded := cr.graph.GetOutEdges(file)
	cr.requireCacheMu.Lock()
	if edges, ok = cr.outEdgesByFile[file]; !ok {
		if cr.outEdgesByFile == nil {
			cr.outEdgesByFile = make(map[string][]*graph.Edge)
		}
		cr.outEdgesByFile[file] = loaded
		edges = loaded
	}
	cr.requireCacheMu.Unlock()
	return edges
}

func (cr *CrossRepoResolver) resolveJSRequireTarget(e *graph.Edge, callerFile, callerRepo, callerWS, spec string) jsRequireTarget {
	cacheKey := callerFile + "\x00" + spec
	cr.requireCacheMu.RLock()
	cached, ok := cr.requireTargetCache[cacheKey]
	cr.requireCacheMu.RUnlock()
	if ok {
		return cached
	}
	target := cr.resolveJSRequireTargetUncached(e, callerFile, callerRepo, callerWS, spec)
	cr.requireCacheMu.Lock()
	if cr.requireTargetCache == nil {
		cr.requireTargetCache = make(map[string]jsRequireTarget)
	}
	cr.requireTargetCache[cacheKey] = target
	cr.requireCacheMu.Unlock()
	return target
}

func (cr *CrossRepoResolver) resolveJSRequireTargetUncached(e *graph.Edge, callerFile, callerRepo, callerWS, spec string) jsRequireTarget {
	if to := resolveJSTSImportTarget(cr.cachedGetNode, cr.pathAlias, callerFile, spec); to != "" {
		return jsRequireTarget{target: to}
	}
	rewritten, npmAliased := rewriteNpmAliasImport(cr.npmAlias, callerFile, spec)
	if npmAliased {
		if to := resolveJSTSImportTarget(cr.cachedGetNode, cr.pathAlias, callerFile, rewritten); to != "" {
			return jsRequireTarget{target: to}
		}
	}
	if cr.npmPackages != nil && cr.eligibleTrackedNpmImport(e, spec) {
		memo := &npmImportMemo{
			probes: make(map[npmProbeKey]*graph.Node),
			nodes:  make(map[string]*graph.Node),
		}
		if node := cr.trackedNpmImport(callerRepo, callerWS, spec, memo); node != nil {
			return jsRequireTarget{target: node.ID, trackedNpmSpec: spec}
		}
	}
	for _, imp := range cr.cachedOutEdges(callerFile) {
		if imp == nil || imp.Kind != graph.EdgeImports {
			continue
		}
		marked, _ := imp.Meta[NpmImportSpecifierMeta].(string)
		if marked == "" {
			continue
		}
		mod, _ := splitImportSpecSymbol(marked)
		if marked != spec && marked != rewritten && mod != spec && mod != rewritten {
			continue
		}
		if to := cr.importedTargetOf(imp); to != "" {
			trackedSpec := ""
			if cr.eligibleTrackedNpmImport(e, mod) {
				trackedSpec = mod
			}
			return jsRequireTarget{target: to, trackedNpmSpec: trackedSpec}
		}
	}
	if !isJSTSBareSpecifier(callerFile, rewritten) || declaresExternalNpmDep(cr.npmDep, callerFile, rewritten) {
		return jsRequireTarget{}
	}
	collectAll := cr.workspaceMembers != nil
	var sameRepo graph.FileNodeIdentity
	var sameRepoFound bool
	var sameRepoAll, crossRepoAll []graph.FileNodeIdentity
	consider := func(file graph.FileNodeIdentity) {
		if !isJSTSDirEntryPoint(file.FilePath) {
			return
		}
		if file.RepoPrefix == callerRepo {
			if !jsBareDirMatches(file, rewritten) {
				return
			}
			if !sameRepoFound {
				sameRepo, sameRepoFound = file, true
			}
			if collectAll {
				sameRepoAll = append(sameRepoAll, file)
			}
			return
		}
		if dirMatchesImport(filePathDir(file.FilePath), rewritten) {
			crossRepoAll = append(crossRepoAll, file)
		}
	}
	stop := func() bool { return sameRepoFound && !collectAll }
	if cr.dirIndex != nil {
		for _, file := range cr.dirIndex[rewritten] {
			consider(file)
			if stop() {
				break
			}
		}
		if !sameRepoFound || collectAll {
			for _, file := range cr.lastDirIndex[lastPathComponent(rewritten)] {
				consider(file)
				if stop() {
					break
				}
			}
		}
	} else {
		for file := range graph.FileNodeIdentitiesSeq(cr.graph, nil) {
			dir := filePathDir(file.FilePath)
			if strings.HasSuffix(dir, lastPathComponent(rewritten)) || dir == rewritten {
				consider(file)
				if stop() {
					break
				}
			}
		}
	}
	if sameRepoFound {
		if workspaceFile, ok := cr.preferSameWorkspaceFile(callerFile, sameRepoAll); ok {
			sameRepo = workspaceFile
		}
		return jsRequireTarget{target: sameRepo.ID}
	}
	if picked, ok := cr.pickImportCandidate(callerWS, rewritten, crossRepoAll); ok {
		return jsRequireTarget{target: picked.ID}
	}
	return jsRequireTarget{}
}

func (cr *CrossRepoResolver) nodeFilePath(id string) string {
	if id == "" {
		return ""
	}
	n := cr.cachedGetNode(id)
	if n == nil {
		return ""
	}
	if n.FilePath != "" {
		return n.FilePath
	}
	if n.Kind == graph.KindFile {
		return n.ID
	}
	return ""
}

func (cr *CrossRepoResolver) importedTargetOf(e *graph.Edge) string {
	to := e.To
	if to == "" {
		return ""
	}
	if graph.IsUnresolvedTarget(to) {
		payload, ok := strings.CutPrefix(graph.UnresolvedName(to), "import::")
		if !ok || payload == "" {
			return ""
		}
		callerFile := jsTSImportCallerFile(e)
		target := resolveJSTSImportTarget(cr.cachedGetNode, cr.pathAlias, callerFile, payload)
		if target == "" {
			if rewritten, aliased := rewriteNpmAliasImport(cr.npmAlias, e.FilePath, payload); aliased {
				target = resolveJSTSImportTarget(cr.cachedGetNode, cr.pathAlias, callerFile, rewritten)
			}
		}
		return target
	}
	if strings.HasPrefix(to, "external::") || strings.HasPrefix(to, "dep::") || graph.IsStdlibStub(to) {
		return ""
	}
	return to
}

func (cr *CrossRepoResolver) importClosureFiles(target string) map[string]struct{} {
	files := make(map[string]struct{})
	var pendingBarrels []string
	addTarget := func(f string) {
		if f == "" {
			return
		}
		if _, dup := files[f]; dup {
			return
		}
		files[f] = struct{}{}
		pendingBarrels = append(pendingBarrels, f)
	}
	addTarget(cr.nodeFilePath(target))
	for len(pendingBarrels) > 0 {
		f := pendingBarrels[len(pendingBarrels)-1]
		pendingBarrels = pendingBarrels[:len(pendingBarrels)-1]
		for _, e := range cr.cachedOutEdges(f) {
			if e == nil || e.Kind != graph.EdgeReExports {
				continue
			}
			addTarget(cr.nodeFilePath(cr.importedTargetOf(e)))
		}
	}
	return files
}

func pickUniqueFileCandidate(files map[string]struct{}, funcName string, candidates []*graph.Node) *graph.Node {
	if len(files) == 0 {
		return nil
	}
	var pick *graph.Node
	for _, c := range candidates {
		if !isImportableCallee(c, funcName) {
			continue
		}
		if _, ok := files[c.FilePath]; !ok {
			continue
		}
		if pick != nil {
			// Two imported files (or one file with two same-named
			// top-level symbols) both define the name — the import
			// statement alone cannot arbitrate. Refuse, exactly like
			// the no-import case.
			return nil
		}
		pick = c
	}
	return pick
}

// pickImportEvidenceCallee is the file-closure fallback, not an explicit binding.
func (r *Resolver) pickImportEvidenceCallee(callerFile, funcName string, candidates []*graph.Node) *graph.Node {
	if callerFile == "" || len(candidates) == 0 || !isJSTSPath(callerFile) {
		return nil
	}
	if hasLocalValue(callerFile, candidates) {
		return nil
	}
	imports := r.importEvidenceFor(callerFile)
	imports.closureOnce.Do(func() { imports.files = r.importClosureFiles(callerFile, imports.direct) })
	return pickUniqueFileCandidate(imports.files, funcName, candidates)
}

// jsCallHasScopeEvidence reports scope facts an edit can change while the call
// keeps its shape, so neither a skip nor a carried binding can stand in for them.
func jsCallHasScopeEvidence(e *graph.Edge) bool {
	return e.Meta[graph.JSLexicalBindingMeta] != nil || e.Meta[graph.JSAmbientShadowMeta] != nil
}

// tryResolveUnshadowedBinding applies the explicit and generated tiers. A
// module-scope declaration owns its name, so neither applies. A module local with
// no value node stays unresolved rather than falling through to a same-named
// generated target by name.
func (r *Resolver) tryResolveUnshadowedBinding(e *graph.Edge, name string, candidates []*graph.Node, stats *ResolveStats) bool {
	ambient, _ := e.Meta[graph.JSAmbientShadowMeta].(string)
	if ambient != graph.JSShadowModule && r.tryResolveExplicitImport(e, name, stats) {
		return true
	}
	if ambient == "" {
		return r.tryResolveNuxtAutoImport(e, name, stats)
	}
	if ambient == graph.JSShadowModule && r.nuxtAutoImport != nil && !hasSameFileValue(e.FilePath, candidates) {
		if stem, _ := r.nuxtAutoImport(e.FilePath, name); stem != "" {
			parkJSBinding(e, graph.JSPendingParameter, stats)
			return true
		}
	}
	return false
}

// hasSameFileValue includes nested declarations, unlike hasLocalValue: a shadow
// stamped on this call can be owned by a helper inside the enclosing function.
func hasSameFileValue(file string, candidates []*graph.Node) bool {
	for _, c := range candidates {
		if c.FilePath != file {
			continue
		}
		switch c.Kind {
		case graph.KindFunction, graph.KindMethod, graph.KindVariable, graph.KindConstant:
			return true
		}
	}
	return false
}

// The module edge retains local:export pairs even for statements too large
// to emit a symbol edge per specifier. Bare entries have identical names.
func (r *Resolver) tryResolveExplicitImport(e *graph.Edge, name string, stats *ResolveStats) bool {
	imports := r.callerEvidence(e.FilePath)
	imports.bindingsOnce.Do(func() {
		for _, imp := range r.callerOutEdges(e.FilePath) {
			if imp.Kind != graph.EdgeImports {
				continue
			}
			raw, _ := imp.Meta[graph.ImportBindingsMeta].(string)
			for local, exported := range graph.JSBindingPairs(raw) {
				if _, taken := imports.bindings[local]; taken || exported == "" {
					continue
				}
				if imports.bindings == nil {
					imports.bindings = make(map[string]jsImportBinding)
				}
				imports.bindings[local] = jsImportBinding{imp, exported}
			}
		}
	})
	binding, ok := imports.bindings[name]
	if !ok {
		return false
	}
	stem, file := r.jsBindingSource(binding.edge)
	r.resolveExactCall(e, name, stem, file, binding.exported, graph.JSPendingImport, "import_binding", stats)
	return true
}

// Expand the source spelling even if the old import row was parked on an
// external stub, or resolved before a higher priority module existed.
func (r *Resolver) jsBindingSource(e *graph.Edge) (stem, file string) {
	spec, _ := e.Meta[graph.JSImportSourceMeta].(string)
	spec, _ = splitImportSpecSymbol(spec)
	stem = expandJSTSSpecifier(r.pathAlias, e.FilePath, spec)
	if stem == "" {
		if rewritten, ok := rewriteNpmAliasImport(r.npmAlias, e.FilePath, spec); ok {
			stem = expandJSTSSpecifier(r.pathAlias, e.FilePath, rewritten)
		}
	}
	if stem != "" {
		if target := probeJSTSFile(r.cachedGetNode, stem); target != nil {
			return stem, target.ID
		}
		return stem, ""
	}
	file = r.importedFileOf(e)
	return file, file
}

func (r *Resolver) resolveExactCall(e *graph.Edge, local, stem, file, exported, kind, evidence string, stats *ResolveStats) {
	if e.Meta == nil {
		e.Meta = make(map[string]any)
	}
	e.Meta[graph.JSBindingNameMeta] = local
	if stem != "" {
		e.Meta[graph.JSBindingModuleMeta] = graph.JSModuleStem(stem)
	} else {
		delete(e.Meta, graph.JSBindingModuleMeta)
	}
	if target := r.exportedCallee(e.FilePath, file, exported); target != nil {
		bindImportCall(e, target, evidence, stats)
	} else {
		parkJSBinding(e, kind, stats)
	}
}

func parkJSBinding(e *graph.Edge, binding string, stats *ResolveStats) {
	if e.Meta == nil {
		e.Meta = make(map[string]any)
	}
	e.Meta[graph.JSPendingBindingMeta] = binding
	stats.Unresolved++
}

func bindImportCall(e *graph.Edge, target *graph.Node, evidence string, stats *ResolveStats) {
	e.To, e.Origin, e.Confidence = target.ID, graph.OriginASTResolved, 0.9
	if e.Meta == nil {
		e.Meta = make(map[string]any)
	}
	e.Meta["resolution"] = evidence
	stats.Resolved++
}

// jsConsumerMode is what a consumer's extension proves about a CommonJS default
// import. A .js or .ts file depends on package "type", compiler module settings
// or a bundler, none of which identify the runtime semantics on their own.
type jsConsumerMode uint8

const (
	jsConsumerUnproven jsConsumerMode = iota
	jsConsumerNative                  // default import is module.exports
	jsConsumerCompiled                // interop helper reads an __esModule .default
)

func jsConsumerModeOf(file string) jsConsumerMode {
	switch path.Ext(file) {
	case ".mjs", ".mts":
		return jsConsumerNative
	case ".cjs", ".cts":
		return jsConsumerCompiled
	}
	return jsConsumerUnproven
}

// A named export (including a local alias or an imported value) wins over
// stars. Stars never forward default and two distinct implementations conflict.
func (r *Resolver) exportedCallee(caller, file, name string) *graph.Node {
	type binding struct {
		file, name string
		mode       jsConsumerMode
	}
	pending := []binding{{file, name, jsConsumerModeOf(caller)}}
	var seen map[binding]bool
	var pick *graph.Node
	for len(pending) > 0 {
		b := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		if b.file == "" || b.name == graph.JSNamespaceBinding || seen[b] {
			continue
		}
		f := r.cachedGetNode(b.file)
		if f == nil {
			continue
		}
		locals, _ := f.Meta[graph.JSLocalExportsMeta].(string)
		local := graph.JSBinding(locals, b.name)
		if b.name == graph.JSDefaultBinding {
			// Compiled interop reads an __esModule object's .default; native ESM
			// takes module.exports. Without a proven consumer mode, both modes
			// must justify the same callable; an absent callable in either mode
			// is disagreement.
			native := local
			if native == "" {
				native, _ = f.Meta[graph.DefaultExportMeta].(string)
			}
			interop := graph.JSBinding(locals, graph.JSInteropDefaultBinding)
			esModule := interop != "" || graph.JSBinding(locals, graph.JSInteropMarkerBinding) != ""
			switch b.mode {
			case jsConsumerNative:
				local = native
			case jsConsumerCompiled:
				if esModule {
					local = interop
				} else {
					local = native
				}
			default:
				if esModule {
					if native == "" || native != interop {
						return nil
					}
				}
				local = native
			}
		}
		if local != "" {
			if n := r.cachedGetNode(graph.JSExportID(f, local)); n != nil && !graph.IsReExportNode(n) && importableValueKind(n.Kind) {
				if pick != nil && pick.ID != n.ID {
					return nil
				}
				pick = n
				continue
			}
		}
		if seen == nil {
			seen = map[binding]bool{}
		}
		seen[b] = true
		if local == "" {
			if n := r.cachedGetNode(graph.JSExportID(f, b.name)); graph.IsReExportNode(n) {
				orig, _ := n.Meta["reexport_original"].(string)
				if orig == "" {
					orig = b.name
				}
				for _, edge := range r.callerOutEdges(n.ID) {
					if edge.Kind == graph.EdgeReExports {
						_, file := r.jsBindingSource(edge)
						pending = append(pending, binding{file, orig, jsConsumerModeOf(b.file)})
					}
				}
				continue
			}
		}
		edges := r.callerOutEdges(b.file)
		explicit := false
		for _, edge := range edges {
			bindings, _ := edge.Meta[graph.ImportBindingsMeta].(string)
			var forwarded string
			switch {
			case local != "" && edge.Kind == graph.EdgeImports:
				forwarded = graph.JSBinding(bindings, local)
			case local == "" && edge.Kind == graph.EdgeReExports:
				forwarded = graph.JSBinding(bindings, b.name)
			default:
				continue
			}
			if forwarded != "" {
				explicit = true
				_, file := r.jsBindingSource(edge)
				pending = append(pending, binding{file, forwarded, jsConsumerModeOf(b.file)})
			}
		}
		if local != "" || explicit || b.name == graph.JSDefaultBinding {
			continue
		}
		for _, edge := range edges {
			if edge.Kind == graph.EdgeReExports && edge.Meta[graph.StarReExportMeta] == true {
				_, file := r.jsBindingSource(edge)
				pending = append(pending, binding{file, b.name, jsConsumerModeOf(b.file)})
			}
		}
	}
	return pick
}

func importBindingAlias(e *graph.Edge) string {
	if alias, _ := e.Meta["import_alias"].(string); alias != "" {
		return alias
	}
	return e.Alias
}

// Methods and type-only declarations do not introduce a module-local callable.
func hasLocalValue(file string, candidates []*graph.Node) bool {
	for _, c := range candidates {
		if c.FilePath == file && isImportableCallee(c, c.Name) {
			return true
		}
	}
	return false
}

// isImportableCallee reports whether a candidate is a symbol an ES import
// of `funcName` can actually bind a bare call to: a TOP-LEVEL function,
// variable, or constant (ID == <file>::<name>, the module's own namespace).
// Methods and nested/local bindings are never import targets — importing a
// file does not bring a class method or a function-scoped helper into the
// caller's scope, so accepting them would mint impossible edges. The
// variable/constant kinds are load-bearing, not a nicety: an identifier
// alias-cast export (zustand's `export const persist = persistImpl as
// unknown as Persist`) lands as a KindVariable/KindConstant node, the
// exact value-callee shape whose repo-wide ambiguity refuses call edges
// today (see pickTopLevelValueCallee).
func isImportableCallee(c *graph.Node, funcName string) bool {
	return c.FilePath != "" && c.ID == c.FilePath+"::"+funcName && importableValueKind(c.Kind)
}

func importableValueKind(kind graph.NodeKind) bool {
	switch kind {
	case graph.KindFunction, graph.KindVariable, graph.KindConstant:
		return true
	}
	return false
}

// direct holds the expanded targets of the caller's imports. Exact bindings
// are resolved through module exports; these targets only seed the file tier.
// Walk barrels only if a call actually needs the file-level evidence.
type callerImportEvidence struct {
	directOnce   sync.Once
	direct       map[string]struct{}
	closureOnce  sync.Once
	files        map[string]struct{}
	bindingsOnce sync.Once
	bindings     map[string]jsImportBinding // first import statement wins
}

type jsImportBinding struct {
	edge     *graph.Edge
	exported string
}

func (r *Resolver) clearImportEvidence() {
	r.importFilesMu.Lock()
	r.importsByCaller = nil
	r.importOutByFile = nil
	r.importFilesMu.Unlock()
}

func (r *Resolver) callerEvidence(callerFile string) *callerImportEvidence {
	r.importFilesMu.RLock()
	imports := r.importsByCaller[callerFile]
	r.importFilesMu.RUnlock()
	if imports == nil {
		r.importFilesMu.Lock()
		if imports = r.importsByCaller[callerFile]; imports == nil {
			if r.importsByCaller == nil {
				r.importsByCaller = make(map[string]*callerImportEvidence)
			}
			imports = new(callerImportEvidence)
			r.importsByCaller[callerFile] = imports
		}
		r.importFilesMu.Unlock()
	}
	return imports
}

func (r *Resolver) importEvidenceFor(callerFile string) *callerImportEvidence {
	imports := r.callerEvidence(callerFile)
	imports.directOnce.Do(func() {
		for _, e := range r.callerOutEdges(callerFile) {
			if e.Kind != graph.EdgeImports {
				continue
			}
			target := r.importedTargetOf(e)
			if target == "" {
				continue
			}
			if imports.direct == nil {
				imports.direct = make(map[string]struct{})
			}
			imports.direct[target] = struct{}{}
		}
	})
	return imports
}

func (r *Resolver) importClosureFiles(excludeFile string, direct map[string]struct{}) map[string]struct{} {
	if len(direct) == 0 {
		return nil
	}
	files := make(map[string]struct{})
	var pendingBarrels []string
	addTarget := func(f string) {
		if f == "" || (excludeFile != "" && f == excludeFile) {
			return
		}
		if _, dup := files[f]; dup {
			return
		}
		files[f] = struct{}{}
		pendingBarrels = append(pendingBarrels, f)
	}

	for target := range direct {
		addTarget(r.nodeFilePath(target))
	}
	// Barrel hops: an import that lands on a re-exporting module makes the
	// re-exported modules visible too (`import { createStore } from
	// 'zustand'` → src/index.ts → src/vanilla.ts). Same transitive walk
	// buildImportClosure performs for the guard, at file granularity.
	for len(pendingBarrels) > 0 {
		f := pendingBarrels[len(pendingBarrels)-1]
		pendingBarrels = pendingBarrels[:len(pendingBarrels)-1]
		for _, e := range r.callerOutEdges(f) {
			if e.Kind != graph.EdgeReExports {
				continue
			}
			addTarget(r.nodeFilePath(r.importedTargetOf(e)))
		}
	}

	return files
}

// importedFileOf maps an EdgeImports / EdgeReExports edge to the file path
// of the module it names, or "" when the edge targets nothing indexable
// (third-party package, stdlib stub, dep contract, unexpandable specifier).
func (r *Resolver) importedFileOf(e *graph.Edge) string {
	return r.nodeFilePath(r.importedTargetOf(e))
}

func (r *Resolver) resolveJSTSSpecTarget(e *graph.Edge, payload string) string {
	// Same expansion, same precedence as resolveImport: tsconfig
	// paths / relative join first, npm-alias rewrite second.
	callerFile := jsTSImportCallerFile(e)
	target := resolveJSTSImportTarget(r.cachedGetNode, r.pathAlias, callerFile, payload)
	if target == "" {
		if rewritten, aliased := rewriteNpmAliasImport(r.npmAlias, e.FilePath, payload); aliased {
			target = resolveJSTSImportTarget(r.cachedGetNode, r.pathAlias, callerFile, rewritten)
		}
	}
	return target
}

// importedTargetOf shares symbol/file expansion across both evidence tiers.
// Parallel resolve workers mutate cloned edges, not these stored import edges.
func (r *Resolver) importedTargetOf(e *graph.Edge) string {
	to := e.To
	if to == "" {
		return ""
	}
	if graph.IsUnresolvedTarget(to) {
		payload, ok := strings.CutPrefix(graph.UnresolvedName(to), "import::")
		if !ok || payload == "" {
			return ""
		}
		return r.resolveJSTSSpecTarget(e, payload)
	}
	if strings.HasPrefix(to, "external::") || strings.HasPrefix(to, "dep::") || graph.IsStdlibStub(to) {
		return ""
	}
	return to
}

// nodeFilePath returns the file path of the node id names — the node's own
// path for a file node, its defining file for a symbol node (a per-binding
// import edge resolves to the exported symbol when it exists).
func (r *Resolver) nodeFilePath(id string) string {
	if id == "" {
		return ""
	}
	n := r.cachedGetNode(id)
	if n == nil {
		return ""
	}
	if n.FilePath != "" {
		return n.FilePath
	}
	if n.Kind == graph.KindFile {
		return n.ID
	}
	return ""
}

// callerOutEdges returns a caller file's outgoing edges from the pass
// prefetch when prefetchImportEdges loaded them, else from the store.
func (r *Resolver) callerOutEdges(callerFile string) []*graph.Edge {
	r.importFilesMu.RLock()
	edges, ok := r.importOutByFile[callerFile]
	r.importFilesMu.RUnlock()
	if ok {
		return edges
	}
	// A re-export hop can reach a file outside the prefetched frontier.
	r.importFilesMu.Lock()
	defer r.importFilesMu.Unlock()
	if edges, ok = r.importOutByFile[callerFile]; !ok {
		edges = r.graph.GetOutEdges(callerFile)
		if r.importOutByFile == nil {
			r.importOutByFile = make(map[string][]*graph.Edge)
		}
		r.importOutByFile[callerFile] = edges
	}
	return edges
}

// prefetchImportEdges loads, in one batched read, the outgoing edges of
// every JS/TS caller file behind edges — the import statements
// importEvidenceFor reads for bare calls. known is an optional
// already-loaded out-edge map (the incremental page frontier) consumed
// before asking the store. Without this every JS/TS call edge cost one
// point GetOutEdges on the caller file, which on a disk backend is one
// round-trip per call.
func (r *Resolver) prefetchImportEdges(edges []*graph.Edge, known map[string][]*graph.Edge) {
	if len(edges) == 0 {
		return
	}
	r.importFilesMu.Lock()
	defer r.importFilesMu.Unlock()
	if r.importOutByFile == nil {
		r.importOutByFile = make(map[string][]*graph.Edge)
	}
	var missing []string
	seen := make(map[string]struct{})
	for _, e := range edges {
		if e == nil {
			continue
		}
		caller := e.FilePath
		if caller == "" || !isJSTSPath(caller) {
			continue
		}
		if _, dup := seen[caller]; dup {
			continue
		}
		seen[caller] = struct{}{}
		if _, ok := r.importOutByFile[caller]; ok {
			continue
		}
		if out, ok := known[caller]; ok {
			r.importOutByFile[caller] = out
			continue
		}
		missing = append(missing, caller)
	}
	if len(missing) == 0 {
		return
	}
	byFile := r.graph.GetOutEdgesByNodeIDs(missing)
	for _, caller := range missing {
		r.importOutByFile[caller] = byFile[caller]
	}
}
