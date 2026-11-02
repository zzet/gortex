package main

import (
	"testing"
	"time"
)

func TestRunOptionsPreserveSmokeVersionAndDiscoveryFlags(t *testing.T) {
	args := []string{
		"--binary", "unused", "--manifest", "unused", "--scratch", t.TempDir(),
		"--scenario", "edit", "--agents", "0", "--edit-agents", "4", "--worktrees", "0",
		"--duration", "20s", "--deadline", "120s", "--pr-smoke-drain", "2m",
		"--pr-smoke-drain-version", "2", "--edit-repos", "1",
		"--discovery-stall-first=false", "--discovery-existing-worktrees", "0", "--discovery-existing-nested", "0",
	}
	options, err := parseRun(args)
	if err != nil {
		t.Fatalf("version2 edit flags plus neutral discovery flags rejected: %v", err)
	}
	if options.Scenario != "edit" || options.EditRepos != 1 || options.Editors != 4 || options.Agents != 0 || options.Worktrees != 0 {
		t.Fatalf("combined flags changed the edit workload: %+v", options)
	}
	if options.Duration != 20*time.Second || options.Deadline != 120*time.Second || options.PRSmokeDrain != 120*time.Second {
		t.Fatalf("combined flags changed the original offer or deadline: %+v", options)
	}
}

func TestRunOptionsKeepDiscoveryScenarioWithoutSmokeDrain(t *testing.T) {
	args := []string{
		"--binary", "unused", "--manifest", "unused", "--scratch", t.TempDir(),
		"--scenario", "worktree-discovery", "--agents", "2", "--edit-agents", "0", "--worktrees", "0",
		"--pr-smoke-drain-version", "1", "--pr-smoke-drain", "0", "--edit-repos", "1",
		"--discovery-stall-first=false", "--discovery-existing-worktrees", "0", "--discovery-existing-nested", "0",
	}
	options, err := parseRun(args)
	if err != nil {
		t.Fatalf("discovery flags plus disabled version1 drain rejected: %v", err)
	}
	scenarios, err := selectedScenarios(options)
	if err != nil || len(scenarios) != 1 || scenarios[0] != "worktree-discovery" {
		t.Fatalf("combined flags changed discovery selection: %v %v", scenarios, err)
	}
	if options.Agents != 2 || options.Editors != 0 || options.Worktrees != 0 || options.PRSmokeDrain != 0 {
		t.Fatalf("combined flags changed the discovery baseline: %+v", options)
	}
}

func TestRunOptionsRejectCrossScopeSmokeDiscoveryFlags(t *testing.T) {
	base := []string{"--binary", "unused", "--manifest", "unused", "--scratch", t.TempDir()}
	edit := append(append([]string(nil), base...), "--scenario", "edit", "--agents", "0", "--edit-agents", "4", "--worktrees", "0", "--duration", "20s", "--pr-smoke-drain", "2m", "--pr-smoke-drain-version", "2")
	discovery := append(append([]string(nil), base...), "--scenario", "worktree-discovery", "--agents", "2", "--edit-agents", "0", "--worktrees", "0")
	for _, tc := range []struct {
		name string
		args []string
	}{
		{"discovery_stall_in_edit_drain", append(append([]string(nil), edit...), "--discovery-stall-first")},
		{"discovery_topology_in_edit_drain", append(append([]string(nil), edit...), "--discovery-existing-worktrees", "1")},
		{"six_repositories_in_edit_drain", append(append([]string(nil), edit...), "--edit-repos", "6")},
		{"edit_drain_in_discovery", append(append([]string(nil), discovery...), "--pr-smoke-drain", "2m", "--pr-smoke-drain-version", "2")},
		{"unowned_nested_topology", append(append([]string(nil), discovery...), "--discovery-existing-nested", "1")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := parseRun(tc.args); err == nil {
				t.Fatal("combined flags accepted an unsupported workload")
			}
		})
	}
}
