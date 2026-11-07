package resolver

import (
	"path"
	"strings"

	"github.com/zzet/gortex/internal/graph"
)

// resolveLuaRequires binds Lua / Luau `require(...)` import edges that the
// extractor left on `unresolved::import::<module>` placeholders onto the
// indexed module file they reference.
//
// The Lua extractor carries the full dotted module path for a classic string
// require (`require("a.b.c")` → target `a.b.c`) and the leaf module name plus
// a `roblox_path` for an instance-path require (`require(script.Parent.Foo)` →
// target `Foo`, with `roblox_path` set). Classic requires map the dotted path
// to candidate files (`a/b/c.lua`, the `.luau` form, then the `a/b/c/init.*`
// directory module), the direct repo-relative path winning, then a unique
// path-suffix match (the package-root net). Roblox instance-path requires bind
// to the unique `<leaf>.lua` / `<leaf>.luau` file in the repo, refusing on
// ambiguity. Luau require-by-string paths (`./X`, `../X`, `@self/X`) bind to
// the one file they name relative to the requiring file. Edges whose target
// is not indexed stay unresolved.
//
// Runs serially in ResolveAll's relative-import settle window, after
// resolveRelativeImports (which never touches Lua). Reaching it at all takes
// isLuaRequireEdge holding the generic import resolver off.
func (r *Resolver) resolveLuaRequires() {
	if !r.graphHasLanguage("lua") && !r.graphHasLanguage("luau") {
		return
	}
	fileLang := make(map[string]string, 1024)
	fileIDs := make(map[string]struct{}, 1024)
	// filesByBase indexes every KindFile by basename for the suffix nets.
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

	var reindexBatch []graph.EdgeReindex
	for e := range r.graph.EdgesByKind(graph.EdgeImports) {
		if e == nil || !strings.HasPrefix(e.To, "unresolved::import::") {
			continue
		}
		// Scoped warm pass: an unchanged repo's requires were already bound by a
		// prior full pass, so only reconsider the changed repos' imports.
		if !r.edgeFromInScope(e.From) {
			continue
		}
		if lang := fileLang[e.From]; lang != "lua" && lang != "luau" {
			continue
		}
		name := strings.TrimPrefix(e.To, "unresolved::import::")
		if name == "" {
			continue
		}
		resolved := resolveLuaRequire(fileIDs, filesByBase, e, name)
		if resolved == "" {
			continue
		}
		oldTo := e.To
		e.To = resolved
		e.Origin = graph.OriginASTResolved
		reindexBatch = append(reindexBatch, graph.EdgeReindex{Edge: e, OldTo: oldTo})
	}
	if len(reindexBatch) > 0 {
		r.noteImportEdgeReindexes(reindexBatch)
		r.graph.ReindexEdges(reindexBatch)
	}
}

// isLuaRequireEdge reports whether an import edge is a Lua / Luau require,
// which resolveLuaRequires owns.
//
// The generic import resolver must leave it alone. It runs first, and it
// either stubs a require it cannot place to `external::<spec>` — which the
// Lua pass never looks at, so no require bound through the real pipeline —
// or matches the spec's last segment against a directory index and binds
// `require("./Pkg")` to an arbitrary file under any `Pkg/` folder. Holding it
// off costs nothing: the pass that owns these runs in the same resolve, and
// mirrors how isGodotResPlaceholder treats `res://` paths.
func isLuaRequireEdge(e *graph.Edge) bool {
	return strings.HasSuffix(e.FilePath, ".lua") || strings.HasSuffix(e.FilePath, ".luau")
}

// resolveLuaRequire dispatches one require on its form: a Roblox instance
// path, a Luau require-by-string path, or a classic dotted module path.
func resolveLuaRequire(fileIDs map[string]struct{}, filesByBase map[string][]string, e *graph.Edge, spec string) string {
	if _, isRoblox := e.Meta["roblox_path"]; isRoblox {
		return resolveLuaRobloxRequire(filesByBase, spec)
	}
	if isLuauPathRequire(spec) {
		return resolveLuauPathRequire(fileIDs, e.From, spec)
	}
	return resolveLuaModuleRequire(fileIDs, filesByBase, spec)
}

