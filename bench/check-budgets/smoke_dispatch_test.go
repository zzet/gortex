package main

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func dispatchSmokeFixture(t *testing.T, mutation string) (laneReceipt, dataset) {
	t.Helper()
	receipt, d, _ := syntheticSmokeJob(t)
	data, err := json.Marshal(receipt)
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	if err = json.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}
	workflow := raw["workflow"].(map[string]any)
	workflow["event"], workflow["ref"] = "workflow_dispatch", "refs/heads/main"
	pins := map[string]any{"workflow_ref": "zzet/gortex/.github/workflows/perf-gate.yml@refs/heads/main", "workflow_revision": receipt.Workflow.Revision, "baseline_revision": receipt.Smoke.Main.Revision, "baseline_artifact_id": receipt.Smoke.Main.Artifact.ID, "baseline_artifact_digest": receipt.Smoke.Main.Artifact.Digest, "candidate_revision": receipt.Workflow.Revision}
	workflow["smoke_dispatch"] = pins
	switch mutation {
	case "missing_pins":
		delete(workflow, "smoke_dispatch")
	case "wrong_baseline":
		pins["baseline_revision"] = strings.Repeat("d", 40)
	case "wrong_artifact_id":
		pins["baseline_artifact_id"] = 999
	case "missing_artifact_digest":
		delete(pins, "baseline_artifact_digest")
	case "wrong_artifact_digest":
		pins["baseline_artifact_digest"] = "sha256:" + strings.Repeat("d", 64)
	case "wrong_candidate":
		pins["candidate_revision"] = strings.Repeat("d", 40)
	case "wrong_workflow_revision":
		pins["workflow_revision"] = strings.Repeat("d", 40)
	case "wrong_workflow_ref":
		pins["workflow_ref"] = "zzet/gortex/.github/workflows/other.yml@refs/heads/main"
	case "missing_job":
		workflow["job"] = "other"
	case "bare_run_id":
		workflow["run_url"] = "789"
	case "branch_ref":
		workflow["ref"] = "refs/heads/feature"
	case "pr_with_dispatch_pins":
		workflow["event"], workflow["ref"] = "pull_request", "refs/pull/123/merge"
	case "missing_candidate_hash":
		raw["smoke_provenance"].(map[string]any)["candidate_binary"].(map[string]any)["binary_sha256"] = ""
	}
	data, err = json.Marshal(raw)
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(data, &receipt); err != nil {
		t.Fatal(err)
	}
	return receipt, d
}

func TestManualSmokeRequiresExactTrustedRolePins(t *testing.T) {
	receipt, d := dispatchSmokeFixture(t, "")
	if err := validateSmokeJob(receipt, d); err != nil {
		t.Fatal(err)
	}
	for _, mutation := range []string{"missing_pins", "wrong_baseline", "wrong_artifact_id", "missing_artifact_digest", "wrong_artifact_digest", "wrong_candidate", "wrong_workflow_revision", "wrong_workflow_ref", "branch_ref", "missing_job", "bare_run_id", "pr_with_dispatch_pins", "missing_candidate_hash"} {
		t.Run(mutation, func(t *testing.T) {
			receipt, d := dispatchSmokeFixture(t, mutation)
			if err := validateSmokeJob(receipt, d); err == nil {
				t.Fatal("manual source/artifact identity mismatch was accepted")
			}
		})
	}
}

