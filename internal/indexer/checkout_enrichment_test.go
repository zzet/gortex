package indexer

import (
	"context"
	"path/filepath"
	"sort"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/graphview"
	"github.com/zzet/gortex/internal/semantic"
)

// The enrichment stage's tests run against the coordinator fixture, so what
// they drive is a real working-tree build over a real linked worktree. The
// only stand-in is the provider: a language server would make the test a test
// of gopls, and what is being pinned here is which ROOT the pass was aimed at
// and what the generation says about it afterwards.

// checkoutEnrichSpy records the roots it was asked to enrich. Two checkouts of
// one family share a repo prefix, so the root is the only thing that tells one
// pass from the other.
type checkoutEnrichSpy struct {
	mu    sync.Mutex
	roots []string
	// partial makes every pass report itself cut short, which is the other way
	// a generation loses its claim to a whole lsp capability.
	partial bool
}

func (s *checkoutEnrichSpy) Name() string        { return "checkout-spy" }
func (s *checkoutEnrichSpy) Languages() []string { return []string{"go"} }
func (s *checkoutEnrichSpy) Available() bool     { return true }
func (s *checkoutEnrichSpy) Close() error        { return nil }

func (s *checkoutEnrichSpy) Enrich(_ graph.Store, repoRoot string) (*semantic.EnrichResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.roots = append(s.roots, repoRoot)
	return &semantic.EnrichResult{
		Provider: "checkout-spy", Language: "go", Partial: s.partial, CoveragePercent: 88,
	}, nil
}

func (s *checkoutEnrichSpy) EnrichFile(graph.Store, string, string) (*semantic.EnrichResult, error) {
	return nil, nil
}

func (s *checkoutEnrichSpy) enriched() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.roots...)
}

// enrichmentManager builds the manager a checkout build enriches through, with
// the workspace cap the test wants.
func enrichmentManager(t *testing.T, cap int, spy *checkoutEnrichSpy) *semantic.Manager {
	t.Helper()
	mgr := semantic.NewManager(semantic.Config{
		Enabled:                  true,
		CheckoutLSPMaxWorkspaces: cap,
		Providers: []semantic.ProviderConfig{
			{Name: "checkout-spy", Languages: []string{"go"}, Priority: 1, Enabled: true},
		},
	}, zap.NewNop())
	mgr.RegisterProvider(spy)
	t.Cleanup(func() { _ = mgr.Close() })
	return mgr
}

// enrichingCoordinator is the fixture's coordinator with the enrichment
// manager wired into its builder — the production wiring, which the checkout
// lifecycle does from the daemon's one manager.
func (f *coordinatorFixture) enrichingCoordinator(
	t *testing.T, mgr *semantic.Manager,
) *CheckoutCoordinator {
	t.Helper()
	builder := builderNewBuilder(f.store)
	builder.Semantic = mgr
	return f.inertCoordinator(t, CheckoutCoordinatorConfig{Builder: builder})
}

// secondWorktree adds a second real automatic checkout to the family and
// returns a coordinator over it. Two checkouts of one family is the case the
// whole scoping exists for: they share the repo prefix and the base corpus,
// and nothing else.
func (f *coordinatorFixture) secondWorktree(
	t *testing.T, adminName string, mgr *semantic.Manager,
) (*CheckoutCoordinator, string) {
	t.Helper()
	root := filepath.Join(filepath.Dir(f.worktree), adminName)
	builderGit(t, f.primary, "worktree", "add", "-b", adminName, root)

	now := time.Now().Unix()
	checkout := store_sqlite.Checkout{
		CheckoutID:     "checkout-" + adminName,
		Incarnation:    "incarnation-" + adminName,
		FamilyID:       f.familyID,
		RootPath:       root,
		GitDir:         filepath.Join(f.primary, ".git", "worktrees", adminName),
		AdminName:      adminName,
		State:          store_sqlite.CheckoutStateReady,
		DesiredMode:    store_sqlite.CheckoutModeAutomatic,
		EffectiveMode:  store_sqlite.CheckoutModeAutomatic,
		HeadRef:        "refs/heads/" + adminName,
		HeadTree:       f.treeA,
		LastAccessible: now,
		LastSeen:       now,
	}
	if err := f.catalog.AllocateCheckout(context.Background(), checkout); err != nil {
		t.Fatalf("allocate %s: %v", adminName, err)
	}

	builder := builderNewBuilder(f.store)
	builder.Semantic = mgr
	coordinator, err := NewCheckoutCoordinator(CheckoutCoordinatorConfig{
		CheckoutID:   checkout.CheckoutID,
		CheckoutRoot: root,
		FamilyID:     f.familyID,
		GraphID:      f.graphID,
		RepoPrefix:   builderRepoPrefix,
		WorkspaceID:  builderRepoPrefix,
		ProjectID:    builderRepoPrefix,
		Store:        f.store,
		Builder:      builder,
		Leases:       f.leases,
		Logger:       zap.NewNop(),
		PollInterval: -1,
	})
	if err != nil {
		t.Fatalf("NewCheckoutCoordinator for %s: %v", adminName, err)
	}
	t.Cleanup(func() { _ = coordinator.Close() })
	if err := coordinator.Close(); err != nil {
		t.Fatalf("stop the %s coordinator loop: %v", adminName, err)
	}
	return coordinator, root
}

