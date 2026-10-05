package indexer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/gitstate"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/pathkey"
)

const (
	maxCheckoutRefreshTickets     = 1024
	checkoutRefreshCaptureTimeout = 5 * time.Second
)

var (
	ErrCheckoutRefreshQueueFull  = errors.New("indexer: checkout refresh ticket queue is full; retry recovery")
	ErrCheckoutRefreshSuperseded = errors.New("indexer: checkout refresh was superseded by a different checkout state")
	ErrCheckoutRefreshStopped    = errors.New("indexer: checkout coordinator stopped before refresh completed")
	checkoutRefreshSequence      atomic.Uint64
)

// CheckoutRefreshTicket identifies a checkout-local graph publication. The
// inner ticket's Generation is an admission sequence; AppliedGeneration in its
// terminal result is the actual SQLite dirty generation, never a primary index.
type CheckoutRefreshTicket struct {
	CheckoutID  string
	Incarnation string
	Root        string
	RepoPrefix  string
	// ContentHash is raw-file SHA256 for a source ticket, empty for root recovery.
	// Callers bind it to the bytes they committed, not to a later external edit.
	ContentHash string
	Ticket      *MutationTicket
}

type checkoutRefreshRequest struct {
	ticket      *CheckoutRefreshTicket
	done        chan MutationResult
	checkout    store_sqlite.Checkout
	rootInfo    os.FileInfo
	headRef     string
	headCommit  string
	headTree    string
	fingerprint string // Exact snapshot consumed by the published dirty generation.
	// bindAtCompletion marks a ticket admitted without a working-copy sample
	// of its own: it promises the first published generation that a sample
	// begun after its admission describes (fingerprint is empty). An edit
	// lease's ticket binds the committed bytes by contentHash and HEAD by the
	// lease's admission sample; a require_fresh ticket binds nothing more.
	bindAtCompletion bool
	// headUnbound marks a ticket whose admission pinned no HEAD (a
	// require_fresh ticket admitted without a sample): the completion sample's
	// fingerprint, which covers HEAD's tree, is the whole check.
	headUnbound bool
	// admittedAt is when the ticket joined the waiters.
	admittedAt time.Time
	// freshAfter is the instant any working-copy sample that decides this
	// ticket must have begun at or after: the start of its own capture sample
	// (which began after the caller's request, and after an edit's disk
	// commit), or its admission when it brought no sample of its own start.
	// A cycle may decide on, and complete the ticket with, any sample begun at
	// or after it.
	freshAfter  time.Time
	contentHash string
	// record is the caller's publication record (WithPublicationRecord),
	// bound to the ticket at admission, before the coordinator is woken.
	record *PublicationPhaseRecord
	// releaseWrite ends the store write announcement the ticket holds from
	// its admission until it completes or fails (AnnounceCheckoutRefresh):
	// WAL reclaim still yields while a ticket waits. Chain folds instead yield
	// to ordinary announcements and actual writer-gate waiters, so a ticket
	// does not block its own inline fold or background copy during CPU work.
	releaseWrite func()
}

// Identity returns the immutable checkout identity admitted before a disk edit.
func (m *CheckoutMutation) Identity() (checkoutID, incarnation string) {
	if m == nil {
		return "", ""
	}
	return m.checkout.CheckoutID, m.checkout.Incarnation
}

