package indexer

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/semantic"
	"go.uber.org/zap"
)

func byteFixtureIncarnation(t *testing.T, f *coordinatorFixture) string {
	t.Helper()
	row, found, err := f.catalog.GetCheckout(t.Context(), f.checkoutID)
	require.NoError(t, err)
	require.True(t, found)
	return row.Incarnation
}

// A byte anchor needs actual unchanged HEAD file evidence. Unsupported
// filesystems retain ordinary sampled admission; exercise that path rather
// than pretending a certificate exists or skipping the mutation contract.
func byteFixtureSupportsAuthorityOrChecksFallback(t *testing.T, f *coordinatorFixture, c *CheckoutCoordinator, l *CheckoutLifecycle) bool {
	t.Helper()
	if c.sampler.CaptureHeadEvidence().Usable() {
		return true
	}
	before := f.route()
	path := filepath.Join(f.worktree, "helper.go")
	original, err := os.ReadFile(path)
	require.NoError(t, err)
	primaryPath := filepath.Join(f.primary, "helper.go")
	primary, err := os.ReadFile(primaryPath)
	require.NoError(t, err)
	held := f.leases.Held()
	func() {
		attempt, finish, ok := c.beginByteMutationAttempt(t.Context())
		require.True(t, ok)
		defer finish()
		m, err := l.BeginCheckoutByteMutation(t.Context(), f.checkoutID, f.worktree, byteFixtureIncarnation(t, f))
		if m != nil {
			m.Close()
		}
		require.Nil(t, m)
		require.ErrorIs(t, err, ErrCheckoutByteAnchorUnavailable)
		require.NoError(t, attempt.Err(), "unsupported evidence must not preempt the registered attempt")
	}()
	require.Equal(t, before, f.route())
	require.Equal(t, held, f.leases.Held())
	c.byteMu.Lock()
	demand := c.byteDemand
	c.byteMu.Unlock()
	require.Zero(t, demand)
	c.mu.Lock()
	admissions := c.sourceMutations
	c.mu.Unlock()
	require.Zero(t, admissions)
	// Ordinary mutation still samples, selects the actual worktree, writes and
	// publishes its exact ticket. No unsupported fast-path claim is made.
	m, err := l.BeginCheckoutMutation(t.Context(), f.checkoutID, f.worktree, before.RouteEpoch)
	require.NoError(t, err)
	defer m.Close()
	require.NoError(t, m.Prepare(t.Context()))
	next := string(original) + "\n// ordinary mutation without HEAD file evidence\n"
	builderWriteFile(t, f.worktree, "helper.go", next)
	ticket, err := m.EnqueueRefresh(t.Context(), path)
	require.NoError(t, err)
	m.Close()
	c.cycle(t.Context())
	result := awaitCheckoutRefresh(t, ticket)
	require.NoError(t, result.Err)
	require.True(t, result.Reindexed)
	disk, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, next, string(disk))
	untouched, err := os.ReadFile(primaryPath)
	require.NoError(t, err)
	require.Equal(t, primary, untouched)
	sample, err := c.sampler.Sample(t.Context())
	require.NoError(t, err)
	row, found := f.generation(f.route().DirtyGenerationID)
	require.True(t, found)
	require.Equal(t, sample.Fingerprint, row.LowerViewFingerprint)
	return false
}

func TestByteMutationUnavailableEvidenceKeepsOrdinaryMutationSafe(t *testing.T) {
	f, c, l := newCheckoutMutationFixture(t)
	forceSampledMutationAdmission(t, f, c)
	require.False(t, byteFixtureSupportsAuthorityOrChecksFallback(t, f, c, l))
}

