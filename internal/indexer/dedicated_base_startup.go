package indexer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/viewmetrics"
)

// The initial committed publication path.
//
// Everything below drives the publication runtime installed once per process
// in serverstack.NewSharedServer, ahead of every owner registration. The
// runtime owns the authority, the per-graph observation gate and the drain;
// this file is the only production caller that asks it to publish, and it asks
// exactly once per dedicated repository per daemon start.
//
// Publication is NOT activation. Adopting a committed base moves
// `dedicated_graphs.active_generation_id` so DEPENDENT checkouts can key their
// commit layers on an immutable lower snapshot; the owning repository's own
// request route stays on legacy generation 0, a declared limitation. Nothing
// here relabels generation 0, and nothing here routes a request.

var errInitialBasePublisherInput = errors.New("indexer: invalid initial dedicated base publisher")

// initialDedicatedBaseAuthorityDomain versions the deterministic authority
// token the startup publisher claims with.
//
// The token is deterministic ON PURPOSE. Catalog.AcquireDedicatedBaseAuthority
// treats a repeated token as a lost-response retry and returns the stored
// authority with ZERO writes; any other token rotates the epoch, which clears
// the attempt record and forces the next claim to re-bind (a catalog write) or
// re-allocate. A warm restart over an unchanged committed tree must perform no
// catalog DML at all, so the token has to be a function of the identity that
// survives the restart rather than of the process that claims it.
//
// The owner incarnation is part of the pre-image, so a retracked checkout — a
// new incarnation for the same path — claims a different token and does rotate.
// The catalog fences owner identity independently (dedicatedBaseOwnerTx joins
// dedicated_graphs to checkouts on the exact incarnation), so a stable token
// never widens who may publish.
const initialDedicatedBaseAuthorityDomain = "gortex.dedicated-base.startup-authority.v1"

// InitialBasePublication is one repository's publication outcome.
//
// It is a value rather than a log line because the startup publisher runs off
// the readiness path: by the time a human or a test asks what happened, the
// work is over. Skipped names a repository nothing was attempted for and why;
// Err names one that was attempted and failed. A failure is recoverable — the
// catalog's failed-claim recovery re-enters the same attempt on the next start
// — so it is reported, never fatal to startup.
type InitialBasePublication struct {
	GraphID        string
	RepoPrefix     string
	CheckoutID     string
	GenerationID   int64
	AlreadyAdopted bool
	Coalesced      bool
	// Advanced is true when the committed tree had moved while this process
	// was not running, so publication took the advancement path rather than
	// the initial full-root path.
	Advanced bool
	Skipped  string
	Err      error

	// Live marks an outcome the git watcher's HEAD-change finalize path asked
	// for, as opposed to one this daemon start scheduled.
	Live bool
	// Demanded marks an outcome a CONSUMER asked for: a dependent checkout
	// whose coordinator found no published base to compose over. It is the
	// on-demand half of the consumer gate — a daemon start and a HEAD movement
	// both decline to publish for a family with no reader, so the first reader
	// is what asks.
	Demanded bool
	// TreeOID is the committed tree the publication was FOR. It is reported
	// because a live advance resolves its own target from Git rather than
	// from the checkout row, so "which tree did this publish" is not
	// answerable from the catalog's checkouts table alone until adoption
	// advances head_tree with it.
	TreeOID string
}

// dedicatedBaseTarget names the committed point one publication is for.
//
// The startup publisher leaves it zero and takes the owner checkout row's
// head_commit/head_tree: a start has just reconciled the family, so that row
// is the freshest fact available.
//
// A LIVE advance cannot use it. `checkouts.head_tree` is written by the
// reconciler's family pass (internal/reconcile/reconcile.go:448, :605 through
// `headFor`) and by nothing on the ref-transition path, so at the instant the
// git watcher observes a new commit the row still names the PREVIOUS tree — a
// window that closes only when adoption advances head_tree with it. The
// trigger therefore resolves the target from Git itself and hands it in.
// Publishing ahead of the row is coherent for every reader: once a base is
// adopted, `primaryBase` reads the tree off the GENERATION row and not off the
// checkout (checkout_coordinator.go:1258-1281), and only the unpublished
// fallback below it reads `owner.HeadTree`.
type dedicatedBaseTarget struct {
	CommitOID string
	TreeOID   string
}

// basePublishRequest is one queued publication.
//
// live requests carry the observed root so the publisher can refuse to
// publish a graph whose owner checkout is NOT the working copy the watcher
// observed: a linked worktree tracked as its own repository has its own HEAD,
// and stamping the owner's base with a sibling's commit would publish a tree
// the owner never had.
type basePublishRequest struct {
	prefix string
	target dedicatedBaseTarget
	root   string
	live   bool
	// demand marks a request one CONSUMER asked for (RequestBase). It is
	// admitted exactly like a startup request — the target is the owner
	// checkout row's own head — and it exists as a separate flag so the
	// outcome can say which door asked, and so a consumer's request is not
	// silenced by the once-per-daemon Schedule memo.
	demand bool
	// done, when set, is called with the outcome after the worker records it.
	// It runs off the publisher's lock.
	done func(InitialBasePublication)
}

