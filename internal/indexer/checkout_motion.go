package indexer

import (
	"context"
	"errors"
	"os"
	"path"
	"path/filepath"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/gitstate"
	"github.com/zzet/gortex/internal/viewmetrics"
)

// Background cycles over a working tree that is still moving.
//
// A background cycle (no refresh ticket names its work: a filesystem change,
// the poll) that starts while the tree is being written builds a state that is
// gone before it can be published. Its sample is refused ("git dirty status
// changed while sampling"), or its build is torn at the prepublish fence, and
// either way it held the one build lane for seconds and published nothing.
// Three rules keep such cycles off the lane, all for background cycles only —
// an interactive ticket is admitted exactly as before:
//
//  1. Quiet admission. While the checkout's watcher keeps reporting changes,
//     the cycle is held (coalesced into the next one, not queued): it is
//     admitted only when no change was reported for a whole quiet window and
//     a re-stat of the reported paths agrees nothing moved since they were
//     reported (a change whose event has not been delivered yet).
//  2. Sample, verify, then the lane. The cycle takes its working-copy sample
//     before it queues for the lane; a sample the tree moved under is a held
//     cycle, not a failed build that waited for the lane first.
//  3. Abort on movement. Once admitted, a background build is abandoned the
//     moment the watcher reports a change to a path the build reads, up to
//     its commit point (the one reachBuildCommitPoint marks, shared with the
//     lane yield), and a build the prepublish fence refuses because the tree
//     moved is rescheduled rather than failed. Nothing it began is published.
//
// Starvation is bounded twice: a window re-armed continuously for longer than
// the coalesce cap fires anyway and is admitted past rule 1 (rule 2 still
// holds — git decides whether the tree is consistent), and once
// maxTreeMovedAborts aborts in a row turn out to have been for changes git
// does not see (the next sample has the aborted build's fingerprint), builds
// are no longer armed, so a path that changes forever but that git ignores
// cannot keep a checkout unbuilt.

const (
	// backgroundCoalesceCapFloor is the least time a burst of changes may keep
	// a background cycle held; the cap is ten quiet windows when that is
	// longer.
	backgroundCoalesceCapFloor = 3 * time.Second
	// maxTreeMovedAborts bounds consecutive movement aborts of one checkout's
	// background builds for changes git does not see; past it builds run to
	// their end until one does.
	maxTreeMovedAborts = 2
	// motionPathsBound bounds the reported paths kept for the admission
	// re-stat. Past it the re-stat is skipped and the quiet window alone
	// decides (the sample still verifies).
	motionPathsBound = 256
	// treeMovedReason is CheckoutCycle.YieldedTo's value for a background
	// build abandoned because the working tree moved under it.
	treeMovedReason = "working_tree_moved"
)

// checkoutMotion is what the checkout's watcher has reported since the last
// admitted background cycle. Its own lock: the watcher reports from its own
// goroutine while the loop runs a cycle.
type checkoutMotion struct {
	mu sync.Mutex
	// watch is the checkout's file watcher; nil when it has none, and then
	// none of the rules that read reported changes apply.
	watch *checkoutWatch
	// roots are the checkout root as configured and symlink-resolved, the
	// forms the watcher may report paths under.
	roots []string
	// lastEvent is when the watcher last reported a change; burstStart when
	// the current run of changes began (zero once a cycle was admitted).
	lastEvent  time.Time
	burstStart time.Time
	// paths are the reported paths and what they looked like when reported;
	// pathsLost when some changes were reported without paths or past the
	// bound.
	paths     map[string]motionStamp
	pathsLost bool
	// abort is the in-flight background build's movement abort, nil when
	// none is armed; armedFingerprint the working tree it was building.
	abort            *treeMoveAbort
	armedFingerprint string
	// abortedFingerprint is the working tree the last aborted build was
	// building, until the next admitted build compares its own sample with
	// it: the same fingerprint means the change that aborted it was one git
	// does not see (an ignored file), and counts toward maxTreeMovedAborts.
	abortedFingerprint string
	// aborts counts consecutive aborts whose change git did not see.
	aborts int
	// movedAborts counts background builds abandoned in a row because the
	// tree moved, for any reason; each one doubles the quiet time and the
	// coalesce cap the next admission needs (up to 8x), so a tree written
	// faster than it can be built is attempted less and less often instead
	// of at every quiet window. A background cycle that ends without an
	// error resets it (settleTreeMoveAborts); a failed one, or one whose two
	// builds the prepublish fence tore, leaves it.
	movedAborts int
}

