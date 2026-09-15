package indexer

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/graphview"
	"github.com/zzet/gortex/internal/viewmetrics"
)

// A routed dependent pins the committed base it was built against.
//
// These tests drive the COMMITTED regime, which is the one the pin applies to
// and the one no other test in this package has ever set up: the family's
// primary graph publishes real dedicated base generations through the real
// publication protocol (authority -> desire -> claim -> build -> adopt), so
// every dependent's commit layer names an immutable ancestor rather than
// composing over the shared corpus.
//
// What the pin buys is stated as a cost: a base advance under a dependent
// whose own tree did not move costs that dependent ZERO commit-layer builds,
// ZERO working-tree builds and zero route writes, however many times the base
// advances — and its view still equals a fresh isolated index of its own tree,
// which is the only oracle that can tell a pin from a splice.
//
// The legacy regime is the other half of the same contract and is pinned by
// dependent_recompose_test.go: there the base is the owner checkout's recorded
// tree, the corpus under the delta is rewritten in place, and a base advance
// MUST recompose. TestALegacyBaseAdvanceIsNeverPinned states the boundary from
// this side.

// committedBaseFixture is the coordinator fixture with the family's primary
// publishing real committed bases.
type committedBaseFixture struct {
	*coordinatorFixture
	publisher *dedicatedBasePublisher
	identity  store_sqlite.DedicatedBaseIdentity
	clock     int64

	// Optional per-fixture factory; each invocation owns a new registry.
	// Other tests retain the fully populated builder through the nil fallback.
	newBuilder func(*store_sqlite.Store) *SparseGenerationBuilder
}

// newCommittedBaseFixture publishes the family's first committed base from the
// primary's current tree and returns the fixture over it.
//
// Everything here is the production protocol. dedicatedBasePublisher is what
// the daemon's runtime drives (dedicated_base_advance.go ensureCurrent), the
// payload is built by BuildClaimedDedicatedBase over a real git tree source,
// and the adoption is the catalog's own transaction — which is also what moves
// the owner checkout's recorded head, so graphBase and the dependents' deltas
// cannot disagree about what the base is.
func newCommittedBaseFixture(t *testing.T) *committedBaseFixture {
	t.Helper()
	f := newUnpublishedCommittedBaseFixture(t)
	f.publishBase(t)
	return f
}

// newUnpublishedCommittedBaseFixture is newCommittedBaseFixture before its
// first publication: the authority is claimed and the publisher is wired, but
// the family's primary graph has published nothing, so every dependent is in
// the LEGACY regime graphBase's second arm serves.
//
// It is the state a family is now in for as long as nothing reads a committed
// base — the publication is deferred until a consumer exists — so the
// transition out of it (the first published base landing under a dependent
// that is already routed over generation 0) is a state the product reaches and
// has to be pinned. See
// TestADependentOnGenerationZeroRecomposesOntoTheFirstPublishedBase.
func newUnpublishedCommittedBaseFixture(t *testing.T) *committedBaseFixture {
	t.Helper()
	f := newCoordinatorFixture(t)
	ctx := context.Background()
	authority, err := f.catalog.AcquireDedicatedBaseAuthority(ctx, store_sqlite.AcquireDedicatedBaseAuthorityRequest{
		GraphID: f.graphID,
		Owner: store_sqlite.DedicatedBaseOwner{
			CheckoutID: f.primaryID, Incarnation: "incarnation-primary",
		},
		Token: "authority-pin",
	})
	if err != nil {
		t.Fatalf("acquire the dedicated base authority: %v", err)
	}
	out := &committedBaseFixture{
		coordinatorFixture: f,
		publisher: &dedicatedBasePublisher{
			runtime:   &dedicatedBaseRuntime{store: f.store},
			authority: authority,
		},
		identity: store_sqlite.DedicatedBaseIdentity{
			ConfigHash: "pin-config", ExtractorVersions: "pin-extractors",
			ResolverVersion: "pin-resolver",
		},
		clock: 1,
	}
	return out
}

func (f *committedBaseFixture) generationBuilder() *SparseGenerationBuilder {
	if f.newBuilder != nil {
		return f.newBuilder(f.store)
	}
	return builderNewBuilder(f.store)
}

// publishBase publishes and adopts one committed base for the primary's
// current HEAD tree, and returns the generation it installed.
func (f *committedBaseFixture) publishBase(t *testing.T) int64 {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	f.clock++
	result, err := f.publisher.ensureCurrent(ctx, f.leases, func(ctx context.Context) (dedicatedBaseObservation, error) {
		row, found, err := f.catalog.GetDedicatedGraph(ctx, f.graphID)
		if err != nil || !found {
			return dedicatedBaseObservation{}, fmt.Errorf("read the dedicated graph: found=%v err=%w", found, err)
		}
		identity := f.identity
		identity.TreeOID = builderGit(t, f.primary, "rev-parse", "HEAD^{tree}")
		return dedicatedBaseObservation{
			Identity:                   identity,
			ExpectedActiveGenerationID: row.ActiveGenerationID,
			RootPath:                   f.primary,
			WorkspaceID:                builderRepoPrefix,
			ProjectID:                  builderRepoPrefix,
			ProvenanceCommitOID:        builderGit(t, f.primary, "rev-parse", "HEAD"),
			CreatedAt:                  f.clock,
			Builder:                    *f.generationBuilder(),
		}, nil
	})
	if err != nil {
		t.Fatalf("publish a committed base: %v", err)
	}
	if result.Adoption.GenerationID != result.Claim.GenerationID || result.Claim.GenerationID <= 0 {
		t.Fatalf("the publication did not adopt what it built: %+v", result)
	}
	return result.Adoption.GenerationID
}

