package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/daemon"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/graphview"
	"github.com/zzet/gortex/internal/reconcile"
	"github.com/zzet/gortex/internal/testenv"
)

// The probe fixture: one family with a dedicated primary and one automatic
// worktree beside it, over a base corpus the worktree's routed generations
// stack on.
const (
	probeFamily     = "fam-probe"
	probePrimaryID  = "co-probe-primary"
	probeWorktreeID = "co-probe-worktree"
	probeGraphID    = "graph-probe"
	probePrefix     = "repo"
	probeFile       = "internal/live.go"
)

// probeFileKey is the key the store holds probeFile under: the repo prefix,
// one '/', then the repo-relative remainder in the indexing machine's native
// separators (see internal/graphpath).
//
// The two spellings coincide on POSIX and diverge on Windows, and the probe is
// exactly the seam between them: fileGraphKey renders `prefix + "/" +
// filepath.Rel(root, path)`, whose remainder carries the host separator.
// Keying the corpus with the '/'-joined form describes a file a Windows daemon
// never indexes, so the key the probe computes misses it and every coverage
// assertion below reads as uncovered.
var probeFileKey = probePrefix + "/" + filepath.FromSlash(probeFile)

// probeFixture is the seeded catalog a probe resolves against, plus the paths
// a caller probes with.
type probeFixture struct {
	controller   *realController
	store        *store_sqlite.Store
	catalog      *store_sqlite.Catalog
	primaryRoot  string
	worktreeRoot string
}

// newProbeFixture builds a controller over a real sqlite store, seeds the base
// corpus, and registers a family whose primary is dedicated and whose worktree
// is automatic. The worktree is left unrouted; the routing helpers below add
// the generations a test needs.
func newProbeFixture(t *testing.T) *probeFixture {
	t.Helper()
	c, mi, catalog, dir := buildCatalogController(t)
	store, ok := c.graph.(*store_sqlite.Store)
	require.True(t, ok, "the fixture opened a %T, not the sqlite store", c.graph)
	_ = mi

	primaryRoot := filepath.Join(dir, "primary")
	worktreeRoot := filepath.Join(dir, "worktree")

	// The base corpus: what the primary's own checkout reads, and what an
	// automatic worktree's generations compose over.
	store.AddBatch([]*graph.Node{
		{
			ID:         probeFileKey + "::BaseOnly",
			Kind:       graph.KindFunction,
			Name:       "BaseOnly",
			FilePath:   probeFileKey,
			RepoPrefix: probePrefix,
			Language:   "go",
			StartLine:  3,
			EndLine:    5,
		},
		{
			ID:         probeFileKey + "::AlsoBase",
			Kind:       graph.KindFunction,
			Name:       "AlsoBase",
			FilePath:   probeFileKey,
			RepoPrefix: probePrefix,
			Language:   "go",
			StartLine:  7,
			EndLine:    9,
		},
	}, nil)

	ctx := context.Background()
	require.NoError(t, catalog.UpsertRepositoryFamily(ctx, store_sqlite.RepositoryFamily{
		FamilyID:          probeFamily,
		CommonDirIdentity: filepath.Join(primaryRoot, ".git"),
		State:             reconcile.FamilyStateReady,
		CreatedAt:         100,
		LastSeen:          100,
	}))
	require.NoError(t, catalog.UpsertCheckout(ctx, store_sqlite.Checkout{
		CheckoutID:    probePrimaryID,
		Incarnation:   "inc-primary",
		FamilyID:      probeFamily,
		RootPath:      primaryRoot,
		GitDir:        filepath.Join(primaryRoot, ".git"),
		AdminName:     "primary",
		State:         store_sqlite.CheckoutStateReady,
		DesiredMode:   store_sqlite.CheckoutModeDedicated,
		EffectiveMode: store_sqlite.CheckoutModeDedicated,
		LastSeen:      101,
	}))
	require.NoError(t, catalog.UpsertCheckout(ctx, store_sqlite.Checkout{
		CheckoutID:    probeWorktreeID,
		Incarnation:   "inc-worktree",
		FamilyID:      probeFamily,
		RootPath:      worktreeRoot,
		GitDir:        filepath.Join(primaryRoot, ".git", "worktrees", "worktree"),
		AdminName:     "worktree",
		State:         store_sqlite.CheckoutStateReady,
		DesiredMode:   store_sqlite.CheckoutModeAutomatic,
		EffectiveMode: store_sqlite.CheckoutModeAutomatic,
		LastSeen:      101,
	}))
	require.NoError(t, catalog.UpsertDedicatedGraph(ctx, store_sqlite.DedicatedGraph{
		GraphID:         probeGraphID,
		OwnerCheckoutID: probePrimaryID,
		RepoPrefix:      probePrefix,
		FamilyID:        probeFamily,
		IsPrimaryBase:   true,
		State:           reconcile.GraphStateReady,
	}))

	c.viewMaterializer = &graphview.Materializer{
		Store:   store,
		Catalog: catalog,
		Leases:  c.lifecycle.ViewLeases(),
	}
	return &probeFixture{
		controller:   c,
		store:        store,
		catalog:      catalog,
		primaryRoot:  primaryRoot,
		worktreeRoot: worktreeRoot,
	}
}

