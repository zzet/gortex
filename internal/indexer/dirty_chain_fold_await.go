package indexer

import (
	"context"
	"slices"
	"time"

	"go.uber.org/zap"
)

// An edit at the cap over a running fold with at most one layer above it.
//
// Folding the layers above a running fold inline is refused when there is only
// one ("the chain is one generation deep"), and building direct costs the
// whole dirty set (36 s measured on the daemon's own repository). The fold
// in flight is the cheaper way out: the edit waits for it, for at most the
// inline budget, and stands on it. The edit holds the cycle lock while it
// waits, so the fold cannot land by itself; once the fold is published the
// edit lands it: it re-bases the one layer above onto the fold (the landing's
// re-base, done here) and chains on that layer. The background attempt finds
// the landing claimed and reports it as the edit's. A fold that is not
// published within the budget, or that fails, leaves the edit to build
// direct, as before.

// foldLandByEdit is a stepped fold landed by an edit that waited for it.
const foldLandByEdit = "by_edit"

// foldPublication is one stepped fold's result, offered to an edit waiting at
// the cap. ready is closed once the fold is published (built set) or has
// failed (built zero).
type foldPublication struct {
	ready   chan struct{}
	built   dirtyLayerBuild
	folded  []int64
	claimed bool
}

// offerFoldPublication registers the stepped fold about to run.
func (c *CheckoutCoordinator) offerFoldPublication() *foldPublication {
	pub := &foldPublication{ready: make(chan struct{})}
	c.compaction.mu.Lock()
	c.compaction.publication = pub
	c.compaction.mu.Unlock()
	return pub
}

// settleFoldPublication records the fold's result (zero built: it failed) and
// wakes a waiting edit.
func (c *CheckoutCoordinator) settleFoldPublication(pub *foldPublication, built dirtyLayerBuild, folded []int64) {
	c.compaction.mu.Lock()
	pub.built, pub.folded = built, slices.Clone(folded)
	c.compaction.mu.Unlock()
	close(pub.ready)
}

// retireFoldPublication forgets pub once its attempt is over.
func (c *CheckoutCoordinator) retireFoldPublication(pub *foldPublication) {
	c.compaction.mu.Lock()
	if c.compaction.publication == pub {
		c.compaction.publication = nil
	}
	c.compaction.mu.Unlock()
}

// foldLandingClaimed reports whether an edit landed pub.
func (c *CheckoutCoordinator) foldLandingClaimed(pub *foldPublication) bool {
	if pub == nil {
		return false
	}
	c.compaction.mu.Lock()
	defer c.compaction.mu.Unlock()
	return pub.claimed
}

// awaitRunningFold waits, for at most the inline budget, for the running fold
// of folding (oldest first) to be published, lands it under the caller's
// cycle lock and returns the generation the edit chains on: the one layer
// above the fold, re-based onto it, or the fold itself. 0 when the fold was
// not published in time, failed, or folded another chain.
func (c *CheckoutCoordinator) awaitRunningFold(ctx context.Context, commitGeneration int64, routed, folding []int64) int64 {
	above := routed[len(folding):]
	if len(above) > 1 {
		return 0
	}
	c.compaction.mu.Lock()
	pub := c.compaction.publication
	c.compaction.mu.Unlock()
	if pub == nil {
		return 0
	}
	budget := c.inlineFoldBudget()
	started := time.Now()
	timer := time.NewTimer(budget)
	defer timer.Stop()
	select {
	case <-pub.ready:
	case <-timer.C:
		c.logger.Info("checkout coordinator: the running fold did not land within the budget; building direct",
			zap.String("checkout", c.checkoutID), zap.Int64s("folding", folding), zap.Duration("budget", budget))
		return 0
	case <-ctx.Done():
		return 0
	}
	c.compaction.mu.Lock()
	if pub.built.GenerationID == 0 || pub.claimed || !slices.Equal(pub.folded, folding) {
		c.compaction.mu.Unlock()
		return 0
	}
	pub.claimed = true
	built := pub.built
	c.compaction.mu.Unlock()

	c.retainDirty(ctx, built.Key, built.GenerationID)
	c.handOverFoldRegistry(ctx, commitGeneration, nil, folding, built.GenerationID, above)
	top := built.GenerationID
	if len(above) == 1 {
		if err := c.foldBackend().RebaseViewGeneration(ctx, above[0], folding[len(folding)-1], built.GenerationID); err != nil {
			// The background attempt lands it (or keeps it as the preferred
			// parent); this edit builds direct.
			c.compaction.mu.Lock()
			pub.claimed = false
			c.compaction.mu.Unlock()
			c.logger.Info("checkout coordinator: re-base onto the waited fold refused; building direct",
				zap.String("checkout", c.checkoutID), zap.Int64("layer", above[0]), zap.Error(err))
			return 0
		}
		top = above[0]
	}
	for _, id := range folding {
		c.releaseDirty(ctx, id)
	}
	c.logger.Info("checkout coordinator: edit at the cap waited for the running fold and chains on it",
		zap.String("checkout", c.checkoutID), zap.Int64s("folded", folding),
		zap.Int64("folded_generation", built.GenerationID), zap.Int64("parent", top),
		zap.Duration("waited", time.Since(started)))
	return top
}
