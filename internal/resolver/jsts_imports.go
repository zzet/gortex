package resolver

import (
	"strings"

	"github.com/zzet/gortex/internal/graph"
)

// PathAliasResolver expands a JS/TS path-alias import specifier — one
// declared in the importing file's nearest-ancestor tsconfig.json /
// jsconfig.json `compilerOptions.paths` (e.g. `@/lib/auth`) or resolved
// against `baseUrl` — to the repo-prefixed, extension-stripped file stem
// it targets (`src/lib/auth`). It returns "" when the specifier is not an
// alias / baseUrl import for that file (a relative path or a genuine
// third-party package).
//
// Like NpmAliasResolver it is defined in the resolver package so the
// resolver carries no compile-time dependency on the indexer or the
// filesystem: the indexer constructs a concrete implementation (which
// reads tsconfig from disk via the tsalias package) and injects it via
// SetPathAliasResolver.
type PathAliasResolver func(callerFile, specifier string) string

// SetPathAliasResolver installs a tsconfig path-alias expander. Pass nil
// to detach. Must be called before ResolveAll / ResolveFile — the
// resolver caches no alias state across passes.
func (r *Resolver) SetPathAliasResolver(fn PathAliasResolver) { r.pathAlias = fn }

// SetPathAliasResolver installs a tsconfig path-alias expander on the
// cross-repo resolver. Same contract as the Resolver method.
func (cr *CrossRepoResolver) SetPathAliasResolver(fn PathAliasResolver) { cr.pathAlias = fn }

// jsTSImportExts are the source extensions a resolved JS/TS module
// specifier may carry on disk, probed in order. The stem (no extension)
// is also probed as a directory whose entry point is `index.<ext>`.
var jsTSImportExts = []string{
	".ts", ".tsx", ".d.ts", ".js", ".jsx", ".mts", ".cts", ".mjs", ".cjs",
}

// jsTSEmittedSourceExts maps an EMITTED JavaScript extension a module
// specifier may carry to the TypeScript source extensions that compile to
// it, in TypeScript's own resolution order.
//
// Under `moduleResolution: node16` / `nodenext` — mandatory for ESM
// TypeScript, and what `"type": "module"` projects use — a specifier names
// the file the compiler EMITS, not the file on disk: `./alpha.ts` is
// imported as `./alpha.js`, `./alpha.mts` as `./alpha.mjs`. Probing such a
// stem only verbatim (and then as `alpha.js.ts`, `alpha.js/index.ts`)
// never matches the real source, so the import edge stays unresolved and
// every call routed through it is reverted by the cross-package guard
// (issue #304).
//
// Deliberate deviation from tsc: the verbatim `.js` is probed BEFORE these
// counterparts, whereas tsc prefers `.ts`/`.d.ts` over a co-located `.js`.
// A graph binds to implementations, so an indexed `alpha.js` that really
// exists is the better target than a sibling `alpha.d.ts` that carries
// types but no body. `.d.*` entries stay last for the same reason.
var jsTSEmittedSourceExts = map[string][]string{
	".js":  {".ts", ".tsx", ".d.ts"},
	".jsx": {".tsx", ".d.ts"},
	".mjs": {".mts", ".d.mts"},
	".cjs": {".cts", ".d.cts"},
}

// EmittedJSSourceExts returns the TypeScript source extensions that compile
// to the emitted-JavaScript extension ext (".js", ".jsx", ".mjs", ".cjs"),
// in probe order — nil for anything else. Exported so the query layer's
// re-export barrel walk expands `nodenext` specifiers exactly the way the
// resolver does, instead of keeping a second copy of the mapping.
func EmittedJSSourceExts(ext string) []string { return jsTSEmittedSourceExts[ext] }

