package indexer

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/semantic"
	"go.uber.org/zap"
)

func newContractDemandSemanticFixture(t *testing.T) (*coordinatorFixture, *CheckoutCoordinator, *CheckoutLifecycle, *motionContextProvider, *linkedHoldProbe) {
	t.Helper()
	t.Setenv("GORTEX_ENRICH_MIN_NODES", "0")
	t.Setenv("GORTEX_ENRICHMENT_FOLLOWUP", "")
	f, c, lifecycle := newCheckoutMutationFixture(t)
	lifecycle.SetBuildGate(c.gate)
	c.followup.manual.Store(true)
	c.followup.quiet = -1
	p := newMotionContextProvider(t, c)
	h := newLinkedHoldProbe(t, ContractLinkedIdle, func(options *ContractAnalysisCoordinatorOptions) {
		options.Demand = func() (bool, uint64) { return lifecycle.EditCycleActive(), 0 }
		options.LinkedInterest = lifecycle.ContractInterest
	})
	lifecycle.SetContractAnalysisCoordinator(h.c)
	return f, c, lifecycle, p, h
}

// Contract interest must not need a second save to release an unchanged
// checkout's blocked semantic load. The canceled private generation is never
// published: the exact ticket retries with truthful semantic debt instead.
func TestContractDemandDefersUnchangedCheckoutSemanticAttempt(t *testing.T) {
	for _, reopen := range []bool{false, true} {
		t.Run(map[bool]string{false: "current_owner", true: "reopened_without_new_save"}[reopen], func(t *testing.T) {
			contractDemandDefersUnchangedCheckout(t, reopen)
		})
	}
}

type contractDemandDeferredCase struct {
	f         *coordinatorFixture
	c         *CheckoutCoordinator
	lifecycle *CheckoutLifecycle
	provider  *motionContextProvider
	scheduler *linkedHoldProbe
	parent    context.Context
	cancel    context.CancelFunc
	label     string
	pending   DerivedDebt
}

func prepareContractDemandDeferredCase(t *testing.T) *contractDemandDeferredCase {
	t.Helper()
	f, c, lifecycle, p, h := newContractDemandSemanticFixture(t)
	parent, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	ticket := queueCheckoutSourceEdit(t, f, lifecycle, "package fixture\nfunc UnchangedHelper() {}\n")
	done := make(chan struct{})
	go func() { defer close(done); c.cycle(parent) }()
	t.Cleanup(func() {
		cancel()
		p.unblock()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Error("owned checkout cycle did not drain")
		}
	})
	providerCtx := semanticMotionReceive(t, p.started)
	require.NoError(t, providerCtx.Err())
	require.True(t, lifecycle.EditCycleActive())
	before, err := c.sampler.Sample(parent)
	require.NoError(t, err)
	route := f.route()
	label := builderRepoPrefix + "/" + f.checkoutID + "@" + before.Fingerprint
	h.hold(label)
	var first ContractScheduleResult
	for i := range 4 {
		result := h.scheduleLinked(parent, f.checkoutID, builderRepoPrefix, before.Fingerprint, false)
		if i == 0 {
			require.Equal(t, ContractScheduled, result.Result)
			first = result
		} else {
			require.Equal(t, ContractCoalesced, result.Result)
			require.Equal(t, first.Actor, result.Actor)
			require.Equal(t, first.ArrivalSeq, result.ArrivalSeq)
		}
	}
	require.Eventually(t, func() bool { return h.held(first.Actor) }, time.Second, time.Millisecond)
	require.Empty(t, h.started(), "the private generation still owns the edit lane")
	after, err := c.sampler.Sample(parent)
	require.NoError(t, err)
	require.Equal(t, before.Fingerprint, after.Fingerprint, "contract interest is not source motion")
	select {
	case <-p.canceled:
	case <-time.After(time.Second):
		t.Fatal("admitted same-checkout contract demand did not cancel the blocked semantic attempt")
	}
	require.NoError(t, parent.Err(), "attempt deferral must preserve the exact ticket's parent")
	assertCheckoutRefreshPending(t, ticket)
	require.Equal(t, route, f.route(), "provider cancellation alone cannot publish")
	select {
	case <-done:
		t.Fatal("the retry did not drain its actual semantic provider")
	default:
	}
	p.unblock()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("same-byte deferred retry did not finish")
	}
	result := awaitCheckoutRefresh(t, ticket)
	require.NoError(t, result.Err)
	require.True(t, result.Reindexed)
	require.Equal(t, ticket.Ticket.Generation, result.RequestedGeneration)
	require.Equal(t, uint64(f.route().DirtyGenerationID), result.AppliedGeneration)
	failed, found := f.generation(p.generation.Load())
	require.True(t, found)
	require.Equal(t, store_sqlite.ViewGenerationFailed, failed.State)
	require.NotEqual(t, p.generation.Load(), f.route().DirtyGenerationID)
	require.EqualValues(t, 1, p.calls.Load(), "the retry owes enrichment rather than reentering the provider")
	require.False(t, c.enrichmentDeferredByMotion(), "unchanged contract demand must not be labeled motion")
	pending, err := c.SemanticPending(parent)
	require.NoError(t, err)
	require.NotEmpty(t, pending.Paths)
	return &contractDemandDeferredCase{f: f, c: c, lifecycle: lifecycle, provider: p, scheduler: h, parent: parent, cancel: cancel, label: label, pending: pending}
}

