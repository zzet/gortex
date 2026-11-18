package store_sqlite

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type checkpointDispatchSpy struct {
	db    *sql.DB
	calls int
}

func (s *checkpointDispatchSpy) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	s.calls++
	return s.db.QueryRowContext(ctx, query, args...)
}

func TestCheckpointWALOnceRefusesCanceledContextBeforeDispatch(t *testing.T) {
	// This raw in-memory connection owns no Store/background worker, so the
	// package-private observers see only this synchronous checkpoint call.
	db, err := sql.Open("sqlite", ":memory:")
	require.NoError(t, err)
	defer func() { require.NoError(t, db.Close()) }()
	oldCalls, oldResults := walCheckpointCallObserver, walCheckpointResultObserver
	defer func() {
		walCheckpointCallObserver, walCheckpointResultObserver = oldCalls, oldResults
	}()
	cause := errors.New("checkpoint attempt withdrew")
	for _, kind := range []string{"canceled", "deadline", "custom_cause"} {
		t.Run(kind, func(t *testing.T) {
			var ctx context.Context
			var cancel func()
			switch kind {
			case "deadline":
				ctx, cancel = context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
			case "custom_cause":
				var cancelCause context.CancelCauseFunc
				ctx, cancelCause = context.WithCancelCause(t.Context())
				cancelCause(cause)
				cancel = func() { cancelCause(context.Canceled) }
			default:
				ctx, cancel = context.WithCancel(t.Context())
				cancel()
			}
			defer cancel()
			require.Error(t, ctx.Err())
			spy := &checkpointDispatchSpy{db: db}
			calls, results := 0, 0
			walCheckpointCallObserver = func(string, time.Time, time.Duration) { calls++ }
			walCheckpointResultObserver = func(string, time.Time, time.Duration, walCheckpointResult, error) { results++ }
			result, checkpointErr := checkpointWALOnceOn(ctx, spy, "PASSIVE")
			require.ErrorIs(t, checkpointErr, ctx.Err())
			require.Equal(t, walCheckpointResult{}, result)
			require.Zero(t, spy.calls, "already canceled attempt crossed the query boundary")
			require.Zero(t, calls, "rejected attempt was reported as a dispatched checkpoint")
			require.Zero(t, results, "no SQL Scan happened, so no result tuple should be reported")
		})
	}
}

func TestCheckpointWALOnceLiveContextDispatchesAndObserves(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	require.NoError(t, err)
	defer func() { require.NoError(t, db.Close()) }()
	spy := &checkpointDispatchSpy{db: db}
	oldCalls, oldResults := walCheckpointCallObserver, walCheckpointResultObserver
	defer func() {
		walCheckpointCallObserver, walCheckpointResultObserver = oldCalls, oldResults
	}()
	calls, results := 0, 0
	var observedResult walCheckpointResult
	var observedErr error
	walCheckpointCallObserver = func(mode string, started time.Time, took time.Duration) {
		calls++
		require.Equal(t, "PASSIVE", mode)
		require.False(t, started.IsZero())
		require.GreaterOrEqual(t, took, time.Duration(0))
	}
	walCheckpointResultObserver = func(mode string, started time.Time, took time.Duration, result walCheckpointResult, scanErr error) {
		results++
		require.Equal(t, "PASSIVE", mode)
		require.False(t, started.IsZero())
		require.GreaterOrEqual(t, took, time.Duration(0))
		observedResult, observedErr = result, scanErr
	}
	result, checkpointErr := checkpointWALOnceOn(t.Context(), spy, "PASSIVE")
	require.NoError(t, checkpointErr)
	require.NoError(t, observedErr)
	require.False(t, result.incomplete())
	require.Equal(t, result, observedResult)
	require.Equal(t, 1, spy.calls, "live control must execute the actual PRAGMA query")
	require.Equal(t, 1, calls)
	require.Equal(t, 1, results)
}