func TestManualSmokeWorkflowSelectsExistingBoundedLane(t *testing.T) {
	data, err := os.ReadFile("../../.github/workflows/perf-gate.yml")
	if err != nil {
		t.Fatal(err)
	}
	var workflow struct {
		On struct {
			Dispatch struct {
				Inputs map[string]struct {
					Type    string `yaml:"type"`
					Default string `yaml:"default"`
				} `yaml:"inputs"`
			} `yaml:"workflow_dispatch"`
		} `yaml:"on"`
		Jobs map[string]struct {
			If    string            `yaml:"if"`
			Env   map[string]string `yaml:"env"`
			Steps []struct {
				Name string            `yaml:"name"`
				Run  string            `yaml:"run"`
				With map[string]string `yaml:"with"`
			} `yaml:"steps"`
		} `yaml:"jobs"`
	}
	if err = yaml.Unmarshal(data, &workflow); err != nil {
		t.Fatal(err)
	}
	inputs := workflow.On.Dispatch.Inputs
	for _, key := range []string{"smoke_baseline_revision", "smoke_baseline_artifact_id", "smoke_baseline_artifact_digest", "smoke_candidate_revision"} {
		if inputs[key].Type != "string" {
			t.Errorf("manual smoke lacks explicit resolved role pin %s", key)
		}
	}
	for _, key := range []string{"run_smoke", "retain_read_source_failures"} {
		if inputs[key].Type != "boolean" || inputs[key].Default != "false" {
			t.Errorf("manual smoke %s must default off", key)
		}
	}
	for _, job := range []string{"nightly-reference", "nightly-mac"} {
		if !strings.Contains(workflow.Jobs[job].If, "!(github.event_name == 'workflow_dispatch' && inputs.run_smoke)") {
			t.Errorf("explicit manual smoke must not also select %s", job)
		}
	}
	job := workflow.Jobs["smoke"]
	if job.If != "(github.event_name == 'pull_request' && vars.PERF_PR_SMOKE_ENABLED == 'true') || (github.event_name == 'workflow_dispatch' && github.ref == 'refs/heads/main' && inputs.run_smoke)" {
		t.Fatal("manual route must be explicit trusted-main-only and preserve PR opt-in")
	}
	for _, key := range []string{"PERF_SMOKE_BASELINE_REVISION", "PERF_SMOKE_BASELINE_ARTIFACT_ID", "PERF_SMOKE_BASELINE_ARTIFACT_DIGEST", "PERF_SMOKE_CANDIDATE_REVISION"} {
		if !strings.Contains(job.Env[key], "inputs.smoke_") {
			t.Errorf("missing dispatch input binding %s", key)
		}
	}
	var build, retrieve, measure, upload string
	for _, step := range job.Steps {
		switch step.Name {
		case "Build clean candidate and owned measurement commands":
			build = step.Run
		case "Retrieve the exact main pinned at PR planning time":
			retrieve = step.Run
		case "Run five standalone steps and same-job microbenchmark comparison":
			measure = step.Run
		case "Preserve attempted smoke reports and failures":
			upload = step.With["path"]
		}
	}
	for _, required := range []string{"git rev-parse HEAD", "GITHUB_SHA", "PERF_SMOKE_CANDIDATE_REVISION", "perf-role-builds.json", "sha256sum"} {
		if !strings.Contains(build, required) {
			t.Errorf("same-job build omitted source/artifact binding %s", required)
		}
	}
	if !strings.Contains(retrieve, "PERF_MAIN_REVISION") {
		t.Fatal("baseline must use exact selected revision")
	}
	for _, required := range []string{"--offer 20s", "--fixture", "--candidate-builds", "--main-bundle", "retain_read_source_failures"} {
		if !strings.Contains(measure, required) {
			t.Errorf("complete smoke path lost %s", required)
		}
	}
	for _, required := range []string{"perf-role-builds.json", "perf-main/selection.json", "perf-main/main-artifact.json", "workload/runs/*/report.json", "microbench/*.json"} {
		if !strings.Contains(upload, required) {
			t.Errorf("explicit role or measurement evidence lost %s", required)
		}
	}
}

// Existing microbundle admission compares workflow structs by value. The
// explicit pins must survive JSON and independently reconstructed observations.
func TestManualSmokeWorkflowPinsKeepExistingValueComparisons(t *testing.T) {
	receipt, _ := dispatchSmokeFixture(t, "")
	data, err := json.Marshal(receipt.Workflow)
	if err != nil {
		t.Fatal(err)
	}
	var roundtrip workflowProvenance
	if err = json.Unmarshal(data, &roundtrip); err != nil {
		t.Fatal(err)
	}
	if roundtrip != receipt.Workflow {
		t.Fatal("same-job manual provenance cannot survive existing microbundle equality guards")
	}
	pr := workflowProvenance{Event: "pull_request"}
	data, err = json.Marshal(pr)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "smoke_dispatch") {
		t.Fatal("default PR provenance gained empty manual pins")
	}
}

