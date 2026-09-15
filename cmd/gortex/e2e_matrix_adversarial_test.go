package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zzet/gortex/internal/graphview"
)

// Matrix 7 of the isolated end-to-end matrix (handoff §8, seventh
// bullet): adversarial fan-out, cycles and pathless identities, bounded
// physical candidate work, chain maintenance and old-route retention, and
// cross-surface coherence.
//
// The scaffolding (rows, gates, outcome table, private-daemon environment and
// every pure verdict) lives in e2e_matrix_lifecycle_test.go; this file is the
// rows. The same three rules hold: opt-in behind GX_E2E_MATRIX_BINARY, private
// daemon and private store, and every assertion names the acceptance gate it
// serves.

// The matrix-7 bullets, from the coordinator's brief. Every one is claimed by
// exactly one row below; e2eLifecycleValidateRows enforces that before anything runs.
var e2eAdversarialMatrix7Bullets = []string{
	"adversarial high fan-out",
	"cycles and pathless identities",
	"bounded physical candidate work: typed limit and completeness fact",
	"chain maintenance, depth bound and old-route retention",
	"source, graph, text, health/stats and caches coherent",
}

// TestE2EMatrix7Adversarial is the opt-in run.
func TestE2EMatrix7Adversarial(t *testing.T) {
	binary := e2eLifecycleRequireBinary(t)
	e2eLifecycleRunMatrix(t, "matrix7_adversarial", e2eAdversarialMatrix7Rows(binary), e2eAdversarialMatrix7Bullets, e2eLifecycleArtifactDir(t))
}

func e2eAdversarialMatrix7Rows(binary string) []e2eLifecycleRow {
	return []e2eLifecycleRow{
		{
			Name:   "high_fanout",
			Bullet: "adversarial high fan-out",
			Gates:  []string{e2eLifecycleGateAdvance, e2eLifecycleGateBounded, e2eLifecycleGateSnapshot},
			Run:    func(t *testing.T, rec *e2eLifecycleRecorder) { e2eAdversarialRowHighFanout(t, rec, binary) },
		},
		{
			Name:   "cycles_and_pathless_identities",
			Bullet: "cycles and pathless identities",
			Gates:  []string{e2eLifecycleGateSnapshot, e2eLifecycleGateBounded},
			Run:    func(t *testing.T, rec *e2eLifecycleRecorder) { e2eAdversarialRowCyclesAndPathless(t, rec, binary) },
		},
		{
			Name:   "bounded_candidate_work",
			Bullet: "bounded physical candidate work: typed limit and completeness fact",
			Gates:  []string{e2eLifecycleGateBounded, e2eLifecycleGateSnapshot},
			Run:    func(t *testing.T, rec *e2eLifecycleRecorder) { e2eAdversarialRowBoundedCandidateWork(t, rec, binary) },
		},
		{
			Name:   "chain_maintenance_and_old_routes",
			Bullet: "chain maintenance, depth bound and old-route retention",
			Gates:  []string{e2eLifecycleGateBounded, e2eLifecycleGateAdvance},
			Run:    func(t *testing.T, rec *e2eLifecycleRecorder) { e2eAdversarialRowChainMaintenance(t, rec, binary) },
		},
		{
			Name:   "cross_surface_coherence",
			Bullet: "source, graph, text, health/stats and caches coherent",
			Gates:  []string{e2eLifecycleGateSnapshot, e2eLifecycleGateEvidence},
			Run:    func(t *testing.T, rec *e2eLifecycleRecorder) { e2eAdversarialRowCrossSurfaceCoherence(t, rec, binary) },
		},
	}
}

// e2eAdversarialFanout is how many dependent checkouts the fan-out row creates. The
// brief's own workload is ten dependents (gate 5); this row is the adversarial
// neighbour of it, so it uses the same order of magnitude and asks a harder
// question of each: not only "did every dependent update" but "did anything
// from main leak into one".
const e2eAdversarialFanout = 10

