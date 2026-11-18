package main

import (
	"os"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestEditLoopWorkflowHasRealBlockingReferenceLanesAndRetainedEvidence(t *testing.T) {
	data, err := os.ReadFile("../../.github/workflows/edit-loop-gate.yml")
	if err != nil {
		t.Fatalf("edit gate has no executable workflow: %v", err)
	}
	var workflow struct {
		Permissions map[string]string `yaml:"permissions"`
		Concurrency struct {
			Cancel bool `yaml:"cancel-in-progress"`
		} `yaml:"concurrency"`
		Env  map[string]string `yaml:"env"`
		Jobs map[string]struct {
			If       string   `yaml:"if"`
			RunsOn   []string `yaml:"runs-on"`
			Continue bool     `yaml:"continue-on-error"`
			Steps    []struct {
				If   string `yaml:"if"`
				Uses string `yaml:"uses"`
				Run  string `yaml:"run"`
			} `yaml:"steps"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(data, &workflow); err != nil {
		t.Fatal(err)
	}
	if workflow.Permissions["contents"] != "read" || len(workflow.Permissions) != 1 || workflow.Concurrency.Cancel || workflow.Env["CGO_ENABLED"] != "1" {
		t.Fatal("workflow changed read-only/CGO/owned cleanup contract")
	}
	for _, lane := range []string{"smoke", "nightly"} {
		job, ok := workflow.Jobs[lane]
		if !ok || job.Continue || !strings.Contains(job.If, "refs/heads/main") || !strings.Contains(job.If, "ENABLED") || strings.Join(job.RunsOn, ",") != "self-hosted,perf-reference-linux" {
			t.Fatalf("%s lane missing explicit trusted reference gate: %+v", lane, job)
		}
		collects, artifacts := 0, 0
		for _, step := range job.Steps {
			if strings.Contains(step.Run, "edit-collect") {
				collects++
				for _, flag := range []string{"--expected-candidate", "--candidate-binary", "--load-binary", "--require-lane " + lane} {
					if !strings.Contains(step.Run, flag) {
						t.Errorf("%s collector lost %s", lane, flag)
					}
				}
			}
			if strings.Contains(step.Uses, "actions/upload-artifact@") && step.If == "always()" {
				artifacts++
			}
		}
		if collects != 1 || artifacts != 1 {
			t.Fatalf("%s has no actual strict collection/retained failure artifact", lane)
		}
	}
	for _, proof := range []string{"ack-wait", "closed-read-gate", "gortex_edit_gate_seed", "edit_gate_seed", "EDIT_GATE_REFERENCE_SMOKE_CONFIG", "EDIT_GATE_REFERENCE_NIGHTLY_CONFIG"} {
		if !strings.Contains(string(data), proof) {
			t.Errorf("workflow has no actual %s contract", proof)
		}
	}
}
