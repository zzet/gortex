package store_sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// UntrackAuthorizationPlan names the durable operation an explicit untrack is
// authorizing. Cleanup plans start a cleanup-journal entry; demotion starts an
// intent transition that is completed only after its replacement route exists.
type UntrackAuthorizationPlan string

const (
	UntrackAuthorizationForget         UntrackAuthorizationPlan = "forget"
	UntrackAuthorizationPrimaryClosure UntrackAuthorizationPlan = "primary_closure"
	UntrackAuthorizationDemote         UntrackAuthorizationPlan = "demote"
)

var untrackAuthorizationPlans = []UntrackAuthorizationPlan{
	UntrackAuthorizationForget,
	UntrackAuthorizationPrimaryClosure,
	UntrackAuthorizationDemote,
}

// AuthorizeUntrackRequest is the complete compare-and-set for an explicit
// untrack. Every identity named here came from the preview. The catalog checks
// them and the active intent set in one write transaction, then records the
// durable work item in that same transaction before intent revocation becomes
// visible.
type AuthorizeUntrackRequest struct {
	Plan UntrackAuthorizationPlan

	CheckoutID  string
	Incarnation string
	FamilyID    string

	OwnedGraphID         string
	PrimaryGraphID       string
	ExpectedPrimaryEpoch int64
	RequiredPrimaryState string

	RevokedAt      int64
	RevocableKinds []IntentSourceKind

	Cleanup    *CleanupEntry
	Transition *IntentTransition
}

// AuthorizeUntrackResult reports the atomic preflight and the durable work item
// that now owns cleanup. Existing carries a journal/transition written by an
// earlier attempt; callers resume it rather than minting competing work.
type AuthorizeUntrackResult struct {
	Revoked    []TrackingIntent
	Blocked    []TrackingIntent
	Cleanup    *CleanupEntry
	Transition *IntentTransition
	Existing   bool
}

// CommitAuthorizedDemotionRequest atomically makes a prepared automatic route
// authoritative. When a dedicated graph has to be retired, Cleanup is inserted
// in the same transaction as the mode flip, so a crash cannot leave an
// automatic checkout with an unjournalled private graph.
type CommitAuthorizedDemotionRequest struct {
	CheckoutID   string
	Incarnation  string
	FamilyID     string
	TransitionID string

	OwnedGraphID         string
	PrimaryGraphID       string
	ExpectedPrimaryEpoch int64
	RequiredPrimaryState string

	State    CheckoutState
	LastSeen int64
	Cleanup  *CleanupEntry
}

