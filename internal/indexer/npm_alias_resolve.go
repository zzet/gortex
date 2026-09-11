package indexer

import (
	"encoding/json"
	"path"
	"strings"
	"sync"

	"github.com/zzet/gortex/internal/modules"
)

// npmAliasIndex implements resolver.NpmAliasResolver: it rewrites a
// JS/TS import specifier that resolves through an npm alias declared
// in the importing file's nearest-ancestor package.json.
//
// An npm alias declares a dependency under one name while pointing it
// at a different real package:
//
//	"dependencies": { "shared": "npm:@acme/shared-lib@1.4.0" }
//
// `import x from 'shared'` then refers to `@acme/shared-lib`, and
// `import x from 'shared/util'` to `@acme/shared-lib/util`. The
// resolver only knows the bare specifier, so without this rewrite a
// locally-vendored `@acme/shared-lib` is missed and the import falls
// through to an external stub.
//
// The index is read-only after construction apart from its parsed-
// manifest cache, which is guarded by mu because the resolver's
// resolveEdge workers run in parallel.
type npmAliasIndex struct {
	// roots maps a repo prefix to its on-disk root. Entries with an
	// empty prefix model single-repo mode (no prefix on graph paths).
	roots map[string]string
	// trees answers with the manifestTree a repo root's manifests must be
	// read out of, so a build indexing a committed snapshot reads that
	// snapshot's package.json rather than the checkout's. A nil provider —
	// or a nil answer — reads the working copy, which is what the live
	// index does.
	trees func(root string) manifestTree

	mu sync.Mutex
	// aliasCache memoises one parsed package.json: disk path → the
	// alias map (dependency key → real package name) for that file.
	// A nil value records "read, but no npm-alias entries / missing
	// file" so a miss is not re-read on every import edge.
	aliasCache map[string]map[string]string
	// depsCache memoises the full declared-dependency map of one
	// package.json: disk path → (package name → verbatim version spec).
	// A nil value records "read, but no dependencies / missing file".
	depsCache map[string]map[string]string
	// exportsCache memoises the parsed `exports` subpath map of one
	// package.json: disk path → packageExports. A nil value records
	// "read, but no usable `exports` field / missing file" so a miss
	// is not re-parsed on every import edge.
	exportsCache map[string]*packageExports
	// wsNames maps a monorepo workspace package's `name` (from its
	// package.json) to its graph-path directory, built lazily once from the
	// root manifests' `workspaces` globs. A non-nil empty map records "built,
	// no workspaces" so it is not re-scanned.
	wsNames map[string]string
}

// packageExports is one package.json's parsed `exports` field: the
// declaring package's own `"name"` plus its subpath map flattened to
// subpath-key → target file path. The name lets the resolver confirm
// an import addresses this very package before consulting the map.
type packageExports struct {
	name string
	// subpaths maps an `exports` key (`"."`, `"./feature"`, `"./*"`)
	// to its resolved target file (`"./dist/feature.js"`), with the
	// leading `./` kept so the matcher can splice wildcard tails.
	subpaths map[string]string
}

// newNpmAliasIndex builds an index over the given repo roots, reading every
// manifest from the working copy. Returns nil when no usable root is supplied
// — callers treat a nil resolver as "no alias rewriting", which is the
// pre-feature behaviour.
func newNpmAliasIndex(roots map[string]string) *npmAliasIndex {
	return newNpmAliasIndexWithTrees(roots, nil)
}

// newNpmAliasIndexWithTrees builds an index whose manifest reads go through
// the tree the provider returns for a repo root. A nil provider, or one that
// answers nil, reads the working copy at that root.
func newNpmAliasIndexWithTrees(roots map[string]string, trees func(root string) manifestTree) *npmAliasIndex {
	usable := make(map[string]string, len(roots))
	for prefix, root := range roots {
		if root != "" {
			usable[prefix] = root
		}
	}
	if len(usable) == 0 {
		return nil
	}
	return &npmAliasIndex{
		roots:        usable,
		trees:        trees,
		aliasCache:   map[string]map[string]string{},
		depsCache:    map[string]map[string]string{},
		exportsCache: map[string]*packageExports{},
	}
}

// tree returns the manifest tree for one repo root.
func (x *npmAliasIndex) tree(root string) manifestTree {
	if x.trees != nil {
		if t := x.trees(root); t != nil {
			return t
		}
	}
	return newDiskManifestTree(root)
}

