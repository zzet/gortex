package indexer

import (
	"context"
	"errors"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/gitstate"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/graphview"
)

// Background compaction of working-tree chains.
//
// A chained edit is cheap because it re-derives only what changed since the
// previous publication, but every link it adds is one more layer a reader
// composes, and the chain has a hard bound (maxDirtyChainDepth) past which the
// next edit has to be built direct, at the cost of the whole accumulated dirty
// set. Compaction keeps a chain from reaching that bound: once a published
// chain is dirtyChainCompactionDepth deep (or its overlay covers more than
// dirtyChainFoldPaths paths), the coordinator folds it into one generation over
// the commit generation by copying rows (dirty_chain_flatten.go), in the
// background — no working-tree file is parsed:
//
//   - it runs outside the cycle lock, on the shared build lane at background
//     priority, so it never holds anything an edit waits on except the lane
//     itself — and it gives the lane back the moment an edit wants it: a cycle
//     of this checkout that needs to build cancels it before queueing, and any
//     interactive build queued on the lane (another checkout's edit included)
//     cancels it too;
//   - on success, if the route still describes the state it compacted (same
//     commit generation, same working-tree fingerprint) and no cycle holds the
//     cycle lock, the route is flipped to the fold with the usual epoch
//     compare-and-set and the old chain is owed to the background sweep;
//     otherwise the fold becomes the preferred parent of the next build, which
//     then stands at depth 2;
//   - a fold that does not reproduce the chain is abandoned before it is
//     published and the chain stays routed; an edit that later meets the
//     depth bound builds its state direct and reports chain_depth_exhausted;
//   - its cost is recorded (DirtyChainCompaction, DirtyChainCompactionStats),
//     never charged to an edit and never hidden: a canceled compaction is
//     counted as canceled.

// dirtyChainCompactionDepth is the soft chain depth: a build that publishes a
// chain this deep schedules a background compaction.
const dirtyChainCompactionDepth = 4

// dirtyChainCompactionYieldPoll is how often a compaction checks for
// foreground work it must yield to (checkoutForegroundActivity).
const dirtyChainCompactionYieldPoll = 10 * time.Millisecond

// dirtyChainCompactionQuiet is how long the checkout must have been free of
// foreground work before a compaction attempt starts: long enough that a
// query bundle's back-to-back requests do not each restart it, short enough
// that an idle checkout compacts almost at once.
const dirtyChainCompactionQuiet = 250 * time.Millisecond

// dirtyChainCompactionAttempts bounds the attempts one scheduled compaction
// makes when foreground work keeps interrupting it; the next build at the
// soft depth schedules a new one.
const dirtyChainCompactionAttempts = 4

// Compaction outcomes, as DirtyChainCompaction.Outcome reports them.
const (
	// dirtyChainCompactionFlipped: the route now names the compacted
	// generation.
	dirtyChainCompactionFlipped = "flipped"
	// dirtyChainCompactionPreferred: the compacted generation was published
	// but the route moved on (or a cycle held the lock); it is the preferred
	// parent of the next build.
	dirtyChainCompactionPreferred = "preferred_parent"
	// dirtyChainCompactionCanceled: foreground work took the lane first.
	dirtyChainCompactionCanceled = "canceled"
	// dirtyChainCompactionNotNeeded: the route is no longer on a chain at the
	// soft depth over the trigger's commit generation (a clean reset, an
	// earlier compaction, a moved commit).
	dirtyChainCompactionNotNeeded = "not_needed"
	// dirtyChainCompactionHeadMoved: the checkout is at another commit than
	// the one the chain is rooted at.
	dirtyChainCompactionHeadMoved = "head_moved"
	// dirtyChainCompactionFoldRefused: the fold did not reproduce the chain
	// and was abandoned; the chain stays routed.
	dirtyChainCompactionFoldRefused = "fold_refused"
	// dirtyChainCompactionFailed: the fold failed.
	dirtyChainCompactionFailed = "failed"
)

// DirtyChainCompaction is what one background compaction did.
type DirtyChainCompaction struct {
	// Landing is how a stepped fold entered the route (flip, rebase, moved),
	// empty for a fold copied at once.
	Landing string
	// Top, Commit and ChainDepthBefore describe the chain the triggering
	// cycle published.
	Top              int64
	Commit           int64
	ChainDepthBefore int
	// GenerationID is the compacted generation, 0 when none was published.
	GenerationID int64
	// Outcome is one of the dirtyChainCompaction* codes.
	Outcome  string
	Canceled bool
	// Duration is the whole compaction, lane wait included; BuildDuration
	// only its fold.
	Duration      time.Duration
	BuildDuration time.Duration
	Err           error
	// Attempts counts the attempts started; YieldedTo names, per attempt
	// given up, the foreground activity it gave the lane and the CPU back to;
	// QuietWait is the time spent waiting for a quiet interval before them.
	Attempts  int
	YieldedTo []string
	QuietWait time.Duration
}

