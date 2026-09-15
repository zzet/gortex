package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

// End-to-end matrix 5: primary dirty edits versus committed main
// advancement, with ten discovered dependent worktrees.
//
// The shared opt-in gate, outcome table, rider readers and store census live in
// e2e_matrix_views_test.go; this file is the second workload of the same item.
//
// What this matrix exists to prove (handoff §7 gate 5):
//
//   - every logical view stays equal to its OWN tree while main advances —
//     a composed pair is never newBase + oldDelta;
//   - ZERO dependent rebuilds while a dependent's own tree is unchanged, in the
//     committed regime 9de4f484 introduced: the dependent stays pinned
//     to the base its routed layers were built against;
//   - recomposition is bounded and only owed once a pinned base is asked back;
//   - old routes keep serving with truthful freshness until replacements are
//     ready — never an exact label over the wrong tree.
//
// The evidence is physical, read read-only from the private store rather than
// inferred from an answer:
//
//	checkout_routes(commit_generation_id, dirty_generation_id)   — the routed pair
//	view_generations(tree_oid, base_generation_id, kind)          — what each layer is
//
// generation_id is an AUTOINCREMENT column (store_sqlite/schema.go:671-672), so
// an unchanged routed pair is a route nothing rebuilt, and a commit layer whose
// base_generation_id did not move while new committed bases were published is
// the pin itself. The identities are the coordinator's:
// commitIdentity stamps TreeOID = the checkout's head tree and BaseGenerationID
// = the base it composes over (internal/indexer/checkout_coordinator.go:3078-3092);
// dirtyIdentity stamps BaseGenerationID = that commit layer (:3098-3112). So
// "never newBase + oldDelta" is checkable exactly: dirty.base == route.commit,
// and commit.tree == the checkout's own head tree.

const (
	// e2eAdvanceDependents is the plan's ten dependent worktrees.
	e2eAdvanceDependents = 10
	// e2eAdvanceCommits is the plan's twenty commits on main.
	e2eAdvanceCommits = 20
	// e2eAdvanceBurstFrom/e2eAdvanceBurstTo bound the rapid-HEAD window: commits landed
	// back to back with no wait between them, so a build that completes is
	// completing against a HEAD that has already moved.
	e2eAdvanceBurstFrom = 11
	e2eAdvanceBurstTo   = 15
)

// e2eAdvanceDependent is one discovered dependent worktree and the marker only its
// own tree carries.
type e2eAdvanceDependent struct {
	path   string
	marker string
	file   string
}

