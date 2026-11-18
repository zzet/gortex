package resolver

import (
	"strings"

	"github.com/zzet/gortex/internal/graph"
)

// resolveVueTemplateImport pins template names to default imports. Bare packages
// stay unresolved; unavailable explicit Vue paths cannot fall back by name.
func (r *Resolver) resolveVueTemplateImport(e *graph.Edge, name string, stats *ResolveStats) bool {
	matched, target, invalid := false, "", false
	importedPath := ""
	raw, _ := e.Meta["template_name"].(string)
	for _, imp := range r.graph.GetOutEdges(e.FilePath) {
		local, _ := imp.Meta["vue_default_import"].(string)
		if imp.Kind != graph.EdgeImports || local == "" || (local != name && local != raw) {
			continue
		}
		matched = true
		path, _, _ := importBindingOf(imp)
		path = strings.TrimPrefix(path, "external::")
		if importedPath != "" && importedPath != path {
			invalid = true
		}
		importedPath = path
		file := path
		if n := r.cachedGetNode(file); n == nil || n.Kind != graph.KindFile {
			file = resolveJSTSImportTarget(r.cachedGetNode, r.pathAlias, e.FilePath, path)
		}
		id := ""
		if file != "" {
			for _, symbol := range []string{local, "default", importPathBasename(file)} {
				if n := r.cachedGetNode(file + "::" + symbol); n != nil {
					id = n.ID
					break
				}
			}
			if id == "" {
				invalid = true
			}
		} else if strings.HasPrefix(path, ".") || strings.HasPrefix(path, "@/") || strings.HasPrefix(path, "~/") {
			// Vite/Nuxt aliases may not be known to the JS/TS path resolver. Narrow
			// the legacy search to Vue candidates whose path has the imported tail.
			tail := strings.TrimPrefix(strings.TrimPrefix(strings.TrimPrefix(path, "./"), "@/"), "~/")
			tail = strings.TrimSuffix(tail, ".vue") + ".vue"
			for _, n := range r.cachedFindNodesByNameInRepoForEdge(importPathBasename(path), r.callerRepoPrefix(e), e) {
				if n.Language != "vue" || n.Meta["component"] != true || (n.FilePath != tail && !strings.HasSuffix(n.FilePath, "/"+tail)) {
					continue
				}
				if id != "" && id != n.ID {
					invalid = true
				}
				id = n.ID
			}
			if id == "" && strings.HasSuffix(path, ".vue") {
				invalid = true
			}
		} else {
			invalid = true
		}
		if target != "" && target != id {
			invalid = true
		}
		target = id
	}
	if !matched || (!invalid && target == "") {
		return false
	}
	if invalid || target == "" {
		stats.Unresolved++
		return true
	}
	e.To = target
	e.Origin = graph.OriginASTResolved
	e.Confidence = 0.95
	e.Meta["resolution"] = "import_binding"
	stats.Resolved++
	return true
}
