package indexer

import (
	"runtime"
	"sync"
	"testing"
	"time"
	"weak"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

func TestCanonicalAttemptWindowFinishesAfterDoneAndCohortRelease(t *testing.T) {
	path, _, w := pointPublicationFixture(t)
	core, logs := observer.New(zap.InfoLevel)
	w.logger = zap.New(core)
	slots := w.mutationSlots()
	drained := make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { slots <- struct{}{} }) }
	t.Cleanup(unblock)
	w.mutationBeforeComplete = func(string, uint64) { <-slots; close(drained) }
	record := newPointPublication(t, "late-physical")
	ticket, err := w.EnqueueFileMutation(WithPublicationRecord(t.Context(), record), path)
	require.NoError(t, err)
	awaitPublicationSignal(t, drained)
	require.NoError(t, awaitMutationRetryResult(t, ticket).Err, "Done delivery precedes cohort release exactly as before")
	record.Mark(PublicationTicketCompleted)
	early := record.Snapshot()
	earlyTimings := early.EditTimings()
	require.Equal(t, "measured", earlyTimings.States["derived"], "open finalization must not invalidate the frozen completed executor")
	require.Nil(t, earlyTimings.Window.PatchWallMS, "actual return is still blocked on cohort release")
	require.Empty(t, logs.FilterMessage("graph patch attempt complete").All())
	completedNS := phaseOffsets(early)[PublicationTicketCompleted]
	require.Eventually(t, func() bool { return time.Since(record.origin).Nanoseconds() > completedNS },
		5*time.Second, time.Millisecond, "the held cohort cannot return until the observed clock advances past Done")
	releaseAt := time.Now()
	unblock()
	require.Eventually(t, func() bool { return record.Snapshot().attemptWindow != nil }, 2*time.Second, time.Millisecond)
	late := record.Snapshot()
	require.GreaterOrEqual(t, late.attemptWindow.endNS, releaseAt.Sub(record.origin).Nanoseconds())
	require.Greater(t, late.attemptWindow.endNS, completedNS, "P can honestly outlive receipt terminal")
	require.NotNil(t, late.EditTimings().Window.PatchWallMS)
	require.Equal(t, "partial", late.EditTimings().Window.Coverage)
	require.Equal(t, earlyTimings, early.EditTimings(), "previously exposed terminal snapshots remain immutable")
	require.Equal(t, early.Phases, late.Phases, "late physical observation never edits the frozen terminal timeline")
	require.Eventually(t, func() bool { return logs.FilterMessage("graph patch attempt complete").Len() == 1 }, 2*time.Second, time.Millisecond)
	fields := logs.FilterMessage("graph patch attempt complete").All()[0].ContextMap()
	require.Equal(t, ticket.Generation, fields["attempt_ticket"])
	accounting := fields["attempt_accounting"].(map[string]any)
	require.Equal(t, fields["attempt_ticket"], accounting["attempt_ticket"], "both log views use the same frozen identity")
	require.EqualValues(t, fields["attempt_retry"], accounting["attempt_retry"])
	require.Equal(t, int64(0), fields["attempt_retry"])
	require.Equal(t, *late.EditTimings().Window.PatchWallMS, fields["attempt_wall_ms"])
	require.Equal(t, "accepted_physical_attempt_complete", fields["patch_lap_scope"])
	require.Equal(t, "physical_attempt_before_receipt_validation", logs.FilterMessage("graph patch complete").All()[0].ContextMap()["patch_lap_scope"])
}

func attemptWindowFixture(t *testing.T) (*PublicationPhaseRecorder, *PublicationPhaseRecord, *PublicationStamps, time.Time) {
	t.Helper()
	origin := time.Now()
	recorder := NewPublicationPhaseRecorder(1, 1)
	record := recorder.Begin("", "original", PublicationSourceMCPEdit, origin)
	require.True(t, record.bindCanonicalTicket(7, origin.Add(time.Millisecond)))
	stamps := record.newCanonicalPatchStamps()
	stamps.markAt(PublicationCycleStarted, origin.Add(3*time.Millisecond))
	stamps.markAt(PublicationPatchExecutionStarted, origin.Add(4*time.Millisecond))
	stamps.markAt(PublicationPatchExecutionFinished, origin.Add(5*time.Millisecond))
	record.Absorb(stamps)
	t.Cleanup(func() { runtime.KeepAlive(recorder) })
	return recorder, record, stamps, origin
}