// e2eAdversarialRowHighFanout advances main under a wide fan of discovered checkouts.
//
// Two facts are asserted, and they pull in opposite directions, which is why
// both are here. Every dependent has to stay correct — it answers exactly for
// its own committed marker, out of its own file, after each advance — and no
// dependent may see main's new marker, because its own tree never moved. The
// second is the isolation half: a fan-out implementation that recomposed
// dependents by splicing main's new base under them would pass the first and
// fail this one.
func e2eAdversarialRowHighFanout(t *testing.T, rec *e2eLifecycleRecorder, binary string) {
	e := e2eLifecycleNewEnv(t, binary, e2eLifecycleSmallSpec(8111), "")
	e.start()

	dependents := make([]string, 0, e2eAdversarialFanout)
	markers := make([]string, 0, e2eAdversarialFanout)
	for i := 1; i <= e2eAdversarialFanout; i++ {
		marker := fmt.Sprintf("GxAdversarial01Dep%02d", i)
		path := e.addWorktree(fmt.Sprintf("wt%02d", i), fmt.Sprintf("w%02d", i), marker)
		dependents, markers = append(dependents, path), append(markers, marker)
	}
	for i, path := range dependents {
		e.awaitExact(path, markers[i], e.markerPath(path), issue767AsAutomaticWorktree)
	}
	e.f.settle()
	before := e.counters()
	rec.note("%d discovered checkouts answer exactly before main moves", len(dependents))

	lastMain := ""
	for commit := 1; commit <= 3; commit++ {
		lastMain = fmt.Sprintf("GxAdversarial01Main%02d", commit)
		e.commitEdit(11+commit, commit, lastMain, fmt.Sprintf("fan-out advance %d", commit))
		e.awaitExact(e.f.primary, lastMain, e.markerPath(e.f.primary), issue767AsPrimary)
		for i, path := range dependents {
			e.awaitExact(path, markers[i], e.markerPath(path), issue767AsAutomaticWorktree)
		}
	}
	// The isolation half, asked of every dependent: main's newest marker is
	// committed on main only, and these trees never moved.
	for _, path := range dependents {
		e.awaitAbsent(path, lastMain, e.markerPath(path), issue767AsAutomaticWorktree, time.Minute)
	}
	rec.note("after %d advances no dependent sees main's committed marker %s", 3, lastMain)

	e.f.settle()
	after := e.counters()
	delta := e2eLifecycleCounterDelta(before, after)
	rec.note("fan-out counters: %s", e2eLifecycleCounterLine(delta,
		"views_dedicated_base_publish_total", "views_dedicated_base_claim_total",
		"views_dependent_recomposition_total", "views_coordinator_cycle_total"))
	if err := e2eLifecycleClosureComplete(after); err != nil {
		t.Error(err)
	}
	if err := e2eLifecycleQuiescedWithEvidence(rec, after); err != nil {
		t.Error(err)
	}
	if err := e2eLifecycleNoStorageFailures(e.status().Views); err != nil {
		t.Error(err)
	}
	// Bounded: a fan of ten dependents over three commits must not leave one
	// live generation per dependent PER COMMIT lying around. The ceiling is
	// the two shapes the design allows to be live at once — at most
	// MaxRepoViewLayers content layers per checkout
	// (internal/graphview/compose.go) plus one committed-base chain no deeper
	// than MaxDedicatedBaseChainDepth (internal/graphview/materialize.go).
	if views := e.status().Views; views != nil {
		live := 0
		for state, count := range views.Generations {
			if state != "retired" {
				live += count
			}
		}
		checkouts := len(dependents) + 1
		ceiling := checkouts*graphview.MaxRepoViewLayers + graphview.MaxDedicatedBaseChainDepth
		rec.note("live generations after the fan-out: %d across %v (leases %d); design ceiling %d", live, views.Generations, views.Leases, ceiling)
		if live > ceiling {
			t.Errorf("%d live generations for %d checkouts exceeds the %d the layer and chain bounds allow: the fan-out accumulates a generation per dependent per advance", live, checkouts, ceiling)
		}
	}
}

// e2eAdversarialCyclicCorpus is a deliberately hostile little repository.
//
//   - Two packages import each other. The generated corpus is acyclic by
//     construction (sustainedIOGenerateFixture only ever imports a HIGHER package
//     index), so a cycle has to be written by hand — and it is the shape the
//     handoff's ancestry hazard warns about, one level down: a resolver walk
//     that does not remember where it has been does not terminate on this.
//   - Both packages import modules that do not exist. An import that resolves
//     to nothing mints a repo-scoped stub whose id carries no path
//     (internal/indexer/builder_closure.go's manifest note), so this is where
//     a pathless identity comes from without inventing one.
//   - One file references a symbol that is never defined anywhere, which parks
//     an unresolved reference in the graph.
func e2eAdversarialCyclicCorpus(f *issue767Fixture) {
	write := func(name, body string) { f.write(filepath.Join(f.primary, filepath.FromSlash(name)), body) }
	write("go.mod", "module example.invalid/issue767\n\ngo 1.24\n")
	write("root.go", sustainedIORootSource())
	write("marker.go", issue767MarkerSource(sustainedIOPrimaryMarker))
	write("cyc/a/a.go", `package a

import (
	"example.invalid/issue767/cyc/b"
	"example.invalid/absent/vendorless"
)

// GxCycleAlpha calls into b, which calls back into a: the import cycle.
func GxCycleAlpha() int { return b.GxCycleBeta() + 1 }

// GxCycleAlphaSelf is the intra-package leg of the cycle.
func GxCycleAlphaSelf() int { return GxCycleAlpha() }

// GxCycleAlphaExternal calls a module that does not exist: a pathless stub.
func GxCycleAlphaExternal() int { return vendorless.Absent() }
`)
	write("cyc/b/b.go", `package b

import (
	"example.invalid/issue767/cyc/a"
	"example.invalid/absent/vendorless"
)

// GxCycleBeta closes the cycle back into a.
func GxCycleBeta() int { return a.GxCycleAlphaSelf() + 1 }

// GxCycleBetaExternal is the second pathless reference.
func GxCycleBetaExternal() int { return vendorless.AlsoAbsent() }

// GxCycleBetaDangling references a symbol that is defined nowhere at all.
func GxCycleBetaDangling() int { return GxNeverDefinedAnywhere() }
`)
	for i := 0; i < 8; i++ {
		write(fmt.Sprintf("cyc/c/c%02d.go", i), fmt.Sprintf(`package c

import "example.invalid/issue767/cyc/a"

func GxCycleGamma%02d() int { return a.GxCycleAlpha() + %d }
`, i, i))
	}
}

