package citest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"
)

var raceFlags = []string{"-race", "-gcflags=modernc.org/sqlite/lib=-race=false", "-gcflags=modernc.org/libc/...=-race=false"}
var coverFlags = []string{"-cover", "-covermode=atomic"}
var darwinRaceTests = regexp.MustCompile(`Watch|Storm|Realtime|Poller`)

const testBudget = 35 * time.Minute

var ErrTestsFailed = errors.New("CI tests failed")

type Evidence struct{ Title, Output string }
type Report struct {
	Scratch         string
	Failures, Flaky []Evidence
	Durations       map[string]float64
}
type RunOptions struct {
	Root, Partition, Target, SelectionFile string
	Race, Windows                          bool
	Workers                                int
	Env                                    []string
	Output                                 io.Writer
	Budget                                 time.Duration
}
type processResult struct {
	code   int
	output string
}
type job struct {
	ctx             context.Context
	root, scratch   string
	env             []string
	race, windows   bool
	workers         int
	cliPackage      string
	deadline        time.Time
	stdout          io.Writer
	mu, outputMu    sync.Mutex
	durations       map[string]float64
	failures, flaky []Evidence
	coverage        []string
	runOverride     func([]string, int, string, string) (processResult, error)
}

func (j *job) printf(format string, args ...any) {
	j.outputMu.Lock()
	defer j.outputMu.Unlock()
	fmt.Fprintf(j.stdout, format, args...)
}
func (j *job) remaining() (time.Duration, error) {
	if err := j.ctx.Err(); err != nil {
		return 0, err
	}
	left := time.Until(j.deadline)
	if left <= 0 {
		return 0, fmt.Errorf("test execution budget exhausted")
	}
	return left, nil
}
func (j *job) fail(title, output string) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.failures = append(j.failures, Evidence{title, output})
}
func (j *job) markFlaky(title, output string) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.flaky = append(j.flaky, Evidence{title, output})
}
func (j *job) record(label string, results map[string]testResult) {
	j.mu.Lock()
	defer j.mu.Unlock()
	for name, r := range results {
		j.durations[label+" "+name] = r.seconds
	}
}
func (j *job) profile(tag string) string {
	path := filepath.Join(j.scratch, "coverage", tag+".out")
	j.mu.Lock()
	j.coverage = append(j.coverage, path)
	j.mu.Unlock()
	return path
}
func (j *job) workerEnv(slot int) ([]string, error) {
	root := filepath.Join(j.scratch, fmt.Sprintf("w%d", slot))
	home, temp := filepath.Join(root, "h"), filepath.Join(root, "t")
	for _, path := range []string{home, temp} {
		if err := os.MkdirAll(path, 0700); err != nil {
			return nil, err
		}
	}
	values := map[string]string{"HOME": home, "TMPDIR": temp, "TMP": temp, "TEMP": temp}
	if j.windows {
		values["USERPROFILE"] = home
		values["APPDATA"] = filepath.Join(home, "AppData", "Roaming")
		values["LOCALAPPDATA"] = filepath.Join(home, "AppData", "Local")
	} else {
		values["XDG_CONFIG_HOME"] = filepath.Join(home, ".config")
		values["XDG_CACHE_HOME"] = filepath.Join(home, ".cache")
		values["XDG_DATA_HOME"] = filepath.Join(home, ".local", "share")
		values["XDG_STATE_HOME"] = filepath.Join(home, ".local", "state")
	}
	return withEnv(j.env, values), nil
}
func (j *job) run(args []string, slot int, tag, dir string) (processResult, error) {
	if j.runOverride != nil {
		return j.runOverride(args, slot, tag, dir)
	}
	path := filepath.Join(j.scratch, "logs", tag+".log")
	log, err := os.Create(path)
	if err != nil {
		return processResult{}, err
	}
	left, budgetErr := j.remaining()
	env, envErr := j.workerEnv(slot)
	if budgetErr != nil || envErr != nil {
		message := fmt.Sprintf("\n%s: %v %v\n", timeoutReport, budgetErr, envErr)
		_, writeErr := io.WriteString(log, message)
		closeErr := log.Close()
		return processResult{-1, message}, errors.Join(writeErr, closeErr)
	}
	ctx, cancel := context.WithTimeout(j.ctx, left+120*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, args[0], args[1:]...)
	configureProcess(cmd)
	cmd.WaitDelay = 5 * time.Second
	cmd.Dir = dir
	cmd.Env = env
	cmd.Stdout = log
	cmd.Stderr = log
	runErr := cmd.Run()
	code := 0
	if runErr != nil {
		code = -1
		var exit *exec.ExitError
		if errors.As(runErr, &exit) {
			code = exit.ExitCode()
		}
		if ctx.Err() != nil {
			_, _ = fmt.Fprintf(log, "\n%s: test process outlived or cancelled its job budget: %v\n", timeoutReport, ctx.Err())
		} else {
			_, _ = fmt.Fprintf(log, "\ncommand exit: %v\n", runErr)
		}
	}
	closeErr := log.Close()
	data, readErr := os.ReadFile(path)
	return processResult{code, strings.ToValidUTF8(string(data), "\uFFFD")}, errors.Join(closeErr, readErr)
}
func (j *job) execute(binary, dir string, names []string, slot int, tag string) (processResult, map[string]testResult, error) {
	left, err := j.remaining()
	if err != nil {
		out := timeoutReport + ": " + err.Error()
		return processResult{-1, out}, parseResults(out), nil
	}
	args := []string{binary, "-test.run", exactPattern(names), "-test.v", "-test.paniconexit0", fmt.Sprintf("-test.timeout=%.0fs", max(left.Seconds(), 1))}
	if j.race {
		args = append(args, "-test.coverprofile", j.profile(tag))
	}
	r, err := j.run(args, slot, tag, dir)
	return r, parseResults(r.output), err
}

