package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"
)

func number(m metric, unit string) (float64, error) {
	if m.Value == nil || m.Reason != "" || m.Unit != unit || !finiteNonnegative(*m.Value) {
		return 0, fmt.Errorf("missing, unavailable, nonfinite or incompatible metric (want %s, observed %s, reason %q)", unit, m.Unit, m.Reason)
	}
	return *m.Value, nil
}

func validateScenario(name string, r report) error {
	spec := scenarioSpecs[name]
	if spec.missing != "" {
		return errors.New(spec.missing)
	}
	var w struct {
		Scenario     string `json:"scenario"`
		Agents       int    `json:"agents"`
		Editors      int    `json:"edit_agents"`
		Worktrees    int    `json:"linked_worktrees"`
		SessionModel string `json:"session_model"`
	}
	if json.Unmarshal(r.Workload, &w) != nil || w.Scenario != spec.wire || r.Phases[0].Name != spec.wire || w.Worktrees != spec.worktrees {
		return errors.New("scenario does not match actual workload/phase")
	}
	model := "per_agent"
	if spec.wire == "sweep" {
		model = "per_operation_isolated"
	}
	if w.SessionModel != model || r.Phases[0].SessionModel != model {
		return errors.New("unknown/coupled session identity cannot establish the frozen standalone workload")
	}
	switch spec.wire {
	case "edit", "many-edit":
		if w.Agents != 0 || w.Editors != spec.editors {
			return errors.New("edit scenario requires the exact combined-editor count and zero extra pure readers")
		}
	case "read", "worktrees":
		if w.Agents != spec.agents {
			return errors.New("incorrect reading-agent count")
		}
	}
	var ready struct {
		Ready    bool     `json:"ready"`
		Enriched bool     `json:"enriched"`
		Roots    []string `json:"loaded_roots"`
	}
	if json.Unmarshal(r.Phases[0].Gate["readiness"], &ready) != nil || !ready.Ready || !ready.Enriched || len(ready.Roots) != spec.repos {
		return errors.New("missing ready/enriched evidence for the actual repository topology")
	}
	if len(r.Corpus) < spec.repos {
		return errors.New("missing pinned corpus identities")
	}
	for _, c := range r.Corpus {
		commit, _ := c["commit"].(string)
		label, _ := c["label"].(string)
		if label == "" || !revisionPattern.MatchString(commit) {
			return errors.New("corpus lacks logical label or pinned commit")
		}
	}
	if strings.HasPrefix(name, "tools-single-") && r.Corpus[0]["label"] != strings.TrimPrefix(name, "tools-single-") {
		return errors.New("sweep's manifest[0] is not the requested logical repository")
	}
	return nil
}

func quantile(d distribution, which string) (float64, error) {
	if d.Count < 1 {
		return 0, errors.New("no completed latency observations")
	}
	m := map[string]metric{"p50": d.P50, "p95": d.P95, "p99": d.P99, "max": d.Max}[which]
	v, err := number(m, "ms")
	return v / 1000, err
}

func readP95(p phase) (float64, error) {
	var values []float64
	for _, a := range p.Attempts {
		if a.Role == "workload" && a.Outcome == "success" {
			if a.LogicalMS == nil || !finiteNonnegative(*a.LogicalMS) {
				return 0, errors.New("invalid logical read latency")
			}
			values = append(values, *a.LogicalMS/1000)
		}
	}
	if len(values) == 0 {
		return 0, errors.New("no workload read observations")
	}
	slices.Sort(values)
	return values[int(math.Ceil(.95*float64(len(values))))-1], nil
}

