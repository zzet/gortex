package indexer

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/gitstate"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/semantic"
)

type motionContextProvider struct {
	*checkoutEnrichSpy
	started     chan context.Context
	canceled    chan struct{}
	release     chan struct{}
	releaseOnce sync.Once
	calls       atomic.Int32
	generation  atomic.Int64
}

func newMotionContextProvider(t *testing.T, c *CheckoutCoordinator) *motionContextProvider {
	t.Helper()
	p := &motionContextProvider{checkoutEnrichSpy: &checkoutEnrichSpy{}, started: make(chan context.Context, 1), canceled: make(chan struct{}), release: make(chan struct{})}
	mgr := semantic.NewManager(semantic.Config{Enabled: true, CheckoutLSPMaxWorkspaces: 4,
		Providers: []semantic.ProviderConfig{{Name: "checkout-spy", Languages: []string{"go"}, Priority: 1, Enabled: true}}}, zap.NewNop())
	mgr.RegisterProvider(p)
	c.builder.Semantic = mgr
	t.Cleanup(func() { p.unblock(); _ = mgr.Close() })
	return p
}

func (p *motionContextProvider) unblock() { p.releaseOnce.Do(func() { close(p.release) }) }

func (p *motionContextProvider) EnrichRepoContext(ctx context.Context, g graph.Store, _, root string, _ semantic.EnrichDeadlinePolicy) (*semantic.EnrichResult, error) {
	p.calls.Add(1)
	p.generation.Store(g.(interface{ ViewGeneration() int64 }).ViewGeneration())
	p.started <- ctx
	select {
	case <-ctx.Done():
		close(p.canceled)
		// The coordinator must drain the actual provider before retrying or
		// publishing, even after the cooperative cancellation was observed.
		<-p.release
		return nil, ctx.Err()
	case <-p.release:
		return p.Enrich(g, root)
	}
}

func semanticMotionReceive[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case value := <-ch:
		return value
	case <-time.After(10 * time.Second):
		t.Fatal("semantic motion barrier did not finish")
		var zero T
		return zero
	}
}

func semanticMotionCycle(t *testing.T, c *CheckoutCoordinator, ctx context.Context, p *motionContextProvider) <-chan struct{} {
	t.Helper()
	done := make(chan struct{})
	go func() { defer close(done); c.cycle(ctx) }()
	t.Cleanup(func() { p.unblock(); semanticMotionReceive(t, done) })
	return done
}

