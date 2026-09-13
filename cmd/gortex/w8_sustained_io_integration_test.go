package main

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// The sustained-workload I/O harness.
//
// issue767_idle_io_integration_test.go measures an idle daemon; this measures a
// working one. It drives nine phases over a generated corpus — cold index, cold
// idle, small edits, touch/stage/unstage, a same-tree amend, main advancement
// under ten discovered dependent worktrees, dependent edits, an explicit
// track/untrack round trip, and warm idle — and records, per phase, the process
// write series, the store/WAL series, WAL resets, the store census, the daemon's
// own viewmetrics counters and a destination census of every byte under the
// private root.
//
// It is opt-in (GXW8_TEST_BINARY) and never runs in a default `go test ./...`.
// The instrument itself — generator, sampler, checkpoint counter, manifest,
// attribution, phase plan, timeout arithmetic — is unit-tested in
// w8_fixture_generator_test.go, w8_sampler_test.go and at the bottom of this
// file, so the parts that can be wrong without a daemon are checked without one.
//
// Caveats that travel with every number this produces:
//   - GORTEX_RECONCILE_INTERVAL=5s accelerates the janitor (the default is 1h).
//     These are not default-configuration numbers.
//   - ri_logical_writes is the primary series; ri_diskio_byteswritten is
//     reported next to it and never alone (it has been observed as zero while
//     logical writes were hundreds of kilobytes).
//   - Process-accounted writes are not SSD NAND writes, and a current WAL size
//     is not cumulative writes.
//   - WAL resets are a lower bound on checkpoints: a PASSIVE checkpoint that
//     does not restart the log is invisible from outside the process.
//   - This item validates the instrument. It states no performance verdict;
//     budgets are frozen from a baseline arm by a later item.

const (
	w8AdvanceMarkerFormat  = "W8Advance%03dMarker"
	w8DependentMarkerForm  = "W8Dependent%02dMarker"
	w8DefaultProbeTimeout  = 3 * time.Minute
	w8PhaseCommandTimeout  = 30 * time.Second
	w8StatusScrapeTimeout  = 30 * time.Second
	w8MinimumFixtureFiles  = 10
	w8MaximumFixtureFiles  = 20000
	w8CommitFilesPerCommit = 3
)

// errW8NoChild is the "there is no daemon process to read" case, which is not
// a measurement failure: a phase that starts the child has no earlier process
// to take a baseline from, and the child's own counters start at zero.
var errW8NoChild = errors.New("no running child")

// w8ProcessWindow is one phase's process-accounted cost.
type w8ProcessWindow struct {
	OK            bool
	Logical       *uint64
	DiskWritten   uint64
	DiskRead      uint64
	CPUUserNS     uint64
	CPUSystemNS   uint64
	PhysFootprint uint64
	Notes         []string
}

// w8ProcessDelta turns a phase's two rusage reads into a delta, and says in
// words why it could not when it could not.
//
// The one case that looks like a failure but is not: the phase that starts the
// daemon has no process to read at its start. The child's counters begin at
// zero, so the end read IS the delta — that is how the cold-index phase gets a
// write series at all instead of an "unavailable".
func w8ProcessDelta(before issue767ProcessIO, beforeErr error, after issue767ProcessIO, afterErr error) w8ProcessWindow {
	window := w8ProcessWindow{}
	if afterErr != nil {
		window.Notes = append(window.Notes, "process I/O unavailable at phase end: "+afterErr.Error())
		return window
	}
	lifetime := false
	if beforeErr != nil {
		if !errors.Is(beforeErr, errW8NoChild) {
			window.Notes = append(window.Notes, "process I/O unavailable at phase start: "+beforeErr.Error())
			return window
		}
		lifetime = true
		before = issue767ProcessIO{StartTicks: after.StartTicks}
		window.Notes = append(window.Notes, "no child was running at phase start; deltas are this child's lifetime totals")
	}
	if !lifetime && before.StartTicks != after.StartTicks {
		window.Notes = append(window.Notes, "the child restarted during this phase; process deltas are not comparable")
		return window
	}
	window.OK = true
	window.DiskWritten = w8Delta(before.BytesWritten, after.BytesWritten)
	window.DiskRead = w8Delta(before.BytesRead, after.BytesRead)
	window.CPUUserNS = w8Delta(before.UserTimeNS, after.UserTimeNS)
	window.CPUSystemNS = w8Delta(before.SystemTimeNS, after.SystemTimeNS)
	window.PhysFootprint = after.PhysFootprint
	if after.LogicalBytesWritten != nil {
		start := uint64(0)
		if before.LogicalBytesWritten != nil {
			start = *before.LogicalBytesWritten
		} else if !lifetime {
			return window
		}
		delta := w8Delta(start, *after.LogicalBytesWritten)
		window.Logical = &delta
	}
	return window
}

// w8Diagnostics is what the harness collects when a phase fails. A bare
// "timed out waiting for symbol X" tells the next reader nothing about why the
// view never became exact; the daemon's own status, the family census and the
// per-path route explanation do.
type w8Diagnostics struct {
	Phase    string            `json:"phase"`
	Commands map[string]string `json:"commands"`
	LogTail  string            `json:"daemon_log_tail,omitempty"`
}

// w8CollectDiagnostics runs the explain-the-view command set. run must never
// fail the test: this executes while a failure is already unwinding.
func w8CollectDiagnostics(phase string, run func(args ...string) string, logTail string, paths []string) w8Diagnostics {
	diagnostics := w8Diagnostics{Phase: phase, Commands: map[string]string{}, LogTail: logTail}
	// The checkout verbs live under `repos` (checkouts_cmd.go registers them
	// on reposCmd), and explain-view answers "which graph serves this path,
	// and why" — the exact question a timed-out exactness wait raises.
	argvs := [][]string{
		{"daemon", "status", "--format", "json", "--no-progress"},
		{"repos", "families", "--format", "json", "--no-progress"},
	}
	for _, path := range paths {
		argvs = append(argvs, []string{"repos", "explain-view", path, "--index", path, "--format", "json", "--no-progress"})
	}
	for _, argv := range argvs {
		diagnostics.Commands[strings.Join(argv, " ")] = run(argv...)
	}
	return diagnostics
}

// w8Config is every knob the workload takes.
type w8Config struct {
	Fixture        w8FixtureSpec
	Worktrees      int
	Commits        int
	Edits          int
	EditInterval   time.Duration
	Idle           time.Duration
	SampleInterval time.Duration
	Repetitions    int
	IdleBudget     uint64 // 0 disables the idle assertion
}

// w8ConfigFromEnv reads the GXW8_* knobs. Bounds are refused by name rather
// than clamped silently: a run whose parameters are not what the operator typed
// is not reproducible from its own manifest.
func w8ConfigFromEnv(lookup func(string) string) (w8Config, error) {
	cfg := w8Config{
		Fixture:        w8FixtureSpec{}.normalize(),
		Worktrees:      10,
		Commits:        20,
		Edits:          10,
		EditInterval:   30 * time.Second,
		Idle:           60 * time.Second,
		SampleInterval: time.Second,
		Repetitions:    1,
	}
	intKnob := func(name string, target *int, low, high int) error {
		raw := lookup(name)
		if raw == "" {
			return nil
		}
		value, err := strconv.Atoi(raw)
		if err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		if value < low || value > high {
			return fmt.Errorf("%s=%d is outside [%d,%d]", name, value, low, high)
		}
		*target = value
		return nil
	}
	durationKnob := func(name string, target *time.Duration, low, high time.Duration) error {
		raw := lookup(name)
		if raw == "" {
			return nil
		}
		value, err := time.ParseDuration(raw)
		if err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		if value < low || value > high {
			return fmt.Errorf("%s=%s is outside [%s,%s]", name, value, low, high)
		}
		*target = value
		return nil
	}
	seed := int(cfg.Fixture.Seed)
	for _, err := range []error{
		intKnob("GXW8_FIXTURE_FILES", &cfg.Fixture.Files, w8MinimumFixtureFiles, w8MaximumFixtureFiles),
		intKnob("GXW8_FIXTURE_PACKAGES", &cfg.Fixture.Packages, 1, 1000),
		intKnob("GXW8_FIXTURE_SEED", &seed, 1, 1<<30),
		intKnob("GXW8_WORKTREES", &cfg.Worktrees, 0, 50),
		intKnob("GXW8_COMMITS", &cfg.Commits, 0, 200),
		intKnob("GXW8_EDITS", &cfg.Edits, 0, 200),
		intKnob("GXW8_REPS", &cfg.Repetitions, 1, 10),
		durationKnob("GXW8_EDIT_INTERVAL", &cfg.EditInterval, 0, 10*time.Minute),
		durationKnob("GXW8_IDLE", &cfg.Idle, time.Second, time.Hour),
		durationKnob("GXW8_SAMPLE_INTERVAL", &cfg.SampleInterval, 100*time.Millisecond, time.Minute),
	} {
		if err != nil {
			return cfg, err
		}
	}
	cfg.Fixture.Seed = int64(seed)
	if raw := lookup("GXW8_IDLE_BUDGET_BYTES"); raw != "" {
		budget, err := strconv.ParseUint(raw, 10, 64)
		if err != nil {
			return cfg, fmt.Errorf("GXW8_IDLE_BUDGET_BYTES: %w", err)
		}
		cfg.IdleBudget = budget
	}
	cfg.Fixture = cfg.Fixture.normalize()
	return cfg, nil
}

