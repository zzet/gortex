package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

// startup-admission measures the acceptance window for a new linked worktree:
// an exact indexed search response must arrive <= --budget after
// t0 = max(worktree discovery, publication of the worktree's OWN valid committed
// base), while an UNRELATED family stays in startup >= --hold-min past t0.
//
// Two own-base cases are covered:
//   - prepared (--prepare, default): family A's committed base is published by an
//     unmeasured preparation boot over a separate A checkout. The measured
//     worktree is created after that daemon stops, so the measured boot discovers
//     it fresh with its family base already servable; t0 = BootStart. Family B's
//     large corpus holds the global view-build gate closed well past t0.
//   - during-startup (--prepare=false): A's base is published during the measured
//     boot; t0 is its adoption instant, read from the catalog by a dedicated tight
//     poll goroutine (no control calls in that loop, so t0 is not biased late).
//
// Readiness is one indexed search response carrying the exact worktree rider
// (view.exact && view.kind=="worktree" && view.repo_prefix==A && view.checkout_id
// == the measured worktree's catalog ID) AND a hit named the fixture marker —
// never a listening socket, a catalog row, or an empty exact view. It must land
// before B's base publishes and before the gate opens; a missing B witness fails.
// Every trial reports its own latency; a timeout, daemon exit, or negative latency
// is a failure, never averaged away. The settled control discovers a FRESH worktree
// after the gate opens and times discovery->first-exact (not a warm repeat query).

type startupAdmissionOptions struct {
	Binary    string
	Scratch   string
	Repeats   int
	AFiles    int
	BFiles    int
	Functions int
	Calls     int
	Timeout   time.Duration
	HoldMin   time.Duration
	Budget    time.Duration
	Prepare   bool
	KeepState bool
}

func bindStartupAdmissionFlags(f *flag.FlagSet, o *startupAdmissionOptions) {
	f.StringVar(&o.Binary, "binary", "", "daemon binary under test")
	f.StringVar(&o.Scratch, "scratch", "", "artifact root (outside every fixture source)")
	f.IntVar(&o.Repeats, "repeats", 10, "isolated trials")
	f.IntVar(&o.AFiles, "a-files", 24, "family A corpus size (small; own base publishes fast)")
	f.IntVar(&o.BFiles, "b-files", 6000, "family B corpus size (large; holds startup past t0)")
	f.IntVar(&o.Functions, "functions", 6, "functions per fixture file")
	f.IntVar(&o.Calls, "calls", 3, "callee edges per fixture function")
	f.DurationVar(&o.Timeout, "timeout", 200*time.Second, "per-trial wall-clock bound")
	f.DurationVar(&o.HoldMin, "hold-min", 60*time.Second, "required unrelated-family startup hold past t0")
	f.DurationVar(&o.Budget, "budget", 10*time.Second, "max t0->first-exact latency")
	f.BoolVar(&o.Prepare, "prepare", true, "publish family A's base in an unmeasured preparation boot")
	f.BoolVar(&o.KeepState, "keep-state", false, "retain private daemon state")
}

type startupAdmissionTrial struct {
	Trial        int       `json:"trial"`
	Prepare      bool      `json:"prepare"`
	BootStart    time.Time `json:"boot_start"`
	ABaseAt      time.Time `json:"a_base_ready_at,omitempty"`
	T0           time.Time `json:"t0,omitempty"`
	FirstExactAt time.Time `json:"first_exact_at,omitempty"`
	LatencyMS    *float64  `json:"t0_to_first_exact_ms,omitempty"`
	GateOpenAt   time.Time `json:"gate_open_at,omitempty"`
	BBaseAt      time.Time `json:"b_base_ready_at,omitempty"`
	HoldMS       *float64  `json:"t0_to_b_base_ready_ms,omitempty"`
	GateHoldMS   *float64  `json:"t0_to_gate_open_ms,omitempty"`
	// BootGateOpenMS is boot start to gate open, independent of where t0 falls,
	// so two binaries' warmup lengths compare directly.
	BootGateOpenMS *float64 `json:"boot_to_gate_open_ms,omitempty"`
	SettledExactMS *float64 `json:"settled_control_exact_ms,omitempty"`
	// HoldAfterExact records that, right after the first exact reply, B's base
	// was still unpublished and the gate still closed (same-boot negatives that
	// the later positive witnesses then close).
	HoldAfterExact bool   `json:"hold_witnessed_after_exact"`
	Outcome        string `json:"outcome"`
	Error          string `json:"error,omitempty"`
	APrefix        string `json:"a_prefix,omitempty"`
	ACheckoutID    string `json:"a_checkout_id,omitempty"`
	BPrefix        string `json:"b_prefix,omitempty"`
	Root           string `json:"root,omitempty"`
	LogPath        string `json:"log_path,omitempty"`
}

