package main

import (
	"bytes"
	"context"
	"debug/buildinfo"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Existing pin in .github/workflows/bench-arm.yml; do not install @latest or
// introduce x/perf as a production module dependency for this command.
const benchstatVersion = "v0.0.0-20260409210113-8e83ce0f7b1c"

var microPackages = []string{"internal/parser/languages", "internal/query", "internal/graph"}

type microPair struct {
	Main      string `json:"main"`
	Candidate string `json:"candidate"`
}
type microConfig struct {
	Version           int                  `json:"schema_version"`
	MainRevision      string               `json:"main_revision"`
	CandidateRevision string               `json:"candidate_revision"`
	Fixture           string               `json:"fixture"`
	FixtureRevision   string               `json:"fixture_revision"`
	Benchstat         string               `json:"benchstat_binary"`
	Benchtime         string               `json:"benchtime"`
	Timeout           string               `json:"timeout"`
	Repeats           int                  `json:"repeats"`
	Binaries          map[string]microPair `json:"binaries"`
}
type microRun struct {
	Package             string    `json:"package"`
	Role                string    `json:"role"`
	Repeat              int       `json:"repeat"`
	Started             time.Time `json:"started"`
	Finished            time.Time `json:"finished"`
	Error               string    `json:"error,omitempty"`
	AbnormalTermination bool      `json:"abnormal_termination"`
	DeadlineExceeded    bool      `json:"deadline_exceeded"`
}
type microReceipt struct {
	Version          int                                `json:"schema_version"`
	Workflow         workflowProvenance                 `json:"workflow"`
	Machine          string                             `json:"machine"`
	OS               string                             `json:"os"`
	Arch             string                             `json:"arch"`
	GOMAXPROCS       int                                `json:"gomaxprocs"`
	Config           microConfig                        `json:"config"`
	Builds           map[string]map[string]mainArtifact `json:"builds"`
	Runs             []microRun                         `json:"runs"`
	Complete         bool                               `json:"complete"`
	FormalAcceptance bool                               `json:"formal_acceptance"`
	Error            string                             `json:"error,omitempty"`
}

type microProcess func(context.Context, string, []string, string, []string) ([]byte, []byte, error)

func executeMicroProcess(ctx context.Context, binary string, args []string, cwd string, env []string) ([]byte, []byte, error) {
	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.Dir = cwd
	cmd.Env = env
	cmd.Cancel = func() error { return cmd.Process.Signal(os.Interrupt) }
	cmd.WaitDelay = 2 * time.Second
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := errors.Join(cmd.Run(), ctx.Err())
	return stdout.Bytes(), stderr.Bytes(), err
}

func validateMicroConfig(c microConfig) error {
	if c.Version != 1 || !revisionPattern.MatchString(c.MainRevision) || !revisionPattern.MatchString(c.CandidateRevision) || !revisionPattern.MatchString(c.FixtureRevision) || c.Fixture == "" || c.Benchstat == "" || c.Repeats < 3 || c.Repeats > 20 || len(c.Binaries) != len(microPackages) {
		return errors.New("microbench requires pinned builds/fixture, explicit tool/profile, >=3 repeats and exactly the existing three CI packages")
	}
	if duration, err := time.ParseDuration(c.Timeout); err != nil || duration <= 0 {
		return errors.New("microbenchmark timeout must be explicit and positive")
	}
	if strings.HasSuffix(c.Benchtime, "x") {
		n, err := strconv.Atoi(strings.TrimSuffix(c.Benchtime, "x"))
		if err != nil || n < 1 {
			return errors.New("invalid explicit iteration benchtime")
		}
	} else if duration, err := time.ParseDuration(c.Benchtime); err != nil || duration <= 0 {
		return errors.New("benchtime must be explicit positive duration or iteration count")
	}
	for _, pkg := range microPackages {
		pair := c.Binaries[pkg]
		if pair.Main == "" || pair.Candidate == "" {
			return errors.New("missing main/candidate benchmark binary")
		}
	}
	return nil
}

func checkBenchstat(path string) error {
	info, err := buildinfo.ReadFile(path)
	if err != nil {
		return err
	}
	if info.Path != "golang.org/x/perf/cmd/benchstat" || info.Main.Path != "golang.org/x/perf" || info.Main.Version != benchstatVersion || info.GoVersion != runtime.Version() {
		return errors.New("benchstat executable must carry the existing repository pin and pinned Go build metadata")
	}
	return nil
}

func microCensus(data []byte) ([]string, error) {
	var keys []string
	for line := range strings.SplitSeq(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 || !strings.HasPrefix(fields[0], "Benchmark") {
			continue
		}
		if len(fields) < 4 || len(fields)%2 != 0 {
			return nil, errors.New("malformed/incomplete benchmark row")
		}
		count, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil || count < 1 {
			return nil, errors.New("benchmark has no measured iterations")
		}
		for i := 2; i < len(fields); i += 2 {
			value, err := strconv.ParseFloat(fields[i], 64)
			if err != nil || !finiteNonnegative(value) {
				return nil, errors.New("benchmark has invalid numeric observation")
			}
			keys = append(keys, fields[0]+" / "+fields[i+1])
		}
	}
	if len(keys) == 0 {
		return nil, errors.New("no benchmark samples retained")
	}
	slices.Sort(keys)
	for i := 1; i < len(keys); i++ {
		if keys[i] == keys[i-1] {
			return nil, errors.New("duplicate benchmark/unit sample in one repeat")
		}
	}
	return keys, nil
}

func validateMicroFixture(c microConfig, out string) error {
	fixture, err := filepath.EvalSymlinks(c.Fixture)
	if err != nil {
		return err
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(out))
	if err != nil {
		return err
	}
	rel, err := filepath.Rel(fixture, filepath.Join(parent, filepath.Base(out)))
	if err != nil {
		return err
	}
	if rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return errors.New("microbenchmark evidence must be outside the pinned read-only fixture")
	}
	head, err := exec.Command("git", "-C", fixture, "rev-parse", "HEAD").Output()
	if err != nil || strings.TrimSpace(string(head)) != c.FixtureRevision {
		return errors.New("microbenchmark fixture is not at its pinned commit")
	}
	dirty, err := exec.Command("git", "-C", fixture, "status", "--porcelain", "--untracked-files=normal").Output()
	if err != nil || len(dirty) != 0 {
		return errors.New("microbenchmark fixture must be clean")
	}
	for _, pkg := range microPackages {
		cwd, err := filepath.EvalSymlinks(filepath.Join(fixture, pkg))
		if err != nil || cwd != filepath.Join(fixture, pkg) {
			return errors.New("benchmark package cwd is missing or escapes the pinned fixture")
		}
	}
	return nil
}

