package indexer

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

func TestModeTransitionFailureDoesNotStarveDurableQueue(t *testing.T) {
	f := newLifecycleFixture(t)
	t.Cleanup(f.close)
	ctx := context.Background()

	type durableTransition struct {
		checkoutID   string
		transitionID string
	}
	transitions := make([]durableTransition, 0, 3)
	for i, name := range []string{"fails-first", "healthy-second", "healthy-third"} {
		root := f.gitRepo(name)
		identity, err := f.lc.recordCheckout(ctx, name, root, TrackSourceConfig, true)
		require.NoError(t, err)
		checkout, found, err := f.catalog.GetCheckout(ctx, identity.checkoutID)
		require.NoError(t, err)
		require.True(t, found)

		transitionID := "transition-" + name
		require.NoError(t, f.catalog.BeginIntentTransition(ctx, store_sqlite.IntentTransition{
			TransitionID:       transitionID,
			CheckoutID:         checkout.CheckoutID,
			Cause:              promotionTransitionCause,
			PriorDesiredMode:   checkout.DesiredMode,
			PriorEffectiveMode: checkout.EffectiveMode,
			RequestedMode:      store_sqlite.CheckoutModeDedicated,
			PriorCheckoutState: checkout.State,
			State:              store_sqlite.IntentTransitionPending,
			CreatedAt:          int64(i + 1),
			LastProgress:       int64(i + 1),
		}))
		transitions = append(transitions, durableTransition{
			checkoutID: checkout.CheckoutID, transitionID: transitionID,
		})
	}

	core, observed := observer.New(zap.WarnLevel)
	f.lc.logger = zap.New(core)
	firstEntered := make(chan struct{})
	releaseFirst := make(chan struct{})
	var attemptsMu sync.Mutex
	var attempts []string
	f.lc.transitionExecute = func(
		ctx context.Context, transition store_sqlite.IntentTransition,
	) modeTransitionOutcome {
		attemptsMu.Lock()
		attempts = append(attempts, transition.TransitionID)
		attemptsMu.Unlock()

		if transition.TransitionID == transitions[0].transitionID {
			close(firstEntered)
			select {
			case <-releaseFirst:
			case <-ctx.Done():
				return modeTransitionOutcome{err: ctx.Err()}
			}
			cause := errors.New("synthetic first-repository failure")
			err := f.catalog.UpdateIntentTransitionProgress(ctx,
				transition.CheckoutID, transition.TransitionID,
				store_sqlite.IntentTransitionPending, cause.Error(), f.clock.Now().Unix())
			if err != nil {
				return modeTransitionOutcome{err: err}
			}
			return modeTransitionOutcome{err: cause}
		}
		if err := f.catalog.CompleteIntentTransition(
			ctx, transition.CheckoutID, transition.TransitionID,
		); err != nil {
			return modeTransitionOutcome{err: err}
		}
		return modeTransitionOutcome{}
	}

	require.NoError(t, f.lc.resumeModeTransitions(ctx))
	select {
	case <-firstEntered:
	case <-time.After(2 * time.Second):
		t.Fatal("oldest transition was not admitted")
	}
	close(releaseFirst)

	require.Eventually(t, func() bool {
		for _, transition := range transitions[1:] {
			_, found, err := f.catalog.GetIntentTransition(ctx, transition.checkoutID)
			if err != nil || found {
				return false
			}
		}
		return true
	}, 5*time.Second, time.Millisecond,
		"healthy durable transitions did not drain after the first failure")

	failed, found, err := f.catalog.GetIntentTransition(ctx, transitions[0].checkoutID)
	require.NoError(t, err)
	require.True(t, found, "failed transition must remain durable and retryable")
	require.Equal(t, "synthetic first-repository failure", failed.LastError)

	attemptsMu.Lock()
	gotAttempts := append([]string(nil), attempts...)
	attemptsMu.Unlock()
	require.Equal(t, []string{
		transitions[0].transitionID,
		transitions[1].transitionID,
		transitions[2].transitionID,
	}, gotAttempts, "failed oldest row must not be retried inside the same drain")
	require.Equal(t, 1, observed.FilterMessage(
		"checkout lifecycle: mode transition failed",
	).Len(), "one asynchronous failure must produce one actionable warning")
}
