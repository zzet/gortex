package citest

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"testing"
)

func TestPolicyDarwinRaceTailHasIndependentExactOnceOwner(t *testing.T) {
	p, err := LoadPolicy()
	if err != nil {
		t.Fatal(err)
	}
	full := p.FullMatrix().Include
	owner := MatrixRow{"macos-latest", "indexer-race"}
	count := 0
	for _, row := range full {
		if row.Partition == "indexer-race" {
			count++
			if row != owner {
				t.Fatalf("non-macOS race owner: %+v", row)
			}
		}
	}
	if count != 1 {
		t.Fatalf("want exactly one independent macOS race job, got %d: %+v", count, full)
	}
	without := slices.DeleteFunc(slices.Clone(full), func(row MatrixRow) bool { return row == owner })
	if CheckLayout(without, p) == nil {
		t.Fatal("missing race coverage accepted")
	}
	for _, rows := range [][]MatrixRow{append(slices.Clone(full), owner), append(slices.Clone(full), MatrixRow{"ubuntu-24.04", "indexer-race"})} {
		if CheckLayout(rows, p) == nil {
			t.Fatal("duplicate or non-macOS race coverage accepted")
		}
	}
	for _, spec := range []string{"indexer-race+rest", "rest+indexer-race", "indexer-race:1/1", "indexer-race:1/2"} {
		if _, err := ParsePartition(spec, p); err == nil {
			t.Fatalf("race job shared or sharded its budget: %s", spec)
		}
	}
	selected := p.SelectedMatrix("example.org/gortex", map[string][]string{"macos-latest": {"example.org/gortex/internal/indexer"}}).Include
	if !slices.Equal(selected, []MatrixRow{{"macos-latest", "indexer:1/2"}, {"macos-latest", "indexer:2/2"}, owner}) {
		t.Fatalf("indexer-only selection widened or lost race coverage: %+v", selected)
	}
	unrelated := p.SelectedMatrix("example.org/gortex", map[string][]string{"macos-latest": {"example.org/gortex/leaf"}}).Include
	if !slices.Equal(unrelated, []MatrixRow{{"macos-latest", "rest"}}) {
		t.Fatalf("unrelated selection added race work: %+v", unrelated)
	}
}

func TestRunnerDarwinRaceRootSetAndFlagsUnchanged(t *testing.T) {
	j, _ := fakeJob(t, nil)
	j.workers = 1
	all := []string{"TestWatchFixture", "TestBeforeStormFixture", "TestRealtimeFixture", "TestPollerFixture", "TestOrdinaryFixture"}
	seen := map[string]int{}
	build, census := 0, 0
	j.runOverride = func(args []string, _ int, _, _ string) (processResult, error) {
		if slices.Contains(args, "-c") {
			build++
			for _, flag := range raceFlags {
				if !slices.Contains(args, flag) {
					t.Fatalf("race flag lost: %s in %v", flag, args)
				}
			}
			return processResult{0, ""}, nil
		}
		if slices.Contains(args, "-test.list") {
			census++
			return processResult{0, strings.Join(all, "\n") + "\n"}, nil
		}
		i := slices.Index(args, "-test.run")
		if i < 0 {
			t.Fatalf("unexpected command %v", args)
		}
		re := regexp.MustCompile(args[i+1])
		var out strings.Builder
		for _, name := range all {
			if re.MatchString(name) {
				seen[name]++
				fmt.Fprintf(&out, "--- PASS: %s (0.01s)\n", name)
			}
		}
		return processResult{0, out.String()}, nil
	}
	if err := j.runPackage("indexer", "example.org/gortex/internal/indexer", Partition{Group: "indexer", Count: 1}, ".", true, darwinRaceTests); err != nil {
		t.Fatal(err)
	}
	if build != 1 || census != 1 || len(seen) != 4 {
		t.Fatalf("changed build/census/population: %d %d %v", build, census, seen)
	}
	for _, name := range all[:4] {
		if seen[name] != 1 {
			t.Fatalf("%s scheduled %d times", name, seen[name])
		}
	}
	if seen[all[4]] != 0 || len(j.failures) != 0 || len(j.flaky) != 0 {
		t.Fatalf("unexpected work/failure: %v %+v", seen, j.failures)
	}
}

func TestRunnerDarwinRaceRejectsWrongPlatformBeforeMetadata(t *testing.T) {
	opts := RunOptions{Root: filepath.Join(t.TempDir(), "does-not-exist"), Partition: "indexer-race", Workers: 1, Windows: true, Output: io.Discard}
	report, err := RunPartition(t.Context(), opts)
	if err == nil || !strings.Contains(err.Error(), "native macOS") || report.Scratch != "" {
		t.Fatalf("wrong-platform job reached metadata: %+v %v", report, err)
	}
}

func TestRunnerDarwinRestDoesNotSpendItsBudgetOnRaceTail(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("native macOS ownership contract")
	}
	f := newFixture(t)
	m, err := f.planner.Plan(t.Context(), Event{Name: "workflow_dispatch", Head: f.base, Full: true})
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	report, err := RunPartition(t.Context(), RunOptions{Root: f.root, Partition: "rest", SelectionFile: writeManifest(t, f, m), Target: nativeTarget(t), Workers: 2, Env: fixtureEnv(), Output: &out})
	if err != nil {
		t.Fatalf("%v\n%s", err, &out)
	}
	if len(report.Durations) == 0 || !strings.Contains(out.String(), "rest: done") {
		t.Fatal("ordinary rest work was omitted")
	}
	files, err := os.ReadDir(filepath.Join(report.Scratch, "logs"))
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range files {
		if strings.HasPrefix(file.Name(), "indexer") {
			t.Fatalf("rest still owns indexer race compilation/census: %s", file.Name())
		}
	}
	if strings.Contains(out.String(), "indexer-race") {
		t.Fatal("race tail still shares rest budget")
	}
}