func TestVerifiedSemanticMotionCancelsOnlyAttemptAndPublishesTicket(t *testing.T) {
	t.Setenv("GORTEX_ENRICH_MIN_NODES", "0")
	f, c, lifecycle := newCheckoutMutationFixture(t)
	p := newMotionContextProvider(t, c)
	parent, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	ticket := queueCheckoutSourceEdit(t, f, lifecycle, "package fixture\nfunc FirstHelper() {}\n")
	done := semanticMotionCycle(t, c, parent, p)
	providerCtx := semanticMotionReceive(t, p.started)
	deadline, ok := parent.Deadline()
	gotDeadline, gotOK := providerCtx.Deadline()
	require.Equal(t, ok, gotOK)
	require.True(t, !gotDeadline.After(deadline), "the provider cannot extend the caller deadline")
	c.motion.mu.Lock()
	attemptCtx := c.motion.semantic.ctx
	c.motion.mu.Unlock()
	attemptDeadline, _ := attemptCtx.Deadline()
	require.Equal(t, deadline, attemptDeadline, "the attempt retains the caller deadline")
	builderWriteFile(t, f.worktree, "helper.go", "package fixture\nfunc LatestHelper() {}\n")
	c.noteFilesystemChange([]string{filepath.Join(f.worktree, "helper.go")})
	semanticMotionReceive(t, p.canceled)
	require.NoError(t, parent.Err(), "motion must not cancel the ticket's parent")
	assertCheckoutRefreshPending(t, ticket)
	// The saved FirstHelper ticket promises those exact bytes, so the new
	// save must supersede it. A require_fresh waiter binds at completion and
	// can ride the retry sample begun after this admission.
	latest, err := lifecycle.RequestBoundCheckoutRefresh(parent, f.checkoutID, f.worktree)
	require.NoError(t, err)
	assertCheckoutRefreshPending(t, latest)
	select {
	case <-done:
		t.Fatal("the cycle did not drain its canceled provider")
	default:
	}
	p.unblock()
	semanticMotionReceive(t, done)
	superseded := awaitCheckoutRefresh(t, ticket)
	require.ErrorIs(t, superseded.Err, ErrCheckoutRefreshSuperseded, "a different save must not certify the original edit")
	require.NotErrorIs(t, superseded.Err, context.Canceled, "attempt cancellation must not cancel the parent ticket")
	require.False(t, superseded.Reindexed)
	require.Zero(t, superseded.AppliedGeneration)
	require.Equal(t, ticket.Ticket.Generation, superseded.RequestedGeneration)
	result := awaitCheckoutRefresh(t, latest)
	require.NoError(t, result.Err)
	require.True(t, result.Reindexed)
	require.Equal(t, latest.Ticket.Generation, result.RequestedGeneration)
	require.Equal(t, uint64(f.route().DirtyGenerationID), result.AppliedGeneration)
	require.EqualValues(t, 1, p.calls.Load(), "the retry must defer enrichment instead of rerunning the moving provider")
	after, err := c.sampler.Sample(t.Context())
	require.NoError(t, err)
	failed, found := f.generation(p.generation.Load())
	require.True(t, found)
	require.Equal(t, store_sqlite.ViewGenerationFailed, failed.State, "the canceled private generation cannot be served or chained")
	require.NotEqual(t, p.generation.Load(), f.route().DirtyGenerationID)
	row, _ := f.generation(f.route().DirtyGenerationID)
	require.Equal(t, after.Fingerprint, row.LowerViewFingerprint)
	require.True(t, c.enrichmentDeferredByMotion())
	pending, err := c.SemanticPending(t.Context())
	require.NoError(t, err)
	require.NotEmpty(t, pending.Paths, "the canceled enrichment remains owed")
	c.motion.mu.Lock()
	armed := c.motion.semantic
	c.motion.mu.Unlock()
	require.Nil(t, armed, "the arm cannot survive into publication")
}

func TestSemanticMotionHintsNeedDifferentSuccessfulSample(t *testing.T) {
	t.Setenv("GORTEX_ENRICH_MIN_NODES", "0")
	for _, ignored := range []bool{false, true} {
		name := "same content"
		if ignored {
			name = "ignored file"
		}
		t.Run(name, func(t *testing.T) {
			f, c, lifecycle := newCheckoutMutationFixture(t)
			if ignored {
				exclude := strings.TrimSpace(builderGit(t, f.worktree, "rev-parse", "--git-path", "info/exclude"))
				if !filepath.IsAbs(exclude) {
					exclude = filepath.Join(f.worktree, exclude)
				}
				require.NoError(t, os.MkdirAll(filepath.Dir(exclude), 0o755))
				require.NoError(t, os.WriteFile(exclude, []byte("ignored-motion.tmp\n"), 0o644))
			}
			p := newMotionContextProvider(t, c)
			parent, cancel := context.WithCancel(t.Context())
			defer cancel()
			ticket := queueCheckoutSourceEdit(t, f, lifecycle, "package fixture\nfunc FirstHelper() {}\n")
			done := semanticMotionCycle(t, c, parent, p)
			providerCtx := semanticMotionReceive(t, p.started)
			c.motion.mu.Lock()
			arm := c.motion.semantic
			c.motion.mu.Unlock()
			require.NotNil(t, arm, "the real semantic call must arm verification")
			path := filepath.Join(f.worktree, "helper.go")
			if ignored {
				path = filepath.Join(f.worktree, "ignored-motion.tmp")
				require.NoError(t, os.WriteFile(path, []byte("ignored"), 0o644))
			}
			c.noteFilesystemChange([]string{path})
			arm.mu.Lock()
			verified := arm.done
			arm.mu.Unlock()
			require.NotNil(t, verified)
			semanticMotionReceive(t, verified)
			require.NoError(t, providerCtx.Err(), "a watcher hint is not proof of a changed input")
			// A second hint cannot launch another verifier in this attempt.
			c.noteFilesystemChange([]string{path})
			arm.mu.Lock()
			sameVerifier := arm.done == verified
			arm.mu.Unlock()
			require.True(t, sameVerifier)
			p.unblock()
			semanticMotionReceive(t, done)
			require.NoError(t, awaitCheckoutRefresh(t, ticket).Err)
			require.False(t, c.enrichmentDeferredByMotion())
			require.EqualValues(t, 1, p.calls.Load())
			// After the semantic call, even real movement cannot cancel its context.
			builderWriteFile(t, f.worktree, "helper.go", "package fixture\nfunc AfterPublication() {}\n")
			c.noteFilesystemChange([]string{filepath.Join(f.worktree, "helper.go")})
			require.NotErrorIs(t, context.Cause(providerCtx), arm, "a stopped arm must never fire after publication")
		})
	}
}

