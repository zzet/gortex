package indexer

import (
	"context"
	"math"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestRequestedBackgroundMeasuresCanceledInitialTimerAndActualP(t *testing.T) {
	path, _, w := pointPublicationFixture(t)
	w.config.DebounceMs = int(time.Hour / time.Millisecond)
	record := newPointPublication(t, "background")
	reached, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(unblock)
	w.mutationBeforeAdmission = func() { close(reached); <-release }
	type admitted struct {
		ticket *MutationTicket
		err    error
	}
	admission := make(chan admitted, 1)
	go func() {
		ticket, err := w.EnqueueFileMutation(WithPublicationRecord(t.Context(), record), path)
		admission <- admitted{ticket, err}
	}()
	awaitPublicationSignal(t, reached)
	gateReleased := time.Now()
	unblock()
	got := <-admission
	require.NoError(t, got.err)
	first := got.ticket
	require.NotNil(t, first)
	w.mu.Lock()
	owner := w.mutationWaiters[path][first.Generation].timerObservation.background
	w.mu.Unlock()
	require.NotNil(t, owner)
	owner.mu.Lock()
	entered, armStarted := owner.origin, owner.armStart
	owner.mu.Unlock()
	require.False(t, entered.After(gateReleased), "actual scheduler entry precedes blocked admission")
	require.False(t, armStarted.Before(gateReleased), "admission delay remains inside the measured prefix")
	before := record.RequestedBackground()
	require.Nil(t, before)
	native := w.scheduleFileMutation(path, ChangeModified)
	require.NotNil(t, native)
	w.mu.Lock()
	w.pending[path].Reset(time.Millisecond)
	w.mu.Unlock()
	result := awaitMutationRetryResult(t, first)
	require.NoError(t, result.Err)
	require.Equal(t, native.Generation, result.AppliedGeneration)
	require.NoError(t, awaitMutationRetryResult(t, native).Err)
	require.Eventually(t, func() bool { return record.RequestedBackground() != nil }, 5*time.Second, time.Millisecond)
	background := record.RequestedBackground()
	require.True(t, background.PartitionComplete)
	require.Equal(t, first.Generation, background.RequestedGeneration)
	require.Equal(t, native.Generation, background.AppliedGeneration)
	require.Equal(t, entered.UTC().Format(time.RFC3339Nano), background.OriginWall)
	require.Equal(t, "accepted_scheduling_call_entry_to_serving_attempt_return", background.Scope)
	require.GreaterOrEqual(t, background.PhasesMS["scheduling_admission_setup"], 0.0)
	require.Greater(t, background.PhasesMS["successor_quiet_wait"], 0.0)
	require.Zero(t, background.UnattributedMS)
	winner := record.CoalescedPublication()
	require.NotNil(t, winner.Background, "B and P come from one related immutable observation")
	require.NotNil(t, winner.Timings.Window.PatchWallMS)
	require.NotNil(t, winner.Timings.Milliseconds["debounce"])
	require.NotContains(t, background.PhasesMS, "debounce", "the whole pair is not added beside B's arm/quiet split")
	require.InDelta(t, *winner.Timings.Milliseconds["debounce"], background.PhasesMS["successor_timer_arm"]+background.PhasesMS["successor_quiet_wait"], 0.000001)
	requirePointExecution(t, record, false)
	sum := 0.0
	for _, value := range background.PhasesMS {
		sum += value
	}
	require.InDelta(t, background.WallMS, sum, 0.000001, "wire conversion only; production validates integer totals")
	background.PhasesMS["timer_stop"] = 999
	require.NotEqual(t, 999.0, record.RequestedBackground().PhasesMS["timer_stop"], "projections own their maps")
	require.Nil(t, before, "an earlier observation cannot acquire late evidence")
	// A retained original cannot resurrect the independent winner after eviction.
	recorder := record.owner.Value()
	require.NotNil(t, recorder)
	recorder.Begin("", "eviction", PublicationSourceMCPEdit, time.Now())
	recorder.Begin("", "eviction2", PublicationSourceMCPEdit, time.Now())
	require.Nil(t, record.RequestedBackground())
	runtime.KeepAlive(recorder)
}

func TestRequestedBackgroundPreservesAcceptedWinnerDuringLaterAdmission(t *testing.T) {
	path, _, w := pointPublicationFixture(t)
	w.config.DebounceMs = int(time.Hour / time.Millisecond)
	slots := w.mutationSlots()
	drained := make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { slots <- struct{}{} }) }
	t.Cleanup(unblock)
	w.mutationBeforeComplete = func(string, uint64) { <-slots; close(drained) }
	record := newPointPublication(t, "background-late-admission")
	first := w.scheduleFileMutationWithRecord(path, ChangeModified, record)
	native := w.scheduleFileMutation(path, ChangeModified)
	w.mu.Lock()
	w.pending[path].Reset(time.Millisecond)
	w.mu.Unlock()
	awaitPublicationSignal(t, drained)
	require.NoError(t, awaitMutationRetryResult(t, first).Err)
	require.NoError(t, awaitMutationRetryResult(t, native).Err)
	require.Nil(t, record.RequestedBackground())
	early := record.CoalescedPublication()
	require.NotNil(t, early)
	require.Nil(t, early.Background)
	require.Nil(t, early.Timings.Window.PatchWallMS)
	// Completion already accepted the serving identity and retired its waiter.
	// Later work cannot erase this historical observation during final release.
	w.scheduleFileMutation("later.go", ChangeModified)
	unblock()
	require.Eventually(t, func() bool {
		winner := record.CoalescedPublication()
		return winner != nil && winner.Timings.Window.PatchWallMS != nil
	}, 5*time.Second, time.Millisecond)
	require.Eventually(t, func() bool { return record.RequestedBackground() != nil }, 5*time.Second, time.Millisecond)
	require.Equal(t, native.Generation, record.RequestedBackground().AppliedGeneration)
	late := record.CoalescedPublication()
	require.NotNil(t, late.Background)
	require.NotNil(t, late.Timings.Window.PatchWallMS, "one response cannot pair complete B with older missing P")
	require.Nil(t, early.Background)
	require.Nil(t, early.Timings.Window.PatchWallMS, "earlier related snapshots remain immutable")
}