// routeWorktree publishes the two generations the worktree's route names and
// activates the route. The dirty generation claims the probed file and emits
// one symbol of its own, so a composed answer is distinguishable from a base
// one by content rather than by shape.
func (f *probeFixture) routeWorktree(t *testing.T) {
	t.Helper()
	ctx := context.Background()

	commitID, commitHandle, err := f.store.BeginPayloadGeneration(ctx, store_sqlite.PayloadGenerationRequest{
		OwnerKind:      "dedicated_graph",
		GraphID:        probeGraphID,
		LayerID:        "layer-probe-commit",
		CheckoutID:     probeWorktreeID,
		GenerationKind: "commit",
		TreeOID:        "tree-probe-commit",
		CreatedAt:      1000,
	})
	require.NoError(t, err)
	_ = commitHandle
	require.NoError(t, f.store.PublishPayloadGeneration(ctx, commitID, 2000))

	dirtyID, dirtyHandle, err := f.store.BeginPayloadGeneration(ctx, store_sqlite.PayloadGenerationRequest{
		OwnerKind:        "dedicated_graph",
		GraphID:          probeGraphID,
		LayerID:          "layer-probe-dirty",
		CheckoutID:       probeWorktreeID,
		GenerationKind:   "dirty",
		BaseGenerationID: commitID,
		TreeOID:          "tree-probe-dirty",
		CreatedAt:        1001,
	})
	require.NoError(t, err)
	dirtyHandle.AddBatch([]*graph.Node{{
		ID:         probeFileKey + "::GenerationOnly",
		Kind:       graph.KindFunction,
		Name:       "GenerationOnly",
		FilePath:   probeFileKey,
		RepoPrefix: probePrefix,
		Language:   "go",
		StartLine:  3,
		EndLine:    6,
	}}, nil)
	require.NoError(t, dirtyHandle.SetFileMasks([]store_sqlite.FileMask{{
		RepoPrefix: probePrefix,
		FilePath:   probeFileKey,
		Mode:       store_sqlite.OwnershipReplace,
	}}))
	require.NoError(t, f.store.PublishPayloadGeneration(ctx, dirtyID, 2001))

	require.NoError(t, f.catalog.UpsertCheckoutRoute(ctx, store_sqlite.CheckoutRoute{
		CheckoutID:         probeWorktreeID,
		GraphID:            probeGraphID,
		CommitGenerationID: commitID,
		DirtyGenerationID:  dirtyID,
		State:              store_sqlite.RouteActive,
	}))
}