func contractDemandDefersUnchangedCheckout(t *testing.T, reopen bool) {
	t.Helper()
	q := prepareContractDemandDeferredCase(t)
	f, c, lifecycle, p, h, parent, label := q.f, q.c, q.lifecycle, q.provider, q.scheduler, q.parent, q.label
	cancel := q.cancel
	pending := q.pending
	if reopen {
		require.NoError(t, c.Close())
		cycles := make(chan CheckoutCycle, 1)
		c = f.coordinator(t, CheckoutCoordinatorConfig{Builder: c.builder, Gate: c.gate,
			ContractAnalysis: h.c, Debounce: time.Hour, debounceDemand: true,
			cycleDone: func(out CheckoutCycle) { cycles <- out }})
		c.followup.quiet = -1
		lifecycle.coordMu.Lock()
		lifecycle.coordinators[f.checkoutID] = c
		lifecycle.coordMu.Unlock()
		require.False(t, c.defersEnrichment(), "new owner has no in-memory deferral")
		require.True(t, c.pendingContractWork())
		// No new save or ticket: reuse the exact persisted routed generation.
		c.cycle(parent)
		var out CheckoutCycle
		select {
		case out = <-cycles:
		case <-time.After(time.Second):
			t.Fatal("reopened cycle returned without its completion callback")
		}
		require.True(t, c.recoveredEnrichmentDebt(), "reopened cycle: %+v", out)
		require.False(t, c.enrichmentDeferredForContract(), "recovery must not invent a demand cancellation")
		require.False(t, c.enrichmentDeferredByMotion())
		require.EqualValues(t, 1, p.calls.Load(), "route recovery must not start semantic work ahead of the pending fill")
	}
	complete, err := c.SemanticComplete(parent, nil)
	require.NoError(t, err)
	require.False(t, complete, "the deferred generation must report semantic debt")
	routed := f.route()
	followup := make(chan EnrichmentFollowupOutcome, 1)
	if reopen {
		go func() {
			c.waitEnrichmentFollowups()
			if c.EnrichmentFollowupStats().Landed == 1 {
				followup <- followupLanded
			} else {
				followup <- followupFailed
			}
		}()
	} else {
		go func() { followup <- c.runEnrichmentFollowup(parent) }()
	}
	t.Cleanup(func() {
		cancel()
		h.release(label)
		select {
		case <-followup:
		case <-time.After(2 * time.Second):
			t.Error("owned semantic follow-up did not drain")
		}
	})
	h.advance(5 * time.Second)
	h.wake(builderRepoPrefix)
	require.Eventually(t, func() bool { return len(h.started()) == 1 }, time.Second, time.Millisecond)
	select {
	case <-followup:
		t.Fatal("semantic debt resumed while its matching contract fill was still running")
	case <-time.After(50 * time.Millisecond):
	}
	require.Equal(t, routed, f.route())
	require.EqualValues(t, 1, p.calls.Load())
	h.release(label)
	require.Eventually(t, func() bool { return !c.pendingContractWork() }, time.Second, time.Millisecond)
	select {
	case outcome := <-followup:
		require.Equal(t, followupLanded, outcome)
		// Put the terminal outcome back for the cleanup's owned join.
		followup <- outcome
	case <-time.After(2 * time.Second):
		t.Fatal("semantic follow-up did not resume after matching contract work ended")
	}
	pending, err = c.SemanticPending(parent)
	require.NoError(t, err)
	require.Empty(t, pending.Paths)
	complete, err = c.SemanticComplete(parent, nil)
	require.NoError(t, err)
	require.True(t, complete)
	require.False(t, c.enrichmentDeferredForContract(), "paid debt restores the demandless default")
	require.False(t, c.recoveredEnrichmentDebt())
	view := chainMaterialize(t, f)
	defer view.Close()
	cleanSemantic := semantic.NewManager(semantic.Config{Enabled: true,
		Providers: []semantic.ProviderConfig{{Name: "checkout-spy", Languages: []string{"go"}, Priority: 1, Enabled: true}}}, zap.NewNop())
	cleanSemantic.RegisterProvider(&checkoutEnrichSpy{})
	t.Cleanup(func() { _ = cleanSemantic.Close() })
	kindParityCheckIndexed(t, "contract-demand follow-up", f.worktree, view.Reader, nil,
		func() graph.Reader { return f.store }, builderIndexSemantic(cleanSemantic))
}