// DirtyChainCompactionStats accumulates every compaction one coordinator ran.
type DirtyChainCompactionStats struct {
	Scheduled   int
	Flipped     int
	Preferred   int
	Canceled    int
	NotNeeded   int
	FoldRefused int
	Failed      int
	Duration    time.Duration
	// AttemptsStarted counts attempts that took the build lane.
	AttemptsStarted int
}

// dirtyChainCompactor is the coordinator's compaction state.
type dirtyChainCompactor struct {
	mu sync.Mutex
	// cancel cancels the compaction scheduled last; nil when none is owed.
	cancel context.CancelFunc
	// running is closed when the compaction scheduled last has finished.
	running chan struct{}
	wg      sync.WaitGroup
	closed  bool
	// preferred is the generation the next working-tree build should try to
	// stand on before the routed top.
	preferred int64
	stats     DirtyChainCompactionStats
	// census caches checkoutLanguageCensus per commit generation.
	census map[int64]map[string]int

	// done is a test seam: it observes every finished compaction.
	done func(DirtyChainCompaction)
	// barrier is a test seam: it runs once the compaction holds the lane,
	// before it samples and builds.
	barrier func(context.Context)
	// quiet is a test seam: the quiet interval (<0: none, 0: the default).
	quiet time.Duration

	// lastForeground is when this checkout's latest foreground cycle ended.
	lastForeground time.Time
	// folding is set while an attempt holds the build lane and folds; the
	// deferred retirement sweep stands down for it (it is this checkout's
	// write that matters in that gap).
	folding atomic.Bool
	// stepping is set while a stepped fold runs its steps and lands: the edit
	// cycle does not cancel it and a new schedule does not replace it.
	stepping atomic.Bool
	// foldingChain is the chain (oldest first) the stepped fold is folding.
	foldingChain []int64
	// backend is a test seam: the fold backend (nil: the store).
	backend chainFoldBackend
	// stepHook is a test seam: it runs before the first step (0) and after
	// every step of a stepped fold.
	stepHook func(ctx context.Context, step int)
}

// CompactionInFlight reports whether a chain fold of this checkout holds the
// build lane now.
func (c *CheckoutCoordinator) compactionInFlight() bool {
	return c != nil && c.compaction.folding.Load()
}

// noteForegroundCycle records the end of one foreground cycle of this
// checkout (one that served a ticket or built a working tree).
func (c *CheckoutCoordinator) noteForegroundCycle(ended time.Time) {
	if c == nil {
		return
	}
	k := &c.compaction
	k.mu.Lock()
	defer k.mu.Unlock()
	if ended.After(k.lastForeground) {
		k.lastForeground = ended
	}
}

// dirtyChainCompactionDue reports whether a cycle's publication owes a
// compaction: the working-tree build it routed stands at least
// dirtyChainCompactionDepth deep.
func (c *CheckoutCoordinator) dirtyChainCompactionDue(out CheckoutCycle) bool {
	return out.DirtyBuilt && out.DirtyGenerationID > 0 &&
		out.DirtyChainDepth >= dirtyChainCompactionDepth
}

// compactionOwed reports whether the compaction a schedule started is still
// queued or running (its done channel not yet closed).
func compactionOwed(running chan struct{}) bool {
	if running == nil {
		return false
	}
	select {
	case <-running:
		return false
	default:
		return true
	}
}

// compactionQuietFor is the quiet interval a compaction attempt waits for
// before it starts: none for a stepped fold, whose steps give way to an
// edit's writes (a burst never leaves a quiet interval, so waiting for one
// kept every fold from starting); the configured one (0: the default)
// otherwise.
func compactionQuietFor(configured time.Duration) time.Duration {
	if steppedChainFoldEnabled {
		return -1
	}
	if configured == 0 {
		return dirtyChainCompactionQuiet
	}
	return configured
}

// scheduleDirtyChainCompaction starts a background compaction for the chain
// trigger published. A compaction still owed from an earlier trigger is
// canceled; the new one starts once it has finished. A closed coordinator
// schedules nothing.
func (c *CheckoutCoordinator) scheduleDirtyChainCompaction(trigger CheckoutCycle) bool {
	if c == nil {
		return false
	}
	k := &c.compaction
	lifetime := c.lifetimeContext()
	k.mu.Lock()
	if k.closed || lifetime.Err() != nil {
		k.mu.Unlock()
		return false
	}
	if k.stepping.Load() || (steppedChainFoldEnabled && compactionOwed(k.running)) {
		// A stepped fold is running, or a compaction is queued: it folds
		// the chain the route holds when it starts, and lands on whatever
		// was published above it. Replacing a queued one with a newer
		// trigger only restarts its wait, so a burst would keep every fold
		// from ever starting.
		k.mu.Unlock()
		return false
	}
	if k.cancel != nil {
		k.cancel()
	}
	previous := k.running
	ctx, cancel := context.WithCancel(lifetime)
	done := make(chan struct{})
	k.cancel, k.running = cancel, done
	k.stats.Scheduled++
	k.wg.Add(1)
	k.mu.Unlock()
	go func() {
		defer k.wg.Done()
		defer close(done)
		defer cancel()
		if previous != nil {
			select {
			case <-previous:
			case <-ctx.Done():
			}
		}
		c.compactDirtyChain(ctx, trigger)
		k.mu.Lock()
		if k.running == done {
			k.cancel = nil
		}
		k.mu.Unlock()
	}()
	return true
}