// e2eAdversarialRowCyclesAndPathless indexes that repository and asks whether the view
// is still a view.
//
// The assertion is gate 1's, kept to what a public surface can prove: the
// declarations in the cyclic packages answer exactly, out of their own files,
// and the pathless and dangling references neither disappear the file that
// holds them nor drag the build into a truncated closure. A daemon that hung
// on the cycle never reaches the first wait; one that dropped the files fails
// it.
func e2eAdversarialRowCyclesAndPathless(t *testing.T, rec *e2eLifecycleRecorder, binary string) {
	f := newIssue767FixtureWithCorpus(t, binary, e2eAdversarialCyclicCorpus)
	e := &e2eLifecycleEnv{t: t, f: f, spec: sustainedIOFixtureSpec{}.normalize(), marker: sustainedIOPrimaryMarker}
	e.start()

	for _, probe := range []struct{ name, file string }{
		{"GxCycleAlpha", "cyc/a/a.go"},
		{"GxCycleAlphaExternal", "cyc/a/a.go"},
		{"GxCycleBeta", "cyc/b/b.go"},
		{"GxCycleBetaDangling", "cyc/b/b.go"},
		{"GxCycleGamma07", "cyc/c/c07.go"},
	} {
		e.awaitExact(e.f.primary, probe.name, filepath.Join(e.f.primary, filepath.FromSlash(probe.file)), issue767AsPrimary)
	}
	rec.note("every declaration in the cyclic, pathless and dangling files answers from its own file")

	// A discovered checkout over the same hostile tree: the composition is the
	// half that has to walk the cycle again.
	worktree := e.addWorktree("wt01", "w01", "GxAdversarial02Dep")
	e.awaitExact(worktree, "GxAdversarial02Dep", e.markerPath(worktree), issue767AsAutomaticWorktree)
	e.awaitExact(worktree, "GxCycleBeta", filepath.Join(worktree, filepath.FromSlash("cyc/b/b.go")), issue767AsAutomaticWorktree)
	rec.note("the composed checkout view answers for the cyclic packages too")

	// Now move one leg of the cycle and watch the other leg follow. This is the
	// part a fixed-point closure walk has to terminate on.
	e.f.write(filepath.Join(e.f.primary, filepath.FromSlash("cyc/a/a.go")), `package a

import (
	"example.invalid/issue767/cyc/b"
	"example.invalid/absent/vendorless"
)

func GxCycleAlpha() int { return b.GxCycleBeta() + 2 }

func GxCycleAlphaSelf() int { return GxCycleAlpha() }

func GxCycleAlphaExternal() int { return vendorless.Absent() }

// GxCycleAlphaRevised is the moved declaration the wait watches for.
func GxCycleAlphaRevised() int { return GxCycleAlphaSelf() }
`)
	e.f.git(e.f.primary, "add", "-A")
	e.f.git(e.f.primary, "commit", "-m", "move one leg of the cycle")
	e.awaitExact(e.f.primary, "GxCycleAlphaRevised", filepath.Join(e.f.primary, filepath.FromSlash("cyc/a/a.go")), issue767AsPrimary)
	e.awaitExact(e.f.primary, "GxCycleBeta", filepath.Join(e.f.primary, filepath.FromSlash("cyc/b/b.go")), issue767AsPrimary)
	rec.note("after one leg of the cycle moved, both legs still answer from their own files")

	e.f.settle()
	counters := e.counters()
	rec.note("cycle counters: %s", e2eLifecycleCounterLine(counters, "views_dedicated_base_publish_total", "views_dedicated_base_closure_truncated_total"))
	if err := e2eLifecycleClosureComplete(counters); err != nil {
		t.Error(err)
	}
	if err := e2eLifecycleQuiescedWithEvidence(rec, counters); err != nil {
		t.Error(err)
	}
	if err := e2eLifecycleNoStorageFailures(e.status().Views); err != nil {
		t.Error(err)
	}
}

// e2eAdversarialNarrowCapConfig is the repo-local configuration that narrows the typed
// limit to something a small corpus can actually reach. The key is the one the
// source documents: "Configured under `index.affected_by_reresolve_max` in
// .gortex.yaml" (internal/config/config.go, IndexConfig.AffectedByReresolveMax),
// and the sparse-generation builder reuses that same cap for its closure
// (internal/indexer/builder_closure.go, builderClosureCap).
// The value is 1, the smallest the source honours: builderClosureCap takes the
// configured number only when it is positive (`if n := b.Config.Affected\
// ByReresolveMax; n > 0`, internal/indexer/builder_closure.go:89), so 1 is the
// narrowest cap that is a cap at all. A first run of this row used 2 and
// truncated nothing on a 48-file corpus whose every file calls into the edited
// root.go; 1 gives the arm its best chance of reaching the limit, and the row
// records it when even that does not.
const e2eAdversarialNarrowCapConfig = "index:\n  affected_by_reresolve_max: 1\n"

