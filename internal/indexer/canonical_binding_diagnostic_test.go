package indexer

import (
	"encoding/json"
	"runtime"
	"sync"
	"testing"
	"time"
	"weak"

	"github.com/stretchr/testify/require"
)

func TestRequestedBackgroundBindingRefusalIdentifiesFailedGuard(t *testing.T) {
	type fixture struct {
		b                 *canonicalBackgroundOwner
		record, winner    *PublicationPhaseRecord
		attempt           *pointPublicationAttempt
		entered, returned time.Time
	}
	coalesced := func(f *fixture) {
		f.b.current, f.b.handoffs = 11, 1
		f.attempt.record, f.attempt.independent = f.winner, true
		f.record.coalescedWinner = weak.Make(f.winner)
		f.winner.attemptWindow = &canonicalAttemptWindow{}
		f.winner.debounce = &canonicalDebounceEvidence{}
	}
	cases := []struct {
		name, predicate, stage, accepted string
		noCallback                       bool
		change                           func(*fixture)
	}{
		{"background_stage", "background_stage_not_executing", "initial_quiet", "not_inspected", true, nil},
		{"callback_entry", "callback_entry_mismatch", "executing", "not_inspected", false, func(f *fixture) { f.entered = f.entered.Add(time.Nanosecond) }},
		{"return_order", "return_before_callback_entry", "executing", "not_inspected", false, func(f *fixture) { f.returned = f.entered.Add(-time.Nanosecond) }},
		{"independent_record_missing", "independent_attempt_record_missing", "returned", "not_inspected", false, func(f *fixture) { f.attempt.record, f.attempt.independent = nil, true }},
		{"independent_reuses_request", "independent_attempt_reuses_request", "returned", "not_inspected", false, func(f *fixture) { f.attempt.independent = true }},
		{"independent_foreign_owner", "independent_attempt_owner_mismatch", "returned", "not_inspected", false, func(f *fixture) {
			foreign := NewPublicationPhaseRecorder(2, 1)
			f.attempt.record = foreign.Begin("", "foreign-owner", PublicationSourceMCPEdit, f.entered)
			f.attempt.independent = true
			t.Cleanup(func() { runtime.KeepAlive(foreign) })
		}},
		{"direct_foreign_record", "direct_attempt_record_mismatch", "returned", "no_accepted_witness", false, func(f *fixture) { f.attempt.record = f.winner }},
		{"direct_generation", "direct_generation_mismatch", "returned", "no_accepted_witness", false, func(f *fixture) { f.b.current = 11 }},
		{"direct_handoff", "direct_handoff_present", "returned", "no_accepted_witness", false, func(f *fixture) { f.b.handoffs = 1 }},
		{"direct_winner", "direct_coalesced_winner_present", "returned", "no_accepted_witness", false, func(f *fixture) { f.record.coalescedWinner = weak.Make(f.winner) }},
		{"direct_no_physical_window", "direct_physical_window_missing", "returned", "no_accepted_witness", false, nil},
		{"direct_accepted_stamps_only", "direct_physical_window_missing", "returned", "accepted_stamps_retained", false, func(f *fixture) { f.record.acceptedAttempt = &PublicationStamps{} }},
		{"direct_no_debounce", "direct_debounce_missing", "returned", "physical_window_retained", false, func(f *fixture) { f.record.attemptWindow = &canonicalAttemptWindow{} }},
		{"coalesced_winner_link", "coalesced_winner_link_mismatch", "returned", "physical_window_retained", false, func(f *fixture) { coalesced(f); f.record.coalescedWinner = weak.Pointer[PublicationPhaseRecord]{} }},
		{"coalesced_no_window", "coalesced_physical_window_missing", "returned", "no_accepted_witness", false, func(f *fixture) { coalesced(f); f.winner.attemptWindow = nil }},
		{"coalesced_no_debounce", "coalesced_debounce_missing", "returned", "physical_window_retained", false, func(f *fixture) { coalesced(f); f.winner.debounce = nil }},
		{"coalesced_ticket", "coalesced_ticket_mismatch", "returned", "physical_window_retained", false, func(f *fixture) { coalesced(f); f.winner.ticket = 12 }},
		{"coalesced_not_newer", "coalesced_ticket_not_newer", "returned", "physical_window_retained", false, func(f *fixture) { coalesced(f); f.winner.ticket, f.b.current = 10, 10 }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b, w, record, origin := controlledBackgroundHandoff(t)
			owner := record.owner.Value()
			require.NotNil(t, owner)
			winner := owner.Begin("", "binding-winner", PublicationSourceMCPEdit, origin)
			require.True(t, winner.bindCanonicalTicket(11, origin))
			f := fixture{b: b, record: record, winner: winner, attempt: &pointPublicationAttempt{record: record}, entered: origin.Add(3 * time.Nanosecond), returned: origin.Add(4 * time.Nanosecond)}
			if !tc.noCallback {
				b.callback(w, "sample.go", 10, false, f.entered)
			}
			if tc.change != nil {
				tc.change(&f)
			}
			b.finish(f.attempt, f.entered, f.returned, 0)
			_, unavailable := record.CoalescedPublicationEvidence()
			require.NotNil(t, unavailable)
			require.Equal(t, "serving_attempt_binding_mismatch", unavailable.Reason)
			require.Equal(t, "requested_background_observation", unavailable.Scope)
			require.Nil(t, record.RequestedBackground(), "diagnostics cannot create background coverage")
			wire, err := json.Marshal(unavailable)
			require.NoError(t, err)
			var fields map[string]any
			require.NoError(t, json.Unmarshal(wire, &fields))
			require.Equal(t, tc.predicate, fields["binding_predicate"])
			require.Equal(t, tc.stage, fields["background_stage"])
			require.Equal(t, tc.accepted, fields["accepted_attempt_stage"])
			// A later guard failure cannot rewrite the already-published refusal.
			b.finish(f.attempt, f.entered, f.returned, 0)
			_, later := record.CoalescedPublicationEvidence()
			laterWire, err := json.Marshal(later)
			require.NoError(t, err)
			require.JSONEq(t, string(wire), string(laterWire))
			runtime.KeepAlive(owner)
		})
	}
}

