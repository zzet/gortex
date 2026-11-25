package indexer

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestContractStrictFillCheckpointsOnlyForStrictWaiters: a running fill stops
// at its next checkpoint when another actor waits for the job slot. While a
// strict request's budget lasts, only an actor that a strict request also
// waits on counts: the fill a caller waits on keeps the slot from a primary or
// linked actor nobody waits on, and still takes turns with another strict
// actor, primary or linked. Once the budget ends every waiter counts again,
// and a fill nobody waits on stops for any waiter as before.
func TestContractStrictFillCheckpointsOnlyForStrictWaiters(t *testing.T) {
	for _, tc := range []struct {
		name                  string
		strict, otherStrict   bool
		otherPrimary, expired bool
		otherState            contractActorState
		want                  bool
	}{
		{name: "strict_vs_queued_primary", strict: true, otherPrimary: true, otherState: contractActorQueued},
		{name: "strict_vs_capturing_primary", strict: true, otherPrimary: true, otherState: contractActorCapturing},
		{name: "strict_vs_queued_linked", strict: true, otherState: contractActorQueued},
		{name: "strict_vs_strict_primary", strict: true, otherStrict: true, otherPrimary: true, otherState: contractActorQueued, want: true},
		{name: "strict_vs_strict_linked", strict: true, otherStrict: true, otherState: contractActorCapturing, want: true},
		{name: "strict_vs_idle_strict_primary", strict: true, otherStrict: true, otherPrimary: true, otherState: contractActorIdle},
		{name: "strict_budget_ended", strict: true, expired: true, otherPrimary: true, otherState: contractActorCapturing, want: true},
		{name: "unwaited_vs_queued_primary", otherPrimary: true, otherState: contractActorQueued, want: true},
		{name: "unwaited_vs_capturing_strict_linked", otherStrict: true, otherState: contractActorCapturing, want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newLinkedHoldProbe(t, ContractLinkedEager, nil)
			otherCheckout := "wt2"
			if tc.otherPrimary {
				otherCheckout = ""
			}
			h.c.mu.Lock()
			now := h.c.now()
			current := h.c.registerLocked(NewContractActor("wt", []string{"r"}), now)
			other := h.c.registerLocked(NewContractActor(otherCheckout, []string{"r"}), now)
			current.state, other.state = contractActorRunning, tc.otherState
			if tc.strict {
				current.strictUntil = now.Add(time.Second)
			}
			if tc.otherStrict {
				other.strictUntil = now.Add(time.Minute)
			}
			h.c.mu.Unlock()
			if tc.expired {
				h.advance(2 * time.Second)
			}
			require.Equal(t, tc.want, h.c.otherActorWaiting(current))
		})
	}
}
