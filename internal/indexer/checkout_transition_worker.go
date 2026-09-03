package indexer

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/reconcile"
)

const (
	promotionTransitionCause = "promote_checkout"
	demotionTransitionCause  = "explicit_untrack_demote"
)

// modeTransitionRun is one daemon-owned execution of a durable transition.
// The catalog row is the authority; this value only coalesces workers inside
// one process and carries the result to a wait-by-default caller.
type modeTransitionRun struct {
	transition store_sqlite.IntentTransition
	done       chan struct{}
	outcome    modeTransitionOutcome
}

type modeTransitionOutcome struct {
	promotion PromoteResult
	demoted   bool
	err       error
}

// scheduleModeTransition starts at most one worker for a durable transition.
// The lifecycle context, rather than the request that admitted the work, owns
// the worker. A client disconnect therefore changes only whether that client
// waits; it cannot orphan the transition.
func (l *CheckoutLifecycle) scheduleModeTransition(
	transition store_sqlite.IntentTransition,
) *modeTransitionRun {
	l.transitionMu.Lock()
	if running := l.transitionRuns[transition.TransitionID]; running != nil {
		select {
		case <-running.done:
			// A successful result is the idempotent answer to a caller that
			// admitted the same durable row just before it completed. Failed
			// rows remain durable and are retried by the next sweep.
			if running.outcome.err == nil {
				l.transitionMu.Unlock()
				return running
			}
			delete(l.transitionRuns, transition.TransitionID)
		default:
			l.transitionMu.Unlock()
			return running
		}
	}
	if l.transitionClosed {
		run := &modeTransitionRun{transition: transition, done: make(chan struct{})}
		run.outcome.err = context.Canceled
		close(run.done)
		l.transitionMu.Unlock()
		return run
	}
	run := &modeTransitionRun{transition: transition, done: make(chan struct{})}
	l.transitionRuns[transition.TransitionID] = run
	l.transitionWG.Add(1)
	l.transitionMu.Unlock()

	go func() {
		defer l.transitionWG.Done()
		run.outcome = l.executeModeTransition(l.transitionCtx, transition)
		close(run.done)
	}()
	return run
}

func (l *CheckoutLifecycle) executeModeTransition(
	ctx context.Context, transition store_sqlite.IntentTransition,
) modeTransitionOutcome {
	if gate := l.buildGate(); gate != nil {
		if err := gate.WaitUntilOpen(ctx); err != nil {
			return modeTransitionOutcome{err: err}
		}
	}

	switch transition.Cause {
	case promotionTransitionCause:
		result, err := l.executePromotionTransition(ctx, transition)
		return modeTransitionOutcome{promotion: result, err: err}
	case demotionTransitionCause:
		err := l.executeDemotionTransition(ctx, transition)
		return modeTransitionOutcome{demoted: err == nil, err: err}
	default:
		return modeTransitionOutcome{err: fmt.Errorf(
			"indexer: transition %s has unsupported cause %q",
			transition.TransitionID, transition.Cause)}
	}
}

func (l *CheckoutLifecycle) executePromotionTransition(
	ctx context.Context, transition store_sqlite.IntentTransition,
) (PromoteResult, error) {
	return l.promoteCheckoutTransition(ctx, transition)
}

func waitModeTransition(ctx context.Context, run *modeTransitionRun) (modeTransitionOutcome, error) {
	select {
	case <-run.done:
		return run.outcome, nil
	case <-ctx.Done():
		return modeTransitionOutcome{}, ctx.Err()
	}
}

// resumeModeTransitions schedules every transition this build knows how to
// execute. Unknown causes are deliberately left standing: they may belong to a
// newer binary, and guessing at a destructive mode change is never recovery.
func (l *CheckoutLifecycle) resumeModeTransitions(ctx context.Context) error {
	if l == nil || l.catalog == nil {
		return nil
	}
	transitions, err := l.catalog.ListIntentTransitions(ctx)
	if err != nil {
		return err
	}
	standing := make(map[string]struct{}, len(transitions))
	for _, transition := range transitions {
		standing[transition.TransitionID] = struct{}{}
	}

	// Successful runs stay briefly so a concurrent waiter can observe their
	// result. The next authoritative journal scan prunes entries whose durable
	// row is gone, bounding the in-process coalescing cache.
	l.transitionMu.Lock()
	for transitionID, run := range l.transitionRuns {
		if _, exists := standing[transitionID]; exists {
			continue
		}
		select {
		case <-run.done:
			delete(l.transitionRuns, transitionID)
		default:
		}
	}
	l.transitionMu.Unlock()

	for _, transition := range transitions {
		switch transition.Cause {
		case promotionTransitionCause, demotionTransitionCause:
			l.scheduleModeTransition(transition)
		}
	}
	return nil
}

