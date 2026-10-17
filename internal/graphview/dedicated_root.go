package graphview

import "github.com/zzet/gortex/internal/graph/store_sqlite"

// validateDedicatedFullRoot validates the oldest dedicated full root, whether
// it is directly requested or inherited through one or more positive deltas.
// That root must not inherit the mutable generation-zero corpus.
// Generation liveness is also checked by openGeneration before this helper.
// The binding comes from a keyed lookup; this helper does not discover roots,
// pin leases, inspect filesystem state, or change graph ownership.
func validateDedicatedFullRoot(row store_sqlite.ViewGeneration, binding store_sqlite.DedicatedGraph, graphID, repoPrefix string) error {
	if row.GenerationID <= 0 || row.BaseGenerationID != 0 || row.TreeOID == "" ||
		row.OwnerKind != "dedicated_graph" || row.GenerationKind != "dedicated" ||
		(row.State != store_sqlite.ViewGenerationReady && row.State != store_sqlite.ViewGenerationSuperseded) ||
		row.GraphID != graphID || binding.GraphID != graphID ||
		row.CheckoutID == "" || binding.OwnerCheckoutID != row.CheckoutID ||
		binding.RepoPrefix != repoPrefix {
		return NewViewError(CodeViewBuilding, "dedicated root generation does not match its graph owner, kind, or full-root identity")
	}
	return nil
}