// InitialBasePublisher publishes the initial committed base for the dedicated
// repositories a daemon start brings up.
//
// Three properties decide its shape.
//
//   - It must not block readiness, for ANY number of repositories. A committed
//     base is a full index of a committed tree; doing that inline in the warmup
//     dispatch would double cold-start cost before the graph is queryable. So
//     the readiness path only ever appends to an UNBOUNDED pending list
//     (Schedule), and the queue is not drained at all until the daemon has
//     flipped ready (BeginDraining). A bounded queue would reintroduce the very
//     coupling this avoids: once it filled, the warmup worker calling Schedule
//     would park behind whole-repository publications.
//   - It must be bounded at shutdown. The lifecycle's Close joins the
//     publisher drain (checkout_lifecycle.go, `<-publishersDrained`) and an
//     admitted publication counts as an admitted actor for its whole length,
//     physical build included. The publisher therefore registers its
//     cancellation with the runtime, which cancels it the moment admission
//     closes — the first thing Close does.
//   - It must publish through the SHARED lease domain. A private
//     graphview.LeaseManager leaves an advanced generation invisible to the
//     retirement sweep the request readers agree on, and ensureObserved's
//     nil-lease branch degrades a claimed delta to a refusal. The constructor
//     refuses to build a publisher whose runtime does not carry the
//     lifecycle's own manager.
type InitialBasePublisher struct {
	lifecycle *CheckoutLifecycle
	runtime   *DedicatedBaseRuntime
	logger    *zap.Logger

	ctx    context.Context
	cancel context.CancelFunc
	// release detaches this publisher's cancellation from the runtime.
	//
	// It is reached from Close, which is the stop for a caller that owns one
	// publisher's lifetime while the runtime outlives it: a test that builds a
	// publisher per fixture, and any future per-request publisher. The daemon
	// does NOT call Close — its shutdown closes publisher admission on the
	// runtime instead, and CloseDedicatedBaseAdmission cancels every
	// registered driver and drops the whole registry in one act. Both routes
	// end with this publisher's context cancelled and no entry left behind.
	release func()

	// advance is the live HEAD-change trigger this publisher owns. It shares
	// this publisher's queue, so the process runs exactly ONE committed-base
	// publication at a time whichever source asked for it: a startup
	// publication and a watcher-observed advance for the same graph can
	// otherwise reach two concurrent physical builds of the same tree.
	advance *DedicatedBaseAdvanceTrigger

	mu        sync.Mutex
	scheduled map[string]struct{}
	// pendingOrder is the FIFO of prefixes with an unattempted request, and
	// pendingReq holds the request itself. It is a slice plus a map, not a
	// channel: the startup enqueue side runs on the readiness path and must
	// never wait for the drain side, whatever the repository count, and the
	// live enqueue side must be able to REPLACE a queued request in place so
	// a burst of HEAD changes coalesces to its newest target instead of
	// publishing every intermediate commit.
	pendingOrder []string
	pendingReq   map[string]basePublishRequest
	// drainReleased is BeginDraining's flag. A startup request is only
	// admitted by the worker once it is set, which is what keeps "ready, then
	// publish" a property of the code. A LIVE request ignores it: the git
	// watcher is started after the readiness flip (warmupDaemonState brings
	// the MultiWatcher up in its last step), so a HEAD change cannot precede
	// readiness, and making an advance wait on a flag it can never observe
	// unset would only strand it.
	drainReleased bool
	// queued counts every request accepted into the queue; attempted counts
	// every one the worker has finished with (published, skipped or failed).
	// Wait blocks while attempted < queued. PublishRepo touches neither,
	// which is what keeps the accounting exact: it is a synchronous call
	// whose result the caller already has, not scheduled work. A live request
	// that REPLACES a queued one does not count twice — the queue depth did
	// not move, so neither does the accounting.
	queued    int
	attempted int
	outcomes  []InitialBasePublication
	closed    bool
	// changed is closed and replaced on every queued/attempted movement, so
	// Wait parks on a channel instead of polling.
	changed chan struct{}

	// wake nudges the worker when pending grows. Capacity 1: it is an edge
	// signal, not a queue.
	wake   chan struct{}
	worker sync.Once
	done   chan struct{}
}

