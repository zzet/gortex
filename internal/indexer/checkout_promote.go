package indexer

import (
	"context"
	"errors"
	"fmt"
	"uuid"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/gitstate"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/reconcile"
)

// ErrCheckoutMoved reports that a checkout changed state under an operation
// that had to describe one state of it. The corpus a promotion built would
// describe a working tree the checkout has already left, so it is discarded
// rather than published against an identity it does not match.
var ErrCheckoutMoved = errors.New("indexer: the checkout moved under the operation")

// PromoteResult is what one promotion did.
type PromoteResult struct {
	// TransitionID is the durable operation identifier. It is empty only when
	// the checkout was already dedicated and no work had to be admitted.
	TransitionID string
	// Pending reports that the transition is durable and owned by the daemon,
	// but this caller chose not to wait or stopped waiting before it finished.
	Pending bool
	// CheckoutID is the identity that was promoted. It does not change: a
	// promotion moves where a checkout is served from, not who it is.
	CheckoutID string
	// Prefix is the repo prefix the new dedicated corpus lives under.
	Prefix string
	// GraphID is the dedicated graph bound to that prefix.
	GraphID string
	// Index is the full index that built the corpus, nil when the corpus was
	// already there.
	Index *IndexResult
	// Resampled counts the times the checkout moved under the index and the
	// build was taken again.
	Resampled int
	// Retryable reports that the journalled intent is still standing, so the
	// same call can be made again. It is only ever true alongside an error.
	Retryable bool
}

// PromoteCheckout gives an automatic checkout a corpus of its own.
//
// The order is what makes it safe to interrupt. The intent is journalled
// first, so a promotion that dies anywhere leaves a durable record of what was
// being asked for. Then the checkout is sampled, indexed into a NEW prefix as
// its own base corpus, and sampled again: a working tree that moved under the
// index produced a corpus describing a state the checkout has left, and the
// build is taken again rather than published. Only once a corpus that matches
// the checkout exists does anything the query surface reads change — the mode
// flips to dedicated and the automatic route and coordinator are retired.
//
// Every failure before that flip leaves the automatic view serving. Cleanup
// evicts only the witnessed registration and its own unpublished graph row;
// replaced or unknown ownership is retained rather than purged by prefix. The
// journalled intent stays pending, which is what Retryable means.
func (l *CheckoutLifecycle) PromoteCheckout(
	ctx context.Context, checkoutID string, source store_sqlite.IntentSourceKind,
) (PromoteResult, error) {
	out, run, err := l.startPromoteCheckout(ctx, checkoutID, source)
	if err != nil || run == nil {
		return out, err
	}
	outcome, waitErr := waitModeTransition(ctx, run)
	if waitErr != nil {
		out.Pending = true
		return out, waitErr
	}
	return outcome.promotion, outcome.err
}

// StartPromoteCheckout durably admits a promotion and returns as soon as its
// lifecycle-owned worker has been scheduled. PromoteCheckout is the
// wait-by-default compatibility wrapper.
func (l *CheckoutLifecycle) StartPromoteCheckout(
	ctx context.Context, checkoutID string, source store_sqlite.IntentSourceKind,
) (PromoteResult, error) {
	out, _, err := l.startPromoteCheckout(ctx, checkoutID, source)
	return out, err
}

