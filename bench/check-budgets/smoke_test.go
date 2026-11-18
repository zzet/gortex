package main

import (
	"encoding/json"
	"fmt"
	"maps"
	"strings"
	"testing"
	"time"
)

func syntheticSmokeJob(t *testing.T) (laneReceipt, dataset, policy) {
	t.Helper()
	selection, mainBinary, _ := syntheticMainArtifact()
	candidate := mainBinary
	candidate.Revision = strings.Repeat("c", 40)
	candidate.Settings = maps.Clone(mainBinary.Settings)
	candidate.Settings["vcs.revision"] = candidate.Revision
	started := time.Date(2026, 10, 30, 1, 0, 0, 0, time.UTC)
	receipt := laneReceipt{Version: 1, Plan: lanePlan{Lane: "smoke", HostClass: hostedSmokeClass, Ready: true}, Started: started, Finished: started.Add(time.Minute), Workflow: workflowProvenance{Event: "pull_request", Ref: "refs/pull/123/merge", Revision: candidate.Revision, RunURL: "https://github.com/zzet/gortex/actions/runs/789", Job: "smoke", Attempt: 1}, Smoke: &smokeProvenance{Main: selection, MainBinary: mainBinary, CandidateBinary: candidate, RunnerEnvironment: "github-hosted", RunnerOS: "Linux", RunnerArch: "ARM64"}}
	d := dataset{config: inputs{Version: 1, CandidateRevision: candidate.Revision, MainRevision: mainBinary.Revision, TagRevision: shippedRevision, Repeats: 3, Hosts: map[string]string{hostedSmokeClass: "synthetic-host"}}}
	for repeat := 1; repeat <= 3; repeat++ {
		for _, role := range []string{"main", "candidate"} {
			file := fmt.Sprintf("%s-%d.json", role, repeat)
			taskStart := started.Add(time.Duration(len(receipt.Tasks)) * time.Second)
			raw := syntheticReport()
			raw["run_id"] = file
			raw["seed"] = repeat
			raw["workload"].(map[string]any)["seed"] = repeat
			raw["request_plan_sha256"] = fmt.Sprintf("%064x", repeat)
			raw["git_sha"] = mainBinary.Revision
			if role == "candidate" {
				raw["git_sha"] = candidate.Revision
			}
			raw["started"] = taskStart.Add(time.Millisecond)
			host := raw["host"].(map[string]any)
			host["profile"] = "native"
			host["validated"] = false
			limits := host["limits"].(map[string]any)
			limits["logical_cpus"] = map[string]any{"value": 4, "unit": "cores", "reason": ""}
			limits["disk_capacity_bytes"] = map[string]any{"value": 14 * 1024 * 1024 * 1024, "unit": "bytes", "reason": ""}
			data, err := json.Marshal(raw)
			if err != nil {
				t.Fatal(err)
			}
			r, err := decodeReport(data)
			if err != nil {
				t.Fatal(err)
			}
			input := runInput{Role: role, HostClass: hostedSmokeClass, Scenario: "load-1", Repeat: repeat, Report: file}
			if err := validateReport(d.config, input, r); err != nil {
				t.Fatal(err)
			}
			d.runs = append(d.runs, observation{input: input, data: r})
			receipt.Tasks = append(receipt.Tasks, taskReceipt{laneTask: laneTask{Role: role, Repeat: repeat, Scenario: "load-1", Seed: int64(repeat)}, Report: file, Started: taskStart, Finished: taskStart.Add(time.Second)})
		}
	}
	p := testPolicy()
	p.Budgets = []budget{{HostClass: hostedSmokeClass, Scenario: "load-1", Metric: "read_p95", Mode: "block", Target: pointer(1), Unit: "s", Compare: "le", Lane: []string{"smoke"}}}
	receipt.Plan.PolicySHA256 = policyDigest(p)
	return receipt, d, p
}

func TestSameJobSmokeRequiresCurrentExecutionAndMatchingBuilds(t *testing.T) {
	receipt, d, _ := syntheticSmokeJob(t)
	if err := validateSmokeJob(receipt, d); err != nil {
		t.Fatal(err)
	}
	for _, mutation := range []string{"missing-job", "wrong-event", "wrong-attempt", "wrong-repository", "wrong-main", "wrong-toolchain", "wrong-build-flags", "old-report", "outside-task", "missing-task", "failed-task"} {
		t.Run(mutation, func(t *testing.T) {
			receipt, d, _ := syntheticSmokeJob(t)
			switch mutation {
			case "missing-job":
				receipt.Workflow.Job = ""
			case "wrong-event":
				receipt.Workflow.Event = "workflow_dispatch"
			case "wrong-attempt":
				receipt.Workflow.Attempt = 0
			case "wrong-repository":
				receipt.Workflow.RunURL = "https://github.com/example/fork/actions/runs/789"
			case "wrong-main":
				receipt.Smoke.Main.Revision = strings.Repeat("f", 40)
			case "wrong-toolchain":
				receipt.Smoke.CandidateBinary.GoVersion = "go0.0.0"
			case "wrong-build-flags":
				receipt.Smoke.CandidateBinary.Settings["CGO_CFLAGS"] = "-O0"
			case "old-report":
				d.runs[0].data.Started = receipt.Started.Add(-time.Hour)
			case "outside-task":
				d.runs[0].data.Started = receipt.Tasks[0].Finished.Add(time.Millisecond)
			case "missing-task":
				receipt.Tasks = receipt.Tasks[1:]
			case "failed-task":
				receipt.Tasks[0].Error = "synthetic failure"
			}
			if err := validateSmokeJob(receipt, d); err == nil {
				t.Fatalf("%s passed same-job provenance", mutation)
			}
		})
	}
}