func TestNoContractDemandKeepsDefaultSynchronousSemanticAttempt(t *testing.T) {
	for _, wrongCheckout := range []bool{false, true} {
		t.Run(map[bool]string{false: "no_interest", true: "other_checkout"}[wrongCheckout], func(t *testing.T) {
			contractDemandDoesNotDefer(t, wrongCheckout)
		})
	}
}

func contractDemandDoesNotDefer(t *testing.T, wrongCheckout bool) {
	t.Helper()
	f, c, lifecycle, p, h := newContractDemandSemanticFixture(t)
	parent, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	ticket := queueCheckoutSourceEdit(t, f, lifecycle, "package fixture\nfunc NoDemandHelper() {}\n")
	done := make(chan struct{})
	go func() { defer close(done); c.cycle(parent) }()
	t.Cleanup(func() {
		cancel()
		p.unblock()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Error("owned demandless checkout cycle did not drain")
		}
	})
	semanticMotionReceive(t, p.started)
	if wrongCheckout {
		result := h.scheduleLinked(parent, "other-checkout", builderRepoPrefix, "other-key", false)
		require.Equal(t, ContractScheduled, result.Result)
		require.True(t, h.c.PendingLinkedWork("other-checkout", builderRepoPrefix))
	}
	require.False(t, c.pendingContractWork())
	select {
	case <-p.canceled:
		t.Fatal("demandless default unexpectedly deferred semantic enrichment")
	case <-time.After(100 * time.Millisecond):
	}
	assertCheckoutRefreshPending(t, ticket)
	p.unblock()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("unblocked default semantic attempt did not finish")
	}
	require.NoError(t, awaitCheckoutRefresh(t, ticket).Err)
	require.False(t, c.enrichmentDeferredByMotion())
	require.False(t, c.enrichmentDeferredForContract())
	require.EqualValues(t, 1, p.calls.Load())
}

func TestContractSemanticDemandPreservesIndependentErrors(t *testing.T) {
	for _, mode := range []string{"owned", "wrapped_cancellation", "provider_error", "joined_provider_error", "storage_error", "joined_storage_error", "parent_canceled", "parent_deadline", "other_attempt", "stopped"} {
		t.Run(mode, func(t *testing.T) {
			parent, cancelParent := context.WithCancel(t.Context())
			defer cancelParent()
			ctx, cancel := context.WithCancelCause(parent)
			defer cancel(nil)
			a := &semanticMotionAttempt{ctx: ctx, cancel: cancel, stopped: mode == "stopped"}
			if mode == "other_attempt" {
				cancel(&contractSemanticDemand{attempt: &semanticMotionAttempt{}})
			} else {
				a.deferForContractDemand()
			}
			if mode == "stopped" {
				require.NoError(t, ctx.Err(), "finished attempt cannot be canceled after publication")
				return
			}
			sentinel := errors.New("independent failure")
			err := error(context.Canceled)
			want := error(nil)
			switch mode {
			case "wrapped_cancellation":
				err, a.providerErr = fmt.Errorf("provider: %w", context.Canceled), fmt.Errorf("provider: %w", context.Canceled)
			case "provider_error":
				a.providerErr, want = sentinel, sentinel
			case "joined_provider_error":
				a.providerErr = errors.Join(context.Canceled, sentinel)
				want = sentinel
			case "storage_error":
				err, want = sentinel, sentinel
			case "joined_storage_error":
				err, want = errors.Join(context.Canceled, sentinel), sentinel
			case "parent_canceled":
				cancelParent()
				want = context.Canceled
			case "parent_deadline":
				var stop context.CancelFunc
				parent, stop = context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
				defer stop()
				want = context.DeadlineExceeded
			case "other_attempt":
				want = context.Canceled
			}
			got := a.buildError(parent, 1, "checkout", err)
			if mode == "owned" || mode == "wrapped_cancellation" {
				var demand *contractSemanticDemand
				require.ErrorAs(t, got, &demand)
				require.Same(t, a, demand.attempt)
				require.NoError(t, parent.Err())
			} else {
				require.ErrorIs(t, got, want)
				var demand *contractSemanticDemand
				require.False(t, errors.As(got, &demand))
			}
			require.NotErrorIs(t, got, ErrDirtySnapshotChanged, "demand is never source movement")
		})
	}
}