// motionStamp is a path's identity for the re-stat: an atomic rename-over is a
// new file, a write moves the mtime or the size.
type motionStamp struct {
	missing bool
	info    os.FileInfo
}

func statMotion(p string) motionStamp {
	info, err := os.Lstat(p)
	if err != nil {
		return motionStamp{missing: true}
	}
	return motionStamp{info: info}
}

func (a motionStamp) same(b motionStamp) bool {
	if a.missing || b.missing {
		return a.missing == b.missing
	}
	return os.SameFile(a.info, b.info) && a.info.ModTime().Equal(b.info.ModTime()) &&
		a.info.Size() == b.info.Size() && a.info.Mode() == b.info.Mode()
}

// treeMoveAbort is one background build's movement abort. It reuses the lane
// yield's cancel/commit exclusion: a build past its commit point is never
// canceled from here.
type treeMoveAbort struct {
	y *backgroundLaneYield
	// dirty and dirs are the build's sampled dirty paths and their
	// directories (slash-separated, root-relative): the working-tree build
	// reads each changed file and the whole directory around it, so a change
	// there is one its prepublish fence would refuse.
	dirty map[string]struct{}
	dirs  map[string]struct{}
}

type treeMoveCommitKey struct{}

// buildInputManifests are the files a working-tree build reads at every read
// directory and its ancestors (build_read_set.go).
var buildInputManifests = map[string]struct{}{
	"go.mod": {}, "go.sum": {}, "go.work": {}, "go.work.sum": {},
	"package.json": {}, "tsconfig.json": {}, "jsconfig.json": {},
}

// touches reports whether a change at rel (root-relative, slash-separated)
// is one the build reads.
func (a *treeMoveAbort) touches(rel string) bool {
	if _, ok := a.dirty[rel]; ok {
		return true
	}
	if _, ok := a.dirs[path.Dir(rel)]; ok {
		return true
	}
	_, manifest := buildInputManifests[path.Base(rel)]
	return manifest
}

// attachFilesystemWatch hands the coordinator its checkout's watcher; the
// coordinator closes it with itself. A coordinator already closing closes it
// at once.
func (c *CheckoutCoordinator) attachFilesystemWatch(w *checkoutWatch) {
	if c == nil || w == nil {
		return
	}
	select {
	case <-c.lifetimeContext().Done():
		w.close()
		return
	default:
	}
	m := &c.motion
	m.mu.Lock()
	old := m.watch
	m.watch = w
	m.mu.Unlock()
	old.close()
	// CloseContext may have run between the check above and the store.
	if c.lifetimeContext().Err() != nil {
		c.closeFilesystemWatch()
	}
}

// closeFilesystemWatch stops the checkout's watcher, if it has one.
func (c *CheckoutCoordinator) closeFilesystemWatch() {
	m := &c.motion
	m.mu.Lock()
	w := m.watch
	m.watch = nil
	m.mu.Unlock()
	w.close()
}

// motionRoots returns the root forms reported paths are made relative to.
func (c *CheckoutCoordinator) motionRoots() []string {
	m := &c.motion
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.roots == nil {
		m.roots = []string{c.root}
		if resolved, err := filepath.EvalSymlinks(c.root); err == nil && resolved != c.root {
			m.roots = append(m.roots, resolved)
		}
	}
	return m.roots
}

