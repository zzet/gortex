package resolver

import (
	"strings"

	"github.com/zzet/gortex/internal/graph"
)

// NpmAliasResolver rewrites a JS/TS import specifier when it resolves
// through an npm alias. npm lets a package.json dependency be declared
// as `"shared": "npm:@acme/shared-lib@1.4.0"`; an `import x from
// 'shared'` then actually refers to the package `@acme/shared-lib`.
// Without the rewrite the resolver treats the bare specifier as an
// external dependency and the cross-package edge to a locally-vendored
// `@acme/shared-lib` is lost.
//
// Given the importing file's repo-prefixed graph path and the verbatim
// import specifier, the implementation finds the nearest-ancestor
// package.json, checks its dependencies / devDependencies for an
// npm-alias entry keyed by the specifier's package portion, and
// returns the specifier with that portion swapped for the alias's real
// package name (a sub-path like `shared/util` keeps its `/util` tail).
// It returns "" when the specifier is not an npm alias — the caller
// then resolves the original specifier unchanged.
//
// The type is defined in the resolver package so the resolver has no
// compile-time dependency on the indexer or the filesystem — the
// indexer constructs a concrete implementation (which reads
// package.json from disk) and injects it via SetNpmAliasResolver.
type NpmAliasResolver func(callerFile, specifier string) string

// SetNpmAliasResolver installs an npm-alias import rewriter. Pass nil
// to detach. Must be called before ResolveAll / ResolveFile — the
// resolver caches no alias state across passes, so mid-pass swaps are
// racy with the parallel resolveEdge workers and are not supported.
func (r *Resolver) SetNpmAliasResolver(fn NpmAliasResolver) {
	r.npmAlias = fn
}

// SetNpmAliasResolver installs an npm-alias import rewriter on the
// cross-repo resolver. Same contract as the Resolver method.
func (cr *CrossRepoResolver) SetNpmAliasResolver(fn NpmAliasResolver) {
	cr.npmAlias = fn
}

// rewriteNpmAliasImport applies the installed NpmAliasResolver to an
// import specifier. It returns the (possibly rewritten) specifier and
// whether a rewrite happened. When no resolver is installed or the
// specifier is not an npm alias the specifier is returned unchanged
// with rewritten=false. Shared by Resolver.resolveImport and
// CrossRepoResolver.resolveImport so both resolution passes treat
// npm-aliased imports identically.
func rewriteNpmAliasImport(fn NpmAliasResolver, callerFile, importPath string) (string, bool) {
	if fn == nil || callerFile == "" || importPath == "" {
		return importPath, false
	}
	if isJSTSPath(callerFile) {
		spec, sym := splitImportSpecSymbol(importPath)
		if real := fn(callerFile, spec); real != "" && real != spec {
			if sym != "" {
				return real + "::" + sym, true
			}
			return real, true
		}
		return importPath, false
	}
	if real := fn(callerFile, importPath); real != "" && real != importPath {
		return real, true
	}
	return importPath, false
}

// npmPackagePrefix returns the package portion of an npm import
// specifier — the part addressing the package itself, with any
// in-package sub-path dropped. A scoped package keeps its
// `@scope/name`. Returns "" when the specifier carries no sub-path
// (the whole specifier is already the package) so callers can skip a
// redundant lookup:
//
//	"@acme/shared-lib/util" → "@acme/shared-lib"
//	"lodash/get"            → "lodash"
//	"@acme/shared-lib"      → ""   (no sub-path)
//	"lodash"                → ""   (no sub-path)
func npmPackagePrefix(specifier string) string {
	if strings.HasPrefix(specifier, "@") {
		// Scoped: package portion is the first two segments.
		first := strings.IndexByte(specifier, '/')
		if first < 0 {
			return ""
		}
		second := strings.IndexByte(specifier[first+1:], '/')
		if second < 0 {
			return "" // exactly `@scope/name`, no sub-path
		}
		return specifier[:first+1+second]
	}
	if i := strings.IndexByte(specifier, '/'); i >= 0 {
		return specifier[:i]
	}
	return ""
}

// --- JSX/template renders-child import-binding resolution -----------------

