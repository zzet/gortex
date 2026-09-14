package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/zzet/gortex/internal/daemon"
)

// W8.10 — the isolated end-to-end matrix, rows 6 and 7 of handoff §8.
//
// This file carries matrix 6 (lifecycle: untrack / remove / recreate, primary
// removal beside a preserved dedicated sibling, drains and late readers,
// duplicate triggers, cancellation, failed build and retry, restart and crash,
// disk full) and the scaffolding both matrix files share.
// w8_matrix_adversarial_test.go carries matrix 7.
//
// Three rules the whole file obeys, from the wave brief:
//
//  1. OPT-IN. Nothing here runs without GXW8_MATRIX_BINARY naming a daemon
//     binary to drive. Every skip states a reason and names the matrix row it
//     belongs to, so a row that could not be exercised on this host is visible
//     in the ledger rather than silently green.
//  2. PRIVATE. Every process is the issue767 fixture's private child: its own
//     XDG directories, its own SQLite store, its own Git fixture and gitconfig
//     under a private /private/tmp root. The user's daemon, store and
//     configuration are never addressed.
//  3. NAMED GATES. Every row declares which acceptance gate (handoff §7) its
//     assertions serve, and the run writes an outcome table naming, per row,
//     the gate and what happened. A row is never asserted against a node count:
//     what it compares is the answer a public surface gives — the symbol, the
//     file it came from, the freshness label, the catalog census — against what
//     the contract in the source says that surface must answer.

const (
	// w8lcBinaryEnv opts into the matrix and names the daemon binary under
	// test. Deliberately distinct from the sustained harness's
	// GXW8_TEST_BINARY: the matrix is minutes of correctness rows, the
	// sustained harness is an hour of measurement, and a run of one must not
	// drag in the other.
	w8lcBinaryEnv = "GXW8_MATRIX_BINARY"
	// w8lcArtifactEnv is where the outcome tables are written. Unset sends
	// them to the test's temp directory, which is removed with the test.
	w8lcArtifactEnv = "GXW8_MATRIX_ARTIFACT_DIR"
	// w8lcDiskFullEnv opts into the disk-full row. It is separate from the
	// matrix gate because that row attaches a disk image: a host-level side
	// effect outside the private root, which a caller has to ask for.
	w8lcDiskFullEnv = "GXW8_MATRIX_DISKFULL"
	// w8lcChildEnv re-enters the driver in a child process so the wiring test
	// can watch a REAL failing row unwind.
	w8lcChildEnv = "GXW8_INTERNAL_MATRIX_DRIVER_DIR"
)

// The acceptance gates, in handoff §7's own numbering and words. A row names
// the gate its assertions serve; w8lcValidateRows refuses a row that names a
// gate outside this vocabulary, so an outcome table can never claim a gate the
// brief does not have.
const (
	w8lcGateSnapshot  = "gate-1 snapshot correctness"
	w8lcGateNoop      = "gate-2 no-op behaviour"
	w8lcGateReuse     = "gate-4 same-branch reuse"
	w8lcGateAdvance   = "gate-5 advancing main"
	w8lcGateAuthority = "gate-6 atomic publication and authority"
	w8lcGateLifetime  = "gate-7 lifetime and cleanup"
	w8lcGateBounded   = "gate-8 bounded costs"
	w8lcGateStorage   = "gate-9 storage and recovery safety"
	w8lcGateEvidence  = "gate-10 reproducible release evidence"
)

var w8lcGateVocabulary = []string{
	w8lcGateSnapshot, w8lcGateNoop, w8lcGateReuse, w8lcGateAdvance,
	w8lcGateAuthority, w8lcGateLifetime, w8lcGateBounded, w8lcGateStorage,
	w8lcGateEvidence,
}

// The matrix-6 bullets, verbatim from the coordinator's brief (handoff §8's
// sixth bullet, split into the units this file exercises). w8lcValidateRows
// requires every one of them to be claimed by exactly one row, so narrowing
// the matrix means deleting a bullet in the open rather than quietly dropping
// a row.
var w8lcMatrix6Bullets = []string{
	"public untrack/remove/recreate",
	"primary removal with preserved independent dedicated siblings",
	"drains and late readers/workers",
	"duplicate triggers",
	"cancellation",
	"failed build and retry",
	"isolated daemon restart and crash recovery",
	"disk-full recovery",
}

// Outcome statuses. "not_exercised" is the honest fourth: a row this branch or
// this host cannot drive is recorded with the reason, never as a pass.
const (
	w8lcStatusPass         = "pass"
	w8lcStatusFail         = "fail"
	w8lcStatusSkipped      = "skipped"
	w8lcStatusNotExercised = "not_exercised"
	// w8lcStatusFiltered is the fifth, and it exists because Go's own runner
	// makes the fourth dangerous: t.Run returns TRUE for a subtest that
	// -test.run filtered out, so a matrix driven with a row filter would file
	// a table in which every row it never ran is recorded as a pass. A partial
	// run is evidence for the rows it drove and for nothing else.
	w8lcStatusFiltered = "filtered"
)

// w8lcRow is one matrix row: what it is, which brief bullet it discharges,
// which acceptance gates its assertions serve, and the body that drives it.
//
// A row with no Run must carry Unexercised — the reason, naming the ledger row
// — and is reported as not_exercised. That pairing is what makes "the branch
// does not handle this case" a statement in the outcome table instead of an
// absent row nobody notices.
type w8lcRow struct {
	Name        string
	Bullet      string
	Gates       []string
	Run         func(t *testing.T, rec *w8lcRecorder)
	Unexercised string
}

// w8lcRecorder collects a row's evidence. Notes are measurements — what a
// counter said, which corpus answered, what a CLI replied — recorded whether
// the row passes or fails. Skip records a named reason AND the row: a skip
// with no row name is exactly the silent pass the brief forbids.
type w8lcRecorder struct {
	mu    sync.Mutex
	notes []string
	skip  string
}

func (r *w8lcRecorder) note(format string, args ...any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.notes = append(r.notes, fmt.Sprintf(format, args...))
}

// skipRow records the reason and stops the row. The reason must name the
// matrix row it belongs to; w8lcRecordOutcome refuses a skip that does not.
func (r *w8lcRecorder) skipRow(t *testing.T, reason string) {
	t.Helper()
	r.mu.Lock()
	r.skip = reason
	r.mu.Unlock()
	t.Skip(reason)
}

func (r *w8lcRecorder) snapshot() ([]string, string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.notes...), r.skip
}

// w8lcOutcome is one row of the outcome table the run records.
type w8lcOutcome struct {
	Matrix  string   `json:"matrix"`
	Name    string   `json:"row"`
	Bullet  string   `json:"bullet"`
	Gates   []string `json:"gates"`
	Status  string   `json:"status"`
	Reason  string   `json:"reason,omitempty"`
	Notes   []string `json:"notes,omitempty"`
	Seconds float64  `json:"seconds"`
}

// w8lcRecordOutcome turns one finished row into its table entry.
//
// It is separated from the driver so the status decision — which is the whole
// evidentiary value of the table — is a pure function a unit test can drive
// through all four outcomes without a daemon. The precedence is deliberate:
// a row that declared itself unexercised is never reported as a pass even if
// nothing failed, a row whose body never ran is never reported as a pass even
// though t.Run returns true for a filtered-out subtest, and a skip is never
// reported as a pass even though the Go test runner counts a skipped subtest
// as "not failed".
func w8lcRecordOutcome(matrix string, row w8lcRow, ran, passed bool, elapsed time.Duration, notes []string, skip string) w8lcOutcome {
	out := w8lcOutcome{
		Matrix: matrix, Name: row.Name, Bullet: row.Bullet,
		Gates: append([]string(nil), row.Gates...), Notes: notes,
		Seconds: elapsed.Seconds(),
	}
	switch {
	case row.Run == nil:
		out.Status, out.Reason = w8lcStatusNotExercised, row.Unexercised
	case !ran:
		out.Status, out.Reason = w8lcStatusFiltered, "the row body never ran: -test.run excluded this subtest, so its result is not evidence"
	case skip != "":
		out.Status, out.Reason = w8lcStatusSkipped, skip
	case passed:
		out.Status = w8lcStatusPass
	default:
		out.Status = w8lcStatusFail
	}
	return out
}

// w8lcValidateRows is the table's own contract, checked before anything runs:
// unique names, a gate from the vocabulary on every row, a claimed bullet, and
// a reason on every row that declares itself unexercised. Every bullet in the
// brief has to be claimed exactly once.
func w8lcValidateRows(matrix string, rows []w8lcRow, bullets []string) error {
	seen := map[string]bool{}
	claimed := map[string]string{}
	for _, row := range rows {
		if row.Name == "" {
			return fmt.Errorf("%s: a row has no name", matrix)
		}
		if seen[row.Name] {
			return fmt.Errorf("%s: duplicate row %q", matrix, row.Name)
		}
		seen[row.Name] = true
		if len(row.Gates) == 0 {
			return fmt.Errorf("%s: row %q names no acceptance gate", matrix, row.Name)
		}
		for _, gate := range row.Gates {
			if !w8lcKnownGate(gate) {
				return fmt.Errorf("%s: row %q names unknown gate %q", matrix, row.Name, gate)
			}
		}
		if row.Bullet == "" {
			return fmt.Errorf("%s: row %q claims no brief bullet", matrix, row.Name)
		}
		if other, dup := claimed[row.Bullet]; dup {
			return fmt.Errorf("%s: rows %q and %q both claim bullet %q", matrix, other, row.Name, row.Bullet)
		}
		claimed[row.Bullet] = row.Name
		if row.Run == nil && strings.TrimSpace(row.Unexercised) == "" {
			return fmt.Errorf("%s: row %q has no body and no reason", matrix, row.Name)
		}
		if row.Run != nil && row.Unexercised != "" {
			return fmt.Errorf("%s: row %q both runs and declares itself unexercised", matrix, row.Name)
		}
	}
	for _, bullet := range bullets {
		if claimed[bullet] == "" {
			return fmt.Errorf("%s: brief bullet %q is claimed by no row", matrix, bullet)
		}
	}
	return nil
}

func w8lcKnownGate(gate string) bool {
	for _, known := range w8lcGateVocabulary {
		if gate == known {
			return true
		}
	}
	return false
}

// w8lcRunMatrix drives the rows and files the outcome table on every exit.
//
// Two things keep the evidence. The recorder is read AFTER the row returns, so
// the notes a row took before it failed survive the runtime.Goexit its t.Fatal
// performs — that is where a failing row's evidence would otherwise be lost.
// And the table is written from a defer, so a fatal in the driver's own body
// (an unreadable row table, an artifact directory that cannot be created)
// still leaves behind the rows that did run. The sustained harness learned the
// same lesson about phase artifacts
// (w8_sustained_io_integration_test.go's w8WithRunArtifacts).
func w8lcRunMatrix(t *testing.T, matrix string, rows []w8lcRow, bullets []string, artifactDir string) []w8lcOutcome {
	t.Helper()
	if err := w8lcValidateRows(matrix, rows, bullets); err != nil {
		t.Fatal(err)
	}
	outcomes := make([]w8lcOutcome, 0, len(rows))
	defer func() {
		path := filepath.Join(artifactDir, matrix+".json")
		if err := w8lcWriteOutcomes(path, matrix, outcomes); err != nil {
			t.Errorf("outcome table %s: %v", path, err)
		} else {
			t.Logf("outcome table: %s", path)
		}
		t.Log("\n" + w8lcRenderOutcomes(matrix, outcomes))
	}()
	for _, row := range rows {
		if row.Run == nil {
			outcomes = append(outcomes, w8lcRecordOutcome(matrix, row, false, false, 0, nil, ""))
			t.Logf("row %s NOT EXERCISED: %s", row.Name, row.Unexercised)
			continue
		}
		rec := &w8lcRecorder{}
		start := time.Now()
		// ran is set from INSIDE the subtest, so it distinguishes "the row ran
		// and did not fail" from "the runner's row filter skipped past it and
		// t.Run reported true". Only the first is evidence.
		ran := false
		passed := t.Run(row.Name, func(t *testing.T) {
			ran = true
			row.Run(t, rec)
		})
		notes, skip := rec.snapshot()
		outcomes = append(outcomes, w8lcRecordOutcome(matrix, row, ran, passed, time.Since(start), notes, skip))
	}
	return outcomes
}