// noteFilesystemChange is the watcher's report: paths changed (nil: unknown
// paths changed). It records the motion, aborts an armed background build the
// change touches, and wakes the loop through the quiet window. The wake claims
// nothing (signaledAt): the change is a hint, and every cycle samples after it
// starts anyway.
func (c *CheckoutCoordinator) noteFilesystemChange(paths []string) {
	if c == nil {
		return
	}
	roots := c.motionRoots()
	stamps := make(map[string]motionStamp, len(paths))
	rels := make([]string, 0, len(paths))
	for _, p := range paths {
		for _, root := range roots {
			if rel, ok := checkoutRelPath(root, p); ok && rel != "." {
				rels = append(rels, rel)
				if len(stamps) < motionPathsBound {
					stamps[p] = statMotion(p)
				}
				break
			}
		}
	}
	if paths != nil && len(rels) == 0 {
		return
	}
	now := time.Now()
	m := &c.motion
	m.mu.Lock()
	m.lastEvent = now
	if m.burstStart.IsZero() {
		m.burstStart = now
	}
	if paths == nil || len(stamps) < len(rels) {
		m.pathsLost = true
	}
	for p, stamp := range stamps {
		if m.paths == nil {
			m.paths = make(map[string]motionStamp)
		}
		if len(m.paths) >= motionPathsBound {
			m.pathsLost = true
			break
		}
		m.paths[p] = stamp
	}
	abort := m.abort
	touched := false
	if abort != nil {
		// Under the lock: a batched build narrows the paths it watches
		// (narrowTreeMoveAbort) while the watcher reports.
		touched = paths == nil
		for _, rel := range rels {
			if touched = abort.touches(rel); touched {
				break
			}
		}
	}
	m.mu.Unlock()
	if touched {
		abort.y.fire()
	}
	c.signalWindow("filesystem change", false)
}

// backgroundCoalesceCap is how long a burst may keep background cycles held.
func (c *CheckoutCoordinator) backgroundCoalesceCap() time.Duration {
	_, capped := c.backgroundAdmissionBounds()
	return capped
}

// backgroundAdmissionBounds is the quiet time a background cycle needs before
// it is admitted, and the coalesce cap past which it is admitted anyway: the
// quiet window and max(3 s, ten windows), each doubled per background build
// the moving tree has abandoned in a row (movedAborts, up to 8x).
func (c *CheckoutCoordinator) backgroundAdmissionBounds() (time.Duration, time.Duration) {
	m := &c.motion
	m.mu.Lock()
	backoff := min(m.movedAborts, 3)
	m.mu.Unlock()
	return c.quiet << backoff, max(backgroundCoalesceCapFloor, 10*c.quiet) << backoff
}

// workingTreeMovedWhileSampling reports whether err is a sample the working
// tree moved under (gitstate's sampling fences), as opposed to a checkout
// that cannot be sampled at all.
func workingTreeMovedWhileSampling(err error) bool {
	return err != nil && errors.Is(err, gitstate.ErrDirtyUnavailable) && errors.Is(err, gitstate.ErrDirtyMoved)
}

// heldBySample is the hold reason when the working-copy sample itself found the
// tree moving under it.
const heldBySample = "the working tree changed while it was sampled"