// NewInitialBasePublisher binds a publisher to the lifecycle's installed
// runtime. It publishes nothing; Schedule and PublishRepo do.
//
// Every input is mandatory and none is defaulted: without the installed
// runtime there is no authority to publish under, and a substituted lease
// manager is a silent downgrade rather than an error at the point of use.
func NewInitialBasePublisher(lifecycle *CheckoutLifecycle) (*InitialBasePublisher, error) {
	if lifecycle == nil || lifecycle.store == nil || lifecycle.catalog == nil || lifecycle.mi == nil {
		return nil, fmt.Errorf("%w: publication requires a lifecycle over a store", errInitialBasePublisherInput)
	}
	runtime, ok := lifecycle.DedicatedBasePublisherRuntime().(*DedicatedBaseRuntime)
	if !ok || runtime == nil || runtime.dedicatedBaseRuntime == nil {
		return nil, fmt.Errorf("%w: no dedicated base publisher runtime is installed", errInitialBasePublisherInput)
	}
	leases := runtime.ViewLeases()
	if leases == nil || leases != lifecycle.ViewLeases() {
		// The runtime's own doc says the field exists so advancement triggers
		// cannot invent a private manager. This is the check that makes that
		// true: a publisher over a private domain would advance generations
		// that the retirement sweep cannot see are in use.
		return nil, fmt.Errorf("%w: publication requires the lifecycle's shared view leases", errInitialBasePublisherInput)
	}
	logger := lifecycle.logger
	if logger == nil {
		logger = zap.NewNop()
	}
	p := &InitialBasePublisher{
		lifecycle:  lifecycle,
		runtime:    runtime,
		logger:     logger,
		scheduled:  map[string]struct{}{},
		pendingReq: map[string]basePublishRequest{},
		changed:    make(chan struct{}),
		wake:       make(chan struct{}, 1),
		done:       make(chan struct{}),
	}
	p.ctx, p.cancel, p.release = runtime.publicationContext(context.Background())
	// The live advancement trigger is installed here rather than by the daemon
	// because this is the one construction site that already holds everything
	// it needs — the lifecycle, the installed runtime and the shared lease
	// domain — and because the git watcher that reaches it is built by
	// MultiWatcher, which holds none of them. See the registry's own comment.
	p.advance = newDedicatedBaseAdvanceTrigger(p)
	return p, nil
}

// AdvanceTrigger is the live HEAD-change trigger bound to this publisher.
func (p *InitialBasePublisher) AdvanceTrigger() *DedicatedBaseAdvanceTrigger {
	if p == nil {
		return nil
	}
	return p.advance
}

// Schedule asks for one repository's initial committed base.
//
// It NEVER publishes and NEVER blocks: it appends to an unbounded pending list
// under a mutex held for the append alone, and returns. That is load-bearing
// rather than incidental — every call site is a warmup worker upstream of the
// readiness flip, so any wait here is a wait for the daemon to become
// queryable. Nothing is drained until BeginDraining.
//
// Repeated calls for the same prefix are one publication: the catalog
// coalesces a concurrent attempt anyway, but a queue that re-enqueues would
// make an idle restart re-observe every repository on every warmup signal.
func (p *InitialBasePublisher) Schedule(repoPrefix string) {
	if p == nil || repoPrefix == "" {
		return
	}
	p.mu.Lock()
	if p.closed || p.ctx.Err() != nil {
		// Stopped, whether by Close or by the runtime cancelling this driver
		// when publisher admission closed. Both are "no more publications".
		p.mu.Unlock()
		return
	}
	if _, dup := p.scheduled[repoPrefix]; dup {
		p.mu.Unlock()
		return
	}
	p.scheduled[repoPrefix] = struct{}{}
	p.enqueueLocked(basePublishRequest{prefix: repoPrefix})
	p.mu.Unlock()
	p.nudge()
}

// RequestBase asks for one repository's committed base because a CONSUMER
// needs it now.
//
// It is the on-demand door the consumer gate makes necessary. publish declines
// a family with no reader ("no dependent checkout"), so the publication a
// dependent needs is not already queued and not already done; the first
// dependent that notices the absence — CheckoutCoordinator.primaryBase's
// unpublished arm — asks here.
//
// Three things separate it from Schedule.
//
//   - It does NOT consult the once-per-daemon `scheduled` memo. That memo
//     exists so an idle restart does not re-observe every repository on every
//     warmup signal; a repository whose startup publication was DECLINED for
//     want of a reader has to be publishable again the moment one appears, and
//     a demand that the memo swallowed would defer the base forever.
//   - It coalesces on the pending slot instead. A request already queued for
//     this prefix — startup, live or demand — will publish the current tree,
//     which is what the caller wants, so a second one would only re-enter the
//     same protocol. A caller polling every 15 s therefore costs one queue
//     lookup, not one publication.
//   - It is admitted by the worker on the same terms as a startup request: a
//     demand that arrives before BeginDraining waits for the readiness flip.
//     "Ready, then publish" is an ordering property of the code and a consumer
//     asking early must not be the hole in it.
//
// It never publishes on the caller's goroutine and never blocks; it reports
// whether the request is now queued (or already was). A stopped publisher
// accepts nothing.
func (p *InitialBasePublisher) RequestBase(repoPrefix string) bool {
	if p == nil || repoPrefix == "" {
		return false
	}
	p.mu.Lock()
	if p.closed || p.ctx.Err() != nil {
		p.mu.Unlock()
		return false
	}
	if _, queued := p.pendingReq[repoPrefix]; queued {
		p.mu.Unlock()
		return true
	}
	p.enqueueLocked(basePublishRequest{prefix: repoPrefix, demand: true})
	p.mu.Unlock()
	// Start the worker for the same reason enqueueAdvance does: a demand can
	// arrive on a daemon whose warmup has already released the queue, and the
	// worker is started once per publisher. popLocked still withholds this
	// request until BeginDraining has run, so starting the goroutine early
	// costs one parked goroutine and changes no ordering.
	p.worker.Do(func() { go p.run() })
	p.nudge()
	return true
}