type contractDemandTerminalProvider struct {
	*motionContextProvider
	independent  error
	currentCause bool
}

func (p *contractDemandTerminalProvider) EnrichRepoContext(ctx context.Context, g graph.Store, repo, root string, policy semantic.EnrichDeadlinePolicy) (*semantic.EnrichResult, error) {
	result, err := p.motionContextProvider.EnrichRepoContext(ctx, g, repo, root, policy)
	if err != nil {
		if p.currentCause {
			return nil, context.Cause(ctx)
		}
		if p.independent != nil {
			return nil, errors.Join(err, p.independent)
		}
		return nil, errors.Join(err, fmt.Errorf("provider: %w", err))
	}
	return result, nil
}

func TestContractDemandRealManagerPreservesProviderTerminalIdentity(t *testing.T) {
	for _, mode := range []string{"mixed_provider_error", "pure_joined_cancellation", "current_context_cause", "foreign_context_cause"} {
		t.Run(mode, func(t *testing.T) {
			f, c, lifecycle, p, h := newContractDemandSemanticFixture(t)
			sentinel := errors.New("independent provider failure during owned cancellation")
			if mode == "foreign_context_cause" {
				sentinel = &contractSemanticDemand{attempt: &semanticMotionAttempt{}}
			}
			mixed := mode == "mixed_provider_error" || mode == "foreign_context_cause"
			provider := &contractDemandTerminalProvider{motionContextProvider: p, currentCause: mode == "current_context_cause"}
			if mixed {
				provider.independent = sentinel
			}
			mgr := semantic.NewManager(semantic.Config{Enabled: true, CheckoutLSPMaxWorkspaces: 4,
				Providers: []semantic.ProviderConfig{{Name: provider.Name(), Languages: []string{"go"}, Priority: 1, Enabled: true}}}, zap.NewNop())
			mgr.RegisterProvider(provider)
			c.builder.Semantic = mgr
			t.Cleanup(func() { _ = mgr.Close() })
			parent, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			ticket := queueCheckoutSourceEdit(t, f, lifecycle, "package fixture\nfunc ProviderIdentityHelper() {}\n")
			before := f.route()
			done := make(chan struct{})
			go func() { defer close(done); c.cycle(parent) }()
			t.Cleanup(func() {
				cancel()
				p.unblock()
				select {
				case <-done:
				case <-time.After(2 * time.Second):
					t.Error("owned error-provider cycle did not drain")
				}
			})
			semanticMotionReceive(t, p.started)
			sample, err := c.sampler.Sample(parent)
			require.NoError(t, err)
			require.Equal(t, ContractScheduled, h.scheduleLinked(parent, f.checkoutID, builderRepoPrefix, sample.Fingerprint, false).Result)
			select {
			case <-p.canceled:
			case <-time.After(time.Second):
				t.Fatal("owned demand did not reach provider")
			}
			require.NoError(t, parent.Err())
			require.Equal(t, before, f.route())
			p.unblock()
			select {
			case <-done:
			case <-time.After(2 * time.Second):
				t.Fatal("drained error-provider cycle did not finish")
			}
			result := awaitCheckoutRefresh(t, ticket)
			if mixed {
				require.ErrorIs(t, result.Err, sentinel, "real Manager must not erase independent terminal provider failure")
				require.False(t, result.Reindexed)
				require.Zero(t, result.AppliedGeneration)
				require.Equal(t, before, f.route())
				require.False(t, c.enrichmentDeferredForContract(), "independent error cannot authorize deferred success")
			} else {
				require.NoError(t, result.Err)
				require.True(t, result.Reindexed)
				require.True(t, c.enrichmentDeferredForContract())
			}
			require.EqualValues(t, 1, p.calls.Load())
			failed, found := f.generation(p.generation.Load())
			require.True(t, found)
			require.Equal(t, store_sqlite.ViewGenerationFailed, failed.State)
			require.NotEqual(t, p.generation.Load(), f.route().DirtyGenerationID)
		})
	}
}