type retryBatch struct {
	names      []string
	tag, first string
}

func completeResults(names []string, results map[string]testResult) bool {
	for _, name := range names {
		if _, ok := results[name]; !ok {
			return false
		}
	}
	return true
}
func anyFailed(results map[string]testResult) bool {
	for _, result := range results {
		if result.status == "FAIL" {
			return true
		}
	}
	return false
}
func (j *job) batchFailure(label, tag string, names []string, results map[string]testResult, r processResult) {
	missing, failed := 0, 0
	for _, name := range names {
		result, ok := results[name]
		if !ok {
			missing++
		} else if result.status == "FAIL" {
			failed++
			j.fail(label+" "+name, testOutput(r.output, name))
		}
	}
	if missing > 0 {
		j.fail(fmt.Sprintf("%s [%s]: %d tests never reported a result", label, tag, missing), r.output)
	} else if failed == 0 {
		j.fail(fmt.Sprintf("%s [%s]: exit %d outside any test or hard failure", label, tag, r.code), r.output)
	}
}
func (j *job) runBatch(label, binary, dir string, names []string, slot int, tag string) (*retryBatch, error) {
	started := time.Now()
	r, results, err := j.execute(binary, dir, names, slot, tag)
	if err != nil {
		return nil, err
	}
	j.record(label, results)
	j.printf("%s [%s] %d tests, exit %d, %.0fs\n", label, tag, len(names), r.code, time.Since(started).Seconds())
	if r.code == 0 && completeResults(names, results) && !anyFailed(results) && !hardFailure(r.output) {
		return nil, nil
	}
	if r.code == 0 || hardFailure(r.output) {
		j.batchFailure(label, tag, names, results, r)
		return nil, nil
	}
	return &retryBatch{names, tag, r.output}, nil
}
func (j *job) rerunBatches(label, binary, dir string, retries []*retryBatch) error {
	for _, retry := range retries {
		if retry == nil {
			continue
		}
		r, results, err := j.execute(binary, dir, retry.names, 0, retry.tag+"-rerun")
		if err != nil {
			return err
		}
		if r.code == 0 && completeResults(retry.names, results) && !anyFailed(results) && !hardFailure(r.output) {
			failed := []string{}
			for name, result := range parseResults(retry.first) {
				if result.status == "FAIL" {
					failed = append(failed, name)
				}
			}
			slices.Sort(failed)
			title := strings.Join(failed, " ")
			if title == "" {
				title = retry.tag
			}
			j.markFlaky(label+" "+title, retry.first)
		} else {
			j.batchFailure(label, retry.tag, retry.names, results, r)
		}
	}
	return nil
}
func (j *job) runPackage(group, pkg string, part Partition, dir string, race bool, selectNames *regexp.Regexp) error {
	started := time.Now()
	suffix := ""
	if race && !j.race {
		suffix = "-race"
	}
	ext := ".test"
	if j.windows {
		ext += ".exe"
	}
	binary := filepath.Join(j.scratch, group+suffix+ext)
	args := []string{"go", "test", "-c", "-o", binary}
	if race {
		args = append(args, raceFlags...)
	}
	if j.race {
		args = append(args, coverFlags...)
	}
	args = append(args, pkg)
	r, err := j.run(args, 0, group+suffix+"-build", j.root)
	if err != nil {
		return err
	}
	if r.code != 0 {
		j.fail("compile "+pkg, r.output)
		return fmt.Errorf("compile %s failed", pkg)
	}
	r, err = j.run([]string{binary, "-test.list", "."}, 0, group+suffix+"-census", dir)
	if err != nil {
		return err
	}
	if r.code != 0 {
		j.fail("census "+pkg, r.output)
		return fmt.Errorf("test census failed: %s", pkg)
	}
	names, err := censusNames(r.output)
	if err != nil {
		j.fail("census "+pkg, r.output)
		return err
	}
	if selectNames != nil {
		filtered := []string{}
		for _, name := range names {
			if selectNames.MatchString(name) {
				filtered = append(filtered, name)
			}
		}
		names = filtered
	}
	batches, err := planBatches(names, part.Shard, part.Count, 8*j.workers)
	if err != nil {
		return err
	}
	label := fmt.Sprintf("%s%s:%d/%d", group, suffix, part.Shard+1, part.Count)
	total := 0
	for _, batch := range batches {
		total += len(batch)
	}
	j.printf("%s: %s runs %d tests in %d batches on %d workers (built in %.0fs)\n", label, pkg, total, len(batches), j.workers, time.Since(started).Seconds())
	retries := make([]*retryBatch, len(batches))
	errs := make([]error, len(batches))
	indices := make(chan int)
	var workers sync.WaitGroup
	for slot := 0; slot < j.workers; slot++ {
		workers.Add(1)
		go func(slot int) {
			defer workers.Done()
			for index := range indices {
				retries[index], errs[index] = j.runBatch(label, binary, dir, batches[index], slot, fmt.Sprintf("%s%s-%d", group, suffix, index))
			}
		}(slot)
	}
	for index := range batches {
		indices <- index
	}
	close(indices)
	workers.Wait()
	for _, err := range errs {
		if err != nil {
			return err
		}
	}
	if err := j.rerunBatches(label, binary, dir, retries); err != nil {
		return err
	}
	j.printf("%s: done in %.0fs\n", label, time.Since(started).Seconds())
	return nil
}
func (j *job) goTest(packages []string, parallel int, tag string, slot int) (processResult, testEvents, error) {
	left, err := j.remaining()
	if err != nil {
		return processResult{}, testEvents{}, err
	}
	args := []string{"go", "test", "-json", "-count=1", fmt.Sprintf("-p=%d", parallel), fmt.Sprintf("-timeout=%.0fs", max(left.Seconds(), 1))}
	if j.race {
		args = append(args, raceFlags...)
		args = append(args, coverFlags...)
		args = append(args, "-coverprofile", j.profile(tag))
	}
	r, err := j.run(append(args, packages...), slot, tag, j.root)
	return r, parseEvents(r.output), err
}
func failedTests(events testEvents, pkg string) []string {
	names := []string{}
	for key, result := range events.tests {
		if key.pkg == pkg && result.status == "fail" {
			names = append(names, key.name)
		}
	}
	slices.Sort(names)
	return names
}
func (j *job) packageFailure(pkg string, events testEvents, text string) {
	failed := failedTests(events, pkg)
	for _, name := range failed {
		out := events.output[testKey{pkg, name}]
		if out == "" {
			out = text
		}
		j.fail("rest "+pkg+" "+name, out)
	}
	if len(failed) == 0 || events.packages[pkg].status != "fail" || hardFailure(text) {
		j.fail("rest "+pkg, text)
	}
}
func unattributedHardEvidence(events testEvents, packages []string) string {
	var out strings.Builder
	if hardFailure(events.unattributed) {
		out.WriteString(events.unattributed)
	}
	for pkg, text := range events.packageOutput {
		if !slices.Contains(packages, pkg) && hardFailure(text) {
			out.WriteString(text)
		}
	}
	return out.String()
}
func eventEvidence(events testEvents, pkg, raw string) string {
	if text := events.packageOutput[pkg]; text != "" {
		return text
	}
	return raw
}
func (j *job) runRest(packages []string) error {
	if j.cliPackage == "" || !slices.Contains(packages, j.cliPackage) {
		return j.runRestGroup(packages, 0, "rest")
	}
	others := make([]string, 0, len(packages)-1)
	for _, pkg := range packages {
		if pkg != j.cliPackage {
			others = append(others, pkg)
		}
	}
	// TestMain guards the CLI's ambient home before installing its own
	// process sandbox. Other packages must not share that guarded namespace.
	// Groups are serial: retain the worker ceiling and the existing deadline.
	if len(others) > 0 {
		if err := j.runRestGroup(others, 0, "rest"); err != nil {
			return err
		}
	}
	return j.runRestGroup([]string{j.cliPackage}, 1, "rest-cli")
}

