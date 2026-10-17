package indexer

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graphview"
	"github.com/zzet/gortex/internal/viewmetrics"
)

func TestContractSchedulerLinkedAcceptancePublishedDuringCallback(t *testing.T) {
	checkLinkedAcceptancePublishedCutpoint(t, false)
}

func TestContractSchedulerLinkedAcceptancePublishedBeforePark(t *testing.T) {
	checkLinkedAcceptancePublishedCutpoint(t, true)
}

// The real companion vector is pending at either cutpoint. The callback gate
// exercises stale validation after acceptance; the release gate runs after both
// pending validations, with its handoff closed but park not yet entered. The
// latter must catch up the Published that capturing actors do not queue directly.
func checkLinkedAcceptancePublishedCutpoint(t *testing.T, beforePark bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	f := newRouteConstantFixture(t)
	newContractLinkedWorktree(t, f, map[string]string{"routes.go": jobRoutes, "consts.go": jobConsts, "helper.go": jobHelper}, "helper.go")
	accepted := graph.ContractInputState{RepoPrefix: "companion", InputVersion: "v", InputFingerprint: "accepted", Accepted: true}
	require.NoError(t, f.store.BeginContractInputMutationContext(ctx, nil, accepted, nil))
	require.NoError(t, f.store.AcceptContractInputMutationContext(ctx, accepted))
	pending := accepted
	pending.InputFingerprint, pending.Accepted = "pending", false
	actor := NewContractActor(contractLinkedCheckout, []string{"companion"})
	factory := NewContractLinkedInputCapture(f.options.Materializer)
	before := f.options.Materializer.Leases.Held()
	inputs, source, err := factory(ctx, actor, "companion")
	require.NoError(t, err)
	defer source.Release()

	p := newSchedulerProbe(t, nil)
	entered, resume := make(chan struct{}), make(chan struct{})
	releaseGate := func() {
		select {
		case <-resume:
		default:
			close(resume)
		}
	}
	defer releaseGate()
	gate := func(callbackContext context.Context) {
		close(entered)
		select {
		case <-resume:
		case <-callbackContext.Done():
		}
	}
	var fresh, released, captures, bodies atomic.Int64
	p.c.options.LinkedInputs = func(callbackContext context.Context, actor ContractActor, repo string) (*graphview.SelectedContractInputs, *ContractCaptureSource, error) {
		n := fresh.Add(1)
		inputs, source, err := factory(callbackContext, actor, repo)
		if err != nil {
			return inputs, source, err
		}
		source.onRelease = func() {
			released.Add(1)
			if n == 1 && beforePark {
				gate(callbackContext)
			}
		}
		if n == 1 && !beforePark {
			gate(callbackContext)
		}
		return inputs, source, nil
	}
	p.c.options.Capture = func(callbackContext context.Context, source *ContractCaptureSource, inputs *graphview.SelectedContractInputs, repo, checkout string) (ContractFollowupSnapshot, []ContractFollowupTarget, error) {
		captures.Add(1)
		// Preserve the production acceptance guard: pending authority must
		// never reach payload reservation or the analysis body.
		if inputs == nil || !inputs.State.Accepted {
			source.Release()
			return ContractFollowupSnapshot{}, nil, graph.ErrContractInputVector
		}
		if err := inputs.Validate(callbackContext); err != nil {
			source.Release()
			return ContractFollowupSnapshot{}, nil, err
		}
		return p.capture(callbackContext, source, inputs, repo, checkout)
	}
	p.outcome = func(_ string, _ int) string {
		if bodies.Add(1) == 1 {
			if err := f.store.BeginContractInputMutationContext(ctx, &accepted, pending, nil); err != nil {
				t.Errorf("begin pending companion mutation: %v", err)
				return viewmetrics.ContractJobFailed
			}
			return viewmetrics.ContractJobFillContinued
		}
		return viewmetrics.ContractJobPublished
	}
	published := make(chan uint64, 1)
	p.c.run = func(job *contractAnalysisJob) string {
		outcome := p.run(job)
		if outcome == viewmetrics.ContractJobPublished {
			p.c.mu.Lock()
			seq := p.c.actors[actor].want.seq
			p.c.mu.Unlock()
			select {
			case published <- seq:
			default:
			}
		}
		return outcome
	}
	first, err := p.c.Schedule(ctx, ContractScheduleRequest{Actor: actor, Key: graph.ContractAttachmentKey{RepoPrefix: "companion", CheckoutID: contractLinkedCheckout}, Inputs: inputs, Source: source})
	require.NoError(t, err)
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("linked reacquisition never reached the controlled wake cutpoint")
	}
	require.EqualValues(t, 1, fresh.Load())
	require.EqualValues(t, 1, captures.Load(), "pending authority reaches neither Capture nor the body")
	require.EqualValues(t, 1, bodies.Load())
	if beforePark {
		require.EqualValues(t, 1, released.Load())
		require.Equal(t, before, f.options.Materializer.Leases.Held(), "handoff closes before the pre-park cutpoint")
	} else {
		require.Zero(t, released.Load(), "callback still owns the pending source")
		require.Greater(t, f.options.Materializer.Leases.Held(), before)
	}
	p.c.mu.Lock()
	a := p.c.actors[actor]
	state, failedKey, publishedKey := a.state, a.failedKey, a.lastPublished.key
	p.c.mu.Unlock()
	require.Equal(t, contractActorCapturing, state, "Published will arrive before the deferred-state wake path")
	require.Empty(t, failedKey)
	require.Empty(t, publishedKey)
	require.NoError(t, f.store.AcceptContractInputMutationContext(ctx, pending))
	p.c.Published(ctx, "companion", "")
	releaseGate()
	var seq uint64
	select {
	case seq = <-published:
	case <-ctx.Done():
		t.Fatal("acceptance Published was lost before linked park or stale validation")
	}
	status, err := p.c.WaitActor(ctx, actor, seq)
	require.NoError(t, err)
	require.Equal(t, ContractActorPublished, status.Result)
	require.Greater(t, seq, first.ArrivalSeq)
	require.EqualValues(t, 2, fresh.Load(), "one fresh accepted vector replaces the pending capture")
	require.EqualValues(t, 2, captures.Load())
	require.EqualValues(t, 2, bodies.Load())
	require.EqualValues(t, 2, released.Load())
	require.Equal(t, before, f.options.Materializer.Leases.Held())
	p.c.mu.Lock()
	a = p.c.actors[actor]
	failedKey, runningSeq, retainedSource := a.failedKey, a.running.seq, a.want.source
	p.c.mu.Unlock()
	require.Empty(t, failedKey)
	require.Zero(t, runningSeq)
	require.Nil(t, retainedSource)
}
