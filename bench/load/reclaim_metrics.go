package main

import (
	"bytes"
	"encoding/json"
	"math"
	"slices"
)

// reclaimMetrics reads the WAL reclaim's counters out of the first and last
// daemon status receipts of a phase and reports what the window between them
// cost: how long reclaims held the writer and how long writers waited for
// them, how often the read gate closed and for how long in each stage, and
// how many attempts ran urgent or gave way to a write. A counter the daemon
// does not report comes out unavailable, never zero.
func reclaimMetrics(receipts []json.RawMessage) map[string]Metric {
	out := make(map[string]Metric)
	if len(receipts) < 2 {
		for name, unit := range reclaimMetricUnits {
			out[name] = unavailable(unit, "fewer than two daemon status receipts")
		}
		return out
	}
	before, after := decodeReclaimCounters(receipts[0]), decodeReclaimCounters(receipts[len(receipts)-1])
	if before == nil || after == nil {
		for name, unit := range reclaimMetricUnits {
			out[name] = unavailable(unit, "daemon status has no storage.wal_reclaim")
		}
		return out
	}

	counter := func(name, field string, read func(*reclaimCounters) *float64) {
		unit := reclaimMetricUnits[name]
		if b, a := read(before), read(after); b != nil && a != nil {
			out[name] = counterDelta(measured(*b, unit), measured(*a, unit))
		} else {
			out[name] = unavailable(unit, "daemon status has no wal_reclaim."+field)
		}
	}
	counter("reclaim_gate_closures", "pause_count", func(c *reclaimCounters) *float64 { return c.PauseCount })
	counter("reclaim_urgent_attempts", "urgent_attempts", func(c *reclaimCounters) *float64 { return c.UrgentAttempts })
	counter("reclaim_write_intent_cancels", "cancelled_write_intent", func(c *reclaimCounters) *float64 {
		if c.CancelledWriterParked == nil || c.CancelledWriteIntent == nil {
			return nil
		}
		sum := *c.CancelledWriterParked + *c.CancelledWriteIntent
		return &sum
	})
	counter("reclaim_pause_drain_ms", "pause_drain", func(c *reclaimCounters) *float64 { return c.PauseDrain.total() })
	counter("reclaim_pause_truncate_ms", "pause_truncate", func(c *reclaimCounters) *float64 { return c.PauseTruncate.total() })

	histogram := func(prefix, field string, b, a *reclaimHistogram) {
		count, p95, peak := prefix+"s", prefix+"_p95_ms", prefix+"_max_ms"
		if b == nil || a == nil {
			reason := "daemon status has no wal_reclaim." + field
			out[count], out[p95], out[peak] = unavailable(reclaimMetricUnits[count], reason), unavailable("ms", reason), unavailable("ms", reason)
			return
		}
		delta, ok := a.minus(b)
		if !ok {
			reason := "wal_reclaim." + field + " buckets changed shape or went backwards"
			out[count], out[p95], out[peak] = unavailable(reclaimMetricUnits[count], reason), unavailable("ms", reason), unavailable("ms", reason)
			return
		}
		n := 0.0
		for _, c := range delta {
			n += c
		}
		out[count] = measured(n, reclaimMetricUnits[count])
		if n == 0 {
			reason := "no " + reclaimMetricUnits[count] + " in the window"
			out[p95], out[peak] = unavailable("ms", reason), unavailable("ms", reason)
			return
		}
		// The status maximum is since the store opened. It is the window's
		// own only when the window raised it; otherwise the highest bucket
		// the window reached bounds it.
		top := a.MaxMS
		if a.MaxMS <= b.MaxMS {
			last := len(delta) - 1
			for delta[last] == 0 {
				last--
			}
			top = min(a.edge(last), a.MaxMS)
		}
		out[p95] = measured(min(a.edge(quantileBucket(delta, n, 0.95)), top), "ms")
		out[peak] = measured(top, "ms")
	}
	histogram("reclaim_writer_hold", "writer_hold", before.WriterHold, after.WriterHold)
	histogram("reclaim_writer_wait", "writer_wait", before.WriterWait, after.WriterWait)
	const burstPrefix = "reclaim_edit_burst_writer_hold"
	if before.pid <= 0 || before.pid != after.pid {
		for _, name := range reclaimBurstMetricNames {
			out[name] = unavailable(reclaimMetricUnits[name], "burst window has no stable daemon pid")
		}
		return out
	}
	for _, c := range []struct {
		name          string
		before, after *int64
	}{
		{"reclaim_edit_burst_gate_closures", before.EditBurstGateClosures, after.EditBurstGateClosures},
		{"reclaim_deferred_edit_burst", before.DeferredEditBurst, after.DeferredEditBurst},
		{"reclaim_deferred_interactive_read", before.DeferredInteractiveRead, after.DeferredInteractiveRead},
		{"reclaim_bulk_exception_holds", before.BulkExceptionHolds, after.BulkExceptionHolds},
		{"reclaim_hard_cap_exception_holds", before.HardCapExceptionHolds, after.HardCapExceptionHolds},
	} {
		unit := reclaimMetricUnits[c.name]
		if c.before == nil || c.after == nil {
			out[c.name] = unavailable(unit, "daemon status has no wal_reclaim."+c.name[len("reclaim_"):])
		} else if *c.before < 0 || *c.after < 0 {
			out[c.name] = unavailable(unit, "invalid negative reclaim counter")
		} else {
			out[c.name] = counterDelta(measured(float64(*c.before), unit), measured(float64(*c.after), unit))
		}
	}
	b, a, reason := reclaimBurstHistograms(before.EditBurstWriterHold, after.EditBurstWriterHold)
	if reason != "" {
		for _, name := range []string{burstPrefix + "s", burstPrefix + "_p95_ms", burstPrefix + "_max_ms"} {
			out[name] = unavailable(reclaimMetricUnits[name], reason)
		}
	} else {
		histogram(burstPrefix, "edit_burst_writer_hold", b, a)
	}
	return out
}