func (j *job) runRestGroup(packages []string, slot int, tag string) error {
	started := time.Now()
	r, events, err := j.goTest(packages, j.workers, tag, slot)
	if err != nil {
		return err
	}
	for key, result := range events.tests {
		j.durations["rest "+key.pkg+" "+key.name] = result.seconds
	}
	type retry struct {
		pkg    string
		failed []string
		first  string
	}
	retries := []retry{}
	globalHard := unattributedHardEvidence(events, packages)
	if globalHard != "" {
		j.fail("rest unattributed hard failure", globalHard)
	}
	for _, pkg := range packages {
		action := events.packages[pkg]
		failed := failedTests(events, pkg)
		j.printf("%-7s %s %.1fs\n", action.status, pkg, action.seconds)
		attributed := events.packageOutput[pkg]
		if (action.status == "pass" || action.status == "skip") && len(failed) == 0 && !hardFailure(attributed) {
			continue
		}
		text := eventEvidence(events, pkg, r.output)
		if action.status == "fail" && !hardFailure(attributed) && (attributed != "" || globalHard == "") {
			retries = append(retries, retry{pkg, failed, text})
		} else {
			j.packageFailure(pkg, events, text)
		}
	}
	if r.code != 0 && len(retries) == 0 && len(j.failures) == 0 {
		j.fail("rest", r.output)
	}
	for i, retry := range retries {
		retryTag := fmt.Sprintf("rerun-%d-%s", i, filepath.Base(retry.pkg))
		if tag != "rest" {
			retryTag = tag + "-" + retryTag
		}
		r, events, err := j.goTest([]string{retry.pkg}, 1, retryTag, slot)
		if err != nil {
			return err
		}
		text := eventEvidence(events, retry.pkg, r.output)
		unattributed := unattributedHardEvidence(events, []string{retry.pkg})
		if unattributed != "" {
			j.fail("rest unattributed hard failure", unattributed)
		}
		if r.code == 0 && events.packages[retry.pkg].status == "pass" && len(failedTests(events, retry.pkg)) == 0 && !hardFailure(events.packageOutput[retry.pkg]) && unattributed == "" {
			j.markFlaky(strings.TrimSpace("rest "+retry.pkg+" "+strings.Join(retry.failed, " ")), retry.first)
		} else {
			j.packageFailure(retry.pkg, events, text)
		}
	}
	j.printf("%s: done in %.0fs\n", tag, time.Since(started).Seconds())
	return nil
}
func newJob(ctx context.Context, opts RunOptions, planner *Planner) (*job, error) {
	base := envValue(opts.Env, "RUNNER_TEMP")
	if base == "" {
		base = os.TempDir()
		if info, err := os.Stat("/tmp"); err == nil && info.IsDir() {
			base = "/tmp"
		}
	}
	root := filepath.Join(base, "gt")
	if err := os.MkdirAll(root, 0700); err != nil {
		return nil, err
	}
	scratch, err := os.MkdirTemp(root, "j")
	if err != nil {
		return nil, err
	}
	for _, name := range []string{"logs", "coverage"} {
		if err := os.MkdirAll(filepath.Join(scratch, name), 0700); err != nil {
			return nil, err
		}
	}
	env := baseEnv(opts.Env)
	out, err := planner.exec(ctx, env, "go", "env", "-json", "GOCACHE", "GOMODCACHE", "GOPATH", "GOENV")
	if err != nil {
		return nil, err
	}
	var cache map[string]string
	if err := json.Unmarshal([]byte(out), &cache); err != nil {
		return nil, err
	}
	env = withEnv(env, cache)
	if opts.Windows {
		env = withEnv(env, map[string]string{"GOTMPDIR": root})
	}
	budget := opts.Budget
	if budget == 0 {
		budget = testBudget
	}
	output := opts.Output
	if output == nil {
		output = os.Stdout
	}
	return &job{ctx: ctx, root: planner.Root, scratch: scratch, env: env, race: opts.Race, windows: opts.Windows, workers: opts.Workers, deadline: time.Now().Add(budget), stdout: output, durations: map[string]float64{}}, nil
}
func RunPartition(ctx context.Context, opts RunOptions) (report Report, err error) {
	policy, err := LoadPolicy()
	if err != nil {
		return report, err
	}
	parts, err := ParsePartition(opts.Partition, policy)
	if err != nil {
		return report, err
	}
	if opts.Workers < 1 || opts.Windows && opts.Race {
		return report, fmt.Errorf("invalid worker count or Windows race setting")
	}
	planner := NewPlanner(opts.Root)
	planner.Env = baseEnv(opts.Env)
	module, err := planner.exec(ctx, planner.Env, "go", "list", "-m")
	if err != nil {
		return report, err
	}
	out, err := planner.exec(ctx, planner.Env, "go", "list", "./...")
	if err != nil {
		return report, err
	}
	packages := strings.Fields(out)
	if len(packages) == 0 || len(newSet(packages)) != len(packages) {
		return report, fmt.Errorf("package census must be nonempty and unique")
	}
	large := map[string]string{}
	all := newSet(packages)
	for group, suffix := range policy.Groups {
		large[group] = module + suffix
		if !all.has(large[group]) {
			return report, fmt.Errorf("missing large package %s", large[group])
		}
	}
	var selected set
	if opts.SelectionFile != "" {
		path := opts.SelectionFile
		if !filepath.IsAbs(path) {
			path = filepath.Join(planner.Root, path)
		}
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return report, readErr
		}
		manifest, decodeErr := DecodeManifest(data)
		if decodeErr != nil {
			return report, decodeErr
		}
		names, validateErr := planner.Validate(ctx, manifest, opts.Target, packages)
		if validateErr != nil {
			return report, validateErr
		}
		if !slices.Contains(manifest.Matrix.Include, MatrixRow{opts.Target, opts.Partition}) {
			return report, fmt.Errorf("this job is absent from the validated selection matrix")
		}
		if manifest.Mode == "affected" {
			selected = newSet(names)
		}
	} else if opts.Target != "" {
		return report, fmt.Errorf("a selection target requires --selection")
	}
	j, err := newJob(ctx, opts, planner)
	if err != nil {
		return report, err
	}
	j.cliPackage = module + "/cmd/gortex"
	defer func() {
		report = Report{j.scratch, j.failures, j.flaky, j.durations}
		err = errors.Join(err, j.report())
		if j.race {
			_, coverageErr := mergeCoverage(j.coverage, filepath.Join(planner.Root, "coverage.out"))
			err = errors.Join(err, coverageErr)
		}
		j.printf("\nlogs: %s\n", filepath.Join(j.scratch, "logs"))
		if len(j.failures) > 0 {
			err = errors.Join(err, ErrTestsFailed)
		}
		err = errors.Join(err, ctx.Err())
	}()
	j.printf("partition %s: race=%t workers=%d scratch=%s\n", opts.Partition, opts.Race, opts.Workers, j.scratch)
	for _, part := range parts {
		if part.Group == "rest" {
			rest := []string{}
			for _, pkg := range packages {
				if !slices.ContainsFunc([]string{"indexer", "mcp", "store"}, func(group string) bool { return large[group] == pkg }) && (selected == nil || selected.has(pkg)) {
					rest = append(rest, pkg)
				}
			}
			if len(rest) > 0 {
				if err := j.runRest(rest); err != nil {
					return report, err
				}
			}
			if runtime.GOOS == "darwin" && !opts.Race && (selected == nil || selected.has(large["indexer"])) {
				dir, err := planner.exec(ctx, planner.Env, "go", "list", "-f", "{{.Dir}}", large["indexer"])
				if err != nil {
					return report, err
				}
				if err := j.runPackage("indexer", large["indexer"], Partition{Group: "indexer", Count: 1}, dir, true, darwinRaceTests); err != nil {
					return report, err
				}
			}
		} else if selected == nil || selected.has(large[part.Group]) {
			dir, err := planner.exec(ctx, planner.Env, "go", "list", "-f", "{{.Dir}}", large[part.Group])
			if err != nil {
				return report, err
			}
			if err := j.runPackage(part.Group, large[part.Group], part, dir, opts.Race, nil); err != nil {
				return report, err
			}
		}
	}
	return report, nil
}
