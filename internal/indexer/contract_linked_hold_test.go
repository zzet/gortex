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

// linkedHoldProbe is a scheduler probe on a fake clock with a fake demand
// signal. Its linked actors reacquire their inputs at the key they were
// scheduled at, so a held actor that dropped its source still starts.
type linkedHoldProbe struct {
	*schedulerProbe
	base       time.Time
	clock      atomic.Int64
	busy       atomic.Bool
	samples    atomic.Int64
	reacquired atomic.Int64
	released   atomic.Int64
}

func newLinkedHoldProbe(t *testing.T, policy ContractLinkedPolicy, adjust func(*ContractAnalysisCoordinatorOptions)) *linkedHoldProbe {
	t.Helper()
	h := &linkedHoldProbe{base: time.Unix(1_000_000, 0)}
	h.schedulerProbe = newSchedulerProbe(t, func(options *ContractAnalysisCoordinatorOptions) {
		options.Now = h.now
		options.LinkedPolicy = policy
		options.Demand = func() bool {
			h.samples.Add(1)
			return h.busy.Load()
		}
		options.LinkedInputs = func(ctx context.Context, actor ContractActor, repo string) (*graphview.SelectedContractInputs, *ContractCaptureSource, error) {
			h.reacquired.Add(1)
			h.c.mu.Lock()
			key := h.c.actors[actor].want.inputs.State.InputFingerprint
			h.c.mu.Unlock()
			return linkedProbeInputs(actor.Checkout, actor.Repos(), key), &ContractCaptureSource{onRelease: func() { h.released.Add(1) }}, ctx.Err()
		}
		if adjust != nil {
			adjust(options)
		}
	})
	return h
}

func (h *linkedHoldProbe) now() time.Time { return h.base.Add(time.Duration(h.clock.Load())) }

func (h *linkedHoldProbe) advance(d time.Duration) { h.clock.Add(int64(d)) }

func linkedProbeInputs(checkout string, repos []string, key string) *graphview.SelectedContractInputs {
	inputs := schedulerInputs(repos, key)
	inputs.State.CheckoutID = checkout
	return inputs
}

// scheduleLinked asks for checkout's actor over repo at key with a source
// of its own, as a worktree request does.
func (h *linkedHoldProbe) scheduleLinked(ctx context.Context, checkout, repo, key string, strict bool) ContractScheduleResult {
	h.t.Helper()
	result, err := h.c.Schedule(ctx, ContractScheduleRequest{Actor: NewContractActor(checkout, []string{repo}), Key: graph.ContractAttachmentKey{RepoPrefix: repo, CheckoutID: checkout}, Inputs: linkedProbeInputs(checkout, []string{repo}, key), Source: &ContractCaptureSource{onRelease: func() { h.released.Add(1) }}, Epoch: h.c.InputEpoch()})
	require.NoError(h.t, err)
	return result
}

// settle has repo's dispatcher run one whole evaluation that started after
// the call: it consumes one signal, then a second one only once that
// evaluation has finished and it waits again.
func (h *linkedHoldProbe) settle(repo string) {
	h.t.Helper()
	h.c.mu.Lock()
	ring := h.c.rings[repo]
	h.c.mu.Unlock()
	require.NotNil(h.t, ring)
	for range 2 {
		ring.signal()
		require.Eventually(h.t, func() bool { return len(ring.wake) == 0 }, 10*time.Second, time.Millisecond)
	}
}

func (h *linkedHoldProbe) triggered() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.triggers...)
}

func (h *linkedHoldProbe) held(actor ContractActor) bool {
	h.c.mu.Lock()
	defer h.c.mu.Unlock()
	a := h.c.actors[actor]
	return a != nil && a.state == contractActorQueued && a.held
}

func linkedCounter(policy ContractLinkedPolicy, event string) int64 {
	return contractCounter("views_contract_linked_jobs_total{policy=" + string(policy) + ",event=" + event + "}")
}