func TestManualSmokeCurrentWorkflowRetainsExplicitDispatchInputs(t *testing.T) {
	receipt, _ := dispatchSmokeFixture(t, "")
	for key, value := range map[string]string{
		"GITHUB_EVENT_NAME": "workflow_dispatch", "GITHUB_REF": "refs/heads/main", "GITHUB_SHA": receipt.Workflow.Revision,
		"GITHUB_WORKFLOW_REF": "zzet/gortex/.github/workflows/perf-gate.yml@refs/heads/main", "GITHUB_WORKFLOW_SHA": receipt.Workflow.Revision,
		"GITHUB_SERVER_URL": "https://github.com", "GITHUB_REPOSITORY": "zzet/gortex", "GITHUB_RUN_ID": "789", "GITHUB_JOB": "smoke", "GITHUB_RUN_ATTEMPT": "1",
		"PERF_SMOKE_BASELINE_REVISION": receipt.Smoke.Main.Revision, "PERF_SMOKE_BASELINE_ARTIFACT_ID": "456", "PERF_SMOKE_BASELINE_ARTIFACT_DIGEST": receipt.Smoke.Main.Artifact.Digest, "PERF_SMOKE_CANDIDATE_REVISION": receipt.Workflow.Revision,
	} {
		t.Setenv(key, value)
	}
	got := currentBuildWorkflow(receipt.Workflow.Revision)
	if got != receipt.Workflow {
		t.Fatal("actual job environment lost explicit manual source/artifact pins")
	}
	if err := validateSmokeWorkflow(got); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PERF_SMOKE_BASELINE_ARTIFACT_ID", "invalid")
	if err := validateSmokeWorkflow(currentBuildWorkflow(receipt.Workflow.Revision)); err == nil {
		t.Fatal("invalid artifact ID became an implicit artifact selection")
	}
	t.Setenv("GITHUB_EVENT_NAME", "pull_request")
	got = currentBuildWorkflow(receipt.Workflow.Revision)
	data, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "smoke_dispatch") {
		t.Fatal("manual environment leaked into unchanged PR provenance")
	}
}

func TestUnrelatedManualWorkflowKeepsOriginalProvenance(t *testing.T) {
	for _, job := range []string{"nightly-reference", "nightly-mac", "main-linux-arm64"} {
		t.Run(job, func(t *testing.T) {
			for key, value := range map[string]string{
				"GITHUB_EVENT_NAME": "workflow_dispatch", "GITHUB_REF": "refs/heads/main", "GITHUB_JOB": job,
				"GITHUB_WORKFLOW_REF": "zzet/gortex/.github/workflows/perf-gate.yml@refs/heads/main", "GITHUB_WORKFLOW_SHA": strings.Repeat("c", 40),
				"PERF_SMOKE_BASELINE_REVISION": strings.Repeat("a", 40), "PERF_SMOKE_BASELINE_ARTIFACT_ID": "456", "PERF_SMOKE_BASELINE_ARTIFACT_DIGEST": "sha256:" + strings.Repeat("b", 64), "PERF_SMOKE_CANDIDATE_REVISION": strings.Repeat("c", 40),
			} {
				t.Setenv(key, value)
			}
			observed := currentBuildWorkflow(strings.Repeat("c", 40))
			data, err := json.Marshal(observed)
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("UNRELATED_MANUAL_PROVENANCE_OBSERVED job=%s event=%s", job, observed.Event)
			if strings.Contains(string(data), "smoke_dispatch") {
				t.Fatal("unrelated manual workflow gained smoke-specific source/artifact pins")
			}
			if err := validateSmokeWorkflow(observed); err == nil {
				t.Fatal("an unrelated job cannot become an admitted smoke measurement")
			}
		})
	}
}
