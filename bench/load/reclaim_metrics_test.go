package main

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

// reclaimReceipt builds a daemon status receipt whose storage.wal_reclaim
// block is reclaim, encoded the way the daemon encodes it.
func reclaimReceipt(t *testing.T, reclaim map[string]any) json.RawMessage {
	t.Helper()
	data, err := json.Marshal(map[string]any{"pid": 1, "storage": map[string]any{"wal_bytes": 4096, "wal_reclaim": reclaim}})
	require.NoError(t, err)
	return data
}

var reclaimBucketsMS = []float64{1, 2, 5, 10, 20, 50, 100, 250, 500, 1000, 2000, 5000}

func reclaimBuckets(maxMS float64, counts map[int]int) map[string]any {
	out := make([]int, len(reclaimBucketsMS)+1)
	for i, c := range counts {
		out[i] = c
	}
	return map[string]any{"count": 0, "total_ms": 0, "max_ms": maxMS, "p50_ms": 0, "p95_ms": 0, "bucket_le_ms": reclaimBucketsMS, "bucket_counts": out}
}

func requireMetric(t *testing.T, metrics map[string]Metric, name string, want float64, unit string) {
	t.Helper()
	m, ok := metrics[name]
	require.Truef(t, ok, "%s missing", name)
	require.NotNilf(t, m.Value, "%s unavailable: %s", name, m.Reason)
	require.InDeltaf(t, want, *m.Value, 1e-9, name)
	require.Equal(t, unit, m.Unit, name)
}

func requireUnavailable(t *testing.T, metrics map[string]Metric, name, reason string) {
	t.Helper()
	m, ok := metrics[name]
	require.Truef(t, ok, "%s missing", name)
	require.Nilf(t, m.Value, "%s must be unavailable", name)
	require.Contains(t, m.Reason, reason, name)
}