// AuthorizeUntrack validates the preview, revokes every revocable active intent
// as one set, and starts the corresponding durable operation in one catalog
// transaction. A stale guard or a blocked intent set writes nothing.
func (c *Catalog) AuthorizeUntrack(ctx context.Context, req AuthorizeUntrackRequest) (AuthorizeUntrackResult, error) {
	if err := validateAuthorizeUntrackRequest(req); err != nil {
		return AuthorizeUntrackResult{}, err
	}
	var out AuthorizeUntrackResult
	err := c.withTx(ctx, func(tx *sql.Tx) error {
		if req.Cleanup != nil {
			entry, found, err := cleanupEntryTx(ctx, tx, req.Cleanup.CleanupID)
			if err != nil {
				return err
			}
			if found {
				if entry.Reason != req.Cleanup.Reason {
					return fmt.Errorf("%w: cleanup %s is %q, not %q", ErrCatalogStaleGuard,
						req.Cleanup.CleanupID, entry.Reason, req.Cleanup.Reason)
				}
				if req.Plan == UntrackAuthorizationPrimaryClosure &&
					entry.PrimaryEpoch != req.ExpectedPrimaryEpoch {
					return fmt.Errorf("%w: primary cleanup %s holds epoch %d, not %d",
						ErrCatalogStaleGuard, req.Cleanup.CleanupID,
						entry.PrimaryEpoch, req.ExpectedPrimaryEpoch)
				}
				out.Cleanup, out.Existing = &entry, true
				return nil
			}
		}

		checkout, err := authorizationCheckoutTx(ctx, tx, req.CheckoutID, req.Incarnation)
		if err != nil {
			return err
		}
		if checkout.familyID != req.FamilyID {
			return fmt.Errorf("%w: checkout %s is in family %s, not %s", ErrCatalogStaleGuard,
				req.CheckoutID, checkout.familyID, req.FamilyID)
		}

		switch req.Plan {
		case UntrackAuthorizationPrimaryClosure:
			if err := verifyPrimaryAuthorizationTx(ctx, tx, req, true); err != nil {
				return err
			}
		case UntrackAuthorizationForget:
			if err := verifyOwnedNonPrimaryTx(ctx, tx, req.CheckoutID, req.FamilyID, req.OwnedGraphID); err != nil {
				return err
			}
		case UntrackAuthorizationDemote:
			if err := verifyOwnedNonPrimaryTx(ctx, tx, req.CheckoutID, req.FamilyID, req.OwnedGraphID); err != nil {
				return err
			}
			if err := verifyPrimaryAuthorizationTx(ctx, tx, req, false); err != nil {
				return err
			}
			standing, found, err := intentTransitionTx(ctx, tx, req.CheckoutID)
			if err != nil {
				return err
			}
			if found {
				if standing.Cause != req.Transition.Cause ||
					standing.RequestedMode != req.Transition.RequestedMode {
					return fmt.Errorf("%w: checkout %s holds transition %s", ErrCatalogIntentTransitionActive,
						req.CheckoutID, standing.TransitionID)
				}
				active, err := activeIntentCountTx(ctx, tx, req.CheckoutID)
				if err != nil {
					return err
				}
				if active != 0 {
					return fmt.Errorf("%w: checkout %s gained %d active intent(s) after demotion authorization",
						ErrCatalogStaleGuard, req.CheckoutID, active)
				}
				out.Transition, out.Existing = &standing, true
				return nil
			}
			if checkout.state != CheckoutStateReady ||
				checkout.desiredMode != CheckoutModeDedicated ||
				checkout.effectiveMode != CheckoutModeDedicated {
				return fmt.Errorf("%w: checkout %s is not a ready dedicated checkout",
					ErrCatalogStaleGuard, req.CheckoutID)
			}
		}

		candidates, blocked, err := trackingIntentPreflightTx(ctx, tx, req.CheckoutID, req.RevocableKinds)
		if err != nil {
			return err
		}
		if len(blocked) != 0 {
			out.Blocked = blocked
			return nil
		}
		if err := revokeTrackingIntentsTx(ctx, tx, req.CheckoutID, req.RevokedAt, candidates); err != nil {
			return err
		}
		out.Revoked = candidates

		if req.Cleanup != nil {
			if err := insertCleanupEntryTx(ctx, tx, *req.Cleanup); err != nil {
				return err
			}
			entry := *req.Cleanup
			out.Cleanup = &entry
			return nil
		}
		if err := insertIntentTransitionTx(ctx, tx, req.Incarnation, *req.Transition); err != nil {
			return err
		}
		transition := *req.Transition
		out.Transition = &transition
		return nil
	})
	if err != nil {
		return AuthorizeUntrackResult{}, err
	}
	return out, nil
}