func (l *CheckoutLifecycle) startPromoteCheckout(
	ctx context.Context, checkoutID string, source store_sqlite.IntentSourceKind,
) (PromoteResult, *modeTransitionRun, error) {
	if l == nil || l.mi == nil {
		return PromoteResult{}, nil, errors.New("indexer: checkout lifecycle is not wired")
	}
	if l.catalog == nil {
		return PromoteResult{}, nil, errNoCatalog
	}
	checkout, err := l.checkoutStateOf(ctx, checkoutID)
	if err != nil {
		return PromoteResult{}, nil, err
	}
	out := PromoteResult{CheckoutID: checkoutID}
	if checkout.EffectiveMode == store_sqlite.CheckoutModeDedicated {
		out.Prefix = l.prefixForCheckout(ctx, checkoutID)
		out.GraphID = GraphIDFor(out.Prefix)
		return out, nil, nil
	}
	if checkout.State != store_sqlite.CheckoutStateReady {
		return out, nil, fmt.Errorf("%w: checkout %s is %s, not ready",
			ErrCheckoutMoved, checkoutID, checkout.State)
	}

	var intent *store_sqlite.TrackingIntent
	if source != TrackSourceImplicit {
		intent = &store_sqlite.TrackingIntent{
			IntentID:      uuid.NewV7().String(),
			CheckoutID:    checkoutID,
			SourceKind:    source,
			SourceLocator: checkout.RootPath,
			Active:        true,
			CreatedAt:     l.clock().Unix(),
		}
	}
	transition, err := l.beginModeChange(ctx, checkout,
		store_sqlite.CheckoutModeDedicated, promotionTransitionCause, intent)
	if err != nil {
		return out, nil, err
	}
	out.TransitionID = transition.TransitionID
	out.Pending = true
	return out, l.scheduleModeTransition(transition), nil
}

func (l *CheckoutLifecycle) promoteCheckoutTransition(
	ctx context.Context, transition store_sqlite.IntentTransition,
) (PromoteResult, error) {
	out := PromoteResult{CheckoutID: transition.CheckoutID, TransitionID: transition.TransitionID}
	if transition.Cause != promotionTransitionCause ||
		transition.RequestedMode != store_sqlite.CheckoutModeDedicated {
		return out, fmt.Errorf("indexer: transition %s is not a promotion", transition.TransitionID)
	}
	standing, found, err := l.catalog.GetIntentTransition(ctx, transition.CheckoutID)
	if err != nil {
		return out, err
	}
	if !found {
		checkout, checkoutErr := l.checkoutStateOf(ctx, transition.CheckoutID)
		if checkoutErr != nil {
			return out, checkoutErr
		}
		if checkout.EffectiveMode != store_sqlite.CheckoutModeDedicated {
			return out, fmt.Errorf("%w: promotion transition %s is no longer standing",
				store_sqlite.ErrCatalogStaleGuard, transition.TransitionID)
		}
		out.Prefix = l.prefixForCheckout(ctx, checkout.CheckoutID)
		if out.Prefix == "" {
			return out, fmt.Errorf("indexer: no dedicated prefix is bound to %s", checkout.CheckoutID)
		}
		out.GraphID = GraphIDFor(out.Prefix)
		return out, nil
	}
	if standing.TransitionID != transition.TransitionID ||
		standing.Cause != promotionTransitionCause ||
		standing.RequestedMode != store_sqlite.CheckoutModeDedicated {
		return out, fmt.Errorf("%w: promotion transition %s was replaced",
			store_sqlite.ErrCatalogStaleGuard, transition.TransitionID)
	}
	transition = standing
	if err := l.catalog.UpdateIntentTransitionProgress(ctx, transition.CheckoutID,
		transition.TransitionID, store_sqlite.IntentTransitionRunning, "", l.clock().Unix()); err != nil {
		return out, err
	}
	checkout, err := l.checkoutStateOf(ctx, transition.CheckoutID)
	if err != nil {
		return out, l.promotionFailed(ctx, &out, transition, err)
	}
	if checkout.EffectiveMode == store_sqlite.CheckoutModeDedicated {
		out.Prefix = l.prefixForCheckout(ctx, checkout.CheckoutID)
		if out.Prefix == "" {
			return out, l.promotionFailed(ctx, &out, transition,
				fmt.Errorf("indexer: no dedicated prefix is bound to %s", checkout.CheckoutID))
		}
		out.GraphID = GraphIDFor(out.Prefix)
		l.attachWatcher(out.Prefix)
		l.saveConfig("promote-resume")
		l.notifyTrackedSetChanged()
		if err := l.catalog.CompleteIntentTransition(ctx, checkout.CheckoutID, transition.TransitionID); err != nil {
			return out, err
		}
		return out, nil
	}
	if checkout.State != store_sqlite.CheckoutStateReady {
		return out, l.promotionFailed(ctx, &out, transition, fmt.Errorf(
			"%w: checkout %s is %s, not ready", ErrCheckoutMoved,
			checkout.CheckoutID, checkout.State))
	}
	defer l.beginBatch()()

	out.Prefix = l.dedicatedPrefixFor(ctx, checkout.RootPath)
	if out.Prefix == "" {
		return out, l.promotionFailed(ctx, &out, transition,
			fmt.Errorf("indexer: no dedicated prefix can be derived for %s", checkout.RootPath))
	}
	index, resampled, owned, err := l.indexPromotedCorpus(ctx, checkout, out.Prefix)
	out.Index, out.Resampled = index, resampled
	if err != nil {
		var collision *PrefixCollisionError
		if !errors.As(err, &collision) {
			err = errors.Join(err, l.rollbackPromotionOrLog(ctx, out.Prefix, "", checkout, owned))
		}
		return out, l.promotionFailed(ctx, &out, transition, err)
	}

	out.GraphID = GraphIDFor(out.Prefix)
	row := store_sqlite.DedicatedGraph{
		GraphID:         out.GraphID,
		OwnerCheckoutID: checkout.CheckoutID,
		RepoPrefix:      out.Prefix,
		FamilyID:        checkout.FamilyID,
		IsPrimaryBase:   false,
		State:           reconcile.GraphStateReady,
	}
	if l.afterPromotionIndex != nil {
		l.afterPromotionIndex()
	}
	if err := l.publishPromotedCorpus(ctx, checkout, row, owned); err != nil {
		err = errors.Join(err, l.rollbackPromotionOrLog(ctx, out.Prefix, out.GraphID, checkout, owned))
		return out, l.promotionFailed(ctx, &out, transition, err)
	}

	l.attachWatcher(out.Prefix)
	l.saveConfig("promote")
	l.notifyTrackedSetChanged()
	if err := l.catalog.CompleteIntentTransition(ctx, checkout.CheckoutID, transition.TransitionID); err != nil {
		l.logger.Warn("checkout lifecycle: could not release the promotion journal",
			zap.String("checkout", checkout.CheckoutID), zap.Error(err))
	}
	return out, nil
}

