package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"reflect"
	"slices"
	"strings"
	"text/tabwriter"
)

func readReport(path string) (Report, error) {
	var r Report
	data, err := os.ReadFile(path)
	if err != nil {
		return r, err
	}
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&r); err != nil {
		return r, err
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return r, errors.New("expected one report JSON object")
	}
	if r.SchemaVersion != reportVersion {
		return r, fmt.Errorf("unsupported report schema %d", r.SchemaVersion)
	}
	if err := validateReport(data, r); err != nil {
		return r, err
	}
	return r, nil
}

func metricTable(r Report) map[string]Metric {
	metrics := make(map[string]Metric)
	for key, m := range r.Metrics {
		metrics["run."+key] = m
	}
	addDist := func(prefix string, d Distribution) {
		metrics[prefix+".p50"], metrics[prefix+".p95"] = d.P50, d.P95
		metrics[prefix+".p99"], metrics[prefix+".max"] = d.P99, d.Max
	}
	for _, phase := range r.Phases {
		prefix := phase.Name + "."
		for key, m := range phase.Metrics {
			metrics[prefix+key] = m
		}
		for _, group := range []struct {
			name string
			rows map[string]ToolMetrics
		}{{"workload", phase.Tools}, {"probe", phase.Probes}} {
			for tool, row := range group.rows {
				path := prefix + group.name + "." + tool
				addDist(path+".latency", row.Latency)
				metrics[path+".successful_calls_per_second"] = row.SuccessPerSec
				metrics[path+".busy_refusals"] = measured(float64(row.BusyRefusals), "attempts")
				metrics[path+".successes"] = measured(float64(row.Successes), "calls")
				metrics[path+".phase_censored"] = measured(float64(row.PhaseCensored), "calls")
				metrics[path+".product_expired"] = measured(float64(row.ProductExpired), "calls")
				for reason, count := range row.Errors {
					metrics[path+".errors."+reason] = measured(float64(count), "calls")
				}
			}
		}
		addDist(prefix+"edit_latency", phase.EditLatency)
		addDist(prefix+"visible_latency", phase.Visibility)
		metrics[prefix+"never_visible"] = measured(float64(phase.NeverSeen), "edits")
		metrics[prefix+"visibility_censored"] = measured(float64(phase.Censored), "edits")
	}
	return metrics
}

func printDelta(w io.Writer, before, after Report) error {
	if err := comparable(before, after); err != nil {
		return err
	}
	a, b := metricTable(before), metricTable(after)
	keys := make([]string, 0, len(a)+len(b))
	for key := range a {
		keys = append(keys, key)
	}
	for key := range b {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	keys = slices.Compact(keys)
	table := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	if _, err := fmt.Fprintf(table, "metric\tbefore\tafter\tchange\n"); err != nil {
		return err
	}
	var worse []string
	for _, key := range keys {
		x, y := a[key], b[key]
		display := func(m Metric) string {
			if m.Value == nil {
				return "unavailable"
			}
			return fmt.Sprintf("%.4g %s", *m.Value, m.Unit)
		}
		change := "n/a"
		if x.Value != nil && y.Value != nil && x.Unit == y.Unit && *x.Value != 0 {
			percent := (*y.Value / *x.Value - 1) * 100
			change = fmt.Sprintf("%+.2f%%", percent)
			if percent > 15 && (x.Unit == "ms" || x.Unit == "s") {
				worse = append(worse, key)
			}
		}
		if _, err := fmt.Fprintf(table, "%s\t%s\t%s\t%s\n", key, display(x), display(y), change); err != nil {
			return err
		}
	}
	if err := table.Flush(); err != nil {
		return err
	}
	_, err := fmt.Fprintf(w, "\nTiming metrics more than 15%% worse: %s\nIncomplete reports: before=%t after=%t. Ratios do not establish independent saturation capacity.\n", strings.Join(worse, ", "), !before.Complete, !after.Complete)
	return err
}

func comparable(a, b Report) error {
	var reasons []string
	if a.Profile != b.Profile || a.Seed != b.Seed || !reflect.DeepEqual(a.Workload, b.Workload) || a.RequestPlan != b.RequestPlan {
		reasons = append(reasons, "offered workload/profile/seed/request plan differs")
	}
	if a.Host.Machine != b.Host.Machine || a.Host.OS != b.Host.OS || a.Host.Arch != b.Host.Arch || a.Host.Kernel != b.Host.Kernel || a.Host.Profile != b.Host.Profile || a.Host.Validated != b.Host.Validated || !reflect.DeepEqual(a.Host.Limits, b.Host.Limits) {
		reasons = append(reasons, "host identity/profile/limits differs")
	}
	normalize := func(corpora []Corpus) []Corpus {
		out := slices.Clone(corpora)
		for i := range out {
			out[i].Source = ""
		}
		return out
	}
	if !reflect.DeepEqual(normalize(a.Corpus), normalize(b.Corpus)) {
		reasons = append(reasons, "corpus labels/commits/targets/order differs")
	}
	if len(a.Phases) != len(b.Phases) {
		reasons = append(reasons, "phase census differs")
	} else {
		for i := range a.Phases {
			if a.Phases[i].Name != b.Phases[i].Name || a.Phases[i].SessionModel != b.Phases[i].SessionModel {
				reasons = append(reasons, "phase order/session model differs")
				break
			}
		}
	}
	if len(reasons) != 0 {
		return fmt.Errorf("incomparable reports; no regression verdict: %s", strings.Join(reasons, "; "))
	}
	return nil
}
