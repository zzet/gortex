package resolver

import (
	"strings"

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
// Precedence (documented here because it is the load-bearing design):
//
//  1. preferScopeCandidate — per-language static scope rules stay first;
//     they carry stronger, language-specific evidence.
//  2. A parameter of the caller or a module-local value shadows imports.
//  3. An explicit value import, then a generated Nuxt binding, then
//     unique imported-file evidence. A known but unresolved binding stops here.
//  4. The existing cascade — same-dir, first-same-repo, JS/TS top-level
//     value callee — untouched.
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

// pickImportEvidenceCallee is the file-closure fallback, not an explicit binding.
func (r *Resolver) pickImportEvidenceCallee(callerFile, funcName string, candidates []*graph.Node) *graph.Node {
	if callerFile == "" || len(candidates) == 0 || !isJSTSPath(callerFile) {
		return nil
	}
	if hasLocalValue(callerFile, candidates) {
		return nil
	}
	imported := r.importedFilesFor(callerFile)
	if len(imported) == 0 {
		return nil
	}
	var pick *graph.Node
	for _, c := range candidates {
		if !isImportableCallee(c, funcName) {
			continue
		}
		if _, ok := imported[c.FilePath]; !ok {
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

// The module edge retains local:export pairs even for statements too large
// to emit a symbol edge per specifier. Bare entries have identical names.
func (r *Resolver) tryResolveExplicitImport(e *graph.Edge, name string, stats *ResolveStats) bool {
	for _, imp := range r.callerOutEdges(e.FilePath) {
		if imp.Kind != graph.EdgeImports {
			continue
		}
		bindings, _ := imp.Meta[graph.ImportBindingsMeta].(string)
		if exported := graph.JSBinding(bindings, name); exported != "" {
			stem, file := r.jsBindingSource(imp)
			r.resolveExactCall(e, name, stem, file, exported, graph.JSPendingImport, "import_closure", stats)
			return true
		}
	}
	return false
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
		return stem, probeJSTSFile(r.cachedGetNode, stem)
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
	if target := r.exportedCallee(file, exported); target != nil {
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

// A named export (including a local alias or an imported value) wins over
// stars. Stars never forward default and two distinct implementations conflict.
func (r *Resolver) exportedCallee(file, name string) *graph.Node {
	type binding struct{ file, name string }
	pending := []binding{{file, name}}
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
		if local == "" && b.name == graph.JSDefaultBinding {
			local, _ = f.Meta[graph.DefaultExportMeta].(string)
		}
		if local != "" {
			if n := r.cachedGetNode(b.file + "::" + local); n != nil && !graph.IsReExportNode(n) && isImportableCallee(n, local) {
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
			if n := r.cachedGetNode(b.file + "::" + b.name); graph.IsReExportNode(n) {
				orig, _ := n.Meta["reexport_original"].(string)
				if orig == "" {
					orig = b.name
				}
				for _, edge := range r.callerOutEdges(n.ID) {
					if edge.Kind == graph.EdgeReExports {
						_, file := r.jsBindingSource(edge)
						pending = append(pending, binding{file, orig})
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
				pending = append(pending, binding{file, forwarded})
			}
		}
		if local != "" || explicit || b.name == graph.JSDefaultBinding {
			continue
		}
		for _, edge := range edges {
			if edge.Kind == graph.EdgeReExports && edge.Meta[graph.StarReExportMeta] == true {
				_, file := r.jsBindingSource(edge)
				pending = append(pending, binding{file, b.name})
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
	switch c.Kind {
	case graph.KindFunction, graph.KindVariable, graph.KindConstant:
		return c.FilePath != "" && c.ID == c.FilePath+"::"+funcName
	}
	return false
}

// importedFilesFor returns the set of file paths the caller file imports —
// directly, plus every file reachable from an imported barrel through
// transitive EdgeReExports hops. Memoised per caller file for the duration
// of a resolve pass (r.importFilesMu guards the map because the resolver's
// worker phase is parallel); cleared with the per-pass lookup caches.
//
// The set is built from the import edges' CURRENT state, whichever that
// is: a resolved edge contributes its target node's file, an unresolved
// one has its raw `import::` specifier expanded exactly the way
// resolveImport will expand it. Both states yield the same file, so a call
// edge resolved before its file's import edge sees the same closure as one
// resolved after.
func (r *Resolver) importedFilesFor(callerFile string) map[string]struct{} {
	r.importFilesMu.RLock()
	files, ok := r.importFilesByCaller[callerFile]
	r.importFilesMu.RUnlock()
	if ok {
		return files
	}

	files = make(map[string]struct{})
	seen := map[string]bool{callerFile: true}
	var pendingBarrels []string
	addTarget := func(f string) {
		if f == "" || f == callerFile {
			return
		}
		if _, dup := files[f]; dup {
			return
		}
		files[f] = struct{}{}
		pendingBarrels = append(pendingBarrels, f)
	}

	for _, e := range r.callerOutEdges(callerFile) {
		if e.Kind != graph.EdgeImports {
			continue
		}
		addTarget(r.importedFileOf(e))
	}
	// Barrel hops: an import that lands on a re-exporting module makes the
	// re-exported modules visible too (`import { createStore } from
	// 'zustand'` → src/index.ts → src/vanilla.ts). Same transitive walk
	// buildImportClosure performs for the guard, at file granularity.
	for len(pendingBarrels) > 0 {
		f := pendingBarrels[len(pendingBarrels)-1]
		pendingBarrels = pendingBarrels[:len(pendingBarrels)-1]
		if seen[f] {
			continue
		}
		seen[f] = true
		for _, e := range r.callerOutEdges(f) {
			if e.Kind != graph.EdgeReExports {
				continue
			}
			addTarget(r.importedFileOf(e))
		}
	}

	r.importFilesMu.Lock()
	if r.importFilesByCaller == nil {
		r.importFilesByCaller = make(map[string]map[string]struct{})
	}
	r.importFilesByCaller[callerFile] = files
	r.importFilesMu.Unlock()
	return files
}

// importedFileOf maps an EdgeImports / EdgeReExports edge to the file path
// of the module it names, or "" when the edge targets nothing indexable
// (third-party package, stdlib stub, dep contract, unexpandable specifier).
func (r *Resolver) importedFileOf(e *graph.Edge) string {
	return r.nodeFilePath(r.importedTargetOf(e))
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
		// Same expansion, same precedence as resolveImport: tsconfig
		// paths / relative join first, npm-alias rewrite second.
		target := resolveJSTSImportTarget(r.cachedGetNode, r.pathAlias, jsTSImportCallerFile(e), payload)
		if target == "" {
			if rewritten, aliased := rewriteNpmAliasImport(r.npmAlias, e.FilePath, payload); aliased {
				target = resolveJSTSImportTarget(r.cachedGetNode, r.pathAlias, jsTSImportCallerFile(e), rewritten)
			}
		}
		return target
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
// importedFilesFor walks for every bare call. known is an optional
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