// publishPromotedCorpus retains the existing repository mutation lane across
// the owner recheck and publication. Untrack/retrack cannot substitute a new
// corpus between that check and the mode flip. No filesystem work or catalog
// call runs under mi.mu.
func (l *CheckoutLifecycle) publishPromotedCorpus(ctx context.Context, checkout store_sqlite.Checkout, row store_sqlite.DedicatedGraph, owned prefixOwnerWitness) error {
	l.mi.mu.RLock()
	err := l.mi.validatePrefixCleanupOwnerLocked(row.RepoPrefix, &owned)
	idx := l.mi.indexers[row.RepoPrefix]
	l.mi.mu.RUnlock()
	if err != nil {
		return err
	}
	lane, current := l.mi.repositoryMutationCoordinatorForSnapshot(row.RepoPrefix, owned.meta, idx)
	if !current || lane == nil {
		return errPrefixOwnerChanged
	}
	return lane.runExclusive(ctx, func() error {
		l.mi.mu.RLock()
		err := l.mi.validatePrefixCleanupOwnerLocked(row.RepoPrefix, &owned)
		l.mi.mu.RUnlock()
		if err != nil {
			return err
		}
		return idx.withOutputGeneration(ctx, OutputEntryPromotionPublication, func() error {
			// Legacy unpublished catalog rows allow rebinding. Promotion cannot use
			// that compatibility behavior to overwrite a different checkout owner.
			binding, found, err := l.catalog.GetDedicatedGraph(ctx, row.GraphID)
			if err != nil {
				return err
			}
			if found && (binding.OwnerCheckoutID != checkout.CheckoutID || binding.FamilyID != checkout.FamilyID) {
				return fmt.Errorf("%w: promotion graph %s belongs to another checkout", store_sqlite.ErrCatalogStaleGuard, row.GraphID)
			}
			if err := l.catalog.UpsertDedicatedGraph(ctx, row); err != nil {
				return err
			}
			return l.serveFromOwnCorpus(ctx, checkout)
		})
	})
}