// resolveJSTSImportTarget resolves a JS/TS EdgeImports / EdgeReExports
// specifier onto the in-repo file (or exported symbol) it names, or
// returns "" when the caller is not JS/TS, the specifier is a genuine
// third-party package, or no indexed file matches. importPath is the raw
// `import::` payload — the module specifier for a module-level edge, or
// `<specifier>::<export>` for a per-binding edge. getNode is the resolver's
// node lookup (cachedGetNode); pathAlias is the injected tsconfig expander
// (may be nil).
//
// Two specifier shapes resolve:
//
//   - Relative (`./auth`, `../lib/auth`) — joined against the importing
//     file's directory.
//   - tsconfig / jsconfig path alias or baseUrl import (`@/lib/auth`) —
//     expanded via the injected PathAliasResolver.
//
// The expanded stem is probed against the indexed file nodes
// (`stem.<ext>`, `stem/index.<ext>`); the first match wins. A module-level
// edge resolves to the file node; a per-binding edge resolves to that
// file's exported-symbol node when it exists, else the file node.
//
// Running this inside resolveImport (rather than as a post-pass) means the
// cold ResolveAll worker phase AND every incremental ResolveFile path land
// the import edge before buildImportClosure reads it — so a cross-directory
// JS/TS import contributes real reachability and the cross-package guard
// stops reverting its callers to `unresolved::*` (issue #136). Shared by
// Resolver.resolveImport and CrossRepoResolver.resolveImport.
func resolveJSTSImportTarget(getNode func(string) *graph.Node, pathAlias PathAliasResolver, callerFile, importPath string) string {
	if !isJSTSPath(callerFile) {
		return ""
	}
	spec, symbol := splitImportSpecSymbol(importPath)
	stem := expandJSTSSpecifier(pathAlias, callerFile, spec)
	if stem == "" {
		return ""
	}
	file := probeJSTSFile(getNode, stem)
	if file == "" {
		return ""
	}
	if symbol != "" {
		if symID := file + "::" + symbol; getNode(symID) != nil {
			return symID
		}
	}
	return file
}

// jsTSImportCallerFile returns the importing file's graph path for an
// import edge, preferring the explicit FilePath and falling back to the
// From end (a file node ID is the file path).
func jsTSImportCallerFile(e *graph.Edge) string {
	if e.FilePath != "" {
		return e.FilePath
	}
	return e.From
}

// expandJSTSSpecifier turns a JS/TS module specifier into a repo-prefixed,
// extension-stripped file stem, or "" when it is a genuine third-party
// package. Relative specifiers are joined against the importing file's
// directory; everything else is handed to the injected PathAliasResolver
// (tsconfig `paths` / `baseUrl`).
func expandJSTSSpecifier(pathAlias PathAliasResolver, callerFile, spec string) string {
	if spec == "" {
		return ""
	}
	if strings.HasPrefix(spec, "./") || strings.HasPrefix(spec, "../") {
		dir := ""
		if i := strings.LastIndex(callerFile, "/"); i >= 0 {
			dir = callerFile[:i]
		}
		return joinRelativePath(dir, spec)
	}
	if pathAlias != nil {
		return pathAlias(callerFile, spec)
	}
	return ""
}

// probeJSTSFile returns the indexed KindFile node ID an extension-stripped
// stem resolves to — trying an explicit author-written extension first,
// then each source extension, then a directory `index.<ext>` barrel — or
// "" when no indexed file matches.
func probeJSTSFile(getNode func(string) *graph.Node, stem string) string {
	if stem == "" {
		return ""
	}
	isFile := func(id string) bool {
		n := getNode(id)
		return n != nil && n.Kind == graph.KindFile
	}
	// An explicit source extension the author wrote (`./auth.js`) is tried
	// verbatim first, then — when it names an emitted JavaScript file —
	// against the TypeScript sources that compile to it.
	switch ext := jsTSExt(stem); ext {
	case ".vue", ".svelte", ".astro":
		// A single-file component is only ever imported by its full name
		// (`./Foo.vue`); there is no extension probing on top of it.
		if isFile(stem) {
			return stem
		}
		return ""
	case ".ts", ".tsx", ".js", ".jsx", ".mts", ".cts", ".mjs", ".cjs":
		if isFile(stem) {
			return stem
		}
		base := strings.TrimSuffix(stem, ext)
		for _, srcExt := range jsTSEmittedSourceExts[ext] {
			if id := base + srcExt; isFile(id) {
				return id
			}
		}
	}
	for _, ext := range jsTSImportExts {
		if id := stem + ext; isFile(id) {
			return id
		}
	}
	for _, ext := range jsTSImportExts {
		if id := stem + "/index" + ext; isFile(id) {
			return id
		}
	}
	return ""
}