// lspStates reads what one generation declared for the language-server
// capabilities. They are declared as one row per capability from one decision,
// so a disagreement between them is itself a failure.
func lspStates(t *testing.T, store *store_sqlite.Store, generationID int64) store_sqlite.ProducerCompleteness {
	t.Helper()
	rows, err := store.AtGeneration(generationID).ProducerStates()
	if err != nil {
		t.Fatalf("read producer states of generation %d: %v", generationID, err)
	}
	lspCapabilities := map[string]bool{}
	for _, capability := range []graphview.CapabilityID{
		graphview.CapLSPReferences, graphview.CapLSPDiagnostics, graphview.CapLSPHover,
		graphview.CapLSPRename, graphview.CapLSPCodeActions,
	} {
		lspCapabilities[string(capability)] = true
	}
	var found []store_sqlite.ProducerCompleteness
	for _, row := range rows {
		if lspCapabilities[row.Producer] {
			found = append(found, row)
		}
	}
	if len(found) != len(lspCapabilities) {
		t.Fatalf("generation %d declared %d lsp capabilities, want %d", generationID, len(found), len(lspCapabilities))
	}
	for _, row := range found[1:] {
		if row.State != found[0].State || row.Reason != found[0].Reason {
			t.Fatalf("generation %d disagrees with itself: %q is %q, %q is %q",
				generationID, found[0].Producer, found[0].State, row.Producer, row.State)
		}
	}
	return found[0]
}

// enrichmentMarker reads the completion marker one generation's pass left,
// under the key the checkout scope spells.
func enrichmentMarker(
	t *testing.T, store *store_sqlite.Store, generationID int64, checkoutID string,
) (graph.EnrichmentState, bool) {
	t.Helper()
	state, found, err := store.AtGeneration(generationID).GetEnrichmentState(
		builderRepoPrefix, "checkout-spy@"+checkoutID)
	if err != nil {
		t.Fatalf("read the enrichment marker of generation %d: %v", generationID, err)
	}
	return state, found
}

// enrichableWorktree gives a checkout an uncommitted edit, so its working-tree
// layer carries a payload with symbols in it for the pass to enrich.
func enrichableWorktree(t *testing.T, root, marker string) {
	t.Helper()
	worktreeWrite(t, root, "island.go",
		"package fixture\n\nfunc Island() {\n\t// "+marker+"\n}\n")
}

