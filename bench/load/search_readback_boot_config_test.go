package main

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Stop the owned subprocess before readiness: this controls configuration
// emission only, never effective ACK semantics or a measured search workload.
func TestSearchReadbackBootstrapSelectsLocalACKOnlyForExplicitProfile(t *testing.T) {
	t.Setenv("LOAD_DAEMON_FIXTURE", "crash")
	base := gitFixture(t)
	corpus := editProbeRenameCorpus(t, "go", 1000)
	corpus.EditTool = "edit.file"
	corpus.Source = base.Source
	if err := os.WriteFile(filepath.Join(corpus.Source, corpus.File), []byte(corpus.EditInitialContent), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"add", corpus.File}, {"commit", "--quiet", "-m", "owned search bootstrap fixture"}, {"rev-parse", "HEAD"}} {
		cmd := exec.Command("git", append([]string{"-C", corpus.Source, "-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "-c", "core.hooksPath=" + os.DevNull}, args...)...)
		data, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v %s", args, err, data)
		}
		if args[0] == "rev-parse" {
			corpus.Commit = strings.TrimSpace(string(data))
		}
	}
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, selected := range []bool{false, true} {
		name, count := "ordinary", 100
		if selected {
			name, count = "explicit_search", 1000
		}
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			options := runOptions{Binary: binary, Profile: "agent", Host: "native", Scenario: editProbeScenario, Editors: 4, EditCount: count, EditRepos: 1, Seed: 42, Deadline: time.Second, WarmTimeout: 2 * time.Second, StopTimeout: time.Second, KeepState: true, SearchReadbackAcceptance: selected}
			_, _, artifacts, _, _, phaseErr := runPhase(ctx, options, editProbeScenario, []Corpus{corpus}, "", t.TempDir())
			if phaseErr == nil {
				t.Fatal("bootstrap control unexpectedly ran a workload")
			}
			root := artifacts["private_root"]
			if root == "" {
				t.Fatalf("bootstrap did not reach owned configuration: %v", phaseErr)
			}
			data, err := os.ReadFile(filepath.Join(root, "config", "gortex", "config.yaml"))
			if err != nil {
				t.Fatal(err)
			}
			var config map[string]any
			if err := json.Unmarshal(data, &config); err != nil {
				t.Fatal(err)
			}
			edit, present := config["edit"].(map[string]any)
			if selected {
				if !present || edit["ack"] != "local" {
					t.Fatalf("explicit search profile did not select owned edit.ack:local: %s", data)
				}
			} else if _, exists := config["edit"]; exists {
				t.Fatalf("ordinary bootstrap changed its ACK policy/default bytes: %s", data)
			}
		})
	}
}
