package indexer

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/graphview"
)

func TestAtomicReadyMutationKeepsLegacyBaseOwnerUntilClose(t *testing.T) {
	f, c, l := newCheckoutMutationFixture(t)
	primary, found, err := f.catalog.GetCheckout(t.Context(), f.primaryID)
	require.NoError(t, err)
	require.True(t, found)
	owner := graphview.RepositoryOwner{GraphID: f.graphID, CheckoutID: primary.CheckoutID, Incarnation: primary.Incarnation, RepoPrefix: c.repoPrefix}
	m, err := l.BeginCheckoutByteHandoff(t.Context(), f.checkoutID, f.worktree, byteFixtureIncarnation(t, f))
	require.NoError(t, err)
	require.NotNil(t, m)
	defer m.Close()
	require.Nil(t, m.byteAnchor)
	require.True(t, f.leases.InUse(graphview.BaseCorpusGeneration))
	drain, err := f.leases.CloseRepositoryAdmission(owner)
	require.NoError(t, err)
	select {
	case <-drain.Done():
		t.Fatal("base owner drained while ordinary file authority held its ancestry")
	default:
	}
	m.Close()
	semanticMotionReceive(t, drain.Done())
	require.Zero(t, f.leases.Held(), "closing an unprepared grant releases its entire ancestry")
}

func TestAtomicReadyMutationAdmissionFailuresReleaseCustody(t *testing.T) {
	for _, kind := range []string{"receipt", "incarnation", "cancellation"} {
		t.Run(kind, func(t *testing.T) {
			f, c, l := newCheckoutMutationFixture(t)
			before := f.route()
			path := filepath.Join(f.worktree, "helper.go")
			disk, err := os.ReadFile(path)
			require.NoError(t, err)
			authority := NewOutputGenerationAuthority(f.leases)
			l.mi = &MultiIndexer{}
			l.mi.SetOutputGenerationAuthority(authority)
			ctx := t.Context()
			incarnation := byteFixtureIncarnation(t, f)
			want := ErrCheckoutMutationStale
			switch kind {
			case "receipt":
				authority.Close()
				want = ErrOutputMutationAuthorityClosed
			case "incarnation":
				incarnation = "not-the-selected-incarnation"
			case "cancellation":
				canceled, cancel := context.WithCancel(ctx)
				cancel()
				ctx = canceled
				want = context.Canceled
			}
			m, err := l.BeginCheckoutByteHandoff(ctx, f.checkoutID, f.worktree, incarnation)
			if m != nil {
				m.Close()
			}
			require.Nil(t, m)
			require.ErrorIs(t, err, want)
			require.Zero(t, f.leases.Held())
			require.Zero(t, authority.Stats().LiveOwners)
			require.Equal(t, before, f.route())
			after, err := os.ReadFile(path)
			require.NoError(t, err)
			require.Equal(t, disk, after)
			c.mu.Lock()
			admissions := c.sourceMutations
			c.mu.Unlock()
			require.Zero(t, admissions)
			c.byteMu.Lock()
			demand := c.byteDemand
			c.byteMu.Unlock()
			require.Zero(t, demand)
			require.True(t, c.cycleMu.TryLock(), "failed admission retained the cycle lock")
			c.cycleMu.Unlock()
		})
	}
}