// CommitAuthorizedDemotion performs the publication half of a demotion. The
// family epoch and primary graph are checked again after the replacement layers
// were built; a primary move during that build therefore leaves the checkout
// dedicated and the authorization transition available for a retry. A
// successful publication deliberately leaves that transition standing until
// the caller persists external configuration cleanup and completes it.
func (c *Catalog) CommitAuthorizedDemotion(ctx context.Context, req CommitAuthorizedDemotionRequest) error {
	if err := validateCommitAuthorizedDemotionRequest(req); err != nil {
		return err
	}
	return c.withTx(ctx, func(tx *sql.Tx) error {
		checkout, err := authorizationCheckoutTx(ctx, tx, req.CheckoutID, req.Incarnation)
		if err != nil {
			return err
		}
		if checkout.familyID != req.FamilyID || checkout.activeTransition != req.TransitionID {
			return fmt.Errorf("%w: checkout %s demotion authorization moved", ErrCatalogStaleGuard, req.CheckoutID)
		}
		standing, found, err := intentTransitionTx(ctx, tx, req.CheckoutID)
		if err != nil {
			return err
		}
		if !found || standing.TransitionID != req.TransitionID ||
			standing.RequestedMode != CheckoutModeAutomatic {
			return fmt.Errorf("%w: transition %s on checkout %s", ErrCatalogStaleGuard,
				req.TransitionID, req.CheckoutID)
		}
		if err := verifyOwnedNonPrimaryTx(ctx, tx, req.CheckoutID, req.FamilyID, req.OwnedGraphID); err != nil {
			return err
		}
		authorization := AuthorizeUntrackRequest{
			CheckoutID:           req.CheckoutID,
			FamilyID:             req.FamilyID,
			PrimaryGraphID:       req.PrimaryGraphID,
			ExpectedPrimaryEpoch: req.ExpectedPrimaryEpoch,
			RequiredPrimaryState: req.RequiredPrimaryState,
		}
		if err := verifyPrimaryAuthorizationTx(ctx, tx, authorization, false); err != nil {
			return err
		}
		active, err := activeIntentCountTx(ctx, tx, req.CheckoutID)
		if err != nil {
			return err
		}
		if active != 0 {
			return fmt.Errorf("%w: checkout %s gained %d active intent(s) during demotion",
				ErrCatalogStaleGuard, req.CheckoutID, active)
		}
		if req.Cleanup != nil {
			entry, exists, err := cleanupEntryTx(ctx, tx, req.Cleanup.CleanupID)
			if err != nil {
				return err
			}
			if exists && entry.Reason != req.Cleanup.Reason {
				return fmt.Errorf("%w: cleanup %s is %q, not %q", ErrCatalogStaleGuard,
					req.Cleanup.CleanupID, entry.Reason, req.Cleanup.Reason)
			}
			if !exists {
				if err := insertCleanupEntryTx(ctx, tx, *req.Cleanup); err != nil {
					return err
				}
			}
		}

		result, err := tx.ExecContext(ctx, `
UPDATE checkouts
   SET state = ?, desired_mode = ?, effective_mode = ?, last_seen = ?, last_error = ''
 WHERE checkout_id = ? AND incarnation = ? AND active_intent_transition_id = ?`,
			string(req.State), string(CheckoutModeAutomatic), string(CheckoutModeAutomatic), req.LastSeen,
			req.CheckoutID, req.Incarnation, req.TransitionID)
		if err != nil {
			return err
		}
		changed, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if changed != 1 {
			return fmt.Errorf("%w: checkout %s demotion transition %s", ErrCatalogStaleGuard,
				req.CheckoutID, req.TransitionID)
		}
		return nil
	})
}

type authorizationCheckout struct {
	familyID         string
	state            CheckoutState
	desiredMode      CheckoutMode
	effectiveMode    CheckoutMode
	activeTransition string
}

func validateAuthorizeUntrackRequest(req AuthorizeUntrackRequest) error {
	if err := requireCatalogValue("plan", req.Plan, untrackAuthorizationPlans); err != nil {
		return err
	}
	for name, value := range map[string]string{
		"checkout_id": req.CheckoutID,
		"incarnation": req.Incarnation,
		"family_id":   req.FamilyID,
	} {
		if err := requireCatalogID(name, value); err != nil {
			return err
		}
	}
	for _, kind := range req.RevocableKinds {
		if err := requireCatalogValue("source_kind", kind, intentSourceKinds); err != nil {
			return err
		}
	}
	switch req.Plan {
	case UntrackAuthorizationForget:
		if req.Cleanup == nil || req.Transition != nil {
			return fmt.Errorf("%w: forget authorization needs cleanup and no transition", ErrCatalogInvalidValue)
		}
	case UntrackAuthorizationPrimaryClosure:
		if req.Cleanup == nil || req.Transition != nil || req.PrimaryGraphID == "" ||
			req.OwnedGraphID != req.PrimaryGraphID || req.Cleanup.PrimaryEpoch != req.ExpectedPrimaryEpoch {
			return fmt.Errorf("%w: primary closure authorization needs its primary graph, epoch and cleanup", ErrCatalogInvalidValue)
		}
	case UntrackAuthorizationDemote:
		if req.Cleanup != nil || req.Transition == nil || req.PrimaryGraphID == "" ||
			req.RequiredPrimaryState == "" {
			return fmt.Errorf("%w: demotion authorization needs a ready primary and transition", ErrCatalogInvalidValue)
		}
		if err := req.Transition.validate(); err != nil {
			return err
		}
		if req.Transition.CheckoutID != req.CheckoutID ||
			req.Transition.RequestedMode != CheckoutModeAutomatic {
			return fmt.Errorf("%w: demotion transition does not match checkout", ErrCatalogInvalidValue)
		}
	}
	if req.Cleanup != nil {
		if err := req.Cleanup.validate(); err != nil {
			return err
		}
		if req.Cleanup.Phase != CleanupPhasePending {
			return fmt.Errorf("%w: authorized cleanup must start pending", ErrCatalogInvalidValue)
		}
	}
	return nil
}

