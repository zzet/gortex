package resolver

import (
	"strings"

	"github.com/zzet/gortex/internal/graph"
)

// NuxtAutoImportResolver returns a generated app binding's source stem and
// original export, or empty strings outside a prepared Nuxt app's scope.
type NuxtAutoImportResolver func(callerFile, name string) (stem, exportedName string)

// SetNuxtAutoImportResolver installs generated bindings before a resolve pass.
func (r *Resolver) SetNuxtAutoImportResolver(fn NuxtAutoImportResolver) { r.nuxtAutoImport = fn }

func (r *Resolver) tryResolveNuxtAutoImport(e *graph.Edge, name string, candidates []*graph.Node, stats *ResolveStats) bool {
	if r.nuxtAutoImport == nil || e.Kind != graph.EdgeCalls || !isJSTSPath(e.FilePath) || strings.Contains(name, ".") {
		return false
	}
	stem, exported := r.nuxtAutoImport(e.FilePath, name)
	if stem == "" || exported == "" || hasLocalDeclaration(e.FilePath, candidates) {
		return false
	}
	// Even an unresolved explicit binding shadows a generated global.
	for _, edge := range r.callerOutEdges(e.FilePath) {
		if edge.Kind != graph.EdgeImports {
			continue
		}
		locals, _ := edge.Meta["import_bindings"].(string)
		for local := range strings.FieldsSeq(locals) {
			if local == name {
				// An external package binding cannot name a local neighbour
				// either. Leave it for external resolution, not name heuristics.
				if r.importedTargetOf(edge) == "" {
					stats.Unresolved++
					return true
				}
				return false
			}
		}
	}
	file := probeJSTSFile(r.cachedGetNode, stem)
	if file == "" {
		return false
	}
	if exported == "default" {
		if local, _ := r.cachedGetNode(file).Meta["default_export"].(string); local != "" {
			exported = local
		}
	}
	candidate := r.cachedGetNode(file + "::" + exported)
	if candidate == nil || !isImportableCallee(candidate, exported) {
		return false
	}
	e.To = candidate.ID
	e.Origin = graph.OriginASTResolved
	e.Confidence = 0.9
	if e.Meta == nil {
		e.Meta = make(map[string]any)
	}
	e.Meta["resolution"] = "nuxt_auto_import"
	stats.Resolved++
	return true
}
