package indexer

import (
	"context"
	"fmt"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

// Derivation stamps: which version of each derived pass wrote a generation's
// rows (store_sqlite/derivation_stamps.go).
//
// A published generation is sealed, so rows an older derivation wrote stay in
// it for as long as it lives: capability rows derived by an older pass, and —
// the case that cost every delta its derived-pass plan — file rows written
// before the derived fingerprints were stamped on them. Every build records
// the versions it derived its rows with; a generation whose stamp is older
// than the running pass (or absent) is corrected once, in place, by the
// startup correction (generation_derived_correction.go).

const (
	// derivationPassCapability is the capability pass (accesses_field,
	// reads_env, executes_process rows).
	derivationPassCapability = "capability"
	// derivationPassFileFingerprints is the derived fingerprints stamped on
	// file rows (derived_invalidation.go).
	derivationPassFileFingerprints = "file_fingerprints"

	// capabilityDerivationVersion is bumped when the capability pass derives
	// different rows from the same input.
	capabilityDerivationVersion = 1
	// fileFingerprintDerivationVersion is bumped when the derived
	// fingerprints change meaning.
	fileFingerprintDerivationVersion = 1
)

// capabilityDerivedEdgeKinds are the rows the capability pass writes.
var capabilityDerivedEdgeKinds = []graph.EdgeKind{graph.EdgeAccessesField, graph.EdgeReadsEnv, graph.EdgeExecutesProcess}

// fileFingerprintCorrectionKind is the correction's edge-kind declaration for
// the fingerprint pass, which writes node rows only: no edge carries it, so a
// fingerprint correction can never delete or insert an edge.
const fileFingerprintCorrectionKind graph.EdgeKind = "derived_file_fingerprints"

// currentDerivationStamps is the running version of every stamped pass.
func currentDerivationStamps() map[string]int {
	return map[string]int{
		derivationPassCapability:       capabilityDerivationVersion,
		derivationPassFileFingerprints: fileFingerprintDerivationVersion,
	}
}

// derivationStampsForBuild is what a build stamps on the generation it
// publishes. A variable only so the in-package tests can build a generation
// an older derivation wrote.
var derivationStampsForBuild = currentDerivationStamps

// stampBuiltGeneration records, on a building generation, the versions its
// rows were derived with. It runs before publication (a published generation
// refuses the write).
func stampBuiltGeneration(ctx context.Context, handle *store_sqlite.Store) error {
	if handle == nil {
		return nil
	}
	stamps := derivationStampsForBuild()
	if len(stamps) == 0 {
		return nil
	}
	if err := handle.WriteDerivationStamps(ctx, stamps); err != nil {
		return fmt.Errorf("indexer: write derivation stamps: %w", err)
	}
	return nil
}

// stampFoldedGeneration records on a folded generation the oldest version
// each pass had across the chain it folds: its rows are theirs, so a pass one
// of them was not stamped for is left unstamped (read as version 0) and the
// correction re-derives it.
func stampFoldedGeneration(ctx context.Context, store *store_sqlite.Store, chain []int64, handle *store_sqlite.Store) error {
	if store == nil || handle == nil || len(chain) == 0 {
		return nil
	}
	folded := currentDerivationStamps()
	for _, generationID := range chain {
		stamps, err := store.AtGeneration(generationID).DerivationStamps(ctx)
		if err != nil {
			return fmt.Errorf("indexer: read derivation stamps of generation %d: %w", generationID, err)
		}
		for pass, version := range folded {
			if got := stamps[pass]; got < version {
				folded[pass] = got
			}
		}
	}
	for pass, version := range folded {
		if version < 1 {
			delete(folded, pass)
		}
	}
	if len(folded) == 0 {
		return nil
	}
	if err := handle.WriteDerivationStamps(ctx, folded); err != nil {
		return fmt.Errorf("indexer: write derivation stamps of the folded generation: %w", err)
	}
	return nil
}