// EnqueueRefresh attaches committed disk content to the coordinator's existing
// background loop. It does not build a generation or wait on the build gate.
// The caller must release this mutation lease before waiting for Ticket.Done.
// Admission after a disk commit outlives request cancellation, but remains
// bounded and cannot outlive coordinator shutdown.
func (m *CheckoutMutation) EnqueueRefresh(ctx context.Context, path string) (*CheckoutRefreshTicket, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed || !m.prepared || !m.refreshReserved || m.fresh || m.refreshQueued {
		return nil, fmt.Errorf("%w: no unqueued committed checkout mutation", ErrCheckoutMutationStale)
	}
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), checkoutRefreshCaptureTimeout)
	defer cancel()
	ctx, cancelLifetime := checkoutMutationContext(ctx, m.coordinator.lifetimeContext())
	defer cancelLifetime()
	if err := m.validateCheckout(ctx); err != nil {
		return nil, err
	}
	// The capture sample binds the exact post-commit snapshot (an unrelated
	// edit or a branch switch after it supersedes the ticket). It is also the
	// serving cycle's decision sample: it began after the disk commit, which
	// is all that cycle needs of it (freshAfter, cycleSample), so the edit
	// pays for one working-copy sample between its write and its build.
	// The capture sample is on the edit's path to its ticket: urgent, like
	// the lease's pre-write sample.
	request, err := m.coordinator.captureCheckoutRefresh(gitstate.WithUrgentSample(ctx), m.checkout, m.rootInfo, path)
	if err != nil {
		return nil, err
	}
	StampPublicationPhase(ctx, PublicationTicketCaptured)
	if request.headRef != m.headRef || request.headCommit != m.headCommit || request.headTree != m.headTree {
		return nil, ErrCheckoutRefreshSuperseded
	}
	ticket, err := m.coordinator.enqueueCheckoutRefresh(request, true)
	m.refreshReserved = false
	if err == nil {
		m.refreshQueued = true
	}
	return ticket, err
}

// RequestCheckoutRefresh explicitly retries an automatic checkout whose route
// may be missing, pending or failed. Identity and availability remain strict,
// but graph readiness is deliberately not a precondition for graph recovery.
func (l *CheckoutLifecycle) RequestCheckoutRefresh(ctx context.Context, checkoutID, expectedRoot string) (*CheckoutRefreshTicket, error) {
	return l.requestCheckoutRefresh(ctx, checkoutID, expectedRoot, nil)
}

// RequestCheckoutRefreshFromSample is RequestCheckoutRefresh admitting the
// ticket against a working-copy sample the caller already took, instead of
// taking another. The caller must have taken sample after its request arrived
// (a freshness proof that found the tree changed does exactly that), and the
// sample must be of this checkout. The ticket still completes only through
// the post-admission verification against a sample the cycle takes after the
// ticket was admitted.
func (l *CheckoutLifecycle) RequestCheckoutRefreshFromSample(
	ctx context.Context, checkoutID, expectedRoot string, sample gitstate.DirtySnapshot,
) (*CheckoutRefreshTicket, error) {
	return l.requestCheckoutRefresh(ctx, checkoutID, expectedRoot, &sample)
}

func (l *CheckoutLifecycle) requestCheckoutRefresh(
	ctx context.Context, checkoutID, expectedRoot string, sample *gitstate.DirtySnapshot,
) (*CheckoutRefreshTicket, error) {
	if l == nil || l.catalog == nil || checkoutID == "" || expectedRoot == "" {
		return nil, fmt.Errorf("%w: checkout identity and root are required", ErrCheckoutMutationStale)
	}
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, c, checkout, rootInfo, cancel, err := l.checkoutRefreshTarget(ctx, checkoutID, expectedRoot)
	if err != nil {
		return nil, err
	}
	defer cancel()
	request, err := c.captureCheckoutRefreshFrom(ctx, checkout, rootInfo, "", sample)
	if err != nil {
		return nil, err
	}
	return c.enqueueCheckoutRefresh(request, false)
}

