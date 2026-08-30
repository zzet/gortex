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

	// One worker may execute while one more durable transition waits in memory.
	// The catalog remains the unbounded queue, so startup cost is independent of
	// how many transitions survived a restart.
	modeTransitionQueueLimit = 1
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

// modeTransitionQueueFullError is the scheduler's retryable backpressure
// signal. It shares ErrViewBuildQueueFull with the physical build gate so every
// caller has one overload contract irrespective of which bounded lane filled.
type modeTransitionQueueFullError struct {
	Limit int
}

func (e *modeTransitionQueueFullError) Error() string {
	return fmt.Sprintf("indexer: mode transition queue is full (limit %d)", e.Limit)
}

func (e *modeTransitionQueueFullError) Unwrap() error { return ErrViewBuildQueueFull }

func failedModeTransitionOutcome(
	transition store_sqlite.IntentTransition, cause error,
) modeTransitionOutcome {
	out := modeTransitionOutcome{err: cause}
	if transition.Cause == promotionTransitionCause {
		out.promotion = PromoteResult{
			CheckoutID: transition.CheckoutID, TransitionID: transition.TransitionID,
			Pending: true, Retryable: true,
		}
	}
	return out
}

// scheduleModeTransition coalesces one durable row and admits it to the single
// lifecycle worker without blocking the caller. Only one transition waits in
// memory; overflow remains pending in the catalog and is retried by the next
// successful drain, sweep, or explicit request.
func (l *CheckoutLifecycle) scheduleModeTransition(
	transition store_sqlite.IntentTransition,
) *modeTransitionRun {
	l.transitionMu.Lock()
	if l.transitionRuns == nil {
		l.transitionRuns = map[string]*modeTransitionRun{}
	}
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
	run := &modeTransitionRun{transition: transition, done: make(chan struct{})}
	if l.transitionClosed {
		run.outcome = failedModeTransitionOutcome(transition, context.Canceled)
		close(run.done)
		l.transitionMu.Unlock()
		return run
	}
	if l.transitionQueue == nil {
		l.transitionQueue = make(chan *modeTransitionRun, modeTransitionQueueLimit)
	}
	select {
	case l.transitionQueue <- run:
		l.transitionRuns[transition.TransitionID] = run
		startWorker := !l.transitionWorkerStarted
		if startWorker {
			l.transitionWorkerStarted = true
			l.transitionWG.Add(1)
		}
		l.transitionMu.Unlock()
		if startWorker {
			go l.runModeTransitionWorker()
		}
		return run
	default:
		run.outcome = failedModeTransitionOutcome(transition, &modeTransitionQueueFullError{
			Limit: modeTransitionQueueLimit,
		})
		close(run.done)
		l.transitionMu.Unlock()
		return run
	}
}

// runModeTransitionWorker owns the only executing mode transition. A promotion
// still acquires the shared build gate for its physical corpus pass and a
// demotion's coordinator acquires it for rehoming; this queue prevents a boot
// journal from allocating one waiting goroutine per row before either reaches
// that common lane.
func (l *CheckoutLifecycle) runModeTransitionWorker() {
	defer l.transitionWG.Done()
	// A failed durable row must not immediately win the next creation-ordered
	// scan and starve every unrelated row behind it. Keep failures out of this
	// worker's current drain; an explicit resume, a later sweep, or a restart
	// may retry them because the catalog row remains pending.
	failedThisDrain := make(map[string]struct{})
	for {
		if err := l.transitionCtx.Err(); err != nil {
			l.cancelQueuedModeTransitions(err)
			return
		}
		select {
		case <-l.transitionCtx.Done():
			l.cancelQueuedModeTransitions(l.transitionCtx.Err())
			return
		case run := <-l.transitionQueue:
			if run == nil {
				continue
			}
			execute := l.executeModeTransition
			if l.transitionExecute != nil {
				execute = l.transitionExecute
			}
			run.outcome = execute(l.transitionCtx, run.transition)
			close(run.done)
			if run.outcome.err != nil {
				failedThisDrain[run.transition.TransitionID] = struct{}{}
				if l.logger != nil && !errors.Is(run.outcome.err, context.Canceled) {
					l.logger.Warn("checkout lifecycle: mode transition failed",
						zap.String("transition", run.transition.TransitionID),
						zap.String("checkout", run.transition.CheckoutID),
						zap.String("cause", run.transition.Cause),
						zap.Error(run.outcome.err))
				}
			} else {
				delete(failedThisDrain, run.transition.TransitionID)
			}
			// Every terminal outcome frees one bounded slot. Pull another durable
			// row immediately, excluding failures from this drain so one broken
			// repository cannot stop healthy repositories behind it or hot-loop.
			if err := l.resumeModeTransitionsExcept(l.transitionCtx, failedThisDrain); err != nil &&
				!errors.Is(err, context.Canceled) && l.logger != nil {
				l.logger.Warn("checkout lifecycle: could not refill mode transition queue",
					zap.Error(err))
			}
		}
	}
}