// upsertWorktree rewrites the automatic checkout's row with the given root and
// state, which is how a test moves it into a grace window or re-homes it.
func (f *probeFixture) upsertWorktree(t *testing.T, root string, state store_sqlite.CheckoutState) {
	t.Helper()
	require.NoError(t, f.catalog.UpsertCheckout(context.Background(), store_sqlite.Checkout{
		CheckoutID:    probeWorktreeID,
		Incarnation:   "inc-worktree",
		FamilyID:      probeFamily,
		RootPath:      root,
		GitDir:        filepath.Join(f.primaryRoot, ".git", "worktrees", "worktree"),
		AdminName:     "worktree",
		State:         state,
		DesiredMode:   store_sqlite.CheckoutModeAutomatic,
		EffectiveMode: store_sqlite.CheckoutModeAutomatic,
		LastSeen:      102,
	}))
	f.worktreeRoot = root
}

// symbolNames flattens a probe answer to the names it cited.
func symbolNames(hits []daemon.SymbolHit) []string {
	out := make([]string, 0, len(hits))
	for _, h := range hits {
		out = append(out, h.Name)
	}
	return out
}

// TestProbeOfRoutedWorktreeReadsTheComposedView pins the whole point of the
// path parameter: a file inside a routed automatic worktree is answered from
// that working copy's composed stack, so the generation's own symbol is cited
// and the base symbol the generation replaced is not.
func TestProbeOfRoutedWorktreeReadsTheComposedView(t *testing.T) {
	f := newProbeFixture(t)
	f.routeWorktree(t)
	ctx := context.Background()
	probed := filepath.Join(f.worktreeRoot, probeFile)

	coverage, err := f.controller.FileCoverage(ctx, daemon.FileCoverageParams{Path: probed})
	require.NoError(t, err)
	assert.True(t, coverage.Covered, "the composed view holds this file")
	assert.Equal(t, 1, coverage.Symbols,
		"the generation replaced the file, so only its own symbol is in it")
	require.NotNil(t, coverage.View)
	assert.Equal(t, daemon.ProbeViewWorktree, coverage.View.Kind)
	assert.Equal(t, probeWorktreeID, coverage.View.CheckoutID)
	assert.True(t, coverage.View.Exact, "the working copy's own view answered")

	found, err := f.controller.SearchSymbols(ctx, daemon.SearchSymbolsParams{
		Query: "GenerationOnly", Path: probed,
	})
	require.NoError(t, err)
	assert.Equal(t, []string{"GenerationOnly"}, symbolNames(found.Hits))

	hidden, err := f.controller.SearchSymbols(ctx, daemon.SearchSymbolsParams{
		Query: "BaseOnly", Path: probed,
	})
	require.NoError(t, err)
	assert.Empty(t, hidden.Hits,
		"the generation owns the file, so the base symbol it dropped must not surface")

	// The same query without a path still reads the base corpus, which is
	// what proves the two answers are different graphs rather than one.
	base, err := f.controller.SearchSymbols(ctx, daemon.SearchSymbolsParams{Query: "BaseOnly"})
	require.NoError(t, err)
	assert.Equal(t, []string{"BaseOnly"}, symbolNames(base.Hits))
}

// TestProbeOfRoutedWorktreeReleasesItsLease proves the lease a probe takes is
// dropped when the answer is built. A lease that outlived the call would make
// the generation permanently unretirable, which is worse than the stale answer
// it protects against.
func TestProbeOfRoutedWorktreeReleasesItsLease(t *testing.T) {
	f := newProbeFixture(t)
	f.routeWorktree(t)
	ctx := context.Background()

	route, found, err := f.catalog.GetCheckoutRoute(ctx, probeWorktreeID)
	require.NoError(t, err)
	require.True(t, found)

	_, err = f.controller.FileCoverage(ctx, daemon.FileCoverageParams{
		Path: filepath.Join(f.worktreeRoot, probeFile),
	})
	require.NoError(t, err)

	inUse := f.controller.lifecycle.ViewLeases().InUse
	assert.False(t, inUse(route.DirtyGenerationID),
		"the probe still holds the working-tree generation after answering")
	assert.False(t, inUse(route.CommitGenerationID),
		"the probe still holds the commit generation after answering")
}