// Resolve is the resolver.NpmAliasResolver entry point. callerFile is
// the importing file's graph path (repo-prefixed in multi-repo mode);
// specifier is the verbatim import specifier. It returns the specifier
// with its package portion swapped for the npm-alias real name, or ""
// when the specifier is not an npm alias for this importer.
func (x *npmAliasIndex) Resolve(callerFile, specifier string) string {
	if x == nil || callerFile == "" || specifier == "" {
		return ""
	}
	// Only JS/TS imports go through npm aliases. A relative or
	// absolute specifier is a path import, never a package name.
	if !isJSTSFile(callerFile) {
		return ""
	}
	if strings.HasPrefix(specifier, ".") || strings.HasPrefix(specifier, "/") {
		return ""
	}
	root, relDir, ok := x.locate(callerFile)
	if !ok {
		return ""
	}
	// Workspace-package rewrite (longest-name-wins): a bare specifier whose
	// leading segments name a monorepo workspace package rewrites to that
	// package's directory + remaining sub-path. Returned as a relative
	// specifier (resolved against the importing file) so it lands on the
	// in-repo file regardless of tsconfig `paths`, and only when the file
	// actually exists on disk — otherwise it falls through to the npm-alias /
	// tsconfig resolution below.
	if rel := x.workspaceRewrite(root, relDir, specifier); rel != "" {
		return rel
	}
	pkgName, subPath := splitPackageSpecifier(specifier)
	if pkgName == "" {
		return ""
	}
	// Walk from the importing file's directory up to the repo root,
	// stopping at the first package.json that declares the specifier
	// — npm resolution honours the nearest manifest.
	for dir := relDir; ; dir = path.Dir(dir) {
		manifest := joinRel(dir, "package.json")
		if real, found := x.aliasesFor(root, manifest)[pkgName]; found {
			if subPath == "" {
				return real
			}
			return real + "/" + subPath
		}
		// `exports` subpath map: when this manifest IS the imported
		// package (its `"name"` matches the specifier's package
		// portion), resolve the sub-path through the package's declared
		// `exports` entry points rather than treating it as a bare
		// directory import. `pkg/feature` → `pkg/dist/feature.js`.
		if mapped := x.exportTargetFor(root, manifest, pkgName, subPath); mapped != "" {
			return pkgName + "/" + mapped
		}
		if dir == "." || dir == "" || dir == "/" {
			return ""
		}
	}
}

// exportTargetFor resolves an import of `pkgName` (sub-path `subPath`,
// "" for the package root) through the `exports` field of the
// package.json at rel under root, but only when that manifest declares
// `pkgName` as its own `"name"`. It returns the mapped target file
// relative to the package root with the leading `./` stripped (so the
// caller can splice it after `pkgName`), or "" when the manifest is a
// different package, declares no `exports`, or maps no matching
// sub-path.
func (x *npmAliasIndex) exportTargetFor(root, rel, pkgName, subPath string) string {
	exp := x.exportsFor(root, rel)
	if exp == nil || exp.name != pkgName {
		return ""
	}
	target := resolveExportsSubpath(exp.subpaths, subPath)
	return strings.TrimPrefix(target, "./")
}

// locate resolves callerFile to (repoRoot, repoRelativeDir). The
// longest matching prefix wins so nested repo roots resolve to the
// most specific one.
func (x *npmAliasIndex) locate(callerFile string) (root, relDir string, ok bool) {
	bestPrefix := ""
	bestRoot := ""
	for prefix, r := range x.roots {
		switch {
		case prefix == "":
			// Single-repo mode: graph paths carry no prefix.
			if bestRoot == "" {
				bestRoot = r
			}
		case callerFile == prefix || strings.HasPrefix(callerFile, prefix+"/"):
			if len(prefix) > len(bestPrefix) {
				bestPrefix = prefix
				bestRoot = r
			}
		}
	}
	if bestRoot == "" {
		return "", "", false
	}
	rel := callerFile
	if bestPrefix != "" {
		rel = strings.TrimPrefix(callerFile, bestPrefix+"/")
	}
	return bestRoot, path.Dir(rel), true
}

