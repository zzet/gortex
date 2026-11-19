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
			if name, _ := n.Meta["default_export"].(string); name != "" {
				return name
			}
		}
	}
	return ""
}

// Resolve the caller's spelling again, not the old implementation name.
func parkNuxtCall(e *graph.Edge, local string) {
	delete(e.Meta, "nuxt_import_name")
	delete(e.Meta, "resolution")
	e.Origin, e.Tier, e.ConfidenceLabel, e.Confidence = "", "", "", 0
	e.To = graph.UnresolvedMarker + local
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
		for _, meta := range mi.AllMetadata() {
			if meta.RepoPrefix != "" && strings.HasPrefix(caller, meta.RepoPrefix+"/") {
				if idx := mi.GetIndexer(meta.RepoPrefix); idx != nil && idx.contentSrc.Load() != nil {
					return idx.resolveNuxtAutoImport(caller, name)
				}
				return resolveNuxtBinding(workingResolutionInputs(meta.RootPath, nil).nuxt, meta.RepoPrefix, caller, name)
			}
		}
		return "", ""
	}
}