// checkoutSample is the state a promotion has to describe: what the checkout
// is committed at, and what its working tree holds on top.
type checkoutSample struct {
	tree        string
	commit      string
	fingerprint string
}

// sampleCheckout reads both halves of a checkout's state in one go.
func sampleCheckout(ctx context.Context, root string) (checkoutSample, error) {
	head, err := gitstate.SampleHEAD(ctx, root)
	if err != nil {
		return checkoutSample{}, fmt.Errorf("indexer: sample HEAD of %s: %w", root, err)
	}
	dirty, err := gitstate.SampleDirty(ctx, root)
	if err != nil {
		return checkoutSample{}, fmt.Errorf("indexer: sample %s: %w", root, err)
	}
	return checkoutSample{tree: head.TreeOID, commit: head.CommitOID, fingerprint: dirty.Fingerprint}, nil
}

// indexPromotedCorpus builds the checkout's own base corpus, and insists that
// the corpus describes the checkout as it was when the index finished.
//
// A full index of a working tree takes as long as it takes, and a checkout
// under an agent's hands can commit or save part way through it. The result
// would be a corpus that is a mixture of two states, published as generation 0
// of a prefix and never diffed against anything — nothing downstream could
// ever notice. So the state is sampled on both sides of the index and the
// build is taken again when it moved.
//
// Twice, and no more. A checkout that will not hold still for one index will
// not hold still for a third, and the promotion has to fail while the
// automatic view is still there to fall back on.
func (l *CheckoutLifecycle) indexPromotedCorpus(
	ctx context.Context, checkout store_sqlite.Checkout, prefix string,
) (*IndexResult, int, prefixOwnerWitness, error) {
	resampled := 0
	var owned prefixOwnerWitness
	for attempt := 0; attempt < 2; attempt++ {
		before, err := sampleCheckout(ctx, checkout.RootPath)
		if err != nil {
			return nil, resampled, owned, err
		}
		if l.indexBarrier != nil {
			l.indexBarrier()
		}
		result, err := l.mi.TrackRepoCtx(WithIndexOrigin(ctx, "checkout_promotion"), config.RepoEntry{Path: checkout.RootPath, Name: prefix})
		if err != nil {
			// Another registration may have claimed this exact root under a
			// different name after promotion selected its prefix. Configured
			// name validation then refuses Track before its normal ownership
			// check. Preserve the typed collision for this established owner;
			// neither the requested corpus nor its rollback is authorized.
			observation, ownerErr := l.mi.observePrefixOwnership(ctx, prefix, checkout.RootPath)
			if ownerErr == nil && observation.registeredPrefix != "" && observation.registeredPrefix != prefix {
				return nil, resampled, owned, newObservedPrefixCollision(prefix, checkout.RootPath, checkout.RootPath, observation.candidate)
			}
			return nil, resampled, owned, err
		}
		owned, err = l.mi.prefixOwnerForRoot(ctx, prefix, checkout.RootPath)
		if err != nil {
			return nil, resampled, owned, err
		}
		after, err := sampleCheckout(ctx, checkout.RootPath)
		if err != nil {
			return nil, resampled, owned, err
		}
		if after == before {
			return result, resampled, owned, nil
		}
		resampled++
		if _, _, err := l.mi.purgeRepoOwnedChecked(ctx, owned, nil); err != nil {
			return nil, resampled, owned, err
		}
		owned = prefixOwnerWitness{}
	}
	return nil, resampled, owned, fmt.Errorf("%w: %s moved under two full indexes",
		ErrCheckoutMoved, checkout.RootPath)
}

