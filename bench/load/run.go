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
	Binary           string
	Manifest         string
	Scratch          string
	Output           string
	GitSHA           string
	Profile          string
	Host             string
	Scenario         string
	Agents           int
	Editors          int
	Worktrees        int
	Seed             int64
	Duration         time.Duration
	Deadline         time.Duration
	WarmTimeout      time.Duration
	StopTimeout      time.Duration
	CPUProfile       time.Duration
	KeepState        bool
	SweepRepetitions int
	SweepBudget      time.Duration
	pairedDriver     func(context.Context, *privateRun, []*socketClient, []*socketClient, *Phase) error
}

func parseRun(args []string) (runOptions, error) {
	var o runOptions
	f := flag.NewFlagSet("load run", flag.ContinueOnError)
	bindRunFlags(f, &o)
	if err := f.Parse(args); err != nil {
		return o, err
	}
	if f.NArg() != 0 {
		return o, errors.New("unexpected positional run arguments")
	}
	return o, validateRunOptions(o)
}

func bindRunFlags(f *flag.FlagSet, o *runOptions) {
	f.StringVar(&o.Binary, "binary", "", "Gortex binary (never an existing daemon)")
	f.StringVar(&o.Manifest, "manifest", "", "pinned JSON corpus manifest; source roots are read-only")
	f.StringVar(&o.Scratch, "scratch", "", "artifact/private-clone parent outside all source roots")
	f.StringVar(&o.Output, "out", "", "run JSON path (default inside owned artifact directory)")
	f.StringVar(&o.GitSHA, "git-sha", "", "full binary revision if absent from Go build metadata")
	f.StringVar(&o.Profile, "profile", "agent", "agent (default) or explicit saturation")
	f.StringVar(&o.Host, "host", "native", "native, reference, or floor (validated, never provisioned)")
	f.StringVar(&o.Scenario, "scenario", "read", "read, edit, many, many-edit, worktrees, cold, restart, sweep, all")
	f.IntVar(&o.Agents, "agents", 4, "pure reading agents (may be 0 only for edit/many-edit)")
	f.IntVar(&o.Editors, "edit-agents", 4, "combined editing/readback/reading agents in edit scenarios, in addition to pure readers")
	f.IntVar(&o.Worktrees, "worktrees", 4, "linked worktrees in addition to primary")
	f.Int64Var(&o.Seed, "seed", 1, "reproducible per-agent request/think plan seed")
	f.DurationVar(&o.Duration, "duration", time.Minute, "measured workload phase duration")
	f.DurationVar(&o.Deadline, "deadline", 120*time.Second, "product call/visibility deadline, independently of phase clipping")
	f.DurationVar(&o.WarmTimeout, "warm-timeout", 30*time.Minute, "startup/enrichment/public analysis gate budget")
	f.DurationVar(&o.StopTimeout, "stop-timeout", 30*time.Second, "graceful owned-daemon shutdown budget before force kill")
	f.DurationVar(&o.CPUProfile, "cpu-profile", 0, "CPU profile duration during eligible 8-reader or primary+4-worktree phase")
	f.BoolVar(&o.KeepState, "keep-state", false, "retain private corpus/store for native diagnostics only (default removes completed scenario state)")
	f.IntVar(&o.SweepRepetitions, "sweep-repetitions", 15, "samples per benchmark operation (smoke may explicitly use 1)")
	f.DurationVar(&o.SweepBudget, "sweep-duration", 10*time.Minute, "separate total budget for the repeated paced sweep")
}

func validateRunOptions(o runOptions) error {
	if o.Binary == "" || o.Manifest == "" || o.Scratch == "" || o.Worktrees < 0 || o.Duration <= 0 || o.Deadline <= 0 || o.WarmTimeout <= 0 || o.StopTimeout <= 0 || o.CPUProfile < 0 || o.CPUProfile > o.Duration {
		return errors.New("run requires --binary, --manifest, --scratch and nonnegative counts and positive budgets")
	}
	if err := validateAgentCounts(o.Scenario, o.Agents, o.Editors); err != nil {
		return err
	}
	if _, err := parseProfile(o.Profile); err != nil {
		return err
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
	if o.Scenario == "edit" || o.Scenario == "many-edit" || o.Scenario == "all" {
		activeAgents += o.Editors
	}
	activeWorktrees := 1
	if o.Scenario == "worktrees" || o.Scenario == "all" {
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
	all := []string{"read", "edit", "many", "many-edit", "worktrees", "cold", "restart", "sweep"}
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
	corpora, err := loadManifest(o.Manifest)
	if err != nil {
		return err
	}
	if err := validateOutputRoots(corpora, o.Scratch, o.Output); err != nil {
		return err
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
	report.Workload.Scenario, report.Workload.Agents, report.Workload.EditAgents, report.Workload.LinkedWorktrees = o.Scenario, o.Agents, o.Editors, o.Worktrees
	report.Workload.DurationSec, report.Workload.DeadlineSec, report.Workload.Seed, report.Workload.Profile = o.Duration.Seconds(), o.Deadline.Seconds(), o.Seed, o.Profile
	report.Workload.CPUProfileSec, report.Workload.WarmTimeoutSec, report.Workload.StopTimeoutSec, report.Workload.KeepState = o.CPUProfile.Seconds(), o.WarmTimeout.Seconds(), o.StopTimeout.Seconds(), o.KeepState
	report.Workload.SessionModel = "per_agent"
	report.Workload.SweepRepetitions, report.Workload.SweepBudgetSec = o.SweepRepetitions, o.SweepBudget.Seconds()
	if o.Scenario == "sweep" {
		report.Workload.SessionModel = "per_operation_isolated"
	}
	if o.Scenario == "all" {
		report.Workload.SessionModel = "per_agent_load_and_per_operation_isolated_sweep"
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
	status, err := control(statusCtx, p.Socket, "status", nil)
	cancel()
	if err != nil {
		phase.Errors = append(phase.Errors, "status receipt: "+err.Error())
	} else {
		phase.Status = append(phase.Status, status)
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
