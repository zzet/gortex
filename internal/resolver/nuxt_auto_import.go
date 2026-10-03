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
	for _, candidate := range candidates {
		if candidate.FilePath == e.FilePath && candidate.Kind != graph.KindImport {
			return false
		}
	}
	// Even an unresolved explicit binding shadows a generated global.
	for _, edge := range r.graph.GetOutEdges(e.FilePath) {
		if edge.Kind != graph.EdgeImports {
			continue
		}
		if edge.Alias == name {
			return false
		}
		target := r.importedTargetOf(edge)
		if n := r.cachedGetNode(target); n != nil && n.Kind != graph.KindFile && n.Name == name && edge.Alias == "" {
			return false
		}
		if graph.IsUnresolvedTarget(edge.To) {
			_, exported := splitImportSpecSymbol(strings.TrimPrefix(graph.UnresolvedName(edge.To), "import::"))
			if exported == name && edge.Alias == "" {
				return false
			}
		}
	}
	stem, exported := r.nuxtAutoImport(e.FilePath, name)
	if stem == "" || exported == "" {
		return false
	}
	file := probeJSTSFile(r.cachedGetNode, stem)
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