// enqueueAdvance queues one live committed-base advance, coalescing a burst to
// its newest target.
//
// A queued request for the same repository is REPLACED rather than appended
// to: the newest observed commit supersedes every older one, so ten commits
// landing while one publication runs cost one follow-up publication of the
// tenth tree, not ten publications of ten trees. Replacement keeps the FIFO
// position and the queue depth, so the accounting Wait reads stays exact.
//
// It reports whether the request was accepted; a stopped publisher accepts
// nothing.
func (p *InitialBasePublisher) enqueueAdvance(req basePublishRequest) bool {
	if p == nil || req.prefix == "" {
		return false
	}
	req.live = true
	p.mu.Lock()
	if p.closed || p.ctx.Err() != nil {
		p.mu.Unlock()
		return false
	}
	p.enqueueLocked(req)
	p.mu.Unlock()
	// A live advance starts the worker itself. Unlike a startup publication it
	// cannot precede the readiness flip (watchers come up after it), so there
	// is no ordering left for BeginDraining to protect here — and a HEAD
	// change on a daemon whose warmup never reached BeginDraining would
	// otherwise never be published at all.
	p.worker.Do(func() { go p.run() })
	p.nudge()
	return true
}

// enqueueLocked appends or replaces one request. The caller holds p.mu.
func (p *InitialBasePublisher) enqueueLocked(req basePublishRequest) {
	if p.pendingReq == nil {
		p.pendingReq = map[string]basePublishRequest{}
	}
	if existing, queued := p.pendingReq[req.prefix]; queued {
		if existing.live && !req.live {
			// A STARTUP request never downgrades a queued LIVE one. The live
			// request carries three things this one does not: the commit the
			// watcher resolved from Git (the checkout row's head_tree still
			// names the previous tree at this instant — see
			// dedicatedBaseCommitTree), the `live` flag that lets the worker
			// admit it before BeginDraining, and the trigger's completion memo,
			// whose loss makes the next observation of the same commit pay for
			// a fresh cohort description. Replacement is for a NEWER target of
			// the same kind, not for an older one arriving late.
			return
		}
		// Same slot, newer target: the depth did not move, so neither does
		// the accounting. A replaced request's completion callback is dropped
		// with it — its caller is the trigger, which re-reads the outcome of
		// whichever request actually runs.
		p.pendingReq[req.prefix] = req
		return
	}
	p.pendingReq[req.prefix] = req
	p.pendingOrder = append(p.pendingOrder, req.prefix)
	p.queued++
	p.notifyLocked()
}

// popLocked takes the first request the worker may attempt now. A startup
// request is admitted only once BeginDraining has released the queue; a live
// one always is. The caller holds p.mu.
func (p *InitialBasePublisher) popLocked() (basePublishRequest, bool) {
	for i, prefix := range p.pendingOrder {
		req, ok := p.pendingReq[prefix]
		if !ok {
			continue
		}
		if !req.live && !p.drainReleased {
			continue
		}
		p.pendingOrder = append(p.pendingOrder[:i:i], p.pendingOrder[i+1:]...)
		delete(p.pendingReq, prefix)
		return req, true
	}
	return basePublishRequest{}, false
}

// BeginDraining releases the queue. Publication starts here and nowhere
// earlier.
//
// The daemon calls this immediately after the readiness flip, so the ordering
// "ready, then publish" is a property of the code rather than a race the
// scheduler usually wins: no committed-tree index can be in front of
// markReady, however many repositories warmup brought up and however slow each
// publication is. It is idempotent, and a call after Close is a no-op.
func (p *InitialBasePublisher) BeginDraining() {
	if p == nil {
		return
	}
	p.mu.Lock()
	stopped := p.closed || p.ctx.Err() != nil
	if !stopped {
		p.drainReleased = true
	}
	p.mu.Unlock()
	if stopped {
		return
	}
	p.worker.Do(func() { go p.run() })
	p.nudge()
}

// nudge is the edge signal to the worker; a full buffer already means "there
// is work", so dropping the second signal loses nothing.
func (p *InitialBasePublisher) nudge() {
	select {
	case p.wake <- struct{}{}:
	default:
	}
}

