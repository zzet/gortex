//go:build !gortex_edit_gate_seed

package store_sqlite

// A production build cannot select the deliberately bad test policy.
func editGateClosedSeedPolicy(walReclaimConfig) bool { return false }
