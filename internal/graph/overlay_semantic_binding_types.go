package graph

import "errors"

var ErrSemanticBindingProjectionUnsupported = errors.New("selected semantic binding projection unsupported")
var ErrSemanticBindingProjectionIncomplete = errors.New("selected semantic binding projection lacks accepted file inventory")

// OverlayLayerSemanticBindingTypeReader reads only compiler evidence owned by
// this immutable layer. Missing bindings in a replaced file shadow the base.
type OverlayLayerSemanticBindingTypeReader interface {
	LayerSemanticBindingTypes([]SemanticBindingSite) (map[SemanticBindingSite]string, error)
}

// SemanticBindingTypes follows the same file ownership as the selected graph.
// It never substitutes a mutable corpus or a process-global compiler reader.
func (v *OverlaidView) SemanticBindingTypes(sites []SemanticBindingSite) (map[SemanticBindingSite]string, error) {
	out := make(map[SemanticBindingSite]string)
	if len(sites) == 0 {
		return out, nil
	}
	seen := make(map[SemanticBindingSite]bool, len(sites))
	baseWanted := make(map[SemanticBindingSite]bool)
	var unique, inherited []SemanticBindingSite
	covered := false
	for _, site := range sites {
		if seen[site] {
			continue
		}
		seen[site] = true
		unique = append(unique, site)
		if v.layer != nil && v.layer.HasFile(site.FilePath) {
			covered = true
			continue
		}
		baseWanted[site] = true
		inherited = append(inherited, site)
	}
	if len(inherited) > 0 {
		reader, ok := v.base.(SemanticBindingTypeReader)
		if !ok {
			return nil, ErrSemanticBindingProjectionUnsupported
		}
		rows, err := reader.SemanticBindingTypes(inherited)
		if err != nil {
			return nil, err
		}
		for site, value := range rows {
			if !baseWanted[site] {
				return nil, ErrSemanticBindingProjectionIncomplete
			}
			out[site] = value
		}
	}
	if reader, ok := v.layer.(OverlayLayerSemanticBindingTypeReader); ok {
		// Include tombstoned/unclaimed sites so persisted layers can validate
		// their cached ownership revision even when there is no own SQL read.
		rows, err := reader.LayerSemanticBindingTypes(unique)
		if err != nil {
			return nil, err
		}
		for site, value := range rows {
			if !seen[site] || !v.layer.HasFile(site.FilePath) || v.layer.IsTombstone(site.FilePath) {
				return nil, ErrSemanticBindingProjectionIncomplete
			}
			out[site] = value
		}
	} else if covered {
		return nil, ErrSemanticBindingProjectionUnsupported
	}
	return out, nil
}
