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
	ExactnessWaitsTotal  int             `json:"exactness_waits_total"`
	ExactnessWaitSeconds float64         `json:"exactness_wait_seconds"`

	// Dir is where this artifact was read from. It is not part of the JSON;
	// it is what a verdict cites so a number can be walked back to its run.
	Dir string `json:"-"`
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
		runs = append(runs, run)
	}
	return runs, nil
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
	Phase           string  `json:"phase"`
	Detail          string  `json:"detail,omitempty"`
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
	Digest        string          `json:"digest"`
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
		var logical, disk, wal, resets, store, generations, sequence []*uint64
		wall := []float64{}
		for _, run := range runs {
			labels = append(labels, filepath.Base(run.Dir))
			report, found := w8FindPhase(run, phase)
			if !found {
				logical, disk, wal, resets, store, generations, sequence =
					append(logical, nil), append(disk, nil), append(wal, nil),
					append(resets, nil), append(store, nil), append(generations, nil), append(sequence, nil)
				continue
			}
			if budget.Detail == "" {
				budget.Detail = report.Detail
			}
			logical = append(logical, report.LogicalWrites)
			disk = append(disk, w8Uint64(report.DiskWritten))
			wal = append(wal, w8Uint64(uint64(max(report.WALAfter, 0))))
			resets = append(resets, w8Uint64(uint64(max(report.WALResets, 0))))
			store = append(store, w8Uint64(uint64(max(report.StoreBytesAfter, 0))))
			generations = append(generations, w8Uint64(uint64(max(report.StoreCensusAfter.Generations.Max, 0))))
			sequence = append(sequence, w8Uint64(uint64(max(report.StoreCensusAfter.Generations.Sequence, 0))))
			wall = append(wall, report.WallSeconds)
		}
		budget.LogicalWrites = w8Statistic(labels, logical)
		budget.DiskWritten = w8Statistic(labels, disk)
		budget.WALBytesAfter = w8Statistic(labels, wal)
		budget.WALResets = w8Statistic(labels, resets)
		budget.StoreBytesAfter = w8Statistic(labels, store)
		budget.GenerationsMax = w8Statistic(labels, generations)
		budget.GenerationSeq = w8Statistic(labels, sequence)
		budget.WallSeconds = w8MedianFloat(wall)
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