// aliasesFor returns the npm-alias map (dependency key → real package
// name) parsed from the package.json at rel under root, reading and caching
// it on first request. The result is never nil-returned to callers as
// a map — a missing or alias-free manifest yields an empty map so the
// caller's lookup is a clean miss.
//
// The manifest is read out of the root's tree, so a build indexing a
// committed snapshot resolves aliases from that snapshot's manifests.
func (x *npmAliasIndex) aliasesFor(root, rel string) map[string]string {
	tree := x.tree(root)
	absPath := joinPath(root, rel)
	x.mu.Lock()
	defer x.mu.Unlock()
	if cached, seen := x.aliasCache[absPath]; seen {
		return cached
	}
	var aliases map[string]string
	if src, ok := tree.readFile(rel); ok {
		for _, spec := range modules.ParsePackageJSON(src) {
			if spec.Ecosystem != "npm" || spec.Alias == "" {
				continue
			}
			if aliases == nil {
				aliases = map[string]string{}
			}
			aliases[spec.Path] = spec.Alias
		}
	}
	x.aliasCache[absPath] = aliases
	return aliases
}

// DeclaresExternalDependency implements resolver.NpmDependencyLookup: it
// reports whether the importing file's nearest-ancestor package.json
// declares the specifier's package portion as a dependency that resolves
// OUTSIDE the repository.
//
// npm honours the nearest manifest, so the walk stops at the first
// package.json declaring the name — a nearer `workspace:*` correctly
// shadows a farther registry range. A name declared nowhere reports false:
// absence of a manifest entry is not evidence either way (the file may sit
// outside any package, or the manifest may be unreadable), and the caller's
// entry-point rule still applies.
func (x *npmAliasIndex) DeclaresExternalDependency(callerFile, specifier string) bool {
	if x == nil || callerFile == "" || specifier == "" {
		return false
	}
	if !isJSTSFile(callerFile) {
		return false
	}
	if strings.HasPrefix(specifier, ".") || strings.HasPrefix(specifier, "/") {
		return false
	}
	root, relDir, ok := x.locate(callerFile)
	if !ok {
		return false
	}
	pkgName, _ := splitPackageSpecifier(specifier)
	if pkgName == "" {
		return false
	}
	for dir := relDir; ; dir = path.Dir(dir) {
		manifest := joinRel(dir, "package.json")
		if version, found := x.dependenciesFor(root, manifest)[pkgName]; found {
			return !npmSpecResolvesInRepo(version)
		}
		if dir == "." || dir == "" || dir == "/" {
			return false
		}
	}
}

// npmSpecResolvesInRepo reports whether an npm dependency version spec
// names something inside the repository rather than a published artifact:
// a workspace member (`workspace:*`), a linked or portalled directory
// (`link:../ui`, `portal:../ui`), a local path (`file:../ui`, `./ui`,
// `../ui`, `/abs/ui`).
//
// Everything else — semver ranges, dist-tags, `npm:` aliases, git/github
// specs, tarball URLs, pnpm `catalog:` entries (which resolve to a registry
// version) — comes from outside the tree. The distinction matters because a
// pnpm monorepo declares its own members as dependencies too, and those DO
// legitimately bind to an in-repo directory; `pnpm-workspace.yaml` members
// are not covered by the root manifest's `workspaces` globs, so they reach
// the resolver's directory cascade and must not be refused there.
func npmSpecResolvesInRepo(version string) bool {
	v := strings.TrimSpace(version)
	if v == "" {
		return false
	}
	for _, prefix := range []string{"workspace:", "link:", "portal:", "file:"} {
		if strings.HasPrefix(v, prefix) {
			return true
		}
	}
	return strings.HasPrefix(v, "./") || strings.HasPrefix(v, "../") || strings.HasPrefix(v, "/")
}

// dependenciesFor returns the declared-dependency map (package name →
// verbatim version spec) of the package.json at rel under root, reading and
// caching it on first request. A nil value records "read, but no
// dependencies / missing file" so a miss is not re-read per import edge.
//
// This reuses the very parse aliasesFor already performs — ParsePackageJSON
// returns every dependencies / devDependencies / peerDependencies /
// optionalDependencies entry, and aliasesFor discards all but the aliased
// ones.
func (x *npmAliasIndex) dependenciesFor(root, rel string) map[string]string {
	tree := x.tree(root)
	absPath := joinPath(root, rel)
	x.mu.Lock()
	defer x.mu.Unlock()
	if cached, seen := x.depsCache[absPath]; seen {
		return cached
	}
	var deps map[string]string
	if src, ok := tree.readFile(rel); ok {
		for _, spec := range modules.ParsePackageJSON(src) {
			if spec.Ecosystem != "npm" || spec.Path == "" {
				continue
			}
			if deps == nil {
				deps = map[string]string{}
			}
			deps[spec.Path] = spec.Version
		}
	}
	x.depsCache[absPath] = deps
	return deps
}