// e2eAdversarialRowBoundedCandidateWork is the typed-limit-and-completeness-fact row.
//
// Two arms, and the second is the point:
//
//   - With the shipped cap, a change to the most-referenced symbol in the
//     corpus must publish a COMPLETE generation:
//     views_dedicated_base_closure_truncated_total "must stay at zero"
//     (internal/viewmetrics/catalog.go).
//   - With the cap narrowed to two files, the same change cannot be complete.
//     What is asserted there is not that it is complete but that it is HONEST:
//     a truncated closure is "published as one, rather than a silent
//     divergence" (internal/indexer/builder_closure.go), so a cut on the lane
//     the counter covers has to reach the census, and the view must still
//     answer exactly for the edited declaration out of its own file — a
//     partial projection must never be served as an exact answer.
//
// The honesty rule is lane-scoped on purpose, and e2eAdversarialTruncationLanes is why:
// THREE code paths log a truncated closure and only ONE of them counts it.
// viewmetrics.DedicatedBaseClosureTruncatedTotal is incremented exactly once,
// beside the dedicated-base publish counters
// (internal/indexer/dedicated_base_runtime.go:469-475). The checkout
// coordinator's commit layer (checkout_coordinator.go:2184-2188) and the ref
// view manager (ref_views.go:876-880) log the same fact and count nothing —
// they are different lanes, and the census does not claim to cover them. A rule
// that demanded the counter move for ANY truncation line in the log would fail
// this row for a lane the branch never promised to count.
func e2eAdversarialRowBoundedCandidateWork(t *testing.T, rec *e2eLifecycleRecorder, binary string) {
	t.Run("shipped_cap_publishes_a_complete_closure", func(t *testing.T) {
		e := e2eLifecycleNewEnv(t, binary, e2eLifecycleSmallSpec(8113), "")
		e.start()
		e.f.settle()
		e2eAdversarialEditTheMostReferencedFile(t, e, "GxAdversarial03Wide")
		e.f.settle()
		counters := e.counters()
		rec.note("shipped cap: %s", e2eLifecycleCounterLine(counters, "views_dedicated_base_publish_total", "views_dedicated_base_closure_truncated_total"))
		if err := e2eLifecycleClosureComplete(counters); err != nil {
			t.Error(err)
		}
		if strings.Contains(e.daemonLog(), "closure truncated") {
			t.Error("the daemon log reports a truncated closure under the shipped cap")
		}
	})

	t.Run("narrow_cap_reports_what_it_cut", func(t *testing.T) {
		e := e2eLifecycleNewEnv(t, binary, e2eLifecycleSmallSpec(8114), e2eAdversarialNarrowCapConfig)
		e.start()
		e.f.settle()
		e2eAdversarialEditTheMostReferencedFile(t, e, "GxAdversarial03Narrow")
		e.f.settle()
		counters := e.counters()
		cut := e2eLifecycleCounterSum(counters, "views_dedicated_base_closure_truncated_total")
		lanes := e2eAdversarialTruncationLanes(e.daemonLog())
		rec.note("narrow cap (%s): truncated counter=%d, lanes that logged a cut: %s",
			strings.TrimSpace(strings.ReplaceAll(e2eAdversarialNarrowCapConfig, "\n", " ")), cut, lanes)
		// The completeness fact has to travel with the generation. A build
		// that cut its closure and told nobody is exactly the silent
		// divergence the source says it refuses to be.
		if err := e2eAdversarialTruncationHonest(lanes, cut); err != nil {
			t.Error(err)
		}
		if !lanes.any() && cut == 0 {
			// A pass here would be a pass for a bullet nothing drove. The
			// typed limit is only observed when something actually hits it,
			// and on this corpus, through repo-local configuration, nothing
			// did — so the arm says so in the outcome table rather than
			// collecting the green.
			rec.note("NOT EXERCISED: with the cap narrowed to its smallest honoured value, no lane truncated a closure on this corpus, so the typed-limit half of the bounded-candidate-work bullet was not driven; only the honesty rule and the no-partial-projection wait above are evidence (ledger row: matrix 7 bounded_candidate_work)")
		}
		// Whatever the closure cost, the answer may not be a partial
		// projection wearing an exact label.
		if err := e2eLifecycleQuiescedWithEvidence(rec, counters); err != nil {
			t.Error(err)
		}
		if err := e2eLifecycleNoStorageFailures(e.status().Views); err != nil {
			t.Error(err)
		}
	})
}

// e2eAdversarialTruncationLanesSeen is which of the three lanes that can cut a closure
// said so in the daemon log. The three messages are verbatim from the source,
// so a rename of one of them shows up here as a lane that stopped reporting
// rather than as a row that quietly stopped checking.
type e2eAdversarialTruncationLanesSeen struct {
	// Builder is the sparse generation builder's own line, emitted by whichever
	// lane asked it to build (internal/indexer/builder_closure.go:202-206).
	Builder bool
	// Publish is the dedicated-base publish lane. It is the ONLY lane that
	// increments views_dedicated_base_closure_truncated_total, and it has no
	// log line of its own — its evidence is the counter
	// (internal/indexer/dedicated_base_runtime.go:469-475).
	//
	// Coordinator and RefView are the two lanes that log and do not count.
	Coordinator bool
	RefView     bool
}

func (l e2eAdversarialTruncationLanesSeen) any() bool { return l.Builder || l.Coordinator || l.RefView }

func (l e2eAdversarialTruncationLanesSeen) String() string {
	var seen []string
	if l.Builder {
		seen = append(seen, "sparse-generation-builder")
	}
	if l.Coordinator {
		seen = append(seen, "checkout-coordinator-commit-layer")
	}
	if l.RefView {
		seen = append(seen, "ref-view-manager")
	}
	if len(seen) == 0 {
		return "(none)"
	}
	return strings.Join(seen, ",")
}

func e2eAdversarialTruncationLanes(log string) e2eAdversarialTruncationLanesSeen {
	return e2eAdversarialTruncationLanesSeen{
		Builder:     strings.Contains(log, "sparse generation closure truncated"),
		Coordinator: strings.Contains(log, "commit layer closure truncated"),
		RefView:     strings.Contains(log, "build closure truncated"),
	}
}

// e2eAdversarialTruncationHonest is the completeness-fact rule, scoped to the lane the
// census covers.
//
// Two directions, and both are failures of the same promise:
//
//   - The census counted a cut the builder never logged. The counter would then
//     be reporting a truncation no build performed, which is worse than not
//     reporting one: it is a fact with no event behind it.
//   - The builder cut a closure, nothing counted it, and neither of the two
//     uncounted lanes was in the log. That leaves the dedicated-base publish
//     lane as the only lane that can have asked for the build, and that lane is
//     required to publish the incompleteness with the generation rather than
//     diverge silently.
//
// A cut on the coordinator or ref-view lane with a zero counter is NOT a
// failure and is returned as nil: those lanes log and do not count, by design.
func e2eAdversarialTruncationHonest(lanes e2eAdversarialTruncationLanesSeen, counted int64) error {
	if counted > 0 && !lanes.Builder {
		return fmt.Errorf("views_dedicated_base_closure_truncated_total is %d but no build logged a truncated closure: the census reports a cut nothing performed", counted)
	}
	if lanes.Builder && counted == 0 && !lanes.Coordinator && !lanes.RefView {
		return fmt.Errorf("a build cut its affected-by closure and only the dedicated-base publish lane can have asked for it, yet views_dedicated_base_closure_truncated_total is 0: the completeness fact did not reach the census")
	}
	return nil
}

