package citest

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

func fakeJob(t *testing.T, runs []processResult) (*job, *[][]string) {
	t.Helper()
	calls := [][]string{}
	index := 0
	j := &job{ctx: t.Context(), root: t.TempDir(), scratch: t.TempDir(), env: os.Environ(), workers: 2, deadline: time.Now().Add(time.Minute), stdout: io.Discard, durations: map[string]float64{}}
	j.runOverride = func(args []string, _ int, _, _ string) (processResult, error) {
		calls = append(calls, append([]string{}, args...))
		if index >= len(runs) {
			t.Fatalf("unexpected extra execution: %v", args)
		}
		out := runs[index]
		index++
		return out, nil
	}
	t.Cleanup(func() {
		if index != len(runs) {
			t.Errorf("only %d/%d scripted executions happened", index, len(runs))
		}
	})
	return j, &calls
}
func TestRunnerBatchRetriesWholeBatchAloneAndKeepsFirstFailure(t *testing.T) {
	first := processResult{1, "--- PASS: TestA (0.10s)\n--- FAIL: TestB (0.20s)\n"}
	passed := processResult{0, "--- PASS: TestA (0.1s)\n--- PASS: TestB (0.1s)\n"}
	for _, initial := range []processResult{first, {1, "--- FAIL: TestA (0.10s)\nexit status 1\n"}} {
		j, calls := fakeJob(t, []processResult{initial, passed})
		names := []string{"TestA", "TestB"}
		retry, err := j.runBatch("indexer:1/1", "fixture.test", ".", names, 0, "b")
		if err != nil || retry == nil {
			t.Fatalf("missing retry: %v", err)
		}
		if err := j.rerunBatches("indexer:1/1", "fixture.test", ".", []*retryBatch{retry}); err != nil {
			t.Fatal(err)
		}
		if len(j.failures) != 0 || len(j.flaky) != 1 || j.flaky[0].Output != initial.output {
			t.Fatalf("lost first attempt: %+v %+v", j.failures, j.flaky)
		}
		for _, cmd := range *calls {
			i := slices.Index(cmd, "-test.run")
			if i < 0 {
				t.Fatal(cmd)
			}
			pattern := regexp.MustCompile(cmd[i+1])
			for _, name := range names {
				if !pattern.MatchString(name) {
					t.Fatal("rerun did not include complete original batch")
				}
			}
		}
	}
}
func TestRunnerBatchRetryFailureMissingResultOrContradictionFails(t *testing.T) {
	first := processResult{1, "--- PASS: TestA (0.1s)\n--- FAIL: TestB (0.1s)\n"}
	for _, second := range []processResult{{1, first.output}, {0, "--- PASS: TestA (0.1s)\n"}, {0, first.output}, {0, raceReport + "\n--- PASS: TestA (0.1s)\n--- PASS: TestB (0.1s)\n"}} {
		j, _ := fakeJob(t, []processResult{first, second})
		retry, err := j.runBatch("indexer:1/1", "fixture.test", ".", []string{"TestA", "TestB"}, 0, "b")
		if err != nil {
			t.Fatal(err)
		}
		if err := j.rerunBatches("indexer:1/1", "fixture.test", ".", []*retryBatch{retry}); err != nil {
			t.Fatal(err)
		}
		if len(j.failures) == 0 || len(j.flaky) != 0 {
			t.Fatalf("bad retry accepted: %+v", j)
		}
	}
}
func TestRunnerHardBatchFailuresNeverRetryEvenExitZero(t *testing.T) {
	for _, report := range []string{raceReport, "panic: boom", "fatal error: concurrent map writes", "SIGSEGV: bad", timeoutReport, "Exception 0xc0000005 at pc"} {
		for _, code := range []int{0, 2} {
			j, _ := fakeJob(t, []processResult{{code, report + "\n--- FAIL: TestB (0.2s)\n--- PASS: TestA (0.1s)\n"}})
			retry, err := j.runBatch("indexer:1/1", "fixture.test", ".", []string{"TestA", "TestB"}, 0, "b")
			if err != nil || retry != nil || len(j.failures) == 0 || len(j.flaky) > 0 {
				t.Fatalf("hard failure retried or passed: %v", err)
			}
		}
	}
	j, _ := fakeJob(t, []processResult{{0, "--- PASS: TestA (0.1s)\n"}})
	retry, err := j.runBatch("indexer:1/1", "fixture.test", ".", []string{"TestA", "TestB"}, 0, "b")
	if err != nil || retry != nil || len(j.failures) != 1 || !strings.Contains(j.failures[0].Title, "never reported") {
		t.Fatal("missing result accepted")
	}
}
func jsonRun(t *testing.T, code int, events ...map[string]any) processResult {
	t.Helper()
	var b strings.Builder
	for _, event := range events {
		if event["Package"] == nil && event["Action"] != "build-output" {
			event["Package"] = "example.org/m/p"
		}
		data, err := json.Marshal(event)
		if err != nil {
			t.Fatal(err)
		}
		b.Write(data)
		b.WriteByte('\n')
	}
	return processResult{code, b.String()}
}
func TestRunnerRestRetriesWholePackageAndRejectsTestMainContradiction(t *testing.T) {
	first := jsonRun(t, 1, map[string]any{"Action": "fail", "Test": "TestA"}, map[string]any{"Action": "run", "Test": "TestB"}, map[string]any{"Action": "fail"})
	passed := jsonRun(t, 0, map[string]any{"Action": "pass", "Test": "TestA"}, map[string]any{"Action": "pass", "Test": "TestB"}, map[string]any{"Action": "pass"})
	failed := jsonRun(t, 1, map[string]any{"Action": "pass", "Test": "TestA"}, map[string]any{"Action": "run", "Test": "TestB"}, map[string]any{"Action": "fail"})
	for _, second := range []processResult{passed, failed} {
		j, calls := fakeJob(t, []processResult{first, second})
		if err := j.runRest([]string{"example.org/m/p"}); err != nil {
			t.Fatal(err)
		}
		if second.code == 0 {
			if len(j.flaky) != 1 || len(j.failures) != 0 || j.flaky[0].Output != first.output {
				t.Fatalf("lost flaky evidence: %+v", j)
			}
		} else if len(j.failures) == 0 || len(j.flaky) != 0 {
			t.Fatal("failed rerun accepted")
		}
		retry := (*calls)[1]
		if !slices.Contains(retry, "-p=1") || retry[len(retry)-1] != "example.org/m/p" || slices.Contains(retry, "-run") {
			t.Fatal("rest rerun was not the whole package alone")
		}
	}
	contradiction := jsonRun(t, 0, map[string]any{"Action": "fail", "Test": "TestA"}, map[string]any{"Action": "pass"})
	j, _ := fakeJob(t, []processResult{contradiction})
	if err := j.runRest([]string{"example.org/m/p"}); err != nil {
		t.Fatal(err)
	}
	if len(j.failures) != 2 || len(j.flaky) > 0 {
		t.Fatal("TestMain contradiction accepted")
	}
}
func TestRunnerRestHardFailuresBuildFailuresAndMissingResultsNeverFlake(t *testing.T) {
	for _, text := range []string{raceReport + "\n", "panic: boom\n", "fatal error: bad\n", timeoutReport + "\n"} {
		r := jsonRun(t, 2, map[string]any{"Action": "output", "Output": text}, map[string]any{"Action": "fail"})
		j, _ := fakeJob(t, []processResult{r})
		if err := j.runRest([]string{"example.org/m/p"}); err != nil {
			t.Fatal(err)
		}
		if len(j.failures) == 0 || len(j.flaky) > 0 {
			t.Fatal("rest hard failure accepted")
		}
	}
	for _, r := range []processResult{jsonRun(t, 1, map[string]any{"Action": "build-output", "ImportPath": "example.org/dependency", "Output": "panic: build helper crashed\n"}, map[string]any{"Action": "fail"}), {1, "non-JSON build failure"}, jsonRun(t, 1, map[string]any{"Action": "pass"})} {
		j, _ := fakeJob(t, []processResult{r})
		if err := j.runRest([]string{"example.org/m/p"}); err != nil {
			t.Fatal(err)
		}
		if len(j.failures) == 0 || len(j.flaky) > 0 {
			t.Fatal("missing/outside result accepted")
		}
	}
}
func TestRunnerSettingsRejectBadRaceBeforeAnyGoCommand(t *testing.T) {
	for _, values := range []map[string]string{{"TEST_RACE": "False"}, {"TEST_WINDOWS": "true", "TEST_RACE": "true"}, {"TEST_WORKERS": "0"}} {
		var out bytes.Buffer
		err := RunTests(t.Context(), t.TempDir(), []string{"--partition", "rest"}, withEnv(os.Environ(), values), &out, &out)
		if err == nil || strings.Contains(err.Error(), "metadata command") {
			t.Fatalf("bad settings not rejected early: %v", err)
		}
	}
}
func fixtureEnv() []string {
	return withEnv(os.Environ(), map[string]string{"GITHUB_OUTPUT": "", "GITHUB_STEP_SUMMARY": ""})
}
func writeManifest(t *testing.T, f *fixture, m Manifest) string {
	t.Helper()
	path := filepath.Join(f.root, "selection.json")
	data, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	return path
}
func TestRunnerRealFullVersusAffectedExecutionAndCoverage(t *testing.T) {
	f := newFixture(t)
	f.pkg("cmd/gortex", "func Value() int { return 42 }", "TestCLIFixture")
	f.pkg("bench/load", "func Value() int { return 42 }", "TestBenchFixture")
	head := f.change("leaf/value.go")
	// The label keeps the macOS rows, so a macOS host also runs the plan.
	affected := f.planWith(head, macOSLabel)
	requireAffected(t, affected)
	full, err := f.planner.Plan(t.Context(), Event{Name: "pull_request", Head: affected.Head, Base: f.base, Full: true})
	if err != nil {
		t.Fatal(err)
	}
	target := nativeTarget(t)
	executed := map[string]set{}
	for _, manifest := range []Manifest{affected, full} {
		selection := writeManifest(t, f, manifest)
		seen := set{}
		for _, row := range manifest.Matrix.Include {
			if row.OS != target {
				continue
			}
			race := manifest.Mode == "affected" && runtime.GOOS != "windows"
			var out bytes.Buffer
			report, err := RunPartition(t.Context(), RunOptions{Root: f.root, Partition: row.Partition, SelectionFile: selection, Target: target, Race: race, Windows: runtime.GOOS == "windows", Workers: 2, Env: fixtureEnv(), Output: &out})
			if err != nil {
				t.Fatalf("%s %s: %v\n%s", manifest.Mode, row.Partition, err, &out)
			}
			for name := range report.Durations {
				fields := strings.Fields(name)
				seen.add(fields[len(fields)-1])
			}
			// Ubuntu gives the CLI and bench packages their own job budget;
			// every other target keeps them in its rest job.
			splitJob := row.Partition == "rest-cli-bench"
			if target != "ubuntu-24.04" {
				splitJob = row.Partition == "rest"
			}
			for _, owned := range []string{"rest example.org/fixture/cmd/gortex TestCLIFixture", "rest example.org/fixture/bench/load TestBenchFixture"} {
				if _, ran := report.Durations[owned]; ran != splitJob {
					t.Fatalf("%s %s: %s has the wrong owner: %v", manifest.Mode, row.Partition, owned, report.Durations)
				}
			}
			files, err := os.ReadDir(filepath.Join(report.Scratch, "logs"))
			if err != nil || len(files) == 0 {
				t.Fatal("per-attempt logs missing")
			}
			if race {
				data, err := os.ReadFile(filepath.Join(f.root, "coverage.out"))
				if err != nil || !strings.HasPrefix(string(data), "mode: atomic\n") {
					t.Fatal("race coverage missing")
				}
			}
		}
		executed[manifest.Mode] = seen
	}
	for _, name := range []string{"TestLeaf", "TestConsumer", "TestIndirect", "TestExternalImport", "TestHelperImport", "TestMCP", "TestRaceConsumer", "TestCLIFixture", "TestBenchFixture"} {
		if !executed["affected"].has(name) || !executed["full"].has(name) {
			t.Fatalf("planned test not executed: %s %v", name, executed)
		}
	}
	for _, name := range []string{"TestIndexer", "TestOtherIndexer", "TestStore"} {
		if executed["affected"].has(name) || !executed["full"].has(name) {
			t.Fatalf("wrong package filtering for %s", name)
		}
	}
}
func TestRunnerCorruptManifestFailsBeforeTestExecution(t *testing.T) {
	f := newFixture(t)
	m := f.plan(f.change("leaf/value.go"))
	m.Packages[nativeTarget(t)] = []string{}
	path := writeManifest(t, f, m)
	var out bytes.Buffer
	report, err := RunPartition(t.Context(), RunOptions{Root: f.root, Partition: "rest", SelectionFile: path, Target: nativeTarget(t), Workers: 2, Env: fixtureEnv(), Output: &out})
	if err == nil || !strings.Contains(err.Error(), "integrity") || report.Scratch != "" || strings.Contains(out.String(), "partition rest:") {
		t.Fatalf("corrupt manifest reached execution: %v %s", err, &out)
	}
}
func TestRunnerDarwinSelectedIndexerKeepsIndependentWatcherRace(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("native macOS watcher-race contract")
	}
	f := newFixture(t)
	m := f.planWith(f.change("internal/indexer/value.go"), macOSLabel)
	var out bytes.Buffer
	report, err := RunPartition(t.Context(), RunOptions{Root: f.root, Partition: "indexer-race", SelectionFile: writeManifest(t, f, m), Target: nativeTarget(t), Workers: 2, Env: fixtureEnv(), Output: &out})
	if err != nil {
		t.Fatalf("%v\n%s", err, &out)
	}
	if len(report.Durations) != 1 || !strings.Contains(out.String(), "indexer-race:1/1 TestWatchFixture") || strings.Contains(out.String(), "rest: done") {
		t.Fatal("watcher owner was lost or unrelated rest work ran")
	}
}
func TestRunnerCancellationReapsOwnedGoTestChildAndRetainsLogs(t *testing.T) {
	f := newFixture(t)
	f.pkg("hang", "func Value()int{return 42}", "TestValue")
	f.write("hang/heartbeat_test.go", "package hang\nimport(\"os\";\"testing\";\"time\")\nfunc TestHeartbeat(t *testing.T){for { _=os.WriteFile(os.Getenv(\"CI_HEARTBEAT\"),[]byte(time.Now().Format(time.RFC3339Nano)),0600);time.Sleep(10*time.Millisecond)}}\n")
	m := f.planWith(f.commit(), macOSLabel) // runs on a macOS host too
	requireAffected(t, m)
	heartbeat := filepath.Join(f.root, "heartbeat")
	env := withEnv(fixtureEnv(), map[string]string{"CI_HEARTBEAT": heartbeat})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	type outcome struct {
		report Report
		err    error
	}
	done := make(chan outcome, 1)
	manifestPath := writeManifest(t, f, m)
	target := nativeTarget(t)
	go func() {
		report, err := RunPartition(ctx, RunOptions{Root: f.root, Partition: "rest", SelectionFile: manifestPath, Target: target, Workers: 1, Windows: runtime.GOOS == "windows", Env: env, Output: io.Discard, Budget: 3 * time.Minute})
		done <- outcome{report, err}
	}()
	deadline := time.NewTimer(90 * time.Second)
	defer deadline.Stop()
	for {
		if _, err := os.Stat(heartbeat); err == nil {
			break
		}
		select {
		case result := <-done:
			t.Fatalf("test process exited before handshake: %v", result.err)
		case <-deadline.C:
			t.Fatal("owned Go test child did not start")
		case <-time.After(10 * time.Millisecond):
		}
	}
	cancel()
	var result outcome
	select {
	case result = <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("cancelled runner did not reap owned processes")
	}
	if result.err == nil || !errors.Is(result.err, context.Canceled) || len(result.report.Flaky) > 0 {
		t.Fatalf("cancellation became success/flaky: %v", result.err)
	}
	before, err := os.ReadFile(heartbeat)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	after, err := os.ReadFile(heartbeat)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("owned test child kept running after cancellation")
	}
	files, _ := filepath.Glob(filepath.Join(result.report.Scratch, "logs", "*.log"))
	hard := false
	for _, file := range files {
		data, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		hard = hard || strings.Contains(string(data), timeoutReport)
	}
	if !hard {
		t.Fatal("cancellation failure evidence was lost")
	}
}
func TestRunnerWorkerIsolationPreservesGoCachesAndWindowsPaths(t *testing.T) {
	j := &job{scratch: t.TempDir(), env: withEnv(os.Environ(), map[string]string{"GOCACHE": "original-cache", "GOMODCACHE": "original-modules", "GOPATH": "original-path", "GOENV": "off"}), windows: true}
	a, err := j.workerEnv(0)
	if err != nil {
		t.Fatal(err)
	}
	b, err := j.workerEnv(1)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"HOME", "TMPDIR", "TEMP", "USERPROFILE", "APPDATA", "LOCALAPPDATA"} {
		if envValue(a, key) == "" || envValue(a, key) == envValue(b, key) {
			t.Errorf("workers share %s", key)
		}
	}
	for _, key := range []string{"GOCACHE", "GOMODCACHE", "GOPATH", "GOENV"} {
		if envValue(a, key) != envValue(j.env, key) {
			t.Errorf("lost %s", key)
		}
	}
}
func TestRunnerWorkflowRowsSupportDynamicAndLegacyLayouts(t *testing.T) {
	p, err := LoadPolicy()
	if err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{"matrix: ${{ fromJSON(needs.plan.outputs.matrix) }}", "    os: [ubuntu-24.04, macos-latest, windows-latest]\n    partition: [\"indexer:1/2\", \"indexer:2/2\", \"mcp+store\", \"rest\"]\n    include: [{os: ubuntu-24.04, partition: rest-cli-bench}, {os: macos-latest, partition: indexer-race}]\n"} {
		rows, err := WorkflowRows(text, p)
		if err != nil {
			t.Fatal(err)
		}
		if err := CheckLayout(rows, p); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := WorkflowRows("  exclude:\n", p); err == nil {
		t.Fatal("excluded layout accepted")
	}
}

func TestRunnerReportRetainsFirstFailureUnicodeAndSummary(t *testing.T) {
	var out bytes.Buffer
	summary := filepath.Join(t.TempDir(), "summary")
	j := &job{stdout: &out, env: withEnv(os.Environ(), map[string]string{"GITHUB_STEP_SUMMARY": summary}), durations: map[string]float64{}, flaky: []Evidence{{"first flaky", "first failure: ошибка\n"}}, failures: []Evidence{{"hard failure", raceReport + "\n"}}}
	for i := 0; i < 21; i++ {
		j.durations["group Test"+strconv.Itoa(i)] = float64(i)
	}
	if err := j.report(); err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{"first failure: ошибка", raceReport, "::warning title=Flaky test::", "::error title=Test failed::"} {
		if !strings.Contains(out.String(), text) {
			t.Errorf("report lost %q", text)
		}
	}
	data, err := os.ReadFile(summary)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "### Flaky") || !strings.Contains(string(data), "### Failed") || strings.Contains(string(data), "`group Test0`") || !strings.Contains(string(data), "`group Test20`") {
		t.Fatal("summary lost failures/slowest20")
	}
}

