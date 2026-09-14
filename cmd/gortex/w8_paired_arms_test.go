package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

// The paired-arm reduction: two arms' run artifacts in, frozen budgets and one
// verdict out.
//
// The sustained harness (w8_sustained_io_integration_test.go) measures ONE arm
// per subtest and files one report.json per arm-repetition. Nothing in it
// compares arms, and nothing in it can: the protocol requires the baseline's
// budgets to be fixed before the candidate's numbers are read, and a function
// that has both in hand cannot prove it looked at them in that order.
//
// This file is that missing half, and the order is enforced by construction:
//
//   - w8FreezeBudgets takes ONLY baseline runs and refuses anything else, so a
//     budget can never be derived from a number the candidate produced;
//   - w8WriteFrozenBudgets refuses to overwrite an existing budgets.json and
//     returns the frozen one instead, so a second pass — after the candidate
//     has been seen — cannot quietly move the goalposts;
//   - the budget set carries a digest of its own canonical bytes, so a verdict
//     can cite which budgets it was judged against and a reader can recompute
//     it.
//
// Every number here is a reduction of numbers the harness measured. It invents
// none, and it substitutes nothing: a phase whose primary series is missing on
// an arm is named as unavailable rather than folded in as a zero, because the
// whole point of the exercise is a comparison between two write series and a
// zero is a legitimate value of one.

// ----------------------------------------------------------- run artifacts ---

// w8RunArtifact is one arm-repetition's report.json, as (*w8Run).finish writes
// it.
type w8RunArtifact struct {
	Manifest             w8Manifest      `json:"manifest"`
	Phases               []w8PhaseReport `json:"phases"`
	FailedPhases         []string        `json:"failed_phases"`
	RetainedCensus       w8Census        `json:"retained_census"`
	WorktreesRemoved     int             `json:"worktrees_removed"`
	WorktreesRetained    []string        `json:"worktrees_retained"`
	Samples              int             `json:"samples"`
	SampleFailures       int             `json:"sample_failures"`
	WALResetsTotal       int             `json:"wal_resets_total"`
	CheckpointBytesTotal uint64          `json:"wal_checkpoint_bytes"`
	ClientCallsTotal     int             `json:"client_calls_total"`
	ReconcileInterval    string          `json:"reconcile_interval,omitempty"`
	ExactnessWaitsTotal  int             `json:"exactness_waits_total"`
	ExactnessWaitSeconds float64         `json:"exactness_wait_seconds"`

	// Dir is where this artifact was read from. It is not part of the JSON;
	// it is what a verdict cites so a number can be walked back to its run.
	Dir string `json:"-"`

	// CheckpointSource says where each phase's wal_checkpoint_bytes came
	// from — the run itself, or this reduction re-deriving it from the run's
	// samples.ndjson. Every artifact frozen before the series existed takes
	// the second path, and a reader is told so rather than left to assume the
	// run measured it.
	CheckpointSource string `json:"-"`
}

// w8LoadArmRuns reads every `<arm>_rep<N>/report.json` under dir, in repetition
// order. A directory that names the arm but carries no report is an error: a
// silently skipped repetition would change a median without changing anything
// a reader can see.
func w8LoadArmRuns(dir, arm string) ([]w8RunArtifact, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	type candidate struct {
		name string
		path string
	}
	var found []candidate
	for _, entry := range entries {
		if !entry.IsDir() || !strings.HasPrefix(entry.Name(), arm+"_rep") {
			continue
		}
		found = append(found, candidate{entry.Name(), filepath.Join(dir, entry.Name(), "report.json")})
	}
	if len(found) == 0 {
		return nil, fmt.Errorf("no %s_rep* run directories under %s", arm, dir)
	}
	sort.Slice(found, func(i, j int) bool { return found[i].name < found[j].name })
	runs := make([]w8RunArtifact, 0, len(found))
	for _, entry := range found {
		data, err := os.ReadFile(entry.path)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", entry.name, err)
		}
		var run w8RunArtifact
		if err := json.Unmarshal(data, &run); err != nil {
			return nil, fmt.Errorf("%s: %w", entry.name, err)
		}
		run.Dir = filepath.Dir(entry.path)
		if run.Manifest.Arm != "" && run.Manifest.Arm != arm {
			return nil, fmt.Errorf("%s: report claims arm %q", entry.name, run.Manifest.Arm)
		}
		w8FillCheckpointSeries(&run)
		runs = append(runs, run)
	}
	return runs, nil
}

// w8FillCheckpointSeries makes sure every phase of a run carries the
// checkpoint-excluded series, deriving it from the run's own samples.ndjson
// when the report predates the field.
//
// This is the whole re-reading of the frozen verdict. The samples were taken
// during the run, before any candidate number was read, and the rule applied
// to them is fixed in w8CheckpointSeries — so deriving the series afterwards
// moves no goalpost: it reads a series that was already recorded, out of bytes
// that were already frozen.
func w8FillCheckpointSeries(run *w8RunArtifact) {
	missing := false
	for _, report := range run.Phases {
		if report.LogicalWritesExcl == nil && report.LogicalWrites != nil {
			missing = true
			break
		}
	}
	if !missing {
		run.CheckpointSource = "measured by the run"
		return
	}
	samples, skipped, err := w8ReadSamples(filepath.Join(run.Dir, "samples.ndjson"))
	if err != nil {
		run.CheckpointSource = "unavailable: " + err.Error()
		return
	}
	series := w8CheckpointSeries(samples)
	run.CheckpointSource = fmt.Sprintf("re-derived from %d samples (%d unparsed, %d checkpoint intervals)", series.Samples, skipped, series.Attributed)
	for i := range run.Phases {
		report := &run.Phases[i]
		if report.LogicalWritesExcl != nil {
			continue
		}
		report.CheckpointBytes = series.Phases[report.Phase]
		report.LogicalWritesExcl = w8ExcludeCheckpoint(report.LogicalWrites, report.CheckpointBytes)
		for j := range report.Windows {
			window := &report.Windows[j]
			if window.LogicalWritesExcl != nil {
				continue
			}
			window.CheckpointBytes = series.Windows[w8WindowKey(report.Phase, window.Window)]
			window.LogicalWritesExcl = w8ExcludeCheckpoint(window.LogicalWrites, window.CheckpointBytes)
		}
	}
}

// ------------------------------------------------------------- statistics ---

// w8Stat is one series across repetitions. Unavailable names the repetitions
// that carried no value; N counts only the ones that did.
type w8Stat struct {
	N           int      `json:"n"`
	Median      uint64   `json:"median"`
	Min         uint64   `json:"min"`
	Max         uint64   `json:"max"`
	Values      []uint64 `json:"values"`
	Unavailable []string `json:"unavailable,omitempty"`
}

// OK reports whether the statistic rests on at least one real reading.
func (s w8Stat) OK() bool { return s.N > 0 }