func TestCanonicalAttemptWindowRequiresMembershipExactWinnerAndOneShot(t *testing.T) {
	for _, name := range []string{"winner_after_terminal", "rekey_same_record", "evicted_rebound_key", "wrong_attempt", "failed_terminal", "absent_owner", "invalid_order", "missing_callback", "before_origin", "one_shot"} {
		t.Run(name, func(t *testing.T) {
			recorder, record, stamps, origin := attemptWindowFixture(t)
			start, end := origin.Add(2*time.Millisecond), origin.Add(8*time.Millisecond)
			want := false
			switch name {
			case "winner_after_terminal":
				record.MarkAt(PublicationTicketCompleted, origin.Add(6*time.Millisecond))
				want = true
			case "rekey_same_record":
				require.True(t, recorder.Rekey("original", "receipt"))
				want = true
			case "evicted_rebound_key":
				recorder.Begin("", "other", PublicationSourceMCPEdit, origin)
				replacement := recorder.Begin("", "original", PublicationSourceMCPEdit, origin)
				require.NotSame(t, record, replacement)
			case "wrong_attempt":
				stamps = record.newCanonicalPatchStamps()
				stamps.markAt(PublicationCycleStarted, origin.Add(3*time.Millisecond))
			case "failed_terminal":
				record.MarkAt(PublicationTicketFailed, origin.Add(6*time.Millisecond))
			case "absent_owner":
				record.owner = weak.Pointer[PublicationPhaseRecorder]{}
			case "invalid_order":
				end = origin.Add(time.Millisecond)
			case "missing_callback":
				start = time.Time{}
			case "before_origin":
				start = origin.Add(-time.Millisecond)
			case "one_shot":
				_, ok, reason, _ := record.completePhysicalAttempt(stamps, start, end, 0)
				require.True(t, ok)
				require.Empty(t, reason)
			}
			_, ok, reason, _ := record.completePhysicalAttempt(stamps, start, end, 0)
			require.Equal(t, want, ok)
			if ok {
				require.Empty(t, reason)
			} else {
				require.NotEmpty(t, reason)
			}
			if name == "evicted_rebound_key" {
				replacement, _ := recorder.Lookup("original")
				require.Nil(t, replacement.Snapshot().attemptWindow)
			}
			require.False(t, record.bindCanonicalTicket(8, end), "a record cannot be rebound to a newer generation")
		})
	}
}

