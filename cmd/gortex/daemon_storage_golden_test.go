package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

// Exercise the actual status constructor and shared wire mapper. Fixed physical
// observations make changes to field names, units, nulls and histogram buckets
// reviewable independently from filesystem sizes or scheduling.
type storageGoldenReporter struct{ graph.Store }

func (*storageGoldenReporter) DBStats() (int64, int64) { return 1024, 2048 }
func (*storageGoldenReporter) CloseCheckpointEstimate() (time.Duration, int64) {
	return 15 * time.Second, 7
}
func (*storageGoldenReporter) WALPassiveStats() store_sqlite.WALPassiveStats {
	return store_sqlite.WALPassiveStats{Attempts: 5, Complete: 1, Incomplete: 2, ZeroProgress: 1, DeferredBuildLane: 3, LongestBuildLaneDeferral: 1500 * time.Millisecond, RefusedLease: 1, RefusedInflight: 2, StoodAsideReclaim: 3, YieldCancelled: 1, Failed: 1, FramesBackfilledTotal: 17, FramesUnobserved: 1, LastOKAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), LastOutcome: "yield_cancelled"}
}
func (*storageGoldenReporter) WALReclaimStats() store_sqlite.WALReclaimStats {
	hold := store_sqlite.DurationHistogram{Total: 75 * time.Millisecond, Max: 50 * time.Millisecond}
	hold.Counts[5] = 2
	wait := store_sqlite.DurationHistogram{Total: time.Millisecond, Max: time.Millisecond}
	wait.Counts[0] = 1
	drain := store_sqlite.DurationHistogram{Total: 2 * time.Millisecond, Max: 2 * time.Millisecond}
	drain.Counts[1] = 1
	reset := store_sqlite.DurationHistogram{Total: time.Millisecond, Max: time.Millisecond}
	reset.Counts[0] = 1
	return store_sqlite.WALReclaimStats{ThresholdBytes: 256 << 20, Attempts: 3, UrgentAttempts: 2, Resets: 1, CancelledWriterParked: 2, CancelledWriteIntent: 1, CancelledOther: 1, ObservedPeakWALBytes: 512 << 20, WALPeakObserved: true, WriterHold: hold, EditBurstWriterHold: hold, DeferredEditBurst: 2, DeferredInteractiveRead: 1, BulkExceptionHolds: 1, HardCapExceptionHolds: 2, WriterWait: wait, PauseDrain: drain, PauseTruncate: reset, PauseCount: 1, PauseTotal: 3 * time.Millisecond, PauseLast: 3 * time.Millisecond, PauseMax: 3 * time.Millisecond}
}

func TestStatusStorageBlockGolden(t *testing.T) {
	got, err := json.MarshalIndent(storageStatusFor(&storageGoldenReporter{Store: graph.New()}), "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	got = append(got, '\n')
	path := filepath.Join("testdata", "storage-counters.json")
	if os.Getenv("UPDATE_RECLAIM_STATUS_GOLDEN") == "1" {
		if err := os.WriteFile(path, got, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatal("storage block changed; review its additive wire contract and regenerate the golden explicitly")
	}
}
