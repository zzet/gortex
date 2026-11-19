package indexer

import (
	"strings"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser/nuxtimports"
	"github.com/zzet/gortex/internal/resolver"
)

func fileDefaultExport(nodes []*graph.Node) string {
	for _, n := range nodes {
		if n != nil && n.Kind == graph.KindFile {
			if name, _ := n.Meta[graph.DefaultExportMeta].(string); name != "" {
				return name
			}
		}
	}
	return ""
}

// Resolve the caller's spelling again, not the old implementation name.
func parkNuxtCall(e *graph.Edge, targetChanged bool, callers *[]string) bool {
	local, _ := e.Meta[graph.NuxtImportNameMeta].(string)
	if !targetChanged || local == "" {
		return false
	}
	delete(e.Meta, graph.NuxtImportNameMeta)
	delete(e.Meta, "resolution")
	e.Origin, e.Tier, e.ConfidenceLabel, e.Confidence = "", "", "", 0
	e.To = graph.UnresolvedMarker + local
	if callers != nil {
		*callers = append(*callers, e.FilePath)
	}
	return true
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

func (idx *Indexer) visitNuxtIncomingNames(file string, visit func(string)) {
	idx.lookupResolutionInputs().nuxt.VisitLocals(strings.TrimPrefix(file, idx.repoPrefix+"/"), visit)
}

func (mi *MultiIndexer) visitNuxtIncomingNames(file string, visit func(string)) {
	in, prefix, _ := mi.resolutionInputsForFile(file)
	if in != nil {
		in.nuxt.VisitLocals(strings.TrimPrefix(file, prefix+"/"), visit)
	}
}

func (idx *Indexer) rebindParkedNuxtCalls(files []string, batch *reparsePendingEnrichmentBatch) {
	if len(files) == 0 {
		return
	}
	if batch != nil && batch.deferResolverCatchup {
		batch.nuxtCallers = appendUniqueSorted(batch.nuxtCallers, files...)
		return
	}
	idx.resolver.ResolveFilesOutgoing(files)
}