func w8lcWriteOutcomes(path, matrix string, outcomes []w8lcOutcome) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	body, err := json.MarshalIndent(map[string]any{
		"matrix":   matrix,
		"go":       runtime.Version(),
		"host":     runtime.GOOS + "/" + runtime.GOARCH,
		"recorded": time.Now().UTC().Format(time.RFC3339),
		"rows":     outcomes,
	}, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(body, '\n'), 0o600)
}

func w8lcRenderOutcomes(matrix string, outcomes []w8lcOutcome) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s outcome table\n", matrix)
	for _, out := range outcomes {
		fmt.Fprintf(&b, "  %-42s %-14s %5.1fs  %s\n", out.Name, strings.ToUpper(out.Status), out.Seconds, strings.Join(out.Gates, "; "))
		if out.Reason != "" {
			fmt.Fprintf(&b, "      reason: %s\n", out.Reason)
		}
		for _, note := range out.Notes {
			fmt.Fprintf(&b, "      · %s\n", note)
		}
	}
	return b.String()
}

// ---------------------------------------------------------------------------
// The private-daemon environment the rows drive.
// ---------------------------------------------------------------------------

// w8lcEnv is one private daemon over one generated corpus. It owns nothing the
// fixture does not already own; it is the place the rows' shared questions
// ("what does the census say", "which generation answered") are asked once.
type w8lcEnv struct {
	t      *testing.T
	f      *issue767Fixture
	spec   w8FixtureSpec
	marker string
}

// w8lcSmallSpec is the corpus every lifecycle row indexes: large enough that a
// committed build has real cross-file resolution to do, small enough that a
// cold index is seconds rather than minutes — this file starts a daemon per
// row, and a row's evidence is about lifecycle, not about corpus size.
func w8lcSmallSpec(seed int64) w8FixtureSpec {
	return w8FixtureSpec{Files: 48, Packages: 6, Seed: seed}
}

func w8lcNewEnv(t *testing.T, binary string, spec w8FixtureSpec, repoConfig string) *w8lcEnv {
	t.Helper()
	spec = spec.normalize()
	files := w8GenerateFixture(spec)
	f := newIssue767FixtureWithCorpus(t, binary, func(f *issue767Fixture) {
		for _, file := range files {
			f.write(filepath.Join(f.primary, filepath.FromSlash(file.Path)), file.Content)
		}
		if repoConfig != "" {
			f.write(filepath.Join(f.primary, ".gortex.yaml"), repoConfig)
		}
	})
	e := &w8lcEnv{t: t, f: f, spec: spec, marker: spec.Marker}
	return e
}

// start brings the private daemon up and waits for the cold index to answer
// for the corpus's own marker out of the primary's own marker.go.
func (e *w8lcEnv) start() {
	e.t.Helper()
	e.f.start()
	e.f.awaitSymbolIn(e.f.primary, e.marker, filepath.Join(e.f.primary, "marker.go"), 5*time.Minute)
}

func (e *w8lcEnv) markerPath(root string) string { return filepath.Join(root, "marker.go") }

// addWorktree creates a linked checkout and gives it its own marker
// declaration, so every later question about that checkout can be asked as
// "this name, out of this file" rather than "this name somewhere".
func (e *w8lcEnv) addWorktree(name, branch, marker string) string {
	e.t.Helper()
	path := filepath.Join(e.f.root, name)
	e.f.git(e.f.primary, "worktree", "add", "-b", branch, path)
	e.f.write(e.markerPath(path), issue767MarkerSource(e.marker, marker))
	return path
}

// status decodes `gortex daemon status --format json` — the public census.
func (e *w8lcEnv) status() daemon.StatusResponse {
	e.t.Helper()
	var status daemon.StatusResponse
	output, err := e.f.tryCommand(60*time.Second, e.f.primary, "daemon", "status", "--format", "json", "--no-progress")
	if err != nil {
		e.t.Fatalf("daemon status: %v\n%s", err, w8Tail(output))
	}
	if err := json.Unmarshal(output, &status); err != nil {
		e.t.Fatalf("daemon status is not JSON: %v\n%s", err, w8Tail(output))
	}
	return status
}

func (e *w8lcEnv) counters() map[string]int64 {
	status := e.status()
	if status.Views == nil {
		return map[string]int64{}
	}
	return status.Views.Counters
}

// w8lcFamilyList is the shape `gortex repos families --format json` answers.
// Only the fields the matrix asserts on are decoded: this is a probe of the
// catalog census, not a second renderer for it.
type w8lcFamilyList struct {
	Families []struct {
		FamilyID  string `json:"family_id"`
		CommonDir string `json:"common_dir"`
		Checkouts []struct {
			CheckoutID    string `json:"checkout_id"`
			AdminName     string `json:"admin_name"`
			RootPath      string `json:"root_path"`
			State         string `json:"state"`
			EffectiveMode string `json:"effective_mode"`
			DesiredMode   string `json:"desired_mode"`
			GraphID       string `json:"graph_id"`
			HeadCommit    string `json:"head_commit"`
			Route         *struct {
				GraphID            string `json:"graph_id"`
				CommitGenerationID int64  `json:"commit_generation_id"`
				DirtyGenerationID  int64  `json:"dirty_generation_id"`
				Ready              bool   `json:"ready"`
				State              string `json:"state"`
				RouteEpoch         int64  `json:"route_epoch"`
			} `json:"route"`
		} `json:"checkouts"`
		Graphs []struct {
			GraphID            string `json:"graph_id"`
			RepoPrefix         string `json:"repo_prefix"`
			ActiveGenerationID int64  `json:"active_generation_id"`
		} `json:"graphs"`
	} `json:"families"`
}

// families is the census asked through the primary. It is the ordinary case and
// it waits, for the reason awaitFamiliesFrom documents.
func (e *w8lcEnv) families() w8lcFamilyList {
	e.t.Helper()
	list, _ := e.awaitFamiliesFrom(e.f.primary, 2*time.Minute)
	return list
}

// tryFamiliesFrom asks `gortex repos families --format json` through one
// tracked path. The path matters: the CLI refuses the verb outright for a
// repository the daemon does not own
// (cmd/gortex/query.go:93, via ErrRepoNotTracked), so a census asked through a
// checkout that was just closed is a refusal about the QUESTION, not an answer
// about the catalog.
func (e *w8lcEnv) tryFamiliesFrom(root string) (w8lcFamilyList, error) {
	var list w8lcFamilyList
	output, err := e.f.tryCommand(60*time.Second, root, "repos", "families", "--format", "json", "--index", root, "--no-progress")
	if err != nil {
		return list, fmt.Errorf("repos families --index %s: %w: %s", root, err, w8Tail(output))
	}
	if err := json.Unmarshal(output, &list); err != nil {
		return list, fmt.Errorf("repos families --index %s is not JSON: %w: %s", root, err, w8Tail(output))
	}
	return list, nil
}

// awaitFamiliesFrom waits for the census to become answerable through one path
// and reports how long that took.
//
// The wait is not politeness, it is a measurement. A daemon that has just come
// back from a SIGKILL answers `daemon status` — the socket is up and the status
// census is served — before it has re-admitted its repositories, and in that
// window this verb returns the CLI's repo-not-tracked refusal for a repository
// the same daemon's own status already lists as tracked. That window was
// observed on this branch (matrix6 restart_and_crash, first run), so the row
// records the latency instead of reading the first refusal as lost tracking.
// The wait is bounded: a census that never comes back is still a failure.
func (e *w8lcEnv) awaitFamiliesFrom(root string, timeout time.Duration) (w8lcFamilyList, time.Duration) {
	e.t.Helper()
	start := time.Now()
	var last error
	for time.Since(start) < timeout {
		list, err := e.tryFamiliesFrom(root)
		if err == nil {
			return list, time.Since(start)
		}
		last = err
		time.Sleep(time.Second)
	}
	e.t.Fatalf("the catalog census never answered through %s within %s: %v", root, timeout, last)
	return w8lcFamilyList{}, timeout
}

// checkoutIdentities is the catalog's identity census: checkout id → its root
// path. Restart and crash rows compare it before and after, because "catalog
// and tracking integrity preserved" is a statement about identities, not about
// how many rows happen to exist.
func (l w8lcFamilyList) checkoutIdentities() map[string]string {
	out := map[string]string{}
	for _, family := range l.Families {
		for _, checkout := range family.Checkouts {
			out[checkout.CheckoutID] = checkout.RootPath
		}
	}
	return out
}

func (l w8lcFamilyList) checkoutAt(path string) (state, mode string, found bool) {
	for _, family := range l.Families {
		for _, checkout := range family.Checkouts {
			if filepath.Clean(checkout.RootPath) == filepath.Clean(path) {
				return checkout.State, checkout.EffectiveMode, true
			}
		}
	}
	return "", "", false
}

// graphPrefixes is every corpus the catalog still holds, family by family. The
// primary-removal row reads it because "the sibling survived" is a statement
// about corpora: the family's own primary is gone and the independent
// instance's graph is still there.
func (l w8lcFamilyList) graphPrefixes() []string {
	var out []string
	for _, family := range l.Families {
		for _, graph := range family.Graphs {
			out = append(out, graph.RepoPrefix)
		}
	}
	sort.Strings(out)
	return out
}

func w8lcTrackedPrefixes(status daemon.StatusResponse) []string {
	var out []string
	for _, repo := range status.TrackedRepos {
		out = append(out, repo.Prefix)
	}
	sort.Strings(out)
	return out
}

// ---------------------------------------------------------------------------
// The pure verdicts. Every one of them is the rule a row asserts, lifted out of
// the row so a unit test can drive it without a daemon — and so a mutation to
// the rule fails a test instead of quietly widening what the matrix accepts.
// ---------------------------------------------------------------------------

// w8lcCounterSum totals every label set of one counter series. Histogram
// entries (the "|count" / "|total_ms" spellings) are not counters and are never
// summed into one.
func w8lcCounterSum(counters map[string]int64, series string) int64 {
	var total int64
	for key, value := range counters {
		if strings.Contains(key, "|") {
			continue
		}
		if key == series || strings.HasPrefix(key, series+"{") {
			total += value
		}
	}
	return total
}

// w8lcCounterWith totals the label sets of one series that carry one label
// assignment, e.g. w8lcCounterWith(c, DedicatedBaseClaimTotal, "outcome=built").
func w8lcCounterWith(counters map[string]int64, series, label string) int64 {
	var total int64
	for key, value := range counters {
		if strings.Contains(key, "|") {
			continue
		}
		if !strings.HasPrefix(key, series+"{") {
			continue
		}
		if strings.Contains(key, "{"+label+",") || strings.Contains(key, ","+label+",") ||
			strings.Contains(key, ","+label+"}") || strings.Contains(key, "{"+label+"}") {
			total += value
		}
	}
	return total
}

// w8lcCounterDelta is after − before over every series either side carries.
// A series that disappeared is reported as its negation rather than dropped:
// a counter that went down is a fact worth seeing, not a fact worth losing.
func w8lcCounterDelta(before, after map[string]int64) map[string]int64 {
	delta := map[string]int64{}
	for key, value := range after {
		if value-before[key] != 0 {
			delta[key] = value - before[key]
		}
	}
	for key, value := range before {
		if _, ok := after[key]; !ok && value != 0 {
			delta[key] = -value
		}
	}
	return delta
}

// w8lcFreshness is one public answer's view label: which graph and checkout
// answered, and whether the answer claimed to be exact.
type w8lcFreshness struct {
	Surface    string `json:"surface"`
	GraphID    string `json:"graph_id"`
	CheckoutID string `json:"checkout_id"`
	Exact      bool   `json:"exact"`
	Actual     string `json:"actual_view"`
	Requested  string `json:"requested_view"`
}