// w8RequiredTimeout is a floor on `go test -timeout` for one invocation. It is
// deliberately generous: the failure mode of an under-budgeted run is a killed
// child daemon and a wasted hour, not a smaller number.
func w8RequiredTimeout(cfg w8Config, arms int) time.Duration {
	if arms < 1 {
		arms = 1
	}
	perArm := 2*cfg.Idle + // cold and warm idle
		time.Duration(cfg.Edits)*(cfg.EditInterval+30*time.Second) +
		time.Duration(cfg.Worktrees)*time.Minute +
		time.Duration(cfg.Commits)*time.Duration(1+cfg.Worktrees)*10*time.Second +
		10*time.Minute // cold index, settles, censuses, teardown
	return time.Duration(arms*cfg.Repetitions) * perArm
}

// w8Phase is one step of the workload. Detail is what the phase did, in the
// operator's vocabulary, and it lands in the artifact next to the numbers.
type w8Phase struct {
	Name   string
	Detail string
	Run    func(r *w8Run)
}

// w8PhasePlan is the nine-phase workload, in order.
func w8PhasePlan(cfg w8Config) []w8Phase {
	return []w8Phase{
		{
			Name:   "P0_cold_index",
			Detail: fmt.Sprintf("cold index of %d generated files in %d packages, to the first exact primary answer", cfg.Fixture.Files, cfg.Fixture.Packages),
			Run:    (*w8Run).phaseColdIndex,
		},
		{
			Name:   "P1_idle_cold",
			Detail: fmt.Sprintf("%s idle on the freshly indexed store, polling one read-only search every 5s", cfg.Idle),
			Run:    (*w8Run).phaseIdleCold,
		},
		{
			Name:   "P2_small_edits",
			Detail: fmt.Sprintf("%d edits %s apart, rotating over the corpus; each rewrites one function body and renames a one-line revision stub (the only declaration that moves, and the only thing a symbol search can wait on), awaited to exact from the edited file", cfg.Edits, cfg.EditInterval),
			Run:    (*w8Run).phaseSmallEdits,
		},
		{
			Name:   "P3_touch_stage_unstage",
			Detail: "touch with identical bytes, then git add -A, then git reset",
			Run:    (*w8Run).phaseTouchStageUnstage,
		},
		{
			Name:   "P4_amend_same_tree",
			Detail: "git commit --amend --no-edit: a new commit id over an unchanged tree",
			Run:    (*w8Run).phaseAmendSameTree,
		},
		{
			Name:   "P5_main_advance",
			Detail: fmt.Sprintf("%d dependent worktrees discovered (never tracked), then %d commits on main touching %d files each, each awaited to exact on the primary and on every dependent", cfg.Worktrees, cfg.Commits, w8CommitFilesPerCommit),
			Run:    (*w8Run).phaseMainAdvance,
		},
		{
			Name:   "P6_dependent_edits",
			Detail: "dirty edits in the dependent worktrees, awaited to exact, with primary isolation rechecked",
			Run:    (*w8Run).phaseDependentEdits,
		},
		{
			Name:   "P7_dependent_untrack_retrack",
			Detail: "explicitly track one dependent, then untrack it back to automatic discovery",
			Run:    (*w8Run).phaseDependentUntrackRetrack,
		},
		{
			Name:   "P8_idle_warm",
			Detail: fmt.Sprintf("%s idle on the worked store", cfg.Idle),
			Run:    (*w8Run).phaseIdleWarm,
		},
	}
}

// w8PhaseReport is one phase's evidence.
type w8PhaseReport struct {
	Phase            string  `json:"phase"`
	Detail           string  `json:"detail"`
	Failed           bool    `json:"failed,omitempty"`
	WallSeconds      float64 `json:"wall_s"`
	LogicalWrites    *uint64 `json:"ri_logical_writes_delta,omitempty"`
	DiskWritten      uint64  `json:"ri_diskio_byteswritten_delta"`
	DiskRead         uint64  `json:"ri_diskio_bytesread_delta,omitempty"`
	CPUUserNS        uint64  `json:"cpu_user_ns_delta,omitempty"`
	CPUSystemNS      uint64  `json:"cpu_system_ns_delta,omitempty"`
	PhysFootprint    uint64  `json:"ri_phys_footprint_end,omitempty"`
	StoreBytesBefore int64   `json:"store_bytes_before"`
	StoreBytesAfter  int64   `json:"store_bytes_after"`
	WALBefore        int64   `json:"wal_bytes_before"`
	WALAfter         int64   `json:"wal_bytes_after"`
	LogBefore        int64   `json:"daemon_log_bytes_before"`
	LogAfter         int64   `json:"daemon_log_bytes_after"`
	WALResets        int     `json:"wal_resets"`
	Samples          int     `json:"samples"`
	SampleFailures   int     `json:"sample_failures"`

	StoreCensusBefore w8StoreCensus `json:"store_census_before"`
	StoreCensusAfter  w8StoreCensus `json:"store_census_after"`

	CountersDelta map[string]int64 `json:"viewmetrics_delta,omitempty"`
	CountersError string           `json:"viewmetrics_error,omitempty"`

	CensusAfter w8Census `json:"destination_census_after"`

	ExactnessWaits   int      `json:"exactness_waits"`
	ExactnessSeconds float64  `json:"exactness_wait_s"`
	Notes            []string `json:"notes,omitempty"`
}

// w8StoreCensus is the SQL side of a phase boundary. Every sub-query is
// tolerated individually: a baseline binary on an older schema must still
// produce a report, with the missing series named rather than zeroed.
type w8StoreCensus struct {
	PageSize      int64                      `json:"page_size"`
	PageCount     int64                      `json:"page_count"`
	FreelistCount int64                      `json:"freelist_count"`
	Generations   issue767GenerationSnapshot `json:"generations"`
	ByState       map[string]w8StateCensus   `json:"generations_by_state,omitempty"`
	Checkouts     int64                      `json:"checkouts"`
	Routes        int64                      `json:"checkout_routes"`
	Errors        []string                   `json:"errors,omitempty"`
}

type w8StateCensus struct {
	Count        int64 `json:"count"`
	StorageBytes int64 `json:"storage_bytes"`
	Covered      int64 `json:"covered_files"`
	Affected     int64 `json:"affected_files"`
}

func w8ReadStoreCensus(ctx context.Context, db *sql.DB) w8StoreCensus {
	census := w8StoreCensus{ByState: map[string]w8StateCensus{}}
	note := func(err error) {
		if err != nil {
			census.Errors = append(census.Errors, err.Error())
		}
	}
	scalar := func(query string, target *int64) {
		note(db.QueryRowContext(ctx, query).Scan(target))
	}
	scalar("PRAGMA page_size", &census.PageSize)
	scalar("PRAGMA page_count", &census.PageCount)
	scalar("PRAGMA freelist_count", &census.FreelistCount)
	scalar("SELECT COUNT(*) FROM checkouts", &census.Checkouts)
	scalar("SELECT COUNT(*) FROM checkout_routes", &census.Routes)
	generations, err := issue767ReadGenerations(ctx, db)
	note(err)
	census.Generations = generations
	rows, err := db.QueryContext(ctx, "SELECT state, COUNT(*), COALESCE(SUM(storage_bytes),0), COALESCE(SUM(covered_files),0), COALESCE(SUM(affected_files),0) FROM view_generations GROUP BY state")
	if err != nil {
		note(err)
		return census
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var state string
		var entry w8StateCensus
		if err := rows.Scan(&state, &entry.Count, &entry.StorageBytes, &entry.Covered, &entry.Affected); err != nil {
			note(err)
			break
		}
		census.ByState[state] = entry
	}
	note(rows.Err())
	return census
}

