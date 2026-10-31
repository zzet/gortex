package indexer

import (
	"runtime"
	"sync"
	"testing"
	"time"
	"weak"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
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
				_, ok := record.completePhysicalAttempt(stamps, start, end, 0)
				require.True(t, ok)
			}
			_, ok := record.completePhysicalAttempt(stamps, start, end, 0)
			require.Equal(t, want, ok)
			if name == "evicted_rebound_key" {
				replacement, _ := recorder.Lookup("original")
				require.Nil(t, replacement.Snapshot().attemptWindow)
			}
			require.False(t, record.bindCanonicalTicket(8, end), "a record cannot be rebound to a newer generation")
		})
	}
}
