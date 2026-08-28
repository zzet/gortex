package reconcile

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/zzet/gortex/internal/gitstate"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

// Saga errors.
var (
	// ErrSagaTarget reports a journal entry this build cannot execute: an
	// unknown saga kind or phase, or a payload that will not decode. The
	// entry is left alone rather than guessed at.
	ErrSagaTarget = errors.New("reconcile: unusable cleanup journal entry")
	// ErrPostcondition reports that a saga finished its phases but the rows
	// it was supposed to remove are still there. It is a bug signal, not a
	// retry signal: the journal entry stays so the state can be inspected.
	ErrPostcondition = errors.New("reconcile: cleanup postcondition not met")
)

// sagaKind is which teardown a journal entry describes.
type sagaKind string

const (
	// sagaPurgeLayers drops one incarnation's built layers and nothing else.
	// It is the only cleanup that keeps its identity: the checkout is still
	// tracked, it just is not being served.
	sagaPurgeLayers sagaKind = "purge_checkout_layers"
	// sagaForgetCheckout removes one checkout and everything that references it.
	sagaForgetCheckout sagaKind = "forget_checkout"
	// sagaRetirePrimaryClosure retires a family's primary graph together with
	// everything that could only be served because of it.
	sagaRetirePrimaryClosure sagaKind = "retire_primary_closure"
	// sagaForgetFamily removes what is left of a family, including its row.
	sagaForgetFamily sagaKind = "forget_family"
	// sagaRetireGraph gives up one dedicated graph and leaves the checkout
	// that owned it in place. It is the demotion half of the primary closure:
	// the same two phases, without the rows that carry the identity.
	sagaRetireGraph sagaKind = "retire_dedicated_graph"
)

// sagaPhase is one durable step. The value is written to the journal before
// its work runs, so a resume knows exactly which step was in flight.
type sagaPhase string

const (
	phaseWithdrawRoute      sagaPhase = "withdraw_route"
	phasePurgeLayers        sagaPhase = "purge_layers"
	phaseDeleteRefViews     sagaPhase = "delete_ref_views"
	phaseReleaseGraph       sagaPhase = "release_graph"
	phaseDeleteCheckoutRow  sagaPhase = "delete_checkout_row"
	phaseVerifyCheckoutGone sagaPhase = "verify_checkout_gone"

	phaseForgetDependents   sagaPhase = "forget_dependents"
	phaseForgetPrimaryOwner sagaPhase = "forget_primary_owner"
	phaseVerifyClosureGone  sagaPhase = "verify_closure_gone"

	phaseForgetFamilyCheckouts sagaPhase = "forget_family_checkouts"
	phaseDeleteFamilyRow       sagaPhase = "delete_family_row"
)

// sagaPhases is the ordered plan of each saga.
//
// Order is the whole design. The layers go first, because purging them is what
// stops the builder that writes this checkout's route — withdrawing while it is
// still running deletes a row the last cycle installs again. Then the route,
// then the rows describing the layers, and the checkout row last because
// everything else references it. The plans are fixed lists rather than computed
// ones so a resume from an old journal entry walks exactly the sequence that
// entry was written against.
var sagaPhases = map[sagaKind][]sagaPhase{
	sagaPurgeLayers: {phasePurgeLayers},
	sagaForgetCheckout: {
		phasePurgeLayers,
		phaseWithdrawRoute,
		phaseDeleteRefViews,
		phaseReleaseGraph,
		phaseDeleteCheckoutRow,
		phaseVerifyCheckoutGone,
	},
	sagaRetirePrimaryClosure: {
		phaseForgetDependents,
		phaseDeleteRefViews,
		phaseForgetPrimaryOwner,
		phaseVerifyClosureGone,
	},
	sagaForgetFamily: {
		phaseForgetFamilyCheckouts,
		phaseDeleteFamilyRow,
	},
	sagaRetireGraph: {
		phaseDeleteRefViews,
		phaseReleaseGraph,
	},
}

// sagaTarget is what a journal entry carries in its opaque payload: which
// teardown, how far it got, and the ids it operates on.
//
// The ids are copied in rather than looked up, because the rows they name are
// exactly the rows the saga is deleting. By the later phases there is nothing
// left to read them back from.
type sagaTarget struct {
	Kind         sagaKind  `json:"kind"`
	Phase        sagaPhase `json:"phase"`
	CheckoutID   string    `json:"checkout_id,omitempty"`
	Incarnation  string    `json:"incarnation,omitempty"`
	FamilyID     string    `json:"family_id,omitempty"`
	GraphID      string    `json:"graph_id,omitempty"`
	RepoPrefix   string    `json:"repo_prefix,omitempty"`
	RootPath     string    `json:"root_path,omitempty"`
	PrimaryEpoch int64     `json:"primary_epoch,omitempty"`
}