// w8Run is one arm of one repetition.
type w8Run struct {
	t           *testing.T
	f           *issue767Fixture
	cfg         w8Config
	arm         string
	artifactDir string
	db          *sql.DB
	sampler     *w8Sampler
	samples     *os.File

	revisions  map[int]int
	rotation   []int
	dependents []string
	commits    int
	lastProbe  string // the probe name the primary currently answers with

	waits        int
	waitSeconds  float64
	phaseWaits   int
	phaseSeconds float64
	phaseNotes   []string

	reports []w8PhaseReport
}

// TestW8SustainedWriteAmplification is the opt-in sustained-workload run.
func TestW8SustainedWriteAmplification(t *testing.T) {
	candidate := os.Getenv("GXW8_TEST_BINARY")
	if candidate == "" {
		t.Skip("set GXW8_TEST_BINARY to opt into the isolated sustained-workload I/O harness")
	}
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("process I/O sampler supports Darwin and Linux")
	}
	cfg, err := w8ConfigFromEnv(os.Getenv)
	if err != nil {
		t.Fatal(err)
	}
	arms := []struct{ name, binary string }{}
	if baseline := os.Getenv("GXW8_BASELINE_BINARY"); baseline != "" {
		arms = append(arms, struct{ name, binary string }{"baseline", baseline})
	}
	arms = append(arms, struct{ name, binary string }{"candidate", candidate})
	required := w8RequiredTimeout(cfg, len(arms))
	if deadline, ok := t.Deadline(); ok && time.Until(deadline) < required {
		t.Fatalf("sustained workload needs at least %s remaining; run with go test -timeout %s or longer", required, required.Round(time.Minute))
	}
	artifactRoot := os.Getenv("GXW8_ARTIFACT_DIR")
	if artifactRoot == "" {
		artifactRoot = t.TempDir()
		t.Logf("GXW8_ARTIFACT_DIR is unset; artifacts go to %s and are removed with the test", artifactRoot)
	}
	for repetition := 1; repetition <= cfg.Repetitions; repetition++ {
		for _, arm := range arms {
			name := fmt.Sprintf("%s_rep%d", arm.name, repetition)
			t.Run(name, func(t *testing.T) {
				binary, err := filepath.Abs(arm.binary)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := os.Stat(binary); err != nil {
					t.Fatal(err)
				}
				w8RunWorkload(t, cfg, arm.name, binary, filepath.Join(artifactRoot, name))
			})
		}
	}
}

func w8RunWorkload(t *testing.T, cfg w8Config, arm, binary, artifactDir string) {
	t.Helper()
	if err := os.MkdirAll(artifactDir, 0o700); err != nil {
		t.Fatal(err)
	}
	fixture := w8GenerateFixture(cfg.Fixture)
	f := newIssue767FixtureWithCorpus(t, binary, func(f *issue767Fixture) {
		for _, file := range fixture {
			f.write(filepath.Join(f.primary, filepath.FromSlash(file.Path)), file.Content)
		}
	})
	samplesPath, err := w8ArtifactPath(artifactDir, f.root, "samples.ndjson")
	if err != nil {
		t.Fatal(err)
	}
	samples, err := os.Create(samplesPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = samples.Close() }()

	manifest := w8BuildManifest(w8Manifest{
		RunID:       filepath.Base(artifactDir),
		Arm:         arm,
		Fixture:     cfg.Fixture,
		Worktrees:   cfg.Worktrees,
		Commits:     cfg.Commits,
		Edits:       cfg.Edits,
		Repetitions: cfg.Repetitions,
		Cold:        true,
		Notes: []string{
			"GORTEX_RECONCILE_INTERVAL=5s accelerates the janitor; these are not default-configuration numbers",
			"ri_logical_writes is the primary series; ri_diskio_byteswritten is reported beside it and never alone",
			"process-accounted writes are not SSD NAND writes; a current WAL size is not cumulative writes",
			"wal_resets is a lower bound on checkpoints: a PASSIVE checkpoint that does not restart the log is invisible",
		},
	}, binary, f.env, fixture)
	if output, err := f.tryCommand(w8PhaseCommandTimeout, f.root, "version", "--short"); err == nil {
		manifest.BinaryVersion = strings.TrimSpace(string(output))
	} else {
		manifest.Notes = append(manifest.Notes, "binary version unavailable: "+err.Error())
	}
	manifest.SourceCommit, manifest.DirtyDigest = w8SourceIdentity()
	w8WriteJSON(t, filepath.Join(artifactDir, "manifest.json"), manifest)

	run := &w8Run{
		t: t, f: f, cfg: cfg, arm: arm, artifactDir: artifactDir,
		samples:   samples,
		revisions: map[int]int{},
		rotation:  w8RotationTargets(cfg.Fixture, max(cfg.Edits, 1)+w8CommitFilesPerCommit),
		lastProbe: w8PrimaryMarker,
	}
	run.db = f.openReadOnly()
	run.sampler = w8NewRunSampler(f, samples, cfg.SampleInterval)

	ctx, cancel := context.WithCancel(context.Background())
	sampling := make(chan struct{})
	go func() { run.sampler.Run(ctx); close(sampling) }()
	stopSampling := sync.OnceFunc(func() {
		cancel()
		<-sampling
	})
	defer stopSampling()
	// The phases run inside w8WithRunArtifacts, which files the run-level
	// artifact whether they finish or unwind: a phase's t.Fatal is a
	// runtime.Goexit, and a run whose last phase failed must still keep
	// report.json, the retained census and the totals of every phase that did
	// run — and must still remove the worktrees it created.
	w8WithRunArtifacts(run, manifest, func() {
		defer stopSampling()
		for _, phase := range w8PhasePlan(cfg) {
			run.execute(phase)
		}
	})
}

// w8WithRunArtifacts runs the phases and files the run-level artifact
// afterwards, on both exits.
func w8WithRunArtifacts(run *w8Run, manifest w8Manifest, phases func()) {
	defer run.finish(manifest)
	phases()
}

