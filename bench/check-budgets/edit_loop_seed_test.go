package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func editLoopSeedConfig(t *testing.T, seed, lane string) string {
	t.Helper()
	config := fixtureLaneConfig()
	config.HostClass, config.Lane = "reference_linux", lane
	data, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	var wire map[string]any
	if err := json.Unmarshal(data, &wire); err != nil {
		t.Fatal(err)
	}
	wire["edit_gate_seed"] = seed
	data, err = json.Marshal(wire)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestEditLoopCollectorSeedUsesExplicitTaskAndActualProcessArguments(t *testing.T) {
	for _, mode := range []string{"ack-wait", "closed-read-gate"} {
		t.Run(mode, func(t *testing.T) {
			path := editLoopSeedConfig(t, mode, "smoke")
			config, err := readLaneConfig(path, true)
			if err != nil {
				t.Fatalf("explicit seed configuration cannot reach collector: %v", err)
			}
			plan, err := makeEditLoopLanePlan(policy{}, config)
			if err != nil {
				t.Fatal(err)
			}
			if len(plan.Tasks) != 3 {
				t.Fatalf("seed lost three consecutive runs: %v", plan.Tasks)
			}
			for _, task := range plan.Tasks {
				data, err := json.Marshal(task)
				if err != nil {
					t.Fatal(err)
				}
				var wire map[string]any
				if err := json.Unmarshal(data, &wire); err != nil {
					t.Fatal(err)
				}
				if wire["edit_gate_seed"] != mode {
					t.Fatalf("actual task lost fault identity: %s", data)
				}
				args := taskArgs(config, task, "manifest", "scratch", "report")
				index := slices.Index(args, "--edit-gate-seed")
				if index < 0 || index+1 >= len(args) || args[index+1] != mode {
					t.Fatalf("actual process cannot activate fault: %v", args)
				}
			}
			if _, err := readLaneConfig(path); err == nil {
				t.Fatal("ordinary collector accepted edit gate fault")
			}
		})
	}
	for _, cell := range []struct{ seed, lane string }{{"unregistered", "smoke"}, {"ack-wait", "nightly"}} {
		if _, err := readLaneConfig(editLoopSeedConfig(t, cell.seed, cell.lane), true); err == nil {
			t.Fatal("fault accepted outside the closed smoke-only contract")
		}
	}
}

func TestEditLoopSixRepoReadP95RemainsVisibleWithoutInventedBudget(t *testing.T) {
	var out, diagnostics bytes.Buffer
	code := run([]string{"edit-loop", "--budgets", "../budgets.yaml", "--scenario", "multirepoedit-6", "--inputs", filepath.Join(t.TempDir(), "absent.json")}, &out, &diagnostics)
	if code != 1 {
		t.Fatalf("missing evidence exit%d, want1", code)
	}
	found := false
	for _, line := range strings.Split(out.String(), "\n") {
		if !strings.Contains(line, "readback_p95") {
			continue
		}
		found = true
		if !strings.Contains(line, "UNAVAILABLE") || strings.Contains(line, "PASS") {
			t.Fatalf("unmeasured six-repo observation claims a verdict: %s", line)
		}
	}
	if !found {
		t.Fatal("six-repository edit plus one-read p95 disappeared instead of being explicitly unbudgeted")
	}
}

func TestSixRepoObservationUsesActualRepeatedSamplesWithoutThreshold(t *testing.T) {
	d := dataset{config: inputs{Repeats: 3}}
	for repeat := 1; repeat <= 3; repeat++ {
		d.runs = append(d.runs, observation{input: runInput{Role: "candidate", HostClass: "reference_linux", Scenario: "multirepoedit-6", Repeat: repeat}, data: editLoopEvidenceFixture(t)})
	}
	row := assessSixRepoReadObservation(d, nil)
	if row.State != "OBSERVATIONAL" || row.Value == nil || *row.Value != .1 || row.Budget != nil || editLoopRowBlocks(row) {
		t.Fatalf("observed p95 acquired an invented verdict: %+v", row)
	}
	row.Metric = "readback_max"
	if !editLoopRowBlocks(row) {
		t.Fatal("observation qualification weakened universal hard cap")
	}
	row = assessSixRepoReadObservation(dataset{config: d.config, runs: d.runs[:2]}, nil)
	if row.State != "UNAVAILABLE" || row.Value != nil || !editLoopRowBlocks(row) {
		t.Fatalf("missing repeat became a numerical observation: %+v", row)
	}
	d.runs = append(d.runs, d.runs[0])
	if row := assessSixRepoReadObservation(d, nil); row.State != "UNAVAILABLE" || row.Value != nil {
		t.Fatalf("duplicate repeat credited twice: %+v", row)
	}
}
