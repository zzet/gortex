package main

import (
	"encoding/json"
	"errors"
	"slices"
)

const editLoopZeroHoldReason = "NOT_EXERCISED: complete same-PID owned-window counters, attempts and every histogram bucket prove zero writer holds; p95 remains null; the 50ms exercised-hold acceptance is still pending"

// Collection applicability is distinct from an observed latency. The existing
// owned capture census binds every raw sample to this phase and process.
func qualifyEditLoopZeroHolds(p phase) error {
	if _, err := validateOwnedStatusCensus(p); err != nil {
		return err
	}
	var first holdHistogram
	var attempts int64
	for i, raw := range p.Status {
		h, err := decodeHoldHistogram(raw)
		if err != nil {
			return err
		}
		var status struct {
			Storage struct {
				Reclaim struct {
					Attempts *int64 `json:"attempts"`
				} `json:"wal_reclaim"`
			} `json:"storage"`
		}
		if json.Unmarshal(raw, &status) != nil || status.Storage.Reclaim.Attempts == nil || *status.Storage.Reclaim.Attempts < 0 || *status.Storage.Reclaim.Attempts > 1<<53 {
			return errors.New("missing or invalid measured-window reclaim attempts")
		}
		if i == 0 {
			first = h
			attempts = *status.Storage.Reclaim.Attempts
			continue
		}
		if h.count != first.count || h.maximum != first.maximum || !slices.Equal(h.edges, first.edges) || !slices.Equal(h.counts, first.counts) || *status.Storage.Reclaim.Attempts != attempts {
			return errors.New("writer holds, buckets or reclaim attempts changed during owned window")
		}
	}
	count, err := number(p.Metrics["reclaim_writer_holds"], "holds")
	if err != nil || count != 0 {
		return errors.New("zero raw hold window disagrees with producer count")
	}
	for _, name := range []string{"reclaim_writer_hold_p95_ms", "reclaim_writer_hold_max_ms"} {
		m, ok := p.Metrics[name]
		if !ok || m.Value != nil || m.Unit != "ms" || m.Reason != "no holds in the window" {
			return errors.New("zero hold window lacks original null quantile/maximum evidence")
		}
	}
	return nil
}

func assessEditLoopHoldSample(row editLoopRow, b budget, p phase) editLoopRow {
	if count, err := number(p.Metrics["reclaim_writer_holds"], "holds"); err == nil && count == 0 {
		if err := qualifyEditLoopZeroHolds(p); err != nil {
			row.Reason = err.Error()
			return row
		}
		row.State = "NOT_EXERCISED"
		row.Reason = editLoopZeroHoldReason
		return row
	}
	value, err := measureEditLoopRow(p, b.Metric)
	if err != nil {
		row.Reason = err.Error()
		if qualifyEditLoopZeroHolds(p) == nil {
			row.State = "NOT_EXERCISED"
			row.Reason = editLoopZeroHoldReason
		}
		return row
	}
	if b.Target == nil {
		row.Reason = "budget target unavailable"
		return row
	}
	row.Value = &value
	if row.Baseline != nil {
		delta := value - *row.Baseline
		row.Delta = &delta
	}
	row.State = "FAIL"
	if editLoopCompare(value, *b.Target, b.Compare) {
		row.State = "PASS"
	}
	return row
}

func assessEditLoopHoldRow(row editLoopRow, b budget, d dataset) editLoopRow {
	if d.config.repeats(b.Scenario) < 3 {
		row.Reason = "at least three actual repeats required"
		return row
	}
	state := ""
	var values []float64
	for repeat := 1; repeat <= d.config.repeats(b.Scenario); repeat++ {
		found := false
		for _, sample := range d.runs {
			if sample.input.Role != "candidate" || sample.input.HostClass != b.HostClass || sample.input.Scenario != b.Scenario || sample.input.Repeat != repeat {
				continue
			}
			found = true
			got := assessEditLoopHoldSample(row, b, sample.data.Phases[0])
			if got.State == "UNAVAILABLE" {
				return got
			}
			if state != "" && state != got.State {
				row.State = "FAIL"
				row.Reason = "same-build repeat hold/applicability verdicts disagree"
				return row
			}
			state = got.State
			if got.Value != nil {
				values = append(values, *got.Value)
			}
		}
		if !found {
			row.Reason = "missing candidate repeat"
			return row
		}
	}
	if state == "NOT_EXERCISED" {
		row.State = state
		row.Reason = editLoopZeroHoldReason
		return row
	}
	if len(values) == 0 {
		row.Reason = "no actual observations"
		return row
	}
	value := slices.Max(values)
	row.State = state
	row.Value = &value
	if row.Baseline != nil {
		delta := value - *row.Baseline
		row.Delta = &delta
	}
	return row
}
