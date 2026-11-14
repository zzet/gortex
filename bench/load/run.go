package main

import (
	"context"
	"debug/buildinfo"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type runOptions struct {
	PairedOwnACKTransactions bool
	RetainReadSourceFailures bool
	NightlySeed              string
	EditACKMode              string
	Binary                   string
	Manifest                 string
	Scratch                  string
	Output                   string
	GitSHA                   string
	Profile                  string
	Host                     string
	Scenario                 string
	Agents                   int
	Editors                  int
	Worktrees                int
	Seed                     int64
	Duration                 time.Duration
	PRSmokeDrain             time.Duration
	PRSmokeDrainVersion      int
	Deadline                 time.Duration
	WarmTimeout              time.Duration
	StopTimeout              time.Duration
	CPUProfile               time.Duration
	Gauges                   bool
	HeapProfiles             bool
	WorktreeSteps            []int
	KeepState                bool
	SweepRepetitions         int
	SweepBudget              time.Duration
	EditMean                 time.Duration
	EditCount                int
	SearchReadbackAcceptance bool
	EditGateModes            bool
	EditGateSeed             string
	EditRepos                int
	EditProbeRole            string
	editProbeBaseline        bool
	Discovery                discoveryOptions
	Startup                  startupOptions
	pairedDriver             func(context.Context, *privateRun, []*socketClient, []*socketClient, *Phase) error
	smokeState               *privateRun // only the fixed smoke entry lends its freshly owned store
	smokeMarkers             []string
}

func parseRun(args []string) (runOptions, error) {
	var o runOptions
	f := flag.NewFlagSet("load run", flag.ContinueOnError)
	bindRunFlags(f, &o)
	f.BoolVar(&o.RetainReadSourceFailures, "retain-read-source-failures", false, "opt-in private bounded raw wire evidence for failed read.source attempts")
	bindMemoryFlags(f, &o)
	if err := f.Parse(args); err != nil {
		return o, err
	}
	if f.NArg() != 0 {
		return o, errors.New("unexpected positional run arguments")
	}
	if o.EditRepos < 1 {
		return o, errors.New("--edit-repos must be positive")
	}
	o.editProbeBaseline = o.EditProbeRole == "baseline"
	return o, errors.Join(validateRunOptions(o), validateMemoryOptions(o))
}

func bindRunFlags(f *flag.FlagSet, o *runOptions) {
	f.StringVar(&o.EditACKMode, "edit-ack", "", "owned daemon edit.ack policy: local or full; omit to preserve the full default")
	f.StringVar(&o.Binary, "binary", "", "Gortex binary (never an existing daemon)")
	f.StringVar(&o.Manifest, "manifest", "", "pinned JSON corpus manifest; source roots are read-only")
	f.StringVar(&o.Scratch, "scratch", "", "artifact/private-clone parent outside all source roots")
	f.StringVar(&o.Output, "out", "", "run JSON path (default inside owned artifact directory)")
	f.StringVar(&o.GitSHA, "git-sha", "", "full binary revision if absent from Go build metadata")
	f.StringVar(&o.Profile, "profile", "agent", "agent (default) or explicit saturation")
	f.StringVar(&o.Host, "host", "native", "native, reference, or floor (validated, never provisioned)")
	f.StringVar(&o.Scenario, "scenario", "read", "read, edit, many, many-edit, worktrees, worktree-discovery, cold, restart, sweep, idle-stop, edit-and-probe, all")
	f.IntVar(&o.Agents, "agents", 4, "pure reading agents (may be 0 for edit, many-edit, or edit-and-probe; idle-stop requires 0)")
	f.IntVar(&o.Editors, "edit-agents", 4, "combined editing/readback/reading agents in edit scenarios, in addition to pure readers")
	f.IntVar(&o.Worktrees, "worktrees", 4, "linked worktrees in addition to primary")
	f.Int64Var(&o.Seed, "seed", 1, "reproducible per-agent request/think plan seed")
	f.DurationVar(&o.Duration, "duration", time.Minute, "measured workload phase duration")
	f.DurationVar(&o.PRSmokeDrain, "pr-smoke-drain", 0, "opt-in standalone PR drain budget after the offered duration; unsupported for paired runs")
	f.IntVar(&o.PRSmokeDrainVersion, "pr-smoke-drain-version", 1, "PR drain identity: 1 for reads, explicit 2 for edits and their visibility probes")
	f.DurationVar(&o.Deadline, "deadline", 120*time.Second, "product call/visibility deadline, independently of phase clipping")
	f.DurationVar(&o.WarmTimeout, "warm-timeout", 30*time.Minute, "startup/enrichment/public analysis gate budget")
	f.DurationVar(&o.StopTimeout, "stop-timeout", 30*time.Second, "graceful owned-daemon shutdown budget before force kill")
	f.DurationVar(&o.CPUProfile, "cpu-profile", 0, "CPU profile duration during eligible 8-reader or primary+4-worktree phase")
	f.BoolVar(&o.KeepState, "keep-state", false, "retain private corpus/store for native diagnostics only (default removes completed scenario state)")
	f.IntVar(&o.SweepRepetitions, "sweep-repetitions", 15, "samples per benchmark operation (smoke may explicitly use 1)")
	f.DurationVar(&o.SweepBudget, "sweep-duration", 10*time.Minute, "separate total budget for the repeated paced sweep")
	bindStartupFlags(f, &o.Startup)
	f.DurationVar(&o.EditMean, "edit-mean", 0, "edit-and-probe per-editor Poisson mean; 0 preserves the 15s default")
	f.IntVar(&o.EditRepos, "edit-repos", 1, "edit-and-probe: one repository (default), or exactly six distinct pinned repositories")
	bindDiscoveryFlags(f, &o.Discovery)
	f.IntVar(&o.EditCount, "edit-count", 100, "edit-and-probe: 100 logical edits, or explicit 1-edit diagnostic")
	f.BoolVar(&o.SearchReadbackAcceptance, "search-readback-acceptance", false, "explicit search acceptance: 1000 immediate same-session search witnesses at 1, 4 or 8 editors")
	f.BoolVar(&o.EditGateModes, "edit-gate-modes", false, "edit-and-probe gate: rotate five probe surfaces over 100 total edits; JSON ACKs remain unchanged")
	f.StringVar(&o.NightlySeed, "nightly-workload-seed", "", "opt-in measured one-second search delay: search-delay-v1 (edit4), search-delay-read16-v1 (read16)")
	f.StringVar(&o.EditGateSeed, "edit-gate-seed", "", "explicit gate-only fault: ack-wait or closed-read-gate; requires a seed-tagged binary")
	f.StringVar(&o.EditProbeRole, "edit-probe-role", "", "edit-and-probe standalone role: candidate (default; phase accounting required) or baseline (absent phase timings reported unavailable)")
}

// The explicit search profile measures the first read after a local ACK.
// Use the same effective policy for owned startup and workload identity.
func (o runOptions) editACKPolicy() string {
	if o.SearchReadbackAcceptance {
		return "local"
	}
	return o.EditACKMode
}

func validateRunOptions(o runOptions) error {
	if err := validateNightlyWorkloadSeed(o); err != nil {
		return err
	}
	if o.EditACKMode != "" && o.EditACKMode != "local" && o.EditACKMode != "full" {
		return errors.New("--edit-ack must be local or full when explicitly set")
	}
	if o.SearchReadbackAcceptance && o.EditACKMode == "full" {
		return errors.New("--search-readback-acceptance requires local ACKs; --edit-ack full conflicts with that profile")
	}
	if o.SearchReadbackAcceptance && (o.Scenario != editProbeScenario || o.EditCount != 1000 || o.Editors != 1 && o.Editors != 4 && o.Editors != 8 || o.Agents != 0 || o.Worktrees != 0 || o.EditRepos != 1) {
		return errors.New("--search-readback-acceptance requires edit-and-probe, 1000 edits, 1/4/8 editors, one primary repository and no pure readers or worktrees")
	}
	if o.EditGateSeed != "" && (!o.EditGateModes || o.EditGateSeed != "ack-wait" && o.EditGateSeed != "closed-read-gate") {
		return errors.New("--edit-gate-seed requires --edit-gate-modes and ack-wait or closed-read-gate")
	}
	if o.EditGateModes && (o.Scenario != editProbeScenario || o.EditCount != 100 || o.Agents != 0) {
		return errors.New("--edit-gate-modes requires edit-and-probe, 100 total edits and no extra pure readers")
	}
	if o.EditMean < 0 || o.EditMean > 0 && o.Scenario != editProbeScenario {
		return errors.New("--edit-mean requires edit-and-probe and a nonnegative duration; 0 uses the 15s default")
	}
	if o.EditRepos < 0 || o.EditRepos > 1 && (o.EditRepos != 6 || o.Scenario != editProbeScenario) {
		return errors.New("--edit-repos selects one repository or exactly six for edit-and-probe only")
	}

	if err := validatePRSmokeDrain(o); err != nil {
		return err
	}
	if o.Binary == "" || o.Manifest == "" || o.Scratch == "" || o.Worktrees < 0 || o.Duration <= 0 || o.Deadline <= 0 || o.WarmTimeout <= 0 || o.StopTimeout <= 0 || o.CPUProfile < 0 || o.CPUProfile > o.Duration {
		return errors.New("run requires --binary, --manifest, --scratch and nonnegative counts and positive budgets")
	}
	if err := validateAgentCounts(o.Scenario, o.Agents, o.Editors); err != nil {
		return err
	}
	if err := o.Discovery.validate(o.Scenario, o.Worktrees); err != nil {
		return err
	}
	if err := o.Startup.validate(o.Scenario); err != nil {
		return err
	}
	if o.Scenario == "idle-stop" && (o.Worktrees != 0 || o.CPUProfile != 0 || o.Profile != "agent") {
		return errors.New("idle-stop requires an agent-profile standalone run with no worktrees or CPU profiling")
	}
	if _, err := parseProfile(o.Profile); err != nil {
		return err
	}
	if o.Scenario == editProbeScenario && (o.Profile != "agent" || o.Agents > editProbeMaxReaders || o.Editors < 1 || o.Editors > editProbeMaxEditors || o.Worktrees > 15 || o.CPUProfile != 0 || !o.SearchReadbackAcceptance && o.EditCount != 1 && o.EditCount != 100 || o.EditCount == 1 && o.Editors != 1) {
		return fmt.Errorf("edit-and-probe requires --profile agent --agents 0..%d --edit-agents 1..%d on one repository or explicit routed checkouts, at most 15 linked worktrees, no CPU profile, and --edit-count 100 (or an explicit 1-edit, 1-editor diagnostic)", editProbeMaxReaders, editProbeMaxEditors)
	}
	if o.Scenario == editProbeScenario && (o.EditRepos > 1 || o.Worktrees > 0) && (o.EditRepos > 1 && o.Worktrees > 0 || o.Agents != 0 || o.EditCount != 100 || o.Editors < max(o.EditRepos, o.Worktrees+1)) {
		return errors.New("routed edit-and-probe selects six repositories or linked checkouts, 100 edits, no pure readers and at least one fixed editor per checkout")
	}
	if o.EditProbeRole != "" && (o.Scenario != editProbeScenario || o.EditProbeRole != "candidate" && o.EditProbeRole != "baseline") {
		return errors.New("--edit-probe-role is candidate or baseline, for edit-and-probe only")
	}
	if o.SweepRepetitions < 1 || o.SweepBudget <= 0 {
		return errors.New("sweep repetitions and duration must be positive")
	}
	if o.KeepState && o.Host != "native" {
		return errors.New("reference/floor runs clean completed scenario stores; --keep-state is native diagnostics only")
	}
	if o.CPUProfile%time.Second != 0 {
		return errors.New("--cpu-profile requires a whole number of seconds")
	}
	activeAgents := o.Agents
	if o.Scenario == "edit" || o.Scenario == "many-edit" || o.Scenario == "all" || o.Scenario == editProbeScenario {
		activeAgents += o.Editors
	}
	activeWorktrees := 1
	if o.Scenario == "worktrees" || o.Scenario == "all" || o.Scenario == editProbeScenario && o.Worktrees > 0 {
		activeWorktrees += o.Worktrees
	}
	if o.Host == "floor" && (activeAgents > 4 || activeWorktrees > 4) {
		return errors.New("floor supports at most 4 total agents and 4 total worktrees (including primary); set counts explicitly")
	}
	if _, err := selectedScenarios(o); err != nil {
		return err
	}
	return nil
}

func selectedScenarios(o runOptions) ([]string, error) {
	if o.Scenario == checkoutDiscoveryScenario {
		if o.Agents+o.Editors < 2 || o.Host == "floor" {
			return nil, errors.New("worktree-discovery requires at least two reading/editing sessions and a native or reference host")
		}
		return []string{checkoutDiscoveryScenario}, nil
	}
	if o.Scenario == "idle-stop" {
		return []string{"idle-stop"}, nil
	}
	all := []string{"read", "edit", "many", "many-edit", "worktrees", "cold", "restart", "sweep"}
	if o.Scenario == startupScenario {
		return []string{startupScenario}, nil
	}
	if o.Scenario == editProbeScenario {
		return []string{editProbeScenario}, nil
	}
	if o.Scenario == "all" {
		if o.Host == "floor" {
			return []string{"read", "edit", "worktrees", "cold", "restart", "sweep"}, nil
		}
		return all, nil
	}
	for _, scenario := range all {
		if scenario == o.Scenario {
			if o.Host == "floor" && strings.HasPrefix(scenario, "many") {
				return nil, errors.New("many-repository scenarios are outside the one-repository floor envelope")
			}
			return []string{scenario}, nil
		}
	}
	return nil, fmt.Errorf("unknown scenario %q", o.Scenario)
}

func binaryRevision(binary, explicit string) (string, error) {
	info, err := buildinfo.ReadFile(binary)
	if err != nil {
		return "", fmt.Errorf("read binary Go build metadata: %w", err)
	}
	sha := ""
	for _, setting := range info.Settings {
		if setting.Key == "vcs.revision" {
			sha = setting.Value
		}
		if setting.Key == "vcs.modified" && setting.Value == "true" {
			return "", errors.New("baseline binary has modified-source build metadata")
		}
	}
	if explicit != "" {
		if sha != "" && sha != explicit {
			return "", errors.New("--git-sha disagrees with binary metadata")
		}
		sha = explicit
	}
	if !commitPattern.MatchString(sha) {
		return "", errors.New("binary needs a full pinned git revision in build metadata or --git-sha")
	}
	return sha, nil
}

func runCommand(ctx context.Context, args []string) error {
	o, err := parseRun(args)
	if err != nil {
		return err
	}
	ctx = withReadSourceFailureEvidence(ctx, o.RetainReadSourceFailures)
	corpora, err := loadManifest(o.Manifest)
	if err != nil {
		return err
	}
	if err := validateOutputRoots(corpora, o.Scratch, o.Output); err != nil {
		return err
	}
	if o.Discovery.Prepare != "" {
		return prepareDiscoveryState(ctx, o, corpora)
	}
	sha, err := binaryRevision(o.Binary, o.GitSHA)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(o.Scratch, 0o700); err != nil {
		return err
	}
	host, group, err := inspectHost(o.Host, o.Scratch)
	if err != nil {
		return err
	}
	outputRoot, err := os.MkdirTemp(o.Scratch, "load-report-")
	if err != nil {
		return err
	}
	if o.Output == "" {
		o.Output = filepath.Join(outputRoot, "run.json")
	}
	report, err := newRunReport(o, corpora, sha, host, outputRoot)
	if err != nil {
		return err
	}
	scenarios, _ := selectedScenarios(o)
	for _, scenario := range scenarios {
		phase, sweep, artifacts, crashes, version, err := runPhase(ctx, o, scenario, corpora, group, outputRoot)
		addPhase(&report, phase, sweep, artifacts, crashes, version, err)
		if err := ctx.Err(); err != nil {
			report.Errors = append(report.Errors, err.Error())
			break
		}
	}
	if err := finishReport(&report, len(scenarios)); err != nil {
		return err
	}
	if err := writeReport(o.Output, report); err != nil {
		return err
	}
	fmt.Fprintln(os.Stderr, "load report:", o.Output)
	if !report.Complete {
		return fmt.Errorf("incomplete run; all samples/errors retained in %s", o.Output)
	}
	return nil
}

func newRunReport(o runOptions, corpora []Corpus, sha string, host Host, outputRoot string) (Report, error) {
	report := Report{SchemaVersion: reportVersion, RunID: filepath.Base(outputRoot), GitSHA: sha, Binary: o.Binary, Started: time.Now().UTC(), Profile: o.Profile, Seed: o.Seed, Host: host, Corpus: corpora, Metrics: make(map[string]Metric), Artifacts: map[string]string{"root": outputRoot}, Crashes: make(map[string]int)}
	if o.RetainReadSourceFailures {
		report.FailureCapture = &readSourceFailureConfig{MaxRecords: readSourceFailureRecords, MaxBytes: readSourceFailureBytes, Encoding: "base64"}
	}
	for _, corpus := range corpora {
		ops, err := readingPlan(corpus)
		if o.Scenario == "sweep" {
			ops, err = benchmarkPlan(corpus)
		}
		if err != nil {
			return report, err
		}
		report.Workload.Operations = append(report.Workload.Operations, ops...)
	}
	if o.Scenario == editProbeScenario {
		plan, err := planEditProbeRoutes(corpora, o)
		if err != nil {
			return report, err
		}
		report.Workload.EditProbe = &plan
		report.Workload.Operations, err = editProbeRouteOperations(plan, corpora)
		if err != nil {
			return report, err
		}
		report.Workload.SessionModel = plan.SessionModel
	}
	report.NightlySeed = o.NightlySeed
	if o.PairedOwnACKTransactions {
		report.Workload.PairedMeasurementUnit = pairedOwnACKTransactionMethod
	}
	report.Workload.EditGateSeed = o.EditGateSeed
	report.Workload.EditACKMode = o.editACKPolicy()
	report.Workload.Scenario, report.Workload.Agents, report.Workload.EditAgents, report.Workload.LinkedWorktrees = o.Scenario, o.Agents, o.Editors, o.Worktrees
	report.Workload.DurationSec, report.Workload.DeadlineSec, report.Workload.Seed, report.Workload.Profile = o.Duration.Seconds(), o.Deadline.Seconds(), o.Seed, o.Profile
	if o.PRSmokeDrain > 0 {
		report.Workload.PRSmokeDrain = &PRSmokeDrain{Version: prSmokeDrainVersion(o), OfferedSeconds: o.Duration.Seconds(), MaxDrainSeconds: o.PRSmokeDrain.Seconds()}
	}
	report.Workload.CPUProfileSec, report.Workload.WarmTimeoutSec, report.Workload.StopTimeoutSec, report.Workload.KeepState = o.CPUProfile.Seconds(), o.WarmTimeout.Seconds(), o.StopTimeout.Seconds(), o.KeepState
	report.Workload.SessionModel = "per_agent"
	report.Workload.SweepRepetitions, report.Workload.SweepBudgetSec = o.SweepRepetitions, o.SweepBudget.Seconds()
	if o.Scenario == "sweep" {
		report.Workload.SessionModel = "per_operation_isolated"
	}
	if o.Scenario == "all" {
		report.Workload.SessionModel = "per_agent_load_and_per_operation_isolated_sweep"
	}
	if report.Workload.EditProbe != nil {
		report.Workload.SessionModel = report.Workload.EditProbe.SessionModel
		builds := 1
		if o.pairedDriver != nil {
			builds = 2
		}
		budget, err := report.Workload.EditProbe.budget(o.Deadline, builds)
		if err != nil {
			return report, err
		}
		report.Workload.DurationSec = budget.Seconds()
	}
	var err error
	report.RequestPlan, err = hashJSON(report.Workload)
	return report, err
}

func addPhase(report *Report, phase Phase, sweep []SweepEntry, artifacts map[string]string, crashes map[string]int, version string, err error) {
	scenario := phase.Name
	if phase.DurationSec > 0 {
		report.Phases = append(report.Phases, phase)
	}
	report.Sweep = append(report.Sweep, sweep...)
	for name, path := range artifacts {
		report.Artifacts[scenario+"."+name] = path
	}
	for name, count := range crashes {
		report.Crashes[name] += count
	}
	report.DaemonVersion = version
	if err != nil {
		report.Errors = append(report.Errors, scenario+": "+err.Error())
	}
}

func finishReport(report *Report, expectedPhases int) error {
	var actualOperations []request
	for _, phase := range report.Phases {
		actualOperations = append(actualOperations, phase.Operations...)
	}
	if len(actualOperations) > 0 {
		report.Workload.Operations = actualOperations
		var err error
		report.RequestPlan, err = hashJSON(report.Workload)
		if err != nil {
			return err
		}
	}
	report.Complete = len(report.Errors) == 0 && len(report.Phases) == expectedPhases
	return nil
}

func captureReceipt(ctx context.Context, p *privateRun, client *socketClient, phase *Phase) {
	statusCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	statusStarted := time.Now()
	status, err := control(statusCtx, p.Socket, "status", nil)
	statusFinished := time.Now()
	cancel()
	if err != nil {
		phase.Errors = append(phase.Errors, "status receipt: "+err.Error())
	} else {
		phase.Status = append(phase.Status, status)
		pid := 0
		if p.process != nil && p.process.Process != nil {
			pid = p.process.Process.Pid
		}
		recordStatusCapture(phase, pid, statusStarted, statusFinished)
	}
	statsCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	stats, err := client.call(statsCtx, "resources/read", map[string]any{"uri": "gortex://stats"})
	cancel()
	if err == nil {
		stats, err = textPayload(stats, "contents")
	}
	if err != nil {
		phase.Errors = append(phase.Errors, "stats receipt: "+err.Error())
	} else {
		phase.Stats = append(phase.Stats, stats)
	}
}

func copyJSON(value any) map[string]any {
	data, _ := json.Marshal(value)
	var result map[string]any
	_ = json.Unmarshal(data, &result)
	return result
}