// checkoutRefreshTarget resolves and validates the live coordinator an
// explicit refresh of checkoutID at expectedRoot is admitted to, and returns
// the admission's bounded context (capture timeout, coordinator lifetime).
// The returned cancel releases it; it is set only on success.
func (l *CheckoutLifecycle) checkoutRefreshTarget(ctx context.Context, checkoutID, expectedRoot string) (
	context.Context, *CheckoutCoordinator, store_sqlite.Checkout, os.FileInfo, context.CancelFunc, error,
) {
	if l == nil || l.catalog == nil || checkoutID == "" || expectedRoot == "" {
		return nil, nil, store_sqlite.Checkout{}, nil, nil, fmt.Errorf("%w: checkout identity and root are required", ErrCheckoutMutationStale)
	}
	if ctx == nil {
		ctx = context.Background()
	}
	checkout, found, err := l.catalog.GetCheckout(ctx, checkoutID)
	if err != nil {
		return nil, nil, checkout, nil, nil, err
	}
	if !found || !sameMutationRoot(checkout.RootPath, expectedRoot) {
		return nil, nil, checkout, nil, nil, fmt.Errorf("%w: checkout root changed", ErrCheckoutMutationStale)
	}
	rootInfo, err := checkoutRootFileInfo(checkout.RootPath)
	if err != nil || !rootInfo.IsDir() {
		return nil, nil, checkout, nil, nil, fmt.Errorf("%w: checkout root is unavailable", ErrCheckoutMutationStale)
	}
	l.coordMu.Lock()
	c, closing := l.coordinators[checkoutID], l.coordinatorClosing
	l.coordMu.Unlock()
	if closing {
		return nil, nil, checkout, nil, nil, ErrCheckoutRefreshStopped
	}
	if c == nil {
		l.ActivateCheckout(checkoutID, "explicit checkout refresh requested")
		return nil, nil, checkout, nil, nil, fmt.Errorf("%w: checkout coordinator is activating; retry recovery", ErrCheckoutMutationBusy)
	}
	if !sameMutationRoot(c.root, expectedRoot) {
		return nil, nil, checkout, nil, nil, ErrCheckoutRefreshStopped
	}
	bounded, cancel := context.WithTimeout(ctx, checkoutRefreshCaptureTimeout)
	bounded, cancelLifetime := checkoutMutationContext(bounded, c.lifetimeContext())
	release := func() { cancelLifetime(); cancel() }
	identity := &CheckoutMutation{coordinator: c, checkout: checkout, rootInfo: rootInfo}
	if err := identity.validateCheckout(bounded); err != nil {
		release()
		return nil, nil, checkout, nil, nil, err
	}
	return bounded, c, checkout, rootInfo, release, nil
}

// requestBoundCheckoutRefresh is RequestCheckoutRefresh admitting the ticket
// without a working-copy sample (bindAtCompletion, HEAD unbound): the caller's
// promise — the route describes the working copy at some instant after its
// request arrived — is exactly what the completion check proves with the
// first sample begun after admission. A ticket admitted while the checkout's
// build is in flight is then completed by that build's own pre-publish sample
// when it began after the admission (completeCheckoutRefreshTickets), instead
// of by a further cycle.
func (l *CheckoutLifecycle) requestBoundCheckoutRefresh(ctx context.Context, checkoutID, expectedRoot string) (*CheckoutRefreshTicket, error) {
	ctx, c, checkout, rootInfo, cancel, err := l.checkoutRefreshTarget(ctx, checkoutID, expectedRoot)
	if err != nil {
		return nil, err
	}
	defer cancel()
	request, err := c.captureBoundCheckoutRefresh(ctx, checkout, rootInfo)
	if err != nil {
		return nil, err
	}
	return c.enqueueCheckoutRefresh(request, false)
}

// captureBoundCheckoutRefresh captures a root ticket without sampling the
// working copy (bindAtCompletion, headUnbound): the first sample begun after
// its admission decides it.
func (c *CheckoutCoordinator) captureBoundCheckoutRefresh(
	ctx context.Context, checkout store_sqlite.Checkout, rootInfo os.FileInfo,
) (*checkoutRefreshRequest, error) {
	request := &checkoutRefreshRequest{
		checkout: checkout, rootInfo: rootInfo, record: publicationRecordFrom(ctx),
		bindAtCompletion: true, headUnbound: true,
	}
	identity := &CheckoutMutation{coordinator: c, checkout: checkout, rootInfo: rootInfo}
	if err := identity.validateCheckout(ctx); err != nil {
		return nil, err
	}
	request.done = make(chan MutationResult, 1)
	request.ticket = &CheckoutRefreshTicket{
		CheckoutID: checkout.CheckoutID, Incarnation: checkout.Incarnation,
		Root: checkout.RootPath, RepoPrefix: c.repoPrefix,
		Ticket: &MutationTicket{Path: checkout.RootPath, Done: request.done},
	}
	return request, nil
}

