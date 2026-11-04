package main

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

func TestNightlySameDayDistinctRunsRemainInspectable(t *testing.T) {
	first, p := completeNightlyHistoryFixture(t, "schedule")
	second, _ := completeNightlyHistoryFixture(t, "workflow_dispatch")
	path := filepath.Join(filepath.Dir(second), "lane.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var receipt laneReceipt
	if err := json.Unmarshal(data, &receipt); err != nil {
		t.Fatal(err)
	}
	receipt.Workflow.RunURL = "https://github.com/example/project/actions/runs/456"
	data, err = json.Marshal(receipt)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	index := filepath.Join(t.TempDir(), "index.json")
	if err := writeNewJSON(index, []string{filepath.Dir(first), filepath.Dir(second)}); err != nil {
		t.Fatal(err)
	}
	items, err := loadNightlyHistory(index, p)
	if err != nil || len(items) != 2 {
		t.Fatalf("same-day independent workflow runs must remain inspectable: %v", err)
	}
}

func TestNightlyHistoryCommandPinsReviewedRevision(t *testing.T) {
	index, p := completeNightlyHistoryFixture(t, "workflow_dispatch")
	data, err := yaml.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	budgets := filepath.Join(filepath.Dir(index), "budgets.yaml")
	if err := os.WriteFile(budgets, data, 0o600); err != nil {
		t.Fatal(err)
	}
	args := []string{"--inputs", index, "--through", "2026-10-30", "--budgets", budgets}
	if code := historyCommand(append(append([]string{}, args...), "--revision", strings.Repeat("a", 40)), io.Discard, io.Discard); code != 0 {
		t.Fatalf("complete reviewed main run rejected: %d", code)
	}
	if code := historyCommand(append(append([]string{}, args...), "--revision", strings.Repeat("b", 40)), io.Discard, io.Discard); code != 1 {
		t.Fatalf("old/different main revision accepted: %d", code)
	}
	if code := historyCommand(args, io.Discard, io.Discard); code != 2 {
		t.Fatalf("missing reviewed revision accepted: %d", code)
	}
}

func TestNightlyManualEquivalentReplaysCompleteArtifacts(t *testing.T) {
	for _, mutation := range []string{"", "pull-request", "synthetic-event", "non-main", "policy", "revision", "host", "missing-attempt", "raw-revision", "raw-host", "raw-population", "missing-raw", "raw-schema", "population", "budget-values", "matrix"} {
		name := mutation
		if name == "" {
			name = "complete"
		}
		t.Run(name, func(t *testing.T) {
			index, p := completeNightlyHistoryFixture(t, "workflow_dispatch")
			dir := filepath.Dir(index)
			change := func(name string, fn func(map[string]any)) {
				t.Helper()
				path := filepath.Join(dir, name)
				data, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				var value map[string]any
				if err := json.Unmarshal(data, &value); err != nil {
					t.Fatal(err)
				}
				fn(value)
				data, err = json.Marshal(value)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, data, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			switch mutation {
			case "pull-request":
				change("lane.json", func(v map[string]any) { v["workflow"].(map[string]any)["event"] = "pull_request" })
			case "synthetic-event":
				change("lane.json", func(v map[string]any) { v["workflow"].(map[string]any)["event"] = "synthetic" })
			case "non-main":
				change("lane.json", func(v map[string]any) { v["workflow"].(map[string]any)["ref"] = "refs/heads/feature" })
			case "policy":
				change("lane.json", func(v map[string]any) { v["plan"].(map[string]any)["policy_sha256"] = strings.Repeat("b", 64) })
			case "revision":
				change("lane.json", func(v map[string]any) { v["workflow"].(map[string]any)["revision"] = strings.Repeat("b", 40) })
			case "host":
				change("lane.json", func(v map[string]any) { v["plan"].(map[string]any)["host_class"] = "reference_mac" })
			case "missing-attempt":
				change("lane.json", func(v map[string]any) { delete(v["workflow"].(map[string]any), "run_attempt") })
			case "raw-revision":
				change("candidate-1.json", func(v map[string]any) { v["git_sha"] = strings.Repeat("b", 40) })
			case "raw-host":
				change("candidate-1.json", func(v map[string]any) { v["host"].(map[string]any)["machine"] = "different-host" })
			case "raw-population":
				change("candidate-1.json", func(v map[string]any) { v["workload"].(map[string]any)["agents"] = 4 })
			case "missing-raw":
				if err := os.Remove(filepath.Join(dir, "candidate-1.json")); err != nil {
					t.Fatal(err)
				}
			case "raw-schema":
				change("candidate-1.json", func(v map[string]any) { v["schema_version"] = 999 })
			case "population":
				change("inputs.json", func(v map[string]any) { v["runs"] = v["runs"].([]any)[1:] })
			case "budget-values":
				change("budget-results.json", func(v map[string]any) { v["rows"].([]any)[0].(map[string]any)["after"] = 0 })
			case "matrix":
				change("budget-results.json", func(v map[string]any) { v["rows"].([]any)[0].(map[string]any)["scenario"] = "load-8" })
			}
			_, err := loadNightlyHistory(index, p)
			if mutation == "" {
				if err != nil {
					t.Fatalf("complete manual main nightly-equivalent artifacts rejected: %v", err)
				}
			} else if err == nil {
				t.Fatalf("%s must not count as complete nightly-equivalent evidence", mutation)
			}
		})
	}
}

func TestNightlyOneRunKeepsRequiredHostsIndependent(t *testing.T) {
	date := time.Date(2026, 10, 30, 0, 0, 0, 0, time.UTC)
	item := nightlyEvidence{day: "2026-10-30", receipt: laneReceipt{Plan: lanePlan{HostClass: "reference_linux", Ready: true}, BudgetPass: true, Workflow: workflowProvenance{Event: "workflow_dispatch", Ref: "refs/heads/main", Revision: strings.Repeat("a", 40), RunURL: "https://github.com/example/project/actions/runs/123", Attempt: 1}}}
	mac := item
	mac.receipt.Plan.HostClass = "reference_mac"
	hosts := []string{"reference_linux", "reference_mac"}
	if !historyVerdict([]nightlyEvidence{item, mac}, date, hosts, strings.Repeat("a", 40), io.Discard) {
		t.Fatal("one complete same-run cohort across required hosts rejected")
	}
	for _, mismatch := range []string{"missing-host", "different-run", "different-revision", "different-attempt"} {
		t.Run(mismatch, func(t *testing.T) {
			other := mac
			switch mismatch {
			case "missing-host":
				other.receipt.Plan.HostClass = "large_mac"
			case "different-run":
				other.receipt.Workflow.RunURL = "https://github.com/example/project/actions/runs/456"
			case "different-revision":
				other.receipt.Workflow.Revision = strings.Repeat("b", 40)
			case "different-attempt":
				other.receipt.Workflow.Attempt = 2
			}
			if historyVerdict([]nightlyEvidence{item, other}, date, hosts, strings.Repeat("a", 40), io.Discard) {
				t.Fatal("incomplete/mixed workflow cohort accepted")
			}
		})
	}
}

func TestNightlyManualOrRetryPassCannotEraseScheduledMisses(t *testing.T) {
	date := time.Date(2026, 10, 30, 0, 0, 0, 0, time.UTC)
	row := result{HostClass: "reference_linux", Scenario: "load-16", Metric: "read_p95", Mode: "block"}
	earlier := nightlyEvidence{day: "2026-10-29", receipt: laneReceipt{Plan: lanePlan{HostClass: "reference_linux"}, Workflow: workflowProvenance{Event: "schedule"}}, budgets: budgetEvidence{Rows: []result{row}}}
	current := earlier
	current.day = "2026-10-30"
	for _, event := range []string{"schedule", "workflow_dispatch"} {
		t.Run(event, func(t *testing.T) {
			passed := nightlyEvidence{day: "2026-10-30", receipt: laneReceipt{Plan: lanePlan{HostClass: "reference_linux", Ready: true}, BudgetPass: true, Workflow: workflowProvenance{Event: event, Ref: "refs/heads/main", Revision: strings.Repeat("a", 40), RunURL: "https://github.com/example/project/actions/runs/456", Attempt: 2}}}
			var out bytes.Buffer
			if !historyVerdict([]nightlyEvidence{earlier, current, passed}, date, []string{"reference_linux"}, strings.Repeat("a", 40), &out) {
				t.Fatal("complete run still requires a calendar wait")
			}
			if !strings.Contains(out.String(), "BLOCKER: reference_linux / load-16 / read_p95") {
				t.Fatalf("passing run erased consecutive scheduled misses: %s", out.String())
			}
		})
	}
}
