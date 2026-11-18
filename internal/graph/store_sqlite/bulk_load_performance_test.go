//go:build performance

package store_sqlite

import (
	"os"
	"testing"
	"time"
)

// TestPerformanceBulkLoadPersistSpeed is the persist-speed evidence: it times the plain
// path vs the fast path on the same fixture and logs both. It asserts
// correctness and that the fast path is not pathologically slower; a strict
// speedup ratio is gated behind GORTEX_BULK_PERF_ASSERT so the default run
// stays deterministic on noisy CI.
func TestPerformanceBulkLoadPersistSpeed(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping persist-speed timing in -short")
	}
	n, e := 8000, 16000
	nodes, edges := bulkFixture(n, e)

	plain, _ := openTempStore(t)
	t0 := time.Now()
	plain.AddBatch(nodes, edges)
	plainDur := time.Since(t0)

	bulk, _ := openTempStore(t)
	t1 := time.Now()
	bulk.BeginBulkLoad()
	bulk.AddBatch(nodes, edges)
	if err := bulk.FlushBulk(); err != nil {
		t.Fatalf("FlushBulk: %v", err)
	}
	bulkDur := time.Since(t1)

	if bulk.NodeCount() != plain.NodeCount() || bulk.EdgeCount() != plain.EdgeCount() {
		t.Fatalf("count mismatch: bulk(%d,%d) plain(%d,%d)",
			bulk.NodeCount(), bulk.EdgeCount(), plain.NodeCount(), plain.EdgeCount())
	}
	integrityOK(t, bulk.db)

	ratio := float64(plainDur) / float64(bulkDur)
	t.Logf("persist %d nodes / %d edges: plain=%s bulk=%s speedup=%.2fx",
		n, e, plainDur, bulkDur, ratio)

	// Sanity floor: the fast path must never be dramatically slower.
	if bulkDur > plainDur*5 {
		t.Fatalf("fast path far slower: plain=%s bulk=%s", plainDur, bulkDur)
	}
	if os.Getenv("GORTEX_BULK_PERF_ASSERT") != "" && ratio < 2.0 {
		t.Fatalf("fast path speedup %.2fx below 2x target", ratio)
	}
}
