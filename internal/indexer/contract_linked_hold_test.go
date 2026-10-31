package indexer

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graphview"
	"github.com/zzet/gortex/internal/viewmetrics"
)

// linkedHoldProbe is a scheduler probe on a fake clock with a fake demand
// signal: busy is work in flight, calls the sequence that moves as a tool
// call starts or ends. Its linked actors reacquire their inputs at the key
// they were scheduled at, so a held actor that dropped its source still
// starts.
type linkedHoldProbe struct {
	*schedulerProbe
	base       time.Time
	clock      atomic.Int64
	busy       atomic.Bool
	calls      atomic.Uint64
	samples    atomic.Int64
	sampled    chan struct{}
	reacquired atomic.Int64
	released   atomic.Int64
}

func newLinkedHoldProbe(t *testing.T, policy ContractLinkedPolicy, adjust func(*ContractAnalysisCoordinatorOptions)) *linkedHoldProbe {
	t.Helper()
	h := &linkedHoldProbe{base: time.Unix(1_000_000, 0), sampled: make(chan struct{}, 64)}
	h.schedulerProbe = newSchedulerProbe(t, func(options *ContractAnalysisCoordinatorOptions) {
		options.Now = h.now
		options.LinkedPolicy = policy
		options.Demand = func() (bool, uint64) {
			h.samples.Add(1)
			select {
			case h.sampled <- struct{}{}:
			default:
			}
			return h.busy.Load(), h.calls.Load()
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

// call is a tool call that starts and ends between two demand samples.
func (h *linkedHoldProbe) call() { h.calls.Add(2) }

func linkedProbeInputs(checkout string, repos []string, key string) *graphview.SelectedContractInputs {
	inputs := schedulerInputs(repos, key)
	inputs.State.CheckoutID = checkout
	return inputs
}

// scheduleLinked asks for checkout's actor over repo at key with a source
// of its own, as a worktree request does.
func (h *linkedHoldProbe) scheduleLinked(ctx context.Context, checkout, repo, key string, strict bool) ContractScheduleResult {
	h.t.Helper()
	result, err := h.c.Schedule(ctx, h.linkedRequest(checkout, repo, key, strict))
	require.NoError(h.t, err)
	return result
}

func (h *linkedHoldProbe) linkedRequest(checkout, repo, key string, strict bool) ContractScheduleRequest {
	return ContractScheduleRequest{Actor: NewContractActor(checkout, []string{repo}), Key: graph.ContractAttachmentKey{RepoPrefix: repo, CheckoutID: checkout}, Inputs: linkedProbeInputs(checkout, []string{repo}, key), Source: &ContractCaptureSource{onRelease: func() { h.released.Add(1) }}, Epoch: h.c.InputEpoch(), Strict: strict}
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

// held reports whether actor waits in its queue, held by its linked policy
// now.
func (h *linkedHoldProbe) held(actor ContractActor) bool {
	h.c.mu.Lock()
	defer h.c.mu.Unlock()
	a := h.c.actors[actor]
	return a != nil && a.state == contractActorQueued && a.held && h.c.linkedHeldLocked(a, h.c.now())
}

// wake signals repo's dispatcher.
func (h *linkedHoldProbe) wake(repo string) {
	h.c.mu.Lock()
	ring := h.c.rings[repo]
	h.c.mu.Unlock()
	ring.signal()
}

// slotWaiters is how many dispatchers wait for a job slot.
func (h *linkedHoldProbe) slotWaiters() int {
	h.c.slots.mu.Lock()
	defer h.c.slots.mu.Unlock()
	return len(h.c.slots.waiters)
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
		require.Eventually(t, func() bool { return h.slotWaiters() == 1 }, 10*time.Second, time.Millisecond)
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
		active, held := h.c.activeLocked(h.c.now())
		h.c.mu.Unlock()
		require.Equal(t, [2]int{1, 1}, [2]int{active, held})
		h.advance(contractInterestTTL)
		h.settle("r")
		h.c.mu.Lock()
		a := h.c.actors[first.Actor]
		require.Equal(t, contractActorIdle, a.state)
		require.True(t, a.interestExpired)
		require.False(t, a.held)
		active, _ = h.c.activeLocked(h.c.now())
		h.c.mu.Unlock()
		require.Zero(t, active, "an expired hold leaves the registry's actors with work")
		deferred := linkedCounter(ContractLinkedIdle, viewmetrics.ContractLinkedDeferred)
		renewed := h.scheduleLinked(t.Context(), "wt", "r", "k", false)
		require.Equal(t, ContractScheduled, renewed.Result)
		h.settle("r")
		require.True(t, h.held(first.Actor), "a renewed request is held again while busy")
		require.Equal(t, deferred+1, linkedCounter(ContractLinkedIdle, viewmetrics.ContractLinkedDeferred))
		require.Empty(t, h.started())
	})
}

// TestContractLinkedIdleWindowSeesCallsBetweenSamples: the idle window runs
// from the last foreground work the scheduler learned of, including tool
// calls that started and ended between two of its demand samples, which no
// sample finds in flight.
func TestContractLinkedIdleWindowSeesCallsBetweenSamples(t *testing.T) {
	t.Run("short_calls_keep_the_window_shut", func(t *testing.T) {
		h := newLinkedHoldProbe(t, ContractLinkedIdle, nil)
		h.busy.Store(true)
		linked := h.scheduleLinked(t.Context(), "wt", "r", "k", false)
		h.settle("r")
		h.busy.Store(false)
		for range 3 {
			h.call()
			h.advance(4 * time.Second)
			h.settle("r")
			require.True(t, h.held(linked.Actor), "a call that came and went closed the window")
		}
		require.Empty(t, h.started())
		h.advance(5 * time.Second)
		h.settle("r")
		require.Equal(t, ContractActorPublished, h.wait(linked).Result)
	})

	t.Run("a_call_nothing_sampled", func(t *testing.T) {
		h := newLinkedHoldProbe(t, ContractLinkedIdle, nil)
		// A long quiet spell with no dispatcher awake to sample, then a
		// call that came and went just before a linked request.
		h.advance(time.Minute)
		h.call()
		linked := h.scheduleLinked(t.Context(), "wt", "r", "k", false)
		h.settle("r")
		require.True(t, h.held(linked.Actor), "the window starts at the call, not at the last sample before it")
		require.Empty(t, h.started())
		h.advance(5 * time.Second)
		h.settle("r")
		require.Equal(t, ContractActorPublished, h.wait(linked).Result)
	})
}

// TestContractLinkedActorsWaitingTheirTurnOutliveTheirInterest: the linked
// actors of one repository share its ring and, with one job slot, run one
// after another. One whose turn comes after its interest ran out still runs:
// the idle policy no longer holds it once the window is open, and a
// throttled one only waits for its turn, as a queued actor waits for the
// slot.
func TestContractLinkedActorsWaitingTheirTurnOutliveTheirInterest(t *testing.T) {
	for _, policy := range []ContractLinkedPolicy{ContractLinkedIdle, ContractLinkedThrottled} {
		t.Run(string(policy), func(t *testing.T) {
			h := newLinkedHoldProbe(t, policy, func(options *ContractAnalysisCoordinatorOptions) { options.JobSlots = 1 })
			h.busy.Store(true)
			h.hold("r/wt1@k")
			var results []ContractScheduleResult
			for _, checkout := range []string{"wt1", "wt2", "wt3"} {
				results = append(results, h.scheduleLinked(t.Context(), checkout, "r", "k", false))
			}
			if policy == ContractLinkedIdle {
				h.settle("r")
				for _, result := range results {
					require.True(t, h.held(result.Actor))
				}
				// The window opens: the first job runs and the second waits
				// for the slot it holds.
				h.busy.Store(false)
				h.advance(5 * time.Second)
				h.wake("r")
				require.Eventually(t, func() bool { return h.slotWaiters() == 1 }, 10*time.Second, time.Millisecond)
			} else {
				h.settle("r")
				require.True(t, h.held(results[1].Actor), "one linked job is in flight already")
				require.True(t, h.held(results[2].Actor))
			}
			require.Equal(t, []string{"r/wt1@k"}, h.waitStarts(1))
			h.advance(contractInterestTTL)
			h.release("r/wt1@k")
			require.Equal(t, []string{"r/wt1@k", "r/wt2@k", "r/wt3@k"}, h.waitStarts(3), "no actor waiting its turn is dropped")
			for _, result := range results {
				require.Equal(t, ContractActorPublished, h.wait(result).Result)
			}
		})
	}
}

// TestContractLinkedIdleHoldsLeaveTheQueueCapacity: a linked actor the idle
// policy holds pins no source, slot or lease and may wait out a long busy
// spell, so it takes no queue capacity: a worktree an agent opens under load
// still records its interest, and a strict request there starts at once.
// Held actors are bounded by the registry instead.
func TestContractLinkedIdleHoldsLeaveTheQueueCapacity(t *testing.T) {
	h := newLinkedHoldProbe(t, ContractLinkedIdle, func(options *ContractAnalysisCoordinatorOptions) { options.QueueCapacity = 2 })
	h.busy.Store(true)
	capacity := h.c.CapacityEpoch()
	for _, checkout := range []string{"wt1", "wt2", "wt3"} {
		require.Equal(t, ContractScheduled, h.scheduleLinked(t.Context(), checkout, "r", "k", false).Result)
		h.settle("r")
		require.True(t, h.held(NewContractActor(checkout, []string{"r"})))
	}
	require.Greater(t, h.c.CapacityEpoch(), capacity, "a hold frees capacity for a request that waits for it")

	h.hold("r/wt4@k")
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	strict := h.scheduleLinked(ctx, "wt4", "r", "k", true)
	require.Equal(t, ContractScheduled, strict.Result)
	require.Equal(t, []string{"r/wt4@k"}, h.waitStarts(1), "a strict request in a new worktree starts at once")
	h.release("r/wt4@k")
	require.Equal(t, ContractActorPublished, h.wait(strict).Result)

	// Held actors count against the registry: once every actor it keeps has
	// work, a new one is refused.
	refused := false
	for i := 5; i < 2*contractSchedulerMaxActors && !refused; i++ {
		_, err := h.c.Schedule(t.Context(), h.linkedRequest(fmt.Sprintf("wt%d", i), "r", "k", false))
		var full *ContractQueueFullError
		if refused = errors.As(err, &full); !refused {
			require.NoError(t, err)
		}
	}
	require.True(t, refused)
	h.c.mu.Lock()
	withWork := 0
	for _, a := range h.c.actors {
		if a.hasWork() {
			withWork++
		}
	}
	h.c.mu.Unlock()
	require.Equal(t, contractSchedulerMaxActors, withWork)
}

// TestContractLinkedPolicyLeavesPrimaryActorsAlone: a held linked actor
// neither delays a primary actor queued behind it on its ring nor makes a
// running primary fill stop at a checkpoint for it.
func TestContractLinkedPolicyLeavesPrimaryActorsAlone(t *testing.T) {
	h := newLinkedHoldProbe(t, ContractLinkedIdle, nil)
	// Whether the primary job can stop at a checkpoint at all, and whether
	// it sees another actor waiting for the slot.
	// The third: it runs on the coordinator's own yield, outside any
	// linked policy.
	waiting := make(chan [3]bool, 1)
	h.c.run = func(job *contractAnalysisJob) string {
		waiting <- [3]bool{job.otherActorWaiting != nil, job.otherActorWaiting != nil && job.otherActorWaiting(), job.yield == nil && job.linkedPolicy == ""}
		return h.run(job)
	}
	h.busy.Store(true)
	linked := h.scheduleLinked(t.Context(), "wt", "r", "k", false)
	h.settle("r")
	require.True(t, h.held(linked.Actor))
	primary := h.schedule("", []string{"r"}, "r", "k")
	require.Equal(t, ContractActorPublished, h.wait(primary).Result)
	require.Equal(t, []string{"r/@k"}, h.started())
	require.Equal(t, [3]bool{true, false, true}, <-waiting, "a held linked actor is not waiting for the slot")
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

// parkedYield calls a job's yield on its own goroutine with demand on and
// reports whether it was still parked after several more demand samples.
func (h *linkedHoldProbe) parkedYield(job *contractAnalysisJob) (returned chan error, parked bool) {
	for len(h.sampled) > 0 {
		<-h.sampled
	}
	h.busy.Store(true)
	returned = make(chan error, 1)
	go func() { returned <- job.yield(context.Background()) }()
	for range 4 {
		select {
		case <-h.sampled:
		case <-returned:
			return returned, false
		case <-time.After(10 * time.Second):
			return returned, false
		}
	}
	select {
	case <-returned:
		return returned, false
	default:
		return returned, true
	}
}

func receiveYield(returned chan error) (bool, error) {
	select {
	case err := <-returned:
		return true, err
	case <-time.After(10 * time.Second):
		return false, nil
	}
}

// TestContractLinkedIdleFillStopsAtCheckpointAndResumesWithProgress: a
// linked fill that meets demand parks with no progress slice, then stops at
// its next checkpoint; it is held until the next idle window and resumes
// from the checkpoint it retained.
func TestContractLinkedIdleFillStopsAtCheckpointAndResumesWithProgress(t *testing.T) {
	t.Run("stops_and_resumes", func(t *testing.T) {
		h := newLinkedHoldProbe(t, ContractLinkedIdle, nil)
		h.advance(5 * time.Second)
		h.outcome = func(_ string, n int) string {
			if n == 1 {
				return viewmetrics.ContractJobFillContinued
			}
			return viewmetrics.ContractJobPublished
		}
		type observed struct {
			linked, parked, returned, stop bool
			err                            error
		}
		first, resumed := make(chan observed, 1), make(chan bool, 1)
		var runs atomic.Int64
		h.c.run = func(job *contractAnalysisJob) string {
			if runs.Add(1) > 1 {
				resumed <- job.delta.cache != nil && job.delta.cache.continuation != nil
				return h.run(job)
			}
			seen := observed{linked: job.yield != nil && job.linkedPolicy == ContractLinkedIdle && job.otherActorWaiting != nil}
			if !seen.linked {
				first <- seen
				return h.run(job)
			}
			returned, parked := h.parkedYield(job)
			seen.parked = parked
			h.busy.Store(false)
			seen.returned, seen.err = receiveYield(returned)
			seen.stop = job.otherActorWaiting()
			unit := &contractUnit{path: "r/file.go"}
			job.delta.cache = &contractDeltaCache{identity: job.delta.identity, units: map[string]*contractUnit{unit.path: unit}, continuation: &contractFillContinuation{identity: job.delta.identity, units: []contractFillUnitProof{{unit: unit}}}}
			job.delta.continued = true
			first <- seen
			return h.run(job)
		}
		stopped := linkedCounter(ContractLinkedIdle, viewmetrics.ContractLinkedCheckpointStopped)
		linked := h.scheduleLinked(t.Context(), "wt", "r", "k", false)
		seen := <-first
		require.True(t, seen.linked, "a linked idle job yields and checkpoints by its policy")
		require.True(t, seen.parked, "demand parks the job across several samples, with no progress slice")
		require.True(t, seen.returned, "the park ends with the demand")
		require.NoError(t, seen.err)
		require.True(t, seen.stop, "a fill that met demand stops at its next checkpoint")
		require.Eventually(t, func() bool { return h.held(linked.Actor) }, 10*time.Second, time.Millisecond, "the stopped fill waits for the next idle window")
		require.Equal(t, stopped+1, linkedCounter(ContractLinkedIdle, viewmetrics.ContractLinkedCheckpointStopped))
		require.Equal(t, ContractFillCheckpointRetained, h.c.Status(linked.Actor).FillCheckpoint)
		h.settle("r")
		require.Len(t, h.started(), 1, "no restart inside the window the demand reopened")
		h.advance(5 * time.Second)
		h.settle("r")
		require.Equal(t, ContractActorPublished, h.wait(linked).Result)
		require.True(t, <-resumed, "the resumed fill starts from its retained checkpoint")
		require.Equal(t, []string{contractJobTriggerInterest, contractJobTriggerRequeue}, h.triggered())
	})

	t.Run("a_call_between_yields_stops_the_fill", func(t *testing.T) {
		h := newLinkedHoldProbe(t, ContractLinkedIdle, nil)
		h.advance(5 * time.Second)
		seen := make(chan [3]bool, 1)
		h.c.run = func(job *contractAnalysisJob) string {
			quiet := job.yield(context.Background()) == nil && !job.otherActorWaiting()
			h.call()
			// Nothing is in flight: the yield returns at once, and the fill
			// stops at its next checkpoint.
			returned := job.yield(context.Background()) == nil
			seen <- [3]bool{quiet, returned, job.otherActorWaiting()}
			return h.run(job)
		}
		linked := h.scheduleLinked(t.Context(), "wt", "r", "k", false)
		require.Equal(t, [3]bool{true, true, true}, <-seen)
		require.Equal(t, ContractActorPublished, h.wait(linked).Result)
		next := h.scheduleLinked(t.Context(), "wt", "r", "k2", false)
		h.settle("r")
		require.True(t, h.held(next.Actor), "the call closed the idle window")
	})

	t.Run("park_is_bounded", func(t *testing.T) {
		h := newLinkedHoldProbe(t, ContractLinkedIdle, nil)
		h.advance(5 * time.Second)
		type observed struct{ parked, bounded, immediate bool }
		seen := make(chan observed, 1)
		h.c.run = func(job *contractAnalysisJob) string {
			var o observed
			returned, parked := h.parkedYield(job)
			o.parked = parked
			h.advance(contractLinkedParkMax)
			o.bounded, _ = receiveYield(returned)
			again := make(chan error, 1)
			go func() { again <- job.yield(context.Background()) }()
			o.immediate, _ = receiveYield(again)
			h.busy.Store(false)
			seen <- o
			return h.run(job)
		}
		linked := h.scheduleLinked(t.Context(), "wt", "r", "k", false)
		require.Equal(t, observed{parked: true, bounded: true, immediate: true}, <-seen, "demand that outlasts the bound no longer parks the job")
		require.Equal(t, ContractActorPublished, h.wait(linked).Result)
	})

	t.Run("a_waiting_actor_ends_the_park", func(t *testing.T) {
		h := newLinkedHoldProbe(t, ContractLinkedIdle, nil)
		h.advance(5 * time.Second)
		parkedCh := make(chan bool, 1)
		ended := make(chan [2]bool, 1)
		h.c.run = func(job *contractAnalysisJob) string {
			if job.linkedPolicy == "" {
				return h.run(job)
			}
			returned, parked := h.parkedYield(job)
			parkedCh <- parked
			ok, _ := receiveYield(returned)
			ended <- [2]bool{ok, job.otherActorWaiting()}
			return h.run(job)
		}
		linked := h.scheduleLinked(t.Context(), "wt", "r", "k", false)
		require.True(t, <-parkedCh)
		primary := h.schedule("", []string{"p"}, "p", "k")
		require.Equal(t, [2]bool{true, true}, <-ended, "a primary waiting for the slot ends the park and the fill")
		require.Equal(t, ContractActorPublished, h.wait(linked).Result)
		require.Equal(t, ContractActorPublished, h.wait(primary).Result)
	})
}

// TestContractLinkedStrictRequestsBypassDeferral: a request whose caller
// waits for fresh results starts its linked actor's job at once under every
// policy, for the rest of the caller's budget only.
func TestContractLinkedStrictRequestsBypassDeferral(t *testing.T) {
	for _, policy := range []ContractLinkedPolicy{ContractLinkedIdle, ContractLinkedThrottled} {
		t.Run(string(policy), func(t *testing.T) {
			h := newLinkedHoldProbe(t, policy, nil)
			h.busy.Store(true)
			if policy == ContractLinkedThrottled {
				// A first job opens the actor's throttle interval.
				require.Equal(t, ContractActorPublished, h.wait(h.scheduleLinked(t.Context(), "wt", "r", "k0", false)).Result)
			}
			before := len(h.started())
			strictStarted := linkedCounter(policy, viewmetrics.ContractLinkedStrictStarted)
			held := h.scheduleLinked(t.Context(), "wt", "r", "k1", false)
			h.settle("r")
			require.True(t, h.held(held.Actor))
			h.hold("r/wt@k1")
			ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
			defer cancel()
			strict := h.scheduleLinked(ctx, "wt", "r", "k1", true)
			require.Equal(t, ContractCoalesced, strict.Result)
			require.Equal(t, held.ArrivalSeq, strict.ArrivalSeq)
			require.Equal(t, "r/wt@k1", h.waitStarts(before + 1)[before], "a strict request releases the hold at once")
			require.Equal(t, strictStarted+1, linkedCounter(policy, viewmetrics.ContractLinkedStrictStarted))
			h.release("r/wt@k1")
			require.Equal(t, ContractActorPublished, h.wait(strict).Result)

			// The exemption ends with the strict caller's budget.
			h.advance(2 * time.Second)
			later := h.scheduleLinked(t.Context(), "wt", "r", "k2", false)
			h.settle("r")
			require.True(t, h.held(later.Actor), "a lapsed strict marker holds again")
			require.Len(t, h.started(), before+1)
		})
	}

	t.Run("a_strict_request_ends_a_park", func(t *testing.T) {
		var shared atomic.Int64
		h := newLinkedHoldProbe(t, ContractLinkedIdle, func(options *ContractAnalysisCoordinatorOptions) {
			options.Yield = func(ctx context.Context) error {
				shared.Add(1)
				return ctx.Err()
			}
		})
		h.advance(5 * time.Second)
		parkedCh := make(chan bool, 1)
		ended := make(chan [3]bool, 1)
		h.c.run = func(job *contractAnalysisJob) string {
			returned, parked := h.parkedYield(job)
			parkedCh <- parked
			ok, _ := receiveYield(returned)
			calls := shared.Load()
			err := job.yield(context.Background())
			ended <- [3]bool{ok, err == nil && shared.Load() == calls+1, !job.otherActorWaiting()}
			h.busy.Store(false)
			return h.run(job)
		}
		linked := h.scheduleLinked(t.Context(), "wt", "r", "k", false)
		require.True(t, <-parkedCh)
		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
		defer cancel()
		strict := h.scheduleLinked(ctx, "wt", "r", "k", true)
		require.Equal(t, ContractRunning, strict.Result)
		require.Equal(t, [3]bool{true, true, true}, <-ended, "a strict request ends the park, restores the shared yield and keeps the fill going")
		require.Equal(t, ContractActorPublished, h.wait(linked).Result)
	})
}