type startupAdmissionReport struct {
	SchemaVersion int                     `json:"schema_version"`
	Binary        string                  `json:"binary"`
	Started       time.Time               `json:"started"`
	Prepare       bool                    `json:"prepare"`
	AFiles        int                     `json:"a_files"`
	BFiles        int                     `json:"b_files"`
	BudgetMS      float64                 `json:"budget_ms"`
	HoldMinMS     float64                 `json:"hold_min_ms"`
	Trials        []startupAdmissionTrial `json:"trials"`
	MaxLatencyMS  *float64                `json:"max_t0_to_first_exact_ms,omitempty"`
	Passed        int                     `json:"passed"`
	Failed        int                     `json:"failed"`
}

func startupAdmissionCommand(ctx context.Context, args []string) error {
	var o startupAdmissionOptions
	f := flag.NewFlagSet("startup-admission", flag.ContinueOnError)
	bindStartupAdmissionFlags(f, &o)
	if err := f.Parse(args); err != nil {
		return err
	}
	if o.Binary == "" || o.Scratch == "" {
		return errors.New("startup-admission requires --binary and --scratch")
	}
	if o.Repeats < 1 {
		return errors.New("--repeats must be >= 1")
	}
	report := startupAdmissionReport{
		SchemaVersion: 1, Binary: o.Binary, Started: time.Now().UTC(), Prepare: o.Prepare,
		AFiles: o.AFiles, BFiles: o.BFiles,
		BudgetMS: float64(o.Budget.Milliseconds()), HoldMinMS: float64(o.HoldMin.Milliseconds()),
	}
	for i := 0; i < o.Repeats; i++ {
		trial := runStartupAdmissionTrial(ctx, o, i)
		report.Trials = append(report.Trials, trial)
		if trial.Outcome == "ready" {
			report.Passed++
		} else {
			report.Failed++
		}
		if trial.LatencyMS != nil && (report.MaxLatencyMS == nil || *trial.LatencyMS > *report.MaxLatencyMS) {
			v := *trial.LatencyMS
			report.MaxLatencyMS = &v
		}
		fmt.Fprintf(os.Stderr, "trial %d: outcome=%s latency=%s hold=%s gate_hold=%s boot_gate_open=%s settled=%s\n",
			i, trial.Outcome, fmtMS(trial.LatencyMS), fmtMS(trial.HoldMS), fmtMS(trial.GateHoldMS), fmtMS(trial.BootGateOpenMS), fmtMS(trial.SettledExactMS))
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(report); err != nil {
		return err
	}
	if report.Failed > 0 {
		return fmt.Errorf("%d/%d trials failed", report.Failed, o.Repeats)
	}
	return nil
}

func fmtMS(v *float64) string {
	if v == nil {
		return "n/a"
	}
	return fmt.Sprintf("%.0fms", *v)
}

func msPtr(d time.Duration) *float64 {
	v := float64(d) / float64(time.Millisecond)
	return &v
}

func runStartupAdmissionTrial(ctx context.Context, o startupAdmissionOptions, index int) startupAdmissionTrial {
	t := startupAdmissionTrial{Trial: index, Prepare: o.Prepare, Outcome: "error"}
	trialCtx, cancel := context.WithTimeout(ctx, o.Timeout)
	defer cancel()
	// A fresh trial root per run: nothing pre-existing under --scratch is ever
	// reused or removed by the fixture.
	if err := os.MkdirAll(o.Scratch, 0o700); err != nil {
		t.Error = err.Error()
		return t
	}
	root, err := os.MkdirTemp(o.Scratch, fmt.Sprintf("trial-%d-", index))
	if err != nil {
		t.Error = err.Error()
		return t
	}
	// Registered first so it runs last, after the daemon has stopped: the whole
	// trial root goes, including the family-B fixture under sources/.
	defer func() {
		if !o.KeepState {
			_ = removeOwnedState(root)
		}
	}()
	sourceRoot := filepath.Join(root, "sources")
	a, err := buildStartupAdmissionFixture(trialCtx, sourceRoot, "alpha", o.AFiles, o.Functions, o.Calls)
	if err != nil {
		t.Error = "fixture A: " + err.Error()
		return t
	}
	b, err := buildStartupAdmissionFixture(trialCtx, sourceRoot, "bravo", o.BFiles, o.Functions, o.Calls)
	if err != nil {
		t.Error = "fixture B: " + err.Error()
		return t
	}
	p, err := newPrivateRun(trialCtx, filepath.Join(root, "run"), o.Binary, []Corpus{a.Corpus, b.Corpus})
	if err != nil {
		t.Error = "private run: " + err.Error()
		return t
	}
	t.Root = p.Root
	defer func() {
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 30*time.Second)
		_, _, _ = p.stop(stopCtx)
		stopCancel()
		_ = os.RemoveAll(p.SocketDir)
		if !o.KeepState {
			_ = p.removeState()
		}
	}()
	aWorktree := filepath.Join(p.Root, "repos", "alpha-worktree")
	bWorktree := filepath.Join(p.Root, "repos", "bravo-worktree")
	if _, err := p.git(trialCtx, p.Repos[1].Path, "worktree", "add", "--detach", bWorktree, b.Corpus.Commit); err != nil {
		t.Error = "worktree B: " + err.Error()
		return t
	}
	// The dedicated-base warmup only publishes a family base when a worktree is
	// present (otherwise it reports skipped_owner_only). In prepared mode a
	// SEPARATE checkout earns A's base, and the measured worktree is created only
	// after the preparation daemon stops, so the measured boot discovers it fresh
	// with no route built for it. In during-startup mode the measured worktree
	// itself is the fresh discovery that publishes the base.
	addMeasured := func() error {
		_, err := p.git(trialCtx, p.Repos[0].Path, "worktree", "add", "--detach", aWorktree, a.Corpus.Commit)
		return err
	}
	if o.Prepare {
		prepWorktree := filepath.Join(p.Root, "repos", "alpha-prep")
		if _, err := p.git(trialCtx, p.Repos[0].Path, "worktree", "add", "--detach", prepWorktree, a.Corpus.Commit); err != nil {
			t.Error = "prep worktree A: " + err.Error()
			return t
		}
	} else if err := addMeasured(); err != nil {
		t.Error = "worktree A: " + err.Error()
		return t
	}
	catalog, err := sql.Open("sqlite", "file:"+p.Store+"?mode=ro")
	if err != nil {
		t.Error = "catalog open: " + err.Error()
		return t
	}
	defer catalog.Close()

	// Preparation boot: publish family A's committed base so it is servable
	// before the measured boot discovers A's worktree. Unmeasured.
	if o.Prepare {
		if err := writeStartupAdmissionConfig(p, p.Repos[:1]); err != nil {
			t.Error = "prep config: " + err.Error()
			return t
		}
		if err := p.start(); err != nil {
			t.Error = "prep start: " + err.Error()
			return t
		}
		if err := waitStartupAdmissionBase(trialCtx, catalog, "alpha", 120*time.Second); err != nil {
			t.Error = "prep base: " + err.Error()
			stopCtx, stopCancel := context.WithTimeout(context.Background(), 30*time.Second)
			_, _, _ = p.stop(stopCtx)
			stopCancel()
			return t
		}
		stopCtx, stopCancel := context.WithTimeout(trialCtx, 30*time.Second)
		_, forced, err := p.stop(stopCtx)
		stopCancel()
		if err != nil || forced {
			t.Error = fmt.Sprintf("prep shutdown: forced=%v: %v", forced, err)
			return t
		}
		if startupAdmissionActiveGen(trialCtx, catalog, "alpha") <= 0 {
			t.Error = "prep base: alpha has no servable base after the preparation boot"
			return t
		}
		if err := addMeasured(); err != nil {
			t.Error = "worktree A: " + err.Error()
			return t
		}
	}

	if err := writeStartupAdmissionConfig(p, p.Repos); err != nil {
		t.Error = "boot config: " + err.Error()
		return t
	}
	t.BootStart = time.Now()
	if err := p.start(); err != nil {
		t.Error = "start: " + err.Error()
		return t
	}
	t.LogPath = filepath.Join(p.Root, fmt.Sprintf("daemon-%d.stderr.log", p.startCount))

	// Own-base readiness (t0) is observed by a dedicated tight catalog poll with
	// NO control calls in its loop, so it is not biased late by slow probe/status
	// calls. In prepared mode A's servable base was validated after the clean
	// preparation shutdown, before boot, so t0 = BootStart.
	var baseMu sync.Mutex
	var aBaseAt, bBaseAt time.Time
	if o.Prepare {
		aBaseAt = t.BootStart
	}
	go func() {
		tick := time.NewTicker(20 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-trialCtx.Done():
				return
			case <-p.done:
				return
			case <-tick.C:
			}
			baseMu.Lock()
			if aBaseAt.IsZero() && startupAdmissionActiveGen(trialCtx, catalog, "alpha") > 0 {
				aBaseAt = time.Now()
			}
			if bBaseAt.IsZero() && startupAdmissionActiveGen(trialCtx, catalog, "bravo") > 0 {
				bBaseAt = time.Now()
			}
			done := !aBaseAt.IsZero() && !bBaseAt.IsZero()
			baseMu.Unlock()
			if done {
				return
			}
		}
	}()
	snapshotBase := func() (time.Time, time.Time) {
		baseMu.Lock()
		defer baseMu.Unlock()
		return aBaseAt, bBaseAt
	}

	var firstExact, gateOpen, settledExact time.Time
	var aPrefix, bPrefix string
	var settledStarted bool
	var settledStart time.Time
	var settledID string
	settledWorktree := filepath.Join(p.Root, "repos", "alpha-settled")
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-trialCtx.Done():
			t.Error = "timeout: " + trialCtx.Err().Error()
			ab, bb := snapshotBase()
			finalizeStartupAdmissionTrial(&t, o, ab, bb, firstExact, gateOpen)
			return t
		case <-p.done:
			t.Error = fmt.Sprintf("daemon exited: %v", p.exitErr)
			ab, bb := snapshotBase()
			finalizeStartupAdmissionTrial(&t, o, ab, bb, firstExact, gateOpen)
			return t
		case <-ticker.C:
		}
		probeCtx, probeCancel := context.WithTimeout(trialCtx, 2*time.Second)
		data, err := control(probeCtx, p.Socket, "probe", nil)
		probeCancel()
		if err == nil {
			var state readiness
			if json.Unmarshal(data, &state) == nil {
				for _, tracked := range state.TrackedRepos {
					if filepath.Clean(tracked.Path) == filepath.Clean(p.Repos[0].Path) && tracked.Prefix != "" {
						aPrefix = tracked.Prefix
					}
					if filepath.Clean(tracked.Path) == filepath.Clean(p.Repos[1].Path) && tracked.Prefix != "" {
						bPrefix = tracked.Prefix
					}
				}
				if gateOpen.IsZero() {
					if open, ok := startupAdmissionGateOpen(trialCtx, p.Socket); ok && open {
						gateOpen = time.Now()
						t.GateOpenAt = gateOpen
					}
				}
			}
		}
		t.APrefix, t.BPrefix = aPrefix, bPrefix
		if t.ACheckoutID == "" {
			t.ACheckoutID = startupAdmissionCheckoutID(trialCtx, catalog, aWorktree)
		}
		if aPrefix != "" && t.ACheckoutID != "" && firstExact.IsZero() {
			exactCtx, exactCancel := context.WithTimeout(trialCtx, 2*time.Second)
			raw, err := control(exactCtx, p.Socket, "search_symbols", map[string]any{"path": aWorktree, "query": a.Marker, "limit": 1})
			exactCancel()
			if err == nil && startupAdmissionExact(raw, aPrefix, t.ACheckoutID, a.Marker) {
				firstExact = time.Now()
				t.FirstExactAt = firstExact
				// Negative witnesses taken after the reply: a positive poll that
				// lands later cannot tell whether its transition preceded the reply.
				open, ok := startupAdmissionGateOpen(trialCtx, p.Socket)
				bGen, bOK := startupAdmissionBaseGen(trialCtx, catalog, "bravo")
				t.HoldAfterExact = ok && !open && bOK && bGen == 0
			}
		}
		// Settled control: once the gate is open, discover a FRESH worktree and
		// time discovery->first-exact. This is the settled-daemon control, not a
		// warm repeat query on an already-served checkout.
		if !gateOpen.IsZero() && !settledStarted {
			settledStarted = true
			if _, err := p.git(trialCtx, p.Repos[0].Path, "worktree", "add", "--detach", settledWorktree, a.Corpus.Commit); err != nil {
				t.Error = "settled worktree: " + err.Error()
			} else {
				settledStart = time.Now()
			}
		}
		if settledID == "" && !settledStart.IsZero() {
			settledID = startupAdmissionCheckoutID(trialCtx, catalog, settledWorktree)
		}
		if settledStarted && settledID != "" && settledExact.IsZero() && aPrefix != "" {
			settledCtx, settledCancel := context.WithTimeout(trialCtx, 2*time.Second)
			raw, err := control(settledCtx, p.Socket, "search_symbols", map[string]any{"path": settledWorktree, "query": a.Marker, "limit": 1})
			settledCancel()
			if err == nil && startupAdmissionExact(raw, aPrefix, settledID, a.Marker) {
				settledExact = time.Now()
				t.SettledExactMS = msPtr(settledExact.Sub(settledStart))
			}
		}
		if !firstExact.IsZero() && !gateOpen.IsZero() && !settledExact.IsZero() && time.Since(gateOpen) > 2*time.Second {
			break
		}
	}
	ab, bb := snapshotBase()
	finalizeStartupAdmissionTrial(&t, o, ab, bb, firstExact, gateOpen)
	return t
}

