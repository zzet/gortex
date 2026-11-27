package resolver

import (
	"strings"

	"github.com/zzet/gortex/internal/graph"
)

// resolveRazorUsings binds Razor / Blazor simple-type references — component
// tags (`<Counter/>`), `@model` / `@inherits` / `@inject` / `@typeof` targets —
// that are reachable only through an imported namespace.
//
// The Razor extractor lowers each `@using Some.Namespace` directive to a
// per-file `unresolved::razor_using::<namespace>` marker import edge. This pass
// computes every `.razor` / `.cshtml` file's effective namespace set — its own
// `@using` directives unioned with the cascade of `_Imports.razor` files from
// its directory up to the root — then, for each still-unresolved simple-type
// reference, binds it to the unique indexed type (or Razor component) whose
// namespace is in that set, refusing on ambiguity. The marker edges are
// scaffolding and are removed once consumed.
//
// Runs serially in ResolveAll's relative-import settle window, gated on the
// presence of Razor in the graph.
func (r *Resolver) resolveRazorUsings() {
	if !r.graphHasLanguage("razor") {
		return
	}

	// Collect each file's @using namespaces from the marker edges.
	usingsByFile := map[string][]string{}
	var markerEdges []*graph.Edge
	for e := range r.graph.EdgesByKind(graph.EdgeImports) {
		if e == nil || !strings.HasPrefix(e.To, "unresolved::razor_using::") {
			continue
		}
		// Scoped warm pass: only the changed repos carry fresh @using markers; an
		// unchanged repo's were consumed (and removed) by a prior full pass.
		if !r.edgeFromInScope(e.From) {
			continue
		}
		if ns := strings.TrimPrefix(e.To, "unresolved::razor_using::"); ns != "" {
			usingsByFile[e.From] = append(usingsByFile[e.From], ns)
		}
		markerEdges = append(markerEdges, e)
	}
	if len(markerEdges) == 0 {
		return
	}

	// The _Imports.razor cascade sources, keyed by their directory.
	importDirs := map[string][]string{}
	for fileID, nss := range usingsByFile {
		if razorBaseName(fileID) == "_Imports.razor" {
			d := razorDir(fileID)
			importDirs[d] = append(importDirs[d], nss...)
		}
	}

	// effectiveNS = a file's own @using set ∪ every _Imports.razor in an
	// ancestor (or the same) directory.
	effectiveNS := func(fileID string) map[string]struct{} {
		set := map[string]struct{}{}
		for _, ns := range usingsByFile[fileID] {
			set[ns] = struct{}{}
		}
		fileDir := razorDir(fileID)
		for impDir, nss := range importDirs {
			if impDir == fileDir || impDir == "" || strings.HasPrefix(fileDir, impDir+"/") {
				for _, ns := range nss {
					set[ns] = struct{}{}
				}
			}
		}
		return set
	}

	// Type index: simple name → candidate (id, namespace, language). Razor
	// components and C# classes/structs/records/enums are KindType; interfaces
	// KindInterface.
	type typeCand struct{ id, ns, lang string }
	typesByName := map[string][]typeCand{}
	for _, kind := range []graph.NodeKind{graph.KindType, graph.KindInterface} {
		for n := range r.graph.NodesByKind(kind) {
			if n == nil || n.Name == "" {
				continue
			}
			ns, _ := n.Meta["scope_ns"].(string)
			typesByName[n.Name] = append(typesByName[n.Name], typeCand{id: n.ID, ns: ns, lang: n.Language})
		}
	}

	fileLang := r.collectFileLanguages()
	var reindexBatch []graph.EdgeReindex
	for e := range r.graph.EdgesByKind(graph.EdgeReferences) {
		if e == nil || !strings.HasPrefix(e.To, "unresolved::") {
			continue
		}
		if !r.edgeFromInScope(e.From) {
			continue
		}
		name := strings.TrimPrefix(e.To, "unresolved::")
		if name == "" {
			continue
		}
		fileID := e.From
		if i := strings.Index(fileID, "::"); i >= 0 {
			fileID = fileID[:i]
		}
		refLang := fileLang[fileID]
		if refLang != "razor" {
			continue
		}
		cands := typesByName[name]
		if len(cands) == 0 {
			continue
		}
		eff := effectiveNS(fileID)
		if len(eff) == 0 {
			continue
		}
		match, ambiguous := "", false
		for _, c := range cands {
			if c.ns == "" {
				continue
			}
			// Only bind within the same language family — a Razor reference may
			// bind a C# type (both dotnet) but never a coincidentally-named
			// TypeScript component.
			if !sameLanguageFamily(refLang, c.lang) {
				continue
			}
			if _, ok := eff[c.ns]; !ok {
				continue
			}
			if match != "" && match != c.id {
				ambiguous = true
				break
			}
			match = c.id
		}
		if ambiguous || match == "" {
			continue
		}
		oldTo := e.To
		e.To = match
		e.Origin = graph.OriginASTResolved
		if e.Meta == nil {
			e.Meta = map[string]any{}
		}
		delete(e.Meta, "guard_reverted")
		e.Meta["resolved_via"] = "razor_using"
		reindexBatch = append(reindexBatch, graph.EdgeReindex{Edge: e, OldTo: oldTo})
	}
	if len(reindexBatch) > 0 {
		if !r.applyReindexEdges(reindexBatch) {
			return
		}
	}
	// The marker edges were scaffolding for this pass — remove them so they do
	// not linger as unresolved imports. The removal rewrites each file's
	// imports rows, so it must dirty the adjacency retention like any other
	// imports-kind write; the reference retargets above cannot (references
	// never carry an imports row).
	r.noteImportEdgeRemovals(markerEdges)
	for _, e := range markerEdges {
		r.graph.RemoveEdge(e.From, e.To, e.Kind)
	}
}

func razorBaseName(id string) string {
	if i := strings.LastIndex(id, "/"); i >= 0 {
		return id[i+1:]
	}
	return id
}

func razorDir(id string) string {
	if i := strings.LastIndex(id, "/"); i >= 0 {
		return id[:i]
	}
	return ""
}
