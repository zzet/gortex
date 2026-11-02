package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestEditGateSeedExplicitOptIn(t *testing.T) {
	base := []string{"--binary", "unused", "--manifest", "unused", "--scratch", t.TempDir(), "--scenario", editProbeScenario, "--agents", "0", "--edit-agents", "4", "--worktrees", "0"}
	for _, mode := range []string{"ack-wait", "closed-read-gate"} {
		t.Run(mode, func(t *testing.T) {
			args := append(append([]string{}, base...), "--edit-gate-modes", "--edit-gate-seed", mode)
			if _, err := parseRun(args); err != nil {
				t.Fatalf("explicit gate-only seed unavailable: %v", err)
			}
		})
	}
	for _, args := range [][]string{
		append(append([]string{}, base...), "--edit-gate-seed", "ack-wait"),
		append(append([]string{}, base...), "--edit-gate-modes", "--edit-gate-seed", "unregistered"),
	} {
		if _, err := parseRun(args); err == nil {
			t.Fatal("seed accepted outside the explicit closed gate contract")
		}
	}
	t.Setenv("GORTEX_EDIT_GATE_SEED", "ack-wait")
	for _, entry := range privateEnv(t.TempDir(), "owned.sock", "owned.gitconfig") {
		if strings.HasPrefix(entry, "GORTEX_EDIT_GATE_SEED=") {
			t.Fatal("ambient seed leaked into ordinary private daemon")
		}
	}
}

func TestEditGateSeedVerifiesActualBinaryAndOwnsEnvironment(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\nfunc main() {}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	options := runOptions{Binary: filepath.Join(dir, "seed-bin"), Manifest: "unused", Scratch: dir, Scenario: editProbeScenario, Editors: 4, EditCount: 100, EditGateModes: true, EditGateSeed: "ack-wait", Duration: 1, Deadline: 1, WarmTimeout: 1, StopTimeout: 1, Profile: "agent", SweepRepetitions: 1, SweepBudget: 1}
	for _, tags := range []string{"gortex_edit_gate_seed_suffix", "", "extra,gortex_edit_gate_seed"} {
		cmd := exec.CommandContext(t.Context(), "go", "build", "-tags="+tags, "-o", options.Binary, filepath.Join(dir, "main.go"))
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("build metadata fixture: %v: %s", err, out)
		}
		env, err := editGateSeedEnvironment(options)
		if tags == "extra,gortex_edit_gate_seed" {
			if err != nil || !reflect.DeepEqual(env, []string{"GORTEX_EDIT_GATE_SEED=ack-wait"}) {
				t.Fatalf("tagged binary seed environment=%v err=%v", env, err)
			}
		} else if err == nil || len(env) != 0 {
			t.Fatalf("untagged/prefix-only binary accepted seed: %v %v", env, err)
		}
	}
	options.Binary = "missing"
	options.EditGateSeed = ""
	if env, err := editGateSeedEnvironment(options); err != nil || len(env) != 0 {
		t.Fatalf("ordinary run unexpectedly inspects metadata or enables a seed: %v %v", env, err)
	}
	options.EditGateSeed = "ack-wait"
	if _, err := editGateSeedEnvironment(options); err == nil {
		t.Fatal("missing executable accepted")
	}
}

func TestEditGateSeedRefusesBeforeAllocatingPrivateState(t *testing.T) {
	options := runOptions{Binary: "missing", Manifest: "unused", Scratch: t.TempDir(), Scenario: editProbeScenario, Editors: 4, EditCount: 100, EditGateModes: true, EditGateSeed: "ack-wait", Duration: 1, Deadline: 1, WarmTimeout: 1, StopTimeout: 1, Profile: "agent", SweepRepetitions: 1, SweepBudget: 1}
	root := t.TempDir()
	_, _, artifacts, _, _, err := runPhase(t.Context(), options, editProbeScenario, nil, "", root)
	if err == nil || len(artifacts) != 0 {
		t.Fatalf("untagged seed reached private state: artifacts=%v err=%v", artifacts, err)
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 0 {
		t.Fatalf("refused seed allocated state: %v %v", entries, err)
	}
}