// TestProbeOfUnroutedWorktreeAnswersUncovered pins the reconcile-then-enforce
// posture: a registered working copy with no composed view answers uncovered,
// so the hook lets the native tool through instead of denying it on the
// primary's content.
func TestProbeOfUnroutedWorktreeAnswersUncovered(t *testing.T) {
	f := newProbeFixture(t)
	f.controller.probeReconcile = func(string) {}
	// An unrouted, ready, automatic worktree wakes its own coordinator; the
	// stub observes the nudge without spawning a real build behind the probe.
	f.controller.probeActivateCheckout = func(string) bool { return true }
	ctx := context.Background()
	probed := filepath.Join(f.worktreeRoot, probeFile)

	coverage, err := f.controller.FileCoverage(ctx, daemon.FileCoverageParams{Path: probed})
	require.NoError(t, err)
	assert.False(t, coverage.Covered, "nothing describes this working copy yet")
	assert.Zero(t, coverage.Symbols)
	require.NotNil(t, coverage.View)
	assert.Equal(t, daemon.ProbeViewUnrouted, coverage.View.Kind)
	assert.False(t, coverage.View.Exact)
	assert.Equal(t, daemon.FallbackViewBuilding, coverage.View.FallbackReason)

	found, err := f.controller.SearchSymbols(ctx, daemon.SearchSymbolsParams{
		Query: "BaseOnly", Path: probed,
	})
	require.NoError(t, err)
	assert.Empty(t, found.Hits,
		"the primary's symbols are another working copy's content, not evidence about this one")
	require.NotNil(t, found.View)
	assert.Equal(t, daemon.ProbeViewUnrouted, found.View.Kind)
}

// TestUnroutedProbeBurstActivatesOncePerWindow pins the debounce. A hook probes
// once per tool call, so an agent working in an unrouted worktree raises the
// same request continuously; the worktree's coordinator must be activated once
// per window however many probes ask for it.
func TestUnroutedProbeBurstActivatesOncePerWindow(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newProbeFixture(t)
		var activated []string
		f.controller.probeActivateCheckout = func(checkoutID string) bool {
			activated = append(activated, checkoutID)
			return true
		}
		ctx := context.Background()
		probed := filepath.Join(f.worktreeRoot, probeFile)

		for range 5 {
			_, err := f.controller.FileCoverage(ctx, daemon.FileCoverageParams{Path: probed})
			require.NoError(t, err)
		}
		synctest.Wait()
		assert.Equal(t, []string{probeWorktreeID}, activated,
			"a burst of probes must not activate the checkout per probe")

		// Inside the window nothing more is asked for, however long the burst
		// runs.
		time.Sleep(probeReconcileDebounce - time.Second)
		_, err := f.controller.FileCoverage(ctx, daemon.FileCoverageParams{Path: probed})
		require.NoError(t, err)
		synctest.Wait()
		assert.Len(t, activated, 1, "the window had not elapsed")

		// Past it, the next probe asks again: the working copy is still
		// unrouted and the janitor's own tick is an hour away.
		time.Sleep(2 * time.Second)
		_, err = f.controller.FileCoverage(ctx, daemon.FileCoverageParams{Path: probed})
		require.NoError(t, err)
		synctest.Wait()
		assert.Equal(t, []string{probeWorktreeID, probeWorktreeID}, activated)
	})
}