func collectMicrobench(ctx context.Context, c microConfig, out string, workflow workflowProvenance, builds map[string]map[string]mainArtifact, process microProcess) error {
	if err := os.Mkdir(out, 0o700); err != nil {
		return err
	}
	machine, err := os.Hostname()
	if err != nil {
		return err
	}
	receipt := microReceipt{Version: 1, Workflow: workflow, Machine: machine, OS: runtime.GOOS, Arch: runtime.GOARCH, GOMAXPROCS: runtime.GOMAXPROCS(0), Config: c, Builds: builds, Runs: []microRun{}, FormalAcceptance: false}
	var failures []error
	combined := map[string][]byte{"main": {}, "candidate": {}}
	census := map[string][]string{}
	for repeat := 1; repeat <= c.Repeats; repeat++ {
		for _, pkg := range microPackages {
			roles := []string{"main", "candidate"}
			if repeat%2 == 0 {
				slices.Reverse(roles)
			}
			for _, role := range roles {
				run := microRun{Package: pkg, Role: role, Repeat: repeat, Started: time.Now().UTC()}
				root := filepath.Join(out, fmt.Sprintf("%s-%03d-%s", strings.ReplaceAll(pkg, "/", "-"), repeat, role))
				err := os.Mkdir(root, 0o700)
				var stdout, stderr []byte
				if err == nil {
					duration, _ := time.ParseDuration(c.Timeout)
					runCtx, cancel := context.WithTimeout(ctx, duration)
					pair := c.Binaries[pkg]
					binary := pair.Main
					if role == "candidate" {
						binary = pair.Candidate
					}
					env := append(os.Environ(), "TMPDIR="+root, "TMP="+root, "TEMP="+root)
					stdout, stderr, err = process(runCtx, binary, []string{"-test.run=^$", "-test.bench=.", "-test.benchmem", "-test.count=1", "-test.benchtime=" + c.Benchtime, "-test.timeout=" + c.Timeout}, filepath.Join(c.Fixture, pkg), env)
					run.AbnormalTermination = err != nil
					run.DeadlineExceeded = errors.Is(err, context.DeadlineExceeded)
					cancel()
					err = errors.Join(err, os.WriteFile(filepath.Join(root, "stdout.txt"), stdout, 0o600), os.WriteFile(filepath.Join(root, "stderr.txt"), stderr, 0o600))
				}
				if err == nil {
					var keys []string
					keys, err = microCensus(stdout)
					if err == nil {
						if expected, exists := census[pkg]; exists && !slices.Equal(expected, keys) {
							err = errors.New("candidate/main/repeat benchmark and unit census differs")
						} else {
							census[pkg] = keys
						}
					}
				}
				if err != nil {
					run.Error = err.Error()
					failures = append(failures, fmt.Errorf("%s/%d/%s: %w", pkg, repeat, role, err))
				}
				combined[role] = append(combined[role], []byte("pkg: github.com/zzet/gortex/"+pkg+"\n")...)
				combined[role] = append(combined[role], stdout...)
				combined[role] = append(combined[role], '\n')
				run.Finished = time.Now().UTC()
				receipt.Runs = append(receipt.Runs, run)
				if ctx.Err() != nil {
					failures = append(failures, ctx.Err())
					break
				}
			}
			if ctx.Err() != nil {
				break
			}
		}
		if ctx.Err() != nil {
			break
		}
	}
	for role, data := range combined {
		if err := os.WriteFile(filepath.Join(out, role+".txt"), data, 0o600); err != nil {
			failures = append(failures, err)
		}
	}
	if len(failures) == 0 {
		if err := validateMicroFixture(c, out); err != nil {
			failures = append(failures, err)
		}
	}
	if len(failures) == 0 {
		duration, _ := time.ParseDuration(c.Timeout)
		statCtx, cancel := context.WithTimeout(ctx, duration)
		stdout, stderr, err := process(statCtx, c.Benchstat, []string{filepath.Join(out, "main.txt"), filepath.Join(out, "candidate.txt")}, out, os.Environ())
		cancel()
		err = errors.Join(err, os.WriteFile(filepath.Join(out, "benchstat.txt"), stdout, 0o600), os.WriteFile(filepath.Join(out, "benchstat.stderr.txt"), stderr, 0o600))
		if err != nil {
			failures = append(failures, err)
		}
	}
	err = errors.Join(failures...)
	receipt.Complete = err == nil
	if err != nil {
		receipt.Error = err.Error()
	}
	return errors.Join(err, writeNewJSON(filepath.Join(out, "microbench.json"), receipt))
}