// w8lcCoherent is the cross-surface rule: several surfaces asked in one settled
// state must all answer from the SAME generation of the SAME checkout, and none
// of them may present a substituted view as exact.
//
// It is the readable form of "one request never mixes generations": the graph,
// the text index and the file bytes are three indexes over one snapshot, and a
// pair of answers naming two graphs or two checkouts is the mixed view the
// handoff's cache hazard describes, whatever either answer says on its own.
func w8lcCoherent(answers []w8lcFreshness) error {
	if len(answers) == 0 {
		return errors.New("no surfaces answered")
	}
	first := answers[0]
	for _, answer := range answers {
		if !answer.Exact {
			return fmt.Errorf("surface %s did not answer exactly (actual view %q for requested %q)", answer.Surface, answer.Actual, answer.Requested)
		}
		if answer.GraphID != first.GraphID {
			return fmt.Errorf("surface %s answered from graph %q; %s answered from %q", answer.Surface, answer.GraphID, first.Surface, first.GraphID)
		}
		if answer.CheckoutID != first.CheckoutID {
			return fmt.Errorf("surface %s answered for checkout %q; %s answered for %q", answer.Surface, answer.CheckoutID, first.Surface, first.CheckoutID)
		}
	}
	return nil
}

// w8lcQuiesced is the settled-daemon rule the lifetime rows assert after the
// thing they did: nothing may be left pinning payload.
//
// views_handoffs_outstanding is the level paired with HandoffTotal{joined} —
// "it must return to zero once every detached worker has released"
// (internal/viewmetrics/catalog.go, HandoffsOutstanding). A queue that never
// drains is the same shape one level up.
func w8lcQuiesced(counters map[string]int64) error {
	if outstanding := w8lcCounterSum(counters, "views_handoffs_outstanding"); outstanding != 0 {
		return fmt.Errorf("views_handoffs_outstanding is %d, want 0: detached work still pins the generations it read", outstanding)
	}
	if queued := w8lcCounterSum(counters, "views_build_queue"); queued != 0 {
		return fmt.Errorf("views_build_queue is %d, want 0: callers are still waiting for the physical build lane", queued)
	}
	return nil
}

// w8lcQuiescenceVacuous reports that neither level w8lcQuiesced guards appears
// in the census at all.
//
// This is the difference between "nothing is outstanding" and "nothing was ever
// counted", and the first run of matrix 6 made it matter: every row's counter
// line printed "(none of the named series is present)" for
// views_handoffs_outstanding and views_build_queue, which means w8lcQuiesced
// summed two empty sets to zero and passed without guarding anything. A verdict
// that cannot fail is not evidence, so a row that is in that position records
// it instead of collecting a green it did not earn. It is deliberately NOT a
// failure: whether those series are emitted at all is W8.3's subject, not this
// matrix's, and a row here must not fail the branch for a counter another item
// owns.
func w8lcQuiescenceVacuous(counters map[string]int64) bool {
	for key := range counters {
		if strings.Contains(key, "|") {
			continue
		}
		if key == "views_handoffs_outstanding" || strings.HasPrefix(key, "views_handoffs_outstanding{") ||
			key == "views_build_queue" || strings.HasPrefix(key, "views_build_queue{") {
			return false
		}
	}
	return true
}

// w8lcQuiescedWithEvidence is w8lcQuiesced with the vacuity recorded. Rows call
// this one; the pure verdict stays pure so its own unit test can drive it.
func w8lcQuiescedWithEvidence(rec *w8lcRecorder, counters map[string]int64) error {
	if w8lcQuiescenceVacuous(counters) {
		rec.note("RECORDED: the quiescence verdict guarded nothing here — neither views_handoffs_outstanding nor views_build_queue appears in this census, so its pass is an absence of counters, not an observed drain")
	}
	return w8lcQuiesced(counters)
}

// w8lcNoStorageFailures is the gate-9 rule: a settled healthy run records no
// storage-maintenance failure. The census carries them precisely so a store
// that cannot delete anything does not look like a store with nothing to
// delete (internal/daemon/proto.go, ViewsStatus.StorageFailures).
func w8lcNoStorageFailures(views *daemon.ViewsStatus) error {
	if views == nil {
		return errors.New("daemon status carries no views census")
	}
	if len(views.StorageFailures) > 0 {
		return fmt.Errorf("%d generation(s) report a storage-maintenance failure: %+v", len(views.StorageFailures), views.StorageFailures)
	}
	if len(views.CoordinatorStartFailures) > 0 {
		return fmt.Errorf("%d checkout(s) have no build loop: %+v", len(views.CoordinatorStartFailures), views.CoordinatorStartFailures)
	}
	return nil
}

// w8lcClosureComplete is the gate-8 correctness level: a published committed
// generation whose affected-by closure was cut is knowingly incomplete, so
// views_dedicated_base_closure_truncated_total "must stay at zero"
// (internal/viewmetrics/catalog.go, DedicatedBaseClosureTruncatedTotal).
func w8lcClosureComplete(counters map[string]int64) error {
	if cut := w8lcCounterSum(counters, "views_dedicated_base_closure_truncated_total"); cut != 0 {
		return fmt.Errorf("views_dedicated_base_closure_truncated_total is %d, want 0: a committed generation was published with a truncated closure", cut)
	}
	return nil
}

// w8lcIdentitiesPreserved is the restart / crash rule: every checkout identity
// that existed before has to exist afterwards, at the same path. A daemon that
// comes back having forgotten a checkout, or having re-minted it at a new id,
// has not preserved catalog and tracking integrity however healthy it looks.
func w8lcIdentitiesPreserved(before, after map[string]string) error {
	for id, path := range before {
		got, ok := after[id]
		if !ok {
			return fmt.Errorf("checkout %s (%s) did not survive", id, path)
		}
		if filepath.Clean(got) != filepath.Clean(path) {
			return fmt.Errorf("checkout %s moved from %s to %s", id, path, got)
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Waits and probes.
// ---------------------------------------------------------------------------

// awaitExact waits for one name to answer from one file through the spelling
// the checkout's mode is served by.
func (e *w8lcEnv) awaitExact(root, name, file string, spelling issue767Spelling) {
	e.t.Helper()
	e.f.awaitSymbolAs(root, name, file, 3*time.Minute, spelling)
}

// awaitAbsent waits for a name to stop answering at all for one file.
//
// The negative is deliberately stricter than the positive's mirror: it waits
// for answer.Found to be false, which is true only when NO node of that name
// appears anywhere in the response. A leak from a sibling checkout, from the
// primary's own copy, or from a stale generation all still carry the name, so
// none of them can pass this.
func (e *w8lcEnv) awaitAbsent(root, name, file string, spelling issue767Spelling, timeout time.Duration) {
	e.t.Helper()
	var last string
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		answer, err := e.f.askSymbol(root, name, file, spelling)
		switch {
		case err != nil:
			last = err.Error()
		case !answer.Found:
			return
		default:
			last = fmt.Sprintf("still answering (prefix %q, exact=%v)", answer.Prefix, answer.Exact)
		}
		time.Sleep(time.Second)
	}
	e.t.Fatalf("%s still answers for %s after %s: %s", name, root, timeout, last)
}

// awaitExactWithin is awaitExact without the fatal: it reports whether the
// answer arrived inside the window. A row that has to tell "recovered on its
// own" from "recovered only after a restart" needs the question asked without
// the first answer ending the row.
func (e *w8lcEnv) awaitExactWithin(root, name, file string, spelling issue767Spelling, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if found, err := e.f.trySearchSymbolAs(root, name, file, spelling); err == nil && found {
			return true
		}
		time.Sleep(2 * time.Second)
	}
	return false
}

// askFrom is issue767Fixture.askSymbol with the working directory chosen by the
// caller instead of taken from the checkout being asked about.
//
// The failed-build row needs exactly this. It makes a checkout's directory
// unreadable, and a CLI whose own cwd IS that directory cannot even exec — the
// "refusal" would be the client's fork/exec failing, not the daemon declining
// to serve a tree it cannot read. Asking from a readable directory keeps the
// evidence about the daemon.
func (e *w8lcEnv) askFrom(dir, root, name, file string, spelling issue767Spelling) (issue767Answer, error) {
	answer := issue767Answer{Spelling: spelling.String()}
	payload, err := json.Marshal(issue767SearchRequest(root, name, spelling))
	if err != nil {
		return answer, err
	}
	output, err := e.f.tryCommand(60*time.Second, dir, "call", "search", "--index", root, "--json", string(payload), "--format", "json")
	if err != nil {
		answer.Error = fmt.Sprintf("search command: %v: %s", err, w8Tail(output))
		return answer, fmt.Errorf("search command: %w: %s", err, w8Tail(output))
	}
	var value any
	if err := json.Unmarshal(output, &value); err != nil {
		answer.Error = fmt.Sprintf("search response: %v: %s", err, w8Tail(output))
		return answer, fmt.Errorf("search response: %w: %s", err, w8Tail(output))
	}
	answer.Found, answer.Fallback = issue767JSONEvidence(value, name)
	answer.Exact = issue767JSONExact(value)
	answer.FromExpectedFile = issue767JSONSource(value, name, file)
	answer.Prefix = issue767JSONPrefix(value, name, file)
	return answer, nil
}

// freshnessOf asks one tool and extracts the view label from its answer.
func (e *w8lcEnv) freshnessOf(surface, root, tool string, request map[string]any) (w8lcFreshness, error) {
	payload, err := json.Marshal(request)
	if err != nil {
		return w8lcFreshness{}, err
	}
	output, err := e.f.tryCommand(60*time.Second, root, "call", tool, "--index", root, "--json", string(payload), "--format", "json")
	if err != nil {
		return w8lcFreshness{Surface: surface}, fmt.Errorf("%s: %w: %s", surface, err, w8Tail(output))
	}
	var value any
	if err := json.Unmarshal(output, &value); err != nil {
		return w8lcFreshness{Surface: surface}, fmt.Errorf("%s response: %w: %s", surface, err, w8Tail(output))
	}
	found := w8lcFindFreshness(value)
	if found == nil {
		return w8lcFreshness{Surface: surface}, fmt.Errorf("%s answered with no freshness block: %s", surface, w8Tail(output))
	}
	found.Surface = surface
	return *found, nil
}

// w8lcFindFreshness pulls the first freshness block out of a tool answer,
// including one nested inside a JSON string payload (the MCP structured-content
// spelling the CLI relays).
func w8lcFindFreshness(value any) *w8lcFreshness {
	switch value := value.(type) {
	case map[string]any:
		if raw, ok := value["freshness"].(map[string]any); ok {
			out := &w8lcFreshness{}
			out.GraphID, _ = raw["graph_id"].(string)
			out.CheckoutID, _ = raw["checkout_id"].(string)
			out.Exact, _ = raw["exact"].(bool)
			out.Actual, _ = raw["actual_view"].(string)
			out.Requested, _ = raw["requested_view"].(string)
			return out
		}
		for _, child := range value {
			if found := w8lcFindFreshness(child); found != nil {
				return found
			}
		}
	case []any:
		for _, child := range value {
			if found := w8lcFindFreshness(child); found != nil {
				return found
			}
		}
	case string:
		var child any
		if (strings.HasPrefix(value, "{") || strings.HasPrefix(value, "[")) && json.Unmarshal([]byte(value), &child) == nil {
			return w8lcFindFreshness(child)
		}
	}
	return nil
}

// commitEdit advances main by one commit: a new marker declaration plus a real
// body edit in one package file, so the commit is a semantic change rather than
// a touch.
func (e *w8lcEnv) commitEdit(index, revision int, marker, message string) {
	e.t.Helper()
	e.f.write(e.markerPath(e.f.primary), issue767MarkerSource(e.marker, marker))
	// The index is folded into the corpus rather than trusted: a row that
	// advances more times than the corpus has files would otherwise walk off
	// the end and quietly commit the marker alone, turning every later advance
	// into a one-line change the caller did not ask for.
	if e.spec.Files > 0 {
		index %= e.spec.Files
	}
	path := filepath.Join(e.f.primary, filepath.FromSlash(w8FilePath(index%e.spec.Packages, index)))
	if source, err := os.ReadFile(path); err == nil {
		if edited, err := w8EditFileSource(string(source), index, revision); err == nil {
			e.f.write(path, edited)
		}
	}
	e.f.git(e.f.primary, "add", "-A")
	e.f.git(e.f.primary, "commit", "-m", message)
}

// crash kills the private daemon outright and reaps it. SIGKILL is the point:
// the child gets no chance to finish a publication, flush a log or run a
// shutdown drain, which is the recovery condition gate 9 names.
func (e *w8lcEnv) crash() {
	e.t.Helper()
	cmd, done, cancel, log := e.f.takeChild()
	if cmd == nil {
		e.t.Fatal("no private daemon to crash")
	}
	if err := cmd.Process.Kill(); err != nil {
		e.t.Fatalf("killing the private daemon: %v", err)
	}
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		e.t.Fatal("the killed daemon did not exit")
	}
	if cancel != nil {
		cancel()
	}
	if log != nil {
		_ = log.Close()
	}
}