func TestCanonicalAttemptWindowRejectionReasons(t *testing.T) {
	for _, reason := range []string{
		"nil_record", "nil_stamps", "callback_start_unobserved", "callback_end_unobserved", "invalid_retry",
		"callback_before_origin", "callback_return_before_entry", "patch_laps_unobserved",
		"captured_callback_window_mismatch", "cycle_before_callback_entry", "stamp_after_callback_return", "cycle_start_unobserved",
		"recorder_owner_unavailable", "recorder_membership_mismatch", "canonical_ticket_unbound", "attempt_ticket_mismatch",
		"accepted_stamps_mismatch", "physical_window_already_recorded", "publication_ticket_failed",
	} {
		t.Run(reason, func(t *testing.T) {
			recorder, record, stamps, origin := attemptWindowFixture(t)
			start, end, retry := origin.Add(2*time.Millisecond), origin.Add(8*time.Millisecond), 0
			switch reason {
			case "nil_record":
				record = nil
			case "nil_stamps":
				stamps = nil
			case "callback_start_unobserved":
				start = time.Time{}
			case "callback_end_unobserved":
				end = time.Time{}
			case "invalid_retry":
				retry = -1
			case "callback_before_origin":
				start = origin.Add(-time.Millisecond)
			case "callback_return_before_entry":
				end = origin.Add(time.Millisecond)
			case "patch_laps_unobserved":
				stamps.patchLaps = nil
			case "captured_callback_window_mismatch":
				stamps, origin, _ = controlledAttemptWindow(t)
				recorder = NewPublicationPhaseRecorder(1, 1)
				t.Cleanup(func() { runtime.KeepAlive(recorder) })
				record = recorder.Begin("", "callback-origin", PublicationSourceMCPEdit, origin)
				require.True(t, record.bindCanonicalTicket(1, origin))
				record.Absorb(stamps)
				start, end = origin.Add(500*time.Microsecond), origin.Add(31*time.Millisecond)
			case "cycle_before_callback_entry":
				start = origin.Add(4 * time.Millisecond)
			case "stamp_after_callback_return":
				end = origin.Add(4 * time.Millisecond)
			case "cycle_start_unobserved":
				stamps.marks = stamps.marks[1:]
			case "recorder_owner_unavailable":
				record.owner = weak.Pointer[PublicationPhaseRecorder]{}
			case "recorder_membership_mismatch":
				delete(recorder.byKey, record.key)
			case "canonical_ticket_unbound":
				record.canonicalBound = false
			case "attempt_ticket_mismatch":
				record.ticket++
			case "accepted_stamps_mismatch":
				record.acceptedAttempt = nil
			case "physical_window_already_recorded":
				record.attemptWindow = &canonicalAttemptWindow{ticket: 7}
			case "publication_ticket_failed":
				record.MarkAt(PublicationTicketFailed, origin.Add(6*time.Millisecond))
			}
			var previous *canonicalAttemptWindow
			if record != nil {
				previous = record.attemptWindow
			}
			window, accepted, got, _ := record.completePhysicalAttempt(stamps, start, end, retry)
			require.False(t, accepted)
			require.Equal(t, canonicalAttemptWindow{}, window)
			require.Equal(t, reason, got)
			if record != nil {
				if previous == nil {
					require.Nil(t, record.attemptWindow)
				} else {
					require.Same(t, previous, record.attemptWindow, "a refusal cannot replace an existing window")
				}
			}
		})
	}
}

func TestCanonicalAttemptWindowKeepsFirstFailureAndFinishesControl(t *testing.T) {
	for _, first := range []string{"patch_laps_unobserved", "cycle_before_callback_entry", "stamp_after_callback_return"} {
		t.Run(first, func(t *testing.T) {
			_, record, stamps, origin := attemptWindowFixture(t)
			start, end := origin.Add(2*time.Millisecond), origin.Add(8*time.Millisecond)
			stamps.startAttemptControl(start)
			early := publicationMark{phase: PublicationCycleStarted, at: origin.Add(time.Millisecond)}
			late := publicationMark{phase: PublicationPatchExecutionFinished, at: origin.Add(9 * time.Millisecond)}
			stamps.marks = []publicationMark{early, late}
			switch first {
			case "patch_laps_unobserved":
				stamps.patchLaps = nil
			case "stamp_after_callback_return":
				stamps.marks = []publicationMark{late, early}
			}
			_, accepted, reason, _ := record.completePhysicalAttempt(stamps, start, end, 0)
			require.False(t, accepted)
			require.Equal(t, first, reason)
			require.Nil(t, record.attemptWindow)
			if stamps.patchLaps != nil {
				require.True(t, stamps.patchLaps.controlFinished, "stamp refusal still finishes the existing control")
				require.Equal(t, end, stamps.patchLaps.attemptFinished)
			}
		})
	}
	_, record, stamps, origin := attemptWindowFixture(t)
	stamps.marks = []publicationMark{{phase: PublicationPatchExecutionFinished, at: origin.Add(9 * time.Millisecond)}}
	_, accepted, reason, _ := record.completePhysicalAttempt(stamps, origin.Add(2*time.Millisecond), origin.Add(8*time.Millisecond), 0)
	require.False(t, accepted)
	require.Equal(t, "stamp_after_callback_return", reason, "a missing cycle cannot replace an earlier failed predicate")
}