// TestProbeOfWorktreeInGraceFallsBackToTheFamilyPrimary pins the grace rule: a
// working copy that stopped answering is served by the family primary, by the
// same fallback a read-only query takes — and the answer says so rather than
// passing for the working copy's own.
func TestProbeOfWorktreeInGraceFallsBackToTheFamilyPrimary(t *testing.T) {
	f := newProbeFixture(t)
	f.controller.probeReconcile = func(string) {}
	f.upsertWorktree(t, f.worktreeRoot, store_sqlite.CheckoutStateAvailabilityGrace)
	ctx := context.Background()
	probed := filepath.Join(f.worktreeRoot, probeFile)

	coverage, err := f.controller.FileCoverage(ctx, daemon.FileCoverageParams{Path: probed})
	require.NoError(t, err)
	assert.True(t, coverage.Covered, "the family primary still describes this file")
	assert.Equal(t, 2, coverage.Symbols)
	require.NotNil(t, coverage.View)
	assert.Equal(t, daemon.ProbeViewBase, coverage.View.Kind)
	assert.False(t, coverage.View.Exact, "a fallback answer must never read as exact")
	assert.Equal(t, string(store_sqlite.CheckoutStateAvailabilityGrace), coverage.View.FallbackReason)
}

// TestProbeOfDedicatedCheckoutReadsTheBaseCorpus pins that a checkout served
// from its own corpus is answered exactly as it was before routed views: from
// the base, unscoped, and marked exact.
func TestProbeOfDedicatedCheckoutReadsTheBaseCorpus(t *testing.T) {
	f := newProbeFixture(t)
	ctx := context.Background()
	probed := filepath.Join(f.primaryRoot, probeFile)

	coverage, err := f.controller.FileCoverage(ctx, daemon.FileCoverageParams{Path: probed})
	require.NoError(t, err)
	assert.True(t, coverage.Covered)
	assert.Equal(t, 2, coverage.Symbols)
	require.NotNil(t, coverage.View)
	assert.Equal(t, daemon.ProbeViewBase, coverage.View.Kind)
	assert.True(t, coverage.View.Exact)

	found, err := f.controller.SearchSymbols(ctx, daemon.SearchSymbolsParams{
		Query: "BaseOnly", Path: probed,
	})
	require.NoError(t, err)
	assert.Equal(t, []string{"BaseOnly"}, symbolNames(found.Hits))
}

func TestProbeOfDedicatedCheckoutInRemovalGraceIsLabeledFallback(t *testing.T) {
	f := newProbeFixture(t)
	ctx := context.Background()
	checkout, found, err := f.catalog.GetCheckout(ctx, probePrimaryID)
	require.NoError(t, err)
	require.True(t, found)
	checkout.State = store_sqlite.CheckoutStateRemovalGrace
	require.NoError(t, f.catalog.UpsertCheckout(ctx, checkout))

	result, err := f.controller.SearchSymbols(ctx, daemon.SearchSymbolsParams{
		Query: "BaseOnly", Path: filepath.Join(f.primaryRoot, probeFile),
	})
	require.NoError(t, err)
	assert.Equal(t, []string{"BaseOnly"}, symbolNames(result.Hits))
	require.NotNil(t, result.View)
	assert.Equal(t, daemon.ProbeViewBase, result.View.Kind)
	assert.False(t, result.View.Exact)
	assert.Equal(t, string(store_sqlite.CheckoutStateRemovalGrace), result.View.FallbackReason)
}

// TestProbeOfUntrackedPathIsUnchanged pins that a path no checkout owns still
// reads the base corpus. It is the ordinary case for every directory nobody
// has tracked, and nothing about routed views may change it.
func TestProbeOfUntrackedPathIsUnchanged(t *testing.T) {
	f := newProbeFixture(t)
	ctx := context.Background()

	found, err := f.controller.SearchSymbols(ctx, daemon.SearchSymbolsParams{
		Query: "BaseOnly", Path: filepath.Join(t.TempDir(), "elsewhere.go"),
	})
	require.NoError(t, err)
	assert.Equal(t, []string{"BaseOnly"}, symbolNames(found.Hits))
	require.NotNil(t, found.View)
	assert.Equal(t, daemon.ProbeViewBase, found.View.Kind)
	assert.True(t, found.View.Exact)
}