// run drains the pending list one repository at a time.
//
// Serial on purpose: each item is a full index of a committed tree, and a
// workspace with twenty repositories would otherwise fork twenty of them
// against the same store while the post-ready enrichment pool is running.
func (p *InitialBasePublisher) run() {
	defer close(p.done)
	for {
		p.mu.Lock()
		req, ok := p.popLocked()
		p.mu.Unlock()
		if !ok {
			select {
			case <-p.ctx.Done():
				return
			case <-p.wake:
				continue
			}
		}
		if p.ctx.Err() != nil {
			// Cancelled between dequeue and publish. Record the skip so the
			// accounting Wait reads stays exact for this item, then stop:
			// whatever is still pending is abandoned, and Wait reports the
			// cancellation rather than completion.
			p.record(InitialBasePublication{RepoPrefix: req.prefix, Live: req.live, Skipped: "publisher stopped"}, req.done)
			return
		}
		p.record(p.publish(p.ctx, req), req.done)
	}
}

func (p *InitialBasePublisher) record(outcome InitialBasePublication, done func(InitialBasePublication)) {
	p.mu.Lock()
	p.outcomes = append(p.outcomes, outcome)
	p.attempted++
	p.notifyLocked()
	p.mu.Unlock()
	switch {
	case outcome.Err != nil:
		// Recoverable by construction: a failed claim is re-entered by the
		// next start's ClaimDedicatedBaseBuild, which verifies and replaces a
		// failed payload rather than allocating beside it.
		p.logger.Warn("daemon: initial committed base publication failed; the dedicated base stays where it was",
			zap.String("repo", outcome.RepoPrefix), zap.String("graph", outcome.GraphID), zap.Error(outcome.Err))
	case outcome.Skipped != "":
		p.logger.Debug("daemon: initial committed base publication skipped",
			zap.String("repo", outcome.RepoPrefix), zap.String("reason", outcome.Skipped))
	default:
		p.logger.Info("daemon: committed base published",
			zap.String("repo", outcome.RepoPrefix), zap.String("graph", outcome.GraphID),
			zap.Int64("generation", outcome.GenerationID),
			zap.Bool("already_adopted", outcome.AlreadyAdopted),
			zap.Bool("advanced", outcome.Advanced),
			zap.Bool("live", outcome.Live))
	}
	if done != nil {
		done(outcome)
	}
}

// notifyLocked publishes a queued/attempted movement to every parked Wait.
// The caller holds p.mu.
func (p *InitialBasePublisher) notifyLocked() {
	close(p.changed)
	p.changed = make(chan struct{})
}

// Pending reports how many scheduled publications have not been attempted yet
// — the queue depth behind the readiness flip.
func (p *InitialBasePublisher) Pending() int {
	if p == nil {
		return 0
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.queued - p.attempted
}

// Outcomes reports what the publisher has done so far, newest last.
func (p *InitialBasePublisher) Outcomes() []InitialBasePublication {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]InitialBasePublication, len(p.outcomes))
	copy(out, p.outcomes)
	return out
}

// Wait blocks until every SCHEDULED publication has been attempted. It is the
// join a test and an orderly shutdown use; nothing on the readiness path calls
// it.
//
// Two things bound it. It parks on the movement channel rather than polling,
// so it costs nothing while a publication runs; and it returns the
// publisher's own context error the moment the driver is cancelled, because
// the abandoned tail of the pending list will never be attempted. A caller
// that never calls BeginDraining therefore waits for its own ctx, which is the
// honest answer: nothing is draining.
func (p *InitialBasePublisher) Wait(ctx context.Context) error {
	if p == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	for {
		p.mu.Lock()
		settled := p.attempted >= p.queued
		changed := p.changed
		p.mu.Unlock()
		if settled {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-p.ctx.Done():
			return p.ctx.Err()
		case <-changed:
		}
	}
}

// Close stops admitting publications and cancels the one in flight.
//
// It is for a caller that owns one publisher's lifetime while the runtime
// outlives it. The daemon does not call it: its shutdown closes publisher
// admission on the runtime, which cancels this driver's context and drops its
// registration wholesale, and the runtime's own drain is what shutdown joins.
// After either route Schedule is a no-op and Wait reports the cancellation.
func (p *InitialBasePublisher) Close() {
	if p == nil {
		return
	}
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	p.closed = true
	p.mu.Unlock()
	p.advance.close()
	p.cancel()
	if p.release != nil {
		p.release()
	}
}

// PublishRepo publishes (or re-adopts) one repository's committed base
// synchronously. Schedule is the production entry point; this is the same work
// without the queue, so a caller that genuinely wants to wait — a test, a
// one-shot server — does not have to poll.
//
// It deliberately does not touch the queue accounting: the outcome is returned
// to the caller, so recording it would inflate Outcomes and, worse, make
// attempted exceed queued for work Wait never promised. Outcomes and Pending
// describe scheduled work only.
func (p *InitialBasePublisher) PublishRepo(ctx context.Context, repoPrefix string) InitialBasePublication {
	if p == nil {
		return InitialBasePublication{RepoPrefix: repoPrefix, Skipped: "no publisher"}
	}
	if ctx == nil {
		ctx = p.ctx
	}
	return p.publish(ctx, basePublishRequest{prefix: repoPrefix})
}

