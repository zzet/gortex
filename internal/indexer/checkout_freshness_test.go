package indexer

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

func newCheckoutFreshnessHarness(
	t testing.TB,
) (*coordinatorFixture, *CheckoutCoordinator, *CheckoutLifecycle) {
	t.Helper()
	f := newCoordinatorFixture(t)
	c := f.coordinator(t, CheckoutCoordinatorConfig{
		Debounce:     time.Hour,
		PollInterval: -1,
	})
	initial := c.reconcile(context.Background())
	if initial.Err != nil {
		t.Fatalf("publish initial route: %v", initial.Err)
	}
	if initial.CommitGenerationID <= 0 || initial.DirtyGenerationID <= 0 {
		t.Fatalf("initial route is incomplete: %+v", initial)
	}
	lifecycle := &CheckoutLifecycle{
		catalog:               f.catalog,
		coordinators:          map[string]*CheckoutCoordinator{f.checkoutID: c},
		coordinatorHeads:      map[string]checkoutHeadIdentity{},
		coordinatorActivating: map[string]struct{}{},
		started:               map[string][]*CheckoutCoordinator{f.checkoutID: {c}},
		owed:                  map[int64]struct{}{},
	}
	return f, c, lifecycle
}

func TestEnsureCheckoutFreshClassifiesCurrentFilesystemState(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*testing.T, *coordinatorFixture)
		fresh  bool
	}{
		{name: "clean", fresh: true},
		{
			name: "HEAD switch",
			mutate: func(_ *testing.T, f *coordinatorFixture) {
				f.commitTreeB()
			},
		},
		{
			name: "tracked edit",
			mutate: func(t *testing.T, f *coordinatorFixture) {
				builderWriteFile(t, f.worktree, "core.go", "package fixture\n\nfunc TrackedEdit() {}\n")
			},
		},
		{
			name: "untracked file",
			mutate: func(t *testing.T, f *coordinatorFixture) {
				builderWriteFile(t, f.worktree, "untracked.go", "package fixture\n\nfunc Untracked() {}\n")
			},
		},
		{
			name: "deleted file",
			mutate: func(t *testing.T, f *coordinatorFixture) {
				require.NoError(t, os.Remove(filepath.Join(f.worktree, "core.go")))
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f, _, lifecycle := newCheckoutFreshnessHarness(t)
			before := f.route()
			if tt.mutate != nil {
				tt.mutate(t, f)
			}

			got, err := lifecycle.EnsureCheckoutFresh(context.Background(), f.checkoutID)
			require.NoError(t, err)
			assert.Equal(t, tt.fresh, got.Fresh)
			assert.Equal(t, !tt.fresh, got.Building)
			assert.Equal(t, before, f.route(), "a freshness probe must not publish a route")
		})
	}
}

type coordinatorFreshnessCaches struct {
	dirtyFingerprint string
	routedDirty      int64
	retained         []retainedCommitLayer
	unbornTreeOID    string
}

func readCoordinatorFreshnessCaches(c *CheckoutCoordinator) coordinatorFreshnessCaches {
	c.mu.Lock()
	got := coordinatorFreshnessCaches{
		dirtyFingerprint: c.dirtyFingerprint,
		routedDirty:      c.routedDirty,
		retained:         slices.Clone(c.retained),
	}
	c.mu.Unlock()
	c.unbornTreeMu.Lock()
	got.unbornTreeOID = c.unbornTreeOID
	c.unbornTreeMu.Unlock()
	return got
}

func fileContentDigest(path string) string {
	body, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return "missing"
	}
	if err != nil {
		return "error:" + err.Error()
	}
	return fmt.Sprintf("%x", sha256.Sum256(body))
}

