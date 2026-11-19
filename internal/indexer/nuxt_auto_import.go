package indexer

import (
	"path"
	"slices"
	"strings"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser/nuxtimports"
	"github.com/zzet/gortex/internal/resolver"
)

// Body-only edits cannot change which implementation a module exports. The
// import fingerprint covers forwarded modules; the file anchor covers local
// export lists (including changes between two already declared defaults).
func jsModuleSurfaceChanged(file string, prior, fresh []*graph.Node) bool {
	if !closureJSTSPath(file) {
		return false
	}
	a, b := storedDerivedFingerprints(prior), storedDerivedFingerprints(fresh)
	if importSurfaceChanged(a, b) || a.declarations == "" || a.declarations != b.declarations {
		return true
	}
	surface := func(nodes []*graph.Node) (string, string) {
		for _, n := range nodes {
			if n != nil && n.Kind == graph.KindFile {
				return stringMeta(n.Meta, graph.DefaultExportMeta), stringMeta(n.Meta, graph.JSLocalExportsMeta)
			}
		}
		return "", ""
	}
	ad, al := surface(prior)
	bd, bl := surface(fresh)
	return ad != bd || al != bl
}

// Resolve the caller's spelling again, not the old implementation name.
func parkExactCall(e *graph.Edge) bool {
	local, _ := e.Meta[graph.JSBindingNameMeta].(string)
	if local == "" {
		return false
	}
	e.CrossRepo = false
	delete(e.Meta, "resolution")
	e.Origin, e.Tier, e.ConfidenceLabel, e.Confidence = "", "", "", 0
	e.To = graph.UnresolvedMarker + local
	return true
}

// Eviction removes edges by destination, even though the import syntax is
// owned by the consumer. Preserve its raw specifier for resolution after the edit.
func restubJSImports(edges []*graph.Edge, evicted map[string]struct{}, moves []graph.EdgeReindex, callers []string) ([]graph.EdgeReindex, []string) {
	for _, e := range edges {
		if e == nil || (e.Kind != graph.EdgeImports && e.Kind != graph.EdgeReExports) || graph.IsUnresolvedTarget(e.To) || strings.HasPrefix(e.To, "external::") {
			continue
		}
		spec, _ := e.Meta[graph.JSImportSourceMeta].(string)
		if _, gone := evicted[e.From]; gone || spec == "" {
			continue
		}
		moves = append(moves, parkJSImport(e, spec))
		callers = append(callers, e.FilePath)
	}
	return moves, callers
}

func parkJSImport(e *graph.Edge, spec string) graph.EdgeReindex {
	old := e.To
	graph.StashRestubProvenance(e)
	e.To = graph.UnresolvedMarker + "import::" + spec
	return graph.EdgeReindex{Edge: e, OldTo: old}
}

// Walk reverse module dependencies, not a name-only call graph. Two unrelated
// modules may expose the same export. Only forwarding imports propagate edits.
func (idx *Indexer) parkModuleDependents(files []string) []string {
	if !slices.ContainsFunc(files, closureJSTSPath) {
		return nil
	}
	var queue []string
	seen := map[string]bool{}
	push := func(stem string) {
		if !seen[stem] {
			seen[stem] = true
			queue = append(queue, stem)
		}
	}
	aliases := idx.tsAliasCollection()
	var aliasChoices map[string]bool
	addStem := func(stem string) {
		push(stem)
		rel := strings.TrimPrefix(stem, idx.repoPrefix+"/")
		aliases.VisitAlternateStems(rel, func(alt string) {
			alt = prefixTSAliasTarget(idx.repoPrefix, alt)
			if aliasChoices == nil {
				aliasChoices = make(map[string]bool)
			}
			aliasChoices[stem], aliasChoices[alt] = true, true
			push(alt)
		})
	}
	addFile := func(file string) {
		if closureJSTSPath(file) {
			stem := graph.JSModuleStem(file)
			addStem(stem)
			if path.Base(stem) == "index" {
				addStem(path.Dir(stem))
			}
		}
	}
	for _, file := range files {
		addFile(file)
	}
	var callers []string
	var moves []graph.EdgeReindex
	lookup := graph.JSModuleDependencyLookup(idx.graph)
	for len(queue) > 0 {
		stem := queue[0]
		queue = queue[1:]
		for _, e := range lookup(stem) {
			if e.Kind != graph.EdgeCalls {
				if e.Kind == graph.EdgeReExports || e.Meta[graph.JSForwardsImportMeta] == true {
					addFile(e.FilePath)
				}
				raw, _ := e.Meta[graph.JSImportSourceMeta].(string)
				if (aliasChoices[stem] || strings.HasPrefix(e.To, "external::")) &&
					raw != "" && !strings.HasPrefix(raw, "./") && !strings.HasPrefix(raw, "../") && e.Meta[resolver.NpmImportSpecifierMeta] == nil {
					callers = append(callers, e.FilePath)
					if !graph.IsUnresolvedTarget(e.To) {
						moves = append(moves, parkJSImport(e, raw))
					}
				}
				continue
			}
			callers = append(callers, e.FilePath)
			if !graph.IsUnresolvedTarget(e.To) {
				old := e.To
				if parkExactCall(e) {
					moves = append(moves, graph.EdgeReindex{Edge: e, OldTo: old})
				}
			}
		}
	}
	if len(moves) > 0 {
		if w, ok := idx.graph.(interface {
			ReindexEdgesApplied([]graph.EdgeReindex) bool
		}); ok {
			if !w.ReindexEdgesApplied(moves) {
				// The batch can be partially applied. Re-extract its callers
				// from source rather than replaying superseded edge identities.
				for _, m := range moves {
					idx.failedBindingWrites = append(idx.failedBindingWrites, m.Edge.FilePath)
				}
			}
		} else {
			idx.graph.ReindexEdges(moves)
		}
	}
	return appendUniqueSorted(nil, callers...)
}

func resolveNuxtBinding(coll *nuxtimports.Collection, prefix, caller, name string) (string, string) {
	if prefix != "" {
		if !strings.HasPrefix(caller, prefix+"/") {
			return "", ""
		}
		caller = strings.TrimPrefix(caller, prefix+"/")
	}
	binding := coll.Resolve(caller, name)
	if binding.File == "" {
		return "", ""
	}
	return prefixTSAliasTarget(prefix, binding.File), binding.Name
}

func (idx *Indexer) resolveNuxtAutoImport(caller, name string) (string, string) {
	return resolveNuxtBinding(idx.lookupResolutionInputs().nuxt, idx.repoPrefix, caller, name)
}

func (mi *MultiIndexer) nuxtAutoImportResolver() resolver.NuxtAutoImportResolver {
	return func(caller, name string) (string, string) {
		in, prefix, _ := mi.resolutionInputsForFile(caller)
		if in == nil {
			return "", ""
		}
		return resolveNuxtBinding(in.nuxt, prefix, caller, name)
	}
}

func (idx *Indexer) rebindParkedCalls(files []string, batch *reparsePendingEnrichmentBatch) {
	if len(files) == 0 {
		return
	}
	if batch != nil && batch.deferResolverCatchup {
		batch.bindingCallers = appendUniqueSorted(batch.bindingCallers, files...)
		return
	}
	idx.resolver.ResolveFilesOutgoing(files)
}
