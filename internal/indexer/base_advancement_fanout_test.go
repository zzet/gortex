package indexer

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/search"
)

// The committed-base advancement fan-out.
//
// A dependent worktree's layers are keyed on the base they were built over
// (CheckoutCoordinator.commitIdentity carries the base's committed tree as the
// layer's lower_view_fingerprint). When the primary publishes a new committed
// base, nothing in the daemon told the dependents: ensureCoordinator signals on
// the DEPENDENT's own HEAD moving, which a base advance is not, so the news
// arrived on the 15-second poll at best. These tests drive the real path — a
// real lifecycle over a real store, a real publication through the catalog's own
// protocol — and assert what the dependents are told and when.

// fanoutFixture is one family in the catalog plus the lifecycle that serves it.
type fanoutFixture struct {
	t         *testing.T
	store     *store_sqlite.Store
	catalog   *store_sqlite.Catalog
	lifecycle *CheckoutLifecycle
	familyID  string
	graphID   string
	ownerID   string
	authority store_sqlite.DedicatedBaseAuthority
	desire    store_sqlite.DedicatedBaseDesire
}

func newFanoutFixture(t *testing.T) *fanoutFixture {
	t.Helper()
	dir := t.TempDir()
	store, err := store_sqlite.Open(filepath.Join(dir, "catalog.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })

	mi := NewMultiIndexer(store, newTestRegistry(), search.NewNull(), nil, zap.NewNop())
	// The real constructor: it is what registers the lifecycle's adoption
	// observer, so nothing about this trace is arranged by the test.
	lifecycle, err := NewCheckoutLifecycle(CheckoutLifecycleConfig{
		MultiIndexer: mi,
		Graph:        store,
		Logger:       zap.NewNop(),
	})
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = lifecycle.Close()
		_ = mi.Close(context.Background())
	})

	f := &fanoutFixture{
		t: t, store: store, catalog: store.Catalog(), lifecycle: lifecycle,
		familyID: "family-fanout", graphID: "graph-fanout", ownerID: "checkout-owner",
	}
	ctx := context.Background()
	require.NoError(t, f.catalog.UpsertRepositoryFamily(ctx, store_sqlite.RepositoryFamily{
		FamilyID: f.familyID, CommonDirIdentity: filepath.Join(dir, "git"), State: "family_ready",
	}))
	require.NoError(t, f.catalog.UpsertCheckout(ctx, store_sqlite.Checkout{
		CheckoutID: f.ownerID, Incarnation: "incarnation-owner", FamilyID: f.familyID,
		RootPath: dir, GitDir: filepath.Join(dir, "git"), AdminName: "main",
		State:       store_sqlite.CheckoutStateReady,
		DesiredMode: store_sqlite.CheckoutModeDedicated, EffectiveMode: store_sqlite.CheckoutModeDedicated,
		HeadRef: "refs/heads/main", HeadCommit: "commit-before", HeadTree: "tree-before",
	}))
	require.NoError(t, f.catalog.UpsertDedicatedGraph(ctx, store_sqlite.DedicatedGraph{
		GraphID: f.graphID, OwnerCheckoutID: f.ownerID, RepoPrefix: "repo-fanout",
		FamilyID: f.familyID, IsPrimaryBase: true, State: store_sqlite.DedicatedGraphReady,
	}))

	f.authority, err = f.catalog.AcquireDedicatedBaseAuthority(ctx, store_sqlite.AcquireDedicatedBaseAuthorityRequest{
		GraphID: f.graphID,
		Owner:   store_sqlite.DedicatedBaseOwner{CheckoutID: f.ownerID, Incarnation: "incarnation-owner"},
		Token:   "authority-1",
	})
	require.NoError(t, err)
	return f
}

// dependent writes one automatic checkout row in the family.
func (f *fanoutFixture) dependent(checkoutID, adminName string) store_sqlite.Checkout {
	f.t.Helper()
	checkout := store_sqlite.Checkout{
		CheckoutID: checkoutID, Incarnation: "incarnation-" + adminName, FamilyID: f.familyID,
		RootPath: "/tmp/" + adminName, GitDir: "/tmp/" + adminName + "/.git", AdminName: adminName,
		State:       store_sqlite.CheckoutStateReady,
		DesiredMode: store_sqlite.CheckoutModeAutomatic, EffectiveMode: store_sqlite.CheckoutModeAutomatic,
		HeadRef:     "refs/heads/" + adminName,
	}
	require.NoError(f.t, f.catalog.UpsertCheckout(context.Background(), checkout))
	return checkout
}

