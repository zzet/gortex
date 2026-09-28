package indexer

import (
	"context"
	"errors"
	"slices"
	"time"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

// The stepped chain fold, wired into the compactor.
//
// The background compaction folds the chain in steps (the store's ChainFold)
// instead of one long copy it has to abandon whenever an edit arrives:
//
//   - it takes the build lane only for its checks, and gives it back before
//     the first step; an edit builds and publishes above the chain while the
//     steps run, and the store hands a step back to an edit's write;
//   - the edit cycle no longer cancels it (cancelDirtyChainCompaction with
//     compactionYieldForegroundCycle stands down while it steps), nor does a
//     new schedule replace it;
//   - it lands under the cycle lock (landChainFold): a flip when nothing was
//     published above it, a re-base of the lowest layer above it otherwise,
//     and the preferred parent when the route moved on;
//   - while it runs, parent selection counts its layers as one
//     (chainBoundAction): an edit keeps chaining above it up to the physical
//     bound, and at the bound folds only the layers above it, in its own
//     cycle.

// steppedChainFoldEnabled is a test seam: off, the compactor copies the chain
// at once and yields to foreground work, as before.
var steppedChainFoldEnabled = true

// compactionYieldForegroundCycle is the reason the edit cycle gives when it
// asks the compaction to yield.
const compactionYieldForegroundCycle = "foreground cycle"

// compactionYieldsToEdits are the reasons an edit gives when it asks the
// compaction to yield: the edit cycle, and the synchronous republish of an
// MCP edit (CheckoutMutation.Refresh). A stepped fold stands down for them;
// a transition of the checkout or the coordinator's shutdown still cancels.
var compactionYieldsToEdits = map[string]bool{
	compactionYieldForegroundCycle:  true,
	compactionYieldCheckoutMutation: true,
}

// compactionYieldCheckoutMutation is the reason an MCP edit's republish gives.
const compactionYieldCheckoutMutation = "synchronous checkout mutation"

// storeChainFoldBackend is chainFoldBackend over the store.
type storeChainFoldBackend struct{ store *store_sqlite.Store }

func (b storeChainFoldBackend) BeginChainFold(ctx context.Context, chain []int64, to int64, owner string) (chainFoldSteps, error) {
	fold, err := b.store.BeginChainFold(ctx, store_sqlite.ChainFoldRequest{Chain: chain, To: to, Owner: owner})
	if err != nil {
		return nil, err
	}
	return fold, nil
}

func (b storeChainFoldBackend) RebaseViewGeneration(ctx context.Context, generationID, fromBase, toBase int64) error {
	return b.store.Catalog().RebaseViewGeneration(ctx, store_sqlite.RebaseViewGenerationRequest{
		GenerationID: generationID, FromBase: fromBase, ToBase: toBase,
	})
}

func (storeChainFoldBackend) StepRetryable(err error) bool {
	return errors.Is(err, store_sqlite.ErrChainFoldYielded) || errors.Is(err, store_sqlite.ErrChainFoldWALMark)
}

// foldBackend is the coordinator's backend: the store's, unless a test
// installed another.
func (c *CheckoutCoordinator) foldBackend() chainFoldBackend {
	if c.compaction.backend != nil {
		return c.compaction.backend
	}
	return storeChainFoldBackend{store: c.store}
}

// foldingChain is the chain (oldest first) a stepped fold of this checkout is
// folding now, nil when none runs.
func (c *CheckoutCoordinator) foldingChain() []int64 {
	if c == nil {
		return nil
	}
	c.compaction.mu.Lock()
	defer c.compaction.mu.Unlock()
	return slices.Clone(c.compaction.foldingChain)
}

// copyChainInSteps is the stepped copier: the store's ChainFold, stepped to
// its end. While it runs the chain is published as the folding chain.
func (c *CheckoutCoordinator) copyChainInSteps(ctx context.Context, oldestFirst []int64, to int64) (store_sqlite.GenerationCopyCounts, func(context.Context, bool), error) {
	backend := c.foldBackend()
	fold, err := backend.BeginChainFold(ctx, oldestFirst, to, c.checkoutID)
	if err != nil {
		return store_sqlite.GenerationCopyCounts{}, nil, err
	}
	c.compaction.mu.Lock()
	c.compaction.foldingChain = slices.Clone(oldestFirst)
	hook := c.compaction.stepHook
	c.compaction.mu.Unlock()
	finish := func(ctx context.Context, ok bool) {
		// Either way the fold is released: the caller marks an abandoned
		// target failed and owes it to the sweep.
		_ = fold.Release(ctx)
		c.compaction.mu.Lock()
		c.compaction.foldingChain = nil
		c.compaction.mu.Unlock()
	}
	if hook != nil {
		hook(ctx, 0)
	}
	started := time.Now()
	steps, retries, err := runChainFoldSteps(ctx, fold, backend.StepRetryable, func(step int) {
		if hook != nil {
			hook(ctx, step)
		}
	})
	var counts store_sqlite.GenerationCopyCounts
	if stepped, ok := fold.(interface {
		Counts() (store_sqlite.GenerationCopyCounts, int, int)
	}); ok {
		counts, _, _ = stepped.Counts()
	}
	c.logger.Info("checkout coordinator: chain fold stepped",
		zap.String("checkout", c.checkoutID), zap.Int64s("chain", oldestFirst), zap.Int64("folded_generation", to),
		zap.Int("steps", steps), zap.Int("retries", retries), zap.Duration("elapsed", time.Since(started)), zap.Error(err))
	if err != nil {
		finish(context.WithoutCancel(ctx), false)
		return counts, nil, err
	}
	return counts, finish, nil
}

// landSteppedFold lands a published stepped fold of folded (oldest first)
// under the cycle lock, waiting for a running cycle to finish. It reports
// whether the fold entered the route (flip or re-base).
func (c *CheckoutCoordinator) landSteppedFold(ctx context.Context, commitGeneration int64, folded []int64, built dirtyLayerBuild) (foldLanding, bool) {
	c.retainDirty(ctx, built.Key, built.GenerationID)
	ticker := time.NewTicker(dirtyChainCompactionYieldPoll)
	defer ticker.Stop()
	for !c.cycleMu.TryLock() {
		select {
		case <-ctx.Done():
			c.setPreferredDirtyParent(built.GenerationID, "folded; the landing was canceled")
			return foldLanding{kind: foldLandMoved}, false
		case <-ticker.C:
		}
	}
	defer c.cycleMu.Unlock()
	route, found, err := c.catalog.GetCheckoutRoute(ctx, c.checkoutID)
	if err != nil || !found || route.State != store_sqlite.RouteActive ||
		route.CommitGenerationID != commitGeneration || route.DirtyGenerationID <= 0 {
		c.setPreferredDirtyParent(built.GenerationID, "folded while the route moved on")
		return foldLanding{kind: foldLandMoved}, false
	}
	routed := c.dirtyChainMembers(ctx, route.DirtyGenerationID)
	slices.Reverse(routed)
	flip := func(ctx context.Context, to int64) error {
		if err := c.flip(ctx, &route, store_sqlite.RouteSlotDirty, to); err != nil {
			return err
		}
		markPublicationPhase(ctx, PublicationRouteFlipped)
		return nil
	}
	release := func(ctx context.Context, members []int64) {
		for _, id := range members {
			c.releaseDirty(ctx, id)
		}
	}
	landing, err := c.landChainFold(ctx, c.foldBackend(), routed, folded, built.GenerationID, flip, release)
	if err != nil {
		// A re-base guard miss (the layer above moved between the read and
		// the write) or a lost flip: the fold is still a valid parent.
		c.logger.Info("checkout coordinator: chain fold landing refused",
			zap.String("checkout", c.checkoutID), zap.String("landing", landing.kind), zap.Error(err))
		c.setPreferredDirtyParent(built.GenerationID, "fold landing refused")
		return landing, false
	}
	return landing, landing.kind == foldLandFlip || landing.kind == foldLandRebase
}

// foldAboveRunningFold folds, in the edit's own cycle, the routed layers above
// the running fold into one generation over the fold's top layer, and returns
// it (0 when refused). The running fold then re-bases that generation when it
// lands.
func (c *CheckoutCoordinator) foldAboveRunningFold(ctx context.Context, route store_sqlite.CheckoutRoute, commitGeneration int64, folding []int64) int64 {
	commit, found, err := c.catalog.GetViewGeneration(ctx, commitGeneration)
	if err != nil || !found {
		return 0
	}
	root, found, err := c.catalog.GetViewGeneration(ctx, folding[len(folding)-1])
	if err != nil || !found {
		return 0
	}
	started := time.Now()
	built, err := c.flattenDirtyChainOver(ctx, commit, root, route.DirtyGenerationID, c.copyChainAtOnce)
	if err != nil {
		c.logger.Info("checkout coordinator: fold above the running fold refused",
			zap.String("checkout", c.checkoutID), zap.Int64("chain_top", route.DirtyGenerationID),
			zap.Duration("elapsed", time.Since(started)), zap.Error(err))
		return 0
	}
	c.retainDirty(ctx, built.Key, built.GenerationID)
	c.logger.Info("checkout coordinator: layers above the running fold folded",
		zap.String("checkout", c.checkoutID), zap.Int64("chain_top", route.DirtyGenerationID),
		zap.Int64("folded_generation", built.GenerationID), zap.Duration("elapsed", time.Since(started)))
	return built.GenerationID
}

// parentChainBound is the depth bound parent selection walks route's chain
// under: the effective bound, or the physical one while a stepped fold runs
// and the bound rule lets an edit chain above it.
func (c *CheckoutCoordinator) parentChainBound(ctx context.Context, route store_sqlite.CheckoutRoute) int {
	folding := c.foldingChain()
	if len(folding) == 0 || route.DirtyGenerationID <= 0 {
		return maxDirtyChainDepth
	}
	routed := c.dirtyChainMembers(ctx, route.DirtyGenerationID)
	slices.Reverse(routed)
	if chainBoundAction(routed, folding) == chainActionChain {
		return maxChainWalkDepth
	}
	return maxDirtyChainDepth
}

// foldAtBoundAroundFold is the fold an edit at the bound makes while a
// stepped fold runs: only the layers above it when the route still stands on
// it (fold_upper), and 0 otherwise (the caller folds the whole chain).
func (c *CheckoutCoordinator) foldAtBoundAroundFold(ctx context.Context, route store_sqlite.CheckoutRoute, commitGeneration int64) int64 {
	folding := c.foldingChain()
	if len(folding) == 0 {
		return 0
	}
	routed := c.dirtyChainMembers(ctx, route.DirtyGenerationID)
	slices.Reverse(routed)
	if chainBoundAction(routed, folding) != chainActionFoldUpper {
		return 0
	}
	return c.foldAboveRunningFold(ctx, route, commitGeneration, folding)
}

// compactDirtyChainStepped is the rest of a compaction attempt with the fold
// in steps: it stops yielding to foreground work, gives the build lane back,
// folds the routed chain in steps and lands the fold. The edit cycle no
// longer cancels it; only ctx (the coordinator's shutdown or a transition of
// the checkout) does.
func (c *CheckoutCoordinator) compactDirtyChainStepped(
	ctx context.Context, trigger CheckoutCycle, commit store_sqlite.ViewGeneration, route store_sqlite.CheckoutRoute,
	report *DirtyChainCompaction, stopYielding, releaseLane func(),
) string {
	k := &c.compaction
	k.stepping.Store(true)
	defer k.stepping.Store(false)
	stopYielding()
	releaseLane()
	if ctx.Err() != nil {
		report.Outcome, report.Canceled = dirtyChainCompactionCanceled, true
		return ""
	}
	var folded []int64
	copier := func(ctx context.Context, oldestFirst []int64, to int64) (store_sqlite.GenerationCopyCounts, func(context.Context, bool), error) {
		folded = slices.Clone(oldestFirst)
		return c.copyChainInSteps(ctx, oldestFirst, to)
	}
	started := time.Now()
	built, err := c.flattenDirtyChainOver(ctx, commit, commit, route.DirtyGenerationID, copier)
	report.BuildDuration = time.Since(started)
	switch {
	case err == nil:
		report.GenerationID = built.GenerationID
		landing, entered := c.landSteppedFold(context.WithoutCancel(ctx), trigger.CommitGenerationID, folded, built)
		report.Landing = landing.kind
		if entered {
			report.Outcome = dirtyChainCompactionFlipped
		} else {
			report.Outcome = dirtyChainCompactionPreferred
		}
	case ctx.Err() != nil:
		report.Outcome, report.Canceled = dirtyChainCompactionCanceled, true
	case errors.Is(err, errFlattenRefused), errors.Is(err, store_sqlite.ErrChainFoldBusy), errors.Is(err, store_sqlite.ErrChainFoldStale):
		c.logger.Debug("checkout coordinator: stepped chain fold refused; the chain stays routed",
			zap.String("checkout", c.checkoutID), zap.Error(err))
		report.Outcome, report.Err = dirtyChainCompactionFoldRefused, err
	default:
		report.Outcome, report.Err = dirtyChainCompactionFailed, err
	}
	return ""
}