// TestTwoCheckoutsOfOneFamilyEnrichIndependently is the claim the whole
// per-checkout registry exists for. The two worktrees share a repo prefix, a
// base corpus and a language, and differ only in which directory they are —
// so each pass has to be aimed at its own root, and each generation has to
// carry its own completion marker rather than one speaking for both.
func TestTwoCheckoutsOfOneFamilyEnrichIndependently(t *testing.T) {
	t.Setenv("GORTEX_ENRICH_MIN_NODES", "0")
	f := newCoordinatorFixture(t)
	spy := &checkoutEnrichSpy{}
	mgr := enrichmentManager(t, 4, spy)

	first := f.enrichingCoordinator(t, mgr)
	second, secondRoot := f.secondWorktree(t, "sibling", mgr)

	enrichableWorktree(t, f.worktree, "first-marker")
	enrichableWorktree(t, secondRoot, "second-marker")

	firstCycle := coordinatorReconcile(t, first)
	secondCycle := coordinatorReconcile(t, second)

	roots := spy.enriched()
	sort.Strings(roots)
	want := []string{f.worktree, secondRoot}
	sort.Strings(want)
	if len(roots) != 2 || roots[0] != want[0] || roots[1] != want[1] {
		t.Fatalf("enriched roots = %v, want one pass per checkout at %v", roots, want)
	}

	for _, generation := range []int64{firstCycle.DirtyGenerationID, secondCycle.DirtyGenerationID} {
		if row := lspStates(t, f.store, generation); row.State != store_sqlite.ProducerStateComplete {
			t.Errorf("generation %d declares lsp %q (%s), want complete",
				generation, row.State, row.Reason)
		}
	}

	// One marker per checkout, each naming its own working-tree state.
	firstMarker, found := enrichmentMarker(t, f.store, firstCycle.DirtyGenerationID, f.checkoutID)
	if !found {
		t.Fatal("the first checkout's pass left no marker")
	}
	secondMarker, found := enrichmentMarker(t, f.store, secondCycle.DirtyGenerationID, "checkout-sibling")
	if !found {
		t.Fatal("the second checkout's pass left no marker")
	}
	if firstMarker.IndexedSHA == "" || firstMarker.IndexedSHA == secondMarker.IndexedSHA {
		t.Errorf("the two checkouts recorded the same working-tree state %q, so the markers collapsed",
			firstMarker.IndexedSHA)
	}
	// Neither pass may claim a checkout it never read.
	if _, found := enrichmentMarker(t, f.store, firstCycle.DirtyGenerationID, "checkout-sibling"); found {
		t.Error("the first checkout's generation carries the sibling's marker")
	}
}

// TestCheckoutEnrichmentEvictsTheLeastRecentlyUsedWorkspace pins the cap doing
// its job across two checkouts: with room for one workspace, the second
// checkout's build takes the first's slot and both builds still enrich.
func TestCheckoutEnrichmentEvictsTheLeastRecentlyUsedWorkspace(t *testing.T) {
	t.Setenv("GORTEX_ENRICH_MIN_NODES", "0")
	f := newCoordinatorFixture(t)
	spy := &checkoutEnrichSpy{}
	mgr := enrichmentManager(t, 1, spy)

	first := f.enrichingCoordinator(t, mgr)
	second, secondRoot := f.secondWorktree(t, "sibling", mgr)
	enrichableWorktree(t, f.worktree, "first-marker")
	enrichableWorktree(t, secondRoot, "second-marker")

	firstCycle := coordinatorReconcile(t, first)
	live := mgr.CheckoutWorkspaces().Live()
	if len(live) != 1 || live[0].Root != f.worktree {
		t.Fatalf("after the first build the live set is %v, want the first checkout alone", live)
	}

	secondCycle := coordinatorReconcile(t, second)
	live = mgr.CheckoutWorkspaces().Live()
	if len(live) != 1 || live[0].Root != secondRoot {
		t.Fatalf("after the second build the live set is %v, want the first checkout evicted", live)
	}

	// Eviction is about the server, not about the facts: a generation that
	// enriched keeps saying so after its workspace is reclaimed.
	for _, generation := range []int64{firstCycle.DirtyGenerationID, secondCycle.DirtyGenerationID} {
		if row := lspStates(t, f.store, generation); row.State != store_sqlite.ProducerStateComplete {
			t.Errorf("generation %d declares lsp %q (%s), want complete",
				generation, row.State, row.Reason)
		}
	}
}

