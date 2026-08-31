package indexer

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"uuid"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/gitstate"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/pathkey"
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
// Every failure before that flip leaves the automatic view serving. The corpus
// half-built for the new prefix is evicted, the graph row that named it is
// dropped, and the journalled intent stays pending, which is what Retryable
// means: nothing about the checkout changed, so asking again is the whole of
// the recovery.
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
			CreatedAt:     l.now().Unix(),
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
		transition.TransitionID, store_sqlite.IntentTransitionRunning, "", l.now().Unix()); err != nil {
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
		if err := l.requireDedicatedRoute(ctx, checkout.CheckoutID, out.GraphID); err != nil {
			return out, l.promotionFailed(ctx, &out, transition, err)
		}
		if err := l.ensurePromotedRepoShell(ctx, checkout, out.Prefix); err != nil {
			return out, l.promotionFailed(ctx, &out, transition, err)
		}
		if err := l.ensurePromotionConfig(checkout, out.Prefix); err != nil {
			return out, l.promotionFailed(ctx, &out, transition, err)
		}
		if err := l.installDedicatedCoordinator(ctx, out.GraphID, checkout); err != nil {
			return out, l.promotionFailed(ctx, &out, transition, err)
		}
		l.detachWatcher(out.Prefix)
		if err := l.EnsureTrackedWatcher(ctx, out.Prefix); err != nil {
			return out, l.promotionFailed(ctx, &out, transition, err)
		}
		l.notifyTrackedSetChanged()
		if err := l.catalog.CompleteIntentTransition(ctx, checkout.CheckoutID, transition.TransitionID); err != nil {
			return out, l.promotionFailed(ctx, &out, transition, err)
		}
		// Cold Seed deliberately withholds automatic siblings until the
		// primary graph is servable. A recovered promotion owns the handoff:
		// publish first, then admit the rest of the family.
		l.reconcileFamilyNow(ctx, checkout.FamilyID, checkout.RootPath)
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
		// Main worktrees intentionally have no derived @instance name. Their
		// explicit registration already bound the base prefix to the owned graph,
		// so recover that stable name from the catalog before building the first
		// immutable corpus.
		out.Prefix = l.prefixForCheckout(ctx, checkout.CheckoutID)
	}
	if out.Prefix == "" {
		// A pre-publication rollback removes both the transient graph binding and
		// its process-local repository shell. The explicit config entry is the
		// remaining durable authority for a main worktree's stable prefix; recover
		// it by canonical root identity so the standing transition can recreate
		// the exact graph instead of becoming permanently un-retryable.
		out.Prefix = l.configuredPrefixForRoot(checkout.RootPath)
	}
	if out.Prefix == "" {
		return out, l.promotionFailed(ctx, &out, transition,
			fmt.Errorf("indexer: no dedicated prefix can be derived for %s", checkout.RootPath))
	}
	out.GraphID, err = l.bindDedicatedGraph(ctx, checkout.FamilyID, checkout.CheckoutID, out.Prefix)
	if err != nil {
		return out, l.promotionFailed(ctx, &out, transition, err)
	}
	index, baseGenerationID, sample, resampled, err := l.buildPromotedCorpus(ctx, out.GraphID, checkout, out.Prefix)
	out.Index, out.Resampled = index, resampled
	if err != nil {
		rollbackErr := l.rollbackPromotion(ctx, out.Prefix, out.GraphID,
			checkout.CheckoutID, checkout.Incarnation)
		return out, l.promotionFailed(ctx, &out, transition, errors.Join(err, rollbackErr))
	}
	if _, err := l.prepareAndPublishPromotion(ctx, checkout, transition,
		out.GraphID, baseGenerationID, sample); err != nil {
		var rollbackErr error
		current, currentErr := l.checkoutStateOf(ctx, checkout.CheckoutID)
		if currentErr == nil && current.EffectiveMode != store_sqlite.CheckoutModeDedicated {
			rollbackErr = l.rollbackPromotion(ctx, out.Prefix, out.GraphID,
				checkout.CheckoutID, checkout.Incarnation)
		}
		return out, l.promotionFailed(ctx, &out, transition, errors.Join(err, rollbackErr))
	}
	if err := l.ensurePromotedRepoShell(ctx, checkout, out.Prefix); err != nil {
		// Publication already committed. Keep the exact route and durable row;
		// the dedicated-mode recovery arm retries this process shell.
		return out, l.promotionFailed(ctx, &out, transition, err)
	}
	if err := l.ensurePromotionConfig(checkout, out.Prefix); err != nil {
		// The route owns the corpus already, so a retry may safely persist the
		// configured entry without rebuilding or exposing generation zero.
		return out, l.promotionFailed(ctx, &out, transition, err)
	}

	l.detachWatcher(out.Prefix)
	if err := l.EnsureTrackedWatcher(ctx, out.Prefix); err != nil {
		return out, l.promotionFailed(ctx, &out, transition, err)
	}
	l.notifyTrackedSetChanged()
	if err := l.catalog.CompleteIntentTransition(ctx, checkout.CheckoutID, transition.TransitionID); err != nil {
		// Publication is durable, but the transition is the completion fence.
		// Reporting success while it remains would strand startup readiness with
		// no failed worker edge and no reason recorded for the retry.
		return out, l.promotionFailed(ctx, &out, transition, err)
	}
	// The active base and owner route are durable now. Only at this point may
	// automatic siblings begin composing layers over the family's primary.
	l.reconcileFamilyNow(ctx, checkout.FamilyID, checkout.RootPath)
	return out, nil
}