// TestContractLinkedIdleDefersWhileBusyAndStartsAfterTheWindow: under the
// idle policy a linked request records its interest and is held while
// demand lasts, without pinning its source; its job starts only once the
// daemon has been idle for the whole window, with its original trigger.
func TestContractLinkedIdleDefersWhileBusyAndStartsAfterTheWindow(t *testing.T) {
	t.Run("held_until_the_window", func(t *testing.T) {
		h := newLinkedHoldProbe(t, ContractLinkedIdle, nil)
		deferred := linkedCounter(ContractLinkedIdle, viewmetrics.ContractLinkedDeferred)
		started := linkedCounter(ContractLinkedIdle, viewmetrics.ContractLinkedStarted)
		h.busy.Store(true)
		first := h.scheduleLinked(t.Context(), "wt", "r", "k", false)
		require.Equal(t, ContractScheduled, first.Result)
		for range 3 {
			h.settle("r")
		}
		require.True(t, h.held(first.Actor))
		require.Equal(t, contractActorQueued.String(), h.c.Status(first.Actor).State)
		require.Equal(t, deferred+1, linkedCounter(ContractLinkedIdle, viewmetrics.ContractLinkedDeferred), "one hold counts once")
		require.EqualValues(t, 1, h.released.Load(), "a held actor pins no handoff source")
		require.Empty(t, h.started())
		again := h.scheduleLinked(t.Context(), "wt", "r", "k", false)
		require.Equal(t, ContractCoalesced, again.Result)
		require.Equal(t, first.ArrivalSeq, again.ArrivalSeq)
		require.EqualValues(t, 2, h.released.Load())

		h.busy.Store(false)
		h.advance(4900 * time.Millisecond)
		h.settle("r")
		require.True(t, h.held(first.Actor), "the window has not passed")
		require.Empty(t, h.started())
		h.advance(100 * time.Millisecond)
		h.settle("r")
		require.Equal(t, []string{"r/wt@k"}, h.waitStarts(1))
		require.Equal(t, ContractActorPublished, h.wait(first).Result)
		require.Equal(t, []string{contractJobTriggerInterest}, h.triggered(), "the hold keeps what queued the job")
		require.EqualValues(t, 1, h.reacquired.Load())
		require.Equal(t, started+1, linkedCounter(ContractLinkedIdle, viewmetrics.ContractLinkedStarted))
		require.Equal(t, deferred+1, linkedCounter(ContractLinkedIdle, viewmetrics.ContractLinkedDeferred))
	})

	t.Run("demand_returns_before_the_body", func(t *testing.T) {
		h := newLinkedHoldProbe(t, ContractLinkedIdle, nil)
		h.advance(5 * time.Second)
		h.hold("p/@k")
		h.schedule("", []string{"p"}, "p", "k")
		h.waitStarts(1)
		deferred := linkedCounter(ContractLinkedIdle, viewmetrics.ContractLinkedDeferred)
		started := linkedCounter(ContractLinkedIdle, viewmetrics.ContractLinkedStarted)
		linked := h.scheduleLinked(t.Context(), "wt", "r", "k", false)
		// The window is open: the linked actor is picked and waits for the
		// slot the primary job holds.
		require.Eventually(t, func() bool {
			h.c.slots.mu.Lock()
			defer h.c.slots.mu.Unlock()
			return len(h.c.slots.waiters) == 1
		}, 10*time.Second, time.Millisecond)
		h.busy.Store(true)
		h.release("p/@k")
		require.Eventually(t, func() bool { return h.held(linked.Actor) }, 10*time.Second, time.Millisecond)
		require.Equal(t, []string{"p/@k"}, h.started(), "no linked body starts while a call is in flight")
		require.Zero(t, h.reacquired.Load(), "nothing was captured for the deferred pick")
		require.Equal(t, deferred+1, linkedCounter(ContractLinkedIdle, viewmetrics.ContractLinkedDeferred))
		require.Equal(t, started, linkedCounter(ContractLinkedIdle, viewmetrics.ContractLinkedStarted))
		h.busy.Store(false)
		h.advance(5 * time.Second)
		h.settle("r")
		require.Equal(t, ContractActorPublished, h.wait(linked).Result)
		require.Equal(t, []string{contractJobTriggerInterest, contractJobTriggerInterest}, h.triggered())
	})

	t.Run("interest_expires_while_held", func(t *testing.T) {
		h := newLinkedHoldProbe(t, ContractLinkedIdle, nil)
		h.busy.Store(true)
		first := h.scheduleLinked(t.Context(), "wt", "r", "k", false)
		h.settle("r")
		require.True(t, h.held(first.Actor))
		h.c.mu.Lock()
		require.Equal(t, 1, h.c.activeLocked())
		h.c.mu.Unlock()
		h.advance(contractInterestTTL)
		h.settle("r")
		h.c.mu.Lock()
		a := h.c.actors[first.Actor]
		require.Equal(t, contractActorIdle, a.state)
		require.True(t, a.interestExpired)
		require.False(t, a.held)
		require.Zero(t, h.c.activeLocked(), "an expired hold frees its capacity")
		h.c.mu.Unlock()
		deferred := linkedCounter(ContractLinkedIdle, viewmetrics.ContractLinkedDeferred)
		renewed := h.scheduleLinked(t.Context(), "wt", "r", "k", false)
		require.Equal(t, ContractScheduled, renewed.Result)
		h.settle("r")
		require.True(t, h.held(first.Actor), "a renewed request is held again while busy")
		require.Equal(t, deferred+1, linkedCounter(ContractLinkedIdle, viewmetrics.ContractLinkedDeferred))
		require.Empty(t, h.started())
	})
}

