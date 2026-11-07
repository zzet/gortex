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
// pickImportEvidenceCallee closes both holes with structural evidence the
// cascade never consulted: the caller file's import closure. When the
// caller imports exactly one candidate's file — directly, or transitively
// through re-export (barrel) hops — that import statement is AST-grade
// proof of which module the name comes from, so the pick is stamped
// OriginASTResolved (the same tier resolveRendersChild's import-binding
// path uses). When the caller imports none of the candidates' files, or
// several, no evidence exists and the cascade proceeds unchanged.
//
// Precedence (documented here because it is the load-bearing design):
//
//  1. preferScopeCandidate — per-language static scope rules stay first;
//     they carry stronger, language-specific evidence.
//  2. Module-local definition — a same-file function/method candidate is
//     bound directly by the file-local tier in resolveFunctionCall, and
//     any same-file candidate (any kind) blocks the import pick. A
//     top-level local definition cannot legally coexist with an imported
//     binding of the same name (redeclaration), and a function-scoped
//     shadow may be the true callee; in both cases the local symbol wins
//     or the cascade safely refuses. Blocking only ever preserves the
//     pre-import-evidence behaviour — it can never mint a new edge.
//  3. Import evidence (this pass) — a unique imported candidate wins,
//     BEFORE the same-directory loop, so an explicit import beats a
//     same-dir-different-file shadow.
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

// pickImportEvidenceCallee returns the single candidate whose defining
// file the caller file imports (directly or through re-export hops), or
// nil when the evidence is absent or ambiguous. JS/TS callers only. A
// single candidate goes through the same check: a cross-directory call
// whose caller imports the candidate's file is structural evidence, and
// without this pick it fell into the name-only fallback, got tagged
// text_matched and find_usages hid it behind the import line.
func (r *Resolver) pickImportEvidenceCallee(callerFile, funcName string, candidates []*graph.Node) *graph.Node {
	if callerFile == "" || len(candidates) == 0 || !isJSTSPath(callerFile) {
		return nil
	}
	// A candidate defined in the caller's own file is (or may be) module-
	// local — the import pick must stand down and let the locality/scope
	// cascade bind it. Any kind blocks: a local class, const, or nested
	// helper of the same name all shadow an import at some scope, and
	// falling through only preserves today's behaviour. The one exception
	// is the caller's own import-statement node: its display name is the
	// imported module's basename, so `import { useX } from './useX'` puts a
	// same-named KindImport node in the caller file. That node is the
	// import evidence itself, not a shadow.
	for _, c := range candidates {
		if c.FilePath == callerFile && c.Kind != graph.KindImport {
			return nil
		}
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
		for _, e := range r.graph.GetOutEdges(f) {
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
		return r.nodeFilePath(target)
	}
	if strings.HasPrefix(to, "external::") || strings.HasPrefix(to, "dep::") || graph.IsStdlibStub(to) {
		return ""
	}
	return r.nodeFilePath(to)
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
	return r.graph.GetOutEdges(callerFile)
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
