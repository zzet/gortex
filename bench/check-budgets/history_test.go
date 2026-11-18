package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestHistoryRequiresOneCompleteMainRunWithoutCalendarWait(t *testing.T) {
	date, err := time.Parse(time.DateOnly, "2026-10-30")
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range []string{"schedule", "workflow_dispatch"} {
		t.Run(event, func(t *testing.T) {
			item := nightlyEvidence{day: "2026-10-30", receipt: laneReceipt{Plan: lanePlan{HostClass: "reference_linux", Ready: true}, BudgetPass: true, Workflow: workflowProvenance{Event: event, Ref: "refs/heads/main", Revision: strings.Repeat("a", 40), RunURL: "https://github.com/example/project/actions/runs/123", Attempt: 1}}}
			if !historyVerdict([]nightlyEvidence{item}, date, []string{"reference_linux"}, strings.Repeat("a", 40), io.Discard) {
				t.Fatal("one complete main nightly-equivalent run must not wait for five calendar nights")
			}
			item.receipt.Plan.Ready = false
			if historyVerdict([]nightlyEvidence{item}, date, []string{"reference_linux"}, strings.Repeat("a", 40), io.Discard) {
				t.Fatal("preflight counted as accepted evidence")
			}
		})
	}
}

func TestHistoryFlagsConsecutiveRowMissesByMode(t *testing.T) {
	date, _ := time.Parse(time.DateOnly, "2026-10-30")
	rows := []result{{HostClass: "reference_linux", Scenario: "load-16", Metric: "read_p95", Mode: "block"}, {HostClass: "reference_linux", Scenario: "load-16", Metric: "peak_memory_mib", Mode: "guard"}}
	items := []nightlyEvidence{{day: "2026-10-29", receipt: laneReceipt{Plan: lanePlan{HostClass: "reference_linux"}, Workflow: workflowProvenance{Event: "schedule"}}, budgets: budgetEvidence{Rows: rows}}, {day: "2026-10-30", receipt: laneReceipt{Plan: lanePlan{HostClass: "reference_linux"}, Workflow: workflowProvenance{Event: "schedule"}}, budgets: budgetEvidence{Rows: rows}}}
	var out bytes.Buffer
	if historyVerdict(items, date, []string{"reference_linux"}, strings.Repeat("a", 40), &out) {
		t.Fatal("failed history passed")
	}
	if !strings.Contains(out.String(), "BLOCKER: reference_linux / load-16 / read_p95") || !strings.Contains(out.String(), "GUARD REVIEW: reference_linux / load-16 / peak_memory_mib") {
		t.Fatalf("missing row flags: %s", out.String())
	}
}

