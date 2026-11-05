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
	if smoke.If != "(github.event_name == 'pull_request' && vars.PERF_PR_SMOKE_ENABLED == 'true') || (github.event_name == 'workflow_dispatch' && github.ref == 'refs/heads/main' && inputs.run_smoke)" || smoke.RunsOn != "ubuntu-24.04-arm" {
		t.Fatal("PR smoke must remain opt-in on the hosted ARM runner")
	}
	if len(smoke.Permissions) != 2 || smoke.Permissions["contents"] != "read" || smoke.Permissions["actions"] != "read" {
		t.Fatal("PR smoke must retain a read-only token")
	}
}

func TestPerformanceTestBuildWorkflowsPinCGO(t *testing.T) {
	for _, name := range []string{"perf-main.yml", "perf-gate.yml"} {
		t.Run(name, func(t *testing.T) {
			data, err := os.ReadFile("../../.github/workflows/" + name)
			if err != nil {
				t.Fatal(err)
			}
			var workflow struct {
				Env  map[string]string `yaml:"env"`
				Jobs map[string]struct {
					Env   map[string]string `yaml:"env"`
					Steps []struct {
						Run string            `yaml:"run"`
						Env map[string]string `yaml:"env"`
					} `yaml:"steps"`
				} `yaml:"jobs"`
			}
			if err := yaml.Unmarshal(data, &workflow); err != nil {
				t.Fatal(err)
			}
			builds := 0
			for jobName, job := range workflow.Jobs {
				for _, step := range job.Steps {
					if !strings.Contains(step.Run, "prepare-tests") {
						continue
					}
					builds++
					env := make(map[string]string)
					for _, scope := range []map[string]string{workflow.Env, job.Env, step.Env} {
						for key, value := range scope {
							env[key] = value
						}
					}
					for key, want := range map[string]string{
						"CGO_ENABLED":  "1",
						"CGO_CFLAGS":   "-O2 -g -fno-strict-aliasing",
						"CGO_CXXFLAGS": "-O2 -g -fno-strict-aliasing",
					} {
						if env[key] != want {
							t.Errorf("%s prepare-tests environment %s = %q, want %q", jobName, key, env[key], want)
						}
					}
				}
			}
			if builds == 0 {
				t.Fatal("workflow has no test-executable preparation step")
			}
		})
	}
}