// w8NewRunSampler is the production wiring between a fixture and the 1 Hz
// sampler: the child's process counters, the store/WAL/SHM/log sizes and the
// WAL header. Every fixture field it reads goes through a guarded accessor,
// because this sampler runs on its own goroutine across the fixture's
// start/stop.
func w8NewRunSampler(f *issue767Fixture, out io.Writer, interval time.Duration) *w8Sampler {
	sampler := newW8Sampler(out, interval)
	sampler.pid = f.pid
	sampler.readIO = func() (issue767ProcessIO, error) {
		pid := f.pid()
		if pid == 0 {
			return issue767ProcessIO{}, errW8NoChild
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return issue767ReadProcessIO(ctx, pid)
	}
	sampler.readSize = func() w8FileSizes {
		return w8FileSizes{
			Store: issue767FileSize(f.store),
			WAL:   issue767FileSize(f.store + "-wal"),
			SHM:   issue767FileSize(f.store + "-shm"),
			Log:   issue767FileSize(f.logPath()),
		}
	}
	sampler.readWAL = func() (w8WALHeader, error) { return w8ReadWALHeader(f.store + "-wal") }
	return sampler
}

// w8PhaseOpening is everything execute reads before a phase runs. It is a
// value, not fields on the run, so the closing half can be reached from the
// success path and from the failure unwind with the same arguments.
type w8PhaseOpening struct {
	started     time.Time
	io          issue767ProcessIO
	ioErr       error
	counters    map[string]int64
	countersErr error
	samples     int
	failures    int
	resets      int
}

// execute brackets one phase with the whole measurement envelope.
func (r *w8Run) execute(phase w8Phase) {
	r.t.Helper()
	r.sampler.SetPhase(phase.Name)
	r.phaseWaits, r.phaseSeconds, r.phaseNotes = 0, 0, nil
	report := w8PhaseReport{Phase: phase.Name, Detail: phase.Detail}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	report.StoreCensusBefore = w8ReadStoreCensus(ctx, r.db)
	cancel()
	report.StoreBytesBefore = issue767FileSize(r.f.store)
	report.WALBefore = issue767FileSize(r.f.store + "-wal")
	report.LogBefore = r.daemonLogBytes()

	opening := w8PhaseOpening{started: time.Now()}
	opening.io, opening.ioErr = r.processIO()
	opening.counters, opening.countersErr = r.counters()
	if opening.countersErr != nil {
		report.CountersError = opening.countersErr.Error()
	}
	opening.samples, opening.failures = r.sampler.Samples()
	opening.resets = r.sampler.Resets()

	// A phase failure unwinds through t.Fatal (runtime.Goexit), so both the
	// diagnostics and the phase's own closing measurements have to be taken
	// from a defer. Returning early would throw away everything the failing
	// phase cost, which is exactly the number a failure makes interesting.
	completed := false
	defer func() {
		if completed {
			return
		}
		report.Failed = true
		r.dumpDiagnostics(phase.Name)
		r.closePhase(&report, opening)
	}()
	phase.Run(r)
	completed = true
	r.closePhase(&report, opening)
}

// closePhase takes the closing half of the envelope and files the phase
// report. It never calls t.Fatal: it runs during a failure unwind.
func (r *w8Run) closePhase(report *w8PhaseReport, opening w8PhaseOpening) {
	report.WallSeconds = time.Since(opening.started).Seconds()

	afterIO, afterErr := r.processIO()
	window := w8ProcessDelta(opening.io, opening.ioErr, afterIO, afterErr)
	report.Notes = append(report.Notes, window.Notes...)
	report.LogicalWrites = window.Logical
	report.DiskWritten, report.DiskRead = window.DiskWritten, window.DiskRead
	report.CPUUserNS, report.CPUSystemNS = window.CPUUserNS, window.CPUSystemNS
	report.PhysFootprint = window.PhysFootprint

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	report.StoreCensusAfter = w8ReadStoreCensus(ctx, r.db)
	cancel()
	report.StoreBytesAfter = issue767FileSize(r.f.store)
	report.WALAfter = issue767FileSize(r.f.store + "-wal")
	report.LogAfter = r.daemonLogBytes()
	if afterCounters, err := r.counters(); err != nil {
		report.CountersError = strings.TrimSpace(report.CountersError + " " + err.Error())
	} else if opening.countersErr == nil {
		report.CountersDelta = w8CounterDelta(opening.counters, afterCounters)
	}
	if census, err := w8WalkCensus(r.f.root); err != nil {
		report.Notes = append(report.Notes, "destination census failed: "+err.Error())
	} else {
		report.CensusAfter = census
	}
	afterSamples, afterFailures := r.sampler.Samples()
	report.Samples = afterSamples - opening.samples
	report.SampleFailures = afterFailures - opening.failures
	report.WALResets = r.sampler.Resets() - opening.resets
	report.ExactnessWaits, report.ExactnessSeconds = r.phaseWaits, r.phaseSeconds
	report.Notes = append(report.Notes, r.phaseNotes...)

	if !report.Failed {
		r.enforceIdleBudget(report)
	}
	r.reports = append(r.reports, *report)
	if err := w8TryWriteJSON(filepath.Join(r.artifactDir, "phase_"+report.Phase+".json"), *report); err != nil {
		r.t.Logf("phase artifact for %s not written: %v", report.Phase, err)
	}
	logical := "unavailable"
	if report.LogicalWrites != nil {
		logical = strconv.FormatUint(*report.LogicalWrites, 10)
	}
	r.t.Logf("%s/%s: failed=%v wall=%.1fs ri_logical_writes=%s ri_diskio_byteswritten=%d store=%d->%d wal=%d->%d resets=%d waits=%d/%.1fs",
		r.arm, report.Phase, report.Failed, report.WallSeconds, logical, report.DiskWritten,
		report.StoreBytesBefore, report.StoreBytesAfter, report.WALBefore, report.WALAfter,
		report.WALResets, report.ExactnessWaits, report.ExactnessSeconds)
}

// finish removes what the run created and writes the run-level artifact. Like
// closePhase it must survive a failure unwind, so every step here reports
// rather than fails.
func (r *w8Run) finish(manifest w8Manifest) {
	removed, retained := 0, []string{}
	for _, dependent := range r.dependents {
		if _, err := os.Stat(dependent); err != nil {
			continue
		}
		if output, err := r.f.tryGit(r.f.primary, "worktree", "remove", "--force", dependent); err != nil {
			retained = append(retained, dependent)
			r.t.Logf("worktree remove %s failed: %v\n%s", dependent, err, output)
			continue
		}
		removed++
	}
	census, err := w8WalkCensus(r.f.root)
	if err != nil {
		r.t.Logf("final census failed: %v", err)
	}
	samples, failures := r.sampler.Samples()
	failed := []string{}
	for _, report := range r.reports {
		if report.Failed {
			failed = append(failed, report.Phase)
		}
	}
	path := filepath.Join(r.artifactDir, "report.json")
	if err := w8TryWriteJSON(path, map[string]any{
		"manifest":               manifest,
		"phases":                 r.reports,
		"failed_phases":          failed,
		"worktrees_removed":      removed,
		"worktrees_retained":     retained,
		"retained_census":        census,
		"samples":                samples,
		"sample_failures":        failures,
		"wal_resets_total":       r.sampler.Resets(),
		"exactness_waits_total":  r.waits,
		"exactness_wait_seconds": r.waitSeconds,
	}); err != nil {
		r.t.Logf("run artifact not written: %v", err)
	}
	r.t.Logf("%s artifacts: %s (phases=%d failed=%v worktrees removed=%d retained=%d)",
		r.arm, r.artifactDir, len(r.reports), failed, removed, len(retained))
}

// ------------------------------------------------------------- run helpers ---

func w8Delta(before, after uint64) uint64 {
	if after < before {
		return 0
	}
	return after - before
}

func (r *w8Run) processIO() (issue767ProcessIO, error) {
	pid := r.f.pid()
	if pid == 0 {
		return issue767ProcessIO{}, errW8NoChild
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return issue767ReadProcessIO(ctx, pid)
}

// counters scrapes the daemon's own viewmetrics through `daemon status
// --format json`. A binary without the flag — every pre-W8.3 baseline arm —
// returns an error, which is recorded by name; it is never substituted with
// zeros.
func (r *w8Run) counters() (map[string]int64, error) {
	output, err := r.f.tryCommand(w8StatusScrapeTimeout, r.f.primary, "daemon", "status", "--format", "json", "--no-progress")
	if err != nil {
		return nil, fmt.Errorf("daemon status --format json unavailable on this arm: %w", err)
	}
	return w8ParseStatusCounters(output)
}

func (r *w8Run) daemonLogBytes() int64 {
	return issue767FileSize(r.f.logPath())
}

// awaitProbe waits for one exact answer and accounts for the wait, so a phase's
// cost includes how long the daemon made a reader wait for exactness.
func (r *w8Run) awaitProbe(root, name, file string) {
	r.t.Helper()
	r.awaitProbeAs(root, name, file, r.f.spellingFor(root))
}

// awaitProbeAs is awaitProbe with the request spelling named explicitly, for
// the phase that changes a checkout's mode under the harness's feet.
func (r *w8Run) awaitProbeAs(root, name, file string, spelling issue767Spelling) {
	r.t.Helper()
	started := time.Now()
	r.f.awaitSymbolAs(root, name, file, w8DefaultProbeTimeout, spelling)
	elapsed := time.Since(started).Seconds()
	r.waits++
	r.phaseWaits++
	r.waitSeconds += elapsed
	r.phaseSeconds += elapsed
}

func (r *w8Run) markerPath(root string) string { return filepath.Join(root, "marker.go") }

func (r *w8Run) filePath(index int) string {
	return filepath.Join(r.f.primary, filepath.FromSlash(w8FilePath(index%r.cfg.Fixture.Packages, index)))
}

// editFile advances one corpus file's revision and waits for the new probe to
// answer exactly out of that same file.
func (r *w8Run) editFile(index int) {
	r.t.Helper()
	path := r.filePath(index)
	source, err := os.ReadFile(path)
	if err != nil {
		r.t.Fatal(err)
	}
	revision := r.revisions[index] + 1
	edited, err := w8EditFileSource(string(source), index, revision)
	if err != nil {
		r.t.Fatal(err)
	}
	r.f.write(path, edited)
	r.revisions[index] = revision
	r.awaitProbe(r.f.primary, w8ProbeName(index, revision), path)
}

func (r *w8Run) idle(duration time.Duration) {
	r.t.Helper()
	deadline := time.Now().Add(duration)
	for time.Now().Before(deadline) {
		found, err := r.f.trySearchSymbolIn(r.f.primary, r.lastProbe, r.lastProbeFile())
		if err != nil || !found {
			r.t.Fatalf("idle read-only query lost the selected ready symbol %s: found=%v err=%v", r.lastProbe, found, err)
		}
		select {
		case <-r.t.Context().Done():
			r.t.Fatal(r.t.Context().Err())
		case <-time.After(5 * time.Second):
		}
	}
}

func (r *w8Run) lastProbeFile() string { return r.markerPath(r.f.primary) }

// ------------------------------------------------------------------ phases ---

func (r *w8Run) phaseColdIndex() {
	r.f.start()
	r.f.command(w8PhaseCommandTimeout, r.f.primary, "daemon", "status", "--no-progress")
	r.awaitProbe(r.f.primary, w8PrimaryMarker, r.markerPath(r.f.primary))
	r.f.settle()
}

func (r *w8Run) phaseIdleCold() {
	r.idle(r.cfg.Idle)
}

func (r *w8Run) phaseSmallEdits() {
	for i := 0; i < r.cfg.Edits; i++ {
		started := time.Now()
		r.editFile(r.rotation[i%len(r.rotation)])
		if remaining := r.cfg.EditInterval - time.Since(started); remaining > 0 && i+1 < r.cfg.Edits {
			select {
			case <-r.t.Context().Done():
				r.t.Fatal(r.t.Context().Err())
			case <-time.After(remaining):
			}
		}
	}
	r.f.settle()
}

func (r *w8Run) phaseTouchStageUnstage() {
	index := r.rotation[0]
	path := r.filePath(index)
	source, err := os.ReadFile(path)
	if err != nil {
		r.t.Fatal(err)
	}
	// Identical bytes, new mtime: the classic no-op the watcher must absorb.
	r.f.write(path, string(source))
	r.settleShort()
	r.f.git(r.f.primary, "add", "-A")
	r.settleShort()
	r.f.git(r.f.primary, "reset")
	r.settleShort()
	if revision := r.revisions[index]; revision > 0 {
		r.awaitProbe(r.f.primary, w8ProbeName(index, revision), path)
	} else {
		r.awaitProbe(r.f.primary, r.lastProbe, r.lastProbeFile())
	}
	r.f.settle()
}

func (r *w8Run) phaseAmendSameTree() {
	// Commit whatever the edit phases left dirty first, so the amend that
	// follows has nothing staged and is a genuine same-tree amend: a new
	// commit id over byte-identical content.
	r.f.git(r.f.primary, "add", "-A")
	r.f.git(r.f.primary, "commit", "--allow-empty", "-m", "pre-amend")
	r.f.settle()
	treeBefore := r.gitOutput("rev-parse", "HEAD^{tree}")
	headBefore := r.gitOutput("rev-parse", "HEAD")
	r.f.git(r.f.primary, "commit", "--amend", "--no-edit", "--date=now")
	treeAfter := r.gitOutput("rev-parse", "HEAD^{tree}")
	headAfter := r.gitOutput("rev-parse", "HEAD")
	if treeBefore != treeAfter {
		r.t.Fatalf("amend was meant to keep the tree: %s -> %s", treeBefore, treeAfter)
	}
	if headBefore == headAfter {
		r.t.Fatalf("amend did not move HEAD away from %s", headBefore)
	}
	r.t.Logf("same-tree amend: HEAD %s -> %s over tree %s", headBefore, headAfter, treeAfter)
	r.awaitProbe(r.f.primary, r.lastProbe, r.lastProbeFile())
	r.f.settle()
}

func (r *w8Run) phaseMainAdvance() {
	for i := 1; i <= r.cfg.Worktrees; i++ {
		path := filepath.Join(r.f.root, fmt.Sprintf("wt%02d", i))
		r.f.git(r.f.primary, "worktree", "add", "-b", fmt.Sprintf("w%02d", i), path)
		r.dependents = append(r.dependents, path)
		// Discovered, never tracked: no track call, no config edit.
		r.awaitProbe(path, w8PrimaryMarker, r.markerPath(path))
	}
	r.f.settle()
	for commit := 1; commit <= r.cfg.Commits; commit++ {
		marker := fmt.Sprintf(w8AdvanceMarkerFormat, commit)
		r.f.write(r.markerPath(r.f.primary), issue767MarkerSource(w8PrimaryMarker, marker))
		for offset := 0; offset < w8CommitFilesPerCommit-1; offset++ {
			index := r.rotation[(commit+offset)%len(r.rotation)]
			path := r.filePath(index)
			source, err := os.ReadFile(path)
			if err != nil {
				r.t.Fatal(err)
			}
			revision := r.revisions[index] + 1
			edited, err := w8EditFileSource(string(source), index, revision)
			if err != nil {
				r.t.Fatal(err)
			}
			r.f.write(path, edited)
			r.revisions[index] = revision
		}
		r.f.git(r.f.primary, "add", "-A")
		r.f.git(r.f.primary, "commit", "-m", fmt.Sprintf("advance %d", commit))
		r.commits++
		r.lastProbe = marker
		r.awaitProbe(r.f.primary, marker, r.markerPath(r.f.primary))
		for _, dependent := range r.dependents {
			// The dependent's own tree did not move; its base did. It must
			// still answer exactly for its own committed marker.
			r.awaitProbe(dependent, w8PrimaryMarker, r.markerPath(dependent))
		}
	}
	if len(r.dependents) > 0 && r.commits > 0 {
		leaked, err := r.f.trySearchSymbolIn(r.dependents[0], r.lastProbe, r.markerPath(r.dependents[0]))
		if err != nil {
			r.t.Fatalf("dependent isolation query failed: %v", err)
		}
		if leaked {
			r.t.Fatalf("main-only symbol %s leaked into dependent %s", r.lastProbe, r.dependents[0])
		}
	}
	r.f.settle()
}

func (r *w8Run) phaseDependentEdits() {
	if len(r.dependents) == 0 {
		r.t.Log("no dependents configured; phase is a no-op")
		return
	}
	edited := min(len(r.dependents), 3)
	for i := 0; i < edited; i++ {
		dependent := r.dependents[i]
		marker := fmt.Sprintf(w8DependentMarkerForm, i+1)
		r.f.write(r.markerPath(dependent), issue767MarkerSource(w8PrimaryMarker, marker))
		r.awaitProbe(dependent, marker, r.markerPath(dependent))
		leaked, err := r.f.trySearchSymbolIn(r.f.primary, marker, r.markerPath(r.f.primary))
		if err != nil {
			r.t.Fatalf("primary isolation query failed: %v", err)
		}
		if leaked {
			r.t.Fatalf("dependent edit %s leaked into the primary view", marker)
		}
	}
	r.f.settle()
}

// phaseDependentUntrackRetrack takes one automatic dependent through the
// explicit-tracking round trip: automatic → dedicated → automatic, with the
// checkout asked, at each step, through the door the product serves that mode
// from.
//
// The CLI spelling is taken from the source, not guessed: a linked worktree of
// an already-tracked family needs --as-worktree to become an independent
// instance ("Track a linked git worktree as an independent instance even when
// its repo is already tracked elsewhere", track.go:76), and --wait to block
// until its graph is queryable. Plain `gortex track <worktree>` is explicitly
// NOT the remedy for an unbound worktree (cli_daemon.go:448).
//
// The REQUEST spelling changes with the mode, and that is the contract this
// phase exists to exercise:
//
//   - Automatic (before track, and again after untrack): the checkout is routed,
//     so the explicit worktree view selector answers and labels the answer exact.
//   - Dedicated (after track --as-worktree): the checkout is served from its own
//     indexed corpus — "Only an automatic checkout is routed here. A dedicated
//     checkout and the family's primary are served from the indexed corpus"
//     (internal/mcp/view_request.go:1038-1041), and the CLI says the same thing
//     from the other side: "A linked worktree registered as a repository of its
//     own is indexed a second time and stops being served through its family"
//     (cmd/gortex/cli_daemon.go:448-450). It owns no checkout_routes row, so it
//     is asked as its own repository, with no view selector, and what pins the
//     answer is physical: the answering symbol's absolute path must be the
//     dependent's own marker file. Which corpus served it is recorded, not
//     asserted — measured as "issue767@wt01", the checkout's own graph.
//
// The worktree-view spelling is also tried against the dedicated checkout and
// its outcome recorded as a phase note — never asserted. That is the measured
// observation this phase contributes: which spellings a dedicated checkout
// answers, and what each answer carries.
//
// untrack then demotes the dedicated checkout back into the family's automatic
// lane, which runs outright because the primary corpus survives to serve it
// (untrackCmd's Long, track.go:60-68); only a row-removing plan needs --confirm.
func (r *w8Run) phaseDependentUntrackRetrack() {
	if len(r.dependents) == 0 {
		r.t.Log("no dependents configured; phase is a no-op")
		return
	}
	dependent := r.dependents[0]
	marker := fmt.Sprintf(w8DependentMarkerForm, 1)
	file := r.markerPath(dependent)

	// Automatic, before anything changes: the routed view answers exactly.
	r.awaitProbeAs(dependent, marker, file, issue767AsAutomaticWorktree)
	r.noteAnswer("automatic checkout", dependent, marker, file, issue767AsAutomaticWorktree)
	r.noteAnswer("automatic checkout", dependent, marker, file, issue767AsOwnCorpus)

	output, err := r.f.tryCommand(8*time.Minute, dependent,
		"track", dependent, "--as-worktree", "--wait", "--wait-timeout", "5m", "--no-progress")
	if err != nil {
		r.t.Fatalf("track --as-worktree %s: %v\n%s", dependent, err, output)
	}
	r.note("track --as-worktree: " + w8Tail(output))

	// Dedicated: asked as its own repository.
	r.awaitProbeAs(dependent, marker, file, issue767AsOwnCorpus)
	r.noteAnswer("dedicated checkout", dependent, marker, file, issue767AsOwnCorpus)
	r.noteAnswer("dedicated checkout", dependent, marker, file, issue767AsAutomaticWorktree)
	r.f.settle()

	output, err = r.f.tryCommand(8*time.Minute, r.f.primary, "untrack", dependent, "--no-progress")
	if err != nil {
		r.t.Fatalf("untrack %s: %v\n%s", dependent, err, output)
	}
	r.note("untrack: " + w8Tail(output))
	// Demoted back to automatic discovery: the primary survives, so the routed
	// spelling must answer exactly from the dependent's own working copy again.
	r.awaitProbeAs(dependent, marker, file, issue767AsAutomaticWorktree)
	r.noteAnswer("re-demoted checkout", dependent, marker, file, issue767AsAutomaticWorktree)
	r.awaitProbe(r.f.primary, r.lastProbe, r.lastProbeFile())
	r.f.settle()
}

// noteAnswer records what one spelling answers for one symbol, as evidence
// rather than as an assertion. A spelling the product refuses is recorded with
// its refusal; a spelling that answers is recorded with which labels the answer
// did and did not carry.
func (r *w8Run) noteAnswer(subject, root, name, file string, spelling issue767Spelling) {
	answer, err := r.f.askSymbol(root, name, file, spelling)
	if err != nil {
		r.note(fmt.Sprintf("%s asked via %s: refused: %s", subject, spelling, w8Tail([]byte(err.Error()))))
		return
	}
	r.note(fmt.Sprintf("%s asked via %s: found=%v exact_label=%v fallback=%v from_expected_file=%v answered_by=%q",
		subject, spelling, answer.Found, answer.Exact, answer.Fallback, answer.FromExpectedFile, answer.Prefix))
}

// dumpDiagnostics writes the failure evidence next to the phase artifacts.
func (r *w8Run) dumpDiagnostics(phase string) {
	run := func(args ...string) string {
		output, err := r.f.tryCommand(60*time.Second, r.f.primary, args...)
		if err != nil {
			return fmt.Sprintf("ERROR %v\n%s", err, output)
		}
		return string(output)
	}
	paths := append([]string{r.f.primary}, r.dependents...)
	diagnostics := w8CollectDiagnostics(phase, run, r.daemonLogTail(), paths)
	data, err := json.MarshalIndent(diagnostics, "", "  ")
	if err != nil {
		return
	}
	path := filepath.Join(r.artifactDir, "diagnostics_"+phase+".json")
	if err := os.WriteFile(path, append(data, '\n'), 0o600); err != nil {
		return
	}
	r.t.Logf("%s failed; diagnostics written to %s", phase, path)
}

// daemonLogTail is the last 8 KiB of the running child's log.
func (r *w8Run) daemonLogTail() string {
	data, err := os.ReadFile(r.f.logPath())
	if err != nil {
		return "daemon log unavailable: " + err.Error()
	}
	if len(data) > 8192 {
		data = data[len(data)-8192:]
	}
	return string(data)
}

// note records a phase-scoped observation; execute folds it into the report.
func (r *w8Run) note(text string) { r.phaseNotes = append(r.phaseNotes, text) }

// w8Tail keeps a command's last line or two, so a phase note carries the
// outcome without carrying a screenful of progress output.
func w8Tail(output []byte) string {
	lines := strings.Split(strings.TrimSpace(string(output)), "\n")
	if len(lines) > 2 {
		lines = lines[len(lines)-2:]
	}
	return strings.TrimSpace(strings.Join(lines, " / "))
}

func (r *w8Run) phaseIdleWarm() {
	r.idle(r.cfg.Idle)
}

// enforceIdleBudget is the only budget in this item, and it is off unless the
// operator names a number. Budgets proper are frozen from a baseline arm by a
// later item; this exists so a run can be given one without editing code. It
// applies to the idle phases only, on the candidate arm only, and it records
// the reason when the series it needs is unavailable rather than passing by
// default.
func (r *w8Run) enforceIdleBudget(report *w8PhaseReport) {
	if r.cfg.IdleBudget == 0 || r.arm != "candidate" || !strings.Contains(report.Phase, "_idle_") {
		return
	}
	if report.LogicalWrites == nil {
		report.Notes = append(report.Notes, "idle budget not checked: ri_logical_writes unavailable")
		return
	}
	if *report.LogicalWrites > r.cfg.IdleBudget {
		r.t.Errorf("%s wrote %d logical bytes, budget %d", report.Phase, *report.LogicalWrites, r.cfg.IdleBudget)
	}
}

func (r *w8Run) settleShort() {
	select {
	case <-r.t.Context().Done():
		r.t.Fatal(r.t.Context().Err())
	case <-time.After(5 * time.Second):
	}
}

func (r *w8Run) gitOutput(args ...string) string {
	r.t.Helper()
	output, err := w8GitOutput(r.t, r.f, args...)
	if err != nil {
		r.t.Fatalf("git %v: %v", args, err)
	}
	return output
}

func w8GitOutput(t *testing.T, f *issue767Fixture, args ...string) (string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir, cmd.Env = f.primary, f.env
	output, err := cmd.Output()
	return strings.TrimSpace(string(output)), err
}

// w8SourceIdentity records which tree the harness itself came from. It is best
// effort: the harness may run from an exported tree with no git metadata.
func w8SourceIdentity() (commit, dirty string) {
	run := func(args ...string) string {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		output, err := exec.CommandContext(ctx, "git", args...).Output()
		if err != nil {
			return ""
		}
		return strings.TrimSpace(string(output))
	}
	commit = run("rev-parse", "HEAD")
	status := run("status", "--porcelain=v1", "-uall")
	if status != "" {
		sum := sha256.Sum256([]byte(status))
		dirty = hex.EncodeToString(sum[:])
	}
	return commit, dirty
}

func w8WriteJSON(t *testing.T, path string, value any) {
	t.Helper()
	if err := w8TryWriteJSON(path, value); err != nil {
		t.Fatal(err)
	}
}

// w8TryWriteJSON is w8WriteJSON without the test handle, for the code paths
// that run while a failure is already unwinding.
func w8TryWriteJSON(path string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o600)
}

// ------------------------------------------------------------------ tests ---

func TestW8ConfigFromEnvDefaultsAndBounds(t *testing.T) {
	cfg, err := w8ConfigFromEnv(func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Fixture.Files != 1500 || cfg.Worktrees != 10 || cfg.Commits != 20 || cfg.Edits != 10 ||
		cfg.EditInterval != 30*time.Second || cfg.Idle != time.Minute || cfg.SampleInterval != time.Second ||
		cfg.Repetitions != 1 || cfg.IdleBudget != 0 {
		t.Fatalf("defaults drifted: %+v", cfg)
	}
	env := map[string]string{
		"GXW8_FIXTURE_FILES":     "6000",
		"GXW8_FIXTURE_PACKAGES":  "120",
		"GXW8_FIXTURE_SEED":      "99",
		"GXW8_WORKTREES":         "2",
		"GXW8_COMMITS":           "3",
		"GXW8_EDITS":             "4",
		"GXW8_EDIT_INTERVAL":     "2s",
		"GXW8_IDLE":              "5s",
		"GXW8_SAMPLE_INTERVAL":   "500ms",
		"GXW8_REPS":              "2",
		"GXW8_IDLE_BUDGET_BYTES": "8388608",
	}
	cfg, err = w8ConfigFromEnv(func(key string) string { return env[key] })
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Fixture.Files != 6000 || cfg.Fixture.Packages != 120 || cfg.Fixture.Seed != 99 ||
		cfg.Worktrees != 2 || cfg.Commits != 3 || cfg.Edits != 4 ||
		cfg.EditInterval != 2*time.Second || cfg.Idle != 5*time.Second ||
		cfg.SampleInterval != 500*time.Millisecond || cfg.Repetitions != 2 || cfg.IdleBudget != 8388608 {
		t.Fatalf("knobs not applied: %+v", cfg)
	}
	for _, tc := range []struct{ key, value string }{
		{"GXW8_FIXTURE_FILES", "1"},
		{"GXW8_FIXTURE_FILES", "nonsense"},
		{"GXW8_WORKTREES", "-1"},
		{"GXW8_REPS", "0"},
		{"GXW8_IDLE", "10ms"},
		{"GXW8_IDLE", "nope"},
		{"GXW8_IDLE_BUDGET_BYTES", "huge"},
	} {
		if _, err := w8ConfigFromEnv(func(key string) string {
			if key == tc.key {
				return tc.value
			}
			return ""
		}); err == nil {
			t.Errorf("%s=%q was accepted; out-of-range knobs must be refused by name", tc.key, tc.value)
		}
	}
}

func TestW8PhasePlanIsTheDeclaredNinePhaseWorkload(t *testing.T) {
	cfg, err := w8ConfigFromEnv(func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	plan := w8PhasePlan(cfg)
	want := []string{
		"P0_cold_index",
		"P1_idle_cold",
		"P2_small_edits",
		"P3_touch_stage_unstage",
		"P4_amend_same_tree",
		"P5_main_advance",
		"P6_dependent_edits",
		"P7_dependent_untrack_retrack",
		"P8_idle_warm",
	}
	if len(plan) != len(want) {
		t.Fatalf("plan has %d phases, want %d", len(plan), len(want))
	}
	for i, phase := range plan {
		if phase.Name != want[i] {
			t.Errorf("phase %d = %q, want %q", i, phase.Name, want[i])
		}
		if phase.Run == nil {
			t.Errorf("phase %s has no runner", phase.Name)
		}
		if phase.Detail == "" {
			t.Errorf("phase %s has no detail line", phase.Name)
		}
	}
	if !strings.Contains(plan[5].Detail, "10 dependent worktrees") || !strings.Contains(plan[5].Detail, "20 commits") {
		t.Errorf("advance phase detail lost its parameters: %q", plan[5].Detail)
	}
	if !strings.Contains(plan[2].Detail, "10 edits") || !strings.Contains(plan[2].Detail, "30s apart") {
		t.Errorf("edit phase detail lost its parameters: %q", plan[2].Detail)
	}
	// The edit phase's detail must say what an edit IS. "Small edit" without
	// the body/stub split is what made the earlier phase label misdescribe a
	// heavier edit class than the one it measures.
	for _, want := range []string{"function body", "revision stub"} {
		if !strings.Contains(plan[2].Detail, want) {
			t.Errorf("edit phase detail does not say it %q: %q", want, plan[2].Detail)
		}
	}
	scaled, err := w8ConfigFromEnv(func(key string) string {
		return map[string]string{"GXW8_WORKTREES": "2", "GXW8_COMMITS": "3"}[key]
	})
	if err != nil {
		t.Fatal(err)
	}
	if detail := w8PhasePlan(scaled)[5].Detail; !strings.Contains(detail, "2 dependent worktrees") || !strings.Contains(detail, "3 commits") {
		t.Errorf("phase detail does not follow the config: %q", detail)
	}
}

func TestW8RequiredTimeoutScalesWithTheWorkload(t *testing.T) {
	small := w8Config{Worktrees: 1, Commits: 1, Edits: 1, EditInterval: time.Second, Idle: 5 * time.Second, Repetitions: 1}
	large := w8Config{Worktrees: 10, Commits: 20, Edits: 10, EditInterval: 30 * time.Second, Idle: time.Minute, Repetitions: 1}
	if w8RequiredTimeout(small, 1) >= w8RequiredTimeout(large, 1) {
		t.Fatal("a larger workload must require a larger timeout")
	}
	if w8RequiredTimeout(large, 2) != 2*w8RequiredTimeout(large, 1) {
		t.Fatal("a second arm must double the requirement")
	}
	paired := large
	paired.Repetitions = 3
	if w8RequiredTimeout(paired, 1) != 3*w8RequiredTimeout(large, 1) {
		t.Fatal("repetitions must multiply the requirement")
	}
	if w8RequiredTimeout(large, 0) != w8RequiredTimeout(large, 1) {
		t.Fatal("an arm count below one must be treated as one arm")
	}
}

// TestW8ProcessDeltaHandlesTheStartingChildAndTheRestart pins the three cases
// the phase envelope must tell apart: an ordinary window, the phase that starts
// the daemon (no baseline process, so the child's own counters ARE the delta),
// and a child that was replaced mid-phase (deltas are meaningless and must not
// be reported as a measurement).
func TestW8ProcessDeltaHandlesTheStartingChildAndTheRestart(t *testing.T) {
	logical := func(v uint64) *uint64 { return &v }
	before := issue767ProcessIO{BytesWritten: 100, BytesRead: 10, UserTimeNS: 5, SystemTimeNS: 6, StartTicks: 7, LogicalBytesWritten: logical(1000)}
	after := issue767ProcessIO{BytesWritten: 400, BytesRead: 30, UserTimeNS: 15, SystemTimeNS: 26, StartTicks: 7, PhysFootprint: 42, LogicalBytesWritten: logical(4000)}

	window := w8ProcessDelta(before, nil, after, nil)
	if !window.OK || window.Logical == nil || *window.Logical != 3000 || window.DiskWritten != 300 ||
		window.DiskRead != 20 || window.CPUUserNS != 10 || window.CPUSystemNS != 20 || window.PhysFootprint != 42 {
		t.Fatalf("ordinary window = %+v (logical=%v)", window, window.Logical)
	}
	if len(window.Notes) != 0 {
		t.Errorf("ordinary window carried notes: %v", window.Notes)
	}

	// The cold-index phase: nothing to read before the child exists.
	window = w8ProcessDelta(issue767ProcessIO{}, errW8NoChild, after, nil)
	if !window.OK || window.Logical == nil || *window.Logical != 4000 || window.DiskWritten != 400 {
		t.Fatalf("starting-child window lost the cold series: %+v", window)
	}
	if len(window.Notes) != 1 || !strings.Contains(window.Notes[0], "lifetime totals") {
		t.Errorf("starting-child window must say what it measured: %v", window.Notes)
	}

	// A restart mid-phase: the counters belong to a different process.
	restarted := after
	restarted.StartTicks = 9
	window = w8ProcessDelta(before, nil, restarted, nil)
	if window.OK || window.Logical != nil || window.DiskWritten != 0 {
		t.Fatalf("a restarted child must not produce a delta: %+v", window)
	}
	if len(window.Notes) != 1 || !strings.Contains(window.Notes[0], "restarted") {
		t.Errorf("restart note missing: %v", window.Notes)
	}

	// Unreadable ends stay unavailable, by name, on both sides.
	if window = w8ProcessDelta(before, nil, after, errors.New("boom")); window.OK || !strings.Contains(window.Notes[0], "boom") {
		t.Errorf("end failure = %+v", window)
	}
	if window = w8ProcessDelta(before, errors.New("kaput"), after, nil); window.OK || !strings.Contains(window.Notes[0], "kaput") {
		t.Errorf("start failure = %+v", window)
	}
}

// TestW8CollectDiagnosticsAsksTheExplainingQuestions pins the failure-evidence
// command set: a timed-out exactness wait must leave behind the daemon status
// payload, the family census and one route explanation per checkout in play.
func TestW8CollectDiagnosticsAsksTheExplainingQuestions(t *testing.T) {
	var calls []string
	run := func(args ...string) string {
		joined := strings.Join(args, " ")
		calls = append(calls, joined)
		return "output of " + joined
	}
	diagnostics := w8CollectDiagnostics("P7_dependent_untrack_retrack", run, "log tail", []string{"/tmp/repo", "/tmp/wt01"})
	if diagnostics.Phase != "P7_dependent_untrack_retrack" || diagnostics.LogTail != "log tail" {
		t.Fatalf("diagnostics envelope = %+v", diagnostics)
	}
	for _, want := range []string{
		"daemon status --format json --no-progress",
		"repos families --format json --no-progress",
		"repos explain-view /tmp/repo --index /tmp/repo --format json --no-progress",
		"repos explain-view /tmp/wt01 --index /tmp/wt01 --format json --no-progress",
	} {
		if got, ok := diagnostics.Commands[want]; !ok || got != "output of "+want {
			t.Errorf("missing or wrong output for %q: %q", want, got)
		}
	}
	if len(diagnostics.Commands) != 4 || len(calls) != 4 {
		t.Errorf("collected %d commands from %d calls, want 4", len(diagnostics.Commands), len(calls))
	}
}

func TestW8StoreCensusSurvivesAMissingSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "empty.sqlite")
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	db.SetMaxOpenConns(1)
	if _, err := db.Exec("CREATE TABLE unrelated(id INTEGER)"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	census := w8ReadStoreCensus(ctx, db)
	if census.PageSize == 0 || census.PageCount == 0 {
		t.Errorf("page pragmas must still be read: %+v", census)
	}
	if len(census.Errors) == 0 {
		t.Fatal("a store without the view tables must name its missing series, not report zeros silently")
	}
}

// TestW8RunSamplerIsSafeWhileTheFixtureStartsItsChild exercises the production
// wiring — w8NewRunSampler, the same call w8RunWorkload makes — against the
// fixture's own start/stop handle mutations, on two goroutines.
//
// It is a regression test for a real defect: the sampler goroutine is started
// BEFORE P0 calls f.start(), so its pid and log-size reads run concurrently
// with the writes that install the child. Before the fixture's handle fields
// were placed under a mutex this pattern tripped the race detector, which made
// the whole harness unrunnable under -race. No daemon is needed: the child
// handle is never started, so pid() reports 0 and the process reader returns
// errW8NoChild — the fields being raced are the same ones either way.
func TestW8RunSamplerIsSafeWhileTheFixtureStartsItsChild(t *testing.T) {
	root := t.TempDir()
	f := &issue767Fixture{t: t, root: root, primary: filepath.Join(root, "repo"), store: filepath.Join(root, "store.sqlite")}
	sampler := w8NewRunSampler(f, io.Discard, time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	sampling := make(chan struct{})
	go func() { sampler.Run(ctx); close(sampling) }()
	hammering := make(chan struct{})
	go func() {
		defer close(hammering)
		for i := 0; i < 200; i++ {
			sampler.Sample()
		}
	}()

	for i := 0; i < 50; i++ {
		run := f.nextRun()
		log, err := os.Create(f.logPathFor(run))
		if err != nil {
			t.Fatal(err)
		}
		f.setChild(exec.Command(os.Args[0], "-test.run=NoSuchTest"), make(chan error, 1), func() {}, log)
		if got := f.logPath(); got != f.logPathFor(run) {
			t.Fatalf("log path %q does not follow the run index %d", got, run)
		}
		if pid := f.pid(); pid != 0 {
			t.Fatalf("an unstarted child reported pid %d", pid)
		}
		_, _, _, taken := f.takeChild()
		if taken != log {
			t.Fatal("takeChild did not return the installed log handle")
		}
		_ = log.Close()
	}

	<-hammering
	cancel()
	<-sampling
	if samples, _ := sampler.Samples(); samples < 200 {
		t.Fatalf("sampler took %d samples, want at least the 200 the hammer asked for", samples)
	}
}

// TestW8ClosePhaseFilesAFailedPhaseAndFinishStillWritesTheRun is the F3
// regression: a phase that fails must not take the run-level artifact with it.
// It drives closePhase and finish the way the failure defer in execute does,
// with no test handle available to fail on.
func TestW8ClosePhaseFilesAFailedPhaseAndFinishStillWritesTheRun(t *testing.T) {
	artifacts := t.TempDir()
	root := t.TempDir()
	f := &issue767Fixture{t: t, binary: filepath.Join(root, "no-such-gortex"), root: root, primary: filepath.Join(root, "repo"), store: filepath.Join(root, "store.sqlite")}
	if err := os.MkdirAll(f.primary, 0o700); err != nil {
		t.Fatal(err)
	}
	// The closing half reads the store census, so it needs a database handle;
	// an empty one exercises the same "name what is missing" path.
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(f.store))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	db.SetMaxOpenConns(1)
	run := &w8Run{t: t, f: f, arm: "candidate", artifactDir: artifacts, db: db, sampler: newW8Sampler(io.Discard, time.Hour)}
	report := w8PhaseReport{Phase: "P7_dependent_untrack_retrack", Detail: "the phase that failed"}
	report.Failed = true
	run.phaseNotes = []string{"the note the failing phase left"}
	run.closePhase(&report, w8PhaseOpening{started: time.Now(), ioErr: errW8NoChild})

	var filed w8PhaseReport
	w8ReadJSON(t, filepath.Join(artifacts, "phase_P7_dependent_untrack_retrack.json"), &filed)
	if !filed.Failed {
		t.Error("the filed phase report does not record that the phase failed")
	}
	if len(filed.Notes) == 0 {
		t.Error("the filed phase report dropped the failing phase's notes")
	}
	if len(run.reports) != 1 {
		t.Fatalf("the run kept %d phase reports, want 1", len(run.reports))
	}

	run.dependents = []string{filepath.Join(root, "never-created-worktree")}
	run.finish(w8Manifest{RunID: "unit"})
	var summary map[string]any
	w8ReadJSON(t, filepath.Join(artifacts, "report.json"), &summary)
	phases, _ := summary["phases"].([]any)
	if len(phases) != 1 {
		t.Fatalf("report.json carries %d phases, want the one that ran", len(phases))
	}
	failed, _ := summary["failed_phases"].([]any)
	if len(failed) != 1 || failed[0] != "P7_dependent_untrack_retrack" {
		t.Fatalf("report.json does not name the failed phase: %v", summary["failed_phases"])
	}
	if _, ok := summary["retained_census"]; !ok {
		t.Error("report.json lost the retained census")
	}
}

func w8ReadJSON(t *testing.T, path string, into any) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, into); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
}