// finalizeStartupAdmissionTrial derives t0 and the verdict. t0 is the later of
// discovery (the measured boot) and A's own-base readiness. A trial with any
// error, no observed A base, a missing exact response, a negative or
// over-budget latency, an unobserved gate open, a hold not witnessed after the
// exact reply, a too-short unrelated hold, or a missing settled control is a
// failure — never counted ready.
func finalizeStartupAdmissionTrial(t *startupAdmissionTrial, o startupAdmissionOptions, aBaseAt, bBaseAt, firstExact, gateOpen time.Time) {
	t0 := t.BootStart
	if aBaseAt.After(t0) {
		t0 = aBaseAt
	}
	t.T0 = t0
	if !aBaseAt.IsZero() {
		t.ABaseAt = aBaseAt
	}
	if !bBaseAt.IsZero() {
		t.BBaseAt = bBaseAt
	}
	if !firstExact.IsZero() {
		t.LatencyMS = msPtr(firstExact.Sub(t0))
	}
	if !bBaseAt.IsZero() {
		t.HoldMS = msPtr(bBaseAt.Sub(t0))
	}
	if !gateOpen.IsZero() {
		t.GateHoldMS = msPtr(gateOpen.Sub(t0))
		t.BootGateOpenMS = msPtr(gateOpen.Sub(t.BootStart))
	}
	// The unrelated hold is the conservative (earlier) of B's catalog base
	// readiness and the observed gate open, so a slow status call cannot inflate it.
	holdMS := t.GateHoldMS
	if t.HoldMS != nil && (holdMS == nil || *t.HoldMS < *holdMS) {
		holdMS = t.HoldMS
	}
	switch {
	case t.Error != "":
		t.Outcome = "error"
	case aBaseAt.IsZero():
		// t0 would fall back to boot start without A's own base: unproven.
		t.Outcome = "no_own_base"
	case firstExact.IsZero():
		t.Outcome = "no_exact"
	case t.LatencyMS != nil && *t.LatencyMS < 0:
		t.Outcome = "error"
		if t.Error == "" {
			t.Error = "negative t0->first-exact latency (harness clock)"
		}
	case t.LatencyMS != nil && *t.LatencyMS > float64(o.Budget.Milliseconds()):
		t.Outcome = "over_budget"
	case t.GateHoldMS == nil:
		t.Outcome = "no_gate_open"
	case t.HoldMS == nil:
		// No witness of the unrelated family's publication: the hold is unproven.
		t.Outcome = "no_hold_witness"
	case !t.HoldAfterExact || !firstExact.Before(bBaseAt) || !firstExact.Before(gateOpen):
		// The first exact answer must land while the unrelated family still holds
		// startup (before its base publishes and before the gate opens).
		t.Outcome = "not_during_hold"
	case *holdMS < float64(o.HoldMin.Milliseconds()):
		t.Outcome = "hold_too_short"
	case t.SettledExactMS == nil:
		t.Outcome = "no_settled_control"
	default:
		t.Outcome = "ready"
	}
}