// exportsFor returns the parsed `exports` subpath map of the
// package.json at rel under root, reading and caching it on first request. A
// nil result records "read, but no usable `exports` field / missing
// file" so a miss is not re-parsed on every import edge.
func (x *npmAliasIndex) exportsFor(root, rel string) *packageExports {
	tree := x.tree(root)
	absPath := joinPath(root, rel)
	x.mu.Lock()
	defer x.mu.Unlock()
	if cached, seen := x.exportsCache[absPath]; seen {
		return cached
	}
	var exp *packageExports
	if src, ok := tree.readFile(rel); ok {
		exp = parsePackageExports(src)
	}
	x.exportsCache[absPath] = exp
	return exp
}

// parsePackageExports parses the `name` and `exports` fields of a
// package.json. The `exports` field is the modern package entry-point
// map; this flattens it to subpath-key → target file, handling the two
// target shapes npm supports:
//
//	"./feature": "./dist/feature.js"                     (string)
//	"./feature": { "import": "...", "default": "..." }   (conditional)
//
// For a conditional object the `import` condition is preferred, then
// `default` — the order an ES-module consumer would resolve. A bare
// string `exports` (`"exports": "./index.js"`) is treated as the `"."`
// root entry. Returns nil when the manifest is unparseable or declares
// no usable `exports` map.
func parsePackageExports(source []byte) *packageExports {
	if len(source) == 0 {
		return nil
	}
	var manifest struct {
		Name    string          `json:"name"`
		Exports json.RawMessage `json:"exports"`
	}
	if err := json.Unmarshal(source, &manifest); err != nil {
		return nil
	}
	if len(manifest.Exports) == 0 {
		return nil
	}
	subpaths := map[string]string{}
	// A string `exports` is the package root entry point; an object is
	// the subpath map. Conditional objects keyed by condition (`import`
	// / `default`) at the top level also collapse to the `"."` root.
	var asString string
	if err := json.Unmarshal(manifest.Exports, &asString); err == nil {
		if t := strings.TrimSpace(asString); t != "" {
			subpaths["."] = t
		}
	} else {
		var asMap map[string]json.RawMessage
		if err := json.Unmarshal(manifest.Exports, &asMap); err != nil {
			return nil
		}
		// A top-level conditional object (no `"."` / `"./..."` keys, just
		// `import` / `default`) is the root entry point in disguise.
		if !hasSubpathKeys(asMap) {
			if t := pickConditionalTarget(manifest.Exports); t != "" {
				subpaths["."] = t
			}
		} else {
			for key, raw := range asMap {
				if t := pickConditionalTarget(raw); t != "" {
					subpaths[key] = t
				}
			}
		}
	}
	if len(subpaths) == 0 {
		return nil
	}
	return &packageExports{name: manifest.Name, subpaths: subpaths}
}

// hasSubpathKeys reports whether an `exports` object is a subpath map
// (keys begin with `.`) rather than a bare top-level conditional object
// (keys like `import` / `default`). An empty map is treated as a
// subpath map — there is nothing to collapse to the root.
func hasSubpathKeys(m map[string]json.RawMessage) bool {
	for key := range m {
		if strings.HasPrefix(key, ".") {
			return true
		}
	}
	return len(m) == 0
}

// pickConditionalTarget resolves one `exports` value to a target file
// string. A JSON string is returned verbatim; a conditional object
// (`{ "import": "...", "require": "...", "default": "..." }`) is
// resolved by preferring the `import` condition, then `default` — the
// resolution an ES-module consumer performs. Returns "" for any other
// shape (nested arrays, the `null` block-out sentinel, unknown
// conditions only).
func pickConditionalTarget(raw json.RawMessage) string {
	var asString string
	if err := json.Unmarshal(raw, &asString); err == nil {
		return strings.TrimSpace(asString)
	}
	var conditions map[string]json.RawMessage
	if err := json.Unmarshal(raw, &conditions); err != nil {
		return ""
	}
	for _, cond := range []string{"import", "default"} {
		if sub, ok := conditions[cond]; ok {
			if t := pickConditionalTarget(sub); t != "" {
				return t
			}
		}
	}
	return ""
}

