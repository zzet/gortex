package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func fixtureLaneConfig() laneConfig {
	return laneConfig{Version: 1, Lane: "smoke", HostClass: "reference_linux", Machine: "synthetic-host", Load: "/synthetic/load", Manifest: "manifest.json", Builds: map[string]laneBuild{
		"candidate": {Binary: "candidate", Revision: strings.Repeat("a", 40)}, "main": {Binary: "main", Revision: strings.Repeat("d", 40)}, "tag": {Binary: "tag", Revision: shippedRevision},
	}, Repeats: 3, Duration: "60s", Deadline: "120s", WarmTimeout: "30m", StopTimeout: "30s", SweepDuration: "10m", SweepRepetitions: 15}
}

func TestLanePlanPreservesFormalGapsAndReleaseCounts(t *testing.T) {
	data, err := os.ReadFile("../budgets.yaml")
	if err != nil {
		t.Fatal(err)
	}
	p, err := loadPolicy(data)
	if err != nil {
		t.Fatal(err)
	}
	c := fixtureLaneConfig()
	for _, lane := range []string{"smoke", "nightly", "release"} {
		c.Lane = lane
		plan, err := makeLanePlan(p, c)
		if err != nil {
			t.Fatal(err)
		}
		if plan.Ready || len(plan.Prerequisites) == 0 || plan.PolicySHA256 == "" {
			t.Fatal("frozen wire gaps became ready")
		}
		counts := map[string]int{}
		for _, task := range plan.Tasks {
			if task.Role == "candidate" {
				counts[task.Scenario]++
			}
		}
		if lane == "smoke" {
			for _, scenario := range []string{"load-16", "loadedit-4", "tools-single-gortex", "warm-restart", "idle-stop"} {
				if counts[scenario] < 3 {
					t.Fatalf("smoke omitted %s", scenario)
				}
			}
		}
		if lane == "release" && (counts["warm-restart"] < 10 || counts["idle-stop-20"] < 20) {
			t.Fatal("release lifecycle repetition minima reduced")
		}
		if lane != "smoke" && counts["loadedit-32"] < 3 {
			t.Fatal("full load matrix reduced")
		}
	}
}

func TestLaneOrderingAndNoExtraReadersOrTagForBlock(t *testing.T) {
	p := testPolicy()
	p.Budgets = []budget{{Scenario: "loadedit-4", Metric: "edit_ack_p95", Mode: "block", HostClass: "reference_linux", Lane: []string{"smoke"}}}
	c := fixtureLaneConfig()
	plan, err := makeLanePlan(p, c)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Tasks) != 6 || plan.Tasks[0].Role == plan.Tasks[2].Role {
		t.Fatal("roles not rotated or unnecessary tag runs added")
	}
	args := taskArgs(c, plan.Tasks[0], "manifest", "scratch", "report")
	for flag, want := range map[string]string{"--agents": "0", "--edit-agents": "4", "--scenario": "edit", "--profile": "agent", "--host": "reference"} {
		i := slices.Index(args, flag)
		if i < 0 || args[i+1] != want {
			t.Fatalf("wrong %s: %v", flag, args)
		}
	}
	for i := 0; i < len(plan.Tasks); i += 2 {
		if plan.Tasks[i].Seed != plan.Tasks[i+1].Seed {
			t.Fatal("matched roles have different seeds")
		}
	}
}

func TestLaneConfigStrictPathsAndFloorRefusal(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "lane.json")
	c := fixtureLaneConfig()
	if err := writeNewJSON(path, c); err != nil {
		t.Fatal(err)
	}
	loaded, err := readLaneConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Manifest != filepath.Join(dir, "manifest.json") || !filepath.IsAbs(loaded.Load) {
		t.Fatal("config-relative paths not resolved")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, bytes.Replace(data, []byte(`"schema_version"`), []byte(`"unknown"`), 1), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readLaneConfig(path); err == nil {
		t.Fatal("unknown config field accepted")
	}
	c.HostClass = "floor"
	data, err = json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readLaneConfig(path); err == nil {
		t.Fatal("floor relabeled as a reviewed reference")
	}
}

func TestCollectorRetainsFailedRepetitionsAndContinues(t *testing.T) {
	parent := t.TempDir()
	source := filepath.Join(parent, "source")
	if err := os.Mkdir(source, 0o700); err != nil {
		t.Fatal(err)
	}
	c := fixtureLaneConfig()
	c.Manifest = filepath.Join(parent, "manifest.json")
	if err := writeNewJSON(c.Manifest, []map[string]any{{"label": "gortex", "source": source, "commit": strings.Repeat("c", 40)}}); err != nil {
		t.Fatal(err)
	}
	p := testPolicy()
	p.Budgets = []budget{{Metric: "read_p95", Scenario: "load-1", HostClass: c.HostClass, Mode: "block", Target: pointer(1), Unit: "s", Compare: "le", Lane: []string{"smoke"}}}
	plan, err := makeLanePlan(p, c)
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	fake := func(_ context.Context, _ string, args []string, log io.Writer) error {
		calls++
		fmtError := errors.New("synthetic workload failure")
		_, err := io.WriteString(log, "retained failed workload\n")
		if err != nil {
			t.Fatal(err)
		}
		return fmtError
	}
	out := filepath.Join(parent, "evidence")
	if err := collectLane(context.Background(), c, p, plan, out, io.Discard, fake); err == nil {
		t.Fatal("failed runs passed")
	}
	if calls != 6 {
		t.Fatalf("failed repetition shrank matrix: %d", calls)
	}
	var receipt laneReceipt
	data, err := os.ReadFile(filepath.Join(out, "lane.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &receipt); err != nil {
		t.Fatal(err)
	}
	if len(receipt.Tasks) != 6 || receipt.BudgetPass || receipt.Error == "" {
		t.Fatal("failure receipt incomplete")
	}
	var index inputs
	data, err = os.ReadFile(filepath.Join(out, "inputs.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &index); err != nil {
		t.Fatal(err)
	}
	if len(index.Runs) != 6 {
		t.Fatal("missing reports omitted instead of retained as failed inputs")
	}
	if err := collectLane(context.Background(), c, p, plan, out, io.Discard, fake); err == nil {
		t.Fatal("existing evidence overwritten")
	}
	if calls != 6 {
		t.Fatal("overwriting collection started workloads")
	}
	// Neither a real source child nor a symlink to it may receive evidence.
	if err := validateLaneOutput(c.Manifest, filepath.Join(source, "evidence")); err == nil {
		t.Fatal("evidence allowed inside corpus")
	}
	link := filepath.Join(parent, "source-link")
	if err := os.Symlink(source, link); err != nil {
		t.Fatal(err)
	}
	if err := validateLaneOutput(c.Manifest, filepath.Join(link, "evidence")); err == nil {
		t.Fatal("symlink bypassed read-only source protection")
	}
}
