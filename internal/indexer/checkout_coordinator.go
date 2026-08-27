package indexer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/gitstate"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/graphview"
	"github.com/zzet/gortex/internal/search/trigram"
	"github.com/zzet/gortex/internal/viewmetrics"
)

// The per-checkout coordinator.
//
// One instance runs per automatic checkout — a worktree of a family whose
// primary checkout owns the base corpus. It keeps that checkout's two routed
// generations in step with what is on its disk: the commit layer, which turns
// the corpus at the primary's tree into the corpus at the checkout's HEAD tree,
// and the dirty layer, which turns that into what the working tree holds.
//
// The loop is signal -> quiet window -> sample -> reconcile, and every part of
// it exists for a reason a simpler shape does not cover:
//
//   - The quiet window coalesces a burst. A branch switch is thousands of
//     filesystem events and one state change; building per event would build
//     thousands of generations of which one describes the checkout.
//
//   - The sample is taken after the window closes, not when the signal
//     arrived. What the coordinator has to reconcile to is the state the
//     checkout is in now; a signal is only the claim that it moved.
//
//   - Each slot is reconciled against the identity of what it is routed to,
//     not against a memory of what was last built. A commit layer's identity is
//     the tree it targets over the base it sits on, so a route that already
//     names a generation with that identity needs no work at all, and a
//     generation built earlier for the same identity can be re-routed without
//     rebuilding. That is the branch-switch cache: A -> B -> A re-routes A's
//     generation instead of re-indexing A's tree.
//
//   - Every flip is a compare-and-set on the route's epoch. Losing it means
//     another actor moved the route; the coordinator supersedes what it built
//     and reschedules rather than forcing its own answer over the winner's.
const (
	// defaultCheckoutQuietWindow is the debounce a coordinator takes when its
	// configuration names none. It matches the watcher's own debounce band:
	// long enough to swallow an editor's save burst and a branch switch's
	// event storm, short enough that a view is current before the next query.
	defaultCheckoutQuietWindow = 300 * time.Millisecond

	// defaultCheckoutPollInterval is how often a coordinator signals itself.
	// It is the only signal source an automatic checkout has today (see
	// CheckoutCoordinatorConfig.PollInterval), so it bounds how stale a
	// worktree's view can be.
	defaultCheckoutPollInterval = 15 * time.Second

	// defaultRetainedCommitLayers bounds how many commit generations one
	// coordinator keeps for re-routing. Each retained generation costs the
	// storage of one branch's difference from the base; four covers the
	// branches an agent switches between in a session without turning the
	// cache into an unbounded ledger of every branch ever visited.
	defaultRetainedCommitLayers = 4

	// checkoutLayerOwnerKind names who owns the generations a coordinator
	// builds: the family's primary dedicated graph, whose corpus they compose
	// over and whose repo prefix their payload is stamped with.
	checkoutLayerOwnerKind = "dedicated_graph"

	// checkoutResolverVersion stamps the resolution contract a coordinator's
	// generations were built under. Raising it makes every cached commit layer
	// miss, which is what a change in what resolution emits requires — the
	// stored payload is not what this binary would produce any more.
	checkoutResolverVersion = "1"
)

// errRouteMoved reports that a route flip lost its compare-and-set: another
// actor repointed the route between the epoch this cycle read and the flip it
// attempted. The cycle stops and reschedules; it never re-reads the epoch and
// flips again, because that would be forcing its answer over the winner's.
var errRouteMoved = errors.New("indexer: the checkout route moved under this coordinator")

// errCheckoutUnsettled reports that two working-tree builds in a row were torn
// by edits landing under them. A cycle answers it by rescheduling; a caller
// driving a transition has to decide whether to wait for the checkout to go
// quiet or to give up.
var errCheckoutUnsettled = errors.New("indexer: the working tree moved under two builds")

// CheckoutCoordinatorConfig is what one coordinator needs to serve one
// automatic checkout.
type CheckoutCoordinatorConfig struct {
	// CheckoutID is the catalog identity of the checkout, and the key its
	// route row is stored under.
	CheckoutID string
	// CheckoutRoot is the working tree the coordinator samples and builds from.
	CheckoutRoot string
	// FamilyID is the family whose primary dedicated graph the checkout's
	// layers sit on. The primary is re-read every cycle rather than captured,
	// so a primary that moves — a new commit on the primary checkout, a
	// promotion — is picked up on the next pass.
	FamilyID string
	// GraphID pins this coordinator to one logical base graph. Automatic
	// checkouts receive the current family primary; dedicated checkouts receive
	// their own graph. Empty preserves the legacy family-primary lookup.
	GraphID string

	// RepoPrefix, WorkspaceID and ProjectID are stamped onto the payload. They
	// are the PRIMARY's, not the checkout's: the layers compose over the
	// primary's corpus, so their nodes have to live in the same namespace.
	RepoPrefix  string
	WorkspaceID string
	ProjectID   string

	// Store is any handle on the database. Generations are begun, published,
	// routed and retired through it.
	Store *store_sqlite.Store
	// Builder builds the sparse generations. It must carry the index
	// configuration the base corpus was indexed with.
	Builder *SparseGenerationBuilder
	// Leases is the lease manager the materializer pins routed generations
	// with. Retirement consults it, so a generation under a live view is
	// refused rather than swept. nil means nothing leases generations.
	Leases *graphview.LeaseManager
	// Config is the index configuration the generations are built under. Its
	// digest is part of every generation's identity, so a configuration change
	// invalidates the cache instead of composing two payloads built under
	// different rules.
	Config config.IndexConfig
	Logger *zap.Logger
	// Gate holds the loop's build cycles while the daemon warms up. nil admits
	// every cycle at once, which is what a coordinator outside a warmup has.
	Gate *ViewBuildGate

	// Debounce is the quiet window; <= 0 takes defaultCheckoutQuietWindow.
	Debounce time.Duration
	// PollInterval is how often the coordinator signals itself; < 0 disables
	// the self-signal and 0 takes defaultCheckoutPollInterval.
	PollInterval time.Duration
	// Retain bounds the commit generations kept for re-routing; <= 0 takes
	// defaultRetainedCommitLayers.
	Retain int

	// cycleDone is a test seam: it runs at the end of every reconcile cycle
	// with what that cycle did. nil in production.
	cycleDone func(CheckoutCycle)
	// dirtyBarrier is a test seam handed to the dirty build: it runs inside
	// the window between a payload being complete and the checkout being
	// re-sampled, which is the window the supersede rule exists to close.
	// nil in production.
	dirtyBarrier func()
}

// CheckoutCycle is what one reconcile pass did. Every field is a decision, not
// a metric: a test asserting that a branch switch back re-routed instead of
// rebuilding reads CommitReused, and one asserting that a torn build left the
// route alone reads Rescheduled.
type CheckoutCycle struct {
	// CommitGenerationID and DirtyGenerationID are what the checkout is routed
	// to when the cycle ends, 0 for a slot the cycle could not settle.
	CommitGenerationID int64
	DirtyGenerationID  int64
	// CommitBuilt / DirtyBuilt report that the cycle ran a build for that slot.
	CommitBuilt bool
	DirtyBuilt  bool
	// CommitReused reports that the commit slot was pointed at a generation
	// built by an earlier cycle — the branch-switch cache hit.
	CommitReused bool
	// Rescheduled reports that the cycle stopped short and signalled itself:
	// a lost route flip, or a working tree that moved under two builds in a
	// row. The route is left exactly as the cycle found it.
	Rescheduled bool
	// Deferred reports that the cycle never ran: the warmup gate is holding
	// build work, and the loop will run this cycle when it opens. Nothing was
	// read and nothing was written, so every other field is zero.
	Deferred bool
	// Err is what stopped the cycle, nil when it settled both slots.
	Err error
}