// TestE2EMatrix5MainAdvanceWithTenDependents is E2E matrix 5.
func TestE2EMatrix5MainAdvanceWithTenDependents(t *testing.T) {
	binary := e2eViewsBinary(t)
	e2eViewsBudget(t, 35*time.Minute)
	table := e2eViewsNewTable(t, "matrix5")

	f := newIssue767Fixture(t, binary)
	t.Cleanup(func() { table.writeTo(f.root) })
	f.start()
	f.awaitSymbol(f.primary, "Issue767PrimaryMarker")
	t.Logf("matrix5 candidate %s, artifacts %s", binary, f.root)

	ctx := t.Context()
	db := f.openReadOnly()
	primaryMarker := filepath.Join(f.primary, "marker.go")

	// --- case 5.1 (gate 5): ten dependents, discovered, each with its own tree.
	//
	// Each dependent commits a marker only it carries, so "equal to its own
	// tree" is answerable by a query rather than only by a catalog row. None of
	// them is tracked: no track call, no configuration edit.
	dependents := make([]e2eAdvanceDependent, 0, e2eAdvanceDependents)
	started := time.Now()
	for i := 1; i <= e2eAdvanceDependents; i++ {
		path := filepath.Join(f.root, fmt.Sprintf("dep%02d", i))
		f.git(f.primary, "worktree", "add", "-b", fmt.Sprintf("m5-dep%02d", i), path)
		dependent := e2eAdvanceDependent{
			path:   path,
			marker: fmt.Sprintf("GxMainAdvanceDep%02dOwn", i),
			file:   filepath.Join(path, "marker.go"),
		}
		f.write(dependent.file, issue767MarkerSource(dependent.marker))
		f.git(path, "add", "marker.go")
		f.git(path, "commit", "-m", fmt.Sprintf("dependent %02d own tree", i))
		dependents = append(dependents, dependent)
	}
	for _, dependent := range dependents {
		f.awaitSymbolAs(dependent.path, dependent.marker, dependent.file, 5*time.Minute, issue767AsAutomaticWorktree)
	}
	f.settle()
	table.pass("5.1", "gate 5", "%d dependent worktrees discovered and each serving its own committed tree exactly in %s",
		len(dependents), time.Since(started).Truncate(time.Second))

	// --- case 5.2 (gate 1): the primary carries a dirty edit across everything.
	//
	// file31 is never staged by the advancement loop below (which names the
	// paths it commits), so this declaration stays uncommitted for the whole
	// matrix: the "primary dirty versus committed main advancement" half.
	dirtyFile := filepath.Join(f.primary, "file31.go")
	f.write(dirtyFile, "package fixture\n"+
		"func Issue767Target31() int { return 31 }\n"+
		"func Issue767Caller31() int { return Issue767Target00() }\n"+
		"func GxMainAdvancePrimaryDirty() int { return Issue767Target00() }\n")
	f.awaitSymbolIn(f.primary, "GxMainAdvancePrimaryDirty", dirtyFile, 3*time.Minute)
	f.settle()
	table.pass("5.2", "gate 1", "an uncommitted declaration in the primary's working tree answers exactly before main advances")

	// The frozen "before" census. Everything the matrix concludes about reuse
	// is a difference against these rows.
	beforeRoutes := e2eViewsRoutes(t, ctx, db)
	beforeGenerations := e2eViewsGenerations(t, ctx, db)
	beforeBases := e2eAdvanceCommittedBases(beforeGenerations)
	beforeCounters, counterErr := e2eAdvanceCounters(t, f)
	if counterErr != nil {
		table.unsupported("5.0", "gate 10", "viewmetrics counters unavailable on this arm: %v", counterErr)
	}
	for _, dependent := range dependents {
		if _, ok := beforeRoutes[filepath.Clean(dependent.path)]; !ok {
			t.Fatalf("case 5.1: dependent %s has no checkouts row before the advance", dependent.path)
		}
	}

	// --- case 5.3 (gates 1+5): twenty commits on main.
	//
	// Commits 11..15 land back to back with no wait between them (the rapid
	// HEAD window). Everything else is paced, so the steady-state claim and the
	// contention claim are separable.
	var lastAdvance string
	burstMarkers := make([]string, 0, e2eAdvanceBurstTo-e2eAdvanceBurstFrom+1)
	truthful := 0
	for commit := 1; commit <= e2eAdvanceCommits; commit++ {
		marker := fmt.Sprintf("GxMainAdvanceCommit%02d", commit)
		f.write(primaryMarker, issue767MarkerSource("Issue767PrimaryMarker", marker))
		staged := []string{"add", "marker.go"}
		for offset := 0; offset < 2; offset++ {
			index := (commit*2 + offset) % 10
			name := fmt.Sprintf("file%02d.go", index)
			f.write(filepath.Join(f.primary, name), fmt.Sprintf(
				"package fixture\nfunc Issue767Target%02d() int { return %d }\nfunc Issue767Caller%02d() int { return Issue767Target00() }\nfunc GxMainAdvanceRev%02dC%02d() int { return Issue767Target00() }\n",
				index, index, index, index, commit))
			staged = append(staged, name)
		}
		f.git(f.primary, staged...)
		f.git(f.primary, "commit", "-m", fmt.Sprintf("advance %d", commit))
		lastAdvance = marker

		inBurst := commit >= e2eAdvanceBurstFrom && commit <= e2eAdvanceBurstTo
		if inBurst {
			burstMarkers = append(burstMarkers, marker)
			// No wait: the next commit lands while this one is still being
			// absorbed. What IS checked, every time, is that nobody answers
			// untruthfully in the meantime.
			truthful += e2eAdvanceTruthfulSample(t, f, "5.6", dependents[0])
			continue
		}
		f.awaitSymbolIn(f.primary, marker, primaryMarker, 5*time.Minute)
		// The primary's uncommitted declaration must survive its own commits.
		e2eViewsPresent(t, f, "5.2", f.primary, "GxMainAdvancePrimaryDirty", dirtyFile, issue767AsPrimary)
		// Dependents: two every commit, all ten every fifth.
		sampled := []e2eAdvanceDependent{dependents[0], dependents[(commit-1)%len(dependents)]}
		if commit%5 == 0 {
			sampled = dependents
		}
		for _, dependent := range sampled {
			e2eViewsPresent(t, f, "5.3", dependent.path, dependent.marker, dependent.file, issue767AsAutomaticWorktree)
			// The main-only marker must never be attributed to the
			// dependent's OWN file. Whether main's newer base content is
			// visible anywhere in the dependent's composed stack is a
			// different question — the pin's currency, not its correctness —
			// and it is measured after the loop rather than asserted here.
			e2eViewsAbsentFromFile(t, f, "5.3", dependent.path, marker, dependent.file, issue767AsAutomaticWorktree)
		}
	}
	f.awaitSymbolIn(f.primary, lastAdvance, primaryMarker, 10*time.Minute)
	f.settle()
	table.pass("5.3", "gate 1+5", "%d commits on main; the primary answered each new marker exactly and no dependent ever answered a main-only marker", e2eAdvanceCommits)

	// --- case 5.4 (gate 5): stale build completion never installs an old tree.
	//
	// Every burst commit except the last is a build whose HEAD had already
	// moved by the time it could finish. None of their markers may survive in
	// the primary's marker file, and the last one must.
	for _, marker := range burstMarkers[:len(burstMarkers)-1] {
		e2eViewsAbsentFromFile(t, f, "5.4", f.primary, marker, primaryMarker, issue767AsPrimary)
	}
	e2eViewsPresent(t, f, "5.4", f.primary, lastAdvance, primaryMarker, issue767AsPrimary)
	table.pass("5.4", "gate 5", "%d back-to-back commits (%s..%s): only the newest tree survives; no superseded build installed an older marker",
		len(burstMarkers), burstMarkers[0], burstMarkers[len(burstMarkers)-1])

	// --- case 5.5 (gates 1+5): every logical view still equals its own tree.
	afterRoutes := e2eViewsRoutes(t, ctx, db)
	afterGenerations := e2eViewsGenerations(t, ctx, db)
	afterBases := e2eAdvanceCommittedBases(afterGenerations)
	publishedBases := len(afterBases) - len(beforeBases)
	absorbed := 0

	for _, dependent := range dependents {
		key := filepath.Clean(dependent.path)
		route, ok := afterRoutes[key]
		if !ok {
			t.Fatalf("case 5.5: dependent %s lost its checkouts row", dependent.path)
		}
		commitLayer, ok := afterGenerations[route.CommitGen]
		if !ok {
			t.Fatalf("case 5.5: dependent %s routes to commit generation %d, which is not in view_generations", dependent.path, route.CommitGen)
		}
		if commitLayer.Kind != "commit" {
			t.Fatalf("case 5.5: dependent %s routes to a %q generation as its commit layer", dependent.path, commitLayer.Kind)
		}
		if commitLayer.TreeOID != route.HeadTree {
			t.Fatalf("case 5.5: dependent %s serves a commit layer for tree %s while its own head tree is %s",
				dependent.path, commitLayer.TreeOID, route.HeadTree)
		}
		dirtyLayer, ok := afterGenerations[route.DirtyGen]
		if !ok {
			t.Fatalf("case 5.5: dependent %s routes to dirty generation %d, which is not in view_generations", dependent.path, route.DirtyGen)
		}
		if dirtyLayer.BaseID != route.CommitGen {
			t.Fatalf("case 5.5: dependent %s composes delta %d over base %d while its routed commit layer is %d — a new base spliced into an old delta",
				dependent.path, dirtyLayer.ID, dirtyLayer.BaseID, route.CommitGen)
		}
		e2eViewsPresent(t, f, "5.5", dependent.path, dependent.marker, dependent.file, issue767AsAutomaticWorktree)
		e2eViewsAbsentFromFile(t, f, "5.5", dependent.path, lastAdvance, dependent.file, issue767AsAutomaticWorktree)
		if e2eAdvanceVisible(t, f, dependent, lastAdvance) {
			absorbed++
		}
	}
	table.pass("5.5", "gate 1+5", "all %d dependents: commit layer tree == the checkout's own head tree, delta composed over that same commit layer, own marker exact, the main-only marker never attributed to the dependent's own file",
		len(dependents))
	table.observed("5.5b", "gate 5", "%d/%d dependents' composed stacks can see main's newest marker %s anywhere at all (currency of the pinned base, recorded not budgeted)",
		absorbed, len(dependents), lastAdvance)

	// --- case 5.6 (gate 5): truthful freshness under contention.
	table.pass("5.6", "gate 5", "%d rider samples taken across the rapid-HEAD window; every answer was either exact for the dependent's own file or carried a stated non-exact reason", truthful)

	// --- case 5.7 (gate 5): ZERO dependent rebuilds while their trees did not move.
	//
	// The dependent pin (9de4f484): a dependent whose own tree did not move stays
	// on the committed base its layers were built against, so its routed pair
	// is untouched and it mints no new generation. The regime matters and is
	// measured, not assumed: where the family publishes no committed base there
	// is no generation to stay on, pinRoutedBase refuses, and the saving is not
	// exercisable (checkout_coordinator.go:1269-1310).
	var moved, unchanged []string
	for _, dependent := range dependents {
		key := filepath.Clean(dependent.path)
		if beforeRoutes[key].pair() == afterRoutes[key].pair() {
			unchanged = append(unchanged, filepath.Base(dependent.path))
			continue
		}
		moved = append(moved, fmt.Sprintf("%s %s->%s", filepath.Base(dependent.path), beforeRoutes[key].pair(), afterRoutes[key].pair()))
	}
	owners := map[string]bool{}
	for _, dependent := range dependents {
		if id := afterRoutes[filepath.Clean(dependent.path)].CheckoutID; id != "" {
			owners[id] = true
		}
	}
	minted := e2eAdvanceMintedFor(owners, beforeGenerations, afterGenerations)
	switch {
	case publishedBases <= 0:
		t.Errorf("case 5.7: gate 5 requires main to publish new committed generations; %d commits produced none (committed bases before=%d after=%d)",
			e2eAdvanceCommits, len(beforeBases), len(afterBases))
		table.unsupported("5.7", "gate 5", "the family published no committed base generation across %d commits, so the dependent pin is not exercisable in this run (legacy regime): routes moved for %v",
			e2eAdvanceCommits, moved)
	case len(moved) > 0 || minted > 0:
		t.Errorf("case 5.7: %d committed bases were published and %d/%d dependents rebuilt anyway (%d new dependent generations): %s",
			publishedBases, len(moved), len(dependents), minted, strings.Join(moved, "; "))
		table.observed("5.7", "gate 5", "%d committed bases published; %d dependents kept their routed pair, %d rebuilt: %s",
			publishedBases, len(unchanged), len(moved), strings.Join(moved, "; "))
	default:
		table.pass("5.7", "gate 5", "%d committed bases published across %d commits; all %d dependents kept their routed pair and minted zero new generations",
			publishedBases, e2eAdvanceCommits, len(dependents))
	}

	// The pin itself: the dependents' commit layers still name a base that is
	// no longer the newest one the family published.
	if publishedBases > 0 {
		newestBase := e2eAdvanceNewest(afterBases)
		pinned := 0
		for _, dependent := range dependents {
			route := afterRoutes[filepath.Clean(dependent.path)]
			if layer, ok := afterGenerations[route.CommitGen]; ok && layer.BaseID > 0 && layer.BaseID < newestBase {
				pinned++
			}
		}
		table.observed("5.8", "gate 5", "%d/%d dependents serve a commit layer composed over a base older than the newest published base %d (the dependent pin, observed)",
			pinned, len(dependents), newestBase)
	}

	// --- case 5.9 (gate 4): an unchanged-content commit reuses.
	sameTreeBefore := e2eViewsRoutes(t, ctx, db)
	countersBefore, _ := e2eAdvanceCounters(t, f)
	treeBefore := e2eAdvanceGit(t, f, "rev-parse", "HEAD^{tree}")
	f.git(f.primary, "commit", "--allow-empty", "-m", "same tree, new commit")
	treeAfter := e2eAdvanceGit(t, f, "rev-parse", "HEAD^{tree}")
	if treeBefore != treeAfter {
		t.Fatalf("case 5.9: the empty commit was meant to keep the tree: %s -> %s", treeBefore, treeAfter)
	}
	f.settle()
	e2eViewsPresent(t, f, "5.9", f.primary, lastAdvance, primaryMarker, issue767AsPrimary)
	e2eViewsPresent(t, f, "5.9", f.primary, "GxMainAdvancePrimaryDirty", dirtyFile, issue767AsPrimary)
	sameTreeAfter := e2eViewsRoutes(t, ctx, db)
	var reKeyed []string
	for _, dependent := range dependents {
		key := filepath.Clean(dependent.path)
		if sameTreeBefore[key].pair() != sameTreeAfter[key].pair() {
			reKeyed = append(reKeyed, filepath.Base(dependent.path))
		}
	}
	if len(reKeyed) > 0 {
		t.Errorf("case 5.9: a commit that changed no content re-keyed %d dependent routes: %v", len(reKeyed), reKeyed)
	}
	countersAfter, counterErr2 := e2eAdvanceCounters(t, f)
	if counterErr2 != nil || countersBefore == nil {
		table.unsupported("5.9", "gate 4", "route stability held over an unchanged-content commit, but the claim counters were unavailable: %v", errors.Join(counterErr, counterErr2))
	} else {
		built := countersAfter["views_dedicated_base_claim_total|outcome=built"] - countersBefore["views_dedicated_base_claim_total|outcome=built"]
		reused := countersAfter["views_dedicated_base_claim_total|outcome=reused"] - countersBefore["views_dedicated_base_claim_total|outcome=reused"]
		if built > 0 {
			table.observed("5.9", "gate 4", "an unchanged-content commit paid for %d physical committed-base build(s) (reused=%d); no dependent route moved", built, reused)
		} else {
			table.pass("5.9", "gate 4", "an unchanged-content commit cost zero physical committed-base builds (reused=%d) and moved no dependent route", reused)
		}
	}

	// --- case 5.10 (gate 5): a dependency-input change is absorbed coherently.
	//
	// go.mod is a cohort input (DependencyRevision rides every layer identity,
	// checkout_coordinator.go:3089,3110), so committing it is the "dependency
	// change" arm of the rapid HEAD/config/dependency bullet.
	f.write(filepath.Join(f.primary, "go.mod"), "module example.invalid/issue767\n\ngo 1.24\n\n// e2eAdvance dependency-input change\n")
	f.write(primaryMarker, issue767MarkerSource("Issue767PrimaryMarker", "GxMainAdvanceAfterDependencyChange"))
	f.git(f.primary, "add", "go.mod", "marker.go")
	f.git(f.primary, "commit", "-m", "dependency input change")
	f.awaitSymbolIn(f.primary, "GxMainAdvanceAfterDependencyChange", primaryMarker, 10*time.Minute)
	f.settle()
	for _, dependent := range dependents {
		e2eViewsPresent(t, f, "5.10", dependent.path, dependent.marker, dependent.file, issue767AsAutomaticWorktree)
	}
	e2eViewsPresent(t, f, "5.10", f.primary, "GxMainAdvancePrimaryDirty", dirtyFile, issue767AsPrimary)
	table.pass("5.10", "gate 5", "a committed dependency-input change left every dependent serving its own tree and the primary's dirty declaration intact")

	// Counter deltas for the whole run, recorded rather than budgeted: this
	// item measures view lifecycle; the I/O budgets belong to the paired
	// sustained-workload arms, not to this matrix.
	if counterErr == nil {
		if final, err := e2eAdvanceCounters(t, f); err == nil {
			table.observed("5.11", "gate 10", "viewmetrics deltas over the whole matrix: %s", e2eAdvanceCounterDigest(beforeCounters, final))
		}
	}
}

