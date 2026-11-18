package citest

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

func automaticMaintenanceStarts(t *testing.T, trace string) int {
	t.Helper()
	data, err := os.ReadFile(trace)
	if err != nil {
		t.Fatal(err)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	starts := 0
	for {
		var event struct {
			Event string   `json:"event"`
			Argv  []string `json:"argv"`
		}
		if err := decoder.Decode(&event); err == io.EOF {
			return starts
		} else if err != nil {
			t.Fatalf("decode Git Trace2: %v", err)
		}
		if event.Event != "child_start" || !slices.Contains(event.Argv, "--auto") {
			continue
		}
		if slices.Contains(event.Argv, "maintenance") || slices.Contains(event.Argv, "gc") {
			starts++
		}
	}
}

func TestSelectionFixtureDoesNotStartAutomaticMaintenance(t *testing.T) {
	scratch := t.TempDir()
	config := filepath.Join(scratch, "gitconfig")
	// Request real maintenance through host and repository configuration.
	// Keep the control and a regressed fixture synchronous, so even a failed
	// assertion cannot leave a detached process writing the temporary repo.
	if err := os.WriteFile(config, []byte(`[maintenance]
	auto = true
	autoDetach = false
[maintenance "commit-graph"]
	enabled = true
	auto = 1
[maintenance "gc"]
	enabled = false
[gc]
	auto = 1
	autoDetach = false
`), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", config)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	f := newFixture(t)
	f.git("config", "maintenance.auto", "true")
	f.git("config", "gc.auto", "1")
	version := f.git("--version")

	controlTrace := filepath.Join(scratch, "control.jsonl")
	t.Setenv("GIT_TRACE2_EVENT", controlTrace)
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	control := exec.CommandContext(ctx, "git", "-c", "user.name=CI Fixture", "-c", "user.email=ci@example.org",
		"-c", "maintenance.auto=true", "-c", "maintenance.autoDetach=false", "-c", "gc.autoDetach=false",
		"commit", "--allow-empty", "-qm", "maintenance control")
	control.Dir = f.root
	if out, err := control.CombinedOutput(); err != nil {
		t.Fatalf("synchronous control commit: %v\n%s", err, out)
	}
	controlStarts := automaticMaintenanceStarts(t, controlTrace)
	if controlStarts == 0 {
		t.Fatal("control did not exercise automatic maintenance child creation")
	}

	fixtureTrace := filepath.Join(scratch, "fixture.jsonl")
	t.Setenv("GIT_TRACE2_EVENT", fixtureTrace)
	f.change("leaf/value.go")
	f.git("reset", "--hard", f.base)
	fixtureStarts := automaticMaintenanceStarts(t, fixtureTrace)
	t.Logf("%s: synchronous control maintenance starts=%d; fixture maintenance starts=%d", version, controlStarts, fixtureStarts)
	if fixtureStarts != 0 {
		t.Fatalf("fixture launched %d automatic maintenance children", fixtureStarts)
	}
	// A single immediate removal is the lifecycle assertion; no grace period
	// or cleanup retry conceals repository writers outliving their commands.
	if err := os.RemoveAll(f.root); err != nil {
		t.Fatalf("fixture repository was not quiescent after Git returned: %v", err)
	}
}