// luaModuleCandidates converts a dotted/slashed Lua module path to its ordered
// candidate file paths.
func luaModuleCandidates(modPath string) []string {
	return luaFileCandidates(strings.Trim(strings.ReplaceAll(modPath, ".", "/"), "/"))
}

// luaFileCandidates lists the files a slash-separated module path can name:
// the `.lua` / `.luau` file, then the `init` directory module.
func luaFileCandidates(p string) []string {
	if p == "" {
		return nil
	}
	return []string{p + ".lua", p + ".luau", p + "/init.lua", p + "/init.luau"}
}

// resolveLuaModuleRequire resolves a classic string require's dotted module
// path: the direct repo-relative candidate file wins; otherwise a unique
// path-suffix match (handling package-root-prefixed layouts) wins, refusing on
// ambiguity.
func resolveLuaModuleRequire(fileIDs map[string]struct{}, filesByBase map[string][]string, modPath string) string {
	cands := luaModuleCandidates(modPath)
	for _, c := range cands {
		if _, ok := fileIDs[c]; ok {
			return c
		}
	}
	for _, c := range cands {
		if m := uniqueFileSuffixMatch(filesByBase, c); m != "" {
			return m
		}
	}
	return ""
}

// resolveLuaRobloxRequire binds a Roblox instance-path require by its leaf
// module name to the unique indexed `<leaf>.lua` / `<leaf>.luau` file, refusing
// on ambiguity.
func resolveLuaRobloxRequire(filesByBase map[string][]string, leaf string) string {
	match := ""
	for _, ext := range []string{".lua", ".luau"} {
		for _, cand := range filesByBase[leaf+ext] {
			if match != "" && match != cand {
				return "" // ambiguous module name in the repo
			}
			match = cand
		}
	}
	return match
}

// isLuauPathRequire reports whether a string require uses Luau's
// require-by-string syntax rather than a classic dotted module path. Luau
// rejects an unprefixed path, so the prefix alone decides it.
func isLuauPathRequire(spec string) bool {
	return strings.HasPrefix(spec, "./") || strings.HasPrefix(spec, "../") || strings.HasPrefix(spec, "@")
}

// resolveLuauPathRequire binds a Luau require-by-string path to the one file
// it names relative to the requiring file. A relative path names exactly one
// place, so a miss stays unresolved instead of falling back to the suffix net,
// which would bind `./Util` to some other folder's `Util.luau`.
func resolveLuauPathRequire(fileIDs map[string]struct{}, fromFile, spec string) string {
	modPath, ok := luauRequireModulePath(fromFile, spec)
	if !ok {
		return ""
	}
	return luauModuleFile(fileIDs, modPath)
}

// luauRequireModulePath joins a require-by-string path onto the requiring
// file, per Luau's amended require resolution: `./` and `../` start at the
// file's directory, and an `init` file stands in for its directory, so its
// `./` starts one level up; `@self` is the requiring module itself. Other
// aliases (`.luaurc` aliases, Rojo's `@game`) need config this pass does not
// read, and a path that climbs out of the repo root names nothing indexed.
func luauRequireModulePath(fromFile, spec string) (string, bool) {
	dir, file := path.Split(fromFile)
	dir = strings.TrimSuffix(dir, "/")
	stem := strings.TrimSuffix(file, path.Ext(file))
	isInit := stem == "init"

	var base, rest string
	switch {
	case spec == "@self" || strings.HasPrefix(spec, "@self/"):
		base, rest = path.Join(dir, stem), strings.TrimPrefix(spec, "@self")
		if isInit {
			base = dir
		}
	case strings.HasPrefix(spec, "@"):
		return "", false
	default:
		base, rest = dir, spec
		if isInit {
			base = path.Join(dir, "..")
		}
	}
	joined := path.Join(base, rest)
	if joined == "." || joined == ".." || strings.HasPrefix(joined, "../") {
		return "", false
	}
	return joined, true
}

// luauModuleFile returns the one indexed file a resolved module path names.
// Luau errors when a path matches more than one candidate (`X.luau` beside
// `X.lua`, or beside `X/init.luau`), so that refuses here too.
func luauModuleFile(fileIDs map[string]struct{}, modPath string) string {
	match := ""
	for _, c := range luaFileCandidates(modPath) {
		if _, ok := fileIDs[c]; !ok {
			continue
		}
		if match != "" {
			return ""
		}
		match = c
	}
	return match
}
