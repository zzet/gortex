//go:build !gortex_edit_gate_seed

package store_sqlite

// The deliberately bad reclaim policy is absent from production builds.
func editGateClosedReclaimSeed(*Store) {}