func TestSameJobSmokeUsesRepeatedMedianAndRefusesSlowerOrMissingEvidence(t *testing.T) {
	receipt, d, p := syntheticSmokeJob(t)
	if err := validateSmokeJob(receipt, d); err != nil {
		t.Fatal(err)
	}
	candidate := []float64{110, 119, 900}
	i := 0
	for n := range d.runs {
		if d.runs[n].input.Role == "candidate" {
			d.runs[n].data.Phases[0].Attempts[0].LogicalMS = pointer(candidate[i])
			i++
		}
	}
	rows, err := check(p, d, "smoke")
	if err != nil || len(rows) != 1 || !rows[0].Pass {
		t.Fatalf("median119ms should fit120ms, despite a slow individual timing repeat: %+v %v", rows, err)
	}
	for n := range d.runs {
		if d.runs[n].input.Role == "candidate" {
			d.runs[n].data.Phases[0].Attempts[0].LogicalMS = pointer(121)
		}
	}
	rows, err = check(p, d, "smoke")
	if err != nil || rows[0].Pass {
		t.Fatal("p95 median>20% regression passed")
	}
	d.runs = d.runs[:len(d.runs)-1]
	rows, err = check(p, d, "smoke")
	if err != nil || rows[0].Pass {
		t.Fatal("missing planned repeat passed")
	}
}

func TestSameJobHostIdentityStillUsesStrictReferenceContract(t *testing.T) {
	_, d, _ := syntheticSmokeJob(t)
	r := d.runs[0].data
	r.Host.Limits["cpu_quota"] = metric{Value: pointer(4), Unit: "cores"}
	if err := validateHost("reference_linux", "synthetic-host", r); err == nil {
		t.Fatal("same-job provenance waived four-versus-eight CPU reference contract")
	}
}

func TestHostedSmokeProfileIsTruthfulAndCannotBecomeReference(t *testing.T) {
	receipt, d, p := syntheticSmokeJob(t)
	r := d.runs[0].data
	if err := validateHost(hostedSmokeClass, "synthetic-host", r); err != nil {
		t.Fatal(err)
	}
	if err := validateSmokeJob(receipt, d); err != nil {
		t.Fatal(err)
	}
	if _, err := check(p, d, "nightly"); err == nil {
		t.Fatal("hosted smoke evidence admitted to nightly")
	}
	if _, err := check(p, d, "release"); err == nil {
		t.Fatal("hosted smoke evidence admitted to release")
	}
	r.Host.Profile = "reference"
	r.Host.Validated = true
	r.Host.Limits["cpu_quota"] = metric{Value: pointer(4), Unit: "cores"}
	if err := validateHost("reference_linux", "synthetic-host", r); err == nil {
		t.Fatal("four-CPU hosted machine became reference capacity")
	}
}

func TestLinuxOOMSafetyRequiresActualCounterAndLogCensus(t *testing.T) {
	_, d, _ := syntheticSmokeJob(t)
	r := d.runs[0].data
	if _, err := extract("oom_kills", hostedSmokeClass, r); err == nil {
		t.Fatal("missing authoritative OOM counter became zero")
	}
	r.Phases[0].Metrics["scenario_cgroup_oom_kills"] = metric{Value: pointer(0), Unit: "events"}
	if value, err := extract("oom_kills", hostedSmokeClass, r); err != nil || value != 0 {
		t.Fatalf("measured zero rejected: %v %v", value, err)
	}
	r.Phases[0].Metrics["scenario_cgroup_oom_kills"] = metric{Value: pointer(1), Unit: "events"}
	if value, err := extract("oom_kills", hostedSmokeClass, r); err != nil || value != 1 {
		t.Fatal("actual OOM hidden by zero log lines")
	}
	if _, err := extract("oom_kills", "reference_mac", r); err == nil {
		t.Fatal("Mac log lines substituted for authoritative OOM evidence")
	}
}