// serveFromOwnCorpus is the flip: the checkout becomes dedicated and stops
// being served through the family's automatic lane.
//
// The coordinator is stopped first, because it is the one thing that would
// rebuild the route this is about to withdraw. Then the mode moves under the
// incarnation guard — it is what every other surface reads to decide which
// lane a checkout is in — and only then does the route come down.
//
// A flip that loses its guard leaves the route and both of its generations
// exactly where they were, so the automatic view keeps serving; what it loses
// is the coordinator, which the next sweep brings back for a checkout the
// catalog still calls automatic.
//
// The mode flip is the commit point. Everything after it is cleanup, and is
// reported as such: a dedicated checkout is read from its own corpus whatever
// the route says, so a withdrawal that fails leaves a row nothing consults —
// not a promotion to undo. Returning the failure would run the rollback over a
// corpus every reader has already been pointed at, and would journal a retry
// that finds the checkout dedicated and does nothing.
func (l *CheckoutLifecycle) serveFromOwnCorpus(ctx context.Context, checkout store_sqlite.Checkout) error {
	l.dropCoordinator(checkout.CheckoutID)
	err := l.catalog.UpdateCheckoutState(ctx, store_sqlite.UpdateCheckoutStateRequest{
		CheckoutID:    checkout.CheckoutID,
		Incarnation:   checkout.Incarnation,
		State:         store_sqlite.CheckoutStateReady,
		DesiredMode:   store_sqlite.CheckoutModeDedicated,
		EffectiveMode: store_sqlite.CheckoutModeDedicated,
		LastSeen:      l.clock().Unix(),
	})
	if err != nil {
		return err
	}
	// Read before the withdrawal: the route row is the only thing in the
	// catalog that names a checkout's two generations, so once it is gone the
	// payload has no id anything could offer for collection.
	l.oweRoutedGenerations(ctx, checkout.CheckoutID)
	l.withdrawAutomaticRoute(ctx, checkout.CheckoutID)
	l.sweepRetirements(ctx)
	return nil
}

// withdrawAutomaticRoute takes down the route a checkout was served through in
// the family's automatic lane.
//
// A failure is logged and left standing rather than reported. The row it could
// not remove routes nothing — the checkout's mode has already moved — and the
// sweep withdraws it on its next pass over the family, which is also what frees
// the two generations it is still naming.
func (l *CheckoutLifecycle) withdrawAutomaticRoute(ctx context.Context, checkoutID string) {
	withdraw := l.catalog.DeleteCheckoutRoute
	if l.routeBarrier != nil {
		withdraw = l.routeBarrier
	}
	if err := withdraw(ctx, checkoutID); err != nil &&
		!errors.Is(err, store_sqlite.ErrCatalogNotFound) {
		l.logger.Warn("checkout lifecycle: could not withdraw a promoted checkout's automatic route",
			zap.String("checkout", checkoutID), zap.Error(err))
	}
}

// rollbackPromotion undoes what a failed promotion built. The automatic view
// was never touched, so putting the corpus and the graph row back the way they
// were is the whole of it.
func (l *CheckoutLifecycle) rollbackPromotion(ctx context.Context, prefix, graphID string, checkout store_sqlite.Checkout, owned prefixOwnerWitness) error {
	if prefix == "" || owned.meta == nil || owned.prefix != prefix {
		return errPrefixIdentityUnknown
	}
	// Even a failure before our Upsert must not purge a corpus now bound to
	// another checkout/family. Read the actual row; never infer ownership from
	// its graph id or the class of the primary failure.
	binding, found, err := l.catalog.GetDedicatedGraph(ctx, GraphIDFor(prefix))
	if err != nil {
		return err
	}
	if found {
		if binding.OwnerCheckoutID != checkout.CheckoutID || binding.FamilyID != checkout.FamilyID {
			owner, ownerFound, ownerErr := l.catalog.GetCheckout(ctx, binding.OwnerCheckoutID)
			if ownerErr != nil {
				return ownerErr
			}
			root := ""
			if ownerFound {
				root = owner.RootPath
			}
			return newObservedPrefixCollision(prefix, root, checkout.RootPath, prefixStatResult{err: errPrefixOwnerChanged})
		}
		_, published, err := l.catalog.DedicatedBasePublication(ctx, binding.GraphID)
		if err != nil {
			return err
		}
		if published {
			return fmt.Errorf("%w: promotion rollback refuses published graph %s", store_sqlite.ErrCatalogStaleGuard, binding.GraphID)
		}
	}
	if graphID != "" && graphID != GraphIDFor(prefix) {
		return fmt.Errorf("%w: promotion rollback graph/prefix mismatch", store_sqlite.ErrCatalogStaleGuard)
	}
	if _, _, err := l.mi.purgeRepoOwnedChecked(ctx, owned, func() { l.detachWatcher(prefix) }); err != nil {
		return fmt.Errorf("indexer: purge owned rolled-back repository %q: %w", prefix, err)
	}
	if graphID != "" && found {
		// This guarded delete compares the complete row and refuses publication
		// acquired after the observation. It never deletes a replacement binding.
		if err := l.catalog.DeleteUnpublishedDedicatedGraph(ctx, binding); err != nil {
			return fmt.Errorf("indexer: drop owned rolled-back graph binding %q: %w", graphID, err)
		}
	}
	return nil
}

