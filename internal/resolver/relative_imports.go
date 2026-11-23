package resolver

import (
	"sort"
	"strings"

	"github.com/zzet/gortex/internal/graph"
)

// resolveRelativeImports rewrites Python and Dart relative-import edges
// onto the internal `KindFile` node they actually reference. The Go
// resolver's resolveImport / dep-module bridge target language-agnostic
// directory paths, which never line up with Python file stems or Dart
// `..`-walking URIs; without this pass, every relative import landed as
// an `external::*` stub and the subsequent module-attribution sweep
// either left them alone (Dart) or mis-attributed them to a phantom
// pypi package called after the first path segment (Python).
//
// Runs serially after the main resolve loop and BEFORE
// attributeNonGoModuleImports so that any edge resolved to an internal
// file no longer participates in pypi/pub attribution. Edges whose
// target file is not in the graph stay as `external::*` so the
// module-attribution pass can decide what to do with them.
func (r *Resolver) resolveRelativeImports() {
	// Relative-import resolution for Python / Dart relative imports, C-family
	// quoted includes, and PHP literal require/include paths; skip the
	// File-node + edge walk when the graph has none of those languages.
	if !r.graphHasLanguage("python") && !r.graphHasLanguage("dart") &&
		!r.graphHasLanguage("c") && !r.graphHasLanguage("cpp") && !r.graphHasLanguage("objc") &&
		!r.graphHasLanguage("php") {
		return
	}
	fileLang := make(map[string]string, 1024)
	var reindexBatch []graph.EdgeReindex

	// Pre-build a map of every KindFile node's ID. The relative-
	// import resolvers below check 1-2 candidate IDs per edge to
	// decide whether a target file exists; doing that as a per-edge
	// GetNode (a per-edge round-trip on a disk backend) is what made
	// this pass dominate disk-backed resolve time. One NodesByKind scan
	// materialises the set once at indexed cost; lookups become
	// O(1) map hits.
	fileIDs := make(map[string]struct{}, 1024)
	// filesByBase indexes every KindFile by its basename so a non-relative
	// C-family include (`foo/bar.h`) can be resolved against an include root
	// without enumerating the whole file set per include (the `-I` search).
	filesByBase := make(map[string][]string, 1024)
	for file := range graph.FileLanguageNodesSeq(r.graph) {
		if file.ID == "" {
			continue
		}
		fileLang[file.ID] = file.Language
		fileIDs[file.ID] = struct{}{}
		base := file.ID
		if i := strings.LastIndex(file.ID, "/"); i >= 0 {
			base = file.ID[i+1:]
		}
		filesByBase[base] = append(filesByBase[base], file.ID)
	}
	resolvePython := func(stem string) string {
		if !strings.Contains(stem, "/") {
			return ""
		}
		for _, cand := range []string{stem + ".py", stem + "/__init__.py"} {
			if _, ok := fileIDs[cand]; ok {
				return cand
			}
		}
		return ""
	}
	// cppDirsUnion is the deterministic union of every compile-DB include dir —
	// the fallback search path for an importing file (typically a header) that
	// has no translation-unit entry of its own.
	var cppDirsUnion []string
	if len(r.cppIncludeDirs) > 0 {
		seen := map[string]bool{}
		for _, dirs := range r.cppIncludeDirs {
			for _, d := range dirs {
				if !seen[d] {
					seen[d] = true
					cppDirsUnion = append(cppDirsUnion, d)
				}
			}
		}
		sort.Strings(cppDirsUnion)
	} else {
		// No compile_commands.json: fall back to the heuristic include roots
		// (conventional dirs + top-level header dirs, in their intrinsic
		// priority order) so the ordered probe still runs and breaks
		// collisions deterministically even without a compile DB.
		cppDirsUnion = r.cppFallbackDirs
	}
	// resolveCInclude resolves a C-family include to an indexed file, returning
	// the resolved file ID and the `-I` dir it was found under ("" for the
	// same-dir or suffix-fallback paths).
	resolveCInclude := func(importingFile, rel string) (string, string) {
		if rel == "" {
			return "", ""
		}
		dir := ""
		if i := strings.LastIndex(importingFile, "/"); i >= 0 {
			dir = importingFile[:i]
		}
		// (1) Same-dir relative join.
		for _, cand := range []string{joinRelativePath(dir, rel), joinRelativePath(dir, "./"+rel)} {
			if cand != "" {
				if _, ok := fileIDs[cand]; ok {
					return cand, ""
				}
			}
		}
		// (2) compile_commands.json `-I` dir-ordered probe: for each include dir
		// in order, probe dir/rel against the indexed files — first existing wins.
		// This breaks basename collisions deterministically by the TU's `-I`
		// order (authoritative), where the suffix search below would refuse.
		incDirs := r.cppIncludeDirs[importingFile]
		if len(incDirs) == 0 {
			incDirs = cppDirsUnion
		}
		for _, idir := range incDirs {
			if id := cppProbeIncludeDir(fileIDs, idir, rel); id != "" {
				return id, idir
			}
		}
		// (3) Suffix-unique fallback: a multi-segment include that is not
		// relative to the including file binds to the uniquely-matching indexed
		// header whose path ends with that suffix. The recall net for headers
		// with no `-I` dir; refuses on ambiguity so no false edge lands.
		if strings.Contains(rel, "/") {
			base := rel
			if i := strings.LastIndex(rel, "/"); i >= 0 {
				base = rel[i+1:]
			}
			suffix := "/" + rel
			match := ""
			for _, cand := range filesByBase[base] {
				if cand == rel || strings.HasSuffix(cand, suffix) {
					if match != "" && match != cand {
						return "", "" // ambiguous across include roots — refuse
					}
					match = cand
				}
			}
			if match != "" {
				return match, ""
			}
		}
		return "", ""
	}
	resolveDart := func(importingFile, uri string) string {
		if uri == "" || strings.HasPrefix(uri, "dart:") || strings.HasPrefix(uri, "package:") {
			return ""
		}
		dir := ""
		if i := strings.LastIndex(importingFile, "/"); i >= 0 {
			dir = importingFile[:i]
		}
		target := joinRelativePath(dir, uri)
		if target == "" {
			return ""
		}
		if _, ok := fileIDs[target]; ok {
			return target
		}
		return ""
	}

	// EdgesByKind pushes the "kind = imports" filter into the store;
	// disk backends only enumerate import edges instead of every
	// edge in the graph.
	for e := range r.graph.EdgesByKind(graph.EdgeImports) {
		// Scoped warm pass: an unchanged repo's imports were already resolved (or
		// left permanently external) by a prior full pass, so reconsidering them
		// is a no-op. Skip them to keep the pass proportional to the changed set.
		if !r.edgeFromInScope(e.From) {
			continue
		}
		lang, ok := fileLang[e.From]
		if !ok {
			continue
		}
		var path string
		var resolved string
		switch {
		case strings.HasPrefix(e.To, "unresolved::pyrel::"):
			// Python parser-emitted relative-import placeholder.
			// Always resolvable via internal-file lookup.
			path = strings.TrimPrefix(e.To, "unresolved::pyrel::")
			if lang == "python" {
				resolved = resolvePython(path)
			}
		case strings.HasPrefix(e.To, "external::"):
			// Fallthrough path for Dart relative URIs the main
			// resolveImport sweep landed at `external::*`, plus a
			// safety net for any Python relative stem that arrived
			// here without the `pyrel::` marker.
			path = strings.TrimPrefix(e.To, "external::")
			switch lang {
			case "python":
				resolved = resolvePython(path)
			case "dart":
				resolved = resolveDart(e.From, path)
			}
		default:
			// C-family include: a quoted `#include "foo.h"` / `#import "Foo.h"`
			// resolves relative to the including file's directory; an angle
			// `#include <foo/bar.h>` resolves through the `-I` search path —
			// except standard-library headers, which stay external.
			if lang == "c" || lang == "cpp" || lang == "objc" {
				k, _ := e.Meta["include_kind"].(string)
				if (k == "quoted" || k == "system") &&
					strings.HasPrefix(e.To, "unresolved::import::") {
					path = strings.TrimPrefix(e.To, "unresolved::import::")
					// A standard-library angle include (<vector>, <stdio.h>, …)
					// is external by construction: never probe `-I` dirs for it,
					// so it can never bind to an in-tree file sharing its
					// basename. The basename-collision guard.
					if k == "system" && IsCppStdlibHeader(path) {
						continue
					}
					var incDir string
					resolved, incDir = resolveCInclude(e.From, path)
					if resolved != "" && incDir != "" {
						if e.Meta == nil {
							e.Meta = map[string]any{}
						}
						// A compile-DB dir is authoritative; the heuristic
						// fallback set is consulted only when no DB exists.
						via := "compile_db"
						if len(r.cppIncludeDirs) == 0 {
							via = "heuristic"
						}
						e.Meta["include_dir"] = incDir
						e.Meta["resolved_via"] = via
					}
				}
			}
			// PHP literal `require`/`include` of a file path. The `use`-namespace
			// imports the extractor also lowers to `unresolved::import::` are NOT
			// path-shaped (no `.php` / leading `./`,`../`,`/`) and resolve through
			// the main import sweep, so isPhpPathInclude keeps them untouched.
			if lang == "php" && strings.HasPrefix(e.To, "unresolved::import::") {
				raw := strings.TrimPrefix(e.To, "unresolved::import::")
				if isPhpPathInclude(raw) {
					path = raw
					resolved = resolvePhpInclude(e.From, raw, fileIDs, filesByBase)
				}
			}
			if resolved == "" {
				continue
			}
		}
		if resolved == "" {
			// pyrel:: edges that don't find an internal target are
			// downgraded to `external::` so the module-attribution
			// pass + audits don't see the internal marker prefix.
			if strings.HasPrefix(e.To, "unresolved::pyrel::") {
				oldTo := e.To
				e.To = "external::" + path
				reindexBatch = append(reindexBatch, graph.EdgeReindex{Edge: e, OldTo: oldTo})
			}
			continue
		}
		oldTo := e.To
		e.To = resolved
		e.Origin = graph.OriginASTResolved
		reindexBatch = append(reindexBatch, graph.EdgeReindex{Edge: e, OldTo: oldTo})
	}
	if len(reindexBatch) > 0 {
		r.noteImportEdgeReindexes(reindexBatch)
		r.applyReindexEdges(reindexBatch)
	}
}