// TestSearchSymbolsWithoutAPathKeepsTheLegacyWireShape is the compatibility
// gate. A client that predates routed views sends no path, and its answer must
// come back byte for byte what it has always been — in particular with no view
// block at all, which an older decoder would reject or silently ignore.
func TestSearchSymbolsWithoutAPathKeepsTheLegacyWireShape(t *testing.T) {
	f := newProbeFixture(t)
	f.routeWorktree(t)

	result, err := f.controller.SearchSymbols(context.Background(),
		daemon.SearchSymbolsParams{Query: "BaseOnly"})
	require.NoError(t, err)
	require.Nil(t, result.View, "an answer to a path-less request names no view")

	encoded, err := json.Marshal(result)
	require.NoError(t, err)
	// The hit carries the node's own file path, so the expectation is the key
	// the corpus was seeded under rather than a '/'-spelled literal that only
	// happens to be that key on POSIX.
	wantPath, err := json.Marshal(probeFileKey)
	require.NoError(t, err)
	assert.JSONEq(t,
		`{"hits":[{"name":"BaseOnly","kind":"function","file_path":`+string(wantPath)+`,"line":3}]}`,
		string(encoded))

	// A miss keeps its shape too: the empty hit list, and nothing else.
	miss, err := f.controller.SearchSymbols(context.Background(),
		daemon.SearchSymbolsParams{Query: "NothingNamedThis"})
	require.NoError(t, err)
	encodedMiss, err := json.Marshal(miss)
	require.NoError(t, err)
	assert.JSONEq(t, `{"hits":[]}`, string(encodedMiss))
}