func extract(name, host string, r report) (float64, error) {
	spec := metricSpecs[name]
	if spec.missing != "" {
		return 0, errors.New(spec.missing)
	}
	p := r.Phases[0]
	switch name {
	case "edit_ack_p95":
		return quantile(p.Edit, "p95")
	case "edit_ack_max":
		return quantile(p.Edit, "max")
	case "read_p95":
		return readP95(p)
	case "search_symbols_p95", "read_source_p99", "outline_p50", "dead_code_p50":
		lookup := map[string][2]string{"search_symbols_p95": {"search.symbols", "p95"}, "read_source_p99": {"read.source", "p99"}, "outline_p50": {"explore.outline", "p50"}, "dead_code_p50": {"analyze.dead_code", "p50"}}[name]
		return quantile(p.Tools[lookup[0]].Latency, lookup[1])
	case "busy_fraction":
		calls, busy := 0, 0
		for _, a := range p.Attempts {
			if a.Role == "workload" {
				calls++
				if a.Outcome == "busy" {
					busy++
				}
			}
		}
		if calls == 0 {
			return 0, errors.New("no workload attempts for busy denominator")
		}
		return float64(busy) / float64(calls), nil
	case "panics", "sigaborts", "oom_kills":
		key := map[string]string{"panics": "panic_lines", "sigaborts": "sigabrt_lines", "oom_kills": "oom_log_lines"}[name]
		n, ok := r.Crashes[key]
		if !ok || n == nil {
			return 0, errors.New("missing safety census")
		}
		if name == "oom_kills" {
			if host != "reference_linux" && host != hostedSmokeClass {
				return 0, errors.New("report-v1 lacks an authoritative OOM-kill observation on macOS; zero log lines are not zero kills")
			}
			actual, err := number(p.Metrics["scenario_cgroup_oom_kills"], "events")
			if err != nil {
				return 0, fmt.Errorf("authoritative Linux OOM-kill census: %w", err)
			}
			return max(actual, float64(*n)), nil
		}
		return float64(*n), nil
	case "force_kills":
		return number(p.Metrics["force_killed"], "boolean")
	case "edit_written_mib":
		return editWrites(p)
	}
	key, unit, scale := "", "bytes", float64(1024*1024)
	switch name {
	case "daemon_cpu_cores":
		key, unit, scale = "daemon_cpu_avg_cores", "cores", 1
	case "stop_seconds":
		key, unit, scale = "stop_seconds", "s", 1
	case "wal_peak_mib":
		key = "peak_wal_bytes"
	case "cold_written_mib":
		key = "startup_logical_written_bytes"
	case "idle_memory_2s_mib", "peak_memory_mib", "worktree_memory_mib":
		field := "phys_footprint_bytes"
		if host == "reference_linux" || host == hostedSmokeClass {
			field = "pss_bytes"
		}
		prefix := "peak_"
		if name == "idle_memory_2s_mib" {
			prefix = "idle_after_2s_"
		}
		key = prefix + field
	default:
		return 0, fmt.Errorf("metric %s needs a matched cross-scenario observation", name)
	}
	v, err := number(p.Metrics[key], unit)
	return v / scale, err
}

func editWrites(p phase) (float64, error) {
	var evidence struct {
		Acknowledged int               `json:"acknowledged_edits"`
		Contained    int               `json:"inferred_contained_patch_intervals"`
		Boundary     int               `json:"inferred_boundary_patch_intervals"`
		Overlapping  int               `json:"overlapping_inferred_contained_intervals"`
		Invalid      int               `json:"invalid_patch_records"`
		Totals       map[string]metric `json:"summed_interval_counters"`
		Events       []struct {
			Relation string `json:"window_relation"`
			Reset    *bool  `json:"wal_bytes_reset"`
		} `json:"events"`
	}
	if json.Unmarshal(p.Gate["patch_write_intervals"], &evidence) != nil || evidence.Acknowledged < 1 || evidence.Contained < 1 || evidence.Boundary != 0 || evidence.Overlapping != 0 || evidence.Invalid != 0 {
		return 0, errors.New("missing or non-comparable top-level patch interval counters/acknowledgement denominator")
	}
	contained := 0
	for _, e := range evidence.Events {
		if e.Relation == "inferred_contained" {
			contained++
		}
		if e.Relation == "inferred_contained" && (e.Reset == nil || *e.Reset) {
			return 0, errors.New("patch WAL count has unknown/reset lower-bound evidence")
		}
	}
	if contained != evidence.Contained {
		return 0, errors.New("missing contained patch interval witnesses")
	}
	wal, err := number(evidence.Totals["wal_bytes"], "bytes")
	if err != nil {
		return 0, err
	}
	main, err := number(evidence.Totals["main_bytes"], "bytes")
	return (wal + main) / float64(evidence.Acknowledged) / (1024 * 1024), err
}
