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

func (r *Resolver) resolveJSRequireCallee(e *graph.Edge, funcName string, candidates []*graph.Node) *graph.Node {
	spec, _ := e.Meta[graph.JSLexicalRequireSpecMeta].(string)
	callerFile := e.FilePath
	if spec == "" || callerFile == "" || !isJSTSPath(callerFile) || len(candidates) == 0 {
		return nil
	}
	target := r.resolveJSTSSpecTarget(e, spec)
	if target == "" {
		return nil
	}
	files := r.importClosureFiles(callerFile, map[string]struct{}{target: {}})
	return pickUniqueFileCandidate(files, funcName, candidates)
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

func (r *Resolver) importClosureFiles(callerFile string, direct map[string]struct{}) map[string]struct{} {
	if len(direct) == 0 {
		return nil
	}
	files := make(map[string]struct{})
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
