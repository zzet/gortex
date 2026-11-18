//go:build performance

package store_sqlite

import "testing"

// Preserve the wall-clock envelope of an edit interrupting a long fold step
// (20 ms; 100 ms under -race) in the performance flow; ordinary CI bounds the
// edit's wait by the step it interrupted and keeps every rollback and parity
// assertion.
func TestPerformanceChainFoldStepGivesWayToAnEditLatency(t *testing.T) {
	runChainFoldStepGivesWayToAnEdit(t, true)
}