// cleanupID is the journal key. It is derived from the target rather than
// generated, so re-entering a teardown finds the entry the interrupted attempt
// left behind instead of starting a second one beside it.
func (t sagaTarget) cleanupID() string {
	switch t.Kind {
	case sagaPurgeLayers:
		return "purge-layers:" + t.CheckoutID + ":" + t.Incarnation
	case sagaForgetCheckout:
		return "forget-checkout:" + t.CheckoutID + ":" + t.Incarnation
	case sagaRetirePrimaryClosure:
		return "retire-primary-closure:" + t.GraphID
	case sagaForgetFamily:
		return "forget-family:" + t.FamilyID
	case sagaRetireGraph:
		return "retire-graph:" + t.GraphID
	}
	return string(t.Kind)
}

// purgeCleanupID is the journal key of the layer purge for this target's
// checkout incarnation, whatever saga the target itself belongs to.
func (t sagaTarget) purgeCleanupID() string {
	return sagaTarget{Kind: sagaPurgeLayers, CheckoutID: t.CheckoutID, Incarnation: t.Incarnation}.cleanupID()
}

// ForgetCheckout removes one checkout and everything that references it.
//
// The incarnation is a guard, not a label: a caller holding the incarnation of
// a path that has since been removed and recreated is asking to delete the
// wrong thing, and is refused. A checkout row that is already gone is not an
// error — the saga runs anyway and cleans up whatever the interrupted attempt
// left behind.
func (r *Reconciler) ForgetCheckout(ctx context.Context, checkoutID, incarnation string) error {
	if checkoutID == "" || incarnation == "" {
		return fmt.Errorf("%w: forget_checkout needs a checkout id and an incarnation", ErrSagaTarget)
	}
	target := sagaTarget{Kind: sagaForgetCheckout, CheckoutID: checkoutID, Incarnation: incarnation}

	existing, ok, err := r.catalog.GetCheckout(ctx, checkoutID)
	if err != nil {
		return err
	}
	if ok {
		if existing.Incarnation != incarnation {
			return fmt.Errorf("%w: checkout %s is at incarnation %s, not %s",
				store_sqlite.ErrCatalogStaleGuard, checkoutID, existing.Incarnation, incarnation)
		}
		target.FamilyID = existing.FamilyID
		owned, err := r.ownedGraph(ctx, existing.FamilyID, checkoutID)
		if err != nil {
			return err
		}
		if owned != nil {
			target.GraphID = owned.GraphID
		}
	}
	return r.enterSaga(ctx, target)
}

// RetirePrimaryClosure retires a family's primary dedicated graph together
// with everything that only existed because of it.
//
// primaryEpoch is the compare-and-set token on the family row. Retiring a
// closure is the most destructive thing in the package, and the epoch is what
// proves the caller is looking at the primary that is current rather than one
// that was replaced while it was deciding.
//
// Independent dedicated graphs in the same family, and the checkouts that own
// them, are untouched: they can be served without the primary. When the
// retirement leaves the family with no dedicated graph at all, the closure
// continues into ForgetFamily.
func (r *Reconciler) RetirePrimaryClosure(ctx context.Context, graphID string, primaryEpoch int64) error {
	if graphID == "" {
		return fmt.Errorf("%w: retire_primary_closure needs a graph id", ErrSagaTarget)
	}
	target := sagaTarget{Kind: sagaRetirePrimaryClosure, GraphID: graphID, PrimaryEpoch: primaryEpoch}

	graph, ok, err := r.catalog.GetDedicatedGraph(ctx, graphID)
	if err != nil {
		return err
	}
	if !ok {
		// Nothing to retire. An entry left over from an interrupted attempt
		// still has to finish, so resume it; otherwise this is a no-op.
		resumed, found, err := r.loadSagaTarget(ctx, target.cleanupID())
		if err != nil || !found {
			return err
		}
		return r.runSaga(ctx, resumed)
	}

	family, ok, err := r.catalog.GetRepositoryFamily(ctx, graph.FamilyID)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("%w: family %s", store_sqlite.ErrCatalogNotFound, graph.FamilyID)
	}
	if family.PrimaryEpoch != primaryEpoch {
		return fmt.Errorf("%w: family %s is at primary epoch %d, not %d",
			store_sqlite.ErrCatalogStaleGuard, family.FamilyID, family.PrimaryEpoch, primaryEpoch)
	}
	target.FamilyID = graph.FamilyID
	target.CheckoutID = graph.OwnerCheckoutID
	return r.enterSaga(ctx, target)
}

