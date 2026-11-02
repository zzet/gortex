package main

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/daemon"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

func burstReceipt(t *testing.T, stats store_sqlite.WALReclaimStats) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(map[string]any{"pid": 1, "storage": map[string]any{"wal_reclaim": daemon.WALReclaimStatusFrom(stats)}})
	require.NoError(t, err)
	return b
}

func TestReclaimBurstMetricsActualProducer(t *testing.T) {
	before := store_sqlite.WALReclaimStats{EditBurstGateClosures: 2, DeferredEditBurst: 3, DeferredInteractiveRead: 4, BulkExceptionHolds: 1, HardCapExceptionHolds: 2}
	before.EditBurstWriterHold.Counts[6] = 1
	before.EditBurstWriterHold.Max = 80 * time.Millisecond
	after := before
	after.EditBurstWriterHold.Counts[5] = 20
	// An old80ms maximum must not turn the new20 burst holds into aggregate80ms.
	after.WriterHold.Counts[10] = 100
	after.WriterHold.Max = 2 * time.Second
	after.DeferredEditBurst += 5
	after.DeferredInteractiveRead += 7
	after.BulkExceptionHolds += 2
	after.HardCapExceptionHolds++
	m := reclaimMetrics([]json.RawMessage{burstReceipt(t, before), burstReceipt(t, after)})
	requireMetric(t, m, "reclaim_edit_burst_writer_holds", 20, "holds")
	requireMetric(t, m, "reclaim_edit_burst_writer_hold_p95_ms", 50, "ms")
	requireMetric(t, m, "reclaim_edit_burst_gate_closures", 0, "closures")
	requireMetric(t, m, "reclaim_deferred_edit_burst", 5, "attempts")
	requireMetric(t, m, "reclaim_deferred_interactive_read", 7, "attempts")
	requireMetric(t, m, "reclaim_bulk_exception_holds", 2, "holds")
	requireMetric(t, m, "reclaim_hard_cap_exception_holds", 1, "holds")
}

func TestReclaimBurstMetricsAvailability(t *testing.T) {
	empty := burstReceipt(t, store_sqlite.WALReclaimStats{}) // explicit producer null, not an absent capability
	measured := store_sqlite.WALReclaimStats{}
	measured.EditBurstWriterHold.Counts[5] = 1
	measured.EditBurstWriterHold.Max = 49 * time.Millisecond
	observed := burstReceipt(t, measured)
	t.Run("first actual sample after explicit unobserved snapshot", func(t *testing.T) {
		m := reclaimMetrics([]json.RawMessage{empty, observed})
		requireMetric(t, m, "reclaim_edit_burst_writer_holds", 1, "holds")
		requireMetric(t, m, "reclaim_edit_burst_writer_hold_p95_ms", 49, "ms")
	})
	t.Run("no observations is not a zero quantile", func(t *testing.T) {
		m := reclaimMetrics([]json.RawMessage{empty, empty})
		requireMetric(t, m, "reclaim_edit_burst_writer_holds", 0, "holds")
		requireUnavailable(t, m, "reclaim_edit_burst_writer_hold_p95_ms", "no holds")
	})
	t.Run("missing older capability is not explicit null", func(t *testing.T) {
		m := reclaimMetrics([]json.RawMessage{reclaimReceipt(t, map[string]any{}), observed})
		requireUnavailable(t, m, "reclaim_edit_burst_writer_holds", "edit_burst_writer_hold")
	})
	t.Run("observed population cannot disappear", func(t *testing.T) {
		m := reclaimMetrics([]json.RawMessage{observed, empty})
		requireUnavailable(t, m, "reclaim_edit_burst_writer_hold_p95_ms", "backwards")
	})
	t.Run("same cumulative sample is not window sample", func(t *testing.T) {
		m := reclaimMetrics([]json.RawMessage{observed, observed})
		requireMetric(t, m, "reclaim_edit_burst_writer_holds", 0, "holds")
		requireUnavailable(t, m, "reclaim_edit_burst_writer_hold_p95_ms", "no holds")
	})
}

