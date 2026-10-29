package indexer

import (
	"math"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func controlledBackgroundHandoff(t *testing.T) (*canonicalBackgroundOwner, *Watcher, *PublicationPhaseRecord, time.Time) {
	t.Helper()
	recorder := NewPublicationPhaseRecorder(2, 1)
	w := &Watcher{}
	t.Cleanup(func() { runtime.KeepAlive(recorder); runtime.KeepAlive(w) })
	origin := time.Now()
	record := recorder.Begin("", "controlled-background", PublicationSourceMCPEdit, origin)
	require.True(t, record.bindCanonicalTicket(10, origin))
	b := newCanonicalBackground(record, w, "sample.go", 10, origin)
	b.armStarted(w, "sample.go", 10, false, origin.Add(time.Nanosecond))
	b.armFinished(w, "sample.go", 10, false, origin.Add(2*time.Nanosecond))
	return b, w, record, origin
}

func TestRequestedBackgroundAccumulatesActualOrderedHandoffsNotGenerationDelta(t *testing.T) {
	b, w, _, origin := controlledBackgroundHandoff(t)
	require.Same(t, b, b.stopped(w, "sample.go", 10, false, origin.Add(3*time.Nanosecond), origin.Add(4*time.Nanosecond), true, true))
	// Other paths may consume intervening global generations. Only these actual
	// Stop/arm observations count; a generation delta is never measured work.
	b.armStarted(w, "sample.go", 40, true, origin.Add(5*time.Nanosecond))
	b.armFinished(w, "sample.go", 40, true, origin.Add(6*time.Nanosecond))
	require.Same(t, b, b.stopped(w, "sample.go", 40, true, origin.Add(7*time.Nanosecond), origin.Add(8*time.Nanosecond), true, true))
	b.armStarted(w, "sample.go", 90, true, origin.Add(9*time.Nanosecond))
	b.armFinished(w, "sample.go", 90, true, origin.Add(10*time.Nanosecond))
	b.callback(w, "sample.go", 90, true, origin.Add(11*time.Nanosecond))
	require.False(t, b.invalid)
	require.Equal(t, uint64(2), b.handoffs)
	require.Equal(t, uint64(90), b.current)
	require.Equal(t, [7]int64{1, 1, 1, 2, 2, 2, 2}, b.phases)
	require.Equal(t, backgroundExecuting, b.stage)
}

func TestRequestedBackgroundHandoffRefusesForeignAndUnobservedTransitions(t *testing.T) {
	for _, name := range []string{"owner", "path", "generation", "stop_failed", "callback_started", "observed_successor", "arm_without_stop", "reordered", "retry", "count_overflow"} {
		t.Run(name, func(t *testing.T) {
			b, w, record, origin := controlledBackgroundHandoff(t)
			want := "foreign_timer_identity"
			switch name {
			case "owner":
				b.stopped(&Watcher{}, "sample.go", 10, false, origin.Add(3*time.Nanosecond), origin.Add(4*time.Nanosecond), true, true)
			case "path":
				b.stopped(w, "foreign.go", 10, false, origin.Add(3*time.Nanosecond), origin.Add(4*time.Nanosecond), true, true)
			case "generation":
				b.stopped(w, "sample.go", 11, false, origin.Add(3*time.Nanosecond), origin.Add(4*time.Nanosecond), true, true)
			case "stop_failed":
				b.stopped(w, "sample.go", 10, false, origin.Add(3*time.Nanosecond), origin.Add(4*time.Nanosecond), false, true)
				want = "timer_stop_not_successful"
			case "observed_successor":
				b.stopped(w, "sample.go", 10, false, origin.Add(3*time.Nanosecond), origin.Add(4*time.Nanosecond), true, false)
				want = "observed_successor"
			case "arm_without_stop":
				b.armStarted(w, "sample.go", 40, true, origin.Add(3*time.Nanosecond))
				want = "unobserved_timer_handoff"
			case "reordered":
				b.stopped(w, "sample.go", 10, false, origin.Add(time.Nanosecond), origin.Add(4*time.Nanosecond), true, true)
				want = "invalid_timing_order"
			case "retry":
				b.invalidate("retry_or_backoff")
				want = "retry_or_backoff"
			case "count_overflow":
				require.Same(t, b, b.stopped(w, "sample.go", 10, false, origin.Add(3*time.Nanosecond), origin.Add(4*time.Nanosecond), true, true))
				b.handoffs = math.MaxUint64
				b.armStarted(w, "sample.go", 40, true, origin.Add(5*time.Nanosecond))
				want = "handoff_count_overflow"
			case "callback_started":
				require.Same(t, b, b.stopped(w, "sample.go", 10, false, origin.Add(3*time.Nanosecond), origin.Add(4*time.Nanosecond), true, true))
				b.armStarted(w, "sample.go", 40, true, origin.Add(5*time.Nanosecond))
				b.armFinished(w, "sample.go", 40, true, origin.Add(6*time.Nanosecond))
				b.callback(w, "sample.go", 40, true, origin.Add(7*time.Nanosecond))
				b.stopped(w, "sample.go", 40, true, origin.Add(8*time.Nanosecond), origin.Add(9*time.Nanosecond), true, true)
				want = "timer_callback_already_started"
			}
			b.publishUnavailable()
			winner, unavailable := record.CoalescedPublicationEvidence()
			require.Nil(t, winner)
			require.NotNil(t, unavailable)
			require.Equal(t, want, unavailable.Reason)
			require.Equal(t, uint64(10), unavailable.RequestedGeneration)
			require.Nil(t, record.RequestedBackground())
			unavailable.Reason = "caller-mutation"
			_, again := record.CoalescedPublicationEvidence()
			require.Equal(t, want, again.Reason)
		})
	}
}

func TestRequestedBackgroundRefusalIsImmutableAndCannotResurrectEvictedOwner(t *testing.T) {
	recorder := NewPublicationPhaseRecorder(1, 1)
	w := &Watcher{}
	origin := time.Now()
	record := recorder.Begin("", "receipt", PublicationSourceMCPEdit, origin)
	require.True(t, record.bindCanonicalTicket(10, origin))
	b := newCanonicalBackground(record, w, "sample.go", 10, origin)
	_, early := record.CoalescedPublicationEvidence()
	require.Nil(t, early)
	b.invalidate("retry_or_backoff")
	b.publishUnavailable()
	_, accepted := record.CoalescedPublicationEvidence()
	require.NotNil(t, accepted)
	b.invalidate("invalid_timing_order")
	b.publishUnavailable()
	_, same := record.CoalescedPublicationEvidence()
	require.Equal(t, "retry_or_backoff", same.Reason)
	recorder.Begin("", "evict", PublicationSourceMCPEdit, origin)
	winner, absent := record.CoalescedPublicationEvidence()
	require.Nil(t, winner)
	require.Nil(t, absent)
	replacement := recorder.Begin("", "receipt", PublicationSourceMCPEdit, origin)
	require.True(t, replacement.bindCanonicalTicket(11, origin))
	b.publishUnavailable()
	_, replacementFailure := replacement.CoalescedPublicationEvidence()
	require.Nil(t, replacementFailure)
	require.Equal(t, "retry_or_backoff", accepted.Reason)
	require.Nil(t, early)
	runtime.KeepAlive(w)
	runtime.KeepAlive(recorder)
}
