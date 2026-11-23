package resolver

import (
	"strings"

	"github.com/zzet/gortex/internal/graph"
)

// godotResScheme is the Godot resource scheme. A `res://` path is
// absolute from the Godot project root — the directory holding
// project.godot — which is not necessarily the repository root, so the
// path cannot be joined at extraction time.
const godotResScheme = "res://"

// godotResPlaceholderPrefix is where an unbound `res://` reference parks
// until this pass lands it on the indexed file.
const godotResPlaceholderPrefix = "unresolved::import::" + godotResScheme

// isGodotResPlaceholder reports whether a target is a parked `res://`
// reference that resolveGodotResPaths owns.
//
// The generic resolvers must leave it alone. A `res://` path is rooted at
// the Godot project, never at a package registry, so the `external::`
// stub they fall back to is always wrong — and it is also unrecoverable:
// once the scheme is buried inside a bookkeeping string, the later Godot
// pass cannot tell it from any other unresolved import, and every scene →
// script edge is lost. That is what used to happen on every real index:
// this pass ran, found no placeholder left to bind, and did nothing.
//
// Holding them back costs nothing — the pass that owns them runs in the
// same resolve — and a path this pass cannot bind stays unresolved rather
// than becoming an `external::` stub, which is the honest answer for an
// asset outside the corpus and the one the pass's own tests pin.
func isGodotResPlaceholder(to string) bool {
	return strings.HasPrefix(to, godotResPlaceholderPrefix)
}

// resolveGodotResPaths binds `res://` references onto the file they name.
//
// Two extractors mint these placeholders and neither could resolve them:
// the scene extractor's `[ext_resource path="res://…"]` (a scene → script
// / sub-scene / asset edge) and GDScript's `preload("res://…")`. Without
// this pass a Godot script's blast radius misses every scene that mounts
// it, because in Godot a script is reached through its scene rather than
// through a call.
//
// The project root is recovered by path suffix rather than by locating
// project.godot: a `res://` path is already rooted, so the indexed file
// it names is the one whose repo-relative path ends with it. Ambiguity
// (the same relative path under two project roots in one workspace)
// refuses rather than guesses — see uniqueFileSuffixMatch.
//
// Runs serially in ResolveAll's relative-import settle window, alongside
// the Lua require binding it mirrors. Reaching that window at all takes
// isGodotResPlaceholder holding the generic resolvers off: they run
// first, and each of them stubs an unrecognised import out to
// `external::<path>` — which this pass can no longer recognise, so
// nothing here ever fired through the real pipeline.
func (r *Resolver) resolveGodotResPaths() {
	if !r.graphHasLanguage("godot_resource") && !r.graphHasLanguage("gdscript") {
		return
	}
	// filesByBase indexes every indexed file by basename for the suffix net.
	filesByBase := make(map[string][]string, 1024)
	for file := range graph.FileLanguageNodesSeq(r.graph) {
		if file.ID == "" {
			continue
		}
		base := file.ID
		if i := strings.LastIndex(file.ID, "/"); i >= 0 {
			base = file.ID[i+1:]
		}
		filesByBase[base] = append(filesByBase[base], file.ID)
	}

	var reindexBatch []graph.EdgeReindex
	// Scene references ride EdgeReferences; GDScript preload/load rides
	// EdgeImports. Both park on the same placeholder shape.
	for _, kind := range []graph.EdgeKind{graph.EdgeReferences, graph.EdgeImports} {
		for e := range r.graph.EdgesByKind(kind) {
			if e == nil || !strings.HasPrefix(e.To, godotResPlaceholderPrefix) {
				continue
			}
			// Scoped warm pass: an unchanged repo's references were bound by
			// a prior full pass, so only reconsider the changed repos'.
			if !r.edgeFromInScope(e.From) {
				continue
			}
			resPath := strings.TrimPrefix(strings.TrimPrefix(e.To, "unresolved::import::"), godotResScheme)
			resPath = strings.Trim(resPath, "/")
			if resPath == "" {
				continue
			}
			target := uniqueFileSuffixMatch(filesByBase, resPath)
			if target == "" {
				continue
			}
			oldTo := e.To
			e.To = target
			// The path is written out in full and matched exactly — this is
			// a structural bind, not a name guess.
			e.Origin = graph.OriginASTResolved
			reindexBatch = append(reindexBatch, graph.EdgeReindex{Edge: e, OldTo: oldTo})
		}
	}
	if len(reindexBatch) > 0 {
		r.noteImportEdgeReindexes(reindexBatch)
		r.applyReindexEdges(reindexBatch)
	}
}