// e2eAdversarialEditTheMostReferencedFile changes root.go — the file every generated
// package file calls into (sustainedIOFileSource emits a call to sustainedIORootTarget) — and
// waits for the moved declaration. A change here has the widest affected-by
// closure the corpus can offer, which is what makes it the right probe for a
// cap.
func e2eAdversarialEditTheMostReferencedFile(t *testing.T, e *e2eLifecycleEnv, marker string) {
	t.Helper()
	source := sustainedIORootSource() + fmt.Sprintf("\n// %s is the moved declaration this edit is waited on.\nfunc %s() int { return %s() }\n", marker, marker, sustainedIORootTarget)
	e.f.write(filepath.Join(e.f.primary, "root.go"), source)
	e.f.git(e.f.primary, "add", "-A")
	e.f.git(e.f.primary, "commit", "-m", "widen the closure: "+marker)
	e.awaitExact(e.f.primary, marker, filepath.Join(e.f.primary, "root.go"), issue767AsPrimary)
}

// e2eAdversarialChainCommits is how many committed advances the chain row makes. It has
// to exceed the publisher's delta-chain bound so the policy is observed doing
// the thing it exists to do: proposing a new full root instead of extending a
// chain that already holds MaxDedicatedBaseChainDepth generations
// (internal/indexer/dedicated_base_advance.go, maxDedicatedBaseDeltaAncestors,
// defined as graphview.MaxDedicatedBaseChainDepth).
//
// The margin is generous on purpose. A first run made MaxDedicatedBaseChainDepth+4
// committed advances and the publisher answered with 29 deltas and no root: not
// every advance publishes, so an advance count barely over the bound cannot
// reach it. The row records whether the bound was reached either way — see the
// NOT EXERCISED note below — but it is worth giving it the chance.
var e2eAdversarialChainCommits = graphview.MaxDedicatedBaseChainDepth + 16

// e2eAdversarialRowChainMaintenance drives enough committed advances to exercise the
// chain's own maintenance, and watches an old route while it happens.
//
// Three claims, each read from a public surface:
//
//   - the depth bound is respected: the deltas published between two roots
//     never exceed MaxDedicatedBaseChainDepth, because the read side nests one
//     OverlaidView per ancestor and the write side is required to respect the
//     read side's number (internal/graphview/materialize.go);
//   - superseded work is retired rather than accumulated: something moves out
//     of the live states as the chain advances;
//   - an old route stays available with truthful freshness while it does: a
//     checkout whose own tree never moved keeps answering exactly for its own
//     committed marker through every advance. "Old coherent routes remain
//     available with truthful freshness until replacement routes are ready"
//     (handoff §7, gate 5).
func e2eAdversarialRowChainMaintenance(t *testing.T, rec *e2eLifecycleRecorder, binary string) {
	e := e2eLifecycleNewEnv(t, binary, e2eLifecycleSmallSpec(8115), "")
	e.start()
	holder := e.addWorktree("wt01", "w01", "GxAdversarial04Held")
	held, heldFile := "GxAdversarial04Held", e.markerPath(holder)
	e.awaitExact(holder, held, heldFile, issue767AsAutomaticWorktree)
	e.f.settle()
	before := e.counters()

	for commit := 1; commit <= e2eAdversarialChainCommits; commit++ {
		marker := fmt.Sprintf("GxAdversarial04Chain%02d", commit)
		e.commitEdit(20+commit, commit, marker, fmt.Sprintf("chain advance %d", commit))
		e.awaitExact(e.f.primary, marker, e.markerPath(e.f.primary), issue767AsPrimary)
		// The old route, on every single advance: it must still answer, and it
		// must still answer EXACTLY — a truthful label on a route whose base
		// is being replaced underneath it.
		e.awaitExact(holder, held, heldFile, issue767AsAutomaticWorktree)
	}
	e.f.settle()

	after := e.counters()
	delta := e2eLifecycleCounterDelta(before, after)
	roots := e2eLifecycleCounterWith(delta, "views_dedicated_base_publish_total", "shape=root")
	deltas := e2eLifecycleCounterWith(delta, "views_dedicated_base_publish_total", "shape=delta")
	retired := e2eLifecycleCounterSum(delta, "views_generation_retired_total")
	swept := e2eLifecycleCounterSum(delta, "views_generation_sweep_collected_total")
	superseded := e2eLifecycleCounterSum(delta, "views_generation_superseded_total")
	rec.note("%d committed advances published %d root(s) and %d delta(s); superseded=%d retired=%d swept=%d",
		e2eAdversarialChainCommits, roots, deltas, superseded, retired, swept)
	rec.note("chain counters: %s", e2eLifecycleCounterLine(delta, "views_generation_retire_refused_total", "views_dedicated_base_claim_total"))

	if err := e2eAdversarialChainBounded(roots, deltas, graphview.MaxDedicatedBaseChainDepth); err != nil {
		t.Error(err)
	}
	// The bound is asserted above whether or not it was approached. Whether the
	// allocation POLICY was observed — "propose a new full root instead of
	// extending a chain that already holds MaxDedicatedBaseChainDepth
	// generations" (internal/indexer/dedicated_base_advance.go:201-207) — is a
	// different question, and it is only observed when the window actually
	// published a root after filling a chain.
	if roots == 0 && deltas < int64(graphview.MaxDedicatedBaseChainDepth) {
		rec.note("NOT EXERCISED: %d committed advances published %d deltas and no root, short of the %d-generation depth at which a new root is proposed; the depth bound held but the new-root-at-depth policy was not observed (ledger row: matrix 7 chain_maintenance_and_old_routes)",
			e2eAdversarialChainCommits, deltas, graphview.MaxDedicatedBaseChainDepth)
	} else if roots > 0 {
		rec.note("the window published %d root(s) alongside %d delta(s): the allocation policy proposed a new full root rather than extending the chain past its depth", roots, deltas)
	}
	// Retirement is RECORDED, not asserted. The execution plan is explicit
	// that real compaction and reseed are out of this branch's scope
	// and that gate 8 is claimed only as "measured chain growth under the
	// generation-retention policy" — so a window in which nothing retired is the documented
	// behaviour, and a row that failed on it would be failing the branch for a
	// promise it never made. What IS asserted is the bound below.
	if superseded+retired+swept == 0 {
		rec.note("RECORDED: %d committed advances retired nothing in this window (retention, not compaction; real compaction/reseed is out of scope)", e2eAdversarialChainCommits)
	}
	// The bound that does hold: live payload is the checkouts' content layers
	// plus the chains the window can legally hold. A run that allocated a
	// generation per advance per checkout and kept them all would be several
	// times this.
	if views := e.status().Views; views != nil {
		live := 0
		for state, count := range views.Generations {
			if state != "retired" {
				live += count
			}
		}
		checkouts := 2 // the primary and the one holder this row creates
		ceiling := checkouts*graphview.MaxRepoViewLayers + int((roots+1)*int64(graphview.MaxDedicatedBaseChainDepth))
		rec.note("live generations after %d advances: %d (ceiling %d)", e2eAdversarialChainCommits, live, ceiling)
		if live > ceiling {
			t.Errorf("%d live generations after %d advances exceeds the %d the layer and chain bounds allow: payload is accumulating per advance rather than per chain", live, e2eAdversarialChainCommits, ceiling)
		}
	}
	if err := e2eLifecycleClosureComplete(after); err != nil {
		t.Error(err)
	}
	if err := e2eLifecycleQuiescedWithEvidence(rec, after); err != nil {
		t.Error(err)
	}
	if err := e2eLifecycleNoStorageFailures(e.status().Views); err != nil {
		t.Error(err)
	}
	if views := e.status().Views; views != nil {
		rec.note("generations after %d advances: %v (leases %d)", e2eAdversarialChainCommits, views.Generations, views.Leases)
	}
}