// TestStarvedCheckoutBuildPublishesWithHonestStates is the cap-starved case:
// the only workspace slot is held by another pass for the whole build, so the
// stage cannot run — and the build still produces a routed, published
// generation whose lsp capabilities say what is missing rather than claiming
// it is there. The next build, once the slot frees, recovers on its own.
func TestStarvedCheckoutBuildPublishesWithHonestStates(t *testing.T) {
	t.Setenv("GORTEX_ENRICH_MIN_NODES", "0")
	f := newCoordinatorFixture(t)
	spy := &checkoutEnrichSpy{}
	mgr := enrichmentManager(t, 1, spy)
	c := f.enrichingCoordinator(t, mgr)
	enrichableWorktree(t, f.worktree, "starved-marker")

	release, ok := mgr.CheckoutWorkspaces().Acquire("go", filepath.Join(f.primary, "..", "elsewhere"))
	if !ok {
		t.Fatal("could not hold the only workspace slot")
	}

	starved := coordinatorReconcile(t, c)
	if len(spy.enriched()) != 0 {
		t.Fatalf("a starved build still enriched %v", spy.enriched())
	}

	// Structurally whole: published, routed, and readable.
	row, found := f.generation(starved.DirtyGenerationID)
	if !found || row.State != store_sqlite.ViewGenerationReady {
		t.Fatalf("the starved build's generation is %q (found=%v), want ready", row.State, found)
	}
	if f.route().DirtyGenerationID != starved.DirtyGenerationID {
		t.Fatalf("the starved build's generation was not routed")
	}

	states := lspStates(t, f.store, starved.DirtyGenerationID)
	if states.State != store_sqlite.ProducerStateIncomplete {
		t.Errorf("a starved build declares lsp %q, want incomplete", states.State)
	}
	if states.Reason == "" {
		t.Error("a starved build named no reason for its lsp state")
	}

	// Recovery rides the next natural build: the slot frees, the working tree
	// moves, and the cycle the edit triggers enriches without anything having
	// to remember the starvation.
	release()
	enrichableWorktree(t, f.worktree, "recovered-marker")
	recovered := coordinatorReconcile(t, c)
	if got := spy.enriched(); len(got) != 1 || got[0] != f.worktree {
		t.Fatalf("the recovery build enriched %v, want one pass at %s", got, f.worktree)
	}
	if row := lspStates(t, f.store, recovered.DirtyGenerationID); row.State != store_sqlite.ProducerStateComplete {
		t.Errorf("the recovery build declares lsp %q (%s), want complete", row.State, row.Reason)
	}
}

// TestPartialCheckoutEnrichmentNeverClaimsComplete pins the other way a pass
// falls short: it ran, it landed edges, and it was cut off before it finished.
// The generation keeps the edges and loses the claim.
func TestPartialCheckoutEnrichmentNeverClaimsComplete(t *testing.T) {
	t.Setenv("GORTEX_ENRICH_MIN_NODES", "0")
	f := newCoordinatorFixture(t)
	spy := &checkoutEnrichSpy{partial: true}
	mgr := enrichmentManager(t, 4, spy)
	c := f.enrichingCoordinator(t, mgr)
	enrichableWorktree(t, f.worktree, "partial-marker")

	cycle := coordinatorReconcile(t, c)
	if len(spy.enriched()) != 1 {
		t.Fatalf("the pass did not run: %v", spy.enriched())
	}
	if row := lspStates(t, f.store, cycle.DirtyGenerationID); row.State != store_sqlite.ProducerStateIncomplete {
		t.Errorf("a cut-short pass declares lsp %q (%s), want incomplete", row.State, row.Reason)
	}
	if _, found := enrichmentMarker(t, f.store, cycle.DirtyGenerationID, f.checkoutID); found {
		t.Error("a cut-short pass left a completion marker")
	}
}

// TestCheckoutEnrichmentOffLeavesTheGenerationDisabled pins the config switch
// end to end: the stage does not run, and the generation says so with the
// state that means waiting will not help.
func TestCheckoutEnrichmentOffLeavesTheGenerationDisabled(t *testing.T) {
	t.Setenv("GORTEX_ENRICH_MIN_NODES", "0")
	f := newCoordinatorFixture(t)
	spy := &checkoutEnrichSpy{}
	mgr := semantic.NewManager(semantic.Config{Enabled: true, CheckoutLSP: "off"}, zap.NewNop())
	mgr.RegisterProvider(spy)
	t.Cleanup(func() { _ = mgr.Close() })

	c := f.enrichingCoordinator(t, mgr)
	enrichableWorktree(t, f.worktree, "off-marker")

	cycle := coordinatorReconcile(t, c)
	if got := spy.enriched(); len(got) != 0 {
		t.Fatalf("a switched-off stage still enriched %v", got)
	}
	row := lspStates(t, f.store, cycle.DirtyGenerationID)
	if row.State != store_sqlite.ProducerStateDisabledByConfig {
		t.Errorf("a switched-off stage declares lsp %q, want disabled_by_config", row.State)
	}
}