// stopOrKill shuts the private daemon down and reports whether SIGINT was
// enough, killing it if not.
//
// It exists because the fixture's own stop() raises a test error when a child
// needs a force kill, and one row deliberately restarts a daemon whose store
// volume just ran out of space — a process that may well be wedged. That is a
// FINDING about disk-full behaviour, and a finding belongs in the row's
// evidence, not in a generic cleanup error that fails the row for a reason its
// assertions never named. So the shutdown is measured here and recorded by the
// caller.
func (e *w8lcEnv) stopOrKill(grace time.Duration) bool {
	cmd, done, cancel, log := e.f.takeChild()
	if cancel != nil {
		defer cancel()
	}
	if cmd == nil {
		return true
	}
	defer func() {
		if log != nil {
			_ = log.Close()
		}
	}()
	_ = cmd.Process.Signal(os.Interrupt)
	select {
	case <-done:
		return true
	case <-time.After(grace):
	}
	_ = cmd.Process.Kill()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		e.t.Errorf("the private daemon did not exit even after SIGKILL")
	}
	return false
}

// daemonLog is the current child's log, for rows that assert a log line and a
// counter agree.
func (e *w8lcEnv) daemonLog() string {
	body, err := os.ReadFile(e.f.logPath())
	if err != nil {
		return ""
	}
	return string(body)
}

// ---------------------------------------------------------------------------
// Matrix 6.
// ---------------------------------------------------------------------------

// TestW8Matrix6Lifecycle is the opt-in run. Every row drives its own private
// daemon, because the rows are destructive to the thing they exercise —
// removing a primary, killing a daemon, filling a volume — and a shared daemon
// would make each row's evidence a statement about the previous row's damage.
func TestW8Matrix6Lifecycle(t *testing.T) {
	binary := w8lcRequireBinary(t)
	rows := w8lcMatrix6Rows(binary)
	w8lcRunMatrix(t, "matrix6_lifecycle", rows, w8lcMatrix6Bullets, w8lcArtifactDir(t))
}

func w8lcRequireBinary(t *testing.T) string {
	t.Helper()
	binary := os.Getenv(w8lcBinaryEnv)
	if binary == "" {
		t.Skipf("set %s to opt into the isolated end-to-end matrix", w8lcBinaryEnv)
	}
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skipf("the private-daemon fixture supports Darwin and Linux")
	}
	absolute, err := filepath.Abs(binary)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(absolute); err != nil {
		t.Fatal(err)
	}
	return absolute
}

func w8lcArtifactDir(t *testing.T) string {
	t.Helper()
	dir := os.Getenv(w8lcArtifactEnv)
	if dir == "" {
		dir = t.TempDir()
		t.Logf("%s is unset; outcome tables go to %s and are removed with the test", w8lcArtifactEnv, dir)
	}
	return dir
}

func w8lcMatrix6Rows(binary string) []w8lcRow {
	return []w8lcRow{
		{
			Name:   "untrack_remove_recreate",
			Bullet: "public untrack/remove/recreate",
			Gates:  []string{w8lcGateLifetime, w8lcGateSnapshot},
			Run:    func(t *testing.T, rec *w8lcRecorder) { w8lcRowUntrackRemoveRecreate(t, rec, binary) },
		},
		{
			Name:   "primary_removal_dedicated_sibling",
			Bullet: "primary removal with preserved independent dedicated siblings",
			Gates:  []string{w8lcGateLifetime, w8lcGateStorage},
			Run:    func(t *testing.T, rec *w8lcRecorder) { w8lcRowPrimaryRemoval(t, rec, binary) },
		},
		{
			Name:   "drains_and_late_readers",
			Bullet: "drains and late readers/workers",
			Gates:  []string{w8lcGateLifetime},
			Run:    func(t *testing.T, rec *w8lcRecorder) { w8lcRowDrains(t, rec, binary) },
		},
		{
			Name:   "duplicate_triggers",
			Bullet: "duplicate triggers",
			Gates:  []string{w8lcGateBounded, w8lcGateAuthority},
			Run:    func(t *testing.T, rec *w8lcRecorder) { w8lcRowDuplicateTriggers(t, rec, binary) },
		},
		{
			Name:   "cancellation",
			Bullet: "cancellation",
			Gates:  []string{w8lcGateLifetime},
			Run:    func(t *testing.T, rec *w8lcRecorder) { w8lcRowCancellation(t, rec, binary) },
		},
		{
			Name:   "failed_build_and_retry",
			Bullet: "failed build and retry",
			Gates:  []string{w8lcGateLifetime, w8lcGateSnapshot},
			Run:    func(t *testing.T, rec *w8lcRecorder) { w8lcRowFailedBuildAndRetry(t, rec, binary) },
		},
		{
			Name:   "restart_and_crash",
			Bullet: "isolated daemon restart and crash recovery",
			Gates:  []string{w8lcGateStorage, w8lcGateAuthority},
			Run:    func(t *testing.T, rec *w8lcRecorder) { w8lcRowRestartAndCrash(t, rec, binary) },
		},
		{
			Name:   "disk_full_recovery",
			Bullet: "disk-full recovery",
			Gates:  []string{w8lcGateStorage, w8lcGateBounded},
			Run:    func(t *testing.T, rec *w8lcRecorder) { w8lcRowDiskFull(t, rec, binary) },
		},
	}
}

// w8lcRowUntrackRemoveRecreate walks one checkout through the whole public
// lifecycle: discovered → dedicated → demoted → removed → recreated at the same
// path.
//
// The recreation is the part that matters. "stale publishers / drain handles /
// cleanup callbacks must not affect replacement registrations at the same path
// or with reused IDs" (handoff §6) — so after the path is reused, the OLD
// checkout's marker must not answer from it, and the NEW one's must, exactly.
func w8lcRowUntrackRemoveRecreate(t *testing.T, rec *w8lcRecorder, binary string) {
	e := w8lcNewEnv(t, binary, w8lcSmallSpec(8101), "")
	e.start()

	worktree := e.addWorktree("wt01", "w01", "W8Lifecycle01First")
	first, file := "W8Lifecycle01First", e.markerPath(filepath.Join(e.f.root, "wt01"))
	e.awaitExact(worktree, first, file, issue767AsAutomaticWorktree)
	rec.note("discovered checkout answers exactly through the worktree view selector")

	output, err := e.f.tryCommand(8*time.Minute, worktree, "track", worktree, "--as-worktree", "--wait", "--wait-timeout", "5m", "--no-progress")
	if err != nil {
		t.Fatalf("track --as-worktree: %v\n%s", err, w8Tail(output))
	}
	e.awaitExact(worktree, first, file, issue767AsOwnCorpus)
	answer, err := e.f.askSymbol(worktree, first, file, issue767AsOwnCorpus)
	if err != nil {
		t.Fatalf("dedicated answer: %v", err)
	}
	rec.note("dedicated checkout answers from corpus %q", answer.Prefix)

	output, err = e.f.tryCommand(8*time.Minute, e.f.primary, "untrack", worktree, "--no-progress")
	if err != nil {
		t.Fatalf("untrack: %v\n%s", err, w8Tail(output))
	}
	e.awaitExact(worktree, first, file, issue767AsAutomaticWorktree)
	rec.note("untrack demoted the checkout back into the family's automatic lane: %s", w8Tail(output))

	if _, err := e.f.tryGit(e.f.primary, "worktree", "remove", "--force", worktree); err != nil {
		t.Fatalf("worktree remove: %v", err)
	}
	e.f.awaitRemovedPath(worktree)
	rec.note("the daemon's own cleanup dropped the removed checkout's catalog row")

	// Same path, new identity. The tree is a fresh checkout of main, so the old
	// marker genuinely is not in it; a hit would be a resurrected generation.
	second := "W8Lifecycle01Second"
	e.f.git(e.f.primary, "worktree", "add", "-b", "w01b", worktree)
	e.f.write(file, issue767MarkerSource(e.marker, second))
	e.awaitExact(worktree, second, file, issue767AsAutomaticWorktree)
	e.awaitAbsent(worktree, first, file, issue767AsAutomaticWorktree, 2*time.Minute)
	rec.note("the recreated checkout answers for its own marker and never for the removed one")

	// The quiescence rules are about a SETTLED daemon: a build still in flight
	// is a queue that has not drained yet, not a queue that never will.
	e.f.settle()
	counters := e.counters()
	if err := w8lcQuiescedWithEvidence(rec, counters); err != nil {
		t.Error(err)
	}
	if err := w8lcClosureComplete(counters); err != nil {
		t.Error(err)
	}
	if err := w8lcNoStorageFailures(e.status().Views); err != nil {
		t.Error(err)
	}
}