// ------------------------------------------------------------- helpers ------

// e2eAdvanceTruthfulSample takes one rider sample from a dependent during the
// rapid-HEAD window and asserts the one thing that must never happen: an exact
// label over an answer that is not the dependent's own file. A non-exact answer
// is acceptable here and must carry a reason — that is what "truthful freshness
// until the replacement route is ready" means.
func e2eAdvanceTruthfulSample(t *testing.T, f *issue767Fixture, kase string, dependent e2eAdvanceDependent) int {
	t.Helper()
	value, raw, err := e2eViewsCLI(t, f, dependent.path, "search",
		e2eViewsSearchArgs(dependent.marker, e2eViewsViewSelector("worktree", map[string]string{"path": dependent.path})), 60*time.Second)
	if err != nil {
		// A refusal under contention is an honest answer; it is not a claim.
		t.Logf("case %s: dependent %s refused under contention: %v", kase, filepath.Base(dependent.path), err)
		return 1
	}
	exact := issue767JSONExact(value)
	fromOwnFile := issue767JSONSource(value, dependent.marker, dependent.file)
	found, fallback := issue767JSONEvidence(value, dependent.marker)
	rider := e2eViewsFreshness(value)
	if exact && found && !fromOwnFile {
		t.Fatalf("case %s: an exact answer for %s came from outside %s: %s", kase, dependent.marker, dependent.file, e2eViewsTail(raw))
	}
	if !exact && !fallback {
		if reason, _ := rider["fallback_reason"].(string); reason == "" && rider != nil {
			t.Fatalf("case %s: a non-exact answer carried no fallback_reason: rider=%v", kase, rider)
		}
	}
	return 1
}

