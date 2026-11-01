package main

import (
	"bytes"
	"encoding/json"
	"slices"
	"testing"
)

func TestEditLoopLanePlanKeepsSmokeAndNightlyDenominators(t *testing.T) {
	for _, lane := range []string{"smoke", "nightly"} {
		t.Run(lane, func(t *testing.T) {
			var out, diagnostics bytes.Buffer
			exit := run([]string{"edit-plan", "--lane", lane, "--host-class", "reference_linux", "--budgets", "../budgets.yaml"}, &out, &diagnostics)
			if exit != 1 {
				t.Fatalf("unconfigured planning must retain missing runtime prerequisites, exit%d: %s", exit, diagnostics.String())
			}
			var plan struct {
				Execution string `json:"execution"`
				Ready     bool   `json:"ready"`
				Tasks     []struct {
					Scenario string   `json:"scenario"`
					Role     string   `json:"role"`
					Count    int      `json:"edit_count"`
					Modes    []string `json:"probe_modes"`
					Gate     bool     `json:"edit_gate"`
				} `json:"tasks"`
			}
			if err := json.Unmarshal(out.Bytes(), &plan); err != nil {
				t.Fatalf("no reviewable edit plan: %v: %s", err, out.String())
			}
			if plan.Ready || plan.Execution != "edit-loop" {
				t.Fatalf("unconfigured execution claims ready or wrong surface: %+v", plan)
			}
			counts := map[string]int{}
			for _, task := range plan.Tasks {
				if task.Role != "candidate" || task.Count != 100 || !task.Gate || !slices.Equal(task.Modes, []string{"json", "toon", "compact", "facade", "cli"}) {
					t.Errorf("gate task changed actual denominator/surfaces: %+v", task)
				}
				counts[task.Scenario]++
			}
			want := []string{"loadedit-4"}
			if lane == "nightly" {
				want = []string{"loadedit-1", "loadedit-4", "loadedit-8", "loadedit-16", "loadedit-32", "multirepoedit-6", "loadedit-4-largefile"}
			}
			if len(counts) != len(want) {
				t.Errorf("wrong matrix: %+v", counts)
			}
			for _, name := range want {
				if counts[name] != 3 {
					t.Errorf("%s has%d repeats, want3", name, counts[name])
				}
			}
		})
	}
}

func TestEditLoopTaskActuallySelectsGateInsteadOfLegacyEditTraffic(t *testing.T) {
	for _, scenario := range []string{"loadedit-4", "multirepoedit-6"} {
		t.Run(scenario, func(t *testing.T) {
			var task laneTask
			if err := json.Unmarshal([]byte(`{"scenario":"`+scenario+`","role":"candidate","repeat":1,"edit_gate":true}`), &task); err != nil {
				t.Fatal(err)
			}
			c := fixtureLaneConfig()
			c.HostClass = "reference_linux"
			args := taskArgs(c, task, "manifest.json", "state", "report.json")
			arg := func(name string) string {
				for i := range args {
					if args[i] == name && i+1 < len(args) {
						return args[i+1]
					}
				}
				return ""
			}
			if arg("--scenario") != "edit-and-probe" || arg("--agents") != "0" || arg("--edit-count") != "100" || !slices.Contains(args, "--edit-gate-modes") {
				t.Errorf("actual process still runs legacy edit traffic: %v", args)
			}
			if scenario == "multirepoedit-6" && (arg("--edit-repos") != "6" || arg("--edit-agents") != "6") {
				t.Errorf("six-repository row lost real routing: %v", args)
			}
		})
	}
}
