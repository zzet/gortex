package citest

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

func runnerAmbientQueryPath(t *testing.T, j *job, slot int) string {
	t.Helper()
	env, err := j.workerEnv(slot)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(envValue(env, "XDG_CACHE_HOME"), "gortex", "query-log.jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

func runnerReadOptional(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return data
}

// This is a deterministic namespace counterexample, not an attribution of the
// historical writer. A foreign write lands inside the modeled guard window.
func TestRunnerRestCLISeparatesForeignWritesFromItsAmbientGuard(t *testing.T) {
	j, _ := fakeJob(t, nil)
	cli, foreign := "example.org/m/cmd/gortex", "example.org/m/foreign"
	j.cliPackage = cli
	foreignPath := runnerAmbientQueryPath(t, j, 0)
	invocations, writes := 0, 0
	j.runOverride = func(args []string, slot int, _, _ string) (processResult, error) {
		invocations++
		events := []map[string]any{}
		if slices.Contains(args, foreign) {
			events = append(events, map[string]any{"Package": foreign, "Action": "pass"})
		}
		code := 0
		if slices.Contains(args, cli) {
			guarded := runnerAmbientQueryPath(t, j, slot)
			before := runnerReadOptional(t, guarded)
			writes++
			if err := os.WriteFile(foreignPath, []byte(strconv.Itoa(writes)), 0o600); err != nil {
				t.Fatal(err)
			}
			status := "pass"
			if !bytes.Equal(before, runnerReadOptional(t, guarded)) {
				code, status = 1, "fail"
			}
			events = append(events, map[string]any{"Package": cli, "Action": status})
		}
		return jsonRun(t, code, events...), nil
	}
	if err := j.runRest([]string{cli, foreign}); err != nil {
		t.Fatal(err)
	}
	if invocations != 2 || len(j.failures) != 0 || len(j.flaky) != 0 {
		t.Fatalf("foreign ambient write tripped the CLI guard: calls=%d failures=%v flaky=%v", invocations, j.failures, j.flaky)
	}
	if len(runnerReadOptional(t, foreignPath)) == 0 {
		t.Fatal("counterexample did not perform a real foreign write")
	}
}

func TestRunnerRestCLIOwnAmbientEscapeStillFailsIncludingOnRetry(t *testing.T) {
	for _, child := range []bool{false, true} {
		t.Run(strconv.FormatBool(child), func(t *testing.T) {
			j, _ := fakeJob(t, nil)
			cli := "example.org/m/cmd/gortex"
			j.cliPackage = cli
			calls := 0
			j.runOverride = func(args []string, slot int, tag, _ string) (processResult, error) {
				calls++
				if slot != 1 || !slices.Contains(args, cli) || !strings.HasPrefix(tag, "rest-cli") || slices.Contains(args, "-run") {
					t.Fatalf("CLI attempt lost its namespace or full-package scope: slot=%d tag=%s args=%v", slot, tag, args)
				}
				if calls == 2 && !slices.Contains(args, "-p=1") {
					t.Fatal("retry is not the whole package alone")
				}
				guarded := runnerAmbientQueryPath(t, j, slot)
				before := runnerReadOptional(t, guarded)
				payload := strconv.Itoa(calls)
				if child {
					cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestRunnerNamespaceWriteChild$", "-test.count=1")
					cmd.Env = withEnv(os.Environ(), map[string]string{"GORTEX_CITEST_NAMESPACE_WRITE_PATH": guarded, "GORTEX_CITEST_NAMESPACE_WRITE_PAYLOAD": payload})
					if out, err := cmd.CombinedOutput(); err != nil {
						t.Fatalf("owned child write: %v\n%s", err, out)
					}
				} else if err := os.WriteFile(guarded, []byte(payload), 0o600); err != nil {
					t.Fatal(err)
				}
				if bytes.Equal(before, runnerReadOptional(t, guarded)) {
					t.Fatal("own escape did not modify the guarded ambient file")
				}
				return jsonRun(t, 1, map[string]any{"Package": cli, "Action": "pass", "Test": "TestOwn"}, map[string]any{"Package": cli, "Action": "output", "Output": "ambient state guard detected an own escape\n"}, map[string]any{"Package": cli, "Action": "fail"}), nil
			}
			if err := j.runRest([]string{cli}); err != nil {
				t.Fatal(err)
			}
			if calls != 2 || len(j.failures) == 0 || len(j.flaky) != 0 {
				t.Fatalf("own TestMain escape was suppressed: calls=%d failures=%v flaky=%v", calls, j.failures, j.flaky)
			}
		})
	}
}

func TestRunnerNamespaceWriteChild(t *testing.T) {
	path := os.Getenv("GORTEX_CITEST_NAMESPACE_WRITE_PATH")
	if path == "" {
		t.Skip("owned child helper")
	}
	if err := os.WriteFile(path, []byte(os.Getenv("GORTEX_CITEST_NAMESPACE_WRITE_PAYLOAD")), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestRunnerRestCLIGroupingPreservesSelectionFlagsCoverageAndBudget(t *testing.T) {
	for _, workers := range []int{1, 4} {
		t.Run(strconv.Itoa(workers), func(t *testing.T) {
			j, _ := fakeJob(t, nil)
			cli := "example.org/m/cmd/gortex"
			j.cliPackage, j.workers, j.race = cli, workers, true
			deadline := j.deadline
			selected := []string{"example.org/m/a", cli, "example.org/m/b"}
			seen, tags := map[string]int{}, map[string]bool{}
			calls := 0
			j.runOverride = func(args []string, slot int, tag, _ string) (processResult, error) {
				calls++
				if !j.deadline.Equal(deadline) || !slices.Contains(args, "-p="+strconv.Itoa(workers)) || !slices.Contains(args, "-count=1") || slices.Contains(args, "-run") {
					t.Fatalf("selection, parallel ceiling or deadline changed: %v", args)
				}
				for _, flag := range append(slices.Clone(raceFlags), coverFlags...) {
					if !slices.Contains(args, flag) {
						t.Fatalf("lost race/coverage flag %s: %v", flag, args)
					}
				}
				profile := slices.Index(args, "-coverprofile")
				if profile < 0 || !strings.HasSuffix(args[profile+1], tag+".out") || tags[tag] {
					t.Fatal("coverage/log tags collide")
				}
				cliCall := slices.Contains(args, cli)
				if (cliCall && (slot != 1 || tag != "rest-cli")) || (!cliCall && (slot != 0 || tag != "rest")) || (calls == 1 && cliCall) {
					t.Fatalf("wrong serial namespace group: slot=%d tag=%s args=%v", slot, tag, args)
				}
				if cliCall && args[len(args)-1] != cli {
					t.Fatal("CLI group contains other packages")
				}
				tags[tag] = true
				events := []map[string]any{}
				for _, pkg := range selected {
					if slices.Contains(args, pkg) {
						seen[pkg]++
						events = append(events, map[string]any{"Package": pkg, "Action": "pass"})
					}
				}
				return jsonRun(t, 0, events...), nil
			}
			if err := j.runRest(selected); err != nil {
				t.Fatal(err)
			}
			for _, pkg := range selected {
				if seen[pkg] != 1 {
					t.Fatalf("selected package execution changed: %v", seen)
				}
			}
			if calls != 2 || len(j.coverage) != 2 || j.coverage[0] == j.coverage[1] || len(j.failures) != 0 || len(j.flaky) != 0 {
				t.Fatalf("group result changed: calls=%d coverage=%v failures=%v flaky=%v", calls, j.coverage, j.failures, j.flaky)
			}
		})
	}
}
