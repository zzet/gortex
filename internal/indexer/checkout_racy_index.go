package indexer

import (
	"context"
	"errors"
	"sync"
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
	if c == nil {
		return
	}
	defer c.racyHeal.finish()
	if c.sampler == nil {
		return
	}
	// A coordinator activated by an edit starts while that edit is being
	// admitted: let the edit's own samples go first.
	if !c.awaitNoEdit(ctx, racyIndexEditWait) {
		return
	}
	// Never while the lifecycle's own git work runs: the refresh takes the
	// index lock, and a git command of the lifecycle's (or one it waits on)
	// would fail on it.
	release, ok := c.racyHeal.gitWork.refresh(ctx, racyIndexEditWait)
	if !ok {
		return
	}
	defer release()
	started := time.Now()
	before, after, ran, err := racyIndexRefresh(ctx, c.sampler)
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
		release, ok := l.gitWork.refresh(ctx, racyIndexEditWait)
		if !ok {
			continue
		}
		started := time.Now()
		before, after, ran, err := racyIndexRefresh(ctx, sampler)
		release()
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

// racyIndexRefresh runs one refresh; a test seam.
var racyIndexRefresh = func(ctx context.Context, s *gitstate.DirtySampler) (gitstate.RacyIndexReport, gitstate.RacyIndexReport, bool, error) {
	return s.RefreshRacyIndex(ctx)
}

// checkoutGitWork serialises the racily clean index refreshes (which take a
// checkout's index lock) against the lifecycle's own git work: a refresh
// never starts while that work runs, and the work waits for a refresh in
// flight to finish before it starts.
type checkoutGitWork struct {
	mu        sync.Mutex
	cond      *sync.Cond
	holders   int
	refreshes int
}

func (w *checkoutGitWork) init() {
	if w.cond == nil {
		w.cond = sync.NewCond(&w.mu)
	}
}

// hold marks the lifecycle's git work in flight, after any refresh already
// running finishes; release ends it.
func (w *checkoutGitWork) hold() (release func()) {
	if w == nil {
		return func() {}
	}
	w.mu.Lock()
	w.init()
	w.holders++
	for w.refreshes > 0 {
		w.cond.Wait()
	}
	w.mu.Unlock()
	return func() {
		w.mu.Lock()
		w.holders--
		w.cond.Broadcast()
		w.mu.Unlock()
	}
}

// refresh admits one refresh once no git work of the lifecycle is in flight,
// waiting at most bound; ok is false when it could not be admitted (the next
// activation refreshes instead).
func (w *checkoutGitWork) refresh(ctx context.Context, bound time.Duration) (release func(), ok bool) {
	if w == nil {
		return func() {}, true
	}
	deadline := time.Now().Add(bound)
	for {
		w.mu.Lock()
		w.init()
		if w.holders == 0 {
			w.refreshes++
			w.mu.Unlock()
			return func() {
				w.mu.Lock()
				w.refreshes--
				w.cond.Broadcast()
				w.mu.Unlock()
			}, true
		}
		w.mu.Unlock()
		if time.Now().After(deadline) {
			return nil, false
		}
		timer := time.NewTimer(20 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, false
		case <-timer.C:
		}
	}
}

// busy reports whether git work of the lifecycle is in flight; a test seam.
func (w *checkoutGitWork) busy() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.holders > 0
}

// racyHealState is a coordinator's start-time refresh: done is closed once it
// finished (or never started).
type racyHealState struct {
	gitWork *checkoutGitWork
	done    chan struct{}
	once    sync.Once
}

func (r *racyHealState) begin(work *checkoutGitWork) {
	r.gitWork = work
	r.done = make(chan struct{})
}

func (r *racyHealState) finish() {
	if r.done != nil {
		r.once.Do(func() { close(r.done) })
	}
}

// awaitRacyIndexHeal waits (bounded) for the coordinator's start-time
// refresh to finish; it reports whether it did.
func (c *CheckoutCoordinator) awaitRacyIndexHeal(bound time.Duration) bool {
	if c == nil || c.racyHeal.done == nil {
		return true
	}
	select {
	case <-c.racyHeal.done:
		return true
	case <-time.After(bound):
		return false
	}
}