// resolveExportsSubpath matches an imported sub-path against a parsed
// `exports` subpath map and returns the mapped target file (leading
// `./` preserved), or "" for no match. The import key is the package
// root (`"."`) when subPath is empty, else `"./" + subPath`. Matching
// order mirrors Node: an exact key wins; otherwise the longest `./*`
// wildcard whose static prefix matches splices the captured tail into
// the target's own `*`. A sub-path containing a `..` segment escapes
// the package and is rejected outright, as Node's resolver does.
func resolveExportsSubpath(subpaths map[string]string, subPath string) string {
	if len(subpaths) == 0 || hasDotDotSegment(subPath) {
		return ""
	}
	key := "."
	if subPath != "" {
		key = "./" + subPath
	}
	if target, ok := subpaths[key]; ok {
		return target
	}
	bestPrefixLen := -1
	best := ""
	for pat, target := range subpaths {
		star := strings.IndexByte(pat, '*')
		if star < 0 {
			continue
		}
		prefix := pat[:star]
		suffix := pat[star+1:]
		if !strings.HasPrefix(key, prefix) || !strings.HasSuffix(key, suffix) {
			continue
		}
		if len(key) < len(prefix)+len(suffix) {
			continue
		}
		if len(prefix) <= bestPrefixLen {
			continue
		}
		captured := key[len(prefix) : len(key)-len(suffix)]
		best = strings.Replace(target, "*", captured, 1)
		bestPrefixLen = len(prefix)
	}
	return best
}

// hasDotDotSegment reports whether a slash-separated sub-path contains
// a `..` segment — a parent-directory escape an `exports` lookup must
// reject so a wildcard like `./*` can never resolve outside the
// package.
func hasDotDotSegment(subPath string) bool {
	for _, seg := range strings.Split(subPath, "/") {
		if seg == ".." {
			return true
		}
	}
	return false
}

// splitPackageSpecifier splits an import specifier into its package
// name and the sub-path within that package. A scoped package keeps
// its `@scope/name` as the package portion:
//
//	"shared"            → ("shared", "")
//	"shared/util"       → ("shared", "util")
//	"@acme/lib"         → ("@acme/lib", "")
//	"@acme/lib/util/x"  → ("@acme/lib", "util/x")
func splitPackageSpecifier(specifier string) (pkgName, subPath string) {
	parts := strings.SplitN(specifier, "/", 4)
	if strings.HasPrefix(specifier, "@") {
		// Scoped: the first two segments form the package name.
		if len(parts) < 2 || parts[0] == "@" || parts[1] == "" {
			return "", ""
		}
		pkgName = parts[0] + "/" + parts[1]
		subPath = strings.TrimPrefix(specifier, pkgName)
		return pkgName, strings.TrimPrefix(subPath, "/")
	}
	pkgName = parts[0]
	subPath = strings.TrimPrefix(specifier, pkgName)
	return pkgName, strings.TrimPrefix(subPath, "/")
}

// joinRel joins a repo-relative directory with a file name, treating
// the repo root (".") as no directory prefix.
func joinRel(dir, name string) string {
	if dir == "." || dir == "" {
		return name
	}
	return dir + "/" + name
}

// isJSTSFile reports whether filePath has a JavaScript/TypeScript
// extension — the only files whose imports resolve through npm.
func isJSTSFile(filePath string) bool {
	switch path.Ext(filePath) {
	case ".ts", ".tsx", ".js", ".jsx", ".mts", ".cts", ".mjs", ".cjs":
		return true
	}
	return false
}

// workspaceRewrite resolves a specifier whose leading segments name a monorepo
// workspace package to a relative import of the matching in-repo file. The
// longest workspace-package name that is a segment-prefix of the specifier
// wins (so `@acme/ui-core/button` binds `@acme/ui-core`, never `@acme/ui`).
// Returns a `../`-relative specifier from relDir to the resolved file stem, or
// "" when no workspace package matches or no file exists on disk.
func (x *npmAliasIndex) workspaceRewrite(root, relDir, specifier string) string {
	names := x.workspaceNames()
	if len(names) == 0 {
		return ""
	}
	segs := strings.Split(specifier, "/")
	minSegs := 1
	if strings.HasPrefix(specifier, "@") {
		minSegs = 2
	}
	for n := len(segs); n >= minSegs; n-- {
		dir, ok := names[strings.Join(segs[:n], "/")]
		if !ok {
			continue
		}
		stem := dir
		if sub := strings.Join(segs[n:], "/"); sub != "" {
			stem = dir + "/" + sub
		}
		if !x.workspaceFileExists(root, stem) {
			return ""
		}
		return relativeImportSpecifier(relDir, stem)
	}
	return ""
}