// startupAdmissionExact requires the exact worktree rider for the measured
// checkout's own catalog identity AND a hit named the fixture marker, so an
// exact-but-empty, fallback, or other-checkout answer never counts.
func startupAdmissionExact(raw json.RawMessage, prefix, checkoutID, marker string) bool {
	var answer struct {
		View struct {
			Exact      bool   `json:"exact"`
			Kind       string `json:"kind"`
			Prefix     string `json:"repo_prefix"`
			CheckoutID string `json:"checkout_id"`
		} `json:"view"`
		Hits []struct {
			Name string `json:"name"`
		} `json:"hits"`
	}
	if json.Unmarshal(raw, &answer) != nil {
		return false
	}
	if !answer.View.Exact || answer.View.Kind != "worktree" || answer.View.Prefix != prefix || answer.View.CheckoutID != checkoutID {
		return false
	}
	for _, hit := range answer.Hits {
		if hit.Name == marker {
			return true
		}
	}
	return false
}

// startupAdmissionActiveGen is startupAdmissionBaseGen for positive polls,
// where a failed read simply means "not yet".
func startupAdmissionActiveGen(ctx context.Context, db *sql.DB, prefix string) int64 {
	gen, _ := startupAdmissionBaseGen(ctx, db, prefix)
	return gen
}

