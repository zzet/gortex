package indexer

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/graphview"
)

// The publisher/drain half used to be unreachable in production: nothing
// installed a DedicatedBaseCleanupRuntime, so closeRepositoryPublisher
// (repository_admission.go:61-64) returned the already-drained sentinel,
// stopRepositoryPublishers (:75-77) returned it too, and
// RegisterRepositoryOwner's prepare hook (:124-131) was a no-op. The runtime is
// now mounted by NewSharedServer (serverstack/shared_server.go:671-681).
//
// These tests drive the PUBLIC doors — Register, Untrack, Close — over a real
// runtime mounted the way that stack mounts it, and assert the four halves of
// gate 7 on the publisher side: closing rejects new admission, an in-flight
// publication holds the untrack drain open, finalization reclaims exactly the
// captured slot, and neither the stale slot nor the stale drain capability can
// reach a replacement registration that reused the same path and graph ID.

// mountPublisherRuntime mounts the runtime exactly where the server stack does:
// over the lifecycle's OWN lease manager (a private one would make advancement
// invisible to the retirement sweep) and before any owner can be registered.
func mountPublisherRuntime(t *testing.T, f *lifecycleFixture) *DedicatedBaseRuntime {
	t.Helper()
	runtime, err := NewDedicatedBaseRuntime(f.store, f.lc.ViewLeases())
	require.NoError(t, err)
	require.NoError(t, f.lc.SetDedicatedBaseCleanupRuntime(runtime))
	require.Same(t, f.lc.ViewLeases(), runtime.ViewLeases())
	return runtime
}

// publisherDrainFixture is one mounted runtime and one repository tracked
// through the public Register door, so every owner slot under test is the one
// RegisterRepositoryOwner's prepare hook actually installed.
type publisherDrainFixture struct {
	*lifecycleFixture
	runtime *DedicatedBaseRuntime
	root    string
	prefix  string
	graphID string
	owner   store_sqlite.DedicatedBaseOwner
}

func newPublisherDrainFixture(t *testing.T, name string) *publisherDrainFixture {
	t.Helper()
	f := newLifecycleFixture(t)
	runtime := mountPublisherRuntime(t, f)
	root := f.gitRepo(name)
	tracked, err := f.lc.Register(context.Background(), config.RepoEntry{Path: root, Name: name}, TrackSourceCLI)
	require.NoError(t, err)
	require.NoError(t, tracked.CatalogErr)
	require.NotEmpty(t, tracked.GraphID)
	require.NotEmpty(t, tracked.Incarnation)
	return &publisherDrainFixture{
		lifecycleFixture: f,
		runtime:          runtime,
		root:             root,
		prefix:           tracked.Prefix,
		graphID:          tracked.GraphID,
		owner:            store_sqlite.DedicatedBaseOwner{CheckoutID: tracked.CheckoutID, Incarnation: tracked.Incarnation},
	}
}

// publisherSlot reads one graph's owner admission under the runtime's own
// mutex; every field on it is written under that mutex.
func publisherSlot(t testing.TB, runtime *DedicatedBaseRuntime, graphID string) *dedicatedBaseOwnerAdmission {
	t.Helper()
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	return runtime.ownerAdmissions[graphID]
}

type publisherSlotState struct {
	present bool
	owner   store_sqlite.DedicatedBaseOwner
	active  int
	closing bool
	drained <-chan struct{}
	slots   int
}

func readPublisherSlot(t testing.TB, runtime *DedicatedBaseRuntime, graphID string) publisherSlotState {
	t.Helper()
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	out := publisherSlotState{slots: len(runtime.ownerAdmissions)}
	state := runtime.ownerAdmissions[graphID]
	if state == nil {
		return out
	}
	out.present, out.owner, out.active, out.closing, out.drained = true, state.owner, state.active, state.closing, state.drained
	return out
}

