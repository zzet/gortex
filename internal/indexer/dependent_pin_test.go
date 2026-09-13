package indexer

import (
	"context"
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

// W4.8 — a routed dependent pins the committed base it was built against.
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
	out.publishBase(t)
	return out
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
			Builder:                    *builderNewBuilder(f.store),
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
		Builder:        builderNewBuilder(f.store),
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

// TestACommittedBaseAdvanceCostsAPinnedDependentNothing is W4.8's headline
// measurement, and the plan's own verification for the item: advance the
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
	// a dependent, which is the number W4.8 is about.
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

// TestTheRetirementSweepKeepsAPinnedBaseAndAsksForItBack is W4.8's retention
// half, driven through the production entrypoint.
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