// holdBackgroundCycle decides, before a background cycle queues for anything,
// whether the working tree is settled enough to build. It reports the reason
// when it holds the cycle. It takes the cycle's shared sample (cycleSample):
// the preflight and the reconcile reuse it, so an admitted cycle pays nothing
// extra.
func (c *CheckoutCoordinator) holdBackgroundCycle(ctx context.Context) (string, bool) {
	m := &c.motion
	quiet, capped := c.backgroundAdmissionBounds()
	now := time.Now()
	m.mu.Lock()
	watched := m.watch != nil
	lastEvent := m.lastEvent
	forced := !m.burstStart.IsZero() && now.Sub(m.burstStart) >= capped
	var reported map[string]motionStamp
	if watched && !forced && !m.pathsLost && len(m.paths) > 0 {
		reported = make(map[string]motionStamp, len(m.paths))
		for p, stamp := range m.paths {
			reported[p] = stamp
		}
	}
	m.mu.Unlock()

	if watched && !lastEvent.IsZero() && !forced {
		if now.Sub(lastEvent) < quiet {
			return "the working tree changed within the quiet window", true
		}
		moved := make(map[string]motionStamp)
		for p, stamp := range reported {
			if current := statMotion(p); !current.same(stamp) {
				moved[p] = current
			}
		}
		if len(moved) > 0 {
			m.mu.Lock()
			m.lastEvent = time.Now()
			for p, stamp := range moved {
				m.paths[p] = stamp
			}
			m.mu.Unlock()
			return "a changed path moved again since it was reported", true
		}
	}
	sample := func(ctx context.Context) error {
		_, err := c.cycleSample(ctx)
		return err
	}
	if c.holdSample != nil {
		sample = c.holdSample
	}
	if err := sample(ctx); workingTreeMovedWhileSampling(err) {
		m.mu.Lock()
		m.lastEvent = time.Now()
		if m.burstStart.IsZero() {
			m.burstStart = m.lastEvent
		}
		m.mu.Unlock()
		return heldBySample, true
	}
	// Admitted: this cycle's sample postdates every change reported up to
	// the snapshot above, so those are its business now.
	m.mu.Lock()
	if !m.lastEvent.After(lastEvent) {
		m.burstStart = time.Time{}
		m.paths = nil
		m.pathsLost = false
	}
	m.mu.Unlock()
	return "", false
}

// heldBackgroundCycle is the outcome of a background cycle holdBackgroundCycle
// kept off the lane: nothing was built, the route is as it was, and the quiet
// window is re-armed so the settled tree still gets its cycle.
func (c *CheckoutCoordinator) heldBackgroundCycle(reason string) CheckoutCycle {
	viewmetrics.Count(viewmetrics.CoordinatorCycleTotal, viewmetrics.OutcomeRescheduled)
	// A hold decided from the watcher's reports costs nothing and recurs at
	// the quiet window's cadence while a tree is written, so it is logged at
	// Debug; a hold the working-copy sample decided (git saw the tree move)
	// is rare and logged at Info.
	log := c.logger.Debug
	if reason == heldBySample {
		log = c.logger.Info
	}
	log("checkout coordinator: background cycle held while the working tree is changing",
		zap.String("checkout", c.checkoutID), zap.String("held_because", reason))
	c.signalWindow("background cycle held: "+reason, false)
	return CheckoutCycle{Rescheduled: true, Held: true}
}

// armTreeMoveAbort arms the movement abort for an admitted background build.
// It returns ctx unchanged and nil when there is nothing to arm: no watcher,
// no sample for this cycle, or maxTreeMovedAborts in a row. A change reported
// after the cycle's sample began aborts the build before it starts.
func (c *CheckoutCoordinator) armTreeMoveAbort(ctx context.Context, cycleStarted time.Time) (context.Context, *backgroundLaneYield) {
	if c.sampler == nil {
		return ctx, nil
	}
	m := &c.motion
	m.mu.Lock()
	watched := m.watch != nil
	m.mu.Unlock()
	if !watched {
		return ctx, nil
	}
	sample, sampled, ok := c.sampler.LatestSampleSince(cycleStarted)
	if !ok {
		return ctx, nil
	}
	m.mu.Lock()
	if m.abortedFingerprint != "" {
		if m.abortedFingerprint == sample.Fingerprint {
			m.aborts++
		} else {
			m.aborts = 0
		}
		m.abortedFingerprint = ""
	}
	armable := m.aborts < maxTreeMovedAborts
	m.mu.Unlock()
	if !armable {
		return ctx, nil
	}
	abort := &treeMoveAbort{dirty: make(map[string]struct{}, len(sample.Entries)), dirs: make(map[string]struct{})}
	for _, entry := range sample.Entries {
		for _, p := range []string{entry.Path, entry.OldPath} {
			if p == "" {
				continue
			}
			abort.dirty[p] = struct{}{}
			abort.dirs[path.Dir(p)] = struct{}{}
		}
	}
	ctx, cancel := context.WithCancel(ctx)
	abort.y = &backgroundLaneYield{cancel: cancel, withdraw: func() {}, stop: make(chan struct{})}
	m.mu.Lock()
	m.abort = abort
	m.armedFingerprint = sample.Fingerprint
	movedSince := m.lastEvent.After(sampled)
	m.mu.Unlock()
	if movedSince {
		abort.y.fire()
	}
	return context.WithValue(ctx, treeMoveCommitKey{}, abort.y), abort.y
}