func validateCommitAuthorizedDemotionRequest(req CommitAuthorizedDemotionRequest) error {
	for name, value := range map[string]string{
		"checkout_id":      req.CheckoutID,
		"incarnation":      req.Incarnation,
		"family_id":        req.FamilyID,
		"transition_id":    req.TransitionID,
		"primary_graph_id": req.PrimaryGraphID,
	} {
		if err := requireCatalogID(name, value); err != nil {
			return err
		}
	}
	if req.RequiredPrimaryState == "" {
		return fmt.Errorf("%w: required primary state is empty", ErrCatalogInvalidValue)
	}
	if err := requireCatalogValue("state", req.State, checkoutStates); err != nil {
		return err
	}
	if req.Cleanup != nil {
		if req.OwnedGraphID == "" {
			return fmt.Errorf("%w: graph cleanup has no owned graph", ErrCatalogInvalidValue)
		}
		if err := req.Cleanup.validate(); err != nil {
			return err
		}
		if req.Cleanup.Phase != CleanupPhasePending {
			return fmt.Errorf("%w: demotion cleanup must start pending", ErrCatalogInvalidValue)
		}
	}
	return nil
}

func authorizationCheckoutTx(ctx context.Context, tx *sql.Tx, checkoutID, incarnation string) (authorizationCheckout, error) {
	var out authorizationCheckout
	var state, desiredMode, effectiveMode string
	var active sql.NullString
	err := tx.QueryRowContext(ctx, `
SELECT family_id, state, desired_mode, effective_mode, active_intent_transition_id
  FROM checkouts WHERE checkout_id = ? AND incarnation = ?`, checkoutID, incarnation).Scan(
		&out.familyID, &state, &desiredMode, &effectiveMode, &active)
	if errors.Is(err, sql.ErrNoRows) {
		return authorizationCheckout{}, fmt.Errorf("%w: checkout %s incarnation %s", ErrCatalogStaleGuard,
			checkoutID, incarnation)
	}
	if err != nil {
		return authorizationCheckout{}, err
	}
	out.state = CheckoutState(state)
	out.desiredMode = CheckoutMode(desiredMode)
	out.effectiveMode = CheckoutMode(effectiveMode)
	out.activeTransition = active.String
	return out, nil
}

func verifyOwnedNonPrimaryTx(ctx context.Context, tx *sql.Tx, checkoutID, familyID, expectedGraphID string) error {
	var graphID string
	var primary int
	err := tx.QueryRowContext(ctx, `
SELECT graph_id, is_primary_base
  FROM dedicated_graphs WHERE owner_checkout_id = ? AND family_id = ?
 ORDER BY graph_id LIMIT 1`, checkoutID, familyID).Scan(&graphID, &primary)
	if errors.Is(err, sql.ErrNoRows) {
		if expectedGraphID == "" {
			return nil
		}
		return fmt.Errorf("%w: checkout %s no longer owns graph %s", ErrCatalogStaleGuard,
			checkoutID, expectedGraphID)
	}
	if err != nil {
		return err
	}
	if graphID != expectedGraphID || primary != 0 {
		return fmt.Errorf("%w: checkout %s graph ownership moved", ErrCatalogStaleGuard, checkoutID)
	}
	return nil
}