var reclaimBurstMetricNames = []string{
	"reclaim_edit_burst_writer_holds", "reclaim_edit_burst_writer_hold_p95_ms", "reclaim_edit_burst_writer_hold_max_ms",
	"reclaim_edit_burst_gate_closures", "reclaim_deferred_edit_burst", "reclaim_deferred_interactive_read",
	"reclaim_bulk_exception_holds", "reclaim_hard_cap_exception_holds",
}

// reclaimBurstHistograms distinguishes an explicitly unobserved producer from
// an older daemon missing the capability. Only explicit null is an empty
// since-open population; a measured population cannot later become null.
func reclaimBurstHistograms(before, after json.RawMessage) (*reclaimHistogram, *reclaimHistogram, string) {
	const field = "wal_reclaim.edit_burst_writer_hold"
	if len(before) == 0 || len(after) == 0 {
		return nil, nil, "daemon status has no " + field
	}
	decode := func(raw json.RawMessage) (*reclaimHistogram, bool) {
		if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			return nil, true
		}
		var h reclaimHistogram
		if json.Unmarshal(raw, &h) != nil || !h.validBurstPopulation() {
			return nil, false
		}
		return &h, true
	}
	b, bOK := decode(before)
	a, aOK := decode(after)
	if !bOK || !aOK {
		return nil, nil, "invalid " + field + " population"
	}
	if a == nil && b != nil {
		return nil, nil, field + " went backwards to unobserved"
	}
	if a == nil {
		a = &reclaimHistogram{BucketCounts: []float64{0}}
	}
	if b == nil {
		b = &reclaimHistogram{BucketLEMS: slices.Clone(a.BucketLEMS), BucketCounts: make([]float64, len(a.BucketCounts))}
	}
	return b, a, ""
}

func (h *reclaimHistogram) validBurstPopulation() bool {
	if h.Count == nil || len(h.BucketLEMS) == 0 || len(h.BucketCounts) != len(h.BucketLEMS)+1 || h.MaxMS < 0 {
		return false
	}
	for i, edge := range h.BucketLEMS {
		if edge <= 0 || i > 0 && edge <= h.BucketLEMS[i-1] {
			return false
		}
	}
	n := 0.0
	for i, c := range h.BucketCounts {
		if c < 0 || math.Trunc(c) != c {
			return false
		}
		if c > 0 && i > 0 && h.MaxMS <= h.BucketLEMS[i-1] {
			return false
		}
		n += c
	}
	return n == float64(*h.Count)
}

