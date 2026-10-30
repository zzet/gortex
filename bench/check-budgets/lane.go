package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"
)

type workflowProvenance struct {
	Event    string `json:"event"`
	Ref      string `json:"ref"`
	Revision string `json:"revision"`
	RunURL   string `json:"run_url"`
	Job      string `json:"job"`
	Attempt  int    `json:"run_attempt"`
}

type budgetEvidence struct {
	Lane string   `json:"lane"`
	Pass bool     `json:"pass"`
	Rows []result `json:"rows"`
}

type laneBuild struct {
	Binary   string `json:"binary"`
	Revision string `json:"revision"`
}

type laneConfig struct {
	Version          int                  `json:"schema_version"`
	Lane             string               `json:"lane"`
	HostClass        string               `json:"host_class"`
	Machine          string               `json:"machine"`
	Load             string               `json:"load_binary"`
	Manifest         string               `json:"manifest"`
	MainArtifact     string               `json:"main_artifact,omitempty"`
	Builds           map[string]laneBuild `json:"builds"`
	Repeats          int                  `json:"repeats"`
	Duration         string               `json:"duration"`
	Deadline         string               `json:"deadline"`
	WarmTimeout      string               `json:"warm_timeout"`
	StopTimeout      string               `json:"stop_timeout"`
	SweepDuration    string               `json:"sweep_duration"`
	SweepRepetitions int                  `json:"sweep_repetitions"`
	started          time.Time
}

type laneTask struct {
	Scenario    string `json:"scenario"`
	Role        string `json:"role"`
	Repeat      int    `json:"repeat"`
	Seed        int64  `json:"seed"`
	Unavailable string `json:"unavailable,omitempty"`
}

type lanePlan struct {
	Lane          string     `json:"lane"`
	HostClass     string     `json:"host_class"`
	Tasks         []laneTask `json:"tasks"`
	Prerequisites []string   `json:"unavailable_prerequisites"`
	Ready         bool       `json:"ready"`
	PolicySHA256  string     `json:"policy_sha256"`
}

type taskReceipt struct {
	laneTask
	Report   string    `json:"report"`
	Started  time.Time `json:"started"`
	Finished time.Time `json:"finished"`
	Error    string    `json:"error,omitempty"`
}

type laneReceipt struct {
	Version     int                `json:"schema_version"`
	Plan        lanePlan           `json:"plan"`
	Started     time.Time          `json:"started"`
	Finished    time.Time          `json:"finished"`
	WallSeconds float64            `json:"wall_seconds"`
	Tasks       []taskReceipt      `json:"tasks"`
	BudgetPass  bool               `json:"budget_pass"`
	Error       string             `json:"error,omitempty"`
	Workflow    workflowProvenance `json:"workflow"`
	Smoke       *smokeProvenance   `json:"smoke_provenance,omitempty"`
}