func TestSemanticMotionVerifierStopsAndPreservesIndependentErrors(t *testing.T) {
	root, sampler := motionRepo(t)
	before, err := sampler.Sample(t.Context())
	require.NoError(t, err)
	c := &CheckoutCoordinator{root: root, sampler: sampler}
	for _, mode := range []string{"sample failure", "stop before result", "provider error", "storage error", "parent canceled after motion", "parent deadline after motion", "parent deadline"} {
		t.Run(mode, func(t *testing.T) {
			parent, cancelParent := context.WithCancel(t.Context())
			defer cancelParent()
			if mode == "parent deadline" {
				var stop context.CancelFunc
				parent, stop = context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
				defer stop()
			}
			ctx, cancel := context.WithCancelCause(parent)
			defer cancel(nil)
			arm := &semanticMotionAttempt{ctx: ctx, cancel: cancel}
			finish := c.armSemanticMotion(arm, before, time.Now())
			entered, release := make(chan struct{}), make(chan struct{})
			sentinel := errors.New("independent failure")
			arm.sample = func(ctx context.Context) (gitstate.DirtySnapshot, error) {
				close(entered)
				if mode == "stop before result" {
					<-ctx.Done()
				} else {
					<-release
				}
				if mode == "sample failure" {
					return gitstate.DirtySnapshot{}, sentinel
				}
				return gitstate.DirtySnapshot{Fingerprint: "different-successful-sample"}, nil
			}
			if mode == "parent deadline" {
				arm.verify()
				finish(nil)
				require.ErrorIs(t, arm.buildError(parent, 1, root, context.DeadlineExceeded), context.DeadlineExceeded)
				return
			}
			arm.verify()
			semanticMotionReceive(t, entered)
			if mode == "stop before result" {
				finish(nil)
				require.NoError(t, ctx.Err())
				return
			}
			close(release)
			semanticMotionReceive(t, arm.done)
			providerErr := error(nil)
			if mode == "provider error" {
				providerErr = sentinel
			}
			finish(providerErr)
			switch mode {
			case "sample failure":
				require.NoError(t, ctx.Err())
			case "provider error":
				require.ErrorIs(t, arm.buildError(parent, 1, root, context.Canceled), sentinel)
			case "storage error":
				require.ErrorIs(t, arm.buildError(parent, 1, root, sentinel), sentinel)
			case "parent deadline after motion":
				// Deterministically exercise the classifier after its owned cause
				// was recorded, without waiting for a wall-clock race.
				expired, stop := context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
				defer stop()
				require.ErrorIs(t, arm.buildError(expired, 1, root, context.Canceled), context.DeadlineExceeded)
				require.ErrorIs(t, arm.buildError(expired, 1, root, sentinel), sentinel, "independent storage errors retain their identity")
			case "parent canceled after motion":
				cancelParent()
				require.ErrorIs(t, arm.buildError(parent, 1, root, context.Canceled), context.Canceled)
				require.NotErrorIs(t, arm.buildError(parent, 1, root, context.Canceled), ErrDirtySnapshotChanged)
			}
		})
	}
}
