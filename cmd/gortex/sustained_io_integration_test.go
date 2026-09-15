package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
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

	"github.com/zzet/gortex/internal/testdsn"
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
// It is opt-in (GX_SUSTAINED_IO_TEST_BINARY) and never runs in a default `go test ./...`.
// The instrument itself — generator, sampler, checkpoint counter, manifest,
// attribution, phase plan, timeout arithmetic — is unit-tested in
// sustained_io_fixture_generator_test.go, sustained_io_sampler_test.go and at the bottom of this
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
	sustainedIOAdvanceMarkerFormat  = "GxAdvance%03dMarker"
	sustainedIODependentMarkerForm  = "GxDependent%02dMarker"
	sustainedIODefaultProbeTimeout  = 3 * time.Minute
	sustainedIOPhaseCommandTimeout  = 30 * time.Second
	sustainedIOStatusScrapeTimeout  = 30 * time.Second
	sustainedIOMinimumFixtureFiles  = 10
	sustainedIOMaximumFixtureFiles  = 20000
	sustainedIOCommitFilesPerCommit = 3
)

// errSustainedIONoChild is the "there is no daemon process to read" case, which is not
// a measurement failure: a phase that starts the child has no earlier process
// to take a baseline from, and the child's own counters start at zero.
var errSustainedIONoChild = errors.New("no running child")

// sustainedIOProcessWindow is one phase's process-accounted cost.
type sustainedIOProcessWindow struct {
	OK            bool
	Logical       *uint64
	DiskWritten   uint64
	DiskRead      uint64
	CPUUserNS     uint64
	CPUSystemNS   uint64
	PhysFootprint uint64
	Notes         []string
}

// sustainedIOProcessDelta turns a phase's two rusage reads into a delta, and says in
// words why it could not when it could not.
//
// The one case that looks like a failure but is not: the phase that starts the
// daemon has no process to read at its start. The child's counters begin at
// zero, so the end read IS the delta — that is how the cold-index phase gets a
// write series at all instead of an "unavailable".
func sustainedIOProcessDelta(before issue767ProcessIO, beforeErr error, after issue767ProcessIO, afterErr error) sustainedIOProcessWindow {
	window := sustainedIOProcessWindow{}
	if afterErr != nil {
		window.Notes = append(window.Notes, "process I/O unavailable at phase end: "+afterErr.Error())
		return window
	}
	lifetime := false
	if beforeErr != nil {
		if !errors.Is(beforeErr, errSustainedIONoChild) {
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
	window.DiskWritten = sustainedIODelta(before.BytesWritten, after.BytesWritten)
	window.DiskRead = sustainedIODelta(before.BytesRead, after.BytesRead)
	window.CPUUserNS = sustainedIODelta(before.UserTimeNS, after.UserTimeNS)
	window.CPUSystemNS = sustainedIODelta(before.SystemTimeNS, after.SystemTimeNS)
	window.PhysFootprint = after.PhysFootprint
	if after.LogicalBytesWritten != nil {
		start := uint64(0)
		if before.LogicalBytesWritten != nil {
			start = *before.LogicalBytesWritten
		} else if !lifetime {
			return window
		}
		delta := sustainedIODelta(start, *after.LogicalBytesWritten)
		window.Logical = &delta
	}
	return window
}

// sustainedIODiagnostics is what the harness collects when a phase fails. A bare
// "timed out waiting for symbol X" tells the next reader nothing about why the
// view never became exact; the daemon's own status, the family census and the
// per-path route explanation do.
type sustainedIODiagnostics struct {
	Phase    string            `json:"phase"`
	Commands map[string]string `json:"commands"`
	LogTail  string            `json:"daemon_log_tail,omitempty"`
	// Isolation carries every isolation answer this phase judged, in full. A
	// phase that failed on an isolation question is unreadable without the
	// answer it failed on.
	Isolation []sustainedIOIsolationRecord `json:"isolation,omitempty"`
}

// sustainedIOCollectDiagnostics runs the explain-the-view command set. run must never
// fail the test: this executes while a failure is already unwinding.
func sustainedIOCollectDiagnostics(phase string, run func(args ...string) string, logTail string, paths []string, isolation []sustainedIOIsolationRecord) sustainedIODiagnostics {
	diagnostics := sustainedIODiagnostics{Phase: phase, Commands: map[string]string{}, LogTail: logTail, Isolation: isolation}
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

// sustainedIOConfig is every knob the workload takes.
type sustainedIOConfig struct {
	Fixture        sustainedIOFixtureSpec
	Worktrees      int
	Commits        int
	Edits          int
	EditInterval   time.Duration
	Idle           time.Duration
	SampleInterval time.Duration
	Repetitions    int
	IdleBudget     uint64 // 0 disables the idle assertion
	// ReconcileInterval is GORTEX_RECONCILE_INTERVAL for the measured daemon.
	// The paired protocol runs at 5s — 720x the product's own default — and
	// every artifact taken under it says so. Making it a knob is what makes
	// the confirmatory arm ("the same workload at the interval the product
	// ships") runnable at all: issue767ProductReconcileInterval leaves the
	// variable unset.
	ReconcileInterval string
}

// sustainedIODefaultReconcileInterval is the paired protocol's janitor interval, kept
// as the default so an unset environment reproduces the frozen arms.
const sustainedIODefaultReconcileInterval = issue767DefaultTestReconcileInterval

// sustainedIOValidateReconcileInterval accepts the product default by name and any
// duration inside the range a measurement can survive. It refuses rather than
// clamps: a run whose janitor is not what the operator typed is not
// reproducible from its own manifest.
func sustainedIOValidateReconcileInterval(raw string) (string, error) {
	switch raw {
	case "":
		return sustainedIODefaultReconcileInterval, nil
	case issue767ProductReconcileInterval, "product":
		return issue767ProductReconcileInterval, nil
	}
	value, err := time.ParseDuration(raw)
	if err != nil {
		return "", fmt.Errorf("GX_SUSTAINED_IO_RECONCILE_INTERVAL: %w", err)
	}
	if value < time.Second || value > 24*time.Hour {
		return "", fmt.Errorf("GX_SUSTAINED_IO_RECONCILE_INTERVAL=%s is outside [1s,24h]", raw)
	}
	return raw, nil
}

// sustainedIOConfigFromEnv reads the GX_SUSTAINED_IO_* knobs. Bounds are refused by name rather
// than clamped silently: a run whose parameters are not what the operator typed
// is not reproducible from its own manifest.
func sustainedIOConfigFromEnv(lookup func(string) string) (sustainedIOConfig, error) {
	cfg := sustainedIOConfig{
		Fixture:           sustainedIOFixtureSpec{}.normalize(),
		Worktrees:         10,
		Commits:           20,
		Edits:             10,
		EditInterval:      30 * time.Second,
		Idle:              60 * time.Second,
		SampleInterval:    time.Second,
		Repetitions:       1,
		ReconcileInterval: sustainedIODefaultReconcileInterval,
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
		intKnob("GX_SUSTAINED_IO_FIXTURE_FILES", &cfg.Fixture.Files, sustainedIOMinimumFixtureFiles, sustainedIOMaximumFixtureFiles),
		intKnob("GX_SUSTAINED_IO_FIXTURE_PACKAGES", &cfg.Fixture.Packages, 1, 1000),
		intKnob("GX_SUSTAINED_IO_FIXTURE_SEED", &seed, 1, 1<<30),
		intKnob("GX_SUSTAINED_IO_WORKTREES", &cfg.Worktrees, 0, 50),
		intKnob("GX_SUSTAINED_IO_COMMITS", &cfg.Commits, 0, 200),
		intKnob("GX_SUSTAINED_IO_EDITS", &cfg.Edits, 0, 200),
		intKnob("GX_SUSTAINED_IO_REPS", &cfg.Repetitions, 1, 10),
		durationKnob("GX_SUSTAINED_IO_EDIT_INTERVAL", &cfg.EditInterval, 0, 10*time.Minute),
		durationKnob("GX_SUSTAINED_IO_IDLE", &cfg.Idle, time.Second, time.Hour),
		durationKnob("GX_SUSTAINED_IO_SAMPLE_INTERVAL", &cfg.SampleInterval, 100*time.Millisecond, time.Minute),
	} {
		if err != nil {
			return cfg, err
		}
	}
	cfg.Fixture.Seed = int64(seed)
	interval, err := sustainedIOValidateReconcileInterval(lookup("GX_SUSTAINED_IO_RECONCILE_INTERVAL"))
	if err != nil {
		return cfg, err
	}
	cfg.ReconcileInterval = interval
	if raw := lookup("GX_SUSTAINED_IO_IDLE_BUDGET_BYTES"); raw != "" {
		budget, err := strconv.ParseUint(raw, 10, 64)
		if err != nil {
			return cfg, fmt.Errorf("GX_SUSTAINED_IO_IDLE_BUDGET_BYTES: %w", err)
		}
		cfg.IdleBudget = budget
	}
	cfg.Fixture = cfg.Fixture.normalize()
	return cfg, nil
}

// sustainedIOReconcileNote states the janitor setting the run was taken under, in the
// manifest, in the operator's words. The accelerated interval is a measurement
// choice and has to travel with every number it produced; the product default
// is equally a choice and says so too.
func sustainedIOReconcileNote(interval string) string {
	if interval == issue767ProductReconcileInterval {
		return "GORTEX_RECONCILE_INTERVAL is unset: the janitor runs at the product default; this is the confirmatory configuration"
	}
	return "GORTEX_RECONCILE_INTERVAL=" + interval + " accelerates the janitor (the product default is 1h); these are not default-configuration numbers"
}

// sustainedIORequiredTimeout is a floor on `go test -timeout` for one invocation. It is
// deliberately generous: the failure mode of an under-budgeted run is a killed
// child daemon and a wasted hour, not a smaller number.
func sustainedIORequiredTimeout(cfg sustainedIOConfig, arms int) time.Duration {
	if arms < 1 {
		arms = 1
	}
	perArm := 4*cfg.Idle + // cold and warm idle, each measured quiet and polling
		time.Duration(cfg.Edits)*(cfg.EditInterval+30*time.Second) +
		time.Duration(cfg.Worktrees)*time.Minute +
		time.Duration(cfg.Commits)*time.Duration(1+cfg.Worktrees)*10*time.Second +
		10*time.Minute // cold index, settles, censuses, teardown
	return time.Duration(arms*cfg.Repetitions) * perArm
}

// The sub-window names, in one place. Phase NAMES are deliberately unchanged:
// the frozen budgets.json of the paired protocol is keyed by phase name, and a
// renamed or re-ordered phase orphans its frozen ceiling — which the post-fix
// re-measurement is supposed to be judged against. What the phases gain is
// named sub-windows, each with its own row, so a phase's own stimulus is
// separable from the thing the phase is named after without moving a ceiling.
// The a/b suffix is the order the window is measured in, not a label chosen
// once: the polling arm of an idle phase runs FIRST (see idleWindows), so it
// is the `a` window and the quiet arm is the `b` window.
const (
	sustainedIOWindowCommitTreeChange = "P4a_commit_tree_change"
	sustainedIOWindowAmendSameTree    = "P4b_amend_same_tree"
	sustainedIOWindowIdleColdPolling  = "P1a_idle_cold_polling"
	sustainedIOWindowIdleColdQuiet    = "P1b_idle_cold_quiet"
	sustainedIOWindowIdleWarmPolling  = "P8a_idle_warm_polling"
	sustainedIOWindowIdleWarmQuiet    = "P8b_idle_warm_quiet"
)

// sustainedIOWindowPlan is the declared sub-window plan, phase by phase, so the split
// is pinned by a test rather than discovered by reading three phase bodies.
// The slice order is the measurement order.
func sustainedIOWindowPlan() map[string][]string {
	return map[string][]string{
		"P1_idle_cold":       {sustainedIOWindowIdleColdPolling, sustainedIOWindowIdleColdQuiet},
		"P4_amend_same_tree": {sustainedIOWindowCommitTreeChange, sustainedIOWindowAmendSameTree},
		"P8_idle_warm":       {sustainedIOWindowIdleWarmPolling, sustainedIOWindowIdleWarmQuiet},
	}
}

// sustainedIOPhase is one step of the workload. Detail is what the phase did, in the
// operator's vocabulary, and it lands in the artifact next to the numbers.
type sustainedIOPhase struct {
	Name   string
	Detail string
	Run    func(r *sustainedIORun)
}

// sustainedIOPhasePlan is the nine-phase workload, in order.
func sustainedIOPhasePlan(cfg sustainedIOConfig) []sustainedIOPhase {
	return []sustainedIOPhase{
		{
			Name:   "P0_cold_index",
			Detail: fmt.Sprintf("cold index of %d generated files in %d packages, to the first exact primary answer", cfg.Fixture.Files, cfg.Fixture.Packages),
			Run:    (*sustainedIORun).phaseColdIndex,
		},
		{
			Name: "P1_idle_cold",
			Detail: fmt.Sprintf("idle on the freshly indexed store, measured twice: %s polling one read-only search every %s first (%s, the arm the frozen ceiling is applied to, opening where the frozen window opened) then %s quiet (%s, un-budgeted)",
				cfg.Idle, sustainedIOIdlePollInterval, sustainedIOWindowIdleColdPolling, cfg.Idle, sustainedIOWindowIdleColdQuiet),
			Run: (*sustainedIORun).phaseIdleCold,
		},
		{
			Name:   "P2_small_edits",
			Detail: fmt.Sprintf("%d edits %s apart, rotating over the corpus; each rewrites one function body and renames a one-line revision stub (the only declaration that moves, and the only thing a symbol search can wait on), awaited to exact from the edited file", cfg.Edits, cfg.EditInterval),
			Run:    (*sustainedIORun).phaseSmallEdits,
		},
		{
			Name:   "P3_touch_stage_unstage",
			Detail: "touch with identical bytes, then git add -A, then git reset",
			Run:    (*sustainedIORun).phaseTouchStageUnstage,
		},
		{
			Name:   "P4_amend_same_tree",
			Detail: "the tree-changing commit the amend needs (" + sustainedIOWindowCommitTreeChange + "), then git commit --amend --no-edit: a new commit id over an unchanged tree (" + sustainedIOWindowAmendSameTree + ")",
			Run:    (*sustainedIORun).phaseAmendSameTree,
		},
		{
			Name:   "P5_main_advance",
			Detail: fmt.Sprintf("%d dependent worktrees discovered (never tracked), then %d commits on main touching %d files each, each awaited to exact on the primary and on every dependent", cfg.Worktrees, cfg.Commits, sustainedIOCommitFilesPerCommit),
			Run:    (*sustainedIORun).phaseMainAdvance,
		},
		{
			Name:   "P6_dependent_edits",
			Detail: "dirty edits in the dependent worktrees, awaited to exact, with primary isolation rechecked",
			Run:    (*sustainedIORun).phaseDependentEdits,
		},
		{
			Name:   "P7_dependent_untrack_retrack",
			Detail: "explicitly track one dependent, then untrack it back to automatic discovery",
			Run:    (*sustainedIORun).phaseDependentUntrackRetrack,
		},
		{
			Name: "P8_idle_warm",
			Detail: fmt.Sprintf("idle on the worked store, measured twice: %s polling one read-only search every %s first (%s, the arm the frozen ceiling is applied to, opening where the frozen window opened) then %s quiet (%s, un-budgeted)",
				cfg.Idle, sustainedIOIdlePollInterval, sustainedIOWindowIdleWarmPolling, cfg.Idle, sustainedIOWindowIdleWarmQuiet),
			Run: (*sustainedIORun).phaseIdleWarm,
		},
	}
}

// sustainedIOPhaseReport is one phase's evidence.
type sustainedIOPhaseReport struct {
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

	StoreCensusBefore sustainedIOStoreCensus `json:"store_census_before"`
	StoreCensusAfter  sustainedIOStoreCensus `json:"store_census_after"`

	CountersDelta map[string]int64 `json:"viewmetrics_delta,omitempty"`
	CountersError string           `json:"viewmetrics_error,omitempty"`

	CensusAfter sustainedIOCensus `json:"destination_census_after"`

	ExactnessWaits   int      `json:"exactness_waits"`
	ExactnessSeconds float64  `json:"exactness_wait_s"`
	Notes            []string `json:"notes,omitempty"`

	// CheckpointBytes is the part of LogicalWrites this phase's samples booked
	// to a WAL checkpoint restarting the log, and LogicalWritesExcl is what is
	// left. A checkpoint drains frames an earlier phase wrote: charging them
	// to whichever window happened to cross the autocheckpoint threshold is
	// what turned a 0.91x edit path into a 3.44x regression in the frozen
	// verdict.
	CheckpointBytes   uint64  `json:"wal_checkpoint_bytes"`
	LogicalWritesExcl *uint64 `json:"ri_logical_writes_delta_excl_checkpoint,omitempty"`

	// ClientCalls is how many times the harness invoked the CLI inside this
	// phase. An idle phase with twelve of them measures a poller, not a floor.
	ClientCalls int `json:"client_calls"`

	// CensusDelta is the per-writer change of the destination census across
	// the phase, so the sidecar and the query log are attributable by name
	// rather than inferred from a total.
	CensusDelta map[string]int64 `json:"destination_census_delta,omitempty"`

	// Windows are the named sub-windows this phase cut itself into.
	Windows []sustainedIOWindowReport `json:"windows,omitempty"`

	// Isolation records every isolation probe the phase judged, in full.
	Isolation []sustainedIOIsolationRecord `json:"isolation,omitempty"`
}

// sustainedIOWindowReport is one named sub-window of a phase: the same envelope, over a
// smaller bracket.
//
// A phase that performs its own stimulus has to say where the stimulus was.
// P4's frozen row reads "270x on a same-tree amend"; the bytes are a ten-file
// `git add -A && git commit` the phase runs first so the amend has a clean
// tree to amend over. The commit is real work and belongs in the artifact —
// under its own name, not under the amend's.
type sustainedIOWindowReport struct {
	Phase             string           `json:"phase"`
	Window            string           `json:"window"`
	Detail            string           `json:"detail,omitempty"`
	Failed            bool             `json:"failed,omitempty"`
	WallSeconds       float64          `json:"wall_s"`
	LogicalWrites     *uint64          `json:"ri_logical_writes_delta,omitempty"`
	LogicalWritesExcl *uint64          `json:"ri_logical_writes_delta_excl_checkpoint,omitempty"`
	CheckpointBytes   uint64           `json:"wal_checkpoint_bytes"`
	DiskWritten       uint64           `json:"ri_diskio_byteswritten_delta"`
	StoreBytesBefore  int64            `json:"store_bytes_before"`
	StoreBytesAfter   int64            `json:"store_bytes_after"`
	WALBefore         int64            `json:"wal_bytes_before"`
	WALAfter          int64            `json:"wal_bytes_after"`
	WALResets         int              `json:"wal_resets"`
	Samples           int              `json:"samples"`
	ClientCalls       int              `json:"client_calls"`
	CountersDelta     map[string]int64 `json:"viewmetrics_delta,omitempty"`
	CountersError     string           `json:"viewmetrics_error,omitempty"`
	CensusDelta       map[string]int64 `json:"destination_census_delta,omitempty"`
	ExactnessWaits    int              `json:"exactness_waits"`
	ExactnessSeconds  float64          `json:"exactness_wait_s"`
	Notes             []string         `json:"notes,omitempty"`
}

// sustainedIOIsolationRecord is one isolation question and the whole answer it was
// judged on — spelling, found, exact label, fallback label, source file,
// answering corpus, error — plus how long the harness waited for an answer it
// could judge at all.
type sustainedIOIsolationRecord struct {
	Subject string         `json:"subject"`
	Probe   issue767Probe  `json:"probe"`
	Answer  issue767Answer `json:"answer"`
	Outcome string         `json:"outcome"`
	Leaked  bool           `json:"leaked"`
	Judged  bool           `json:"judged"`
}

// sustainedIOStoreCensus is the SQL side of a phase boundary. Every sub-query is
// tolerated individually: a baseline binary on an older schema must still
// produce a report, with the missing series named rather than zeroed.
type sustainedIOStoreCensus struct {
	PageSize      int64                             `json:"page_size"`
	PageCount     int64                             `json:"page_count"`
	FreelistCount int64                             `json:"freelist_count"`
	Generations   issue767GenerationSnapshot        `json:"generations"`
	ByState       map[string]sustainedIOStateCensus `json:"generations_by_state,omitempty"`
	Checkouts     int64                             `json:"checkouts"`
	Routes        int64                             `json:"checkout_routes"`
	Errors        []string                          `json:"errors,omitempty"`
}

type sustainedIOStateCensus struct {
	Count        int64 `json:"count"`
	StorageBytes int64 `json:"storage_bytes"`
	Covered      int64 `json:"covered_files"`
	Affected     int64 `json:"affected_files"`
}

func sustainedIOReadStoreCensus(ctx context.Context, db *sql.DB) sustainedIOStoreCensus {
	census := sustainedIOStoreCensus{ByState: map[string]sustainedIOStateCensus{}}
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
		var entry sustainedIOStateCensus
		if err := rows.Scan(&state, &entry.Count, &entry.StorageBytes, &entry.Covered, &entry.Affected); err != nil {
			note(err)
			break
		}
		census.ByState[state] = entry
	}
	note(rows.Err())
	return census
}

// sustainedIORun is one arm of one repetition.
type sustainedIORun struct {
	t           *testing.T
	f           *issue767Fixture
	cfg         sustainedIOConfig
	arm         string
	artifactDir string
	db          *sql.DB
	sampler     *sustainedIOSampler
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

	// Sub-window state, reset by execute at every phase boundary.
	phase         string
	windowWaits   int
	windowSeconds float64
	windowNotes   []string
	windowOpen    bool
	windows       []sustainedIOWindowReport

	isolation      []sustainedIOIsolationRecord
	phaseIsolation []sustainedIOIsolationRecord

	reports []sustainedIOPhaseReport
}

// TestSustainedIOSustainedWriteAmplification is the opt-in sustained-workload run.
func TestSustainedIOSustainedWriteAmplification(t *testing.T) {
	candidate := os.Getenv("GX_SUSTAINED_IO_TEST_BINARY")
	if candidate == "" {
		t.Skip("set GX_SUSTAINED_IO_TEST_BINARY to opt into the isolated sustained-workload I/O harness")
	}
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("process I/O sampler supports Darwin and Linux")
	}
	cfg, err := sustainedIOConfigFromEnv(os.Getenv)
	if err != nil {
		t.Fatal(err)
	}
	arms := []struct{ name, binary string }{}
	if baseline := os.Getenv("GX_SUSTAINED_IO_BASELINE_BINARY"); baseline != "" {
		arms = append(arms, struct{ name, binary string }{"baseline", baseline})
	}
	arms = append(arms, struct{ name, binary string }{"candidate", candidate})
	required := sustainedIORequiredTimeout(cfg, len(arms))
	if deadline, ok := t.Deadline(); ok && time.Until(deadline) < required {
		t.Fatalf("sustained workload needs at least %s remaining; run with go test -timeout %s or longer", required, required.Round(time.Minute))
	}
	artifactRoot := os.Getenv("GX_SUSTAINED_IO_ARTIFACT_DIR")
	if artifactRoot == "" {
		artifactRoot = t.TempDir()
		t.Logf("GX_SUSTAINED_IO_ARTIFACT_DIR is unset; artifacts go to %s and are removed with the test", artifactRoot)
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
				sustainedIORunWorkload(t, cfg, arm.name, binary, filepath.Join(artifactRoot, name))
			})
		}
	}
}

