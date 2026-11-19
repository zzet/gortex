package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"time"
)

type worktreeStartupOptions struct {
	Binary        string        `json:"binary"`
	Scratch       string        `json:"scratch"`
	Files         int           `json:"files"`
	Changes       int           `json:"changes"`
	Functions     int           `json:"functions"`
	Calls         int           `json:"calls"`
	Repeats       int           `json:"repeats"`
	PressureFiles int           `json:"pressure_files"`
	SeedWorktrees int           `json:"seed_worktrees"`
	Timeout       time.Duration `json:"timeout_ns"`
	Poll          time.Duration `json:"poll_interval_ns"`
	CPUProfile    time.Duration `json:"cpu_profile_ns"`
}

type worktreeStartupAttempt struct {
	StartedMS  float64         `json:"started_ms"`
	FinishedMS float64         `json:"finished_ms"`
	Response   json.RawMessage `json:"response,omitempty"`
	Error      string          `json:"error,omitempty"`
}

type worktreeStartupCase struct {
	Arm                    string                   `json:"arm"`
	Root                   string                   `json:"artifact_root"`
	CheckoutID             string                   `json:"checkout_id"`
	Outcome                string                   `json:"outcome"`
	Complete               bool                     `json:"complete"`
	Iteration              int                      `json:"iteration"`
	Created                time.Time                `json:"creation_started"`
	SwitchStarted          time.Time                `json:"switch_started"`
	SwitchFinished         time.Time                `json:"switch_finished"`
	ReadyMS                *float64                 `json:"first_correct_exact_ms"`
	SwitchToReadyMS        *float64                 `json:"switch_to_ready_ms"`
	Met30SecondSetupBudget *bool                    `json:"met_30s_setup_budget"`
	ObservedMS             float64                  `json:"observed_ms"`
	Attempts               []worktreeStartupAttempt `json:"attempts"`
	PublicationPhases      json.RawMessage          `json:"publication_phases,omitempty"`
	PhaseEvidence          string                   `json:"phase_evidence"`
	Error                  string                   `json:"error,omitempty"`
}

type worktreeStartupProbe struct {
	readyAt  time.Time
	checkout string
	attempts []worktreeStartupAttempt
	err      error
}