func TestByteMutationHandoffDrainsAttemptAndPreservesExactTickets(t *testing.T) {
	t.Setenv("GORTEX_ENRICH_MIN_NODES", "0")
	f, c, l := newCheckoutMutationFixture(t)
	if !byteFixtureSupportsAuthorityOrChecksFallback(t, f, c, l) {
		return
	}
	p := newMotionContextProvider(t, c)
	old := queueCheckoutSourceEdit(t, f, l, "package fixture\nfunc FirstHelper() {}\n")
	done := semanticMotionCycle(t, c, t.Context(), p)
	semanticMotionReceive(t, p.started)
	// The route is withdrawn, but its genuine accepted ancestry was protected.
	require.Zero(t, f.route().DirtyGenerationID)
	path := filepath.Join(f.worktree, "byte.go")
	before := []byte("package fixture\nfunc ByteBefore() {}\n")
	require.NoError(t, os.WriteFile(path, before, 0600))
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
	semanticMotionReceive(t, p.canceled)
	assertCheckoutRefreshPending(t, old)
	select {
	case <-got:
		t.Fatal("byte authority returned before canceled provider drained")
	default:
	}
	disk, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, before, disk)
	failedID := p.generation.Load()
	p.unblock()
	semanticMotionReceive(t, done)
	c.publication.mu.Lock()
	stalls := c.publication.consecutive
	c.publication.mu.Unlock()
	require.Zero(t, stalls, "owned preemption must not be accounted as a failed publication or motion")
	result := semanticMotionReceive(t, got)
	require.NoError(t, result.err)
	require.NotNil(t, result.m)
	m := result.m
	defer m.Close()
	assertCheckoutRefreshPending(t, old)
	require.NoError(t, m.Prepare(t.Context()))
	builderWriteFile(t, f.worktree, "byte.go", "package fixture\nfunc ByteAfter() {}\n")
	next, err := m.EnqueueRefresh(t.Context(), path)
	require.NoError(t, err)
	m.Close()
	// No cycle was allowed to start between drain and the end of the lease.
	require.EqualValues(t, 1, p.calls.Load())
	c.cycle(t.Context())
	latest := awaitCheckoutRefresh(t, next)
	require.NoError(t, latest.Err)
	require.True(t, latest.Reindexed)
	previous := awaitCheckoutRefresh(t, old)
	// The later mutation changed byte.go, while this ticket's helper.go bytes
	// survived into the same published sample. Both exact writes are proven.
	require.NoError(t, previous.Err)
	require.True(t, previous.Reindexed)
	require.Equal(t, latest.AppliedGeneration, previous.AppliedGeneration)
	require.NotErrorIs(t, previous.Err, context.Canceled)
	require.Equal(t, old.Ticket.Generation, previous.RequestedGeneration)
	failed, found := f.generation(failedID)
	require.True(t, found)
	require.Equal(t, store_sqlite.ViewGenerationFailed, failed.State)
	sample, err := c.sampler.Sample(t.Context())
	require.NoError(t, err)
	row, _ := f.generation(f.route().DirtyGenerationID)
	require.Equal(t, sample.Fingerprint, row.LowerViewFingerprint)
	disk, err = os.ReadFile(filepath.Join(f.worktree, "helper.go"))
	require.NoError(t, err)
	require.Contains(t, string(disk), "FirstHelper")
	c.byteMu.Lock()
	demand := c.byteDemand
	c.byteMu.Unlock()
	require.Zero(t, demand)
}

func TestByteMutationAuthorityRefusesMotionAndRouteABA(t *testing.T) {
	for _, kind := range []string{"disk", "route", "head", "incarnation"} {
		t.Run(kind, func(t *testing.T) {
			f, c, l := newCheckoutMutationFixture(t)
			if !byteFixtureSupportsAuthorityOrChecksFallback(t, f, c, l) {
				return
			}
			m, err := l.BeginCheckoutByteMutation(t.Context(), f.checkoutID, f.worktree, byteFixtureIncarnation(t, f))
			require.NoError(t, err)
			defer m.Close()
			before, err := os.ReadFile(filepath.Join(f.worktree, "helper.go"))
			require.NoError(t, err)
			switch kind {
			case "disk":
				builderWriteFile(t, f.worktree, "external.go", "package fixture\nfunc External(){}\n")
			case "route":
				r := f.route()
				require.NoError(t, f.catalog.FlipCheckoutRouteSlot(t.Context(), store_sqlite.FlipCheckoutRouteSlotRequest{CheckoutID: f.checkoutID, Slot: store_sqlite.RouteSlotDirty, GenerationID: r.DirtyGenerationID, ExpectedRouteEpoch: r.RouteEpoch, State: store_sqlite.RouteActive}))
			case "head":
				builderGit(t, f.worktree, "checkout", "-b", "moved-byte-head")
			case "incarnation":
				m.checkout.Incarnation = "different-incarnation"
			}
			require.Error(t, m.Prepare(t.Context()))
			after, err := os.ReadFile(filepath.Join(f.worktree, "helper.go"))
			require.NoError(t, err)
			require.Equal(t, before, after)
		})
	}
}