func TestEnsureCheckoutFreshIsReadOnlyEvenWhenCheckoutHeadLabelChanged(t *testing.T) {
	f, c, lifecycle := newCheckoutFreshnessHarness(t)
	ctx := context.Background()

	// A new branch at the same commit changes only the checkout label. The
	// routed tree/fingerprint remains exact, making this the case that catches
	// an accidental UpdateCheckoutHead without requiring a rebuild.
	builderGit(t, f.worktree, "checkout", "-b", "freshness-alias")
	checkoutBefore, found, err := f.catalog.GetCheckout(ctx, f.checkoutID)
	require.NoError(t, err)
	require.True(t, found)
	require.NotEqual(t, "refs/heads/freshness-alias", checkoutBefore.HeadRef)
	routeBefore := f.route()
	commitBefore, found := f.generation(routeBefore.CommitGenerationID)
	require.True(t, found)
	dirtyBefore, found := f.generation(routeBefore.DirtyGenerationID)
	require.True(t, found)
	cachesBefore := readCoordinatorFreshnessCaches(c)
	dbBefore := fileContentDigest(f.store.Path())
	walBefore := fileContentDigest(f.store.Path() + "-wal")
	indexPath := filepath.Join(checkoutBefore.GitDir, "index")
	indexBefore := fileContentDigest(indexPath)

	// Holding the writer gate makes an accidental catalog mutation block. The
	// read-only probe must remain fast and complete through the read connection.
	release, err := f.store.HoldWriteGate(ctx)
	require.NoError(t, err)
	probeCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	started := time.Now()
	got, probeErr := lifecycle.EnsureCheckoutFresh(probeCtx, f.checkoutID)
	elapsed := time.Since(started)
	cancel()
	release()
	require.NoError(t, probeErr)
	assert.Equal(t, CheckoutFreshness{Fresh: true}, got)
	assert.Less(t, elapsed, time.Second)

	checkoutAfter, found, err := f.catalog.GetCheckout(ctx, f.checkoutID)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, checkoutBefore, checkoutAfter)
	assert.Equal(t, routeBefore, f.route())
	commitAfter, found := f.generation(routeBefore.CommitGenerationID)
	require.True(t, found)
	dirtyAfter, found := f.generation(routeBefore.DirtyGenerationID)
	require.True(t, found)
	assert.Equal(t, commitBefore, commitAfter)
	assert.Equal(t, dirtyBefore, dirtyAfter)
	assert.Equal(t, cachesBefore, readCoordinatorFreshnessCaches(c))
	assert.Equal(t, dbBefore, fileContentDigest(f.store.Path()))
	assert.Equal(t, walBefore, fileContentDigest(f.store.Path()+"-wal"))
	assert.Equal(t, indexBefore, fileContentDigest(indexPath))
	assert.Empty(t, c.signal, "a fresh route must not schedule a build")
}

func TestEnsureCheckoutFreshBusyIsNonBlockingAndSignalsCoalesce(t *testing.T) {
	f := newCoordinatorFixture(t)
	c := &CheckoutCoordinator{
		checkoutID: f.checkoutID,
		root:       f.worktree,
		signal:     make(chan struct{}, 1),
		done:       make(chan struct{}),
	}
	lifecycle := &CheckoutLifecycle{
		catalog:      f.catalog,
		coordinators: map[string]*CheckoutCoordinator{f.checkoutID: c},
	}
	c.cycleMu.Lock()
	defer c.cycleMu.Unlock()

	started := time.Now()
	for range 64 {
		got, err := lifecycle.EnsureCheckoutFresh(context.Background(), f.checkoutID)
		require.NoError(t, err)
		require.Equal(t, CheckoutFreshness{Building: true}, got)
	}
	assert.Less(t, time.Since(started), time.Second,
		"freshness probes waited behind the held build mutex")
	assert.Len(t, c.signal, 1, "busy requests must share one buffered follow-up signal")
}

