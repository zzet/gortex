package indexer

import (
	"sync"
	"testing"
	"time"
	"weak"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

func TestPhysicalAttemptGuardDiagnosticEligibleWinner(t *testing.T) {
	_, record, stamps, origin := attemptWindowFixture(t)
	window, accepted, reason, guard := record.completePhysicalAttempt(stamps, origin.Add(2*time.Millisecond), origin.Add(8*time.Millisecond), 0)
	require.True(t, accepted)
	require.Empty(t, reason)
	require.EqualValues(t, 7, window.ticket)
	require.True(t, guard.observed)
	require.Equal(t, "same", guard.acceptedStamps)
	require.False(t, guard.windowPresent, "state is frozen before installing the successful window")
	require.Nil(t, record.acceptedAttempt)
	require.NotNil(t, record.attemptWindow)
	require.Equal(t, "same", guard.acceptedStamps, "later pointer clearing cannot alter the diagnostic")
}

func TestPhysicalAttemptGuardDiagnosticDistinguishesRejectedCollectors(t *testing.T) {
	for _, state := range []string{"absent", "other", "prior_window"} {
		t.Run(state, func(t *testing.T) {
			_, record, stamps, origin := attemptWindowFixture(t)
			start, end := origin.Add(2*time.Millisecond), origin.Add(8*time.Millisecond)
			input := stamps
			var previous *canonicalAttemptWindow
			switch state {
			case "absent":
				record.acceptedAttempt = nil
			case "other":
				input = record.newCanonicalPatchStamps()
				input.markAt(PublicationCycleStarted, origin.Add(3*time.Millisecond))
				input.markAt(PublicationPatchExecutionStarted, origin.Add(4*time.Millisecond))
				input.markAt(PublicationPatchExecutionFinished, origin.Add(5*time.Millisecond))
			case "prior_window":
				_, accepted, _, _ := record.completePhysicalAttempt(stamps, start, end, 0)
				require.True(t, accepted)
				previous = record.attemptWindow
			}
			window, accepted, reason, guard := record.completePhysicalAttempt(input, start, end, 0)
			require.False(t, accepted)
			require.Equal(t, canonicalAttemptWindow{}, window)
			require.Equal(t, "accepted_stamps_mismatch", reason, "the original first-failed guard order is unchanged")
			require.True(t, guard.observed)
			require.Equal(t, state == "prior_window", guard.windowPresent)
			if state == "other" {
				require.Equal(t, "other", guard.acceptedStamps)
			} else {
				require.Equal(t, "absent", guard.acceptedStamps)
			}
			if previous == nil {
				require.Nil(t, record.attemptWindow)
			} else {
				require.Same(t, previous, record.attemptWindow, "diagnosis never replaces a prior physical window")
			}
		})
	}
}

func TestPhysicalAttemptGuardDiagnosticFreezesRekeyIdentity(t *testing.T) {
	recorder, record, stamps, origin := attemptWindowFixture(t)
	record.acceptedAttempt = nil
	_, accepted, reason, guard := record.completePhysicalAttempt(stamps, origin.Add(2*time.Millisecond), origin.Add(8*time.Millisecond), 0)
	require.False(t, accepted)
	require.Equal(t, "accepted_stamps_mismatch", reason)
	require.Equal(t, recorder.sharedInstance, guard.recorder)
	require.NotEmpty(t, guard.recorder)
	require.Equal(t, "original", guard.key)
	require.EqualValues(t, 7, guard.recordTicket)
	require.EqualValues(t, 7, guard.attemptTicket)
	require.True(t, recorder.Rekey("original", "renamed"))
	require.Equal(t, "renamed", record.Key())
	require.Equal(t, "original", guard.key, "the guard key is not a later Key() lookup")
}

func TestPhysicalAttemptGuardDiagnosticUnobservedWithoutOwner(t *testing.T) {
	_, record, stamps, origin := attemptWindowFixture(t)
	record.owner = weak.Pointer[PublicationPhaseRecorder]{}
	_, accepted, reason, guard := record.completePhysicalAttempt(stamps, origin.Add(2*time.Millisecond), origin.Add(8*time.Millisecond), 0)
	require.False(t, accepted)
	require.Equal(t, "recorder_owner_unavailable", reason)
	require.False(t, guard.observed, "unavailable state is not a fabricated absent-pointer observation")
}

func TestPointAttemptDiagnosticReportsLegalCoveredFallback(t *testing.T) {
	path, _, w := pointPublicationFixture(t)
	core, logs := observer.New(zap.InfoLevel)
	w.logger = zap.New(core)
	beforeComplete, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	t.Cleanup(unblock)
	w.mutationBeforeComplete = func(_ string, generation uint64) {
		if generation == 1 {
			close(beforeComplete)
			<-release
		}
	}
	older := newPointPublication(t, "covered-diagnostic")
	first, err := w.EnqueueFileMutation(WithPublicationRecord(t.Context(), older), path)
	require.NoError(t, err)
	awaitPublicationSignal(t, beforeComplete)
	bumpMtime(t, path, "package p\n\nfunc NewerDiagnostic() {}\n")
	newer := newPointPublication(t, "winner-diagnostic")
	second, err := w.EnqueueFileMutation(WithPublicationRecord(t.Context(), newer), path)
	require.NoError(t, err)
	unblock()
	for _, ticket := range []*MutationTicket{first, second} {
		result := awaitMutationRetryResult(t, ticket)
		require.NoError(t, result.Err)
		require.True(t, result.Reindexed)
		require.Equal(t, ticket.Generation, result.AppliedGeneration)
	}
	require.Eventually(t, func() bool {
		return logs.FilterMessage("graph patch attempt timing unavailable").FilterField(zap.String("attempt_guard_publication_key", older.Key())).Len() == 1
	}, 2*time.Second, time.Millisecond)
	fields := logs.FilterMessage("graph patch attempt timing unavailable").FilterField(zap.String("attempt_guard_publication_key", older.Key())).All()[0].ContextMap()
	require.Equal(t, "accepted_stamps_mismatch", fields["attempt_rejection_reason"])
	require.Equal(t, "absent", fields["attempt_guard_accepted_stamps"])
	require.Equal(t, false, fields["attempt_guard_window_present"])
	require.Equal(t, true, fields["attempt_completion_observed"])
	require.Equal(t, "covered_without_acceptance", fields["attempt_completion_disposition"])
	require.Equal(t, true, fields["attempt_completion_fallback"])
	require.Equal(t, first.Generation, fields["attempt_completion_applied"])
	require.Equal(t, second.Generation, fields["attempt_completion_pending_at_check"])
	require.EqualValues(t, 1, fields["attempt_completion_count"])
	requirePointExecution(t, older, false)
	require.Eventually(t, func() bool { return newer.Snapshot().attemptWindow != nil }, 2*time.Second, time.Millisecond)
	requirePointExecution(t, newer, true)
}

func TestPointAttemptDiagnosticReportsTerminalBeforeAbsorb(t *testing.T) {
	path, _, w := pointPublicationFixture(t)
	core, logs := observer.New(zap.InfoLevel)
	w.logger = zap.New(core)
	record := newPointPublication(t, "terminal-before-absorb")
	w.mutationBeforeComplete = func(string, uint64) { record.Mark(PublicationTicketCompleted) }
	ticket, err := w.EnqueueFileMutation(WithPublicationRecord(t.Context(), record), path)
	require.NoError(t, err)
	require.NoError(t, awaitMutationRetryResult(t, ticket).Err)
	require.Eventually(t, func() bool { return logs.FilterMessage("graph patch attempt timing unavailable").Len() == 1 }, 2*time.Second, time.Millisecond)
	fields := logs.FilterMessage("graph patch attempt timing unavailable").All()[0].ContextMap()
	require.Equal(t, "accepted_stamps_mismatch", fields["attempt_rejection_reason"])
	require.Equal(t, "selected_for_absorb", fields["attempt_completion_disposition"], "selection is not falsely called accepted stamps")
	require.Equal(t, false, fields["attempt_completion_fallback"])
	require.Equal(t, "absent", fields["attempt_guard_accepted_stamps"])
	require.Equal(t, true, fields["attempt_guard_terminal"])
	require.Equal(t, false, fields["attempt_guard_patch_present"])
	require.Nil(t, record.Snapshot().attemptWindow)
}