func (c *CheckoutCoordinator) captureCheckoutRefresh(ctx context.Context, checkout store_sqlite.Checkout, rootInfo os.FileInfo, path string) (*checkoutRefreshRequest, error) {
	return c.captureCheckoutRefreshFrom(ctx, checkout, rootInfo, path, nil)
}

// captureCheckoutRefreshFrom captures a ticket against given, or against a
// new sample when given is nil.
func (c *CheckoutCoordinator) captureCheckoutRefreshFrom(
	ctx context.Context, checkout store_sqlite.Checkout, rootInfo os.FileInfo, path string, given *gitstate.DirtySnapshot,
) (*checkoutRefreshRequest, error) {
	request := &checkoutRefreshRequest{checkout: checkout, rootInfo: rootInfo, record: publicationRecordFrom(ctx)}
	if path != "" {
		canonical, hash, err := checkoutRefreshFileHash(ctx, checkout.RootPath, rootInfo, path)
		if err != nil {
			return nil, err
		}
		path, request.contentHash = canonical, hash
	}
	var sample gitstate.DirtySnapshot
	if given != nil {
		sample = *given
	} else {
		var err error
		if sample, request.freshAfter, err = c.sampler.SampleSinceStarted(ctx, time.Now()); err != nil {
			return nil, err
		}
	}
	request.headRef, request.headCommit, request.headTree = sample.HeadRef, sample.HeadCommit, sample.HeadTree
	request.fingerprint = sample.Fingerprint
	if path == "" {
		path = checkout.RootPath
	} else {
		// Bind the snapshot and bytes together. An external edit during capture
		// must not turn a receipt for old content into a promise about new bytes.
		_, hash, err := checkoutRefreshFileHash(ctx, checkout.RootPath, rootInfo, path)
		if err != nil {
			return nil, err
		}
		if hash != request.contentHash {
			return nil, ErrCheckoutRefreshSuperseded
		}
	}
	identity := &CheckoutMutation{coordinator: c, checkout: checkout, rootInfo: rootInfo}
	if err := identity.validateCheckout(ctx); err != nil {
		return nil, err
	}
	request.done = make(chan MutationResult, 1)
	request.ticket = &CheckoutRefreshTicket{
		CheckoutID: checkout.CheckoutID, Incarnation: checkout.Incarnation,
		Root: checkout.RootPath, RepoPrefix: c.repoPrefix, ContentHash: request.contentHash,
		Ticket: &MutationTicket{Path: path, Done: request.done},
	}
	return request, nil
}

func (c *CheckoutCoordinator) reserveCheckoutRefresh() error {
	c.refreshMu.Lock()
	defer c.refreshMu.Unlock()
	if c.refreshClosed || c.lifetimeContext().Err() != nil {
		return ErrCheckoutRefreshStopped
	}
	if len(c.refreshWaiters)+c.refreshReserved >= maxCheckoutRefreshTickets {
		return ErrCheckoutRefreshQueueFull
	}
	c.refreshReserved++
	return nil
}

func (c *CheckoutCoordinator) releaseCheckoutRefreshReservation() {
	c.refreshMu.Lock()
	c.refreshReserved--
	c.refreshMu.Unlock()
}