// w8lcRowPrimaryRemoval removes a family's primary while an independently
// tracked dedicated sibling exists.
//
// Two contracts, both from the source: untrack previews a row-removing plan and
// runs it only with --confirm (cmd/gortex/track.go, untrackCmd.Long), and a
// preview writes nothing. What the row then asserts is the survival: an
// independent instance is a corpus of its own, so removing the family's primary
// must not take it with it.
func w8lcRowPrimaryRemoval(t *testing.T, rec *w8lcRecorder, binary string) {
	e := w8lcNewEnv(t, binary, w8lcSmallSpec(8102), "")
	e.start()

	sibling := e.addWorktree("wt01", "w01", "W8Lifecycle02Sibling")
	marker, file := "W8Lifecycle02Sibling", e.markerPath(sibling)
	e.awaitExact(sibling, marker, file, issue767AsAutomaticWorktree)
	output, err := e.f.tryCommand(8*time.Minute, sibling, "track", sibling, "--as-worktree", "--wait", "--wait-timeout", "5m", "--no-progress")
	if err != nil {
		t.Fatalf("track --as-worktree: %v\n%s", err, w8Tail(output))
	}
	e.awaitExact(sibling, marker, file, issue767AsOwnCorpus)
	rec.note("the sibling is an independent instance before the primary is touched")

	// The preview half: no --confirm, so nothing is written.
	preview, err := e.f.tryCommand(4*time.Minute, e.f.root, "untrack", e.f.primary, "--format", "json", "--no-progress")
	if err != nil {
		t.Fatalf("untrack preview: %v\n%s", err, w8Tail(preview))
	}
	var plan map[string]any
	if err := json.Unmarshal(preview, &plan); err != nil {
		t.Fatalf("untrack preview is not JSON: %v\n%s", err, w8Tail(preview))
	}
	if status, _ := plan["status"].(string); status != "preview" {
		t.Fatalf("untrack of a primary reported %q, want a preview", status)
	}
	if confirm, _ := plan["confirm_required"].(bool); !confirm {
		t.Fatal("untrack of a primary did not require --confirm")
	}
	rec.note("untrack previewed plan %v (sole_primary=%v) and wrote nothing", plan["plan"], plan["sole_primary"])
	e.awaitExact(e.f.primary, e.marker, e.markerPath(e.f.primary), issue767AsPrimary)
	e.awaitExact(sibling, marker, file, issue767AsOwnCorpus)
	rec.note("after the preview both corpora still answer")

	output, err = e.f.tryCommand(8*time.Minute, e.f.root, "untrack", e.f.primary, "--confirm", "--format", "json", "--no-progress")
	if err != nil {
		t.Fatalf("untrack --confirm: %v\n%s", err, w8Tail(output))
	}
	rec.note("primary closure ran: %s", w8Tail(output))

	// The surviving independent instance is the assertion. It owns its own
	// corpus, so it has to keep answering from its own file.
	e.awaitExact(sibling, marker, file, issue767AsOwnCorpus)
	e.f.settle()
	status := e.status()
	// The census is asked through the SIBLING, not through the primary. The
	// primary was just closed, and the CLI refuses this verb for a repository
	// the daemon no longer owns (cmd/gortex/query.go:93) — asking through it
	// would end the row on a refusal about the question rather than on the
	// answer the row exists to read. The surviving independent instance is
	// exactly the path that is still entitled to ask, which is the point.
	if _, err := e.tryFamiliesFrom(e.f.primary); err == nil {
		rec.note("RECORDED: the closed primary still answers the catalog census verb")
	} else {
		rec.note("the closed primary no longer answers the census verb (%v); the census is read through the surviving sibling", err)
	}
	families, waited := e.awaitFamiliesFrom(sibling, 2*time.Minute)
	rec.note("tracked prefixes after primary closure: %v; catalog corpora seen from the sibling: %v (census answered in %s)",
		w8lcTrackedPrefixes(status), families.graphPrefixes(), waited.Round(time.Millisecond))
	if _, _, present := families.checkoutAt(e.f.primary); present {
		rec.note("RECORDED: the closed primary's checkout row is still in the catalog census")
	}
	// The survival claim, read off the census rather than only off a query:
	// the sibling's own corpus is still a corpus the catalog holds.
	if _, _, present := families.checkoutAt(sibling); !present {
		t.Fatalf("the independent dedicated sibling is no longer in the catalog census after the family's primary was closed; corpora: %v", families.graphPrefixes())
	}
	if err := w8lcNoStorageFailures(status.Views); err != nil {
		t.Error(err)
	}
	if err := w8lcQuiescedWithEvidence(rec, e.counters()); err != nil {
		t.Error(err)
	}
}

// w8lcRowDrains hammers a checkout with readers while its registration is
// closed underneath them.
//
// "Closing rejects new admission, drains existing work and finalizes only the
// captured registration" (gate 7). What a public reader can see of that is
// narrow but real: no answer may be a fallback wearing an exact label, and once
// the dust settles nothing may still be pinning payload.
func w8lcRowDrains(t *testing.T, rec *w8lcRecorder, binary string) {
	e := w8lcNewEnv(t, binary, w8lcSmallSpec(8103), "")
	e.start()

	worktree := e.addWorktree("wt01", "w01", "W8Lifecycle03Reader")
	marker, file := "W8Lifecycle03Reader", e.markerPath(worktree)
	e.awaitExact(worktree, marker, file, issue767AsAutomaticWorktree)
	output, err := e.f.tryCommand(8*time.Minute, worktree, "track", worktree, "--as-worktree", "--wait", "--wait-timeout", "5m", "--no-progress")
	if err != nil {
		t.Fatalf("track --as-worktree: %v\n%s", err, w8Tail(output))
	}
	e.awaitExact(worktree, marker, file, issue767AsOwnCorpus)

	stop := make(chan struct{})
	var wg sync.WaitGroup
	var mu sync.Mutex
	var answers, refusals, lies int
	for reader := 0; reader < 4; reader++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				// Both spellings are asked, because during the demotion the
				// checkout is crossing between the two doors and a reader that
				// only knew one of them could not tell a refusal from a lie.
				for _, spelling := range []issue767Spelling{issue767AsOwnCorpus, issue767AsAutomaticWorktree} {
					answer, err := e.f.askSymbol(worktree, marker, file, spelling)
					mu.Lock()
					switch {
					case err != nil:
						refusals++
					case answer.Fallback:
						lies++
					case answer.Found && !answer.FromExpectedFile:
						lies++
					default:
						answers++
					}
					mu.Unlock()
				}
			}
		}()
	}

	// The late worker: an untrack while the readers are mid-flight.
	time.Sleep(2 * time.Second)
	output, err = e.f.tryCommand(8*time.Minute, e.f.primary, "untrack", worktree, "--no-progress")
	close(stop)
	wg.Wait()
	if err != nil {
		t.Fatalf("untrack under load: %v\n%s", err, w8Tail(output))
	}
	mu.Lock()
	rec.note("readers during the drain: %d coherent answers, %d clean refusals, %d incoherent", answers, refusals, lies)
	incoherent := lies
	mu.Unlock()
	if incoherent != 0 {
		t.Fatalf("%d reader answers were a fallback or came from the wrong file while the registration closed", incoherent)
	}

	// The demoted checkout is served through the family again, and the late
	// readers left nothing behind.
	e.awaitExact(worktree, marker, file, issue767AsAutomaticWorktree)
	e.f.settle()
	counters := e.counters()
	rec.note("drain counters: %s", w8lcCounterLine(counters, "views_dedicated_base_drain_total", "views_handoff_total", "views_handoffs_outstanding"))
	if err := w8lcQuiescedWithEvidence(rec, counters); err != nil {
		t.Error(err)
	}
	if err := w8lcNoStorageFailures(e.status().Views); err != nil {
		t.Error(err)
	}
}

// w8lcRowDuplicateTriggers asks the same question many times at once and
// asserts the daemon bought at most one physical build for it.
//
// The counter vocabulary states the rule this row reads:
// DedicatedBaseClaimTotal{outcome=built} "is the only one that represents
// physical payload work"; reused is the zero-catalog-DML replay and coalesced
// joined a build already running (internal/viewmetrics/catalog.go). One commit
// observed N times must therefore not produce N builts.
func w8lcRowDuplicateTriggers(t *testing.T, rec *w8lcRecorder, binary string) {
	e := w8lcNewEnv(t, binary, w8lcSmallSpec(8104), "")
	e.start()
	e.f.settle()
	before := e.counters()

	e.commitEdit(3, 1, "W8Lifecycle04Advance", "advance once")

	// Every one of these is a trigger for the same observation.
	var wg sync.WaitGroup
	for trigger := 0; trigger < 6; trigger++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = e.f.tryCommand(4*time.Minute, e.f.primary, "repos", "reconcile", "--no-progress")
		}()
	}
	wg.Wait()
	e.awaitExact(e.f.primary, "W8Lifecycle04Advance", e.markerPath(e.f.primary), issue767AsPrimary)
	e.f.settle()

	after := e.counters()
	delta := w8lcCounterDelta(before, after)
	built := w8lcCounterWith(delta, "views_dedicated_base_claim_total", "outcome=built")
	reused := w8lcCounterWith(delta, "views_dedicated_base_claim_total", "outcome=reused")
	coalesced := w8lcCounterWith(delta, "views_dedicated_base_claim_total", "outcome=coalesced")
	repeats := w8lcCounterWith(delta, "views_dedicated_base_advance_total", "outcome=repeat")
	rec.note("one commit, six concurrent triggers: built=%d reused=%d coalesced=%d advance_repeat=%d", built, reused, coalesced, repeats)
	rec.note("cycle outcomes: %s", w8lcCounterLine(delta, "views_coordinator_cycle_total"))
	if built > 1 {
		t.Fatalf("six duplicate triggers over one commit bought %d physical builds, want at most 1", built)
	}
	if err := w8lcClosureComplete(after); err != nil {
		t.Error(err)
	}
	if err := w8lcQuiescedWithEvidence(rec, after); err != nil {
		t.Error(err)
	}
}

// w8lcRowCancellation kills requests mid-flight and asserts the daemon is
// unharmed.
//
// "Borrowed readers and source providers must remain alive until actual worker
// completion, including cancellation tails. Do not release on handler return
// while workers still use the payload" (handoff §6). A caller that walks away
// is the ordinary shape of that, and the public evidence is that the next
// request is answered exactly and nothing is left outstanding.
func w8lcRowCancellation(t *testing.T, rec *w8lcRecorder, binary string) {
	e := w8lcNewEnv(t, binary, w8lcSmallSpec(8105), "")
	e.start()
	worktree := e.addWorktree("wt01", "w01", "W8Lifecycle05Cancel")
	marker, file := "W8Lifecycle05Cancel", e.markerPath(worktree)
	e.awaitExact(worktree, marker, file, issue767AsAutomaticWorktree)

	// Work in flight, then callers that abandon it.
	//
	// The deadline is MEASURED, not guessed. A fixed 300 ms budget was tried
	// first and the row recorded "0 of 8 requests were abandoned": every
	// request finished well inside it, so nothing was ever cancelled and the
	// bullet was exercised by a row that passed. The deadline is therefore
	// derived from what the request actually costs on this host, which is the
	// only way to be inside it rather than after it.
	e.commitEdit(5, 1, "W8Lifecycle05Advance", "advance during cancellation")
	request := map[string]any{"operation": "symbols", "query": "Issue767", "options": map[string]any{"limit": 500, "expand": "off"}}
	payload, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	baseline := time.Now()
	if _, err := e.f.tryCommand(2*time.Minute, worktree, "call", "search", "--index", worktree, "--json", string(payload), "--format", "json"); err != nil {
		t.Fatalf("the request the row cancels does not succeed uncancelled: %v", err)
	}
	full := time.Since(baseline)
	rec.note("the request this row abandons costs %s uncancelled", full.Round(time.Millisecond))

	// A ladder, not one budget: the fraction of that cost at which the client
	// is still connected and the daemon is still working is a property of this
	// host, so the row walks down the ladder and counts what each rung did.
	abandoned, completed := 0, 0
	var rungs []string
	for _, fraction := range []float64{0.1, 0.2, 0.3, 0.4, 0.5, 0.6, 0.7, 0.8} {
		budget := time.Duration(float64(full) * fraction)
		if budget < 5*time.Millisecond {
			budget = 5 * time.Millisecond
		}
		if _, err := e.f.tryCommand(budget, worktree, "call", "search", "--index", worktree, "--json", string(payload), "--format", "json"); err != nil {
			abandoned++
			rungs = append(rungs, fmt.Sprintf("%s:abandoned", budget.Round(time.Millisecond)))
		} else {
			completed++
			rungs = append(rungs, fmt.Sprintf("%s:completed", budget.Round(time.Millisecond)))
		}
	}
	rec.note("%d abandoned / %d completed across the deadline ladder: %s", abandoned, completed, strings.Join(rungs, " "))
	if abandoned == 0 {
		// Not a failure of the branch — a failure of the row to create the
		// condition it exists to test. It is recorded as such rather than
		// collected as a pass for a bullet nothing exercised.
		rec.note("NOT EXERCISED: no rung of the ladder killed a client mid-flight on this host (uncancelled cost %s); the cancellation bullet was not driven and only the recovery half below is evidence", full.Round(time.Millisecond))
	}

	// The daemon has to be exactly as usable afterwards.
	e.awaitExact(e.f.primary, "W8Lifecycle05Advance", e.markerPath(e.f.primary), issue767AsPrimary)
	e.awaitExact(worktree, marker, file, issue767AsAutomaticWorktree)
	e.f.settle()
	counters := e.counters()
	rec.note("after the cancellations: %s", w8lcCounterLine(counters, "views_handoff_total", "views_handoffs_outstanding", "views_build_queue"))
	if err := w8lcQuiescedWithEvidence(rec, counters); err != nil {
		t.Error(err)
	}
	if err := w8lcNoStorageFailures(e.status().Views); err != nil {
		t.Error(err)
	}
}