// CheckoutCoordinator keeps one automatic checkout's routed view in step with
// its disk. It is created by the checkout lifecycle when a family's
// reconciliation reports an accessible automatic checkout, and closed when
// that checkout leaves.
type CheckoutCoordinator struct {
	checkoutID  string
	root        string
	familyID    string
	graphID     string
	repoPrefix  string
	workspaceID string
	projectID   string

	store   *store_sqlite.Store
	catalog *store_sqlite.Catalog
	builder *SparseGenerationBuilder
	leases  *graphview.LeaseManager
	logger  *zap.Logger
	gate    *ViewBuildGate

	quiet      time.Duration
	poll       time.Duration
	retain     int
	configHash string
	extractors string

	// signal carries a wake to the run loop. It is buffered to one: a burst of
	// signals has exactly one thing to say, and the loop re-arms the quiet
	// window once per wake it reads.
	signal chan struct{}
	stop   chan struct{}
	done   chan struct{}
	once   sync.Once

	// lifetime is canceled before Close waits. Every loop-owned admission and
	// build derives from it, so removing a checkout cannot sit behind an
	// unrelated generation for minutes.
	lifetime       context.Context
	cancelLifetime context.CancelFunc

	// cycleMu serializes the loop's own cycles against a caller-driven
	// transition. Both move this checkout's route, and a transition that
	// builds off-route only keeps its promise if the loop cannot flip the
	// route to a half-built state while it is building.
	cycleMu sync.Mutex

	mu sync.Mutex
	// retained is the commit-layer reuse cache, most recently routed first.
	retained []retainedCommitLayer
	// backlog holds generations a retire refused. The janitor retries them.
	backlog map[int64]struct{}
	// routedDirty is the working-tree generation the route names. The reuse
	// cache already remembers the commit half; this is the other one, and
	// together they are the only record of a checkout's payload once its route
	// row has been withdrawn — see DrainRetirements.
	routedDirty int64
	// reason is the last signal's reason, carried into the cycle's logging so
	// a build can be traced back to what asked for it.
	reason string
	// dirtyFingerprint is the working tree the last cycle sampled. It is what
	// the checkout's text searcher is keyed by — see checkout_text_search.go.
	dirtyFingerprint string

	// textMu guards the checkout's own trigram searcher and is held across the
	// build, so concurrent searches on one checkout pay for one index.
	textMu    sync.Mutex
	textIndex *trigram.Searcher
	textKey   string

	cycleDone    func(CheckoutCycle)
	dirtyBarrier func()

	// cyclePreflight and cycleBarrier are focused test seams. Production uses
	// settledWithoutBuild and has no barrier.
	cyclePreflight func(context.Context) (CheckoutCycle, bool)
	cycleBarrier   func(context.Context)
}

// retainedCommitLayer is one commit generation kept for re-routing, keyed by
// the build identity that produced it.
type retainedCommitLayer struct {
	key          string
	generationID int64
}

// primaryBase is the family's primary dedicated graph and the state the base
// corpus underneath every layer is at.
type primaryBase struct {
	// graphID is the primary dedicated graph. It is what the route names and
	// what the materializer resolves the repo prefix from.
	graphID string
	// generationID is the primary's published generation, 0 when the corpus
	// itself is the base — which is what a plainly indexed primary checkout
	// looks like today.
	generationID int64
	// treeOID is the committed tree the base corpus holds. It is the left-hand
	// side of the commit layer's diff.
	treeOID string
}

// NewCheckoutCoordinator builds a coordinator and starts its loop. The caller
// owns it and must Close it; a coordinator that is not closed keeps a
// goroutine and a timer alive.
func NewCheckoutCoordinator(cfg CheckoutCoordinatorConfig) (*CheckoutCoordinator, error) {
	switch {
	case cfg.CheckoutID == "":
		return nil, errors.New("indexer: checkout coordinator needs a checkout id")
	case cfg.CheckoutRoot == "":
		return nil, errors.New("indexer: checkout coordinator needs a checkout root")
	case cfg.FamilyID == "":
		return nil, errors.New("indexer: checkout coordinator needs a family id")
	case cfg.Store == nil:
		return nil, errors.New("indexer: checkout coordinator needs a store")
	case cfg.Builder == nil:
		return nil, errors.New("indexer: checkout coordinator needs a generation builder")
	}
	logger := cfg.Logger
	if logger == nil {
		logger = zap.NewNop()
	}
	lifetime, cancelLifetime := context.WithCancel(context.Background())
	c := &CheckoutCoordinator{
		checkoutID:     cfg.CheckoutID,
		root:           cfg.CheckoutRoot,
		familyID:       cfg.FamilyID,
		graphID:        cfg.GraphID,
		repoPrefix:     cfg.RepoPrefix,
		workspaceID:    cfg.WorkspaceID,
		projectID:      cfg.ProjectID,
		store:          cfg.Store,
		catalog:        cfg.Store.Catalog(),
		builder:        cfg.Builder,
		leases:         cfg.Leases,
		logger:         logger,
		gate:           cfg.Gate,
		quiet:          cfg.Debounce,
		poll:           cfg.PollInterval,
		retain:         cfg.Retain,
		configHash:     indexConfigHash(cfg.Config),
		extractors:     extractorVersionsFingerprint(),
		signal:         make(chan struct{}, 1),
		stop:           make(chan struct{}),
		done:           make(chan struct{}),
		lifetime:       lifetime,
		cancelLifetime: cancelLifetime,
		backlog:        map[int64]struct{}{},
		cycleDone:      cfg.cycleDone,
		dirtyBarrier:   cfg.dirtyBarrier,
	}
	if c.quiet <= 0 {
		c.quiet = defaultCheckoutQuietWindow
	}
	if c.poll == 0 {
		c.poll = defaultCheckoutPollInterval
	}
	if c.retain <= 0 {
		c.retain = defaultRetainedCommitLayers
	}
	go c.run()
	return c, nil
}

// Signal marks the checkout dirty. Any caller may signal, as often as it
// likes: the quiet window coalesces a burst into one reconcile cycle, and a
// signal that arrives while a cycle is running schedules the next one rather
// than being dropped.
func (c *CheckoutCoordinator) Signal(reason string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	c.reason = reason
	c.mu.Unlock()
	select {
	case c.signal <- struct{}{}:
	default:
	}
}

// Close stops the loop and waits for cooperative cancellation to finish.
func (c *CheckoutCoordinator) Close() error {
	return c.CloseContext(context.Background())
}

