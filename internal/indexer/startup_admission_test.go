package indexer

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/graphview"
	"github.com/zzet/gortex/internal/reconcile"
	"github.com/zzet/gortex/internal/search"
)

// Startup admission: a worktree whose own committed base is published must be
// served while an UNRELATED family is still warming up.
//
// The defect these tests pin is the single global view-build latch standing in
// for two different decisions. During daemon startup the latch stays closed
// until every scheduled family's initial base has been joined
// (joinKnownStartupBases), and a checkout coordinator with a closed latch
// defers its whole cycle — it builds nothing and routes view_building. So a
// worktree in family A, whose base is already published and servable, waits for
// family B's unrelated publication to finish before it can build its own
// (zero-file) commit layer. The historical trace is two same-tree worktrees
// held ~105s behind an unrelated frontend base.
//
// The ordering these tests demand: readiness is PER FAMILY. A family whose
// committed base is published is admitted even while the global latch is closed
// and another family warms; a family whose base is not published keeps the
// protection. The publisher itself is lane-independent, so admitting a
// dependent never starves the publication that warms the rest.
//
// Everything is real: two git families in one store, real corpora, a real
// claimed-base publication for A through the catalog's own protocol, and the
// production coordinator loop. B's startup publication is held behind a channel
// barrier, and the global latch opens only when B releases — the warmup tail
// after the join. No elapsed-time sleep decides anything; the quiet window runs
// on synctest's fake clock.

// startupFamily is one repository family in the shared store.
type startupFamily struct {
	prefix      string
	familyID    string
	graphID     string
	primaryID   string
	checkoutID  string
	incarnation string
	primary     string
	worktree    string
	treeA       string
	commitA     string
}

type startupAdmissionFixture struct {
	t       testing.TB
	store   *store_sqlite.Store
	catalog *store_sqlite.Catalog
	leases  *graphview.LeaseManager
	a, b    startupFamily
}

func newStartupAdmissionFixture(t testing.TB) *startupAdmissionFixture {
	t.Helper()
	builderIsolateGit(t)
	store := builderOpenStoreAt(t, filepath.Join(t.TempDir(), "base.sqlite"))
	t.Cleanup(func() { _ = store.Close() })
	leases := graphview.NewLeaseManager()
	f := &startupAdmissionFixture{
		t: t, store: store, catalog: store.Catalog(), leases: leases,
		a: buildStartupFamily(t, store, leases, "a", builderRepoPrefix),
		b: buildStartupFamily(t, store, leases, "b", "repo-b"),
	}
	return f
}

