package resolver

import (
	"github.com/zzet/gortex/internal/graph"
)

// NuxtAutoImportResolver returns a generated app binding's source stem and
// original export, or empty strings outside a prepared Nuxt app's scope.
type NuxtAutoImportResolver func(callerFile, name string) (stem, exportedName string)

// SetNuxtAutoImportResolver installs generated bindings before a resolve pass.
func (r *Resolver) SetNuxtAutoImportResolver(fn NuxtAutoImportResolver) { r.nuxtAutoImport = fn }

// SetNuxtIncomingNames installs the reverse side of the generated bindings.
func (r *Resolver) SetNuxtIncomingNames(visit func(string, func(string))) {
	r.nuxtIncomingNames = visit
}

func (r *Resolver) appendNuxtIncomingKeys(keys []string, file string, nodes []*graph.Node) []string {
	if r.nuxtIncomingNames == nil || !isJSTSPath(file) || len(nodes) == 0 {
		return keys
	}
	r.nuxtIncomingNames(file, func(name string) {
		keys = append(keys, graph.UnresolvedNameCandidateIDsForName(name, nodes[0].RepoPrefix)...)
	})
	return keys
}

func (r *Resolver) extraIncomingKeys(paths []string, nodes map[string][]*graph.Node) []string {
	keys := r.vanishedDeclarationKeys(paths, nodes)
	for _, file := range paths {
		keys = r.appendNuxtIncomingKeys(keys, file, nodes[file])
	}
	return keys
}

// A known binding consumes the call even if its implementation isn't indexed.
func (r *Resolver) tryResolveNuxtAutoImport(e *graph.Edge, name string, stats *ResolveStats) bool {
	if r.nuxtAutoImport == nil {
		return false
	}
	stem, exported := r.nuxtAutoImport(e.FilePath, name)
	if stem == "" {
		return false
	}
	target := r.exportedCallee(probeJSTSFile(r.cachedGetNode, stem), exported)
	if target == nil {
		parkJSBinding(e, "nuxt", stats)
		return true
	}
	bindImportCall(e, target, "nuxt_auto_import", stats)
	e.Meta[graph.NuxtImportNameMeta] = name
	return true
}
