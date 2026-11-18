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

func TestByteHandoffReadyIdleKeepsOrdinaryAuthority(t *testing.T) {
	f, c, l := newCheckoutMutationFixture(t)
	before := f.route()
	require.Equal(t, store_sqlite.RouteActive, before.State)
	require.Positive(t, before.CommitGenerationID)
	require.Positive(t, before.DirtyGenerationID)
	c.byteAnchorMu.Lock()
	anchor := c.byteAnchor
	c.byteAnchorMu.Unlock()
	m, err := l.BeginCheckoutByteHandoff(t.Context(), f.checkoutID, f.worktree, byteFixtureIncarnation(t, f))
	require.NoError(t, err)
	require.NotNil(t, m)
	defer m.Close()
	require.Nil(t, m.byteAnchor)
	require.Nil(t, m.byteSnapshot)
	require.NotNil(t, m.Receipt())
	if c.cycleMu.TryLock() {
		c.cycleMu.Unlock()
		t.Fatal("ordinary grant did not retain cycle lock")
	}
	for _, id := range []int64{graphview.BaseCorpusGeneration, before.CommitGenerationID, before.DirtyGenerationID} {
		require.True(t, f.leases.InUse(id), "ordinary file authority must hold full ancestry")
	}
	require.Equal(t, before, f.route())
	c.byteAnchorMu.Lock()
	unchangedAnchor := anchor == c.byteAnchor
	c.byteAnchorMu.Unlock()
	require.True(t, unchangedAnchor, "an ordinary grant must not capture a byte anchor")
	c.byteMu.Lock()
	attempt, demand := c.byteAttempt, c.byteDemand
	c.byteMu.Unlock()
	require.Nil(t, attempt)
	require.Zero(t, demand)
	c.mu.Lock()
	admissions := c.sourceMutations
	c.mu.Unlock()
	require.Equal(t, 1, admissions)

	// No graph reader was needed to grant ordinary authority; its original
	// fresh whole-checkout/route checks still run before this real WT write.
	path := filepath.Join(f.worktree, "helper.go")
	primary, err := os.ReadFile(filepath.Join(f.primary, "helper.go"))
	require.NoError(t, err)
	require.NoError(t, m.Prepare(t.Context()))
	builderWriteFile(t, f.worktree, "helper.go", "package fixture\nfunc IdleOrdinaryHelper() {}\n")
	ticket, err := m.EnqueueRefresh(t.Context(), path)
	require.NoError(t, err)
	m.Close()
	c.cycle(t.Context())
	result := awaitCheckoutRefresh(t, ticket)
	require.NoError(t, result.Err)
	require.True(t, result.Reindexed)
	disk, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Contains(t, string(disk), "IdleOrdinaryHelper")
	unchanged, err := os.ReadFile(filepath.Join(f.primary, "helper.go"))
	require.NoError(t, err)
	require.Equal(t, primary, unchanged)
	sample, err := c.sampler.Sample(t.Context())
	require.NoError(t, err)
	row, found := f.generation(f.route().DirtyGenerationID)
	require.True(t, found)
	require.Equal(t, sample.Fingerprint, row.LowerViewFingerprint)
}