// narrowTreeMoveAbort restricts the armed movement abort to the paths the
// build actually builds (a batch of a large working tree): a write elsewhere
// in the dirty set does not touch what the batch generation claims, and the
// next batch reads it from the next sample. A write to one of these paths, to
// their directories or to a build manifest still aborts, and the prepublish
// fence still proves the batch's own reads.
func (c *CheckoutCoordinator) narrowTreeMoveAbort(paths []string) {
	if c == nil {
		return
	}
	m := &c.motion
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.abort == nil {
		return
	}
	dirty := make(map[string]struct{}, len(paths))
	dirs := make(map[string]struct{}, len(paths))
	for _, p := range paths {
		dirty[p] = struct{}{}
		dirs[path.Dir(p)] = struct{}{}
	}
	m.abort.dirty, m.abort.dirs = dirty, dirs
}

// disarmTreeMoveAbort ends the build's movement abort.
func (c *CheckoutCoordinator) disarmTreeMoveAbort(y *backgroundLaneYield) {
	if y == nil {
		return
	}
	m := &c.motion
	m.mu.Lock()
	if m.abort != nil && m.abort.y == y {
		m.abort = nil
	}
	m.mu.Unlock()
	y.close()
}

// commitTreeMoveAbort is the movement abort's side of the build commit point
// (reachBuildCommitPoint).
func commitTreeMoveAbort(ctx context.Context) {
	if y, _ := ctx.Value(treeMoveCommitKey{}).(*backgroundLaneYield); y != nil {
		y.commit()
	}
}

// settleTreeMoveAborts records an admitted background cycle that ended without
// an error: the run of aborts for changes git does not see is over. A cycle
// that failed, or whose two builds were torn, does not call it, so either
// keeps the backoff.
func (c *CheckoutCoordinator) settleTreeMoveAborts() {
	m := &c.motion
	m.mu.Lock()
	m.aborts = 0
	m.movedAborts = 0
	m.mu.Unlock()
}

// treeMovedCycle turns a background build that ended because the working tree
// moved — the watcher's abort, or the prepublish fence refusing a moved tree
// — into a rescheduled cycle: nothing it began was published, the route is as
// it found it, and the quiet window decides when the next one runs. A cycle the
// lifetime canceled stays an error.
func (c *CheckoutCoordinator) treeMovedCycle(out CheckoutCycle, admission cycleAdmission, detectedBy string) CheckoutCycle {
	if c.lifetimeContext().Err() != nil {
		return out
	}
	m := &c.motion
	m.mu.Lock()
	if detectedBy == "watcher" {
		m.abortedFingerprint = m.armedFingerprint
	}
	m.movedAborts++
	aborts, moved := m.aborts, m.movedAborts
	m.mu.Unlock()
	if detectedBy == "watcher" {
		c.noteBuildMotion()
	}
	cause := out.Err
	out.Err = nil
	out.Rescheduled = true
	out.YieldedTo = treeMovedReason
	out.Admission = admission
	viewmetrics.Count(viewmetrics.CoordinatorCycleTotal, viewmetrics.OutcomeRescheduled)
	c.logger.Info("checkout coordinator: background build abandoned because the working tree moved",
		zap.String("checkout", c.checkoutID),
		zap.String("detected_by", detectedBy),
		zap.Int("unseen_aborts", aborts),
		zap.Int("aborts_in_a_row", moved),
		zap.Int("max_unseen_aborts", maxTreeMovedAborts),
		zap.Bool("dirty_built", out.DirtyBuilt),
		zap.Bool("commit_built", out.CommitBuilt),
		zap.Duration("lane_wait", admission.Lane),
		zap.NamedError("cause", cause))
	c.signalWindow("background build abandoned: the working tree moved", false)
	return out
}

