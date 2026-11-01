package store_sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
)

type badCreditSQL struct{ *sql.DB }

func (q badCreditSQL) QueryRowContext(ctx context.Context, _ string, _ ...any) *sql.Row {
	return q.DB.QueryRowContext(ctx, `SELECT nonexistent_credit_column FROM wal_churn`)
}

// Real checkpoint work reaches the owned child's admission exit. Errors from
// the caller, finite SQL scope, custom withdrawal or SQL itself stay distinct.
func TestWALOwnedCreditExpiryPreservesOperationAndSQLCauses(t *testing.T) {
	for _, kind := range []string{"owned_credit", "unowned_credit", "parent_cancel", "parent_deadline", "parent_custom_deadline", "child_custom_deadline", "child_wrapped_deadline", "child_custom_cancel", "sql_deadline", "sql_error"} {
		t.Run(kind, func(t *testing.T) {
			s, db := finalBackfillFixture(t)
			stopStoreBackgroundReaders(s)
			synctest.Test(t, func(t *testing.T) {
				custom := error(errors.New("owned credit custom withdrawal"))
				if kind == "child_wrapped_deadline" {
					custom = fmt.Errorf("custom withdrawal: %w", context.DeadlineExceeded)
				}
				parentBudget := 20 * time.Second
				parentCause := error(context.DeadlineExceeded)
				if kind == "parent_deadline" || kind == "parent_custom_deadline" {
					parentBudget = 40 * time.Millisecond
				}
				if kind == "parent_custom_deadline" {
					parentCause = custom
				}
				parent, stopParent := context.WithTimeoutCause(t.Context(), parentBudget, parentCause)
				defer stopParent()
				operation, cancelOperation := context.WithCancelCause(parent)
				defer cancelOperation(context.Canceled)
				childCause := error(context.DeadlineExceeded)
				if kind == "child_custom_deadline" || kind == "child_wrapped_deadline" {
					childCause = custom
				}
				child, stopChild := context.WithTimeoutCause(operation, 25*time.Millisecond, childCause)
				defer stopChild()
				child, cancelChild := context.WithCancelCause(child)
				defer cancelChild(context.Canceled)
				admission := child
				if kind == "unowned_credit" {
					var stop context.CancelFunc
					admission, stop = context.WithTimeout(operation, 25*time.Millisecond)
					defer stop()
				}
				var sqlScope context.Context
				var sqlExitErr error
				var copies int
				previous := walReclaimCreditPassiveQueryerObserver
				walReclaimCreditPassiveQueryerObserver = func(ctx context.Context, _ *Store, queryDB *sql.DB) (walCheckpointQueryer, func()) {
					sqlScope = ctx
					queryer := walCheckpointQueryer(queryDB)
					if kind == "sql_error" {
						queryer = badCreditSQL{queryDB}
					}
					return reclaimCopyReturnQueryer{walCheckpointQueryer: queryer, pause: func() {
						copies++
						if kind == "parent_cancel" {
							cancelOperation(custom)
						}
						if kind == "child_custom_cancel" {
							cancelChild(custom)
						}
						time.Sleep(50 * time.Millisecond)
					}}, func() { sqlExitErr = ctx.Err() }
				}
				defer func() { walReclaimCreditPassiveQueryerObserver = previous }()

				priorCheckpoint := walCheckpointCallObserver
				walCheckpointCallObserver = func(mode string, _ time.Time, _ time.Duration) {
					if kind == "sql_deadline" && mode == "PASSIVE" {
						time.Sleep(walReclaimLaneBudget + time.Millisecond)
					}
				}
				defer func() { walCheckpointCallObserver = priorCheckpoint }()
				s.writeMu.Lock()
				writer := newWALReclaimWriterCredit(s, time.Now())
				writer.readmitted = true
				writer.resetCtx = child
				writer.spent = 25 * time.Millisecond // already consumed by its original slice
				defer writer.release()
				_, err := writer.passive(admission, operation, db, 10*time.Millisecond, false)
				require.Equal(t, 1, copies, "the observed operation must not renew credit or retry SQL")
				require.NotNil(t, sqlScope)
				require.False(t, writer.held)
				wantSpent := 35 * time.Millisecond
				switch kind {
				case "parent_cancel", "child_custom_cancel":
					wantSpent = 25 * time.Millisecond
				}
				require.Equal(t, wantSpent, writer.spent)
				switch kind {
				case "owned_credit":
					require.NoError(t, operation.Err())
					require.NoError(t, sqlExitErr)
					require.ErrorIs(t, err, errWALReclaimReadersInFlight)
					require.NotErrorIs(t, err, context.DeadlineExceeded)
				case "parent_cancel":
					require.ErrorIs(t, err, context.Canceled)
					require.Same(t, custom, context.Cause(operation))
				case "parent_deadline", "parent_custom_deadline":
					require.ErrorIs(t, err, context.DeadlineExceeded)
					require.ErrorIs(t, operation.Err(), context.DeadlineExceeded)
					if kind == "parent_custom_deadline" {
						require.Same(t, custom, context.Cause(operation))
					}
				case "child_custom_cancel":
					require.NoError(t, operation.Err())
					require.ErrorIs(t, err, context.Canceled)
					require.Same(t, custom, context.Cause(child))
				case "child_custom_deadline", "child_wrapped_deadline":
					require.NoError(t, operation.Err())
					require.ErrorIs(t, err, context.DeadlineExceeded)
					require.Same(t, custom, context.Cause(child))
				case "sql_deadline":
					require.NoError(t, operation.Err())
					require.ErrorIs(t, sqlExitErr, context.DeadlineExceeded)
					require.ErrorIs(t, err, context.DeadlineExceeded)
				case "sql_error":
					require.NoError(t, operation.Err())
					require.NoError(t, sqlExitErr)
					require.ErrorContains(t, err, "nonexistent_credit_column")
				case "unowned_credit":
					require.NoError(t, operation.Err())
					require.NoError(t, sqlExitErr)
					require.ErrorIs(t, err, context.DeadlineExceeded)
				}
				if kind != "owned_credit" {
					require.NotErrorIs(t, err, errWALReclaimReadersInFlight)
				}
				require.True(t, s.writeMu.TryLock(), "returned operation must release its physical writer credit")
				s.writeMu.Unlock()
			})
		})
	}
}