// cppProbeIncludeDir probes a single include directory for `dir/rel` against
// the indexed file set, trying common header extensions when the include omits
// one. Returns the matching file ID, or "".
func cppProbeIncludeDir(fileIDs map[string]struct{}, dir, rel string) string {
	cand := joinRelativePath(dir, rel)
	if cand == "" {
		return ""
	}
	if _, ok := fileIDs[cand]; ok {
		return cand
	}
	base := rel
	if i := strings.LastIndex(rel, "/"); i >= 0 {
		base = rel[i+1:]
	}
	if !strings.Contains(base, ".") {
		for _, ext := range []string{".h", ".hpp", ".hh", ".hxx"} {
			if _, ok := fileIDs[cand+ext]; ok {
				return cand + ext
			}
		}
	}
	return ""
}

// isPhpPathInclude reports whether a PHP `unresolved::import::` target is a
// file-path include (`require 'lib/x.php'`, `require __DIR__ . '/lib/x.php'`)
// rather than a `use`-namespace import. Namespaces are lowered with `\`→`/` but
// never carry a `.php` extension or a leading `./`,`../`,`/`, so the extension
// and path-prefix shape distinguishes the two unambiguously.
func isPhpPathInclude(target string) bool {
	if target == "" {
		return false
	}
	return strings.HasSuffix(strings.ToLower(target), ".php") ||
		strings.HasPrefix(target, "/") ||
		strings.HasPrefix(target, "./") ||
		strings.HasPrefix(target, "../")
}

