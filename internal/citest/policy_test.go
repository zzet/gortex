package citest

import (
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"
)

// Each expensive package needs its own finite job budget. The package and
// compiled-test census must still be covered exactly once on every target.
func TestPolicyMCPAndStoreIndependentBudgets(t *testing.T) {
	p, err := LoadPolicy()
	if err != nil {
		t.Fatal(err)
	}
	census := []string{"TestFast", "TestRecovery", "TestTail"}
	for _, target := range p.Targets {
		seen := map[string]int{}
		for _, row := range p.FullMatrix().Include {
			if row.OS != target.Runner {
				continue
			}
			parts, err := ParsePartition(row.Partition, p)
			if err != nil {
				t.Fatal(err)
			}
			for _, part := range parts {
				if part.Group != "mcp" && part.Group != "store" {
					continue
				}
				if len(parts) != 1 || row.Partition != part.Group {
					t.Errorf("%s: %s shares its job budget in %q", row.OS, part.Group, row.Partition)
				}
				batches, err := planBatches(census, part.Shard, part.Count, 24)
				if err != nil {
					t.Fatal(err)
				}
				for _, batch := range batches {
					for _, name := range batch {
						seen[part.Group+"/"+name]++
					}
				}
			}
		}
		for _, group := range []string{"mcp", "store"} {
			for _, name := range census {
				if got := seen[group+"/"+name]; got != 1 {
					t.Errorf("%s: %s/%s covered %d times", target.Runner, group, name, got)
				}
			}
		}
	}
	for _, groups := range [][]string{{"mcp"}, {"store"}, {"mcp", "store"}} {
		selected := map[string][]string{}
		for _, target := range p.Targets {
			for _, group := range groups {
				selected[target.Runner] = append(selected[target.Runner], "example.org/gortex"+p.Groups[group])
			}
		}
		rows := p.SelectedMatrix("example.org/gortex", selected).Include
		if len(rows) != len(p.Targets)*len(groups) {
			t.Errorf("selected %v: want one job per package and target, got %v", groups, rows)
		}
		for _, row := range rows {
			if !slices.Contains(groups, row.Partition) {
				t.Errorf("selected %v includes unselected or shared job %q", groups, row.Partition)
			}
		}
		for _, target := range p.Targets {
			for _, group := range groups {
				count := 0
				for _, row := range rows {
					if row.OS == target.Runner && row.Partition == group {
						count++
					}
				}
				if count != 1 {
					t.Errorf("selected %v: %s/%s covered %d times", groups, target.Runner, group, count)
				}
			}
		}
	}
	// Keep the explicit CLI alias available to existing local callers. It is
	// not a matrix row, so it cannot duplicate either package's scheduled job.
	alias, err := ParsePartition("mcp+store", p)
	if err != nil || !slices.Equal(alias, []Partition{{Group: "mcp", Count: 1}, {Group: "store", Count: 1}}) {
		t.Fatalf("legacy combined alias changed: %v %v", alias, err)
	}
}

func TestPolicySplitRetainsExecutionLimits(t *testing.T) {
	if testBudget != 35*time.Minute {
		t.Fatalf("internal job budget changed: %s", testBudget)
	}
	env := []string{"TEST_RACE=false", "TEST_WINDOWS=false"}
	opts, err := runnerOptions(".", env)
	if err != nil || opts.Workers != runtime.NumCPU() {
		t.Fatalf("default worker count changed: %+v %v", opts, err)
	}
	opts, err = runnerOptions(".", append(env, "TEST_WORKERS=3"))
	if err != nil || opts.Workers != 3 {
		t.Fatalf("explicit worker count changed: %+v %v", opts, err)
	}
	data, err := os.ReadFile(filepath.Join("..", "..", ".github", "workflows", "ci.yml"))
	if err != nil {
		t.Fatal(err)
	}
	_, testJob, found := strings.Cut(string(data), "\n  test:\n")
	if !found {
		t.Fatal("test workflow job missing")
	}
	testJob, _, _ = strings.Cut(testJob, "\n  build-native:")
	if !strings.Contains(testJob, "    timeout-minutes: 45\n") {
		t.Fatal("outer test job no longer has its existing 45-minute budget")
	}
}

func TestPolicyFullLayoutAndInvalidShards(t *testing.T) {
	p, err := LoadPolicy()
	if err != nil {
		t.Fatal(err)
	}
	full := p.FullMatrix().Include
	if len(full) != 15 {
		t.Fatal(full)
	}
	if err := CheckLayout(full, p); err != nil {
		t.Fatal(err)
	}
	for _, spec := range []string{"indexer:0/2", "indexer:3/2", "rest:1/2", "nope", "indexer-1", "", "mcp+store+mcp", "indexer:01/2"} {
		if _, err := ParsePartition(spec, p); err == nil {
			t.Errorf("invalid partition accepted: %q", spec)
		}
	}
	for name, edit := range map[string]func([]MatrixRow) []MatrixRow{
		"missing shard":   func(rows []MatrixRow) []MatrixRow { return rows[1:] },
		"duplicate shard": func(rows []MatrixRow) []MatrixRow { return append(rows, rows[0]) },
		"mixed counts":    func(rows []MatrixRow) []MatrixRow { rows[1].Partition = "indexer:2/3"; return rows },
		"missing group":   func(rows []MatrixRow) []MatrixRow { return append(rows[:3], rows[4:]...) },
		"group twice":     func(rows []MatrixRow) []MatrixRow { rows[2].Partition = "mcp+store+mcp"; return rows },
	} {
		t.Run(name, func(t *testing.T) {
			rows := append([]MatrixRow{}, full...)
			if err := CheckLayout(edit(rows), p); err == nil {
				t.Fatal("invalid layout accepted")
			}
		})
	}
}