func (c *CheckoutCoordinator) enqueueCheckoutRefresh(request *checkoutRefreshRequest, reserved bool) (*CheckoutRefreshTicket, error) {
	c.refreshMu.Lock()
	if reserved {
		c.refreshReserved--
	}
	if c.refreshClosed || c.lifetimeContext().Err() != nil {
		c.refreshMu.Unlock()
		return nil, ErrCheckoutRefreshStopped
	}
	if !reserved && len(c.refreshWaiters)+c.refreshReserved >= maxCheckoutRefreshTickets {
		c.refreshMu.Unlock()
		return nil, ErrCheckoutRefreshQueueFull
	}
	if c.refreshWaiters == nil {
		c.refreshWaiters = make(map[uint64]*checkoutRefreshRequest)
	}
	sequence := checkoutRefreshSequence.Add(1)
	request.ticket.Ticket.Generation = sequence
	request.admittedAt = time.Now()
	if request.freshAfter.IsZero() {
		request.freshAfter = request.admittedAt
	}
	c.ticketDemand.Store(request.admittedAt.UnixNano())
	c.refreshHighWater = sequence
	c.refreshWaiters[sequence] = request
	request.releaseWrite = c.announceTicketWrite()
	// Bind before the wake: the cycle SignalDemand starts marks every record
	// bound at or below its high-water mark, and one bound after this call
	// returns could miss cycle_started and admitted.
	if request.record != nil {
		request.record.BindTicket(sequence)
		request.record.MarkAt(PublicationTicketEnqueued, request.admittedAt)
	}
	c.refreshMu.Unlock()
	// A ticket is demand: its cycle starts now rather than after a quiet
	// window. It still completes only through completeCheckoutRefreshTickets'
	// verification against a sample taken after it was admitted.
	// A ticket is a use: the cycle it demands applies a pending base
	// advance (checkout_propagation.go). Recorded before the selection below,
	// which would otherwise signal a second window for it.
	c.wantRebase("refresh ticket", false)
	c.SignalDemand("checkout refresh ticket admitted")
	// A cycle of this checkout already queued for the build lane at
	// background priority (a filesystem change the loop found first) now
	// serves a waiting caller: promote it to the interactive queue rather
	// than leave it behind other checkouts' background work.
	c.PrioritizeSelection()
	return request.ticket, nil
}

func (c *CheckoutCoordinator) checkoutRefreshHighWater() uint64 {
	c.refreshMu.Lock()
	defer c.refreshMu.Unlock()
	if len(c.refreshWaiters) == 0 {
		return 0
	}
	return c.refreshHighWater
}

func (c *CheckoutCoordinator) reportCheckoutCycle(ctx context.Context, through uint64, out CheckoutCycle) {
	c.completeCheckoutRefreshTickets(ctx, through, out)
	finishObservedChangeRecord(ctx, out)
	if c.cycleDone != nil {
		c.cycleDone(out)
	}
}

// Moving publication out of the MCP request must not lose its operational
// storage-panic firewall. Only typed storage faults are recovered; programmer
// and parser panics still propagate, rather than hiding corrupted execution.
func (c *CheckoutCoordinator) guardCheckoutRefreshCycle(ctx context.Context, through uint64) {
	recovered := recover()
	if recovered == nil {
		return
	}
	err, ok := watcherStoragePanicError("checkout refresh", recovered)
	if !ok {
		panic(recovered)
	}
	if c.logger != nil {
		c.logger.Warn("checkout coordinator: storage failure", zap.String("checkout", c.checkoutID), zap.Error(err))
	}
	c.completeCheckoutRefreshTickets(ctx, through, CheckoutCycle{Err: err})
}

