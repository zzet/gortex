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
	contentHash string
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
	request, err := m.coordinator.captureCheckoutRefresh(ctx, m.checkout, m.rootInfo, path)
	if err != nil {
		return nil, err
	}
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
	if l == nil || l.catalog == nil || checkoutID == "" || expectedRoot == "" {
		return nil, fmt.Errorf("%w: checkout identity and root are required", ErrCheckoutMutationStale)
	}
	if ctx == nil {
		ctx = context.Background()
	}
	checkout, found, err := l.catalog.GetCheckout(ctx, checkoutID)
	if err != nil {
		return nil, err
	}
	if !found || !sameMutationRoot(checkout.RootPath, expectedRoot) {
		return nil, fmt.Errorf("%w: checkout root changed", ErrCheckoutMutationStale)
	}
	rootInfo, err := checkoutRootFileInfo(checkout.RootPath)
	if err != nil || !rootInfo.IsDir() {
		return nil, fmt.Errorf("%w: checkout root is unavailable", ErrCheckoutMutationStale)
	}
	l.coordMu.Lock()
	c, closing := l.coordinators[checkoutID], l.coordinatorClosing
	l.coordMu.Unlock()
	if closing {
		return nil, ErrCheckoutRefreshStopped
	}
	if c == nil {
		l.ActivateCheckout(checkoutID, "explicit checkout refresh requested")
		return nil, fmt.Errorf("%w: checkout coordinator is activating; retry recovery", ErrCheckoutMutationBusy)
	}
	if !sameMutationRoot(c.root, expectedRoot) {
		return nil, ErrCheckoutRefreshStopped
	}
	ctx, cancel := context.WithTimeout(ctx, checkoutRefreshCaptureTimeout)
	defer cancel()
	ctx, cancelLifetime := checkoutMutationContext(ctx, c.lifetimeContext())
	defer cancelLifetime()
	identity := &CheckoutMutation{coordinator: c, checkout: checkout, rootInfo: rootInfo}
	if err := identity.validateCheckout(ctx); err != nil {
		return nil, err
	}
	request, err := c.captureCheckoutRefresh(ctx, checkout, rootInfo, "")
	if err != nil {
		return nil, err
	}
	return c.enqueueCheckoutRefresh(request, false)
}

func (c *CheckoutCoordinator) captureCheckoutRefresh(ctx context.Context, checkout store_sqlite.Checkout, rootInfo os.FileInfo, path string) (*checkoutRefreshRequest, error) {
	request := &checkoutRefreshRequest{checkout: checkout, rootInfo: rootInfo}
	if path != "" {
		canonical, hash, err := checkoutRefreshFileHash(ctx, checkout.RootPath, rootInfo, path)
		if err != nil {
			return nil, err
		}
		path, request.contentHash = canonical, hash
	}
	sample, err := c.sampler.Sample(ctx)
	if err != nil {
		return nil, err
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
	c.refreshHighWater = sequence
	c.refreshWaiters[sequence] = request
	c.refreshMu.Unlock()
	c.Signal("checkout refresh ticket admitted")
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
func (c *CheckoutCoordinator) completeCheckoutRefreshTickets(ctx context.Context, through uint64, out CheckoutCycle) {
	c.refreshMu.Lock()
	requests := make([]*checkoutRefreshRequest, 0, len(c.refreshWaiters))
	for sequence, request := range c.refreshWaiters {
		if sequence <= through {
			requests = append(requests, request)
		}
	}
	c.refreshMu.Unlock()
	if len(requests) == 0 {
		return
	}
	if c.lifetimeContext().Err() != nil {
		c.failCheckoutRefreshRequests(requests, ErrCheckoutRefreshStopped)
		return
	}
	if out.Deferred || out.Rescheduled {
		return
	}
	if out.Err != nil {
		if retryableCheckoutRefreshError(out.Err) && c.lifetimeContext().Err() == nil {
			return
		}
		for _, request := range requests {
			c.finishCheckoutRefresh(request, 0, out.Err)
		}
		return
	}
	if out.CommitGenerationID <= 0 || out.DirtyGenerationID <= 0 {
		return
	}
	route, found, err := c.catalog.GetCheckoutRoute(ctx, c.checkoutID)
	if err != nil {
		c.failCheckoutRefreshRequests(requests, err)
		return
	}
	if !found || route.State != store_sqlite.RouteActive || route.CommitGenerationID != out.CommitGenerationID || route.DirtyGenerationID != out.DirtyGenerationID {
		return
	}
	dirty, found, err := c.catalog.GetViewGeneration(ctx, out.DirtyGenerationID)
	if err != nil {
		c.failCheckoutRefreshRequests(requests, err)
		return
	}
	if !found || !servableGeneration(dirty.State) || dirty.BaseGenerationID != out.CommitGenerationID {
		return
	}
	sample, err := c.sampler.Sample(ctx)
	if err != nil {
		c.failCheckoutRefreshRequests(requests, err)
		return
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
		if current.Incarnation != request.checkout.Incarnation || !os.SameFile(request.rootInfo, rootInfo) || request.headRef != sample.HeadRef || request.headCommit != sample.HeadCommit || request.headTree != sample.HeadTree {
			c.finishCheckoutRefresh(request, 0, ErrCheckoutRefreshSuperseded)
			continue
		}
		// The graph's persisted publication fingerprint, not a later disk hash
		// alone, must identify the state admitted by this ticket. Until the
		// builder exposes per-file publication receipts, even an unrelated
		// intervening edit explicitly supersedes this exact-snapshot promise.
		if request.fingerprint != dirty.LowerViewFingerprint {
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
