package indexer

import (
	"runtime"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
)

func TestObservedMutationTimerZeroDelayPublishesBeforeCallback(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		recorder := NewPublicationPhaseRecorder(2, 1)
		w := &Watcher{}
		origin := time.Now()
		record := recorder.Begin("", "zero-delay", PublicationSourceMCPEdit, origin)
		require.True(t, record.bindCanonicalTicket(10, origin))
		background := newCanonicalBackground(record, w, "sample.go", 10, origin)
		time.Sleep(time.Nanosecond)
		background.armStarted(w, "sample.go", 10, false, time.Now())
		release := make(chan struct{})
		type readings struct{ entered, returned time.Time }
		done := make(chan readings)
		armObservedMutationTimer(0, func() {
			entered := time.Now() // Actual callback body entry, before admission.
			background.callback(w, "sample.go", 10, false, entered)
			<-release
			time.Sleep(time.Nanosecond)
			done <- readings{entered, time.Now()}
		}, func(_ *time.Timer) {
			// Run the real zero-delay timer until its callback is durably
			// blocked. Ungated arming therefore enters the actual callback
			// before armFinished; a publication handoff must hold it outside.
			synctest.Wait()
			time.Sleep(time.Nanosecond)
			background.armFinished(w, "sample.go", 10, false, time.Now())
		})
		close(release)
		actual := <-done
		require.False(t, background.invalid, background.reason)
		require.Empty(t, background.reason)
		require.Equal(t, backgroundExecuting, background.stage)
		require.Equal(t, actual.entered, background.entered)
		var prefix int64
		for _, phase := range background.phases {
			prefix += phase
		}
		require.Equal(t, actual.entered.Sub(origin).Nanoseconds(), prefix)
		require.Equal(t, actual.returned.Sub(origin).Nanoseconds(), prefix+actual.returned.Sub(actual.entered).Nanoseconds())
		runtime.KeepAlive(recorder)
		runtime.KeepAlive(w)
	})
}

func TestObservedMutationTimerStopAndResetRetainTimerIdentity(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		calls := make(chan struct{}, 2)
		var timer *time.Timer
		armObservedMutationTimer(time.Hour, func() { calls <- struct{}{} }, func(armed *time.Timer) {
			timer = armed
		})
		require.NotNil(t, timer)
		require.True(t, timer.Stop())
		synctest.Wait()
		require.Empty(t, calls)
		require.False(t, timer.Reset(0))
		<-calls
		synctest.Wait()
		require.False(t, timer.Stop(), "a started callback retains completion ownership")
		require.Empty(t, calls, "reset invoked the original callback exactly once")
	})
}

func TestObservedMutationTimerReleasesCallbackWhenPublicationPanics(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		entered := make(chan struct{})
		require.PanicsWithValue(t, "publication panic", func() {
			armObservedMutationTimer(0, func() { close(entered) }, func(_ *time.Timer) {
				synctest.Wait()
				select {
				case <-entered:
					t.Fatal("callback entered before publication completed")
				default:
				}
				panic("publication panic")
			})
		})
		<-entered // The deferred handoff cannot strand a queued callback.
	})
}