// w8lcRowFailedBuildAndRetry makes a checkout's tree unreadable under the
// daemon, advances main so a build is actually attempted for it, and then gives
// the tree back.
//
// This is the public shape of a failed build: the coordinator cannot read the
// working copy it is supposed to compose. The two halves asserted are the two
// halves of the contract — while the tree is unreadable the daemon may refuse
// but may never answer a stale view wearing an exact label, and once the tree
// returns the retry has to succeed on its own.
func w8lcRowFailedBuildAndRetry(t *testing.T, rec *w8lcRecorder, binary string) {
	e := w8lcNewEnv(t, binary, w8lcSmallSpec(8106), "")
	e.start()
	worktree := e.addWorktree("wt01", "w01", "W8Lifecycle06Before")
	before, file := "W8Lifecycle06Before", e.markerPath(worktree)
	e.awaitExact(worktree, before, file, issue767AsAutomaticWorktree)
	e.f.settle()

	restored := false
	restore := func() {
		if !restored {
			_ = os.Chmod(worktree, 0o700)
			restored = true
		}
	}
	t.Cleanup(restore)
	if err := os.Chmod(worktree, 0o000); err != nil {
		rec.skipRow(t, "matrix6 failed_build_and_retry: this host will not make a directory unreadable to its owner: "+err.Error())
	}
	if entries, err := os.ReadDir(worktree); err == nil {
		restore()
		rec.skipRow(t, fmt.Sprintf("matrix6 failed_build_and_retry: a 0000 directory is still readable on this host (%d entries); the build cannot be made to fail this way", len(entries)))
	}

	// Advance main so the dependent has a reason to rebuild while it is
	// unreadable, and give the reconciler a pass over it.
	e.commitEdit(7, 1, "W8Lifecycle06Advance", "advance while the dependent is unreadable")
	e.awaitExact(e.f.primary, "W8Lifecycle06Advance", e.markerPath(e.f.primary), issue767AsPrimary)
	_, _ = e.f.tryCommand(4*time.Minute, e.f.primary, "repos", "reconcile", "--no-progress")
	time.Sleep(10 * time.Second)

	// Asked from the fixture root, not from inside the unreadable checkout:
	// see askFrom. A query whose cwd is the 0000 directory fails in the CLI's
	// own fork/exec and would prove nothing about the daemon.
	answer, err := e.askFrom(e.f.root, worktree, before, file, issue767AsAutomaticWorktree)
	switch {
	case err != nil:
		rec.note("while the tree was unreadable the checkout refused: %v", err)
	case answer.Fallback:
		t.Fatal("an unreadable checkout was answered with a fallback")
	case answer.Found && !answer.FromExpectedFile:
		t.Fatal("an unreadable checkout was answered from another file")
	default:
		rec.note("while the tree was unreadable the checkout answered from its own file (found=%v exact=%v)", answer.Found, answer.Exact)
	}
	state, mode, found := e.families().checkoutAt(worktree)
	rec.note("catalog census during the failure: state=%q mode=%q present=%v", state, mode, found)

	restore()
	// The retry is the daemon's own: nothing is re-tracked, nothing is
	// restarted. The tree came back and the next reconciliation has to take it.
	_, _ = e.f.tryCommand(4*time.Minute, e.f.primary, "repos", "reconcile", "--no-progress")
	after := "W8Lifecycle06After"
	e.f.write(file, issue767MarkerSource(e.marker, before, after))
	e.awaitExact(worktree, after, file, issue767AsAutomaticWorktree)
	rec.note("after the tree returned the checkout rebuilt and answers exactly again")

	state, mode, _ = e.families().checkoutAt(worktree)
	rec.note("catalog census after the retry: state=%q mode=%q", state, mode)
	e.f.settle()
	if err := w8lcQuiescedWithEvidence(rec, e.counters()); err != nil {
		t.Error(err)
	}
}

// w8lcRowRestartAndCrash takes the private daemon down twice: once politely,
// once with SIGKILL while a publication is in flight.
//
// The assertion is gate 9's, in its own words: "crashes ... preserve
// catalog/tracking integrity". Preserved means the identities come back — the
// same checkouts, at the same paths, in the same family — and the view answers
// the committed state again without being re-tracked.
func w8lcRowRestartAndCrash(t *testing.T, rec *w8lcRecorder, binary string) {
	e := w8lcNewEnv(t, binary, w8lcSmallSpec(8107), "")
	e.start()
	worktree := e.addWorktree("wt01", "w01", "W8Lifecycle07Dependent")
	dependent, file := "W8Lifecycle07Dependent", e.markerPath(worktree)
	e.awaitExact(worktree, dependent, file, issue767AsAutomaticWorktree)
	e.f.settle()

	identities := e.families().checkoutIdentities()
	tracked := w8lcTrackedPrefixes(e.status())
	rec.note("before the restart: %d checkouts, tracked prefixes %v", len(identities), tracked)

	// A polite restart first: the warm path has to come back on its own.
	e.f.stop()
	e.f.start()
	e.awaitExact(e.f.primary, e.marker, e.markerPath(e.f.primary), issue767AsPrimary)
	e.awaitExact(worktree, dependent, file, issue767AsAutomaticWorktree)
	if err := w8lcIdentitiesPreserved(identities, e.families().checkoutIdentities()); err != nil {
		t.Errorf("after a polite restart: %v", err)
	}
	rec.note("a polite restart preserved every checkout identity and both views answer")

	// Now the crash, taken while a committed publication is in flight: the
	// commit lands and the daemon is killed without waiting for the advance.
	e.commitEdit(9, 1, "W8Lifecycle07Crash", "advance, then crash")
	time.Sleep(1500 * time.Millisecond)
	e.crash()
	rec.note("SIGKILL delivered ~1.5s after the commit that triggers the advance")

	e.f.start()
	after := e.status()
	if got := w8lcTrackedPrefixes(after); !w8lcSameStrings(tracked, got) {
		t.Fatalf("tracking did not survive the crash: %v, want %v", got, tracked)
	}
	// The census is waited for, and the wait is the evidence. On the first run
	// of this row the daemon answered `daemon status` — listing exactly the
	// tracked prefixes above — while `repos families` still returned the CLI's
	// repo-not-tracked refusal: the socket and the status census come back
	// before the repositories are re-admitted. That is a readiness window, not
	// lost tracking, and the row records how wide it was.
	crashed, waited := e.awaitFamiliesFrom(e.f.primary, 2*time.Minute)
	rec.note("after the crash the catalog census became answerable in %s", waited.Round(time.Millisecond))
	if waited > 2*time.Second {
		rec.note("RECORDED: `daemon status` answered immediately after the crash restart but the catalog census verb refused for %s; a caller that reads the first refusal as lost tracking would be wrong", waited.Round(time.Millisecond))
	}
	if err := w8lcIdentitiesPreserved(identities, crashed.checkoutIdentities()); err != nil {
		t.Errorf("after the crash: %v", err)
	}
	// The committed state has to be reachable again without being asked twice.
	e.awaitExact(e.f.primary, "W8Lifecycle07Crash", e.markerPath(e.f.primary), issue767AsPrimary)
	e.awaitExact(worktree, dependent, file, issue767AsAutomaticWorktree)
	e.f.settle()

	status := e.status()
	if status.Views != nil {
		rec.note("after the crash: generations %v, leases %d, checkouts %v", status.Views.Generations, status.Views.Leases, status.Views.Checkouts)
	}
	if err := w8lcNoStorageFailures(status.Views); err != nil {
		t.Error(err)
	}
	if err := w8lcClosureComplete(e.counters()); err != nil {
		t.Error(err)
	}
}

func w8lcSameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// w8lcRowDiskFull puts the private store on a small volume, fills it, and asks
// what the daemon does — then gives the space back and asks whether it recovers.
//
// The volume is a disk image this row attaches and detaches itself. That is a
// host-level side effect outside the private root, so it is behind its own
// opt-in: without GXW8_MATRIX_DISKFULL=1 the row records a named skip rather
// than a pass. macOS offers no per-directory quota, and the store's own
// SQLITE_FULL seam (internal/graph/store_sqlite/storage_error_sqlite_full_test.go
// drives PRAGMA max_page_count on the Store's own writer connection) is
// in-process — it is not reachable from a child daemon through the environment,
// so a real small volume is the only public route to this row.
func w8lcRowDiskFull(t *testing.T, rec *w8lcRecorder, binary string) {
	if os.Getenv(w8lcDiskFullEnv) != "1" {
		rec.skipRow(t, "matrix6 disk_full_recovery: set "+w8lcDiskFullEnv+"=1 to let this row attach a private disk image (no per-directory quota exists on macOS, and the store's SQLITE_FULL seam is in-process and unreachable from a child daemon)")
	}
	if runtime.GOOS != "darwin" {
		rec.skipRow(t, "matrix6 disk_full_recovery: the small-volume route is implemented with hdiutil and runs on Darwin only")
	}
	volume := w8lcAttachSmallVolume(t, rec, 48)

	e := w8lcNewEnv(t, binary, w8lcSmallSpec(8108), "")
	// The store — and only the store — lives on the small volume. The fixture
	// has not started its child yet, so this is the path the daemon opens.
	e.f.store = filepath.Join(volume, "store.sqlite")
	e.start()
	rec.note("the private store is on a %s volume", w8lcVolumeFree(volume))

	// Leave a sliver of space and make the daemon write.
	filler := filepath.Join(volume, "filler")
	if err := w8lcFillVolume(filler, 256*1024); err != nil {
		rec.note("could not fill the volume: %v", err)
	}
	rec.note("volume filled to %s free", w8lcVolumeFree(volume))
	for commit := 1; commit <= 3; commit++ {
		e.commitEdit(10+commit, commit, fmt.Sprintf("W8Lifecycle08Full%02d", commit), fmt.Sprintf("advance on a full volume %d", commit))
	}
	_, _ = e.f.tryCommand(4*time.Minute, e.f.primary, "repos", "reconcile", "--no-progress")
	time.Sleep(20 * time.Second)

	// Whatever the daemon did, it must not have lied about it. Either it is
	// alive and says so in the census, or it is gone — both are recorded.
	alive := true
	statusOutput, err := e.f.tryCommand(60*time.Second, e.f.primary, "daemon", "status", "--format", "json", "--no-progress")
	if err != nil {
		alive = false
		rec.note("the daemon did not answer status on a full volume: %v", err)
	} else {
		var status daemon.StatusResponse
		if err := json.Unmarshal(statusOutput, &status); err == nil && status.Views != nil {
			rec.note("on a full volume the census reports %d storage failure(s): %+v", len(status.Views.StorageFailures), status.Views.StorageFailures)
		}
	}
	logged := strings.Contains(e.daemonLog(), "SQLite code 13") || strings.Contains(e.daemonLog(), "database or disk is full") || strings.Contains(e.daemonLog(), "disk is full")
	rec.note("the daemon log names the full volume: %v", logged)

	// Give the space back; the store has to become usable again.
	if err := os.Remove(filler); err != nil {
		t.Fatalf("freeing the volume: %v", err)
	}
	rec.note("volume freed to %s", w8lcVolumeFree(volume))
	if !alive {
		e.f.start()
		rec.note("the daemon was restarted because the full volume took it down")
	}
	recovery := "W8Lifecycle08Recovered"
	e.commitEdit(20, 1, recovery, "advance after the volume was freed")
	_, _ = e.f.tryCommand(4*time.Minute, e.f.primary, "repos", "reconcile", "--no-progress")

	// Two different questions, and the row reports which one recovery needed.
	// Gate 9 asks that a disk-full "preserve catalog/tracking integrity" with
	// "bounded peak-space requirements" — it does not promise that the LIVE
	// daemon retries the build it lost. So a live retry is measured, a restart
	// is the fallback, and only a store that is still unusable after both is a
	// failure of the gate.
	if e.awaitExactWithin(e.f.primary, recovery, e.markerPath(e.f.primary), issue767AsPrimary, 2*time.Minute) {
		rec.note("the running daemon retried on its own once the space came back")
	} else {
		rec.note("GAP: the running daemon did not re-index after the space came back; the failed build was not retried in this process")
		if !e.stopOrKill(30 * time.Second) {
			rec.note("GAP: the daemon whose store volume had filled did not honour SIGINT within 30s and needed SIGKILL")
		}
		e.f.start()
		e.awaitExact(e.f.primary, recovery, e.markerPath(e.f.primary), issue767AsPrimary)
		rec.note("a restart recovered the store: the primary answers for a marker committed while the volume was full")
	}

	// Catalog and tracking integrity, which is what gate 9 asks about.
	status := e.status()
	if prefixes := w8lcTrackedPrefixes(status); len(prefixes) == 0 {
		t.Fatal("tracking did not survive the full volume")
	} else {
		rec.note("tracked prefixes after recovery: %v", prefixes)
	}
	if len(e.families().Families) == 0 {
		t.Fatal("the catalog lost every family across the full volume")
	}
}