// configuredPrefixForRoot resolves the exact durable config entry for one
// checkout root. It is deliberately a fallback, not the primary naming path:
// an existing graph binding remains authoritative, while this scan repairs the
// narrow state where rollback removed that binding but retained the explicit
// intent and config that authorize its recreation.
func (l *CheckoutLifecycle) configuredPrefixForRoot(root string) string {
	if l == nil || l.cfgMgr == nil || root == "" {
		return ""
	}
	registrations := l.cfgMgr.RepoRegistrations()
	entries := make([]config.RepoEntry, len(registrations))
	for i := range registrations {
		entries[i] = registrations[i].Entry
		entries[i].Path = registrations[i].CanonicalPath
	}
	want, err := filepath.Abs(root)
	if err != nil {
		want = filepath.Clean(root)
	}
	prefixOf := func(i int) string {
		prefix := EffectiveRepoPrefix(l.cfgMgr, entries[i])
		if prefix != "" && prefix != "." {
			return prefix
		}
		return ""
	}

	// The normal case is byte-equal or fold-equal and needs no filesystem
	// traversal. Keep it as a first pass: this fallback may scan hundreds of
	// configured repositories, but it should not resolve hundreds of unrelated
	// symlink chains merely to find the exact path stored by the user.
	for i, entry := range entries {
		if entry.Path == "" {
			continue
		}
		candidate, absErr := filepath.Abs(entry.Path)
		if absErr != nil {
			candidate = filepath.Clean(entry.Path)
		}
		if pathkey.EqualPaths(candidate, want) {
			if prefix := prefixOf(i); prefix != "" {
				return prefix
			}
		}
	}

	// Git commonly reports a canonical spelling that differs lexically from
	// config (macOS /var -> /private/var). SameFile is the authoritative
	// canonical-root comparison and avoids allocating full resolved paths for
	// every non-matching entry.
	wantInfo, statErr := os.Stat(want)
	if statErr == nil {
		for i, entry := range entries {
			if entry.Path == "" {
				continue
			}
			candidateInfo, candidateErr := os.Stat(entry.Path)
			if candidateErr == nil && os.SameFile(wantInfo, candidateInfo) {
				if prefix := prefixOf(i); prefix != "" {
					return prefix
				}
			}
		}
		return ""
	}

	// Promotion requires an accessible root, so this is defensive support for
	// callers inspecting an offline entry: preserve canonical lexical matching
	// when file identity cannot be sampled.
	want = pathkey.CanonicalExistingRoot(want)
	for i, entry := range entries {
		if entry.Path == "" {
			continue
		}
		if pathkey.EqualPaths(pathkey.CanonicalExistingRoot(entry.Path), want) {
			if prefix := prefixOf(i); prefix != "" {
				return prefix
			}
		}
	}
	return ""
}