// executeDemotionTransition reconstructs the authorization entirely from its
// durable row. SourceSnapshotHash is not a content hash for a demotion; it is
// the guarded tuple written by AuthorizeDemotion: owned graph, primary graph,
// and primary epoch.
func (l *CheckoutLifecycle) executeDemotionTransition(
	ctx context.Context, transition store_sqlite.IntentTransition,
) error {
	standing, found, err := l.catalog.GetIntentTransition(ctx, transition.CheckoutID)
	if err != nil {
		return err
	}
	if !found {
		checkout, checkoutErr := l.checkoutStateOf(ctx, transition.CheckoutID)
		if checkoutErr != nil {
			return checkoutErr
		}
		// Compatibility recovery for the historical crash window in which the
		// catalog committed automatic mode and deleted the journal before the
		// external config entry was persisted. The worker input is that durable
		// request; require its complete demotion identity and an unclaimed
		// automatic checkout before touching configuration.
		if transition.Cause == demotionTransitionCause &&
			transition.RequestedMode == store_sqlite.CheckoutModeAutomatic &&
			checkout.CheckoutID == transition.CheckoutID &&
			checkout.EffectiveMode == store_sqlite.CheckoutModeAutomatic &&
			checkout.ActiveIntentTransitionID == "" {
			prefix := l.ResolvePrefix(checkout.RootPath)
			if _, _, err := l.evictRepoChecked(ctx, prefix, checkout.RootPath); err != nil {
				return fmt.Errorf("indexer: recover demotion configuration: %w", err)
			}
			return nil
		}
		return fmt.Errorf("%w: demotion transition %s is no longer standing",
			store_sqlite.ErrCatalogStaleGuard, transition.TransitionID)
	}
	if standing.TransitionID != transition.TransitionID ||
		standing.Cause != demotionTransitionCause ||
		standing.RequestedMode != store_sqlite.CheckoutModeAutomatic {
		return fmt.Errorf("%w: demotion transition %s was replaced",
			store_sqlite.ErrCatalogStaleGuard, transition.TransitionID)
	}
	transition = standing
	if err := l.catalog.UpdateIntentTransitionProgress(ctx, transition.CheckoutID,
		transition.TransitionID, store_sqlite.IntentTransitionRunning, "", l.now().Unix()); err != nil {
		return err
	}
	checkout, err := l.checkoutStateOf(ctx, transition.CheckoutID)
	if err != nil {
		return l.deferModeTransition(ctx, transition, err)
	}
	if checkout.EffectiveMode == store_sqlite.CheckoutModeAutomatic {
		if checkout.CheckoutID != transition.CheckoutID ||
			checkout.ActiveIntentTransitionID != transition.TransitionID {
			return l.deferModeTransition(ctx, transition, fmt.Errorf(
				"%w: demotion transition %s no longer owns checkout %s",
				store_sqlite.ErrCatalogStaleGuard, transition.TransitionID, checkout.CheckoutID))
		}
		prefix := l.ResolvePrefix(checkout.RootPath)
		if _, _, err := l.evictRepoChecked(ctx, prefix, checkout.RootPath); err != nil {
			return l.deferModeTransition(ctx, transition, fmt.Errorf(
				"indexer: persist cleanup-pending demotion configuration: %w", err))
		}
		if err := l.catalog.CompleteIntentTransition(ctx, checkout.CheckoutID,
			transition.TransitionID); err != nil {
			return l.deferModeTransition(ctx, transition, err)
		}
		return nil
	}
	parts := strings.SplitN(transition.SourceSnapshotHash, ":", 3)
	if len(parts) != 3 {
		return l.deferModeTransition(ctx, transition, fmt.Errorf(
			"indexer: demotion transition %s has invalid source snapshot",
			transition.TransitionID))
	}
	epoch, err := strconv.ParseInt(parts[2], 10, 64)
	if err != nil {
		return l.deferModeTransition(ctx, transition, fmt.Errorf(
			"indexer: demotion transition %s has invalid primary epoch: %w",
			transition.TransitionID, err))
	}
	var owned *store_sqlite.DedicatedGraph
	if parts[0] != "" {
		graph, found, err := l.catalog.GetDedicatedGraph(ctx, parts[0])
		if err != nil {
			return l.deferModeTransition(ctx, transition, err)
		}
		if found {
			owned = &graph
		}
	}
	authorization := reconcile.DemotionAuthorization{
		Transition:     transition,
		OwnedGraphID:   parts[0],
		PrimaryGraphID: parts[1],
		PrimaryEpoch:   epoch,
	}
	if err := l.demote(ctx, checkout, owned, authorization); err != nil {
		return l.deferModeTransition(ctx, transition, err)
	}
	return nil
}

func (l *CheckoutLifecycle) deferModeTransition(
	ctx context.Context, transition store_sqlite.IntentTransition, cause error,
) error {
	if cause == nil {
		return nil
	}
	if err := l.catalog.UpdateIntentTransitionProgress(ctx, transition.CheckoutID,
		transition.TransitionID, store_sqlite.IntentTransitionPending,
		cause.Error(), l.now().Unix()); err != nil && !errors.Is(err, context.Canceled) {
		l.logger.Warn("checkout lifecycle: could not defer mode transition",
			zap.String("transition", transition.TransitionID), zap.Error(err))
	}
	return cause
}
