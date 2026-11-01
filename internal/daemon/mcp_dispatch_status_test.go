package daemon

import (
	"errors"
	"fmt"
	"slices"
	"testing"

	"github.com/zzet/gortex/internal/viewmetrics"
)

func TestDispatchQueueWaitMetricBucketsMatchStatusBounds(t *testing.T) {
	declared := viewmetrics.LabelVocabularies(viewmetrics.MCPDispatchQueueWaitBucketsTotal)["bucket_ms"]
	if !slices.Equal(declared, mcpWaitBucketLabels[:]) {
		t.Fatalf("emitted buckets=%v declared=%v", mcpWaitBucketLabels, declared)
	}
	for i, bound := range mcpWaitBoundsMS {
		if want := fmt.Sprintf("%dms", bound); mcpWaitBucketLabels[i] != want {
			t.Fatalf("bucket %d label=%q status bound=%dms", i, mcpWaitBucketLabels[i], bound)
		}
	}
	if mcpWaitBucketLabels[len(mcpWaitBoundsMS)] != "inf" {
		t.Fatal("final metric bucket must retain values above the last status bound")
	}
}

func TestDispatchStatusCountsAdmittedAndRefusedQueueWaits(t *testing.T) {
	pool := testDispatchPool(1)
	before := viewmetrics.Read()
	holder := takeScheduled(t, pool, MCPDispatchFast, "holder")
	ctx := withMCPDispatchQueueWait(t.Context(), 0)
	_, err := pool.acquire(ctx, MCPDispatchFast, "refused")
	if !errors.Is(err, errMCPDispatchSaturated) {
		t.Fatalf("saturated result=%v", err)
	}
	status := pool.snapshot()
	lane := status.Lanes["fast"]
	if lane.InFlight != 1 || lane.QueueDepth != 0 || lane.Refusals != 1 || lane.QueueWait["admitted"].Count != 1 || lane.QueueWait["refused"].Count != 1 {
		t.Fatalf("lane status=%+v", lane)
	}
	for _, outcome := range []string{"admitted", "refused"} {
		histogram := lane.QueueWait[outcome]
		total := uint64(0)
		for _, count := range histogram.Buckets {
			total += count
		}
		if total != histogram.Count || len(histogram.BoundsMS)+1 != len(histogram.Buckets) {
			t.Fatalf("incomplete %s histogram=%+v", outcome, histogram)
		}
	}
	key := viewmetrics.MCPDispatchRefusalsTotal + "{lane=fast}"
	if delta := viewmetrics.Read().Counters[key] - before.Counters[key]; delta != 1 {
		t.Errorf("metrics refusal delta=%d", delta)
	}
	lane.QueueWait["admitted"].Buckets[0] = 999
	if pool.snapshot().Lanes["fast"].QueueWait["admitted"].Buckets[0] == 999 {
		t.Fatal("status exposes mutable histogram storage")
	}
	holder.complete()
	if pool.snapshot().Lanes["fast"].InFlight != 0 {
		t.Fatal("completed dispatch still in status")
	}
}

func TestDispatchStatusTracksReservedUseAcrossSharedMutators(t *testing.T) {
	pool := testDispatchPool(3)
	a := takeScheduled(t, pool, MCPDispatchMutating, "editor")
	b := takeScheduled(t, pool, MCPDispatchMutating, "editor")
	c := takeScheduled(t, pool, MCPDispatchMutating, "editor")
	status := pool.snapshot()
	if status.ReservedSlotsUsed != 2 || status.ReservedAdmissions != 2 || status.Lanes["mutating"].InFlight != 3 {
		t.Fatalf("reserved/shared snapshot=%+v", status)
	}
	a.complete()
	if pool.snapshot().ReservedSlotsUsed != 2 {
		t.Fatal("remaining shared mutators should still occupy both reservations")
	}
	b.complete()
	if pool.snapshot().ReservedSlotsUsed != 1 {
		t.Fatal("reservation did not follow remaining mutator")
	}
	c.complete()
	if pool.snapshot().ReservedSlotsUsed != 0 {
		t.Fatal("reserved capacity leaked")
	}
}