func verifyPrimaryAuthorizationTx(ctx context.Context, tx *sql.Tx, req AuthorizeUntrackRequest, ownerMustMatch bool) error {
	var ownerID, state string
	var isPrimary int
	var epoch int64
	err := tx.QueryRowContext(ctx, `
SELECT graph.owner_checkout_id, graph.state, graph.is_primary_base, family.primary_epoch
  FROM dedicated_graphs AS graph
  JOIN repository_families AS family ON family.family_id = graph.family_id
 WHERE graph.graph_id = ? AND graph.family_id = ?`, req.PrimaryGraphID, req.FamilyID).Scan(
		&ownerID, &state, &isPrimary, &epoch)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: primary graph %s in family %s", ErrCatalogStaleGuard,
			req.PrimaryGraphID, req.FamilyID)
	}
	if err != nil {
		return err
	}
	if isPrimary != 1 || epoch != req.ExpectedPrimaryEpoch {
		return fmt.Errorf("%w: family %s primary epoch or graph moved", ErrCatalogStaleGuard, req.FamilyID)
	}
	if ownerMustMatch {
		if ownerID != req.CheckoutID {
			return fmt.Errorf("%w: graph %s is owned by %s, not %s", ErrCatalogStaleGuard,
				req.PrimaryGraphID, ownerID, req.CheckoutID)
		}
		return nil
	}
	if ownerID == req.CheckoutID || state != req.RequiredPrimaryState {
		return fmt.Errorf("%w: graph %s cannot serve checkout %s", ErrCatalogStaleGuard,
			req.PrimaryGraphID, req.CheckoutID)
	}
	return nil
}

func trackingIntentPreflightTx(ctx context.Context, tx *sql.Tx, checkoutID string, revocable []IntentSourceKind) (candidates, blocked []TrackingIntent, err error) {
	allowed := make(map[IntentSourceKind]struct{}, len(revocable))
	for _, kind := range revocable {
		allowed[kind] = struct{}{}
	}
	rows, err := tx.QueryContext(ctx, `
SELECT intent_id, source_kind, source_locator, active, created_at, revoked_at, last_error
  FROM tracking_intents WHERE checkout_id = ? ORDER BY source_kind, source_locator`, checkoutID)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	for rows.Next() {
		intent := TrackingIntent{CheckoutID: checkoutID}
		var sourceKind string
		var active int
		if err := rows.Scan(&intent.IntentID, &sourceKind, &intent.SourceLocator, &active,
			&intent.CreatedAt, &intent.RevokedAt, &intent.LastError); err != nil {
			return nil, nil, err
		}
		intent.SourceKind, intent.Active = IntentSourceKind(sourceKind), active != 0
		if !intent.Active {
			continue
		}
		if _, ok := allowed[intent.SourceKind]; !ok {
			blocked = append(blocked, intent)
		} else {
			candidates = append(candidates, intent)
		}
	}
	return candidates, blocked, rows.Err()
}

func revokeTrackingIntentsTx(ctx context.Context, tx *sql.Tx, checkoutID string, revokedAt int64, candidates []TrackingIntent) error {
	if len(candidates) == 0 {
		return nil
	}
	result, err := tx.ExecContext(ctx, `
UPDATE tracking_intents SET active = 0, revoked_at = ?
 WHERE checkout_id = ? AND active = 1`, revokedAt, checkoutID)
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed != int64(len(candidates)) {
		return ErrCatalogStaleGuard
	}
	for i := range candidates {
		candidates[i].Active = false
		candidates[i].RevokedAt = revokedAt
	}
	return nil
}

