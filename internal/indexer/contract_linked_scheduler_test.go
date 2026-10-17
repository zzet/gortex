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

func linkedSchedulerInputs(repos []string, key string) *graphview.SelectedContractInputs {
	inputs := schedulerInputs(repos, key)
	inputs.State.CheckoutID = "wt"
	return inputs
}

func scheduleLinkedProbe(t *testing.T, p *schedulerProbe, key string, source *ContractCaptureSource) ContractScheduleResult {
	t.Helper()
	result, err := p.c.Schedule(t.Context(), ContractScheduleRequest{Actor: NewContractActor("wt", []string{"r"}), Key: graph.ContractAttachmentKey{RepoPrefix: "r", CheckoutID: "wt"}, Inputs: linkedSchedulerInputs([]string{"r"}, key), Source: source})
	require.NoError(t, err)
	return result
}

func TestContractSchedulerLinkedContinuationReacquiresWithoutRetainingSource(t *testing.T) {
	p := newSchedulerProbe(t, nil)
	var released, reacquired atomic.Int64
	p.c.options.LinkedInputs = func(ctx context.Context, actor ContractActor, repo string) (*graphview.SelectedContractInputs, *ContractCaptureSource, error) {
		reacquired.Add(1)
		return linkedSchedulerInputs([]string{"r"}, "k"), &ContractCaptureSource{onRelease: func() { released.Add(1) }}, ctx.Err()
	}
	p.c.run = func(job *contractAnalysisJob) string {
		require.NotNil(t, job.otherActorWaiting, "linked continuation is enabled only with the fresh-source adapter")
		job.delta.cache = &contractDeltaCache{identity: job.delta.identity, continuation: &contractFillContinuation{identity: job.delta.identity}}
		return p.run(job)
	}
	p.outcome = func(_ string, n int) string {
		if n == 1 {
			return viewmetrics.ContractJobFillContinued
		}
		return viewmetrics.ContractJobPublished
	}
	first := scheduleLinkedProbe(t, p, "k", &ContractCaptureSource{onRelease: func() { released.Add(1) }})
	p.wait(first)
	require.EqualValues(t, 1, reacquired.Load())
	require.EqualValues(t, 2, released.Load(), "each independent source is consumed/released once")
	p.c.mu.Lock()
	require.Nil(t, p.c.actors[first.Actor].want.source, "no consumed source is retained for another segment")
	p.c.mu.Unlock()
	require.Equal(t, []string{"r/wt@k", "r/wt@k"}, p.started())
}

func TestContractSchedulerLinkedReacquisitionMovesKeyAndOldWaiter(t *testing.T) {
	p := newSchedulerProbe(t, nil)
	p.hold("r/wt@old")
	p.hold("r/wt@new")
	p.c.options.LinkedInputs = func(ctx context.Context, actor ContractActor, repo string) (*graphview.SelectedContractInputs, *ContractCaptureSource, error) {
		return linkedSchedulerInputs([]string{"r"}, "new"), &ContractCaptureSource{}, ctx.Err()
	}
	p.outcome = func(key string, _ int) string {
		if key == "r/wt@old" {
			return viewmetrics.ContractJobFillContinued
		}
		return viewmetrics.ContractJobPublished
	}
	old := scheduleLinkedProbe(t, p, "old", &ContractCaptureSource{})
	p.waitStarts(1)
	p.release("r/wt@old")
	p.waitStarts(2)
	status := p.wait(old)
	require.Equal(t, ContractActorKeyMoved, status.Result)
	p.c.mu.Lock()
	seq := p.c.actors[old.Actor].want.seq
	require.Greater(t, seq, old.ArrivalSeq)
	require.Empty(t, p.c.actors[old.Actor].lastPublished.key, "old capture never resolves as published at the new route")
	p.c.mu.Unlock()
	p.release("r/wt@new")
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	status, err := p.c.WaitActor(ctx, old.Actor, seq)
	require.NoError(t, err)
	require.Equal(t, ContractActorPublished, status.Result)
}

func TestContractSchedulerConcurrentLinkedWantWinsOverReacquisition(t *testing.T) {
	p := newSchedulerProbe(t, nil)
	entered, resume := make(chan struct{}), make(chan struct{})
	var discarded atomic.Int64
	p.c.options.LinkedInputs = func(ctx context.Context, actor ContractActor, repo string) (*graphview.SelectedContractInputs, *ContractCaptureSource, error) {
		close(entered)
		select {
		case <-resume:
		case <-ctx.Done():
		}
		return linkedSchedulerInputs([]string{"r"}, "obsolete"), &ContractCaptureSource{onRelease: func() { discarded.Add(1) }}, ctx.Err()
	}
	p.outcome = func(key string, _ int) string {
		if key == "r/wt@old" {
			return viewmetrics.ContractJobFillContinued
		}
		return viewmetrics.ContractJobPublished
	}
	scheduleLinkedProbe(t, p, "old", &ContractCaptureSource{})
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("linked reacquisition never started")
	}
	latest := scheduleLinkedProbe(t, p, "latest", &ContractCaptureSource{})
	close(resume)
	p.wait(latest)
	require.EqualValues(t, 1, discarded.Load())
	require.Equal(t, []string{"r/wt@old", "r/wt@latest"}, p.started(), "obsolete reacquisition must not replace the newer scheduled source")
}