// e2eAdvanceVisible reports whether a name answers ANYWHERE in the view a dependent
// addresses — including out of the family base's own files, which keep the
// primary's paths in a composed stack. It is a measurement of how current a
// pinned dependent's base is, never an assertion: a dependent that has not yet
// absorbed main's newest commit is behind, not wrong (checkout_coordinator.go:1269-1283
// — "Recomposing here buys currency and availability, not correctness").
func e2eAdvanceVisible(t *testing.T, f *issue767Fixture, dependent e2eAdvanceDependent, name string) bool {
	t.Helper()
	answer, err := f.askSymbol(dependent.path, name, dependent.file, issue767AsAutomaticWorktree)
	if err != nil {
		t.Logf("visibility probe for %s in %s: %v", name, filepath.Base(dependent.path), err)
		return false
	}
	return answer.Found
}

// e2eAdvanceCommittedBases is the set of committed (dedicated) base generations —
// owner_kind dedicated_graph with generation_kind "dedicated"
// (internal/graph/store_sqlite/catalog_dedicated_base.go:365,568). Checkout
// layers share the owner kind (checkout_coordinator.go:89), so the generation
// kind is the discriminator, never the owner.
func e2eAdvanceCommittedBases(generations map[int64]e2eViewsGeneration) map[int64]e2eViewsGeneration {
	bases := map[int64]e2eViewsGeneration{}
	for id, generation := range generations {
		if generation.Kind == "dedicated" {
			bases[id] = generation
		}
	}
	return bases
}

