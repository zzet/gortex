package resolver

import (
	"github.com/zzet/gortex/internal/graph"
)

// NuxtAutoImportResolver returns a generated app binding's source stem and
// original export, or empty strings outside a prepared Nuxt app's scope.
type NuxtAutoImportResolver func(callerFile, name string) (stem, exportedName string)

// SetNuxtAutoImportResolver installs generated bindings before a resolve pass.
func (r *Resolver) SetNuxtAutoImportResolver(fn NuxtAutoImportResolver) { r.nuxtAutoImport = fn }

// A known binding consumes the call even if its implementation isn't indexed.
func (r *Resolver) tryResolveNuxtAutoImport(e *graph.Edge, name string, stats *ResolveStats) bool {
	if r.nuxtAutoImport == nil {
		return false
	}
	stem, exported := r.nuxtAutoImport(e.FilePath, name)
	if stem == "" {
		return false
	}
	var file string
	if n := probeJSTSFile(r.cachedGetNode, stem); n != nil {
		file = n.ID
	}
	r.resolveExactCall(e, name, stem, file, exported, graph.JSPendingGenerated, "nuxt_auto_import", stats)
	return true
}