// startupAdmissionBaseGen reads the family's primary dedicated graph active
// generation read-only, >0 only when graphBase's positive arm would serve it:
// a dedicated generation of that graph and owner, in a servable state, with a
// committed tree. ok is false when the read failed, so a negative witness is
// never inferred from an error. Identical on both binaries.
func startupAdmissionBaseGen(ctx context.Context, db *sql.DB, prefix string) (gen int64, ok bool) {
	queryCtx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	err := db.QueryRowContext(queryCtx, `
SELECT g.generation_id FROM dedicated_graphs d
JOIN view_generations g ON g.generation_id = d.active_generation_id
WHERE d.repo_prefix=? AND d.is_primary_base=1
  AND g.owner_kind = 'dedicated_graph' AND g.graph_id = d.graph_id AND g.checkout_id = d.owner_checkout_id
  AND g.generation_kind = 'dedicated' AND g.state IN ('ready', 'superseded') AND g.tree_oid != ''`,
		prefix).Scan(&gen)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, true
	}
	if err != nil {
		return 0, false
	}
	return gen, true
}

// startupAdmissionGateOpen reads the daemon's build-lane latch; ok is false
// when the status call or its decoding failed.
func startupAdmissionGateOpen(ctx context.Context, socket string) (open, ok bool) {
	statusCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	raw, err := control(statusCtx, socket, "status", nil)
	if err != nil {
		return false, false
	}
	var status struct {
		BuildLane *struct {
			Open bool `json:"open"`
		} `json:"build_lane"`
	}
	if json.Unmarshal(raw, &status) != nil || status.BuildLane == nil {
		return false, false
	}
	return status.BuildLane.Open, true
}