func TestByteMutationRequiresAcceptedAnchorAndReleasesAncestry(t *testing.T) {
	f, c, l := newCheckoutMutationFixture(t)
	if !byteFixtureSupportsAuthorityOrChecksFallback(t, f, c, l) {
		return
	}
	route := f.route()
	require.NoError(t, c.captureByteAnchor(t.Context(), route))
	old := c.byteAnchor
	require.NotEmpty(t, old.ids)
	for _, id := range old.ids {
		require.True(t, f.leases.InUse(id))
	}
	require.NoError(t, c.clearDirtySlot(t.Context(), &route))
	require.Zero(t, c.routedDirty, "routedDirty cannot stand in for authenticated anchor")
	m, err := l.BeginCheckoutByteMutation(t.Context(), f.checkoutID, f.worktree, byteFixtureIncarnation(t, f))
	require.NoError(t, err)
	c.closeByteAnchor()
	for _, id := range old.ids {
		require.True(t, f.leases.InUse(id), "each byte authority has its own ancestry hold")
	}
	m.Close()
	for _, id := range old.ids {
		require.False(t, f.leases.InUse(id))
	}
	_, err = l.BeginCheckoutByteMutation(t.Context(), f.checkoutID, f.worktree, byteFixtureIncarnation(t, f))
	require.ErrorIs(t, err, ErrCheckoutByteAnchorUnavailable)
}

func TestByteMutationAttemptCommitWinsAndErrorsStayIndependent(t *testing.T) {
	for _, committed := range []bool{false, true} {
		t.Run(map[bool]string{false: "precommit", true: "commit_entire_attempt"}[committed], func(t *testing.T) {
			c := &CheckoutCoordinator{}
			parent, cancel := context.WithCancel(t.Context())
			defer cancel()
			ctx, finish, ok := c.beginByteMutationAttempt(parent)
			require.True(t, ok)
			a := ctx.Value(byteMutationAttemptKey{}).(*byteMutationAttempt)
			if committed {
				reachBuildCommitPoint(ctx)
				reachBuildCommitPoint(ctx)
			}
			release, done := c.reserveByteMutation()
			defer release()
			if committed {
				require.NoError(t, ctx.Err())
			} else {
				require.ErrorIs(t, ctx.Err(), context.Canceled)
				out := a.outcome(CheckoutCycle{Err: ctx.Err()})
				require.True(t, out.Rescheduled)
				require.NoError(t, out.Err)
			}
			sentinel := errors.New("independent storage failure")
			require.ErrorIs(t, a.outcome(CheckoutCycle{Err: sentinel}).Err, sentinel)
			cancel()
			require.ErrorIs(t, a.outcome(CheckoutCycle{Err: context.Canceled}).Err, context.Canceled)
			_, _, admitted := c.beginByteMutationAttempt(t.Context())
			require.False(t, admitted, "demand excludes restart")
			finish()
			semanticMotionReceive(t, done)
		})
	}
}

func TestByteMutationPreemptsOwnGateQueueWithoutTakingOtherCheckoutLane(t *testing.T) {
	f, c, l := newCheckoutMutationFixture(t)
	if !byteFixtureSupportsAuthorityOrChecksFallback(t, f, c, l) {
		return
	}
	old := queueCheckoutSourceEdit(t, f, l, "package fixture\nfunc QueuedHelper(){}\n")
	release, err := c.gate.Acquire(t.Context(), ViewBuildInteractive)
	require.NoError(t, err)
	defer release()
	done := make(chan struct{})
	go func() { defer close(done); c.cycle(t.Context()) }()
	require.Eventually(t, func() bool { return c.gate.Stats().InteractiveQueued > 0 }, 10*time.Second, time.Millisecond)
	m, err := l.BeginCheckoutByteHandoff(t.Context(), f.checkoutID, f.worktree, byteFixtureIncarnation(t, f))
	require.NoError(t, err)
	defer m.Close()
	semanticMotionReceive(t, done)
	require.True(t, c.gate.Stats().Active, "the unrelated checkout still owns the lane")
	assertCheckoutRefreshPending(t, old)
	m.Close()
	release()
	c.cycle(t.Context())
	require.NoError(t, awaitCheckoutRefresh(t, old).Err, "no-Prepare close must resume old exact work")
}