func sustainedIORunWorkload(t *testing.T, cfg sustainedIOConfig, arm, binary, artifactDir string) {
	t.Helper()
	if err := os.MkdirAll(artifactDir, 0o700); err != nil {
		t.Fatal(err)
	}
	fixture := sustainedIOGenerateFixture(cfg.Fixture)
	f := newIssue767FixtureWithCorpus(t, binary, func(f *issue767Fixture) {
		for _, file := range fixture {
			f.write(filepath.Join(f.primary, filepath.FromSlash(file.Path)), file.Content)
		}
	}, issue767WithReconcileInterval(cfg.ReconcileInterval))
	samplesPath, err := sustainedIOArtifactPath(artifactDir, f.root, "samples.ndjson")
	if err != nil {
		t.Fatal(err)
	}
	samples, err := os.Create(samplesPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = samples.Close() }()

	manifest := sustainedIOBuildManifest(sustainedIOManifest{
		RunID:       filepath.Base(artifactDir),
		Arm:         arm,
		Fixture:     cfg.Fixture,
		Worktrees:   cfg.Worktrees,
		Commits:     cfg.Commits,
		Edits:       cfg.Edits,
		Repetitions: cfg.Repetitions,
		Cold:        true,
		Notes: []string{
			sustainedIOReconcileNote(cfg.ReconcileInterval),
			"ri_logical_writes is the primary series; ri_diskio_byteswritten is reported beside it and never alone",
			"process-accounted writes are not SSD NAND writes; a current WAL size is not cumulative writes",
			"wal_resets is a lower bound on checkpoints: a PASSIVE checkpoint that does not restart the log is invisible",
		},
	}, binary, f.env, fixture)
	if output, err := f.tryCommand(sustainedIOPhaseCommandTimeout, f.root, "version", "--short"); err == nil {
		manifest.BinaryVersion = strings.TrimSpace(string(output))
	} else {
		manifest.Notes = append(manifest.Notes, "binary version unavailable: "+err.Error())
	}
	var identityNotes []string
	manifest.SourceCommit, manifest.DirtyDigest, identityNotes = sustainedIOSourceIdentity()
	manifest.Notes = append(manifest.Notes, identityNotes...)
	if manifest.SourceCommit == "" {
		t.Logf("manifest carries no source commit: %v", identityNotes)
	}
	sustainedIOWriteJSON(t, filepath.Join(artifactDir, "manifest.json"), manifest)

	run := &sustainedIORun{
		t: t, f: f, cfg: cfg, arm: arm, artifactDir: artifactDir,
		samples:   samples,
		revisions: map[int]int{},
		rotation:  sustainedIORotationTargets(cfg.Fixture, max(cfg.Edits, 1)+sustainedIOCommitFilesPerCommit),
		lastProbe: sustainedIOPrimaryMarker,
	}
	run.db = f.openReadOnly()
	run.sampler = sustainedIONewRunSampler(f, samples, cfg.SampleInterval)

	ctx, cancel := context.WithCancel(context.Background())
	sampling := make(chan struct{})
	go func() { run.sampler.Run(ctx); close(sampling) }()
	stopSampling := sync.OnceFunc(func() {
		cancel()
		<-sampling
	})
	defer stopSampling()
	// The phases run inside sustainedIOWithRunArtifacts, which files the run-level
	// artifact whether they finish or unwind: a phase's t.Fatal is a
	// runtime.Goexit, and a run whose last phase failed must still keep
	// report.json, the retained census and the totals of every phase that did
	// run — and must still remove the worktrees it created.
	sustainedIOWithRunArtifacts(run, manifest, func() {
		defer stopSampling()
		for _, phase := range sustainedIOPhasePlan(cfg) {
			run.execute(phase)
		}
	})
}

// sustainedIOWithRunArtifacts runs the phases and files the run-level artifact
// afterwards, on both exits.
func sustainedIOWithRunArtifacts(run *sustainedIORun, manifest sustainedIOManifest, phases func()) {
	defer run.finish(manifest)
	phases()
}

