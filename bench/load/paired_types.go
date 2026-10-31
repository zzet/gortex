package main

import (
	"fmt"
	"slices"
	"time"
)

// Paired native runs compare matched request latency under a shared, coupled
// workload. They are never independent capacity/reference/floor measurements.
type pairedReport struct {
	SchemaVersion int                               `json:"paired_schema_version"`
	Kind          string                            `json:"kind"`
	RunID         string                            `json:"run_id"`
	Window        measurementWindow                 `json:"group_window"`
	BuildOrder    []string                          `json:"build_order"`
	Builds        map[string]Report                 `json:"builds"`
	Records       []pairedRecord                    `json:"requests"`
	Ratios        map[string]map[string]pairedRatio `json:"matched_latency_ratios"`
	Metrics       map[string]Metric                 `json:"group_metrics"`
	Errors        []string                          `json:"errors"`
	Complete      bool                              `json:"complete"`
}

type measurementWindow struct {
	Started time.Time `json:"started"`
	Ended   time.Time `json:"ended"`
	Method  string    `json:"method"`
}

type requestTuple struct {
	Repo        string `json:"repo"`
	Commit      string `json:"commit"`
	Tool        string `json:"tool"`
	Role        string `json:"role"`
	Agent       int    `json:"agent"`
	Step        int    `json:"step"`
	Slot        int    `json:"slot"`
	RequestHash string `json:"request_sha256"`
	Ordinal     uint64 `json:"category_ordinal"`
}

type pairedRecord struct {
	Tuple     requestTuple            `json:"tuple"`
	TupleHash string                  `json:"tuple_sha256"`
	Order     []string                `json:"order"`
	Samples   map[string]pairedSample `json:"samples"`
}

type pairedSample struct {
	Repo        string    `json:"repo"`
	Commit      string    `json:"commit"`
	RequestHash string    `json:"request_sha256"`
	Started     time.Time `json:"started"`
	Ended       time.Time `json:"ended"`
	Latency     Metric    `json:"logical_latency_ms"`
	Outcome     string    `json:"outcome"`
	Code        int       `json:"code"`
	Error       string    `json:"error"`
}

type pairedRatio struct {
	Median           Metric         `json:"median"`
	Matched          int            `json:"matched"`
	Excluded         int            `json:"excluded"`
	FirstBuildCounts map[string]int `json:"first_build_counts"`
}

func rotatingOrder(labels []string, ordinal uint64) []string {
	order := make([]string, len(labels))
	for i := range order {
		order[i] = labels[(int(ordinal%uint64(len(labels)))+i)%len(labels)]
	}
	return order
}

func median(values []float64) Metric {
	if len(values) == 0 {
		return unavailable("ratio", "no successful identical matched requests")
	}
	ordered := slices.Clone(values)
	slices.Sort(ordered)
	n := len(ordered)
	value := ordered[n/2]
	if n%2 == 0 {
		value = (ordered[n/2-1] + value) / 2
	}
	return measured(value, "ratio")
}

func matchedLatencyRatios(labels []string, records []pairedRecord) map[string]map[string]pairedRatio {
	result := make(map[string]map[string]pairedRatio)
	if len(labels) < 2 {
		return result
	}
	for _, label := range labels[1:] {
		comparison := label + "/" + labels[0]
		rows := make(map[string]pairedRatio)
		values := make(map[string][]float64)
		for _, record := range records {
			category := record.Tuple.Role + "." + record.Tuple.Tool
			row := rows[category]
			if row.FirstBuildCounts == nil {
				row.FirstBuildCounts = make(map[string]int)
			}
			if len(record.Order) > 0 {
				row.FirstBuildCounts[record.Order[0]]++
			}
			a, aOK := record.Samples[labels[0]]
			b, bOK := record.Samples[label]
			if !aOK || !bOK || a.Outcome != "success" || b.Outcome != "success" || a.RequestHash != record.Tuple.RequestHash || b.RequestHash != record.Tuple.RequestHash || a.Repo != record.Tuple.Repo || b.Repo != record.Tuple.Repo || a.Commit != record.Tuple.Commit || b.Commit != record.Tuple.Commit || a.Latency.Value == nil || b.Latency.Value == nil || *a.Latency.Value <= 0 || *b.Latency.Value <= 0 {
				row.Excluded++
			} else {
				row.Matched++
				values[category] = append(values[category], *b.Latency.Value / *a.Latency.Value)
			}
			rows[category] = row
		}
		for category, row := range rows {
			row.Median = median(values[category])
			rows[category] = row
		}
		result[comparison] = rows
	}
	return result
}

func validateBuildLabels(labels []string) error {
	if len(labels) < 2 || len(labels) > 3 {
		return fmt.Errorf("paired native requires 2–3 labeled builds")
	}
	seen := make(map[string]bool)
	for _, label := range labels {
		if !labelPattern.MatchString(label) || seen[label] {
			return fmt.Errorf("invalid/duplicate build label %q", label)
		}
		seen[label] = true
	}
	return nil
}
