package resolver

import (
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

// handleJSLexicalBinding handles a bare JS/TS call whose name is bound in an
// enclosing scope. A parameter always stays unresolved; a local with a
// same-file callable/value candidate continues to the generic cascade (and
// otherwise stays unresolved). A local initialized from an unshadowed static
// require() carries no binding and keeps the ordinary import tiers.
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
	}
	return false
}

// pickUniqueFileCandidate returns the only importable candidate defined in
// files, or nil when none or several are.
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
		if !imports.direct[c.ID] || c.Meta["js_named_export"] != true || !isImportableCallee(c, funcName) {
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
	r.importFilesMu.Unlock()
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
			if e.Kind != graph.EdgeReExports {
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