// sustainedIONewRunSampler is the production wiring between a fixture and the 1 Hz
// sampler: the child's process counters, the store/WAL/SHM/log sizes and the
// WAL header. Every fixture field it reads goes through a guarded accessor,
// because this sampler runs on its own goroutine across the fixture's
// start/stop.
func sustainedIONewRunSampler(f *issue767Fixture, out io.Writer, interval time.Duration) *sustainedIOSampler {
	sampler := newSustainedIOSampler(out, interval)
	sampler.pid = f.pid
	sampler.readIO = func() (issue767ProcessIO, error) {
		pid := f.pid()
		if pid == 0 {
			return issue767ProcessIO{}, errSustainedIONoChild
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return issue767ReadProcessIO(ctx, pid)
	}
	sampler.readSize = func() sustainedIOFileSizes {
		return sustainedIOFileSizes{
			Store: issue767FileSize(f.store),
			WAL:   issue767FileSize(f.store + "-wal"),
			SHM:   issue767FileSize(f.store + "-shm"),
			Log:   issue767FileSize(f.logPath()),
		}
	}
	sampler.readWAL = func() (sustainedIOWALHeader, error) { return sustainedIOReadWALHeader(f.store + "-wal") }
	return sampler
}

// sustainedIOPhaseOpening is everything execute reads before a phase runs. It is a
// value, not fields on the run, so the closing half can be reached from the
// success path and from the failure unwind with the same arguments.
type sustainedIOPhaseOpening struct {
	started     time.Time
	io          issue767ProcessIO
	ioErr       error
	counters    map[string]int64
	countersErr error
	samples     int
	failures    int
	resets      int
	checkpoint  uint64
	clientCalls int
	storeBytes  int64
	walBytes    int64
	census      sustainedIOCensus
	// scraped says whether this bracket took a `daemon status` scrape. A
	// scrape is a client call: taking it before the clock keeps it out of the
	// bracket it opens, but it is still inside every bracket that ENCLOSES it.
	// A sub-window of an idle phase therefore opens a scrape-free bracket —
	// see openBracketScraping.
	scraped bool
}

// openBracket takes the opening half of a measurement envelope — a phase's or a
// sub-window's, they are the same envelope over different spans.
//
// The order is the measurement: the daemon-status scrape is a client call that
// costs the daemon a write, so it is taken BEFORE the clock starts and before
// the process counters are read. Taking it after (which is what the harness
// did) put one `daemon status` round trip inside every phase's own window and
// charged the phase for it.
func (r *sustainedIORun) openBracket() sustainedIOPhaseOpening {
	return r.openBracketScraping(true)
}

// openBracketScraping is openBracket with the viewmetrics scrape made optional.
//
// Moving a scrape before the clock keeps it out of the bracket it opens, but an
// interior bracket's scrapes still land inside the enclosing phase. Splitting
// an idle phase into a quiet arm and a polling arm added four `daemon status`
// round trips inside a phase whose whole subject is what the daemon writes when
// nobody asks it anything — and the frozen baseline, whose ceiling those phases
// are judged against, had none of them. So an idle sub-window opens a
// scrape-free bracket: the counter delta for the idle phase is still taken by
// the PHASE bracket, whose own scrapes sit outside the phase's measured span.
func (r *sustainedIORun) openBracketScraping(scrape bool) sustainedIOPhaseOpening {
	opening := sustainedIOPhaseOpening{scraped: scrape}
	if scrape {
		opening.counters, opening.countersErr = r.counters()
	}
	if census, err := sustainedIOWalkCensus(r.f.root); err == nil {
		opening.census = census
	}
	opening.storeBytes = issue767FileSize(r.f.store)
	opening.walBytes = issue767FileSize(r.f.store + "-wal")
	opening.clientCalls = r.f.clientCallCount()
	opening.started = time.Now()
	opening.io, opening.ioErr = r.processIO()
	opening.samples, opening.failures = r.sampler.Samples()
	opening.resets = r.sampler.Resets()
	opening.checkpoint = r.sampler.CheckpointBytes()
	return opening
}

// sustainedIOExcludeCheckpoint is the checkpoint-excluded write series: the window's
// logical writes less the part its own samples booked to a WAL checkpoint.
// It is nil exactly when the total is, and it never goes below zero — the
// sample-interval attribution is coarse enough that a checkpoint interval can
// carry more bytes than the bracket's own delta when the bracket is shorter
// than one sample.
func sustainedIOExcludeCheckpoint(total *uint64, checkpoint uint64) *uint64 {
	if total == nil {
		return nil
	}
	excluded := uint64(0)
	if *total > checkpoint {
		excluded = *total - checkpoint
	}
	return &excluded
}

// sustainedIOCensusDelta is the per-writer change between two destination censuses.
func sustainedIOCensusDelta(before, after sustainedIOCensus) map[string]int64 {
	delta := map[string]int64{}
	for bucket, bytes := range after.Bytes {
		if d := bytes - before.Bytes[bucket]; d != 0 {
			delta[bucket] = d
		}
	}
	for bucket, bytes := range before.Bytes {
		if _, ok := after.Bytes[bucket]; !ok && bytes != 0 {
			delta[bucket] = -bytes
		}
	}
	if len(delta) == 0 {
		return nil
	}
	return delta
}

// inWindow measures one named sub-window of the running phase. Like a phase it
// files its row on both exits: a t.Fatal inside body is a runtime.Goexit, and
// a window whose work failed is exactly the window a reader wants the numbers
// of.
func (r *sustainedIORun) inWindow(name, detail string, body func()) {
	r.t.Helper()
	r.inWindowScraping(name, detail, true, body)
}

// inScrapeFreeWindow measures one sub-window without the bracket's own
// `daemon status` round trips. It is what an idle arm has to use: the bracket's
// scrape would be client traffic inside the phase the frozen ceiling measured
// without any, and the quiet arm's whole claim is that it made no client calls
// at all.
func (r *sustainedIORun) inScrapeFreeWindow(name, detail string, body func()) {
	r.t.Helper()
	r.inWindowScraping(name, detail, false, body)
}

func (r *sustainedIORun) inWindowScraping(name, detail string, scrape bool, body func()) {
	r.t.Helper()
	r.sampler.SetWindow(name)
	r.windowWaits, r.windowSeconds, r.windowNotes = 0, 0, nil
	r.windowOpen = true
	opening := r.openBracketScraping(scrape)
	report := sustainedIOWindowReport{Phase: r.phase, Window: name, Detail: detail,
		StoreBytesBefore: opening.storeBytes, WALBefore: opening.walBytes}
	if !scrape {
		report.CountersError = sustainedIONoScrapeInsideWindow
	}
	completed := false
	defer func() {
		report.Failed = !completed
		r.closeWindow(&report, opening)
	}()
	body()
	completed = true
}

// closeWindow takes the closing half of a sub-window's envelope. Like
// closePhase it never calls t.Fatal: it runs during a failure unwind.
func (r *sustainedIORun) closeWindow(report *sustainedIOWindowReport, opening sustainedIOPhaseOpening) {
	report.WallSeconds = time.Since(opening.started).Seconds()
	afterIO, afterErr := r.processIO()
	window := sustainedIOProcessDelta(opening.io, opening.ioErr, afterIO, afterErr)
	report.Notes = append(report.Notes, window.Notes...)
	report.LogicalWrites = window.Logical
	report.DiskWritten = window.DiskWritten
	report.StoreBytesAfter = issue767FileSize(r.f.store)
	report.WALAfter = issue767FileSize(r.f.store + "-wal")
	samples, _ := r.sampler.Samples()
	report.Samples = samples - opening.samples
	report.WALResets = r.sampler.Resets() - opening.resets
	report.CheckpointBytes = r.sampler.CheckpointBytes() - opening.checkpoint
	report.LogicalWritesExcl = sustainedIOExcludeCheckpoint(report.LogicalWrites, report.CheckpointBytes)
	report.ClientCalls = r.f.clientCallCount() - opening.clientCalls
	report.ExactnessWaits, report.ExactnessSeconds = r.windowWaits, r.windowSeconds
	report.Notes = append(report.Notes, r.windowNotes...)
	// The closing scrape and census come last, so their own cost lands outside
	// the window they describe — and a scrape-free bracket takes no closing
	// scrape at all, because the enclosing phase would be charged for it.
	switch {
	case !opening.scraped:
		report.CountersError = sustainedIONoScrapeInsideWindow
	default:
		if afterCounters, err := r.counters(); err != nil {
			report.CountersError = strings.TrimSpace(opening.errorText() + " " + err.Error())
		} else if opening.countersErr == nil {
			report.CountersDelta = sustainedIOCounterDelta(opening.counters, afterCounters)
		} else {
			report.CountersError = opening.countersErr.Error()
		}
	}
	if census, err := sustainedIOWalkCensus(r.f.root); err == nil {
		report.CensusDelta = sustainedIOCensusDelta(opening.census, census)
	}
	r.windows = append(r.windows, *report)
	r.sampler.SetWindow("")
	r.windowOpen = false
	r.windowWaits, r.windowSeconds, r.windowNotes = 0, 0, nil
	logical := "unavailable"
	if report.LogicalWrites != nil {
		logical = strconv.FormatUint(*report.LogicalWrites, 10)
	}
	r.t.Logf("%s/%s/%s: failed=%v wall=%.1fs ri_logical_writes=%s checkpoint=%d client_calls=%d store=%d->%d",
		r.arm, report.Phase, report.Window, report.Failed, report.WallSeconds, logical,
		report.CheckpointBytes, report.ClientCalls, report.StoreBytesBefore, report.StoreBytesAfter)
}

func (o sustainedIOPhaseOpening) errorText() string {
	if o.countersErr == nil {
		return ""
	}
	return o.countersErr.Error()
}

// execute brackets one phase with the whole measurement envelope.
func (r *sustainedIORun) execute(phase sustainedIOPhase) {
	r.t.Helper()
	r.sampler.SetPhase(phase.Name)
	r.phase = phase.Name
	r.phaseWaits, r.phaseSeconds, r.phaseNotes = 0, 0, nil
	r.windows, r.phaseIsolation = nil, nil
	report := sustainedIOPhaseReport{Phase: phase.Name, Detail: phase.Detail}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	report.StoreCensusBefore = sustainedIOReadStoreCensus(ctx, r.db)
	cancel()
	report.LogBefore = r.daemonLogBytes()

	opening := r.openBracket()
	if opening.countersErr != nil {
		report.CountersError = opening.countersErr.Error()
	}
	report.StoreBytesBefore = opening.storeBytes
	report.WALBefore = opening.walBytes

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
func (r *sustainedIORun) closePhase(report *sustainedIOPhaseReport, opening sustainedIOPhaseOpening) {
	report.WallSeconds = time.Since(opening.started).Seconds()

	afterIO, afterErr := r.processIO()
	// Read the client-call count on the same side of the bracket as the
	// process counters: everything below this line — the store census, the
	// closing daemon-status scrape — is the harness measuring, and a phase
	// must not be charged for its own instrumentation in one series while the
	// other series excludes it.
	report.ClientCalls = r.f.clientCallCount() - opening.clientCalls
	window := sustainedIOProcessDelta(opening.io, opening.ioErr, afterIO, afterErr)
	report.Notes = append(report.Notes, window.Notes...)
	report.LogicalWrites = window.Logical
	report.DiskWritten, report.DiskRead = window.DiskWritten, window.DiskRead
	report.CPUUserNS, report.CPUSystemNS = window.CPUUserNS, window.CPUSystemNS
	report.PhysFootprint = window.PhysFootprint

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	report.StoreCensusAfter = sustainedIOReadStoreCensus(ctx, r.db)
	cancel()
	report.StoreBytesAfter = issue767FileSize(r.f.store)
	report.WALAfter = issue767FileSize(r.f.store + "-wal")
	report.LogAfter = r.daemonLogBytes()
	if afterCounters, err := r.counters(); err != nil {
		report.CountersError = strings.TrimSpace(report.CountersError + " " + err.Error())
	} else if opening.countersErr == nil {
		report.CountersDelta = sustainedIOCounterDelta(opening.counters, afterCounters)
	}
	if census, err := sustainedIOWalkCensus(r.f.root); err != nil {
		report.Notes = append(report.Notes, "destination census failed: "+err.Error())
	} else {
		report.CensusAfter = census
	}
	afterSamples, afterFailures := r.sampler.Samples()
	report.Samples = afterSamples - opening.samples
	report.SampleFailures = afterFailures - opening.failures
	report.WALResets = r.sampler.Resets() - opening.resets
	report.CheckpointBytes = r.sampler.CheckpointBytes() - opening.checkpoint
	report.LogicalWritesExcl = sustainedIOExcludeCheckpoint(report.LogicalWrites, report.CheckpointBytes)
	report.CensusDelta = sustainedIOCensusDelta(opening.census, report.CensusAfter)
	report.Windows = append(report.Windows, r.windows...)
	report.Isolation = append(report.Isolation, r.phaseIsolation...)
	report.ExactnessWaits, report.ExactnessSeconds = r.phaseWaits, r.phaseSeconds
	report.Notes = append(report.Notes, r.phaseNotes...)

	if !report.Failed {
		r.enforceIdleBudget(report)
	}
	r.reports = append(r.reports, *report)
	if err := sustainedIOTryWriteJSON(filepath.Join(r.artifactDir, "phase_"+report.Phase+".json"), *report); err != nil {
		r.t.Logf("phase artifact for %s not written: %v", report.Phase, err)
	}
	logical := "unavailable"
	if report.LogicalWrites != nil {
		logical = strconv.FormatUint(*report.LogicalWrites, 10)
	}
	excluded := "unavailable"
	if report.LogicalWritesExcl != nil {
		excluded = strconv.FormatUint(*report.LogicalWritesExcl, 10)
	}
	r.t.Logf("%s/%s: failed=%v wall=%.1fs ri_logical_writes=%s (excl_checkpoint=%s checkpoint=%d) ri_diskio_byteswritten=%d store=%d->%d wal=%d->%d resets=%d client_calls=%d waits=%d/%.1fs",
		r.arm, report.Phase, report.Failed, report.WallSeconds, logical, excluded, report.CheckpointBytes, report.DiskWritten,
		report.StoreBytesBefore, report.StoreBytesAfter, report.WALBefore, report.WALAfter,
		report.WALResets, report.ClientCalls, report.ExactnessWaits, report.ExactnessSeconds)
}

// finish removes what the run created and writes the run-level artifact. Like
// closePhase it must survive a failure unwind, so every step here reports
// rather than fails.
func (r *sustainedIORun) finish(manifest sustainedIOManifest) {
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
	census, err := sustainedIOWalkCensus(r.f.root)
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
	if err := sustainedIOTryWriteJSON(path, map[string]any{
		"manifest":               manifest,
		"phases":                 r.reports,
		"failed_phases":          failed,
		"worktrees_removed":      removed,
		"worktrees_retained":     retained,
		"retained_census":        census,
		"samples":                samples,
		"sample_failures":        failures,
		"wal_resets_total":       r.sampler.Resets(),
		"wal_checkpoint_bytes":   r.sampler.CheckpointBytes(),
		"client_calls_total":     r.f.clientCallCount(),
		"isolation":              r.isolation,
		"exactness_waits_total":  r.waits,
		"exactness_wait_seconds": r.waitSeconds,
		"reconcile_interval":     r.cfg.ReconcileInterval,
	}); err != nil {
		r.t.Logf("run artifact not written: %v", err)
	}
	r.t.Logf("%s artifacts: %s (phases=%d failed=%v worktrees removed=%d retained=%d)",
		r.arm, r.artifactDir, len(r.reports), failed, removed, len(retained))
}

// ------------------------------------------------------------- run helpers ---

func sustainedIODelta(before, after uint64) uint64 {
	if after < before {
		return 0
	}
	return after - before
}

func (r *sustainedIORun) processIO() (issue767ProcessIO, error) {
	pid := r.f.pid()
	if pid == 0 {
		return issue767ProcessIO{}, errSustainedIONoChild
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return issue767ReadProcessIO(ctx, pid)
}

// counters scrapes the daemon's own viewmetrics through `daemon status
// --format json`. A binary without the flag — every baseline arm built before
// the counters landed — returns an error, which is recorded by name; it is never substituted with
// zeros.
func (r *sustainedIORun) counters() (map[string]int64, error) {
	output, err := r.f.tryCommand(sustainedIOStatusScrapeTimeout, r.f.primary, "daemon", "status", "--format", "json", "--no-progress")
	if err != nil {
		return nil, fmt.Errorf("daemon status --format json unavailable on this arm: %w", err)
	}
	return sustainedIOParseStatusCounters(output)
}

func (r *sustainedIORun) daemonLogBytes() int64 {
	return issue767FileSize(r.f.logPath())
}

// awaitProbe waits for one exact answer and accounts for the wait, so a phase's
// cost includes how long the daemon made a reader wait for exactness.
func (r *sustainedIORun) awaitProbe(root, name, file string) {
	r.t.Helper()
	r.awaitProbeAs(root, name, file, r.f.spellingFor(root))
}

// awaitProbeAs is awaitProbe with the request spelling named explicitly, for
// the phase that changes a checkout's mode under the harness's feet.
func (r *sustainedIORun) awaitProbeAs(root, name, file string, spelling issue767Spelling) {
	r.t.Helper()
	started := time.Now()
	r.f.awaitSymbolAs(root, name, file, sustainedIODefaultProbeTimeout, spelling)
	r.accountWait(time.Since(started).Seconds())
}

// accountWait books one bounded wait to the run, the phase and the open
// sub-window. Every wait the harness takes is part of what the phase cost,
// including the ones spent waiting for an answer that could be judged at all.
func (r *sustainedIORun) accountWait(elapsed float64) {
	r.waits++
	r.phaseWaits++
	r.windowWaits++
	r.waitSeconds += elapsed
	r.phaseSeconds += elapsed
	r.windowSeconds += elapsed
}

func (r *sustainedIORun) markerPath(root string) string { return filepath.Join(root, "marker.go") }

func (r *sustainedIORun) filePath(index int) string {
	return filepath.Join(r.f.primary, filepath.FromSlash(sustainedIOFilePath(index%r.cfg.Fixture.Packages, index)))
}

// editFile advances one corpus file's revision and waits for the new probe to
// answer exactly out of that same file.
func (r *sustainedIORun) editFile(index int) {
	r.t.Helper()
	path := r.filePath(index)
	source, err := os.ReadFile(path)
	if err != nil {
		r.t.Fatal(err)
	}
	revision := r.revisions[index] + 1
	edited, err := sustainedIOEditFileSource(string(source), index, revision)
	if err != nil {
		r.t.Fatal(err)
	}
	r.f.write(path, edited)
	r.revisions[index] = revision
	r.awaitProbe(r.f.primary, sustainedIOProbeName(index, revision), path)
}

// sustainedIOIdleMode is what "idle" means for one idle window.
//
// The harness has always called one read-only `call search` every 5 s inside
// an idle phase — twelve daemon round trips per 60 s window — and reported the
// result as an idle floor. It is not one: the per-call sidecar transaction
// (37,080 B of WAL per read-only call) and the query log are client-driven
// writes, and a floor that includes them cannot answer "what does this daemon
// write when nobody asks it anything". Both questions are real, so both are
// measured, each in its own window with its own client-call count.
type sustainedIOIdleMode int

const (
	// sustainedIOIdleQuiet issues no client calls at all: the daemon's own floor.
	sustainedIOIdleQuiet sustainedIOIdleMode = iota
	// sustainedIOIdlePolling is the historical arm: one read-only search every 5 s.
	sustainedIOIdlePolling
)

func (m sustainedIOIdleMode) String() string {
	if m == sustainedIOIdleQuiet {
		return "quiet"
	}
	return "polling"
}

// sustainedIOIdlePollInterval is the polling arm's period, unchanged from the frozen
// protocol so the polling window stays comparable with the frozen phases.
const sustainedIOIdlePollInterval = 5 * time.Second

// idleAs holds the workload still for duration. The quiet arm touches nothing:
// it does not even ask whether the symbol is still there, because asking is
// the cost the other arm exists to measure.
func (r *sustainedIORun) idleAs(duration time.Duration, mode sustainedIOIdleMode) {
	r.t.Helper()
	deadline := time.Now().Add(duration)
	polls := 0
	for time.Now().Before(deadline) {
		if mode == sustainedIOIdlePolling {
			polls++
			found, err := r.f.trySearchSymbolIn(r.f.primary, r.lastProbe, r.lastProbeFile())
			if err != nil || !found {
				r.t.Fatalf("idle read-only query lost the selected ready symbol %s: found=%v err=%v", r.lastProbe, found, err)
			}
		}
		wait := sustainedIOIdlePollInterval
		if remaining := time.Until(deadline); remaining < wait {
			wait = remaining
		}
		if wait <= 0 {
			break
		}
		select {
		case <-r.t.Context().Done():
			r.t.Fatal(r.t.Context().Err())
		case <-time.After(wait):
		}
	}
	r.note(fmt.Sprintf("idle arm %s: %s held with %d harness read-only searches", mode, duration, polls))
}

// sustainedIONoScrapeInsideWindow is why a sub-window carries no viewmetrics delta. It
// is recorded by name: a missing series is a fact about the measurement, and a
// silently absent one is indistinguishable from a zero.
const sustainedIONoScrapeInsideWindow = "no daemon-status scrape inside this window: the bracket's own round trip is client traffic the enclosing phase would be charged for; the phase-level counter delta covers both idle arms"

// idleWindows measures idle twice: once polling, once with no client traffic.
//
// The POLLING arm runs FIRST, because where a window sits inside the run is
// part of what it measures. The frozen baseline's single idle window opened at
// the idle phase's own start — immediately after P0_cold_index for P1, after
// P7_dependent_untrack_retrack for P8 — so whatever the preceding phase left
// decaying (a deferred WAL drain, a settling publication) was charged to the
// window the ceiling was frozen over. Running the quiet arm first would have
// absorbed that tail into the arm that carries NO ceiling and handed the
// judged arm a window opening one full cfg.Idle further from the work. That
// bias is one-directional and flatters the candidate, on exactly the two
// phases whose frozen ceiling is live — the same class of harness-constructed
// error the split exists to remove, pointed the other way. The quiet arm goes
// second and inherits the polling arm's residue instead, which costs it
// nothing it is judged on: it is an un-budgeted floor reading, and one
// read-only search every 5 s leaves a floor's worth of residue at most.
//
// Both arms hold for the FULL duration, not half of it. The polling arm is the
// frozen protocol byte for byte — the same 60 s, the same 5 s read-only search,
// now also opening at the same point in the run — so it stays the 1:1
// counterpart of the frozen P1/P8 ceiling, which is what the reduction maps the
// frozen phase name onto (sustainedIOJudgedWindows). The quiet arm is an additional
// measurement of a question the frozen protocol never asked, and it is recorded
// as its own un-budgeted row rather than folded into a ceiling that was never
// measured over it. Halving both arms would have kept the phase's wall clock at
// 60 s and made the judged arm incomparable with the number it is judged
// against, which is the more expensive mistake.
//
// Neither arm's bracket scrapes `daemon status`: an interior scrape is client
// traffic inside a phase the frozen baseline measured with none.
func (r *sustainedIORun) idleWindows(quietWindow, pollingWindow string, duration time.Duration) {
	r.t.Helper()
	r.inScrapeFreeWindow(pollingWindow, fmt.Sprintf("%s idle polling one read-only search every %s: the frozen protocol's own measurement, unchanged in duration, period and position (it opens the phase, as the frozen window did)", duration, sustainedIOIdlePollInterval), func() {
		r.idleAs(duration, sustainedIOIdlePolling)
	})
	r.inScrapeFreeWindow(quietWindow, fmt.Sprintf("%s idle with no client calls: the daemon's own floor (un-budgeted: the frozen protocol never measured it, and it runs second so the judged arm keeps the frozen window's position)", duration), func() {
		r.idleAs(duration, sustainedIOIdleQuiet)
	})
	r.note(fmt.Sprintf("idle split: %s ran first and %s second, each holding %s; %s is the arm the frozen %s ceiling is applied to, and it runs first so it opens where the frozen window opened",
		pollingWindow, quietWindow, duration, pollingWindow, r.phase))
}

func (r *sustainedIORun) lastProbeFile() string { return r.markerPath(r.f.primary) }

// ------------------------------------------------------------------ phases ---

func (r *sustainedIORun) phaseColdIndex() {
	r.f.start()
	r.f.command(sustainedIOPhaseCommandTimeout, r.f.primary, "daemon", "status", "--no-progress")
	r.awaitProbe(r.f.primary, sustainedIOPrimaryMarker, r.markerPath(r.f.primary))
	r.f.settle()
	r.awaitIndexTimeWorkSettled()
}

// sustainedIOIndexSettleCounters are the counter families whose movement means index-
// time work is still in flight. Names are matched by prefix because each
// carries labels (`views_dedicated_base_publication_total{outcome=published}`).
var sustainedIOIndexSettleCounters = []string{
	"views_dedicated_base_publication_total",
	"views_dedicated_base_publish_total",
	"views_dedicated_base_claim_total",
	"views_generation_published_total",
}

const (
	// sustainedIOIndexSettlePoll is how often the publication family is re-read, and
	// sustainedIOIndexSettleQuiet how many consecutive unchanged reads end the wait.
	sustainedIOIndexSettlePoll  = 5 * time.Second
	sustainedIOIndexSettleQuiet = 4
)

// awaitIndexTimeWorkSettled holds the cold-index phase open until any
// dedicated-base publication scheduled by the daemon start has settled — or
// until the publication family has demonstrably not moved for
// sustainedIOIndexSettleQuiet consecutive reads, which is the shape of a daemon that
// scheduled none.
//
// The phase used to end at the first exact primary answer plus settle(), which
// only watches view_generations. At 6,000 files the `shape=root` publication
// the start schedules had not finished by then, so an index-time cost landed
// inside the next window and was reported as a 7.44x idle regression. Booking
// it to the phase that caused it is the difference between a measurement and a
// mislabel.
func (r *sustainedIORun) awaitIndexTimeWorkSettled() {
	started := time.Now()
	result := sustainedIOAwaitCountersQuiet(r.counters, sustainedIOIndexSettleCounters, sustainedIOIndexSettlePoll, sustainedIOIndexSettleQuiet,
		time.Now().Add(sustainedIODefaultProbeTimeout), r.sleepOrFail)
	r.accountWait(time.Since(started).Seconds())
	r.note(fmt.Sprintf("index-time settle: %s", result))
	if !result.Settled {
		r.t.Logf("%s/%s: index-time work did not go quiet inside %s: %s", r.arm, r.phase, sustainedIODefaultProbeTimeout, result)
	}
}

// sleepOrFail waits, and fails the phase if the test's context is cancelled
// underneath the wait.
func (r *sustainedIORun) sleepOrFail(d time.Duration) {
	select {
	case <-r.t.Context().Done():
		r.t.Fatal(r.t.Context().Err())
	case <-time.After(d):
	}
}

// sustainedIOSettleResult is what one quiescence wait observed.
type sustainedIOSettleResult struct {
	Polls      int      `json:"polls"`
	Activity   int64    `json:"activity"`
	Moved      int64    `json:"moved"`
	StableFor  int      `json:"stable_polls"`
	Settled    bool     `json:"settled"`
	Seconds    float64  `json:"wait_s"`
	Unreadable int      `json:"unreadable_polls,omitempty"`
	Errors     []string `json:"errors,omitempty"`
}

func (s sustainedIOSettleResult) String() string {
	return fmt.Sprintf("settled=%v polls=%d activity=%d moved=%d stable_for=%d unreadable=%d",
		s.Settled, s.Polls, s.Activity, s.Moved, s.StableFor, s.Unreadable)
}

// sustainedIOCounterActivity sums every counter whose name starts with one of the named
// families. A family that does not appear contributes nothing, which is the
// same as a family at zero — for a monotone counter the distinction does not
// change whether it MOVED, which is the only question here.
func sustainedIOCounterActivity(counters map[string]int64, prefixes []string) int64 {
	total := int64(0)
	for name, value := range counters {
		for _, prefix := range prefixes {
			if strings.HasPrefix(name, prefix) {
				total += value
				break
			}
		}
	}
	return total
}

// sustainedIOAwaitCountersQuiet polls read until the named counter families have not
// moved across `quiet` consecutive readings, or until the deadline.
//
// A reading that fails is not a quiet reading: it resets the streak and is
// counted, because "the daemon did not answer" and "the daemon answered the
// same number again" are different facts and only the second one ends a wait.
func sustainedIOAwaitCountersQuiet(
	read func() (map[string]int64, error),
	prefixes []string,
	interval time.Duration,
	quiet int,
	deadline time.Time,
	sleep func(time.Duration),
) sustainedIOSettleResult {
	result := sustainedIOSettleResult{}
	started := time.Now()
	var previous int64
	seeded := false
	for {
		result.Polls++
		counters, err := read()
		if err != nil {
			result.Unreadable++
			result.StableFor = 0
			if len(result.Errors) < 5 {
				result.Errors = append(result.Errors, err.Error())
			}
		} else {
			activity := sustainedIOCounterActivity(counters, prefixes)
			result.Activity = activity
			if seeded {
				if activity == previous {
					result.StableFor++
				} else {
					result.Moved += activity - previous
					result.StableFor = 0
				}
			}
			previous, seeded = activity, true
			if result.StableFor >= quiet {
				result.Settled = true
				result.Seconds = time.Since(started).Seconds()
				return result
			}
		}
		if !time.Now().Before(deadline) {
			result.Seconds = time.Since(started).Seconds()
			return result
		}
		sleep(interval)
	}
}

func (r *sustainedIORun) phaseIdleCold() {
	r.idleWindows(sustainedIOWindowIdleColdQuiet, sustainedIOWindowIdleColdPolling, r.cfg.Idle)
}

func (r *sustainedIORun) phaseSmallEdits() {
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

func (r *sustainedIORun) phaseTouchStageUnstage() {
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
		r.awaitProbe(r.f.primary, sustainedIOProbeName(index, revision), path)
	} else {
		r.awaitProbe(r.f.primary, r.lastProbe, r.lastProbeFile())
	}
	r.f.settle()
}

// phaseAmendSameTree measures a same-tree amend — and, in its own window, the
// tree-changing commit it has to perform first.
//
// The amend needs a clean tree to amend over, so the phase commits whatever the
// edit phases left dirty. That commit is a real ten-file content change and
// costs a committed-base advance: 104 MB in the frozen candidate artifacts, all
// of it landing 1–4 s after the commit and inside a window named
// "P4_amend_same_tree". The amend itself is below the idle floor — zero
// catalog DML in all 62 tables over six repetitions. Measuring them together
// produced the verdict's 270x row and pointed every reader at the wrong
// stimulus.
//
// The two are now bracketed separately. The phase keeps its name and its
// total — the frozen ceiling stays comparable — and the artifact carries
// P4a_commit_tree_change and P4b_amend_same_tree as their own rows, which is
// where the amend's number can honestly be read.
func (r *sustainedIORun) phaseAmendSameTree() {
	r.inWindow(sustainedIOWindowCommitTreeChange,
		"git add -A && git commit of whatever the edit phases left dirty: a tree-changing commit, not the amend",
		func() {
			r.f.git(r.f.primary, "add", "-A")
			r.f.git(r.f.primary, "commit", "--allow-empty", "-m", "pre-amend")
			r.f.settle()
			// The committed-base advance the commit triggers lands seconds
			// after git returns; hold this window open until it has settled so
			// its bytes are booked here and not to the amend.
			r.awaitIndexTimeWorkSettled()
		})
	r.inWindow(sustainedIOWindowAmendSameTree,
		"git commit --amend --no-edit: a new commit id over an unchanged tree",
		func() {
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
		})
}

func (r *sustainedIORun) phaseMainAdvance() {
	for i := 1; i <= r.cfg.Worktrees; i++ {
		path := filepath.Join(r.f.root, fmt.Sprintf("wt%02d", i))
		r.f.git(r.f.primary, "worktree", "add", "-b", fmt.Sprintf("w%02d", i), path)
		r.dependents = append(r.dependents, path)
		// Discovered, never tracked: no track call, no config edit.
		r.awaitProbe(path, sustainedIOPrimaryMarker, r.markerPath(path))
	}
	r.f.settle()
	for commit := 1; commit <= r.cfg.Commits; commit++ {
		marker := fmt.Sprintf(sustainedIOAdvanceMarkerFormat, commit)
		r.f.write(r.markerPath(r.f.primary), issue767MarkerSource(sustainedIOPrimaryMarker, marker))
		for offset := 0; offset < sustainedIOCommitFilesPerCommit-1; offset++ {
			index := r.rotation[(commit+offset)%len(r.rotation)]
			path := r.filePath(index)
			source, err := os.ReadFile(path)
			if err != nil {
				r.t.Fatal(err)
			}
			revision := r.revisions[index] + 1
			edited, err := sustainedIOEditFileSource(string(source), index, revision)
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
			r.awaitProbe(dependent, sustainedIOPrimaryMarker, r.markerPath(dependent))
		}
	}
	if len(r.dependents) > 0 && r.commits > 0 {
		r.probeIsolation("main-only symbol "+r.lastProbe+" in dependent "+filepath.Base(r.dependents[0]),
			r.dependents[0], r.lastProbe, r.markerPath(r.dependents[0]))
	}
	r.f.settle()
}

// probeIsolation asks one isolation question until it has an answer the rule
// can be applied to, then applies it.
//
// The shot it replaces was single: one trySearchSymbolIn, t.Fatal on any error,
// while issue767Verdict turns any inexact answer into an error. At 6,000 files
// the answer is inexact 3 times in 14 — a truthful, self-healing
// `fallback_reason:"base_changed"` that clears in 0.45–1.31 s because a base
// pin is a witness rather than a read gate. The single shot turned that label
// into a failed phase and took P6, P7 and P8 with it. Every other check in
// these phases already goes through a bounded await; this one now does too.
//
// What does NOT change is what an answer means once it can be judged:
// sustainedIOIsolationOutcome is untouched, a leak is still a leak, and a question that
// never becomes judgeable inside the probe timeout is a failure with the whole
// answer attached — never a quiet pass.
func (r *sustainedIORun) probeIsolation(subject, root, name, file string) {
	r.t.Helper()
	probe := r.f.awaitJudgeable(root, name, file, r.f.spellingFor(root), sustainedIODefaultProbeTimeout)
	r.accountWait(probe.Seconds)
	record := sustainedIOIsolationRecord{Subject: subject, Probe: probe, Answer: probe.Answer, Judged: probe.Judgeable, Leaked: probe.Found}
	if !probe.Judgeable {
		record.Outcome = fmt.Sprintf("NOT JUDGEABLE: no answer the rule applies to within %s after %d asks (%.2fs); last answer %+v; last error %q",
			sustainedIODefaultProbeTimeout, probe.Polls, probe.Seconds, probe.Answer, probe.Err)
		r.recordIsolation(record)
		r.t.Fatalf("%s: %s", subject, record.Outcome)
		return
	}
	if probe.Err != "" {
		record.Outcome = "ISOLATION QUERY REFUSED: " + probe.Err
		r.recordIsolation(record)
		r.t.Fatalf("%s: %s", subject, record.Outcome)
		return
	}
	r.judgeIsolation(record)
}

// requireIsolation applies sustainedIOIsolationOutcome to an answer the caller already
// judged, and records it either way.
func (r *sustainedIORun) requireIsolation(subject string, leaked bool) {
	r.t.Helper()
	r.judgeIsolation(sustainedIOIsolationRecord{
		Subject: subject, Judged: true, Leaked: leaked,
		Answer: issue767Answer{Found: leaked},
	})
}

// judgeIsolation is the one place gate 5's rule is applied. It is unchanged:
// what the bounded probe changed is WHEN the harness may reach it.
func (r *sustainedIORun) judgeIsolation(record sustainedIOIsolationRecord) {
	r.t.Helper()
	fatal, note := sustainedIOIsolationOutcome(r.arm, record.Leaked)
	record.Outcome = note
	r.recordIsolation(record)
	if fatal {
		r.t.Fatalf("%s: %s", record.Subject, note)
	}
	if record.Leaked {
		r.t.Logf("%s/%s: %s", r.arm, record.Subject, note)
	}
}

// recordIsolation files the whole answer — spelling, found, exact label,
// fallback label, source file, answering corpus, error, asks and wait — on the
// phase note, on the phase report and on the run, so it is in the artifact
// whether or not the phase went on to fail.
func (r *sustainedIORun) recordIsolation(record sustainedIOIsolationRecord) {
	r.note(fmt.Sprintf("%s: %s [spelling=%s found=%v exact_label=%v fallback=%v from_expected_file=%v answered_by=%q asks=%d waited=%.2fs error=%q]",
		record.Subject, record.Outcome, record.Answer.Spelling, record.Answer.Found, record.Answer.Exact,
		record.Answer.Fallback, record.Answer.FromExpectedFile, record.Answer.Prefix,
		record.Probe.Polls, record.Probe.Seconds, record.Probe.Err))
	r.phaseIsolation = append(r.phaseIsolation, record)
	r.isolation = append(r.isolation, record)
}

// sustainedIOIsolationOutcome says what a leaked symbol means for an arm.
//
// Gate 5 — "a workload with ten dependent worktrees updates every logical view
// correctly" — is a claim about the branch. The baseline binary predates it:
// main 56a1c29d serves a discovered dependent out of the family's advancing
// base, so the commit that moves main is visible inside a worktree whose own
// tree never moved. That is the defect under repair, and on the baseline arm it
// is the measurement, not a harness failure. Stopping the baseline there would
// end the arm at P5 and leave the headline phase — and every phase after it —
// with nothing to compare the candidate against, which is the one outcome this
// item exists to prevent.
//
// So a leak is recorded by name on the baseline and fatal everywhere else.
// "Everywhere else" is deliberate: any arm whose name is not exactly "baseline"
// is held to the gate, so a renamed or mistyped arm can never inherit the
// exemption.
func sustainedIOIsolationOutcome(arm string, leaked bool) (fatal bool, note string) {
	if !leaked {
		return false, "isolation held"
	}
	if arm == "baseline" {
		return false, "BASELINE ISOLATION VIOLATION: the symbol is visible where it must not be; " +
			"this is gate 5's pre-repair behaviour on main 56a1c29d, recorded as evidence, and it is not a budget"
	}
	return true, "ISOLATION VIOLATION: the symbol is visible where it must not be (gate 5)"
}

func (r *sustainedIORun) phaseDependentEdits() {
	if len(r.dependents) == 0 {
		r.t.Log("no dependents configured; phase is a no-op")
		return
	}
	edited := min(len(r.dependents), 3)
	for i := 0; i < edited; i++ {
		dependent := r.dependents[i]
		marker := fmt.Sprintf(sustainedIODependentMarkerForm, i+1)
		r.f.write(r.markerPath(dependent), issue767MarkerSource(sustainedIOPrimaryMarker, marker))
		r.awaitProbe(dependent, marker, r.markerPath(dependent))
		r.probeIsolation("dependent edit "+marker+" in the primary view",
			r.f.primary, marker, r.markerPath(r.f.primary))
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
func (r *sustainedIORun) phaseDependentUntrackRetrack() {
	if len(r.dependents) == 0 {
		r.t.Log("no dependents configured; phase is a no-op")
		return
	}
	dependent := r.dependents[0]
	marker := fmt.Sprintf(sustainedIODependentMarkerForm, 1)
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
	r.note("track --as-worktree: " + sustainedIOTail(output))

	// Dedicated: asked as its own repository.
	r.awaitProbeAs(dependent, marker, file, issue767AsOwnCorpus)
	r.noteAnswer("dedicated checkout", dependent, marker, file, issue767AsOwnCorpus)
	r.noteAnswer("dedicated checkout", dependent, marker, file, issue767AsAutomaticWorktree)
	r.f.settle()

	output, err = r.f.tryCommand(8*time.Minute, r.f.primary, "untrack", dependent, "--no-progress")
	if err != nil {
		r.t.Fatalf("untrack %s: %v\n%s", dependent, err, output)
	}
	r.note("untrack: " + sustainedIOTail(output))
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
func (r *sustainedIORun) noteAnswer(subject, root, name, file string, spelling issue767Spelling) {
	answer, err := r.f.askSymbol(root, name, file, spelling)
	if err != nil {
		r.note(fmt.Sprintf("%s asked via %s: refused: %s", subject, spelling, sustainedIOTail([]byte(err.Error()))))
		return
	}
	r.note(fmt.Sprintf("%s asked via %s: found=%v exact_label=%v fallback=%v from_expected_file=%v answered_by=%q",
		subject, spelling, answer.Found, answer.Exact, answer.Fallback, answer.FromExpectedFile, answer.Prefix))
}

// dumpDiagnostics writes the failure evidence next to the phase artifacts.
func (r *sustainedIORun) dumpDiagnostics(phase string) {
	run := func(args ...string) string {
		output, err := r.f.tryCommand(60*time.Second, r.f.primary, args...)
		if err != nil {
			return fmt.Sprintf("ERROR %v\n%s", err, output)
		}
		return string(output)
	}
	paths := append([]string{r.f.primary}, r.dependents...)
	diagnostics := sustainedIOCollectDiagnostics(phase, run, r.daemonLogTail(), paths, r.phaseIsolation)
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
func (r *sustainedIORun) daemonLogTail() string {
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
// While a sub-window is open the note lands on that window's row too, so a
// window's evidence is readable without reassembling it from the phase.
func (r *sustainedIORun) note(text string) {
	r.phaseNotes = append(r.phaseNotes, text)
	if r.windowOpen {
		r.windowNotes = append(r.windowNotes, text)
	}
}

// sustainedIOTail keeps a command's last line or two, so a phase note carries the
// outcome without carrying a screenful of progress output.
func sustainedIOTail(output []byte) string {
	lines := strings.Split(strings.TrimSpace(string(output)), "\n")
	if len(lines) > 2 {
		lines = lines[len(lines)-2:]
	}
	return strings.TrimSpace(strings.Join(lines, " / "))
}

func (r *sustainedIORun) phaseIdleWarm() {
	r.idleWindows(sustainedIOWindowIdleWarmQuiet, sustainedIOWindowIdleWarmPolling, r.cfg.Idle)
}

// enforceIdleBudget is the only budget in this item, and it is off unless the
// operator names a number. Budgets proper are frozen from a baseline arm by a
// later item; this exists so a run can be given one without editing code. It
// applies to the idle phases only, on the candidate arm only, and it records
// the reason when the series it needs is unavailable rather than passing by
// default.
func (r *sustainedIORun) enforceIdleBudget(report *sustainedIOPhaseReport) {
	if r.cfg.IdleBudget == 0 || r.arm != "candidate" || !strings.Contains(report.Phase, "_idle_") {
		return
	}
	breaches, note := sustainedIOIdleBudgetBreaches(report, r.cfg.IdleBudget)
	if note != "" {
		report.Notes = append(report.Notes, note)
	}
	for _, breach := range breaches {
		r.t.Errorf("%s", breach)
	}
}

// sustainedIOIdleBudgetBreaches applies an operator's idle byte budget to an idle
// phase's report, and it applies it to each idle ARM rather than to their sum.
//
// The budget names a number of bytes per idle window. Since the phase measures
// idle twice — quiet, then polling — judging their sum against a single-window
// number would either fail an arm that is inside its budget or, if the number
// were doubled to compensate, let an arm write twice what the budget allows.
// Per-window is the only reading that keeps the operator's number meaning what
// it says, and it is strictly stronger than the phase-total check it replaces:
// every window has to be inside the budget, and a phase with no windows is
// still judged as a whole.
func sustainedIOIdleBudgetBreaches(report *sustainedIOPhaseReport, budget uint64) ([]string, string) {
	if budget == 0 {
		return nil, ""
	}
	windows := []sustainedIOWindowReport{}
	for _, window := range report.Windows {
		if window.LogicalWrites != nil {
			windows = append(windows, window)
		}
	}
	if len(windows) == 0 {
		if report.LogicalWrites == nil {
			return nil, "idle budget not checked: ri_logical_writes unavailable"
		}
		if *report.LogicalWrites > budget {
			return []string{fmt.Sprintf("%s wrote %d logical bytes, budget %d", report.Phase, *report.LogicalWrites, budget)}, ""
		}
		return nil, ""
	}
	breaches := []string{}
	for _, window := range windows {
		if *window.LogicalWrites > budget {
			breaches = append(breaches, fmt.Sprintf("%s/%s wrote %d logical bytes, budget %d per idle window",
				report.Phase, window.Window, *window.LogicalWrites, budget))
		}
	}
	return breaches, fmt.Sprintf("idle budget %d applied to each of the %d idle windows, not to their sum", budget, len(windows))
}

func (r *sustainedIORun) settleShort() {
	select {
	case <-r.t.Context().Done():
		r.t.Fatal(r.t.Context().Err())
	case <-time.After(5 * time.Second):
	}
}

func (r *sustainedIORun) gitOutput(args ...string) string {
	r.t.Helper()
	output, err := sustainedIOGitOutput(r.t, r.f, args...)
	if err != nil {
		r.t.Fatalf("git %v: %v", args, err)
	}
	return output
}

func sustainedIOGitOutput(t *testing.T, f *issue767Fixture, args ...string) (string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir, cmd.Env = f.primary, f.env
	output, err := cmd.Output()
	return strings.TrimSpace(string(output)), err
}

// sustainedIOSourceIdentity records which tree the harness itself came from. It is best
// effort: the harness may run from an exported tree with no git metadata.
//
// The digest covers `git status --porcelain=v1 -uall` AND `git diff HEAD`.
// The status alone lists paths and status letters, never contents, so two
// different working trees with the same modified-file list hash identically —
// which is how a run's manifest can claim a source identity it does not have
// while a neighbouring agent rewrites a file in the same worktree. Hashing the
// diff as well is what makes the identity answer "which bytes", not "which
// filenames". Untracked contents remain outside both (git diff does not carry
// them); the file list still names them.
// The cwd it must NOT use is the process's. A measurement run compiles this
// package into a test binary and executes it from a private root under /tmp —
// `go test -c` plus a short run directory is the whole protocol — and git run
// there answers nothing, so every frozen manifest in the paired1500 artifact
// set carries no source_commit and no
// dirty_tree_digest at all. A manifest whose whole job is "which tree produced
// this number" silently recorded nothing. The tree is resolved from the
// compiled-in path of this source file instead, which is where the harness
// actually came from.
func sustainedIOSourceIdentity() (commit, dirty string, notes []string) {
	dir, note := sustainedIOHarnessSourceDir()
	if note != "" {
		notes = append(notes, note)
	}
	if dir == "" {
		return "", "", notes
	}
	commit, dirty, gitNotes := sustainedIOGitIdentity(dir)
	return commit, dirty, append(notes, gitNotes...)
}

// sustainedIOHarnessSourceDir is the directory this file was compiled from. Under
// -trimpath the recorded path is relative to the module root and no longer
// resolves, which is reported by name rather than papered over with the
// process's working directory.
func sustainedIOHarnessSourceDir() (string, string) {
	_, file, _, ok := runtime.Caller(0)
	if !ok || file == "" {
		return "", "source identity unavailable: runtime.Caller gave no file for the harness"
	}
	dir := filepath.Dir(file)
	if !filepath.IsAbs(dir) {
		return "", "source identity unavailable: harness path " + file + " is not absolute (built with -trimpath?)"
	}
	if _, err := os.Stat(dir); err != nil {
		return "", "source identity unavailable: harness path " + dir + " does not resolve: " + err.Error()
	}
	return dir, ""
}

// sustainedIOGitIdentity reads the commit and the dirty digest of the tree that owns
// dir. A directory that is not in a work tree is named as such: an empty
// identity with no explanation is what produced the frozen artifacts' silence.
func sustainedIOGitIdentity(dir string) (commit, dirty string, notes []string) {
	run := func(args ...string) (string, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		output, err := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...).Output()
		if err != nil {
			return "", err
		}
		return strings.TrimSpace(string(output)), nil
	}
	commit, err := run("rev-parse", "HEAD")
	if err != nil {
		return "", "", []string{"source commit unavailable for " + dir + ": " + err.Error()}
	}
	status, statusErr := run("status", "--porcelain=v1", "-uall")
	diff, diffErr := run("diff", "HEAD")
	if statusErr != nil {
		notes = append(notes, "dirty status unavailable for "+dir+": "+statusErr.Error())
	}
	if diffErr != nil {
		notes = append(notes, "dirty diff unavailable for "+dir+": "+diffErr.Error())
	}
	return commit, sustainedIODirtyDigest(status, diff), notes
}

// sustainedIODirtyDigest is the content-sensitive half of the source identity: the
// modified-file list AND the modified bytes. A clean tree digests to "".
func sustainedIODirtyDigest(status, diff string) string {
	if status == "" && diff == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(status + "\x00" + diff))
	return hex.EncodeToString(sum[:])
}

func sustainedIOWriteJSON(t *testing.T, path string, value any) {
	t.Helper()
	if err := sustainedIOTryWriteJSON(path, value); err != nil {
		t.Fatal(err)
	}
}

// sustainedIOTryWriteJSON is sustainedIOWriteJSON without the test handle, for the code paths
// that run while a failure is already unwinding.
func sustainedIOTryWriteJSON(path string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o600)
}

// ------------------------------------------------------------------ tests ---

func TestSustainedIOConfigFromEnvDefaultsAndBounds(t *testing.T) {
	cfg, err := sustainedIOConfigFromEnv(func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Fixture.Files != 1500 || cfg.Worktrees != 10 || cfg.Commits != 20 || cfg.Edits != 10 ||
		cfg.EditInterval != 30*time.Second || cfg.Idle != time.Minute || cfg.SampleInterval != time.Second ||
		cfg.Repetitions != 1 || cfg.IdleBudget != 0 {
		t.Fatalf("defaults drifted: %+v", cfg)
	}
	env := map[string]string{
		"GX_SUSTAINED_IO_FIXTURE_FILES":     "6000",
		"GX_SUSTAINED_IO_FIXTURE_PACKAGES":  "120",
		"GX_SUSTAINED_IO_FIXTURE_SEED":      "99",
		"GX_SUSTAINED_IO_WORKTREES":         "2",
		"GX_SUSTAINED_IO_COMMITS":           "3",
		"GX_SUSTAINED_IO_EDITS":             "4",
		"GX_SUSTAINED_IO_EDIT_INTERVAL":     "2s",
		"GX_SUSTAINED_IO_IDLE":              "5s",
		"GX_SUSTAINED_IO_SAMPLE_INTERVAL":   "500ms",
		"GX_SUSTAINED_IO_REPS":              "2",
		"GX_SUSTAINED_IO_IDLE_BUDGET_BYTES": "8388608",
	}
	cfg, err = sustainedIOConfigFromEnv(func(key string) string { return env[key] })
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
		{"GX_SUSTAINED_IO_FIXTURE_FILES", "1"},
		{"GX_SUSTAINED_IO_FIXTURE_FILES", "nonsense"},
		{"GX_SUSTAINED_IO_WORKTREES", "-1"},
		{"GX_SUSTAINED_IO_REPS", "0"},
		{"GX_SUSTAINED_IO_IDLE", "10ms"},
		{"GX_SUSTAINED_IO_IDLE", "nope"},
		{"GX_SUSTAINED_IO_IDLE_BUDGET_BYTES", "huge"},
	} {
		if _, err := sustainedIOConfigFromEnv(func(key string) string {
			if key == tc.key {
				return tc.value
			}
			return ""
		}); err == nil {
			t.Errorf("%s=%q was accepted; out-of-range knobs must be refused by name", tc.key, tc.value)
		}
	}
}

func TestSustainedIOPhasePlanIsTheDeclaredNinePhaseWorkload(t *testing.T) {
	cfg, err := sustainedIOConfigFromEnv(func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	plan := sustainedIOPhasePlan(cfg)
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
	scaled, err := sustainedIOConfigFromEnv(func(key string) string {
		return map[string]string{"GX_SUSTAINED_IO_WORKTREES": "2", "GX_SUSTAINED_IO_COMMITS": "3"}[key]
	})
	if err != nil {
		t.Fatal(err)
	}
	if detail := sustainedIOPhasePlan(scaled)[5].Detail; !strings.Contains(detail, "2 dependent worktrees") || !strings.Contains(detail, "3 commits") {
		t.Errorf("phase detail does not follow the config: %q", detail)
	}
}

func TestSustainedIORequiredTimeoutScalesWithTheWorkload(t *testing.T) {
	small := sustainedIOConfig{Worktrees: 1, Commits: 1, Edits: 1, EditInterval: time.Second, Idle: 5 * time.Second, Repetitions: 1}
	large := sustainedIOConfig{Worktrees: 10, Commits: 20, Edits: 10, EditInterval: 30 * time.Second, Idle: time.Minute, Repetitions: 1}
	if sustainedIORequiredTimeout(small, 1) >= sustainedIORequiredTimeout(large, 1) {
		t.Fatal("a larger workload must require a larger timeout")
	}
	if sustainedIORequiredTimeout(large, 2) != 2*sustainedIORequiredTimeout(large, 1) {
		t.Fatal("a second arm must double the requirement")
	}
	paired := large
	paired.Repetitions = 3
	if sustainedIORequiredTimeout(paired, 1) != 3*sustainedIORequiredTimeout(large, 1) {
		t.Fatal("repetitions must multiply the requirement")
	}
	if sustainedIORequiredTimeout(large, 0) != sustainedIORequiredTimeout(large, 1) {
		t.Fatal("an arm count below one must be treated as one arm")
	}
}

// TestSustainedIOProcessDeltaHandlesTheStartingChildAndTheRestart pins the three cases
// the phase envelope must tell apart: an ordinary window, the phase that starts
// the daemon (no baseline process, so the child's own counters ARE the delta),
// and a child that was replaced mid-phase (deltas are meaningless and must not
// be reported as a measurement).
func TestSustainedIOProcessDeltaHandlesTheStartingChildAndTheRestart(t *testing.T) {
	logical := func(v uint64) *uint64 { return &v }
	before := issue767ProcessIO{BytesWritten: 100, BytesRead: 10, UserTimeNS: 5, SystemTimeNS: 6, StartTicks: 7, LogicalBytesWritten: logical(1000)}
	after := issue767ProcessIO{BytesWritten: 400, BytesRead: 30, UserTimeNS: 15, SystemTimeNS: 26, StartTicks: 7, PhysFootprint: 42, LogicalBytesWritten: logical(4000)}

	window := sustainedIOProcessDelta(before, nil, after, nil)
	if !window.OK || window.Logical == nil || *window.Logical != 3000 || window.DiskWritten != 300 ||
		window.DiskRead != 20 || window.CPUUserNS != 10 || window.CPUSystemNS != 20 || window.PhysFootprint != 42 {
		t.Fatalf("ordinary window = %+v (logical=%v)", window, window.Logical)
	}
	if len(window.Notes) != 0 {
		t.Errorf("ordinary window carried notes: %v", window.Notes)
	}

	// The cold-index phase: nothing to read before the child exists.
	window = sustainedIOProcessDelta(issue767ProcessIO{}, errSustainedIONoChild, after, nil)
	if !window.OK || window.Logical == nil || *window.Logical != 4000 || window.DiskWritten != 400 {
		t.Fatalf("starting-child window lost the cold series: %+v", window)
	}
	if len(window.Notes) != 1 || !strings.Contains(window.Notes[0], "lifetime totals") {
		t.Errorf("starting-child window must say what it measured: %v", window.Notes)
	}

	// A restart mid-phase: the counters belong to a different process.
	restarted := after
	restarted.StartTicks = 9
	window = sustainedIOProcessDelta(before, nil, restarted, nil)
	if window.OK || window.Logical != nil || window.DiskWritten != 0 {
		t.Fatalf("a restarted child must not produce a delta: %+v", window)
	}
	if len(window.Notes) != 1 || !strings.Contains(window.Notes[0], "restarted") {
		t.Errorf("restart note missing: %v", window.Notes)
	}

	// Unreadable ends stay unavailable, by name, on both sides.
	if window = sustainedIOProcessDelta(before, nil, after, errors.New("boom")); window.OK || !strings.Contains(window.Notes[0], "boom") {
		t.Errorf("end failure = %+v", window)
	}
	if window = sustainedIOProcessDelta(before, errors.New("kaput"), after, nil); window.OK || !strings.Contains(window.Notes[0], "kaput") {
		t.Errorf("start failure = %+v", window)
	}
}

// TestSustainedIOCollectDiagnosticsAsksTheExplainingQuestions pins the failure-evidence
// command set: a timed-out exactness wait must leave behind the daemon status
// payload, the family census and one route explanation per checkout in play.
func TestSustainedIOCollectDiagnosticsAsksTheExplainingQuestions(t *testing.T) {
	var calls []string
	run := func(args ...string) string {
		joined := strings.Join(args, " ")
		calls = append(calls, joined)
		return "output of " + joined
	}
	diagnostics := sustainedIOCollectDiagnostics("P7_dependent_untrack_retrack", run, "log tail", []string{"/tmp/repo", "/tmp/wt01"}, nil)
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

func TestSustainedIOStoreCensusSurvivesAMissingSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "empty.sqlite")
	db, err := sql.Open("sqlite", testdsn.FileURI(path, ""))
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
	census := sustainedIOReadStoreCensus(ctx, db)
	if census.PageSize == 0 || census.PageCount == 0 {
		t.Errorf("page pragmas must still be read: %+v", census)
	}
	if len(census.Errors) == 0 {
		t.Fatal("a store without the view tables must name its missing series, not report zeros silently")
	}
}

// TestSustainedIORunSamplerIsSafeWhileTheFixtureStartsItsChild exercises the production
// wiring — sustainedIONewRunSampler, the same call sustainedIORunWorkload makes — against the
// fixture's own start/stop handle mutations, on two goroutines.
//
// It is a regression test for a real defect: the sampler goroutine is started
// BEFORE P0 calls f.start(), so its pid and log-size reads run concurrently
// with the writes that install the child. Before the fixture's handle fields
// were placed under a mutex this pattern tripped the race detector, which made
// the whole harness unrunnable under -race. No daemon is needed: the child
// handle is never started, so pid() reports 0 and the process reader returns
// errSustainedIONoChild — the fields being raced are the same ones either way.
func TestSustainedIORunSamplerIsSafeWhileTheFixtureStartsItsChild(t *testing.T) {
	root := t.TempDir()
	f := &issue767Fixture{t: t, root: root, primary: filepath.Join(root, "repo"), store: filepath.Join(root, "store.sqlite")}
	sampler := sustainedIONewRunSampler(f, io.Discard, time.Millisecond)

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

// TestSustainedIOIsolationOutcomeExemptsOnlyTheBaselineArm pins the one arm-conditional
// rule in the harness.
//
// An exemption that spreads is worse than no measurement: if the candidate
// could ever inherit it, the paired run would report a write reduction that was
// partly bought by serving the wrong corpus, and gate 5 would be untested by
// the only workload that exercises ten dependents at once.
func TestSustainedIOIsolationOutcomeExemptsOnlyTheBaselineArm(t *testing.T) {
	for _, arm := range []string{"candidate", "baseline", "", "Baseline", "baseline2", "control"} {
		fatal, note := sustainedIOIsolationOutcome(arm, false)
		if fatal || note != "isolation held" {
			t.Fatalf("arm %q with no leak: fatal=%v note=%q", arm, fatal, note)
		}
	}
	fatal, note := sustainedIOIsolationOutcome("baseline", true)
	if fatal {
		t.Fatal("a baseline leak stopped the arm; the headline phase and everything after it would have no baseline")
	}
	if !strings.Contains(note, "BASELINE ISOLATION VIOLATION") || !strings.Contains(note, "not a budget") {
		t.Fatalf("the baseline leak is not recorded loudly enough: %q", note)
	}
	// Every other arm name, including near-misses, is held to the gate.
	for _, arm := range []string{"candidate", "", "Baseline", "baseline2", "baseline ", "control"} {
		fatal, note := sustainedIOIsolationOutcome(arm, true)
		if !fatal {
			t.Fatalf("arm %q inherited the baseline exemption: %q", arm, note)
		}
		if !strings.Contains(note, "gate 5") {
			t.Fatalf("arm %q's violation does not name the gate it breaks: %q", arm, note)
		}
	}
}

// TestSustainedIODirtyDigestSeesContentNotOnlyFilenames pins the source identity a
// measurement manifest claims.
//
// Five agents edit _test.go files in this worktree while a measured run is in
// flight, so "which tree produced this number" is a real question. A digest
// over `git status --porcelain` alone answers a different one: it hashes paths
// and status letters, so two trees whose files differ in every byte but agree
// on which files are modified hash identically — and a manifest that cannot
// distinguish them is not a source identity, it is a file list.
func TestSustainedIODirtyDigestSeesContentNotOnlyFilenames(t *testing.T) {
	const status = " M cmd/gortex/sustained_io_integration_test.go\n M internal/indexer/multi.go\n"
	first := sustainedIODirtyDigest(status, "@@ -1 +1 @@\n-a\n+b\n")
	second := sustainedIODirtyDigest(status, "@@ -1 +1 @@\n-a\n+c\n")
	if first == "" || second == "" {
		t.Fatal("a dirty tree digested to the clean-tree sentinel")
	}
	if first == second {
		t.Fatal("two trees with the same modified-file list and different contents digested identically; " +
			"the manifest cannot tell which bytes produced its numbers")
	}
	if same := sustainedIODirtyDigest(status, "@@ -1 +1 @@\n-a\n+b\n"); same != first {
		t.Fatal("the digest is not stable for one tree state")
	}
	if sustainedIODirtyDigest("", "") != "" {
		t.Fatal("a clean tree must digest to the empty sentinel, not to a hash of nothing")
	}
	// A status-only change still moves it: neither half may be dropped.
	if sustainedIODirtyDigest(status+"?? new.go\n", "@@ -1 +1 @@\n-a\n+b\n") == first {
		t.Fatal("an added untracked file did not move the digest")
	}
}

// TestSustainedIONewRunSamplerWiresEveryProductionReader pins the production wiring
// itself, reader by reader.
//
// sustainedIONewRunSampler is the only place the measured child, the measured store and
// the measured WAL are connected to the instrument, and each connection is one
// deletable line. With defaults in newSustainedIOSampler each deletion used to leave the
// whole suite green while the corresponding series read zero for an entire
// measured run — a missing PID, a store/WAL/log size series stuck at 0, or a
// wal_resets count of 0 that no sample_failure contradicted. Zeros are exactly
// what a quiet phase looks like, so the deletion was unobservable in the
// artifact as well as in the suite.
//
// So this test builds the sampler the way sustainedIORunWorkload does, over a fixture
// with a real WAL-mode store, a real log file and a real live child, and
// asserts that every series carries the value that reader is supposed to
// deliver — plus that nothing is reported as unwired.
func TestSustainedIONewRunSamplerWiresEveryProductionReader(t *testing.T) {
	root := t.TempDir()
	f := &issue767Fixture{t: t, root: root, primary: filepath.Join(root, "repo"), store: filepath.Join(root, "store.sqlite")}

	db, err := sql.Open("sqlite", testdsn.FileURI(f.store, ""))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	db.SetMaxOpenConns(1)
	var mode string
	if err := db.QueryRow("PRAGMA journal_mode=WAL").Scan(&mode); err != nil {
		t.Fatal(err)
	}
	if !strings.EqualFold(mode, "wal") {
		t.Skipf("driver refused WAL journal mode (got %q); the readWAL wiring cannot be pinned on this host", mode)
	}
	if _, err := db.Exec("CREATE TABLE probe(id INTEGER PRIMARY KEY, payload BLOB)"); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 64; i++ {
		if _, err := db.Exec("INSERT INTO probe(payload) VALUES (?)", make([]byte, 4096)); err != nil {
			t.Fatal(err)
		}
	}

	log, err := os.Create(f.logPathFor(f.nextRun()))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = log.Close() }()
	logBytes := strings.Repeat("daemon log line\n", 64)
	if _, err := log.WriteString(logBytes); err != nil {
		t.Fatal(err)
	}

	// A real, live, own child: the process reader needs a process, and the pid
	// wiring is only proven by a pid that is not the zero an absent child gives.
	child := exec.Command("/bin/sleep", "60")
	if err := child.Start(); err != nil {
		t.Skipf("cannot start a probe child on this host: %v", err)
	}
	defer func() {
		_ = child.Process.Kill()
		_ = child.Wait()
	}()
	f.setChild(child, make(chan error, 1), func() {}, log)

	var out bytes.Buffer
	sampler := sustainedIONewRunSampler(f, &out, time.Hour)
	sampler.SetPhase("P_wiring")
	sample := sampler.Sample()

	if len(sample.Unwired) != 0 {
		t.Fatalf("sustainedIONewRunSampler left %v unwired; the production wiring is the only thing that connects the instrument to the measured process and store", sample.Unwired)
	}
	if sample.PID == 0 || sample.PID != child.Process.Pid {
		t.Fatalf("pid wiring: sample pid %d, want the live child %d", sample.PID, child.Process.Pid)
	}
	if storeBytes := issue767FileSize(f.store); sample.StoreBytes == 0 || sample.StoreBytes != storeBytes {
		t.Fatalf("readSize wiring: store_bytes %d, want the store's own %d", sample.StoreBytes, storeBytes)
	}
	if sample.LogBytes != int64(len(logBytes)) {
		t.Fatalf("readSize wiring: log_bytes %d, want the child log's %d", sample.LogBytes, len(logBytes))
	}
	if !sample.WALPresent || sample.WALBytes == 0 {
		t.Fatalf("readWAL wiring: present=%v wal_bytes=%d, want the store's live WAL", sample.WALPresent, sample.WALBytes)
	}
	if walBytes := issue767FileSize(f.store + "-wal"); sample.WALBytes != walBytes {
		t.Fatalf("readSize wiring: wal_bytes %d, want %d", sample.WALBytes, walBytes)
	}
	if sample.Phase != "P_wiring" {
		t.Fatalf("sample carries phase %q, want the label SetPhase installed", sample.Phase)
	}
	// readIO is wired (nothing is named unwired above). Whether this host lets
	// it answer is a separate, named condition: an error here is the reader
	// reporting, not the reader missing.
	if sample.Error != "" {
		t.Logf("process reader wired but unavailable on this host: %s", sample.Error)
	} else if runtime.GOOS == "darwin" && sample.LogicalWrites == nil {
		t.Fatal("readIO wiring: the darwin reader answered without ri_logical_writes, the harness's primary series")
	}

	var decoded sustainedIOSample
	if err := json.Unmarshal([]byte(strings.TrimSpace(out.String())), &decoded); err != nil {
		t.Fatalf("sample line is not JSON: %v (%s)", err, out.String())
	}
	if decoded.PID != sample.PID || decoded.StoreBytes != sample.StoreBytes || decoded.Phase != sample.Phase {
		t.Fatalf("the written sample line lost a series: %+v", decoded)
	}
}

// TestSustainedIOExecuteLabelsItsSamplesWithThePhaseItIsRunning pins the other half of
// the phase attribution: the run-level call that tells the sampler which phase
// its ticks belong to. Without it every sample in a run carries "init", and
// every per-phase series in samples.ndjson silently becomes unattributable —
// while the phase reports, which take their deltas from Samples()/Resets()
// counters rather than from the labels, stay exactly as green as before.
func TestSustainedIOExecuteLabelsItsSamplesWithThePhaseItIsRunning(t *testing.T) {
	root := t.TempDir()
	f := &issue767Fixture{t: t, binary: filepath.Join(root, "no-such-gortex"), root: root, primary: filepath.Join(root, "repo"), store: filepath.Join(root, "store.sqlite")}
	if err := os.MkdirAll(f.primary, 0o700); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", testdsn.FileURI(f.store, ""))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	db.SetMaxOpenConns(1)

	var out bytes.Buffer
	sampler := newSustainedIOSampler(&out, time.Hour)
	sampler.readSize = func() sustainedIOFileSizes { return sustainedIOFileSizes{Store: 1} }
	sampler.readWAL = func() (sustainedIOWALHeader, error) { return sustainedIOWALHeader{}, nil }
	sampler.pid = func() int { return 0 }
	sampler.readIO = func() (issue767ProcessIO, error) { return issue767ProcessIO{}, errSustainedIONoChild }
	run := &sustainedIORun{t: t, f: f, arm: "candidate", artifactDir: t.TempDir(), db: db, sampler: sampler}

	var duringPhase string
	run.execute(sustainedIOPhase{
		Name:   "P4_amend_same_tree",
		Detail: "a phase that samples itself",
		Run: func(r *sustainedIORun) {
			duringPhase = r.sampler.Sample().Phase
		},
	})
	if duringPhase != "P4_amend_same_tree" {
		t.Fatalf("a sample taken inside the phase is labelled %q, want the phase's own name", duringPhase)
	}
	run.execute(sustainedIOPhase{
		Name:   "P8_idle_warm",
		Detail: "the next phase relabels the series",
		Run:    func(r *sustainedIORun) { duringPhase = r.sampler.Sample().Phase },
	})
	if duringPhase != "P8_idle_warm" {
		t.Fatalf("the second phase's sample is labelled %q; the label does not follow the phase", duringPhase)
	}
}

// TestSustainedIOClosePhaseFilesAFailedPhaseAndFinishStillWritesTheRun is the
// artifact-durability regression: a phase that fails must not take the
// run-level artifact with it.
// It drives closePhase and finish the way the failure defer in execute does,
// with no test handle available to fail on.
func TestSustainedIOClosePhaseFilesAFailedPhaseAndFinishStillWritesTheRun(t *testing.T) {
	artifacts := t.TempDir()
	root := t.TempDir()
	f := &issue767Fixture{t: t, binary: filepath.Join(root, "no-such-gortex"), root: root, primary: filepath.Join(root, "repo"), store: filepath.Join(root, "store.sqlite")}
	if err := os.MkdirAll(f.primary, 0o700); err != nil {
		t.Fatal(err)
	}
	// The closing half reads the store census, so it needs a database handle;
	// an empty one exercises the same "name what is missing" path.
	db, err := sql.Open("sqlite", testdsn.FileURI(f.store, ""))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	db.SetMaxOpenConns(1)
	run := &sustainedIORun{t: t, f: f, arm: "candidate", artifactDir: artifacts, db: db, sampler: newSustainedIOSampler(io.Discard, time.Hour)}
	report := sustainedIOPhaseReport{Phase: "P7_dependent_untrack_retrack", Detail: "the phase that failed"}
	report.Failed = true
	run.phaseNotes = []string{"the note the failing phase left"}
	run.closePhase(&report, sustainedIOPhaseOpening{started: time.Now(), ioErr: errSustainedIONoChild})

	var filed sustainedIOPhaseReport
	sustainedIOReadJSON(t, filepath.Join(artifacts, "phase_P7_dependent_untrack_retrack.json"), &filed)
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
	run.finish(sustainedIOManifest{RunID: "unit"})
	var summary map[string]any
	sustainedIOReadJSON(t, filepath.Join(artifacts, "report.json"), &summary)
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

func sustainedIOReadJSON(t *testing.T, path string, into any) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, into); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
}

// sustainedIOFailingPhaseDirEnv switches TestSustainedIOExecuteFilesTheReportOfAFailingPhase into
// its child role. The child deliberately fails, so it has to run in its own
// process: a phase failure is a runtime.Goexit on the test goroutine, and the
// only way to assert what survives it is to let it happen for real.
const sustainedIOFailingPhaseDirEnv = "GX_SUSTAINED_IO_INTERNAL_FAILING_PHASE_DIR"

// TestSustainedIOExecuteFilesTheReportOfAFailingPhase proves the wiring, not just the
// primitive: execute's own failure path must reach closePhase. Before it did,
// a phase's t.Fatal unwound straight out of the run and everything the failing
// phase measured — and every earlier phase's totals, which only report.json
// carries — was lost with it.
func TestSustainedIOExecuteFilesTheReportOfAFailingPhase(t *testing.T) {
	if dir := os.Getenv(sustainedIOFailingPhaseDirEnv); dir != "" {
		sustainedIORunFailingPhaseChild(t, dir)
		return
	}
	dir := t.TempDir()
	child := exec.Command(os.Args[0], "-test.run=^TestSustainedIOExecuteFilesTheReportOfAFailingPhase$", "-test.v=true")
	child.Env = append(os.Environ(), sustainedIOFailingPhaseDirEnv+"="+dir)
	output, err := child.CombinedOutput()
	if err == nil {
		t.Fatalf("the child test was supposed to fail its phase:\n%s", output)
	}
	if !strings.Contains(string(output), "deliberate phase failure") {
		t.Fatalf("the child failed for the wrong reason:\n%s", output)
	}
	var filed sustainedIOPhaseReport
	sustainedIOReadJSON(t, filepath.Join(dir, "phase_PX_failing.json"), &filed)
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
	sustainedIOReadJSON(t, filepath.Join(dir, "report.json"), &summary)
	failed, _ := summary["failed_phases"].([]any)
	if len(failed) != 1 || failed[0] != "PX_failing" {
		t.Fatalf("report.json does not name the failed phase: %v", summary["failed_phases"])
	}
	if phases, _ := summary["phases"].([]any); len(phases) != 1 {
		t.Fatalf("report.json carries %d phases, want the one that ran", len(phases))
	}
}

// sustainedIOIsolationChildEnv switches TestSustainedIORequireIsolationStopsTheCandidatePhase
// into its child role, for the same reason the failing-phase test has one: the
// thing under test ends in t.Fatal, and a t.Fatal can only be observed for real
// from outside the process it kills.
const sustainedIOIsolationChildEnv = "GX_SUSTAINED_IO_INTERNAL_ISOLATION_DIR"

// TestSustainedIORequireIsolationStopsTheCandidatePhase pins the wiring between the
// isolation rule and the phase, not just the rule.
//
// sustainedIOIsolationOutcome returning fatal=true is worth nothing if requireIsolation
// does not act on it: the exemption would then be universal in practice while
// the rule's own unit test stayed green, which is precisely how a gate gets
// retired without anything going red. So this drives the real call — baseline
// arm first, which must survive and record, then candidate arm, which must take
// the phase down — and reads both filed phase reports back.
func TestSustainedIORequireIsolationStopsTheCandidatePhase(t *testing.T) {
	if dir := os.Getenv(sustainedIOIsolationChildEnv); dir != "" {
		sustainedIORunIsolationChild(t, dir)
		return
	}
	dir := t.TempDir()
	child := exec.Command(os.Args[0], "-test.run=^TestSustainedIORequireIsolationStopsTheCandidatePhase$", "-test.v=true")
	child.Env = append(os.Environ(), sustainedIOIsolationChildEnv+"="+dir)
	output, err := child.CombinedOutput()
	if err == nil {
		t.Fatalf("a leak on the candidate arm did not fail the phase:\n%s", output)
	}
	if !strings.Contains(string(output), "ISOLATION VIOLATION") || !strings.Contains(string(output), "gate 5") {
		t.Fatalf("the child failed for the wrong reason:\n%s", output)
	}

	var recorded sustainedIOPhaseReport
	sustainedIOReadJSON(t, filepath.Join(dir, "phase_PB_isolation.json"), &recorded)
	if recorded.Failed {
		t.Error("the baseline arm's leak stopped its phase; the baseline would end at P5 with nothing to compare")
	}
	if !sustainedIONotesContain(recorded.Notes, "BASELINE ISOLATION VIOLATION") {
		t.Errorf("the baseline arm's leak was not recorded in its phase report: %v", recorded.Notes)
	}

	var failed sustainedIOPhaseReport
	sustainedIOReadJSON(t, filepath.Join(dir, "phase_PC_isolation.json"), &failed)
	if !failed.Failed {
		t.Error("the candidate arm's phase report does not record the isolation failure")
	}
	if !sustainedIONotesContain(failed.Notes, "ISOLATION VIOLATION") {
		t.Errorf("the candidate arm's leak was not recorded in its phase report: %v", failed.Notes)
	}
}

func sustainedIONotesContain(notes []string, want string) bool {
	for _, note := range notes {
		if strings.Contains(note, want) {
			return true
		}
	}
	return false
}

func sustainedIORunIsolationChild(t *testing.T, dir string) {
	root := t.TempDir()
	f := &issue767Fixture{t: t, binary: filepath.Join(root, "no-such-gortex"), root: root, primary: filepath.Join(root, "repo"), store: filepath.Join(root, "store.sqlite")}
	if err := os.MkdirAll(f.primary, 0o700); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", testdsn.FileURI(f.store, ""))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	db.SetMaxOpenConns(1)
	newRun := func(arm string) *sustainedIORun {
		return &sustainedIORun{t: t, f: f, arm: arm, artifactDir: dir, db: db, sampler: newSustainedIOSampler(io.Discard, time.Hour)}
	}
	// The baseline arm records the leak and keeps going.
	baseline := newRun("baseline")
	sustainedIOWithRunArtifacts(baseline, sustainedIOManifest{RunID: "isolation-baseline"}, func() {
		baseline.execute(sustainedIOPhase{Name: "PB_isolation", Detail: "a baseline leak", Run: func(r *sustainedIORun) {
			r.requireIsolation("main-only symbol X in dependent wt01", true)
		}})
	})
	if len(baseline.reports) != 1 || baseline.reports[0].Failed {
		t.Fatalf("the baseline arm did not complete its phase: %+v", baseline.reports)
	}
	// The candidate arm does not.
	candidate := newRun("candidate")
	sustainedIOWithRunArtifacts(candidate, sustainedIOManifest{RunID: "isolation-candidate"}, func() {
		candidate.execute(sustainedIOPhase{Name: "PC_isolation", Detail: "a candidate leak", Run: func(r *sustainedIORun) {
			r.requireIsolation("main-only symbol X in dependent wt01", true)
		}})
	})
	t.Fatal("the candidate arm's isolation violation did not stop its phase")
}

func sustainedIORunFailingPhaseChild(t *testing.T, dir string) {
	root := t.TempDir()
	f := &issue767Fixture{t: t, binary: filepath.Join(root, "no-such-gortex"), root: root, primary: filepath.Join(root, "repo"), store: filepath.Join(root, "store.sqlite")}
	if err := os.MkdirAll(f.primary, 0o700); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", testdsn.FileURI(f.store, ""))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	db.SetMaxOpenConns(1)
	run := &sustainedIORun{t: t, f: f, arm: "candidate", artifactDir: dir, db: db, sampler: newSustainedIOSampler(io.Discard, time.Hour)}
	sustainedIOWithRunArtifacts(run, sustainedIOManifest{RunID: "failing-phase-child"}, func() {
		run.execute(sustainedIOPhase{
			Name:   "PX_failing",
			Detail: "a phase that fails after doing measurable work",
			Run: func(r *sustainedIORun) {
				r.note("the note the failing phase left")
				time.Sleep(10 * time.Millisecond)
				r.t.Fatal("deliberate phase failure")
			},
		})
	})
	t.Fatal("execute returned from a phase that called t.Fatal")
}

// ----------------------------------------------- instrument unit tests ---

// TestSustainedIOWindowPlanIsTheDeclaredSubWindowSplit pins the sub-window names and,
// just as importantly, that the PHASE names did not move.
//
// The frozen budgets.json of the paired protocol is keyed by phase name, and
// the post-fix re-measurement reuses it. Splitting P4 or the idle phases into
// new phases would orphan three frozen ceilings; splitting them into named
// sub-windows of the same phases costs none.
func TestSustainedIOWindowPlanIsTheDeclaredSubWindowSplit(t *testing.T) {
	plan := sustainedIOPhasePlan(sustainedIOConfig{Fixture: sustainedIOFixtureSpec{}.normalize(), Idle: time.Minute})
	names := map[string]bool{}
	for _, phase := range plan {
		names[phase.Name] = true
	}
	for phase, windows := range sustainedIOWindowPlan() {
		if !names[phase] {
			t.Fatalf("the window plan names phase %q, which the phase plan does not contain", phase)
		}
		if len(windows) != 2 {
			t.Fatalf("phase %q declares %d windows", phase, len(windows))
		}
	}
	for _, want := range [][2]string{
		{"P4_amend_same_tree", sustainedIOWindowCommitTreeChange},
		{"P4_amend_same_tree", sustainedIOWindowAmendSameTree},
		{"P1_idle_cold", sustainedIOWindowIdleColdQuiet},
		{"P1_idle_cold", sustainedIOWindowIdleColdPolling},
		{"P8_idle_warm", sustainedIOWindowIdleWarmQuiet},
		{"P8_idle_warm", sustainedIOWindowIdleWarmPolling},
	} {
		found := false
		for _, window := range sustainedIOWindowPlan()[want[0]] {
			if window == want[1] {
				found = true
			}
		}
		if !found {
			t.Fatalf("%s does not declare window %s", want[0], want[1])
		}
	}
	// The commit window must come first: it is what the amend needs a clean
	// tree from, and a reader has to see the order the bytes were produced in.
	if sustainedIOWindowPlan()["P4_amend_same_tree"][0] != sustainedIOWindowCommitTreeChange {
		t.Fatal("the tree-changing commit must be the first window of P4")
	}
	// The POLLING arm must come first, in both idle phases: it is the arm the
	// frozen ceiling is applied to, and the frozen window opened at the idle
	// phase's own start. Measuring the quiet arm first pushes the judged window
	// one full cfg.Idle away from the preceding phase and lets the un-judged
	// arm absorb its decaying tail — a one-directional bias in the candidate's
	// favour on the only two phases whose ceiling is live.
	for _, phase := range []struct{ name, first string }{
		{"P1_idle_cold", sustainedIOWindowIdleColdPolling},
		{"P8_idle_warm", sustainedIOWindowIdleWarmPolling},
	} {
		if got := sustainedIOWindowPlan()[phase.name][0]; got != phase.first {
			t.Fatalf("%s measures %s first; the polling arm carries the frozen ceiling and must open the phase", phase.name, got)
		}
		if sustainedIOJudgedWindows[phase.name] != phase.first {
			t.Fatalf("%s is judged on %s but measures %s first", phase.name, sustainedIOJudgedWindows[phase.name], phase.first)
		}
	}
	// The a/b suffix is the measurement order, so the judged arm is the `a`
	// window. A name that says `b` while running first is an artifact that
	// contradicts itself.
	for _, window := range sustainedIOWindowPlan() {
		if !strings.Contains(window[0], "a_") || !strings.Contains(window[1], "b_") {
			t.Fatalf("the sub-window names no longer encode their order: %v", window)
		}
	}
}

// TestSustainedIOCounterActivitySumsOnlyTheNamedFamilies pins the settle predicate's
// input: labelled series are matched by family prefix, and nothing else counts.
func TestSustainedIOCounterActivitySumsOnlyTheNamedFamilies(t *testing.T) {
	counters := map[string]int64{
		"views_dedicated_base_publication_total{outcome=published}": 1,
		"views_dedicated_base_publication_total{outcome=skipped}":   2,
		"views_dedicated_base_publish_total{shape=root}":            1,
		"views_generation_published_total{owner=checkout}":          3,
		"views_request_served_total{kind=base}":                     99,
		"views_family_discovery_lag_seconds|count":                  42,
	}
	if got := sustainedIOCounterActivity(counters, sustainedIOIndexSettleCounters); got != 7 {
		t.Fatalf("activity = %d, want the 7 of the publication families alone", got)
	}
	if got := sustainedIOCounterActivity(nil, sustainedIOIndexSettleCounters); got != 0 {
		t.Fatalf("an empty scrape = %d", got)
	}
}

// TestSustainedIOAwaitCountersQuietEndsOnAStreakAndNeverOnAFailedRead is the guard
// the cold-index settle needs: the phase holds until the publication family stops
// moving, and a scrape that could not be read is not a quiet scrape.
//
// The 6,000-file arm's `shape=root` publication had not finished when the phase
// ended at the first exact answer, so index-time work was booked to a phase
// named idle and read as a 7.44x idle regression.
func TestSustainedIOAwaitCountersQuietEndsOnAStreakAndNeverOnAFailedRead(t *testing.T) {
	series := []struct {
		counters map[string]int64
		err      error
	}{
		{counters: map[string]int64{"views_dedicated_base_publish_total{shape=root}": 0}},
		{counters: map[string]int64{"views_dedicated_base_publish_total{shape=root}": 1}},
		{err: errors.New("daemon status unavailable")},
		{counters: map[string]int64{"views_dedicated_base_publish_total{shape=root}": 1}},
		{counters: map[string]int64{"views_dedicated_base_publish_total{shape=root}": 1}},
		{counters: map[string]int64{"views_dedicated_base_publish_total{shape=root}": 1}},
		{counters: map[string]int64{"views_dedicated_base_publish_total{shape=root}": 1}},
	}
	index := 0
	read := func() (map[string]int64, error) {
		if index >= len(series) {
			return series[len(series)-1].counters, series[len(series)-1].err
		}
		entry := series[index]
		index++
		return entry.counters, entry.err
	}
	slept := 0
	result := sustainedIOAwaitCountersQuiet(read, sustainedIOIndexSettleCounters, time.Millisecond, 3,
		time.Now().Add(time.Minute), func(time.Duration) { slept++ })
	if !result.Settled {
		t.Fatalf("the wait did not settle: %+v", result)
	}
	if result.Polls != 6 {
		t.Fatalf("polls = %d; want 6 — the failed read resets the streak, so the three quiet reads after it are what ends the wait (%+v)", result.Polls, result)
	}
	if result.Unreadable != 1 || len(result.Errors) != 1 {
		t.Fatalf("the unreadable scrape was not named: %+v", result)
	}
	if result.Activity != 1 || result.Moved != 1 {
		t.Fatalf("the movement was not recorded: %+v", result)
	}
	if slept == 0 {
		t.Fatal("the wait never yielded between scrapes")
	}

	// A family that never goes quiet hits the deadline and says so — it does
	// not pretend to have settled.
	moving := int64(0)
	busy := func() (map[string]int64, error) {
		moving++
		return map[string]int64{"views_dedicated_base_publish_total{shape=root}": moving}, nil
	}
	timed := sustainedIOAwaitCountersQuiet(busy, sustainedIOIndexSettleCounters, time.Millisecond, 3,
		time.Now().Add(20*time.Millisecond), func(d time.Duration) { time.Sleep(d) })
	if timed.Settled {
		t.Fatalf("a family that never stops moving must not report settled: %+v", timed)
	}
}

// TestSustainedIOValidateReconcileIntervalNamesTheProductDefault pins the reconcile-interval
// contract: the janitor interval is an option, the paired protocol's 5 s is the default, and
// the product's own default is selectable by name so a confirmatory arm can be
// run at all.
func TestSustainedIOValidateReconcileIntervalNamesTheProductDefault(t *testing.T) {
	for raw, want := range map[string]string{
		"":        sustainedIODefaultReconcileInterval,
		"default": issue767ProductReconcileInterval,
		"product": issue767ProductReconcileInterval,
		"30s":     "30s",
		"1h":      "1h",
	} {
		got, err := sustainedIOValidateReconcileInterval(raw)
		if err != nil || got != want {
			t.Errorf("sustainedIOValidateReconcileInterval(%q) = %q, %v; want %q", raw, got, err, want)
		}
	}
	for _, raw := range []string{"nonsense", "500ms", "48h"} {
		if _, err := sustainedIOValidateReconcileInterval(raw); err == nil {
			t.Errorf("sustainedIOValidateReconcileInterval(%q) was accepted", raw)
		}
	}
	cfg, err := sustainedIOConfigFromEnv(func(name string) string {
		if name == "GX_SUSTAINED_IO_RECONCILE_INTERVAL" {
			return "default"
		}
		return ""
	})
	if err != nil || cfg.ReconcileInterval != issue767ProductReconcileInterval {
		t.Fatalf("config reconcile interval = %q, %v", cfg.ReconcileInterval, err)
	}
	if defaults, err := sustainedIOConfigFromEnv(func(string) string { return "" }); err != nil || defaults.ReconcileInterval != "5s" {
		t.Fatalf("the default protocol interval moved: %q, %v", defaults.ReconcileInterval, err)
	}
	if note := sustainedIOReconcileNote(issue767ProductReconcileInterval); !strings.Contains(note, "product default") {
		t.Fatalf("the manifest note does not state the product default: %q", note)
	}
	if note := sustainedIOReconcileNote("5s"); !strings.Contains(note, "GORTEX_RECONCILE_INTERVAL=5s") || !strings.Contains(note, "not default-configuration") {
		t.Fatalf("the manifest note does not state the accelerated janitor: %q", note)
	}
}

// TestIssue767FixtureOmitsTheReconcileVariableForTheProductDefault proves the
// option reaches the daemon's environment, which is the only place it can
// change what is measured.
func TestIssue767FixtureOmitsTheReconcileVariableForTheProductDefault(t *testing.T) {
	tiny := func(f *issue767Fixture) {
		f.write(filepath.Join(f.primary, "go.mod"), "module example.invalid/issue767\n\ngo 1.24\n")
		f.write(filepath.Join(f.primary, "marker.go"), issue767MarkerSource("GxOptionMarker"))
	}
	binary := filepath.Join(t.TempDir(), "unused-gortex")
	countReconcile := func(env []string) []string {
		var found []string
		for _, entry := range env {
			if strings.HasPrefix(entry, "GORTEX_RECONCILE_INTERVAL=") {
				found = append(found, entry)
			}
		}
		return found
	}
	def := newIssue767FixtureWithCorpus(t, binary, tiny)
	if got := countReconcile(def.env); len(got) != 1 || got[0] != "GORTEX_RECONCILE_INTERVAL=5s" {
		t.Fatalf("default fixture env carries %v, want exactly the protocol's 5s", got)
	}
	custom := newIssue767FixtureWithCorpus(t, binary, tiny, issue767WithReconcileInterval("30s"))
	if got := countReconcile(custom.env); len(got) != 1 || got[0] != "GORTEX_RECONCILE_INTERVAL=30s" {
		t.Fatalf("custom fixture env carries %v", got)
	}
	product := newIssue767FixtureWithCorpus(t, binary, tiny, issue767WithReconcileInterval(issue767ProductReconcileInterval))
	if got := countReconcile(product.env); len(got) != 0 {
		t.Fatalf("the product-default arm must not set the variable at all: %v", got)
	}
}

// TestSustainedIOSourceIdentityResolvesTheHarnessTreeNotTheProcessWorkingDirectory is
// the revert-red of the source-identity correction.
//
// A measurement run executes the compiled test binary from a private root under
// /tmp, where `git rev-parse HEAD` answers nothing — which is why every frozen
// manifest in the paired1500 artifact set carries an empty source_commit and an
// empty
// dirty_tree_digest. Resolving the tree from the compiled-in path of the
// harness source fixes it; running git in the process's working directory does
// not, and this test fails in that case.
func TestSustainedIOSourceIdentityResolvesTheHarnessTreeNotTheProcessWorkingDirectory(t *testing.T) {
	dir, note := sustainedIOHarnessSourceDir()
	if dir == "" {
		t.Skipf("the harness source path does not resolve here: %s", note)
	}
	if _, err := os.Stat(filepath.Join(dir, "sustained_io_integration_test.go")); err != nil {
		t.Fatalf("the resolved harness dir %s is not this package: %v", dir, err)
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not available")
	}
	// Stand somewhere that is emphatically not a work tree, exactly as a
	// measurement run does.
	elsewhere := t.TempDir()
	t.Chdir(elsewhere)
	if _, _, err := sustainedIOGitIdentity(elsewhere); err == nil {
		t.Skip("the temporary directory is inside a git work tree; this machine cannot distinguish the two")
	}
	commit, _, notes := sustainedIOSourceIdentity()
	if commit == "" {
		t.Fatalf("the manifest would record no source commit from %s: %v", elsewhere, notes)
	}
	if len(commit) != 40 {
		t.Fatalf("source commit %q is not a resolved object id", commit)
	}
	// A directory that is not a work tree must be named as such rather than
	// producing a silent empty identity.
	if _, _, notes := sustainedIOGitIdentity(elsewhere); len(notes) == 0 {
		t.Fatal("a non-worktree directory produced an empty identity with no explanation")
	}
}

// TestSustainedIOCensusDeltaNamesEveryWriterThatMoved pins the per-writer attribution
// an idle floor is read off: the sidecar and the query log by name, including
// a bucket that disappeared.
func TestSustainedIOCensusDeltaNamesEveryWriterThatMoved(t *testing.T) {
	before := sustainedIOCensus{Bytes: map[string]int64{sustainedIOBucketSidecar: 100, sustainedIOBucketQueryLog: 10, sustainedIOBucketStore: 5, sustainedIOBucketDaemonLog: 7}}
	after := sustainedIOCensus{Bytes: map[string]int64{sustainedIOBucketSidecar: 137, sustainedIOBucketQueryLog: 365, sustainedIOBucketStore: 5}}
	delta := sustainedIOCensusDelta(before, after)
	if delta[sustainedIOBucketSidecar] != 37 || delta[sustainedIOBucketQueryLog] != 355 {
		t.Fatalf("delta = %+v", delta)
	}
	if _, ok := delta[sustainedIOBucketStore]; ok {
		t.Fatalf("an unchanged writer must not appear: %+v", delta)
	}
	if delta[sustainedIOBucketDaemonLog] != -7 {
		t.Fatalf("a writer that vanished must be reported: %+v", delta)
	}
	if sustainedIOCensusDelta(before, before) != nil {
		t.Fatal("an unchanged census must produce no delta map at all")
	}
}

// sustainedIOStubFixture builds a fixture whose "daemon binary" is this native test executable, so
// the harness's own client-facing behaviour — how many calls it makes, how it
// reacts to an inexact answer — can be exercised without a daemon.
//
// It deliberately does NOT go through newIssue767FixtureWithCorpus: that
// recipe builds a git repository and a config for a real daemon. What is under
// test here is the harness, not the product.
func sustainedIOStubFixture(t *testing.T, mode string) *issue767Fixture {
	t.Helper()
	root := t.TempDir()
	primary := filepath.Join(root, "repo")
	if err := os.MkdirAll(primary, 0o700); err != nil {
		t.Fatal(err)
	}
	binary := portableHarnessStubBinary(t)
	f := &issue767Fixture{t: t, binary: binary, root: root, primary: primary,
		linked: filepath.Join(root, "linked"), store: filepath.Join(root, "store.sqlite"), env: append(os.Environ(), portableHarnessModeEnv+"="+mode)}
	if err := os.WriteFile(f.markerFileForTest(primary), []byte("package fixture\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return f
}

// markerFileForTest is the marker path the stub answers out of.
func (f *issue767Fixture) markerFileForTest(root string) string {
	return filepath.Join(root, "marker.go")
}

// sustainedIOStubRun wires a run around a stub fixture with a sampler that never ticks.
func sustainedIOStubRun(t *testing.T, f *issue767Fixture, arm string) *sustainedIORun {
	t.Helper()
	return &sustainedIORun{t: t, f: f, arm: arm, artifactDir: t.TempDir(),
		cfg:       sustainedIOConfig{Idle: 2 * time.Second, ReconcileInterval: sustainedIODefaultReconcileInterval},
		sampler:   newSustainedIOSampler(io.Discard, time.Hour),
		revisions: map[int]int{}, lastProbe: sustainedIOPrimaryMarker}
}

// TestSustainedIOIdleQuietArmMakesNoClientCallsAtAll is the revert-red of the quiet-idle-arm correction.
//
// `idle()` has always issued one read-only `call search` every 5 s and the
// result was reported as an idle floor. Twelve daemon round trips per 60 s
// window are not a floor: each one costs a durable 37,080 B sidecar
// transaction plus a query-log append. The quiet arm is what answers "what
// does this daemon write when nobody asks it anything"; if it makes any client
// call at all, it is measuring the other question.
func TestSustainedIOIdleQuietArmMakesNoClientCallsAtAll(t *testing.T) {
	f := sustainedIOStubFixture(t, "search")
	f.env = append(f.env, "GX_SUSTAINED_IO_STUB_MARKER="+f.markerFileForTest(f.primary))
	run := sustainedIOStubRun(t, f, "candidate")

	before := f.clientCallCount()
	run.idleAs(2*time.Second, sustainedIOIdleQuiet)
	if got := f.clientCallCount() - before; got != 0 {
		t.Fatalf("the quiet idle arm made %d client calls; a floor with client traffic in it is not a floor", got)
	}
	before = f.clientCallCount()
	run.idleAs(2*time.Second, sustainedIOIdlePolling)
	if got := f.clientCallCount() - before; got != 1 {
		t.Fatalf("the polling idle arm made %d client calls, want 1 in a 2s window at a %s interval", got, sustainedIOIdlePollInterval)
	}
	quiet, polling := false, false
	for _, note := range run.phaseNotes {
		if strings.Contains(note, "idle arm quiet") && strings.Contains(note, "0 harness read-only searches") {
			quiet = true
		}
		if strings.Contains(note, "idle arm polling") && strings.Contains(note, "1 harness read-only searches") {
			polling = true
		}
	}
	if !quiet || !polling {
		t.Fatalf("the arms did not record their own client traffic: %v", run.phaseNotes)
	}
}

// TestSustainedIOIdleWindowsHoldTheFrozenIdleDurationAndScrapeNothing is the repair of
// the idle split, and it is two claims at once.
//
// The split measured idle twice but left the frozen ceiling applied to the sum:
// P1/P8 spanned 2 x cfg.Idle where the frozen baseline spanned one, and each
// sub-window bracket added a `daemon status` round trip — four of them — inside
// a phase the frozen protocol measured with none. Both halves inflate a
// post-fix idle row by construction, and neither is visible in the number.
//
// The repair keeps each arm at the FULL cfg.Idle, so the polling arm remains
// the frozen measurement byte for byte and can be judged against it 1:1
// (sustainedIOJudgedWindows), and opens both arms scrape-free, so the only client
// traffic inside an idle phase is the polling arm's own searches.
func TestSustainedIOIdleWindowsHoldTheFrozenIdleDurationAndScrapeNothing(t *testing.T) {
	f := sustainedIOStubFixture(t, "search")
	f.env = append(f.env, "GX_SUSTAINED_IO_STUB_MARKER="+f.markerFileForTest(f.primary))
	run := sustainedIOStubRun(t, f, "candidate")
	run.cfg.Idle = time.Second
	run.phase = "P8_idle_warm"

	before := f.clientCallCount()
	run.idleWindows(sustainedIOWindowIdleWarmQuiet, sustainedIOWindowIdleWarmPolling, run.cfg.Idle)
	calls := f.clientCallCount() - before

	// One poll, from the polling arm. A scraping bracket would add two per
	// window: an idle phase cannot be instrumented with the very traffic it
	// exists to measure the absence of.
	if calls != 1 {
		t.Fatalf("the idle phase made %d client calls; only the polling arm's own searches may happen inside it", calls)
	}
	if len(run.windows) != 2 {
		t.Fatalf("the idle split filed %d window rows", len(run.windows))
	}
	// The polling arm runs FIRST: it is the arm the frozen ceiling is applied
	// to, and the frozen window opened at the idle phase's own start. With the
	// quiet arm first, the judged window opens one full cfg.Idle later than the
	// number it is compared against and the un-judged arm eats the preceding
	// phase's decaying tail — a bias that only ever lowers the judged reading.
	polling, quiet := run.windows[0], run.windows[1]
	if polling.Window != sustainedIOWindowIdleWarmPolling || quiet.Window != sustainedIOWindowIdleWarmQuiet {
		t.Fatalf("the arms are out of order: %s then %s; the judged (polling) arm must open the phase",
			run.windows[0].Window, run.windows[1].Window)
	}
	if quiet.ClientCalls != 0 || polling.ClientCalls != 1 {
		t.Fatalf("client traffic is misattributed: quiet=%d polling=%d", quiet.ClientCalls, polling.ClientCalls)
	}
	// Each arm holds the whole configured idle. Halving them to keep the
	// phase's wall at cfg.Idle would make the judged arm a 30 s window judged
	// against a frozen 60 s ceiling — the same error in the other direction.
	// The floor is generous against scheduler jitter and still well above the
	// half-duration a split arm would report.
	floor := 0.8 * run.cfg.Idle.Seconds()
	for _, window := range run.windows {
		if window.WallSeconds < floor {
			t.Fatalf("%s held %.3fs of the configured %s; each idle arm holds the full duration",
				window.Window, window.WallSeconds, run.cfg.Idle)
		}
	}
	for _, window := range run.windows {
		if window.CountersError != sustainedIONoScrapeInsideWindow {
			t.Fatalf("%s does not record why it carries no viewmetrics delta: %q", window.Window, window.CountersError)
		}
		if len(window.CountersDelta) != 0 {
			t.Fatalf("%s carries a viewmetrics delta, so its bracket scraped: %+v", window.Window, window.CountersDelta)
		}
	}
	if sustainedIOJudgedWindows["P8_idle_warm"] != sustainedIOWindowIdleWarmPolling ||
		sustainedIOJudgedWindows["P1_idle_cold"] != sustainedIOWindowIdleColdPolling {
		t.Fatalf("the frozen idle phases must be judged on the polling arm: %+v", sustainedIOJudgedWindows)
	}
	named, ordered := false, false
	for _, note := range run.phaseNotes {
		if strings.Contains(note, sustainedIOWindowIdleWarmPolling) && strings.Contains(note, "ceiling is applied to") {
			named = true
		}
		if strings.Contains(note, sustainedIOWindowIdleWarmPolling+" ran first") {
			ordered = true
		}
	}
	if !named {
		t.Fatalf("the phase does not say which arm carries the frozen ceiling: %v", run.phaseNotes)
	}
	if !ordered {
		t.Fatalf("the phase does not record that the judged arm opened it: %v", run.phaseNotes)
	}
}

// TestSustainedIOIdleBudgetIsAppliedToEachIdleArmNotToTheirSum pins the operator's idle
// byte budget against the same doubling.
//
// The budget names bytes per idle window. Applied to a phase that now measures
// idle twice, the sum would fail an arm that is inside its budget — and
// doubling the number to compensate would let one arm write twice what the
// operator allowed.
func TestSustainedIOIdleBudgetIsAppliedToEachIdleArmNotToTheirSum(t *testing.T) {
	value := func(v uint64) *uint64 { return &v }
	report := &sustainedIOPhaseReport{
		Phase: "P8_idle_warm", LogicalWrites: value(900),
		Windows: []sustainedIOWindowReport{
			{Phase: "P8_idle_warm", Window: sustainedIOWindowIdleWarmQuiet, LogicalWrites: value(400)},
			{Phase: "P8_idle_warm", Window: sustainedIOWindowIdleWarmPolling, LogicalWrites: value(500)},
		},
	}
	breaches, note := sustainedIOIdleBudgetBreaches(report, 600)
	if len(breaches) != 0 {
		t.Fatalf("two arms inside the per-window budget were failed on their sum: %v", breaches)
	}
	if !strings.Contains(note, "each of the 2 idle windows") {
		t.Fatalf("the report does not record how the budget was applied: %q", note)
	}
	breaches, _ = sustainedIOIdleBudgetBreaches(report, 450)
	if len(breaches) != 1 || !strings.Contains(breaches[0], sustainedIOWindowIdleWarmPolling) {
		t.Fatalf("the arm over the per-window budget was not named: %v", breaches)
	}
	// A phase that cut no windows is still judged whole, exactly as before.
	plain := &sustainedIOPhaseReport{Phase: "P1_idle_cold", LogicalWrites: value(900)}
	if breaches, _ := sustainedIOIdleBudgetBreaches(plain, 600); len(breaches) != 1 {
		t.Fatalf("a phase with no windows must still be judged as a whole: %v", breaches)
	}
	if breaches, note := sustainedIOIdleBudgetBreaches(&sustainedIOPhaseReport{Phase: "P1_idle_cold"}, 600); len(breaches) != 0 ||
		!strings.Contains(note, "ri_logical_writes unavailable") {
		t.Fatalf("a missing series must be named, never passed by default: %v / %q", breaches, note)
	}
}

// TestSustainedIOInWindowFilesARowWithItsOwnClientCallsAndCheckpointBytes pins the
// sub-window envelope, including the path that matters most: a window whose
// body fails still files its row, because the numbers of a failing window are
// exactly the ones a reader wants.
func TestSustainedIOInWindowFilesARowWithItsOwnClientCallsAndCheckpointBytes(t *testing.T) {
	f := sustainedIOStubFixture(t, "noop")
	run := sustainedIOStubRun(t, f, "candidate")
	run.phase = "P4_amend_same_tree"

	run.inWindow(sustainedIOWindowCommitTreeChange, "the commit", func() {
		_, _ = f.tryCommand(5*time.Second, f.primary, "noop")
		_, _ = f.tryCommand(5*time.Second, f.primary, "noop")
		run.accountWait(1.5)
	})
	run.inWindow(sustainedIOWindowAmendSameTree, "the amend", func() {})
	if len(run.windows) != 2 {
		t.Fatalf("windows = %+v", run.windows)
	}
	commit := run.windows[0]
	if commit.Phase != "P4_amend_same_tree" || commit.Window != sustainedIOWindowCommitTreeChange {
		t.Fatalf("the window row is not labelled with its phase: %+v", commit)
	}
	if commit.ClientCalls != 2 {
		t.Fatalf("the commit window counted %d client calls, want the 2 it made", commit.ClientCalls)
	}
	if commit.ExactnessWaits != 1 || commit.ExactnessSeconds != 1.5 {
		t.Fatalf("the window did not account its own wait: %+v", commit)
	}
	amend := run.windows[1]
	if amend.ClientCalls != 0 || amend.ExactnessWaits != 0 {
		t.Fatalf("the amend window inherited the commit window's traffic: %+v", amend)
	}
	if run.windowOpen {
		t.Fatal("a window outlived its body")
	}
}

// TestSustainedIOScrapingBracketTakesBothScrapesAndReportsATrueDelta pins the `true`
// direction of openBracketScraping, which nothing pinned before.
//
// The scrape-free bracket added for the idle arms made the opening scrape
// conditional. Only the `false` direction was covered — by absence checks — so
// a bracket that stopped taking its OPENING scrape passed the whole suite. The
// failure shape is what makes that gap expensive: with opening.counters nil and
// opening.countersErr nil, closeWindow and closePhase take the `default` arm
// and publish sustainedIOCounterDelta(nil, after) — the daemon's ABSOLUTE counters
// presented as a per-window delta. Silently wrong, not visibly missing, in
// exactly the series the idle split nominates as the compensation for the
// window scrapes it removed.
//
// The stub advances one counter per invocation, so a bracket that took both
// scrapes reports a delta of 1 and a bracket that took only the closing one
// reports the running total.
func TestSustainedIOScrapingBracketTakesBothScrapesAndReportsATrueDelta(t *testing.T) {
	const series = "views_generation_published_total"
	f := sustainedIOStubFixture(t, "counter")
	f.env = append(f.env, "GX_SUSTAINED_IO_STUB_COUNT="+filepath.Join(t.TempDir(), "count"))
	run := sustainedIOStubRun(t, f, "candidate")
	run.phase = "P4_amend_same_tree"

	run.inWindow(sustainedIOWindowCommitTreeChange, "a scraping window", func() {})
	run.inScrapeFreeWindow(sustainedIOWindowAmendSameTree, "a scrape-free window", func() {})
	if len(run.windows) != 2 {
		t.Fatalf("windows = %+v", run.windows)
	}

	scraping := run.windows[0]
	if scraping.CountersError != "" {
		t.Fatalf("a scraping window reported a counter error against a stub that answers: %q", scraping.CountersError)
	}
	if len(scraping.CountersDelta) != 1 || scraping.CountersDelta[series] != 1 {
		t.Fatalf("a scraping window's counter delta is %+v, want %s=1 (one stub tick between the opening and the closing scrape); "+
			"a delta the size of the running total means the OPENING scrape was skipped and an absolute reading was published as a delta",
			scraping.CountersDelta, series)
	}
	// The bracket's own two round trips are instrumentation, not the window's
	// traffic: the opening scrape is taken before the client-call mark and the
	// closing one after it.
	if scraping.ClientCalls != 0 {
		t.Fatalf("a scraping window charged itself %d client calls; its own scrapes sit outside its measured span", scraping.ClientCalls)
	}

	free := run.windows[1]
	if free.CountersError != sustainedIONoScrapeInsideWindow {
		t.Fatalf("a scrape-free window does not name why it carries no delta: %q", free.CountersError)
	}
	if len(free.CountersDelta) != 0 {
		t.Fatalf("a scrape-free window published a counter delta, so its bracket scraped: %+v", free.CountersDelta)
	}
	if free.ClientCalls != 0 {
		t.Fatalf("a scrape-free window made %d client calls", free.ClientCalls)
	}
}

// TestSustainedIOProbeIsolationRetriesAnInexactAnswerAndJudgesOnlyAJudgeableOne is the
// revert-red of the probe-isolation correction.
//
// The probe it replaces was single-shot and t.Fatal'd on any error, while
// issue767Verdict turns any inexact answer into one. A `base_changed` rider on
// a dependent read that overlaps a committed-base advance is truthful and
// self-healing — 3 of 14 single shots at 6,000 files, clearing in 0.45–1.31 s —
// and the single shot turned it into a failed phase that took P6, P7 and P8
// with it. A single-shot probe fails this test on the first answer.
func TestSustainedIOProbeIsolationRetriesAnInexactAnswerAndJudgesOnlyAJudgeableOne(t *testing.T) {
	f := sustainedIOStubFixture(t, "inexact-then-empty")
	counter := filepath.Join(f.root, "stub-count")
	f.env = append(f.env, "GX_SUSTAINED_IO_STUB_COUNT="+counter)
	run := sustainedIOStubRun(t, f, "candidate")
	run.phase = "P5_main_advance"
	dependent := filepath.Join(f.root, "wt01")
	if err := os.MkdirAll(dependent, 0o700); err != nil {
		t.Fatal(err)
	}

	run.probeIsolation("main-only symbol GxAdvanceMarker in dependent wt01",
		dependent, "GxAdvance001Marker", filepath.Join(dependent, "marker.go"))

	if len(run.phaseIsolation) != 1 {
		t.Fatalf("the probe filed %d records", len(run.phaseIsolation))
	}
	record := run.phaseIsolation[0]
	if !record.Judged || record.Leaked {
		t.Fatalf("a judgeable, non-leaking answer was judged as %+v", record)
	}
	if record.Outcome != "isolation held" {
		t.Fatalf("outcome = %q", record.Outcome)
	}
	if record.Probe.Polls != 3 {
		t.Fatalf("the probe asked %d times; two inexact answers must be retried, not judged", record.Probe.Polls)
	}
	if run.waits != 1 || run.phaseWaits != 1 || run.waitSeconds <= 0 {
		t.Fatalf("the probe's wait was not accounted: waits=%d phase=%d seconds=%.3f", run.waits, run.phaseWaits, run.waitSeconds)
	}
	// The whole answer travels with the verdict, in the phase note and in the
	// diagnostics envelope.
	joined := strings.Join(run.phaseNotes, "\n")
	for _, want := range []string{"exact_label=true", "fallback=false", "asks=3", "answered_by="} {
		if !strings.Contains(joined, want) {
			t.Fatalf("the phase note does not carry %q: %s", want, joined)
		}
	}
	diagnostics := sustainedIOCollectDiagnostics("P5_main_advance", func(...string) string { return "" }, "", nil, run.phaseIsolation)
	if len(diagnostics.Isolation) != 1 || diagnostics.Isolation[0].Probe.Polls != 3 {
		t.Fatalf("the diagnostics envelope lost the isolation answer: %+v", diagnostics.Isolation)
	}
}

// TestIssue767VerdictSeparatesRetryableInexactnessFromAWrongAnswer pins the
// distinction the bounded probe rests on — and pins that it is a distinction,
// not a relaxation: an automatic checkout that does not prove exact freshness
// is still not a pass, and an answer out of the wrong file is still a flat
// failure that no amount of retrying may turn into one.
func TestIssue767VerdictSeparatesRetryableInexactnessFromAWrongAnswer(t *testing.T) {
	fallback := issue767Answer{Fallback: true}
	found, err := issue767Verdict(fallback, issue767AsAutomaticWorktree)
	if found || err == nil || !issue767Inexact(err) {
		t.Fatalf("a fallback answer = %v, %v", found, err)
	}
	inexact := issue767Answer{Found: true, FromExpectedFile: true, Exact: false}
	found, err = issue767Verdict(inexact, issue767AsAutomaticWorktree)
	if found || err == nil || !issue767Inexact(err) {
		t.Fatalf("an answer with no exact label = %v, %v", found, err)
	}
	wrongFile := issue767Answer{Found: true, Exact: true, FromExpectedFile: false}
	found, err = issue767Verdict(wrongFile, issue767AsAutomaticWorktree)
	if found || err == nil || issue767Inexact(err) {
		t.Fatalf("an answer from the wrong file must be a flat failure: %v, %v", found, err)
	}
	// The own-corpus spelling never demanded the exact label and still does not.
	ok := issue767Answer{Found: true, FromExpectedFile: true}
	if found, err := issue767Verdict(ok, issue767AsOwnCorpus); !found || err != nil {
		t.Fatalf("own-corpus answer = %v, %v", found, err)
	}
}

// TestSustainedIOAwaitIndexTimeWorkSettledAccountsItsWaitOnTheRun exercises the
// cold-index hold through the run object that calls it, with a stub daemon
// whose publication family is already quiet.
func TestSustainedIOAwaitIndexTimeWorkSettledAccountsItsWaitOnTheRun(t *testing.T) {
	f := sustainedIOStubFixture(t, "settled")
	run := sustainedIOStubRun(t, f, "candidate")
	run.phase = "P0_cold_index"
	before := f.clientCallCount()
	run.awaitIndexTimeWorkSettled()
	if run.waits != 1 || run.phaseWaits != 1 {
		t.Fatalf("the settle wait was not accounted: waits=%d phase=%d", run.waits, run.phaseWaits)
	}
	if calls := f.clientCallCount() - before; calls < sustainedIOIndexSettleQuiet {
		t.Fatalf("the hold scraped the publication family %d times, want at least the %d that prove a streak", calls, sustainedIOIndexSettleQuiet)
	}
	settled := false
	for _, note := range run.phaseNotes {
		if strings.Contains(note, "index-time settle: settled=true") {
			settled = true
		}
	}
	if !settled {
		t.Fatalf("the phase does not record what the hold observed: %v", run.phaseNotes)
	}
}

// TestSustainedIOPhaseBodiesReachTheInstrumentTheyAreNamedFor is the wiring pin.
//
// Every instrument in this item is reachable and unit-tested on its own; what
// a unit test of a primitive cannot show is that the phase which is supposed to
// use it does. These phases run only under an opt-in daemon workload, so the
// production path cannot be exercised here — but it can be read, and a phase
// that stops calling its instrument is exactly the regression that produced
// the artifacts this item exists to correct.
func TestSustainedIOPhaseBodiesReachTheInstrumentTheyAreNamedFor(t *testing.T) {
	dir, note := sustainedIOHarnessSourceDir()
	if dir == "" {
		t.Skipf("the harness source path does not resolve here: %s", note)
	}
	path := filepath.Join(dir, "sustained_io_integration_test.go")
	source, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	bodies, err := sustainedIOFunctionBodies(path, source)
	if err != nil {
		t.Fatal(err)
	}
	for name, wants := range map[string][]string{
		// The cold index is held open until index-time work is quiet, so a
		// publication the daemon start scheduled is booked to P0 and not to
		// the phase named idle.
		"phaseColdIndex": {"awaitIndexTimeWorkSettled()"},
		// The tree-changing commit and the same-tree amend are bracketed
		// separately, in that order. The wants are call-shaped on purpose: a
		// body that still mentions the window constants but no longer brackets
		// with them is exactly the regression this phase was split to prevent.
		"phaseAmendSameTree": {"r.inWindow(sustainedIOWindowCommitTreeChange", "r.inWindow(sustainedIOWindowAmendSameTree", "commit\", \"--amend\""},
		// Both idle phases measure a quiet arm and a polling arm.
		"phaseIdleCold": {"idleWindows(sustainedIOWindowIdleColdQuiet, sustainedIOWindowIdleColdPolling"},
		"phaseIdleWarm": {"idleWindows(sustainedIOWindowIdleWarmQuiet, sustainedIOWindowIdleWarmPolling"},
		// Both idle arms are bracketed scrape-free and each holds the whole
		// configured idle: a scraping bracket puts four daemon round trips
		// inside the phase, and a halved arm is no longer the frozen window.
		"idleWindows": {"r.inScrapeFreeWindow(quietWindow", "r.inScrapeFreeWindow(pollingWindow",
			"r.idleAs(duration, sustainedIOIdleQuiet)", "r.idleAs(duration, sustainedIOIdlePolling)"},
		// Both isolation questions go through the bounded probe.
		"phaseMainAdvance":    {"r.probeIsolation("},
		"phaseDependentEdits": {"r.probeIsolation("},
		// The janitor interval reaches the measured daemon.
		"sustainedIORunWorkload": {"issue767WithReconcileInterval(cfg.ReconcileInterval)", "sustainedIOSourceIdentity()"},
	} {
		body, ok := bodies[name]
		if !ok {
			t.Errorf("no function %s in %s", name, path)
			continue
		}
		for _, want := range wants {
			if !strings.Contains(body, want) {
				t.Errorf("%s no longer reaches %q", name, want)
			}
		}
	}
	// The single-shot isolation query the bounded probe replaced must not come
	// back into either phase.
	for _, name := range []string{"phaseMainAdvance", "phaseDependentEdits"} {
		if strings.Contains(bodies[name], "trySearchSymbolIn(") {
			t.Errorf("%s judges a single unbounded shot again", name)
		}
	}
	// A scraping bracket inside an idle phase is the regression the scrape-free
	// window exists to prevent; it must not come back by name either.
	if strings.Contains(bodies["idleWindows"], "r.inWindow(") {
		t.Error("idleWindows brackets an idle arm with a scraping window again")
	}
	// Each arm holds the configured idle in full. Halving them would keep the
	// phase's wall at 60 s and silently make the judged arm a 30 s window.
	for _, halved := range []string{"duration/2", "duration / 2"} {
		if strings.Contains(bodies["idleWindows"], halved) {
			t.Errorf("idleWindows splits the frozen idle duration between the arms (%q); the polling arm must stay the frozen window", halved)
		}
	}
	// The judged (polling) arm is bracketed FIRST in the source, not just
	// declared first in sustainedIOWindowPlan. Position inside the run is part of the
	// measurement: the frozen window opened at the idle phase's own start, and
	// an arm that opens one full cfg.Idle later reads lower for a reason that
	// has nothing to do with the daemon.
	polling := strings.Index(bodies["idleWindows"], "r.inScrapeFreeWindow(pollingWindow")
	quiet := strings.Index(bodies["idleWindows"], "r.inScrapeFreeWindow(quietWindow")
	switch {
	case polling < 0 || quiet < 0:
		t.Errorf("idleWindows no longer brackets both arms scrape-free (polling=%d quiet=%d)", polling, quiet)
	case quiet < polling:
		t.Error("idleWindows measures the quiet arm before the polling one; the judged arm must open the phase where the frozen window opened")
	}
}

// sustainedIOFunctionBodies maps every top-level func (and method) in a file to its
// source text.
func sustainedIOFunctionBodies(path string, source []byte) (map[string]string, error) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, source, 0)
	if err != nil {
		return nil, err
	}
	bodies := map[string]string{}
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			continue
		}
		start := fset.Position(fn.Body.Pos()).Offset
		end := fset.Position(fn.Body.End()).Offset
		if start < 0 || end > len(source) || start >= end {
			continue
		}
		bodies[fn.Name.Name] = string(source[start:end])
	}
	return bodies, nil
}