// ensurePromotionConfig persists a newly explicit checkout but leaves an
// already-authoritative entry untouched. In particular, a configured root may
// be stored through a filesystem alias (/var on macOS) while Git reports its
// canonical spelling (/private/var); adding the latter would duplicate one
// logical repository even though the retry is merely restoring its graph.
func (l *CheckoutLifecycle) ensurePromotionConfig(
	checkout store_sqlite.Checkout,
	prefix string,
) error {
	if l.configuredPrefixForRoot(checkout.RootPath) == prefix {
		return nil
	}
	return l.persistPromotedRepoConfig(checkout, prefix)
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
	tree, err := gitstate.CanonicalHeadTreeOID(ctx, root, head.CommitOID, head.TreeOID)
	if err != nil {
		return checkoutSample{}, fmt.Errorf("indexer: resolve HEAD tree of %s: %w", root, err)
	}
	dirty, err := gitstate.SampleDirty(ctx, root)
	if err != nil {
		return checkoutSample{}, fmt.Errorf("indexer: sample %s: %w", root, err)
	}
	return checkoutSample{tree: tree, commit: head.CommitOID, fingerprint: dirty.Fingerprint}, nil
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
func (l *CheckoutLifecycle) rollbackPromotion(
	ctx context.Context, prefix, graphID, checkoutID, incarnation string,
) error {
	if graphID == "" {
		if prefix == "" {
			return nil
		}
		l.detachWatcher(prefix)
		_, _, err := l.mi.purgeRepoChecked(ctx, prefix, nil)
		return err
	}
	if checkoutID == "" || incarnation == "" {
		return fmt.Errorf("indexer: rolled-back graph %q has no checkout incarnation", graphID)
	}
	graph, found, err := l.catalog.GetDedicatedGraph(ctx, graphID)
	if err != nil {
		return err
	}
	if !found {
		l.queueGraphFenceRetirement(graphID)
		l.sweepTopologyFenceRetirements(ctx)
		return nil
	}
	if graph.OwnerCheckoutID != checkoutID {
		return nil
	}
	checkout, found, err := l.catalog.GetCheckout(ctx, checkoutID)
	if err != nil {
		return err
	}
	if !found || checkout.Incarnation != incarnation {
		return nil
	}
	// A topology reconciliation can race between transient graph binding and
	// full-corpus publication. Stop only a coordinator admitted against this
	// provisional graph before deleting its identity; otherwise it would poll a
	// graph that can never return and warn forever.
	l.dropCoordinatorForGraph(checkoutID, graphID)
	l.oweRetirement(l.graphGenerations(ctx, graphID)...)
	finalize := func(*RepoMetadata) error {
		deleted, err := l.catalog.DeleteDedicatedGraphForIncarnation(
			ctx, graphID, checkoutID, incarnation)
		if err != nil {
			return err
		}
		if !deleted {
			return fmt.Errorf("%w: rolled-back graph %s was replaced before deletion",
				store_sqlite.ErrCatalogStaleGuard, graphID)
		}
		return nil
	}
	if prefix == "" {
		if err := finalize(nil); err != nil {
			return err
		}
		l.queueGraphFenceRetirement(graphID)
		l.sweepTopologyFenceRetirements(ctx)
		return nil
	}
	l.detachWatcher(prefix)
	if _, _, err := l.mi.purgeRepoChecked(ctx, prefix, finalize); err != nil {
		err = l.restoreGraphAfterFailedRelease(ctx, graph, checkoutID, incarnation, err)
		return fmt.Errorf("indexer: purge rolled-back repository %q: %w", prefix, err)
	}
	l.queueGraphFenceRetirement(graphID)
	l.sweepTopologyFenceRetirements(ctx)
	return nil
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
	now := l.now().Unix()
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
	err := l.catalog.UpdateIntentTransitionProgress(ctx, out.CheckoutID, transition.TransitionID,
		store_sqlite.IntentTransitionPending, cause.Error(), l.now().Unix())
	if err != nil {
		l.logger.Warn("checkout lifecycle: could not journal a failed promotion",
			zap.String("checkout", out.CheckoutID), zap.Error(err))
		return cause
	}
	out.Pending = true
	out.Retryable = true
	return cause
}