// resolveRendersChild binds an EdgeRendersChild (`<Button/>`) to the exact
// component the caller file imported under that name. Import bindings are
// ground truth — they pin the right component even when the name is ambiguous
// repo-wide — so this runs ahead of the name/dir-proximity cascade, which only
// takes over for a locally-defined component (no matching import).
func (r *Resolver) resolveRendersChild(e *graph.Edge, componentName string, stats *ResolveStats) {
	if importPath, exportName := r.importBindingTarget(e.FilePath, componentName); importPath != "" {
		if targetID := r.resolveImportedComponent(e.FilePath, importPath, exportName); targetID != "" {
			e.To = targetID
			e.Origin = graph.OriginASTResolved
			e.Confidence = 0.95
			if e.Meta == nil {
				e.Meta = map[string]any{}
			}
			e.Meta["resolution"] = "import_binding"
			return
		}
	}
	// Locally defined or unresolvable import: fall through to the existing
	// name/dir-proximity cascade. A component is a function or a class, so the
	// type-or-func resolver is the right fallback.
	r.resolveTypeOrFunc(e, componentName, stats)
}

// importBindingTarget returns the (module path, export name) for the component
// the caller file imported under the given LOCAL name (as used in the JSX), or
// ("","") when the name was not imported (so it is locally defined and the
// cascade should handle it). The export name differs from the local name for an
// aliased import (`import { Avatar as Ava }` — local Ava, export Avatar) and is
// what the component node is named by.
func (r *Resolver) importBindingTarget(filePath, localName string) (path, exportName string) {
	for _, e := range r.graph.GetOutEdges(filePath) {
		if e.Kind != graph.EdgeImports {
			continue
		}
		p, local, export := importBindingOf(e)
		if local == localName && p != "" {
			return p, export
		}
	}
	return "", ""
}

// importBindingOf returns the (module path, LOCAL binding name, EXPORT name) an
// import edge carries. A per-binding named edge is
// `unresolved::import::<path>::<orig>` (export = <orig>, local = Alias if
// renamed else <orig>); a module edge is `unresolved::import::<path>` whose
// export/local is the path basename — the default-import convention
// `import Button from './Button'`.
func importBindingOf(e *graph.Edge) (path, local, exportName string) {
	const pfx = "unresolved::import::"
	to := e.To
	if rest, ok := strings.CutPrefix(to, pfx); ok {
		if i := strings.LastIndex(rest, "::"); i >= 0 {
			path, exportName = rest[:i], rest[i+2:]
		} else {
			path, exportName = rest, importPathBasename(rest)
		}
	} else {
		path, exportName = to, importPathBasename(to)
	}
	local = exportName
	if e.Alias != "" {
		local = e.Alias // the name used at the JSX render site
	}
	return path, local, exportName
}

// importPathBasename returns the trailing path segment of a module specifier,
// extension stripped (`./components/Button.tsx` -> `Button`).
func importPathBasename(p string) string {
	p = strings.TrimSuffix(p, "/")
	if i := strings.LastIndexByte(p, '/'); i >= 0 {
		p = p[i+1:]
	}
	if i := strings.LastIndexByte(p, '.'); i > 0 {
		p = p[:i]
	}
	return p
}

// resolveImportedComponent resolves a relative import path to the component
// node named `name` it exports, trying the JS/TS file-resolution candidates
// (extension + index). Returns "" for a bare/aliased module (not a local file).
func (r *Resolver) resolveImportedComponent(filePath, importPath, name string) string {
	if !strings.HasPrefix(importPath, ".") {
		return "" // bare / npm / path-aliased module — not a local component file
	}
	dir := ""
	if i := strings.LastIndexByte(filePath, '/'); i >= 0 {
		dir = filePath[:i]
	}
	base := joinRelativePath(dir, importPath)
	if base == "" {
		return ""
	}
	exts := []string{".tsx", ".ts", ".jsx", ".js"}
	for _, ext := range exts {
		if id := base + ext + "::" + name; r.cachedGetNode(id) != nil {
			return id
		}
	}
	for _, ext := range exts {
		if id := base + "/index" + ext + "::" + name; r.cachedGetNode(id) != nil {
			return id
		}
	}
	return ""
}