// workspaceFileExists reports whether the repo-relative JS/TS module stem
// resolves to a file in root's tree — either `stem.<ext>` or
// `stem/index.<ext>`. The probe reads the same snapshot the payload does, so
// a workspace rewrite a committed build makes is one that tree supports.
func (x *npmAliasIndex) workspaceFileExists(root, stem string) bool {
	tree := x.tree(root)
	for _, ext := range []string{".ts", ".tsx", ".d.ts", ".js", ".jsx", ".mts", ".cts", ".mjs", ".cjs"} {
		if tree.isFile(stem + ext) {
			return true
		}
	}
	for _, ext := range []string{".ts", ".tsx", ".js", ".jsx", ".mts", ".cts", ".mjs", ".cjs"} {
		if tree.isFile(joinRel(stem, "index"+ext)) {
			return true
		}
	}
	return false
}

// relativeImportSpecifier returns a `./`/`../`-prefixed specifier that resolves
// from the importing file's directory fromDir to the repo-relative target stem.
func relativeImportSpecifier(fromDir, toStem string) string {
	fromSegs := splitNonEmptyPath(fromDir)
	toSegs := splitNonEmptyPath(toStem)
	i := 0
	for i < len(fromSegs) && i < len(toSegs) && fromSegs[i] == toSegs[i] {
		i++
	}
	var rel []string
	for j := i; j < len(fromSegs); j++ {
		rel = append(rel, "..")
	}
	rel = append(rel, toSegs[i:]...)
	if len(rel) == 0 {
		return "."
	}
	p := strings.Join(rel, "/")
	if !strings.HasPrefix(p, ".") {
		p = "./" + p
	}
	return p
}

func splitNonEmptyPath(p string) []string {
	var out []string
	for _, seg := range strings.Split(p, "/") {
		if seg != "" && seg != "." {
			out = append(out, seg)
		}
	}
	return out
}

// workspaceNames builds (once) the workspace-package name → graph-path-dir map
// from each root manifest's `workspaces` globs.
func (x *npmAliasIndex) workspaceNames() map[string]string {
	x.mu.Lock()
	defer x.mu.Unlock()
	if x.wsNames != nil {
		return x.wsNames
	}
	names := map[string]string{}
	for prefix, root := range x.roots {
		tree := x.tree(root)
		for _, pkgDir := range workspacePackageDirs(tree) {
			src, ok := tree.readFile(joinRel(pkgDir, "package.json"))
			if !ok {
				continue
			}
			var m struct {
				Name string `json:"name"`
			}
			if json.Unmarshal(src, &m) != nil || m.Name == "" {
				continue
			}
			dir := pkgDir
			if prefix != "" {
				dir = prefix + "/" + pkgDir
			}
			names[m.Name] = dir
		}
	}
	x.wsNames = names
	return names
}

// workspacePackageDirs returns the repo-relative directories of every workspace
// package declared by the root package.json's `workspaces` field (npm/yarn
// array form and the yarn `{ "packages": [...] }` object form), expanding each
// glob against tree — the same snapshot the manifest itself was read from, so
// a committed build never discovers a workspace the checkout grew after the
// tree it indexes.
func workspacePackageDirs(tree manifestTree) []string {
	src, ok := tree.readFile("package.json")
	if !ok {
		return nil
	}
	var m struct {
		Workspaces json.RawMessage `json:"workspaces"`
	}
	if json.Unmarshal(src, &m) != nil {
		return nil
	}
	var dirs []string
	for _, glob := range parseWorkspacesField(m.Workspaces) {
		dirs = append(dirs, tree.matchDirs(glob)...)
	}
	return dirs
}

// parseWorkspacesField normalises the two `workspaces` shapes (a bare array of
// globs, or a `{ "packages": [...] }` object) to a glob list.
func parseWorkspacesField(raw json.RawMessage) []string {
	if len(raw) == 0 {
		return nil
	}
	var asArray []string
	if json.Unmarshal(raw, &asArray) == nil {
		return asArray
	}
	var asObj struct {
		Packages []string `json:"packages"`
	}
	if json.Unmarshal(raw, &asObj) == nil {
		return asObj.Packages
	}
	return nil
}