// buildStartupFamily builds one real family: a primary committed at tree A, a
// linked worktree at the same tree, the corpus indexed at the family's own
// prefix, and the catalog identity a coordinator needs to exist.
func buildStartupFamily(t testing.TB, store *store_sqlite.Store, leases *graphview.LeaseManager, suffix, prefix string) startupFamily {
	t.Helper()
	family := builderTempDir(t, "family-"+suffix)
	primary := filepath.Join(family, "primary")
	if err := os.MkdirAll(primary, 0o755); err != nil {
		t.Fatalf("mkdir primary: %v", err)
	}
	builderGit(t, primary, "init", "--initial-branch=main")
	builderWriteTree(t, primary, builderTreeA())
	builderGit(t, primary, "add", "-A")
	builderGit(t, primary, "commit", "-m", "A")
	treeA := builderGit(t, primary, "rev-parse", "HEAD^{tree}")
	commitA := builderGit(t, primary, "rev-parse", "HEAD")
	worktree := filepath.Join(family, "worktree")
	builderGit(t, primary, "worktree", "add", "-b", "feature", worktree)

	idx := New(store, builderRegistry(), config.Default().Index, zap.NewNop())
	idx.SetRepoPrefix(prefix)
	idx.SetWorkspaceID(prefix)
	idx.SetProjectID(prefix)
	if _, err := idx.Index(primary); err != nil {
		t.Fatalf("index %s: %v", primary, err)
	}
	idx.Close()

	fam := startupFamily{
		prefix: prefix, familyID: "family-" + suffix, graphID: GraphIDFor(prefix),
		primaryID: "checkout-primary-" + suffix, checkoutID: "checkout-worktree-" + suffix,
		incarnation: "incarnation-primary-" + suffix,
		primary:     primary, worktree: worktree, treeA: treeA, commitA: commitA,
	}
	ctx := context.Background()
	now := time.Now().Unix()
	if err := store.Catalog().UpsertRepositoryFamily(ctx, store_sqlite.RepositoryFamily{
		FamilyID: fam.familyID, CommonDirIdentity: filepath.Join(primary, ".git"),
		State: reconcile.FamilyStateReady, CreatedAt: now, LastSeen: now,
	}); err != nil {
		t.Fatalf("upsert family %s: %v", suffix, err)
	}
	primaryRow := store_sqlite.Checkout{
		CheckoutID: fam.primaryID, Incarnation: fam.incarnation, FamilyID: fam.familyID,
		RootPath: primary, GitDir: filepath.Join(primary, ".git"), AdminName: "@main",
		State: store_sqlite.CheckoutStateReady, DesiredMode: store_sqlite.CheckoutModeDedicated,
		EffectiveMode: store_sqlite.CheckoutModeDedicated, HeadRef: "refs/heads/main",
		HeadTree: treeA, HeadCommit: commitA, LastAccessible: now, LastSeen: now,
	}
	if err := store.Catalog().AllocateCheckout(ctx, primaryRow); err != nil {
		t.Fatalf("allocate primary %s: %v", suffix, err)
	}
	worktreeRow := primaryRow
	worktreeRow.CheckoutID = fam.checkoutID
	worktreeRow.Incarnation = "incarnation-worktree-" + suffix
	worktreeRow.RootPath = worktree
	worktreeRow.GitDir = filepath.Join(primary, ".git", "worktrees", "worktree")
	worktreeRow.AdminName = "worktree"
	worktreeRow.DesiredMode = store_sqlite.CheckoutModeAutomatic
	worktreeRow.EffectiveMode = store_sqlite.CheckoutModeAutomatic
	worktreeRow.HeadRef = "refs/heads/feature"
	if err := store.Catalog().AllocateCheckout(ctx, worktreeRow); err != nil {
		t.Fatalf("allocate worktree %s: %v", suffix, err)
	}
	if err := store.Catalog().UpsertDedicatedGraph(ctx, store_sqlite.DedicatedGraph{
		GraphID: fam.graphID, OwnerCheckoutID: fam.primaryID, RepoPrefix: prefix,
		FamilyID: fam.familyID, IsPrimaryBase: true, State: reconcile.GraphStateReady,
	}); err != nil {
		t.Fatalf("bind graph %s: %v", suffix, err)
	}
	if err := leases.RegisterRepositoryOwner(graphview.RepositoryOwner{
		GraphID: fam.graphID, CheckoutID: fam.primaryID, Incarnation: fam.incarnation, RepoPrefix: prefix,
	}); err != nil {
		t.Fatalf("register owner %s: %v", suffix, err)
	}
	return fam
}

// stageRealBaseClaim runs the committed-base protocol up to a FULL physical
// claimed-base build (authority, desire, claim, build) and returns the claim,
// leaving adoption to the caller. The build runs subprocesses, so a synctest
// bubble stages the claim BEFORE entering and only adopts inside.
func (f *startupAdmissionFixture) stageRealBaseClaim(t testing.TB, fam startupFamily) store_sqlite.DedicatedBaseBuildClaim {
	t.Helper()
	ctx := context.Background()
	authority, err := f.catalog.AcquireDedicatedBaseAuthority(ctx, store_sqlite.AcquireDedicatedBaseAuthorityRequest{
		GraphID: fam.graphID,
		Owner:   store_sqlite.DedicatedBaseOwner{CheckoutID: fam.primaryID, Incarnation: fam.incarnation},
		Token:   "authority-" + fam.prefix,
	})
	if err != nil {
		t.Fatalf("acquire authority %s: %v", fam.prefix, err)
	}
	desire, err := f.catalog.RecordDedicatedBaseDesire(ctx, store_sqlite.RecordDedicatedBaseDesireRequest{
		Authority: authority, ExpectedDesiredEpoch: 0,
		Identity: store_sqlite.DedicatedBaseIdentity{
			TreeOID: fam.treeA, ConfigHash: "config-" + fam.prefix,
			ExtractorVersions: extractorVersionsFingerprint(), ResolverVersion: "resolver-" + fam.prefix,
		},
	})
	if err != nil {
		t.Fatalf("record desire %s: %v", fam.prefix, err)
	}
	claim, err := f.catalog.ClaimDedicatedBaseBuild(ctx, store_sqlite.ClaimDedicatedBaseBuildRequest{
		Desire: desire, AttemptToken: "attempt-" + fam.prefix, ProvenanceCommitOID: fam.commitA,
		CreatedAt: 1, ExpectedActiveGenerationID: 0,
	})
	if err != nil {
		t.Fatalf("claim base %s: %v", fam.prefix, err)
	}
	builder := builderNewBuilder(f.store)
	if _, _, err := builder.BuildClaimedDedicatedBase(ctx, ClaimedDedicatedBaseRequest{
		Claim: claim, RootPath: fam.primary, WorkspaceID: fam.prefix, ProjectID: fam.prefix,
	}); err != nil {
		t.Fatalf("build base %s: %v", fam.prefix, err)
	}
	return claim
}