func e2eAdvanceNewest(generations map[int64]e2eViewsGeneration) int64 {
	var newest int64
	for id := range generations {
		if id > newest {
			newest = id
		}
	}
	return newest
}

// e2eAdvanceMintedFor counts generations that did not exist before the advance and
// belong to one of the dependents' checkouts — the direct census of dependent
// rebuild work.
func e2eAdvanceMintedFor(owners map[string]bool, before, after map[int64]e2eViewsGeneration) int {
	minted := 0
	for id, generation := range after {
		if _, existed := before[id]; existed {
			continue
		}
		if !owners[generation.CheckoutID] {
			continue
		}
		if generation.Kind == "commit" || generation.Kind == "dirty" {
			minted++
		}
	}
	return minted
}

// e2eAdvanceCounters scrapes the daemon's own viewmetrics through
// `daemon status --format json`. A binary without the flag or without the views
// block returns an error, which is recorded by name and never substituted with
// zeros. The parse is local to this item deliberately: a matrix must not break
// because a sibling item renames its own helper.
func e2eAdvanceCounters(t *testing.T, f *issue767Fixture) (map[string]int64, error) {
	t.Helper()
	output, err := f.tryCommand(60*time.Second, f.primary, "daemon", "status", "--format", "json", "--no-progress")
	if err != nil {
		return nil, fmt.Errorf("daemon status --format json: %w: %s", err, e2eViewsTail(output))
	}
	return e2eAdvanceParseCounters(output)
}