// e2eAdversarialChainBounded is the depth rule as arithmetic over what the census can
// see.
//
// The census counts publications by SHAPE, not by chain: it says how many
// roots and how many deltas were published in a window, never which chain each
// delta joined. The readable invariant is therefore the ratio. A window that
// published r roots can hold at most r+1 chains — the r it rooted itself, plus
// the one chain that already existed when the window opened — and no chain may
// hold more than depth generations, so more than (r+1)*depth deltas means some
// chain grew past the bound the read side is willing to compose.
//
// The +1 is the measurement's honesty, not slack: the cold index publishes its
// root before any of these rows opens its counter window, so a rule without it
// would fail every run for the chain it inherited.
func e2eAdversarialChainBounded(roots, deltas int64, depth int) error {
	if deltas == 0 {
		return nil
	}
	if max := (roots + 1) * int64(depth); deltas > max {
		return fmt.Errorf("%d delta generations over %d root(s) in this window exceeds the %d-generation chain bound (at most %d, counting the chain that already existed)", deltas, roots, depth, max)
	}
	return nil
}

// e2eAdversarialRowCrossSurfaceCoherence asks one settled state through every public
// surface at once, then moves it and asks again.
//
// "Caches and sidecars must be keyed by selected snapshot/capability identity.
// A selected graph plus global source/search/cache data can produce a mixed
// view even if the graph itself is correct" (handoff §6). That is the failure
// this row is shaped to catch: each surface can be individually right while the
// set of them is incoherent, so the rule is applied to the SET — same graph,
// same checkout, exact everywhere — rather than to any one answer.
func e2eAdversarialRowCrossSurfaceCoherence(t *testing.T, rec *e2eLifecycleRecorder, binary string) {
	e := e2eLifecycleNewEnv(t, binary, e2eLifecycleSmallSpec(8116), "")
	e.start()
	worktree := e.addWorktree("wt01", "w01", "GxAdversarial05First")
	first, file := "GxAdversarial05First", e.markerPath(worktree)
	e.awaitExact(worktree, first, file, issue767AsAutomaticWorktree)
	e.f.settle()

	answers := e2eAdversarialAskEverySurface(t, rec, e, worktree, first)
	if err := e2eLifecycleCoherent(answers); err != nil {
		t.Fatalf("the surfaces disagreed about the settled state: %v", err)
	}
	rec.note("settled state: %d surfaces answered from graph %s / checkout %s", len(answers), answers[0].GraphID, answers[0].CheckoutID)

	// The health census is the surface with no freshness block of its own, so
	// it is checked against the family listing instead: the route the catalog
	// reports for this checkout has to be the one that just answered.
	state, mode, found := e.families().checkoutAt(worktree)
	if !found {
		t.Fatalf("the catalog census does not carry the checkout that just answered")
	}
	rec.note("catalog census agrees: state=%q mode=%q", state, mode)

	// Now move the working tree and re-ask. Every surface has to move
	// together: the graph, the text index and the file bytes are three indexes
	// over one snapshot, and one of them lagging is the mixed view.
	second := "GxAdversarial05Second"
	e.f.write(file, issue767MarkerSource(e.marker, second))
	e.awaitExact(worktree, second, file, issue767AsAutomaticWorktree)

	moved := e2eAdversarialAskEverySurface(t, rec, e, worktree, second)
	if err := e2eLifecycleCoherent(moved); err != nil {
		t.Fatalf("the surfaces disagreed after the edit: %v", err)
	}
	// The bytes surface is the one that can silently serve the old snapshot,
	// so it is checked against the file rather than only against its label.
	if body := e2eAdversarialReadFile(t, e, worktree, "marker.go"); !strings.Contains(body, second) {
		t.Fatalf("the source surface served bytes without the edited declaration:\n%s", body)
	}
	if !e2eAdversarialTextSearchFinds(t, e, worktree, second) {
		t.Fatal("the text surface does not carry the edited declaration the graph surface answered for")
	}
	rec.note("after the edit all surfaces carry %s and still name one graph/checkout pair", second)

	e.f.settle()
	if err := e2eLifecycleQuiescedWithEvidence(rec, e.counters()); err != nil {
		t.Error(err)
	}
	if err := e2eLifecycleNoStorageFailures(e.status().Views); err != nil {
		t.Error(err)
	}
}

