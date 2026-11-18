package indexer

import (
	"slices"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

// contractSparseDeclarationShape describes ONLY the sparse suffix's exact
// declaration answer and its hiding predicate over an unchanged prefix. It
// does not prove that prefix is unchanged, that a captured debt is covered, or
// that a full-node/semantic/edge answer can be reused. It owns data, not layers.
// A future current-job certificate must establish those separate conditions.
type contractSparseDeclarationShape struct {
	files map[string]bool
	ids   map[string]bool
	names map[string][]store_sqlite.ContractProjectionDeclaration
}

type contractSparseDeclarationLayer struct {
	files, context, ids map[string]bool
	rows                []store_sqlite.ContractProjectionDeclaration
}

// sparseDeclarationShape preserves the production exact-name visitor's
// overlay-first order. A multiset is insufficient when a fold changes order.
// Its input is bottom-to-top, with each checked physical footprint ordered by
// ID. It neither queries an unleased prefix nor expands a mask from stale rows.
func sparseDeclarationShape(footprints []store_sqlite.ContractProjectionFootprint) contractSparseDeclarationShape {
	out := contractSparseDeclarationShape{files: map[string]bool{}, ids: map[string]bool{}, names: map[string][]store_sqlite.ContractProjectionDeclaration{}}
	layers := make([]contractSparseDeclarationLayer, len(footprints))
	for i, footprint := range footprints {
		layer := contractSparseDeclarationLayer{files: map[string]bool{}, context: map[string]bool{}, ids: map[string]bool{}, rows: footprint.Declarations}
		for _, mask := range footprint.Files {
			switch mask.Mode {
			case store_sqlite.OwnershipReplace, store_sqlite.OwnershipDelete:
				layer.files[mask.FilePath], out.files[mask.FilePath] = true, true
			case store_sqlite.OwnershipContext:
				layer.context[mask.FilePath] = true
			}
		}
		rows := make(map[string]store_sqlite.ContractProjectionDeclaration, len(layer.rows))
		for _, row := range layer.rows {
			rows[row.ID] = row
		}
		for _, mask := range footprint.Identities {
			row, present := rows[mask.NodeID]
			// Match GenerationLayer's explicit identity ownership: legacy
			// always claims; a replacement at context-only path claims
			// nothing. An independent covered-file claim still claims the ID.
			if mask.Kind == store_sqlite.NodeIdentityMaskLegacy || present && (layer.files[row.FilePath] || !layer.context[row.FilePath]) {
				layer.ids[mask.NodeID], out.ids[mask.NodeID] = true, true
			}
		}
		layers[i] = layer
	}
	// Redundant ID markers covered by the same file predicate do not make a
	// logically identical sparse fold look like a visibility change.
	for id := range out.ids {
		if sparseFileOwnsID(out.files, id) {
			delete(out.ids, id)
		}
	}
	for i := len(layers) - 1; i >= 0; i-- {
		for _, row := range layers[i].rows {
			if layers[i].context[row.FilePath] {
				continue
			}
			visible := true
			for _, above := range layers[i+1:] {
				if sparseFileOwnsID(above.files, row.ID) || above.ids[row.ID] {
					visible = false
					break
				}
			}
			if visible {
				out.names[row.Name] = append(out.names[row.Name], row)
			}
		}
	}
	return out
}

func sparseFileOwnsID(files map[string]bool, id string) bool {
	if id == "" {
		return false
	}
	if path := graph.IDFile(id); path != "" {
		return files[path]
	}
	return files[id]
}

// sameNameProjection is conditional equality, NOT proof authority. Even two
// empty suffix answers are unequal here if a changed hiding predicate could
// reveal an inherited candidate: old positive owners cannot guard that case.
// A caller must still prove the same prefix and every prefix mutation interval,
// source/census/config/allowedRepo facts, and final publication versions.
func (s contractSparseDeclarationShape) sameNameProjection(other contractSparseDeclarationShape, name string) bool {
	if len(s.files) != len(other.files) || len(s.ids) != len(other.ids) {
		return false
	}
	for path := range s.files {
		if !other.files[path] {
			return false
		}
	}
	for id := range s.ids {
		if !other.ids[id] {
			return false
		}
	}
	return slices.Equal(s.names[name], other.names[name])
}