// resolvePhpInclude resolves a PHP literal include path to an indexed file:
// relative to the including file's directory first (the `__DIR__` convention,
// leading `/` being relative to that dir), then repo-root-relative, then a
// unique path-suffix match. A `.php` extension is appended when the include
// omits one. Returns "" when no unique indexed file matches.
func resolvePhpInclude(importingFile, rel string, fileIDs map[string]struct{}, filesByBase map[string][]string) string {
	withExt := rel
	if !strings.HasSuffix(strings.ToLower(rel), ".php") {
		withExt += ".php"
	}
	dir := ""
	if i := strings.LastIndex(importingFile, "/"); i >= 0 {
		dir = importingFile[:i]
	}
	for _, cand := range []string{joinRelativePath(dir, withExt), joinRelativePath("", withExt)} {
		if cand != "" {
			if _, ok := fileIDs[cand]; ok {
				return cand
			}
		}
	}
	// Project-root-relative suffix net: a unique indexed file ending with the
	// include path. Refuses on ambiguity so no false edge lands.
	base := withExt
	if i := strings.LastIndex(withExt, "/"); i >= 0 {
		base = withExt[i+1:]
	}
	suffix := "/" + withExt
	match := ""
	for _, cand := range filesByBase[base] {
		if cand == withExt || strings.HasSuffix(cand, suffix) {
			if match != "" && match != cand {
				return "" // ambiguous across roots
			}
			match = cand
		}
	}
	return match
}

// joinRelativePath joins a relative URI onto a directory and collapses
// `.`/`..` segments. Returns "" when the path walks above the repo root
// (which we never want to silently silently fall through to an
// arbitrary file).
func joinRelativePath(dir, rel string) string {
	var parts []string
	if dir != "" {
		parts = strings.Split(dir, "/")
	}
	for _, seg := range strings.Split(rel, "/") {
		switch seg {
		case "", ".":
			// noop
		case "..":
			if len(parts) == 0 {
				return ""
			}
			parts = parts[:len(parts)-1]
		default:
			parts = append(parts, seg)
		}
	}
	return strings.Join(parts, "/")
}