// publish runs one whole committed-base publication for a tree: desire, claim,
// a ready payload row, adoption. It is the catalog protocol the publisher runs,
// with the physical build replaced by the state transition it ends in.
func (f *fanoutFixture) publish(t *testing.T, tree, commit string, clock, expectedActive int64) store_sqlite.DedicatedBaseAdoption {
	t.Helper()
	ctx := context.Background()
	desire, err := f.catalog.RecordDedicatedBaseDesire(ctx, store_sqlite.RecordDedicatedBaseDesireRequest{
		Authority:            f.authority,
		ExpectedDesiredEpoch: f.desire.Epoch,
		Identity: store_sqlite.DedicatedBaseIdentity{
			TreeOID: tree, ConfigHash: "config", ExtractorVersions: "extractors", ResolverVersion: "resolver",
		},
	})
	require.NoError(t, err)
	f.desire = desire

	claim, err := f.catalog.ClaimDedicatedBaseBuild(ctx, store_sqlite.ClaimDedicatedBaseBuildRequest{
		Desire: desire, AttemptToken: "attempt-" + tree, ProvenanceCommitOID: commit,
		CreatedAt: clock, ExpectedActiveGenerationID: expectedActive,
	})
	require.NoError(t, err)
	require.NoError(t, f.catalog.PublishViewGeneration(ctx, claim.GenerationID, clock))

	adoption, err := f.catalog.AdoptDedicatedBaseGeneration(ctx, store_sqlite.AdoptDedicatedBaseGenerationRequest{Claim: claim})
	require.NoError(t, err)
	return adoption
}

func (f *fanoutFixture) owner(t *testing.T) store_sqlite.Checkout {
	t.Helper()
	row, found, err := f.catalog.GetCheckout(context.Background(), f.ownerID)
	require.NoError(t, err)
	require.True(t, found)
	return row
}

// fanoutCoordinator is a real coordinator loop over a checkout identity, with
// every cycle settled by the preflight. Nothing it does touches Git or the
// catalog, so a completed cycle can only have come from a signal.
type fanoutCoordinator struct {
	coordinator *CheckoutCoordinator
	cycles      chan struct{}
}

func newFanoutCoordinator(t *testing.T, checkoutID, familyID string) *fanoutCoordinator {
	t.Helper()
	gate := NewViewBuildGate()
	gate.Open()
	lifetime, cancel := context.WithCancel(context.Background())
	out := &fanoutCoordinator{cycles: make(chan struct{}, 8)}
	out.coordinator = &CheckoutCoordinator{
		checkoutID:     checkoutID,
		familyID:       familyID,
		gate:           gate,
		logger:         zap.NewNop(),
		quiet:          time.Millisecond,
		poll:           -1,
		signal:         make(chan struct{}, 1),
		stop:           make(chan struct{}),
		done:           make(chan struct{}),
		lifetime:       lifetime,
		cancelLifetime: cancel,
		backlog:        map[int64]struct{}{},
		cyclePreflight: func(context.Context) (CheckoutCycle, bool) { return CheckoutCycle{}, true },
		cycleDone:      func(CheckoutCycle) { out.cycles <- struct{}{} },
	}
	go out.coordinator.run()
	t.Cleanup(func() { require.NoError(t, out.coordinator.Close()) })
	return out
}

// waitForCycle waits for one cycle to complete.
func (c *fanoutCoordinator) waitForCycle(t *testing.T, what string) {
	t.Helper()
	select {
	case <-c.cycles:
	case <-time.After(2 * time.Second):
		t.Fatalf("%s never recomposed: the base advance did not reach its coordinator", what)
	}
}

// requireQuiet fails when a cycle arrives inside the settle window.
func (c *fanoutCoordinator) requireQuiet(t *testing.T, what string) {
	t.Helper()
	select {
	case <-c.cycles:
		t.Fatalf("%s was woken by a base advance that is none of its business", what)
	case <-time.After(50 * time.Millisecond):
	}
}