func worktreeStartupCommand(ctx context.Context, args []string) error {
	var o worktreeStartupOptions
	f := flag.NewFlagSet("worktree-startup", flag.ContinueOnError)
	f.StringVar(&o.Binary, "binary", "", "explicit daemon binary; always launches a private child")
	f.StringVar(&o.Scratch, "scratch", "", "artifact parent; a fresh owned run directory is allocated")
	f.IntVar(&o.Files, "files", 256, "synthetic Go files in the indexed primary")
	f.IntVar(&o.Changes, "changes", 26, "changed files in each committed target")
	f.IntVar(&o.Functions, "functions", 8, "functions per file")
	f.IntVar(&o.Calls, "calls", 4, "calls per function to the changed declaration")
	f.IntVar(&o.Repeats, "repeats", 1, "fresh daemon/store repetitions per arm")
	f.IntVar(&o.PressureFiles, "pressure-files", 0, "files in an additional unrelated indexed corpus (0 disables)")
	f.IntVar(&o.SeedWorktrees, "seed-worktrees", 1, "retained divergent worktrees built before measurement (0 allows primary-build overlap)")
	f.DurationVar(&o.Timeout, "timeout", 10*time.Minute, "per-case boot and readiness budgets")
	f.DurationVar(&o.Poll, "poll", 100*time.Millisecond, "interval between indexed readiness responses")
	f.DurationVar(&o.CPUProfile, "cpu-profile", 0, "fixed CPU capture starting before worktree creation; 0 disables all profiles")
	if err := f.Parse(args); err != nil {
		return err
	}
	if len(f.Args()) != 0 || o.Binary == "" || o.Scratch == "" || o.Files < 2 || o.Changes < 1 || o.Changes > o.Files || o.Functions < 1 || o.Calls < 1 || o.Repeats < 1 || o.PressureFiles < 0 || o.PressureFiles == 1 || o.SeedWorktrees < 0 || o.Timeout <= 0 || o.Poll <= 0 || o.CPUProfile < 0 || o.CPUProfile > o.Timeout || o.CPUProfile%time.Second != 0 {
		return errors.New("worktree-startup requires --binary, --scratch, files >= changes >= 1, files >= 2, positive functions/calls/repeats/timeout/poll, and 0 <= cpu-profile <= timeout; pressure-files is 0 or >= 2")
	}
	binary, err := filepath.Abs(o.Binary)
	if err != nil {
		return err
	}
	o.Binary, err = filepath.EvalSymlinks(binary)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(o.Scratch, 0o700); err != nil {
		return err
	}
	root, err := os.MkdirTemp(o.Scratch, "startup-bench-")
	if err != nil {
		return err
	}
	root, err = filepath.Abs(root)
	if err != nil {
		return err
	}
	fixture, err := newWorktreeStartupFixture(ctx, filepath.Join(root, "fixture"), o.Files, o.Changes, o.Functions, o.Calls)
	if err != nil {
		return err
	}
	corpora := []Corpus{fixture.Corpus}
	if o.PressureFiles > 0 {
		pressure, err := newWorktreeStartupFixture(ctx, filepath.Join(root, "pressure-fixture"), o.PressureFiles, 1, o.Functions, o.Calls)
		if err != nil {
			return err
		}
		pressure.Corpus.Label = "pressure"
		corpora = append(corpora, pressure.Corpus)
	}
	sha, err := discoveryFileSHA(o.Binary)
	if err != nil {
		return err
	}
	report := struct {
		SchemaVersion int                    `json:"schema_version"`
		Benchmark     string                 `json:"benchmark"`
		Options       worktreeStartupOptions `json:"options"`
		BinarySHA256  string                 `json:"binary_sha256"`
		OS            string                 `json:"os"`
		Arch          string                 `json:"arch"`
		CPUs          int                    `json:"cpus"`
		Fixture       map[string]any         `json:"fixture"`
		Notes         []string               `json:"notes"`
		Cases         []worktreeStartupCase  `json:"cases"`
	}{1, "worktree-startup", o, sha, runtime.GOOS, runtime.GOARCH, runtime.NumCPU(),
		map[string]any{"base": fixture.Corpus.Commit, "initial": fixture.Initial, "final": fixture.Final, "seed": fixture.Seed},
		[]string{"Readiness is one public control search response with an exact worktree rider, current marker and no predecessor markers; it is not MCP/hook readiness or complete relations.", "Each arm/repetition uses a new private daemon/store; boot is outside the measured worktree window. Poll latency is included in observed readiness.", "HEAD-race validation correlates daemon monotonic phase offsets through OriginWall with the harness Git command's wall times; missed or missing evidence fails the arm.", "CPU profiles use a fixed window and can include an idle tail after readiness. Block/mutex before/after profiles include warmup and can be differenced; goroutine profiles are snapshots.", "No missing phase is represented as measured zero. Raw status, every probe and daemon logs are retained. No physical store-size or latency SLO is inferred from a small fixture."}, nil}
	var failures []error
	for iteration := 0; iteration < o.Repeats && ctx.Err() == nil; iteration++ {
		for _, arm := range []string{"equal_head", "divergent_head", "head_switch"} {
			fmt.Fprintf(os.Stderr, "startup benchmark: %s repetition %d; artifacts %s\n", arm, iteration+1, root)
			row, runErr := runWorktreeStartupCase(ctx, o, root, fixture, corpora, arm, iteration)
			if runErr != nil {
				row.Error = runErr.Error()
				failures = append(failures, fmt.Errorf("%s/%d: %w", arm, iteration+1, runErr))
			}
			report.Cases = append(report.Cases, row)
			if err := writeStartupJSON(filepath.Join(root, "report.json"), report); err != nil {
				return err
			}
		}
	}
	failures = append(failures, ctx.Err())
	fmt.Fprintf(os.Stderr, "startup benchmark report: %s\n", filepath.Join(root, "report.json"))
	return errors.Join(errors.Join(failures...), json.NewEncoder(os.Stdout).Encode(report))
}

