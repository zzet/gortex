package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"syscall"
	"time"
)

const smokeFixtureRevision = "c6c6de19da02fc698db7a89f9922a8ec49cc61b5"
const smokeFixtureTree = "47debc33a46a06eed403a3ea455feda97ca9a609"

// These are operational measurement bounds. The original full-job p95 target
// remains fifteen minutes; its acceptance is explicitly deferred, not widened.
const (
	smokeMeasurementWindow  = 120 * time.Minute
	smokeCleanupAllowance   = 2 * time.Minute
	smokeDeferredWallTarget = 15 * time.Minute
)

var smokeSteps = []string{"load-16", "loadedit-4", "tools-single-gortex", "warm-restart", "idle-stop"}

// The entry owns no extra process supervisor: collectLane already joins the
// load client's daemon cleanup, and collectMicrobench bounds binary exits.
func smokeDriverCommand(args []string, stdout, stderr io.Writer) int {
	started := time.Now().UTC()
	f := flag.NewFlagSet("smoke", flag.ContinueOnError)
	f.SetOutput(stderr)
	candidate := f.String("candidate-binary", "", "clean current-job candidate daemon")
	load := f.String("load-binary", "", "current-job load client")
	bundle := f.String("main-bundle", "", "verified downloaded exact-main full bundle")
	tests := f.String("candidate-builds", "", "current-job prepared test executables")
	fixture := f.String("fixture", "", "owned prepared contemporary fixture directory")
	out := f.String("out", "", "NEW evidence directory outside the fixture")
	budgets := f.String("budgets", "bench/budgets.yaml", "full strict policy")
	offer := f.Duration("offer", 20*time.Second, "version 2 PR-only offered window: 20s")
	retainFailures := f.Bool("retain-read-source-failures", false, "opt in to bounded read.source wire failure evidence in every smoke report")
	if err := f.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if f.NArg() != 0 || *candidate == "" || *load == "" || *bundle == "" || *tests == "" || *fixture == "" || *out == "" || *offer != 20*time.Second {
		fmt.Fprintln(stderr, "smoke needs candidate/load/main-bundle/candidate-builds/fixture/out and the original 20s PR profile")
		return 2
	}
	workflow := currentBuildWorkflow(os.Getenv("GITHUB_SHA"))
	if err := validateSmokeWorkflow(workflow); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	output := pathAbsolute(*out)
	mainBundle, candidateTests, prepared := pathAbsolute(*bundle), pathAbsolute(*tests), pathAbsolute(*fixture)
	_, metadata, err := verifiedMainBundle(mainBundle)
	if err == nil && (metadata.Microbench == nil || metadata.Microbench.FixtureRevision != smokeFixtureRevision || metadata.Microbench.FixtureTree != smokeFixtureTree) {
		err = errors.New("smoke requires a full main bundle using the reviewed contemporary fixture")
	}
	var setup fixtureSetup
	if err == nil {
		err = readStrictJSON(filepath.Join(prepared, "fixture.json"), &setup)
	}
	if err == nil && (setup.Version != 1 || setup.Kind != "fixture_setup_only" || !setup.Complete || setup.Error != "" || setup.Repository != fixtureRepository || setup.Revision != smokeFixtureRevision || setup.Tree != smokeFixtureTree) {
		err = errors.New("smoke fixture is not the complete reviewed contemporary snapshot")
	}
	micro := microConfig{Version: 1, MainRevision: metadata.Revision, CandidateRevision: workflow.Revision, MainBundle: mainBundle, CandidateBuilds: candidateTests, Fixture: filepath.Join(prepared, "repo"), FixtureRevision: smokeFixtureRevision, Benchstat: filepath.Join(mainBundle, bundledBenchstatName), Benchtime: "1x", Timeout: "2m", Repeats: 3, Binaries: map[string]microPair{}}
	for _, pkg := range microPackages {
		micro.Binaries[pkg] = microPair{Main: filepath.Join(mainBundle, microTestName(pkg)), Candidate: filepath.Join(candidateTests, microTestName(pkg))}
	}
	if err == nil {
		err = validateMicroFixture(micro, output)
	}
	var builds map[string]map[string]executableObservation
	if err == nil {
		builds, err = admitMicroBundle(micro, workflow)
	}
	c := laneConfig{Version: 1, Lane: "smoke", HostClass: hostedSmokeClass, Load: pathAbsolute(*load), Manifest: filepath.Join(output, "manifest.json"), MainArtifact: mainBundle, Builds: map[string]laneBuild{"main": {Binary: filepath.Join(mainBundle, mainArtifactBinary), Revision: metadata.Revision}, "candidate": {Binary: pathAbsolute(*candidate), Revision: workflow.Revision}}, Repeats: 3, Duration: offer.String(), Deadline: "120s", PRSmokeDrain: "120s", WarmTimeout: "120m", StopTimeout: "30s", SweepDuration: "2m", SweepRepetitions: 1, started: started}
	c.RetainReadSourceFailures = *retainFailures
	if err == nil {
		c.Machine, err = os.Hostname()
	}
	if err == nil {
		_, err = prepareSmokeProvenance(c, workflow)
	}
	var p policy
	if err == nil {
		var data []byte
		data, err = os.ReadFile(*budgets)
		if err == nil {
			p, err = loadPolicy(data)
		}
	}
	var plan lanePlan
	if err == nil {
		plan, err = orderedSmokePlan(p, c)
	}
	if err == nil {
		err = os.Mkdir(output, 0o700)
	}
	if err == nil {
		err = writeSmokeManifest(c.Manifest, micro.Fixture)
	}
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	signals, cancelSignals := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancelSignals()
	// Reserve two minutes for an interrupted load client's owned stop/receipt.
	ctx, cancel := context.WithDeadline(signals, started.Add(smokeMeasurementWindow))
	defer cancel()
	workloadErr := collectSmokeSequence(ctx, c, p, plan, filepath.Join(output, "workload"), stdout, runLaneProcess)
	microErr := ctx.Err()
	if microErr == nil {
		microErr = validateMicroFixture(micro, filepath.Join(output, "microbench"))
	}
	if microErr == nil {
		microErr = collectMicrobench(ctx, micro, filepath.Join(output, "microbench"), workflow, builds, executeMicroProcess)
	}
	finished := time.Now().UTC()
	receipt, err := smokeMeasurementReceipt(started, finished, errors.Join(workloadErr, microErr))
	err = errors.Join(err, writeNewJSON(filepath.Join(output, "smoke.json"), receipt))
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	fmt.Fprintln(stdout, "five-step smoke and microbenchmark evidence retained; empirical lane acceptance remains separate:", output)
	return 0
}