// TestLSPProducerRowTruthTable pins every arm of the decision in one place,
// including the two a build over a working copy cannot reach: a ref view,
// which has no working copy to enrich from, and a build that never asked for
// the stage.
func TestLSPProducerRowTruthTable(t *testing.T) {
	checkout := GenerationIdentity{OwnerKind: checkoutLayerOwnerKind}
	refView := GenerationIdentity{OwnerKind: refViewOwnerKind}

	cases := []struct {
		name     string
		identity GenerationIdentity
		outcome  EnrichmentOutcome
		want     store_sqlite.ProducerState
	}{
		{"a ref view has no working copy", refView, EnrichmentOutcome{Requested: true, Ran: []string{"go"}},
			store_sqlite.ProducerStateDisabledByConfig},
		{"a build that never asked", checkout, EnrichmentOutcome{},
			store_sqlite.ProducerStateDisabledByConfig},
		{"switched off", checkout, EnrichmentOutcome{Requested: true, Disabled: true, Reason: "off"},
			store_sqlite.ProducerStateDisabledByConfig},
		{"nothing ran", checkout, EnrichmentOutcome{Requested: true, Starved: []string{"go"}},
			store_sqlite.ProducerStateIncomplete},
		{"cut short", checkout, EnrichmentOutcome{Requested: true, Ran: []string{"go"}, Partial: true},
			store_sqlite.ProducerStateIncomplete},
		{"one language starved", checkout,
			EnrichmentOutcome{Requested: true, Ran: []string{"go"}, Starved: []string{"typescript"}},
			store_sqlite.ProducerStateIncomplete},
		{"whole", checkout, EnrichmentOutcome{Requested: true, Ran: []string{"go"}},
			store_sqlite.ProducerStateComplete},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			row := lspProducerRow(tc.identity, tc.outcome)
			if row.State != tc.want {
				t.Errorf("state = %q, want %q", row.State, tc.want)
			}
			if tc.want == store_sqlite.ProducerStateComplete {
				if row.Reason != "" {
					t.Errorf("a complete capability carries a reason: %q", row.Reason)
				}
				return
			}
			if row.Reason == "" {
				t.Error("a narrowed capability carries no reason")
			}
		})
	}
}

// TestDroppingACoordinatorStopsItsCheckoutWorkspaces pins the reclamation: a
// checkout that loses its coordinator loses its language servers with it,
// rather than leaving them rooted at a directory nothing serves until the
// router's idle reaper or another checkout's cap pressure gets to them.
func TestDroppingACoordinatorStopsItsCheckoutWorkspaces(t *testing.T) {
	t.Setenv("GORTEX_ENRICH_MIN_NODES", "0")
	f := newCoordinatorFixture(t)
	spy := &checkoutEnrichSpy{}
	mgr := enrichmentManager(t, 4, spy)
	c := f.enrichingCoordinator(t, mgr)
	enrichableWorktree(t, f.worktree, "dropped-marker")

	coordinatorReconcile(t, c)
	live := mgr.CheckoutWorkspaces().Live()
	if len(live) != 1 || live[0].Root != f.worktree {
		t.Fatalf("after the build the live set is %v, want the checkout's own workspace", live)
	}

	l := &CheckoutLifecycle{
		mi:           &MultiIndexer{semanticMgr: mgr},
		coordinators: map[string]*CheckoutCoordinator{f.checkoutID: c},
	}
	l.dropCoordinator(f.checkoutID)

	if live := mgr.CheckoutWorkspaces().Live(); len(live) != 0 {
		t.Errorf("the dropped checkout's workspaces are still live: %v", live)
	}
}