// w8Statistic reduces one series. A nil reading is named, never zeroed: three
// repetitions of which one lost its process counters is a median over two
// readings plus a named gap, not a median over a 0.
//
// The median of an even count is the upper of the two middle values. With the
// protocol's three repetitions this never arises; it is fixed here so a
// four-repetition run is still reproducible from the definition.
func w8Statistic(labels []string, values []*uint64) w8Stat {
	stat := w8Stat{}
	for i, value := range values {
		label := fmt.Sprintf("rep%d", i+1)
		if i < len(labels) {
			label = labels[i]
		}
		if value == nil {
			stat.Unavailable = append(stat.Unavailable, label)
			continue
		}
		stat.Values = append(stat.Values, *value)
	}
	stat.N = len(stat.Values)
	if stat.N == 0 {
		return stat
	}
	sorted := append([]uint64(nil), stat.Values...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	stat.Min, stat.Max = sorted[0], sorted[len(sorted)-1]
	stat.Median = sorted[len(sorted)/2]
	return stat
}

func w8Uint64(value uint64) *uint64 { return &value }

// ---------------------------------------------------------------- budgets ---

// w8PhaseBudget is one phase's frozen baseline, plus the ceiling the candidate
// is judged against.
//
// LimitRule is the rule in words and travels with the number, because a bare
// ceiling in a table is unfalsifiable a week later.
type w8PhaseBudget struct {
	Phase  string `json:"phase"`
	Detail string `json:"detail,omitempty"`
	// LogicalWritesExcl and CheckpointBytes are omitted when zero so a budget
	// set frozen before these fields existed marshals to the same bytes — and
	// therefore the same digest — as it did when it was written. A frozen
	// ceiling that a later field addition invalidates is not frozen.
	LogicalWritesExcl w8Stat `json:"ri_logical_writes_excl_checkpoint,omitzero"`
	CheckpointBytes   w8Stat `json:"wal_checkpoint_bytes,omitzero"`
	JudgedSeries      string `json:"judged_series,omitempty"`
	// JudgedSource is the row the ceiling was read off: the phase, or the
	// sub-window that reproduces the frozen measurement. It is omitted for the
	// phase row so a budget set frozen before sub-windows existed marshals —
	// and digests — exactly as it did.
	JudgedSource    string  `json:"judged_source,omitempty"`
	JudgedWall      float64 `json:"judged_wall_s,omitempty"`
	LogicalWrites   w8Stat  `json:"ri_logical_writes"`
	DiskWritten     w8Stat  `json:"ri_diskio_byteswritten"`
	WALBytesAfter   w8Stat  `json:"wal_bytes_after"`
	WALResets       w8Stat  `json:"wal_resets"`
	StoreBytesAfter w8Stat  `json:"store_bytes_after"`
	GenerationsMax  w8Stat  `json:"generation_max_after"`
	GenerationSeq   w8Stat  `json:"generation_sequence_after"`
	WallSeconds     float64 `json:"wall_s_median"`
	Limit           uint64  `json:"ri_logical_writes_limit,omitempty"`
	LimitRule       string  `json:"limit_rule"`
	// Windows are the baseline's sub-window rows. They are omitted when the
	// phase cut none, so a budget set frozen before sub-windows existed
	// marshals — and digests — exactly as it did.
	Windows []w8WindowBudget `json:"windows,omitempty"`
}

// w8WindowBudget is one sub-window's frozen baseline. It carries no ceiling:
// a sub-window is a finer reading of a phase whose ceiling already exists, and
// inventing a second ceiling for it would be inventing a second contract.
type w8WindowBudget struct {
	Window            string  `json:"window"`
	Detail            string  `json:"detail,omitempty"`
	LogicalWrites     w8Stat  `json:"ri_logical_writes"`
	LogicalWritesExcl w8Stat  `json:"ri_logical_writes_excl_checkpoint"`
	CheckpointBytes   w8Stat  `json:"wal_checkpoint_bytes"`
	ClientCalls       w8Stat  `json:"client_calls"`
	WallSeconds       float64 `json:"wall_s_median,omitempty"`
}

// w8BudgetSet is everything frozen from the baseline arm.
type w8BudgetSet struct {
	FrozenAt      string          `json:"frozen_at"`
	Arm           string          `json:"arm"`
	Runs          []string        `json:"runs"`
	BinarySHA256  string          `json:"binary_sha256"`
	FixtureDigest string          `json:"fixture_digest"`
	Phases        []w8PhaseBudget `json:"phases"`
	RetainedBytes w8Stat          `json:"retained_bytes"`
	RetainedLimit uint64          `json:"retained_bytes_limit"`
	Notes         []string        `json:"notes,omitempty"`
	// AugmentedFrom is the digest the frozen file carried before a derived
	// series was filled in from the same baseline artifacts. It is empty for a
	// set that was frozen with every series it is judged on, which is what
	// keeps an untouched frozen file byte-identical.
	AugmentedFrom string `json:"augmented_from_digest,omitempty"`
	Digest        string `json:"digest"`
}

// The idle ceiling from the plan: 8 MiB per 60 s of idling, scaled by the
// phase's own measured length so a longer idle is not judged by a shorter
// phase's number.
const (
	w8IdleBudgetPerMinute = 8 << 20
	// The headline claim: main advancement under ten dependents at most half
	// the baseline's process-accounted writes.
	w8MainAdvanceFactor = 0.5
	// Retained storage after teardown, at most 1.2x the baseline's.
	w8RetainedFactor = 1.2
	// A candidate phase above this multiple of the baseline is preserved as a
	// regression whether or not the phase carries a ceiling.
	w8RegressionFactor = 1.10
)

var w8IdlePhases = map[string]bool{"P1_idle_cold": true, "P8_idle_warm": true}

// w8FreezeBudgets reduces the baseline arm to budgets.
//
// It refuses any run that is not the baseline arm. That refusal is the whole
// mechanism: a budget derived — even partly, even accidentally — from the
// candidate's own numbers is not a budget, it is a description.
func w8FreezeBudgets(runs []w8RunArtifact) (w8BudgetSet, error) {
	if len(runs) == 0 {
		return w8BudgetSet{}, errors.New("no baseline runs to freeze budgets from")
	}
	set := w8BudgetSet{
		FrozenAt:      time.Now().UTC().Format(time.RFC3339),
		Arm:           "baseline",
		BinarySHA256:  runs[0].Manifest.BinarySHA256,
		FixtureDigest: runs[0].Manifest.FixtureDigest,
	}
	for _, run := range runs {
		if run.Manifest.Arm != "baseline" {
			return w8BudgetSet{}, fmt.Errorf("run %s is arm %q; budgets are frozen from the baseline arm only", run.Dir, run.Manifest.Arm)
		}
		if run.Manifest.FixtureDigest != set.FixtureDigest {
			return w8BudgetSet{}, fmt.Errorf("run %s used fixture %s, not %s; a paired comparison needs one fixture", run.Dir, run.Manifest.FixtureDigest, set.FixtureDigest)
		}
		if len(run.FailedPhases) > 0 {
			set.Notes = append(set.Notes, fmt.Sprintf("%s failed phases %v; its numbers are frozen as measured and the failure travels with them", filepath.Base(run.Dir), run.FailedPhases))
		}
		if run.SampleFailures > 0 {
			set.Notes = append(set.Notes, fmt.Sprintf("%s took %d samples with %d failures", filepath.Base(run.Dir), run.Samples, run.SampleFailures))
		}
		set.Runs = append(set.Runs, filepath.Base(run.Dir))
	}

	for _, phase := range w8PhaseNames(runs) {
		budget := w8PhaseBudget{Phase: phase}
		labels := []string{}
		var resets, store, generations, sequence []*uint64
		for _, run := range runs {
			labels = append(labels, filepath.Base(run.Dir))
			report, found := w8FindPhase(run, phase)
			if !found {
				resets, store, generations, sequence =
					append(resets, nil), append(store, nil), append(generations, nil), append(sequence, nil)
				continue
			}
			resets = append(resets, w8Uint64(uint64(max(report.WALResets, 0))))
			store = append(store, w8Uint64(uint64(max(report.StoreBytesAfter, 0))))
			generations = append(generations, w8Uint64(uint64(max(report.StoreCensusAfter.Generations.Max, 0))))
			sequence = append(sequence, w8Uint64(uint64(max(report.StoreCensusAfter.Generations.Sequence, 0))))
		}
		// The write series go through the one reduction both arms use, so a
		// budget and the candidate row it is compared with can never have been
		// reduced by two different rules.
		series := w8ReducePhase(phase, runs)
		budget.Detail = series.Detail
		budget.LogicalWrites = series.LogicalWrites
		budget.LogicalWritesExcl = series.LogicalWritesExcl
		budget.CheckpointBytes = series.CheckpointBytes
		budget.DiskWritten = series.DiskWritten
		budget.WALBytesAfter = series.WALBytesAfter
		budget.WALResets = w8Statistic(labels, resets)
		budget.StoreBytesAfter = w8Statistic(labels, store)
		budget.GenerationsMax = w8Statistic(labels, generations)
		budget.GenerationSeq = w8Statistic(labels, sequence)
		budget.WallSeconds = series.WallSeconds
		for _, window := range series.Windows {
			budget.Windows = append(budget.Windows, w8WindowBudget{
				Window: window.Window, Detail: window.Detail,
				LogicalWrites: window.LogicalWrites, LogicalWritesExcl: window.LogicalWritesExcl,
				CheckpointBytes: window.CheckpointBytes, ClientCalls: window.ClientCalls,
				WallSeconds: window.WallSeconds,
			})
		}
		budget.JudgedSeries, budget.JudgedSource, budget.JudgedWall = w8BudgetJudgment(budget)
		budget.Limit, budget.LimitRule = w8LimitFor(phase, budget)
		set.Phases = append(set.Phases, budget)
	}

	retainedLabels := []string{}
	retained := []*uint64{}
	for _, run := range runs {
		retainedLabels = append(retainedLabels, filepath.Base(run.Dir))
		retained = append(retained, w8Uint64(uint64(max(run.RetainedCensus.TotalBytes, 0))))
	}
	set.RetainedBytes = w8Statistic(retainedLabels, retained)
	if set.RetainedBytes.OK() {
		set.RetainedLimit = uint64(float64(set.RetainedBytes.Median) * w8RetainedFactor)
	}
	set.Notes = append(set.Notes,
		"ri_logical_writes is the primary series; ri_diskio_byteswritten is recorded beside it and never alone",
		"process-accounted writes are not SSD NAND writes; a WAL size is not cumulative writes",
		"a small-fixture replay is not sustained daemon behaviour",
	)
	set.Digest = w8BudgetDigest(set)
	return set, nil
}

// w8CheckpointExcludedPhases are the phases judged on the checkpoint-excluded
// series rather than on the total.
//
// A WAL checkpoint drains frames earlier phases wrote. Which window it lands
// in is decided by an autocheckpoint threshold
// (sqliteWALAutoCheckpointPages = 8000), not by what that window did, so a
// phase that merely happens to cross the threshold is charged for the whole
// accumulated log. That is the entire P2 row of the frozen verdict: 42,607,016
// of its 58,040,984 bytes are one drain, and the edit path underneath is
// 15,433,968 against the baseline's 16,879,664 — 0.91x, inside the ceiling.
//
// The exclusion is applied ONLY to the low-activity phases. The attribution is
// per sample interval, so in a phase whose own work runs continuously (a cold
// index, ten dependents' worth of advancement) the interval that carried the
// drain carried a second of real work as well, and excluding it would flatter
// the arm rather than correct it. Those phases keep the total and report the
// checkpoint bytes beside it.
var w8CheckpointExcludedPhases = map[string]bool{
	"P2_small_edits":         true,
	"P3_touch_stage_unstage": true,
	"P4_amend_same_tree":     true,
	"P8_idle_warm":           true,
}

const (
	w8SeriesTotal    = "ri_logical_writes"
	w8SeriesExcluded = "ri_logical_writes_excl_checkpoint"
)

// w8JudgedSeriesName is which write series a phase's ceiling is applied to.
func w8JudgedSeriesName(phase string) string {
	if w8CheckpointExcludedPhases[phase] {
		return w8SeriesExcluded
	}
	return w8SeriesTotal
}

// w8JudgedStat picks the series a phase is judged on out of a budget, falling
// back to the total when the excluded series is unavailable — and saying so,
// because a phase judged on a different series than its rule names is a phase
// whose verdict cannot be read.
func w8JudgedStat(phase string, total, excluded w8Stat) (w8Stat, string) {
	if !w8CheckpointExcludedPhases[phase] {
		return total, w8SeriesTotal
	}
	if excluded.OK() {
		return excluded, w8SeriesExcluded
	}
	return total, w8SeriesTotal + " (the checkpoint-excluded series is unavailable for this phase)"
}

// w8JudgedWindows maps a frozen phase name onto the sub-window that reproduces
// the measurement its ceiling was frozen from.
//
// The frozen P1/P8 ceilings were measured over one 60 s window that polled a
// read-only search every 5 s. The workload now measures idle twice — a quiet
// arm and a polling arm, each holding the same 60 s — so the PHASE spans twice
// the frozen duration and its total is the sum of two different questions.
// Judging that sum against the frozen number would inflate every post-fix
// P1/P8 row by construction. The polling arm is the frozen measurement,
// unchanged in duration and period, so the frozen phase name is judged on it
// and the quiet arm stays an un-budgeted row of its own. An arm that carries no
// such window — every artifact measured before the split, the frozen baseline
// included — is judged on the phase, exactly as before.
var w8JudgedWindows = map[string]string{
	"P1_idle_cold": w8WindowIdleColdPolling,
	"P8_idle_warm": w8WindowIdleWarmPolling,
}

// w8JudgedSourcePhase is the source label of an arm judged on its phase row.
const w8JudgedSourcePhase = "phase"

// w8WindowStats is one arm's reading of one sub-window, in the shape the
// selection needs. A frozen budget's windows and a reduced arm's windows both
// lower into it, so neither side can be selected by a different rule.
type w8WindowStats struct {
	Window            string
	Present           bool
	LogicalWrites     w8Stat
	LogicalWritesExcl w8Stat
	WallSeconds       float64
}

// w8Judged is one arm's judged reading: which number, off which series, out of
// which row, over how long.
type w8Judged struct {
	Stat   w8Stat
	Series string
	Source string
	Wall   float64
	Note   string
}

// w8SelectJudged picks the number one arm is judged on. It is the single place
// the phase/sub-window choice is made, so a budget's ceiling and the candidate
// row it is compared with can never be taken off different rows by accident.
func w8SelectJudged(phase string, total, excluded w8Stat, wall float64, window w8WindowStats) w8Judged {
	mapped, hasMapping := w8JudgedWindows[phase]
	if hasMapping && window.Present && window.Window == mapped {
		stat, series := w8JudgedStat(phase, window.LogicalWrites, window.LogicalWritesExcl)
		if stat.OK() {
			judgedWall := window.WallSeconds
			note := fmt.Sprintf("judged on sub-window %s (%.0fs), the 1:1 counterpart of the frozen %s measurement", mapped, judgedWall, phase)
			if judgedWall <= 0 {
				judgedWall = wall
				note = fmt.Sprintf("judged on sub-window %s, the 1:1 counterpart of the frozen %s measurement; the window recorded no wall, so the phase's %.0fs is used for any duration scaling", mapped, phase, wall)
			}
			return w8Judged{Stat: stat, Series: series, Source: "window:" + mapped, Wall: judgedWall, Note: note}
		}
	}
	stat, series := w8JudgedStat(phase, total, excluded)
	judged := w8Judged{Stat: stat, Series: series, Source: w8JudgedSourcePhase, Wall: wall}
	if hasMapping {
		if window.Present {
			judged.Note = fmt.Sprintf("sub-window %s carries no usable reading; judged on the phase row instead", mapped)
		} else {
			judged.Note = fmt.Sprintf("this arm carries no %s sub-window; judged on the phase row, which is what the frozen protocol measured", mapped)
		}
	}
	return judged
}

// w8BudgetWindowStats lowers a frozen budget's sub-windows into the selection's
// shape.
func w8BudgetWindowStats(phase string, windows []w8WindowBudget) w8WindowStats {
	mapped, ok := w8JudgedWindows[phase]
	if !ok {
		return w8WindowStats{}
	}
	for _, window := range windows {
		if window.Window != mapped {
			continue
		}
		return w8WindowStats{Window: window.Window, Present: true,
			LogicalWrites: window.LogicalWrites, LogicalWritesExcl: window.LogicalWritesExcl,
			WallSeconds: window.WallSeconds}
	}
	return w8WindowStats{Window: mapped}
}

// w8SeriesWindowStats lowers a reduced arm's sub-windows into the same shape.
func w8SeriesWindowStats(phase string, windows []w8WindowSeries) w8WindowStats {
	mapped, ok := w8JudgedWindows[phase]
	if !ok {
		return w8WindowStats{}
	}
	for _, window := range windows {
		if window.Window != mapped {
			continue
		}
		return w8WindowStats{Window: window.Window, Present: true,
			LogicalWrites: window.LogicalWrites, LogicalWritesExcl: window.LogicalWritesExcl,
			WallSeconds: window.WallSeconds}
	}
	return w8WindowStats{Window: mapped}
}

// w8BudgetJudged is the baseline half of the selection, off a frozen budget.
func w8BudgetJudged(budget w8PhaseBudget) w8Judged {
	return w8SelectJudged(budget.Phase, budget.LogicalWrites, budget.LogicalWritesExcl,
		budget.WallSeconds, w8BudgetWindowStats(budget.Phase, budget.Windows))
}

// w8BudgetJudgment is what a budget records about its own selection: the
// series, and — only when the ceiling was read off a sub-window rather than the
// phase — which window and over what wall. A phase-judged budget records
// neither, so a budget set that predates sub-windows marshals, and digests,
// exactly as it did.
func w8BudgetJudgment(budget w8PhaseBudget) (string, string, float64) {
	selection := w8BudgetJudged(budget)
	if selection.Source == w8JudgedSourcePhase {
		return selection.Series, "", 0
	}
	return selection.Series, selection.Source, selection.Wall
}

// w8LimitFor is the plan's ceiling table, in one place.
func w8LimitFor(phase string, budget w8PhaseBudget) (uint64, string) {
	selection := w8BudgetJudged(budget)
	judged, series := selection.Stat, selection.Series
	if !judged.OK() {
		return 0, "no baseline " + series + " reading; this phase carries no ceiling and is reported as evidence only"
	}
	baseline := judged.Median
	checkpoint := ""
	if budget.CheckpointBytes.OK() {
		checkpoint = fmt.Sprintf("; baseline wal_checkpoint_bytes median %d is reported as its own row", budget.CheckpointBytes.Median)
	}
	source := ""
	if selection.Source != w8JudgedSourcePhase {
		source = "; baseline read off " + selection.Source
	}
	switch {
	case w8IdlePhases[phase]:
		absolute := uint64(float64(w8IdleBudgetPerMinute) * max(selection.Wall, 1) / 60)
		limit := baseline
		if absolute < limit {
			limit = absolute
		}
		return limit, fmt.Sprintf("idle on %s: min(baseline median %d, %d MiB per 60s scaled to %.0fs = %d)%s%s", series, baseline, w8IdleBudgetPerMinute>>20, selection.Wall, absolute, checkpoint, source)
	case phase == "P5_main_advance":
		return uint64(float64(baseline) * w8MainAdvanceFactor), fmt.Sprintf("headline on %s: %.2f x baseline median %d%s%s", series, w8MainAdvanceFactor, baseline, checkpoint, source)
	case phase == "P2_small_edits":
		return baseline, fmt.Sprintf("dirty edits on %s: at most the baseline median %d%s%s", series, baseline, checkpoint, source)
	default:
		return 0, fmt.Sprintf("no frozen ceiling; baseline %s median %d is recorded and a candidate above %.0f%% of it is preserved as a regression%s%s", series, baseline, w8RegressionFactor*100, checkpoint, source)
	}
}

func w8PhaseNames(runs []w8RunArtifact) []string {
	seen := map[string]bool{}
	names := []string{}
	for _, run := range runs {
		for _, report := range run.Phases {
			if !seen[report.Phase] {
				seen[report.Phase] = true
				names = append(names, report.Phase)
			}
		}
	}
	return names
}

func w8FindPhase(run w8RunArtifact, phase string) (w8PhaseReport, bool) {
	for _, report := range run.Phases {
		if report.Phase == phase {
			return report, true
		}
	}
	return w8PhaseReport{}, false
}

func w8MedianFloat(values []float64) float64 {
	if len(values) == 0 {
		return 0
	}
	sorted := append([]float64(nil), values...)
	sort.Float64s(sorted)
	return sorted[len(sorted)/2]
}

// w8BudgetDigest is the canonical digest of a budget set, computed over the set
// with its own Digest field cleared.
func w8BudgetDigest(set w8BudgetSet) string {
	set.Digest = ""
	data, err := json.Marshal(set)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// w8WriteFrozenBudgets writes budgets.json if it does not exist, and otherwise
// returns what is already frozen there, untouched.
//
// This is the freeze. The protocol's requirement is an ordering claim —
// "budgets were fixed before the candidate was read" — and an ordering claim
// that rests on the operator's memory is not evidence. A file that refuses to
// be rewritten turns it into something a reader can check: the frozen bytes,
// their digest, and their mtime.
func w8WriteFrozenBudgets(path string, set w8BudgetSet) (w8BudgetSet, bool, error) {
	if data, err := os.ReadFile(path); err == nil {
		var existing w8BudgetSet
		if err := json.Unmarshal(data, &existing); err != nil {
			return w8BudgetSet{}, false, fmt.Errorf("%s exists but is not a budget set: %w", path, err)
		}
		return existing, false, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return w8BudgetSet{}, false, err
	}
	data, err := json.MarshalIndent(set, "", "  ")
	if err != nil {
		return w8BudgetSet{}, false, err
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o600); err != nil {
		return w8BudgetSet{}, false, err
	}
	return set, true, nil
}

// w8StatEqual compares two reductions exactly, readings and named gaps alike.
func w8StatEqual(a, b w8Stat) bool {
	if a.N != b.N || a.Median != b.Median || a.Min != b.Min || a.Max != b.Max {
		return false
	}
	if len(a.Values) != len(b.Values) || len(a.Unavailable) != len(b.Unavailable) {
		return false
	}
	for i := range a.Values {
		if a.Values[i] != b.Values[i] {
			return false
		}
	}
	for i := range a.Unavailable {
		if a.Unavailable[i] != b.Unavailable[i] {
			return false
		}
	}
	return true
}

// w8AugmentFrozenBudgets fills the derived checkpoint series onto a budget set
// that was frozen before the series existed, from the SAME baseline artifacts
// the budgets were frozen from.
//
// This is not a re-freeze and it cannot become one. The only fields it writes
// are derived from bytes that were already frozen — samples.ndjson was written
// during the baseline run, before any candidate number existed — and every
// series the file already carries has to come back byte-identical from the
// runs it is handed, otherwise the augmentation is refused outright. A set
// whose totals do not reproduce is a set being shown different runs, and no
// ceiling may be recomputed from those.
//
// The frozen FILE is never rewritten. What changes is what this process holds:
// the derived series, the ceiling recomputed over the series each phase's rule
// names, and a note saying so, with the pre-augmentation digest kept beside the
// new one.
func w8AugmentFrozenBudgets(set w8BudgetSet, runs []w8RunArtifact) (w8BudgetSet, error) {
	if digest := w8BudgetDigest(set); digest != set.Digest {
		return set, fmt.Errorf("budget set digest %s does not match its contents %s; the frozen budgets were edited", set.Digest, digest)
	}
	needed := false
	for _, budget := range set.Phases {
		if budget.LogicalWrites.OK() && !budget.LogicalWritesExcl.OK() {
			needed = true
			break
		}
	}
	if !needed {
		return set, nil
	}
	if len(runs) == 0 {
		return set, errors.New("the frozen budgets carry no checkpoint-excluded series and no baseline runs were supplied to derive it from")
	}
	for _, run := range runs {
		if run.Manifest.Arm != "baseline" {
			return set, fmt.Errorf("run %s is arm %q; the derived series is filled from the baseline arm only", run.Dir, run.Manifest.Arm)
		}
	}
	augmented := set
	augmented.Phases = append([]w8PhaseBudget(nil), set.Phases...)
	for i := range augmented.Phases {
		budget := &augmented.Phases[i]
		series := w8ReducePhase(budget.Phase, runs)
		if !w8StatEqual(series.LogicalWrites, budget.LogicalWrites) {
			return set, fmt.Errorf("phase %s reduces to a different ri_logical_writes series than the frozen one (%+v vs %+v); these are not the runs the budgets were frozen from",
				budget.Phase, series.LogicalWrites, budget.LogicalWrites)
		}
		budget.LogicalWritesExcl = series.LogicalWritesExcl
		budget.CheckpointBytes = series.CheckpointBytes
		budget.Windows = nil
		for _, window := range series.Windows {
			budget.Windows = append(budget.Windows, w8WindowBudget{
				Window: window.Window, Detail: window.Detail,
				LogicalWrites: window.LogicalWrites, LogicalWritesExcl: window.LogicalWritesExcl,
				CheckpointBytes: window.CheckpointBytes, ClientCalls: window.ClientCalls,
				WallSeconds: window.WallSeconds,
			})
		}
		budget.JudgedSeries, budget.JudgedSource, budget.JudgedWall = w8BudgetJudgment(*budget)
		budget.Limit, budget.LimitRule = w8LimitFor(budget.Phase, *budget)
	}
	augmented.Notes = append(append([]string(nil), set.Notes...),
		"the checkpoint-excluded series was derived after the freeze from the same baseline runs' samples.ndjson; every frozen total reproduced exactly, and the frozen file was not rewritten")
	augmented.AugmentedFrom = set.Digest
	augmented.Digest = w8BudgetDigest(augmented)
	return augmented, nil
}

// ---------------------------------------------------------------- verdict ---

// w8PhaseVerdict is one phase's paired outcome.
type w8PhaseVerdict struct {
	Phase string `json:"phase"`
	// JudgedSeries names which of the two write series the ceiling was applied
	// to, and Ratio is that series' ratio. RatioTotal keeps the total-series
	// ratio beside it so the correction is visible rather than substituted:
	// P2 reads 3.44x on the total and 0.91x on the excluded series, and both
	// numbers are true statements about different things.
	JudgedSeries string `json:"judged_series"`
	// BaselineSource / CandidateSource name the row each arm's judged number
	// was read off — "phase", or "window:<name>" when the arm carries the
	// sub-window that reproduces the frozen measurement. They are not
	// decoration: a post-split candidate is judged on its polling window while
	// a pre-split frozen baseline is judged on its phase, and a reader has to
	// be able to see that the two are the same 60 s question rather than
	// assume it. BaselineWallJudged / CandidateWallJudged are those rows' own
	// walls, so "1:1 in duration" is checkable instead of asserted.
	BaselineSource      string  `json:"baseline_judged_source"`
	CandidateSource     string  `json:"candidate_judged_source"`
	BaselineWallJudged  float64 `json:"baseline_judged_wall_s,omitempty"`
	CandidateWallJudged float64 `json:"candidate_judged_wall_s,omitempty"`
	JudgedNote          string  `json:"judged_note,omitempty"`
	// BaselineJudged / CandidateJudged are the two numbers the ratio and the
	// ceiling were actually applied to, kept on the row so the rendered table
	// cannot re-derive a different pair than the verdict used.
	BaselineJudged      w8Stat  `json:"baseline_judged"`
	CandidateJudged     w8Stat  `json:"candidate_judged"`
	BaselineExcl        w8Stat  `json:"baseline_ri_logical_writes_excl_checkpoint"`
	CandidateExcl       w8Stat  `json:"candidate_ri_logical_writes_excl_checkpoint"`
	BaselineCheckpoint  w8Stat  `json:"baseline_wal_checkpoint_bytes"`
	CandidateCheckpoint w8Stat  `json:"candidate_wal_checkpoint_bytes"`
	RatioTotal          float64 `json:"ratio_total_series,omitempty"`
	// Windows are the phase's sub-windows, recorded with both arms' numbers.
	// No frozen ceiling names them, so they are never judged — a window row is
	// evidence, and a reader is told so.
	Windows []w8WindowVerdict `json:"windows,omitempty"`

	Baseline       w8Stat  `json:"baseline_ri_logical_writes"`
	Candidate      w8Stat  `json:"candidate_ri_logical_writes"`
	BaselineDisk   w8Stat  `json:"baseline_ri_diskio_byteswritten"`
	CandidateDisk  w8Stat  `json:"candidate_ri_diskio_byteswritten"`
	BaselineWAL    w8Stat  `json:"baseline_wal_bytes_after"`
	CandidateWAL   w8Stat  `json:"candidate_wal_bytes_after"`
	Ratio          float64 `json:"ratio,omitempty"`
	Limit          uint64  `json:"limit,omitempty"`
	LimitRule      string  `json:"limit_rule"`
	WithinBudget   bool    `json:"within_budget"`
	Regressed      bool    `json:"regressed_over_10pct"`
	Comparable     bool    `json:"comparable"`
	Incomparable   string  `json:"incomparable_reason,omitempty"`
	CandidateWall  float64 `json:"candidate_wall_s_median"`
	BaselineWall   float64 `json:"baseline_wall_s_median"`
	CounterSummary string  `json:"candidate_counters,omitempty"`
}

// w8WindowVerdict is one sub-window's paired row. Sub-windows exist so a
// phase's own stimulus is separable from the thing the phase is named after;
// they carry no frozen ceiling and are recorded, never judged.
type w8WindowVerdict struct {
	Window              string  `json:"window"`
	Detail              string  `json:"detail,omitempty"`
	Baseline            w8Stat  `json:"baseline_ri_logical_writes"`
	Candidate           w8Stat  `json:"candidate_ri_logical_writes"`
	BaselineExcl        w8Stat  `json:"baseline_ri_logical_writes_excl_checkpoint"`
	CandidateExcl       w8Stat  `json:"candidate_ri_logical_writes_excl_checkpoint"`
	BaselineCheckpoint  w8Stat  `json:"baseline_wal_checkpoint_bytes"`
	CandidateCheckpoint w8Stat  `json:"candidate_wal_checkpoint_bytes"`
	BaselineCalls       w8Stat  `json:"baseline_client_calls"`
	CandidateCalls      w8Stat  `json:"candidate_client_calls"`
	Ratio               float64 `json:"ratio,omitempty"`
	Note                string  `json:"note"`
}

// w8Verdict is the paired outcome of one measurement.
type w8Verdict struct {
	BudgetDigest      string           `json:"budget_digest"`
	BaselineBinary    string           `json:"baseline_binary_sha256"`
	CandidateBinary   string           `json:"candidate_binary_sha256"`
	BaselineRuns      []string         `json:"baseline_runs"`
	CandidateRuns     []string         `json:"candidate_runs"`
	Phases            []w8PhaseVerdict `json:"phases"`
	RetainedBaseline  w8Stat           `json:"retained_bytes_baseline"`
	RetainedCandidate w8Stat           `json:"retained_bytes_candidate"`
	RetainedLimit     uint64           `json:"retained_bytes_limit"`
	RetainedOK        bool             `json:"retained_within_budget"`
	Regressions       []string         `json:"regressions"`
	OverBudget        []string         `json:"over_budget"`
	Incomparable      []string         `json:"incomparable"`
	Notes             []string         `json:"notes,omitempty"`

	// Unbudgeted are phases the candidate ran that the frozen budgets do not
	// name. They cannot be judged — there is no baseline for them — but they
	// must not vanish either: a phase that produces numbers nobody reports is
	// the same failure as a phase nobody measured.
	Unbudgeted []w8PhaseVerdict `json:"unbudgeted_phases,omitempty"`
}

// w8CompareArms judges the candidate against frozen budgets.
//
// A phase with no baseline reading is never judged: it is listed as
// incomparable with the reason, because "the candidate wrote X and we have
// nothing to compare it with" is a fact, and a pass is not.
func w8CompareArms(set w8BudgetSet, candidate []w8RunArtifact) (w8Verdict, error) {
	if len(candidate) == 0 {
		return w8Verdict{}, errors.New("no candidate runs to compare")
	}
	verdict := w8Verdict{
		BudgetDigest:     set.Digest,
		BaselineBinary:   set.BinarySHA256,
		CandidateBinary:  candidate[0].Manifest.BinarySHA256,
		BaselineRuns:     set.Runs,
		RetainedLimit:    set.RetainedLimit,
		RetainedBaseline: set.RetainedBytes,
	}
	for _, run := range candidate {
		if run.Manifest.Arm != "candidate" {
			return w8Verdict{}, fmt.Errorf("run %s is arm %q, not the candidate", run.Dir, run.Manifest.Arm)
		}
		if set.FixtureDigest != "" && run.Manifest.FixtureDigest != set.FixtureDigest {
			return w8Verdict{}, fmt.Errorf("run %s used fixture %s; the budgets were frozen over %s", run.Dir, run.Manifest.FixtureDigest, set.FixtureDigest)
		}
		verdict.CandidateRuns = append(verdict.CandidateRuns, filepath.Base(run.Dir))
		if len(run.FailedPhases) > 0 {
			verdict.Notes = append(verdict.Notes, fmt.Sprintf("%s failed phases %v", filepath.Base(run.Dir), run.FailedPhases))
		}
	}
	if digest := w8BudgetDigest(set); digest != set.Digest {
		return w8Verdict{}, fmt.Errorf("budget set digest %s does not match its contents %s; the frozen budgets were edited", set.Digest, digest)
	}

	budgeted := map[string]bool{}
	for _, budget := range set.Phases {
		budgeted[budget.Phase] = true
		row := w8PhaseVerdict{
			Phase: budget.Phase, Limit: budget.Limit, LimitRule: budget.LimitRule,
			Baseline: budget.LogicalWrites, BaselineDisk: budget.DiskWritten,
			BaselineWAL: budget.WALBytesAfter, BaselineWall: budget.WallSeconds,
			BaselineExcl: budget.LogicalWritesExcl, BaselineCheckpoint: budget.CheckpointBytes,
		}
		candidateSeries := w8FillCandidateSide(&row, budget.Phase, candidate, budget.Windows)

		baselineSelection := w8BudgetJudged(budget)
		candidateSelection := w8SelectJudged(budget.Phase, row.Candidate, row.CandidateExcl,
			candidateSeries.WallSeconds, w8SeriesWindowStats(budget.Phase, candidateSeries.Windows))
		baselineJudged, series := baselineSelection.Stat, baselineSelection.Series
		candidateJudged := candidateSelection.Stat
		row.JudgedSeries = series
		row.BaselineSource, row.CandidateSource = baselineSelection.Source, candidateSelection.Source
		row.BaselineWallJudged, row.CandidateWallJudged = baselineSelection.Wall, candidateSelection.Wall
		row.JudgedNote = w8JoinJudgedNotes(baselineSelection.Note, candidateSelection.Note)
		row.BaselineJudged, row.CandidateJudged = baselineJudged, candidateJudged
		switch {
		case !baselineJudged.OK():
			row.Incomparable = "no baseline " + series + " reading for this phase"
		case !candidateJudged.OK():
			row.Incomparable = "no candidate " + series + " reading for this phase"
		case candidateSelection.Series != series:
			// One arm falling back to a different write series than the other
			// is not a comparison: the ceiling would be derived from one
			// quantity and applied to another, which reads as a false OVER
			// BUDGET in one direction and a false pass in the other.
			row.Incomparable = fmt.Sprintf("the arms would be judged on different series (baseline %s off the %s, candidate %s off the %s)",
				series, baselineSelection.Source, candidateSelection.Series, candidateSelection.Source)
		default:
			row.Comparable = true
			row.Ratio = float64(candidateJudged.Median) / float64(baselineJudged.Median)
			if row.Baseline.OK() && row.Candidate.OK() {
				row.RatioTotal = float64(row.Candidate.Median) / float64(row.Baseline.Median)
			}
			row.WithinBudget = row.Limit == 0 || candidateJudged.Median <= row.Limit
			row.Regressed = row.Ratio > w8RegressionFactor
		}
		if !row.Comparable {
			verdict.Incomparable = append(verdict.Incomparable, budget.Phase+": "+row.Incomparable)
		} else {
			if row.Limit > 0 && !row.WithinBudget {
				verdict.OverBudget = append(verdict.OverBudget, fmt.Sprintf("%s: %s %d > %d (%s)", budget.Phase, series, candidateJudged.Median, row.Limit, row.LimitRule))
			}
			if row.Regressed {
				verdict.Regressions = append(verdict.Regressions, fmt.Sprintf("%s: %s %d vs baseline %d (%.2fx)", budget.Phase, series, candidateJudged.Median, baselineJudged.Median, row.Ratio))
			}
		}
		verdict.Phases = append(verdict.Phases, row)
	}

	// Phases the candidate ran that no frozen budget names — a phase the
	// workload gained after the freeze. They are recorded with their numbers
	// and explicitly not judged.
	for _, phase := range w8PhaseNames(candidate) {
		if budgeted[phase] {
			continue
		}
		row := w8PhaseVerdict{Phase: phase, JudgedSeries: w8JudgedSeriesName(phase),
			LimitRule: "no frozen budget names this phase; recorded as evidence and not judged"}
		series := w8FillCandidateSide(&row, phase, candidate, nil)
		selection := w8SelectJudged(phase, row.Candidate, row.CandidateExcl,
			series.WallSeconds, w8SeriesWindowStats(phase, series.Windows))
		row.JudgedSeries, row.CandidateSource = selection.Series, selection.Source
		row.CandidateWallJudged, row.CandidateJudged = selection.Wall, selection.Stat
		row.JudgedNote = selection.Note
		verdict.Unbudgeted = append(verdict.Unbudgeted, row)
		verdict.Notes = append(verdict.Notes, fmt.Sprintf("phase %s has no frozen budget; its numbers are recorded and not judged", phase))
	}

	retainedLabels := []string{}
	retained := []*uint64{}
	for _, run := range candidate {
		retainedLabels = append(retainedLabels, filepath.Base(run.Dir))
		retained = append(retained, w8Uint64(uint64(max(run.RetainedCensus.TotalBytes, 0))))
	}
	verdict.RetainedCandidate = w8Statistic(retainedLabels, retained)
	verdict.RetainedOK = verdict.RetainedLimit == 0 ||
		(verdict.RetainedCandidate.OK() && verdict.RetainedCandidate.Median <= verdict.RetainedLimit)
	if verdict.RetainedCandidate.OK() && verdict.RetainedLimit > 0 && !verdict.RetainedOK {
		verdict.OverBudget = append(verdict.OverBudget, fmt.Sprintf("retained_bytes: %d > %d (%.1fx baseline)", verdict.RetainedCandidate.Median, verdict.RetainedLimit, w8RetainedFactor))
	}
	return verdict, nil
}

// w8WindowSeries is one sub-window's reduction across an arm's repetitions.
type w8WindowSeries struct {
	Window            string
	Detail            string
	LogicalWrites     w8Stat
	LogicalWritesExcl w8Stat
	CheckpointBytes   w8Stat
	ClientCalls       w8Stat
	WallSeconds       float64
}

// w8PhaseSeries is one phase's reduction across an arm's repetitions. Both
// halves of the paired comparison are built with it, so a budget and a
// candidate row can never be reduced by two different rules.
type w8PhaseSeries struct {
	Detail            string
	LogicalWrites     w8Stat
	LogicalWritesExcl w8Stat
	CheckpointBytes   w8Stat
	DiskWritten       w8Stat
	WALBytesAfter     w8Stat
	ClientCalls       w8Stat
	WallSeconds       float64
	Counters          map[string]int64
	Windows           []w8WindowSeries
}

// w8ReducePhase reduces one phase across one arm's runs.
func w8ReducePhase(phase string, runs []w8RunArtifact) w8PhaseSeries {
	series := w8PhaseSeries{Counters: map[string]int64{}}
	labels := []string{}
	var logical, excluded, checkpoint, disk, wal, calls []*uint64
	wall := []float64{}
	windowNames := []string{}
	seenWindow := map[string]bool{}
	windowValues := map[string]*w8WindowSeries{}
	windowLogical := map[string][]*uint64{}
	windowExcluded := map[string][]*uint64{}
	windowCheckpoint := map[string][]*uint64{}
	windowCalls := map[string][]*uint64{}
	windowWall := map[string][]float64{}
	for _, run := range runs {
		label := filepath.Base(run.Dir)
		labels = append(labels, label)
		report, found := w8FindPhase(run, phase)
		if !found {
			logical, excluded, checkpoint, disk, wal, calls =
				append(logical, nil), append(excluded, nil), append(checkpoint, nil),
				append(disk, nil), append(wal, nil), append(calls, nil)
			for _, name := range windowNames {
				windowLogical[name] = append(windowLogical[name], nil)
				windowExcluded[name] = append(windowExcluded[name], nil)
				windowCheckpoint[name] = append(windowCheckpoint[name], nil)
				windowCalls[name] = append(windowCalls[name], nil)
			}
			continue
		}
		if series.Detail == "" {
			series.Detail = report.Detail
		}
		logical = append(logical, report.LogicalWrites)
		excluded = append(excluded, report.LogicalWritesExcl)
		if report.LogicalWrites == nil {
			checkpoint = append(checkpoint, nil)
		} else {
			checkpoint = append(checkpoint, w8Uint64(report.CheckpointBytes))
		}
		disk = append(disk, w8Uint64(report.DiskWritten))
		wal = append(wal, w8Uint64(uint64(max(report.WALAfter, 0))))
		calls = append(calls, w8Uint64(uint64(max(report.ClientCalls, 0))))
		wall = append(wall, report.WallSeconds)
		for name, value := range report.CountersDelta {
			series.Counters[name] += value
		}
		present := map[string]bool{}
		for _, window := range report.Windows {
			if !seenWindow[window.Window] {
				seenWindow[window.Window] = true
				windowNames = append(windowNames, window.Window)
				windowValues[window.Window] = &w8WindowSeries{Window: window.Window, Detail: window.Detail}
				// A window first seen in a later repetition has no reading in
				// the earlier ones; name them rather than shift the series.
				for i := 0; i < len(labels)-1; i++ {
					windowLogical[window.Window] = append(windowLogical[window.Window], nil)
					windowExcluded[window.Window] = append(windowExcluded[window.Window], nil)
					windowCheckpoint[window.Window] = append(windowCheckpoint[window.Window], nil)
					windowCalls[window.Window] = append(windowCalls[window.Window], nil)
				}
			}
			if windowValues[window.Window].Detail == "" {
				windowValues[window.Window].Detail = window.Detail
			}
			present[window.Window] = true
			windowLogical[window.Window] = append(windowLogical[window.Window], window.LogicalWrites)
			windowExcluded[window.Window] = append(windowExcluded[window.Window], window.LogicalWritesExcl)
			if window.LogicalWrites == nil {
				windowCheckpoint[window.Window] = append(windowCheckpoint[window.Window], nil)
			} else {
				windowCheckpoint[window.Window] = append(windowCheckpoint[window.Window], w8Uint64(window.CheckpointBytes))
			}
			windowCalls[window.Window] = append(windowCalls[window.Window], w8Uint64(uint64(max(window.ClientCalls, 0))))
			windowWall[window.Window] = append(windowWall[window.Window], window.WallSeconds)
		}
		for _, name := range windowNames {
			if present[name] {
				continue
			}
			windowLogical[name] = append(windowLogical[name], nil)
			windowExcluded[name] = append(windowExcluded[name], nil)
			windowCheckpoint[name] = append(windowCheckpoint[name], nil)
			windowCalls[name] = append(windowCalls[name], nil)
		}
	}
	series.LogicalWrites = w8Statistic(labels, logical)
	series.LogicalWritesExcl = w8Statistic(labels, excluded)
	series.CheckpointBytes = w8Statistic(labels, checkpoint)
	series.DiskWritten = w8Statistic(labels, disk)
	series.WALBytesAfter = w8Statistic(labels, wal)
	series.ClientCalls = w8Statistic(labels, calls)
	series.WallSeconds = w8MedianFloat(wall)
	for _, name := range windowNames {
		window := windowValues[name]
		window.LogicalWrites = w8Statistic(labels, windowLogical[name])
		window.LogicalWritesExcl = w8Statistic(labels, windowExcluded[name])
		window.CheckpointBytes = w8Statistic(labels, windowCheckpoint[name])
		window.ClientCalls = w8Statistic(labels, windowCalls[name])
		window.WallSeconds = w8MedianFloat(windowWall[name])
		series.Windows = append(series.Windows, *window)
	}
	return series
}

// w8FillCandidateSide fills the candidate half of a phase row, including every
// sub-window either arm recorded.
func w8FillCandidateSide(row *w8PhaseVerdict, phase string, candidate []w8RunArtifact, baselineWindows []w8WindowBudget) w8PhaseSeries {
	series := w8ReducePhase(phase, candidate)
	row.Candidate = series.LogicalWrites
	row.CandidateExcl = series.LogicalWritesExcl
	row.CandidateCheckpoint = series.CheckpointBytes
	row.CandidateDisk = series.DiskWritten
	row.CandidateWAL = series.WALBytesAfter
	row.CandidateWall = series.WallSeconds
	row.CounterSummary = w8FormatCounters(series.Counters)

	baseline := map[string]w8WindowBudget{}
	for _, window := range baselineWindows {
		baseline[window.Window] = window
	}
	judgedWindow := w8JudgedWindows[phase]
	for _, window := range series.Windows {
		note := "sub-window: recorded, never judged — no frozen ceiling names it"
		if window.Window == judgedWindow {
			note = fmt.Sprintf("sub-window: this is the row the frozen %s ceiling is applied to — the arm that reproduces the frozen measurement", phase)
		}
		verdictRow := w8WindowVerdict{
			Window: window.Window, Detail: window.Detail,
			Candidate: window.LogicalWrites, CandidateExcl: window.LogicalWritesExcl,
			CandidateCheckpoint: window.CheckpointBytes, CandidateCalls: window.ClientCalls,
			Note: note,
		}
		if base, ok := baseline[window.Window]; ok {
			verdictRow.Baseline, verdictRow.BaselineExcl = base.LogicalWrites, base.LogicalWritesExcl
			verdictRow.BaselineCheckpoint, verdictRow.BaselineCalls = base.CheckpointBytes, base.ClientCalls
			if base.LogicalWrites.OK() && window.LogicalWrites.OK() {
				verdictRow.Ratio = float64(window.LogicalWrites.Median) / float64(base.LogicalWrites.Median)
			}
		}
		row.Windows = append(row.Windows, verdictRow)
	}
	return series
}

// w8FormatCounters renders the reuse counters a phase moved, largest first, so
// a write reduction can be read next to the reuse that produced it.
func w8FormatCounters(counters map[string]int64) string {
	if len(counters) == 0 {
		return ""
	}
	names := make([]string, 0, len(counters))
	for name := range counters {
		if counters[name] != 0 {
			names = append(names, name)
		}
	}
	sort.Slice(names, func(i, j int) bool {
		if counters[names[i]] != counters[names[j]] {
			return counters[names[i]] > counters[names[j]]
		}
		return names[i] < names[j]
	})
	parts := make([]string, 0, len(names))
	for _, name := range names {
		parts = append(parts, fmt.Sprintf("%s=%d", name, counters[name]))
	}
	return strings.Join(parts, " ")
}

// w8RenderVerdictTable renders the verdict as the markdown table the
// measurement document carries. Both write series appear in every row: the
// primary one is never published without the disk one beside it.
func w8RenderVerdictTable(verdict w8Verdict) string {
	var out strings.Builder
	out.WriteString("| phase | judged series | judged row (baseline / candidate) | baseline judged median (min/max) | candidate judged median (min/max) | ratio | total-series ratio | baseline wal_checkpoint_bytes | candidate wal_checkpoint_bytes | ceiling | verdict | baseline ri_diskio_byteswritten | candidate ri_diskio_byteswritten |\n")
	out.WriteString("|---|---|---|---|---|---|---|---|---|---|---|---|---|\n")
	rows := append(append([]w8PhaseVerdict{}, verdict.Phases...), verdict.Unbudgeted...)
	for _, row := range rows {
		baselineJudged, candidateJudged := row.BaselineJudged, row.CandidateJudged
		state := "incomparable"
		switch {
		case !row.Comparable && row.Incomparable == "":
			state = "unbudgeted: recorded, not judged"
		case !row.Comparable:
			state = "incomparable: " + row.Incomparable
		case row.Limit > 0 && !row.WithinBudget:
			state = "OVER BUDGET"
		case row.Regressed:
			state = "regression >10%"
		case row.Limit > 0:
			state = "within budget"
		default:
			state = "recorded"
		}
		ceiling := "none"
		if row.Limit > 0 {
			ceiling = fmt.Sprintf("%d", row.Limit)
		}
		ratio, totalRatio := "n/a", "n/a"
		if row.Comparable {
			ratio = fmt.Sprintf("%.2fx", row.Ratio)
		}
		if row.RatioTotal > 0 {
			totalRatio = fmt.Sprintf("%.2fx", row.RatioTotal)
		}
		out.WriteString(fmt.Sprintf("| %s | %s | %s | %s | %s | %s | %s | %s | %s | %s | %s | %s | %s |\n",
			row.Phase, row.JudgedSeries, w8FormatJudgedRows(row), w8FormatStat(baselineJudged), w8FormatStat(candidateJudged), ratio, totalRatio,
			w8FormatStat(row.BaselineCheckpoint), w8FormatStat(row.CandidateCheckpoint), ceiling, state,
			w8FormatStat(row.BaselineDisk), w8FormatStat(row.CandidateDisk)))
	}
	windows := w8RenderWindowTable(rows)
	if windows != "" {
		out.WriteString("\n")
		out.WriteString(windows)
	}
	return out.String()
}

// w8RenderWindowTable renders the sub-window rows. They carry no ceiling and
// the table says so in every row rather than once in a caption.
func w8RenderWindowTable(rows []w8PhaseVerdict) string {
	var out strings.Builder
	any := false
	for _, row := range rows {
		for _, window := range row.Windows {
			if !any {
				out.WriteString("| phase | window | baseline ri_logical_writes | candidate ri_logical_writes | candidate excl checkpoint | candidate wal_checkpoint_bytes | candidate client_calls | note |\n")
				out.WriteString("|---|---|---|---|---|---|---|---|\n")
				any = true
			}
			out.WriteString(fmt.Sprintf("| %s | %s | %s | %s | %s | %s | %s | %s |\n",
				row.Phase, window.Window, w8FormatStat(window.Baseline), w8FormatStat(window.Candidate),
				w8FormatStat(window.CandidateExcl), w8FormatStat(window.CandidateCheckpoint),
				w8FormatStat(window.CandidateCalls), window.Note))
		}
	}
	return out.String()
}

// w8JoinJudgedNotes carries both arms' selection notes on one row, and says a
// thing once when it is true of both arms.
func w8JoinJudgedNotes(baseline, candidate string) string {
	switch {
	case baseline == candidate:
		return baseline
	case baseline == "":
		return "candidate: " + candidate
	case candidate == "":
		return "baseline: " + baseline
	default:
		return "baseline: " + baseline + "; candidate: " + candidate
	}
}

// w8FormatJudgedRows renders which row each arm was judged off, with the wall
// beside it. A post-split candidate judged on its 60 s polling window against a
// pre-split baseline judged on its 62 s phase is a valid 1:1 comparison, and
// this cell is how a reader checks that rather than trusting it.
func w8FormatJudgedRows(row w8PhaseVerdict) string {
	label := func(source string, wall float64) string {
		if source == "" {
			source = "n/a"
		}
		if wall > 0 {
			return fmt.Sprintf("%s @%.0fs", source, wall)
		}
		return source
	}
	return label(row.BaselineSource, row.BaselineWallJudged) + " / " + label(row.CandidateSource, row.CandidateWallJudged)
}

func w8FormatStat(stat w8Stat) string {
	if !stat.OK() {
		return "unavailable (" + strings.Join(stat.Unavailable, ",") + ")"
	}
	text := fmt.Sprintf("%d (%d/%d)", stat.Median, stat.Min, stat.Max)
	if len(stat.Unavailable) > 0 {
		text += " [missing: " + strings.Join(stat.Unavailable, ",") + "]"
	}
	return text
}

// ------------------------------------------------------------ the opt-in ---

// TestW8PairedArmsVerdict is the reduction step of the paired protocol, run
// over an artifact directory a sustained run produced.
//
// It is opt-in and names its skip: it reduces artifacts, it does not make them.
func TestW8PairedArmsVerdict(t *testing.T) {
	dir := os.Getenv("GXW8_PAIRED_ARTIFACT_DIR")
	if dir == "" {
		t.Skip("set GXW8_PAIRED_ARTIFACT_DIR to reduce a completed paired run into frozen budgets and a verdict")
	}
	baseline, err := w8LoadArmRuns(dir, "baseline")
	if err != nil {
		t.Fatal(err)
	}
	frozen, err := w8FreezeBudgets(baseline)
	if err != nil {
		t.Fatal(err)
	}
	budgetsPath := filepath.Join(dir, "budgets.json")
	frozen, wrote, err := w8WriteFrozenBudgets(budgetsPath, frozen)
	if err != nil {
		t.Fatal(err)
	}
	// A budget set frozen before the checkpoint-excluded series existed gets
	// the derived half filled in from the same baseline artifacts, under the
	// refusal in w8AugmentFrozenBudgets. The file on disk is not rewritten.
	frozen, err = w8AugmentFrozenBudgets(frozen, baseline)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("budgets %s (newly frozen=%v) digest=%s augmented_from=%s over runs %v", budgetsPath, wrote, frozen.Digest, frozen.AugmentedFrom, frozen.Runs)
	for _, run := range baseline {
		t.Logf("baseline %s checkpoint series: %s", filepath.Base(run.Dir), run.CheckpointSource)
	}
	for _, budget := range frozen.Phases {
		t.Logf("BUDGET %s judged on %s: ri_logical_writes median=%d excl_checkpoint median=%d checkpoint median=%d limit=%d (%s)",
			budget.Phase, budget.JudgedSeries, budget.LogicalWrites.Median, budget.LogicalWritesExcl.Median,
			budget.CheckpointBytes.Median, budget.Limit, budget.LimitRule)
	}

	if os.Getenv("GXW8_BUDGETS_ONLY") != "" {
		t.Log("GXW8_BUDGETS_ONLY is set; the candidate arm was not read")
		return
	}
	candidate, err := w8LoadArmRuns(dir, "candidate")
	if err != nil {
		t.Fatal(err)
	}
	verdict, err := w8CompareArms(frozen, candidate)
	if err != nil {
		t.Fatal(err)
	}
	w8WriteJSON(t, filepath.Join(dir, "verdict.json"), verdict)
	table := w8RenderVerdictTable(verdict)
	if err := os.WriteFile(filepath.Join(dir, "verdict.md"), []byte(table), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Logf("verdict table:\n%s", table)
	for _, run := range candidate {
		t.Logf("candidate %s checkpoint series: %s", filepath.Base(run.Dir), run.CheckpointSource)
	}
	t.Logf("over budget: %v", verdict.OverBudget)
	t.Logf("regressions preserved: %v", verdict.Regressions)
	t.Logf("incomparable: %v", verdict.Incomparable)
}

// ------------------------------------------------------------------ tests ---

func TestW8StatisticNamesAMissingReadingInsteadOfCountingItAsZero(t *testing.T) {
	stat := w8Statistic([]string{"rep1", "rep2", "rep3"}, []*uint64{w8Uint64(100), nil, w8Uint64(300)})
	if stat.N != 2 {
		t.Fatalf("n = %d, want the two readings that exist", stat.N)
	}
	if stat.Min != 100 || stat.Max != 300 {
		t.Fatalf("a missing reading moved the range: %+v", stat)
	}
	if len(stat.Unavailable) != 1 || stat.Unavailable[0] != "rep2" {
		t.Fatalf("the missing repetition is not named: %+v", stat)
	}
	// If a nil were folded in as 0 the min would be 0 and the median 100.
	if stat.Median != 300 {
		t.Fatalf("median = %d, want the upper of two readings (100,300)", stat.Median)
	}
	if empty := w8Statistic(nil, []*uint64{nil, nil}); empty.OK() || len(empty.Unavailable) != 2 {
		t.Fatalf("a series with no readings must not report OK: %+v", empty)
	}
	odd := w8Statistic(nil, []*uint64{w8Uint64(5), w8Uint64(1), w8Uint64(3)})
	if odd.Median != 3 || odd.Min != 1 || odd.Max != 5 {
		t.Fatalf("three readings reduced to %+v", odd)
	}
}

// w8SyntheticRun builds one arm-repetition artifact for the reduction tests.
func w8SyntheticRun(arm string, logical map[string]uint64, retained int64) w8RunArtifact {
	run := w8RunArtifact{
		Manifest:       w8Manifest{Arm: arm, BinarySHA256: arm + "-sha", FixtureDigest: "fixture-1"},
		RetainedCensus: w8Census{TotalBytes: retained},
	}
	for _, phase := range []string{"P1_idle_cold", "P2_small_edits", "P5_main_advance"} {
		value, ok := logical[phase]
		report := w8PhaseReport{Phase: phase, Detail: phase + " detail", WallSeconds: 60, WALAfter: 4096, StoreBytesAfter: 1 << 20}
		if ok {
			report.LogicalWrites = w8Uint64(value)
			report.DiskWritten = value / 2
		}
		run.Phases = append(run.Phases, report)
	}
	return run
}

func TestW8FreezeBudgetsRefusesAnythingButTheBaselineArm(t *testing.T) {
	baseline := []w8RunArtifact{
		w8SyntheticRun("baseline", map[string]uint64{"P1_idle_cold": 1000, "P2_small_edits": 2000, "P5_main_advance": 8_000_000}, 100),
		w8SyntheticRun("candidate", map[string]uint64{"P1_idle_cold": 1, "P2_small_edits": 2, "P5_main_advance": 3}, 100),
	}
	if _, err := w8FreezeBudgets(baseline); err == nil {
		t.Fatal("budgets were frozen from a set containing a candidate run; the candidate's numbers must never reach a budget")
	}
	if _, err := w8FreezeBudgets(nil); err == nil {
		t.Fatal("budgets were frozen from no runs at all")
	}
	mixed := []w8RunArtifact{
		w8SyntheticRun("baseline", map[string]uint64{"P2_small_edits": 2000}, 100),
		w8SyntheticRun("baseline", map[string]uint64{"P2_small_edits": 2000}, 100),
	}
	mixed[1].Manifest.FixtureDigest = "fixture-2"
	if _, err := w8FreezeBudgets(mixed); err == nil {
		t.Fatal("budgets were frozen across two different fixtures; the arms would not be paired")
	}
}

func TestW8FreezeBudgetsAppliesThePlansCeilings(t *testing.T) {
	runs := []w8RunArtifact{
		w8SyntheticRun("baseline", map[string]uint64{"P1_idle_cold": 30 << 20, "P2_small_edits": 2000, "P5_main_advance": 8_000_000}, 100),
		w8SyntheticRun("baseline", map[string]uint64{"P1_idle_cold": 30 << 20, "P2_small_edits": 2000, "P5_main_advance": 8_000_000}, 100),
		w8SyntheticRun("baseline", map[string]uint64{"P1_idle_cold": 30 << 20, "P2_small_edits": 2000, "P5_main_advance": 8_000_000}, 100),
	}
	set, err := w8FreezeBudgets(runs)
	if err != nil {
		t.Fatal(err)
	}
	limits := map[string]uint64{}
	for _, budget := range set.Phases {
		limits[budget.Phase] = budget.Limit
	}
	// An idle phase whose baseline is far above the absolute cap is held to
	// the cap, not to its own baseline.
	if limits["P1_idle_cold"] != w8IdleBudgetPerMinute {
		t.Fatalf("idle ceiling = %d, want the %d-byte absolute cap", limits["P1_idle_cold"], w8IdleBudgetPerMinute)
	}
	if limits["P2_small_edits"] != 2000 {
		t.Fatalf("dirty-edit ceiling = %d, want the baseline median 2000", limits["P2_small_edits"])
	}
	if limits["P5_main_advance"] != 4_000_000 {
		t.Fatalf("main-advance ceiling = %d, want half of the baseline median", limits["P5_main_advance"])
	}
	if set.RetainedLimit != 120 {
		t.Fatalf("retained ceiling = %d, want 1.2x the baseline 100", set.RetainedLimit)
	}
	if set.Digest == "" || set.Digest != w8BudgetDigest(set) {
		t.Fatal("the budget set does not carry a digest of its own contents")
	}
}

func TestW8FrozenBudgetsRefuseToBeRewrittenOnceTheCandidateHasBeenSeen(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "budgets.json")
	first, err := w8FreezeBudgets([]w8RunArtifact{
		w8SyntheticRun("baseline", map[string]uint64{"P5_main_advance": 8_000_000}, 100),
	})
	if err != nil {
		t.Fatal(err)
	}
	stored, wrote, err := w8WriteFrozenBudgets(path, first)
	if err != nil || !wrote {
		t.Fatalf("first freeze did not write: wrote=%v err=%v", wrote, err)
	}
	if stored.Digest != first.Digest {
		t.Fatal("the first freeze did not return what it wrote")
	}

	// A second, laxer budget over the same file — the shape of moving the
	// goalposts after the candidate's numbers are known.
	looser, err := w8FreezeBudgets([]w8RunArtifact{
		w8SyntheticRun("baseline", map[string]uint64{"P5_main_advance": 800_000_000}, 100),
	})
	if err != nil {
		t.Fatal(err)
	}
	back, wrote, err := w8WriteFrozenBudgets(path, looser)
	if err != nil {
		t.Fatal(err)
	}
	if wrote {
		t.Fatal("a second freeze overwrote the frozen budgets")
	}
	if back.Digest != first.Digest {
		t.Fatalf("the frozen budgets changed: %s -> %s", first.Digest, back.Digest)
	}
	onDisk, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(onDisk), first.Digest) {
		t.Fatal("budgets.json on disk no longer carries the originally frozen digest")
	}
}

func TestW8CompareArmsJudgesAgainstTheFrozenCeilingsAndPreservesRegressions(t *testing.T) {
	baseline := []w8RunArtifact{
		w8SyntheticRun("baseline", map[string]uint64{"P1_idle_cold": 1 << 20, "P2_small_edits": 2000, "P5_main_advance": 8_000_000}, 1000),
		w8SyntheticRun("baseline", map[string]uint64{"P1_idle_cold": 1 << 20, "P2_small_edits": 2000, "P5_main_advance": 8_000_000}, 1000),
		w8SyntheticRun("baseline", map[string]uint64{"P1_idle_cold": 1 << 20, "P2_small_edits": 2000, "P5_main_advance": 8_000_000}, 1000),
	}
	set, err := w8FreezeBudgets(baseline)
	if err != nil {
		t.Fatal(err)
	}
	candidate := []w8RunArtifact{
		// idle within budget, edits regressed 50%, main advance halved.
		w8SyntheticRun("candidate", map[string]uint64{"P1_idle_cold": 1 << 19, "P2_small_edits": 3000, "P5_main_advance": 3_000_000}, 1100),
		w8SyntheticRun("candidate", map[string]uint64{"P1_idle_cold": 1 << 19, "P2_small_edits": 3000, "P5_main_advance": 3_000_000}, 1100),
		w8SyntheticRun("candidate", map[string]uint64{"P1_idle_cold": 1 << 19, "P2_small_edits": 3000, "P5_main_advance": 3_000_000}, 1100),
	}
	verdict, err := w8CompareArms(set, candidate)
	if err != nil {
		t.Fatal(err)
	}
	rows := map[string]w8PhaseVerdict{}
	for _, row := range verdict.Phases {
		rows[row.Phase] = row
	}
	if row := rows["P5_main_advance"]; !row.WithinBudget || row.Ratio >= 0.5 || row.Regressed {
		t.Fatalf("a halved main advance was not judged within the headline ceiling: %+v", row)
	}
	if row := rows["P2_small_edits"]; row.WithinBudget || !row.Regressed {
		t.Fatalf("a 1.5x dirty-edit phase was not flagged: %+v", row)
	}
	if len(verdict.Regressions) != 1 || !strings.Contains(verdict.Regressions[0], "P2_small_edits") {
		t.Fatalf("the regression was not preserved: %v", verdict.Regressions)
	}
	if len(verdict.OverBudget) != 1 || !strings.Contains(verdict.OverBudget[0], "P2_small_edits") {
		t.Fatalf("the over-budget phase was not named: %v", verdict.OverBudget)
	}
	if !verdict.RetainedOK {
		t.Fatalf("retained 1100 against a 1200 ceiling was judged over budget: %+v", verdict.RetainedCandidate)
	}
	table := w8RenderVerdictTable(verdict)
	for _, want := range []string{"P5_main_advance", "OVER BUDGET", "ri_diskio_byteswritten"} {
		if !strings.Contains(table, want) {
			t.Fatalf("the rendered table is missing %q:\n%s", want, table)
		}
	}
	// The disk series is published in the same row as the primary one, always.
	if strings.Count(table, "ri_diskio_byteswritten") != 2 {
		t.Fatalf("the table does not carry both arms' secondary series:\n%s", table)
	}
}

func TestW8CompareArmsRefusesEditedBudgetsAndUnpairedFixtures(t *testing.T) {
	set, err := w8FreezeBudgets([]w8RunArtifact{
		w8SyntheticRun("baseline", map[string]uint64{"P5_main_advance": 8_000_000}, 100),
	})
	if err != nil {
		t.Fatal(err)
	}
	candidate := []w8RunArtifact{w8SyntheticRun("candidate", map[string]uint64{"P5_main_advance": 1_000_000}, 100)}

	edited := set
	edited.Phases = append([]w8PhaseBudget(nil), set.Phases...)
	edited.Phases[0].Limit = 1 << 40
	if _, err := w8CompareArms(edited, candidate); err == nil {
		t.Fatal("a verdict was produced against budgets whose contents no longer match their digest")
	}
	unpaired := []w8RunArtifact{w8SyntheticRun("candidate", map[string]uint64{"P5_main_advance": 1}, 100)}
	unpaired[0].Manifest.FixtureDigest = "fixture-9"
	if _, err := w8CompareArms(set, unpaired); err == nil {
		t.Fatal("a verdict was produced across two different fixtures")
	}
	if _, err := w8CompareArms(set, []w8RunArtifact{w8SyntheticRun("baseline", nil, 100)}); err == nil {
		t.Fatal("a baseline run was accepted as the candidate arm")
	}
}

func TestW8CompareArmsNeverPassesAPhaseItCannotCompare(t *testing.T) {
	// The baseline lost its process counters for the headline phase.
	blind := w8SyntheticRun("baseline", nil, 100)
	set, err := w8FreezeBudgets([]w8RunArtifact{blind})
	if err != nil {
		t.Fatal(err)
	}
	for _, budget := range set.Phases {
		if budget.Limit != 0 {
			t.Fatalf("%s got a ceiling out of a baseline with no reading: %+v", budget.Phase, budget)
		}
		if !strings.Contains(budget.LimitRule, "no baseline") {
			t.Fatalf("%s does not say why it has no ceiling: %q", budget.Phase, budget.LimitRule)
		}
	}
	candidate := []w8RunArtifact{w8SyntheticRun("candidate", map[string]uint64{"P5_main_advance": 1_000_000}, 100)}
	verdict, err := w8CompareArms(set, candidate)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range verdict.Phases {
		if row.Comparable || row.WithinBudget {
			t.Fatalf("%s was judged without a baseline: %+v", row.Phase, row)
		}
	}
	if len(verdict.Incomparable) != len(verdict.Phases) {
		t.Fatalf("incomparable phases were not all listed: %v", verdict.Incomparable)
	}
	if len(verdict.OverBudget) != 0 || len(verdict.Regressions) != 0 {
		t.Fatalf("a phase with no baseline produced a judgement: over=%v regressed=%v", verdict.OverBudget, verdict.Regressions)
	}
	table := w8RenderVerdictTable(verdict)
	if !strings.Contains(table, "incomparable") {
		t.Fatalf("the table hides the incomparable phases:\n%s", table)
	}
}

func TestW8LoadArmRunsReadsEveryRepetitionInOrder(t *testing.T) {
	dir := t.TempDir()
	for i, value := range []uint64{300, 100, 200} {
		name := fmt.Sprintf("baseline_rep%d", i+1)
		if err := os.MkdirAll(filepath.Join(dir, name), 0o700); err != nil {
			t.Fatal(err)
		}
		run := w8SyntheticRun("baseline", map[string]uint64{"P5_main_advance": value}, int64(value))
		data, err := json.Marshal(run)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name, "report.json"), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// A directory for the other arm must not be picked up here.
	if err := os.MkdirAll(filepath.Join(dir, "candidate_rep1"), 0o700); err != nil {
		t.Fatal(err)
	}
	runs, err := w8LoadArmRuns(dir, "baseline")
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 3 {
		t.Fatalf("loaded %d runs, want 3", len(runs))
	}
	for i, want := range []uint64{300, 100, 200} {
		report, found := w8FindPhase(runs[i], "P5_main_advance")
		if !found || report.LogicalWrites == nil || *report.LogicalWrites != want {
			t.Fatalf("run %d is not rep%d: %+v", i, i+1, report)
		}
	}
	if _, err := w8LoadArmRuns(dir, "candidate"); err == nil {
		t.Fatal("an arm directory with no report.json must be an error, not a silently shorter series")
	}
	if _, err := w8LoadArmRuns(dir, "nosucharm"); err == nil {
		t.Fatal("an arm with no run directories must be an error")
	}
}

// -------------------------------- checkpoint-excluded reduction tests ---

// w8CheckpointRun builds one arm-repetition artifact carrying both write
// series, so the reduction can be exercised on the exact numbers the frozen
// paired1500 artifacts hold.
func w8CheckpointRun(arm string, totals, checkpoints map[string]uint64) w8RunArtifact {
	run := w8RunArtifact{
		Manifest:       w8Manifest{Arm: arm, BinarySHA256: arm + "-sha", FixtureDigest: "fixture-1"},
		RetainedCensus: w8Census{TotalBytes: 1024},
	}
	for _, phase := range []string{"P2_small_edits", "P5_main_advance"} {
		total, ok := totals[phase]
		report := w8PhaseReport{Phase: phase, Detail: phase + " detail", WallSeconds: 60, WALAfter: 4096, StoreBytesAfter: 1 << 20}
		if ok {
			value := total
			report.LogicalWrites = &value
			report.DiskWritten = total / 2
			report.CheckpointBytes = checkpoints[phase]
			report.LogicalWritesExcl = w8ExcludeCheckpoint(&value, report.CheckpointBytes)
		}
		run.Phases = append(run.Phases, report)
	}
	return run
}

// w8IdleArtifact builds one arm-repetition carrying an idle phase, optionally
// split into a quiet arm and a polling arm. `split=false` is the shape of every
// artifact measured before the split — the frozen baseline included.
func w8IdleArtifact(arm, phase string, quiet, polling uint64, split bool, idleWall float64) w8RunArtifact {
	value := func(v uint64) *uint64 { return &v }
	run := w8CheckpointRun(arm, map[string]uint64{"P2_small_edits": 10, "P5_main_advance": 20}, map[string]uint64{})
	report := w8PhaseReport{Phase: phase, Detail: "idle", WallSeconds: idleWall}
	if !split {
		total := polling
		report.LogicalWrites, report.LogicalWritesExcl = value(total), value(total)
		run.Phases = append(run.Phases, report)
		return run
	}
	quietName, pollingName := w8WindowIdleColdQuiet, w8WindowIdleColdPolling
	if phase == "P8_idle_warm" {
		quietName, pollingName = w8WindowIdleWarmQuiet, w8WindowIdleWarmPolling
	}
	report.WallSeconds = idleWall * 2
	report.LogicalWrites, report.LogicalWritesExcl = value(quiet+polling), value(quiet+polling)
	report.Windows = []w8WindowReport{
		{Phase: phase, Window: quietName, WallSeconds: idleWall, ClientCalls: 0,
			LogicalWrites: value(quiet), LogicalWritesExcl: value(quiet)},
		{Phase: phase, Window: pollingName, WallSeconds: idleWall, ClientCalls: 12,
			LogicalWrites: value(polling), LogicalWritesExcl: value(polling)},
	}
	run.Phases = append(run.Phases, report)
	return run
}

// TestW8IdlePhaseIsJudgedOnThePollingArmAgainstTheFrozenPhase is the revert-red
// of the idle-split repair, on the reduction side.
//
// The frozen P1/P8 ceilings were measured over ONE 60 s window that polled every
// 5 s. A post-split candidate measures idle twice and its phase total is the sum
// of two 60 s windows, so judging the frozen ceiling against the phase total
// compares ~124 s of candidate against ~62 s of baseline and reports a
// regression that is an artifact of the harness. The reduction maps the frozen
// phase name onto the arm that reproduces the frozen measurement — the polling
// window — and leaves the quiet arm as an un-budgeted row.
func TestW8IdlePhaseIsJudgedOnThePollingArmAgainstTheFrozenPhase(t *testing.T) {
	// The frozen baseline: measured before the split, so it carries no windows.
	baseline := []w8RunArtifact{w8IdleArtifact("baseline", "P8_idle_warm", 0, 3_299_312, false, 62)}
	set, err := w8FreezeBudgets(baseline)
	if err != nil {
		t.Fatal(err)
	}
	budget := w8FindBudget(t, set, "P8_idle_warm")
	if budget.JudgedSource != "" || budget.JudgedWall != 0 {
		t.Fatalf("a pre-split baseline must be judged on its phase row and record nothing extra: %+v", budget)
	}

	// The candidate: split, and its quiet arm writes as much again.
	candidate := []w8RunArtifact{w8IdleArtifact("candidate", "P8_idle_warm", 3_000_000, 3_100_000, true, 60)}
	verdict, err := w8CompareArms(set, candidate)
	if err != nil {
		t.Fatal(err)
	}
	row := w8FindVerdict(t, verdict, "P8_idle_warm")
	if row.CandidateSource != "window:"+w8WindowIdleWarmPolling {
		t.Fatalf("the candidate was judged off %q, not its polling arm", row.CandidateSource)
	}
	if row.BaselineSource != w8JudgedSourcePhase {
		t.Fatalf("the pre-split baseline was judged off %q", row.BaselineSource)
	}
	if row.CandidateJudged.Median != 3_100_000 {
		t.Fatalf("the judged candidate number is %d, want the polling arm's 3,100,000 (the phase total is 6,100,000)", row.CandidateJudged.Median)
	}
	if row.Candidate.Median != 6_100_000 {
		t.Fatalf("the phase total must stay on the row as evidence: %+v", row.Candidate)
	}
	// 3,100,000 / 3,299,312 = 0.94x. On the phase total it would read 1.85x
	// and be preserved as a regression that the harness invented.
	if row.Ratio < 0.93 || row.Ratio > 0.95 {
		t.Fatalf("ratio = %.4f, want the polling arm against the frozen phase (~0.94)", row.Ratio)
	}
	if !row.Comparable || !row.WithinBudget {
		t.Fatalf("the idle row is not readable: %+v", row)
	}
	for _, regression := range verdict.Regressions {
		if strings.HasPrefix(regression, "P8_idle_warm:") {
			t.Fatalf("the doubled phase was preserved as a regression: %s", regression)
		}
	}
	if row.CandidateWallJudged != 60 || row.BaselineWallJudged != 62 {
		t.Fatalf("the judged walls are not recorded: candidate %.0fs baseline %.0fs", row.CandidateWallJudged, row.BaselineWallJudged)
	}
	// The quiet arm is recorded, and it is recorded as un-budgeted.
	quiet, polling := w8FindWindowRow(t, row, w8WindowIdleWarmQuiet), w8FindWindowRow(t, row, w8WindowIdleWarmPolling)
	if quiet.Candidate.Median != 3_000_000 || !strings.Contains(quiet.Note, "never judged") {
		t.Fatalf("the quiet arm must be an un-budgeted row of its own: %+v", quiet)
	}
	if !strings.Contains(polling.Note, "ceiling is applied to") {
		t.Fatalf("the judged arm's row does not say it carries the ceiling: %q", polling.Note)
	}
	table := w8RenderVerdictTable(verdict)
	if !strings.Contains(table, "window:"+w8WindowIdleWarmPolling) || !strings.Contains(table, "phase @62s") {
		t.Fatalf("the rendered table does not show which row each arm was judged off:\n%s", table)
	}
	// The table prints the numbers the verdict judged, not a pair it re-derives
	// off the phase rows: 3,100,000, never the doubled phase's 6,100,000.
	if !strings.Contains(table, "3100000") || strings.Contains(table, "6100000") {
		t.Fatalf("the rendered table does not carry the judged pair:\n%s", table)
	}
}

// TestW8FrozenIdleArtifactsStayJudgedOnThePhaseOnBothArms is the other half of
// the same contract: re-reading artifacts that predate the split must not move.
//
// The frozen paired1500 arms both predate the sub-windows, so both are judged on
// their phase rows and the re-reduction reproduces the numbers the freeze was
// read with. That is what keeps the post-fix candidate comparable with the
// frozen baseline without re-running it.
func TestW8FrozenIdleArtifactsStayJudgedOnThePhaseOnBothArms(t *testing.T) {
	set, err := w8FreezeBudgets([]w8RunArtifact{w8IdleArtifact("baseline", "P1_idle_cold", 0, 1_490_944, false, 62)})
	if err != nil {
		t.Fatal(err)
	}
	verdict, err := w8CompareArms(set, []w8RunArtifact{w8IdleArtifact("candidate", "P1_idle_cold", 0, 1_388_592, false, 62)})
	if err != nil {
		t.Fatal(err)
	}
	row := w8FindVerdict(t, verdict, "P1_idle_cold")
	if row.BaselineSource != w8JudgedSourcePhase || row.CandidateSource != w8JudgedSourcePhase {
		t.Fatalf("pre-split artifacts must both be judged on the phase: %s / %s", row.BaselineSource, row.CandidateSource)
	}
	if !row.Comparable || row.CandidateJudged.Median != 1_388_592 || row.BaselineJudged.Median != 1_490_944 {
		t.Fatalf("the frozen re-read moved: %+v", row)
	}
	if ratio := row.Ratio; ratio < 0.92 || ratio > 0.94 {
		t.Fatalf("P1 ratio = %.4f, want the frozen 0.93x", ratio)
	}
	if !strings.Contains(row.JudgedNote, "carries no "+w8WindowIdleColdPolling+" sub-window") {
		t.Fatalf("the row does not say why it was judged on the phase: %q", row.JudgedNote)
	}
}

// TestW8CompareArmsRefusesToJudgeTwoArmsOnDifferentSeries pins the asymmetry the
// selection makes possible.
//
// One arm falling back to the total while the other is judged on the
// checkpoint-excluded series is not a comparison: the ceiling is derived from
// one quantity and applied to another. It reads as a false OVER BUDGET in one
// direction and as a false pass in the other, and the row still claims a single
// series. Incomparable is the only honest verdict.
func TestW8CompareArmsRefusesToJudgeTwoArmsOnDifferentSeries(t *testing.T) {
	baseline := []w8RunArtifact{w8CheckpointRun("baseline",
		map[string]uint64{"P2_small_edits": 16_879_664, "P5_main_advance": 100}, map[string]uint64{})}
	set, err := w8FreezeBudgets(baseline)
	if err != nil {
		t.Fatal(err)
	}
	candidate := w8CheckpointRun("candidate",
		map[string]uint64{"P2_small_edits": 58_040_984, "P5_main_advance": 50}, map[string]uint64{"P2_small_edits": 42_607_016})
	// The candidate's excluded series goes missing — a run whose samples.ndjson
	// could not be read.
	for i := range candidate.Phases {
		if candidate.Phases[i].Phase == "P2_small_edits" {
			candidate.Phases[i].LogicalWritesExcl = nil
		}
	}
	verdict, err := w8CompareArms(set, []w8RunArtifact{candidate})
	if err != nil {
		t.Fatal(err)
	}
	row := w8FindVerdict(t, verdict, "P2_small_edits")
	if row.Comparable {
		t.Fatalf("the candidate's total was judged against a ceiling derived from the baseline's excluded median: %+v", row)
	}
	if !strings.Contains(row.Incomparable, "different series") {
		t.Fatalf("the reason does not name the asymmetry: %q", row.Incomparable)
	}
	if len(verdict.OverBudget) != 0 {
		t.Fatalf("an incomparable row must not be reported over budget: %v", verdict.OverBudget)
	}
	named := false
	for _, entry := range verdict.Incomparable {
		if strings.HasPrefix(entry, "P2_small_edits:") {
			named = true
		}
	}
	if !named {
		t.Fatalf("the incomparable row was not listed: %v", verdict.Incomparable)
	}
}

// TestW8LimitForIdleScalesByTheJudgedWindowWall pins the ceiling's duration
// term against the same doubling: an idle ceiling scaled by a phase that now
// spans two arms would hand the candidate twice the allowance.
func TestW8LimitForIdleScalesByTheJudgedWindowWall(t *testing.T) {
	budget := w8PhaseBudget{
		Phase:         "P8_idle_warm",
		WallSeconds:   120,
		LogicalWrites: w8Statistic(nil, []*uint64{w8Uint64(100 << 20)}),
		Windows: []w8WindowBudget{
			{Window: w8WindowIdleWarmQuiet, WallSeconds: 60, LogicalWrites: w8Statistic(nil, []*uint64{w8Uint64(1 << 20)})},
			{Window: w8WindowIdleWarmPolling, WallSeconds: 60, LogicalWrites: w8Statistic(nil, []*uint64{w8Uint64(50 << 20)})},
		},
	}
	limit, rule := w8LimitFor("P8_idle_warm", budget)
	if limit != w8IdleBudgetPerMinute {
		t.Fatalf("limit = %d, want the 8 MiB/60s absolute scaled by the judged window's own 60s", limit)
	}
	if !strings.Contains(rule, "scaled to 60s") || !strings.Contains(rule, "window:"+w8WindowIdleWarmPolling) {
		t.Fatalf("the rule does not name the row and the wall it scaled by: %q", rule)
	}
	// A window with no recorded wall falls back to the phase's, and says so
	// rather than collapsing the ceiling to one second's worth of bytes.
	budget.Windows[1].WallSeconds = 0
	if _, rule := w8LimitFor("P8_idle_warm", budget); !strings.Contains(rule, "scaled to 120s") {
		t.Fatalf("a window without a wall must fall back to the phase's: %q", rule)
	}
}

func w8FindWindowRow(t *testing.T, row w8PhaseVerdict, window string) w8WindowVerdict {
	t.Helper()
	for _, candidate := range row.Windows {
		if candidate.Window == window {
			return candidate
		}
	}
	t.Fatalf("no %s row on %s: %+v", window, row.Phase, row.Windows)
	return w8WindowVerdict{}
}

// TestW8PairedVerdictReadsP2OnTheCheckpointExcludedSeries is the revert-red of
// §F5(1), on the frozen paired1500 numbers.
//
// The frozen verdict rows P2_small_edits at 58,040,984 against a baseline of
// 16,879,664 = 3.44x, OVER BUDGET. 42,607,016 of those bytes are one
// wal_autocheckpoint drain that landed inside the window because the candidate
// entered P2 about 1,120 frames deeper — work earlier phases wrote, charged to
// whichever window crossed the threshold. Judged on the checkpoint-excluded
// series the same artifacts read 15,433,968 vs 16,879,664 = 0.91x, inside the
// ceiling, and the drain is reported as its own row.
//
// Deleting the exclusion — judging P2 on the total again — puts the phase back
// over budget at 3.44x and fails this test.
func TestW8PairedVerdictReadsP2OnTheCheckpointExcludedSeries(t *testing.T) {
	baseline := []w8RunArtifact{w8CheckpointRun("baseline",
		map[string]uint64{"P2_small_edits": 16_879_664, "P5_main_advance": 5_147_295_504},
		map[string]uint64{})}
	set, err := w8FreezeBudgets(baseline)
	if err != nil {
		t.Fatal(err)
	}
	p2 := w8FindBudget(t, set, "P2_small_edits")
	if p2.JudgedSeries != w8SeriesExcluded {
		t.Fatalf("P2 is judged on %q, want the checkpoint-excluded series", p2.JudgedSeries)
	}
	if p2.Limit != 16_879_664 {
		t.Fatalf("P2 ceiling = %d, want the baseline's excluded median 16,879,664", p2.Limit)
	}
	if p5 := w8FindBudget(t, set, "P5_main_advance"); p5.JudgedSeries != w8SeriesTotal {
		t.Fatalf("P5 is judged on %q; a continuously-writing phase keeps the total series", p5.JudgedSeries)
	}

	candidate := []w8RunArtifact{w8CheckpointRun("candidate",
		map[string]uint64{"P2_small_edits": 58_040_984, "P5_main_advance": 1_321_807_648},
		map[string]uint64{"P2_small_edits": 42_607_016})}
	verdict, err := w8CompareArms(set, candidate)
	if err != nil {
		t.Fatal(err)
	}
	row := w8FindVerdict(t, verdict, "P2_small_edits")
	if row.CandidateExcl.Median != 15_433_968 {
		t.Fatalf("candidate excluded median = %d, want 15,433,968", row.CandidateExcl.Median)
	}
	if row.CandidateCheckpoint.Median != 42_607_016 {
		t.Fatalf("checkpoint bytes are not reported as their own row: %+v", row.CandidateCheckpoint)
	}
	if ratio := row.Ratio; ratio < 0.90 || ratio > 0.92 {
		t.Fatalf("P2 ratio on the judged series = %.4f, want ~0.91", ratio)
	}
	if total := row.RatioTotal; total < 3.43 || total > 3.45 {
		t.Fatalf("the total-series ratio must stay visible beside it: %.4f", total)
	}
	if !row.WithinBudget {
		t.Fatalf("P2 is still over budget on the excluded series: %+v", row)
	}
	for _, over := range verdict.OverBudget {
		if strings.HasPrefix(over, "P2_small_edits:") {
			t.Fatalf("P2 was reported over budget: %s", over)
		}
	}
	if !strings.Contains(w8RenderVerdictTable(verdict), "42607016") {
		t.Fatal("the rendered table does not carry the checkpoint row")
	}
}

func w8FindBudget(t *testing.T, set w8BudgetSet, phase string) w8PhaseBudget {
	t.Helper()
	for _, budget := range set.Phases {
		if budget.Phase == phase {
			return budget
		}
	}
	t.Fatalf("no budget for %s", phase)
	return w8PhaseBudget{}
}

func w8FindVerdict(t *testing.T, verdict w8Verdict, phase string) w8PhaseVerdict {
	t.Helper()
	for _, row := range verdict.Phases {
		if row.Phase == phase {
			return row
		}
	}
	for _, row := range verdict.Unbudgeted {
		if row.Phase == phase {
			return row
		}
	}
	t.Fatalf("no verdict row for %s", phase)
	return w8PhaseVerdict{}
}

// TestW8ExcludeCheckpointNeverInventsANegativeSeries pins the one arithmetic
// hazard of a per-sample-interval attribution: a bracket shorter than one
// sample can be handed a checkpoint total larger than its own delta.
func TestW8ExcludeCheckpointNeverInventsANegativeSeries(t *testing.T) {
	if w8ExcludeCheckpoint(nil, 10) != nil {
		t.Fatal("a missing total must stay missing, not become a zero")
	}
	total := uint64(100)
	if got := w8ExcludeCheckpoint(&total, 400); got == nil || *got != 0 {
		t.Fatalf("excluded = %v, want 0", got)
	}
	if got := w8ExcludeCheckpoint(&total, 40); got == nil || *got != 60 {
		t.Fatalf("excluded = %v, want 60", got)
	}
}

// TestW8FillCheckpointSeriesRederivesFromTheRunsOwnSamples is what makes the
// frozen artifacts re-readable: a report.json written before the series
// existed, next to the samples.ndjson the run took, yields the same numbers.
func TestW8FillCheckpointSeriesRederivesFromTheRunsOwnSamples(t *testing.T) {
	dir := t.TempDir()
	value := func(v uint64) *uint64 { return &v }
	stream := []w8Sample{
		{Phase: "P2_small_edits", LogicalWrites: value(0)},
		{Phase: "P2_small_edits", LogicalWrites: value(15_433_968)},
		{Phase: "P2_small_edits", LogicalWrites: value(58_040_984), WALResets: 1},
	}
	var lines strings.Builder
	for _, sample := range stream {
		data, err := json.Marshal(sample)
		if err != nil {
			t.Fatal(err)
		}
		lines.Write(append(data, '\n'))
	}
	if err := os.WriteFile(filepath.Join(dir, "samples.ndjson"), []byte(lines.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	run := w8RunArtifact{Dir: dir, Phases: []w8PhaseReport{
		{Phase: "P2_small_edits", LogicalWrites: value(58_040_984)},
	}}
	w8FillCheckpointSeries(&run)
	if run.Phases[0].CheckpointBytes != 42_607_016 {
		t.Fatalf("re-derived checkpoint bytes = %d", run.Phases[0].CheckpointBytes)
	}
	if run.Phases[0].LogicalWritesExcl == nil || *run.Phases[0].LogicalWritesExcl != 15_433_968 {
		t.Fatalf("re-derived excluded series = %v", run.Phases[0].LogicalWritesExcl)
	}
	if !strings.Contains(run.CheckpointSource, "re-derived") {
		t.Fatalf("the provenance of a re-derived series must say so: %q", run.CheckpointSource)
	}
	// A run that measured the series itself is left alone and says so.
	measured := w8RunArtifact{Dir: dir, Phases: []w8PhaseReport{
		{Phase: "P2_small_edits", LogicalWrites: value(10), LogicalWritesExcl: value(7), CheckpointBytes: 3},
	}}
	w8FillCheckpointSeries(&measured)
	if measured.Phases[0].CheckpointBytes != 3 || measured.CheckpointSource != "measured by the run" {
		t.Fatalf("a measured series was overwritten: %+v %q", measured.Phases[0], measured.CheckpointSource)
	}
}

// TestW8BudgetDigestSurvivesTheAddedSeriesWhenAPhaseCarriesNone is the guard
// that keeps every already-frozen budgets.json valid.
//
// w8CompareArms recomputes the digest of the set it is handed and refuses it if
// the bytes do not match. A field added to w8PhaseBudget that marshals even
// when zero would therefore invalidate every frozen file in existence — and the
// post-fix re-measurement is specified to reuse one. The new fields are
// `omitzero`/`omitempty` precisely so a set without them digests as it always
// did, and this test pins both halves: the keys are absent, and the digest is
// the one a set without those fields has always had.
func TestW8BudgetDigestSurvivesTheAddedSeriesWhenAPhaseCarriesNone(t *testing.T) {
	set := w8BudgetSet{
		FrozenAt: "2026-09-13T00:00:00Z", Arm: "baseline", Runs: []string{"baseline_rep1"},
		BinarySHA256: "sha", FixtureDigest: "fixture-1",
		Phases: []w8PhaseBudget{{
			Phase:         "P2_small_edits",
			LogicalWrites: w8Statistic([]string{"baseline_rep1"}, []*uint64{w8Uint64(16_879_664)}),
			Limit:         16_879_664, LimitRule: "dirty edits: at most the baseline median 16879664",
		}},
	}
	data, err := json.Marshal(set)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"ri_logical_writes_excl_checkpoint", "wal_checkpoint_bytes", "judged_series", "judged_source", "judged_wall_s", "\"windows\"", "augmented_from_digest"} {
		if strings.Contains(string(data), key) {
			t.Fatalf("a zero-valued new field is serialised (%s), which changes every frozen digest:\n%s", key, data)
		}
	}
	// The golden digest of this exact set, computed before the fields existed.
	const golden = "33f58820264c6e06ee8b4c029d4618e74f1fea30ee64a5fc0c325ecb8d28a7f2"
	if got := w8BudgetDigest(set); got != golden && os.Getenv("GXW8_ACCEPT_GOLDEN") == "" {
		t.Fatalf("the canonical digest of a pre-checkpoint budget set changed: %s (golden %s); re-run with GXW8_ACCEPT_GOLDEN=1 only after confirming no frozen budgets.json exists", got, golden)
	}
}

// TestW8AugmentFrozenBudgetsFillsTheDerivedHalfAndRefusesForeignRuns pins the
// one path by which a frozen ceiling may be recomputed: from the same baseline
// artifacts, with every already-frozen total reproducing exactly.
func TestW8AugmentFrozenBudgetsFillsTheDerivedHalfAndRefusesForeignRuns(t *testing.T) {
	runs := []w8RunArtifact{w8CheckpointRun("baseline",
		map[string]uint64{"P2_small_edits": 16_879_664, "P5_main_advance": 5_147_295_504},
		map[string]uint64{"P2_small_edits": 24_752})}
	runs[0].Dir = "baseline_rep1"
	// A set frozen without the derived half: strip it.
	frozen, err := w8FreezeBudgets(runs)
	if err != nil {
		t.Fatal(err)
	}
	for i := range frozen.Phases {
		frozen.Phases[i].LogicalWritesExcl = w8Stat{}
		frozen.Phases[i].CheckpointBytes = w8Stat{}
		frozen.Phases[i].JudgedSeries = ""
		frozen.Phases[i].Limit, frozen.Phases[i].LimitRule = w8LimitFor(frozen.Phases[i].Phase, frozen.Phases[i])
	}
	frozen.Digest = w8BudgetDigest(frozen)
	before := w8FindBudget(t, frozen, "P2_small_edits")
	if before.Limit != 16_879_664 {
		t.Fatalf("the stripped set's P2 ceiling = %d", before.Limit)
	}

	augmented, err := w8AugmentFrozenBudgets(frozen, runs)
	if err != nil {
		t.Fatal(err)
	}
	after := w8FindBudget(t, augmented, "P2_small_edits")
	if after.CheckpointBytes.Median != 24_752 || after.LogicalWritesExcl.Median != 16_854_912 {
		t.Fatalf("the derived half was not filled: %+v", after)
	}
	if after.Limit != 16_854_912 || after.JudgedSeries != w8SeriesExcluded {
		t.Fatalf("the ceiling was not recomputed over the judged series: limit=%d series=%s", after.Limit, after.JudgedSeries)
	}
	if augmented.AugmentedFrom != frozen.Digest || augmented.Digest == frozen.Digest {
		t.Fatalf("the augmentation did not record what it was derived from: from=%q digest=%q", augmented.AugmentedFrom, augmented.Digest)
	}
	if _, err := w8CompareArms(augmented, []w8RunArtifact{w8CheckpointRun("candidate",
		map[string]uint64{"P2_small_edits": 1, "P5_main_advance": 1}, map[string]uint64{})}); err != nil {
		t.Fatalf("the augmented set no longer passes its own digest check: %v", err)
	}

	// Different runs must be refused outright: a ceiling recomputed from
	// numbers the budgets were not frozen over is not a frozen ceiling.
	foreign := []w8RunArtifact{w8CheckpointRun("baseline",
		map[string]uint64{"P2_small_edits": 99, "P5_main_advance": 5_147_295_504}, map[string]uint64{})}
	foreign[0].Dir = "baseline_rep1"
	if _, err := w8AugmentFrozenBudgets(frozen, foreign); err == nil {
		t.Fatal("a baseline whose totals do not reproduce must be refused")
	}
	if _, err := w8AugmentFrozenBudgets(frozen, []w8RunArtifact{w8CheckpointRun("candidate", nil, nil)}); err == nil {
		t.Fatal("the candidate arm must never be a source for a frozen ceiling")
	}
	// Idempotent: a set that already carries the series is returned untouched.
	again, err := w8AugmentFrozenBudgets(augmented, runs)
	if err != nil || again.Digest != augmented.Digest {
		t.Fatalf("augmenting twice moved the set: err=%v", err)
	}
}

// TestW8CompareArmsRecordsPhasesNoFrozenBudgetNames keeps a workload that
// gained a phase after the freeze honest: the phase cannot be judged, and it
// must not disappear either.
func TestW8CompareArmsRecordsPhasesNoFrozenBudgetNames(t *testing.T) {
	baseline := []w8RunArtifact{w8CheckpointRun("baseline",
		map[string]uint64{"P2_small_edits": 100, "P5_main_advance": 200}, map[string]uint64{})}
	set, err := w8FreezeBudgets(baseline)
	if err != nil {
		t.Fatal(err)
	}
	candidate := w8CheckpointRun("candidate",
		map[string]uint64{"P2_small_edits": 90, "P5_main_advance": 80}, map[string]uint64{})
	extra := uint64(4242)
	candidate.Phases = append(candidate.Phases, w8PhaseReport{
		Phase: "P9_new_phase", Detail: "a phase the freeze never saw",
		LogicalWrites: &extra, LogicalWritesExcl: &extra, WallSeconds: 5,
	})
	verdict, err := w8CompareArms(set, []w8RunArtifact{candidate})
	if err != nil {
		t.Fatal(err)
	}
	if len(verdict.Unbudgeted) != 1 || verdict.Unbudgeted[0].Phase != "P9_new_phase" {
		t.Fatalf("the unbudgeted phase was lost: %+v", verdict.Unbudgeted)
	}
	if verdict.Unbudgeted[0].Candidate.Median != 4242 || verdict.Unbudgeted[0].Comparable {
		t.Fatalf("an unbudgeted phase must be recorded and not judged: %+v", verdict.Unbudgeted[0])
	}
	if !strings.Contains(w8RenderVerdictTable(verdict), "P9_new_phase") {
		t.Fatal("the rendered table dropped the unbudgeted phase")
	}
}

// TestW8ReducePhaseCarriesSubWindowRowsThroughTheVerdict pins that a phase's
// sub-windows reach the paired output with their client-call counts — the row
// the idle floor is read off — and are marked as never judged.
func TestW8ReducePhaseCarriesSubWindowRowsThroughTheVerdict(t *testing.T) {
	value := func(v uint64) *uint64 { return &v }
	withWindows := func(arm string, quiet, polling uint64, quietCalls, pollingCalls int) w8RunArtifact {
		run := w8CheckpointRun(arm, map[string]uint64{"P2_small_edits": quiet + polling, "P5_main_advance": 1}, map[string]uint64{})
		run.Phases = append(run.Phases, w8PhaseReport{
			Phase: "P8_idle_warm", Detail: "idle", WallSeconds: 120,
			LogicalWrites: value(quiet + polling), LogicalWritesExcl: value(quiet + polling),
			Windows: []w8WindowReport{
				{Phase: "P8_idle_warm", Window: w8WindowIdleWarmQuiet, LogicalWrites: value(quiet), LogicalWritesExcl: value(quiet), ClientCalls: quietCalls},
				{Phase: "P8_idle_warm", Window: w8WindowIdleWarmPolling, LogicalWrites: value(polling), LogicalWritesExcl: value(polling), ClientCalls: pollingCalls},
			},
		})
		return run
	}
	set, err := w8FreezeBudgets([]w8RunArtifact{withWindows("baseline", 100, 1_500_000, 0, 12)})
	if err != nil {
		t.Fatal(err)
	}
	budget := w8FindBudget(t, set, "P8_idle_warm")
	if len(budget.Windows) != 2 || budget.Windows[0].Window != w8WindowIdleWarmQuiet {
		t.Fatalf("the baseline's sub-windows were not frozen: %+v", budget.Windows)
	}
	if budget.Windows[0].ClientCalls.Median != 0 || budget.Windows[1].ClientCalls.Median != 12 {
		t.Fatalf("client-call counts did not survive the reduction: %+v", budget.Windows)
	}
	verdict, err := w8CompareArms(set, []w8RunArtifact{withWindows("candidate", 120, 1_400_000, 0, 12)})
	if err != nil {
		t.Fatal(err)
	}
	row := w8FindVerdict(t, verdict, "P8_idle_warm")
	if len(row.Windows) != 2 {
		t.Fatalf("the sub-window rows did not reach the verdict: %+v", row.Windows)
	}
	quiet := row.Windows[0]
	if quiet.Candidate.Median != 120 || quiet.Baseline.Median != 100 || quiet.CandidateCalls.Median != 0 {
		t.Fatalf("the quiet window row = %+v", quiet)
	}
	if !strings.Contains(quiet.Note, "never judged") {
		t.Fatalf("a sub-window row must say it carries no ceiling: %q", quiet.Note)
	}
}