func TestNightlyArtifactProvenanceCoverageAndReplay(t *testing.T) {
	p := testPolicy()
	p.Budgets = []budget{{HostClass: "reference_linux", Scenario: "load-1", Metric: "read_p95", Mode: "block", Unit: "s", Lane: []string{"nightly"}, Target: pointer(1), Compare: "le"}}
	makeIndex := func(t *testing.T, mutation string) string {
		t.Helper()
		dir := t.TempDir()
		artifact := filepath.Join(dir, "night")
		if err := os.Mkdir(artifact, 0o700); err != nil {
			t.Fatal(err)
		}
		started := time.Date(2026, 10, 30, 1, 19, 0, 0, time.UTC)
		r := laneReceipt{Version: 1, Plan: lanePlan{Lane: "nightly", HostClass: "reference_linux", PolicySHA256: policyDigest(p), Ready: true}, Started: started, Finished: started.Add(time.Hour), Workflow: workflowProvenance{Event: "schedule", Ref: "refs/heads/main", Revision: strings.Repeat("a", 40), RunURL: "https://github.com/example/project/actions/runs/123", Attempt: 1}}
		b := budgetEvidence{Lane: "nightly", Rows: []result{{HostClass: "reference_linux", Scenario: "load-1", Metric: "read_p95", Mode: "block", Unit: "s"}}}
		switch mutation {
		case "pull-request":
			r.Workflow.Event = "pull_request"
		case "branch":
			r.Workflow.Ref = "refs/heads/feature"
		case "policy":
			r.Plan.PolicySHA256 = strings.Repeat("b", 64)
		case "shortened-table":
			b.Rows = nil
		case "claimed-green-without-reports":
			r.BudgetPass = true
			b.Pass = true
			b.Rows[0].Pass = true
		}
		if err := writeNewJSON(filepath.Join(artifact, "lane.json"), r); err != nil {
			t.Fatal(err)
		}
		if err := writeNewJSON(filepath.Join(artifact, "budget-results.json"), b); err != nil {
			t.Fatal(err)
		}
		paths := []string{"night"}
		if mutation == "duplicate-run" {
			paths = append(paths, "night")
		}
		index := filepath.Join(dir, "index.json")
		if err := writeNewJSON(index, paths); err != nil {
			t.Fatal(err)
		}
		return index
	}
	if _, err := loadNightlyHistory(makeIndex(t, ""), p); err != nil {
		t.Fatalf("complete red table should remain inspectable: %v", err)
	}
	for _, mutation := range []string{"pull-request", "branch", "policy", "shortened-table", "duplicate-run", "claimed-green-without-reports"} {
		t.Run(mutation, func(t *testing.T) {
			if _, err := loadNightlyHistory(makeIndex(t, mutation), p); err == nil {
				t.Fatalf("%s counted as formal history", mutation)
			}
		})
	}
}

func TestNightlyReplayRejectsDifferentPassingTable(t *testing.T) {
	dir := t.TempDir()
	p := testPolicy()
	p.Budgets = []budget{{HostClass: "reference_linux", Scenario: "load-1", Metric: "read_p95", Mode: "block", Unit: "s", Lane: []string{"nightly"}, Target: pointer(1), Compare: "le"}}
	cfg := inputs{Version: 1, Repeats: 3, CandidateRevision: strings.Repeat("a", 40), MainRevision: strings.Repeat("d", 40), TagRevision: shippedRevision, Hosts: map[string]string{"reference_linux": "synthetic-host"}}
	for _, role := range []string{"candidate", "main"} {
		for repeat := 1; repeat <= 3; repeat++ {
			raw := syntheticReport()
			raw["run_id"] = fmt.Sprintf("%s-%d", role, repeat)
			raw["seed"] = repeat
			raw["workload"].(map[string]any)["seed"] = repeat
			raw["request_plan_sha256"] = fmt.Sprintf("%064x", repeat)
			if role == "main" {
				raw["git_sha"] = cfg.MainRevision
			}
			name := fmt.Sprintf("%s-%d.json", role, repeat)
			if err := writeNewJSON(filepath.Join(dir, name), raw); err != nil {
				t.Fatal(err)
			}
			cfg.Runs = append(cfg.Runs, runInput{Role: role, HostClass: "reference_linux", Scenario: "load-1", Repeat: repeat, Report: name})
		}
	}
	if err := writeNewJSON(filepath.Join(dir, "inputs.json"), cfg); err != nil {
		t.Fatal(err)
	}
	dataset, err := loadInputs(filepath.Join(dir, "inputs.json"))
	if err != nil {
		t.Fatal(err)
	}
	rows, err := check(p, dataset, "nightly")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || !rows[0].Pass {
		t.Fatalf("synthetic report should pass: %+v", rows)
	}
	started := time.Date(2026, 10, 30, 1, 19, 0, 0, time.UTC)
	receipt := laneReceipt{Version: 1, Plan: lanePlan{Lane: "nightly", HostClass: "reference_linux", PolicySHA256: policyDigest(p), Ready: true}, Started: started, Finished: started.Add(time.Hour), BudgetPass: true, Workflow: workflowProvenance{Event: "schedule", Ref: "refs/heads/main", Revision: cfg.CandidateRevision, RunURL: "https://github.com/example/project/actions/runs/123", Attempt: 1}}
	if err := writeNewJSON(filepath.Join(dir, "lane.json"), receipt); err != nil {
		t.Fatal(err)
	}
	evidence := budgetEvidence{Lane: "nightly", Pass: true, Rows: rows}
	path := filepath.Join(dir, "budget-results.json")
	if err := writeNewJSON(path, evidence); err != nil {
		t.Fatal(err)
	}
	index := filepath.Join(dir, "index.json")
	if err := writeNewJSON(index, []string{"."}); err != nil {
		t.Fatal(err)
	}
	if _, err := loadNightlyHistory(index, p); err != nil {
		t.Fatalf("exact replay rejected: %v", err)
	}
	// Both tables claim pass, but this table has a fabricated better after value.
	evidence.Rows[0].After = pointer(0)
	data, err := json.Marshal(evidence)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadNightlyHistory(index, p); err == nil {
		t.Fatal("different passing table accepted without exact replay")
	}
}