// sampleMovedTicketCycle turns a ticket's cycle whose own working-copy sample
// the tree moved under (a sample or prepublish fence of the cycle reporting
// gitstate.ErrDirtyMoved) into a rescheduled one. Such a sample says nothing
// either way: the cycle published nothing, and failing the tickets it owes
// would end their waits as a publication error over a save. They keep
// waiting (completeCheckoutRefreshTickets leaves a rescheduled cycle's
// tickets), and the next cycle runs at once, without the background backoff
// — a ticket is a caller waiting. A cycle the lifetime canceled stays an
// error.
func (c *CheckoutCoordinator) sampleMovedTicketCycle(out CheckoutCycle, admission cycleAdmission) CheckoutCycle {
	if c.lifetimeContext().Err() != nil {
		return out
	}
	cause := out.Err
	out.Err = nil
	out.Rescheduled = true
	out.YieldedTo = treeMovedReason
	out.Admission = admission
	viewmetrics.Count(viewmetrics.CoordinatorCycleTotal, viewmetrics.OutcomeRescheduled)
	c.logger.Info("checkout coordinator: build for a refresh ticket abandoned because the working tree moved while it was sampled",
		zap.String("checkout", c.checkoutID),
		zap.Bool("dirty_built", out.DirtyBuilt),
		zap.Bool("commit_built", out.CommitBuilt),
		zap.NamedError("cause", cause))
	c.SignalDemand("the working tree moved while a refresh ticket's cycle sampled it")
	return out
}

// errRefreshTicketArrived is the cause a background cycle's pre-build context
// is canceled with when a refresh ticket of the same checkout arrives.
var errRefreshTicketArrived = errors.New("indexer: a refresh ticket arrived for this checkout")

// backgroundDemandPoll is how often a background cycle, before it queues for
// the build lane, checks whether a refresh ticket of its checkout arrived.
const backgroundDemandPoll = 5 * time.Millisecond

// preemptBackgroundOnDemand derives the context a background cycle's pre-build
// steps run under — the hold's sample, the preflight, the wait for the cycle
// lock — canceled the moment a refresh ticket of this checkout arrives. The
// loop runs one cycle at a time, so without it a ticket (an edit someone is
// waiting on) would wait behind a background cycle of the same checkout that
// is still sampling or waiting for the lock; with it the background cycle
// steps aside and the ticket's cycle, which covers the same working tree,
// runs next. stop ends the watch and reports whether the ticket preempted the
// cycle; it must be called exactly once. Once a background cycle has queued
// for the lane, a ticket promotes it instead (PrioritizeSelection) and rides
// its publication.
func (c *CheckoutCoordinator) preemptBackgroundOnDemand(ctx context.Context) (context.Context, func() bool) {
	ctx, cancel := context.WithCancelCause(ctx)
	done := make(chan struct{})
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		ticker := time.NewTicker(backgroundDemandPoll)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
				if c.checkoutRefreshHighWater() != 0 {
					cancel(errRefreshTicketArrived)
					return
				}
			}
		}
	}()
	return ctx, func() bool {
		close(done)
		<-finished
		preempted := errors.Is(context.Cause(ctx), errRefreshTicketArrived)
		cancel(nil)
		return preempted
	}
}

// ticketPreemptedCycle is the outcome of a background cycle a refresh ticket
// preempted before it queued: it built nothing and is rescheduled; the demand
// the ticket raised runs the next cycle.
func (c *CheckoutCoordinator) ticketPreemptedCycle() CheckoutCycle {
	viewmetrics.Count(viewmetrics.CoordinatorCycleTotal, viewmetrics.OutcomeRescheduled)
	c.logger.Debug("checkout coordinator: background cycle stepped aside for a refresh ticket",
		zap.String("checkout", c.checkoutID))
	return CheckoutCycle{Rescheduled: true, Held: true, YieldedTo: "refresh_ticket"}
}