// w8lcAttachSmallVolume creates and attaches a private disk image of megabytes
// megabytes, mounted inside the test's own directory, and detaches it on
// cleanup. It never touches a volume it did not create.
func w8lcAttachSmallVolume(t *testing.T, rec *w8lcRecorder, megabytes int) string {
	t.Helper()
	root, err := os.MkdirTemp("/tmp", "gxh-w810-vol-")
	if err != nil {
		t.Fatal(err)
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	image, mount := filepath.Join(root, "store.dmg"), filepath.Join(root, "mnt")
	if err := os.MkdirAll(mount, 0o700); err != nil {
		t.Fatal(err)
	}
	if output, err := exec.Command("hdiutil", "create", "-size", fmt.Sprintf("%dm", megabytes), "-fs", "HFS+", "-volname", "GXW8MATRIX", "-quiet", image).CombinedOutput(); err != nil {
		rec.skipRow(t, fmt.Sprintf("matrix6 disk_full_recovery: this host will not create a disk image: %v: %s", err, w8Tail(output)))
	}
	if output, err := exec.Command("hdiutil", "attach", "-mountpoint", mount, "-nobrowse", "-quiet", image).CombinedOutput(); err != nil {
		rec.skipRow(t, fmt.Sprintf("matrix6 disk_full_recovery: this host will not attach a disk image: %v: %s", err, w8Tail(output)))
	}
	t.Cleanup(func() {
		if output, err := exec.Command("hdiutil", "detach", mount, "-force", "-quiet").CombinedOutput(); err != nil {
			t.Logf("detaching the private volume: %v: %s", err, w8Tail(output))
		}
	})
	return mount
}

// w8lcFillVolume writes a filler file until the volume has at most keepFree
// bytes left, so the next real write is the one that runs out.
func w8lcFillVolume(path string, keepFree int64) error {
	file, err := os.Create(path)
	if err != nil {
		return err
	}
	defer func() { _ = file.Close() }()
	block := make([]byte, 1<<20)
	for {
		if _, err := file.Write(block); err != nil {
			return nil // the volume is full: that is the point
		}
		if err := file.Sync(); err != nil {
			return nil
		}
		free, err := w8lcFreeBytes(filepath.Dir(path))
		if err != nil {
			return err
		}
		if free <= keepFree {
			return nil
		}
	}
}

// w8lcFreeBytes is the free space on the volume holding path. Only the
// disk-full row uses it, and only against a volume this file created.
func w8lcFreeBytes(path string) (int64, error) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return 0, err
	}
	return int64(uint64(stat.Bavail) * uint64(stat.Bsize)), nil
}

func w8lcVolumeFree(path string) string {
	free, err := w8lcFreeBytes(path)
	if err != nil {
		return "unknown"
	}
	return fmt.Sprintf("%.1f MiB", float64(free)/(1<<20))
}

// w8lcCounterLine renders selected counter series for a note.
func w8lcCounterLine(counters map[string]int64, series ...string) string {
	var parts []string
	for _, name := range series {
		var keys []string
		for key := range counters {
			if key == name || strings.HasPrefix(key, name+"{") || strings.HasPrefix(key, name+"|") {
				keys = append(keys, key)
			}
		}
		sort.Strings(keys)
		for _, key := range keys {
			parts = append(parts, fmt.Sprintf("%s=%d", key, counters[key]))
		}
	}
	if len(parts) == 0 {
		return "(none of the named series is present)"
	}
	return strings.Join(parts, " ")
}

// ---------------------------------------------------------------------------
// The non-opt-in tests: the table's contract, the driver's wiring and every
// verdict the rows delegate to.
// ---------------------------------------------------------------------------

func TestW8Matrix6RowsCoverEveryBriefBulletAndNameTheirGates(t *testing.T) {
	rows := w8lcMatrix6Rows("/nonexistent/gortex")
	if err := w8lcValidateRows("matrix6_lifecycle", rows, w8lcMatrix6Bullets); err != nil {
		t.Fatal(err)
	}
	if len(rows) != len(w8lcMatrix6Bullets) {
		t.Fatalf("%d rows for %d brief bullets: a bullet is either unclaimed or claimed twice", len(rows), len(w8lcMatrix6Bullets))
	}
	// Every gate the sixth matrix bullet exists to serve has to be named by at
	// least one row, or the run's outcome table cannot be read as evidence for
	// it. The execution plan files W8.10 under gates 7, 9 and 10.
	for _, gate := range []string{w8lcGateLifetime, w8lcGateStorage} {
		named := false
		for _, row := range rows {
			for _, declared := range row.Gates {
				if declared == gate {
					named = true
				}
			}
		}
		if !named {
			t.Errorf("no matrix-6 row serves %s", gate)
		}
	}
}

func TestW8MatrixRowValidationRefusesAnUnreadableTable(t *testing.T) {
	body := func(*testing.T, *w8lcRecorder) {}
	for _, tc := range []struct {
		name string
		rows []w8lcRow
		want string
	}{
		{"no gate", []w8lcRow{{Name: "a", Bullet: "b", Run: body}}, "names no acceptance gate"},
		{"unknown gate", []w8lcRow{{Name: "a", Bullet: "b", Gates: []string{"gate-99 invented"}, Run: body}}, "unknown gate"},
		{"no bullet", []w8lcRow{{Name: "a", Gates: []string{w8lcGateLifetime}, Run: body}}, "claims no brief bullet"},
		{"duplicate row", []w8lcRow{
			{Name: "a", Bullet: "b", Gates: []string{w8lcGateLifetime}, Run: body},
			{Name: "a", Bullet: "c", Gates: []string{w8lcGateLifetime}, Run: body},
		}, "duplicate row"},
		{"two rows one bullet", []w8lcRow{
			{Name: "a", Bullet: "b", Gates: []string{w8lcGateLifetime}, Run: body},
			{Name: "c", Bullet: "b", Gates: []string{w8lcGateLifetime}, Run: body},
		}, "both claim bullet"},
		{"silent omission", []w8lcRow{{Name: "a", Bullet: "b", Gates: []string{w8lcGateLifetime}}}, "no body and no reason"},
		{"body and excuse", []w8lcRow{{Name: "a", Bullet: "b", Gates: []string{w8lcGateLifetime}, Run: body, Unexercised: "because"}}, "both runs and declares"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := w8lcValidateRows("m", tc.rows, []string{"b"})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("validation returned %v, want an error containing %q", err, tc.want)
			}
		})
	}
	// An unclaimed bullet is the failure that matters most: it is how a matrix
	// quietly shrinks.
	err := w8lcValidateRows("m", []w8lcRow{{Name: "a", Bullet: "b", Gates: []string{w8lcGateLifetime}, Run: body}}, []string{"b", "unclaimed"})
	if err == nil || !strings.Contains(err.Error(), "is claimed by no row") {
		t.Fatalf("validation returned %v, want the unclaimed bullet named", err)
	}
}

func TestW8MatrixOutcomeStatusIsNeverASilentPass(t *testing.T) {
	body := func(*testing.T, *w8lcRecorder) {}
	for _, tc := range []struct {
		name   string
		row    w8lcRow
		ran    bool
		passed bool
		skip   string
		want   string
		reason string
	}{
		{"pass", w8lcRow{Name: "r", Run: body}, true, true, "", w8lcStatusPass, ""},
		{"fail", w8lcRow{Name: "r", Run: body}, true, false, "", w8lcStatusFail, ""},
		{"skip beats pass", w8lcRow{Name: "r", Run: body}, true, true, "no quota filesystem", w8lcStatusSkipped, "no quota filesystem"},
		{"unexercised beats pass", w8lcRow{Name: "r", Unexercised: "the branch does not handle it"}, false, true, "", w8lcStatusNotExercised, "the branch does not handle it"},
		// The one Go's runner hands you for free: a filtered-out subtest is
		// "not failed", and a table that took that for a pass would report a
		// green matrix for rows it never drove.
		{"filtered beats pass", w8lcRow{Name: "r", Run: body}, false, true, "", w8lcStatusFiltered, "the row body never ran: -test.run excluded this subtest, so its result is not evidence"},
		{"filtered beats skip", w8lcRow{Name: "r", Run: body}, false, true, "a reason recorded by an earlier run", w8lcStatusFiltered, "the row body never ran: -test.run excluded this subtest, so its result is not evidence"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := w8lcRecordOutcome("m", tc.row, tc.ran, tc.passed, 2*time.Second, []string{"note"}, tc.skip)
			if out.Status != tc.want {
				t.Fatalf("status %q, want %q", out.Status, tc.want)
			}
			if out.Reason != tc.reason {
				t.Fatalf("reason %q, want %q", out.Reason, tc.reason)
			}
			if out.Seconds != 2 {
				t.Fatalf("seconds %v, want the measured 2", out.Seconds)
			}
		})
	}
}

func TestW8MatrixCounterArithmetic(t *testing.T) {
	before := map[string]int64{
		"views_dedicated_base_claim_total{outcome=built}":    2,
		"views_dedicated_base_claim_total{outcome=reused}":   1,
		"views_coordinator_build_seconds{slot=commit}|count": 5,
		"views_handoffs_outstanding":                         3,
	}
	after := map[string]int64{
		"views_dedicated_base_claim_total{outcome=built}":     3,
		"views_dedicated_base_claim_total{outcome=coalesced}": 4,
		"views_coordinator_build_seconds{slot=commit}|count":  9,
		"views_dedicated_base_closure_truncated_total":        1,
	}
	delta := w8lcCounterDelta(before, after)
	if delta["views_dedicated_base_claim_total{outcome=built}"] != 1 {
		t.Fatalf("built delta %d, want 1", delta["views_dedicated_base_claim_total{outcome=built}"])
	}
	if delta["views_dedicated_base_claim_total{outcome=coalesced}"] != 4 {
		t.Fatalf("coalesced delta %d, want 4", delta["views_dedicated_base_claim_total{outcome=coalesced}"])
	}
	// A series that vanished is reported as its negation, not dropped.
	if delta["views_dedicated_base_claim_total{outcome=reused}"] != -1 {
		t.Fatalf("a disappearing series was lost: %v", delta)
	}
	if delta["views_handoffs_outstanding"] != -3 {
		t.Fatalf("a disappearing level was lost: %v", delta)
	}
	// A histogram is not a counter and must never be summed into one.
	if sum := w8lcCounterSum(after, "views_coordinator_build_seconds"); sum != 0 {
		t.Fatalf("histogram entries were summed as a counter: %d", sum)
	}
	if sum := w8lcCounterSum(after, "views_dedicated_base_claim_total"); sum != 7 {
		t.Fatalf("claim total %d, want 3+4", sum)
	}
	if got := w8lcCounterWith(after, "views_dedicated_base_claim_total", "outcome=built"); got != 3 {
		t.Fatalf("built %d, want 3", got)
	}
	if got := w8lcCounterWith(after, "views_dedicated_base_claim_total", "outcome=reused"); got != 0 {
		t.Fatalf("a label set that is absent answered %d, want 0", got)
	}
}