func TestReclaimBurstMetricsRejectMalformedPopulation(t *testing.T) {
	for name, change := range map[string]func(map[string]any){
		"count mismatch":   func(h map[string]any) { h["count"] = 9 },
		"negative count":   func(h map[string]any) { h["bucket_counts"].([]int)[5] = -1 },
		"fractional count": func(h map[string]any) { h["bucket_counts"] = []float64{0, 0, 0, 0, 0, .5, 0, 0, 0, 0, 0, 0, 0} },
		"unordered edges": func(h map[string]any) {
			h["bucket_le_ms"] = []float64{1, 2, 5, 10, 50, 20, 100, 250, 500, 1000, 2000, 5000}
		},
		"max below occupied bucket": func(h map[string]any) { h["max_ms"] = 1 },
	} {
		t.Run(name, func(t *testing.T) {
			h := reclaimBuckets(49, map[int]int{5: 1})
			h["count"] = 1
			change(h)
			m := reclaimMetrics([]json.RawMessage{burstReceipt(t, store_sqlite.WALReclaimStats{}), reclaimReceipt(t, map[string]any{"edit_burst_writer_hold": h})})
			requireUnavailable(t, m, "reclaim_edit_burst_writer_hold_p95_ms", "invalid")
		})
	}
}

func TestReclaimBurstMetricsRejectChangedIdentity(t *testing.T) {
	before := burstReceipt(t, store_sqlite.WALReclaimStats{})
	var changed map[string]any
	require.NoError(t, json.Unmarshal(before, &changed))
	changed["pid"] = 2
	after, err := json.Marshal(changed)
	require.NoError(t, err)
	m := reclaimMetrics([]json.RawMessage{before, after})
	for _, name := range []string{"reclaim_edit_burst_writer_holds", "reclaim_edit_burst_gate_closures", "reclaim_deferred_interactive_read"} {
		requireUnavailable(t, m, name, "stable daemon pid")
	}
}

func TestReclaimBurstCounterCompleteness(t *testing.T) {
	for _, field := range []string{"edit_burst_gate_closures", "deferred_edit_burst", "deferred_interactive_read", "bulk_exception_holds", "hard_cap_exception_holds"} {
		t.Run(field, func(t *testing.T) {
			for _, tc := range []struct {
				name          string
				before, after map[string]any
				reason        string
			}{
				{"missing", map[string]any{}, map[string]any{field: 1}, field},
				{"negative", map[string]any{field: -1}, map[string]any{field: 0}, "negative"},
				{"reset", map[string]any{field: 2}, map[string]any{field: 1}, "counter reset"},
			} {
				t.Run(tc.name, func(t *testing.T) {
					m := reclaimMetrics([]json.RawMessage{reclaimReceipt(t, tc.before), reclaimReceipt(t, tc.after)})
					requireUnavailable(t, m, "reclaim_"+field, tc.reason)
				})
			}
		})
	}
}

func TestReclaimBurstMetricsRequiresObservedMaximum(t *testing.T) {
	for _, tc := range []struct {
		name     string
		max      any
		present  bool
		observed bool
	}{
		{name: "missing maximum", present: false},
		{name: "null maximum", present: true, max: nil},
		{name: "maximum above last occupied finite bucket", present: true, max: 1000},
		{name: "genuine measured zero", present: true, max: 0, observed: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := reclaimBuckets(0, map[int]int{0: 1})
			h["count"] = 1
			delete(h, "max_ms")
			if tc.present {
				h["max_ms"] = tc.max
			}
			m := reclaimMetrics([]json.RawMessage{burstReceipt(t, store_sqlite.WALReclaimStats{}), reclaimReceipt(t, map[string]any{"edit_burst_writer_hold": h})})
			if tc.observed {
				requireMetric(t, m, "reclaim_edit_burst_writer_holds", 1, "holds")
				requireMetric(t, m, "reclaim_edit_burst_writer_hold_p95_ms", 0, "ms")
			} else {
				requireUnavailable(t, m, "reclaim_edit_burst_writer_hold_p95_ms", "invalid")
			}
		})
	}
}