// ForgetFamily removes what is left of a family, its own row included.
func (r *Reconciler) ForgetFamily(ctx context.Context, familyID string) error {
	if familyID == "" {
		return fmt.Errorf("%w: forget_family needs a family id", ErrSagaTarget)
	}
	return r.enterSaga(ctx, sagaTarget{Kind: sagaForgetFamily, FamilyID: familyID})
}

// Resume re-runs every teardown the journal still shows as unfinished.
//
// It is the restart path: nothing about a saga lives in memory, so a process
// that died mid-teardown leaves its progress in the journal and the next one
// picks it up from there. Entries are independent, so one that keeps failing
// does not hold up the rest — every error is collected and reported together.
func (r *Reconciler) Resume(ctx context.Context) error {
	entries, err := r.catalog.ListCleanupEntries(ctx)
	if err != nil {
		return err
	}
	var errs []error
	for _, entry := range entries {
		if entry.Phase == store_sqlite.CleanupPhaseDone {
			continue
		}
		// A nested saga may have finished (and deleted) this entry after the
		// listing was taken.
		if _, present, err := r.catalog.GetCleanupEntry(ctx, entry.CleanupID); err != nil {
			errs = append(errs, err)
			continue
		} else if !present {
			continue
		}
		target, err := decodeSagaTarget(entry)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if err := r.runSaga(ctx, target); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// enterSaga starts a teardown, or picks up where an interrupted one stopped.
func (r *Reconciler) enterSaga(ctx context.Context, target sagaTarget) error {
	resumed, found, err := r.loadSagaTarget(ctx, target.cleanupID())
	if err != nil {
		return err
	}
	if found {
		// The journal is the authority on both progress and identity. In
		// particular, never replace its incarnation with one read from a row
		// that may have been deleted and re-created under the same stable IDs.
		target.Phase = resumed.Phase
		if resumed.FamilyID != "" {
			target.FamilyID = resumed.FamilyID
		}
		if resumed.GraphID != "" {
			target.GraphID = resumed.GraphID
		}
		if resumed.CheckoutID != "" {
			target.CheckoutID = resumed.CheckoutID
		}
		target.Incarnation = resumed.Incarnation
		target.RepoPrefix = resumed.RepoPrefix
		target.RootPath = resumed.RootPath
		target.PrimaryEpoch = resumed.PrimaryEpoch
	}
	return r.runSaga(ctx, target)
}

// repairLegacyRetireGraphTarget upgrades cleanup rows written before graph
// retirement carried the checkout incarnation. Repair is allowed only while
// the graph binding and its live owner row agree; ambiguity remains durable and
// retryable instead of authorizing a prefix purge with a guessed identity.
func (r *Reconciler) repairLegacyRetireGraphTarget(
	ctx context.Context, target sagaTarget,
) (sagaTarget, error) {
	graph, present, err := r.catalog.GetDedicatedGraph(ctx, target.GraphID)
	if err != nil {
		return target, err
	}
	if !present || graph.OwnerCheckoutID == "" {
		return target, fmt.Errorf(
			"%w: legacy graph cleanup %s has no verifiable owner binding",
			ErrSagaTarget, target.GraphID)
	}
	if target.CheckoutID != "" && target.CheckoutID != graph.OwnerCheckoutID {
		return target, fmt.Errorf(
			"%w: legacy graph cleanup %s names checkout %s but is owned by %s",
			ErrSagaTarget, target.GraphID, target.CheckoutID, graph.OwnerCheckoutID)
	}
	checkout, present, err := r.catalog.GetCheckout(ctx, graph.OwnerCheckoutID)
	if err != nil {
		return target, err
	}
	if !present || checkout.Incarnation == "" || checkout.FamilyID != graph.FamilyID {
		return target, fmt.Errorf(
			"%w: legacy graph cleanup %s has no verifiable checkout incarnation",
			ErrSagaTarget, target.GraphID)
	}
	if target.FamilyID != "" && target.FamilyID != graph.FamilyID {
		return target, fmt.Errorf(
			"%w: legacy graph cleanup %s moved from family %s to %s",
			ErrSagaTarget, target.GraphID, target.FamilyID, graph.FamilyID)
	}

	transition, transitionPresent, err := r.catalog.GetIntentTransition(ctx, checkout.CheckoutID)
	if err != nil {
		return target, err
	}
	stateOwnsCleanup := transition.State == store_sqlite.IntentTransitionPending ||
		transition.State == store_sqlite.IntentTransitionRunning ||
		transition.State == store_sqlite.IntentTransitionFailed
	if checkout.ActiveIntentTransitionID == "" || !transitionPresent ||
		transition.TransitionID != checkout.ActiveIntentTransitionID ||
		transition.CheckoutID != checkout.CheckoutID ||
		transition.Cause != explicitUntrackDemotionCause ||
		transition.RequestedMode != store_sqlite.CheckoutModeAutomatic ||
		transition.PriorEffectiveMode != store_sqlite.CheckoutModeDedicated ||
		!stateOwnsCleanup ||
		!strings.HasPrefix(transition.SourceSnapshotHash, target.GraphID+":") {
		return target, fmt.Errorf(
			"%w: legacy graph cleanup %s has no active demotion ownership proof",
			ErrSagaTarget, target.GraphID)
	}

	target.CheckoutID = checkout.CheckoutID
	target.Incarnation = checkout.Incarnation
	target.FamilyID = graph.FamilyID
	target.RepoPrefix = graph.RepoPrefix
	target.RootPath = checkout.RootPath
	return target, nil
}

// hydrateGraphReleaseAddress copies the filesystem address of a positively
// identified graph binding into its durable saga target. A token mismatch is
// intentionally left untouched: releaseGraph will treat that journal entry as
// stale instead of borrowing the replacement's prefix or root.
func (r *Reconciler) hydrateGraphReleaseAddress(
	ctx context.Context, target sagaTarget,
) (sagaTarget, error) {
	if target.GraphID == "" || (target.RepoPrefix != "" && target.RootPath != "") {
		return target, nil
	}
	graph, present, err := r.catalog.GetDedicatedGraph(ctx, target.GraphID)
	if err != nil || !present {
		return target, err
	}
	if target.FamilyID != "" && target.FamilyID != graph.FamilyID {
		return target, nil
	}
	if target.CheckoutID != "" && target.CheckoutID != graph.OwnerCheckoutID {
		return target, nil
	}
	checkout, present, err := r.catalog.GetCheckout(ctx, graph.OwnerCheckoutID)
	if err != nil || !present {
		return target, err
	}
	if target.Incarnation != "" && target.Incarnation != checkout.Incarnation {
		return target, nil
	}
	if target.FamilyID == "" {
		target.FamilyID = graph.FamilyID
	}
	if target.CheckoutID == "" {
		target.CheckoutID = checkout.CheckoutID
	}
	if target.Incarnation == "" {
		target.Incarnation = checkout.Incarnation
	}
	if target.RepoPrefix == "" {
		target.RepoPrefix = graph.RepoPrefix
	}
	if target.RootPath == "" {
		target.RootPath = checkout.RootPath
	}
	return target, nil
}

// runSaga walks a plan from its persisted phase to the end.
//
// Each phase is written to the journal before it runs, so a crash anywhere
// resumes at a phase that either did nothing yet or did idempotent work. The
// journal entry is deleted last, which makes its absence the only record that
// the teardown completed — except for a layer purge, whose entry is kept in
// the done phase so nothing ever purges the same incarnation twice.
func (r *Reconciler) runSaga(ctx context.Context, target sagaTarget) error {
	if target.Kind == sagaRetireGraph && target.Incarnation == "" {
		repaired, err := r.repairLegacyRetireGraphTarget(ctx, target)
		if err != nil {
			return err
		}
		target = repaired
	}
	if target.GraphID != "" {
		hydrated, err := r.hydrateGraphReleaseAddress(ctx, target)
		if err != nil {
			return err
		}
		target = hydrated
	}
	plan := sagaPhases[target.Kind]
	if len(plan) == 0 {
		return fmt.Errorf("%w: unknown saga kind %q", ErrSagaTarget, target.Kind)
	}
	start := 0
	if target.Phase != "" {
		start = slices.Index(plan, target.Phase)
		if start < 0 {
			return fmt.Errorf("%w: phase %q is not part of saga %q", ErrSagaTarget, target.Phase, target.Kind)
		}
	}

	id := target.cleanupID()
	for _, phase := range plan[start:] {
		target.Phase = phase
		if err := r.persistPhase(ctx, id, target, store_sqlite.CleanupPhaseDeleting); err != nil {
			return err
		}
		if err := r.runPhase(ctx, target); err != nil {
			return errors.Join(err, r.persistPhase(ctx, id, target, store_sqlite.CleanupPhaseFailed))
		}
	}

	if target.Kind == sagaPurgeLayers {
		return r.persistPhase(ctx, id, target, store_sqlite.CleanupPhaseDone)
	}
	if err := r.catalog.DeleteCleanupEntry(ctx, id); err != nil && !errors.Is(err, store_sqlite.ErrCatalogNotFound) {
		return err
	}
	return nil
}

// runPhase performs one step.
func (r *Reconciler) runPhase(ctx context.Context, target sagaTarget) error {
	switch target.Phase {
	case phaseWithdrawRoute:
		return r.withdrawRoute(ctx, target.CheckoutID)
	case phasePurgeLayers:
		return r.purgeLayers(ctx, target)
	case phaseDeleteRefViews:
		return r.deleteRefViews(ctx, target.GraphID)
	case phaseReleaseGraph:
		return r.releaseGraph(ctx, target)
	case phaseDeleteCheckoutRow:
		return r.deleteCheckoutRow(ctx, target)
	case phaseVerifyCheckoutGone:
		return r.verifyCheckoutGone(ctx, target)
	case phaseForgetDependents:
		return r.forgetDependents(ctx, target)
	case phaseForgetPrimaryOwner:
		return r.forgetPrimaryOwner(ctx, target)
	case phaseVerifyClosureGone:
		return r.verifyClosureGone(ctx, target)
	case phaseForgetFamilyCheckouts:
		return r.forgetFamilyCheckouts(ctx, target)
	case phaseDeleteFamilyRow:
		return r.deleteFamilyRow(ctx, target)
	}
	return fmt.Errorf("%w: unknown phase %q", ErrSagaTarget, target.Phase)
}

// withdrawRoute removes the route row. It is the one child of a checkout that
// does not cascade, so it has to go before the checkout delete or that delete
// is refused — and after the purge that stopped the only writer of it, or the
// row comes back between the two.
func (r *Reconciler) withdrawRoute(ctx context.Context, checkoutID string) error {
	if checkoutID == "" {
		return nil
	}
	return ignoreNotFound(r.catalog.DeleteCheckoutRoute(ctx, checkoutID))
}

// purgeLayers calls the layer owner, unless the identity is already gone —
// there is nothing to purge for a checkout that no longer exists, and skipping
// is what makes re-entering a completed teardown a true no-op.
func (r *Reconciler) purgeLayers(ctx context.Context, target sagaTarget) error {
	if target.CheckoutID == "" {
		return nil
	}
	_, present, err := r.catalog.GetCheckout(ctx, target.CheckoutID)
	if err != nil {
		return err
	}
	if !present {
		return nil
	}
	return r.hooks.PurgeCheckoutLayers(ctx, target.CheckoutID, target.Incarnation)
}

// deleteRefViews removes every named view rooted in a graph. Their build
// attempts cascade with them.
func (r *Reconciler) deleteRefViews(ctx context.Context, graphID string) error {
	if graphID == "" {
		return nil
	}
	views, err := r.catalog.ListRefViews(ctx, graphID)
	if err != nil {
		return err
	}
	for _, view := range views {
		if err := ignoreNotFound(r.catalog.DeleteRefView(ctx, view.RefViewID)); err != nil {
			return err
		}
	}
	return nil
}

// releaseGraph hands the graph back to its owner and drops its row while the
// repository prefix is still closed to new index admission. Stable graph and
// checkout IDs are reusable, so the durable checkout incarnation is the ABA
// guard for both the hook and the catalog delete.
func (r *Reconciler) releaseGraph(ctx context.Context, target sagaTarget) error {
	if target.GraphID == "" {
		return nil
	}
	release := GraphReleaseTarget{
		GraphID: target.GraphID, CheckoutID: target.CheckoutID,
		Incarnation: target.Incarnation, RepoPrefix: target.RepoPrefix, RootPath: target.RootPath,
	}
	graph, present, err := r.catalog.GetDedicatedGraph(ctx, target.GraphID)
	if err != nil {
		return err
	}
	if !present {
		// Only the saga that directly owned the graph finalizer may need to
		// finish config after a crash between the two cross-store commits. A
		// parent primary-closure saga reaches the same absent row after its
		// nested checkout saga already completed and must not purge the prefix
		// a second time.
		if target.Kind != sagaRetireGraph && target.Kind != sagaForgetCheckout {
			return nil
		}
		// A graph ID and repository prefix may be reused after re-tracking. If
		// the checkout ID is live again at a different positive incarnation,
		// this journal belongs to the old binding and must stop before its
		// prefix or config are touched. A matching row, or a genuinely absent
		// checkout, is the interrupted-finalizer recovery case.
		if release.CheckoutID != "" && release.Incarnation != "" {
			checkout, checkoutPresent, err := r.catalog.GetCheckout(ctx, release.CheckoutID)
			if err != nil {
				return err
			}
			if checkoutPresent && checkout.Incarnation != release.Incarnation {
				return nil
			}
		}
		if release.RepoPrefix == "" {
			// Legacy journals written before the durable address existed reached
			// this state only after the old finalizer had already removed config.
			return nil
		}
		return r.hooks.ReleaseGraph(ctx, release, func() error { return nil })
	}
	if target.CheckoutID == "" || target.Incarnation == "" {
		return fmt.Errorf("%w: graph cleanup %s has no checkout incarnation",
			ErrSagaTarget, target.GraphID)
	}
	if graph.OwnerCheckoutID != target.CheckoutID ||
		(target.FamilyID != "" && graph.FamilyID != target.FamilyID) {
		// A positive token names an older binding. The stale cleanup is done;
		// it must not touch the replacement graph or its repository payload.
		return nil
	}
	checkout, present, err := r.catalog.GetCheckout(ctx, target.CheckoutID)
	if err != nil {
		return err
	}
	if !present {
		return fmt.Errorf("%w: graph cleanup %s cannot verify checkout %s",
			ErrSagaTarget, target.GraphID, target.CheckoutID)
	}
	if checkout.Incarnation != target.Incarnation {
		return nil
	}
	if release.RepoPrefix == "" {
		release.RepoPrefix = graph.RepoPrefix
	}
	if release.RootPath == "" {
		release.RootPath = checkout.RootPath
	}
	finalize := func() error {
		deleted, err := r.catalog.DeleteDedicatedGraphForIncarnation(
			ctx, target.GraphID, target.CheckoutID, target.Incarnation)
		if err != nil {
			return err
		}
		if deleted {
			return nil
		}
		_, replacementPresent, err := r.catalog.GetDedicatedGraph(ctx, target.GraphID)
		if err != nil {
			return err
		}
		if !replacementPresent {
			// A concurrent completion already removed the expected row.
			return nil
		}
		return fmt.Errorf("%w: graph %s was replaced before guarded deletion",
			store_sqlite.ErrCatalogStaleGuard, target.GraphID)
	}
	return r.hooks.ReleaseGraph(ctx, release, finalize)
}

// deleteCheckoutRow drops the checkout, taking its intents, its in-flight
// transition and its path evidence with it through the catalog's cascades.
//
// The layer-purge evidence for this incarnation goes too. That entry exists to
// stop a second purge of layers that are still tracked; once the identity is
// gone it guards nothing, and leaving it behind would grow the journal by a
// row for every checkout that is ever forgotten.
func (r *Reconciler) deleteCheckoutRow(ctx context.Context, target sagaTarget) error {
	if target.CheckoutID == "" {
		return nil
	}
	if target.Incarnation == "" {
		return fmt.Errorf("%w: checkout cleanup %s has no incarnation",
			ErrSagaTarget, target.CheckoutID)
	}
	if _, err := r.catalog.DeleteCheckoutForIncarnation(
		ctx, target.CheckoutID, target.Incarnation,
	); err != nil {
		return err
	}
	return ignoreNotFound(r.catalog.DeleteCleanupEntry(ctx, target.purgeCleanupID()))
}

// verifyCheckoutGone refuses to declare a teardown finished while anything
// still points at the checkout.
//
// It checks the tables that reference the checkout row by foreign key, which
// are the ones that would either strand data or block a future re-allocation
// of the same administrative name.
func (r *Reconciler) verifyCheckoutGone(ctx context.Context, target sagaTarget) error {
	if target.CheckoutID == "" {
		return nil
	}
	var leftover []string
	if checkout, present, err := r.catalog.GetCheckout(ctx, target.CheckoutID); err != nil {
		return err
	} else if present && target.Incarnation != "" && checkout.Incarnation != target.Incarnation {
		// The old identity is gone and this stable checkout ID has been reused.
		// None of the replacement's rows are postconditions of the stale saga.
		return nil
	} else if present {
		leftover = append(leftover, "checkouts")
	}
	if _, present, err := r.catalog.GetCheckoutRoute(ctx, target.CheckoutID); err != nil {
		return err
	} else if present {
		leftover = append(leftover, "checkout_routes")
	}
	if _, present, err := r.catalog.GetCheckoutPathEvidence(ctx, target.CheckoutID); err != nil {
		return err
	} else if present {
		leftover = append(leftover, "checkout_path_evidence")
	}
	if _, present, err := r.catalog.GetIntentTransition(ctx, target.CheckoutID); err != nil {
		return err
	} else if present {
		leftover = append(leftover, "intent_transitions")
	}
	if intents, err := r.catalog.ListTrackingIntents(ctx, target.CheckoutID); err != nil {
		return err
	} else if len(intents) > 0 {
		leftover = append(leftover, "tracking_intents")
	}
	owned, err := r.ownedGraph(ctx, target.FamilyID, target.CheckoutID)
	if err != nil {
		return err
	}
	if owned != nil {
		leftover = append(leftover, "dedicated_graphs")
	}
	if _, present, err := r.catalog.GetCleanupEntry(ctx, target.purgeCleanupID()); err != nil {
		return err
	} else if present {
		leftover = append(leftover, "cleanup_journal")
	}
	if len(leftover) > 0 {
		return fmt.Errorf("%w: checkout %s is still referenced by %s",
			ErrPostcondition, target.CheckoutID, strings.Join(leftover, ", "))
	}
	return nil
}

// forgetDependents forgets the checkouts that could only be served because the
// primary existed — the family's automatic ones. A checkout in dedicated mode
// has a graph of its own and survives the retirement.
func (r *Reconciler) forgetDependents(ctx context.Context, target sagaTarget) error {
	if target.FamilyID == "" {
		return nil
	}
	checkouts, err := r.catalog.ListCheckouts(ctx, target.FamilyID)
	if err != nil {
		return err
	}
	for _, checkout := range checkouts {
		if checkout.CheckoutID == target.CheckoutID {
			// The primary's own owner goes in its own phase, after the views
			// rooted in the graph have been dropped.
			continue
		}
		if checkout.EffectiveMode != store_sqlite.CheckoutModeAutomatic {
			continue
		}
		if err := r.ForgetCheckout(ctx, checkout.CheckoutID, checkout.Incarnation); err != nil {
			return err
		}
	}
	return nil
}

// forgetPrimaryOwner removes the primary graph and the checkout that owns it.
// Forgetting the owner is what drops the graph row, because a checkout's
// teardown already releases the graph it owns; a graph with no owner is
// released directly.
func (r *Reconciler) forgetPrimaryOwner(ctx context.Context, target sagaTarget) error {
	if target.CheckoutID != "" {
		owner, present, err := r.catalog.GetCheckout(ctx, target.CheckoutID)
		if err != nil {
			return err
		}
		if present {
			if target.Incarnation != "" && owner.Incarnation != target.Incarnation {
				return nil
			}
			return r.ForgetCheckout(ctx, owner.CheckoutID, owner.Incarnation)
		}
	}
	return r.releaseGraph(ctx, target)
}

// verifyClosureGone checks the family really has no primary left, and carries
// on into a family teardown when nothing is left to serve it from.
//
// The cascade lives inside the phase rather than after the saga on purpose: a
// crash between two calls would lose it, but a crash inside a phase is exactly
// what the journal is for.
func (r *Reconciler) verifyClosureGone(ctx context.Context, target sagaTarget) error {
	if target.FamilyID == "" {
		return nil
	}
	graphs, err := r.catalog.ListDedicatedGraphs(ctx, target.FamilyID)
	if err != nil {
		return err
	}
	for _, graph := range graphs {
		if graph.IsPrimaryBase {
			return fmt.Errorf("%w: family %s still has primary graph %s",
				ErrPostcondition, target.FamilyID, graph.GraphID)
		}
	}
	if len(graphs) == 0 {
		return r.ForgetFamily(ctx, target.FamilyID)
	}
	return nil
}

// forgetFamilyCheckouts forgets every checkout the family still holds, main
// worktree last: the linked worktrees are the ones with administrative
// directories hanging off the main one, so they come off first.
func (r *Reconciler) forgetFamilyCheckouts(ctx context.Context, target sagaTarget) error {
	if target.FamilyID == "" {
		return nil
	}
	checkouts, err := r.catalog.ListCheckouts(ctx, target.FamilyID)
	if err != nil {
		return err
	}
	slices.SortStableFunc(checkouts, func(a, b store_sqlite.Checkout) int {
		return boolOrder(a.AdminName == gitstate.MainAdminName) - boolOrder(b.AdminName == gitstate.MainAdminName)
	})
	for _, checkout := range checkouts {
		if err := r.ForgetCheckout(ctx, checkout.CheckoutID, checkout.Incarnation); err != nil {
			return err
		}
	}
	return nil
}

// deleteFamilyRow drops the family. Every checkout and dedicated graph
// references it with ON DELETE RESTRICT, so reaching this phase with anything
// left produces a foreign-key failure rather than a silent cascade.
func (r *Reconciler) deleteFamilyRow(ctx context.Context, target sagaTarget) error {
	if target.FamilyID == "" {
		return nil
	}
	return ignoreNotFound(r.catalog.DeleteRepositoryFamily(ctx, target.FamilyID))
}

// purgeLayersOnce runs the standalone layer purge unless the journal already
// records it as done for this exact incarnation. It is what an expiring
// availability grace calls, and the journal entry is what stops a restart from
// purging the same incarnation a second time.
func (r *Reconciler) purgeLayersOnce(ctx context.Context, checkout store_sqlite.Checkout) error {
	target := sagaTarget{
		Kind:        sagaPurgeLayers,
		CheckoutID:  checkout.CheckoutID,
		Incarnation: checkout.Incarnation,
		FamilyID:    checkout.FamilyID,
	}
	entry, present, err := r.catalog.GetCleanupEntry(ctx, target.cleanupID())
	if err != nil {
		return err
	}
	if present && entry.Phase == store_sqlite.CleanupPhaseDone {
		return nil
	}
	return r.runSaga(ctx, target)
}

// persistPhase writes a journal entry at one phase.
func (r *Reconciler) persistPhase(ctx context.Context, id string, target sagaTarget, phase store_sqlite.CleanupPhase) error {
	payload, err := json.Marshal(target)
	if err != nil {
		return fmt.Errorf("%w: encoding %s: %w", ErrSagaTarget, target.Kind, err)
	}
	return r.catalog.UpsertCleanupEntry(ctx, store_sqlite.CleanupEntry{
		CleanupID:       id,
		OpaqueTargetIDs: string(payload),
		Reason:          string(target.Kind),
		Phase:           phase,
		PrimaryEpoch:    target.PrimaryEpoch,
		LastProgress:    r.now().Unix(),
	})
}

// loadSagaTarget reads back an unfinished journal entry. A finished one is
// reported as absent, because there is nothing left to resume from it.
func (r *Reconciler) loadSagaTarget(ctx context.Context, id string) (sagaTarget, bool, error) {
	entry, present, err := r.catalog.GetCleanupEntry(ctx, id)
	if err != nil || !present || entry.Phase == store_sqlite.CleanupPhaseDone {
		return sagaTarget{}, false, err
	}
	target, err := decodeSagaTarget(entry)
	if err != nil {
		return sagaTarget{}, false, err
	}
	return target, true, nil
}

// decodeSagaTarget reads a journal entry's opaque payload.
func decodeSagaTarget(entry store_sqlite.CleanupEntry) (sagaTarget, error) {
	var target sagaTarget
	if err := json.Unmarshal([]byte(entry.OpaqueTargetIDs), &target); err != nil {
		return sagaTarget{}, fmt.Errorf("%w: entry %s: %w", ErrSagaTarget, entry.CleanupID, err)
	}
	if target.Kind == "" {
		return sagaTarget{}, fmt.Errorf("%w: entry %s names no saga kind", ErrSagaTarget, entry.CleanupID)
	}
	return target, nil
}

// ignoreNotFound treats an already-deleted row as success. Every phase may run
// again after a crash, and a row it removed the first time being missing the
// second time is the phase working, not failing.
func ignoreNotFound(err error) error {
	if errors.Is(err, store_sqlite.ErrCatalogNotFound) {
		return nil
	}
	return err
}

// boolOrder maps a flag to a sort rank, true last.
func boolOrder(v bool) int {
	if v {
		return 1
	}
	return 0
}