// publicationOutcome classifies one settled publication for the counter.
//
// The five outcomes are mutually exclusive and ordered by which fact is the
// stronger one: a failure is a failure whatever else it carried, a skip means
// nothing was attempted, and between the three successes the reuse facts
// (re-adoption, then a coalesced build) outrank "published" because a
// publication that reused work is precisely what the series exists to count.
func publicationOutcome(out InitialBasePublication) string {
	switch {
	case out.Err != nil:
		return viewmetrics.PublicationFailed
	case out.Skipped != "":
		return viewmetrics.PublicationSkipped
	case out.AlreadyAdopted:
		return viewmetrics.PublicationReadopted
	case out.Coalesced:
		return viewmetrics.PublicationCoalesced
	default:
		return viewmetrics.PublicationPublished
	}
}

// publish is the whole protocol for one repository.
func (p *InitialBasePublisher) publish(ctx context.Context, req basePublishRequest) (out InitialBasePublication) {
	// One publication, one counted outcome, whichever door asked for it. The
	// queue worker and the synchronous PublishRepo both run this body, so a
	// seam in only one of them would make the series a property of which door
	// a caller picked rather than of what the daemon did. The deferred count
	// also covers the early returns, because a skip is as much an outcome as a
	// published generation. The one thing it deliberately does NOT count is
	// the abandoned tail of the pending queue (run's "publisher stopped"
	// record): that work never entered the protocol at all.
	defer func() {
		viewmetrics.Count(viewmetrics.DedicatedBasePublicationTotal, publicationOutcome(out))
	}()
	repoPrefix := req.prefix
	out = InitialBasePublication{RepoPrefix: repoPrefix, Live: req.live, Demanded: req.demand}
	if err := ctx.Err(); err != nil {
		out.Skipped = "publisher stopped"
		return out
	}
	l := p.lifecycle
	graphID := GraphIDFor(repoPrefix)
	out.GraphID = graphID
	graph, found, err := l.catalog.GetDedicatedGraph(ctx, graphID)
	if err != nil {
		out.Err = err
		return out
	}
	if !found || graph.State != store_sqlite.DedicatedGraphReady {
		out.Skipped = "no ready dedicated graph"
		return out
	}
	checkout, found, err := l.catalog.GetCheckout(ctx, graph.OwnerCheckoutID)
	if err != nil {
		out.Err = err
		return out
	}
	if !found || checkout.Incarnation == "" {
		out.Skipped = "no owner checkout"
		return out
	}
	out.CheckoutID = checkout.CheckoutID
	target := dedicatedBaseTarget{CommitOID: checkout.HeadCommit, TreeOID: checkout.HeadTree}
	if req.live {
		// A watcher observes ONE working copy. If the graph this prefix names
		// is owned by a different checkout, that checkout's HEAD is not the
		// one that moved, and stamping its base with this commit would
		// publish a tree the owner never had.
		if !sameCheckoutRoot(req.root, checkout.RootPath) {
			out.Skipped = "observed root is not the dedicated owner"
			return out
		}
		tree, err := dedicatedBaseCommitTree(ctx, checkout.RootPath, req.target.CommitOID)
		if err != nil {
			out.Err = err
			return out
		}
		target = dedicatedBaseTarget{CommitOID: req.target.CommitOID, TreeOID: tree}
	}
	out.TreeOID = target.TreeOID
	if target.TreeOID == "" {
		// Nothing committed to publish. A family whose reconcile has not yet
		// named a HEAD tree is not an error; the next start, or the live
		// advancement trigger, publishes it.
		out.Skipped = "owner has no committed tree"
		return out
	}
	// The fourth skip: nothing can read this base yet.
	//
	// Publication is NOT activation (this file's header). The owning
	// repository's own request route stays on legacy generation 0, so a
	// committed base has exactly one purpose — to give a DEPENDENT checkout or
	// a REF VIEW an immutable lower snapshot to key a layer on. A family with
	// neither is a reader set of size zero, and publishing for it costs a
	// second full index of the committed tree: measured at +847 MB of logical
	// writes and a store of 61 -> 122 MB on the 1,500-file cold-index phase,
	// for a base no route names.
	//
	// This is DEFER, not drop, and the deferral is recoverable by exactly the
	// mechanisms the three skips above already rely on:
	//
	//   - the first dependent asks for it (CheckoutCoordinator.primaryBase ->
	//     CheckoutLifecycle.requestDedicatedBase -> RequestBase), which is the
	//     moment the absence first matters to anyone;
	//   - the live advance trigger re-enters on the next HEAD movement, and by
	//     then the census is re-read, so a consumer that appeared meanwhile
	//     gets the CURRENT tree rather than the one this attempt declined;
	//   - the next daemon start schedules the repository again.
	//
	// Until one of those lands, a dependent composes over generation 0 — the
	// legacy regime graphBase's second arm serves and recomposeOverAdvancedBase
	// keeps coherent — which is exactly where every dependent was before a
	// committed base existed at all.
	//
	// The gate sits HERE, after the committed-tree check and before the
	// authority block below, because everything above it is a read and
	// AcquireDedicatedBaseAuthority is the first thing on this path that can
	// write. A declined publication therefore performs zero catalog DML.
	consumers, err := l.dedicatedBaseConsumers(ctx, graph)
	if err != nil {
		out.Err = err
		return out
	}
	if !consumers {
		out.Skipped = "no dependent checkout"
		return out
	}
	owner := store_sqlite.DedicatedBaseOwner{CheckoutID: checkout.CheckoutID, Incarnation: checkout.Incarnation}

	authority := store_sqlite.AcquireDedicatedBaseAuthorityRequest{
		GraphID: graphID, Owner: owner, Token: initialDedicatedBaseAuthorityToken(graphID, owner),
	}
	// Read the stored authority so a token that does NOT match can still
	// rotate. When it does match, AcquireDedicatedBaseAuthority returns before
	// it looks at these, which is the zero-write warm-restart path.
	if publication, found, err := l.catalog.DedicatedBasePublication(ctx, graphID); err != nil {
		out.Err = err
		return out
	} else if found {
		authority.ExpectedEpoch = publication.Desire.Authority.Epoch
		authority.ExpectedToken = publication.Desire.Authority.Token
	}
	publisher, err := p.runtime.install(ctx, authority)
	if err != nil {
		out.Err = err
		return out
	}

	observe := func(ctx context.Context) (dedicatedBaseObservation, error) {
		return p.observe(ctx, graphID, repoPrefix, target)
	}
	var result dedicatedBaseResult
	if graph.ActiveGenerationID == 0 {
		// Cold: nothing is published for this graph, so the first committed
		// generation is a self-contained full root. ensureInitial withholds
		// the lease manager, which is what refuses a claimed delta on a path
		// that has no lower snapshot to compose over.
		result, err = publisher.ensureInitial(ctx, observe)
	}
	if graph.ActiveGenerationID != 0 || errors.Is(err, errDedicatedBaseAdvanceRequired) {
		// Warm: a base is already published. If the committed tree, the
		// configuration or the dependency revision moved while this process
		// was not running, nothing else will notice — the Git watcher only
		// sees HEAD changes it observes live — so startup is the advancement
		// trigger for that window. An unchanged identity takes the catalog's
		// adopted-replay path and writes nothing.
		//
		// The advance-required fallback covers the narrow window where the
		// pointer moved between the read above and the observation gate:
		// ensureInitial leaves desire and the active pointer untouched when it
		// refuses, so re-entering through ensureCurrent is safe rather than a
		// second publication attempt.
		out.Advanced = true
		result, err = publisher.ensureCurrent(ctx, p.runtime.ViewLeases(), observe)
	}
	out.GenerationID = result.Adoption.GenerationID
	out.AlreadyAdopted = result.Adoption.AlreadyAdopted
	out.Coalesced = result.Report.Coalesced
	switch {
	case err == nil:
	case errors.Is(err, context.Canceled), errors.Is(err, errDedicatedBaseRuntimeClosed):
		// Shutdown cancelled the publication. The attempt record survives and
		// the next start recovers it; this is not a failure to report.
		out.Skipped = "publisher stopped"
	default:
		out.Err = err
	}
	if out.AlreadyAdopted {
		// A replay is not an advancement, whichever entry point reached it.
		out.Advanced = false
	}
	return out
}