// e2eAdvanceParseCounters lifts views.counters out of a `daemon status --format
// json` response. A binary without the flag, without the views block or
// without counters is an error by name — never zeros, which would read as "the
// workload cost nothing".
func e2eAdvanceParseCounters(output []byte) (map[string]int64, error) {
	var payload struct {
		Views *struct {
			Counters map[string]int64 `json:"counters"`
		} `json:"views"`
	}
	if err := json.Unmarshal(output, &payload); err != nil {
		return nil, fmt.Errorf("daemon status is not JSON: %w: %s", err, e2eViewsTail(output))
	}
	if payload.Views == nil {
		return nil, errors.New("daemon status carries no views block")
	}
	if payload.Views.Counters == nil {
		return nil, errors.New("daemon status views block carries no counters")
	}
	return payload.Views.Counters, nil
}

// e2eAdvanceCounterDigest renders the non-zero deltas between two counter snapshots,
// in a stable order.
func e2eAdvanceCounterDigest(before, after map[string]int64) string {
	deltas := map[string]int64{}
	for key, value := range after {
		if delta := value - before[key]; delta != 0 {
			deltas[key] = delta
		}
	}
	for key, value := range before {
		if _, ok := after[key]; !ok && value != 0 {
			deltas[key] = -value
		}
	}
	keys := make([]string, 0, len(deltas))
	for key := range deltas {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		parts = append(parts, fmt.Sprintf("%s=%+d", key, deltas[key]))
	}
	if len(parts) == 0 {
		return "no series moved"
	}
	return strings.Join(parts, " ")
}