func TestCanonicalAttemptWindowLogsRejectedTimingOutsideLocks(t *testing.T) {
	recorder, record, stamps, origin := attemptWindowFixture(t)
	require.True(t, recorder.Rekey("original", "observed-current-key"))
	record.acceptedAttempt = nil
	before := record.Snapshot()
	core, logs := observer.New(zap.InfoLevel)
	var held []string
	hooks := 0
	logger := zap.New(core, zap.Hooks(func(entry zapcore.Entry) error {
		if entry.Message != "graph patch attempt timing unavailable" {
			return nil
		}
		hooks++
		for _, lock := range []struct {
			name string
			mu   *sync.Mutex
		}{{"stamps", &stamps.mu}, {"owner", &recorder.mu}, {"record", &record.mu}} {
			if lock.mu.TryLock() {
				lock.mu.Unlock()
			} else {
				held = append(held, lock.name)
			}
		}
		return nil
	}))
	attempt := &pointPublicationAttempt{record: record, stamps: stamps}
	for i := 1; i <= 2; i++ {
		attempt.finishPhysicalAttempt(origin.Add(2*time.Millisecond), origin.Add(8*time.Millisecond), 3, logger)
		require.Len(t, logs.All(), i, "one synchronous entry per rejected invocation")
	}
	require.Equal(t, 2, hooks)
	require.Empty(t, held, "logging must run after all guard locks are released")
	for _, entry := range logs.All() {
		require.Equal(t, "graph patch attempt timing unavailable", entry.Message)
		fields := entry.ContextMap()
		require.Len(t, fields, 19)
		require.Equal(t, true, fields["attempt_guard_observed"])
		require.Equal(t, recorder.sharedInstance, fields["attempt_guard_recorder"])
		require.Equal(t, "observed-current-key", fields["attempt_guard_publication_key"])
		require.EqualValues(t, 7, fields["attempt_guard_ticket"])
		require.EqualValues(t, 7, fields["attempt_guard_collector_ticket"])
		require.Equal(t, "absent", fields["attempt_guard_accepted_stamps"])
		require.Equal(t, false, fields["attempt_guard_window_present"])
		require.Equal(t, false, fields["attempt_guard_terminal"])
		require.Equal(t, true, fields["attempt_guard_patch_present"])
		require.Equal(t, false, fields["attempt_completion_observed"], "direct helper calls have no fabricated completion disposition")
		require.Empty(t, fields["attempt_completion_disposition"])
		require.Equal(t, false, fields["attempt_completion_fallback"])
		require.Zero(t, fields["attempt_completion_applied"])
		require.Zero(t, fields["attempt_completion_pending_at_check"])
		require.Zero(t, fields["attempt_completion_count"])
		require.Equal(t, "physical_attempt_timing_unavailable", fields["patch_lap_scope"])
		require.Equal(t, "observed-current-key", fields["publication_key"])
		require.EqualValues(t, 3, fields["attempt_retry"])
		require.Equal(t, "accepted_stamps_mismatch", fields["attempt_rejection_reason"])
		require.NotContains(t, fields, "attempt_wall_ms")
		require.NotContains(t, fields, "attempt_ticket")
		require.NotContains(t, fields, "attempt_accounting")
	}
	require.Empty(t, logs.FilterMessage("graph patch attempt complete").All())
	require.Nil(t, record.attemptWindow)
	after := record.Snapshot()
	require.Equal(t, before.Phases, after.Phases)
	require.Equal(t, before.EditTimings(), after.EditTimings())
	require.Nil(t, record.RequestedBackground(), "a diagnostic cannot create P/B coverage")
}

func TestCanonicalAttemptWindowRejectedTimingLoggingNeedsLoggerAndIdentity(t *testing.T) {
	for _, name := range []string{"nil_logger", "nil_record", "empty_key"} {
		t.Run(name, func(t *testing.T) {
			_, record, stamps, origin := attemptWindowFixture(t)
			record.acceptedAttempt = nil
			core, logs := observer.New(zap.InfoLevel)
			logger := zap.New(core)
			attempt := &pointPublicationAttempt{record: record, stamps: stamps}
			switch name {
			case "nil_logger":
				logger = nil
			case "nil_record":
				attempt.record = nil
			case "empty_key":
				record.key = ""
			}
			attempt.finishPhysicalAttempt(origin.Add(2*time.Millisecond), origin.Add(8*time.Millisecond), 0, logger)
			require.Empty(t, logs.All())
			require.Nil(t, record.attemptWindow)
		})
	}
}