// e2eAdversarialAskEverySurface asks the graph, the text index and the file bytes for
// one state and returns each answer's view label.
func e2eAdversarialAskEverySurface(t *testing.T, rec *e2eLifecycleRecorder, e *e2eLifecycleEnv, root, name string) []e2eLifecycleFreshness {
	t.Helper()
	requests := []struct {
		surface string
		tool    string
		request map[string]any
	}{
		{"graph", "search", map[string]any{"operation": "symbols", "query": name, "options": map[string]any{"limit": 5, "query_class": "symbol", "expand": "off"}}},
		{"text", "search", map[string]any{"operation": "text", "query": name, "options": map[string]any{"limit": 5}}},
		{"source", "read", map[string]any{"operation": "file", "target": map[string]any{"file": "marker.go"}}},
	}
	answers := make([]e2eLifecycleFreshness, 0, len(requests))
	for _, request := range requests {
		answer, err := e.freshnessOf(request.surface, root, request.tool, request.request)
		if err != nil {
			t.Fatalf("%s surface: %v", request.surface, err)
		}
		rec.note("surface %s answered exact=%v from %s", answer.Surface, answer.Exact, answer.Actual)
		answers = append(answers, answer)
	}
	return answers
}

func e2eAdversarialReadFile(t *testing.T, e *e2eLifecycleEnv, root, file string) string {
	t.Helper()
	payload, err := json.Marshal(map[string]any{"operation": "file", "target": map[string]any{"file": file}})
	if err != nil {
		t.Fatal(err)
	}
	output, err := e.f.tryCommand(60*time.Second, root, "call", "read", "--index", root, "--json", string(payload), "--format", "json")
	if err != nil {
		t.Fatalf("read %s: %v\n%s", file, err, sustainedIOTail(output))
	}
	return string(output)
}

func e2eAdversarialTextSearchFinds(t *testing.T, e *e2eLifecycleEnv, root, needle string) bool {
	t.Helper()
	payload, err := json.Marshal(map[string]any{"operation": "text", "query": needle, "options": map[string]any{"limit": 5}})
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		output, err := e.f.tryCommand(60*time.Second, root, "call", "search", "--index", root, "--json", string(payload), "--format", "json")
		if err == nil && strings.Contains(string(output), needle) {
			return true
		}
		time.Sleep(2 * time.Second)
	}
	return false
}

// ---------------------------------------------------------------------------
// The non-opt-in tests for this file's own rules.
// ---------------------------------------------------------------------------

func TestE2EMatrix7RowsCoverEveryBriefBulletAndNameTheirGates(t *testing.T) {
	rows := e2eAdversarialMatrix7Rows("/nonexistent/gortex")
	if err := e2eLifecycleValidateRows("matrix7_adversarial", rows, e2eAdversarialMatrix7Bullets); err != nil {
		t.Fatal(err)
	}
	if len(rows) != len(e2eAdversarialMatrix7Bullets) {
		t.Fatalf("%d rows for %d brief bullets", len(rows), len(e2eAdversarialMatrix7Bullets))
	}
	// This matrix's bullets are filed under gates 8, 1 and 10; a row
	// set that named none of them could not be read as evidence for any.
	for _, gate := range []string{e2eLifecycleGateBounded, e2eLifecycleGateSnapshot, e2eLifecycleGateEvidence} {
		named := false
		for _, row := range rows {
			for _, declared := range row.Gates {
				if declared == gate {
					named = true
				}
			}
		}
		if !named {
			t.Errorf("no matrix-7 row serves %s", gate)
		}
	}
}

func TestE2EMatrixAdversarialChainBoundIsTheReadSidesOwnNumber(t *testing.T) {
	// The bound the row asserts must be the constant the publisher is defined
	// in terms of, not a number this test picked: the allocation policy IS the
	// read side's depth (internal/indexer/dedicated_base_advance.go defines
	// maxDedicatedBaseDeltaAncestors as graphview.MaxDedicatedBaseChainDepth).
	if graphview.MaxDedicatedBaseChainDepth <= 0 {
		t.Fatal("the chain depth bound is not a positive number")
	}
	if graphview.MaxGenerationAncestryDepth != graphview.MaxDedicatedBaseChainDepth+1 {
		t.Fatalf("the ancestry walk bound is %d, want the chain bound plus the one checkout layer that stands on a dedicated head", graphview.MaxGenerationAncestryDepth)
	}
	if e2eAdversarialChainCommits <= graphview.MaxDedicatedBaseChainDepth {
		t.Fatalf("the chain row makes %d advances, which cannot reach the %d-generation bound it exists to exercise", e2eAdversarialChainCommits, graphview.MaxDedicatedBaseChainDepth)
	}
}

func TestE2EMatrixAdversarialChainBoundedArithmetic(t *testing.T) {
	if err := e2eAdversarialChainBounded(0, 0, 32); err != nil {
		t.Fatalf("a window that published nothing was refused: %v", err)
	}
	// One root in the window plus the chain that was already there: two chains,
	// so 64 deltas is the ceiling and 65 is over it.
	if err := e2eAdversarialChainBounded(1, 64, 32); err != nil {
		t.Fatalf("a window that filled both its chains was refused: %v", err)
	}
	if err := e2eAdversarialChainBounded(1, 65, 32); err == nil {
		t.Fatal("a chain one generation past the bound was accepted")
	}
	// A window that rooted nothing can only have extended the inherited chain,
	// which still cannot pass the bound.
	if err := e2eAdversarialChainBounded(0, 32, 32); err != nil {
		t.Fatalf("a window that extended only the inherited chain was refused: %v", err)
	}
	if err := e2eAdversarialChainBounded(0, 33, 32); err == nil {
		t.Fatal("an inherited chain past the bound was accepted")
	}
}