// e2eAdvanceGit runs one read-only git command in the primary and returns its
// trimmed output.
func e2eAdvanceGit(t *testing.T, f *issue767Fixture, args ...string) string {
	t.Helper()
	output, err := f.tryGit(f.primary, args...)
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, output)
	}
	return strings.TrimSpace(string(output))
}

// ------------------------------------------------- readers under test -------

func TestE2EMatrixAdvanceMatrixShapeIsThePlansShape(t *testing.T) {
	if e2eAdvanceDependents != 10 || e2eAdvanceCommits != 20 {
		t.Fatalf("matrix 5 is specified as twenty commits with ten dependents, got %d/%d", e2eAdvanceCommits, e2eAdvanceDependents)
	}
	if e2eAdvanceBurstFrom < 1 || e2eAdvanceBurstTo > e2eAdvanceCommits || e2eAdvanceBurstFrom > e2eAdvanceBurstTo {
		t.Fatalf("the rapid-HEAD window %d..%d is not inside the commit range", e2eAdvanceBurstFrom, e2eAdvanceBurstTo)
	}
	if e2eAdvanceBurstTo == e2eAdvanceCommits {
		t.Fatal("the burst must not be the tail of the run: the stale-completion case needs a paced commit after it to settle against")
	}
}

func TestE2EMatrixAdvanceCommittedBasesAreKeyedOnTheGenerationKind(t *testing.T) {
	// Checkout layers share the owner kind with the committed base
	// (checkout_coordinator.go:89 vs catalog_dedicated_base.go:568), so a
	// census keyed on owner_kind would count every layer as a base.
	generations := map[int64]e2eViewsGeneration{
		7:  {ID: 7, OwnerKind: "dedicated_graph", Kind: "dedicated"},
		11: {ID: 11, OwnerKind: "dedicated_graph", Kind: "commit", CheckoutID: "c1", BaseID: 7},
		12: {ID: 12, OwnerKind: "dedicated_graph", Kind: "dirty", CheckoutID: "c1", BaseID: 11},
		20: {ID: 20, OwnerKind: "dedicated_graph", Kind: "dedicated"},
	}
	bases := e2eAdvanceCommittedBases(generations)
	if len(bases) != 2 {
		t.Fatalf("committed bases = %v, want only the two dedicated generations", bases)
	}
	if newest := e2eAdvanceNewest(bases); newest != 20 {
		t.Fatalf("newest base = %d, want 20", newest)
	}
}