func runWorktreeStartupCase(parent context.Context, o worktreeStartupOptions, scratch string, fixture worktreeStartupFixture, corpora []Corpus, arm string, iteration int) (row worktreeStartupCase, resultErr error) {
	row.Arm, row.Iteration, row.Outcome = arm, iteration+1, "failed"
	row.PhaseEvidence = "unavailable: no final status observation"
	bootCtx, bootCancel := context.WithTimeout(parent, o.Timeout)
	defer bootCancel()
	p, err := newPrivateRun(bootCtx, scratch, o.Binary, corpora)
	if err != nil {
		return row, err
	}
	row.Root = p.Root
	defer func() {
		stopCtx, cancel := context.WithTimeout(context.WithoutCancel(parent), 30*time.Second)
		defer cancel()
		_, forced, stopErr := p.stop(stopCtx)
		if forced {
			stopErr = errors.Join(stopErr, errors.New("private daemon required forced shutdown"))
		}
		resultErr = errors.Join(resultErr, stopErr, os.RemoveAll(p.SocketDir))
		row.Complete = resultErr == nil && row.Outcome == "ready"
	}()
	if o.CPUProfile > 0 {
		p.daemonEnv = append(append([]string(nil), p.Env...), "GORTEX_DAEMON_BLOCK_PROFILE_RATE=1000000", "GORTEX_DAEMON_MUTEX_PROFILE_FRACTION=1")
	}
	configured, err := p.writeBootRepos(false)
	if err != nil {
		return row, err
	}
	started := time.Now()
	if err := p.start(); err != nil {
		return row, err
	}
	if _, _, err := p.awaitReady(bootCtx, started, configured); err != nil {
		return row, err
	}
	primary := p.Repos[0]
	boot := probeWorktreeStartup(bootCtx, p, primary.Path, "StartupBaseMarker", false, o.Poll, started)
	if boot.err != nil {
		return row, fmt.Errorf("primary marker: %w", boot.err)
	}
	for seed := range o.SeedWorktrees {
		path := filepath.Join(p.Root, "repos", fmt.Sprintf("seed-%03d", seed))
		if _, err := p.git(bootCtx, primary.Path, "worktree", "add", "--detach", path, fixture.Seed); err != nil {
			return row, err
		}
		seeded := probeWorktreeStartup(bootCtx, p, path, "StartupSeedMarker", true, o.Poll, time.Now())
		if seeded.err != nil {
			return row, fmt.Errorf("seed worktree %d: %w", seed, seeded.err)
		}
	}
	status, err := control(bootCtx, p.Socket, "status", nil)
	if err != nil {
		return row, err
	}
	if err := os.WriteFile(filepath.Join(p.Root, "before.status.json"), status, 0o600); err != nil {
		return row, err
	}
	if o.CPUProfile > 0 {
		if err := startupDiagnosticProfiles(bootCtx, status, p.Root, "before"); err != nil {
			return row, err
		}
	}
	// The foreground child writes structured application logs to its captured
	// stderr; GORTEX_DAEMON_LOGFILE can remain an empty lifecycle log.
	buildLog := p.stderr.Name()
	logStart := int64(0)
	if info, err := os.Stat(buildLog); err == nil {
		logStart = info.Size()
	}
	ctx, cancel := context.WithTimeout(parent, o.Timeout)
	defer cancel()
	var profileDone chan error
	if o.CPUProfile > 0 {
		profileDone = make(chan error, 1)
		go func() {
			profileDone <- captureCPUProfile(ctx, status, o.CPUProfile, filepath.Join(p.Root, "startup.cpu.pprof"))
		}()
		defer func() { resultErr = errors.Join(resultErr, <-profileDone) }()
	}
	row.Created = time.Now()
	defer func() { row.ObservedMS = float64(time.Since(row.Created)) / float64(time.Millisecond) }()
	linked := filepath.Join(p.Root, "repos", "linked")
	target, expected := fixture.Corpus.Commit, "StartupBaseMarker"
	if arm != "equal_head" {
		target, expected = fixture.Initial, "StartupInitialMarker"
	}
	if arm == "head_switch" {
		expected = "StartupFinalMarker"
	}
	if _, err := p.git(ctx, primary.Path, "worktree", "add", "--detach", linked, target); err != nil {
		return row, err
	}
	probeDone := make(chan worktreeStartupProbe, 1)
	go func() { probeDone <- probeWorktreeStartup(ctx, p, linked, expected, true, o.Poll, row.Created) }()
	if arm == "head_switch" {
		if err := awaitStartupBuild(ctx, buildLog, logStart); err != nil {
			cancel()
			probe := <-probeDone
			row.Attempts = probe.attempts
			return row, err
		}
		row.SwitchStarted = time.Now()
		_, err = p.git(ctx, linked, "checkout", "--quiet", "--detach", fixture.Final)
		row.SwitchFinished = time.Now()
		if err != nil {
			cancel()
		}
	}
	probe := <-probeDone
	row.Attempts, row.CheckoutID = probe.attempts, probe.checkout
	resultErr = errors.Join(err, probe.err)
	if resultErr == nil {
		ms := float64(probe.readyAt.Sub(row.Created)) / float64(time.Millisecond)
		row.ReadyMS = &ms
		within := ms <= 30000
		row.Met30SecondSetupBudget, row.Outcome = &within, "ready"
		if arm == "head_switch" {
			delta := float64(probe.readyAt.Sub(row.SwitchFinished)) / float64(time.Millisecond)
			row.SwitchToReadyMS = &delta
		}
	} else if errors.Is(probe.err, context.DeadlineExceeded) {
		row.Outcome = "timeout"
	}
	// Final evidence is bounded separately so a timed-out arm still retains it.
	evidenceCtx, evidenceCancel := context.WithTimeout(context.WithoutCancel(parent), 5*time.Second)
	defer evidenceCancel()
	after, statusErr := control(evidenceCtx, p.Socket, "status", nil)
	if statusErr == nil {
		statusErr = os.WriteFile(filepath.Join(p.Root, "after.status.json"), after, 0o600)
		attachStartupPhases(&row, after)
	}
	if arm == "head_switch" && resultErr == nil {
		if err := worktreeStartupRace(after, row.CheckoutID, row.Created, row.SwitchStarted, row.SwitchFinished); err != nil {
			row.Outcome = "missed_race"
			resultErr = err
		}
	}
	if resultErr == nil {
		raw, err := control(evidenceCtx, p.Socket, "search_symbols", map[string]any{"path": primary.Path, "query": "Startup", "limit": 20})
		ready, _, decodeErr := worktreeStartupReady(raw, "StartupBaseMarker", false)
		if err != nil || decodeErr != nil || !ready {
			resultErr = errors.Join(err, decodeErr, errors.New("primary marker isolation was not preserved"))
		}
	}
	if o.CPUProfile > 0 && statusErr == nil {
		statusErr = startupDiagnosticProfiles(evidenceCtx, after, p.Root, "after")
	}
	return row, errors.Join(resultErr, statusErr)
}