// advanceCommittedBase commits one new file on the primary and publishes the
// committed base for the tree that commit produced.
//
// One file, deliberately: it is what makes a splice observable. The dependents
// are on their own branches and never see this path, so a dependent's view
// that carries it is a view composed over a base the dependent's delta was not
// built against.
func (f *committedBaseFixture) advanceCommittedBase(t *testing.T, file, symbol string) (generationID int64, path string) {
	t.Helper()
	contents := "package fixture\n\nfunc " + symbol + "() {\n\tHelper()\n}\n"
	if err := os.WriteFile(filepath.Join(f.primary, file), []byte(contents), 0o644); err != nil {
		t.Fatalf("write %s in the primary: %v", file, err)
	}
	builderGit(t, f.primary, "add", "-A")
	builderGit(t, f.primary, "commit", "-m", "advance the committed base")
	return f.publishBase(t), file
}

// pinDependent adds one more real automatic checkout to the family, commits a
// file of its own on its own branch so its delta is non-empty, and returns an
// inert coordinator over it.
func (f *committedBaseFixture) pinDependent(t *testing.T, adminName string) (*CheckoutCoordinator, string) {
	t.Helper()
	root := filepath.Join(filepath.Dir(f.worktree), adminName)
	builderGit(t, f.primary, "worktree", "add", "-b", adminName, root)
	contents := "package fixture\n\nfunc " + adminName + "Only() {\n\tHelper()\n}\n"
	if err := os.WriteFile(filepath.Join(root, adminName+"_only.go"), []byte(contents), 0o644); err != nil {
		t.Fatalf("write %s's own file: %v", adminName, err)
	}
	builderGit(t, root, "add", "-A")
	builderGit(t, root, "commit", "-m", "the dependent's own commit")

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
		HeadTree:       builderGit(t, root, "rev-parse", "HEAD^{tree}"),
		LastAccessible: now,
		LastSeen:       now,
	}
	if err := f.catalog.AllocateCheckout(context.Background(), checkout); err != nil {
		t.Fatalf("allocate %s: %v", adminName, err)
	}
	coordinator, err := NewCheckoutCoordinator(CheckoutCoordinatorConfig{
		CheckoutID:     checkout.CheckoutID,
		CheckoutRoot:   root,
		FamilyID:       f.familyID,
		RepoPrefix:     builderRepoPrefix,
		WorkspaceID:    builderRepoPrefix,
		ProjectID:      builderRepoPrefix,
		Store:          f.store,
		Builder:        f.generationBuilder(),
		Leases:         f.leases,
		Config:         config.Default().Index,
		ConfigSections: dedicatedBaseConfigSections(config.Default()),
		Logger:         zap.NewNop(),
		PollInterval:   -1,
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

// routeOf reads one checkout's route.
func (f *committedBaseFixture) routeOf(t *testing.T, checkoutID string) store_sqlite.CheckoutRoute {
	t.Helper()
	route, found, err := f.catalog.GetCheckoutRoute(context.Background(), checkoutID)
	if err != nil || !found {
		t.Fatalf("read the route of %s: found=%v err=%v", checkoutID, found, err)
	}
	return route
}

// assertViewIsItsOwnTree is the gate-1 oracle for one dependent: the composed
// checkout view must be indistinguishable from a fresh isolated index of that
// checkout's own working tree.
//
// It is the only assertion that can tell a pin from a splice. A dependent
// serving its delta over the base the family has MOVED ON to would carry the
// primary's new paths — which its own tree does not have — and a dependent
// serving a delta rebuilt against a base it was not built over would be
// missing or doubling whatever the two bases differ by. Node counts cannot see
// either; the differential can.
func assertViewIsItsOwnTree(t *testing.T, f *committedBaseFixture, checkoutID, root string) {
	t.Helper()
	materializer := &graphview.Materializer{Store: f.store, Catalog: f.catalog, Leases: f.leases}
	view, err := materializer.MaterializeCheckout(context.Background(), checkoutID)
	if err != nil {
		t.Fatalf("MaterializeCheckout(%s): %v", checkoutID, err)
	}
	defer view.Close()

	flatStore := builderOpenStore(t, "oracle-"+filepath.Base(root))
	builderIndex(t, flatStore, root)
	builderAssertReadersAgree(t, view.Reader, flatStore.AtGeneration(0))
}

// TestACommittedBaseAdvanceCostsAPinnedDependentNothing is the pin's headline
// measurement, and the named verification for it: advance the
// family's committed base twenty times, with three dependent worktrees whose
// own trees never move, and every one of them keeps serving without building
// anything at all.
//
// Three things are asserted per advance per dependent, because a pin that got
// any one of them wrong would still look free:
//
//   - no commit-layer build and no working-tree build (the cycle's own
//     decisions) and no recomposition;
//   - the route is byte-for-byte the one the dependent already had, epoch
//     included — a re-route is a write, and this cycle makes none;
//   - the coordinator reports itself pinned to the generation it was built
//     against, which is what the retirement sweep reads.
//
// And once, at the end, the assertion none of the above can make: each
// dependent's composed view still equals a fresh isolated index of its own
// tree. Twenty advances of one new file each is twenty paths the pinned
// dependents must NOT have.
//
// Revert-red: drop the pinRoutedBase substitution from reconcile and every
// advance recomposes both layers of all three dependents — 120 builds where
// this test demands zero.
func TestACommittedBaseAdvanceCostsAPinnedDependentNothing(t *testing.T) {
	f := newCommittedBaseFixture(t)
	// builderTreeA, the three dependents and every advanced file are Go-only.
	// Keep the initial full-registry setup and independent oracle unchanged;
	// subsequent subject builders each own a fresh Go extractor.
	f.newBuilder = func(store *store_sqlite.Store) *SparseGenerationBuilder {
		return &SparseGenerationBuilder{
			Store: store, Registry: builderGoRegistry(),
			Config: config.Default().Index, Logger: zap.NewNop(),
		}
	}
	ctx := context.Background()

	type dependent struct {
		name        string
		coordinator *CheckoutCoordinator
		root        string
		route       store_sqlite.CheckoutRoute
		base        int64
	}
	dependents := []*dependent{{name: "alpha"}, {name: "beta"}, {name: "gamma"}}
	for _, d := range dependents {
		d.coordinator, d.root = f.pinDependent(t, d.name)
		first := d.coordinator.reconcile(ctx)
		if first.Err != nil {
			t.Fatalf("%s: first reconcile: %v", d.name, first.Err)
		}
		if !first.CommitBuilt || !first.DirtyBuilt {
			t.Fatalf("%s: the first cycle did not build the stack: %+v", d.name, first)
		}
		if first.BasePinned {
			t.Fatalf("%s: the first cycle claims a pin; it built over the base the family is ON", d.name)
		}
		d.route = f.routeOf(t, d.coordinator.checkoutID)
		commit, found := f.generation(d.route.CommitGenerationID)
		if !found || commit.BaseGenerationID <= 0 {
			t.Fatalf("%s: the routed delta names ancestor %d; the committed regime stamps a real one",
				d.name, commit.BaseGenerationID)
		}
		d.base = commit.BaseGenerationID
	}

	// The audit counts every payload and generation write in the store, so it
	// is read around the DEPENDENTS' cycles alone: the publication that
	// precedes them is the primary's own work and writes a generation by
	// definition. What this measures is the marginal cost of a base advance to
	// a dependent, which is the number the pin is about.
	writes := installCheckoutLayerWriteAudit(t, f.storePath)
	// The counter evidence, beside the cycle's own decisions and the write
	// audit: CoordinatorBuildSeconds is observed inside resolveCommitLayer and
	// buildDirtyLayerOver, so its per-slot count is how many layer builds the
	// dependents actually ran. (The cycle-outcome counters are not the
	// evidence here — recordCoordinatorCycle is cycle()'s, and these tests
	// drive reconcile directly so nothing would move them either way.)
	beforeMetrics := viewmetrics.Read()

	const advances = 20
	for i := 0; i < advances; i++ {
		f.advanceCommittedBase(t, fmt.Sprintf("advanced_%02d.go", i), fmt.Sprintf("Advanced%02d", i))
		before := writes(t)
		for _, d := range dependents {
			out := d.coordinator.reconcile(ctx)
			if out.Err != nil {
				t.Fatalf("%s: reconcile after advance %d: %v", d.name, i, out.Err)
			}
			if out.CommitBuilt || out.DirtyBuilt || out.CommitReused || out.DirtyReused || out.Recomposed {
				t.Fatalf("%s: advance %d cost it work: %+v", d.name, i, out)
			}
			if !out.BasePinned {
				t.Fatalf("%s: advance %d did not take the pin: %+v", d.name, i, out)
			}
			if got := f.routeOf(t, d.coordinator.checkoutID); got != d.route {
				t.Fatalf("%s: advance %d moved the route\n got: %+v\nwant: %+v", d.name, i, got, d.route)
			}
			if got := d.coordinator.PinnedBaseGeneration(); got != d.base {
				t.Fatalf("%s: advance %d reports pin %d, want the base it was built against %d",
					d.name, i, got, d.base)
			}
		}
		if counted := writes(t) - before; counted != 0 {
			t.Fatalf("advance %d cost the three dependents %d payload/generation writes, want none",
				i, counted)
		}
	}

	afterMetrics := viewmetrics.Read()
	for _, slot := range []string{viewmetrics.SlotCommit, viewmetrics.SlotDirty} {
		if got := buildSeconds(afterMetrics, slot) - buildSeconds(beforeMetrics, slot); got != 0 {
			t.Fatalf("twenty base advances ran %d %s-layer builds, want none", got, slot)
		}
	}

	// The oracle, per dependent: still exactly its own tree, twenty advances
	// later, with none of the primary's new paths in it.
	for _, d := range dependents {
		assertViewIsItsOwnTree(t, f, d.coordinator.checkoutID, d.root)
	}
}

// TestThePollSettlesAPinnedDependentWithoutTakingTheBuildLane pins the arm the
// headline measurement cannot reach by driving reconcile directly.
//
// settledWithoutBuild is the preflight every poll and every signal goes
// through, and it runs BEFORE the cycle lock and the build lane (see cycle()).
// A pin that only existed in reconcile would still be free of builds, but
// every base advance would queue all ten dependents behind the one build lane
// to discover that there was nothing to do. Settling here is what makes a base
// advance cost a woken dependent a sample and a few metadata reads.
//
// Revert-red: drop the pinRoutedBase call from settledWithoutBuild and the
// preflight reports unsettled — the identity re-keys against the base the
// family has moved to, which is not the one the route names.
func TestThePollSettlesAPinnedDependentWithoutTakingTheBuildLane(t *testing.T) {
	f := newCommittedBaseFixture(t)
	ctx := context.Background()
	c, _ := f.pinDependent(t, "alpha")

	if first := c.reconcile(ctx); first.Err != nil || !first.CommitBuilt {
		t.Fatalf("the first cycle did not build the stack: %+v", first)
	}
	before := f.routeOf(t, c.checkoutID)

	f.advanceCommittedBase(t, "advanced.go", "Advanced")

	out, settled := c.settledWithoutBuild(ctx)
	if !settled {
		t.Fatalf("the poll did not settle a pinned dependent: %+v", out)
	}
	if !out.BasePinned {
		t.Fatalf("the poll settled without reporting the pin: %+v", out)
	}
	if out.CommitGenerationID != before.CommitGenerationID || out.DirtyGenerationID != before.DirtyGenerationID {
		t.Fatalf("the poll settled on (%d, %d), want the routed pair (%d, %d)",
			out.CommitGenerationID, out.DirtyGenerationID,
			before.CommitGenerationID, before.DirtyGenerationID)
	}
	if got := f.routeOf(t, c.checkoutID); got != before {
		t.Fatalf("the preflight moved the route\n got: %+v\nwant: %+v", got, before)
	}
}

// TestAPinnedDependentRebuildsItsOwnCommitAgainstThePinnedBase is the fourth
// clause of the contract: the pin is about the BASE, not about the checkout.
//
// A dependent that commits on its own branch has a new tree and therefore a
// new delta to build — one bounded delta, diffed from the base it is pinned
// to, not from the base the family has moved on to. Building it against the
// current base instead would be correct too, but it would throw the pin away
// on the checkout's first commit and re-key every layer the reuse cache holds;
// the whole saving lasts exactly as long as the base does.
//
// Revert-red: pin the base only when the routed row's tree equals the sample's
// (substitute head.HeadTree for commitRow.TreeOID in pinnedBaseFor's probe)
// and the rebuilt delta names the family's current base instead.
func TestAPinnedDependentRebuildsItsOwnCommitAgainstThePinnedBase(t *testing.T) {
	f := newCommittedBaseFixture(t)
	ctx := context.Background()
	c, root := f.pinDependent(t, "alpha")

	if first := c.reconcile(ctx); first.Err != nil || !first.CommitBuilt {
		t.Fatalf("the first cycle did not build the stack: %+v", first)
	}
	pinnedBase, found := f.generation(f.routeOf(t, c.checkoutID).CommitGenerationID)
	if !found {
		t.Fatal("the routed commit generation is not in the catalog")
	}
	base := pinnedBase.BaseGenerationID

	advanced, advancedPath := f.advanceCommittedBase(t, "advanced.go", "Advanced")
	if advanced == base {
		t.Fatal("the advance published no new generation")
	}

	// The dependent now commits a file of its own.
	own := "package fixture\n\nfunc AlphaSecond() {\n\tHelper()\n}\n"
	if err := os.WriteFile(filepath.Join(root, "alpha_second.go"), []byte(own), 0o644); err != nil {
		t.Fatalf("write the dependent's second file: %v", err)
	}
	builderGit(t, root, "add", "-A")
	builderGit(t, root, "commit", "-m", "the dependent's second commit")

	out := c.reconcile(ctx)
	if out.Err != nil {
		t.Fatalf("reconcile after the checkout's own commit: %v", out.Err)
	}
	if !out.CommitBuilt {
		t.Fatalf("the checkout's own commit did not rebuild its delta: %+v", out)
	}
	if !out.BasePinned {
		t.Fatalf("the rebuild left the pin: %+v", out)
	}
	rebuilt, found := f.generation(out.CommitGenerationID)
	if !found {
		t.Fatalf("the rebuilt commit generation %d is not in the catalog", out.CommitGenerationID)
	}
	if rebuilt.BaseGenerationID != base {
		t.Fatalf("the rebuilt delta names base %d, want the pinned %d (the family is on %d)",
			rebuilt.BaseGenerationID, base, advanced)
	}
	if rebuilt.LowerViewFingerprint != pinnedBase.LowerViewFingerprint {
		t.Fatalf("the rebuilt delta was diffed from %q, want the pinned base tree %q",
			rebuilt.LowerViewFingerprint, pinnedBase.LowerViewFingerprint)
	}
	// Bounded: the delta claims the path the two trees differ by, and the
	// primary's advance — which this checkout has never had — is not in it.
	claimed := claimedPaths(t, f.store, out.CommitGenerationID)
	for _, path := range claimed {
		if path == builderRepoPrefix+"/"+advancedPath {
			t.Fatalf("the rebuilt delta claims the primary's %q; it was diffed from the wrong base", path)
		}
	}
	assertViewIsItsOwnTree(t, f, c.checkoutID, root)
}

// TestReleasingThePinRecomposesOnceBeforeTheBaseIsRetired is the bound.
//
// The pin is indefinite by design — the dependent's own delta is what keeps
// its base servable, and the catalog refuses to retire a generation another
// generation is based on — so something has to be able to ask for it back.
// That something is the retirement sweep, through RequestBaseRelease, and what
// it gets is ONE bounded recomposition: the replacement stack is built
// off-route and installed in a single compare-and-set, the old pair serves for
// the whole of it, and only then does the reference on the old base disappear.
// The cycle after that settles again, pinned to nothing, with no further work.
//
// Revert-red: drop the release check from pinnedBaseFor and the release is
// ignored — the dependent re-pins the base it was asked to give up and the
// recomposition never happens.
func TestReleasingThePinRecomposesOnceBeforeTheBaseIsRetired(t *testing.T) {
	f := newCommittedBaseFixture(t)
	ctx := context.Background()
	c, root := f.pinDependent(t, "alpha")

	if first := c.reconcile(ctx); first.Err != nil || !first.CommitBuilt {
		t.Fatalf("the first cycle did not build the stack: %+v", first)
	}
	before := f.routeOf(t, c.checkoutID)
	commit, found := f.generation(before.CommitGenerationID)
	if !found {
		t.Fatal("the routed commit generation is not in the catalog")
	}
	base := commit.BaseGenerationID

	advanced, _ := f.advanceCommittedBase(t, "advanced.go", "Advanced")
	if pinned := c.reconcile(ctx); pinned.Err != nil || !pinned.BasePinned || pinned.CommitBuilt {
		t.Fatalf("the advance was not absorbed by the pin: %+v", pinned)
	}

	// While the pin holds, the base cannot be retired: the routed delta names
	// it, and that is the reference the catalog refuses on.
	if err := f.store.RetirePayloadGeneration(ctx, base, nil); err == nil {
		t.Fatal("the pinned base retired while a dependent's route was composed over it")
	}

	if !c.RequestBaseRelease(base, "the sweep wants the base back") {
		t.Fatal("the release request was not accepted")
	}
	if again := c.RequestBaseRelease(base, "the same sweep, an hour later"); again {
		t.Fatal("the same release was accepted twice; an hourly sweep would re-signal it forever")
	}

	out := c.reconcile(ctx)
	if out.Err != nil {
		t.Fatalf("reconcile after the release request: %v", out.Err)
	}
	if !out.Recomposed {
		t.Fatalf("the released pin did not recompose: %+v", out)
	}
	if out.BasePinned {
		t.Fatalf("the recomposition claims a pin it was asked to give up: %+v", out)
	}
	after := f.routeOf(t, c.checkoutID)
	if after.CommitGenerationID == before.CommitGenerationID {
		t.Fatal("the recomposition left the old delta routed")
	}
	recomposed, found := f.generation(after.CommitGenerationID)
	if !found || recomposed.BaseGenerationID != advanced {
		t.Fatalf("the recomposed delta names base %d, want the family's current %d", recomposed.BaseGenerationID, advanced)
	}
	if c.PinnedBaseGeneration() != 0 {
		t.Fatalf("the coordinator still reports pin %d after recomposing onto the current base",
			c.PinnedBaseGeneration())
	}

	// Exactly once: the next cycle has nothing left to do.
	settled := c.reconcile(ctx)
	if settled.Err != nil {
		t.Fatalf("the cycle after the recomposition: %v", settled.Err)
	}
	if settled.CommitBuilt || settled.DirtyBuilt || settled.Recomposed {
		t.Fatalf("the release cost a second recomposition: %+v", settled)
	}
	assertViewIsItsOwnTree(t, f, c.checkoutID, root)

	// And the old base is collectable now — once the reuse cache that is
	// holding the replaced delta lets go of it. The route no longer names it,
	// which is the half this item owns.
	if route := f.routeOf(t, c.checkoutID); route.CommitGenerationID == before.CommitGenerationID {
		t.Fatal("the old delta is still routed, so its base can never retire")
	}
}

// pinningCoordinator is a live coordinator that reports itself pinned to one
// committed base and records what it is asked to release.
//
// It is the fan-out tests' coordinator — a real loop over a checkout identity,
// every cycle settled by the preflight — with the pin its last cycle would
// have recorded set through the same method the production path uses. What the
// retirement sweep asks a live coordinator is exactly two questions: what is
// your route composed over, and please let it go. Both are answered here by
// the production methods, over the registry the lifecycle really fills.
func pinningCoordinator(t *testing.T, checkoutID, familyID string, generationID int64) *CheckoutCoordinator {
	t.Helper()
	c := newFanoutCoordinator(t, checkoutID, familyID).coordinator
	c.notePinnedBase(generationID)
	return c
}

// retirementSweepFixture publishes four committed bases on one graph, which
// puts the oldest outside the default two-chain retention window and therefore
// in front of the retirement sweep's decision.
func retirementSweepFixture(t *testing.T) (*fanoutFixture, []int64) {
	t.Helper()
	f := newFanoutFixture(t)
	var generations []int64
	active := int64(0)
	for i, tree := range []string{"tree-a", "tree-b", "tree-c", "tree-d"} {
		adoption := f.publish(t, tree, "commit-"+tree, int64(1000+i), active)
		if adoption.GenerationID <= 0 {
			t.Fatalf("publication %d adopted nothing", i)
		}
		generations = append(generations, adoption.GenerationID)
		active = adoption.GenerationID
	}
	return f, generations
}

func sweptGenerationState(t *testing.T, f *fanoutFixture, generationID int64) (store_sqlite.ViewGenerationState, bool) {
	t.Helper()
	row, found, err := f.catalog.GetViewGeneration(context.Background(), generationID)
	if err != nil {
		t.Fatalf("read generation %d: %v", generationID, err)
	}
	return row.State, found
}

// TestTheRetirementSweepOffersAReplacedBaseNothingIsHolding is the control arm
// of the pin's retention half: without a dependent composed over it, a base
// past the retention window is offered and collected exactly as before.
//
// It exists so the next test cannot pass by accident. "The sweep did not
// retire the pinned base" means nothing unless the same sweep, on the same
// shape, retires the unpinned one.
func TestTheRetirementSweepOffersAReplacedBaseNothingIsHolding(t *testing.T) {
	f, generations := retirementSweepFixture(t)
	oldest := generations[0]

	f.lifecycle.sweepRetirements(context.Background())

	state, found := sweptGenerationState(t, f, oldest)
	if found && state != store_sqlite.ViewGenerationRetiring {
		t.Fatalf("the sweep left the oldest replaced base in %q; nothing was holding it", state)
	}
}

// TestTheRetirementSweepKeepsAPinnedBaseAndAsksForItBack is the pin's
// retention half, driven through the production entrypoint.
//
// A dependent that stays on the base it was built against holds that base
// servable — its own delta is the catalog reference that refuses the
// retirement — so the sweep that keeps offering it would be refused on every
// pass forever and would never collect the payload either. The sweep honours
// the pin instead: it retains the generation like the live chain, and it asks
// the holders to recompose off it so a later pass can finally have it.
//
// The whole path is production: sweepRetirements snapshots the live
// coordinator registry the lifecycle itself fills (installCoordinatorAtHead),
// asks each one what its route is composed over (PinnedBaseGeneration), and
// dispatches RequestBaseRelease outside its own lock.
//
// Revert-red: delete the pins.pinned arm from
// dedicatedGraphRetirementCandidates and the pinned base is offered and
// retired under a live dependent, with nobody asked for anything.
func TestTheRetirementSweepKeepsAPinnedBaseAndAsksForItBack(t *testing.T) {
	f, generations := retirementSweepFixture(t)
	oldest := generations[0]

	dependent := f.dependent("checkout-pinned", "pinned")
	holder := pinningCoordinator(t, dependent.CheckoutID, f.familyID, oldest)
	if !f.lifecycle.installCoordinatorAtHead(store_sqlite.Checkout{
		CheckoutID:    dependent.CheckoutID,
		EffectiveMode: store_sqlite.CheckoutModeAutomatic,
	}, holder) {
		t.Fatal("the coordinator was not installed in the lifecycle's registry")
	}

	f.lifecycle.sweepRetirements(context.Background())

	state, found := sweptGenerationState(t, f, oldest)
	if !found {
		t.Fatal("the sweep collected a base a live dependent is composed over")
	}
	if state == store_sqlite.ViewGenerationRetiring {
		t.Fatalf("the sweep fenced a base a live dependent is composed over: state %q", state)
	}
	if got := holder.releaseRequestedBasePin(); got != oldest {
		t.Fatalf("the sweep asked the holder to release %d, want the pinned base %d", got, oldest)
	}
	// The newer chains the window covers were never the question: the sweep
	// must not have asked for one of them instead.
	for _, generationID := range generations[1:] {
		if _, found := sweptGenerationState(t, f, generationID); !found {
			t.Fatalf("the sweep collected generation %d, which the retention window covers", generationID)
		}
	}
}

// TestALegacyBaseAdvanceIsNeverPinned states the two-regime boundary from the
// pin's side.
//
// The coordinator fixture without a published base is the legacy regime: the
// base is the owner checkout's recorded tree, the routed delta names ancestor
// zero, and what it composes over is the shared corpus — which is rewritten in
// place as the primary moves. There is nothing immutable to stay on, so
// pinRoutedBase must refuse, and the base advance must still recompose.
//
// Revert-red: drop the generationID <= 0 clause from pinnedBaseFor and this
// test fails at the pin, not at the recomposition — which is the bug the
// clause exists to prevent, since the delta would then keep serving the paths
// the two bases differ by from a base that has been rewritten under it.
func TestALegacyBaseAdvanceIsNeverPinned(t *testing.T) {
	f := newCoordinatorFixture(t)
	c := f.inertCoordinator(t, CheckoutCoordinatorConfig{})
	ctx := context.Background()

	first := coordinatorReconcile(t, c)
	if !first.CommitBuilt || !first.DirtyBuilt {
		t.Fatalf("the first cycle did not build the stack: %+v", first)
	}
	base, err := c.primaryBase(ctx)
	if err != nil {
		t.Fatalf("primaryBase: %v", err)
	}
	if base.generationID != 0 {
		t.Fatalf("the fixture published generation %d; this test is about the regime with none",
			base.generationID)
	}

	advancePrimaryBase(t, f)
	out := coordinatorReconcile(t, c)
	if out.BasePinned {
		t.Fatalf("a base with no published generation was pinned: %+v", out)
	}
	if !out.Recomposed {
		t.Fatalf("the legacy regime stopped recomposing on a base advance: %+v", out)
	}

	// And the predicate itself, directly, on the state the cycle left: the
	// route is complete and active, and the refusal is the regime.
	if _, pinned := c.pinRoutedBase(ctx, base, f.route()); pinned {
		t.Fatal("pinRoutedBase accepted a base that names no generation")
	}
}

// --- the two refusals inside pinnedBaseFor that nothing was holding --------
//
// The tests above pin what the pin BUYS. These pin what it must refuse, and
// they exist because the pin's own verification found both refusals survived
// deletion with the whole suite green: the substituted-identity probe
// (checkout_coordinator.go:1877-1882) and the pinned base row's own
// consistency clauses (:1887-1896).
//
// Neither is defensive dressing. The probe is the ONLY thing that distinguishes
// "the base moved and nothing else did" — which the pin absorbs for free — from
// "the base moved and so did something the payload is a function of", which is
// a semantic invalidation and must go to a rebuild against the base the family
// is on NOW. Without it a configuration reload, an extractor upgrade, a
// resolver-contract bump or a dependency-cohort move under a pinned dependent
// would rebuild the delta against the base it happened to be pinned to,
// carrying the pin — and the divergence it stands for — across the very event
// that was supposed to end it.
//
// Production is unchanged by this item: both refusals are present and correct.
// What was missing was the test that goes red when they are not.

// pinnedDependent is the shared setup for the refusal tests: one committed-base
// family, one dependent worktree with its own commit and its own built stack,
// and then one base advance under it.
//
// It returns the dependent's coordinator and working tree, the generation its
// routed delta was built against (the pin candidate), the family's CURRENT base
// after the advance, and the dependent's route. The two generations are always
// different — the fixture asserts it — so a test that cannot tell them apart is
// failing for the reason it names rather than for a fixture that never moved.
func pinnedDependent(t *testing.T, f *committedBaseFixture) (
	c *CheckoutCoordinator, root string, builtAgainst int64,
	current primaryBase, route store_sqlite.CheckoutRoute,
) {
	t.Helper()
	ctx := context.Background()
	c, root = f.pinDependent(t, "alpha")
	if first := c.reconcile(ctx); first.Err != nil || !first.CommitBuilt {
		t.Fatalf("the first cycle did not build the stack: %+v", first)
	}
	route = f.routeOf(t, c.checkoutID)
	routed, found := f.generation(route.CommitGenerationID)
	if !found {
		t.Fatal("the routed commit generation is not in the catalog")
	}
	if routed.BaseGenerationID <= 0 {
		t.Fatalf("the routed delta names ancestor %d; this is the committed regime",
			routed.BaseGenerationID)
	}
	builtAgainst = routed.BaseGenerationID

	f.advanceCommittedBase(t, "advanced.go", "Advanced")
	current, err := c.primaryBase(ctx)
	if err != nil {
		t.Fatalf("primaryBase after the advance: %v", err)
	}
	if current.generationID == builtAgainst {
		t.Fatalf("the advance left the family on generation %d; there is nothing to pin AGAINST",
			current.generationID)
	}
	return c, root, builtAgainst, current, route
}

// assertPinsTheBaseItWasBuiltAgainst is the control every refusal arm is read
// against. A refusal proves nothing unless the same predicate, on the same
// state with the doctoring undone, takes the pin.
func assertPinsTheBaseItWasBuiltAgainst(
	t *testing.T, c *CheckoutCoordinator, current primaryBase,
	route store_sqlite.CheckoutRoute, builtAgainst int64, when string,
) {
	t.Helper()
	got, pinned := c.pinRoutedBase(context.Background(), current, route)
	if !pinned {
		t.Fatalf("%s: the pin was refused on an unchanged identity", when)
	}
	if got.generationID != builtAgainst {
		t.Fatalf("%s: the pin took generation %d, want the base the delta was built against %d",
			when, got.generationID, builtAgainst)
	}
}

// assertRefusesThePin is the arm itself.
func assertRefusesThePin(
	t *testing.T, c *CheckoutCoordinator, current primaryBase,
	route store_sqlite.CheckoutRoute, because string,
) {
	t.Helper()
	got, pinned := c.pinRoutedBase(context.Background(), current, route)
	if pinned {
		t.Fatalf("%s: the pin was taken anyway, on generation %d", because, got.generationID)
	}
	if got.generationID != current.generationID || got.treeOID != current.treeOID || got.pinned {
		t.Fatalf("%s: the refusal did not return the family's current base unchanged: %+v want %+v",
			because, got, current)
	}
	if reported := c.PinnedBaseGeneration(); reported != 0 {
		t.Fatalf("%s: the coordinator still reports pin %d after refusing", because, reported)
	}
}

// TestASemanticChangeRefusesToPinTheBaseItWasBuiltAgainst is the substituted-
// identity probe, one identity input at a time.
//
// pinnedBaseFor re-renders the commit identity this coordinator would mint NOW
// over the routed row's own tree, substitutes ONLY the base the row names, and
// requires the result to be the key the row already carries. So every field of
// the identity that is not the base is a refusal: the payload is a function of
// the configuration, the extractor set, the resolver contract and the
// dependency cohort, and a pin that ignored any of them would keep serving a
// payload built under inputs that no longer hold.
//
// Revert-red: drop the probe (checkout_coordinator.go:1877-1882) and all four
// arms take the pin, with the control still green — which is exactly the state
// the pin's own verification found the suite in.
func TestASemanticChangeRefusesToPinTheBaseItWasBuiltAgainst(t *testing.T) {
	f := newCommittedBaseFixture(t)
	c, _, builtAgainst, current, route := pinnedDependent(t, f)

	readRevision := func() string {
		c.revisionMu.RLock()
		defer c.revisionMu.RUnlock()
		return c.revision
	}
	writeRevision := func(revision string) {
		c.revisionMu.Lock()
		c.revision = revision
		c.revisionMu.Unlock()
	}

	assertPinsTheBaseItWasBuiltAgainst(t, c, current, route, builtAgainst, "before any semantic change")

	for _, arm := range []struct {
		name   string
		change func() func()
	}{
		{"the index configuration was reloaded", func() func() {
			was := c.configHash
			c.configHash = was + "-reloaded"
			return func() { c.configHash = was }
		}},
		{"an extractor version moved", func() func() {
			was := c.extractors
			c.extractors = was + "-upgraded"
			return func() { c.extractors = was }
		}},
		{"the resolver contract moved", func() func() {
			was := c.resolverVersion
			c.resolverVersion = was + "-upgraded"
			return func() { c.resolverVersion = was }
		}},
		{"the dependency cohort moved", func() func() {
			was := readRevision()
			writeRevision("cohort-moved-" + was)
			return func() { writeRevision(was) }
		}},
	} {
		t.Run(arm.name, func(t *testing.T) {
			restore := arm.change()
			defer restore()
			assertRefusesThePin(t, c, current, route, arm.name)
		})
		assertPinsTheBaseItWasBuiltAgainst(t, c, current, route, builtAgainst,
			"after undoing: "+arm.name)
	}
}

// TestASemanticChangeUnderAPinRebuildsAgainstTheCurrentBase is the same refusal
// read as a cycle outcome, which is where it actually costs something.
//
// A configuration reload under a pinned dependent is the shape: the routed
// delta is no longer the payload this coordinator would build, so the cycle
// rebuilds it — and the base it must be diffed from is the one the family is on
// NOW, not the one the route happened to be pinned to. Building against the pin
// instead would be a strictly worse answer than the behaviour before the pin:
// the delta would be re-minted under the new inputs and immediately pinned to a
// base the family left, with no event left that would ever move it off.
//
// Revert-red: drop the probe and the rebuilt delta names the pinned base
// (`builtAgainst`) instead of the family's current one.
func TestASemanticChangeUnderAPinRebuildsAgainstTheCurrentBase(t *testing.T) {
	f := newCommittedBaseFixture(t)
	ctx := context.Background()
	c, root, builtAgainst, current, route := pinnedDependent(t, f)

	// The pin holds first, so the rebuild below is attributable to the config
	// change and to nothing else about the advance.
	if pinned := c.reconcile(ctx); pinned.Err != nil || !pinned.BasePinned || pinned.CommitBuilt {
		t.Fatalf("the advance was not absorbed by the pin: %+v", pinned)
	}
	if got := f.routeOf(t, c.checkoutID); got != route {
		t.Fatalf("the pinned cycle moved the route\n got: %+v\nwant: %+v", got, route)
	}

	reloaded := c.configHash + "-reloaded"
	c.configHash = reloaded

	out := c.reconcile(ctx)
	if out.Err != nil {
		t.Fatalf("reconcile after the configuration reload: %v", out.Err)
	}
	if out.BasePinned {
		t.Fatalf("a configuration reload kept the pin: %+v", out)
	}
	after := f.routeOf(t, c.checkoutID)
	if after.CommitGenerationID == route.CommitGenerationID {
		t.Fatalf("the reload left the stale delta routed: %+v", out)
	}
	rebuilt, found := f.generation(after.CommitGenerationID)
	if !found {
		t.Fatalf("the rebuilt commit generation %d is not in the catalog", after.CommitGenerationID)
	}
	if rebuilt.ConfigHash != reloaded {
		t.Fatalf("the rebuilt delta carries config hash %q, want the reloaded %q",
			rebuilt.ConfigHash, reloaded)
	}
	if rebuilt.BaseGenerationID != current.generationID {
		t.Fatalf("the rebuilt delta names base %d, want the family's current %d (it was pinned to %d)",
			rebuilt.BaseGenerationID, current.generationID, builtAgainst)
	}
	if rebuilt.LowerViewFingerprint != current.treeOID {
		t.Fatalf("the rebuilt delta was diffed from %q, want the family's current base tree %q",
			rebuilt.LowerViewFingerprint, current.treeOID)
	}
	if reported := c.PinnedBaseGeneration(); reported != 0 {
		t.Fatalf("the coordinator reports pin %d after rebuilding onto the current base", reported)
	}
	// And the answer is still this checkout's own tree, which is the only
	// oracle that can tell a correct rebase of the delta from a splice.
	assertViewIsItsOwnTree(t, f, c.checkoutID, root)
}

// doctoredGenerationColumns are the only columns the refusal tests below
// overwrite. The list is closed so the column name can be interpolated into the
// statement without a second thought about what it could be.
var doctoredGenerationColumns = map[string]struct{}{
	"state":                  {},
	"generation_kind":        {},
	"graph_id":               {},
	"tree_oid":               {},
	"lower_view_fingerprint": {},
}

// doctorGenerationColumn overwrites one column of one view_generations row on
// the fixture's own store file and returns the undo.
//
// It writes the store directly for the reason installCheckoutLayerWriteAudit
// does — instrumenting a test-owned database beats exporting a production-only
// hook — and for a second reason of its own: the states it has to produce are
// states the catalog's own API refuses to put a LIVE base into, because the
// routed delta references it. That refusal is the production guard (and its own
// test); what is under test here is the coordinator's behaviour when it reads a
// base row that is not the one its delta was diffed from, whatever put it
// there — a torn retirement, a graph reset, a restore over a moved store.
func doctorGenerationColumn(
	t *testing.T, storePath string, generationID int64, column, value string,
) func() {
	t.Helper()
	if _, ok := doctoredGenerationColumns[column]; !ok {
		t.Fatalf("doctorGenerationColumn: %q is not one of the columns these tests doctor", column)
	}
	read := fmt.Sprintf("SELECT %s FROM view_generations WHERE generation_id = ?", column)
	write := fmt.Sprintf("UPDATE view_generations SET %s = ? WHERE generation_id = ?", column)
	exec := func(t *testing.T, statement, argument string) {
		t.Helper()
		db, err := sql.Open("sqlite", storePath+"?_pragma=busy_timeout(5000)")
		if err != nil {
			t.Fatalf("open the store to doctor %s: %v", column, err)
		}
		defer db.Close()
		if _, err := db.Exec(statement, argument, generationID); err != nil {
			t.Fatalf("doctor %s of generation %d: %v", column, generationID, err)
		}
	}

	db, err := sql.Open("sqlite", storePath+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatalf("open the store to read %s: %v", column, err)
	}
	var previous string
	err = db.QueryRow(read, generationID).Scan(&previous)
	_ = db.Close()
	if err != nil {
		t.Fatalf("read %s of generation %d: %v", column, generationID, err)
	}
	exec(t, write, value)
	return func() { exec(t, write, previous) }
}

// TestThePinRefusesABaseRowThatIsNotTheOneTheDeltaWasDiffedFrom pins the pinned
// base row's own consistency clauses.
//
// Staying on a replaced base is only safe because that base is still exactly
// the corpus the routed delta was diffed from: B1 + diffTreeChanges(B1, T) = T
// holds for B1 and for nothing else. So before the cycle composes over it,
// pinnedBaseFor asks the row five questions — is it still servable, is it a
// dedicated base at all, is it this graph's, does it name a tree at all, and is
// that tree the one the delta names as its lower view — and any answer but yes
// sends the cycle to the base the family is on, which is the behaviour this
// path replaced.
//
// Each arm doctors exactly one column and puts it back, with the control run
// between arms, so a refusal is attributable to the clause it names.
//
// Revert-red: drop the clauses (checkout_coordinator.go:1887-1896) and every
// arm takes the pin over a base that is retiring, is somebody else's, is not a
// base at all, or is not the tree the delta was diffed from. Dropping any one
// of the five reddens exactly the arm that names it.
func TestThePinRefusesABaseRowThatIsNotTheOneTheDeltaWasDiffedFrom(t *testing.T) {
	f := newCommittedBaseFixture(t)
	c, _, builtAgainst, current, route := pinnedDependent(t, f)
	routed, found := f.generation(route.CommitGenerationID)
	if !found {
		t.Fatal("the routed commit generation is not in the catalog")
	}

	assertPinsTheBaseItWasBuiltAgainst(t, c, current, route, builtAgainst, "before any doctoring")

	for _, arm := range []struct {
		name   string
		doctor func(t *testing.T) func()
	}{
		{"the pinned base is already retiring", func(t *testing.T) func() {
			return doctorGenerationColumn(t, f.storePath, builtAgainst,
				"state", string(store_sqlite.ViewGenerationRetiring))
		}},
		{"the pinned base is not a dedicated base", func(t *testing.T) func() {
			return doctorGenerationColumn(t, f.storePath, builtAgainst,
				"generation_kind", CommitLayerGenerationKind)
		}},
		{"the pinned base belongs to another graph", func(t *testing.T) func() {
			return doctorGenerationColumn(t, f.storePath, builtAgainst,
				"graph_id", current.graphID+"-elsewhere")
		}},
		{"the pinned base is not the tree the delta was diffed from", func(t *testing.T) func() {
			return doctorGenerationColumn(t, f.storePath, builtAgainst,
				"tree_oid", routed.LowerViewFingerprint+"-moved")
		}},
		{"neither the pinned base nor the delta names a tree at all", func(t *testing.T) func() {
			// The empty-tree clause is the one the inequality above cannot
			// stand in for: with both sides empty they are equal, and what
			// refuses is the base naming no tree. A delta whose lower view
			// fingerprint is empty still re-keys clean, because the probe
			// substitutes that very field from the row.
			undoBase := doctorGenerationColumn(t, f.storePath, builtAgainst, "tree_oid", "")
			undoDelta := doctorGenerationColumn(t, f.storePath, route.CommitGenerationID,
				"lower_view_fingerprint", "")
			return func() { undoDelta(); undoBase() }
		}},
	} {
		t.Run(arm.name, func(t *testing.T) {
			undo := arm.doctor(t)
			defer undo()
			assertRefusesThePin(t, c, current, route, arm.name)
		})
		assertPinsTheBaseItWasBuiltAgainst(t, c, current, route, builtAgainst,
			"after undoing: "+arm.name)
	}
}