// w8LimitFor is the plan's ceiling table, in one place.
func w8LimitFor(phase string, budget w8PhaseBudget) (uint64, string) {
	if !budget.LogicalWrites.OK() {
		return 0, "no baseline ri_logical_writes reading; this phase carries no ceiling and is reported as evidence only"
	}
	baseline := budget.LogicalWrites.Median
	switch {
	case w8IdlePhases[phase]:
		absolute := uint64(float64(w8IdleBudgetPerMinute) * max(budget.WallSeconds, 1) / 60)
		limit := baseline
		if absolute < limit {
			limit = absolute
		}
		return limit, fmt.Sprintf("idle: min(baseline median %d, %d MiB per 60s scaled to %.0fs = %d)", baseline, w8IdleBudgetPerMinute>>20, budget.WallSeconds, absolute)
	case phase == "P5_main_advance":
		return uint64(float64(baseline) * w8MainAdvanceFactor), fmt.Sprintf("headline: %.2f x baseline median %d", w8MainAdvanceFactor, baseline)
	case phase == "P2_small_edits":
		return baseline, fmt.Sprintf("dirty edits: at most the baseline median %d", baseline)
	default:
		return 0, fmt.Sprintf("no frozen ceiling; baseline median %d is recorded and a candidate above %.0f%% of it is preserved as a regression", baseline, w8RegressionFactor*100)
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

// ---------------------------------------------------------------- verdict ---

// w8PhaseVerdict is one phase's paired outcome.
type w8PhaseVerdict struct {
	Phase          string  `json:"phase"`
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

	for _, budget := range set.Phases {
		row := w8PhaseVerdict{
			Phase: budget.Phase, Limit: budget.Limit, LimitRule: budget.LimitRule,
			Baseline: budget.LogicalWrites, BaselineDisk: budget.DiskWritten,
			BaselineWAL: budget.WALBytesAfter, BaselineWall: budget.WallSeconds,
		}
		labels := []string{}
		var logical, disk, wal []*uint64
		wall := []float64{}
		counters := map[string]int64{}
		for _, run := range candidate {
			labels = append(labels, filepath.Base(run.Dir))
			report, found := w8FindPhase(run, budget.Phase)
			if !found {
				logical, disk, wal = append(logical, nil), append(disk, nil), append(wal, nil)
				continue
			}
			logical = append(logical, report.LogicalWrites)
			disk = append(disk, w8Uint64(report.DiskWritten))
			wal = append(wal, w8Uint64(uint64(max(report.WALAfter, 0))))
			wall = append(wall, report.WallSeconds)
			for name, value := range report.CountersDelta {
				counters[name] += value
			}
		}
		row.Candidate = w8Statistic(labels, logical)
		row.CandidateDisk = w8Statistic(labels, disk)
		row.CandidateWAL = w8Statistic(labels, wal)
		row.CandidateWall = w8MedianFloat(wall)
		row.CounterSummary = w8FormatCounters(counters)

		switch {
		case !row.Baseline.OK():
			row.Incomparable = "no baseline ri_logical_writes reading for this phase"
		case !row.Candidate.OK():
			row.Incomparable = "no candidate ri_logical_writes reading for this phase"
		default:
			row.Comparable = true
			row.Ratio = float64(row.Candidate.Median) / float64(row.Baseline.Median)
			row.WithinBudget = row.Limit == 0 || row.Candidate.Median <= row.Limit
			row.Regressed = row.Ratio > w8RegressionFactor
		}
		if !row.Comparable {
			verdict.Incomparable = append(verdict.Incomparable, budget.Phase+": "+row.Incomparable)
		} else {
			if row.Limit > 0 && !row.WithinBudget {
				verdict.OverBudget = append(verdict.OverBudget, fmt.Sprintf("%s: %d > %d (%s)", budget.Phase, row.Candidate.Median, row.Limit, row.LimitRule))
			}
			if row.Regressed {
				verdict.Regressions = append(verdict.Regressions, fmt.Sprintf("%s: %d vs baseline %d (%.2fx)", budget.Phase, row.Candidate.Median, row.Baseline.Median, row.Ratio))
			}
		}
		verdict.Phases = append(verdict.Phases, row)
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
	out.WriteString("| phase | baseline ri_logical_writes median (min/max) | candidate median (min/max) | ratio | ceiling | verdict | baseline ri_diskio_byteswritten | candidate ri_diskio_byteswritten |\n")
	out.WriteString("|---|---|---|---|---|---|---|---|\n")
	for _, row := range verdict.Phases {
		state := "incomparable"
		switch {
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
		ratio := "n/a"
		if row.Comparable {
			ratio = fmt.Sprintf("%.2fx", row.Ratio)
		}
		out.WriteString(fmt.Sprintf("| %s | %s | %s | %s | %s | %s | %s | %s |\n",
			row.Phase, w8FormatStat(row.Baseline), w8FormatStat(row.Candidate), ratio, ceiling, state,
			w8FormatStat(row.BaselineDisk), w8FormatStat(row.CandidateDisk)))
	}
	return out.String()
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
	t.Logf("budgets %s (newly frozen=%v) digest=%s over runs %v", budgetsPath, wrote, frozen.Digest, frozen.Runs)
	for _, budget := range frozen.Phases {
		t.Logf("BUDGET %s: ri_logical_writes median=%d min=%d max=%d limit=%d (%s)",
			budget.Phase, budget.LogicalWrites.Median, budget.LogicalWrites.Min, budget.LogicalWrites.Max, budget.Limit, budget.LimitRule)
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