func TestAtomicReadyMutationPinRaceKeepsReceiptUnissued(t *testing.T) {
	f, c, l := newCheckoutMutationFixture(t)
	before := f.route()
	authority := NewOutputGenerationAuthority(f.leases)
	l.mi = &MultiIndexer{}
	l.mi.SetOutputGenerationAuthority(authority)
	checkout, found, err := f.catalog.GetCheckout(t.Context(), f.checkoutID)
	require.NoError(t, err)
	require.True(t, found)
	rootInfo, err := checkoutRootFileInfo(f.worktree)
	require.NoError(t, err)
	require.Zero(t, f.leases.Held())
	// The shared locked helper preserves its caller's lock/source ownership
	// on error. A noncancelable context makes the existing final-pin barrier
	// depend only on the actual full legacy ancestry, not SQL cancel watchers.
	require.True(t, c.admitSourceMutation())
	c.cycleMu.Lock()
	defer func() { c.cycleMu.Unlock(); c.releaseSourceMutation() }()
	barrier := &byteAnchorRereadContext{Context: context.Background(), leases: f.leases, entered: make(chan struct{}), release: make(chan struct{})}
	var once sync.Once
	release := func() { once.Do(func() { close(barrier.release) }) }
	type admission struct {
		m   *CheckoutMutation
		err error
	}
	got := make(chan admission, 1)
	go func() {
		m, err := l.admitCheckoutMutationLocked(barrier, c, checkout, rootInfo, before.RouteEpoch, &before)
		got <- admission{m, err}
	}()
	received := false
	defer func() {
		release()
		if !received {
			result := semanticMotionReceive(t, got)
			if result.m != nil {
				result.m.settleReceipt()
				if result.m.byteRelease != nil {
					result.m.byteRelease()
				}
			}
		}
	}()
	semanticMotionReceive(t, barrier.entered)
	for _, id := range []int64{graphview.BaseCorpusGeneration, before.CommitGenerationID, before.DirtyGenerationID} {
		require.True(t, f.leases.InUse(id))
	}
	require.Zero(t, authority.Stats().Issued, "ancestry is pinned before opening an output receipt")
	require.NoError(t, f.catalog.FlipCheckoutRouteSlot(t.Context(), store_sqlite.FlipCheckoutRouteSlotRequest{CheckoutID: f.checkoutID, Slot: store_sqlite.RouteSlotDirty, GenerationID: before.DirtyGenerationID, ExpectedRouteEpoch: before.RouteEpoch, State: store_sqlite.RouteActive}))
	release()
	result := semanticMotionReceive(t, got)
	received = true
	if result.m != nil {
		result.m.settleReceipt()
		if result.m.byteRelease != nil {
			result.m.byteRelease()
		}
	}
	require.Nil(t, result.m)
	require.ErrorIs(t, result.err, ErrCheckoutMutationRouteMoved)
	require.ErrorIs(t, result.err, graphview.ErrViewBuilding)
	require.Zero(t, authority.Stats().Issued)
	require.Zero(t, f.leases.Held())
	if c.cycleMu.TryLock() {
		c.cycleMu.Unlock()
		t.Fatal("helper released its caller's lock on error")
	}
	c.mu.Lock()
	admissions := c.sourceMutations
	c.mu.Unlock()
	require.Equal(t, 1, admissions, "helper must not release caller source admission")
}

// The ready file-only grant shares ordinary sampled admission when the layout
// cannot provide HEAD evidence. Disk motion is refused before pins or receipts;
// MCP may then use its existing bounded whole-view recomposition.
func TestAtomicReadyMutationSampledDiskMotionReleasesCustody(t *testing.T) {
	f, c, l := newCheckoutMutationFixture(t)
	require.NoError(t, os.Mkdir(filepath.Join(f.primary, ".git", "reftable"), 0755))
	require.False(t, c.sampler.CaptureHeadEvidence().Usable())
	before := f.route()
	target := filepath.Join(f.worktree, "helper.go")
	original, err := os.ReadFile(target)
	require.NoError(t, err)
	unrelated := []byte("package fixture\nfunc UnrelatedSaved() {}\n")
	require.NoError(t, os.WriteFile(filepath.Join(f.worktree, "concurrent.go"), unrelated, 0600))
	authority := NewOutputGenerationAuthority(f.leases)
	l.mi = &MultiIndexer{}
	l.mi.SetOutputGenerationAuthority(authority)
	m, err := l.BeginCheckoutByteHandoff(t.Context(), f.checkoutID, f.worktree, byteFixtureIncarnation(t, f))
	if m != nil {
		m.Close()
	}
	require.Nil(t, m)
	require.ErrorIs(t, err, ErrCheckoutMutationDiskMoved)
	require.Zero(t, authority.Stats().Issued)
	require.Zero(t, authority.Stats().LiveOwners)
	require.Zero(t, f.leases.Held())
	require.Equal(t, before, f.route())
	data, err := os.ReadFile(target)
	require.NoError(t, err)
	require.Equal(t, original, data)
	data, err = os.ReadFile(filepath.Join(f.worktree, "concurrent.go"))
	require.NoError(t, err)
	require.Equal(t, unrelated, data)
	c.mu.Lock()
	admissions := c.sourceMutations
	c.mu.Unlock()
	require.Zero(t, admissions)
	c.byteMu.Lock()
	demand, attempt := c.byteDemand, c.byteAttempt
	c.byteMu.Unlock()
	require.Zero(t, demand)
	require.Nil(t, attempt)
	require.True(t, c.cycleMu.TryLock())
	c.cycleMu.Unlock()
}
