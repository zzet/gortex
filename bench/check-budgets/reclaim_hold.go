package main

import (
	"encoding/json"
	"errors"
	"slices"
)

type holdHistogram struct {
	count   int64
	edges   []float64
	counts  []int64
	maximum float64
}

func decodeHoldHistogram(raw json.RawMessage) (holdHistogram, error) {
	var status struct {
		Storage struct {
			Reclaim struct {
				Hold *struct {
					Count   *int64     `json:"count"`
					Edges   []*float64 `json:"bucket_le_ms"`
					Counts  []*int64   `json:"bucket_counts"`
					Maximum *float64   `json:"max_ms"`
				} `json:"writer_hold"`
			} `json:"wal_reclaim"`
		} `json:"storage"`
	}
	var h holdHistogram
	if json.Unmarshal(raw, &status) != nil || status.Storage.Reclaim.Hold == nil {
		return h, errors.New("missing writer hold histogram capability")
	}
	w := status.Storage.Reclaim.Hold
	if w.Count == nil || *w.Count < 0 || *w.Count > 1<<53 || w.Maximum == nil || !finiteNonnegative(*w.Maximum) || len(w.Edges) == 0 || len(w.Counts) != len(w.Edges)+1 {
		return h, errors.New("missing or incompatible hold histogram census")
	}
	h.count = *w.Count
	h.maximum = *w.Maximum
	for _, edge := range w.Edges {
		if edge == nil || !finiteNonnegative(*edge) || *edge <= 0 || len(h.edges) > 0 && *edge <= h.edges[len(h.edges)-1] {
			return h, errors.New("invalid hold histogram bucket")
		}
		h.edges = append(h.edges, *edge)
	}
	var sum int64
	last := -1
	for i, n := range w.Counts {
		if n == nil || *n < 0 || *n > 1<<53 || sum > 1<<53-*n {
			return h, errors.New("invalid hold histogram bucket count")
		}
		sum += *n
		h.counts = append(h.counts, *n)
		if *n > 0 {
			last = i
		}
	}
	if sum != h.count || last < 0 && h.maximum != 0 || last >= 0 && (last > 0 && h.maximum <= h.edges[last-1] || last < len(h.edges) && h.maximum > h.edges[last]) {
		return h, errors.New("hold count/maximum does not match complete bucket population")
	}
	return h, nil
}

// This reproduces the existing producer's histogram-derived p95 upper bound,
// not an exact quantile of individual durations and never RPC response time.
func measureReclaimHoldP95(p phase) (float64, error) {
	if _, err := validateOwnedStatusCensus(p); err != nil {
		return 0, err
	}
	var first, last holdHistogram
	for i, raw := range p.Status {
		h, err := decodeHoldHistogram(raw)
		if err != nil {
			return 0, err
		}
		if i == 0 {
			first = h
		} else {
			if !slices.Equal(h.edges, last.edges) || h.count < last.count || h.maximum < last.maximum {
				return 0, errors.New("hold histogram reset or changed shape")
			}
			for j, n := range h.counts {
				if n < last.counts[j] {
					return 0, errors.New("hold histogram bucket went backwards")
				}
			}
		}
		last = h
	}
	n := last.count - first.count
	if n <= 0 {
		return 0, errors.New("no observed writer holds in the owned window")
	}
	// Exact ceil(95*n/100), without floating rounding or multiplication overflow.
	rank := (n/100)*95 + ((n%100)*95+99)/100
	var seen int64
	bucket, topBucket := -1, -1
	for i, count := range last.counts {
		delta := count - first.counts[i]
		seen += delta
		if bucket < 0 && seen >= rank {
			bucket = i
		}
		if delta > 0 {
			topBucket = i
		}
	}
	edge := func(i int) float64 {
		if i >= len(last.edges) {
			return last.maximum
		}
		return last.edges[i]
	}
	top := last.maximum
	if last.maximum <= first.maximum {
		top = min(edge(topBucket), last.maximum)
	}
	value := min(edge(bucket), top)
	aggregate, err := number(p.Metrics["reclaim_writer_hold_p95_ms"], "ms")
	if err != nil || aggregate != value {
		return 0, errors.New("raw hold histogram does not reproduce producer percentile bound")
	}
	return value / 1000, nil
}