func TestRequestedBackgroundMeasuresNativeChainButRefusesObservedPredecessor(t *testing.T) {
	for _, secondObserved := range []bool{false, true} {
		t.Run(map[bool]string{false: "second_native_reset", true: "second_observed_owner"}[secondObserved], func(t *testing.T) {
			path, _, w := pointPublicationFixture(t)
			w.config.DebounceMs = int(time.Hour / time.Millisecond)
			firstRecord := newPointPublication(t, "first-background")
			first := w.scheduleFileMutationWithRecord(path, ChangeModified, firstRecord)
			secondRecord := newPointPublication(t, "second-background")
			var second *MutationTicket
			if secondObserved {
				second = w.scheduleFileMutationWithRecord(path, ChangeModified, secondRecord)
			} else {
				second = w.scheduleFileMutation(path, ChangeModified)
			}
			third := w.scheduleFileMutation(path, ChangeModified)
			w.mu.Lock()
			w.pending[path].Reset(time.Millisecond)
			w.mu.Unlock()
			for _, ticket := range []*MutationTicket{first, second, third} {
				require.NoError(t, awaitMutationRetryResult(t, ticket).Err)
			}
			require.Eventually(t, func() bool {
				r := firstRecord
				if secondObserved {
					r = secondRecord
				}
				winner := r.CoalescedPublication()
				return winner != nil && winner.Timings.Window.PatchWallMS != nil
			}, 5*time.Second, time.Millisecond)
			if secondObserved {
				require.Nil(t, firstRecord.RequestedBackground())
				_, firstFailure := firstRecord.CoalescedPublicationEvidence()
				_, secondFailure := secondRecord.CoalescedPublicationEvidence()
				require.NotNil(t, firstFailure)
				require.NotNil(t, secondFailure)
				require.Equal(t, "observed_successor", firstFailure.Reason)
				require.Equal(t, "preexisting_path_work", secondFailure.Reason)
			} else {
				require.Eventually(t, func() bool { return firstRecord.RequestedBackground() != nil }, 5*time.Second, time.Millisecond)
				background := firstRecord.RequestedBackground()
				require.Equal(t, uint64(2), background.NativeHandoffs)
				require.Equal(t, third.Generation, background.AppliedGeneration)
				require.Equal(t, "successful_pre_callback_native_handoffs", background.Coverage)
			}
			require.Nil(t, secondRecord.RequestedBackground())
		})
	}
}

func TestRequestedBackgroundRejectsIncompleteOrInvalidIntegerBounds(t *testing.T) {
	_, origin, window := controlledAttemptWindow(t)
	window.ticket = 2
	valid := canonicalBackgroundEvidence{origin: origin, requested: 1, applied: 2, handoffs: 1, physical: window, debounce: canonicalDebounceEvidence{startedNS: 0, enteredNS: 1e6, entered: true}, phases: [7]int64{1, 1, 1, 1, 1, 1, 999999}, lastArmStartNS: 5, startNS: 1e6 + 5, endNS: 31e6 + 5}
	require.True(t, valid.valid())
	for _, name := range []string{"missing", "reversed", "overflow", "incomplete_P", "wrong_winner", "retry", "gap", "debounce_disagrees", "debounce_missing"} {
		t.Run(name, func(t *testing.T) {
			b := valid
			switch name {
			case "missing":
				b.origin = time.Time{}
			case "reversed":
				b.phases[3] = -1
			case "overflow":
				b.phases[6] = math.MaxInt64
			case "incomplete_P":
				b.physical.accounting.laps[patchParse] = canonicalLapEvidence{}
			case "wrong_winner":
				b.applied = 3
			case "retry":
				b.physical.retry = 1
			case "gap":
				b.endNS++
			case "debounce_disagrees":
				b.debounce.enteredNS++
			case "debounce_missing":
				b.debounce.entered = false
			}
			require.False(t, b.valid())
		})
	}
}