type contractDemandLiveFailureProvider struct {
	*checkoutEnrichSpy
	failure error
}

func (p *contractDemandLiveFailureProvider) EnrichRepoContext(context.Context, graph.Store, string, string, semantic.EnrichDeadlinePolicy) (*semantic.EnrichResult, error) {
	return nil, p.failure
}

// The public optional result can already have returned nil before demand
// cancels the still-armed attempt. Its known terminal failure must survive
// until the attempt unarms, independently of the earlier context check.
func TestContractDemandAfterLivePartialRetainsProviderFailure(t *testing.T) {
	f, c, _, _, h := newContractDemandSemanticFixture(t)
	sentinel := errors.New("provider failed before late owned cancellation")
	provider := &contractDemandLiveFailureProvider{checkoutEnrichSpy: &checkoutEnrichSpy{}, failure: sentinel}
	mgr := semantic.NewManager(semantic.Config{Enabled: true, CheckoutLSPMaxWorkspaces: 4,
		Providers: []semantic.ProviderConfig{{Name: provider.Name(), Languages: []string{"go"}, Priority: 1, Enabled: true}}}, zap.NewNop())
	mgr.RegisterProvider(provider)
	t.Cleanup(func() { _ = mgr.Close() })
	parent, stop := context.WithTimeout(t.Context(), 5*time.Second)
	defer stop()
	ctx, cancel := context.WithCancelCause(parent)
	defer cancel(nil)
	before, err := c.sampler.Sample(parent)
	require.NoError(t, err)
	attempt := &semanticMotionAttempt{ctx: ctx, cancel: cancel}
	finish := c.armSemanticMotion(attempt, before, time.Now())
	report, err := mgr.EnrichCheckoutContext(ctx, f.store, semantic.CheckoutEnrichRequest{RepoPrefix: builderRepoPrefix,
		CheckoutID: f.checkoutID, Root: f.worktree, Fingerprint: before.Fingerprint})
	require.NoError(t, err, "live optional failure must retain outward partial behavior")
	require.True(t, report.Partial)
	require.NoError(t, ctx.Err(), "demand arrives after the public live-context return check")
	require.Equal(t, ContractScheduled, h.scheduleLinked(parent, f.checkoutID, builderRepoPrefix, before.Fingerprint, false).Result)
	require.ErrorIs(t, ctx.Err(), context.Canceled)
	require.NoError(t, parent.Err())
	// This is the current builder's finishMotion input. The real carrier fix
	// must preserve the pass's terminal identity here without changing err.
	finish(errors.Join(err, report.ProviderTerminalError()))
	got := attempt.buildError(parent, 0, f.worktree, ctx.Err())
	require.ErrorIs(t, got, sentinel, "late cancellation must not erase the earlier independent provider failure")
}

func TestContractDemandTranslationRequiresCurrentAttemptAuthority(t *testing.T) {
	parent, stop := context.WithCancel(t.Context())
	defer stop()
	ctx, cancel := context.WithCancelCause(parent)
	defer cancel(nil)
	a := &semanticMotionAttempt{ctx: ctx, cancel: cancel}
	a.deferForContractDemand()
	foreign := &contractSemanticDemand{attempt: &semanticMotionAttempt{}}
	for _, err := range []error{foreign, fmt.Errorf("foreign provider: %w", foreign), errors.Join(context.Canceled, foreign)} {
		a.providerErr = err
		got := a.buildError(parent, 1, "checkout", ctx.Err())
		require.ErrorIs(t, got, foreign)
		require.False(t, a.ownsContractDemand(got), "an independent retained marker cannot authorize this attempt's retry")
	}
	a.providerErr = context.Cause(ctx)
	got := a.buildError(parent, 1, "checkout", ctx.Err())
	require.True(t, a.ownsContractDemand(got), "returning the current context's actual owned cause is cancellation")
	require.False(t, a.ownsContractDemand(&contractSemanticDemand{attempt: a}), "an unowned copy is not the recorded context cause")
}

func TestContractDemandFollowupPreservesCurrentCauseAuthority(t *testing.T) {
	for _, foreign := range []bool{false, true} {
		t.Run(map[bool]string{false: "owned_demand_resumes_after_fill", true: "foreign_provider_cause_fails"}[foreign], func(t *testing.T) {
			contractDemandFollowupAuthority(t, foreign)
		})
	}
}

