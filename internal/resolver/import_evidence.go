package resolver

import (
	"maps"
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
// After language-specific scope and same-file resolution, an unrenamed import
// of a directly named export beats file-level evidence. Without a symbol pick,
// exactly one candidate in the caller's imported files (including transitive
// barrel re-exports) wins. Otherwise leave the locality cascade unchanged.
// Same-file declarations block both import tiers. Restrict these rules to
// JS/TS: importing a package does not put its bare names in scope in Go.

func jsLexicalCallBinding(e *graph.Edge, target string) string {
	if e == nil || e.Kind != graph.EdgeCalls || e.Meta == nil {
		return ""
	}
	if strings.HasPrefix(target, "*.") || strings.HasPrefix(target, "import::") || strings.HasPrefix(target, "extern::") {
		return ""
	}
	b, _ := e.Meta[graph.JSLexicalBindingMeta].(string)
	return b
}

// handleJSLexicalRequire resolves a bare JS/TS call bound to an unshadowed
// static require(spec) strictly against spec (and its ES/CJS barrel re-exports)
// using O(1) point lookups without eager FindNodesByName seeding.
func (r *Resolver) handleJSLexicalRequire(e *graph.Edge, funcName, callerRepo string, stats *ResolveStats) {
	if pick, trackedNpmSpec := r.resolveJSRequireCallee(e, funcName, callerRepo); pick != nil {
		e.To = pick.ID
		e.Origin = graph.OriginASTResolved
		e.Confidence = 0.9
		if e.Meta == nil {
			e.Meta = map[string]any{}
		}
		clearEdgeTerminal(e)
		e.Meta["resolution"] = "import_closure"
		if trackedNpmSpec != "" {
			e.Meta[NpmImportSpecifierMeta] = trackedNpmSpec
		} else {
			delete(e.Meta, NpmImportSpecifierMeta)
		}
		stats.Resolved++
		return
	}
	stats.Unresolved++
}

// handleJSLexicalBinding handles a bare JS/TS call whose name is bound in an
// enclosing scope. A parameter always stays unresolved; a local with a
// same-file callable/value candidate continues to the generic cascade (and
// otherwise stays unresolved); a local initialized from an unshadowed static
// require(spec) resolves strictly against spec (and its barrel re-exports) and
// otherwise stays unresolved rather than falling through to file-wide imports
// or locality heuristics.
func (r *Resolver) handleJSLexicalBinding(e *graph.Edge, funcName string, candidates []*graph.Node, stats *ResolveStats) bool {
	switch e.Meta[graph.JSLexicalBindingMeta] {
	case graph.JSPendingParameter:
		stats.Unresolved++
		return true
	case graph.JSLexicalLocal:
		for _, c := range candidates {
			if c.FilePath != e.FilePath {
				continue
			}
			switch c.Kind {
			case graph.KindFunction, graph.KindMethod, graph.KindVariable, graph.KindConstant:
				return false
			}
		}
		stats.Unresolved++
		return true
	case graph.JSLexicalRequire:
		r.handleJSLexicalRequire(e, funcName, r.callerRepoPrefix(e), stats)
		return true
	}
	return false
}

func (r *Resolver) resolveJSRequireCallee(e *graph.Edge, funcName, callerRepo string) (*graph.Node, string) {
	spec, _ := e.Meta[graph.JSLexicalRequireSpecMeta].(string)
	callerFile := e.FilePath
	if spec == "" || callerFile == "" || !isJSTSPath(callerFile) {
		return nil, ""
	}
	lookupName, _ := e.Meta[graph.JSLexicalRequireSymbolMeta].(string)
	if lookupName == "" {
		lookupName = funcName
	}
	if lookupName == "" {
		return nil, ""
	}
	target, trackedNpmSpec := r.resolveJSRequireTarget(e, spec)
	targetFile := nodeFilePathWith(r.requireGetNode, target)
	if targetFile == "" {
		return nil, ""
	}
	env := jsRequireEnv{
		getNode:  r.requireGetNode,
		outEdges: r.callerOutEdges,
		resolveReExport: func(re *graph.Edge) (string, string) {
			return r.importedTargetOf(re), ""
		},
		eligible: func(n *graph.Node) bool {
			return n != nil && (callerRepo == "" || n.RepoPrefix == callerRepo)
		},
		fetchCandidates: func(name string) []*graph.Node {
			return withoutReExportForwarders(r.cachedFindNodesByNameInRepoForEdge(name, callerRepo, e))
		},
	}
	return resolveJSRequireFromTargetFile(env, targetFile, lookupName, trackedNpmSpec)
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
	// Node always loads a core module for a bare specifier, never a
	// same-named local directory.
	if !isJSTSBareSpecifier(callerFile, rewritten) || isNodeCoreModule(rewritten) ||
		declaresExternalNpmDep(r.npmDep, callerFile, spec) ||
		declaresExternalNpmDep(r.npmDep, callerFile, rewritten) {
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
	if source := r.requireGetNode(callerFile); source != nil {
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

// --- Shared Steps 1-5 require() + ES/CJS barrel resolution -----------------

type jsRequireEnv struct {
	getNode         func(id string) *graph.Node
	outEdges        func(nodeID string) []*graph.Edge
	resolveReExport func(e *graph.Edge) (target string, trackedNpmSpec string)
	eligible        func(n *graph.Node) bool
	fetchCandidates func(name string) []*graph.Node
}

type reExportTier uint8

const (
	tierNone reExportTier = iota
	tierBinding
	tierFallback
	tierExact
)

type reExportVisitKey struct {
	file string
	name string
}

type reExportMemoEntry struct {
	pick           *graph.Node
	trackedNpmSpec string
	tier           reExportTier
	ambiguous      bool
	hasReExports   bool
}

type reExportWalkState struct {
	inStack map[reExportVisitKey]bool
	memo    map[reExportVisitKey]reExportMemoEntry
	visits  int
}

type reExportResult struct {
	pick           *graph.Node
	trackedNpmSpec string
	tier           reExportTier
	ambiguous      bool
	hasReExports   bool
	cycleHit       bool
	depthTruncated bool
}

func reExportAlias(e *graph.Edge) string {
	if e == nil {
		return ""
	}
	if e.Alias != "" {
		return e.Alias
	}
	if e.Meta != nil {
		if alias, _ := e.Meta["import_alias"].(string); alias != "" {
			return alias
		}
	}
	return ""
}

func reExportOriginal(e *graph.Edge) string {
	if e == nil {
		return ""
	}
	if e.Meta != nil {
		if orig, _ := e.Meta["reexport_original"].(string); orig != "" {
			return orig
		}
	}
	to := e.To
	if graph.IsUnresolvedTarget(to) {
		if payload, ok := strings.CutPrefix(graph.UnresolvedName(to), "import::"); ok {
			_, orig := splitImportSpecSymbol(payload)
			return orig
		}
		return ""
	}
	if strings.HasPrefix(to, "external::") {
		_, orig := splitImportSpecSymbol(strings.TrimPrefix(to, "external::"))
		return orig
	}
	if strings.HasPrefix(to, "dep::") || graph.IsStdlibStub(to) {
		return ""
	}
	_, sym := splitImportSpecSymbol(to)
	return sym
}

func reExportBindings(e *graph.Edge) map[string]any {
	if e == nil || e.Meta == nil {
		return nil
	}
	b, _ := e.Meta["reexport_bindings"].(map[string]any)
	return b
}

func isReExportWildcardFlag(e *graph.Edge) bool {
	if e == nil || e.Meta == nil {
		return false
	}
	v, _ := e.Meta["reexport_wildcard"].(bool)
	return v
}

func isNamespaceReExport(e *graph.Edge) bool {
	if e == nil {
		return false
	}
	if e.Meta != nil {
		if ns, _ := e.Meta["reexport_namespace"].(bool); ns {
			return true
		}
	}
	if len(reExportBindings(e)) > 0 {
		return false
	}
	return reExportOriginal(e) == "" && reExportAlias(e) != ""
}

func nodeFilePathWith(getNode func(string) *graph.Node, id string) string {
	if id == "" {
		return ""
	}
	n := getNode(id)
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

func resolveJSRequireFromTargetFile(env jsRequireEnv, targetFile, lookupName, initialTrackedNpmSpec string) (*graph.Node, string) {
	if targetFile == "" || lookupName == "" {
		return nil, ""
	}
	// Step 1: Direct ES named export fast path (depth == 0).
	if n := env.getNode(targetFile + "::" + lookupName); n != nil &&
		isImportableCallee(n, lookupName) &&
		!graph.IsReExportNode(n) &&
		n.Meta["js_cjs_reexport_binding"] != true &&
		n.Meta["js_named_export"] == true &&
		env.eligible(n) {
		return n, initialTrackedNpmSpec
	}

	// Step 2: Unified precedence-aware ES/CJS barrel walk.
	res := pickDirectReExportCandidate(env, targetFile, lookupName, 0, &reExportWalkState{})

	// Step 3: Accept unambiguous hit (tierExact beats dead truncated branches).
	if res.ambiguous {
		return nil, ""
	}
	if res.pick != nil && (res.tier == tierExact || !res.depthTruncated) {
		spec := res.trackedNpmSpec
		if spec == "" {
			spec = initialTrackedNpmSpec
		}
		return res.pick, spec
	}

	// Step 4: Zero-alloc leaf fast path when targetFile has no re-exports.
	if !res.hasReExports {
		if n := env.getNode(targetFile + "::" + lookupName); n != nil &&
			isImportableCallee(n, lookupName) &&
			!graph.IsReExportNode(n) &&
			env.eligible(n) {
			return n, initialTrackedNpmSpec
		}
		return nil, ""
	}

	// Step 5: Depth/visit-truncated fallback only (0% of normal graphs).
	if res.depthTruncated && env.fetchCandidates != nil {
		files, npmSpecByFile := requireClosureFiles(env, targetFile, lookupName)
		candidates := env.fetchCandidates(lookupName)
		if pick := pickRequireClosureCandidate(files, lookupName, candidates); pick != nil {
			spec := npmSpecByFile[pick.FilePath]
			if spec == "" {
				spec = initialTrackedNpmSpec
			}
			return pick, spec
		}
		if res.pick != nil {
			spec := res.trackedNpmSpec
			if spec == "" {
				spec = initialTrackedNpmSpec
			}
			return res.pick, spec
		}
	}
	return nil, ""
}

func pickDirectReExportCandidate(env jsRequireEnv, file, exportedName string, depth int, state *reExportWalkState) reExportResult {
	if file == "" || exportedName == "" {
		return reExportResult{}
	}
	if depth > 8 || state.visits >= 256 {
		return reExportResult{depthTruncated: true}
	}

	fileOut := env.outEdges(file)
	hasFileReExports := false
	for _, e := range fileOut {
		if e != nil && e.Kind == graph.EdgeReExports && !isNamespaceReExport(e) {
			hasFileReExports = true
			break
		}
	}
	fwd := env.getNode(file + "::" + exportedName)
	hasFwd := fwd != nil && graph.IsReExportNode(fwd)
	hasReExports := hasFileReExports || hasFwd
	if depth == 0 && !hasReExports {
		return reExportResult{}
	}

	key := reExportVisitKey{file: file, name: exportedName}
	if state.inStack[key] {
		return reExportResult{hasReExports: hasReExports, cycleHit: true}
	}
	if state.memo != nil {
		if entry, ok := state.memo[key]; ok {
			return reExportResult{
				pick:           entry.pick,
				trackedNpmSpec: entry.trackedNpmSpec,
				tier:           entry.tier,
				ambiguous:      entry.ambiguous,
				hasReExports:   entry.hasReExports,
			}
		}
	}
	if state.inStack == nil {
		state.inStack = make(map[reExportVisitKey]bool)
	}
	state.inStack[key] = true
	defer delete(state.inStack, key)
	state.visits++

	finish := func(res reExportResult) reExportResult {
		res.hasReExports = res.hasReExports || hasReExports
		if !res.cycleHit && !res.depthTruncated {
			if state.memo == nil {
				state.memo = make(map[reExportVisitKey]reExportMemoEntry)
			}
			state.memo[key] = reExportMemoEntry{
				pick:           res.pick,
				trackedNpmSpec: res.trackedNpmSpec,
				tier:           res.tier,
				ambiguous:      res.ambiguous,
				hasReExports:   res.hasReExports,
			}
		}
		return res
	}

	// Direct exported leaf at depth > 0.
	if depth > 0 {
		if n := env.getNode(file + "::" + exportedName); n != nil &&
			isImportableCallee(n, exportedName) &&
			!graph.IsReExportNode(n) &&
			n.Meta["js_cjs_reexport_binding"] != true &&
			n.Meta["js_named_export"] == true &&
			env.eligible(n) {
			return finish(reExportResult{pick: n, tier: tierExact})
		}
	}

	innerSpec := func(childSpec, hopSpec string) string {
		if childSpec != "" {
			return childSpec
		}
		return hopSpec
	}

	type bucket struct {
		pick      *graph.Node
		npmSpec   string
		ambiguous bool
	}
	mergeBucket := func(b *bucket, cand *graph.Node, spec string, amb bool) {
		if amb {
			b.pick = nil
			b.npmSpec = ""
			b.ambiguous = true
			return
		}
		if cand == nil || b.ambiguous {
			return
		}
		if b.pick == nil {
			b.pick = cand
			b.npmSpec = spec
			return
		}
		if b.pick.ID == cand.ID {
			if spec != "" && (b.npmSpec == "" || spec < b.npmSpec) {
				b.npmSpec = spec
			}
			return
		}
		b.pick = nil
		b.npmSpec = ""
		b.ambiguous = true
	}

	// Phase 1: Explicit named EdgeReExports + minted ES reexport node.
	var phase1 bucket
	var phase1Cycle, phase1Truncated bool

	handlePhase1Target := func(target, hopNpmSpec, orig string) {
		if target == "" {
			return
		}
		n := env.getNode(target)
		if n == nil {
			return
		}
		if n.Kind == graph.KindFile {
			sub := pickDirectReExportCandidate(env, n.ID, orig, depth+1, state)
			if sub.cycleHit {
				phase1Cycle = true
			}
			if sub.depthTruncated {
				phase1Truncated = true
			}
			if sub.ambiguous {
				mergeBucket(&phase1, nil, "", true)
			} else if sub.pick != nil {
				mergeBucket(&phase1, sub.pick, innerSpec(sub.trackedNpmSpec, hopNpmSpec), false)
			}
			return
		}
		if graph.IsReExportNode(n) {
			sub := pickDirectReExportCandidate(env, n.FilePath, n.Name, depth+1, state)
			if sub.cycleHit {
				phase1Cycle = true
			}
			if sub.depthTruncated {
				phase1Truncated = true
			}
			if sub.ambiguous {
				mergeBucket(&phase1, nil, "", true)
			} else if sub.pick != nil {
				mergeBucket(&phase1, sub.pick, innerSpec(sub.trackedNpmSpec, hopNpmSpec), false)
			}
			return
		}
		if n.Meta["js_cjs_reexport_binding"] == true {
			sub := pickDirectReExportCandidate(env, n.FilePath, orig, depth+1, state)
			if sub.cycleHit {
				phase1Cycle = true
			}
			if sub.depthTruncated {
				phase1Truncated = true
			}
			if sub.ambiguous {
				mergeBucket(&phase1, nil, "", true)
			} else if sub.pick != nil {
				mergeBucket(&phase1, sub.pick, innerSpec(sub.trackedNpmSpec, hopNpmSpec), false)
			} else if !sub.cycleHit && !sub.depthTruncated && isImportableCallee(n, n.Name) && env.eligible(n) {
				mergeBucket(&phase1, n, hopNpmSpec, false)
			}
			return
		}
		if isImportableCallee(n, n.Name) && env.eligible(n) {
			mergeBucket(&phase1, n, hopNpmSpec, false)
		}
	}

	if hasFwd {
		for _, e := range env.outEdges(fwd.ID) {
			if e == nil || e.Kind != graph.EdgeReExports || isNamespaceReExport(e) {
				continue
			}
			orig := reExportOriginal(e)
			if orig == "" {
				orig, _ = fwd.Meta["reexport_original"].(string)
			}
			if orig == "" {
				orig = exportedName
			}
			target, hopNpmSpec := env.resolveReExport(e)
			handlePhase1Target(target, hopNpmSpec, orig)
		}
	}

	for _, e := range fileOut {
		if e == nil || e.Kind != graph.EdgeReExports || isNamespaceReExport(e) {
			continue
		}
		if bindings := reExportBindings(e); len(bindings) > 0 {
			if orig, ok := bindings[exportedName].(string); ok && orig != "" {
				target, hopNpmSpec := env.resolveReExport(e)
				handlePhase1Target(target, hopNpmSpec, orig)
			}
			continue
		}
		orig := reExportOriginal(e)
		if orig == "" {
			continue
		}
		pub := reExportAlias(e)
		if pub == "" {
			pub = orig
		}
		if pub != exportedName {
			continue
		}
		target, hopNpmSpec := env.resolveReExport(e)
		handlePhase1Target(target, hopNpmSpec, orig)
	}

	if phase1.ambiguous {
		return finish(reExportResult{
			tier:           tierExact,
			ambiguous:      true,
			cycleHit:       phase1Cycle,
			depthTruncated: phase1Truncated,
		})
	}
	if phase1.pick != nil {
		return finish(reExportResult{
			pick:           phase1.pick,
			trackedNpmSpec: phase1.npmSpec,
			tier:           tierExact,
			cycleHit:       phase1Cycle,
			depthTruncated: phase1Truncated,
		})
	}

	// Phase 2: Wildcard EdgeReExports on file.
	var exactB, fallbackB, bindingB bucket
	anyCycle := phase1Cycle
	anyTruncated := phase1Truncated
	for _, e := range fileOut {
		if e == nil || e.Kind != graph.EdgeReExports || isNamespaceReExport(e) {
			continue
		}
		isWildcard := false
		if bindings := reExportBindings(e); len(bindings) > 0 {
			isWildcard = isReExportWildcardFlag(e)
		} else {
			isWildcard = reExportOriginal(e) == "" && reExportAlias(e) == ""
		}
		if !isWildcard {
			continue
		}
		target, hopNpmSpec := env.resolveReExport(e)
		targetFile := nodeFilePathWith(env.getNode, target)
		if targetFile == "" {
			continue
		}
		sub := pickDirectReExportCandidate(env, targetFile, exportedName, depth+1, state)
		if sub.cycleHit {
			anyCycle = true
		}
		if sub.depthTruncated {
			anyTruncated = true
		}
		spec := innerSpec(sub.trackedNpmSpec, hopNpmSpec)
		switch {
		case sub.ambiguous:
			switch sub.tier {
			case tierFallback:
				mergeBucket(&fallbackB, nil, "", true)
			case tierBinding:
				mergeBucket(&bindingB, nil, "", true)
			default:
				mergeBucket(&exactB, nil, "", true)
			}
		case sub.pick != nil:
			switch sub.tier {
			case tierExact:
				mergeBucket(&exactB, sub.pick, spec, false)
			case tierFallback:
				mergeBucket(&fallbackB, sub.pick, spec, false)
			case tierBinding:
				mergeBucket(&bindingB, sub.pick, spec, false)
			}
		}
	}

	if exactB.ambiguous {
		return finish(reExportResult{
			tier:           tierExact,
			ambiguous:      true,
			cycleHit:       anyCycle,
			depthTruncated: anyTruncated,
		})
	}
	if exactB.pick != nil {
		return finish(reExportResult{
			pick:           exactB.pick,
			trackedNpmSpec: exactB.npmSpec,
			tier:           tierExact,
			cycleHit:       anyCycle,
			depthTruncated: anyTruncated,
		})
	}

	// Unmarked leaf fallback (both depth == 0 when hasReExports and depth > 0).
	if n := env.getNode(file + "::" + exportedName); n != nil &&
		isImportableCallee(n, exportedName) &&
		!graph.IsReExportNode(n) &&
		n.Meta["js_named_export"] != true &&
		env.eligible(n) {
		if n.Meta["js_cjs_reexport_binding"] == true {
			if !anyCycle && !anyTruncated {
				mergeBucket(&bindingB, n, "", false)
			}
		} else {
			mergeBucket(&fallbackB, n, "", false)
		}
	}

	if fallbackB.ambiguous {
		return finish(reExportResult{
			tier:           tierFallback,
			ambiguous:      true,
			cycleHit:       anyCycle,
			depthTruncated: anyTruncated,
		})
	}
	if fallbackB.pick != nil {
		return finish(reExportResult{
			pick:           fallbackB.pick,
			trackedNpmSpec: fallbackB.npmSpec,
			tier:           tierFallback,
			cycleHit:       anyCycle,
			depthTruncated: anyTruncated,
		})
	}
	if bindingB.ambiguous {
		return finish(reExportResult{
			tier:           tierBinding,
			ambiguous:      true,
			cycleHit:       anyCycle,
			depthTruncated: anyTruncated,
		})
	}
	if bindingB.pick != nil {
		return finish(reExportResult{
			pick:           bindingB.pick,
			trackedNpmSpec: bindingB.npmSpec,
			tier:           tierBinding,
			cycleHit:       anyCycle,
			depthTruncated: anyTruncated,
		})
	}
	return finish(reExportResult{
		cycleHit:       anyCycle,
		depthTruncated: anyTruncated,
	})
}

func requireClosureFiles(env jsRequireEnv, targetFile, lookupName string) (map[string]struct{}, map[string]string) {
	files := map[string]struct{}{targetFile: {}}
	npmSpecByFile := make(map[string]string)
	pending := []string{targetFile}
	for len(pending) > 0 {
		f := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		parentSpec := npmSpecByFile[f]
		for _, e := range env.outEdges(f) {
			if e == nil || e.Kind != graph.EdgeReExports || isNamespaceReExport(e) {
				continue
			}
			if bindings := reExportBindings(e); len(bindings) > 0 {
				_, hasNamed := bindings[lookupName].(string)
				if !hasNamed && !isReExportWildcardFlag(e) {
					continue
				}
			} else if orig := reExportOriginal(e); orig != "" {
				pub := reExportAlias(e)
				if pub == "" {
					pub = orig
				}
				if pub != lookupName {
					continue
				}
			}
			target, hopNpmSpec := env.resolveReExport(e)
			nextFile := nodeFilePathWith(env.getNode, target)
			if nextFile == "" {
				continue
			}
			nextSpec := hopNpmSpec
			if nextSpec == "" {
				nextSpec = parentSpec
			}
			if _, seen := files[nextFile]; !seen {
				files[nextFile] = struct{}{}
				if nextSpec != "" {
					npmSpecByFile[nextFile] = nextSpec
				}
				pending = append(pending, nextFile)
			} else if nextSpec != "" {
				if cur := npmSpecByFile[nextFile]; cur == "" || nextSpec < cur {
					npmSpecByFile[nextFile] = nextSpec
				}
			}
		}
	}
	return files, npmSpecByFile
}

// pickRequireClosureCandidate prefers a unique directly named export across
// the required barrel closure, falling back to any unique importable symbol
// (preferring canonical symbols over 2-step CJS re-export bindings) only when
// the closure has no directly named export (e.g. pure CJS modules).
func pickRequireClosureCandidate(files map[string]struct{}, funcName string, candidates []*graph.Node) *graph.Node {
	if len(files) == 0 {
		return nil
	}
	var exported *graph.Node
	for _, c := range candidates {
		if c.Meta["js_named_export"] != true ||
			graph.IsReExportNode(c) || c.Meta["js_cjs_reexport_binding"] == true ||
			!isImportableCallee(c, funcName) {
			continue
		}
		if _, ok := files[c.FilePath]; !ok {
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
	return pickUniqueFileCandidate(files, funcName, candidates)
}

type jsRequireTarget struct {
	target         string
	trackedNpmSpec string
}

func (cr *CrossRepoResolver) requireGetNode(id string) *graph.Node {
	if id == "" {
		return nil
	}
	if cr.nodeByID != nil {
		if n, ok := cr.nodeByID[id]; ok && n != nil {
			return n
		}
	}
	cr.requireCacheMu.RLock()
	n, ok := cr.requireNodeByID[id]
	cr.requireCacheMu.RUnlock()
	if ok {
		return n
	}
	loaded := cr.graph.GetNode(id)
	cr.requireCacheMu.Lock()
	if cr.requireNodeByID == nil {
		cr.requireNodeByID = make(map[string]*graph.Node)
	}
	if existing, exists := cr.requireNodeByID[id]; exists {
		loaded = existing
	} else {
		cr.requireNodeByID[id] = loaded
	}
	cr.requireCacheMu.Unlock()
	return loaded
}

func (cr *CrossRepoResolver) jsRequireEnvWith(callerRepo, callerWS string, getNode func(string) *graph.Node) jsRequireEnv {
	return cr.jsRequireEnvMemo(callerRepo, callerWS, nil, getNode)
}

func (cr *CrossRepoResolver) jsRequireEnvMemo(callerRepo, callerWS string, memo *npmImportMemo, getNode func(string) *graph.Node) jsRequireEnv {
	return jsRequireEnv{
		getNode:  getNode,
		outEdges: cr.cachedOutEdges,
		resolveReExport: func(re *graph.Edge) (string, string) {
			return cr.importedTargetOfMemo(re, memo, getNode)
		},
		eligible: func(n *graph.Node) bool {
			return n != nil && (!isCrossRepoHop(callerRepo, n.RepoPrefix) || cr.crossWorkspaceEligible(callerWS, candidateWorkspaceID(n), ""))
		},
		fetchCandidates: func(name string) []*graph.Node {
			raw := withoutReExportForwarders(cr.cachedFindNodesByName(name))
			out := make([]*graph.Node, 0, len(raw))
			for _, c := range raw {
				if isCrossRepoHop(callerRepo, c.RepoPrefix) && !cr.crossWorkspaceEligible(callerWS, candidateWorkspaceID(c), "") {
					continue
				}
				out = append(out, c)
			}
			return out
		},
	}
}

func (cr *CrossRepoResolver) resolveJSRequireCall(e *graph.Edge, funcName string, stats *CrossRepoStats) {
	spec, _ := e.Meta[graph.JSLexicalRequireSpecMeta].(string)
	callerFile := e.FilePath
	if spec == "" || callerFile == "" || !isJSTSPath(callerFile) {
		stats.Unresolved++
		return
	}
	lookupName, _ := e.Meta[graph.JSLexicalRequireSymbolMeta].(string)
	if lookupName == "" {
		lookupName = funcName
	}
	if lookupName == "" {
		stats.Unresolved++
		return
	}
	callerRepo := cr.callerRepoPrefix(e)
	callerWS := cr.callerWorkspaceID(e)
	resolved := cr.resolveJSRequireTargetWith(e, callerFile, callerRepo, callerWS, spec, cr.requireGetNode)
	targetFile := nodeFilePathWith(cr.requireGetNode, resolved.target)
	if targetFile == "" {
		stats.Unresolved++
		return
	}
	env := cr.jsRequireEnvWith(callerRepo, callerWS, cr.requireGetNode)
	pick, npmSpec := resolveJSRequireFromTargetFile(env, targetFile, lookupName, resolved.trackedNpmSpec)
	if pick == nil {
		stats.Unresolved++
		return
	}
	e.To = pick.ID
	e.Origin = graph.OriginASTResolved
	e.Confidence = 0.9
	if e.Meta == nil {
		e.Meta = map[string]any{}
	} else {
		e.Meta = maps.Clone(e.Meta)
	}
	clearEdgeTerminal(e)
	e.Meta["resolution"] = "import_closure"
	if npmSpec != "" {
		e.Meta[NpmImportSpecifierMeta] = npmSpec
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

func (cr *CrossRepoResolver) resolveJSRequireTargetWith(e *graph.Edge, callerFile, callerRepo, callerWS, spec string, getNode func(string) *graph.Node) jsRequireTarget {
	return cr.resolveJSRequireTargetMemo(e, callerFile, callerRepo, callerWS, spec, nil, getNode)
}

func (cr *CrossRepoResolver) resolveJSRequireTargetMemo(e *graph.Edge, callerFile, callerRepo, callerWS, spec string, memo *npmImportMemo, getNode func(string) *graph.Node) jsRequireTarget {
	cacheKey := "req\x00" + callerFile + "\x00" + spec
	cr.requireCacheMu.RLock()
	cached, ok := cr.requireTargetCache[cacheKey]
	cr.requireCacheMu.RUnlock()
	if ok {
		return cached
	}
	target := cr.resolveJSRequireTargetUncached(e, callerFile, callerRepo, callerWS, spec, memo, getNode)
	cr.requireCacheMu.Lock()
	if cr.requireTargetCache == nil {
		cr.requireTargetCache = make(map[string]jsRequireTarget)
	}
	cr.requireTargetCache[cacheKey] = target
	cr.requireCacheMu.Unlock()
	return target
}

func (cr *CrossRepoResolver) resolveJSRequireTargetUncached(e *graph.Edge, callerFile, callerRepo, callerWS, spec string, memo *npmImportMemo, getNode func(string) *graph.Node) jsRequireTarget {
	if to := resolveJSTSImportTarget(getNode, cr.pathAlias, callerFile, spec); to != "" {
		return jsRequireTarget{target: to}
	}
	rewritten, npmAliased := rewriteNpmAliasImport(cr.npmAlias, callerFile, spec)
	if npmAliased {
		if to := resolveJSTSImportTarget(getNode, cr.pathAlias, callerFile, rewritten); to != "" {
			return jsRequireTarget{target: to}
		}
	}
	triedTrackedNpm := false
	if cr.npmPackages != nil && cr.eligibleTrackedNpmImport(e, spec) {
		triedTrackedNpm = true
		if node := cr.trackedNpmImportWith(callerRepo, callerWS, spec, memo, getNode); node != nil {
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
		if to, _ := cr.importedTargetOfMemo(imp, memo, getNode); to != "" {
			trackedSpec := ""
			if cr.eligibleTrackedNpmImport(e, mod) {
				trackedSpec = mod
			}
			return jsRequireTarget{target: to, trackedNpmSpec: trackedSpec}
		}
	}
	if triedTrackedNpm {
		return jsRequireTarget{}
	}
	return cr.resolveJSBareOrTrackedNpmTargetMemo(e, callerFile, callerRepo, callerWS, spec, memo, getNode)
}

func (cr *CrossRepoResolver) resolveJSBareOrTrackedNpmTargetMemo(e *graph.Edge, callerFile, callerRepo, callerWS, mod string, memo *npmImportMemo, getNode func(string) *graph.Node) jsRequireTarget {
	cacheKey := "bare\x00" + callerFile + "\x00" + mod
	cr.requireCacheMu.RLock()
	cached, ok := cr.requireTargetCache[cacheKey]
	cr.requireCacheMu.RUnlock()
	if ok {
		return cached
	}
	result := cr.resolveJSBareOrTrackedNpmTargetUncached(e, callerFile, callerRepo, callerWS, mod, memo, getNode)
	cr.requireCacheMu.Lock()
	if cr.requireTargetCache == nil {
		cr.requireTargetCache = make(map[string]jsRequireTarget)
	}
	cr.requireTargetCache[cacheKey] = result
	cr.requireCacheMu.Unlock()
	return result
}

func (cr *CrossRepoResolver) resolveJSBareOrTrackedNpmTargetUncached(e *graph.Edge, callerFile, callerRepo, callerWS, mod string, memo *npmImportMemo, getNode func(string) *graph.Node) jsRequireTarget {
	if cr.npmPackages != nil && cr.eligibleTrackedNpmImport(e, mod) {
		if node := cr.trackedNpmImportWith(callerRepo, callerWS, mod, memo, getNode); node != nil {
			return jsRequireTarget{target: node.ID, trackedNpmSpec: mod}
		}
	}
	rewritten, _ := rewriteNpmAliasImport(cr.npmAlias, callerFile, mod)
	// Node always loads a core module for a bare specifier, never a
	// same-named local directory.
	if !isJSTSBareSpecifier(callerFile, rewritten) || isNodeCoreModule(rewritten) ||
		declaresExternalNpmDep(cr.npmDep, callerFile, mod) ||
		declaresExternalNpmDep(cr.npmDep, callerFile, rewritten) {
		return jsRequireTarget{}
	}
	if cr.dirIndex == nil {
		cr.buildDirIndexes()
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

func (cr *CrossRepoResolver) importedTargetOfMemo(e *graph.Edge, memo *npmImportMemo, getNode func(string) *graph.Node) (string, string) {
	to := e.To
	if to == "" {
		return "", ""
	}
	if graph.IsUnresolvedTarget(to) {
		payload, ok := strings.CutPrefix(graph.UnresolvedName(to), "import::")
		if !ok || payload == "" {
			return "", ""
		}
		callerFile := jsTSImportCallerFile(e)
		target := resolveJSTSImportTarget(getNode, cr.pathAlias, callerFile, payload)
		if target == "" {
			if rewritten, aliased := rewriteNpmAliasImport(cr.npmAlias, callerFile, payload); aliased {
				target = resolveJSTSImportTarget(getNode, cr.pathAlias, callerFile, rewritten)
			}
		}
		if target != "" {
			return target, ""
		}
		if e.Kind == graph.EdgeReExports {
			mod, _ := splitImportSpecSymbol(payload)
			from := getNode(callerFile)
			callerRepo := graph.RepoPrefixOfID(callerFile)
			if from != nil && from.RepoPrefix != "" {
				callerRepo = from.RepoPrefix
			}
			callerWS := candidateWorkspaceID(from)
			if callerWS == "" {
				callerWS = callerRepo
			}
			res := cr.resolveJSBareOrTrackedNpmTargetMemo(e, callerFile, callerRepo, callerWS, mod, memo, getNode)
			return res.target, res.trackedNpmSpec
		}
		return "", ""
	}
	if strings.HasPrefix(to, "external::") {
		if e.Kind == graph.EdgeReExports && cr.npmPackages != nil {
			mod, _ := splitImportSpecSymbol(strings.TrimPrefix(to, "external::"))
			if mod != "" && cr.eligibleTrackedNpmImport(e, mod) {
				callerFile := jsTSImportCallerFile(e)
				from := getNode(callerFile)
				callerRepo := graph.RepoPrefixOfID(callerFile)
				if from != nil && from.RepoPrefix != "" {
					callerRepo = from.RepoPrefix
				}
				callerWS := candidateWorkspaceID(from)
				if callerWS == "" {
					callerWS = callerRepo
				}
				if node := cr.trackedNpmImportWith(callerRepo, callerWS, mod, memo, getNode); node != nil {
					return node.ID, mod
				}
			}
		}
		return "", ""
	}
	if strings.HasPrefix(to, "dep::") || graph.IsStdlibStub(to) {
		return "", ""
	}
	if e.Kind == graph.EdgeReExports && e.Meta != nil {
		if marked, _ := e.Meta[NpmImportSpecifierMeta].(string); marked != "" {
			mod, _ := splitImportSpecSymbol(marked)
			return to, mod
		}
	}
	return to, ""
}

func (cr *CrossRepoResolver) importClosureFilesMemo(target string, memo *npmImportMemo, getNode func(string) *graph.Node) map[string]struct{} {
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
	addTarget(nodeFilePathWith(getNode, target))
	for len(pendingBarrels) > 0 {
		f := pendingBarrels[len(pendingBarrels)-1]
		pendingBarrels = pendingBarrels[:len(pendingBarrels)-1]
		for _, e := range cr.cachedOutEdges(f) {
			if e == nil || e.Kind != graph.EdgeReExports || isNamespaceReExport(e) {
				continue
			}
			to, _ := cr.importedTargetOfMemo(e, memo, getNode)
			addTarget(nodeFilePathWith(getNode, to))
		}
	}
	return files
}

// pickUniqueFileCandidate returns the only importable candidate defined in
// files, preferring canonical symbols over 2-step CJS re-export bindings, or
// nil when none or several are.
func pickUniqueFileCandidate(files map[string]struct{}, funcName string, candidates []*graph.Node) *graph.Node {
	if len(files) == 0 {
		return nil
	}
	var canonical, binding *graph.Node
	canonicalAmbig, bindingAmbig := false, false
	for _, c := range candidates {
		if !isImportableCallee(c, funcName) {
			continue
		}
		if _, ok := files[c.FilePath]; !ok {
			continue
		}
		if c.Meta["js_cjs_reexport_binding"] == true {
			if binding != nil && binding.ID != c.ID {
				bindingAmbig = true
			} else {
				binding = c
			}
			continue
		}
		if canonical != nil && canonical.ID != c.ID {
			// Two imported files (or one file with two same-named
			// top-level symbols) both define the name — the import
			// statement alone cannot arbitrate. Refuse, exactly like
			// the no-import case.
			canonicalAmbig = true
		} else {
			canonical = c
		}
	}
	if canonicalAmbig {
		return nil
	}
	if canonical != nil {
		return canonical
	}
	if bindingAmbig {
		return nil
	}
	return binding
}

func (r *Resolver) pickImportEvidenceCallee(callerFile, funcName string, candidates []*graph.Node) (*graph.Node, string) {
	if callerFile == "" || len(candidates) == 0 || !isJSTSPath(callerFile) {
		return nil, ""
	}
	// A candidate defined in the caller's own file is (or may be) module-
	// local — the import pick must stand down and let the locality/scope
	// cascade bind it. Any kind blocks: a local class, const, or nested
	// helper of the same name all shadow an import at some scope, and
	// falling through only preserves today's behaviour. Two exceptions: the
	// caller's own import-statement node, whose display name is the imported
	// module's basename (`import { useX } from './useX'`), is the import
	// evidence itself; and a parameter or local of some other function says
	// nothing about this call, whose own scope handleJSLexicalBinding checked.
	for _, c := range candidates {
		if c.FilePath != callerFile {
			continue
		}
		switch c.Kind {
		case graph.KindImport, graph.KindParam, graph.KindLocal:
		default:
			return nil, ""
		}
	}
	imports := r.importEvidenceFor(callerFile)
	var symbolPick *graph.Node
	for _, c := range candidates {
		if !imports.direct[c.ID] || c.Meta["js_named_export"] != true ||
			graph.IsReExportNode(c) || c.Meta["js_cjs_reexport_binding"] == true ||
			!isImportableCallee(c, funcName) {
			continue
		}
		if symbolPick != nil && symbolPick.ID != c.ID {
			return nil, ""
		}
		symbolPick = c
	}
	if symbolPick != nil {
		return symbolPick, "import_binding"
	}
	imports.closureOnce.Do(func() { imports.files = r.importClosureFiles(callerFile, imports.direct) })
	pick := pickUniqueFileCandidate(imports.files, funcName, candidates)
	if pick == nil {
		return nil, ""
	}
	return pick, "import_closure"
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
	switch c.Kind {
	case graph.KindFunction, graph.KindVariable, graph.KindConstant:
		return c.FilePath != "" && c.ID == c.FilePath+"::"+funcName
	}
	return false
}

// direct maps expanded target IDs to whether any import of that target was
// unrenamed. Module IDs also seed the file tier but cannot match a callee ID.
// Walk barrels only if a call actually needs the weaker file-level evidence.
type callerImportEvidence struct {
	directOnce  sync.Once
	direct      map[string]bool
	closureOnce sync.Once
	files       map[string]struct{}
}

func (r *Resolver) clearImportEvidence() {
	r.importFilesMu.Lock()
	r.importsByCaller = nil
	r.importOutByFile = nil
	r.requireNodeByID = nil
	r.importFilesMu.Unlock()
}

func (r *Resolver) requireGetNode(id string) *graph.Node {
	if id == "" {
		return nil
	}
	if r.nodeByID != nil {
		if n, ok := r.nodeByID[id]; ok && n != nil {
			return n
		}
	}
	if _, missing := r.missingNodeByID[id]; missing {
		return nil
	}
	r.importFilesMu.RLock()
	n, ok := r.requireNodeByID[id]
	r.importFilesMu.RUnlock()
	if ok {
		return n
	}
	loaded := r.graph.GetNode(id)
	r.importFilesMu.Lock()
	if r.requireNodeByID == nil {
		r.requireNodeByID = make(map[string]*graph.Node)
	}
	if existing, exists := r.requireNodeByID[id]; exists {
		loaded = existing
	} else {
		r.requireNodeByID[id] = loaded
	}
	r.importFilesMu.Unlock()
	return loaded
}

func (r *Resolver) importEvidenceFor(callerFile string) *callerImportEvidence {
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
				imports.direct = make(map[string]bool)
			}
			// SQLite retains import_alias even when Alias is not projected.
			alias, _ := e.Meta["import_alias"].(string)
			imports.direct[target] = imports.direct[target] || (e.Alias == "" && alias == "")
		}
	})
	return imports
}

func (r *Resolver) importClosureFiles(excludeFile string, direct map[string]bool) map[string]struct{} {
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
			if e == nil || e.Kind != graph.EdgeReExports || isNamespaceReExport(e) {
				continue
			}
			addTarget(r.nodeFilePath(r.importedTargetOf(e)))
		}
	}

	return files
}

func (r *Resolver) resolveJSTSSpecTarget(e *graph.Edge, payload string) string {
	// Same expansion, same precedence as resolveImport: tsconfig
	// paths / relative join first, npm-alias rewrite second.
	callerFile := jsTSImportCallerFile(e)
	target := resolveJSTSImportTarget(r.requireGetNode, r.pathAlias, callerFile, payload)
	if target == "" {
		if rewritten, aliased := rewriteNpmAliasImport(r.npmAlias, callerFile, payload); aliased {
			target = resolveJSTSImportTarget(r.requireGetNode, r.pathAlias, callerFile, rewritten)
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
		if target := r.resolveJSTSSpecTarget(e, payload); target != "" {
			return target
		}
		if e.Kind == graph.EdgeReExports {
			callerFile := jsTSImportCallerFile(e)
			mod, _ := splitImportSpecSymbol(payload)
			rewritten, _ := rewriteNpmAliasImport(r.npmAlias, callerFile, mod)
			if isJSTSBareSpecifier(callerFile, rewritten) && !isNodeCoreModule(rewritten) &&
				!declaresExternalNpmDep(r.npmDep, callerFile, mod) &&
				!declaresExternalNpmDep(r.npmDep, callerFile, rewritten) {
				return r.resolveJSBareDirTarget(e, callerFile, rewritten)
			}
		}
		return ""
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
	return nodeFilePathWith(r.requireGetNode, id)
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
	// A re-export hop can reach a file outside the prefetched frontier. Read
	// it without the lock so a slow store read never blocks other callers;
	// two racing loads read the same edges and the first insert wins.
	loaded := r.graph.GetOutEdges(callerFile)
	r.importFilesMu.Lock()
	defer r.importFilesMu.Unlock()
	if edges, ok = r.importOutByFile[callerFile]; ok {
		return edges
	}
	if r.importOutByFile == nil {
		r.importOutByFile = make(map[string][]*graph.Edge)
	}
	r.importOutByFile[callerFile] = loaded
	return loaded
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
