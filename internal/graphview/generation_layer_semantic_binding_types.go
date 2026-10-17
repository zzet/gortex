package graphview

import (
	"context"

	"github.com/zzet/gortex/internal/graph"
)

var _ graph.OverlayLayerSemanticBindingTypeReader = (*GenerationLayer)(nil)

func (l *GenerationLayer) LayerSemanticBindingTypes(sites []graph.SemanticBindingSite) (map[graph.SemanticBindingSite]string, error) {
	ctx := context.Background()
	if err := l.checkContractInputRevision(ctx); err != nil {
		return nil, err
	}
	var owned []graph.SemanticBindingSite
	var files []graph.ConstantFileKey
	seen := make(map[graph.ConstantFileKey]bool)
	for _, site := range sites {
		if !l.HasFile(site.FilePath) || l.IsTombstone(site.FilePath) {
			continue
		}
		owned = append(owned, site)
		key := graph.ConstantFileKey{RepoPrefix: site.RepoPrefix, FilePath: site.FilePath}
		if !seen[key] {
			seen[key] = true
			files = append(files, key)
		}
	}
	// Reuse the existing bounded accepted-file projection. A mask alone
	// cannot certify that an empty compiler sidecar replaces the base. Its
	// empty projection also checks the handle lifetime for tombstoned sites.
	inventory, err := l.LayerConstantValueProjectionContext(ctx, nil, files)
	if err != nil {
		return nil, err
	}
	for _, file := range files {
		if _, ok := inventory.Files[file]; !ok {
			return nil, graph.ErrSemanticBindingProjectionIncomplete
		}
	}
	rows, err := l.handle.SemanticBindingTypes(owned)
	if err != nil {
		return nil, err
	}
	if err := l.checkContractInputRevision(ctx); err != nil {
		return nil, err
	}
	return rows, nil
}