func contractDemandFollowupAuthority(t *testing.T, foreignCause bool) {
	t.Helper()
	q := prepareContractDemandDeferredCase(t)
	f, c, h, parent := q.f, q.c, q.scheduler, q.parent
	h.release(q.label)
	h.advance(5 * time.Second)
	h.wake(builderRepoPrefix)
	require.Eventually(t, func() bool { return !c.pendingContractWork() }, time.Second, time.Millisecond)
	initialSemantic := c.builder.Semantic
	p := newMotionContextProvider(t, c)
	foreign := &contractSemanticDemand{attempt: &semanticMotionAttempt{}}
	provider := &contractDemandTerminalProvider{motionContextProvider: p}
	if foreignCause {
		provider.independent = foreign
	}
	mgr := semantic.NewManager(semantic.Config{Enabled: true, CheckoutLSPMaxWorkspaces: 4,
		Providers: []semantic.ProviderConfig{{Name: provider.Name(), Languages: []string{"go"}, Priority: 1, Enabled: true}}}, zap.NewNop())
	mgr.RegisterProvider(provider)
	c.builder.Semantic = mgr
	t.Cleanup(func() { _ = mgr.Close() })
	before := f.route()
	result := make(chan EnrichmentFollowupOutcome, 1)
	ended := make(chan struct{})
	if foreignCause {
		go func() { defer close(ended); result <- c.runEnrichmentFollowup(parent) }()
	} else {
		c.followup.manual.Store(false)
		c.scheduleEnrichmentFollowup()
		go func() {
			defer close(ended)
			c.waitEnrichmentFollowups()
			if c.EnrichmentFollowupStats().Landed == 1 {
				result <- followupLanded
			} else {
				result <- followupFailed
			}
		}()
	}
	key := "next-contract-input"
	label := builderRepoPrefix + "/" + f.checkoutID + "@" + key
	t.Cleanup(func() {
		p.unblock()
		h.release(label)
		select {
		case <-ended:
		case <-time.After(2 * time.Second):
			t.Error("owned foreign-error follow-up did not drain")
		}
	})
	semanticMotionReceive(t, p.started)
	h.hold(label)
	require.Equal(t, ContractScheduled, h.scheduleLinked(parent, f.checkoutID, builderRepoPrefix, key, false).Result)
	select {
	case <-p.canceled:
	case <-time.After(time.Second):
		t.Fatal("new interest did not reach the current private follow-up")
	}
	require.Equal(t, before, f.route())
	failedGeneration := p.generation.Load()
	p.unblock()
	if foreignCause {
		select {
		case outcome := <-result:
			require.Equal(t, followupFailed, outcome, "foreign provider cause must not become owned reschedule")
		case <-time.After(2 * time.Second):
			t.Fatal("drained foreign-error follow-up did not finish")
		}
	} else {
		require.Eventually(t, func() bool { return c.EnrichmentFollowupStats().Canceled == 1 }, time.Second, time.Millisecond)
		require.EqualValues(t, 1, p.calls.Load(), "owned follow-up cannot retry ahead of the matching running fill")
	}
	c.followup.mu.Lock()
	again := c.followup.again
	running := c.followup.running
	c.followup.mu.Unlock()
	if foreignCause {
		require.False(t, again)
	} else {
		require.True(t, running)
	}
	require.Equal(t, before, f.route())
	failed, found := f.generation(failedGeneration)
	require.True(t, found)
	require.Equal(t, store_sqlite.ViewGenerationFailed, failed.State)
	pending, err := c.SemanticPending(parent)
	require.NoError(t, err)
	require.NotEmpty(t, pending.Paths, "failed follow-up must retain truthful debt")
	h.release(label)
	require.Eventually(t, func() bool { return !c.pendingContractWork() }, time.Second, time.Millisecond)
	if foreignCause {
		c.builder.Semantic = initialSemantic
		require.Equal(t, followupLanded, c.runEnrichmentFollowup(parent))
	} else {
		select {
		case outcome := <-result:
			require.Equal(t, followupLanded, outcome)
		case <-time.After(2 * time.Second):
			t.Fatal("owned follow-up did not resume after fill terminal")
		}
	}
	pending, err = c.SemanticPending(parent)
	require.NoError(t, err)
	require.Empty(t, pending.Paths)
}
