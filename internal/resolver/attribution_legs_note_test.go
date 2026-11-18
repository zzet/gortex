package resolver

import (
	"testing"
	"time"

	"go.uber.org/zap"
)

// A leg's sub-laps ride on the legs' record after the legs themselves.
func TestAttributionLegsCarryTheirSubLaps(t *testing.T) {
	legs := newAttributionLegs()
	legs.lap("rebind_receivers")
	legs.note(zap.Int64("rebind_receivers_file_node_reads", 1600), zap.Duration("rebind_receivers_outside_file_node_reads", 19*time.Second))
	got := map[string]bool{}
	for _, f := range legs.fields() {
		got[f.Key] = true
	}
	for _, k := range []string{"rebind_receivers", "rebind_receivers_faults", "rebind_receivers_file_node_reads", "rebind_receivers_outside_file_node_reads"} {
		if !got[k] {
			t.Fatalf("legs record lacks %s: %v", k, got)
		}
	}
	if fileNodeReadStatsOf := func() (int64, time.Duration) { return fileNodeReadStats(struct{}{}) }; true {
		if r, w := fileNodeReadStatsOf(); r != 0 || w != 0 {
			t.Fatalf("a graph without a counter reports %d reads, %v", r, w)
		}
	}
}