func TestE2EMatrixAdvanceMintedForCountsOnlyNewDependentLayers(t *testing.T) {
	before := map[int64]e2eViewsGeneration{
		11: {ID: 11, Kind: "commit", CheckoutID: "c1"},
		12: {ID: 12, Kind: "dirty", CheckoutID: "c1"},
	}
	after := map[int64]e2eViewsGeneration{
		11: {ID: 11, Kind: "commit", CheckoutID: "c1"},
		12: {ID: 12, Kind: "dirty", CheckoutID: "c1"},
		20: {ID: 20, Kind: "dedicated"},                   // the family's new base, not a dependent rebuild
		21: {ID: 21, Kind: "commit", CheckoutID: "other"}, // a checkout this matrix does not own
		22: {ID: 22, Kind: "commit", CheckoutID: "c1"},    // a real dependent rebuild
	}
	owners := map[string]bool{"c1": true}
	if minted := e2eAdvanceMintedFor(owners, before, after); minted != 1 {
		t.Fatalf("minted = %d, want exactly the one new dependent layer", minted)
	}
	if minted := e2eAdvanceMintedFor(owners, after, after); minted != 0 {
		t.Fatalf("an unchanged census minted %d", minted)
	}
}

func TestE2EMatrixAdvanceCounterParseRefusesAnArmWithoutTheViewsBlock(t *testing.T) {
	counters, err := e2eAdvanceParseCounters([]byte(`{"views":{"counters":{"views_dedicated_base_claim_total|outcome=reused":3}}}`))
	if err != nil {
		t.Fatal(err)
	}
	if counters["views_dedicated_base_claim_total|outcome=reused"] != 3 {
		t.Fatalf("counters = %v", counters)
	}
	for _, payload := range []string{`{}`, `{"views":{}}`, `not json`} {
		if _, err := e2eAdvanceParseCounters([]byte(payload)); err == nil {
			t.Fatalf("payload %q parsed as counters; a missing block must never read as zeros", payload)
		}
	}
}

func TestE2EMatrixAdvanceCounterDigestNamesEverySeriesEitherSideMoved(t *testing.T) {
	digest := e2eAdvanceCounterDigest(
		map[string]int64{"a": 1, "gone": 2},
		map[string]int64{"a": 4, "new": 1},
	)
	for _, want := range []string{"a=+3", "gone=-2", "new=+1"} {
		if !strings.Contains(digest, want) {
			t.Fatalf("digest %q is missing %q", digest, want)
		}
	}
	if digest := e2eAdvanceCounterDigest(map[string]int64{"a": 1}, map[string]int64{"a": 1}); digest != "no series moved" {
		t.Fatalf("digest = %q", digest)
	}
}