func TestE2EMatrixAdversarialTruncationHonestyIsScopedToTheLaneTheCensusCovers(t *testing.T) {
	// The three lines, verbatim from the three call sites. They are repeated
	// here rather than shared with the probe so a rename in the source that the
	// probe silently stopped matching fails this test too.
	const (
		builderLine     = `indexer: sparse generation closure truncated	{"repo": "issue767", "closure": 2, "cap": 2}`
		coordinatorLine = `checkout coordinator: commit layer closure truncated	{"checkout": "c1", "generation": 7, "cap": 2}`
		refViewLine     = `ref view manager: build closure truncated	{"ref": "refs/heads/main", "cap": 2}`
	)
	t.Run("lanes are told apart", func(t *testing.T) {
		if got := e2eAdversarialTruncationLanes("nothing interesting happened"); got.any() {
			t.Fatalf("a clean log reported lanes %s", got)
		}
		got := e2eAdversarialTruncationLanes(builderLine)
		if !got.Builder || got.Coordinator || got.RefView {
			t.Fatalf("the builder line was read as %s", got)
		}
		// The coordinator always logs AFTER the builder it called, so the real
		// log carries both lines; the probe must still name both lanes.
		got = e2eAdversarialTruncationLanes(builderLine + "\n" + coordinatorLine)
		if !got.Builder || !got.Coordinator || got.RefView {
			t.Fatalf("the coordinator lane was read as %s", got)
		}
		got = e2eAdversarialTruncationLanes(builderLine + "\n" + refViewLine)
		if !got.Builder || !got.RefView {
			t.Fatalf("the ref-view lane was read as %s", got)
		}
	})
	t.Run("the publish lane must reach the census", func(t *testing.T) {
		lanes := e2eAdversarialTruncationLanes(builderLine)
		if err := e2eAdversarialTruncationHonest(lanes, 1); err != nil {
			t.Fatalf("a counted cut on the publish lane was refused: %v", err)
		}
		if err := e2eAdversarialTruncationHonest(lanes, 0); err == nil {
			t.Fatal("a cut that only the dedicated-base publish lane can have asked for went uncounted and was accepted")
		}
	})
	t.Run("the uncounted lanes are not failures", func(t *testing.T) {
		// This is the false blocker the lane scoping exists to prevent: the
		// checkout coordinator's commit layer and the ref view manager log a
		// truncation and increment nothing, so a zero counter beside their
		// lines is the documented behaviour, not a missing fact.
		if err := e2eAdversarialTruncationHonest(e2eAdversarialTruncationLanes(builderLine+"\n"+coordinatorLine), 0); err != nil {
			t.Fatalf("a cut on the checkout-coordinator lane was read as a missing census fact: %v", err)
		}
		if err := e2eAdversarialTruncationHonest(e2eAdversarialTruncationLanes(builderLine+"\n"+refViewLine), 0); err != nil {
			t.Fatalf("a cut on the ref-view lane was read as a missing census fact: %v", err)
		}
	})
	t.Run("the census may not invent a cut", func(t *testing.T) {
		if err := e2eAdversarialTruncationHonest(e2eAdversarialTruncationLanes("clean"), 2); err == nil {
			t.Fatal("a counter that moved with no build behind it was accepted")
		}
	})
	t.Run("a clean run is clean", func(t *testing.T) {
		if err := e2eAdversarialTruncationHonest(e2eAdversarialTruncationLanes("clean"), 0); err != nil {
			t.Fatalf("a run that truncated nothing was refused: %v", err)
		}
	})
}

func TestE2EMatrixAdversarialCyclicCorpusReallyContainsTheHostileShapes(t *testing.T) {
	// The corpus is the row's whole premise: a "cycle" fixture that is
	// accidentally acyclic, or a "pathless" fixture whose imports all resolve,
	// would leave the row passing while testing nothing.
	root := t.TempDir()
	f := &issue767Fixture{t: t, root: root, primary: filepath.Join(root, "repo")}
	e2eAdversarialCyclicCorpus(f)
	read := func(name string) string {
		body, err := os.ReadFile(filepath.Join(f.primary, filepath.FromSlash(name)))
		if err != nil {
			t.Fatal(err)
		}
		return string(body)
	}
	a, b := read("cyc/a/a.go"), read("cyc/b/b.go")
	if !strings.Contains(a, "example.invalid/issue767/cyc/b") || !strings.Contains(b, "example.invalid/issue767/cyc/a") {
		t.Fatal("the two packages do not import each other: there is no cycle")
	}
	if !strings.Contains(a, "example.invalid/absent/vendorless") || !strings.Contains(b, "example.invalid/absent/vendorless") {
		t.Fatal("no import of a module that does not exist: there is no pathless identity")
	}
	if !strings.Contains(b, "GxNeverDefinedAnywhere") {
		t.Fatal("no reference to an undefined symbol: there is no dangling reference")
	}
	// The generated corpus cannot supply the cycle, which is why this one is
	// written by hand; pin that, so a later "just use sustainedIOGenerateFixture" edit
	// fails here instead of silently removing the hostility.
	for _, file := range sustainedIOGenerateFixture(sustainedIOFixtureSpec{Files: 12, Packages: 3, Seed: 1}) {
		if strings.Contains(file.Content, "p000\"") && strings.Contains(file.Path, "p002/") {
			t.Fatal("the generated corpus now imports a lower package index; it is no longer acyclic by construction")
		}
	}
}