func activeIntentCountTx(ctx context.Context, tx *sql.Tx, checkoutID string) (int, error) {
	var count int
	err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM tracking_intents WHERE checkout_id = ? AND active = 1`, checkoutID).Scan(&count)
	return count, err
}

func insertIntentTransitionTx(ctx context.Context, tx *sql.Tx, incarnation string, transition IntentTransition) error {
	if _, err := tx.ExecContext(ctx, `
INSERT INTO intent_transitions
  (transition_id, checkout_id, cause, prior_desired_mode, prior_effective_mode,
   requested_mode, prior_checkout_state, source_snapshot_hash, state,
   created_at, last_progress, last_error)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		transition.TransitionID, transition.CheckoutID, transition.Cause,
		catalogNullString(string(transition.PriorDesiredMode)),
		catalogNullString(string(transition.PriorEffectiveMode)),
		catalogNullString(string(transition.RequestedMode)),
		catalogNullString(string(transition.PriorCheckoutState)),
		catalogNullString(transition.SourceSnapshotHash), string(transition.State),
		transition.CreatedAt, transition.LastProgress, transition.LastError); err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, `
UPDATE checkouts SET active_intent_transition_id = ?
 WHERE checkout_id = ? AND incarnation = ? AND active_intent_transition_id IS NULL`,
		transition.TransitionID, transition.CheckoutID, incarnation)
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed != 1 {
		return fmt.Errorf("%w: checkout %s transition slot moved", ErrCatalogStaleGuard, transition.CheckoutID)
	}
	return nil
}

func intentTransitionTx(ctx context.Context, tx *sql.Tx, checkoutID string) (IntentTransition, bool, error) {
	transition := IntentTransition{CheckoutID: checkoutID}
	var priorDesired, priorEffective, requested, priorState, sourceHash sql.NullString
	var state string
	err := tx.QueryRowContext(ctx, `
SELECT transition_id, cause, prior_desired_mode, prior_effective_mode, requested_mode,
       prior_checkout_state, source_snapshot_hash, state, created_at, last_progress, last_error
  FROM intent_transitions WHERE checkout_id = ?`, checkoutID).Scan(
		&transition.TransitionID, &transition.Cause, &priorDesired, &priorEffective,
		&requested, &priorState, &sourceHash, &state, &transition.CreatedAt,
		&transition.LastProgress, &transition.LastError)
	if errors.Is(err, sql.ErrNoRows) {
		return IntentTransition{}, false, nil
	}
	if err != nil {
		return IntentTransition{}, false, err
	}
	transition.PriorDesiredMode = CheckoutMode(priorDesired.String)
	transition.PriorEffectiveMode = CheckoutMode(priorEffective.String)
	transition.RequestedMode = CheckoutMode(requested.String)
	transition.PriorCheckoutState = CheckoutState(priorState.String)
	transition.SourceSnapshotHash = sourceHash.String
	transition.State = IntentTransitionState(state)
	return transition, true, nil
}

func cleanupEntryTx(ctx context.Context, tx *sql.Tx, cleanupID string) (CleanupEntry, bool, error) {
	entry := CleanupEntry{CleanupID: cleanupID}
	var phase string
	err := tx.QueryRowContext(ctx, `
SELECT opaque_target_ids, reason, phase, grace_deadline, primary_epoch, last_progress, last_error
  FROM cleanup_journal WHERE cleanup_id = ?`, cleanupID).Scan(
		&entry.OpaqueTargetIDs, &entry.Reason, &phase, &entry.GraceDeadline,
		&entry.PrimaryEpoch, &entry.LastProgress, &entry.LastError)
	if errors.Is(err, sql.ErrNoRows) {
		return CleanupEntry{}, false, nil
	}
	if err != nil {
		return CleanupEntry{}, false, err
	}
	entry.Phase = CleanupPhase(phase)
	return entry, true, nil
}

func insertCleanupEntryTx(ctx context.Context, tx *sql.Tx, entry CleanupEntry) error {
	_, err := tx.ExecContext(ctx, `
INSERT INTO cleanup_journal
  (cleanup_id, opaque_target_ids, reason, phase, grace_deadline, primary_epoch, last_progress, last_error)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		entry.CleanupID, entry.OpaqueTargetIDs, entry.Reason, string(entry.Phase),
		entry.GraceDeadline, entry.PrimaryEpoch, entry.LastProgress, entry.LastError)
	return err
}
