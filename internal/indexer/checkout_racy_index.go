package indexer

import (
	"context"
	"errors"
	"time"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/gitstate"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

// healRacyIndex runs once when a coordinator starts — on registration, on
// activation, and at every daemon start for a checkout already registered —
// and heals a racily clean index (gitstate.RefreshRacyIndex): every sample
// this coordinator takes runs `git --no-optional-locks status`, which never
// writes the index back, so a checkout born racily clean (a fresh `git
// worktree add`, a checkout of many files in one second) would otherwise pay
// a content hash of every tracked file on every sample for as long as it
// lives. It detects the state from the index file first and does nothing when
// the index is healthy; it holds the sampler's lease while it runs (so no
// sample overlaps it) and does not start while an edit's sample waits; a
// locked index is left to the next activation.
func (c *CheckoutCoordinator) healRacyIndex(ctx context.Context) {
	if c == nil || c.sampler == nil {
		return
	}
	// A coordinator activated by an edit starts while that edit is being
	// admitted: let the edit's own samples go first.
	if !c.awaitNoEdit(ctx, racyIndexEditWait) {
		return
	}
	started := time.Now()
	before, after, ran, err := c.sampler.RefreshRacyIndex(ctx)
	if c.logger == nil || (!ran && err == nil) {
		return
	}
	fields := []zap.Field{
		zap.String("checkout", c.checkoutID),
		zap.Int("entries", before.Entries),
		zap.Int("racy_before", before.Racy), zap.Int("smudged_before", before.Smudged),
		zap.Int("racy_after", after.Racy), zap.Int("smudged_after", after.Smudged),
		zap.Duration("elapsed", time.Since(started)),
	}
	switch {
	case errors.Is(err, gitstate.ErrIndexLocked), errors.Is(err, gitstate.ErrRefreshYielded):
		c.logger.Info("checkout coordinator: racily clean index left for the next activation", append(fields, zap.Error(err))...)
	case err != nil:
		c.logger.Warn("checkout coordinator: racily clean index refresh failed", append(fields, zap.Error(err))...)
	default:
		c.logger.Info("checkout coordinator: racily clean index refreshed", fields...)
	}
}

// racyIndexEditWait bounds how long the start-time refresh waits for the
// checkout's edits to finish before giving up until the next activation.
const racyIndexEditWait = 30 * time.Second

// awaitNoEdit waits (bounded) until no source mutation is admitted on this
// checkout and no urgent sample is waiting for or holding the sampler.
func (c *CheckoutCoordinator) awaitNoEdit(ctx context.Context, bound time.Duration) bool {
	deadline := time.Now().Add(bound)
	for {
		c.mu.Lock()
		editing := c.sourceMutations > 0
		c.mu.Unlock()
		if !editing && !c.sampler.UrgentBusy() {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		timer := time.NewTimer(50 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return false
		case <-timer.C:
		}
	}
}

// healFamilyRacyIndexes refreshes the racily clean index of every ready
// checkout the catalog knows for familyID, dormant ones included, once per
// daemon start, in the background: a start resumes only routed checkouts'
// coordinators, and a dormant checkout's first edit would otherwise pay a
// content hash of every tracked file on each of its samples until its
// coordinator heals it.
func (l *CheckoutLifecycle) healFamilyRacyIndexes(ctx context.Context, familyID string) {
	if l == nil || l.catalog == nil || familyID == "" {
		return
	}
	checkouts, err := l.catalog.ListCheckouts(ctx, familyID)
	if err != nil {
		return
	}
	for _, checkout := range checkouts {
		if ctx.Err() != nil {
			return
		}
		if checkout.RootPath == "" || checkout.State != store_sqlite.CheckoutStateReady {
			continue
		}
		sampler, err := gitstate.NewDirtySampler(checkout.RootPath, "", "")
		if err != nil {
			continue
		}
		started := time.Now()
		before, after, ran, err := sampler.RefreshRacyIndex(ctx)
		if l.logger == nil || (!ran && err == nil) {
			continue
		}
		fields := []zap.Field{
			zap.String("checkout", checkout.CheckoutID), zap.Int("entries", before.Entries),
			zap.Int("racy_before", before.Racy), zap.Int("smudged_before", before.Smudged),
			zap.Int("racy_after", after.Racy), zap.Int("smudged_after", after.Smudged),
			zap.Duration("elapsed", time.Since(started)),
		}
		if err != nil {
			l.logger.Info("checkout lifecycle: racily clean index left for the next activation", append(fields, zap.Error(err))...)
			continue
		}
		l.logger.Info("checkout lifecycle: racily clean index refreshed at start", fields...)
	}
}