// reclaimMetricUnits names every metric reclaimMetrics reports, with its unit.
var reclaimMetricUnits = map[string]string{
	"reclaim_edit_burst_writer_holds":       "holds",
	"reclaim_edit_burst_writer_hold_p95_ms": "ms",
	"reclaim_edit_burst_writer_hold_max_ms": "ms",
	"reclaim_edit_burst_gate_closures":      "closures",
	"reclaim_deferred_edit_burst":           "attempts",
	"reclaim_deferred_interactive_read":     "attempts",
	"reclaim_bulk_exception_holds":          "holds",
	"reclaim_hard_cap_exception_holds":      "holds",
	"reclaim_gate_closures":                 "closures",
	"reclaim_urgent_attempts":               "attempts",
	"reclaim_write_intent_cancels":          "attempts",
	"reclaim_pause_drain_ms":                "ms",
	"reclaim_pause_truncate_ms":             "ms",
	"reclaim_writer_holds":                  "holds",
	"reclaim_writer_hold_p95_ms":            "ms",
	"reclaim_writer_hold_max_ms":            "ms",
	"reclaim_writer_waits":                  "waits",
	"reclaim_writer_wait_p95_ms":            "ms",
	"reclaim_writer_wait_max_ms":            "ms",
}

// reclaimCounters is the part of a daemon status receipt's storage.wal_reclaim
// block these metrics read. Every field is optional: older daemons report a
// subset, and v0.64.10 reports no block at all.
type reclaimCounters struct {
	pid                     int
	EditBurstWriterHold     json.RawMessage    `json:"edit_burst_writer_hold"`
	EditBurstGateClosures   *int64             `json:"edit_burst_gate_closures"`
	DeferredEditBurst       *int64             `json:"deferred_edit_burst"`
	DeferredInteractiveRead *int64             `json:"deferred_interactive_read"`
	BulkExceptionHolds      *int64             `json:"bulk_exception_holds"`
	HardCapExceptionHolds   *int64             `json:"hard_cap_exception_holds"`
	PauseCount              *float64           `json:"pause_count"`
	UrgentAttempts          *float64           `json:"urgent_attempts"`
	CancelledWriterParked   *float64           `json:"cancelled_writer_parked"`
	CancelledWriteIntent    *float64           `json:"cancelled_write_intent"`
	PauseDrain              *reclaimPauseStage `json:"pause_drain"`
	PauseTruncate           *reclaimPauseStage `json:"pause_truncate"`
	WriterHold              *reclaimHistogram  `json:"writer_hold"`
	WriterWait              *reclaimHistogram  `json:"writer_wait"`
}

func decodeReclaimCounters(receipt json.RawMessage) *reclaimCounters {
	var status struct {
		PID     int `json:"pid"`
		Storage *struct {
			WALReclaim *reclaimCounters `json:"wal_reclaim"`
		} `json:"storage"`
	}
	if json.Unmarshal(receipt, &status) != nil || status.Storage == nil {
		return nil
	}
	if status.Storage.WALReclaim != nil {
		status.Storage.WALReclaim.pid = status.PID
	}
	return status.Storage.WALReclaim
}

// reclaimPauseStage is one stage of the time the read gate was closed.
type reclaimPauseStage struct {
	TotalMS float64 `json:"total_ms"`
}

func (s *reclaimPauseStage) total() *float64 {
	if s == nil {
		return nil
	}
	return &s.TotalMS
}

// reclaimHistogram is a duration histogram on the wire. BucketCounts[i] holds
// the samples at most BucketLEMS[i] and above the edge before it; one extra
// last count holds the samples above every edge, which MaxMS bounds.
type reclaimHistogram struct {
	Count        *int64    `json:"count"`
	BucketLEMS   []float64 `json:"bucket_le_ms"`
	BucketCounts []float64 `json:"bucket_counts"`
	MaxMS        float64   `json:"max_ms"`
}

// minus returns the per-bucket counts h gained since earlier, or false when
// the two are not the same buckets or a count went backwards (a restart).
func (h *reclaimHistogram) minus(earlier *reclaimHistogram) ([]float64, bool) {
	if len(h.BucketCounts) != len(h.BucketLEMS)+1 || !slices.Equal(h.BucketLEMS, earlier.BucketLEMS) || len(earlier.BucketCounts) != len(h.BucketCounts) {
		return nil, false
	}
	delta := make([]float64, len(h.BucketCounts))
	for i := range delta {
		delta[i] = h.BucketCounts[i] - earlier.BucketCounts[i]
		if delta[i] < 0 {
			return nil, false
		}
	}
	return delta, true
}

// edge is bucket i's upper edge; MaxMS bounds the bucket above every edge.
func (h *reclaimHistogram) edge(i int) float64 {
	if i >= len(h.BucketLEMS) {
		return h.MaxMS
	}
	return h.BucketLEMS[i]
}

// quantileBucket is the index of the bucket holding the q-quantile of n
// samples.
func quantileBucket(counts []float64, n, q float64) int {
	rank, seen := max(math.Ceil(q*n), 1), 0.0
	for i, c := range counts {
		if seen += c; seen >= rank {
			return i
		}
	}
	return len(counts) - 1
}