func (l *CheckoutLifecycle) cancelQueuedModeTransitions(cause error) {
	for {
		select {
		case run := <-l.transitionQueue:
			if run == nil {
				continue
			}
			run.outcome = failedModeTransitionOutcome(run.transition, cause)
			close(run.done)
		default:
			return
		}
	}
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

// resumeModeTransitions fills the bounded in-process queue from the durable
// journal. Unknown causes are deliberately left standing: they may belong to a
// newer binary, and guessing at a destructive mode change is never recovery.
// Rows beyond admission capacity remain only in SQLite until an admitted
// transition finishes and drains a slot, or a later sweep retries them.
func (l *CheckoutLifecycle) resumeModeTransitions(ctx context.Context) error {
	return l.resumeModeTransitionsExcept(ctx, nil)
}

// resumeModeTransitionsExcept fills the bounded process queue while skipping
// rows a worker already failed in its current drain. The exclusion is
// deliberately process-local: durability stays in SQLite and an independent
// ResumePendingTransitions or later sweep is allowed to try the row again.
func (l *CheckoutLifecycle) resumeModeTransitionsExcept(
	ctx context.Context, excluded map[string]struct{},
) error {
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
		if _, skip := excluded[transition.TransitionID]; skip {
			continue
		}
		switch transition.Cause {
		case promotionTransitionCause, demotionTransitionCause:
			run := l.scheduleModeTransition(transition)
			select {
			case <-run.done:
				if errors.Is(run.outcome.err, ErrViewBuildQueueFull) {
					// Admission is intentionally lossy only in memory. The row is
					// still standing, and continuing this scan would allocate one
					// rejected run per remaining row for no useful work.
					return nil
				}
			default:
			}
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
	parts := strings.SplitN(transition.SourceSnapshotHash, ":", 3)
	if len(parts) != 3 {
		return l.deferModeTransition(ctx, transition, fmt.Errorf(
			"indexer: demotion transition %s has invalid source snapshot",
			transition.TransitionID))
	}
	if checkout.EffectiveMode == store_sqlite.CheckoutModeAutomatic {
		if checkout.CheckoutID != transition.CheckoutID ||
			checkout.ActiveIntentTransitionID != transition.TransitionID {
			return l.deferModeTransition(ctx, transition, fmt.Errorf(
				"%w: demotion transition %s no longer owns checkout %s",
				store_sqlite.ErrCatalogStaleGuard, transition.TransitionID, checkout.CheckoutID))
		}
		if parts[0] != "" {
			if err := l.rec.RetireDedicatedGraph(ctx, parts[0]); err != nil {
				return l.deferModeTransition(ctx, transition, fmt.Errorf(
					"indexer: finish cleanup-pending demotion: %w", err))
			}
		} else {
			prefix := l.ResolvePrefix(checkout.RootPath)
			if _, _, err := l.evictRepoChecked(ctx, prefix, checkout.RootPath); err != nil {
				return l.deferModeTransition(ctx, transition, fmt.Errorf(
					"indexer: persist cleanup-pending demotion configuration: %w", err))
			}
		}
		if err := l.catalog.CompleteIntentTransition(ctx, checkout.CheckoutID,
			transition.TransitionID); err != nil {
			return l.deferModeTransition(ctx, transition, err)
		}
		return nil
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