// startupAdmissionCheckoutID reads the catalog checkout ID registered for a
// worktree root, "" until the daemon has discovered it.
func startupAdmissionCheckoutID(ctx context.Context, db *sql.DB, root string) string {
	if canonical, err := filepath.EvalSymlinks(root); err == nil {
		root = canonical
	}
	queryCtx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	var id string
	if err := db.QueryRowContext(queryCtx, `SELECT checkout_id FROM checkouts WHERE root_path=?`, root).Scan(&id); err != nil {
		return ""
	}
	return id
}

func waitStartupAdmissionBase(ctx context.Context, db *sql.DB, prefix string, bound time.Duration) error {
	deadline := time.Now().Add(bound)
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	for {
		if startupAdmissionActiveGen(ctx, db, prefix) > 0 {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("family %s base not adopted within %s", prefix, bound)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

// writeStartupAdmissionConfig writes the daemon's repo list (a subset for the
// preparation boot). Mirrors privateRun.writeBootRepos.
func writeStartupAdmissionConfig(p *privateRun, repos []privateRepo) error {
	entries := make([]map[string]string, 0, len(repos))
	for _, repo := range repos {
		entries = append(entries, map[string]string{"path": repo.Path, "name": repo.Corpus.Label})
	}
	data, err := json.Marshal(map[string]any{"repos": entries})
	if err != nil {
		return err
	}
	dir := filepath.Join(p.Root, "config", "gortex")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "config.yaml"), data, 0o600)
}