func microbenchCommand(args []string, stdout, stderr io.Writer) int {
	f := flag.NewFlagSet("microbench", flag.ContinueOnError)
	f.SetOutput(stderr)
	input := f.String("inputs", "", "strict pinned microbenchmark config")
	out := f.String("out", "", "NEW external evidence directory")
	if err := f.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if *input == "" || *out == "" || f.NArg() != 0 {
		fmt.Fprintln(stderr, "--inputs/--out required")
		return 2
	}
	var c microConfig
	err := readStrictJSON(*input, &c)
	if err == nil {
		err = validateMicroConfig(c)
	}
	base := filepath.Dir(pathAbsolute(*input))
	resolve := func(path string) string {
		if filepath.IsAbs(path) {
			return path
		}
		return filepath.Join(base, path)
	}
	c.Fixture, c.Benchstat = resolve(c.Fixture), resolve(c.Benchstat)
	for pkg, pair := range c.Binaries {
		pair.Main, pair.Candidate = resolve(pair.Main), resolve(pair.Candidate)
		c.Binaries[pkg] = pair
	}
	output := pathAbsolute(*out)
	if err == nil {
		err = validateMicroFixture(c, output)
	}
	if err == nil {
		err = checkBenchstat(c.Benchstat)
	}
	workflow := workflowProvenance{Event: os.Getenv("GITHUB_EVENT_NAME"), Ref: os.Getenv("GITHUB_REF"), Revision: c.CandidateRevision, RunURL: os.Getenv("GITHUB_SERVER_URL") + "/" + os.Getenv("GITHUB_REPOSITORY") + "/actions/runs/" + os.Getenv("GITHUB_RUN_ID"), Job: os.Getenv("GITHUB_JOB")}
	workflow.Attempt, _ = strconv.Atoi(os.Getenv("GITHUB_RUN_ATTEMPT"))
	if err == nil {
		err = validateSmokeWorkflow(workflow)
	}
	if err == nil && (os.Getenv("GITHUB_ACTIONS") != "true" || os.Getenv("GITHUB_SHA") != c.CandidateRevision) {
		err = errors.New("microbench requires the current owned PR job identity")
	}
	builds := map[string]map[string]mainArtifact{}
	if err == nil {
		for _, pkg := range microPackages {
			pair := c.Binaries[pkg]
			main, mainErr := inspectBinaryArtifact(pair.Main, "github.com/zzet/gortex/"+pkg+".test")
			candidate, candidateErr := inspectBinaryArtifact(pair.Candidate, "github.com/zzet/gortex/"+pkg+".test")
			if mainErr != nil || candidateErr != nil {
				err = errors.Join(mainErr, candidateErr)
				break
			}
			if main.Revision != c.MainRevision || candidate.Revision != c.CandidateRevision || !sameBuildSettings(main, candidate) || main.OS != runtime.GOOS || main.Arch != runtime.GOARCH {
				err = errors.New("microbenchmark binaries lack matching pinned revisions/build/platform identities")
				break
			}
			builds[pkg] = map[string]mainArtifact{"main": main, "candidate": candidate}
		}
	}
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err := collectMicrobench(ctx, c, output, workflow, builds, executeMicroProcess); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	fmt.Fprintln(stdout, "microbenchmark/benchstat evidence retained; this is not formal workload acceptance:", output)
	return 0
}