// CloseContext cancels queued admission and in-flight cooperative work before
// waiting. The cancellation is permanent even when the caller's wait deadline
// expires; a later CloseContext may wait for the same shutdown to finish.
func (c *CheckoutCoordinator) CloseContext(ctx context.Context) error {
	if c == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	c.once.Do(func() {
		if c.cancelLifetime != nil {
			c.cancelLifetime()
		}
		if c.stop != nil {
			close(c.stop)
		}
	})
	select {
	case <-c.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Running reports whether the loop goroutine is still there.
//
// It is what the administrative surfaces mean by a live coordinator, and it is
// not the same question as whether the lifecycle's registry holds one: a
// coordinator built for a transition runs a whole rebuild before anything
// registers it, and one being dropped keeps running until its in-flight cycle
// ends.
func (c *CheckoutCoordinator) Running() bool {
	if c == nil {
		return false
	}
	select {
	case <-c.done:
		return false
	default:
		return true
	}
}

// run is the loop. It owns the quiet-window timer and the poll ticker, so
// there is exactly one goroutine per coordinator and closing it stops
// everything it armed.
//
// The warmup gate is a fourth wake. A window that closes while builds are
// deferred spends the claim it was armed for — the checkout is dirty and
// nothing is going to say so again, because the signal that said it has been
// consumed — so the loop remembers the claim and runs it when the gate opens.
// Waiting for the gate inside the window instead would hold a wake the loop
// still has to answer for stop and for signals.
func (c *CheckoutCoordinator) run() {
	defer close(c.done)
	defer c.releaseTextSearcher()
	lifetime := c.lifetimeContext()

	quiet := time.NewTimer(c.quiet)
	stopTimer(quiet)
	defer stopTimer(quiet)

	var pollC <-chan time.Time
	if c.poll > 0 {
		ticker := time.NewTicker(c.poll)
		defer ticker.Stop()
		pollC = ticker.C
	}

	// nil once builds are admitted: an open gate's channel is always ready,
	// and selecting on it would spin the loop.
	admitted := c.admissionWait()
	claimed := false
	var armed <-chan time.Time
	for {
		select {
		case <-c.stop:
			return
		case <-lifetime.Done():
			return
		case <-c.signal:
			// Re-arm on every signal: the window is quiet time since the LAST
			// claim that the checkout moved, not since the first.
			stopTimer(quiet)
			quiet.Reset(c.quiet)
			armed = quiet.C
		case <-pollC:
			c.Signal("poll")
		case <-armed:
			armed = nil
			if admitted != nil {
				claimed = true
				c.deferCycle()
				continue
			}
			c.cycle(lifetime)
		case <-admitted:
			admitted = nil
			if !claimed {
				continue
			}
			claimed = false
			c.cycle(lifetime)
		}
	}
}

// admissionWait is the channel the loop waits for build admission on, nil when
// builds are already admitted.
func (c *CheckoutCoordinator) admissionWait() <-chan struct{} {
	if c.gate.Admitted() {
		return nil
	}
	return c.gate.Opened()
}

// deferCycle records a cycle the warmup gate held back. Nothing is read and
// nothing is written: the route keeps serving what the last run published, and
// the claim this window spent is carried to the gate's own wake.
func (c *CheckoutCoordinator) deferCycle() {
	c.mu.Lock()
	reason := c.reason
	c.mu.Unlock()
	viewmetrics.Count(viewmetrics.CoordinatorCycleTotal, viewmetrics.OutcomeDeferred)
	c.logger.Debug("checkout coordinator: build deferred until the daemon has warmed up",
		zap.String("checkout", c.checkoutID), zap.String("reason", reason))
	if c.cycleDone != nil {
		c.cycleDone(CheckoutCycle{Deferred: true})
	}
}

// stopTimer stops a timer and drains a callback that already fired, so a
// following Reset arms a window that is empty.
func stopTimer(t *time.Timer) {
	if !t.Stop() {
		select {
		case <-t.C:
		default:
		}
	}
}

// cycle runs one reconcile pass and reports it.
func (c *CheckoutCoordinator) cycle(ctx context.Context) {
	if ctx == nil {
		ctx = context.Background()
	}
	c.mu.Lock()
	reason := c.reason
	c.mu.Unlock()

	preflight := c.settledWithoutBuild
	if c.cyclePreflight != nil {
		preflight = c.cyclePreflight
	}
	if out, settled := preflight(ctx); settled {
		recordCoordinatorCycle(out)
		if c.cycleDone != nil {
			c.cycleDone(out)
		}
		return
	}

	release, err := c.gate.Acquire(ctx, ViewBuildBackground)
	if err != nil {
		out := CheckoutCycle{Err: fmt.Errorf("indexer: wait for checkout build admission: %w", err)}
		recordCoordinatorCycle(out)
		if c.cycleDone != nil {
			c.cycleDone(out)
		}
		return
	}
	defer release()

	c.cycleMu.Lock()
	defer c.cycleMu.Unlock()
	if c.cycleBarrier != nil {
		c.cycleBarrier(ctx)
	}
	if err := ctx.Err(); err != nil {
		out := CheckoutCycle{Err: err}
		recordCoordinatorCycle(out)
		if c.cycleDone != nil {
			c.cycleDone(out)
		}
		return
	}
	out := c.reconcile(ctx)
	recordCoordinatorCycle(out)
	switch {
	case out.Err != nil && !errors.Is(out.Err, context.Canceled):
		c.logger.Warn("checkout coordinator: reconcile failed",
			zap.String("checkout", c.checkoutID), zap.String("root", c.root),
			zap.String("reason", reason), zap.Error(out.Err))
	case out.CommitBuilt || out.DirtyBuilt || out.CommitReused:
		c.logger.Debug("checkout coordinator: route updated",
			zap.String("checkout", c.checkoutID), zap.String("reason", reason),
			zap.Int64("commit_generation", out.CommitGenerationID),
			zap.Int64("dirty_generation", out.DirtyGenerationID),
			zap.Bool("commit_reused", out.CommitReused))
	}
	if c.cycleDone != nil {
		c.cycleDone(out)
	}
}

// settledWithoutBuild recognizes the overwhelmingly common poll result before
// it queues for the one build lane. It performs the same identity and dirty
// fingerprint checks as reconcile, but makes no catalog route change.
func (c *CheckoutCoordinator) settledWithoutBuild(ctx context.Context) (CheckoutCycle, bool) {
	var out CheckoutCycle
	if err := ctx.Err(); err != nil {
		return out, false
	}
	base, err := c.primaryBase(ctx)
	if err != nil {
		return out, false
	}
	sample, err := gitstate.SampleDirty(ctx, c.root)
	if err != nil || sample.HeadTree == "" {
		return out, false
	}
	route, found, err := c.catalog.GetCheckoutRoute(ctx, c.checkoutID)
	if err != nil || !found || route.State != store_sqlite.RouteActive ||
		route.GraphID != base.graphID || route.CommitGenerationID <= 0 || route.DirtyGenerationID <= 0 {
		return out, false
	}
	commit, found, err := c.catalog.GetViewGeneration(ctx, route.CommitGenerationID)
	if err != nil || !found || !servableGeneration(commit.State) ||
		generationRowKey(commit) != generationIdentityKey(c.commitIdentity(base, sample.HeadTree)) {
		return out, false
	}
	dirty, found, err := c.catalog.GetViewGeneration(ctx, route.DirtyGenerationID)
	if err != nil || !found || !servableGeneration(dirty.State) ||
		dirty.BaseGenerationID != commit.GenerationID || dirty.LowerViewFingerprint != sample.Fingerprint {
		return out, false
	}

	c.noteDirtyFingerprint(sample.Fingerprint)
	c.retainCommit(ctx, generationIdentityKey(c.commitIdentity(base, sample.HeadTree)), commit.GenerationID)
	c.rememberRoutedDirty(dirty.GenerationID)
	out.CommitGenerationID = commit.GenerationID
	out.DirtyGenerationID = dirty.GenerationID
	return out, true
}

func (c *CheckoutCoordinator) lifetimeContext() context.Context {
	if c != nil && c.lifetime != nil {
		return c.lifetime
	}
	return context.Background()
}

// recordCoordinatorCycle counts what one cycle did.
//
// A cycle can settle both slots, so it can carry more than one outcome; the
// counter reads as "how often did a cycle do this", not as a partition of the
// cycles. The rescheduled case is deliberately silent here — a lost route flip,
// a working tree that moved under two builds and a checkout that committed
// under the cycle share the field but are different failures, so each is
// counted where it is decided.
func recordCoordinatorCycle(out CheckoutCycle) {
	switch {
	case out.Err != nil:
		viewmetrics.Count(viewmetrics.CoordinatorCycleTotal, viewmetrics.OutcomeFailed)
	case out.Rescheduled:
	case out.CommitBuilt || out.CommitReused || out.DirtyBuilt:
		if out.CommitBuilt {
			viewmetrics.Count(viewmetrics.CoordinatorCycleTotal, viewmetrics.OutcomeBuiltCommit)
		}
		if out.CommitReused {
			viewmetrics.Count(viewmetrics.CoordinatorCycleTotal, viewmetrics.OutcomeAdoptedCommit)
		}
		if out.DirtyBuilt {
			viewmetrics.Count(viewmetrics.CoordinatorCycleTotal, viewmetrics.OutcomeBuiltDirty)
		}
	default:
		viewmetrics.Count(viewmetrics.CoordinatorCycleTotal, viewmetrics.OutcomeSkipped)
	}
}

// reconcile brings the checkout's route in line with the state its disk is in
// right now.
func (c *CheckoutCoordinator) reconcile(ctx context.Context) CheckoutCycle {
	var out CheckoutCycle

	base, err := c.primaryBase(ctx)
	if err != nil {
		out.Err = err
		return out
	}
	head, err := gitstate.SampleHEAD(ctx, c.root)
	if err != nil {
		out.Err = fmt.Errorf("indexer: sample HEAD of %s: %w", c.root, err)
		return out
	}
	if head.TreeOID == "" {
		// An unborn branch has no tree to build a commit layer from, and a
		// checkout with no commit generation has no view. There is nothing to
		// reconcile to until it has one commit.
		out.Err = fmt.Errorf("indexer: checkout %s has no HEAD tree", c.root)
		return out
	}

	route, err := c.ensureRoute(ctx, base)
	if err != nil {
		if errors.Is(err, errRouteMoved) {
			out.Rescheduled = true
			c.rescheduleOnLostRoute("route moved under the graph reset")
			return out
		}
		out.Err = err
		return out
	}

	commitGeneration, err := c.reconcileCommitSlot(ctx, base, head.TreeOID, &route, &out)
	if err != nil {
		if errors.Is(err, errRouteMoved) {
			out.Rescheduled = true
			c.rescheduleOnLostRoute("route moved under the commit flip")
			return out
		}
		out.Err = err
		return out
	}
	out.CommitGenerationID = commitGeneration

	if err := c.reconcileDirtySlot(ctx, commitGeneration, head.TreeOID, &route, &out); err != nil {
		if errors.Is(err, errRouteMoved) {
			out.Rescheduled = true
			c.rescheduleOnLostRoute("route moved under the dirty flip")
			return out
		}
		out.Err = err
	}
	return out
}

// rescheduleOnLostRoute records a compare-and-set this cycle lost and signals
// the retry. It is the cas_lost half of a rescheduled cycle: another actor
// moved the route, which is a different condition from a working tree that
// would not settle, and the two must not add up to one number.
func (c *CheckoutCoordinator) rescheduleOnLostRoute(reason string) {
	viewmetrics.Count(viewmetrics.CoordinatorCycleTotal, viewmetrics.OutcomeCASLost)
	c.logger.Debug("checkout coordinator: route flip lost",
		zap.String("checkout", c.checkoutID), zap.String("reason", reason))
	c.Signal(reason)
}

// RehomeTo rebuilds this checkout's whole stack over another dedicated graph
// and installs it in one route write.
//
// It is the transition primitive both mode changes are built on. The layers
// are built while the route still says whatever it said before, and the graph
// and both generation pointers move together in a single compare-and-set, so a
// reader materializing during the rebuild gets the old stack or the new one —
// never a route naming the new graph with no layers over it, which is what the
// ordinary cycle leaves for the length of a build when it finds the primary
// moved under it.
//
// A checkout with no route at all — a dedicated one being demoted — has its
// route installed by the same write. There is nothing to compare against in
// that case: only a coordinator writes a checkout's route, and a checkout that
// has none has no coordinator either.
func (c *CheckoutCoordinator) RehomeTo(ctx context.Context, graphID string) (CheckoutCycle, error) {
	var out CheckoutCycle
	if c == nil {
		return out, errors.New("indexer: no coordinator to rehome")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithCancel(ctx)
	stopLifetimeCancel := context.AfterFunc(c.lifetimeContext(), cancel)
	defer func() {
		stopLifetimeCancel()
		cancel()
	}()
	release, err := c.gate.Acquire(ctx, ViewBuildInteractive)
	if err != nil {
		return out, fmt.Errorf("indexer: wait for checkout build admission: %w", err)
	}
	defer release()

	c.cycleMu.Lock()
	defer c.cycleMu.Unlock()

	dedicated, found, err := c.catalog.GetDedicatedGraph(ctx, graphID)
	if err != nil {
		return out, err
	}
	if !found {
		return out, fmt.Errorf("%w: dedicated graph %s", store_sqlite.ErrCatalogNotFound, graphID)
	}
	base, err := graphBase(ctx, c.catalog, dedicated)
	if err != nil {
		return out, err
	}
	head, err := gitstate.SampleHEAD(ctx, c.root)
	if err != nil {
		return out, fmt.Errorf("indexer: sample HEAD of %s: %w", c.root, err)
	}
	if head.TreeOID == "" {
		return out, fmt.Errorf("indexer: checkout %s has no HEAD tree", c.root)
	}
	route, routed, err := c.catalog.GetCheckoutRoute(ctx, c.checkoutID)
	if err != nil {
		return out, err
	}

	commitGeneration, reused, err := c.resolveCommitLayer(ctx, base, head.TreeOID)
	if err != nil {
		return out, err
	}
	out.CommitGenerationID, out.CommitBuilt, out.CommitReused = commitGeneration, !reused, reused

	dirtyGeneration, err := c.buildDirtyLayerOver(ctx, base.graphID, commitGeneration)
	if err != nil {
		c.abandonBuild(ctx, commitGeneration, !reused)
		return out, err
	}
	if dirtyGeneration == 0 {
		c.abandonBuild(ctx, commitGeneration, !reused)
		out.Rescheduled = true
		return out, errCheckoutUnsettled
	}
	out.DirtyGenerationID, out.DirtyBuilt = dirtyGeneration, true

	if err := c.installStack(ctx, route, routed, base.graphID, commitGeneration, dirtyGeneration); err != nil {
		c.abandonBuild(ctx, dirtyGeneration, true)
		c.abandonBuild(ctx, commitGeneration, !reused)
		if errors.Is(err, errRouteMoved) {
			out.Rescheduled = true
		}
		return out, err
	}

	// Everything the cache held was built over the graph the checkout has just
	// left, so none of it can ever be routed here again — except the layer
	// this transition routed, which the cache may have supplied.
	c.dropRetained(ctx, commitGeneration)
	c.retainCommit(ctx, generationIdentityKey(c.commitIdentity(base, head.TreeOID)), commitGeneration)
	c.rememberRoutedDirty(dirtyGeneration)
	if routed {
		c.offerRetire(ctx, route.DirtyGenerationID)
		c.offerRetire(ctx, route.CommitGenerationID)
	}
	return out, nil
}

type checkoutStackPublisher func(
	context.Context, store_sqlite.CheckoutRoute, bool, string, int64, int64,
) error

// preparePromotion builds a complete dedicated stack over an explicitly
// captured immutable base. Publication is delegated to the caller so the
// route and effective-mode flip can share one guarded catalog transaction.
func (c *CheckoutCoordinator) preparePromotion(
	ctx context.Context,
	base primaryBase,
	expectedHeadTree string,
	publish checkoutStackPublisher,
) (CheckoutCycle, error) {
	var out CheckoutCycle
	if c == nil {
		return out, errors.New("indexer: no coordinator to prepare promotion")
	}
	if publish == nil {
		return out, errors.New("indexer: promotion publisher is required")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithCancel(ctx)
	stopLifetimeCancel := context.AfterFunc(c.lifetimeContext(), cancel)
	defer func() {
		stopLifetimeCancel()
		cancel()
	}()
	release, err := c.gate.Acquire(ctx, ViewBuildInteractive)
	if err != nil {
		return out, fmt.Errorf("indexer: wait for checkout build admission: %w", err)
	}
	defer release()

	c.cycleMu.Lock()
	defer c.cycleMu.Unlock()

	head, err := gitstate.SampleHEAD(ctx, c.root)
	if err != nil {
		return out, fmt.Errorf("indexer: sample HEAD of %s: %w", c.root, err)
	}
	if head.TreeOID == "" || head.TreeOID != expectedHeadTree {
		return out, errCheckoutUnsettled
	}
	route, routed, err := c.catalog.GetCheckoutRoute(ctx, c.checkoutID)
	if err != nil {
		return out, err
	}

	commitGeneration, reused, err := c.resolveCommitLayer(ctx, base, expectedHeadTree)
	if err != nil {
		return out, err
	}
	out.CommitGenerationID, out.CommitBuilt, out.CommitReused = commitGeneration, !reused, reused

	dirtyGeneration, err := c.buildDirtyLayerOver(ctx, base.graphID, commitGeneration)
	if err != nil {
		c.abandonBuild(ctx, commitGeneration, !reused)
		return out, err
	}
	if dirtyGeneration == 0 {
		c.abandonBuild(ctx, commitGeneration, !reused)
		out.Rescheduled = true
		return out, errCheckoutUnsettled
	}
	out.DirtyGenerationID, out.DirtyBuilt = dirtyGeneration, true

	if err := publish(ctx, route, routed, base.graphID, commitGeneration, dirtyGeneration); err != nil {
		c.abandonBuild(ctx, dirtyGeneration, true)
		c.abandonBuild(ctx, commitGeneration, !reused)
		if errors.Is(err, store_sqlite.ErrCatalogStaleGuard) {
			out.Rescheduled = true
		}
		return out, err
	}

	c.dropRetained(ctx, commitGeneration)
	c.retainCommit(ctx, generationIdentityKey(c.commitIdentity(base, expectedHeadTree)), commitGeneration)
	c.rememberRoutedDirty(dirtyGeneration)
	if routed {
		c.offerRetire(ctx, route.DirtyGenerationID)
		c.offerRetire(ctx, route.CommitGenerationID)
	}
	return out, nil
}

// installStack points a checkout's route at a graph and both of its
// generations in one write.
func (c *CheckoutCoordinator) installStack(
	ctx context.Context,
	route store_sqlite.CheckoutRoute,
	routed bool,
	graphID string,
	commitGeneration, dirtyGeneration int64,
) error {
	if !routed {
		return c.catalog.UpsertCheckoutRoute(ctx, store_sqlite.CheckoutRoute{
			CheckoutID:         c.checkoutID,
			GraphID:            graphID,
			CommitGenerationID: commitGeneration,
			DirtyGenerationID:  dirtyGeneration,
			State:              store_sqlite.RouteActive,
		})
	}
	err := c.catalog.FlipCheckoutRoute(ctx, store_sqlite.FlipCheckoutRouteRequest{
		CheckoutID:         c.checkoutID,
		ExpectedRouteEpoch: route.RouteEpoch,
		GraphID:            graphID,
		CommitGenerationID: commitGeneration,
		DirtyGenerationID:  dirtyGeneration,
		State:              store_sqlite.RouteActive,
	})
	if errors.Is(err, store_sqlite.ErrCatalogStaleGuard) {
		return fmt.Errorf("%w: whole stack", errRouteMoved)
	}
	return err
}

// abandonBuild gives up a generation a transition will not route. One this
// call built is superseded and offered for collection; one it took from the
// reuse cache belongs to the cycle that built it and is left alone.
func (c *CheckoutCoordinator) abandonBuild(ctx context.Context, generationID int64, built bool) {
	if generationID <= 0 || !built {
		return
	}
	c.supersede(ctx, generationID)
	c.offerRetire(ctx, generationID)
}

// primaryBase reads the family's primary dedicated graph and the committed
// state the corpus under it is at.
//
// The tree comes from the primary's published generation when it has one, and
// from the primary checkout's recorded HEAD tree when it does not — which is
// what a plainly indexed primary looks like today, its corpus being the whole
// of its content. The primary's own uncommitted edits are not in that tree and
// are not described by any layer: they belong to the live incremental index,
// which keeps writing them into the corpus. A commit layer built over a dirty
// primary therefore diffs from the primary's HEAD while the corpus holds the
// primary's working tree, and the difference between the two shows through at
// exactly the paths the primary has edited and the checkout has not.
func (c *CheckoutCoordinator) primaryBase(ctx context.Context) (primaryBase, error) {
	if c.graphID != "" {
		dedicated, found, err := c.catalog.GetDedicatedGraph(ctx, c.graphID)
		if err != nil {
			return primaryBase{}, err
		}
		if !found || dedicated.FamilyID != c.familyID {
			return primaryBase{}, fmt.Errorf("%w: dedicated graph %s",
				store_sqlite.ErrCatalogNotFound, c.graphID)
		}
		return graphBase(ctx, c.catalog, dedicated)
	}
	graphs, err := c.catalog.ListDedicatedGraphs(ctx, c.familyID)
	if err != nil {
		return primaryBase{}, err
	}
	var primary *store_sqlite.DedicatedGraph
	for i := range graphs {
		if graphs[i].IsPrimaryBase {
			primary = &graphs[i]
			break
		}
	}
	if primary == nil {
		return primaryBase{}, fmt.Errorf("indexer: family %s has no primary dedicated graph", c.familyID)
	}
	return graphBase(ctx, c.catalog, *primary)
}

// graphBase resolves the corpus state a layer over one dedicated graph sits
// on: the graph's published generation when it has one, and the tree its
// owning checkout is committed at otherwise. Every layer built over a graph —
// a checkout's commit layer, a ref view's — reads its base from here, so the
// two cannot disagree about what "the corpus" is.
func graphBase(
	ctx context.Context,
	catalog *store_sqlite.Catalog,
	dedicated store_sqlite.DedicatedGraph,
) (primaryBase, error) {
	out := primaryBase{graphID: dedicated.GraphID}
	if dedicated.ActiveGenerationID > 0 {
		row, found, err := catalog.GetViewGeneration(ctx, dedicated.ActiveGenerationID)
		if err != nil {
			return primaryBase{}, err
		}
		if found {
			out.generationID = row.GenerationID
			out.treeOID = row.TreeOID
			return out, nil
		}
	}
	owner, found, err := catalog.GetCheckout(ctx, dedicated.OwnerCheckoutID)
	if err != nil {
		return primaryBase{}, err
	}
	if !found || owner.HeadTree == "" {
		return primaryBase{}, fmt.Errorf(
			"indexer: primary graph %s names no committed tree to build over", dedicated.GraphID)
	}
	out.treeOID = owner.HeadTree
	return out, nil
}

// ensureRoute reads the checkout's route, installing one when the checkout has
// never been routed and repointing one that names a different graph.
//
// A route whose graph changed is reset to pending with both slots cleared
// rather than repointed slot by slot: its generations were built over another
// graph's corpus and compose over nothing here. The reset is a compare-and-set
// like every other route write, so a coordinator that loses it leaves the
// winner's route alone.
func (c *CheckoutCoordinator) ensureRoute(ctx context.Context, base primaryBase) (store_sqlite.CheckoutRoute, error) {
	route, found, err := c.catalog.GetCheckoutRoute(ctx, c.checkoutID)
	if err != nil {
		return route, err
	}
	if !found {
		route = store_sqlite.CheckoutRoute{
			CheckoutID: c.checkoutID,
			GraphID:    base.graphID,
			State:      store_sqlite.RoutePending,
		}
		if err := c.catalog.UpsertCheckoutRoute(ctx, route); err != nil {
			return route, err
		}
		return route, nil
	}
	if route.GraphID == base.graphID {
		return route, nil
	}
	err = c.catalog.FlipCheckoutRoute(ctx, store_sqlite.FlipCheckoutRouteRequest{
		CheckoutID:         c.checkoutID,
		GraphID:            base.graphID,
		ExpectedRouteEpoch: route.RouteEpoch,
		State:              store_sqlite.RoutePending,
	})
	if err != nil {
		if errors.Is(err, store_sqlite.ErrCatalogStaleGuard) {
			return route, errRouteMoved
		}
		return route, err
	}
	previousCommit, previousDirty := route.CommitGenerationID, route.DirtyGenerationID
	route.GraphID = base.graphID
	route.CommitGenerationID, route.DirtyGenerationID = 0, 0
	route.RouteEpoch++
	route.State = store_sqlite.RoutePending
	c.dropRetained(ctx, 0)
	c.offerRetire(ctx, previousDirty)
	c.offerRetire(ctx, previousCommit)
	return route, nil
}

// reconcileCommitSlot points the commit slot at a generation describing the
// checkout's HEAD tree over the primary's base, building one only when no
// canonical ready generation with that content identity can be reached.
func (c *CheckoutCoordinator) reconcileCommitSlot(
	ctx context.Context,
	base primaryBase,
	targetTree string,
	route *store_sqlite.CheckoutRoute,
	out *CheckoutCycle,
) (int64, error) {
	identity := c.commitIdentity(base, targetTree)
	key := generationIdentityKey(identity)

	// Already routed to exactly this state: the cheapest outcome, and the one
	// every poll takes on a checkout nobody has touched.
	if route.CommitGenerationID > 0 {
		row, found, err := c.catalog.GetViewGeneration(ctx, route.CommitGenerationID)
		if err != nil {
			return 0, err
		}
		if found && servableGeneration(row.State) && generationRowKey(row) == key {
			c.retainCommit(ctx, key, row.GenerationID)
			return row.GenerationID, nil
		}
	}

	previous := route.CommitGenerationID
	resolved, err := c.resolveReadyCommitLayer(ctx, base, targetTree)
	if err != nil {
		return 0, err
	}
	if resolved.built {
		out.CommitBuilt = true
	}
	// A generation adopted from another checkout carries that checkout's
	// catalog owner identity. Content identity, not owner identity, makes it
	// settled; avoid needlessly advancing the route epoch on every poll.
	if route.CommitGenerationID == resolved.generationID && route.DirtyGenerationID <= 0 {
		c.releaseReadyCommitLease(resolved.leaseToken)
		c.retainCommit(ctx, key, resolved.generationID)
		if resolved.reused {
			out.CommitReused = true
		}
		return resolved.generationID, nil
	}
	if err := c.moveReadyCommitSlot(ctx, route, resolved); err != nil {
		if errors.Is(err, store_sqlite.ErrCatalogStaleGuard) {
			return 0, fmt.Errorf("%w: %s slot", errRouteMoved, store_sqlite.RouteSlotCommit)
		}
		return 0, err
	}
	if resolved.reused {
		out.CommitReused = true
	}
	c.retainCommit(ctx, key, resolved.generationID)
	c.releaseCommit(ctx, previous)
	return resolved.generationID, nil
}

// resolveCommitLayer reaches a commit generation describing one tree over one
// base, re-routing a retained generation with that identity when the cache
// holds one and building otherwise.
//
// It touches the route not at all. That is what lets a transition build a
// whole stack for a graph the checkout is not being served from yet and only
// then decide, in one write, that it is.
func (c *CheckoutCoordinator) resolveCommitLayer(
	ctx context.Context, base primaryBase, targetTree string,
) (generationID int64, reused bool, err error) {
	identity := c.commitIdentity(base, targetTree)
	if cached, ok := c.cachedCommit(ctx, generationIdentityKey(identity)); ok {
		return cached, true, nil
	}
	started := time.Now()
	generationID, report, err := c.builder.BuildCommitLayer(ctx, CommitLayerRequest{
		Identity:      identity,
		Base:          c.store.AtGeneration(base.generationID),
		RepoDir:       c.root,
		BaseTreeOID:   base.treeOID,
		TargetTreeOID: targetTree,
		RootPath:      c.root,
		RepoPrefix:    c.repoPrefix,
		WorkspaceID:   c.workspaceID,
		ProjectID:     c.projectID,
	})
	viewmetrics.Observe(viewmetrics.CoordinatorBuildSeconds, time.Since(started), viewmetrics.SlotCommit)
	if err != nil {
		return 0, false, err
	}
	if report.ClosureTruncated {
		c.logger.Warn("checkout coordinator: commit layer closure truncated",
			zap.String("checkout", c.checkoutID), zap.Int64("generation", generationID),
			zap.Int("cap", report.ClosureCap))
	}
	return generationID, false, nil
}

// moveCommitSlot points the commit slot at a different generation, dropping
// the working-tree slot in the same write.
//
// A routed dirty generation describes the working tree over the commit layer
// it was built against. Once the commit slot names a different layer, that
// generation over the new one is a state of the world the checkout was never
// in — one branch's uncommitted edits laid over another branch's tree — and
// the route would still report itself ready to serve it for the whole of the
// rebuild that follows. Clearing both pointers in one compare-and-set makes
// the route say what is true instead: it is mid-build, and the reader takes
// its base-corpus fallback until the working-tree layer has been rebuilt.
//
// The slot-at-a-time flip is kept for the case it is safe in: a route with no
// dirty generation has nothing to tear.
func (c *CheckoutCoordinator) moveCommitSlot(
	ctx context.Context,
	route *store_sqlite.CheckoutRoute,
	generationID int64,
) error {
	if route.DirtyGenerationID <= 0 {
		return c.flip(ctx, route, store_sqlite.RouteSlotCommit, generationID)
	}
	err := c.catalog.FlipCheckoutRoute(ctx, store_sqlite.FlipCheckoutRouteRequest{
		CheckoutID:         c.checkoutID,
		ExpectedRouteEpoch: route.RouteEpoch,
		GraphID:            route.GraphID,
		CommitGenerationID: generationID,
		DirtyGenerationID:  0,
		State:              store_sqlite.RoutePending,
	})
	if err != nil {
		if errors.Is(err, store_sqlite.ErrCatalogStaleGuard) {
			return fmt.Errorf("%w: %s slot", errRouteMoved, store_sqlite.RouteSlotCommit)
		}
		return err
	}
	dropped := route.DirtyGenerationID
	route.RouteEpoch++
	route.State = store_sqlite.RoutePending
	route.CommitGenerationID = generationID
	route.DirtyGenerationID = 0
	c.rememberRoutedDirty(0)
	c.offerRetire(ctx, dropped)
	return nil
}

// clearDirtySlot withdraws the working-tree slot and offers what it named for
// collection.
//
// moveCommitSlot closes the window this exists for whenever the coordinator
// moves the commit slot itself. What is left is a route some other writer left
// naming a working-tree layer built over a commit layer the route no longer
// names — a store written by a binary that flipped the two slots separately,
// or a slot-at-a-time flip from another surface. The rebuild that follows
// would serve that pair for its whole duration, so the slot goes first.
func (c *CheckoutCoordinator) clearDirtySlot(ctx context.Context, route *store_sqlite.CheckoutRoute) error {
	err := c.catalog.FlipCheckoutRouteSlot(ctx, store_sqlite.FlipCheckoutRouteSlotRequest{
		CheckoutID:         c.checkoutID,
		Slot:               store_sqlite.RouteSlotDirty,
		GenerationID:       0,
		ExpectedRouteEpoch: route.RouteEpoch,
		State:              store_sqlite.RoutePending,
	})
	if err != nil {
		if errors.Is(err, store_sqlite.ErrCatalogStaleGuard) {
			return fmt.Errorf("%w: %s slot", errRouteMoved, store_sqlite.RouteSlotDirty)
		}
		return err
	}
	dropped := route.DirtyGenerationID
	route.RouteEpoch++
	route.State = store_sqlite.RoutePending
	route.DirtyGenerationID = 0
	c.rememberRoutedDirty(0)
	c.offerRetire(ctx, dropped)
	return nil
}

// reconcileDirtySlot points the dirty slot at a generation describing the
// working tree over the commit layer beneath it.
//
// The comparison that decides whether to build is between the working tree's
// fingerprint and the one recorded on the routed generation. BuildDirtyLayer
// stamps that fingerprint into lower_view_fingerprint, which is the honest
// column for it: the dirty layer's lower view IS the working tree it was read
// from, and the fingerprint is what identifies it. The routed generation's
// base is checked alongside, because a commit layer that was just rebuilt puts
// the same working tree over different content underneath — and a routed
// generation that fails that check is withdrawn before the rebuild starts
// rather than after it ends, since the pair it forms with the routed commit
// generation describes no state the checkout has ever been in.
//
// targetTree is the committed tree the commit slot was settled against, and the
// sample has to agree with it before anything is built or kept. A commit layer
// takes as long as the tree it indexes, and a checkout is free to commit while
// one is being built for it: the sample taken afterwards then describes the
// working tree of a HEAD the layer beneath knows nothing about, and a layer
// built from it would carry no payload for the paths the new commit moved —
// they are committed, not dirty — so the pair would serve the old tree's
// content as the checkout's current state. The cycle reschedules instead, and
// the next one rebuilds the commit slot for the head the checkout is really at.
func (c *CheckoutCoordinator) reconcileDirtySlot(
	ctx context.Context,
	commitGeneration int64,
	targetTree string,
	route *store_sqlite.CheckoutRoute,
	out *CheckoutCycle,
) error {
	sample, err := gitstate.SampleDirty(ctx, c.root)
	if err != nil {
		return fmt.Errorf("indexer: sample %s: %w", c.root, err)
	}
	c.noteDirtyFingerprint(sample.Fingerprint)
	if sample.HeadTree != targetTree {
		out.Rescheduled = true
		viewmetrics.Count(viewmetrics.CoordinatorCycleTotal, viewmetrics.OutcomeHeadMoved)
		c.logger.Debug("checkout coordinator: the checkout committed under the cycle",
			zap.String("checkout", c.checkoutID),
			zap.String("built_for", targetTree), zap.String("now_at", sample.HeadTree))
		c.Signal("the checkout moved to another commit under the cycle")
		return nil
	}
	if route.DirtyGenerationID > 0 {
		row, found, err := c.catalog.GetViewGeneration(ctx, route.DirtyGenerationID)
		if err != nil {
			return err
		}
		servable := found && servableGeneration(row.State)
		if servable && row.BaseGenerationID == commitGeneration {
			// A layer over the routed commit generation describes a state the
			// checkout really was in, so it keeps serving while the working
			// tree it no longer matches is rebuilt underneath the route.
			if row.LowerViewFingerprint == sample.Fingerprint {
				c.rememberRoutedDirty(row.GenerationID)
				out.DirtyGenerationID = row.GenerationID
				return nil
			}
		} else if err := c.clearDirtySlot(ctx, route); err != nil {
			return err
		}
	}

	generationID, err := c.buildDirtyLayerOver(ctx, route.GraphID, commitGeneration)
	if err != nil {
		return err
	}
	if generationID == 0 {
		// The route still names the last coherent state, which is the point: a
		// stale view of a real state beats a torn view of a state that never was.
		out.Rescheduled = true
		viewmetrics.Count(viewmetrics.CoordinatorCycleTotal, viewmetrics.OutcomeRescheduled)
		c.Signal("the working tree moved under two builds")
		return nil
	}
	out.DirtyBuilt = true
	previous := route.DirtyGenerationID
	if err := c.flip(ctx, route, store_sqlite.RouteSlotDirty, generationID); err != nil {
		c.supersede(ctx, generationID)
		c.offerRetire(ctx, generationID)
		return err
	}
	out.DirtyGenerationID = generationID
	c.offerRetire(ctx, previous)
	return nil
}

// buildDirtyLayerOver builds the working-tree layer over one commit
// generation, and reports 0 with a nil error when two attempts in a row were
// torn by edits landing under them.
//
// Two attempts, no more. Each build re-samples the checkout itself and refuses
// to publish a payload the working tree has already moved past, so the second
// attempt is the "one more try against what it is now" the refusal is worth. A
// checkout under a stream of edits would invalidate every attempt, and a caller
// that kept trying would spin instead of letting the next quiet window decide.
//
// Like resolveCommitLayer it writes nothing to the route: what the checkout
// reads is the caller's decision.
func (c *CheckoutCoordinator) buildDirtyLayerOver(
	ctx context.Context, graphID string, commitGeneration int64,
) (int64, error) {
	dirtyBase, err := c.commitLayerReader(ctx, commitGeneration)
	if err != nil {
		return 0, err
	}
	identity := c.dirtyIdentity(graphID, commitGeneration)
	for attempt := 0; attempt < 2; attempt++ {
		started := time.Now()
		generationID, _, err := c.builder.BuildDirtyLayer(ctx, DirtyLayerRequest{
			Identity:     identity,
			Base:         dirtyBase,
			CheckoutRoot: c.root,
			RepoPrefix:   c.repoPrefix,
			WorkspaceID:  c.workspaceID,
			ProjectID:    c.projectID,
			buildBarrier: c.dirtyBarrier,
		})
		viewmetrics.Observe(viewmetrics.CoordinatorBuildSeconds, time.Since(started), viewmetrics.SlotDirty)
		if err == nil {
			return generationID, nil
		}
		if !errors.Is(err, ErrDirtySnapshotChanged) {
			return 0, err
		}
		// The refused attempt is a whole payload for a state the checkout has
		// already left. confirmDirtySnapshot superseded it, and nothing will
		// ever route it, so it is collected here — an editor saving over a
		// build would otherwise leak one payload per save for the life of the
		// daemon.
		var torn *DirtySnapshotChangedError
		if errors.As(err, &torn) {
			c.offerRetire(ctx, torn.GenerationID)
		}
	}
	return 0, nil
}

// commitLayerReader is the reader a dirty-layer build computes its affected
// closure against: the checkout's commit generation composed over the exact
// base generation recorded in the catalog.
func (c *CheckoutCoordinator) commitLayerReader(ctx context.Context, commitGeneration int64) (LayerBase, error) {
	row, found, err := c.catalog.GetViewGeneration(ctx, commitGeneration)
	if err != nil {
		return nil, fmt.Errorf("indexer: read commit generation %d: %w", commitGeneration, err)
	}
	if !found || !servableGeneration(row.State) {
		return nil, fmt.Errorf("indexer: commit generation %d is not servable", commitGeneration)
	}
	layer, err := graphview.NewGenerationLayer(c.store.AtGeneration(commitGeneration))
	if err != nil {
		return nil, fmt.Errorf("indexer: open commit generation %d: %w", commitGeneration, err)
	}
	corpus := c.store.AtGeneration(row.BaseGenerationID)
	return commitLayerBase{
		Reader: graph.NewOverlaidViewWithLayer(corpus, layer),
		corpus: corpus,
	}, nil
}

// flip repoints one slot under the route epoch this cycle read, and advances
// the caller's copy of the route so the next flip carries the epoch the
// database now holds.
func (c *CheckoutCoordinator) flip(
	ctx context.Context,
	route *store_sqlite.CheckoutRoute,
	slot store_sqlite.RouteSlot,
	generationID int64,
) error {
	// The builder publishes as its last step, so a generation reaching here is
	// already ready and PublishAndRoute — which publishes and then flips —
	// would refuse it for not being in the building state. The flip alone is
	// what is left of that pair for a caller holding a published generation.
	err := c.catalog.FlipCheckoutRouteSlot(ctx, store_sqlite.FlipCheckoutRouteSlotRequest{
		CheckoutID:         c.checkoutID,
		Slot:               slot,
		GenerationID:       generationID,
		ExpectedRouteEpoch: route.RouteEpoch,
		State:              store_sqlite.RouteActive,
	})
	if err != nil {
		if errors.Is(err, store_sqlite.ErrCatalogStaleGuard) {
			return fmt.Errorf("%w: %s slot", errRouteMoved, slot)
		}
		return err
	}
	route.RouteEpoch++
	route.State = store_sqlite.RouteActive
	switch slot {
	case store_sqlite.RouteSlotCommit:
		route.CommitGenerationID = generationID
	case store_sqlite.RouteSlotDirty:
		route.DirtyGenerationID = generationID
		c.rememberRoutedDirty(generationID)
	}
	return nil
}

// rememberRoutedDirty records which working-tree generation the route names.
func (c *CheckoutCoordinator) rememberRoutedDirty(generationID int64) {
	c.mu.Lock()
	c.routedDirty = generationID
	c.mu.Unlock()
}

// supersede marks a generation nothing will read. It is the answer to a lost
// route flip: the payload is whole and published, and the checkout it was
// built for is being served from something else.
func (c *CheckoutCoordinator) supersede(ctx context.Context, generationID int64) {
	if generationID <= 0 {
		return
	}
	viewmetrics.Count(viewmetrics.CoordinatorCycleTotal, viewmetrics.OutcomeSuperseded)
	if err := c.store.MarkPayloadGenerationSuperseded(ctx, generationID); err != nil {
		c.logger.Debug("checkout coordinator: could not supersede an unrouted generation",
			zap.String("checkout", c.checkoutID),
			zap.Int64("generation", generationID), zap.Error(err))
	}
}

// --- the commit-layer reuse cache ---------------------------------------

// cachedCommit returns a retained generation for an identity, after confirming
// the catalog still holds it in a state that can be served. A generation that
// has gone is dropped from the cache rather than re-routed.
func (c *CheckoutCoordinator) cachedCommit(ctx context.Context, key string) (int64, bool) {
	c.mu.Lock()
	var generationID int64
	for _, entry := range c.retained {
		if entry.key == key {
			generationID = entry.generationID
			break
		}
	}
	c.mu.Unlock()
	if generationID == 0 {
		return 0, false
	}
	row, found, err := c.catalog.GetViewGeneration(ctx, generationID)
	if err != nil || !found || !servableGeneration(row.State) || generationRowKey(row) != key {
		c.forgetRetained(generationID)
		return 0, false
	}
	return generationID, true
}

// retainCommit records a commit generation as re-routable and retires whatever
// the cache had to give up to hold it.
func (c *CheckoutCoordinator) retainCommit(ctx context.Context, key string, generationID int64) {
	c.mu.Lock()
	retained := c.retained[:0:0]
	retained = append(retained, retainedCommitLayer{key: key, generationID: generationID})
	for _, entry := range c.retained {
		if entry.key == key || entry.generationID == generationID {
			continue
		}
		retained = append(retained, entry)
	}
	var evicted []int64
	if len(retained) > c.retain {
		for _, entry := range retained[c.retain:] {
			evicted = append(evicted, entry.generationID)
		}
		retained = retained[:c.retain]
	}
	c.retained = retained
	c.mu.Unlock()

	for _, generation := range evicted {
		c.offerRetire(ctx, generation)
	}
}

// releaseCommit is what a replaced commit generation gets: a place in the
// reuse cache rather than immediate retirement.
//
// This is the whole of the branch-switch cache. Retiring the generation the
// route just left would make the next switch back re-index a tree whose
// payload is still sitting in the database; keeping it costs one branch's
// difference from the base until the cache evicts it.
func (c *CheckoutCoordinator) releaseCommit(ctx context.Context, generationID int64) {
	if generationID <= 0 {
		return
	}
	c.mu.Lock()
	held := false
	for _, entry := range c.retained {
		if entry.generationID == generationID {
			held = true
			break
		}
	}
	c.mu.Unlock()
	if !held {
		c.offerRetire(ctx, generationID)
	}
}

// forgetRetained drops one generation from the reuse cache.
func (c *CheckoutCoordinator) forgetRetained(generationID int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	kept := c.retained[:0]
	for _, entry := range c.retained {
		if entry.generationID != generationID {
			kept = append(kept, entry)
		}
	}
	c.retained = kept
}

// dropRetained empties the reuse cache and offers everything it held for
// retirement. The graph the cached generations composed over is gone, so none
// of them will ever be routed again.
//
// keep names the one generation to spare — the layer a transition has just
// routed, which is in the cache because it was reachable there and must not be
// collected out from under the route it now serves. 0 spares nothing.
func (c *CheckoutCoordinator) dropRetained(ctx context.Context, keep int64) {
	c.mu.Lock()
	retained := c.retained
	c.retained = nil
	c.mu.Unlock()
	for _, entry := range retained {
		if entry.generationID == keep {
			continue
		}
		c.offerRetire(ctx, entry.generationID)
	}
}

// --- retirement ---------------------------------------------------------

// offerRetire tries to collect a generation nothing should be reading. A
// refusal is not an error: the generation may be leased by a live view, routed
// by another checkout that adopted the same build, or still named as the base
// of a layer above it. Whatever refused it is expected to stop refusing later,
// so the generation goes on the backlog the janitor retries.
func (c *CheckoutCoordinator) offerRetire(ctx context.Context, generationID int64) {
	if generationID <= 0 {
		return
	}
	if err := c.store.RetirePayloadGeneration(ctx, generationID, c.inUse()); err != nil {
		if errors.Is(err, store_sqlite.ErrCatalogNotFound) {
			return
		}
		c.mu.Lock()
		c.backlog[generationID] = struct{}{}
		held := len(c.backlog)
		c.mu.Unlock()
		// The blocking reason and how many generations this coordinator is
		// now owing a retirement for: together they say whether one holder is
		// stuck or the backlog is growing.
		c.logger.Debug("checkout coordinator: generation retirement deferred",
			zap.String("checkout", c.checkoutID),
			zap.Int64("generation", generationID),
			zap.String("blocked_by", retireBlockReason(err)),
			zap.Int("backlog", held), zap.Error(err))
	}
}

// retireBlockReason names what refused a retirement, in the same bounded
// vocabulary the metric label uses. The error carries the id; this carries the
// class, so a log line and a counter can be read against each other.
func retireBlockReason(err error) string {
	switch {
	case errors.Is(err, store_sqlite.ErrPayloadGenerationInUse):
		return viewmetrics.RefusedLeased
	case errors.Is(err, store_sqlite.ErrCatalogGenerationReferenced):
		return viewmetrics.RefusedRouted
	default:
		return viewmetrics.RefusedError
	}
}

// SweepRetirements retries every generation a retire refused, and reports how
// many were collected. It is the janitor's half of retirement: the coordinator
// offers, the sweep insists.
func (c *CheckoutCoordinator) SweepRetirements(ctx context.Context) int {
	if c == nil {
		return 0
	}
	c.mu.Lock()
	pending := make([]int64, 0, len(c.backlog))
	for generationID := range c.backlog {
		pending = append(pending, generationID)
	}
	c.mu.Unlock()
	retireNewestFirst(pending)

	retired := 0
	for _, generationID := range pending {
		err := c.store.RetirePayloadGeneration(ctx, generationID, c.inUse())
		if err != nil && !errors.Is(err, store_sqlite.ErrCatalogNotFound) {
			continue
		}
		retired++
		viewmetrics.Count(viewmetrics.GenerationSweepCollectedTotal, viewmetrics.SweepCheckout)
		c.mu.Lock()
		delete(c.backlog, generationID)
		c.mu.Unlock()
	}
	return retired
}

// DrainRetirements hands over every generation this coordinator still owes a
// retirement for, and forgets them.
//
// Three sets go: the offers a refusal deferred, the commit layers the reuse
// cache was holding for a branch switch back, and the working-tree generation
// the route names. A closed coordinator has no loop left to retry the first
// and no cycle left to make use of the second; the third is there because the
// teardown that closes a coordinator withdraws the route first, and once that
// row is gone nothing in the catalog can be asked which generations a checkout
// had — a payload with no reachable id is a payload nothing can ever collect.
//
// What the route still names is refused retirement while it does, which is the
// correct answer for a coordinator that was dropped for any other reason: the
// offer stays on the owner's list and succeeds when — and only when — the
// route stops naming it.
func (c *CheckoutCoordinator) DrainRetirements() []int64 {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]int64, 0, len(c.backlog)+len(c.retained)+1)
	for generationID := range c.backlog {
		out = append(out, generationID)
	}
	for _, entry := range c.retained {
		out = append(out, entry.generationID)
	}
	out = append(out, c.routedDirty)
	c.backlog = map[int64]struct{}{}
	c.retained = nil
	c.routedDirty = 0
	return out
}

// retireNewestFirst orders a batch of retirements so a layer is offered before
// the generation it sits on.
//
// Retirement refuses a generation another one names as its base, so a working-
// tree layer and the commit layer under it can only be collected in one order,
// and a single pass that took them the other way round would leave the commit
// layer for the next sweep. Generation ids are an ascending sequence and a base
// is always created before what sits on it, so descending id IS that order.
func retireNewestFirst(generations []int64) {
	slices.Sort(generations)
	slices.Reverse(generations)
}

// inUse is the lease predicate retirement consults, nil when nothing leases
// generations.
func (c *CheckoutCoordinator) inUse() func(int64) bool {
	if c.leases == nil {
		return nil
	}
	return c.leases.InUse
}

// --- identity -----------------------------------------------------------

// commitIdentity is the catalog identity of the commit layer for one tree.
//
// It names everything the payload is a function of and nothing else. The base
// the layer sits on is carried as lower_view_fingerprint — the base corpus's
// committed tree — so a primary that moves invalidates the cache rather than
// leaving a layer composed over content it was not built against. The commit
// oid is deliberately NOT part of it: two commits with the same tree produce
// the same payload, and keying on the commit would rebuild for a rebase that
// changed nothing a reader can see.
func (c *CheckoutCoordinator) commitIdentity(base primaryBase, targetTree string) GenerationIdentity {
	return GenerationIdentity{
		OwnerKind:            checkoutLayerOwnerKind,
		GraphID:              base.graphID,
		LayerID:              commitLayerID(c.checkoutID),
		CheckoutID:           c.checkoutID,
		GenerationKind:       CommitLayerGenerationKind,
		BaseGenerationID:     base.generationID,
		LowerViewFingerprint: base.treeOID,
		TreeOID:              targetTree,
		ConfigHash:           c.configHash,
		ExtractorVersions:    c.extractors,
		ResolverVersion:      checkoutResolverVersion,
	}
}

// dirtyIdentity is the catalog identity of the working-tree layer. The three
// fields that identify WHICH working-tree state it describes — the tree, the
// commit and the content fingerprint — are stamped by BuildDirtyLayer from its
// own sample, so a caller cannot name one state and build another.
func (c *CheckoutCoordinator) dirtyIdentity(graphID string, commitGeneration int64) GenerationIdentity {
	return GenerationIdentity{
		OwnerKind:         checkoutLayerOwnerKind,
		GraphID:           graphID,
		LayerID:           dirtyLayerID(c.checkoutID),
		CheckoutID:        c.checkoutID,
		GenerationKind:    DirtyLayerGenerationKind,
		BaseGenerationID:  commitGeneration,
		ConfigHash:        c.configHash,
		ExtractorVersions: c.extractors,
		ResolverVersion:   checkoutResolverVersion,
	}
}

// commitLayerID and dirtyLayerID name a checkout's two layers. They are
// derived rather than generated so the catalog's in-flight coalescing can
// recognise two builds of the same layer as the same build.
func commitLayerID(checkoutID string) string { return "commit-" + checkoutID }
func dirtyLayerID(checkoutID string) string  { return "dirty-" + checkoutID }

// generationIdentityKey renders the build identity as one comparable string.
// It carries exactly the columns the catalog's in-flight coalescing compares,
// so the reuse cache and the catalog agree on what "the same build" means.
func generationIdentityKey(identity GenerationIdentity) string {
	var b strings.Builder
	for _, field := range []string{
		identity.OwnerKind,
		identity.GraphID,
		identity.LayerID,
		identity.CheckoutID,
		identity.GenerationKind,
		strconv.FormatInt(identity.BaseGenerationID, 10),
		identity.LowerViewFingerprint,
		identity.TreeOID,
		identity.ProvenanceCommitOID,
		identity.ConfigHash,
		identity.ExtractorVersions,
		identity.ResolverVersion,
	} {
		b.WriteString(field)
		b.WriteByte(0)
	}
	return b.String()
}

// generationRowKey renders a stored generation's identity the same way, so a
// row read back from the catalog can be compared with a request.
func generationRowKey(row store_sqlite.ViewGeneration) string {
	return generationIdentityKey(GenerationIdentity{
		OwnerKind:            row.OwnerKind,
		GraphID:              row.GraphID,
		LayerID:              row.LayerID,
		CheckoutID:           row.CheckoutID,
		GenerationKind:       row.GenerationKind,
		BaseGenerationID:     row.BaseGenerationID,
		LowerViewFingerprint: row.LowerViewFingerprint,
		TreeOID:              row.TreeOID,
		ProvenanceCommitOID:  row.ProvenanceCommitOID,
		ConfigHash:           row.ConfigHash,
		ExtractorVersions:    row.ExtractorVersions,
		ResolverVersion:      row.ResolverVersion,
	})
}

// servableGeneration mirrors the materializer's rule for a generation a route
// may name: ready serves, and so does superseded — superseded says only that a
// newer generation exists, and the route decides what a checkout reads.
func servableGeneration(state store_sqlite.ViewGenerationState) bool {
	return state == store_sqlite.ViewGenerationReady || state == store_sqlite.ViewGenerationSuperseded
}

// indexConfigHash digests the index configuration a generation was built
// under. A payload built under different extraction rules does not compose
// with one built under these, so the digest is part of the build identity
// rather than a diagnostic.
func indexConfigHash(cfg config.IndexConfig) string {
	encoded, err := json.Marshal(cfg)
	if err != nil {
		// An unencodable configuration cannot be compared, and treating that
		// as "matches everything" would reuse a payload built under rules
		// nobody can name. A unique digest makes every such build its own.
		return "unhashable-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:16])
}

// extractorVersionsFingerprint renders the extractor policy versions the same
// way the per-repo freshness row does, so a language whose extractor was
// bumped re-builds the layers that carry its files.
func extractorVersionsFingerprint() string {
	encoded, err := json.Marshal(extractorVersionsSnapshot())
	if err != nil {
		return ""
	}
	return string(encoded)
}

// commitLayerBase presents a checkout's commit layer, composed over the base
// corpus, as the base a dirty-layer build reads.
//
// The composition answers every identity read on its own. The two reads it
// does not carry are served from the corpus handle underneath it: the batched
// file-node lookup, which is the per-file read in a loop, and the durable
// reference facts, whose rows are the corpus's. The facts are a hint the
// closure adds to its edge walk rather than its only source, so serving the
// corpus's rows leaves the walk — which does see the commit layer — as the
// authority on what depends on what.
type commitLayerBase struct {
	graph.Reader
	corpus *store_sqlite.Store
}

var _ LayerBase = commitLayerBase{}
var _ graph.RefFactsReader = commitLayerBase{}

// GetFileNodesByPaths answers the batched read from the composed view one path
// at a time. The closure asks it once per build with the change set's paths,
// so the loop is bounded by the change rather than by the repository.
func (b commitLayerBase) GetFileNodesByPaths(filePaths []string) map[string][]*graph.Node {
	out := make(map[string][]*graph.Node, len(filePaths))
	for _, path := range filePaths {
		if nodes := b.GetFileNodes(path); len(nodes) > 0 {
			out[path] = nodes
		}
	}
	return out
}

// LoadRefFactsByFiles serves the corpus's persisted forward facts.
func (b commitLayerBase) LoadRefFactsByFiles(repoPrefix string, files []string) ([]graph.RefFact, error) {
	return b.corpus.LoadRefFactsByFiles(repoPrefix, files)
}

// LoadRefFactsByTargets serves the corpus's persisted reverse facts.
func (b commitLayerBase) LoadRefFactsByTargets(repoPrefix string, targetIDs []string) (map[string][]graph.RefFact, error) {
	return b.corpus.LoadRefFactsByTargets(repoPrefix, targetIDs)
}
