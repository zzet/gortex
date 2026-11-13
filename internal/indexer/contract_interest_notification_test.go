package indexer

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestContractLinkedInterestNotificationFollowsAcceptedWork(t *testing.T) {
	var h *linkedHoldProbe
	var calls atomic.Int32
	var blocked atomic.Bool
	h = newLinkedHoldProbe(t, ContractLinkedIdle, func(options *ContractAnalysisCoordinatorOptions) {
		options.LinkedInterest = func(actor ContractActor) {
			calls.Add(1)
			// This query needs the scheduler mutex. A regression that invokes
			// us under that mutex times out here, then releases it normally;
			// the test owns and joins the query rather than deadlocking.
			queried := make(chan struct{})
			go func() {
				h.c.PendingLinkedWork(actor.Checkout, "r")
				close(queried)
			}()
			select {
			case <-queried:
			case <-time.After(time.Second):
				blocked.Store(true)
				t.Cleanup(func() {
					select {
					case <-queried:
					case <-time.After(time.Second):
						t.Error("notification query did not drain")
					}
				})
			}
		}
	})
	h.busy.Store(true)
	first := h.scheduleLinked(t.Context(), "wt", "r", "k", false)
	require.Equal(t, ContractScheduled, first.Result)
	for range 3 {
		require.Equal(t, ContractCoalesced, h.scheduleLinked(t.Context(), "wt", "r", "k", false).Result)
	}
	require.EqualValues(t, 4, calls.Load())
	require.False(t, blocked.Load(), "notification must run outside scheduler locks")
	require.True(t, h.c.PendingLinkedWork("wt", "r"))
	require.False(t, h.c.PendingLinkedWork("other", "r"))
	require.False(t, h.c.PendingLinkedWork("wt", "other"))

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := h.c.Schedule(ctx, h.linkedRequest("wt", "r", "k", false))
	require.ErrorIs(t, err, context.Canceled)
	require.EqualValues(t, 4, calls.Load(), "refused request must not notify")
	h.busy.Store(false)
	h.advance(5 * time.Second)
	h.wake("r")
	require.Equal(t, ContractActorPublished, h.wait(first).Result)
	require.False(t, h.c.PendingLinkedWork("wt", "r"))
	require.Equal(t, ContractNoop, h.scheduleLinked(t.Context(), "wt", "r", "k", false).Result)
	require.EqualValues(t, 4, calls.Load(), "already-published noop must not notify")
}

func TestContractInputsPendingCannotDeferSemanticPublication(t *testing.T) {
	var calls atomic.Int32
	h := newLinkedHoldProbe(t, ContractLinkedIdle, func(options *ContractAnalysisCoordinatorOptions) {
		options.LinkedInterest = func(ContractActor) { calls.Add(1) }
	})
	h.busy.Store(true)
	req := h.linkedRequest("wt", "r", "pending", false)
	req.Inputs.State.Accepted = false
	for i := range req.Inputs.Witnesses {
		req.Inputs.Witnesses[i].State.Accepted = false
	}
	result, err := h.c.Schedule(t.Context(), req)
	require.NoError(t, err)
	require.Equal(t, ContractDeferred, result.Result)
	require.Zero(t, calls.Load())
	require.False(t, h.c.PendingLinkedWork("wt", "r"), "input admission must not wait on its own semantic publication")
}

func TestContractQueueRefusalDoesNotNotifySemanticAttempt(t *testing.T) {
	var calls atomic.Int32
	h := newLinkedHoldProbe(t, ContractLinkedEager, func(options *ContractAnalysisCoordinatorOptions) {
		options.QueueCapacity = 1
		options.LinkedInterest = func(ContractActor) { calls.Add(1) }
	})
	h.hold("r/wt@k")
	first := h.scheduleLinked(t.Context(), "wt", "r", "k", false)
	require.Eventually(t, func() bool { return len(h.started()) == 1 }, time.Second, time.Millisecond)
	_, err := h.c.Schedule(t.Context(), h.linkedRequest("other", "r", "other-key", false))
	var full *ContractQueueFullError
	require.ErrorAs(t, err, &full)
	require.EqualValues(t, 1, calls.Load())
	h.release("r/wt@k")
	require.Equal(t, ContractActorPublished, h.wait(first).Result)
}
