package main

import (
	"maps"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"gopkg.in/yaml.v3"
)

type workflowEventContract struct {
	On          map[string]yaml.Node `yaml:"on"`
	Permissions map[string]string    `yaml:"permissions"`
}

func readWorkflowEvents(t *testing.T, name string) workflowEventContract {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", ".github", "workflows", name))
	if err != nil {
		t.Fatal(err)
	}
	var workflow workflowEventContract
	if err := yaml.Unmarshal(data, &workflow); err != nil {
		t.Fatal(err)
	}
	return workflow
}

func TestCodeChecksRunForEveryPullRequestBase(t *testing.T) {
	for _, name := range []string{"ci.yml", "security.yml"} {
		t.Run(name, func(t *testing.T) {
			workflow := readWorkflowEvents(t, name)
			event, ok := workflow.On["pull_request"]
			if !ok {
				t.Fatal("normal pull request checks are absent")
			}
			var filters map[string]yaml.Node
			if err := event.Decode(&filters); err != nil {
				t.Fatal(err)
			}
			for _, key := range []string{"branches", "branches-ignore"} {
				if _, filtered := filters[key]; filtered {
					t.Errorf("%s restricts PR bases with %s; dependency-base PR heads receive no checks", name, key)
				}
			}
		})
	}
}

func TestCodeTriggerChangePreservesPushAndMeasurementBoundaries(t *testing.T) {
	for _, name := range []string{"ci.yml", "security.yml"} {
		workflow := readWorkflowEvents(t, name)
		event, ok := workflow.On["push"]
		if !ok {
			t.Fatal("missing main push checks")
		}
		var push struct {
			Branches []string `yaml:"branches"`
		}
		if err := event.Decode(&push); err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(push.Branches, []string{"main"}) {
			t.Errorf("%s changed push scope: %v", name, push.Branches)
		}
		if _, trusted := workflow.On["pull_request_target"]; trusted {
			t.Fatal("untrusted PR source must not switch to target-context execution")
		}
		var request struct {
			Types []string `yaml:"types"`
		}
		event = workflow.On["pull_request"]
		if err := event.Decode(&request); err != nil {
			t.Fatal(err)
		}
		if name == "ci.yml" && !slices.Equal(request.Types, []string{"opened", "synchronize", "reopened", "labeled", "unlabeled"}) {
			t.Fatalf("CI PR activity types changed: %v", request.Types)
		}
		if name == "security.yml" {
			if len(request.Types) != 0 {
				t.Fatal("security must retain its default PR activity types")
			}
			if !maps.Equal(workflow.Permissions, map[string]string{"contents": "read", "security-events": "write"}) {
				t.Fatalf("security token permissions changed: %v", workflow.Permissions)
			}
		}
	}
	// Performance measurement keeps its separate opt-in main-base contract.
	perf := readWorkflowEvents(t, "perf-gate.yml")
	var request struct {
		Branches []string `yaml:"branches"`
	}
	event := perf.On["pull_request"]
	if err := event.Decode(&request); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(request.Branches, []string{"main"}) {
		t.Fatalf("performance trigger scope changed: %v", request.Branches)
	}
}
