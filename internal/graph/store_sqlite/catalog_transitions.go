package store_sqlite

import (
	"context"
	"database/sql"
	"fmt"
)

// BeginIntentTransitionRequest is the durable admission record for a mode
// change. When TrackingIntent is present, the reason the checkout is becoming
// dedicated and the transition that will do it are committed together.
type BeginIntentTransitionRequest struct {
	Transition     IntentTransition
	Incarnation    string
	TrackingIntent *TrackingIntent
}

// BeginIntentTransitionWithTrackingIntent records a transition and its
// optional tracking intent in one transaction. No worker may begin before this
// method returns: a crash can therefore lose the worker, but never the request
// the next daemon needs to resume.
func (c *Catalog) BeginIntentTransitionWithTrackingIntent(
	ctx context.Context, req BeginIntentTransitionRequest,
) (IntentTransition, bool, error) {
	if err := req.Transition.validate(); err != nil {
		return IntentTransition{}, false, err
	}
	if err := requireCatalogID("incarnation", req.Incarnation); err != nil {
		return IntentTransition{}, false, err
	}
	if req.TrackingIntent != nil {
		if err := req.TrackingIntent.validate(); err != nil {
			return IntentTransition{}, false, err
		}
		if req.TrackingIntent.CheckoutID != req.Transition.CheckoutID {
			return IntentTransition{}, false, fmt.Errorf(
				"%w: transition and tracking intent name different checkouts",
				ErrCatalogInvalidValue)
		}
	}

	standing := req.Transition
	adopted := false
	err := c.withTx(ctx, func(tx *sql.Tx) error {
		checkout, err := authorizationCheckoutTx(ctx, tx,
			req.Transition.CheckoutID, req.Incarnation)
		if err != nil {
			return err
		}
		existing, found, err := intentTransitionTx(ctx, tx, req.Transition.CheckoutID)
		if err != nil {
			return err
		}
		if found {
			if existing.Cause != req.Transition.Cause ||
				existing.RequestedMode != req.Transition.RequestedMode {
				return fmt.Errorf(
					"%w: checkout %s already has incompatible transition %s",
					ErrCatalogStaleGuard, req.Transition.CheckoutID, existing.TransitionID)
			}
			standing = existing
			adopted = true
		} else {
			if checkout.state != req.Transition.PriorCheckoutState ||
				checkout.desiredMode != req.Transition.PriorDesiredMode ||
				checkout.effectiveMode != req.Transition.PriorEffectiveMode {
				return fmt.Errorf(
					"%w: checkout %s changed before transition admission",
					ErrCatalogStaleGuard, req.Transition.CheckoutID)
			}
			if err := insertIntentTransitionTx(ctx, tx, req.Incarnation, req.Transition); err != nil {
				return err
			}
		}
		if req.TrackingIntent == nil {
			return nil
		}
		intent := *req.TrackingIntent
		_, err = tx.ExecContext(ctx, `
INSERT INTO tracking_intents
  (intent_id, checkout_id, source_kind, source_locator, active, created_at, revoked_at, last_error)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(checkout_id, source_kind, source_locator) DO UPDATE SET
  active     = excluded.active,
  revoked_at = excluded.revoked_at,
  last_error = excluded.last_error`,
			intent.IntentID, intent.CheckoutID, string(intent.SourceKind), intent.SourceLocator,
			catalogBoolInt(intent.Active), intent.CreatedAt, intent.RevokedAt, intent.LastError)
		return err
	})
	if err != nil {
		return IntentTransition{}, false, err
	}
	return standing, adopted, nil
}

// ListIntentTransitions enumerates durable mode changes in creation order.
// It is the restart read: individual transitions are addressable by checkout,
// but a new daemon does not know which checkouts were mid-change until it scans
// this journal.
func (c *Catalog) ListIntentTransitions(ctx context.Context) ([]IntentTransition, error) {
	rows, err := c.store.db.QueryContext(ctx, `
SELECT transition_id, checkout_id, cause, prior_desired_mode, prior_effective_mode,
       requested_mode, prior_checkout_state, source_snapshot_hash, state,
       created_at, last_progress, last_error
  FROM intent_transitions
 ORDER BY created_at, transition_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var transitions []IntentTransition
	for rows.Next() {
		var (
			transition                              IntentTransition
			priorDesired, priorEffective, requested sql.NullString
			priorState, snapshotHash                sql.NullString
			state                                   string
		)
		if err := rows.Scan(
			&transition.TransitionID, &transition.CheckoutID, &transition.Cause,
			&priorDesired, &priorEffective, &requested, &priorState, &snapshotHash,
			&state, &transition.CreatedAt, &transition.LastProgress,
			&transition.LastError); err != nil {
			return nil, err
		}
		transition.PriorDesiredMode = CheckoutMode(priorDesired.String)
		transition.PriorEffectiveMode = CheckoutMode(priorEffective.String)
		transition.RequestedMode = CheckoutMode(requested.String)
		transition.PriorCheckoutState = CheckoutState(priorState.String)
		transition.SourceSnapshotHash = snapshotHash.String
		transition.State = IntentTransitionState(state)
		transitions = append(transitions, transition)
	}
	return transitions, rows.Err()
}