// TestContractLinkedPolicyLeavesPrimaryActorsAlone: a held linked actor
// neither delays a primary actor queued behind it on its ring nor makes a
// running primary fill stop at a checkpoint for it.
func TestContractLinkedPolicyLeavesPrimaryActorsAlone(t *testing.T) {
	h := newLinkedHoldProbe(t, ContractLinkedIdle, nil)
	// Whether the primary job can stop at a checkpoint at all, and whether
	// it sees another actor waiting for the slot.
	waiting := make(chan [2]bool, 1)
	h.c.run = func(job *contractAnalysisJob) string {
		waiting <- [2]bool{job.otherActorWaiting != nil, job.otherActorWaiting != nil && job.otherActorWaiting()}
		return h.run(job)
	}
	h.busy.Store(true)
	linked := h.scheduleLinked(t.Context(), "wt", "r", "k", false)
	h.settle("r")
	require.True(t, h.held(linked.Actor))
	primary := h.schedule("", []string{"r"}, "r", "k")
	require.Equal(t, ContractActorPublished, h.wait(primary).Result)
	require.Equal(t, []string{"r/@k"}, h.started())
	require.Equal(t, [2]bool{true, false}, <-waiting, "a held linked actor is not waiting for the slot")
	require.True(t, h.held(linked.Actor))
}

// TestContractLinkedThrottledRunsOneJobAndSpacesStarts: under the throttled
// policy demand does not matter; at most one linked job is in flight even
// with a free job slot, and one linked actor starts at most once per
// interval.
func TestContractLinkedThrottledRunsOneJobAndSpacesStarts(t *testing.T) {
	h := newLinkedHoldProbe(t, ContractLinkedThrottled, func(options *ContractAnalysisCoordinatorOptions) { options.JobSlots = 2 })
	h.outcome = func(key string, n int) string {
		if key == "r1/wt1@k" && n == 1 {
			return viewmetrics.ContractJobFillContinued
		}
		return viewmetrics.ContractJobPublished
	}
	deferred := linkedCounter(ContractLinkedThrottled, viewmetrics.ContractLinkedDeferred)
	started := linkedCounter(ContractLinkedThrottled, viewmetrics.ContractLinkedStarted)
	h.busy.Store(true)
	h.hold("r1/wt1@k")
	h.hold("r2/wt2@k")
	first := h.scheduleLinked(t.Context(), "wt1", "r1", "k", false)
	h.waitStarts(1)
	second := h.scheduleLinked(t.Context(), "wt2", "r2", "k", false)
	h.settle("r2")
	require.True(t, h.held(second.Actor), "one linked job is in flight already")
	require.Equal(t, deferred+1, linkedCounter(ContractLinkedThrottled, viewmetrics.ContractLinkedDeferred))

	// The first job stops at a checkpoint and is requeued: the held actor
	// takes its turn, and the first waits for its interval.
	h.release("r1/wt1@k")
	require.Equal(t, []string{"r1/wt1@k", "r2/wt2@k"}, h.waitStarts(2))
	require.Eventually(t, func() bool { return h.held(first.Actor) }, 10*time.Second, time.Millisecond)
	h.release("r2/wt2@k")
	require.Equal(t, ContractActorPublished, h.wait(second).Result)
	h.advance(29900 * time.Millisecond)
	h.settle("r1")
	require.True(t, h.held(first.Actor), "the interval since its last start has not passed")
	require.Len(t, h.started(), 2)
	h.advance(100 * time.Millisecond)
	h.settle("r1")
	require.Equal(t, ContractActorPublished, h.wait(first).Result)
	require.Equal(t, []string{"r1/wt1@k", "r2/wt2@k", "r1/wt1@k"}, h.started())
	h.mu.Lock()
	require.Equal(t, 1, h.maxAll, "linked jobs never overlapped")
	h.mu.Unlock()
	require.Equal(t, deferred+2, linkedCounter(ContractLinkedThrottled, viewmetrics.ContractLinkedDeferred))
	require.Equal(t, started+3, linkedCounter(ContractLinkedThrottled, viewmetrics.ContractLinkedStarted))
	require.Zero(t, h.samples.Load(), "the throttled policy never asks for demand")
}