func TestEnsureCheckoutFreshCanceledRequestDoesNotSignal(t *testing.T) {
	f, c, lifecycle := newCheckoutFreshnessHarness(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	got, err := lifecycle.EnsureCheckoutFresh(ctx, f.checkoutID)
	assert.ErrorIs(t, err, context.Canceled)
	assert.Zero(t, got)
	assert.Empty(t, c.signal, "a canceled request must not schedule background work")
}

func TestEnsureCheckoutFreshMissingRouteAndRemovedCheckout(t *testing.T) {
	t.Run("missing route schedules rebuild", func(t *testing.T) {
		f, _, lifecycle := newCheckoutFreshnessHarness(t)
		require.NoError(t, f.catalog.DeleteCheckoutRoute(context.Background(), f.checkoutID))
		got, err := lifecycle.EnsureCheckoutFresh(context.Background(), f.checkoutID)
		require.NoError(t, err)
		assert.Equal(t, CheckoutFreshness{Building: true}, got)
	})

	t.Run("unknown checkout is an error", func(t *testing.T) {
		f, _, lifecycle := newCheckoutFreshnessHarness(t)
		got, err := lifecycle.EnsureCheckoutFresh(context.Background(), "checkout-does-not-exist")
		assert.ErrorIs(t, err, ErrCheckoutNotTracked)
		assert.Zero(t, got)
		_ = f
	})

	t.Run("removal grace is an error", func(t *testing.T) {
		f, _, lifecycle := newCheckoutFreshnessHarness(t)
		ctx := context.Background()
		checkout, found, err := f.catalog.GetCheckout(ctx, f.checkoutID)
		require.NoError(t, err)
		require.True(t, found)
		require.NoError(t, f.catalog.UpdateCheckoutState(ctx, store_sqlite.UpdateCheckoutStateRequest{
			CheckoutID:    checkout.CheckoutID,
			Incarnation:   checkout.Incarnation,
			State:         store_sqlite.CheckoutStateRemovalGrace,
			DesiredMode:   checkout.DesiredMode,
			EffectiveMode: checkout.EffectiveMode,
			LastSeen:      checkout.LastSeen,
		}))
		got, err := lifecycle.EnsureCheckoutFresh(ctx, f.checkoutID)
		assert.ErrorIs(t, err, ErrCheckoutMoved)
		assert.Zero(t, got)
	})
}

func TestEnsureCheckoutFreshAcceptsRoutedDedicatedCheckout(t *testing.T) {
	f, c, lifecycle := newCheckoutFreshnessHarness(t)
	ctx := context.Background()
	checkout, found, err := f.catalog.GetCheckout(ctx, f.checkoutID)
	require.NoError(t, err)
	require.True(t, found)
	require.NoError(t, f.catalog.UpdateCheckoutState(ctx, store_sqlite.UpdateCheckoutStateRequest{
		CheckoutID:    checkout.CheckoutID,
		Incarnation:   checkout.Incarnation,
		State:         checkout.State,
		DesiredMode:   store_sqlite.CheckoutModeDedicated,
		EffectiveMode: store_sqlite.CheckoutModeDedicated,
		LastSeen:      checkout.LastSeen,
	}))

	got, err := lifecycle.EnsureCheckoutFresh(ctx, f.checkoutID)
	require.NoError(t, err)
	assert.Equal(t, CheckoutFreshness{Fresh: true}, got)
	assert.Empty(t, c.signal)
}

func TestEnsureCheckoutFreshReactivatesRoutedDedicatedCheckout(t *testing.T) {
	f := newLifecycleFixture(t)
	defer f.close()
	ctx := context.Background()
	main := f.gitRepo("freshness-dedicated-reactivation")
	tracked, err := f.lc.Register(ctx, config.RepoEntry{
		Path: main,
		Name: "freshness-dedicated-reactivation",
	}, TrackSourceCLI)
	require.NoError(t, err)
	require.NoError(t, tracked.CatalogErr)
	_, routed := f.routeOf(tracked.CheckoutID)
	require.True(t, routed, "dedicated checkout did not publish its exact route")
	require.True(t, f.lc.SignalCheckout(tracked.CheckoutID, "precondition"))

	// Preserve the durable route while removing only its process-local owner,
	// then fence activation so concurrent requests can be inspected before the
	// worker reaches coordinator construction.
	f.lc.dropCoordinator(tracked.CheckoutID)
	require.False(t, f.lc.SignalCheckout(tracked.CheckoutID, "dropped precondition"))
	topology, err := f.lc.AcquireCheckoutTopology(ctx, tracked.CheckoutID)
	require.NoError(t, err)
	topologyHeld := true
	defer func() {
		if topologyHeld {
			topology.Release()
		}
	}()
	for range 32 {
		got, ensureErr := f.lc.EnsureCheckoutFresh(ctx, tracked.CheckoutID)
		require.NoError(t, ensureErr)
		require.Equal(t, CheckoutFreshness{Building: true}, got)
	}
	f.lc.coordMu.Lock()
	_, activating := f.lc.coordinatorActivating[tracked.CheckoutID]
	activationCount := len(f.lc.coordinatorActivating)
	f.lc.coordMu.Unlock()
	require.True(t, activating)
	require.Equal(t, 1, activationCount, "dedicated freshness requests did not coalesce")
	topology.Release()
	topologyHeld = false

	require.Eventually(t, func() bool {
		got, ensureErr := f.lc.EnsureCheckoutFresh(ctx, tracked.CheckoutID)
		return ensureErr == nil && got.Fresh && !got.Building
	}, 10*time.Second, 20*time.Millisecond,
		"routed dedicated checkout did not regain a live exact publisher")
}

func BenchmarkEnsureCheckoutFresh(b *testing.B) {
	f, c, lifecycle := newCheckoutFreshnessHarness(b)
	ctx := context.Background()

	b.Run("fresh_read_only", func(b *testing.B) {
		b.ReportAllocs()
		for range b.N {
			got, err := lifecycle.EnsureCheckoutFresh(ctx, f.checkoutID)
			if err != nil || !got.Fresh || got.Building {
				b.Fatalf("freshness=%+v err=%v", got, err)
			}
		}
	})

	stagedRoot := filepath.Join(f.worktree, "freshness-staged")
	untrackedRoot := filepath.Join(f.worktree, "freshness-untracked")
	if err := os.MkdirAll(stagedRoot, 0o755); err != nil {
		b.Fatal(err)
	}
	if err := os.MkdirAll(untrackedRoot, 0o755); err != nil {
		b.Fatal(err)
	}
	for i := range 1000 {
		body := []byte(fmt.Sprintf("package fixture\n\nfunc FreshnessFile%04d() {}\n", i))
		if err := os.WriteFile(filepath.Join(stagedRoot, fmt.Sprintf("file_%04d.go", i)), body, 0o644); err != nil {
			b.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(untrackedRoot, fmt.Sprintf("file_%04d.go", i)), body, 0o644); err != nil {
			b.Fatal(err)
		}
	}
	builderGit(b, f.worktree, "add", "--", "freshness-staged")

	b.Run("dirty_2000_tracked_and_untracked", func(b *testing.B) {
		b.ReportAllocs()
		for range b.N {
			got, err := lifecycle.EnsureCheckoutFresh(ctx, f.checkoutID)
			if err != nil || got.Fresh || !got.Building {
				b.Fatalf("freshness=%+v err=%v", got, err)
			}
		}
	})

	b.Run("busy_nonblocking", func(b *testing.B) {
		c.cycleMu.Lock()
		defer c.cycleMu.Unlock()
		b.ReportAllocs()
		for range b.N {
			got, err := lifecycle.EnsureCheckoutFresh(ctx, f.checkoutID)
			if err != nil || got.Fresh || !got.Building {
				b.Fatalf("freshness=%+v err=%v", got, err)
			}
		}
	})
}
