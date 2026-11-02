package reach

import (
	"context"
	"testing"
	"testing/synctest"
)

// A real admission/release inside a fake-clock bubble must not leave a
// bubble-owned notification channel for the next ordinary graph operation.
func TestTopologyWakeDoesNotEscapeSynctestBubble(t *testing.T) {
	for range 2 {
		synctest.Test(t, func(t *testing.T) {
			finish := beginTopologyWrite(nil, "")
			finish(false, nil)
		})
		finish := beginTopologyWrite(nil, "")
		finish(false, nil)
	}
}

// The holder can outlive a cancelled reader. Once that reader has returned,
// its bubble must not remain the owner of the global notification channel.
func TestTopologyCancelledWaitDoesNotEscapeSynctestBubble(t *testing.T) {
	finish := beginTopologyWrite(nil, "")
	defer finish(false, nil)
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() {
			defer close(done)
			release, admitted := beginTopologyRead(ctx, nil)
			if admitted {
				release()
				t.Error("reader entered while writer held the graph")
			}
		}()
		synctest.Wait()
		cancel()
		<-done
	})
	finish(false, nil)
}

func TestTopologyWakeKeepsOtherWaitersWhenOneCancels(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		finish := beginTopologyWrite(nil, "")
		defer finish(false, nil)
		ctx, cancel := context.WithCancel(context.Background())
		cancelled := make(chan struct{})
		entered := make(chan struct{})
		go func() {
			defer close(cancelled)
			release, admitted := beginTopologyRead(ctx, nil)
			if admitted {
				release()
				t.Error("cancelled reader entered while writer held the graph")
			}
		}()
		go func() {
			release, admitted := beginTopologyRead(context.Background(), nil)
			if !admitted {
				t.Error("live waiter was cancelled by another reader")
				return
			}
			release()
			close(entered)
		}()
		synctest.Wait()
		cancel()
		<-cancelled
		synctest.Wait()
		select {
		case <-entered:
			t.Fatal("live reader entered before writer release")
		default:
		}
		finish(false, nil)
		<-entered
	})
	finish := beginTopologyWrite(nil, "")
	finish(false, nil)
}
