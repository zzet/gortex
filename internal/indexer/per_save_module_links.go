package indexer

import (
	"path/filepath"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/modules"
)

// relinkImportNodesToModules gives the re-parsed files' import nodes the
// depends_on_module edges a whole index gives them.
//
// A whole index links every import node to its dependency module once, when
// it reads the root manifests (extractOneModuleManifestSource ->
// modules.LinkImportsIn). A per-save evicts the re-parsed files' import nodes
// with every edge out of them and re-adds the nodes from the fresh extraction,
// but no pass of the save re-reads a manifest, so the edge was lost — for a
// surviving import and a newly added one alike. The same link is applied here
// to the fresh import nodes against the manifests as they are on disk now;
// LinkImportsIn is a pure function of (import path, manifest specs), so the row
// is the whole index's row. A manifest edit is refreshed by its own path.
func (idx *Indexer) relinkImportNodesToModules(nodes []*graph.Node) {
	if idx == nil || idx.rootPath == "" || !idx.config.Coverage.IsEnabled("modules") {
		return
	}
	var imports []*graph.Node
	for _, node := range nodes {
		if node != nil && node.Kind == graph.KindImport {
			imports = append(imports, node)
		}
	}
	if len(imports) == 0 {
		return
	}
	for _, m := range rootManifests() {
		src, err := idx.readFileContent(filepath.Join(idx.rootPath, m.path))
		if err != nil {
			continue
		}
		specs := m.parse(src)
		if len(specs) == 0 {
			continue
		}
		var own string
		if m.ownPathFromSrc != nil {
			own = m.ownPathFromSrc(src)
		}
		modules.LinkImportsIn(idx.graph, imports, specs, own)
	}
}
