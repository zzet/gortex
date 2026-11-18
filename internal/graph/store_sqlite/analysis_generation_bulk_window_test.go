package store_sqlite

import (
	"testing"
	"time"

	"github.com/zzet/gortex/internal/graph"
)

// TestAnalysisAbortInsideGenerationBulkWindowDoesNotDeadlock pins the daemon
// hang where a whole-graph analysis pass lost its revision race to a build
// and aborted its half-written generation while that build's bulk window was
// open. The window pins the writer pool's only connection; the abort took
// writeMu and then asked the pool for a connection, so it waited forever for
// the window's owner, which in turn waited on writeMu to drain its shadow.
// The abort must reuse the pinned connection like every other analysis write.
func TestAnalysisAbortInsideGenerationBulkWindowDoesNotDeadlock(t *testing.T) {
	store, _ := openTempStore(t)
	generationID, accepted, err := store.BeginAnalysisGeneration(store.AnalysisMutationRevision(), graph.AnalysisGenerationHeader{FormatVersion: 1})
	requireAnalysisAccepted(t)(accepted, err)

	engaged, err := store.BeginGenerationBulkLoad(5)
	if err != nil || !engaged {
		t.Fatalf("BeginGenerationBulkLoad = (%v, %v), want engaged", engaged, err)
	}
	// Read before any other goroutine exists: the only way to unblock a
	// regressed abort that is parked on the pool while holding writeMu.
	pinned := store.bulkConn

	const bound = 10 * time.Second
	aborted := make(chan error, 1)
	go func() { aborted <- store.AbortAnalysisGeneration(generationID) }()
	select {
	case err := <-aborted:
		if err != nil {
			t.Fatalf("AbortAnalysisGeneration inside a bulk window: %v", err)
		}
	case <-time.After(bound):
		// Return the pinned connection to the pool so the parked abort can
		// finish and the store can close; the test has already failed.
		_ = pinned.Close()
		<-aborted
		t.Fatalf("AbortAnalysisGeneration blocked for %s behind the open generation bulk window", bound)
	}

	// The window's owner must still get the write gate and close its window.
	ended := make(chan error, 1)
	go func() { ended <- store.EndGenerationBulkLoad() }()
	select {
	case err := <-ended:
		if err != nil {
			t.Fatalf("EndGenerationBulkLoad: %v", err)
		}
	case <-time.After(bound):
		t.Fatalf("EndGenerationBulkLoad blocked for %s after the abort", bound)
	}

	var state int
	if err := store.db.QueryRow(`SELECT state FROM analysis_generations WHERE generation_id = ?`, generationID).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != analysisGenerationStale {
		t.Fatalf("aborted generation state = %d, want stale (%d)", state, analysisGenerationStale)
	}
}
