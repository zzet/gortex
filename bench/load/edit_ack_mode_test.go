package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func editACKModeArgs() []string {
	return []string{"--binary", "binary", "--manifest", "manifest", "--scratch", "scratch", "--scenario", "read", "--agents", "1", "--edit-agents", "0", "--worktrees", "0"}
}

func TestExplicitEditACKModeValidatesOnlyTheTwoConfigurationPolicies(t *testing.T) {
	if _, err := parseRun(editACKModeArgs()); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"local", "full", "unknown", "LOCAL"} {
		t.Run(mode, func(t *testing.T) {
			options, err := parseRun(append(editACKModeArgs(), "--edit-ack", mode))
			if mode != "local" && mode != "full" {
				if err == nil {
					t.Fatal("invalid startup ACK policy accepted")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			data, err := json.Marshal(options)
			if err != nil {
				t.Fatal(err)
			}
			var fields map[string]any
			if err := json.Unmarshal(data, &fields); err != nil {
				t.Fatal(err)
			}
			if fields["EditACKMode"] != mode {
				t.Fatalf("selected startup policy lost: %s", data)
			}
		})
	}
}

func TestExplicitEditACKModeSeparatesWorkloadIdentity(t *testing.T) {
	options, err := parseRun(editACKModeArgs())
	if err != nil {
		t.Fatal(err)
	}
	hashes := map[string]string{}
	for _, mode := range []string{"", "local", "full"} {
		selected := options
		// Decode by field name so this regression executes before the new
		// option exists; an ignored option cannot manufacture plan identity.
		data, err := json.Marshal(map[string]string{"EditACKMode": mode})
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(data, &selected); err != nil {
			t.Fatal(err)
		}
		report, err := newRunReport(selected, []Corpus{sampleCorpus()}, strings.Repeat("a", 40), Host{}, "owned-ack-mode")
		if err != nil {
			t.Fatal(err)
		}
		fields := copyJSON(report.Workload)
		if mode == "" {
			if _, exists := fields["edit_ack_mode"]; exists {
				t.Fatal("ordinary workload acquired explicit ACK policy metadata")
			}
		} else if fields["edit_ack_mode"] != mode {
			t.Errorf("requested startup mode missing from normalized workload: mode=%q fields=%v", mode, fields)
		}
		hashes[mode] = report.RequestPlan
	}
	if hashes["local"] == hashes["full"] {
		t.Fatal("local and full ACK configurations have identical workload identity")
	}
}

// Crash the owned fixture before readiness. This proves startup configuration
// only, never effective ACK semantics or any measured edit workload.
func TestExplicitEditACKModeReachesOwnedBootstrapConfig(t *testing.T) {
	t.Setenv("LOAD_DAEMON_FIXTURE", "crash")
	corpus := gitFixture(t)
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"", "local", "full"} {
		name := mode
		if name == "" {
			name = "ordinary"
		}
		t.Run(name, func(t *testing.T) {
			options := runOptions{Binary: binary, Profile: "agent", Host: "native", Scenario: "read", Agents: 1, Worktrees: 0, Seed: 42, Duration: time.Millisecond, Deadline: time.Second, WarmTimeout: 2 * time.Second, StopTimeout: time.Second, KeepState: true}
			data, err := json.Marshal(map[string]string{"EditACKMode": mode})
			if err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(data, &options); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			_, _, artifacts, _, _, phaseErr := runPhase(ctx, options, "read", []Corpus{corpus}, "", t.TempDir())
			if phaseErr == nil {
				t.Fatal("startup control unexpectedly ran a workload")
			}
			root := artifacts["private_root"]
			if root == "" {
				t.Fatalf("startup did not reach owned configuration: %v", phaseErr)
			}
			data, err = os.ReadFile(filepath.Join(root, "config", "gortex", "config.yaml"))
			if err != nil {
				t.Fatal(err)
			}
			var config map[string]any
			if err := json.Unmarshal(data, &config); err != nil {
				t.Fatal(err)
			}
			if mode == "" {
				if _, exists := config["edit"]; exists {
					t.Fatal("ordinary configuration changed default bytes")
				}
			} else if edit, ok := config["edit"].(map[string]any); !ok || edit["ack"] != mode {
				t.Fatalf("owned daemon did not receive selected edit.ack=%s: %s", mode, data)
			}
		})
	}
}