func TestContractSchedulerLinkedReacquisitionErrorReleasesReturnedSource(t *testing.T) {
	p := newSchedulerProbe(t, nil)
	var released atomic.Int64
	p.c.options.LinkedInputs = func(ctx context.Context, actor ContractActor, repo string) (*graphview.SelectedContractInputs, *ContractCaptureSource, error) {
		return nil, &ContractCaptureSource{onRelease: func() { released.Add(1) }}, graph.ErrContractProjectionStale
	}
	p.outcome = func(_ string, _ int) string { return viewmetrics.ContractJobFillContinued }
	first := scheduleLinkedProbe(t, p, "k", &ContractCaptureSource{})
	status := p.wait(first)
	require.Equal(t, ContractActorInputsStale, status.Result)
	require.EqualValues(t, 1, released.Load())
	require.Len(t, p.started(), 1)
}

func TestContractSchedulerCancellationClosesLinkedReacquisitionSource(t *testing.T) {
	p := newSchedulerProbe(t, nil)
	entered := make(chan struct{})
	var released atomic.Int64
	p.c.options.LinkedInputs = func(ctx context.Context, actor ContractActor, repo string) (*graphview.SelectedContractInputs, *ContractCaptureSource, error) {
		source := &ContractCaptureSource{onRelease: func() { released.Add(1) }}
		close(entered)
		<-ctx.Done()
		return nil, source, ctx.Err()
	}
	p.outcome = func(_ string, _ int) string { return viewmetrics.ContractJobFillContinued }
	first := scheduleLinkedProbe(t, p, "k", &ContractCaptureSource{})
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("linked reacquisition never started")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	require.NoError(t, p.c.CloseContext(ctx))
	require.EqualValues(t, 1, released.Load())
	require.Len(t, p.started(), 1)
	require.Empty(t, p.c.Status(first.Actor).Result, "cancellation is not an attachment result")
}

func TestContractSchedulerFreshPendingLinkedInputsDeferUntilPublished(t *testing.T) {
	f := newRouteConstantFixture(t)
	newContractLinkedWorktree(t, f, map[string]string{"routes.go": jobRoutes, "consts.go": jobConsts, "helper.go": jobHelper}, "helper.go")
	state := graph.ContractInputState{RepoPrefix: "companion", InputVersion: "v", InputFingerprint: "accepted", Accepted: true}
	require.NoError(t, f.store.BeginContractInputMutationContext(t.Context(), nil, state, nil))
	require.NoError(t, f.store.AcceptContractInputMutationContext(t.Context(), state))
	pending := state
	pending.InputFingerprint, pending.Accepted = "pending", false
	actor := NewContractActor(contractLinkedCheckout, []string{"companion"})
	factory := NewContractLinkedInputCapture(f.options.Materializer)
	before := f.options.Materializer.Leases.Held()
	inputs, source, err := factory(t.Context(), actor, "companion")
	require.NoError(t, err)
	p := newSchedulerProbe(t, nil)
	var fresh, released, captures atomic.Int64
	p.c.options.LinkedInputs = func(ctx context.Context, actor ContractActor, repo string) (*graphview.SelectedContractInputs, *ContractCaptureSource, error) {
		fresh.Add(1)
		inputs, source, err := factory(ctx, actor, repo)
		if source != nil {
			source.onRelease = func() { released.Add(1) }
		}
		return inputs, source, err
	}
	p.c.options.Capture = func(ctx context.Context, source *ContractCaptureSource, inputs *graphview.SelectedContractInputs, repo, checkout string) (ContractFollowupSnapshot, []ContractFollowupTarget, error) {
		captures.Add(1)
		// The production capture's acceptance guard must never see pending
		// inputs. A failure here would become failedKey, preventing recovery.
		if inputs == nil || !inputs.State.Accepted {
			source.Release()
			return ContractFollowupSnapshot{}, nil, graph.ErrContractInputVector
		}
		return p.capture(ctx, source, inputs, repo, checkout)
	}
	p.outcome = func(_ string, _ int) string {
		if captures.Load() == 1 {
			require.NoError(t, f.store.BeginContractInputMutationContext(t.Context(), &state, pending, nil))
			return viewmetrics.ContractJobFillContinued
		}
		return viewmetrics.ContractJobPublished
	}
	first, err := p.c.Schedule(t.Context(), ContractScheduleRequest{Actor: actor, Key: graph.ContractAttachmentKey{RepoPrefix: "companion", CheckoutID: contractLinkedCheckout}, Inputs: inputs, Source: source})
	require.NoError(t, err)
	require.Eventually(t, func() bool { return p.c.Status(actor).State == contractActorDeferred.String() }, 10*time.Second, time.Millisecond)
	require.EqualValues(t, 1, fresh.Load())
	require.EqualValues(t, 1, released.Load())
	require.EqualValues(t, 1, captures.Load(), "pending recapture reserves no payload and runs no analysis")
	require.Equal(t, before, f.options.Materializer.Leases.Held())
	p.c.mu.Lock()
	a := p.c.actors[actor]
	require.Empty(t, a.failedKey)
	require.Empty(t, a.lastPublished.key)
	require.Zero(t, a.running.seq)
	require.Nil(t, a.running.cancel)
	require.Nil(t, a.want.source)
	p.c.mu.Unlock()
	require.NoError(t, f.store.AcceptContractInputMutationContext(t.Context(), pending))
	p.c.Published(t.Context(), "companion", "")
	p.waitStarts(2)
	require.Eventually(t, func() bool { return p.c.Status(actor).State == contractActorIdle.String() }, 10*time.Second, time.Millisecond)
	p.c.mu.Lock()
	seq := p.c.actors[actor].want.seq
	require.Greater(t, seq, first.ArrivalSeq)
	p.c.mu.Unlock()
	status, err := p.c.WaitActor(t.Context(), actor, seq)
	require.NoError(t, err)
	require.Equal(t, ContractActorPublished, status.Result)
	require.EqualValues(t, 2, fresh.Load())
	require.EqualValues(t, 2, released.Load())
	require.EqualValues(t, 2, captures.Load())
	require.Equal(t, before, f.options.Materializer.Leases.Held())
}
