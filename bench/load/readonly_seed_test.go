package main

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"
	"time"
)

func TestReadOnlySearchSeedUsesExistingMeasuredCallAndDrain(t *testing.T) {
	args := []string{"--binary", "unused", "--manifest", "unused", "--scratch", t.TempDir(), "--scenario", "read", "--agents", "16", "--edit-agents", "0", "--worktrees", "0", "--host", "native", "--profile", "agent", "--duration", "20s", "--deadline", "120s", "--pr-smoke-drain", "120s", "--pr-smoke-drain-version", "1"}
	off, err := parseRun(args)
	if err != nil {
		t.Fatal(err)
	}
	on, err := parseRun(append(args, "--nightly-workload-seed", "search-delay-read16-v1"))
	if err != nil {
		t.Fatalf("supported read16 seed rejected: %v", err)
	}
	on.NightlySeed = ""
	if !reflect.DeepEqual(off, on) {
		t.Fatal("seed changed original workload, budgets or drain")
	}
	caller := &nightlySeedCaller{}
	ctx := context.WithValue(t.Context(), nightlySeedKey{}, "search-delay-read16-v1")
	_, rows, err := callTool(ctx, caller, request{Tool: "search", Operation: "symbols"}, nil, "workload", 0, 0, 0, 3*time.Second, waitContext)
	if err != nil || caller.calls != 1 || len(rows) != 1 || rows[0].NightlySeed != "search-delay-read16-v1" || rows[0].NightlyDelayNS < int64(time.Second) || rows[0].LogicalMS < 1000 {
		t.Fatalf("delay not actually measured before one RPC: rows=%+v err=%v calls=%d", rows, err, caller.calls)
	}
	_, rows, err = callTool(ctx, caller, request{Tool: "read", Operation: "source"}, nil, "workload", 0, 0, 1, time.Second, waitContext)
	if err != nil || rows[0].NightlyDelayNS != 0 || rows[0].NightlySeed != "" {
		t.Fatal("seed leaked to other tool")
	}
	_, rows, err = callTool(ctx, caller, request{Tool: "search", Operation: "symbols"}, nil, "probe", 0, 0, 1, time.Second, waitContext)
	if err != nil || rows[0].NightlyDelayNS != 0 {
		t.Fatal("seed leaked to probe")
	}
	data, err := json.Marshal(rows[0])
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}
	if _, ok := raw["nightly_workload_seed"]; ok {
		t.Fatal("ordinary wire/report shape changed")
	}
}

func TestReadOnlySearchSeedRejectsForeignTopologyAndDeadline(t *testing.T) {
	base := runOptions{NightlySeed: "search-delay-read16-v1", Scenario: "read", Agents: 16, Host: "native", Profile: "agent", Duration: 20 * time.Second, Deadline: 120 * time.Second, PRSmokeDrain: 120 * time.Second, PRSmokeDrainVersion: 1}
	for name, change := range map[string]func(*runOptions){
		"editor":        func(o *runOptions) { o.Editors = 1 },
		"other_readers": func(o *runOptions) { o.Agents = 8 },
		"edit_protocol": func(o *runOptions) { o.PRSmokeDrainVersion = 2 },
		"worktree":      func(o *runOptions) { o.Worktrees = 1 },
		"wrong_kind":    func(o *runOptions) { o.NightlySeed = "search-delay-v1" },
	} {
		t.Run(name, func(t *testing.T) {
			o := base
			change(&o)
			if validateNightlyWorkloadSeed(o) == nil {
				t.Fatal("foreign read-only seed admitted")
			}
		})
	}
	caller := &nightlySeedCaller{}
	ctx, cancel := context.WithTimeout(context.WithValue(t.Context(), nightlySeedKey{}, "search-delay-read16-v1"), 5*time.Millisecond)
	defer cancel()
	_, rows, err := callTool(ctx, caller, request{Tool: "search", Operation: "symbols"}, nil, "workload", 0, 0, 0, time.Second, waitContext)
	if err == nil || caller.calls != 0 || len(rows) != 1 || rows[0].Outcome != "phase_censored" {
		t.Fatalf("cancelled delay dispatched or lost censoring: %+v %v", rows, err)
	}
}