func TestByteHandoffRetainsPendingAttemptAndDemand(t *testing.T) {
	for _, kind := range []string{"pending", "registered attempt", "existing demand"} {
		t.Run(kind, func(t *testing.T) {
			f, c, l := newCheckoutMutationFixture(t)
			if !byteFixtureSupportsAuthorityOrChecksFallback(t, f, c, l) {
				return
			}
			route := f.route()
			var release func()
			var attempt context.Context
			switch kind {
			case "pending":
				require.NoError(t, c.clearDirtySlot(t.Context(), &route))
				require.Equal(t, store_sqlite.RoutePending, route.State)
			case "existing demand":
				require.NoError(t, c.captureByteAnchor(t.Context(), route))
				release, _ = c.reserveByteMutation()
				defer release()
			default:
				var finish func()
				var ok bool
				attempt, finish, ok = c.beginByteMutationAttempt(t.Context())
				require.True(t, ok)
				release = finish
			}
			type admission struct {
				m   *CheckoutMutation
				err error
			}
			got := make(chan admission, 1)
			incarnation := byteFixtureIncarnation(t, f)
			go func() {
				m, err := l.BeginCheckoutByteHandoff(t.Context(), f.checkoutID, f.worktree, incarnation)
				got <- admission{m, err}
			}()
			received := false
			defer func() {
				if release != nil {
					release()
				}
				if !received {
					result := semanticMotionReceive(t, got)
					if result.m != nil {
						result.m.Close()
					}
				}
			}()
			if attempt != nil {
				semanticMotionReceive(t, attempt.Done())
				require.ErrorIs(t, context.Cause(attempt), errByteMutationHandoff)
				select {
				case early := <-got:
					received = true
					if early.m != nil {
						early.m.Close()
					}
					t.Fatal("handoff returned before registered attempt drained")
				default:
				}
				release()
				release = nil
			}
			result := semanticMotionReceive(t, got)
			received = true
			require.NoError(t, result.err)
			require.NotNil(t, result.m)
			defer result.m.Close()
			require.NotNil(t, result.m.byteAnchor)
			require.NotNil(t, result.m.byteSnapshot)
			result.m.Close()
			if release != nil {
				release()
			}
			c.byteMu.Lock()
			demand := c.byteDemand
			c.byteMu.Unlock()
			require.Zero(t, demand)
		})
	}
}

func TestAtomicReadyMutationSerializesLaterCycle(t *testing.T) {
	f, c, l := newCheckoutMutationFixture(t)
	m, err := l.BeginCheckoutByteHandoff(t.Context(), f.checkoutID, f.worktree, byteFixtureIncarnation(t, f))
	require.NoError(t, err)
	require.NotNil(t, m)
	defer m.Close()
	require.Nil(t, m.byteAnchor)
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	c.cyclePreflight = func(context.Context) (CheckoutCycle, bool) {
		close(entered)
		<-release
		return CheckoutCycle{}, false
	}
	done := make(chan struct{})
	go func() { defer close(done); c.cycle(t.Context()) }()
	defer func() { m.Close(); unblock(); semanticMotionReceive(t, done); c.cyclePreflight = nil }()
	semanticMotionReceive(t, entered)
	// A real cycle registered after the atomic grant. It may preflight, but
	// cannot obtain the cycle lock or publish over this admitted mutation.
	if c.cycleMu.TryLock() {
		c.cycleMu.Unlock()
		t.Fatal("later cycle found an unowned mutation lock")
	}
	unblock()
	require.NoError(t, m.Prepare(t.Context()))
	builderWriteFile(t, f.worktree, "helper.go", "package fixture\nfunc AtomicOrdinaryHelper() {}\n")
	ticket, err := m.EnqueueRefresh(t.Context(), filepath.Join(f.worktree, "helper.go"))
	require.NoError(t, err)
	require.Equal(t, store_sqlite.RoutePending, f.route().State)
	// A new ticket may legitimately preempt the background preflight; even
	// then it cannot publish while this mutation owns the cycle lock.
	m.Close()
	semanticMotionReceive(t, done)
	c.cyclePreflight = nil
	// A ticket admitted after the earlier cycle's watermark may need the
	// original next cycle; do not manufacture completion on that earlier one.
	c.cycle(t.Context())
	result := awaitCheckoutRefresh(t, ticket)
	require.NoError(t, result.Err)
	require.True(t, result.Reindexed)
	sample, err := c.sampler.Sample(t.Context())
	require.NoError(t, err)
	row, found := f.generation(f.route().DirtyGenerationID)
	require.True(t, found)
	require.Equal(t, sample.Fingerprint, row.LowerViewFingerprint)
}
