//go:build gortex_edit_gate_seed

package store_sqlite

import "os"

// Reuse the existing seed's exact selection fault. This is not a runtime
// policy switch: this implementation is absent from normal builds.
func editGateClosedSeedPolicy(cfg walReclaimConfig) bool {
	return cfg.thresholdBytes == -1 && cfg.readerWait == 0 && os.Getenv("GORTEX_EDIT_GATE_SEED") == "closed-read-gate"
}