// observe assembles one fresh observation inside the runtime's per-graph gate.
//
// Everything the identity names is read HERE rather than carried in from the
// caller: the active-generation pointer the claim is fenced against, the
// committed tree, the frozen configuration digest and the dependency-revision
// cohort. That is what makes the observation stable — the runtime holds the
// graph's observation gate across this call and compares the pointer it
// returns against the catalog inside the same gate.
// The target names the committed point being published: a live advance
// resolved it from Git before entering the gate, a startup publication leaves
// it zero and takes the owner checkout row's own head.
func (p *InitialBasePublisher) observe(ctx context.Context, graphID, repoPrefix string, target dedicatedBaseTarget) (dedicatedBaseObservation, error) {
	l := p.lifecycle
	graph, found, err := l.catalog.GetDedicatedGraph(ctx, graphID)
	if err != nil {
		return dedicatedBaseObservation{}, err
	}
	if !found {
		return dedicatedBaseObservation{}, fmt.Errorf("%w: dedicated graph %s vanished", store_sqlite.ErrCatalogStaleGuard, graphID)
	}
	checkout, found, err := l.catalog.GetCheckout(ctx, graph.OwnerCheckoutID)
	if err != nil {
		return dedicatedBaseObservation{}, err
	}
	if !found {
		return dedicatedBaseObservation{}, fmt.Errorf("%w: dedicated owner %s is gone",
			store_sqlite.ErrCatalogStaleGuard, graph.OwnerCheckoutID)
	}
	if target.TreeOID == "" {
		target = dedicatedBaseTarget{CommitOID: checkout.HeadCommit, TreeOID: checkout.HeadTree}
	}
	if target.TreeOID == "" {
		return dedicatedBaseObservation{}, fmt.Errorf("%w: dedicated owner %s has no committed tree",
			store_sqlite.ErrCatalogStaleGuard, graph.OwnerCheckoutID)
	}
	idx := l.mi.GetIndexer(repoPrefix)
	if idx == nil {
		return dedicatedBaseObservation{}, fmt.Errorf("%w: repository %s is not indexed", errInitialBasePublisherInput, repoPrefix)
	}
	repoCfg := config.Default()
	if l.cfgMgr != nil {
		repoCfg = l.cfgMgr.GetRepoConfig(repoPrefix)
	}
	// GetRepoConfig hands back a SHALLOW result. snapshotDedicatedBaseConfig
	// deep-clones it and re-owns the synthesizer slice, so what the builder
	// indexes under cannot change while it indexes, and the digest it returns
	// is the same one the coordinator derives for its layers.
	frozen, fingerprint, err := snapshotDedicatedBaseConfig(
		repoCfg.Index, repoPrefix, idx.WorkspaceID(), idx.ProjectID())
	if err != nil {
		return dedicatedBaseObservation{}, fmt.Errorf("freeze the index configuration for %s: %w", repoPrefix, err)
	}
	sections := dedicatedBaseConfigSections(repoCfg)
	builder := &SparseGenerationBuilder{
		Store:      l.store,
		Registry:   l.mi.registry,
		Config:     frozen,
		Logger:     l.logger,
		Admissions: idx,
		Embedder:   l.mi.embedder,
		Semantic:   l.mi.semanticMgr,
	}
	cohort := dependencyCohortSource{
		Target: DependencyRevisionTarget{
			RepoPrefix: repoPrefix, WorkspaceID: idx.WorkspaceID(), ProjectID: idx.ProjectID(),
		},
		Leases:           l.leases,
		Catalog:          l.catalog,
		WorkspaceMembers: builderWorkspaceMembers(builder, idx.WorkspaceID()),
		Config:           frozen,
		ConfigSections:   sections,
		Ownership: []DependencyRevisionOwnership{{
			RepoPrefix: repoPrefix, Language: "go", Owner: goPackageOwnershipTargetEvidence,
		}},
		Producers:         cohortProducerPolicy(frozen, l.mi.embedder != nil),
		Capabilities:      cohortCapabilityVocabulary(),
		ExtractorVersions: extractorVersionsFingerprint(),
		SourceBudget:      dependencyRevisionSourceBudget,
	}
	// The revision is never empty. An empty revision is the legacy value the
	// reuse guards read as "matches anything"; a cohort that could not be
	// described says so in a degraded revision instead, which fails closed
	// (nothing reuses it) without blocking publication.
	revision, revErr := cohort.revision(ctx)
	if revErr != nil {
		reason := dependencyCohortRefusalReason(revErr)
		revision = cohort.degradedRevision(reason)
		p.logger.Warn("daemon: the resolver-visible input cohort could not be described for the committed base; "+
			"it carries a degraded revision",
			zap.String("repo", repoPrefix), zap.String("reason", reason), zap.Error(revErr))
	}
	now := time.Now
	if l.now != nil {
		now = l.now
	}
	return dedicatedBaseObservation{
		Identity: store_sqlite.DedicatedBaseIdentity{
			TreeOID:            target.TreeOID,
			ConfigHash:         checkoutConfigHash(fingerprint, sections),
			ExtractorVersions:  extractorVersionsFingerprint(),
			ResolverVersion:    resolverVersionFingerprint(),
			DependencyRevision: revision,
		},
		ExpectedActiveGenerationID: graph.ActiveGenerationID,
		RootPath:                   checkout.RootPath,
		WorkspaceID:                idx.WorkspaceID(),
		ProjectID:                  idx.ProjectID(),
		ProvenanceCommitOID:        target.CommitOID,
		CreatedAt:                  now().Unix(),
		Builder:                    *builder,
	}, nil
}

// initialDedicatedBaseAuthorityToken derives the deterministic authority token
// described on initialDedicatedBaseAuthorityDomain.
func initialDedicatedBaseAuthorityToken(graphID string, owner store_sqlite.DedicatedBaseOwner) string {
	var e dependencyRevisionEncoder
	e.field("domain", initialDedicatedBaseAuthorityDomain)
	e.field("graph", graphID)
	e.field("checkout", owner.CheckoutID)
	e.field("incarnation", owner.Incarnation)
	sum := sha256.Sum256([]byte(e.b.String()))
	return initialDedicatedBaseAuthorityDomain + ":" + hex.EncodeToString(sum[:16])
}
