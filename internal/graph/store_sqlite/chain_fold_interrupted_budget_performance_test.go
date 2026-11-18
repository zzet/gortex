//go:build performance

package store_sqlite

import "testing"

// Preserve the original queued-writer wall-clock envelope in the existing
// performance flow; ordinary CI retains every interruption/parity assertion.
func TestPerformanceChainFoldInterruptedPagesYieldWriterLatency(t *testing.T) {
	runChainFoldInterruptedPagesReduceOnlyAfterTransactionWork(t, true)
}
