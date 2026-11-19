package citest

import (
	"bytes"
	"runtime"
	"slices"
	"strings"
	"testing"
)

func TestPolicyCLIHasIndependentBudget(t *testing.T) {
	p, err := LoadPolicy()
	if err != nil {
		t.Fatal(err)
	}
	if p.Groups["cli"] != "/cmd/gortex" {
		t.Fatal("CLI tests still share the rest partition's budget")
	}
	for _, target := range p.Targets {
		owners := 0
		for _, row := range p.FullMatrix().Include {
			if row.OS != target.Runner {
				continue
			}
			parts, err := ParsePartition(row.Partition, p)
			if err != nil {
				t.Fatal(err)
			}
			for _, part := range parts {
				if part.Group == "cli" {
					owners++
					if row.Partition != "cli" || len(parts) != 1 || part.Count != 1 {
						t.Fatalf("CLI shares or shards its budget: %+v", row)
					}
				}
			}
		}
		if owners != 1 {
			t.Fatalf("%s has %d CLI owners", target.Runner, owners)
		}
		selected := p.SelectedMatrix("example.org/m", map[string][]string{target.Runner: {"example.org/m/cmd/gortex"}}).Include
		if !slices.Equal(selected, []MatrixRow{{target.Runner, "cli"}}) {
			t.Fatalf("CLI-only selection widened or lost coverage: %+v", selected)
		}
	}
	for _, spec := range []string{"cli+rest", "rest+cli", "cli:1/1", "cli:1/2"} {
		if _, err := ParsePartition(spec, p); err == nil {
			t.Fatalf("CLI budget can still be shared or sharded: %s", spec)
		}
	}
}

// Execute both partitions with a failing test in the other partition. Neither
// source selection nor a passing package status can hide accidental overlap.
func TestRunnerCLIAndRestExecuteDisjointPackages(t *testing.T) {
	for _, partition := range []string{"cli", "rest"} {
		t.Run(partition, func(t *testing.T) {
			f := newFixture(t)
			forbidden := "cmd/gortex"
			if partition == "cli" {
				forbidden = "leaf"
			}
			pkg := "gortex"
			if forbidden == "leaf" {
				pkg = "leaf"
			}
			f.write(forbidden+"/value_test.go", "package "+pkg+"\nimport \"testing\"\nfunc TestWrongPartition(t *testing.T){t.Fatal(\"wrong partition executed\")}\n")
			f.base = f.commit()
			manifest, err := f.planner.Plan(t.Context(), Event{Name: "workflow_dispatch", Head: f.base, Full: true})
			if err != nil {
				t.Fatal(err)
			}
			var out bytes.Buffer
			report, err := RunPartition(t.Context(), RunOptions{Root: f.root, Partition: partition,
				SelectionFile: writeManifest(t, f, manifest), Target: nativeTarget(t),
				Workers: 2, Windows: runtime.GOOS == "windows", Env: fixtureEnv(), Output: &out})
			if err != nil {
				t.Fatalf("%v\n%s", err, &out)
			}
			if len(report.Durations) == 0 {
				t.Fatal("partition executed no tests")
			}
			for key := range report.Durations {
				cli := strings.Contains(key, "example.org/fixture/cmd/gortex ")
				if cli != (partition == "cli") {
					t.Fatalf("wrong package executed by %s: %s", partition, key)
				}
			}
		})
	}
}