// A complete measurement is separate from the deferred wall-time acceptance.
// In particular, a twenty-minute successful collection must not be presented
// as meeting the original fifteen-minute full-job p95 target.
func smokeMeasurementReceipt(started, finished time.Time, runErr error) (map[string]any, error) {
	elapsed := finished.Sub(started)
	if elapsed > smokeMeasurementWindow+smokeCleanupAllowance {
		runErr = errors.Join(runErr, errors.New("smoke exceeded its measurement safety bound including owned cleanup"))
	}
	receipt := map[string]any{
		"started": started, "finished": finished, "wall_seconds": elapsed.Seconds(),
		"pass": runErr == nil, "formal_acceptance": false,
		"timing_acceptance": "deferred", "original_full_job_p95_target_seconds": smokeDeferredWallTarget.Seconds(),
		"entry_exceeded_original_wall_target": elapsed > smokeDeferredWallTarget,
		"measurement_bound_seconds":           smokeMeasurementWindow.Seconds(), "cleanup_allowance_seconds": smokeCleanupAllowance.Seconds(),
	}
	if runErr != nil {
		receipt["error"] = runErr.Error()
	}
	return receipt, runErr
}

func orderedSmokePlan(p policy, c laneConfig) (lanePlan, error) {
	plan, err := makeLanePlan(p, c)
	if err != nil {
		return plan, err
	}
	plan.Execution = "two_owned_smoke_stores_v1"
	seen := map[string]bool{}
	for _, task := range plan.Tasks {
		if !slices.Contains(smokeSteps, task.Scenario) {
			return plan, errors.New("smoke policy includes an unplanned step; no row may be silently omitted")
		}
		seen[task.Scenario] = true
	}
	if len(seen) != len(smokeSteps) {
		return plan, errors.New("smoke policy must retain all five required steps")
	}
	position := func(task laneTask) int {
		role := 0
		if task.Role == "candidate" {
			role = 1
		}
		if task.Repeat%2 == 0 {
			role = 1 - role
		}
		return ((task.Repeat-1)*2+role)*len(smokeSteps) + slices.Index(smokeSteps, task.Scenario)
	}
	slices.SortStableFunc(plan.Tasks, func(a, b laneTask) int { return position(a) - position(b) })
	return plan, nil
}

func writeSmokeManifest(path, repo string) error {
	anchor := "func keyOf(e *Edge) edgeKey {\n\treturn edgeKey{From: e.From, To: e.To, Kind: e.Kind, FilePath: e.FilePath, Line: e.Line}\n}"
	return writeNewJSON(path, []map[string]any{{"label": "gortex", "source": repo, "commit": smokeFixtureRevision, "file": "internal/graph/graph.go", "symbol": "internal/graph/graph.go::fnv1aEdge", "query": "fnv1aEdge", "edit_anchor": anchor, "edit_template": "\n\nfunc {{marker}}() {}\n", "proposed_signature": "func fnv1aEdge(seed uint64, k edgeKey, reversed bool) (uint64, error)"}})
}