// w8FailingPhaseDirEnv switches TestW8ExecuteFilesTheReportOfAFailingPhase into
// its child role. The child deliberately fails, so it has to run in its own
// process: a phase failure is a runtime.Goexit on the test goroutine, and the
// only way to assert what survives it is to let it happen for real.
const w8FailingPhaseDirEnv = "GXW8_INTERNAL_FAILING_PHASE_DIR"

// TestW8ExecuteFilesTheReportOfAFailingPhase proves the wiring, not just the
// primitive: execute's own failure path must reach closePhase. Before it did,
// a phase's t.Fatal unwound straight out of the run and everything the failing
// phase measured — and every earlier phase's totals, which only report.json
// carries — was lost with it.
func TestW8ExecuteFilesTheReportOfAFailingPhase(t *testing.T) {
	if dir := os.Getenv(w8FailingPhaseDirEnv); dir != "" {
		w8RunFailingPhaseChild(t, dir)
		return
	}
	dir := t.TempDir()
	child := exec.Command(os.Args[0], "-test.run=^TestW8ExecuteFilesTheReportOfAFailingPhase$", "-test.v=true")
	child.Env = append(os.Environ(), w8FailingPhaseDirEnv+"="+dir)
	output, err := child.CombinedOutput()
	if err == nil {
		t.Fatalf("the child test was supposed to fail its phase:\n%s", output)
	}
	if !strings.Contains(string(output), "deliberate phase failure") {
		t.Fatalf("the child failed for the wrong reason:\n%s", output)
	}
	var filed w8PhaseReport
	w8ReadJSON(t, filepath.Join(dir, "phase_PX_failing.json"), &filed)
	if !filed.Failed {
		t.Error("the failing phase's report does not record the failure")
	}
	if filed.Phase != "PX_failing" || filed.Detail == "" {
		t.Errorf("the failing phase's report lost its identity: %+v", filed)
	}
	if filed.WallSeconds <= 0 {
		t.Error("the failing phase's report carries no wall time")
	}
	if _, err := os.Stat(filepath.Join(dir, "diagnostics_PX_failing.json")); err != nil {
		t.Errorf("the failure diagnostics were not written: %v", err)
	}
	// The run-level artifact is the half that carries every EARLIER phase's
	// totals; a failing last phase must not take it down.
	var summary map[string]any
	w8ReadJSON(t, filepath.Join(dir, "report.json"), &summary)
	failed, _ := summary["failed_phases"].([]any)
	if len(failed) != 1 || failed[0] != "PX_failing" {
		t.Fatalf("report.json does not name the failed phase: %v", summary["failed_phases"])
	}
	if phases, _ := summary["phases"].([]any); len(phases) != 1 {
		t.Fatalf("report.json carries %d phases, want the one that ran", len(phases))
	}
}

func w8RunFailingPhaseChild(t *testing.T, dir string) {
	root := t.TempDir()
	f := &issue767Fixture{t: t, binary: filepath.Join(root, "no-such-gortex"), root: root, primary: filepath.Join(root, "repo"), store: filepath.Join(root, "store.sqlite")}
	if err := os.MkdirAll(f.primary, 0o700); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(f.store))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	db.SetMaxOpenConns(1)
	run := &w8Run{t: t, f: f, arm: "candidate", artifactDir: dir, db: db, sampler: newW8Sampler(io.Discard, time.Hour)}
	w8WithRunArtifacts(run, w8Manifest{RunID: "failing-phase-child"}, func() {
		run.execute(w8Phase{
			Name:   "PX_failing",
			Detail: "a phase that fails after doing measurable work",
			Run: func(r *w8Run) {
				r.note("the note the failing phase left")
				time.Sleep(10 * time.Millisecond)
				r.t.Fatal("deliberate phase failure")
			},
		})
	})
	t.Fatal("execute returned from a phase that called t.Fatal")
}