func TestRunnerRestMixedPackagesKeepHardFailureAttributionAndOrdinaryRetry(t *testing.T) {
	first := jsonRun(t, 1,
		map[string]any{"Action": "pass", "Package": "example.org/a"},
		map[string]any{"Action": "output", "Package": "example.org/b", "Output": "panic: package B crashed\n"},
		map[string]any{"Action": "fail", "Package": "example.org/b"},
		map[string]any{"Action": "output", "Package": "example.org/c", "Test": "TestC", "Output": "ordinary failure C\n"},
		map[string]any{"Action": "fail", "Package": "example.org/c", "Test": "TestC"},
		map[string]any{"Action": "fail", "Package": "example.org/c"})
	second := jsonRun(t, 0, map[string]any{"Action": "pass", "Package": "example.org/c", "Test": "TestC"}, map[string]any{"Action": "pass", "Package": "example.org/c"})
	j, calls := fakeJob(t, []processResult{first, second})
	if err := j.runRest([]string{"example.org/a", "example.org/b", "example.org/c"}); err != nil {
		t.Fatal(err)
	}
	if len(j.failures) != 1 || j.failures[0].Title != "rest example.org/b" || !strings.Contains(j.failures[0].Output, "package B crashed") {
		t.Fatalf("hard failure misattributed: %+v", j.failures)
	}
	if len(j.flaky) != 1 || j.flaky[0].Title != "rest example.org/c TestC" || j.flaky[0].Output != "ordinary failure C\n" {
		t.Fatalf("ordinary retry evidence lost: %+v", j.flaky)
	}
	if cmd := (*calls)[1]; cmd[len(cmd)-1] != "example.org/c" || !slices.Contains(cmd, "-p=1") {
		t.Fatalf("retried wrong package: %v", cmd)
	}
	global := jsonRun(t, 0, map[string]any{"Action": "pass", "Package": "example.org/a"})
	global.output += "panic: unattributed helper\n"
	j, _ = fakeJob(t, []processResult{global})
	if err := j.runRest([]string{"example.org/a"}); err != nil {
		t.Fatal(err)
	}
	if len(j.failures) != 1 || j.failures[0].Title != "rest unattributed hard failure" {
		t.Fatalf("unattributed panic was hidden or spread: %+v", j.failures)
	}
}
