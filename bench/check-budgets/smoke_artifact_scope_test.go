package main

import (
	"os"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestPRSmokeUploadContainsEvidenceWithoutOwnedState(t *testing.T) {
	data, err := os.ReadFile("../../.github/workflows/perf-gate.yml")
	if err != nil {
		t.Fatal(err)
	}
	var workflow struct {
		Jobs map[string]struct {
			Steps []struct {
				Name string            `yaml:"name"`
				If   string            `yaml:"if"`
				With map[string]string `yaml:"with"`
			} `yaml:"steps"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(data, &workflow); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, step := range workflow.Jobs["smoke"].Steps {
		if step.Name != "Preserve attempted smoke reports and failures" {
			continue
		}
		found = true
		if step.If != "always()" {
			t.Fatal("failed attempts must retain evidence")
		}
		paths := strings.Split(strings.TrimSpace(step.With["path"]), "\n")
		for _, path := range paths {
			if strings.TrimSuffix(strings.TrimSpace(path), "/") == "${{ runner.temp }}/perf-smoke-evidence" {
				t.Error("recursive evidence-root upload includes failed owned HOME/module caches, clones and stores")
			}
		}
		for _, required := range []string{
			"perf-smoke-evidence/*.json",
			"perf-smoke-evidence/workload/runs/sequence.json",
			"perf-smoke-evidence/workload/runs/*/report.json",
			"perf-smoke-evidence/workload/runs/*/logs/",
			"perf-fixture/fixture.json", "perf-candidate-tests/test-build.json",
		} {
			if !strings.Contains(step.With["path"], required) {
				t.Errorf("artifact evidence allowlist omits %s", required)
			}
		}
	}
	if !found {
		t.Fatal("missing smoke evidence upload")
	}
}

func TestPRSmokeResolvesCurrentJobCompilerCachesBeforeHomeIsolation(t *testing.T) {
	data, err := os.ReadFile("../../.github/workflows/perf-gate.yml")
	if err != nil {
		t.Fatal(err)
	}
	var workflow struct {
		Jobs map[string]struct {
			Steps []struct {
				Run string `yaml:"run"`
			} `yaml:"steps"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(data, &workflow); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, step := range workflow.Jobs["smoke"].Steps {
		if !strings.Contains(step.Run, " smoke ") {
			continue
		}
		found = true
		for _, resolution := range []string{`GOCACHE="$(go env GOCACHE)"`, `GOMODCACHE="$(go env GOMODCACHE)"`} {
			if !strings.Contains(step.Run, resolution) {
				t.Errorf("smoke must explicitly reuse the current job's prepared compiler cache before isolating HOME: %s", resolution)
			}
		}
	}
	if !found {
		t.Fatal("missing standalone smoke command")
	}
}