func TestByteMutationNoPrepareCloseResumesDrainedOldWork(t *testing.T) {
	t.Setenv("GORTEX_ENRICH_MIN_NODES", "0")
	f, c, l := newCheckoutMutationFixture(t)
	if !byteFixtureSupportsAuthorityOrChecksFallback(t, f, c, l) {
		return
	}
	p := newMotionContextProvider(t, c)
	old := queueCheckoutSourceEdit(t, f, l, "package fixture\nfunc PreservedHelper(){}\n")
	done := semanticMotionCycle(t, c, t.Context(), p)
	semanticMotionReceive(t, p.started)
	incarnation := byteFixtureIncarnation(t, f)
	admitted := make(chan *CheckoutMutation, 1)
	fail := make(chan error, 1)
	go func() {
		m, err := l.BeginCheckoutByteHandoff(t.Context(), f.checkoutID, f.worktree, incarnation)
		if err != nil {
			fail <- err
			return
		}
		admitted <- m
	}()
	semanticMotionReceive(t, p.canceled)
	p.unblock()
	semanticMotionReceive(t, done)
	var m *CheckoutMutation
	select {
	case m = <-admitted:
	case err := <-fail:
		t.Fatal(err)
	case <-time.After(10 * time.Second):
		t.Fatal("no byte admission")
	}
	assertCheckoutRefreshPending(t, old)
	// A wrong target SHA/occurrence/parse rejection has not called Prepare.
	m.Close()
	c.cycle(t.Context())
	result := awaitCheckoutRefresh(t, old)
	require.NoError(t, result.Err)
	require.True(t, result.Reindexed)
	require.Equal(t, old.Ticket.Generation, result.RequestedGeneration)
}

// Immutable commit enrichment runs in its own checkout. Hold only the later
// working-copy semantic phase, after the production builder published commit.
type byteDirtyPhaseProvider struct {
	*motionContextProvider
	root string
}

func (p *byteDirtyPhaseProvider) EnrichRepoContext(ctx context.Context, g graph.Store, lang, root string, policy semantic.EnrichDeadlinePolicy) (*semantic.EnrichResult, error) {
	if !sameMutationRoot(root, p.root) {
		return p.Enrich(g, root)
	}
	return p.motionContextProvider.EnrichRepoContext(ctx, g, lang, root, policy)
}

func TestByteMutationFirstCommitPublicationProtectsLaterDirtyWork(t *testing.T) {
	t.Setenv("GORTEX_ENRICH_MIN_NODES", "0")
	f, c, l := newCheckoutMutationFixture(t)
	prior := f.route()
	p := &motionContextProvider{checkoutEnrichSpy: &checkoutEnrichSpy{}, started: make(chan context.Context, 1), canceled: make(chan struct{}), release: make(chan struct{})}
	mgr := semantic.NewManager(semantic.Config{Enabled: true, CheckoutLSPMaxWorkspaces: 4, Providers: []semantic.ProviderConfig{{Name: "checkout-spy", Languages: []string{"go"}, Priority: 1, Enabled: true}}}, zap.NewNop())
	mgr.RegisterProvider(&byteDirtyPhaseProvider{motionContextProvider: p, root: f.worktree})
	c.builder.Semantic = mgr
	t.Cleanup(func() { p.unblock(); _ = mgr.Close() })
	builderWriteFile(t, f.worktree, "helper.go", "package fixture\nfunc CommittedHelper(){}\n")
	builderGit(t, f.worktree, "add", "helper.go")
	builderGit(t, f.worktree, "commit", "-m", "new checkout commit")
	builderWriteFile(t, f.worktree, "dirty.go", "package fixture\nfunc DirtyAfterCommit(){}\n")
	ticket, err := l.RequestBoundCheckoutRefresh(t.Context(), f.checkoutID, f.worktree)
	require.NoError(t, err)
	done := semanticMotionCycle(t, c, t.Context(), p)
	providerCtx := semanticMotionReceive(t, p.started)
	routed := f.route()
	require.NotEqual(t, prior.CommitGenerationID, routed.CommitGenerationID, "first commit payload must really be published before the barrier")
	row, found := f.generation(routed.CommitGenerationID)
	require.True(t, found)
	require.True(t, servableGeneration(row.State))
	release, attemptDone := c.reserveByteMutation()
	defer release()
	require.NoError(t, providerCtx.Err(), "first publication commits the ENTIRE attempt")
	select {
	case <-p.canceled:
		t.Fatal("later dirty work rearmed byte preemption")
	default:
	}
	select {
	case <-attemptDone:
		t.Fatal("commit-winning cycle must finish normally before admission")
	default:
	}
	p.unblock()
	semanticMotionReceive(t, done)
	semanticMotionReceive(t, attemptDone)
	result := awaitCheckoutRefresh(t, ticket)
	require.NoError(t, result.Err)
	require.True(t, result.Reindexed)
}