func TestRequestedBackgroundOtherRefusalDoesNotInventBindingState(t *testing.T) {
	b, _, record, _ := controlledBackgroundHandoff(t)
	b.invalidate("timer_stop_not_successful")
	b.publishUnavailable()
	_, unavailable := record.CoalescedPublicationEvidence()
	require.NotNil(t, unavailable)
	wire, err := json.Marshal(unavailable)
	require.NoError(t, err)
	var fields map[string]any
	require.NoError(t, json.Unmarshal(wire, &fields))
	require.Equal(t, "timer_stop_not_successful", fields["reason"])
	for _, key := range []string{"binding_predicate", "background_stage", "accepted_attempt_stage"} {
		require.NotContains(t, fields, key)
	}
}

// This is a current generic refusal control, not a historical smoke replay.
func TestRequestedBackgroundDirectUnacceptedBodyRetainsBindingCause(t *testing.T) {
	path, _, w := pointPublicationFixture(t)
	w.config.DebounceMs = int(time.Hour / time.Millisecond)
	record := newPointPublication(t, "direct-unaccepted-body")
	reached, release := make(chan struct{}), make(chan struct{})
	var releaseOnce, enteredOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(unblock)
	w.mutationBeforeComplete = func(string, uint64) {
		enteredOnce.Do(func() { close(reached); <-release })
	}
	first, err := w.EnqueueToolMutation(WithPublicationRecord(t.Context(), record), path)
	require.NoError(t, err)
	awaitPublicationSignal(t, reached)
	// Explicit admission can supersede the landed body. Native echoes are
	// held while tool work exists and are not the event modeled here.
	newer := w.scheduleFileMutation(path, ChangeModified)
	require.NotNil(t, newer)
	unblock()
	result := awaitMutationRetryResult(t, first)
	require.NoError(t, result.Err)
	require.Equal(t, first.Generation, result.RequestedGeneration)
	require.Equal(t, first.Generation, result.AppliedGeneration)
	select {
	case premature := <-newer.Done:
		t.Fatalf("old body consumed unexecuted successor: %+v", premature)
	default:
	}
	var unavailable *RequestedBackgroundUnavailable
	require.Eventually(t, func() bool {
		_, unavailable = record.CoalescedPublicationEvidence()
		return unavailable != nil
	}, 5*time.Second, time.Millisecond)
	require.Equal(t, "serving_attempt_binding_mismatch", unavailable.Reason)
	require.Zero(t, unavailable.ObservedNativeHandoffs)
	require.Equal(t, first.Generation, unavailable.LastObservedGeneration)
	require.Nil(t, record.RequestedBackground())
	require.Nil(t, record.Snapshot().attemptWindow, "a covered body is not an accepted physical timing winner")
	wire, err := json.Marshal(unavailable)
	require.NoError(t, err)
	var fields map[string]any
	require.NoError(t, json.Unmarshal(wire, &fields))
	require.Equal(t, "direct_physical_window_missing", fields["binding_predicate"])
	require.Equal(t, "returned", fields["background_stage"])
	require.Equal(t, "no_accepted_witness", fields["accepted_attempt_stage"])
	w.mu.Lock()
	w.pending[path].Reset(time.Millisecond)
	w.mu.Unlock()
	require.NoError(t, awaitMutationRetryResult(t, newer).Err)
	_, later := record.CoalescedPublicationEvidence()
	laterWire, err := json.Marshal(later)
	require.NoError(t, err)
	require.JSONEq(t, string(wire), string(laterWire), "later work cannot rewrite the original refusal")
}
