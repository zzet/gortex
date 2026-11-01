package main

import (
	"os"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestPRSmokeWorkflowIsReportOnly(t *testing.T) {
	data, err := os.ReadFile("../../.github/workflows/perf-gate.yml")
	if err != nil {
		t.Fatal(err)
	}
	var workflow struct {
		Jobs map[string]struct {
			Name            string            `yaml:"name"`
			If              string            `yaml:"if"`
			RunsOn          any               `yaml:"runs-on"`
			ContinueOnError bool              `yaml:"continue-on-error"`
			Permissions     map[string]string `yaml:"permissions"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(data, &workflow); err != nil {
		t.Fatal(err)
	}
	smoke, ok := workflow.Jobs["smoke"]
	if !ok {
		t.Fatal("missing PR smoke job")
	}
	if !smoke.ContinueOnError {
		t.Fatal("PR smoke must tolerate failures at job scope, including setup and artifact failures")
	}
	if !strings.Contains(smoke.Name, "report-only") {
		t.Fatal("PR smoke must identify its report-only status")
	}
	if smoke.If != "github.event_name == 'pull_request' && vars.PERF_PR_SMOKE_ENABLED == 'true'" || smoke.RunsOn != "ubuntu-24.04-arm" {
		t.Fatal("PR smoke must remain opt-in on the hosted ARM runner")
	}
	if len(smoke.Permissions) != 2 || smoke.Permissions["contents"] != "read" || smoke.Permissions["actions"] != "read" {
		t.Fatal("PR smoke must retain a read-only token")
	}
}