func readLaneConfig(path string) (laneConfig, error) {
	var c laneConfig
	data, err := os.ReadFile(path)
	if err != nil {
		return c, err
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(&c); err != nil {
		return c, err
	}
	if d.Decode(new(any)) != io.EOF {
		return c, errors.New("lane config must contain exactly one JSON document")
	}
	if c.Version != 1 || !slices.Contains([]string{"smoke", "nightly", "release"}, c.Lane) || c.Repeats < 3 || c.Repeats > 100 || c.Machine == "" {
		return c, errors.New("lane config needs schema1, smoke/nightly/release, 3–100 repeats and reviewed machine binding")
	}
	if !knownHostClass(c.HostClass) || c.HostClass == hostedSmokeClass && c.Lane != "smoke" {
		return c, errors.New("unsupported host class; floor envelope is not yet a budget-policy host and cannot be relabeled reference")
	}
	if len(c.Builds) != 3 {
		return c, errors.New("lane needs exactly candidate/main/tag builds")
	}
	for _, role := range []string{"candidate", "main", "tag"} {
		b := c.Builds[role]
		if b.Binary == "" || !revisionPattern.MatchString(b.Revision) || role == "tag" && b.Revision != shippedRevision {
			return c, fmt.Errorf("missing or unpinned %s build", role)
		}
	}
	for _, duration := range []string{c.Duration, c.Deadline, c.WarmTimeout, c.StopTimeout, c.SweepDuration} {
		if v, err := time.ParseDuration(duration); err != nil || v <= 0 {
			return c, errors.New("all workload/deadline/startup/stop/sweep durations must be explicit and positive")
		}
	}
	if stop, _ := time.ParseDuration(c.StopTimeout); stop > 2*time.Minute {
		return c, errors.New("gate stop_timeout must be at most two minutes, within the release cleanup reserve")
	}
	if c.SweepRepetitions < 1 || c.Load == "" || c.Manifest == "" {
		return c, errors.New("load binary, pinned manifest and explicit sweep repetition count required")
	}
	// Config-relative paths are deterministic; they never depend on the caller's CWD.
	base := filepath.Dir(pathAbsolute(path))
	resolve := func(value string) string {
		if filepath.IsAbs(value) {
			return value
		}
		return filepath.Join(base, value)
	}
	c.Load, c.Manifest = resolve(c.Load), resolve(c.Manifest)
	if c.MainArtifact != "" {
		c.MainArtifact = resolve(c.MainArtifact)
	}
	if c.Lane == "smoke" && c.MainArtifact == "" {
		return c, errors.New("smoke requires the verified exact-main artifact bundle")
	}
	for role, build := range c.Builds {
		build.Binary = resolve(build.Binary)
		c.Builds[role] = build
	}
	return c, nil
}

func pathAbsolute(path string) string { absolute, _ := filepath.Abs(path); return absolute }

func makeLanePlan(p policy, c laneConfig) (lanePlan, error) {
	plan := lanePlan{Lane: c.Lane, HostClass: c.HostClass, Ready: true, Prerequisites: []string{}, PolicySHA256: policyDigest(p)}
	scenarios := map[string]int{}
	needsTag := map[string]bool{}
	gaps := map[string]bool{}
	for _, b := range p.Budgets {
		if b.HostClass != c.HostClass || !slices.Contains(b.Lane, c.Lane) {
			continue
		}
		scenarios[b.Scenario] = max(scenarios[b.Scenario], c.Repeats, b.MinRepeats)
		needsTag[b.Scenario] = needsTag[b.Scenario] || b.Mode == "guard"
		if missing := metricSpecs[b.Metric].missing; missing != "" {
			gaps[b.Scenario+" / "+b.Metric+": "+missing] = true
		}
		if b.Metric == "edit_written_mib" {
			gaps[b.Scenario+" / edit_written_mib: shipped tag lacks patch-write counters; equivalent baseline strategy requires review"] = true
		}
		if b.Metric == "oom_kills" && c.HostClass != "reference_linux" && c.HostClass != hostedSmokeClass {
			gaps[b.Scenario+" / oom_kills: Mac report-v1 lacks an authoritative OOM-kill census; logs are not a substitute"] = true
		}
	}
	if len(scenarios) == 0 {
		return plan, errors.New("no budget rows for requested lane/host")
	}
	names := make([]string, 0, len(scenarios))
	for name := range scenarios {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		if c.Lane == "release" && name == "warm-restart" {
			scenarios[name] = max(scenarios[name], 10)
		}
		missing := scenarioSpecs[name].missing
		if missing != "" {
			gaps[name+": "+missing] = true
		}
		for repeat := 1; repeat <= scenarios[name]; repeat++ {
			// Rotate standalone role order to avoid always measuring candidate last.
			roles := []string{"main", "candidate"}
			if needsTag[name] {
				roles = append(roles, "tag")
			}
			offset := (repeat - 1) % len(roles)
			for n := range roles {
				plan.Tasks = append(plan.Tasks, laneTask{Scenario: name, Role: roles[(n+offset)%len(roles)], Repeat: repeat, Seed: int64(repeat), Unavailable: missing})
			}
		}
	}
	for gap := range gaps {
		plan.Prerequisites = append(plan.Prerequisites, gap)
	}
	slices.Sort(plan.Prerequisites)
	plan.Ready = len(plan.Prerequisites) == 0
	return plan, nil
}

func policyDigest(p policy) string {
	data, _ := json.Marshal(p)
	return fmt.Sprintf("%x", sha256.Sum256(data))
}

func laneCommand(command string, args []string, stdout, stderr io.Writer) int {
	f := flag.NewFlagSet(command, flag.ContinueOnError)
	f.SetOutput(stderr)
	config := f.String("config", "", "strict pinned lane config")
	budgets := f.String("budgets", "bench/budgets.yaml", "strict policy")
	out := f.String("out", "", "NEW JSON plan file or external collection directory")
	lane := f.String("lane", "", "plan-only lane without runtime config")
	host := f.String("host-class", "reference_linux", "plan-only host without runtime config")
	expected := f.String("expected-candidate", "", "require candidate revision to match this exact workflow commit")
	candidate := f.String("candidate-binary", "", "fresh workflow-built candidate binary; requires --expected-candidate")
	loadBinary := f.String("load-binary", "", "fresh workflow-built load client")
	requireLane := f.String("require-lane", "", "refuse config for any other lane")
	releaseStarted := f.String("release-started", "", "release entry RFC3339 start, including its compilation time")
	if err := f.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if f.NArg() != 0 || command == "collect" && (*config == "" || *out == "") || *config == "" && *lane == "" {
		fmt.Fprintln(stderr, "plan needs --config or --lane; collect needs --config and --out; no positional arguments")
		return 2
	}
	if *config == "" && (*candidate != "" || *loadBinary != "" || *expected != "") {
		fmt.Fprintln(stderr, "runtime overrides require a pinned configuration")
		return 2
	}
	c := laneConfig{Lane: *lane, HostClass: *host, Repeats: 3}
	var err error
	if *config != "" {
		c, err = readLaneConfig(*config)
	} else if !slices.Contains([]string{"smoke", "nightly", "release"}, *lane) {
		err = errors.New("unknown lane")
	}
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if *requireLane != "" && c.Lane != *requireLane {
		fmt.Fprintln(stderr, "configuration selects the wrong required lane")
		return 1
	}
	if *releaseStarted != "" {
		c.started, err = time.Parse(time.RFC3339Nano, *releaseStarted)
		if err != nil || c.Lane != "release" || c.started.After(time.Now()) {
			fmt.Fprintln(stderr, "release start must be a valid nonfuture timestamp in the release lane")
			return 2
		}
	}
	if *candidate != "" {
		if !revisionPattern.MatchString(*expected) {
			fmt.Fprintln(stderr, "candidate binary override requires full expected candidate revision")
			return 2
		}
		c.Builds["candidate"] = laneBuild{Binary: pathAbsolute(*candidate), Revision: *expected}
	}
	if *expected != "" && (!revisionPattern.MatchString(*expected) || c.Builds["candidate"].Revision != *expected) {
		fmt.Fprintln(stderr, "configured candidate revision does not match expected workflow commit")
		return 1
	}
	if *loadBinary != "" {
		c.Load = pathAbsolute(*loadBinary)
	}
	data, err := os.ReadFile(*budgets)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	p, err := loadPolicy(data)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	plan, err := makeLanePlan(p, c)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if command == "plan" {
		if *out != "" {
			if err := writeNewJSON(*out, plan); err != nil {
				fmt.Fprintln(stderr, err)
				return 1
			}
		}
		encoder := json.NewEncoder(stdout)
		encoder.SetIndent("", "  ")
		if err := encoder.Encode(plan); err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		if err := laneSummary("Performance readiness — NOT ACCEPTANCE", plan); err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		if !plan.Ready {
			fmt.Fprintln(stderr, "NOT ACCEPTANCE: unavailable formal observations remain hard prerequisites")
			return 1
		}
		return 0
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	err = collectLane(ctx, c, p, plan, *out, stdout, runLaneProcess)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	return 0
}

type laneProcess func(context.Context, string, []string, io.Writer) error

// Interrupt the load client so its signal handler performs owned-daemon cleanup.
// Do not kill the client on a short WaitDelay: that could strand its daemon.
func runLaneProcess(ctx context.Context, binary string, args []string, log io.Writer) error {
	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.Stdout, cmd.Stderr = log, log
	cmd.Cancel = func() error { return cmd.Process.Signal(os.Interrupt) }
	return cmd.Run()
}

func writeNewJSON(path string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	_, err = file.Write(append(data, '\n'))
	return errors.Join(err, file.Close())
}

func taskManifest(c laneConfig, task laneTask, root string) (string, error) {
	data, err := os.ReadFile(c.Manifest)
	if err != nil {
		return "", err
	}
	var corpora []map[string]any
	if err := json.Unmarshal(data, &corpora); err != nil {
		return "", err
	}
	spec := scenarioSpecs[task.Scenario]
	if len(corpora) < spec.repos {
		return "", fmt.Errorf("%s needs %d pinned corpora", task.Scenario, spec.repos)
	}
	if strings.HasPrefix(task.Scenario, "tools-single-") {
		label := strings.TrimPrefix(task.Scenario, "tools-single-")
		found := -1
		for i, corpus := range corpora {
			if corpus["label"] == label {
				found = i
				break
			}
		}
		if found < 0 {
			return "", fmt.Errorf("missing pinned corpus alias %s", label)
		}
		corpora = []map[string]any{corpora[found]}
	} else {
		corpora = corpora[:spec.repos]
	}
	// Source paths in the original manifest are defined by #996 as absolute paths.
	path := filepath.Join(root, "manifest.json")
	if err := writeNewJSON(path, corpora); err != nil {
		return "", err
	}
	return path, nil
}

func taskArgs(c laneConfig, task laneTask, manifest, scratch, report string) []string {
	spec := scenarioSpecs[task.Scenario]
	host := "native"
	if c.HostClass == "reference_linux" {
		host = "reference"
	}
	build := c.Builds[task.Role]
	return []string{"run", "--binary", build.Binary, "--git-sha", build.Revision, "--manifest", manifest, "--scratch", scratch, "--out", report, "--host", host, "--profile", "agent", "--scenario", spec.wire, "--agents", fmt.Sprint(spec.agents), "--edit-agents", fmt.Sprint(spec.editors), "--worktrees", fmt.Sprint(spec.worktrees), "--seed", fmt.Sprint(task.Seed), "--duration", c.Duration, "--deadline", c.Deadline, "--warm-timeout", c.WarmTimeout, "--stop-timeout", c.StopTimeout, "--sweep-repetitions", fmt.Sprint(c.SweepRepetitions), "--sweep-duration", c.SweepDuration}
}

func collectLane(ctx context.Context, c laneConfig, p policy, plan lanePlan, out string, stdout io.Writer, runProcess laneProcess) error {
	out, err := filepath.Abs(out)
	if err != nil {
		return err
	}
	if err := validateLaneOutput(c.Manifest, out); err != nil {
		return err
	}
	// The directory must not exist, including empty directories or symlinks.
	if err := os.Mkdir(out, 0o700); err != nil {
		return err
	}
	receipt := laneReceipt{Version: 1, Plan: plan, Started: time.Now().UTC(), Tasks: []taskReceipt{}}
	if !c.started.IsZero() {
		receipt.Started = c.started
	}
	receipt.Workflow = workflowProvenance{Event: os.Getenv("GITHUB_EVENT_NAME"), Ref: os.Getenv("GITHUB_REF"), Revision: c.Builds["candidate"].Revision}
	receipt.Workflow.Job = os.Getenv("GITHUB_JOB")
	receipt.Workflow.Attempt, _ = strconv.Atoi(os.Getenv("GITHUB_RUN_ATTEMPT"))
	if server, repo, id := os.Getenv("GITHUB_SERVER_URL"), os.Getenv("GITHUB_REPOSITORY"), os.Getenv("GITHUB_RUN_ID"); server != "" && repo != "" && id != "" {
		receipt.Workflow.RunURL = server + "/" + repo + "/actions/runs/" + id
	}
	if c.Lane == "smoke" {
		proof, err := prepareSmokeProvenance(c, receipt.Workflow)
		if err != nil {
			receipt.Error = err.Error()
			receipt.Finished = time.Now().UTC()
			return errors.Join(err, writeNewJSON(filepath.Join(out, "lane.json"), receipt))
		}
		receipt.Smoke = &proof
	}
	index := inputs{Version: 1, CandidateRevision: c.Builds["candidate"].Revision, MainRevision: c.Builds["main"].Revision, TagRevision: c.Builds["tag"].Revision, Repeats: c.Repeats, ScenarioRepeats: map[string]int{}, Hosts: map[string]string{c.HostClass: c.Machine}, Runs: []runInput{}}
	var failures []error
	// Release wall time includes all startup, teardown and budget checking, not
	// just offered load. Reserve three minutes for the harness's independent
	// <=2min owned-daemon stop, copied logs, state removal and budget receipts.
	// This interrupts the signal-aware load client; it never hard-kills it.
	if c.Lane == "release" {
		var cancel context.CancelFunc
		ctx, cancel = context.WithDeadline(ctx, receipt.Started.Add(8*time.Hour-3*time.Minute))
		defer cancel()
	}
	for _, task := range plan.Tasks {
		if task.Repeat > c.Repeats {
			index.ScenarioRepeats[task.Scenario] = max(index.ScenarioRepeats[task.Scenario], task.Repeat)
		}
		tr := taskReceipt{laneTask: task, Started: time.Now().UTC()}
		fmt.Fprintf(stdout, "collect %s / %d / %s\n", task.Scenario, task.Repeat, task.Role)
		if err := ctx.Err(); err != nil {
			failures = append(failures, err)
			break
		}
		if task.Unavailable != "" {
			tr.Error = task.Unavailable
			tr.Finished = time.Now().UTC()
			receipt.Tasks = append(receipt.Tasks, tr)
			continue
		}
		root := filepath.Join(out, fmt.Sprintf("%s-%03d-%s", task.Scenario, task.Repeat, task.Role))
		err := os.Mkdir(root, 0o700)
		var manifest string
		if err == nil {
			manifest, err = taskManifest(c, task, root)
		}
		reportPath := filepath.Join(root, "report.json")
		if err == nil {
			var log *os.File
			log, err = os.OpenFile(filepath.Join(root, "client.log"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
			if err == nil {
				err = runProcess(ctx, c.Load, taskArgs(c, task, manifest, filepath.Join(root, "state"), reportPath), log)
				err = errors.Join(err, log.Close())
			}
		}
		tr.Report, _ = filepath.Rel(out, reportPath)
		index.Runs = append(index.Runs, runInput{Role: task.Role, HostClass: c.HostClass, Scenario: task.Scenario, Repeat: task.Repeat, Report: tr.Report})
		if err != nil {
			tr.Error = err.Error()
			failures = append(failures, fmt.Errorf("%s/%d/%s: %w", task.Scenario, task.Repeat, task.Role, err))
		}
		tr.Finished = time.Now().UTC()
		receipt.Tasks = append(receipt.Tasks, tr)
	}
	if err := writeNewJSON(filepath.Join(out, "inputs.json"), index); err != nil {
		failures = append(failures, err)
	}
	// Even unavailable scenarios remain in the policy. Missing/failed runs cannot
	// become a smaller successful matrix, and the original checker is authoritative.
	d, err := loadInputs(filepath.Join(out, "inputs.json"))
	if err == nil {
		if c.Lane == "smoke" {
			receipt.Finished = time.Now().UTC()
			if provenanceErr := validateSmokeJob(receipt, d); provenanceErr != nil {
				failures = append(failures, provenanceErr)
			}
		}
		var rows []result
		rows, err = check(p, d, c.Lane)
		if err == nil {
			var table bytes.Buffer
			receipt.BudgetPass = printRows(io.MultiWriter(stdout, &table), rows)
			err = writeNewJSON(filepath.Join(out, "budget-results.json"), budgetEvidence{c.Lane, receipt.BudgetPass, rows})
			if err == nil {
				err = os.WriteFile(filepath.Join(out, "budget-table.txt"), table.Bytes(), 0o600)
			}
		}
	}
	if err != nil {
		failures = append(failures, err)
	}
	if !plan.Ready {
		failures = append(failures, errors.New("formal observations/fixtures unavailable; see plan.unavailable_prerequisites"))
	}
	if !receipt.BudgetPass {
		failures = append(failures, errors.New("performance budgets did not pass"))
	}
	receipt.Finished = time.Now().UTC()
	receipt.WallSeconds = receipt.Finished.Sub(receipt.Started).Seconds()
	if c.Lane == "release" && receipt.WallSeconds > 8*time.Hour.Seconds() {
		failures = append(failures, errors.New("release gate exceeded eight hours"))
	}
	err = errors.Join(failures...)
	if err != nil {
		receipt.Error = err.Error()
	}
	return errors.Join(err, writeNewJSON(filepath.Join(out, "lane.json"), receipt), laneSummary("Performance collection receipt", receipt))
}

func laneSummary(title string, value any) error {
	path := os.Getenv("GITHUB_STEP_SUMMARY")
	if path == "" {
		return nil
	}
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(file, "%s\n\n```json\n%s\n```\n", title, data)
	return errors.Join(err, file.Close())
}

// Check before writing even the first log/manifest. #996 checks this again
// before each owned daemon is launched, including source and scratch symlinks.
func validateLaneOutput(manifest, out string) error {
	data, err := os.ReadFile(manifest)
	if err != nil {
		return err
	}
	var corpora []struct {
		Source string `json:"source"`
	}
	if err := json.Unmarshal(data, &corpora); err != nil {
		return err
	}
	if len(corpora) == 0 {
		return errors.New("manifest is empty")
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(out))
	if err != nil {
		return err
	}
	canonical := filepath.Join(parent, filepath.Base(out))
	within := func(child, root string) bool {
		rel, err := filepath.Rel(root, child)
		return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
	}
	for _, corpus := range corpora {
		if !filepath.IsAbs(corpus.Source) {
			return errors.New("manifest source paths must be absolute")
		}
		source, err := filepath.EvalSymlinks(corpus.Source)
		if err != nil {
			return err
		}
		if within(canonical, source) || within(source, canonical) {
			return errors.New("evidence directory overlaps a read-only corpus")
		}
	}
	return nil
}