func TestReclaimMetricsFromStatusReceipts(t *testing.T) {
	before := map[string]any{
		"pause_count": 3, "urgent_attempts": 1, "cancelled_writer_parked": 2, "cancelled_write_intent": 0,
		"pause_drain":    map[string]any{"count": 3, "total_ms": 100, "max_ms": 60},
		"pause_truncate": map[string]any{"count": 3, "total_ms": 50, "max_ms": 20},
		"writer_hold":    reclaimBuckets(9, map[int]int{3: 2}),
		"writer_wait":    reclaimBuckets(0, nil),
	}

	t.Run("the window between the first and last receipt", func(t *testing.T) {
		after := map[string]any{
			"pause_count": 5, "urgent_attempts": 4, "cancelled_writer_parked": 3, "cancelled_write_intent": 2,
			"pause_drain":    map[string]any{"count": 5, "total_ms": 350, "max_ms": 200},
			"pause_truncate": map[string]any{"count": 5, "total_ms": 60, "max_ms": 20},
			// 19 holds of at most 10 ms and one of 80 ms, a new maximum.
			"writer_hold": reclaimBuckets(80, map[int]int{3: 21, 6: 1}),
			"writer_wait": reclaimBuckets(15, map[int]int{4: 1}),
		}
		middle := reclaimReceipt(t, map[string]any{"pause_count": 1000})
		m := reclaimMetrics([]json.RawMessage{reclaimReceipt(t, before), middle, reclaimReceipt(t, after)})
		requireMetric(t, m, "reclaim_gate_closures", 2, "closures")
		requireMetric(t, m, "reclaim_urgent_attempts", 3, "attempts")
		requireMetric(t, m, "reclaim_write_intent_cancels", 3, "attempts")
		requireMetric(t, m, "reclaim_pause_drain_ms", 250, "ms")
		requireMetric(t, m, "reclaim_pause_truncate_ms", 10, "ms")
		requireMetric(t, m, "reclaim_writer_holds", 20, "holds")
		requireMetric(t, m, "reclaim_writer_hold_p95_ms", 10, "ms")
		requireMetric(t, m, "reclaim_writer_hold_max_ms", 80, "ms")
		requireMetric(t, m, "reclaim_writer_waits", 1, "waits")
		requireMetric(t, m, "reclaim_writer_wait_p95_ms", 15, "ms")
		requireMetric(t, m, "reclaim_writer_wait_max_ms", 15, "ms")
		require.Len(t, m, len(reclaimMetricUnits))
	})

	t.Run("a maximum the window did not raise is bounded by the highest bucket it reached", func(t *testing.T) {
		older := map[string]any{"writer_hold": reclaimBuckets(80, map[int]int{6: 1}), "writer_wait": reclaimBuckets(0, nil)}
		newer := map[string]any{"writer_hold": reclaimBuckets(80, map[int]int{3: 4, 6: 1}), "writer_wait": reclaimBuckets(0, nil)}
		m := reclaimMetrics([]json.RawMessage{reclaimReceipt(t, older), reclaimReceipt(t, newer)})
		requireMetric(t, m, "reclaim_writer_holds", 4, "holds")
		requireMetric(t, m, "reclaim_writer_hold_p95_ms", 10, "ms")
		requireMetric(t, m, "reclaim_writer_hold_max_ms", 10, "ms")
		// No reclaim held the writer while anyone queued: zero waits, and
		// no quantile of nothing.
		requireMetric(t, m, "reclaim_writer_waits", 0, "waits")
		requireUnavailable(t, m, "reclaim_writer_wait_p95_ms", "no waits in the window")
		requireUnavailable(t, m, "reclaim_writer_wait_max_ms", "no waits in the window")
	})

	t.Run("a daemon reporting only today's fields", func(t *testing.T) {
		m := reclaimMetrics([]json.RawMessage{
			reclaimReceipt(t, map[string]any{"pause_count": 3, "writer_hold_max_ms": 40}),
			reclaimReceipt(t, map[string]any{"pause_count": 3, "writer_hold_max_ms": 40}),
		})
		requireMetric(t, m, "reclaim_gate_closures", 0, "closures")
		requireUnavailable(t, m, "reclaim_urgent_attempts", "wal_reclaim.urgent_attempts")
		requireUnavailable(t, m, "reclaim_write_intent_cancels", "wal_reclaim.cancelled_write_intent")
		requireUnavailable(t, m, "reclaim_pause_drain_ms", "wal_reclaim.pause_drain")
		requireUnavailable(t, m, "reclaim_writer_holds", "wal_reclaim.writer_hold")
		requireUnavailable(t, m, "reclaim_writer_hold_p95_ms", "wal_reclaim.writer_hold")
		requireUnavailable(t, m, "reclaim_writer_wait_p95_ms", "wal_reclaim.writer_wait")
	})

	t.Run("a daemon without a reclaim, or too few receipts", func(t *testing.T) {
		bare, err := json.Marshal(map[string]any{"pid": 1, "storage": map[string]any{"wal_bytes": 4096}})
		require.NoError(t, err)
		for name, receipts := range map[string][]json.RawMessage{
			"daemon status has no storage.wal_reclaim": {bare, bare},
			"fewer than two daemon status receipts":    {reclaimReceipt(t, before)},
		} {
			m := reclaimMetrics(receipts)
			require.Len(t, m, len(reclaimMetricUnits))
			for metric := range reclaimMetricUnits {
				requireUnavailable(t, m, metric, name)
			}
		}
	})

	t.Run("a restart between the receipts", func(t *testing.T) {
		restarted := map[string]any{
			"pause_count": 1, "urgent_attempts": 0, "cancelled_writer_parked": 0, "cancelled_write_intent": 0,
			"writer_hold": reclaimBuckets(4, map[int]int{2: 1}),
			"writer_wait": map[string]any{"max_ms": 1, "bucket_le_ms": []float64{1, 2}, "bucket_counts": []int{1, 0, 0}},
		}
		m := reclaimMetrics([]json.RawMessage{reclaimReceipt(t, before), reclaimReceipt(t, restarted)})
		requireUnavailable(t, m, "reclaim_gate_closures", "counter reset")
		requireUnavailable(t, m, "reclaim_write_intent_cancels", "counter reset")
		requireUnavailable(t, m, "reclaim_writer_hold_p95_ms", "went backwards")
		requireUnavailable(t, m, "reclaim_writer_wait_p95_ms", "changed shape")
	})
}

// The nightly adapter consumes these exact public cumulative bucket fields.
// Unlike the older legacy fixtures, count here agrees with the raw population.
func TestReclaimHoldNightlyRawPopulationMatchesProducer(t *testing.T) {
	before := map[string]any{"count": int64(2), "bucket_le_ms": []int{10, 50, 100}, "bucket_counts": []int{2, 0, 0, 0}, "max_ms": 8}
	after := map[string]any{"count": int64(22), "bucket_le_ms": []int{10, 50, 100}, "bucket_counts": []int{21, 0, 1, 0}, "max_ms": 80}
	raw := []json.RawMessage{reclaimReceipt(t, map[string]any{"writer_hold": before}), reclaimReceipt(t, map[string]any{"writer_hold": after})}
	metrics := reclaimMetrics(raw)
	requireMetric(t, metrics, "reclaim_writer_hold_p95_ms", 10, "ms")
	requireMetric(t, metrics, "reclaim_writer_holds", 20, "holds")
}
