//go:build performance

package store_sqlite

import (
	"path/filepath"
	"testing"
	"time"
)

// Records what the window end costs over the same backlog with the deferral
// and with an inline PASSIVE that is allowed to copy all of it (the former
// window-end behaviour, bounded at the routine one-second window). Only the
// deferral's shape is asserted; the timings are logged.
func TestPerformanceGenerationWindowEndCostOverABacklog(t *testing.T) {
	for _, inline := range []bool{false, true} {
		name := "deferred"
		if inline {
			name = "inline"
		}
		t.Run(name, func(t *testing.T) {
			if inline {
				setGenerationInlineCheckpointMaxFrames(t, 1<<40)
				previous := generationInlineCheckpointWindow
				generationInlineCheckpointWindow = walPassiveCheckpointTimeout
				t.Cleanup(func() { generationInlineCheckpointWindow = previous })
			}
			path := filepath.Join(t.TempDir(), name+".sqlite")
			store, err := openPristine(t, path)
			if err != nil {
				t.Fatalf("open: %v", err)
			}
			defer func() { _ = store.Close() }()
			backlog := seedUncheckpointedBacklog(t, store, path, 16384, 32768)
			var passive, deferred int
			store.bulkFinalizeObserver = func(event bulkFinalizeEvent) {
				switch event.Stage {
				case "checkpoint_passive":
					passive++
				case "checkpoint_deferred":
					deferred++
				}
			}
			const generationID = int64(13)
			engaged, err := store.BeginGenerationBulkLoad(generationID)
			if err != nil || !engaged {
				t.Fatalf("BeginGenerationBulkLoad = (%v, %v)", engaged, err)
			}
			nodes, edges := bulkFixture(256, 512)
			if err := store.AtGeneration(generationID).AddBatchChecked(nodes, edges); err != nil {
				t.Fatalf("AddBatchChecked: %v", err)
			}
			started := time.Now()
			if err := store.EndGenerationBulkLoadFor(generationID); err != nil {
				t.Fatalf("EndGenerationBulkLoadFor: %v", err)
			}
			elapsed := time.Since(started)
			t.Logf("%s window end over a %d-frame backlog: %s (inline PASSIVE=%d deferred=%d)",
				name, backlog, elapsed, passive, deferred)
			if !inline && (passive != 0 || deferred != 1) {
				t.Fatalf("deferred window end ran %d inline PASSIVE and %d deferrals", passive, deferred)
			}
		})
	}
}