func TestW8MatrixVerdictsRefuseTheThingTheyGuard(t *testing.T) {
	t.Run("closure completeness", func(t *testing.T) {
		if err := w8lcClosureComplete(map[string]int64{"views_dedicated_base_publish_total{shape=root}": 3}); err != nil {
			t.Fatalf("a healthy run was refused: %v", err)
		}
		if err := w8lcClosureComplete(map[string]int64{"views_dedicated_base_closure_truncated_total": 1}); err == nil {
			t.Fatal("a knowingly incomplete committed generation was accepted")
		}
	})
	t.Run("quiesced", func(t *testing.T) {
		if err := w8lcQuiesced(map[string]int64{"views_coordinators": 1}); err != nil {
			t.Fatalf("a settled daemon was refused: %v", err)
		}
		// The pass above is exactly the vacuous one the rows have to record:
		// neither guarded series is in that census, so the verdict summed two
		// empty sets. w8lcQuiescenceVacuous is what makes that visible.
		if !w8lcQuiescenceVacuous(map[string]int64{"views_coordinators": 1}) {
			t.Fatal("a census carrying neither guarded series was not reported as a vacuous pass")
		}
		if w8lcQuiescenceVacuous(map[string]int64{"views_handoffs_outstanding": 0}) {
			t.Fatal("a census that does carry the outstanding level was called vacuous")
		}
		if w8lcQuiescenceVacuous(map[string]int64{"views_build_queue{priority=interactive}": 0}) {
			t.Fatal("a labelled build-queue level was not recognised as the series it is")
		}
		// A histogram entry is not the level: views_build_wait_seconds|count
		// must not be mistaken for views_build_queue's presence.
		if !w8lcQuiescenceVacuous(map[string]int64{"views_build_queue|count": 3}) {
			t.Fatal("a histogram spelling was accepted as the level's presence")
		}
		if err := w8lcQuiesced(map[string]int64{"views_handoffs_outstanding": 1}); err == nil {
			t.Fatal("an unreleased handoff was accepted")
		}
		if err := w8lcQuiesced(map[string]int64{"views_build_queue": 2}); err == nil {
			t.Fatal("a queue that never drained was accepted")
		}
	})
	t.Run("coherence", func(t *testing.T) {
		base := w8lcFreshness{Surface: "graph", GraphID: "g1", CheckoutID: "c1", Exact: true}
		text := w8lcFreshness{Surface: "text", GraphID: "g1", CheckoutID: "c1", Exact: true}
		if err := w8lcCoherent([]w8lcFreshness{base, text}); err != nil {
			t.Fatalf("two surfaces on one snapshot were refused: %v", err)
		}
		mixed := text
		mixed.GraphID = "g2"
		if err := w8lcCoherent([]w8lcFreshness{base, mixed}); err == nil {
			t.Fatal("one request mixing two graphs was accepted")
		}
		otherCheckout := text
		otherCheckout.CheckoutID = "c2"
		if err := w8lcCoherent([]w8lcFreshness{base, otherCheckout}); err == nil {
			t.Fatal("one request mixing two checkouts was accepted")
		}
		fallback := text
		fallback.Exact = false
		fallback.Actual = "repo:issue767"
		if err := w8lcCoherent([]w8lcFreshness{base, fallback}); err == nil {
			t.Fatal("a substituted view was accepted as an exact answer")
		}
		if err := w8lcCoherent(nil); err == nil {
			t.Fatal("an empty answer set passed for coherence")
		}
	})
	t.Run("identities", func(t *testing.T) {
		before := map[string]string{"c1": "/a", "c2": "/b"}
		if err := w8lcIdentitiesPreserved(before, map[string]string{"c1": "/a", "c2": "/b", "c3": "/c"}); err != nil {
			t.Fatalf("a restart that also discovered a checkout was refused: %v", err)
		}
		if err := w8lcIdentitiesPreserved(before, map[string]string{"c1": "/a"}); err == nil {
			t.Fatal("a forgotten checkout was accepted as preserved integrity")
		}
		if err := w8lcIdentitiesPreserved(before, map[string]string{"c1": "/a", "c2": "/moved"}); err == nil {
			t.Fatal("a checkout that changed path was accepted as preserved integrity")
		}
	})
	t.Run("storage failures", func(t *testing.T) {
		if err := w8lcNoStorageFailures(&daemon.ViewsStatus{Families: 1}); err != nil {
			t.Fatalf("a healthy census was refused: %v", err)
		}
		if err := w8lcNoStorageFailures(nil); err == nil {
			t.Fatal("a missing census passed for a healthy one")
		}
		if err := w8lcNoStorageFailures(&daemon.ViewsStatus{StorageFailures: []daemon.StorageFailure{{}}}); err == nil {
			t.Fatal("a storage-maintenance failure was accepted")
		}
		if err := w8lcNoStorageFailures(&daemon.ViewsStatus{CoordinatorStartFailures: []daemon.CoordinatorStartFailure{{}}}); err == nil {
			t.Fatal("a checkout with no build loop was accepted")
		}
	})
}

func TestW8MatrixFreshnessIsFoundWhereverTheToolPutsIt(t *testing.T) {
	// The CLI relays MCP structured content, which can arrive as a JSON string
	// inside the envelope. A probe that only looked at the top level would read
	// every such answer as "no freshness block" and the coherence rule would
	// never run.
	nested := `{"content":[{"type":"text","text":"{\"freshness\":{\"exact\":true,\"graph_id\":\"g1\",\"checkout_id\":\"c1\",\"actual_view\":\"worktree:c1\"}}"}]}`
	var value any
	if err := json.Unmarshal([]byte(nested), &value); err != nil {
		t.Fatal(err)
	}
	found := w8lcFindFreshness(value)
	if found == nil {
		t.Fatal("no freshness block was found inside the relayed structured content")
	}
	if !found.Exact || found.GraphID != "g1" || found.CheckoutID != "c1" {
		t.Fatalf("the freshness block lost its labels: %+v", found)
	}
	if w8lcFindFreshness(map[string]any{"count": 0}) != nil {
		t.Fatal("an answer with no freshness block reported one")
	}
}

// TestW8MatrixDriverFilesTheTableOfAFailingRow is the wiring test: the
// production entrypoint — w8lcRunMatrix, the call TestW8Matrix6Lifecycle makes
// — has to file a table in which a REALLY failing row is recorded as failed,
// with the evidence it took before it failed. A row's t.Fatal is a
// runtime.Goexit out of the subtest's goroutine, so this cannot be faked with
// a stub; the child process is what lets the row really fail.
func TestW8MatrixDriverFilesTheTableOfAFailingRow(t *testing.T) {
	if dir := os.Getenv(w8lcChildEnv); dir != "" {
		w8lcRunFailingMatrixChild(t, dir)
		return
	}
	dir := t.TempDir()
	child := exec.Command(os.Args[0], "-test.run=^TestW8MatrixDriverFilesTheTableOfAFailingRow$", "-test.v=true")
	child.Env = append(os.Environ(), w8lcChildEnv+"="+dir)
	output, err := child.CombinedOutput()
	if err == nil {
		t.Fatalf("the child matrix was supposed to fail a row:\n%s", output)
	}
	if !strings.Contains(string(output), "deliberate row failure") {
		t.Fatalf("the child failed for the wrong reason:\n%s", output)
	}
	var filed struct {
		Matrix string        `json:"matrix"`
		Rows   []w8lcOutcome `json:"rows"`
	}
	w8ReadJSON(t, filepath.Join(dir, "childmatrix.json"), &filed)
	if filed.Matrix != "childmatrix" {
		t.Fatalf("the table lost its matrix name: %q", filed.Matrix)
	}
	if len(filed.Rows) != 3 {
		t.Fatalf("the table carries %d rows, want all three", len(filed.Rows))
	}
	byName := map[string]w8lcOutcome{}
	for _, row := range filed.Rows {
		byName[row.Name] = row
	}
	if got := byName["passing"]; got.Status != w8lcStatusPass || len(got.Notes) == 0 {
		t.Errorf("the passing row lost its evidence: %+v", got)
	}
	if got := byName["failing"]; got.Status != w8lcStatusFail {
		t.Errorf("the failing row is recorded as %q", got.Status)
	} else if len(got.Notes) == 0 {
		t.Error("the failing row's notes did not survive its unwind")
	}
	if got := byName["unexercised"]; got.Status != w8lcStatusNotExercised || got.Reason == "" {
		t.Errorf("the unexercised row is recorded as %+v, want a named not_exercised", got)
	}
	if !strings.Contains(string(output), "childmatrix outcome table") {
		t.Error("the rendered table was not logged")
	}
}

// TestW8MatrixDriverRecordsAFilteredRowAsNotEvidence is the second wiring
// test, and it drives the same production entrypoint through the trap Go's
// runner sets: a subtest excluded by -test.run is never executed, and t.Run
// still reports true for it. A driver that believed that would file a table in
// which a row nobody ran is a pass — the exact silent pass the brief forbids,
// arriving through the runner rather than through a skip.
//
// The child is real: it re-execs this binary with a row filter, so the
// "filtered" verdict comes from the runner's own behaviour and not from a
// value this test handed the recorder.
func TestW8MatrixDriverRecordsAFilteredRowAsNotEvidence(t *testing.T) {
	if os.Getenv(w8lcChildEnv) != "" {
		// The child arm is TestW8MatrixDriverFilesTheTableOfAFailingRow's;
		// this test only ever runs as the parent.
		t.Skip("child processes drive the other driver test")
	}
	dir := t.TempDir()
	child := exec.Command(os.Args[0],
		"-test.run=^TestW8MatrixDriverFilesTheTableOfAFailingRow$/^passing$", "-test.v=true")
	child.Env = append(os.Environ(), w8lcChildEnv+"="+dir)
	output, err := child.CombinedOutput()
	if err != nil {
		t.Fatalf("the filtered child was supposed to succeed (the failing row is filtered out): %v\n%s", err, output)
	}
	if strings.Contains(string(output), "deliberate row failure") {
		t.Fatalf("the filtered row ran anyway:\n%s", output)
	}
	var filed struct {
		Rows []w8lcOutcome `json:"rows"`
	}
	w8ReadJSON(t, filepath.Join(dir, "childmatrix.json"), &filed)
	byName := map[string]w8lcOutcome{}
	for _, row := range filed.Rows {
		byName[row.Name] = row
	}
	if got := byName["passing"]; got.Status != w8lcStatusPass {
		t.Errorf("the row the filter selected is recorded as %q, want a pass", got.Status)
	}
	got := byName["failing"]
	if got.Status != w8lcStatusFiltered {
		t.Fatalf("a row the runner never executed is recorded as %q; a filtered row read as a pass is a green matrix for work nobody did", got.Status)
	}
	if got.Reason == "" {
		t.Error("the filtered row carries no reason, so the table cannot be read as a partial run")
	}
	if len(got.Notes) != 0 {
		t.Errorf("a row that never ran carries evidence: %v", got.Notes)
	}
	if byName["unexercised"].Status != w8lcStatusNotExercised {
		t.Errorf("the unexercised row changed status under a filter: %q", byName["unexercised"].Status)
	}
}

func w8lcRunFailingMatrixChild(t *testing.T, dir string) {
	rows := []w8lcRow{
		{
			Name: "passing", Bullet: "one", Gates: []string{w8lcGateLifetime},
			Run: func(t *testing.T, rec *w8lcRecorder) { rec.note("the passing row recorded this") },
		},
		{
			Name: "failing", Bullet: "two", Gates: []string{w8lcGateStorage},
			Run: func(t *testing.T, rec *w8lcRecorder) {
				rec.note("the failing row recorded this before it failed")
				t.Fatal("deliberate row failure")
			},
		},
		{
			Name: "unexercised", Bullet: "three", Gates: []string{w8lcGateBounded},
			Unexercised: "matrix child: nothing to drive",
		},
	}
	w8lcRunMatrix(t, "childmatrix", rows, []string{"one", "two", "three"}, dir)
}