func probeWorktreeStartup(ctx context.Context, p *privateRun, path, expected string, linked bool, interval time.Duration, origin time.Time) (result worktreeStartupProbe) {
	for {
		started := time.Now()
		callCtx, cancel := context.WithTimeout(ctx, time.Second)
		raw, err := control(callCtx, p.Socket, "search_symbols", map[string]any{"path": path, "query": "Startup", "limit": 20})
		cancel()
		finished := time.Now()
		attempt := worktreeStartupAttempt{StartedMS: float64(started.Sub(origin)) / float64(time.Millisecond), FinishedMS: float64(finished.Sub(origin)) / float64(time.Millisecond), Response: raw}
		ready, checkout, decodeErr := false, "", error(nil)
		if err == nil {
			ready, checkout, decodeErr = worktreeStartupReady(raw, expected, linked)
		}
		if err = errors.Join(err, decodeErr); err != nil {
			attempt.Error = err.Error()
		}
		result.attempts = append(result.attempts, attempt)
		if checkout != "" {
			if result.checkout != "" && result.checkout != checkout {
				result.err = fmt.Errorf("selected checkout identity changed from %q to %q", result.checkout, checkout)
				result.checkout = ""
				return result
			}
			result.checkout = checkout
		}
		if ready && ctx.Err() == nil {
			result.readyAt = finished
			return result
		}
		if err := waitContext(ctx, interval); err != nil {
			result.err = err
			return result
		}
	}
}

func awaitStartupBuild(ctx context.Context, path string, offset int64) error {
	for {
		found, err := startupBuildInLog(path, offset)
		if err != nil || found {
			return err
		}
		if err := waitContext(ctx, 10*time.Millisecond); err != nil {
			return fmt.Errorf("initial commit build was not observed: %w", err)
		}
	}
}

func startupBuildInLog(path string, offset int64) (bool, error) {
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer file.Close()
	if _, err := file.Seek(offset, io.SeekStart); err != nil {
		return false, err
	}
	scan := bufio.NewScanner(file)
	scan.Buffer(make([]byte, 4096), 4<<20)
	for scan.Scan() {
		var event struct {
			Message string `json:"msg"`
			Origin  string `json:"origin"`
		}
		if json.Unmarshal(scan.Bytes(), &event) == nil && event.Message == "indexer: full index started" && event.Origin == "checkout_coordinator" {
			return true, nil
		}
	}
	return false, scan.Err()
}

func startupDiagnosticProfiles(ctx context.Context, status json.RawMessage, root, prefix string) error {
	var failures []error
	for _, name := range []string{"goroutine", "block", "mutex"} {
		failures = append(failures, capturePProf(ctx, status, "/debug/pprof/"+name, 5*time.Second, filepath.Join(root, prefix+"."+name+".pprof")))
	}
	return errors.Join(failures...)
}

func writeStartupJSON(path string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o600)
}