// adoptStagedClaim adopts an already-built claimed base, ending in a servable
// positive generation. It is a catalog write with no subprocess, so it is safe
// inside a synctest bubble and fires the synchronous adoption observer.
func (f *startupAdmissionFixture) adoptStagedClaim(t testing.TB, claim store_sqlite.DedicatedBaseBuildClaim) {
	t.Helper()
	if _, err := f.catalog.AdoptDedicatedBaseGeneration(context.Background(), store_sqlite.AdoptDedicatedBaseGenerationRequest{Claim: claim}); err != nil {
		t.Fatalf("adopt base: %v", err)
	}
}

// publishRealBase runs one whole committed-base publication for the family with
// a real payload: authority, desire, claim, a full claimed-base build, adoption.
// It is the catalog protocol the initial publisher runs, ending in a servable
// positive generation the materializer can compose over.
func (f *startupAdmissionFixture) publishRealBase(t testing.TB, fam startupFamily) {
	t.Helper()
	f.adoptStagedClaim(t, f.stageRealBaseClaim(t, fam))
}

// publishBasePointer publishes the family's base pointer through the catalog
// protocol with the physical build replaced by the ready state transition it
// ends in. It is synctest-safe (no subprocess), and leaves a servable positive
// generation — enough to mark the family ready for admission.
func (f *startupAdmissionFixture) publishBasePointer(t testing.TB, fam startupFamily) {
	t.Helper()
	ctx := context.Background()
	authority, err := f.catalog.AcquireDedicatedBaseAuthority(ctx, store_sqlite.AcquireDedicatedBaseAuthorityRequest{
		GraphID: fam.graphID,
		Owner:   store_sqlite.DedicatedBaseOwner{CheckoutID: fam.primaryID, Incarnation: fam.incarnation},
		Token:   "authority-" + fam.prefix,
	})
	if err != nil {
		t.Fatalf("acquire authority %s: %v", fam.prefix, err)
	}
	desire, err := f.catalog.RecordDedicatedBaseDesire(ctx, store_sqlite.RecordDedicatedBaseDesireRequest{
		Authority: authority, ExpectedDesiredEpoch: 0,
		Identity: store_sqlite.DedicatedBaseIdentity{
			TreeOID: fam.treeA, ConfigHash: "config-" + fam.prefix,
			ExtractorVersions: extractorVersionsFingerprint(), ResolverVersion: "resolver-" + fam.prefix,
		},
	})
	if err != nil {
		t.Fatalf("record desire %s: %v", fam.prefix, err)
	}
	claim, err := f.catalog.ClaimDedicatedBaseBuild(ctx, store_sqlite.ClaimDedicatedBaseBuildRequest{
		Desire: desire, AttemptToken: "attempt-" + fam.prefix, ProvenanceCommitOID: fam.commitA,
		CreatedAt: 1, ExpectedActiveGenerationID: 0,
	})
	if err != nil {
		t.Fatalf("claim base %s: %v", fam.prefix, err)
	}
	if err := f.catalog.PublishViewGeneration(ctx, claim.GenerationID, 1); err != nil {
		t.Fatalf("publish base row %s: %v", fam.prefix, err)
	}
	if _, err := f.catalog.AdoptDedicatedBaseGeneration(ctx, store_sqlite.AdoptDedicatedBaseGenerationRequest{Claim: claim}); err != nil {
		t.Fatalf("adopt base %s: %v", fam.prefix, err)
	}
}