// TestProbeMatchesAPathSpelledThroughASymlink pins the spelling rule the
// catalog already holds to: git records a worktree root with its symlinks
// resolved while a shell hands over the path it was given, and measuring one
// against the other reports a file inside the checkout as outside it.
func TestProbeMatchesAPathSpelledThroughASymlink(t *testing.T) {
	f := newProbeFixture(t)
	dir := t.TempDir()
	realRoot := filepath.Join(dir, "real-wt")
	linkRoot := filepath.Join(dir, "link-wt")
	require.NoError(t, os.MkdirAll(filepath.Join(realRoot, "internal"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(realRoot, probeFile), []byte("package live\n"), 0o644))
	// Creating a symlink on Windows needs a privilege the host may withhold,
	// which is a limitation of the host rather than a defect in the fold.
	// Everywhere else the failure is real.
	if err := os.Symlink(realRoot, linkRoot); err != nil {
		if runtime.GOOS == "windows" {
			t.Skipf("symlink creation unavailable on this Windows host: %v", err)
		}
		t.Fatal(err)
	}

	// The catalog holds the spelling git records — symlinks resolved — while
	// the probe arrives spelled the way the caller's shell handed it over.
	resolvedRoot, err := filepath.EvalSymlinks(realRoot)
	require.NoError(t, err)
	f.upsertWorktree(t, resolvedRoot, store_sqlite.CheckoutStateReady)
	f.routeWorktree(t)

	coverage, coverageErr := f.controller.FileCoverage(context.Background(), daemon.FileCoverageParams{
		Path: filepath.Join(linkRoot, probeFile),
	})
	require.NoError(t, coverageErr)
	assert.True(t, coverage.Covered, "the path names a file the composed view holds")
	assert.Equal(t, 1, coverage.Symbols)
}

// TestControllerAnswersTheFileCoverageVerb pins that the production controller
// is what the daemon dispatches the coverage kind to. Without it the verb
// exists on the wire and nothing on this side implements it.
func TestControllerAnswersTheFileCoverageVerb(t *testing.T) {
	var _ daemon.FileCoverageController = newProbeFixture(t).controller
}

func TestTopologyNudgeRunsImmediatelyAndKeepsATrailingEvent(t *testing.T) {
	testenv.Sandbox(t)
	firstStarted := make(chan struct{})
	releaseFirst := make(chan struct{})
	calls := make(chan string, 3)
	releases := make(chan string, 4)
	callOrdinal := 0
	controller := &realController{
		// A recent probe nudge must not suppress a filesystem event.
		probeNudgedAt: map[string]time.Time{"family": time.Now()},
	}
	controller.probeReconcile = func(familyID string) {
		callOrdinal++
		calls <- familyID
		if callOrdinal == 1 {
			close(firstStarted)
			<-releaseFirst
		}
	}

	controller.nudgeFamilyTopologyRequest(context.Background(), "family", func() {
		releases <- "running"
	})
	select {
	case <-firstStarted:
	case <-time.After(time.Second):
		t.Fatal("topology reconciliation did not start promptly")
	}

	// A burst during the active pass is coalesced to exactly one trailing
	// reconciliation rather than discarded by the probe debounce. The latest
	// event's values survive cancellation so teardown reached by that trailing
	// pass can still identify its exact MultiWatcher dispatch.
	type topologyContextKey struct{}
	firstPending, cancel := context.WithCancel(context.WithValue(
		context.Background(), topologyContextKey{}, "first-pending"))
	controller.nudgeFamilyTopologyRequest(firstPending, "family", func() {
		releases <- "superseded"
	})
	cancel()
	controller.nudgeFamilyTopologyRequest(context.WithValue(
		context.Background(), topologyContextKey{}, "latest-pending"), "family", func() {
		releases <- "trailing"
	})
	select {
	case released := <-releases:
		assert.Equal(t, "superseded", released)
	case <-time.After(time.Second):
		t.Fatal("superseded pending lease was not released promptly")
	}
	controller.topologyNudgeMu.Lock()
	pendingRequest := controller.topologyNudges["family"].pending
	controller.topologyNudgeMu.Unlock()
	require.NotNil(t, pendingRequest)
	pendingContext := pendingRequest.ctx
	require.NotNil(t, pendingContext)
	assert.NoError(t, pendingContext.Err())
	assert.Equal(t, "latest-pending", pendingContext.Value(topologyContextKey{}))
	close(releaseFirst)

	for want := 0; want < 2; want++ {
		select {
		case familyID := <-calls:
			if familyID != "family" {
				t.Fatalf("reconciled family %q, want family", familyID)
			}
		case <-time.After(time.Second):
			t.Fatalf("received %d reconciliation calls, want two", want)
		}
	}
	seen := map[string]bool{}
	for len(seen) < 2 {
		select {
		case released := <-releases:
			seen[released] = true
		case <-time.After(time.Second):
			t.Fatalf("completed releases = %v, want running and trailing", seen)
		}
	}
	assert.True(t, seen["running"])
	assert.True(t, seen["trailing"])
	select {
	case familyID := <-calls:
		t.Fatalf("unexpected third reconciliation for %q", familyID)
	case released := <-releases:
		t.Fatalf("lease %q released more than once", released)
	case <-time.After(25 * time.Millisecond):
	}
}

func TestTopologyNudgeReleasesRejectedRequest(t *testing.T) {
	controller := &realController{}
	released := make(chan struct{}, 1)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	controller.nudgeFamilyTopologyRequest(ctx, "family", func() { released <- struct{}{} })
	select {
	case <-released:
	case <-time.After(time.Second):
		t.Fatal("rejected topology request retained its lease")
	}
}

func TestTopologyNudgePanicReleasesCurrentAndPending(t *testing.T) {
	controller := &realController{topologyNudges: make(map[string]*topologyNudgeState)}
	released := make(chan string, 2)
	current := newTopologyNudgeRequest(context.Background(), func() { released <- "current" })
	pending := newTopologyNudgeRequest(context.Background(), func() { released <- "pending" })
	controller.topologyNudges["family"] = &topologyNudgeState{pending: &pending}

	assert.Panics(t, func() {
		controller.runTopologyNudgeLoop(func(context.Context, string) {
			panic("topology reconcile panic")
		}, "family", current)
	})
	seen := map[string]bool{}
	for range 2 {
		select {
		case name := <-released:
			seen[name] = true
		case <-time.After(time.Second):
			t.Fatalf("panic releases = %v, want current and pending", seen)
		}
	}
	assert.True(t, seen["current"])
	assert.True(t, seen["pending"])
	assert.Empty(t, controller.topologyNudges)
}