// completeCheckoutRefreshTickets observes a real loop result. A ticket admitted
// during a cycle cannot inherit that earlier cycle's error or stale success;
// its signal schedules a subsequent cycle (usually the cheap settled path).
//
// A successful publication completes every ticket the completion sample was
// taken after, not only the ones admitted before the cycle started: a ticket
// admitted while the build was in flight (a require_fresh request that found
// the route withdrawn by the edit it waits for, say) is answered by the
// build's own pre-publish sample when that sample's git status began after the
// ticket was admitted and it equals the published generation — the same
// guarantee a further cycle's sample would give, without the cycle.
func (c *CheckoutCoordinator) completeCheckoutRefreshTickets(ctx context.Context, through uint64, out CheckoutCycle) {
	c.refreshMu.Lock()
	owed := make([]*checkoutRefreshRequest, 0, len(c.refreshWaiters))
	var riders []*checkoutRefreshRequest
	for sequence, request := range c.refreshWaiters {
		if sequence <= through {
			owed = append(owed, request)
		} else {
			riders = append(riders, request)
		}
	}
	c.refreshMu.Unlock()
	if len(owed) == 0 && len(riders) == 0 {
		return
	}
	if c.lifetimeContext().Err() != nil {
		c.failCheckoutRefreshRequests(owed, ErrCheckoutRefreshStopped)
		return
	}
	if out.Deferred || out.Rescheduled {
		return
	}
	if out.Err != nil {
		if retryableCheckoutRefreshError(out.Err) && c.lifetimeContext().Err() == nil {
			return
		}
		for _, request := range owed {
			c.finishCheckoutRefresh(request, 0, out.Err)
		}
		return
	}
	if out.CommitGenerationID <= 0 || out.DirtyGenerationID <= 0 {
		return
	}
	route, found, err := c.catalog.GetCheckoutRoute(ctx, c.checkoutID)
	if err != nil {
		c.failCheckoutRefreshRequests(owed, err)
		return
	}
	if !found || route.State != store_sqlite.RouteActive || route.CommitGenerationID != out.CommitGenerationID || route.DirtyGenerationID != out.DirtyGenerationID {
		return
	}
	dirty, found, err := c.catalog.GetViewGeneration(ctx, out.DirtyGenerationID)
	if err != nil {
		c.failCheckoutRefreshRequests(owed, err)
		return
	}
	if !found || !servableGeneration(dirty.State) {
		return
	}
	// Rooted at the routed commit generation, directly or through a chain.
	rooted, err := c.dirtyRootedAt(ctx, dirty, out.CommitGenerationID)
	if err != nil {
		c.failCheckoutRefreshRequests(owed, err)
		return
	}
	if !rooted {
		return
	}
	// A ticket completes against a sample whose git status began at or after
	// its freshAfter (after the request it answers arrived, after an edit's
	// disk commit). The newest such sample is taken for the tickets the
	// cycle owes — usually its own pre-publish fence, or the ticket's capture
	// sample for a cycle that settled on it — and a cycle that started
	// nowhere (a direct caller) samples afresh. A ticket admitted while the
	// cycle ran rides on that same sample when it began after the ticket's
	// freshAfter; no sample is ever taken for such a ticket alone.
	var sample gitstate.DirtySnapshot
	var sampleStarted time.Time
	if len(owed) > 0 {
		since := time.Now()
		if !out.cycleStarted.IsZero() {
			since = time.Time{}
			for _, request := range owed {
				if request.freshAfter.After(since) {
					since = request.freshAfter
				}
			}
		}
		sample, sampleStarted, err = c.sampler.SampleSinceStarted(ctx, since)
		if err != nil {
			c.failCheckoutRefreshRequests(owed, err)
			return
		}
	} else {
		earliest := riders[0].freshAfter
		for _, request := range riders[1:] {
			if request.freshAfter.Before(earliest) {
				earliest = request.freshAfter
			}
		}
		var ok bool
		if sample, sampleStarted, ok = c.sampler.LatestSampleSince(earliest); !ok {
			return
		}
	}
	requests := owed
	for _, request := range riders {
		if !sampleStarted.Before(request.freshAfter) {
			requests = append(requests, request)
		}
	}
	if len(requests) == 0 {
		return
	}
	// A ticket that rode this cycle joined a build already past its lane
	// admission: on its record the cycle and the lane are reached the
	// instant it was admitted (no wait of its own), so its phases stay
	// ordered and attribute no lane wait to it. First-wins marks leave any
	// phase the cycle already marked alone.
	for _, request := range requests[len(owed):] {
		if request.record != nil {
			request.record.MarkAt(PublicationCycleStarted, request.admittedAt)
			request.record.MarkAt(PublicationAdmitted, request.admittedAt)
		}
	}
	if sample.Fingerprint != dirty.LowerViewFingerprint {
		return
	}
	current, found, err := c.catalog.GetCheckout(ctx, c.checkoutID)
	if err != nil {
		c.failCheckoutRefreshRequests(requests, err)
		return
	}
	if !found || current.State != store_sqlite.CheckoutStateReady || current.EffectiveMode != store_sqlite.CheckoutModeAutomatic || current.DesiredMode != store_sqlite.CheckoutModeAutomatic || current.ActiveIntentTransitionID != "" || current.UnavailableSince != 0 || current.RemovalDetectedAt != 0 || !sameMutationRoot(current.RootPath, c.root) {
		c.failCheckoutRefreshRequests(requests, ErrCheckoutRefreshSuperseded)
		return
	}
	rootInfo, err := checkoutRootFileInfo(current.RootPath)
	if err != nil {
		c.failCheckoutRefreshRequests(requests, ErrCheckoutRefreshSuperseded)
		return
	}
	// A coalesced cycle hashes each requested file at most once, even when many
	// callers are waiting for the same content.
	hashes := make(map[string]string)
	for _, request := range requests {
		if current.Incarnation != request.checkout.Incarnation || !os.SameFile(request.rootInfo, rootInfo) {
			c.finishCheckoutRefresh(request, 0, ErrCheckoutRefreshSuperseded)
			continue
		}
		if !request.headUnbound && (request.headRef != sample.HeadRef || request.headCommit != sample.HeadCommit || request.headTree != sample.HeadTree) {
			c.finishCheckoutRefresh(request, 0, ErrCheckoutRefreshSuperseded)
			continue
		}
		// The graph's persisted publication fingerprint, not a later disk hash
		// alone, must identify the state admitted by this ticket. Until the
		// builder exposes per-file publication receipts, even an unrelated
		// intervening edit explicitly supersedes this exact-snapshot promise.
		// A ticket bound at completion promised the first state a sample
		// begun after its admission shows, which is this one.
		if !request.bindAtCompletion && request.fingerprint != dirty.LowerViewFingerprint {
			c.finishCheckoutRefresh(request, 0, ErrCheckoutRefreshSuperseded)
			continue
		}
		if request.contentHash != "" {
			path := request.ticket.Ticket.Path
			hash, ok := hashes[path]
			if !ok {
				_, hash, err = checkoutRefreshFileHash(ctx, current.RootPath, rootInfo, path)
				if err != nil {
					c.finishCheckoutRefresh(request, 0, fmt.Errorf("%w: %v", ErrCheckoutRefreshSuperseded, err))
					continue
				}
				hashes[path] = hash
			}
			if hash != request.contentHash {
				c.finishCheckoutRefresh(request, 0, ErrCheckoutRefreshSuperseded)
				continue
			}
		}
		c.finishCheckoutRefresh(request, uint64(out.DirtyGenerationID), nil)
	}
}