func completeNightlyHistoryFixture(t *testing.T, event string) (string, policy) {
	t.Helper()
	dir := t.TempDir()
	p := testPolicy()
	p.Allowances = []allowance{}
	p.Budgets = []budget{{HostClass: "reference_linux", Scenario: "load-1", Metric: "read_p95", Mode: "block", OwningRelease: "v0.64.12", Allowance: []string{}, Unit: "s", Lane: []string{"nightly"}, Target: pointer(1), Compare: "le"}}
	cfg := inputs{Version: 1, Repeats: 3, CandidateRevision: strings.Repeat("a", 40), MainRevision: strings.Repeat("d", 40), TagRevision: shippedRevision, Hosts: map[string]string{"reference_linux": "synthetic-host"}}
	for _, role := range []string{"candidate", "main"} {
		for repeat := 1; repeat <= 3; repeat++ {
			raw := syntheticReport()
			raw["run_id"] = fmt.Sprintf("%s-%d", role, repeat)
			raw["seed"] = repeat
			raw["workload"].(map[string]any)["seed"] = repeat
			raw["request_plan_sha256"] = fmt.Sprintf("%064x", repeat)
			if role == "main" {
				raw["git_sha"] = cfg.MainRevision
			}
			name := fmt.Sprintf("%s-%d.json", role, repeat)
			if err := writeNewJSON(filepath.Join(dir, name), raw); err != nil {
				t.Fatal(err)
			}
			cfg.Runs = append(cfg.Runs, runInput{Role: role, HostClass: "reference_linux", Scenario: "load-1", Repeat: repeat, Report: name})
		}
	}
	if err := writeNewJSON(filepath.Join(dir, "inputs.json"), cfg); err != nil {
		t.Fatal(err)
	}
	dataset, err := loadInputs(filepath.Join(dir, "inputs.json"))
	if err != nil {
		t.Fatal(err)
	}
	rows, err := check(p, dataset, "nightly")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || !rows[0].Pass {
		t.Fatalf("synthetic report should pass: %+v", rows)
	}
	started := time.Date(2026, 10, 30, 1, 19, 0, 0, time.UTC)
	receipt := laneReceipt{Version: 1, Plan: lanePlan{Lane: "nightly", HostClass: "reference_linux", PolicySHA256: policyDigest(p), Ready: true}, Started: started, Finished: started.Add(time.Hour), BudgetPass: true, Workflow: workflowProvenance{Event: event, Ref: "refs/heads/main", Revision: cfg.CandidateRevision, RunURL: "https://github.com/example/project/actions/runs/123", Attempt: 1}}
	if err := writeNewJSON(filepath.Join(dir, "lane.json"), receipt); err != nil {
		t.Fatal(err)
	}
	evidence := budgetEvidence{Lane: "nightly", Pass: true, Rows: rows}
	path := filepath.Join(dir, "budget-results.json")
	if err := writeNewJSON(path, evidence); err != nil {
		t.Fatal(err)
	}
	index := filepath.Join(dir, "index.json")
	if err := writeNewJSON(index, []string{"."}); err != nil {
		t.Fatal(err)
	}
	return index, p
}