// coordinator builds the production loop over a family's worktree, self-signal
// off, on the shared gate.
func (f *startupAdmissionFixture) coordinator(t testing.TB, fam startupFamily, gate *ViewBuildGate) *CheckoutCoordinator {
	t.Helper()
	builder := builderNewBuilder(f.store)
	// The edit-delta pre-warm is a background cache warm on its own 20-minute
	// context, unrelated to admission; defer it so it spawns no worker that
	// outlives the bubble. The test asserts routing, not the warm.
	builder.PrewarmDeferred = func([]int64) bool { return true }
	c, err := NewCheckoutCoordinator(CheckoutCoordinatorConfig{
		CheckoutID: fam.checkoutID, CheckoutRoot: fam.worktree, FamilyID: fam.familyID,
		RepoPrefix: fam.prefix, WorkspaceID: fam.prefix, ProjectID: fam.prefix,
		Store: f.store, Builder: builder, Leases: f.leases,
		Config: config.Default().Index, ConfigSections: dedicatedBaseConfigSections(config.Default()),
		Gate: gate, PollInterval: -1, Logger: zap.NewNop(),
	})
	if err != nil {
		t.Fatalf("NewCheckoutCoordinator %s: %v", fam.prefix, err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func (f *startupAdmissionFixture) materializer() *graphview.Materializer {
	return &graphview.Materializer{Store: f.store, Catalog: f.catalog, Leases: f.leases}
}

// TestStartupAdmissionServesReadyFamilyWhileUnrelatedWarms is the production
// trace: family A's base is published before its worktree is discovered, family
// B's startup publication is held behind a barrier, and the global latch is
// closed. A's worktree must serve its exact view NOW; B, whose base is not
// published, must keep deferring. Releasing B publishes its base and opens the
// latch (the warmup tail), after which B is admitted too.
func TestStartupAdmissionServesReadyFamilyWhileUnrelatedWarms(t *testing.T) {
	f := newStartupAdmissionFixture(t)
	f.publishRealBase(t, f.a) // A's own committed base is prepared.

	synctest.Test(t, func(t *testing.T) {
		gate := NewViewBuildGate() // closed: the warmup tail has not run.
		a := f.coordinator(t, f.a, gate)
		b := f.coordinator(t, f.b, gate)

		// B's startup publication is held; the latch opens only once it lands.
		bRelease := make(chan struct{})
		bDone := make(chan struct{})
		var releaseOnce sync.Once
		releaseB := func() { releaseOnce.Do(func() { close(bRelease) }) }
		// Release on any exit so a failed assertion does not leave the barrier
		// goroutine blocked when the bubble's main goroutine leaves.
		defer releaseB()
		go func() {
			<-bRelease
			f.publishBasePointer(t, f.b)
			gate.Open()
			close(bDone)
		}()

		a.Signal("checkout registered")
		b.Signal("checkout registered")
		synctest.Wait()
		time.Sleep(2 * time.Second) // past the quiet window, on the fake clock.
		synctest.Wait()

		if gate.Admitted() {
			t.Fatal("the global latch opened before the unrelated family released")
		}

		// A's base is published: its exact view must be servable while B warms.
		ctx := context.Background()
		view, err := f.materializer().MaterializeCheckout(ctx, f.a.checkoutID)
		if err != nil {
			t.Fatalf("family A must serve exact while family B warms: %v", err)
		}
		defer view.Close()
		flat := builderOpenStore(t, "flat-a")
		builderIndex(t, flat, f.a.worktree)
		builderAssertReadersAgree(t, view.Reader, flat)

		// B's base is not published: the protection holds — it has built nothing.
		if route, found, _ := f.catalog.GetCheckoutRoute(ctx, f.b.checkoutID); found && route.State == store_sqlite.RouteActive {
			t.Fatalf("family B served an active route before its base published: %+v", route)
		}

		releaseB()
		synctest.Wait()
		<-bDone
		if !gate.Admitted() {
			t.Fatal("the latch did not open after the unrelated family released")
		}
	})
}

// TestStartupAdmissionWakesWhenOwnBasePublishesDuringStartup is the second
// acceptance case: the worktree is discovered BEFORE its own base is published.
// It defers (the base is not ready), then its base publishes mid-startup. The
// adoption must wake it and admit it while the global latch is still closed and
// the unrelated family still warms — not wait for the warmup tail.
//
// The lifecycle is constructed inside the bubble because the catalog delivers
// an adoption synchronously to its observers: the publication, the fan-out and
// the coordinator wake all run on the bubble's own goroutines.
func TestStartupAdmissionWakesWhenOwnBasePublishesDuringStartup(t *testing.T) {
	f := newStartupAdmissionFixture(t) // neither base published yet.
	// Stage A's real committed payload BEFORE the bubble (the physical build
	// runs subprocesses); inside the bubble only the adoption runs, so the wake
	// is exercised with a servable payload whose exact content is then compared.
	aClaim := f.stageRealBaseClaim(t, f.a)

	synctest.Test(t, func(t *testing.T) {
		ctx := context.Background()
		gate := NewViewBuildGate()
		mi := NewMultiIndexer(f.store, builderRegistry(), search.NewNull(), nil, zap.NewNop())
		lc, err := NewCheckoutLifecycle(CheckoutLifecycleConfig{MultiIndexer: mi, Graph: f.store, Logger: zap.NewNop()})
		if err != nil {
			t.Fatalf("NewCheckoutLifecycle: %v", err)
		}
		lc.SetBuildGate(gate)
		t.Cleanup(func() { _ = lc.Close(); _ = mi.Close(ctx) })

		a := f.coordinator(t, f.a, gate)
		b := f.coordinator(t, f.b, gate)
		lc.installCoordinatorAtHead(store_sqlite.Checkout{CheckoutID: f.a.checkoutID, EffectiveMode: store_sqlite.CheckoutModeAutomatic, HeadRef: "refs/heads/feature"}, a)
		lc.installCoordinatorAtHead(store_sqlite.Checkout{CheckoutID: f.b.checkoutID, EffectiveMode: store_sqlite.CheckoutModeAutomatic, HeadRef: "refs/heads/feature"}, b)

		bRelease := make(chan struct{})
		bDone := make(chan struct{})
		var releaseOnce sync.Once
		releaseB := func() { releaseOnce.Do(func() { close(bRelease) }) }
		defer releaseB()
		go func() {
			<-bRelease
			f.publishBasePointer(t, f.b)
			gate.Open()
			close(bDone)
		}()

		a.Signal("checkout registered")
		b.Signal("checkout registered")
		synctest.Wait()
		time.Sleep(2 * time.Second)
		synctest.Wait()

		// A's base is not published yet: it defers, like any unready family.
		if route, found, _ := f.catalog.GetCheckoutRoute(ctx, f.a.checkoutID); found && route.State == store_sqlite.RouteActive {
			t.Fatalf("family A served before its own base published: %+v", route)
		}

		// A's own base publishes mid-startup. The adoption must wake A.
		f.adoptStagedClaim(t, aClaim)
		synctest.Wait()
		time.Sleep(2 * time.Second)
		synctest.Wait()

		if gate.Admitted() {
			t.Fatal("the global latch opened before the unrelated family released")
		}
		// A must now be routed and servable while B still warms.
		if route, found, _ := f.catalog.GetCheckoutRoute(ctx, f.a.checkoutID); !found || route.State != store_sqlite.RouteActive {
			t.Fatalf("family A was not woken by its own base publication: found=%v route=%+v", found, route)
		}
		view, err := f.materializer().MaterializeCheckout(ctx, f.a.checkoutID)
		if err != nil {
			t.Fatalf("family A must serve once its base publishes mid-startup: %v", err)
		}
		defer view.Close()
		// The woken view must carry A's exact committed content, not merely an
		// active route over an empty payload.
		flat := builderOpenStore(t, "flat-wake")
		builderIndex(t, flat, f.a.worktree)
		builderAssertReadersAgree(t, view.Reader, flat)

		releaseB()
		synctest.Wait()
		<-bDone
	})
}

// TestStartupAdmissionServesMultipleReadyFamiliesWhileClosed pins the bypass
// at scale: two families with published bases are both admitted while the
// global latch is closed, the one physical lane still serializes their builds
// (bounded resource), and both serve. The bypass is per family, not a blanket
// early open — the latch stays closed and a family-less build would still wait.
func TestStartupAdmissionServesMultipleReadyFamiliesWhileClosed(t *testing.T) {
	f := newStartupAdmissionFixture(t)
	f.publishRealBase(t, f.a)
	f.publishRealBase(t, f.b)

	synctest.Test(t, func(t *testing.T) {
		gate := NewViewBuildGate()
		a := f.coordinator(t, f.a, gate)
		b := f.coordinator(t, f.b, gate)
		a.Signal("checkout registered")
		b.Signal("checkout registered")
		synctest.Wait()
		time.Sleep(2 * time.Second)
		synctest.Wait()

		if gate.Admitted() {
			t.Fatal("the global latch opened on its own")
		}
		if !gate.AdmittedForFamily(f.a.familyID) || !gate.AdmittedForFamily(f.b.familyID) {
			t.Fatal("a published family was not admitted while the latch was closed")
		}
		// The ready families must have ACQUIRED the lane (a grant), not bypassed
		// it: eligibility is permission to compete for the one physical lane,
		// not a skip around it. A regression that skipped lane acquisition would
		// still route, so pin the grant itself.
		if s := gate.Stats(); s.AdmittedBackground+s.AdmittedInteractive == 0 {
			t.Fatal("ready families served without acquiring the build lane (grant-vs-bypass)")
		}
		ctx := context.Background()
		for _, fam := range []startupFamily{f.a, f.b} {
			route, found, _ := f.catalog.GetCheckoutRoute(ctx, fam.checkoutID)
			if !found || route.State != store_sqlite.RouteActive {
				t.Fatalf("family %s not routed while the latch was closed: found=%v route=%+v", fam.prefix, found, route)
			}
			view, err := f.materializer().MaterializeCheckout(ctx, fam.checkoutID)
			if err != nil {
				t.Fatalf("family %s must serve while the latch is closed: %v", fam.prefix, err)
			}
			view.Close()
		}
		view, err := f.materializer().MaterializeCheckout(ctx, f.a.checkoutID)
		if err != nil {
			t.Fatalf("materialize A: %v", err)
		}
		defer view.Close()
		flat := builderOpenStore(t, "flat-multi")
		builderIndex(t, flat, f.a.worktree)
		builderAssertReadersAgree(t, view.Reader, flat)
	})
}

// TestStartupAdmissionKeepsUnreadyFamilyDeferred is the protection half: a
// family whose committed base is NOT published keeps deferring its whole cycle
// while the global latch is closed. Admitting ready families must not admit
// unready ones — this passes before and after the fix and guards the boundary.
func TestStartupAdmissionKeepsUnreadyFamilyDeferred(t *testing.T) {
	f := newStartupAdmissionFixture(t) // B's base never published.

	synctest.Test(t, func(t *testing.T) {
		gate := NewViewBuildGate()
		b := f.coordinator(t, f.b, gate)
		b.Signal("checkout registered")
		synctest.Wait()
		time.Sleep(2 * time.Second)
		synctest.Wait()

		if gate.Admitted() {
			t.Fatal("the latch opened on its own")
		}
		// Deferral, not merely refusal: the unready family acquired no lane
		// grant and built nothing while the latch was closed.
		if s := gate.Stats(); s.AdmittedBackground+s.AdmittedInteractive != 0 {
			t.Fatalf("an unready family acquired a lane grant while the latch was closed: %+v", s)
		}
		ctx := context.Background()
		if route, found, _ := f.catalog.GetCheckoutRoute(ctx, f.b.checkoutID); found && route.State == store_sqlite.RouteActive {
			t.Fatalf("an unready family served an active route while the latch was closed: %+v", route)
		}
		if _, err := f.materializer().MaterializeCheckout(ctx, f.b.checkoutID); err == nil {
			t.Fatal("an unready family materialized a view while the latch was closed")
		}
	})
}
