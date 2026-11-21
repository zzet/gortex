package citest

import (
	"io"
	"slices"
	"strings"
	"testing"
)

func TestPolicyUbuntuCLIHasIndependentPartition(t *testing.T) {
	policy, err := LoadPolicy()
	if err != nil {
		t.Fatal(err)
	}
	owner := MatrixRow{"ubuntu-24.04", "rest-cli"}
	full := policy.FullMatrix().Include
	var cliRows []MatrixRow
	for _, row := range full {
		if row.Partition == owner.Partition {
			cliRows = append(cliRows, row)
		}
	}
	if !slices.Equal(cliRows, []MatrixRow{owner}) {
		t.Fatalf("full CI must include one Ubuntu CLI job: %v", cliRows)
	}
	if err := CheckLayout(full, policy); err != nil {
		t.Fatal(err)
	}
	for _, bad := range [][]MatrixRow{
		slices.DeleteFunc(slices.Clone(full), func(row MatrixRow) bool { return row == owner }),
		append(slices.Clone(full), owner),
		append(slices.Clone(full), MatrixRow{"macos-latest", owner.Partition}),
		append(slices.Clone(full), MatrixRow{"windows-latest", owner.Partition}),
	} {
		if err := CheckLayout(bad, policy); err == nil {
			t.Fatalf("invalid CLI ownership passed: %v", bad)
		}
	}
	for _, spec := range []string{"rest-cli+rest", "rest+rest-cli", "rest-cli:1/1"} {
		if _, err := ParsePartition(spec, policy); err == nil {
			t.Fatalf("independent CLI partition joined or sharded: %s", spec)
		}
	}
}

func TestPolicyUbuntuCLISelectionDoesNotAddOtherPlatformJobs(t *testing.T) {
	policy, err := LoadPolicy()
	if err != nil {
		t.Fatal(err)
	}
	module := "example.org/fixture"
	cli, bench := module+"/cmd/gortex", module+"/bench/load"
	for _, row := range []struct {
		name         string
		packages     []string
		ubuntu       []string
		otherTargets []string
	}{
		{"CLI only", []string{cli}, []string{"rest-cli"}, []string{"rest"}},
		{"non-CLI only", []string{bench}, []string{"rest"}, []string{"rest"}},
		{"mixed", []string{cli, bench}, []string{"rest", "rest-cli"}, []string{"rest"}},
	} {
		t.Run(row.name, func(t *testing.T) {
			selected := map[string][]string{}
			for _, target := range policy.Targets {
				selected[target.Runner] = row.packages
			}
			byTarget := map[string][]string{}
			for _, job := range policy.SelectedMatrix(module, selected).Include {
				byTarget[job.OS] = append(byTarget[job.OS], job.Partition)
			}
			for _, target := range policy.Targets {
				want := row.otherTargets
				if target.Runner == "ubuntu-24.04" {
					want = row.ubuntu
				}
				if !slices.Equal(byTarget[target.Runner], want) {
					t.Errorf("%s selected %v, want %v", target.Runner, byTarget[target.Runner], want)
				}
			}
		})
	}
}

func TestRunnerUbuntuCLISplitRejectsIncompatibleFullSelection(t *testing.T) {
	f := newFixture(t)
	f.pkg("cmd/gortex", "func Value() int { return 42 }", "TestCLIFixture")
	head := f.commit()
	manifest, err := f.planner.Plan(t.Context(), Event{Name: "push", Head: head})
	if err != nil || manifest.Mode != "full" {
		t.Fatalf("expected a sealed full selection: %v %v", manifest.Mode, err)
	}
	selection := writeManifest(t, f, manifest)
	for _, partition := range []string{"rest", "rest-cli"} {
		report, err := RunPartition(t.Context(), RunOptions{
			Root: f.root, Partition: partition, SelectionFile: selection, Target: "ubuntu-24.04",
			Windows: true, Workers: 1, Env: fixtureEnv(), Output: io.Discard,
		})
		if err == nil || !strings.Contains(err.Error(), "selected Ubuntu rest job requires native Linux") || report.Scratch != "" {
			t.Fatalf("%s accepted an incompatible full selection: %s %v", partition, report.Scratch, err)
		}
	}
}