// cancelDirtyChainCompaction cancels the owed compaction, if any. It reports
// whether there was one.
func (c *CheckoutCoordinator) cancelDirtyChainCompaction(why string) bool {
	if c == nil {
		return false
	}
	k := &c.compaction
	if compactionYieldsToEdits[why] && (steppedChainFoldEnabled || k.stepping.Load()) {
		// A stepped fold does not yield to an edit: it holds the build lane
		// only for its checks, the store hands its steps back to the edit's
		// writes, and it lands on what the edit publishes. Cancelling it on
		// every cycle, the checkout's own follow-up cycles that build nothing
		// included, kept most folds from ever starting.
		return false
	}
	k.mu.Lock()
	cancel := k.cancel
	k.cancel = nil
	k.mu.Unlock()
	if cancel == nil {
		return false
	}
	cancel()
	c.logger.Debug("checkout coordinator: background chain compaction yields",
		zap.String("checkout", c.checkoutID), zap.String("why", why))
	return true
}

// closeDirtyChainCompactor refuses every later schedule and cancels the owed
// compaction. CloseContext waits for it (waitDirtyChainCompactions).
func (c *CheckoutCoordinator) closeDirtyChainCompactor() {
	k := &c.compaction
	k.mu.Lock()
	k.closed = true
	cancel := k.cancel
	k.cancel = nil
	k.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// waitDirtyChainCompactions waits for every scheduled compaction to finish.
func (c *CheckoutCoordinator) waitDirtyChainCompactions(ctx context.Context) error {
	finished := make(chan struct{})
	go func() {
		c.compaction.wg.Wait()
		close(finished)
	}()
	select {
	case <-finished:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// DirtyChainCompactionStats reports what this coordinator's compactions did.
func (c *CheckoutCoordinator) DirtyChainCompactionStats() DirtyChainCompactionStats {
	if c == nil {
		return DirtyChainCompactionStats{}
	}
	c.compaction.mu.Lock()
	defer c.compaction.mu.Unlock()
	return c.compaction.stats
}

// setPreferredDirtyParent records the generation the next working-tree build
// should try to stand on before the routed top. The selection re-validates it
// (servable, rooted at the commit generation, same policy, complete
// manifests) like any parent.
func (c *CheckoutCoordinator) setPreferredDirtyParent(generationID int64, why string) {
	if c == nil || generationID <= 0 {
		return
	}
	c.compaction.mu.Lock()
	c.compaction.preferred = generationID
	c.compaction.mu.Unlock()
	c.logger.Debug("checkout coordinator: preferred working-tree parent",
		zap.String("checkout", c.checkoutID),
		zap.Int64("generation", generationID), zap.String("why", why))
}

// takePreferredDirtyParent returns and forgets the preferred parent. It is
// tried by exactly one build.
func (c *CheckoutCoordinator) takePreferredDirtyParent() int64 {
	c.compaction.mu.Lock()
	defer c.compaction.mu.Unlock()
	preferred := c.compaction.preferred
	c.compaction.preferred = 0
	return preferred
}

// selectDirtyParentForSlot is reportDirtyParent honoring the preferred parent:
// a preferred generation the selection accepts is used before the routed top
// (it is a compacted chain, or the top an edit lease withdrew from the route);
// anything else selects over the route as before.
func (c *CheckoutCoordinator) selectDirtyParentForSlot(
	ctx context.Context,
	route store_sqlite.CheckoutRoute,
	commitGeneration int64,
	sample gitstate.DirtySnapshot,
	out *CheckoutCycle,
) dirtyParentSelection {
	atBound := route
	if preferred := c.takePreferredDirtyParent(); preferred > 0 && preferred != route.DirtyGenerationID {
		alternative := route
		alternative.DirtyGenerationID = preferred
		selection := c.reportDirtyParent(ctx, alternative, commitGeneration, sample, out)
		if selection.Parent > 0 {
			out.DirtyParentPreferred = true
			return selection
		}
		if selection.Reason == dirtyChainFallbackChainDepthExhausted {
			// A checkout mutation withdrew the routed top (the route's dirty
			// slot is empty while it writes), so the chain at the bound is the
			// preferred one: it is the one to fold, not the empty route.
			atBound = alternative
		}
	}
	selection := c.reportDirtyParent(ctx, route, commitGeneration, sample, out)
	if selection.Parent == 0 && atBound.DirtyGenerationID != route.DirtyGenerationID {
		selection = dirtyParentSelection{Reason: dirtyChainFallbackChainDepthExhausted}
	}
	if selection.Parent == 0 && selection.Reason == dirtyChainFallbackChainDepthExhausted && atBound.DirtyGenerationID > 0 {
		// The chain is at its bound and no fold landed. Folding it here —
		// a copy of the chain's rows, no file parsed — costs a fraction of
		// the direct build the edit would otherwise pay over the whole
		// accumulated dirty set, and the edit then chains on the fold.
		folded := c.foldAtBoundAroundFold(ctx, atBound, commitGeneration)
		if folded == 0 {
			folded = c.foldChainAtBound(ctx, atBound, commitGeneration)
		}
		if folded > 0 {
			alternative := route
			alternative.DirtyGenerationID = folded
			if refold := c.reportDirtyParent(ctx, alternative, commitGeneration, sample, out); refold.Parent > 0 {
				out.DirtyParentPreferred = true
				return refold
			}
		}
		out.DirtyParentCandidate, out.DirtyChainReason = selection.Parent, selection.Reason
	}
	return selection
}

// foldChainAtBound folds the routed chain in the cycle that met the bound and
// returns the folded generation (0 when the fold was refused or failed; the
// edit then builds direct as before). The fold is filed in the reuse cache so
// it owes no retirement while the edit stands on it; the edit's route flip
// then releases the old chain to the sweep.
func (c *CheckoutCoordinator) foldChainAtBound(ctx context.Context, route store_sqlite.CheckoutRoute, commitGeneration int64) int64 {
	if !foldChainAtBoundEnabled {
		return 0
	}
	commit, found, err := c.catalog.GetViewGeneration(ctx, commitGeneration)
	if err != nil || !found {
		return 0
	}
	started := time.Now()
	built, err := c.flattenDirtyChain(ctx, commit, route.DirtyGenerationID)
	if err != nil {
		c.logger.Info("checkout coordinator: chain fold at the bound refused; building direct",
			zap.String("checkout", c.checkoutID), zap.Int64("chain_top", route.DirtyGenerationID),
			zap.Duration("elapsed", time.Since(started)), zap.Error(err))
		return 0
	}
	c.retainDirty(ctx, built.Key, built.GenerationID)
	c.logger.Info("checkout coordinator: chain folded at the bound",
		zap.String("checkout", c.checkoutID), zap.Int64("chain_top", route.DirtyGenerationID),
		zap.Int64("folded_generation", built.GenerationID), zap.Duration("elapsed", time.Since(started)))
	return built.GenerationID
}

// foldChainAtBoundEnabled is a test seam (the fallback it replaces is what a
// test compares against).
var foldChainAtBoundEnabled = true

// holdWithdrawnDirty files the routed working-tree generation an edit lease is
// about to withdraw in the reuse cache under its logical key and returns it,
// so the withdrawal owes it no retirement and the edit's build can stand on
// it. It returns 0 (and holds nothing) when the routed generation is not a
// servable layer rooted at the routed commit generation.
func (c *CheckoutCoordinator) holdWithdrawnDirty(ctx context.Context, route store_sqlite.CheckoutRoute) int64 {
	if c == nil || route.DirtyGenerationID <= 0 || route.CommitGenerationID <= 0 {
		return 0
	}
	row, found, err := c.catalog.GetViewGeneration(ctx, route.DirtyGenerationID)
	if err != nil || !found || !servableGeneration(row.State) {
		return 0
	}
	rooted, err := c.dirtyRootedAt(ctx, row, route.CommitGenerationID)
	if err != nil || !rooted {
		return 0
	}
	c.retainDirty(ctx, logicalDirtyKey(row, route.CommitGenerationID), row.GenerationID)
	return row.GenerationID
}

// deferFailedGeneration owes a failed build's generation a retirement: one the
// builder abandoned (failed) or refused at its pre-publish fence
// (superseded). A generation still building or ready is left alone: a
// coalesced follower's error says nothing about the leader's generation. The
// sweep retires nothing a route, a lease or a layer above still references.
func (c *CheckoutCoordinator) deferFailedGeneration(ctx context.Context, generationID int64) {
	if generationID <= 0 {
		return
	}
	lookup := context.WithoutCancel(ctx)
	row, found, err := c.catalog.GetViewGeneration(lookup, generationID)
	if err != nil || !found ||
		(row.State != store_sqlite.ViewGenerationFailed && row.State != store_sqlite.ViewGenerationSuperseded) {
		return
	}
	c.deferRetire(generationID, "failed working-tree build")
}

// checkoutForegroundActivity names the foreground work a compaction must not
// compete with, empty when there is none: an interactive build queued on the
// shared lane (any checkout's), or a refresh ticket or a demand wake waiting
// on this coordinator.
//
// A reader of the served view is not on the list. A query's materialized view
// leases the routed working-tree generation for as long as it runs, and the
// fold shares nothing with it: it reads the chain's immutable layers, writes a
// generation of its own, and flips the dirty slot with the route epoch's
// compare-and-set; the reader's lease keeps the generations it reads from
// retirement whatever the route then names. Yielding to readers starved the
// fold under ordinary use — an agent searches after every edit — until the
// chain reached its bound and the next edit built direct.
func (c *CheckoutCoordinator) checkoutForegroundActivity() string {
	if c.gate != nil && c.gate.Stats().InteractiveQueued > 0 {
		return "interactive_build"
	}
	if c.checkoutRefreshHighWater() != 0 {
		return "refresh_ticket"
	}
	if len(c.demand) > 0 {
		return "demand"
	}
	return ""
}

// awaitForegroundQuiet waits until the checkout has had no foreground
// activity for quiet, polling at dirtyChainCompactionYieldPoll. It returns
// ctx's error when ctx ends first.
func (c *CheckoutCoordinator) awaitForegroundQuiet(ctx context.Context, quiet time.Duration) error {
	if quiet <= 0 {
		return ctx.Err()
	}
	ticker := time.NewTicker(dirtyChainCompactionYieldPoll)
	defer ticker.Stop()
	quietSince := time.Now()
	if c.checkoutForegroundActivity() != "" {
		quietSince = time.Time{}
	}
	for {
		if !quietSince.IsZero() && time.Since(quietSince) >= quiet {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if c.checkoutForegroundActivity() != "" {
				quietSince = time.Time{}
			} else if quietSince.IsZero() {
				quietSince = time.Now()
			}
		}
	}
}

// yieldToForeground cancels one compaction attempt as soon as foreground
// activity appears (checkoutForegroundActivity) and records what it yielded
// to. stop ends the watch.
func (c *CheckoutCoordinator) yieldToForeground(ctx context.Context, cancel context.CancelFunc) (stop func(), yielded *atomic.Value) {
	yielded = &atomic.Value{}
	quit := make(chan struct{})
	var once sync.Once
	go func() {
		ticker := time.NewTicker(dirtyChainCompactionYieldPoll)
		defer ticker.Stop()
		for {
			select {
			case <-quit:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
				if reason := c.checkoutForegroundActivity(); reason != "" {
					yielded.Store(reason)
					cancel()
					return
				}
			}
		}
	}()
	return func() { once.Do(func() { close(quit) }) }, yielded
}

// compactDirtyChain is one compaction: the body the background goroutine runs,
// synchronous here so a caller that owns the schedule (a test, a harness
// measuring the compactor itself) can run it in place.
func (c *CheckoutCoordinator) compactDirtyChain(ctx context.Context, trigger CheckoutCycle) (report DirtyChainCompaction) {
	report = DirtyChainCompaction{
		Top: trigger.DirtyGenerationID, Commit: trigger.CommitGenerationID, ChainDepthBefore: trigger.DirtyChainDepth,
	}
	started := time.Now()
	record := DefaultPublicationPhases().BeginBackgroundCompaction(c.checkoutID, started)
	ctx = withPhaseRecord(ctx, record)
	defer func() {
		report.Duration = time.Since(started)
		if record != nil {
			record.SetGeneration(report.GenerationID)
			record.FinishBackgroundWork(report.Outcome == dirtyChainCompactionFlipped || report.Outcome == dirtyChainCompactionPreferred)
		}
		c.recordDirtyChainCompaction(report)
	}()
	c.compaction.mu.Lock()
	quiet := c.compaction.quiet
	c.compaction.mu.Unlock()
	quiet = compactionQuietFor(quiet)
	for attempt := 1; ; attempt++ {
		// Start only in a quiet interval: no reader on the served view, no
		// ticket or demand, no interactive build queued.
		waitStarted := time.Now()
		if err := c.awaitForegroundQuiet(ctx, quiet); err != nil {
			report.QuietWait += time.Since(waitStarted)
			report.Outcome, report.Canceled = dirtyChainCompactionCanceled, true
			return report
		}
		report.QuietWait += time.Since(waitStarted)
		report.Attempts = attempt
		yielded := c.compactDirtyChainOnce(ctx, trigger, &report)
		if yielded == "" {
			return report
		}
		report.YieldedTo = append(report.YieldedTo, yielded)
		if attempt >= dirtyChainCompactionAttempts || ctx.Err() != nil {
			report.Outcome, report.Canceled = dirtyChainCompactionCanceled, true
			return report
		}
		// Foreground work arrived mid-attempt; wait for the next quiet
		// interval and start over (nothing the attempt began was published).
		report.Outcome, report.Canceled, report.Err = "", false, nil
	}
}

// compactDirtyChainOnce is one compaction attempt. It returns the foreground
// activity it yielded to, empty when the attempt ran to an outcome (recorded
// in report) or was canceled by its caller.
func (c *CheckoutCoordinator) compactDirtyChainOnce(parent context.Context, trigger CheckoutCycle, out *DirtyChainCompaction) (yieldedTo string) {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	var yielded *atomic.Value
	defer func() {
		if yielded == nil || parent.Err() != nil {
			return
		}
		if reason, _ := yielded.Load().(string); reason != "" && out.Outcome == dirtyChainCompactionCanceled {
			yieldedTo = reason
		}
	}()
	report := out
	canceled := func() string {
		report.Outcome, report.Canceled = dirtyChainCompactionCanceled, true
		return ""
	}
	if ctx.Err() != nil {
		return canceled()
	}
	release, err := c.gate.AcquirePromotable(ctx, ViewBuildBackground, nil)
	if err != nil {
		if ctx.Err() != nil {
			return canceled()
		}
		report.Outcome, report.Err = dirtyChainCompactionFailed, err
		return ""
	}
	releaseLane := sync.OnceFunc(release)
	defer releaseLane()
	c.compaction.folding.Store(true)
	defer c.compaction.folding.Store(false)
	c.logger.Info("checkout coordinator: chain fold started",
		zap.String("checkout", c.checkoutID), zap.Int64("chain_top", trigger.DirtyGenerationID),
		zap.Int("chain_depth", trigger.DirtyChainDepth))
	defer c.gate.NoteHolder(ViewBuildLaneHolder{
		Kind: "dirty_chain_compaction", CheckoutID: c.checkoutID,
		Priority: viewBuildPriorityLabel(ViewBuildBackground), Generation: trigger.DirtyGenerationID,
	})()
	c.compaction.mu.Lock()
	c.compaction.stats.AttemptsStarted++
	c.compaction.mu.Unlock()
	markPublicationPhase(ctx, PublicationAdmitted)
	var stop func()
	if steppedChainFoldEnabled {
		// A stepped fold holds the lane only for its checks, and its steps
		// give way to an edit's writes: it does not yield to foreground work.
		stop, yielded = func() {}, &atomic.Value{}
	} else {
		stop, yielded = c.yieldToForeground(ctx, cancel)
	}
	defer stop()
	c.compaction.mu.Lock()
	barrier := c.compaction.barrier
	c.compaction.mu.Unlock()
	if barrier != nil {
		barrier(ctx)
	}
	if ctx.Err() != nil {
		return canceled()
	}

	route, found, err := c.catalog.GetCheckoutRoute(ctx, c.checkoutID)
	if err != nil {
		if ctx.Err() != nil {
			return canceled()
		}
		report.Outcome, report.Err = dirtyChainCompactionFailed, err
		return ""
	}
	if !found || route.State != store_sqlite.RouteActive || route.CommitGenerationID != trigger.CommitGenerationID ||
		route.DirtyGenerationID <= 0 {
		report.Outcome = dirtyChainCompactionNotNeeded
		return ""
	}
	depth := len(c.dirtyChainMembers(ctx, route.DirtyGenerationID))
	foldable := depth > 1 && c.chainCoveredPaths(ctx, route.DirtyGenerationID) > dirtyChainFoldPaths
	if depth < dirtyChainCompactionDepth && !foldable {
		// Neither at the depth bound nor an overlay large enough to fold.
		report.Outcome = dirtyChainCompactionNotNeeded
		return ""
	}
	commit, found, err := c.catalog.GetViewGeneration(ctx, route.CommitGenerationID)
	if err != nil || !found {
		if ctx.Err() != nil {
			return canceled()
		}
		report.Outcome, report.Err = dirtyChainCompactionFailed, err
		return ""
	}
	sample, err := c.sampler.Sample(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return canceled()
		}
		report.Outcome, report.Err = dirtyChainCompactionFailed, err
		return ""
	}
	if sample.HeadTree != commit.TreeOID {
		report.Outcome = dirtyChainCompactionHeadMoved
		return ""
	}
	clean := true
	for _, content := range sample.Contents {
		if !content.HeadEqual {
			clean = false
			break
		}
	}
	if clean {
		// A clean checkout routes to an empty direct generation on its next
		// cycle; there is no chain state to compact.
		report.Outcome = dirtyChainCompactionNotNeeded
		return ""
	}
	if steppedChainFoldEnabled {
		return c.compactDirtyChainStepped(ctx, trigger, commit, route, report, stop, releaseLane)
	}
	// The chain is folded by copy: no working-tree file is parsed, so the size
	// of the dirty set does not matter. A fold that does not reproduce the
	// chain is abandoned before it is published and the chain stays routed.
	flattenStarted := time.Now()
	built, err := c.flattenDirtyChain(ctx, commit, route.DirtyGenerationID)
	report.BuildDuration = time.Since(flattenStarted)
	switch {
	case err == nil:
		report.GenerationID = built.GenerationID
		if c.installCompactedDirty(ctx, trigger.CommitGenerationID, built) {
			report.Outcome = dirtyChainCompactionFlipped
		} else {
			report.Outcome = dirtyChainCompactionPreferred
		}
	case ctx.Err() != nil:
		return canceled()
	case errors.Is(err, errFlattenRefused):
		c.logger.Debug("checkout coordinator: chain fold refused; the chain stays routed",
			zap.String("checkout", c.checkoutID), zap.Error(err))
		report.Outcome, report.Err = dirtyChainCompactionFoldRefused, err
	default:
		report.Outcome, report.Err = dirtyChainCompactionFailed, err
	}
	return ""
}

// installCompactedDirty routes a compacted generation when the route still
// describes the working tree it compacted, and otherwise keeps it as the
// preferred parent. Either way it is filed in the reuse cache under its
// logical key, so it owes no retirement while it can still be used.
//
// The flip takes the cycle lock without waiting: a cycle holding it is about
// to move the route itself, and the compacted generation serves that cycle
// better as its parent than as a route it would have to CAS against.
func (c *CheckoutCoordinator) installCompactedDirty(ctx context.Context, commitGeneration int64, built dirtyLayerBuild) bool {
	row, found, err := c.catalog.GetViewGeneration(ctx, built.GenerationID)
	if err != nil || !found || !servableGeneration(row.State) {
		return false
	}
	flipped := false
	var previous int64
	if ctx.Err() == nil && c.cycleMu.TryLock() {
		func() {
			defer c.cycleMu.Unlock()
			route, found, err := c.catalog.GetCheckoutRoute(ctx, c.checkoutID)
			if err != nil || !found || route.State != store_sqlite.RouteActive ||
				route.CommitGenerationID != commitGeneration || route.DirtyGenerationID <= 0 ||
				route.DirtyGenerationID == built.GenerationID {
				return
			}
			top, found, err := c.catalog.GetViewGeneration(ctx, route.DirtyGenerationID)
			if err != nil || !found || !servableGeneration(top.State) || top.LowerViewFingerprint != row.LowerViewFingerprint {
				return
			}
			previous = route.DirtyGenerationID
			if err := c.flip(ctx, &route, store_sqlite.RouteSlotDirty, built.GenerationID); err != nil {
				return
			}
			markPublicationPhase(ctx, PublicationRouteFlipped)
			flipped = true
		}()
	}
	c.retainDirty(ctx, built.Key, built.GenerationID)
	if flipped {
		c.releaseDirtyChain(ctx, previous, built.GenerationID)
		return true
	}
	c.setPreferredDirtyParent(built.GenerationID, "compacted while the route moved on")
	return false
}

// recordDirtyChainCompaction counts, logs and reports one compaction.
func (c *CheckoutCoordinator) recordDirtyChainCompaction(report DirtyChainCompaction) {
	k := &c.compaction
	k.mu.Lock()
	switch report.Outcome {
	case dirtyChainCompactionFlipped:
		k.stats.Flipped++
	case dirtyChainCompactionPreferred:
		k.stats.Preferred++
	case dirtyChainCompactionCanceled:
		k.stats.Canceled++
	case dirtyChainCompactionNotNeeded, dirtyChainCompactionHeadMoved:
		k.stats.NotNeeded++
	case dirtyChainCompactionFoldRefused:
		k.stats.FoldRefused++
	default:
		k.stats.Failed++
	}
	k.stats.Duration += report.Duration
	done := k.done
	k.mu.Unlock()
	// Info, not Debug: how often the chain is folded, and whether the folds
	// keep up with the edits, is read off this line (each delta logs the
	// chain_depth it composed over).
	c.logger.Info("checkout coordinator: background chain compaction",
		zap.String("checkout", c.checkoutID),
		zap.String("outcome", report.Outcome),
		zap.Int64("chain_top", report.Top),
		zap.Int("chain_depth_before", report.ChainDepthBefore),
		zap.Int64("compacted_generation", report.GenerationID),
		zap.Bool("canceled", report.Canceled),
		zap.Duration("compaction_duration", report.Duration),
		zap.Error(report.Err))
	if done != nil {
		done(report)
	}
}

// dedicatedGenerationKind is the catalog kind of a dedicated graph's
// generations; a chain rooted at one does not compose over generation 0.
const dedicatedGenerationKind = "dedicated"

// checkoutLanguageCensus is the language census of the committed state a
// working-tree layer over commitGeneration composes over: the commit
// generation and its committed ancestry, each a generation-scoped grouped
// count, plus the base corpus (generation 0) when the view actually composes
// over it. The materializer stands a chain whose root is a full dedicated
// generation on that generation alone (graphview's assemble: base =
// firstHandle), so generation 0 is not part of such a view and is not
// counted; counting it paid a whole-repository grouped scan of the flat base
// per coordinator and commit generation for rows the view never serves. A
// chain whose root is not a dedicated generation, or whose walk did not reach
// its root, keeps the base count. It is cached per commit generation, which
// is immutable.
func (c *CheckoutCoordinator) checkoutLanguageCensus(ctx context.Context, commitGeneration int64) map[string]int {
	c.compaction.mu.Lock()
	if cached, ok := c.compaction.census[commitGeneration]; ok {
		c.compaction.mu.Unlock()
		return cached
	}
	c.compaction.mu.Unlock()
	started := time.Now()
	counted := 0
	census := map[string]int{}
	add := func(generationID int64, published bool) {
		counted++
		handle := c.store.AtGeneration(generationID)
		var counts map[string]int
		if published {
			// A ready generation's rows are immutable: its count is shared by
			// every checkout standing on it and paid once per process.
			counts = handle.PublishedRepoLanguageCounts(c.repoPrefix)
		} else {
			counts = handle.RepoLanguageCounts([]string{c.repoPrefix})[c.repoPrefix]
		}
		for language, count := range counts {
			census[language] += count
		}
	}
	dedicatedRoot := false
	seen := map[int64]bool{0: true}
	id := commitGeneration
	for depth := 0; id > 0 && !seen[id] && depth < graphview.MaxGenerationAncestryDepth; depth++ {
		seen[id] = true
		row, found, err := c.catalog.GetViewGeneration(ctx, id)
		if err != nil || !found {
			break
		}
		add(id, row.State == store_sqlite.ViewGenerationReady)
		if row.BaseGenerationID <= 0 {
			dedicatedRoot = row.GenerationKind == dedicatedGenerationKind
		}
		id = row.BaseGenerationID
	}
	if !dedicatedRoot {
		add(0, false)
	}
	c.compaction.mu.Lock()
	if c.compaction.census == nil {
		c.compaction.census = map[int64]map[string]int{}
	}
	if len(c.compaction.census) > 8 {
		clear(c.compaction.census)
	}
	c.compaction.census[commitGeneration] = census
	c.compaction.mu.Unlock()
	// The one cold cost of the census: a whole-generation count per level the
	// process has not counted yet (the published ones are memoized per
	// process), paid only by a build whose own files leave an enrichable
	// language below the admission floor.
	if c.logger == nil {
		return census
	}
	c.logger.Info("indexer: checkout language census counted",
		zap.String("checkout", c.checkoutID),
		zap.Int64("commit_generation", commitGeneration),
		zap.Int("generations", counted),
		zap.Bool("base_counted", !dedicatedRoot),
		zap.Duration("elapsed", time.Since(started)))
	return census
}

// cycleAdmission is how long a cycle waited before building, by stage: the
// settle check (the shared working-copy sample), the cycle lock, and the
// build lane; and what held the lane when the cycle queued for it (a Kind
// "undeclared" holder is a builder that declares nothing, nil an idle lane).
type cycleAdmission struct {
	Preflight  time.Duration
	CycleLock  time.Duration
	Lane       time.Duration
	LaneHeldBy *ViewBuildLaneHolder
}

// slowAdmission is the admission wait above which a cycle logs what it waited
// for.
const slowAdmission = 50 * time.Millisecond

// logSlowAdmission records, for a cycle whose admission took long enough to
// matter to an edit's latency, where the time went and who held the lane.
func (c *CheckoutCoordinator) logSlowAdmission(reason string, through uint64, admission cycleAdmission) {
	if admission.Preflight+admission.CycleLock+admission.Lane < slowAdmission {
		return
	}
	fields := []zap.Field{
		zap.String("checkout", c.checkoutID),
		zap.String("reason", reason),
		zap.Uint64("through_ticket", through),
		zap.Duration("preflight", admission.Preflight),
		zap.Duration("cycle_lock_wait", admission.CycleLock),
		zap.Duration("lane_wait", admission.Lane),
	}
	if holder := admission.LaneHeldBy; holder != nil {
		fields = append(fields,
			zap.String("lane_holder", holder.Kind),
			zap.String("lane_holder_checkout", holder.CheckoutID),
			zap.Int64("lane_holder_generation", holder.Generation),
			zap.String("lane_holder_priority", holder.Priority),
			zap.String("lane_holder_root", holder.Root),
			zap.String("lane_holder_reason", holder.Reason),
			zap.Duration("lane_holder_held_for", time.Since(holder.Since)))
	}
	c.logger.Info("checkout coordinator: slow build admission", fields...)
}

// publicationSourceObservedChange is the source of a record a cycle opens for
// a working-tree change no ticket named (a filesystem edit the poll or a
// signal found). It is the same string the phase recorder's vocabulary uses
// for it.
const publicationSourceObservedChange = "observed_change"

var observedChangeSequence atomic.Uint64

// openObservedChangeRecord opens the publication record of a change a
// ticketless cycle found: the cycle's shared sample (taken by its settle
// check) differs from what the routed working-tree generation describes. Its
// origin is when that sample's git status started, and change_observed is
// marked now, when the cycle has it. nil when nothing changed or the route
// cannot be read.
func (c *CheckoutCoordinator) openObservedChangeRecord(ctx context.Context) *PublicationPhaseRecord {
	sample, err := c.cycleSample(ctx)
	if err != nil || sample.Fingerprint == "" {
		return nil
	}
	route, found, err := c.catalog.GetCheckoutRoute(ctx, c.checkoutID)
	if err != nil {
		return nil
	}
	if found && route.DirtyGenerationID > 0 {
		row, found, err := c.catalog.GetViewGeneration(ctx, route.DirtyGenerationID)
		if err != nil || (found && row.LowerViewFingerprint == sample.Fingerprint) {
			return nil
		}
	}
	origin := c.sampler.LastSampleStarted()
	if origin.IsZero() {
		origin = time.Now()
	}
	key := "observed-" + strconv.FormatUint(observedChangeSequence.Add(1), 10)
	record := DefaultPublicationPhases().Begin(c.checkoutID, key, publicationSourceObservedChange, origin)
	record.Mark(PublicationChangeObserved)
	return record
}

// finishObservedChangeRecord closes a cycle's observed-change record with
// what the cycle published: completed when the route names a working-tree
// generation it built or re-routed, failed otherwise.
func finishObservedChangeRecord(ctx context.Context, out CheckoutCycle) {
	record := phaseRecordFrom(ctx)
	if record == nil {
		return
	}
	if out.Err == nil && out.DirtyGenerationID > 0 && (out.DirtyBuilt || out.DirtyReused) {
		record.SetGeneration(out.DirtyGenerationID)
		record.Mark(PublicationTicketCompleted)
		return
	}
	record.Mark(PublicationTicketFailed)
}