// rollbackPromotionOrLog retains cleanup errors alongside the primary failure.
// Unknown/replaced ownership is a refusal to purge, never a successful cleanup.
func (l *CheckoutLifecycle) rollbackPromotionOrLog(ctx context.Context, prefix, graphID string, checkout store_sqlite.Checkout, owned prefixOwnerWitness) error {
	err := l.rollbackPromotion(ctx, prefix, graphID, checkout, owned)
	if err != nil {
		l.logger.Warn("checkout lifecycle: could not roll back a failed promotion",
			zap.String("prefix", prefix), zap.String("root", checkout.RootPath), zap.String("graph", graphID), zap.Error(err))
	}
	return err
}

// beginModeChange journals a mode change, adopting the entry an interrupted
// attempt left behind rather than refusing to start beside it.
func (l *CheckoutLifecycle) beginModeChange(
	ctx context.Context,
	checkout store_sqlite.Checkout,
	requested store_sqlite.CheckoutMode,
	cause string,
	trackingIntent *store_sqlite.TrackingIntent,
) (store_sqlite.IntentTransition, error) {
	now := l.clock().Unix()
	transition := store_sqlite.IntentTransition{
		TransitionID:       uuid.NewV7().String(),
		CheckoutID:         checkout.CheckoutID,
		Cause:              cause,
		PriorDesiredMode:   checkout.DesiredMode,
		PriorEffectiveMode: checkout.EffectiveMode,
		RequestedMode:      requested,
		PriorCheckoutState: checkout.State,
		State:              store_sqlite.IntentTransitionPending,
		CreatedAt:          now,
		LastProgress:       now,
	}
	standing, _, err := l.catalog.BeginIntentTransitionWithTrackingIntent(ctx,
		store_sqlite.BeginIntentTransitionRequest{
			Transition: transition, Incarnation: checkout.Incarnation,
			TrackingIntent: trackingIntent,
		})
	if err != nil {
		return store_sqlite.IntentTransition{}, err
	}
	return standing, nil
}

// promotionFailed leaves the journalled intent standing and pending, which is
// what makes the same call a retry rather than a second request — and says so
// on the result, since nothing about the checkout changed.
func (l *CheckoutLifecycle) promotionFailed(
	ctx context.Context, out *PromoteResult, transition store_sqlite.IntentTransition, cause error,
) error {
	lastError := cause.Error()
	var collision *PrefixCollisionError
	if errors.As(cause, &collision) {
		lastError = fmt.Sprintf("repository prefix %q is already used by %s; promotion will retry under a suffixed prefix",
			collision.Prefix, collision.ExistingPath)
	}
	err := l.catalog.UpdateIntentTransitionProgress(ctx, out.CheckoutID, transition.TransitionID,
		store_sqlite.IntentTransitionPending, lastError, l.clock().Unix())
	if err != nil {
		l.logger.Warn("checkout lifecycle: could not journal a failed promotion",
			zap.String("checkout", out.CheckoutID), zap.Error(err))
		return cause
	}
	out.Pending = true
	out.Retryable = true
	return cause
}
