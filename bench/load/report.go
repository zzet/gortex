// Command load measures a private Gortex daemon over its public socket protocol.
package main

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"slices"
	"time"
)

const reportVersion = 1

// Metric never represents missing observations as zero. Zero is a measurement;
// unavailable values have a reason and encode as JSON null.
type Metric struct {
	Value  *float64 `json:"value"`
	Unit   string   `json:"unit"`
	Reason string   `json:"reason"`
}

func measured(value float64, unit string) Metric { return Metric{Value: &value, Unit: unit} }
func unavailable(unit, reason string) Metric     { return Metric{Unit: unit, Reason: reason} }

type Distribution struct {
	Count int    `json:"count"`
	P50   Metric `json:"p50"`
	P95   Metric `json:"p95"`
	P99   Metric `json:"p99"`
	Max   Metric `json:"max"`
}

func distribution(values []float64, unit string) Distribution {
	d := Distribution{Count: len(values)}
	if len(values) == 0 {
		d.P50 = unavailable(unit, "no completed observations")
		d.P95, d.P99, d.Max = d.P50, d.P50, d.P50
		return d
	}
	v := slices.Clone(values)
	slices.Sort(v)
	quantile := func(p float64) Metric {
		return measured(v[max(0, int(math.Ceil(p*float64(len(v))))-1)], unit)
	}
	d.P50, d.P95, d.P99, d.Max = quantile(.50), quantile(.95), quantile(.99), quantile(1)
	return d
}

type Corpus struct {
	Label             string `json:"label"`
	Source            string `json:"source"`
	Commit            string `json:"commit"`
	File              string `json:"file"`
	Symbol            string `json:"symbol"`
	Query             string `json:"query"`
	EditTemplate      string `json:"edit_template"`
	EditAnchor        string `json:"edit_anchor"`
	ProposedSignature string `json:"proposed_signature"`
}

type Host struct {
	Machine      string            `json:"machine"`
	OS           string            `json:"os"`
	Arch         string            `json:"arch"`
	Kernel       string            `json:"kernel"`
	Profile      string            `json:"profile"`
	Validated    bool              `json:"validated"`
	Limits       map[string]Metric `json:"limits"`
	Availability map[string]string `json:"availability"`
}

// Every attempt is retained, including busy refusals and phase-censored calls.
// Workload and readiness/visibility probes have distinct Role denominators.
type Attempt struct {
	Tool       string    `json:"tool"`
	Role       string    `json:"role"`
	Request    string    `json:"request_sha256"`
	Agent      int       `json:"agent"`
	Step       int       `json:"step"`
	Slot       int       `json:"slot"`
	Retry      int       `json:"retry"`
	Started    time.Time `json:"started"`
	DurationMS float64   `json:"duration_ms"`
	LogicalMS  float64   `json:"logical_ms"` // includes earlier refusals and backoff, excludes think time
	Code       int       `json:"code"`
	Error      string    `json:"error"`
	Outcome    string    `json:"outcome"` // success, error, busy, product_deadline, phase_censored
}

type ToolMetrics struct {
	Latency        Distribution   `json:"latency_ms"`
	SuccessPerSec  Metric         `json:"successful_calls_per_second"`
	Successes      int            `json:"successes"`
	BusyRefusals   int            `json:"busy_refusals"`
	Errors         map[string]int `json:"errors"`
	PhaseCensored  int            `json:"phase_censored"`
	ProductExpired int            `json:"product_expired"`
}

type SweepEntry struct {
	Tool      string `json:"tool"`
	Operation string `json:"operation"`
	Status    string `json:"status"` // planned, measured, missing, unsupported, error
	Reason    string `json:"reason"`
}

type EditObservation struct {
	Repo            string          `json:"repo"`
	Marker          string          `json:"marker"`
	WriteMS         float64         `json:"write_ms"`
	VisibilityMS    Metric          `json:"visibility_ms"`
	Outcome         string          `json:"outcome"`
	Started         time.Time       `json:"started"`
	Acknowledgement json.RawMessage `json:"acknowledgement"`
}

type Phase struct {
	SessionModel string                     `json:"session_model"`
	Operations   []request                  `json:"operations"`
	Name         string                     `json:"name"`
	Started      time.Time                  `json:"started"`
	DurationSec  float64                    `json:"duration_seconds"`
	Gate         map[string]json.RawMessage `json:"gate"`
	Tools        map[string]ToolMetrics     `json:"tools"`
	Probes       map[string]ToolMetrics     `json:"probes"`
	Attempts     []Attempt                  `json:"attempts"`
	Edits        []EditObservation          `json:"edits"`
	EditLatency  Distribution               `json:"edit_latency_ms"`
	Visibility   Distribution               `json:"visible_latency_ms"`
	NeverSeen    int                        `json:"never_visible"`
	Censored     int                        `json:"visibility_censored"`
	Metrics      map[string]Metric          `json:"metrics"`
	Status       []json.RawMessage          `json:"daemon_status"`
	Stats        []json.RawMessage          `json:"graph_stats"`
	Errors       []string                   `json:"errors"`
}

type Report struct {
	SchemaVersion int               `json:"schema_version"`
	RunID         string            `json:"run_id"`
	GitSHA        string            `json:"git_sha"`
	Binary        string            `json:"binary"`
	DaemonVersion string            `json:"daemon_version"`
	Started       time.Time         `json:"started"`
	Profile       string            `json:"profile"`
	Seed          int64             `json:"seed"`
	Workload      Workload          `json:"workload"`
	Host          Host              `json:"host"`
	Corpus        []Corpus          `json:"corpus"`
	RequestPlan   string            `json:"request_plan_sha256"`
	Phases        []Phase           `json:"phases"`
	Sweep         []SweepEntry      `json:"tool_sweep"`
	Metrics       map[string]Metric `json:"metrics"`
	Artifacts     map[string]string `json:"artifacts"`
	Crashes       map[string]int    `json:"crashes"`
	Errors        []string          `json:"errors"`
	Complete      bool              `json:"complete"`
}

func writeReport(path string, report Report) error {
	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return fmt.Errorf("encode report: %w", err)
	}
	return os.WriteFile(path, append(data, '\n'), 0o600)
}

func summarize(attempts []Attempt, seconds float64, role string) map[string]ToolMetrics {
	result := make(map[string]ToolMetrics)
	latencies := make(map[string][]float64)
	for _, a := range attempts {
		if a.Role != role {
			continue
		}
		m := result[a.Tool]
		if m.Errors == nil {
			m.Errors = make(map[string]int)
		}
		switch a.Outcome {
		case "success":
			m.Successes++
			latencies[a.Tool] = append(latencies[a.Tool], a.LogicalMS)
		case "busy":
			m.BusyRefusals++
		case "phase_censored":
			m.PhaseCensored++
		case "product_deadline":
			m.ProductExpired++
		default:
			m.Errors[fmt.Sprintf("%d:%s", a.Code, a.Error)]++
		}
		result[a.Tool] = m
	}
	for tool, m := range result {
		m.Latency = distribution(latencies[tool], "ms")
		m.SuccessPerSec = unavailable("calls/s", "phase has no measured duration")
		if role == "probe" {
			m.SuccessPerSec = unavailable("calls/s", "intermittent setup/visibility probes do not have a continuous offered-load interval")
		} else if seconds > 0 {
			m.SuccessPerSec = measured(float64(m.Successes)/seconds, "calls/s")
		}
		result[tool] = m
	}
	return result
}
