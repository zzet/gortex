package indexer

import (
	"context"
	"path/filepath"
	"testing"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

// secondWorktreeCoordinator adds a second linked worktree of the fixture's
// family, detached at commit, registers it as an automatic checkout and
// returns a stopped coordinator for it.
func (f *coordinatorFixture) secondWorktreeCoordinator(t *testing.T, commit string) (*CheckoutCoordinator, string) {
	t.Helper()
	const admin = "second"
	root := filepath.Join(filepath.Dir(f.worktree), admin)
	builderGit(t, f.primary, "worktree", "add", "--detach", root, commit)
	checkout := store_sqlite.Checkout{
		CheckoutID:    "checkout-second",
		Incarnation:   "incarnation-second",
		FamilyID:      f.familyID,
		RootPath:      root,
		GitDir:        filepath.Join(f.primary, ".git", "worktrees", admin),
		AdminName:     admin,
		State:         store_sqlite.CheckoutStateReady,
		DesiredMode:   store_sqlite.CheckoutModeAutomatic,
		EffectiveMode: store_sqlite.CheckoutModeAutomatic,
		HeadTree:      builderGit(t, root, "rev-parse", "HEAD^{tree}"),
	}
	if err := f.catalog.AllocateCheckout(context.Background(), checkout); err != nil {
		t.Fatalf("allocate the second checkout: %v", err)
	}
	coordinator, err := NewCheckoutCoordinator(CheckoutCoordinatorConfig{
		CheckoutID: checkout.CheckoutID, CheckoutRoot: root, FamilyID: f.familyID,
		RepoPrefix: builderRepoPrefix, WorkspaceID: builderRepoPrefix, ProjectID: builderRepoPrefix,
		Store: f.store, Builder: builderNewBuilder(f.store), Leases: f.leases,
		Config: config.Default().Index, ConfigSections: dedicatedBaseConfigSections(config.Default()),
		Logger: zap.NewNop(), PollInterval: -1,
	})
	if err != nil {
		t.Fatalf("NewCheckoutCoordinator: %v", err)
	}
	if err := coordinator.Close(); err != nil {
		t.Fatalf("stop the coordinator loop: %v", err)
	}
	return coordinator, checkout.CheckoutID
}

// A new checkout at a tree another checkout of the graph already routes serves
// that checkout's commit layer instead of indexing the tree again; the layer
// survives the first checkout moving on, because the second one routes it.
func TestNewCheckoutAdoptsAnotherCheckoutsCommitLayer(t *testing.T) {
	f := newCoordinatorFixture(t)
	first := f.inertCoordinator(t, CheckoutCoordinatorConfig{})
	f.commitTreeB()
	commitB := builderGit(t, f.worktree, "rev-parse", "HEAD")
	built := coordinatorReconcile(t, first)
	if !built.CommitBuilt || built.CommitGenerationID == 0 {
		t.Fatalf("the first checkout did not build B's commit layer: %+v", built)
	}

	second, secondID := f.secondWorktreeCoordinator(t, commitB)
	adopted := coordinatorReconcile(t, second)
	if adopted.CommitBuilt {
		t.Fatalf("the second checkout indexed B again: %+v", adopted)
	}
	if !adopted.CommitReused || adopted.CommitGenerationID != built.CommitGenerationID {
		t.Fatalf("the second checkout routed commit generation %d (reused=%v), want the first's %d",
			adopted.CommitGenerationID, adopted.CommitReused, built.CommitGenerationID)
	}
	// The next cycle over the same tree serves the adopted layer from the
	// checkout's reuse cache, without scanning the catalog again.
	lookups := sharedCommitLookups.Load()
	again := coordinatorReconcile(t, second)
	if again.CommitBuilt || again.CommitGenerationID != built.CommitGenerationID {
		t.Fatalf("the second checkout's next cycle: %+v", again)
	}
	if n := sharedCommitLookups.Load() - lookups; n != 0 {
		t.Fatalf("the next cycle scanned the catalog for a shared layer %d times; the adopted layer is cached", n)
	}
	route, found, err := f.catalog.GetCheckoutRoute(context.Background(), secondID)
	if err != nil || !found || route.CommitGenerationID != built.CommitGenerationID {
		t.Fatalf("the second checkout's route: %+v found=%v err=%v", route, found, err)
	}

	// The first checkout moves back to A and lets B's layer go; the second
	// still routes it, so it is not retired.
	builderGit(t, f.worktree, "checkout", "--detach", "main")
	coordinatorReconcile(t, first)
	// Everything the first checkout holds is offered for retirement, as a
	// teardown does.
	for _, generation := range first.DrainRetirements() {
		first.offerRetire(context.Background(), generation)
	}
	if row, ok := f.generation(built.CommitGenerationID); !ok || !servableGeneration(row.State) {
		t.Fatalf("the shared commit layer was retired under the second checkout: %+v found=%v", row, ok)
	}
}

// A commit layer is shared only when every input it was built from matches;
// the checkout and layer names are the only fields allowed to differ.
func TestSharedCommitLayerRequiresEveryInput(t *testing.T) {
	identity := GenerationIdentity{
		OwnerKind: checkoutLayerOwnerKind, GraphID: "g", LayerID: "layer-mine", CheckoutID: "mine",
		GenerationKind: CommitLayerGenerationKind, BaseGenerationID: 7, LowerViewFingerprint: "base-tree",
		TreeOID: "tree", ProvenanceCommitOID: "commit", ConfigHash: "cfg", ExtractorVersions: "ext",
		ResolverVersion: "res", DependencyRevision: "dep",
	}
	row := store_sqlite.ViewGeneration{
		OwnerKind: identity.OwnerKind, GraphID: "g", LayerID: "layer-other", CheckoutID: "other",
		GenerationKind: identity.GenerationKind, BaseGenerationID: 7, LowerViewFingerprint: "base-tree",
		TreeOID: "tree", ProvenanceCommitOID: "commit", ConfigHash: "cfg", ExtractorVersions: "ext",
		ResolverVersion: "res", DependencyRevision: "dep",
	}
	if !sameCommitLayerInputs(row, identity) {
		t.Fatal("a layer built from the same inputs by another checkout is not shared")
	}
	for name, change := range map[string]func(*store_sqlite.ViewGeneration){
		"graph":      func(r *store_sqlite.ViewGeneration) { r.GraphID = "h" },
		"base":       func(r *store_sqlite.ViewGeneration) { r.BaseGenerationID = 8 },
		"base_tree":  func(r *store_sqlite.ViewGeneration) { r.LowerViewFingerprint = "other" },
		"tree":       func(r *store_sqlite.ViewGeneration) { r.TreeOID = "other" },
		"provenance": func(r *store_sqlite.ViewGeneration) { r.ProvenanceCommitOID = "other" },
		"config":     func(r *store_sqlite.ViewGeneration) { r.ConfigHash = "other" },
		"extractors": func(r *store_sqlite.ViewGeneration) { r.ExtractorVersions = "other" },
		"resolver":   func(r *store_sqlite.ViewGeneration) { r.ResolverVersion = "other" },
		"deps":       func(r *store_sqlite.ViewGeneration) { r.DependencyRevision = "other" },
	} {
		changed := row
		change(&changed)
		if sameCommitLayerInputs(changed, identity) {
			t.Errorf("a layer with a different %s is shared", name)
		}
	}
}
