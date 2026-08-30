package indexer

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

func TestModeTransitionObserverReportsWorkerOutcomeOutsideLifecycleLock(t *testing.T) {
	tests := []struct {
		name   string
		failed bool
	}{
		{name: "success"},
		{name: "failure", failed: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			lifecycle := &CheckoutLifecycle{
				transitionCtx:     ctx,
				cancelTransitions: cancel,
				transitionRuns:    map[string]*modeTransitionRun{},
				transitionQueue:   make(chan *modeTransitionRun, modeTransitionQueueLimit),
			}
			lifecycle.transitionExecute = func(
				context.Context, store_sqlite.IntentTransition,
			) modeTransitionOutcome {
				if tt.failed {
					return modeTransitionOutcome{err: errors.New("synthetic failure")}
				}
				return modeTransitionOutcome{}
			}
			events := make(chan ModeTransitionEvent, 1)
			lifecycle.SetModeTransitionObserver(func(event ModeTransitionEvent) {
				// Re-entering the setter proves the callback is not invoked under mu.
				lifecycle.SetModeTransitionObserver(nil)
				events <- event
			})

			transition := store_sqlite.IntentTransition{
				TransitionID: "transition-1",
				CheckoutID:   "checkout-1",
				Cause:        promotionTransitionCause,
			}
			run := lifecycle.scheduleModeTransition(transition)
			select {
			case <-run.done:
			case <-time.After(2 * time.Second):
				t.Fatal("transition worker did not finish")
			}
			select {
			case event := <-events:
				assert.Equal(t, transition.TransitionID, event.TransitionID)
				assert.Equal(t, transition.CheckoutID, event.CheckoutID)
				assert.Equal(t, tt.failed, event.Failed)
			case <-time.After(2 * time.Second):
				t.Fatal("transition observer was not notified")
			}

			cancel()
			lifecycle.transitionWG.Wait()
			require.ErrorIs(t, ctx.Err(), context.Canceled)
		})
	}
}