func lifecycleRepositoryTombstones(t testing.TB, l *CheckoutLifecycle, graphID string) (owned, closing bool) {
	t.Helper()
	l.repositoryAdmissionMu.Lock()
	defer l.repositoryAdmissionMu.Unlock()
	_, owned = l.repositoryOwners[graphID]
	_, closing = l.repositoryClosing[graphID]
	return owned, closing
}

func assertDrainOpen(t testing.TB, drained <-chan struct{}, why string) {
	t.Helper()
	require.NotNil(t, drained)
	select {
	case <-drained:
		t.Fatalf("publisher drain closed while %s", why)
	default:
	}
}

// finishRepositoryCleanup drives the same pair the two production retry drivers
// pair — the cleanup runtime loop (repository_cleanup.go:339-341) and public
// Untrack (applyUntrack, checkout_lifecycle.go:1115) — until the prefix stops
// being pending.
// The background loop may win any given round; both are idempotent.
func finishRepositoryCleanup(t testing.TB, l *CheckoutLifecycle, prefix string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		_ = l.rec.Resume(context.Background())
		pending, err := l.finalizeRepositoryCleanups(context.Background(), prefix)
		if err == nil && !pending {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("repository cleanup for %s never finalized: pending=%v err=%v", prefix, pending, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestPublicUntrackWaitsForTheInFlightPublicationThenFinalizesTheCapturedSlot
// is the production-entrypoint trace for the drain half. Register mounts the
// owner slot through the prepare hook; public Untrack closes it and must not
// finish while a counted publication actor is still running; finalization then
// reclaims exactly the slot the close captured.
func TestPublicUntrackWaitsForTheInFlightPublicationThenFinalizesTheCapturedSlot(t *testing.T) {
	f := newPublisherDrainFixture(t, "publisher-drain-untrack")
	// Registered before any publication is admitted, so the LIFO cleanup order
	// releases every actor before the fixture's Close waits on the drain.
	t.Cleanup(f.close)
	ctx := context.Background()

	// The public Register door reached the publisher runtime.
	opened := readPublisherSlot(t, f.runtime, f.graphID)
	require.True(t, opened.present, "public Register did not install a publisher owner slot")
	require.Equal(t, f.owner, opened.owner)
	require.False(t, opened.closing)

	slot := publisherSlot(t, f.runtime, f.graphID)
	// One admitted publication, holding its exact slot the way ensureObserved
	// does (dedicated_base_runtime.go:301).
	release, admission, err := f.runtime.admitOwnerState(ctx, f.graphID, f.owner, slot)
	require.NoError(t, err)
	require.Same(t, slot, admission)
	// The release is idempotent, and Close joins every admitted actor: a
	// failed assertion below must not leave the fixture's teardown wedged on a
	// publication this test is holding.
	t.Cleanup(release)

	out, untrackErr := f.lc.Untrack(ctx, f.root)
	if untrackErr != nil {
		require.ErrorIs(t, untrackErr, ErrRepositoryCleanupPending)
	}
	require.True(t, out.Pending, "untrack reported completion while a publication was still admitted")

	held := readPublisherSlot(t, f.runtime, f.graphID)
	require.True(t, held.present, "untrack dropped the publisher slot before its actor drained")
	require.True(t, held.closing, "untrack did not close publisher admission")
	require.Equal(t, 1, held.active)
	assertDrainOpen(t, held.drained, "a publication was still admitted")

	owned, closing := lifecycleRepositoryTombstones(t, f.lc, f.graphID)
	require.True(t, owned && closing, "untrack lost its cleanup capability: owned=%v closing=%v", owned, closing)

	// Closing rejects new admission — both a fresh publication and the
	// in-flight one's next counted phase.
	if _, _, err := f.runtime.admitOwnerState(ctx, f.graphID, f.owner, nil); !errors.Is(err, errDedicatedBaseRuntimeClosed) {
		t.Fatalf("closing publisher admitted a fresh publication: %v", err)
	}
	if _, _, err := f.runtime.admitOwnerState(ctx, f.graphID, f.owner, slot); !errors.Is(err, errDedicatedBaseRuntimeClosed) {
		t.Fatalf("closing publisher re-admitted its in-flight publication: %v", err)
	}

	// Cleanup stays pending for exactly as long as the actor runs.
	pending, err := f.lc.finalizeRepositoryCleanups(ctx, f.prefix)
	require.NoError(t, err)
	require.True(t, pending, "cleanup finalized while its publication was still running")

	release()
	awaitDedicatedDrain(t, held.drained)
	finishRepositoryCleanup(t, f.lc, f.prefix)

	after := readPublisherSlot(t, f.runtime, f.graphID)
	require.False(t, after.present, "finalization left the drained publisher slot behind")
	require.Zero(t, after.slots)
	owned, closing = lifecycleRepositoryTombstones(t, f.lc, f.graphID)
	require.False(t, owned || closing, "finalization left lifecycle tombstones: owned=%v closing=%v", owned, closing)
	require.False(t, f.lc.RepositoryAdmissionClosed(f.prefix))
	require.Nil(t, f.mi.GetMetadata(f.prefix), "finalized cleanup left the repository in the corpus")
}

// TestReplacementRegistrationSurvivesTheStalePublisherAfterUntrack covers the
// path/ID-reuse half of gate 7. GraphIDFor is deterministic from the prefix, so
// re-tracking the same root reuses the same graph ID; neither the old slot
// pointer nor the old drain capability may reach the replacement.
func TestReplacementRegistrationSurvivesTheStalePublisherAfterUntrack(t *testing.T) {
	f := newPublisherDrainFixture(t, "publisher-drain-retrack")
	t.Cleanup(f.close)
	ctx := context.Background()

	staleSlot := publisherSlot(t, f.runtime, f.graphID)
	require.NotNil(t, staleSlot)
	staleOwner := f.owner

	if _, err := f.lc.Untrack(ctx, f.root); err != nil {
		require.ErrorIs(t, err, ErrRepositoryCleanupPending)
	}
	finishRepositoryCleanup(t, f.lc, f.prefix)
	staleDrain := readPublisherSlot(t, f.runtime, f.graphID)
	require.False(t, staleDrain.present, "untrack left the publisher slot behind")

	tracked, err := f.lc.Register(ctx, config.RepoEntry{Path: f.root, Name: "publisher-drain-retrack"}, TrackSourceCLI)
	require.NoError(t, err)
	require.NoError(t, tracked.CatalogErr)
	require.Equal(t, f.prefix, tracked.Prefix, "the replacement did not reuse the repository path")
	require.Equal(t, f.graphID, tracked.GraphID, "the replacement did not reuse the graph ID")

	replacement := publisherSlot(t, f.runtime, tracked.GraphID)
	require.NotNil(t, replacement, "the replacement registration installed no publisher slot")
	require.NotSame(t, staleSlot, replacement, "the replacement reopened the retired slot")

	owner := store_sqlite.DedicatedBaseOwner{CheckoutID: tracked.CheckoutID, Incarnation: tracked.Incarnation}
	require.NotEqual(t, staleOwner, owner, "the replacement reused the retired incarnation")

	// The retired owner tuple is refused outright. A refused admission returns
	// no release, so an admission that wrongly succeeds is released here rather
	// than left counted — an orphaned actor would wedge the fixture's Close.
	if stale, _, err := f.runtime.admitOwnerState(ctx, tracked.GraphID, staleOwner, staleSlot); err == nil {
		t.Cleanup(stale)
		t.Fatal("a stale publisher admission reached the replacement registration")
	}
	// And so is a stale capability carrying the CURRENT owner tuple: the slot
	// pointer and the drain channel are the identities, not the tuple. Without
	// the expected-slot fence (dedicated_base_runtime_drain.go:45-48) and the
	// drain-channel fence (dedicated_base_runtime_finalize.go:26) a publication
	// held across the untrack would adopt into, and finalize, the replacement.
	if stale, _, err := f.runtime.admitOwnerState(ctx, tracked.GraphID, owner, staleSlot); !errors.Is(err, errDedicatedBaseRuntimeClosed) {
		if stale != nil {
			t.Cleanup(stale)
		}
		t.Fatalf("a stale publisher slot admitted against the replacement: %v", err)
	}
	if err := f.runtime.FinalizeDedicatedBaseOwner(tracked.GraphID, owner, staleSlot.drained); !errors.Is(err, store_sqlite.ErrCatalogStaleGuard) {
		t.Fatalf("a stale drain capability finalized the replacement registration: %v", err)
	}
	require.Same(t, replacement, publisherSlot(t, f.runtime, tracked.GraphID))

	// The replacement is usable through its own identity.
	release, admission, err := f.runtime.admitOwnerState(ctx, tracked.GraphID, owner, replacement)
	require.NoError(t, err)
	require.Same(t, replacement, admission)
	t.Cleanup(release)
	release()
}

// TestLifecycleCloseJoinsEveryAdmittedPublisher pins the shutdown actor join.
// Close stops publisher admission first (CheckoutLifecycle.Close,
// checkout_lifecycle.go:2565) and waits on that drain last (:2633); an admitted
// publication counts for its whole length, physical build included, so Close
// must not return while one runs.
func TestLifecycleCloseJoinsEveryAdmittedPublisher(t *testing.T) {
	f := newLifecycleFixture(t)
	// Registered first, so it runs last: every admitted publication is released
	// before the store this test's Close is still draining over goes away.
	t.Cleanup(func() {
		_ = f.mi.Close(context.Background())
		_ = f.store.Close()
	})
	runtime := mountPublisherRuntime(t, f)
	ctx := context.Background()

	graphs := []string{"graph-a", "graph-b"}
	releases := make([]func(), 0, len(graphs))
	for i, graphID := range graphs {
		owner := store_sqlite.DedicatedBaseOwner{CheckoutID: "checkout", Incarnation: "incarnation"}
		require.NoError(t, runtime.RegisterDedicatedBaseOwner(graphID, owner))
		slot := publisherSlot(t, runtime, graphID)
		require.NotNil(t, slot)
		release, _, err := runtime.admitOwnerState(ctx, graphID, owner, slot)
		require.NoError(t, err, "admit publication %d", i)
		t.Cleanup(release) // idempotent; keeps a failed assertion from wedging Close
		releases = append(releases, release)
	}

	closed := make(chan error, 1)
	go func() { closed <- f.lc.Close() }()
	for _, release := range releases {
		select {
		case err := <-closed:
			t.Fatalf("Close returned with publications still admitted: %v", err)
		case <-time.After(150 * time.Millisecond):
		}
		release()
	}
	select {
	case err := <-closed:
		require.NoError(t, err)
	case <-time.After(30 * time.Second):
		t.Fatal("Close did not join the publisher drain after every publication finished")
	}
	require.Zero(t, readPublisherSlot(t, runtime, graphs[0]).active)
	// Shutdown closed admission for every owner, not just the one being torn down.
	for _, graphID := range graphs {
		state := readPublisherSlot(t, runtime, graphID)
		require.True(t, state.present && state.closing, "shutdown left %s open", graphID)
		awaitDedicatedDrain(t, state.drained)
		owner := store_sqlite.DedicatedBaseOwner{CheckoutID: "checkout", Incarnation: "incarnation"}
		if _, _, err := runtime.admitOwnerState(ctx, graphID, owner, nil); !errors.Is(err, errDedicatedBaseRuntimeClosed) {
			t.Fatalf("shutdown admitted a publication for %s: %v", graphID, err)
		}
	}
	if err := f.lc.RegisterRepositoryOwner(ctx, "graph-a"); !errors.Is(err, graphview.ErrRepositoryAdmissionsStopped) {
		t.Fatalf("registration after shutdown: %v", err)
	}
}