// TestBaseAdvanceWakesEveryDependentExactlyOnce is the production trace: a
// committed base adopted through the catalog reaches the coordinators the
// lifecycle is running, without any poll and without anything else being woken.
func TestBaseAdvanceWakesEveryDependentExactlyOnce(t *testing.T) {
	f := newFanoutFixture(t)

	first := f.dependent("checkout-one", "one")
	second := f.dependent("checkout-two", "two")
	one := newFanoutCoordinator(t, first.CheckoutID, f.familyID)
	two := newFanoutCoordinator(t, second.CheckoutID, f.familyID)
	// The owner is the checkout the base is published FOR; its route is not
	// composed over itself. A checkout in another family shares nothing with
	// this publication at all.
	owner := newFanoutCoordinator(t, f.ownerID, f.familyID)
	stranger := newFanoutCoordinator(t, "checkout-stranger", "family-other")
	for _, c := range []*fanoutCoordinator{one, two, owner, stranger} {
		require.True(t, f.lifecycle.installCoordinatorAtHead(store_sqlite.Checkout{
			CheckoutID: c.coordinator.checkoutID, EffectiveMode: store_sqlite.CheckoutModeAutomatic,
		}, c.coordinator))
	}
	one.requireQuiet(t, "a dependent before any publication")

	adoption := f.publish(t, "tree-a", "commit-a", 1000, 0)
	require.True(t, adoption.GenerationID > 0)

	one.waitForCycle(t, "the first dependent")
	two.waitForCycle(t, "the second dependent")
	owner.requireQuiet(t, "the base's own owner")
	stranger.requireQuiet(t, "a checkout in another family")
	// Exactly once: one advance is one recomposition, not a burst.
	one.requireQuiet(t, "the first dependent, after its recomposition")
	two.requireQuiet(t, "the second dependent, after its recomposition")

	// The head the dependents key on moved with the adoption, in the adoption's
	// own transaction — not an hour later when the janitor next samples.
	owned := f.owner(t)
	require.Equal(t, "tree-a", owned.HeadTree, "the adoption did not advance the owner's committed tree")
	require.Equal(t, "commit-a", owned.HeadCommit)
	require.True(t, adoption.HeadAdvanced)
}

// TestBaseReplayWakesNobody keeps the fan-out honest about what it is
// announcing. Re-adopting the pointer that is already installed changes nothing
// a dependent could observe, and a signal for it would be a cycle — and, on a
// checkout whose identity has moved, a build — bought with nothing.
func TestBaseReplayWakesNobody(t *testing.T) {
	f := newFanoutFixture(t)
	dependent := f.dependent("checkout-one", "one")
	one := newFanoutCoordinator(t, dependent.CheckoutID, f.familyID)
	require.True(t, f.lifecycle.installCoordinatorAtHead(store_sqlite.Checkout{
		CheckoutID: dependent.CheckoutID, EffectiveMode: store_sqlite.CheckoutModeAutomatic,
	}, one.coordinator))

	f.publish(t, "tree-a", "commit-a", 1000, 0)
	one.waitForCycle(t, "the dependent")

	ctx := context.Background()
	publication, found, err := f.catalog.DedicatedBasePublication(ctx, f.graphID)
	require.NoError(t, err)
	require.True(t, found)
	replay, err := f.catalog.AdoptDedicatedBaseGeneration(ctx, store_sqlite.AdoptDedicatedBaseGenerationRequest{Claim: publication.Claim})
	require.NoError(t, err)
	require.True(t, replay.AlreadyAdopted)
	one.requireQuiet(t, "the dependent, on a replayed adoption")
}

// TestClosedLifecycleStopsHearingAdoptions pins the release half. A lifecycle
// that has shut down still shares the store with whatever publishes next — a
// one-shot server in the same process, another fixture — and an announcement
// that reached its torn-down registry would signal coordinators it has joined.
func TestClosedLifecycleStopsHearingAdoptions(t *testing.T) {
	f := newFanoutFixture(t)
	dependent := f.dependent("checkout-one", "one")
	one := newFanoutCoordinator(t, dependent.CheckoutID, f.familyID)
	require.True(t, f.lifecycle.installCoordinatorAtHead(store_sqlite.Checkout{
		CheckoutID: dependent.CheckoutID, EffectiveMode: store_sqlite.CheckoutModeAutomatic,
	}, one.coordinator))

	require.NoError(t, f.lifecycle.Close())
	// Closing the lifecycle closes the coordinators it holds, so this one is
	// re-armed as a fresh loop the closed registry knows nothing about.
	fresh := newFanoutCoordinator(t, dependent.CheckoutID, f.familyID)
	f.lifecycle.coordMu.Lock()
	f.lifecycle.coordinators[dependent.CheckoutID] = fresh.coordinator
	f.lifecycle.coordMu.Unlock()

	f.publish(t, "tree-a", "commit-a", 1000, 0)
	fresh.requireQuiet(t, "a coordinator held by a closed lifecycle")
}
