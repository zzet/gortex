package indexer

import (
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestCanonicalDebounceObservesInitialTimerBeforeClaimedHook(t *testing.T) {
	path, _, w := pointPublicationFixture(t)
	w.config.DebounceMs = 20
	entered := make(chan time.Time, 1)
	release := make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	t.Cleanup(unblock)
	w.pointMutationClaimed = func(string) {
		entered <- time.Now()
		<-release
	}
	record := newPointPublication(t, "quiet-wait")
	ticket, err := w.EnqueueFileMutation(WithPublicationRecord(t.Context(), record), path)
	require.NoError(t, err)
	var claimed time.Time
	select {
	case claimed = <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("initial timer did not claim its generation")
	}
	require.Nil(t, record.Snapshot().debounce, "unvalidated attempt must not publish its pair")
	unblock()
	require.NoError(t, awaitMutationRetryResult(t, ticket).Err)
	snapshot := record.Snapshot()
	require.NotNil(t, snapshot.debounce)
	require.LessOrEqual(t, snapshot.debounce.enteredNS, claimed.Sub(record.origin).Nanoseconds(), "hook/admission work is outside quiet wait")
	timing := snapshot.EditTimings()
	require.Equal(t, "measured", timing.States["debounce"])
	require.NotNil(t, timing.Milliseconds["debounce"])
	require.Equal(t, "partial", timing.Window.Coverage, "quiet wait and the separate physical wall do not close accounting")
}

func TestCanonicalDebounceRequiresActualContainedPair(t *testing.T) {
	for _, tc := range []struct {
		name       string
		pair       canonicalDebounceEvidence
		terminal   bool
		remove     PublicationPhase
		terminalNS int64
		want       string
	}{
		{name: "actual_zero", pair: canonicalDebounceEvidence{startedNS: 2, enteredNS: 2, entered: true}, want: "measured"},
		{name: "actual_wait", pair: canonicalDebounceEvidence{startedNS: 2, enteredNS: 4, entered: true}, terminal: true, terminalNS: 8, want: "measured"},
		{name: "negative", pair: canonicalDebounceEvidence{startedNS: -1, enteredNS: 4, entered: true}, want: "invalid_order"},
		{name: "reversed", pair: canonicalDebounceEvidence{startedNS: 4, enteredNS: 2, entered: true}, want: "invalid_order"},
		{name: "before_enqueue", pair: canonicalDebounceEvidence{startedNS: 0, enteredNS: 4, entered: true}, want: "invalid_order"},
		{name: "after_cycle", pair: canonicalDebounceEvidence{startedNS: 2, enteredNS: 7, entered: true}, want: "invalid_order"},
		{name: "missing_enqueue", pair: canonicalDebounceEvidence{startedNS: 2, enteredNS: 4, entered: true}, remove: PublicationTicketEnqueued, want: "not_observed"},
		{name: "missing_cycle", pair: canonicalDebounceEvidence{startedNS: 2, enteredNS: 4, entered: true}, remove: PublicationCycleStarted, want: "not_observed"},
		{name: "unfinished", pair: canonicalDebounceEvidence{startedNS: 2}, want: "pending"},
		{name: "terminal_unfinished", pair: canonicalDebounceEvidence{startedNS: 2}, terminal: true, terminalNS: 8, want: "not_observed"},
		{name: "missing_terminal", pair: canonicalDebounceEvidence{startedNS: 2, enteredNS: 4, entered: true}, terminal: true, want: "not_observed"},
		{name: "outside_terminal", pair: canonicalDebounceEvidence{startedNS: 2, enteredNS: 4, entered: true}, terminal: true, terminalNS: 3, want: "invalid_order"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			offsets := map[PublicationPhase]int64{PublicationTicketEnqueued: 1, PublicationCycleStarted: 6}
			if tc.terminalNS != 0 {
				offsets[PublicationTicketCompleted] = tc.terminalNS
			}
			delete(offsets, tc.remove)
			out := EditPhaseTimings{Milliseconds: map[string]*float64{}, States: map[string]string{}}
			tc.pair.project(offsets, tc.terminal, &out)
			require.Equal(t, tc.want, out.States["debounce"])
			if tc.want == "measured" {
				require.NotNil(t, out.Milliseconds["debounce"])
			} else {
				require.Nil(t, out.Milliseconds["debounce"])
			}
		})
	}
}

func TestCanonicalNativeDebounceUsesOnlyActualArmProof(t *testing.T) {
	for _, name := range []string{"actual_arm", "missing_arm", "arm_after_start", "terminal_before_callback"} {
		t.Run(name, func(t *testing.T) {
			pair := canonicalDebounceEvidence{startedNS: 0, enteredNS: 5, entered: true}
			offsets := map[PublicationPhase]int64{PublicationTimerArmStarted: 0, PublicationCycleStarted: 6, PublicationTicketCompleted: 10}
			switch name {
			case "missing_arm":
				delete(offsets, PublicationTimerArmStarted)
			case "arm_after_start":
				offsets[PublicationTimerArmStarted] = 1
			case "terminal_before_callback":
				offsets[PublicationTicketCompleted] = 4
			}
			out := EditPhaseTimings{Milliseconds: make(map[string]*float64), States: make(map[string]string)}
			pair.project(offsets, true, &out)
			if name == "actual_arm" {
				require.Equal(t, "measured", out.States["debounce"])
				require.Equal(t, 0.000005, *out.Milliseconds["debounce"])
			} else {
				require.Nil(t, out.Milliseconds["debounce"])
			}
			require.NotContains(t, offsets, PublicationTicketEnqueued, "native arm must not invent request admission")
		})
	}
}
