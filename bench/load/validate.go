package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"reflect"
	"strings"
)

// A bounded fixed-format check supplements decoding: absent fields are not
// silently accepted as Go zero values. This is not a general schema engine.
func requireFields(data []byte, prototype any) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	t := reflect.TypeOf(prototype)
	for i := range t.NumField() {
		field := t.Field(i)
		tag := field.Tag.Get("json")
		if field.PkgPath != "" || tag == "-" || strings.Contains(tag, ",omitempty") {
			continue
		}
		name := strings.Split(tag, ",")[0]
		if _, ok := fields[name]; !ok {
			return fmt.Errorf("missing required %s field", name)
		}
	}
	return nil
}

func validMetric(m Metric) error {
	if m.Unit == "" {
		return errors.New("metric is missing its unit")
	}
	if m.Value == nil {
		if m.Reason == "" {
			return errors.New("unavailable metric is missing its reason")
		}
	} else if m.Reason != "" || math.IsNaN(*m.Value) || math.IsInf(*m.Value, 0) {
		return errors.New("measured metric has invalid value or unavailable reason")
	}
	return nil
}

func validateReport(data []byte, r Report) error {
	if err := requireFields(data, Report{}); err != nil {
		return err
	}
	if r.RunID == "" || !commitPattern.MatchString(r.GitSHA) || r.Binary == "" || r.Started.IsZero() || len(r.Corpus) == 0 {
		return errors.New("report is missing run/revision/binary/time/corpus identity")
	}
	if _, err := parseProfile(r.Profile); err != nil {
		return err
	}
	if r.Workload.SessionModel == "" {
		return errors.New("offered workload is missing its session model")
	}
	if r.Workload.Profile != r.Profile || r.Workload.Seed != r.Seed || r.Workload.Scenario == "" || r.Workload.LinkedWorktrees < 0 || r.Workload.DurationSec <= 0 || r.Workload.DeadlineSec <= 0 || len(r.Workload.Operations) == 0 {
		return errors.New("invalid or incomplete offered workload")
	}
	if err := validateAgentCounts(r.Workload.Scenario, r.Workload.Agents, r.Workload.EditAgents); err != nil {
		return err
	}
	if r.Host.Profile == "floor" {
		activeAgents := r.Workload.Agents
		if r.Workload.Scenario == "edit" || r.Workload.Scenario == "many-edit" || r.Workload.Scenario == "all" {
			activeAgents += r.Workload.EditAgents
		}
		checkouts := 1
		if r.Workload.Scenario == "worktrees" || r.Workload.Scenario == "all" {
			checkouts += r.Workload.LinkedWorktrees
		}
		if activeAgents > 4 || checkouts > 4 {
			return errors.New("floor report exceeds four total agents/checkouts")
		}
	}
	hash, err := hashJSON(r.Workload)
	if err != nil || hash != r.RequestPlan {
		return errors.New("offered workload does not match request plan hash")
	}
	for _, corpus := range r.Corpus {
		if err := validateCorpus(corpus); err != nil {
			return err
		}
	}
	if r.Host.Machine == "" || r.Host.OS == "" || r.Host.Arch == "" || r.Host.Profile == "" || len(r.Host.Limits) == 0 {
		return errors.New("report is missing host identity/limits")
	}
	if r.Host.Profile != "native" && !r.Host.Validated {
		return errors.New("reference/floor host has not been validated")
	}
	check := func(metrics map[string]Metric) error {
		for name, m := range metrics {
			if err := validMetric(m); err != nil {
				return fmt.Errorf("metric %s: %w", name, err)
			}
		}
		return nil
	}
	if err := check(r.Host.Limits); err != nil {
		return err
	}
	if err := check(r.Metrics); err != nil {
		return err
	}
	if r.Complete && (len(r.Phases) == 0 || len(r.Errors) != 0) {
		return errors.New("complete report has no phases or contains errors")
	}
	if r.Complete && r.DaemonVersion == "" {
		return errors.New("complete report is missing daemon version")
	}
	seen := make(map[string]bool)
	for _, phase := range r.Phases {
		if phase.Name == "" || seen[phase.Name] || phase.Started.IsZero() || phase.DurationSec <= 0 || (r.Complete && len(phase.Errors) != 0) {
			return errors.New("invalid, duplicate or incomplete phase")
		}
		seen[phase.Name] = true
		if r.Complete {
			if phase.SessionModel == "" {
				return errors.New("complete phase is missing its session model")
			}
			if len(phase.Operations) == 0 || len(phase.Gate) == 0 || len(phase.Status) == 0 || len(phase.Stats) == 0 {
				return errors.New("complete phase is missing plan/gate/status/stats observations")
			}
			if phase.Name != "cold" && phase.Name != "restart" && len(phase.Tools) == 0 {
				return errors.New("complete workload phase has no tool observations")
			}
		}
		if err := check(phase.Metrics); err != nil {
			return err
		}
		for _, tool := range []map[string]ToolMetrics{phase.Tools, phase.Probes} {
			for name, row := range tool {
				if name == "" || row.Successes < 0 || row.BusyRefusals < 0 || row.RetryableRefusals < 0 || row.HarnessIncomplete < 0 || row.ProductExpired < 0 || row.PhaseCensored < 0 {
					return errors.New("invalid tool identity/count")
				}
				if err := validMetric(row.SuccessPerSec); err != nil {
					return err
				}
				if err := validDistribution(row.Latency); err != nil {
					return err
				}
			}
		}
		if err := validDistribution(phase.EditLatency); err != nil {
			return err
		}
		if err := validDistribution(phase.Visibility); err != nil {
			return err
		}
	}
	return nil
}

func validDistribution(d Distribution) error {
	if d.Count < 0 {
		return errors.New("negative distribution count")
	}
	for _, m := range []Metric{d.P50, d.P95, d.P99, d.Max} {
		if err := validMetric(m); err != nil {
			return err
		}
		if (d.Count == 0) != (m.Value == nil) {
			return errors.New("distribution count disagrees with observations")
		}
	}
	return nil
}