// splitImportSpecSymbol splits an `import::` edge payload into the module
// specifier and the per-binding export name. The per-binding edge the JS
// extractor emits is `<specifier>::<exportName>`; a module-level edge is
// just `<specifier>`. A module specifier never contains `::`, so the final
// `::` (when present) is the separator.
//
//	"./auth"            → ("./auth", "")
//	"./auth::getAuth"   → ("./auth", "getAuth")
//	"@/lib/auth::Foo"   → ("@/lib/auth", "Foo")
func splitImportSpecSymbol(importPath string) (spec, symbol string) {
	if i := strings.LastIndex(importPath, "::"); i >= 0 {
		return importPath[:i], importPath[i+2:]
	}
	return importPath, ""
}

// jsTSExt returns the lowercase file extension of p, or "" when none.
func jsTSExt(p string) string {
	dot := strings.LastIndexByte(p, '.')
	if dot < 0 {
		return ""
	}
	if slash := strings.LastIndexByte(p, '/'); slash > dot {
		return ""
	}
	return strings.ToLower(p[dot:])
}

// isJSTSPath reports whether a file path is a JavaScript / TypeScript
// source file - the only importers whose specifiers this resolver expands.
//
// Single-file components (.vue / .svelte / .astro) count too: their <script>
// block is carved out and parsed by the JS/TS extractor, so the import edges
// it emits carry the host .vue path as FilePath. Rejecting them here left
// every relative import from a .vue script as an external stub, and calls
// into the imported .ts symbol lost reachability and got reverted.
func isJSTSPath(p string) bool {
	switch jsTSExt(p) {
	case ".ts", ".tsx", ".js", ".jsx", ".mts", ".cts", ".mjs", ".cjs",
		".vue", ".svelte", ".astro":
		return true
	}
	return false
}

// isJSTSBareSpecifier reports whether callerFile is a JS/TS source importing
// a BARE specifier — one Node resolves through `node_modules` / a workspace
// member (`graphql`, `@acme/utils`, `lodash/merge`, `node:fs`) rather than
// through the file system (`./auth`, `../lib/auth`, `/abs`). importPath is the
// raw `import::` payload, so the optional `::<export>` suffix is stripped
// first.
//
// Callers pair this with isJSTSDirEntryPoint to gate the package-directory
// cascade in resolveImport; see that function for why.
func isJSTSBareSpecifier(callerFile, importPath string) bool {
	if !isJSTSPath(callerFile) {
		return false
	}
	spec, _ := splitImportSpecSymbol(importPath)
	switch {
	case spec == "", spec == ".", spec == "..":
		return false
	case strings.HasPrefix(spec, "./"), strings.HasPrefix(spec, "../"), strings.HasPrefix(spec, "/"):
		return false
	}
	return true
}

// isJSTSDirEntryPoint reports whether filePath is the module entry point of
// its own directory — `index.<ext>` for any JS/TS module extension.
//
// This is the evidence bar the dirIndex/lastDirIndex cascade in resolveImport
// must clear before binding a JS/TS BARE specifier to an in-repo file. The
// cascade is package-directory-oriented — a Go/Python notion, documented on
// buildDirIndexes as "an import of `logger` matches any file under
// .../logger/" — and lastDirIndex keys on nothing but the last path component
// of each file's directory. For a bare specifier that is every indexed file
// under any same-named directory, and the same-repo branch of `consider`
// accepts the first one with no further check. So a repo that merely contains
// a `*/graphql/` directory made every `import … from 'graphql'` also bind to
// an arbitrary file inside it, accumulating phantom importers on that file
// (issue #450).
//
// Node and tsc load a directory-shaped module through its entry point
// (`package.json` main/exports, else `index.<ext>`) — never through an
// arbitrary file inside it. Requiring an `index.*` candidate is therefore not
// just a filter but the correct rule: it keeps the intended monorepo
// behaviour (a bare `logger` resolving to `packages/app/logger/index.ts`,
// pinned by the workspace-membership tests) while refusing the false binding,
// which by construction never names an entry point.
//
// Deliberately NOT solved by applying dirMatchesImport to the same-repo
// branch: that gate answers a different question (does this directory
// genuinely spell out the import path) and would regress that same monorepo
// behaviour, since dirMatchesImport("packages/app/logger", "logger") is false.
func isJSTSDirEntryPoint(filePath string) bool {
	base := filePath
	if i := strings.LastIndexByte(base, '/'); i >= 0 {
		base = base[i+1:]
	}
	base = strings.ToLower(base)
	for _, ext := range jsTSImportExts {
		if base == "index"+ext {
			return true
		}
	}
	return false
}