func (c *CheckoutCoordinator) finishCheckoutRefresh(request *checkoutRefreshRequest, generation uint64, err error) {
	c.refreshMu.Lock()
	sequence := request.ticket.Ticket.Generation
	if c.refreshWaiters[sequence] != request {
		c.refreshMu.Unlock()
		return
	}
	delete(c.refreshWaiters, sequence)
	c.refreshMu.Unlock()
	if request.releaseWrite != nil {
		request.releaseWrite()
	}
	// The coordinator's own completion instant, on the record the ticket was
	// admitted with (first-wins: the caller's later mark of the same phase,
	// taken when it receives the result, is a no-op). A failure is left to
	// the caller to mark: ticket_failed is terminal, and a caller may retry a
	// superseded ticket on the same record.
	if request.record != nil && err == nil {
		request.record.SetGeneration(int64(generation))
		request.record.Mark(PublicationTicketCompleted)
	}
	request.done <- MutationResult{RequestedGeneration: sequence, AppliedGeneration: generation, Reindexed: err == nil, Err: err}
	close(request.done)
}

func (c *CheckoutCoordinator) failCheckoutRefreshRequests(requests []*checkoutRefreshRequest, err error) {
	if retryableCheckoutRefreshError(err) && c.lifetimeContext().Err() == nil {
		return
	}
	for _, request := range requests {
		c.finishCheckoutRefresh(request, 0, err)
	}
}