func TestRequestedBackgroundCallbackAndStopFailuresInvalidateOwnership(t *testing.T) {
	for _, name := range []string{"callback_ran", "stop_false", "observed_successor", "retry", "second_stop"} {
		t.Run(name, func(t *testing.T) {
			record := newPointPublication(t, "background-invalid")
			w := &Watcher{}
			b := newCanonicalBackground(record, w, "sample.go", 1, time.Now())
			require.NotNil(t, b)
			switch name {
			case "callback_ran":
				b.callback(w, "sample.go", 1, false, time.Now())
			case "stop_false":
				b.stopped(w, "sample.go", 1, false, time.Now(), time.Now(), false, true)
			case "observed_successor":
				b.stopped(w, "sample.go", 1, false, time.Now(), time.Now(), true, false)
			case "retry":
				b.invalidate("retry_or_backoff")
			case "second_stop":
				b.stopped(w, "sample.go", 1, false, time.Now(), time.Now(), true, true)
				b.stopped(w, "sample.go", 1, true, time.Now(), time.Now(), true, true)
			}
			b.mu.Lock()
			invalid := b.invalid
			b.mu.Unlock()
			require.True(t, invalid)
			require.Nil(t, record.RequestedBackground())
		})
	}
}

func TestCanonicalSchedulingEntryKeepsOuterIdentityAndNativeAbstention(t *testing.T) {
	plain := context.Background()
	ctx, at := canonicalSchedulingEntry(plain)
	require.Equal(t, plain, ctx)
	require.True(t, at.IsZero())
	require.Zero(t, testing.AllocsPerRun(100, func() { canonicalSchedulingEntry(plain) }))
	original := WithPublicationRecord(plain, newPointPublication(t, "entry"))
	outer, entered := canonicalSchedulingEntry(original)
	require.False(t, entered.IsZero())
	inner, reused := canonicalSchedulingEntry(outer)
	require.Same(t, outer, inner)
	require.Equal(t, entered, reused)
}

func TestRequestedBackgroundRejectsSupersessionBeforeAcceptance(t *testing.T) {
	path, _, w := pointPublicationFixture(t)
	w.config.DebounceMs = int(time.Hour / time.Millisecond)
	// Retain the original plus both independent physical attempts; otherwise
	// this exercises bounded eviction before it can inspect supersession.
	recorder := NewPublicationPhaseRecorder(4, 1)
	t.Cleanup(func() { runtime.KeepAlive(recorder) })
	record := recorder.Begin("", "background-before-acceptance", PublicationSourceMCPEdit, time.Now())
	first := w.scheduleFileMutationWithRecord(path, ChangeModified, record)
	native := w.scheduleFileMutation(path, ChangeModified)
	reached, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	t.Cleanup(unblock)
	w.mutationBeforeComplete = func(_ string, generation uint64) {
		if generation == native.Generation {
			close(reached)
			<-release
		}
	}
	w.mu.Lock()
	w.pending[path].Reset(time.Millisecond)
	w.mu.Unlock()
	awaitPublicationSignal(t, reached)
	require.Nil(t, record.RequestedBackground())
	newer := w.scheduleFileMutation(path, ChangeModified)
	w.mu.Lock()
	w.pending[path].Reset(time.Millisecond)
	w.mu.Unlock()
	unblock()
	for _, ticket := range []*MutationTicket{first, native, newer} {
		result := awaitMutationRetryResult(t, ticket)
		require.NoError(t, result.Err)
		require.Equal(t, newer.Generation, result.AppliedGeneration)
	}
	require.Eventually(t, func() bool {
		winner := record.CoalescedPublication()
		return winner != nil && winner.Timings.Window.PatchWallMS != nil
	}, 5*time.Second, time.Millisecond)
	require.Equal(t, newer.Generation, record.CoalescedPublication().AppliedGeneration)
	require.Nil(t, record.RequestedBackground(), "the unobserved superseding quiet/reset chain is unavailable")
	_, unavailable := record.CoalescedPublicationEvidence()
	require.NotNil(t, unavailable)
	require.Equal(t, "serving_attempt_binding_mismatch", unavailable.Reason)
}
