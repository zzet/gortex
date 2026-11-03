package main

import (
	"reflect"
	"testing"
	"time"
)

// This is a CLI compatibility regression, not an actual seeded workload or
// permission to relabel native runs as reference Linux capacity evidence.
func TestNightlySeedPreservesExistingV2OfferedDrainProtocol(t *testing.T) {
	args := []string{"--binary", "unused", "--manifest", "unused", "--scratch", t.TempDir(), "--scenario", "edit", "--agents", "0", "--edit-agents", "4", "--worktrees", "0", "--host", "native", "--profile", "agent", "--duration", "20s", "--deadline", "120s", "--pr-smoke-drain", "120s", "--pr-smoke-drain-version", "2"}
	clean, err := parseRun(args)
	if err != nil {
		t.Fatalf("existing clean v2 protocol refused: %v", err)
	}
	seeded, err := parseRun(append(args, "--nightly-workload-seed", "search-delay-v1"))
	if err != nil {
		t.Fatalf("workload seed cannot use identical bounded admission/drain: %v", err)
	}
	if seeded.NightlySeed != "search-delay-v1" || seeded.PRSmokeDrainVersion != 2 || seeded.PRSmokeDrain != 120*time.Second {
		t.Fatalf("seed changed selected protocol: %+v", seeded)
	}
	seeded.NightlySeed = ""
	if !reflect.DeepEqual(clean, seeded) {
		t.Fatalf("seed-off and seed-on differ beyond workload fault: clean=%+v seeded=%+v", clean, seeded)
	}
}

func TestNightlySeedDoesNotRelaxExistingDrainConstraints(t *testing.T) {
	base := runOptions{NightlySeed: "search-delay-v1", Scenario: "edit", Editors: 4, Host: "native", Profile: "agent", Duration: 20 * time.Second, Deadline: 120 * time.Second, PRSmokeDrain: 120 * time.Second, PRSmokeDrainVersion: 2}
	for name, change := range map[string]func(*runOptions){
		"read-version":            func(o *runOptions) { o.PRSmokeDrainVersion = 1 },
		"reference-not-supported": func(o *runOptions) { o.Host = "reference" },
		"short-product-deadline":  func(o *runOptions) { o.Deadline = time.Second },
		"different-offer":         func(o *runOptions) { o.Duration = 30 * time.Second },
		"short-drain":             func(o *runOptions) { o.PRSmokeDrain = time.Second },
		"long-drain":              func(o *runOptions) { o.PRSmokeDrain = 3 * time.Minute },
		"other-editor-count":      func(o *runOptions) { o.Editors = 8 },
		"worktree":                func(o *runOptions) { o.Worktrees = 1 },
		"profile":                 func(o *runOptions) { o.Profile = "saturation" },
		"profile-capture":         func(o *runOptions) { o.CPUProfile = time.Second },
		"keep-state":              func(o *runOptions) { o.KeepState = true },
	} {
		t.Run(name, func(t *testing.T) {
			o := base
			change(&o)
			if err := validateNightlyWorkloadSeed(o); err == nil {
				t.Fatal("seed bypassed the selected four-editor/v2 constraints")
			}
		})
	}
}