func retryableCheckoutRefreshError(err error) bool {
	var committed interface{ Committed() bool }
	if errors.As(err, &committed) && committed.Committed() {
		return false
	}
	if retryableMutationError(err) || errors.Is(err, ErrViewBuildQueueFull) {
		return true
	}
	// SQLite's extended BUSY/LOCKED variants retain the same primary code.
	// errors.As also reaches driver errors wrapped by typed storage failures.
	var coded interface{ Code() int }
	if errors.As(err, &coded) {
		code := coded.Code() & 0xff
		return code == 5 || code == 6
	}
	return false
}

func (c *CheckoutCoordinator) closeCheckoutRefreshTickets() {
	c.refreshMu.Lock()
	c.refreshClosed = true
	requests := make([]*checkoutRefreshRequest, 0, len(c.refreshWaiters))
	for _, request := range c.refreshWaiters {
		requests = append(requests, request)
	}
	c.refreshMu.Unlock()
	for _, request := range requests {
		c.finishCheckoutRefresh(request, 0, ErrCheckoutRefreshStopped)
	}
}

// checkoutRefreshFileHash confines ticket content reads to the captured physical
// checkout, including aliases and case-normalized paths. os.Root keeps a later
// symlink swap from escaping the selected working copy.
func checkoutRefreshFileHash(ctx context.Context, root string, rootInfo os.FileInfo, path string) (string, string, error) {
	root = pathkey.CanonicalExistingRoot(root)
	path = pathkey.CanonicalPath(path)
	if !filepath.IsAbs(path) || !pathkey.HasPathPrefix(path, root) || pathkey.EqualPaths(path, root) {
		return "", "", fmt.Errorf("checkout refresh path is outside selected checkout: %q", path)
	}
	var components []string
	ancestor := path
	for !pathkey.EqualPaths(ancestor, root) {
		component := filepath.Base(ancestor)
		if strings.EqualFold(component, ".git") {
			return "", "", fmt.Errorf("checkout refresh cannot read Git metadata: %q", path)
		}
		components = append(components, component)
		parent := filepath.Dir(ancestor)
		if pathkey.EqualPaths(parent, ancestor) {
			return "", "", fmt.Errorf("checkout refresh path has no selected root: %q", path)
		}
		ancestor = parent
	}
	matched, err := checkoutRootFileInfo(ancestor)
	if err != nil || !os.SameFile(rootInfo, matched) {
		return "", "", ErrCheckoutRefreshSuperseded
	}
	slices.Reverse(components)
	rooted, err := os.OpenRoot(root)
	if err != nil {
		return "", "", err
	}
	defer rooted.Close()
	rootFile, err := rooted.Open(".")
	if err != nil {
		return "", "", ErrCheckoutRefreshSuperseded
	}
	openedRoot, err := rootFile.Stat()
	closeErr := rootFile.Close()
	if err != nil || closeErr != nil || !os.SameFile(rootInfo, openedRoot) {
		return "", "", ErrCheckoutRefreshSuperseded
	}
	file, err := rooted.Open(filepath.Join(components...))
	if err != nil {
		return "", "", err
	}
	defer file.Close()
	before, err := file.Stat()
	if err != nil {
		return "", "", err
	}
	if !before.Mode().IsRegular() {
		return "", "", fmt.Errorf("checkout refresh target is not a regular file: %q", path)
	}
	hash := sha256.New()
	buffer := make([]byte, 32*1024)
	for {
		if err := ctx.Err(); err != nil {
			return "", "", err
		}
		n, readErr := file.Read(buffer)
		if n > 0 {
			_, _ = hash.Write(buffer[:n])
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return "", "", readErr
		}
	}
	after, err := file.Stat()
	if err != nil {
		return "", "", err
	}
	if before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) {
		return "", "", ErrCheckoutRefreshSuperseded
	}
	return path, hex.EncodeToString(hash.Sum(nil)), nil
}

// announceTicketWrite announces a waiting ticket's mutation to the store (see
// checkoutRefreshRequest.releaseWrite); announceWrite is the test seam.
func (c *CheckoutCoordinator) announceTicketWrite() func() {
	if c.announceWrite != nil {
		return c.announceWrite()
	}
	if c.store == nil {
		return nil
	}
	return c.store.AnnounceCheckoutRefresh()
}
