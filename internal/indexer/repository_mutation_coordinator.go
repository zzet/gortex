package indexer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"sync"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/graphview"
)

const repositoryMutationPathCap = 2048

var errRepositoryMutationCoordinatorClosed = errors.New("repository mutation coordinator is closed")

type repositoryMutationExecutor func(paths []string) (*IndexResult, error)

type repositoryMutationScope struct {
	full  bool
	paths map[string]struct{}
}

func (s *repositoryMutationScope) merge(paths []string) error {
	// Explicit paths are validated before any shared scope mutation. Even a
	// pending full request must not let a malformed waiter join its outcome.
	for _, path := range paths {
		if path == "" {
			return errEmptyRepositoryMutationPath
		}
	}
	if s.full {
		return nil
	}
	if len(paths) == 0 {
		s.full = true
		s.paths = nil
		return nil
	}
	if s.paths == nil {
		s.paths = make(map[string]struct{}, len(paths))
	}
	for _, path := range paths {
		s.paths[filepath.Clean(path)] = struct{}{}
		if len(s.paths) > repositoryMutationPathCap {
			s.full = true
			s.paths = nil
			return nil
		}
	}
	return nil
}

func (s *repositoryMutationScope) take() []string {
	if s.full {
		s.full = false
		s.paths = nil
		return nil
	}
	paths := make([]string, 0, len(s.paths))
	for path := range s.paths {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	s.paths = nil
	return paths
}

type repositoryMutationOutcome struct {
	result *IndexResult
	err    error
}

type repositoryMutationWaiter struct {
	generation uint64
	done       chan repositoryMutationOutcome
}

// repositoryMutationCoordinator is the stable single mutation lane for one repository.
// Reconcile requests coalesce while queued: path scopes union, a full request
// dominates, and requests admitted during execution advance the dirty
// generation and force another pass before the worker becomes idle. Point
// mutations use runExclusive and share the same lane without being folded
// together, preserving watcher receipts and callbacks.
type repositoryMutationCoordinator struct {
	mu sync.Mutex

	lane       chan struct{}
	work       sync.WaitGroup
	closeDrain chan struct{}

	// batchMutationGate is shared by every stable repository lane owned by a
	// MultiIndexer. Admission takes the read side before a lane: a queued batch
	// writer can then never own the gate while a mutation owns a lane and waits
	// for that same gate. The read side stays held through the complete pipeline.
	batchMutationGate *sync.RWMutex
	// batchAdmissionHook is a deterministic test observation point. Production
	// coordinators leave it nil; tests set it before admitting concurrent work.
	batchAdmissionHook func()
	executor           repositoryMutationExecutor
	// outputGeneration wraps ONE executed coalesced batch in an
	// output-generation receipt. The coalescing lane, not the queuing caller,
	// is the mutation entry point here: many callers merge into one execution,
	// and it is that execution that names an output generation and owner. The
	// batch reports the source content it moved through the observer it is
	// handed, which is what keeps the gen0 source fingerprint content-derived.
	outputGeneration func(fn func(observe func(*OutputSourceContent)) error) error
	// authorityRequired marks a lane minted by a PRODUCTION factory. Such a
	// lane must never execute a coalesced batch with no receipt: an unbound
	// lane fails closed rather than writing generation zero unfenced. Lanes
	// hand-built by fixtures leave it false and keep running unfenced.
	authorityRequired bool
	closed            bool
	running           bool

	requestedGeneration uint64
	completedGeneration uint64
	pending             repositoryMutationScope
	waiters             []*repositoryMutationWaiter
}

func newRepositoryMutationCoordinator(executor repositoryMutationExecutor) *repositoryMutationCoordinator {
	lane := make(chan struct{}, 1)
	lane <- struct{}{}
	return &repositoryMutationCoordinator{lane: lane, executor: executor}
}

func (c *repositoryMutationCoordinator) reconcile(
	ctx context.Context,
	paths []string,
) (*IndexResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	waiter := &repositoryMutationWaiter{done: make(chan repositoryMutationOutcome, 1)}
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil, errRepositoryMutationCoordinatorClosed
	}
	if err := c.pending.merge(paths); err != nil {
		c.mu.Unlock()
		return nil, err
	}
	c.requestedGeneration++
	waiter.generation = c.requestedGeneration
	c.waiters = append(c.waiters, waiter)
	if !c.running {
		c.running = true
		c.work.Add(1)
		go c.drain()
	}
	c.mu.Unlock()

	select {
	case outcome := <-waiter.done:
		return outcome.result, outcome.err
	case <-ctx.Done():
		// Admission is durable. Cancellation stops this caller waiting but does
		// not abandon disk reconciliation or the dirty follow-up generation.
		return nil, ctx.Err()
	}
}

func (c *repositoryMutationCoordinator) drain() {
	defer c.work.Done()
	for {
		// Admit through the shared batch generation before taking the stable
		// repository lane. Requests admitted while a point mutation owns the lane
		// still coalesce below, but a queued batch writer can no longer take the
		// gate and then deadlock waiting for a lane held by this drain.
		c.mu.Lock()
		batchMutationGate := c.batchMutationGate
		c.mu.Unlock()
		if batchMutationGate != nil {
			batchMutationGate.RLock()
		}
		<-c.lane

		c.mu.Lock()
		if len(c.waiters) == 0 {
			c.running = false
			c.mu.Unlock()
			c.lane <- struct{}{}
			if batchMutationGate != nil {
				batchMutationGate.RUnlock()
			}
			return
		}
		generation := c.requestedGeneration
		paths := c.pending.take()
		waiters := c.waiters
		c.waiters = nil
		executor := c.executor
		outputGeneration := c.outputGeneration
		authorityRequired := c.authorityRequired
		c.mu.Unlock()

		outcome := executeRepositoryMutationUnderAuthority(
			outputGeneration, authorityRequired, executor, paths)
		c.lane <- struct{}{}
		if batchMutationGate != nil {
			batchMutationGate.RUnlock()
		}

		c.mu.Lock()
		if generation > c.completedGeneration {
			c.completedGeneration = generation
		}
		c.mu.Unlock()
		for _, waiter := range waiters {
			waiter.done <- outcome
		}
	}
}

func executeRepositoryMutation(executor repositoryMutationExecutor, paths []string) (outcome repositoryMutationOutcome) {
	defer func() {
		recovered := recover()
		if recovered == nil {
			return
		}
		storageErr, ok := store_sqlite.StorageErrorFromPanic(recovered)
		if !ok {
			panic(recovered)
		}
		outcome.result = nil
		outcome.err = fmt.Errorf("repository mutation storage failure: %w", storageErr)
	}()
	if executor == nil {
		outcome.err = errors.New("repository mutation executor is not configured")
		return outcome
	}
	outcome.result, outcome.err = executor(paths)
	return outcome
}

// executeRepositoryMutationUnderAuthority runs one coalesced batch under the
// lane's output-generation receipt. An authority refusal (an unregistered entry
// point, or a receipt a newer mutation for the same owner superseded) becomes
// the batch's error: superseded work must not report a fulfilled reconcile.
//
// The refusal never DISCARDS a result that exists. Preventive refusals happen
// before the executor runs and therefore carry no result to lose; a refusal
// raised after the executor already ran leaves the real result in place beside
// the error, so a coalesced waiter is never handed "nil result, no explanation"
// for work that actually ran. Callers that key on the error (GitWatcher's
// finalizeReconcile leaves the prior SHA for the next notification) still
// retry, which is the correct response to an unfulfilled generation.
// An unbound lane fails CLOSED when the lane was minted by a production
// factory: writing generation zero with no receipt is a mutation nothing speaks
// for, and silently degrading to that is exactly the hole the authority exists
// to close. Fixture lanes (authorityRequired false) keep running unfenced.
func executeRepositoryMutationUnderAuthority(
	outputGeneration func(fn func(observe func(*OutputSourceContent)) error) error,
	authorityRequired bool,
	executor repositoryMutationExecutor,
	paths []string,
) repositoryMutationOutcome {
	if outputGeneration == nil {
		if authorityRequired {
			return repositoryMutationOutcome{err: ErrOutputMutationLaneUnbound}
		}
		return executeRepositoryMutation(executor, paths)
	}
	var outcome repositoryMutationOutcome
	err := outputGeneration(func(observe func(*OutputSourceContent)) error {
		outcome = executeRepositoryMutation(executor, paths)
		// The coalesced batch is the one door that KNOWS what it wrote — the
		// path set it took and the result's own stale/deleted/full-retrack
		// counts — so it is the door that reports content. The reconcile lane
		// is also the watcher-tick path, which is where a revision-derived
		// fingerprint would have told every request the corpus moved on every
		// tick.
		observe(outputSourceContentFor("", paths, outcome.result, outcome.err))
		return outcome.err
	})
	if err != nil && outcome.err == nil {
		outcome.err = err
	}
	return outcome
}

// bindOutputGeneration attaches the receipt wrapper for this lane's coalesced
// executions. It is set from repositoryMutations, the one place that knows both
// the coordinator and the Indexer whose output generation it writes.
func (c *repositoryMutationCoordinator) bindOutputGeneration(
	wrap func(fn func(observe func(*OutputSourceContent)) error) error,
) {
	if c == nil {
		return
	}
	c.mu.Lock()
	c.outputGeneration = wrap
	c.mu.Unlock()
}

func (c *repositoryMutationCoordinator) runExclusive(ctx context.Context, fn func() error) error {
	return c.runExclusiveMode(ctx, true, fn)
}

// runExclusiveLaneOnly acquires only the stable repository lane. A multi-repo
// transition uses it recursively in sorted-prefix order, then acquires the one
// shared batch gate after every lane is held. Calling runExclusive there would
// nest batch read locks and can deadlock behind a queued transition writer.
func (c *repositoryMutationCoordinator) runExclusiveLaneOnly(ctx context.Context, fn func() error) error {
	return c.runExclusiveMode(ctx, false, fn)
}

func (c *repositoryMutationCoordinator) runExclusiveMode(
	ctx context.Context,
	acquireBatchGate bool,
	fn func() error,
) (retErr error) {
	// This boundary is registered before the lane and batch-gate defers, so
	// every admission resource is released before an operational store panic is
	// returned to the watcher. Programmer and runtime panics retain their exact
	// payload and propagation semantics.
	defer func() {
		recovered := recover()
		if recovered == nil {
			return
		}
		storageErr, ok := store_sqlite.StorageErrorFromPanic(recovered)
		if !ok {
			panic(recovered)
		}
		retErr = fmt.Errorf("repository exclusive mutation storage failure: %w", storageErr)
	}()
	if fn == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return errRepositoryMutationCoordinatorClosed
	}
	var batchMutationGate *sync.RWMutex
	if acquireBatchGate {
		batchMutationGate = c.batchMutationGate
	}
	c.work.Add(1)
	c.mu.Unlock()
	defer c.work.Done()

	// Batch admission precedes the repository lane. Otherwise a queued writer
	// can acquire the batch gate while this call owns the lane, then wait for
	// that lane itself — a gate/lane cycle that prevents either side advancing.
	if batchMutationGate != nil {
		batchMutationGate.RLock()
		defer batchMutationGate.RUnlock()
	}
	if hook := c.batchAdmissionHook; hook != nil {
		hook()
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-c.lane:
	}
	defer func() { c.lane <- struct{}{} }()
	if err := ctx.Err(); err != nil {
		return err
	}
	return fn()
}

func (c *repositoryMutationCoordinator) closeAdmission() {
	c.mu.Lock()
	c.closed = true
	c.mu.Unlock()
}

func (c *repositoryMutationCoordinator) wait(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	done := make(chan struct{})
	go func() {
		c.work.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (c *repositoryMutationCoordinator) closeAndWait(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	done := c.closeAndDrain()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

type repositoryMutationCoordinatorStats struct {
	RequestedGeneration uint64
	CompletedGeneration uint64
	Running             bool
	PendingPaths        int
	PendingFull         bool
}

func (c *repositoryMutationCoordinator) stats() repositoryMutationCoordinatorStats {
	c.mu.Lock()
	defer c.mu.Unlock()
	return repositoryMutationCoordinatorStats{
		RequestedGeneration: c.requestedGeneration,
		CompletedGeneration: c.completedGeneration,
		Running:             c.running,
		PendingPaths:        len(c.pending.paths),
		PendingFull:         c.pending.full,
	}
}

func (mi *MultiIndexer) repositoryMutationCoordinator(repoPrefix string) *repositoryMutationCoordinator {
	mi.repositoryMutationMu.Lock()
	defer mi.repositoryMutationMu.Unlock()
	if mi.lifecycleClosed {
		coordinator := newRepositoryMutationCoordinator(func([]string) (*IndexResult, error) {
			return nil, errMultiIndexerClosed
		})
		coordinator.closeAdmission()
		return coordinator
	}
	if mi.repositoryMutations == nil {
		mi.repositoryMutations = make(map[string]*repositoryMutationCoordinator)
	}
	coordinator := mi.repositoryMutations[repoPrefix]
	if coordinator == nil {
		coordinator = newRepositoryMutationCoordinator(func(paths []string) (*IndexResult, error) {
			return mi.incrementalReindexRepoRaw(repoPrefix, paths)
		})
		// Attach before publishing the slot: every execution through this stable
		// lane must participate in the owning MultiIndexer's batch transition.
		coordinator.batchMutationGate = &mi.batchMutationGate
		// A lane minted here is a PRODUCTION lane, so it is bound to the
		// authority here rather than only when an Indexer later attaches to it
		// (attachRepositoryMutationCoordinator / repositoryMutations). Both
		// halves matter: the binding stops a lane reached before any Indexer
		// attaches from reconciling unfenced, and authorityRequired makes an
		// unbound one fail closed instead of degrading silently.
		coordinator.authorityRequired = true
		coordinator.outputGeneration = func(fn func(observe func(*OutputSourceContent)) error) error {
			return mi.withRepositoryOutputGenerationSource(
				context.Background(), repoPrefix, OutputEntryRepositoryReconcileLane, fn)
		}
		mi.repositoryMutations[repoPrefix] = coordinator
	}
	return coordinator
}

// withRepositoryOutputGenerationSource opens a receipt for one tracked
// repository's generation-zero output without going through its Indexer. It is
// the MultiIndexer-level binding of a stable lane, used until (and after) a
// per-repository Indexer attaches its own.
//
// The target is resolved lazily, inside the call: repoRootPath takes the
// registry read lock, and the lane is minted under repositoryMutationMu.
func (mi *MultiIndexer) withRepositoryOutputGenerationSource(
	ctx context.Context,
	repoPrefix string,
	entry OutputMutationEntry,
	fn func(observe func(*OutputSourceContent)) error,
) error {
	if fn == nil {
		return nil
	}
	target := legacyOutputTargetFor(
		outputStoreIdentity(mi.graph), repoPrefix, mi.repoRootPath(repoPrefix), "prefix:"+repoPrefix)
	receipt, err := mi.outputGenerationAuthority().Begin(ctx, entry, target)
	if err != nil {
		return err
	}
	return runUnderOutputReceipt(receipt, func() error {
		return fn(func(content *OutputSourceContent) {
			if content != nil && content.Root == "" {
				content.Root = target.RootPath
			}
			receipt.ObserveSourceContent(content)
		})
	})
}

// withRepositoryMutationLanes acquires a deterministic set of stable lanes.
// The caller must already hold the shared batch gate's read side so batch
// admission precedes every lane. Recursive defers release lanes in reverse
// order, preserving the global sorted-prefix lock order on every exit path.
func (mi *MultiIndexer) withRepositoryMutationLanes(
	ctx context.Context,
	prefixes []string,
	fn func() error,
) error {
	if fn == nil {
		return nil
	}
	ordered := append([]string(nil), prefixes...)
	sort.Strings(ordered)
	compact := ordered[:0]
	for _, prefix := range ordered {
		if len(compact) == 0 || compact[len(compact)-1] != prefix {
			compact = append(compact, prefix)
		}
	}
	coordinators := make([]*repositoryMutationCoordinator, len(compact))
	for i, prefix := range compact {
		coordinators[i] = mi.repositoryMutationCoordinator(prefix)
	}
	var acquire func(int) error
	acquire = func(i int) error {
		if i == len(coordinators) {
			return fn()
		}
		return coordinators[i].runExclusiveLaneOnly(ctx, func() error {
			return acquire(i + 1)
		})
	}
	return acquire(0)
}

// existingRepositoryMutationCoordinator returns the exact coordinator slot
// currently owned by repoPrefix without creating a replacement. Lifecycle
// teardown must use this lookup: a create-if-missing lookup can cross an
// untrack/retrack boundary and close the new repository generation.
func (mi *MultiIndexer) existingRepositoryMutationCoordinator(repoPrefix string) *repositoryMutationCoordinator {
	mi.repositoryMutationMu.Lock()
	defer mi.repositoryMutationMu.Unlock()
	return mi.repositoryMutations[repoPrefix]
}

// attachedRepositoryMutationCoordinator snapshots an Indexer's exact lane
// generation without creating or replacing one. A stale Indexer must never be
// attached to a coordinator minted by a later untrack/retrack generation.
func (idx *Indexer) attachedRepositoryMutationCoordinator() *repositoryMutationCoordinator {
	if idx == nil {
		return nil
	}
	idx.repositoryMutationMu.Lock()
	defer idx.repositoryMutationMu.Unlock()
	return idx.repositoryMutation
}

// repositoryMutationCoordinatorForSnapshot returns the lane already attached
// to an Indexer snapshot. Only direct-map fixtures/restores with no attachment
// take the backstop path; that path creates and attaches a lane while mi.mu
// proves both metadata and Indexer pointers still name the same generation.
func (mi *MultiIndexer) repositoryMutationCoordinatorForSnapshot(
	repoPrefix string,
	expectedMeta *RepoMetadata,
	expectedIdx *Indexer,
) (*repositoryMutationCoordinator, bool) {
	if expectedIdx == nil {
		return nil, false
	}
	if coordinator := expectedIdx.attachedRepositoryMutationCoordinator(); coordinator != nil {
		return coordinator, true
	}

	mi.mu.RLock()
	defer mi.mu.RUnlock()
	if mi.repos[repoPrefix] != expectedMeta || mi.indexers[repoPrefix] != expectedIdx {
		return nil, false
	}
	coordinator := mi.repositoryMutationCoordinator(repoPrefix)
	expectedIdx.attachRepositoryMutationCoordinator(coordinator)
	return coordinator, true
}

// repositoryMutationCoordinatorForTeardownSnapshot atomically backfills the
// prefix-owned lane for legacy/direct-map registry entries that predate stable
// coordinator attachment. Holding mi.mu through slot creation and attachment
// prevents an old teardown from ever borrowing a retracked generation's lane.
func (mi *MultiIndexer) repositoryMutationCoordinatorForTeardownSnapshot(
	repoPrefix string,
	expectedMeta *RepoMetadata,
	expectedIdx *Indexer,
) (*repositoryMutationCoordinator, bool) {
	mi.mu.RLock()
	defer mi.mu.RUnlock()
	if mi.repos[repoPrefix] != expectedMeta || mi.indexers[repoPrefix] != expectedIdx {
		return nil, false
	}
	coordinator := mi.repositoryMutationCoordinator(repoPrefix)
	if expectedIdx != nil {
		expectedIdx.attachRepositoryMutationCoordinator(coordinator)
	}
	return coordinator, true
}

// detachRepositoryMutationCoordinator removes repoPrefix only when the slot is
// still the coordinator this teardown drained. A stale teardown must never
// detach a replacement installed by a later track generation.
func (mi *MultiIndexer) detachRepositoryMutationCoordinator(
	repoPrefix string,
	expected *repositoryMutationCoordinator,
) bool {
	if expected == nil {
		return false
	}
	mi.repositoryMutationMu.Lock()
	defer mi.repositoryMutationMu.Unlock()
	if mi.repositoryMutations[repoPrefix] != expected {
		return false
	}
	delete(mi.repositoryMutations, repoPrefix)
	return true
}

func (idx *Indexer) hasRepositoryMutationCoordinator(expected *repositoryMutationCoordinator) bool {
	if idx == nil || expected == nil {
		return false
	}
	idx.repositoryMutationMu.Lock()
	defer idx.repositoryMutationMu.Unlock()
	return idx.repositoryMutation == expected
}

func (idx *Indexer) attachRepositoryMutationCoordinator(coordinator *repositoryMutationCoordinator) {
	idx.repositoryMutationMu.Lock()
	idx.repositoryMutation = coordinator
	idx.repositoryMutationMu.Unlock()
	// A coordinator becomes this Indexer's lane here as well as in
	// repositoryMutations (SetRepoPrefix attaches the owned lane directly), so
	// the receipt wrapper is bound at BOTH attachment points. A lane reachable
	// with no wrapper is a mutation path with no named output generation.
	idx.bindMutationLaneAuthority(coordinator)
}

// bindMutationLaneAuthority attaches the output-generation receipt wrapper the
// lane worker opens around every coalesced batch it executes.
func (idx *Indexer) bindMutationLaneAuthority(coordinator *repositoryMutationCoordinator) {
	if coordinator == nil {
		return
	}
	coordinator.mu.Lock()
	coordinator.authorityRequired = true
	coordinator.mu.Unlock()
	coordinator.bindOutputGeneration(func(fn func(observe func(*OutputSourceContent)) error) error {
		return idx.withOutputGenerationSource(context.Background(), OutputEntryRepositoryReconcileLane, fn)
	})
}

func (idx *Indexer) ensureRepositoryMutationRoot(root string) error {
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return err
	}
	idx.repositoryMutationMu.Lock()
	defer idx.repositoryMutationMu.Unlock()
	if idx.rootPath == "" {
		idx.storeRootPath(absRoot)
		return nil
	}
	current, err := filepath.Abs(idx.rootPath)
	if err != nil {
		return err
	}
	if filepath.Clean(current) != filepath.Clean(absRoot) {
		return fmt.Errorf("repository mutation root changed from %q to %q", current, absRoot)
	}
	return nil
}

func (idx *Indexer) repositoryMutations() *repositoryMutationCoordinator {
	idx.repositoryMutationMu.Lock()
	defer idx.repositoryMutationMu.Unlock()
	if idx.repositoryMutation == nil {
		if idx.repositoryMutationOwner != nil && idx.repoPrefix != "" {
			idx.repositoryMutation = idx.repositoryMutationOwner.repositoryMutationCoordinator(idx.repoPrefix)
		} else {
			idx.repositoryMutation = newRepositoryMutationCoordinator(func(paths []string) (*IndexResult, error) {
				return idx.incrementalReindexWatcherPaths(idx.rootPath, paths)
			})
		}
		// Both lanes — the MultiIndexer-owned one and the orphan one a
		// standalone Indexer mints — name their output generation through the
		// same authority. Binding here is what stops the orphan lane from
		// being the one mutation path with no named owner.
		idx.bindMutationLaneAuthority(idx.repositoryMutation)
	}
	return idx.repositoryMutation
}

// coordinateRepositoryReindex coalesces a reconciliation onto the stable lane.
//
// It does NOT open a receipt here: reconcile requests coalesce, so the caller
// that queues is not the work that executes. The receipt is opened by the lane
// worker itself (drain -> outputGeneration, bound in repositoryMutations), so
// exactly one receipt covers the one batch that actually mutates.
func (idx *Indexer) coordinateRepositoryReindex(
	ctx context.Context,
	paths []string,
) (*IndexResult, error) {
	return idx.repositoryMutations().reconcile(ctx, paths)
}

// coordinateRepositoryMutation serializes one non-coalescible repository
// mutation with every watcher, reconciliation, and janitor pipeline for this
// Indexer. The callback must use raw mutation methods and must not submit back
// into the coordinator.
//
// entry is the caller's registered identity with the output-generation
// authority. The authority admission is nested INSIDE the lane on purpose:
// receipts for one owner are then totally ordered by execution, and the raw
// source gate is taken after batch admission and the repository lane, which is
// the order graphview's raw mutation API requires.
func (idx *Indexer) coordinateRepositoryMutation(ctx context.Context, entry OutputMutationEntry, fn func() error) error {
	return idx.repositoryMutations().runExclusive(ctx, func() error {
		return idx.withOutputGeneration(ctx, entry, fn)
	})
}

// ---------------------------------------------------------------------------
// Output-generation authority (D12, gate 6)
// ---------------------------------------------------------------------------
//
// coordinateRepositoryMutation and BeginCheckoutMutation are the two lanes a
// production repository mutation reaches the store through. Serializing them
// is not the same as knowing what they write: a lane says "one at a time for
// this repository", it does not say WHICH output generation the work landing
// on it belongs to, nor which owner speaks for that generation. Without that
// second fact there is no mutation receipt, so nothing can refuse superseded
// work that arrives after a newer mutation was admitted for the same owner —
// acceptance gate 6's last clause.
//
// The authority supplies exactly that. Every mutation entry point names itself
// (a registered OutputMutationEntry) and names exactly one output generation
// and owner (an OutputMutationTarget):
//
//   - OutputGenerationLegacy    generation 0, the legacy mutable corpus. The
//                               generation number MUST be 0; the authority
//                               never lets a legacy write claim a committed
//                               generation number, which is the relabelling
//                               refusal expressed at admission time.
//   - OutputGenerationCheckout  the routed dirty/commit generation a checkout
//                               source edit withdraws and republishes.
//   - OutputGenerationDedicated a claimed dedicated generation for the
//                               publication path.
//
// Admission returns a receipt. Receipts for one owner are totally ordered by
// issue sequence; issuing a newer one supersedes every outstanding older one,
// and a superseded receipt cannot be fulfilled. The fence has two halves:
//
//   - PREVENTIVE — runUnderOutputReceipt (and OutputMutationReceipts.Current,
//     and CheckoutMutation.receiptStillCurrent before Prepare/Refresh) refuse a
//     receipt that already lost its authority BEFORE the payload runs, so no
//     byte moves. Admission is not instantaneous — Begin blocks on the source
//     gate — so this window is real, not theoretical.
//   - REPORTING — Complete refuses a receipt superseded WHILE its payload ran.
//     Those bytes are already written; the refusal exists so the work is not
//     reported as a fulfilled generation and the caller retries.
//
// Today the production lanes make the reporting half rare by construction: the
// repository lane serializes generation-zero mutations for one owner, and
// BeginCheckoutMutation holds the checkout's cycle lock from admission through
// Close, so two live receipts for one "checkout:<id>" owner cannot overlap. The
// case the fence exists for is the one the lanes do NOT cover: the standalone
// (orphan-lane) Indexer a stack hands to the MCP server and an owned
// per-repository lane naming the same repository root.
//
// The authority also owns the SOURCE half of the same choke point. A legacy
// mutation changes bytes under generation 0 while requests may be reading it,
// so it is witnessed through graphview's raw-source authority: the mutation
// takes the exclusive data gate for the repository, which moves the owner's
// source revision, and graphview.BasePin.ValidateCurrent — held by a routed
// request for its lifetime — then reports ErrBaseCorpusChanged instead of
// silently claiming exactness. A repository with no registered raw source
// authority stays UNWITNESSED, which BasePin reports as "unknown"; the
// authority never manufactures a witness it did not take.
//
// Plan-bookkeeping obligation B1 — internal/persistence (notes, memories,
// scopes, notebooks, feedback, suppressions, frecency) is a SEPARATE sidecar
// database with no generation axis at all. It is outside this payload
// authority: it is never an output generation, it is never named by a target,
// and repository untrack / generation retirement / payload cleanup must never
// treat it as payload. Neither internal/indexer nor internal/graph/store_sqlite
// imports it, and TestRepositoryCleanupLeavesPersistenceSidecarsAlone pins
// both the import boundary and the on-disk files.

// OutputGenerationKind names which of the three output axes a mutation writes.
type OutputGenerationKind uint8

const (
	// OutputGenerationLegacy is the mutable legacy corpus: generation zero.
	OutputGenerationLegacy OutputGenerationKind = iota + 1
	// OutputGenerationCheckout is a checkout's routed dirty/commit generation.
	OutputGenerationCheckout
	// OutputGenerationDedicated is a claimed dedicated generation.
	OutputGenerationDedicated
)

func (k OutputGenerationKind) String() string {
	switch k {
	case OutputGenerationLegacy:
		return "legacy"
	case OutputGenerationCheckout:
		return "checkout"
	case OutputGenerationDedicated:
		return "dedicated"
	default:
		return "invalid"
	}
}

var (
	// ErrOutputMutationEntryUnregistered refuses a mutation raised from a call
	// site the authority does not know. A new production mutation entry point
	// must declare itself in outputMutationEntryKinds; until it does, it cannot
	// name an output generation and is refused rather than admitted silently.
	ErrOutputMutationEntryUnregistered = errors.New("indexer: repository mutation entry point is not registered with the output-generation authority")
	// ErrOutputMutationTargetInvalid refuses a mutation that does not name
	// exactly one output generation and owner.
	ErrOutputMutationTargetInvalid = errors.New("indexer: repository mutation does not name exactly one output generation")
	// ErrOutputMutationReceiptSuperseded refuses work whose authority was taken
	// over by a newer mutation for the same owner. Gate 6, last clause.
	ErrOutputMutationReceiptSuperseded = errors.New("indexer: superseded work cannot fulfil a newer mutation receipt")
	// ErrOutputMutationReceiptSettled refuses a second settlement of a receipt
	// that already settled. It is a DIFFERENT fact from supersession: the first
	// settlement may well have fulfilled the generation, and reporting it as
	// "superseded" would be a false identity.
	ErrOutputMutationReceiptSettled = errors.New("indexer: mutation receipt has already settled")
	// ErrOutputMutationAuthorityClosed refuses admission after teardown.
	ErrOutputMutationAuthorityClosed = errors.New("indexer: output-generation authority is closed")
	// ErrOutputMutationLaneUnbound refuses a coalesced batch on a production
	// lane nobody bound to the authority. Such a lane would write generation
	// zero with no receipt and no named owner, so it fails CLOSED: the batch is
	// refused rather than degraded into an unfenced write.
	ErrOutputMutationLaneUnbound = errors.New("indexer: repository mutation lane is not bound to the output-generation authority")
)

// OutputMutationEntry is the stable name of one production mutation entry
// point. The constants below are the complete registry; see
// TestEveryProductionMutationEntryPointIsRegistered, which reads the package
// source and fails when a call site names anything else.
type OutputMutationEntry string

const (
	OutputEntryIndexCtx            OutputMutationEntry = "indexer.Indexer.IndexCtx"
	OutputEntryIndexFile           OutputMutationEntry = "indexer.Indexer.IndexFile"
	OutputEntryEvictFile           OutputMutationEntry = "indexer.Indexer.EvictFile"
	OutputEntryReresolveFileScoped OutputMutationEntry = "indexer.Indexer.ReresolveFileScoped"
	// IncrementalReindexPaths has no entry of its own on purpose: it COALESCES
	// through coordinateRepositoryReindex, so the execution that writes is the
	// lane worker, which names OutputEntryRepositoryReconcileLane below.
	OutputEntryDeferredPasses          OutputMutationEntry = "indexer.MultiIndexer.withDeferredRepositoryMutation"
	OutputEntryRepositoryTopology      OutputMutationEntry = "indexer.MultiIndexer.coordinateRepositoryTopologyMutation"
	OutputEntryIndexMultiRepo          OutputMutationEntry = "indexer.MultiIndexer.indexMultiRepo"
	OutputEntryIndexRepo               OutputMutationEntry = "indexer.MultiIndexer.IndexRepo"
	OutputEntryIncrementalDiscoverRepo OutputMutationEntry = "indexer.MultiIndexer.incrementalDiscoverRepo"
	OutputEntryGitWatcherFinalize      OutputMutationEntry = "indexer.GitWatcher.finalizeReconcile"
	OutputEntryPollerFinalizeGitHead   OutputMutationEntry = "indexer.Poller.finalizeGitHead"
	OutputEntryWatcherDirScan          OutputMutationEntry = "indexer.Watcher.runDirScan"
	OutputEntryWatcherPatchGraph       OutputMutationEntry = "indexer.Watcher.patchGraphWithReceiptState"
	OutputEntryWatcherEnqueueReresolve OutputMutationEntry = "indexer.Watcher.enqueueReresolve"
	OutputEntryCheckoutSourceMutation  OutputMutationEntry = "indexer.CheckoutLifecycle.BeginCheckoutMutation"
	// OutputEntryRepositoryReconcileLane is the coalescing lane worker itself.
	// Reconcile requests merge, so the execution — not the queuing caller — is
	// the entry point that names an output generation.
	OutputEntryRepositoryReconcileLane OutputMutationEntry = "indexer.repositoryMutationCoordinator.drain"
	// OutputEntryEnrichmentCorpus is the enrichment door: blame / churn /
	// coverage / releases / co-change / LSP semantic enrichment, and the
	// counter reconciliation the exact status path runs. They stamp derived
	// meta onto an already-built payload rather than extracting it, and they
	// write the CORPUS — generation zero — because every derived generation a
	// reader can reach is published, and publication seals the payload
	// (store_sqlite.ErrPayloadGenerationSealed). There is deliberately no
	// checkout-axis enrichment entry: an enrichment raised under a routed view
	// is refused at the request surface rather than being redirected here.
	//
	// One entry covers every producer because this registry keys the output
	// AXIS, not the payload. Which producer ran rides on the target's owner
	// key, which is producer-scoped so two runs of one enricher over one output
	// supersede each other while an enrichment never takes the authority away
	// from a live index mutation or checkout source edit.
	//
	// The call sites are internal/mcp (the tool surface) and cmd/gortex (the
	// control socket), so no mutation door in this package names it.
	OutputEntryEnrichmentCorpus OutputMutationEntry = "gortex.enrichment.corpus"
)

// outputMutationEntryKinds is the registry. An entry that is absent here cannot
// open a receipt, which is what makes "every production mutation entry passes
// through the authority" a checkable property rather than a convention.
var outputMutationEntryKinds = map[OutputMutationEntry]OutputGenerationKind{
	OutputEntryIndexCtx:                OutputGenerationLegacy,
	OutputEntryIndexFile:               OutputGenerationLegacy,
	OutputEntryEvictFile:               OutputGenerationLegacy,
	OutputEntryReresolveFileScoped:     OutputGenerationLegacy,
	OutputEntryDeferredPasses:          OutputGenerationLegacy,
	OutputEntryRepositoryTopology:      OutputGenerationLegacy,
	OutputEntryIndexMultiRepo:          OutputGenerationLegacy,
	OutputEntryIndexRepo:               OutputGenerationLegacy,
	OutputEntryIncrementalDiscoverRepo: OutputGenerationLegacy,
	OutputEntryGitWatcherFinalize:      OutputGenerationLegacy,
	OutputEntryPollerFinalizeGitHead:   OutputGenerationLegacy,
	OutputEntryWatcherDirScan:          OutputGenerationLegacy,
	OutputEntryWatcherPatchGraph:       OutputGenerationLegacy,
	OutputEntryWatcherEnqueueReresolve: OutputGenerationLegacy,
	OutputEntryCheckoutSourceMutation:  OutputGenerationCheckout,
	OutputEntryRepositoryReconcileLane: OutputGenerationLegacy,
	OutputEntryEnrichmentCorpus:        OutputGenerationLegacy,
}

// OutputMutationTarget is the one output generation and owner a mutation
// writes. OwnerKey is the serialization identity: two mutations that name the
// same OwnerKey are mutations of the same output, whichever lane raised them.
type OutputMutationTarget struct {
	Kind        OutputGenerationKind
	OwnerKey    string
	RepoPrefix  string
	RootPath    string
	GraphID     string
	CheckoutID  string
	Incarnation string
	// Generation is the output generation identifier. It MUST be zero for a
	// legacy target and positive for a checkout or dedicated one.
	Generation int64
}

func (t OutputMutationTarget) validate() error {
	if t.OwnerKey == "" {
		return fmt.Errorf("%w: no owner", ErrOutputMutationTargetInvalid)
	}
	switch t.Kind {
	case OutputGenerationLegacy:
		// Generation zero is the legacy corpus and is never relabelled as a
		// committed generation, so a legacy target may not carry one.
		if t.Generation != 0 {
			return fmt.Errorf("%w: legacy owner %q named generation %d, which is not generation zero",
				ErrOutputMutationTargetInvalid, t.OwnerKey, t.Generation)
		}
	case OutputGenerationCheckout:
		if t.Generation <= 0 || t.CheckoutID == "" || t.Incarnation == "" {
			return fmt.Errorf("%w: checkout owner %q named generation %d without a complete checkout identity",
				ErrOutputMutationTargetInvalid, t.OwnerKey, t.Generation)
		}
	case OutputGenerationDedicated:
		if t.Generation <= 0 || t.GraphID == "" {
			return fmt.Errorf("%w: dedicated owner %q named generation %d without a graph",
				ErrOutputMutationTargetInvalid, t.OwnerKey, t.Generation)
		}
	default:
		return fmt.Errorf("%w: owner %q named no output axis", ErrOutputMutationTargetInvalid, t.OwnerKey)
	}
	return nil
}

type outputGenerationOwnerState struct {
	latest uint64
	live   int
}

// OutputGenerationAuthority is the process-wide authority every repository
// mutation passes through. One per stack; NewSharedServer installs it on the
// standalone Indexer and on the MultiIndexer so the owned lanes and the
// orphan standalone lane cannot name the same output without noticing.
type OutputGenerationAuthority struct {
	// leases is the stack's view-lease manager, the same one the publisher
	// runtime and the request readers share. Nil is allowed and means this
	// authority takes no source witness.
	leases *graphview.LeaseManager

	mu         sync.Mutex
	closed     bool
	seq        uint64
	owners     map[string]*outputGenerationOwnerState
	issued     uint64
	settled    uint64
	superseded uint64
	witnessed  uint64
	// byEntry counts admissions per registered entry point. It is what makes
	// "this door opened a receipt" a checkable runtime fact rather than only a
	// static one: a door whose receipts are deleted stops appearing here.
	byEntry map[OutputMutationEntry]uint64
	// unchanged counts fulfilled mutations whose CONTENT fingerprint matched
	// the one they displaced, so the source witness was restored rather than
	// moved. A watcher tick over a repository nothing changed lands here.
	unchanged uint64
	// invalidated counts mutations admitted against an owner that could no
	// longer hand out a lease (closing admission, stopped manager) and whose
	// source observation was therefore moved without one, so the pins draining
	// behind the close report ErrBaseCorpusChanged rather than nil.
	invalidated uint64
}

// NewOutputGenerationAuthority builds the authority over one lease manager.
// A nil manager is legal: the authority still names output generations and
// still fences superseded receipts, it simply takes no source witness.
func NewOutputGenerationAuthority(leases *graphview.LeaseManager) *OutputGenerationAuthority {
	return &OutputGenerationAuthority{leases: leases, owners: make(map[string]*outputGenerationOwnerState)}
}

// ViewLeases reports the lease manager this authority witnesses source through.
func (a *OutputGenerationAuthority) ViewLeases() *graphview.LeaseManager {
	if a == nil {
		return nil
	}
	return a.leases
}

// OutputGenerationAuthorityStats is the authority's own counters. Receipts that
// were issued, receipts that settled, and the subset refused as superseded.
type OutputGenerationAuthorityStats struct {
	Issued     uint64
	Settled    uint64
	Superseded uint64
	Witnessed  uint64
	// Unchanged is the subset of witnessed fulfilments whose content
	// fingerprint matched the source they displaced, so no live BasePin was
	// told the corpus moved.
	Unchanged uint64
	// Invalidated counts admissions whose owner could no longer hand out a
	// source lease (closing admission, stopped manager) and whose observation
	// was moved without one. They are NOT counted as Witnessed: no lease was
	// held, so nothing will certify what the write leaves behind.
	Invalidated uint64
	LiveOwners  int
	// Entries is admissions per entry point, a copy the caller owns.
	Entries map[OutputMutationEntry]uint64
}

func (a *OutputGenerationAuthority) Stats() OutputGenerationAuthorityStats {
	if a == nil {
		return OutputGenerationAuthorityStats{}
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	entries := make(map[OutputMutationEntry]uint64, len(a.byEntry))
	for entry, count := range a.byEntry {
		entries[entry] = count
	}
	return OutputGenerationAuthorityStats{
		Issued: a.issued, Settled: a.settled, Superseded: a.superseded,
		Witnessed: a.witnessed, Unchanged: a.unchanged, Invalidated: a.invalidated,
		LiveOwners: len(a.owners), Entries: entries,
	}
}

// Close stops admission. Outstanding receipts may still settle: closing must
// refuse new work, not strand work already running on a lane.
func (a *OutputGenerationAuthority) Close() {
	if a == nil {
		return
	}
	a.mu.Lock()
	a.closed = true
	a.mu.Unlock()
}

// OutputMutationReceipt is one admitted mutation's authority over one output
// generation. It settles exactly once, through Complete or Abandon.
type OutputMutationReceipt struct {
	authority *OutputGenerationAuthority
	entry     OutputMutationEntry
	target    OutputMutationTarget
	seq       uint64

	mu      sync.Mutex
	settled bool
	source  *graphview.RawRepositoryMutationLease
	content *OutputSourceContent
}

// OutputSourceContent is what one executed mutation reports about the SOURCE
// content it moved under generation zero.
//
// It is the input to the generation-zero source fingerprint. Without it the
// fingerprint can only be derived from the revision the gate allocated, and a
// revision moves for every ADMITTED mutation — so a watcher tick over a file
// nothing changed would tell every live BasePin the corpus moved. With it the
// fingerprint is derived from the mutated path set and those paths' content
// identities, and a mutation that moved nothing restores the observation
// readers already hold instead of publishing a new one.
//
// A door that cannot report (it does not know its path set, or the payload
// failed) leaves this nil, and the fingerprint stays revision-derived — the
// conservative direction: it can only over-report movement.
type OutputSourceContent struct {
	// Root is the repository root Paths are relative to, or absolute under.
	Root string
	// Paths is the path set the mutation wrote. Nil means "a whole-repository
	// pass": the set is not enumerable, so Stale/Deleted/Full carry the fact.
	Paths []string
	// Moved reports whether the payload actually wrote anything. It is the
	// warm-restart predicate IndexResult already documents: stale files,
	// deleted files, or a full re-track.
	Moved bool
	// Stale, Deleted and Full are the counts behind Moved, folded into the
	// fingerprint so two passes over one path set with different outcomes are
	// different content.
	Stale   int
	Deleted int
	Full    bool
}

// outputSourceContentFor derives the source report of one executed mutation
// from its IndexResult. A failed or resultless pass reports nothing.
func outputSourceContentFor(root string, paths []string, result *IndexResult, err error) *OutputSourceContent {
	if err != nil || result == nil {
		return nil
	}
	return &OutputSourceContent{
		Root:    root,
		Paths:   paths,
		Moved:   result.StaleFileCount > 0 || result.DeletedFileCount > 0 || result.FullRetrack,
		Stale:   result.StaleFileCount,
		Deleted: result.DeletedFileCount,
		Full:    result.FullRetrack,
	}
}

// OutputMutationReceipts is a batch admitted together, for an entry point that
// mutates several repositories under one held set of lanes.
type OutputMutationReceipts []*OutputMutationReceipt

// Begin admits one mutation. It refuses an unregistered entry point, an entry
// whose declared axis does not match the target, a target that does not name
// exactly one output generation and owner, and admission after Close.
func (a *OutputGenerationAuthority) Begin(
	ctx context.Context, entry OutputMutationEntry, target OutputMutationTarget,
) (*OutputMutationReceipt, error) {
	if a == nil {
		return nil, ErrOutputMutationAuthorityClosed
	}
	kind, registered := outputMutationEntryKinds[entry]
	if !registered {
		return nil, fmt.Errorf("%w: %q", ErrOutputMutationEntryUnregistered, entry)
	}
	if target.Kind != kind {
		return nil, fmt.Errorf("%w: entry %q writes %s, target named %s",
			ErrOutputMutationTargetInvalid, entry, kind, target.Kind)
	}
	if err := target.validate(); err != nil {
		return nil, err
	}
	if ctx == nil {
		ctx = context.Background()
	}
	a.mu.Lock()
	if a.closed {
		a.mu.Unlock()
		return nil, ErrOutputMutationAuthorityClosed
	}
	a.seq++
	seq := a.seq
	state := a.owners[target.OwnerKey]
	if state == nil {
		state = &outputGenerationOwnerState{}
		a.owners[target.OwnerKey] = state
	}
	state.latest = seq
	state.live++
	a.issued++
	if a.byEntry == nil {
		a.byEntry = make(map[OutputMutationEntry]uint64)
	}
	a.byEntry[entry]++
	a.mu.Unlock()

	receipt := &OutputMutationReceipt{authority: a, entry: entry, target: target, seq: seq}
	// The source gate blocks, so it is taken outside the authority mutex and
	// only after the caller already owns its repository lane, which is the
	// order graphview's raw mutation API documents.
	if err := a.openSourceWitness(ctx, receipt); err != nil {
		receipt.Abandon()
		return nil, err
	}
	return receipt, nil
}

// BeginAll admits one mutation per target under a single entry point, in the
// caller's order. A failure part-way abandons everything it already admitted,
// so the batch is all-or-nothing at admission.
func (a *OutputGenerationAuthority) BeginAll(
	ctx context.Context, entry OutputMutationEntry, targets []OutputMutationTarget,
) (OutputMutationReceipts, error) {
	receipts := make(OutputMutationReceipts, 0, len(targets))
	for _, target := range targets {
		receipt, err := a.Begin(ctx, entry, target)
		if err != nil {
			receipts.Abandon()
			return nil, err
		}
		receipts = append(receipts, receipt)
	}
	return receipts, nil
}

// openSourceWitness takes the repository's exclusive raw-source data gate for
// the length of a legacy mutation, so a live BasePin sees the revision move.
//
// The gate is addressed by PREFIX and ROOT, through
// graphview.AcquireBaseCorpusMutation, not through a raw registration handle.
// That is what makes this half reachable in a real daemon: the lifecycle
// registers one DEDICATED owner per tracked repository prefix
// (bindDedicatedGraph -> RegisterRepositoryOwner, checkout_lifecycle.go:931/947,
// repository_admission.go:124), and nothing in the tree ever registers a RAW
// owner, so a registration-keyed lookup found nothing and every BasePin could
// only answer "unwitnessed". A raw registration, when one exists, is still
// matched on its canonical root exactly as before.
//
// A repository with NO registered owner at all is left unwitnessed on purpose:
// BasePin reports that as "unknown", and inventing a witness here would let a
// request claim an exactness nobody observed.
//
// Every OTHER refusal of the lease door is a reason this mutation may not write
// through the owner's gate, and none of them is a reason to leave that owner's
// readers holding a witness this write invalidates. The loudest case is a
// CLOSING admission (or a stopped manager): closing refuses new leases but
// leaves the pins already out valid and still answering requests, and
// finalization cannot run until they drain, so a write admitted at that moment
// really does change the corpus those requests are reading. Leaving it
// unwitnessed would make a pin taken BEFORE the close answer nil — which
// internal/mcp/view_request.go reads as "the answer is as exact as the route
// said it was" — about a corpus that has moved. So a refusal that still leaves
// a live owner behind moves the witness without a lease
// (graphview.InvalidateBaseCorpusSource), which is also a no-op for the prefixes
// nobody is registered for. A mutation is still never REFUSED because its
// corpus has no live reader authority to notify.
func (a *OutputGenerationAuthority) openSourceWitness(ctx context.Context, receipt *OutputMutationReceipt) error {
	if a.leases == nil || receipt.target.Kind != OutputGenerationLegacy {
		return nil
	}
	prefix, root := receipt.target.RepoPrefix, receipt.target.RootPath
	if prefix == "" || root == "" {
		return nil
	}
	lease, err := a.leases.AcquireBaseCorpusMutation(ctx, prefix, root)
	if err != nil {
		if unwitnessedRepository(err) {
			return a.invalidateSourceWitness(prefix)
		}
		return err
	}
	receipt.source = lease
	a.mu.Lock()
	a.witnessed++
	a.mu.Unlock()
	return nil
}

// invalidateSourceWitness moves the source observation of an owner that would
// not hand out a mutation lease, so the pins still reading behind it stop
// reporting an exactness this write just invalidated. It is a no-op for a
// prefix no owner is registered for.
//
// A refusal that means "there is nobody to notify" (the owner finished closing
// between the two calls, or was never there) does not fail the mutation: that
// is the honest unwitnessed case again. Any other refusal does — a witness that
// cannot be moved and cannot be reported unwitnessed is exactly the false-exact
// answer this door exists to prevent, so the write is refused rather than
// admitted behind a stale witness.
func (a *OutputGenerationAuthority) invalidateSourceWitness(prefix string) error {
	moved, err := a.leases.InvalidateBaseCorpusSource(prefix)
	if err != nil && !unwitnessedRepository(err) {
		return err
	}
	if !moved {
		return nil
	}
	a.mu.Lock()
	a.invalidated++
	a.mu.Unlock()
	return nil
}

// unwitnessedRepository reports the graphview refusals that mean "this
// repository has no source authority to move right now", as opposed to a real
// failure of this mutation (a cancelled context, a deadline).
func unwitnessedRepository(err error) bool {
	return errors.Is(err, graphview.ErrRepositoryOwnerUnknown) ||
		errors.Is(err, graphview.ErrRepositoryOwnerInvalid) ||
		errors.Is(err, graphview.ErrRawRepositoryNotReady) ||
		errors.Is(err, graphview.ErrRepositoryAdmissionClosed) ||
		errors.Is(err, graphview.ErrRepositoryAdmissionsStopped)
}

// Entry reports the registered entry point that opened this receipt.
func (r *OutputMutationReceipt) Entry() OutputMutationEntry {
	if r == nil {
		return ""
	}
	return r.entry
}

// Target reports the single output generation and owner this receipt names.
func (r *OutputMutationReceipt) Target() OutputMutationTarget {
	if r == nil {
		return OutputMutationTarget{}
	}
	return r.target
}

// Witnessed reports whether this mutation moved a real source witness.
func (r *OutputMutationReceipt) Witnessed() bool {
	if r == nil {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.source != nil
}

// Superseded reports whether a newer receipt for the same owner was admitted
// after this one. A superseded receipt can no longer fulfil its generation.
func (r *OutputMutationReceipt) Superseded() bool {
	if r == nil || r.authority == nil {
		return false
	}
	a := r.authority
	a.mu.Lock()
	defer a.mu.Unlock()
	state := a.owners[r.target.OwnerKey]
	return state == nil || state.latest != r.seq
}

// Complete fulfils the output generation this receipt named.
//
// It refuses when a newer mutation for the same owner was admitted while this
// one ran: that work is superseded and must not publish over the newer
// decision. The source witness is completed only on a fulfilled receipt; a
// refused or abandoned one deliberately leaves the source marked unavailable,
// because a mutation that lost its authority may have written part of it.
func (r *OutputMutationReceipt) Complete() error {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.settled {
		return fmt.Errorf("%w: receipt for owner %q", ErrOutputMutationReceiptSettled, r.target.OwnerKey)
	}
	r.settled = true
	superseded := r.authority.settle(r)
	source := r.source
	r.source = nil
	if superseded {
		if source != nil {
			source.Release()
		}
		return fmt.Errorf("%w: entry %q owner %q generation %d",
			ErrOutputMutationReceiptSuperseded, r.entry, r.target.OwnerKey, r.target.Generation)
	}
	if source != nil {
		unchanged, completeErr := source.CompleteUnchanged(gen0SourceFingerprint(source, r.content))
		source.Release()
		if completeErr != nil {
			return completeErr
		}
		if unchanged {
			r.authority.countUnchanged()
		}
	}
	return nil
}

func (a *OutputGenerationAuthority) countUnchanged() {
	if a == nil {
		return
	}
	a.mu.Lock()
	a.unchanged++
	a.mu.Unlock()
}

// ObserveSourceContent records what this mutation's payload moved, so the
// generation-zero source fingerprint it completes with is derived from CONTENT
// rather than from the revision the gate allocated. Calling it more than once
// keeps the last report; not calling it at all keeps the conservative
// revision-derived fingerprint.
func (r *OutputMutationReceipt) ObserveSourceContent(content *OutputSourceContent) {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.content = content
	r.mu.Unlock()
}

// gen0SourceFingerprint is the CONTENT identity of one generation-zero
// mutation, used as the source witness's fingerprint.
//
// Three cases, in order:
//
//   - the mutation reported that it moved NOTHING and the owner already had an
//     available source: the fingerprint is the one that is already there, which
//     is what makes CompleteUnchanged restore the prior observation instead of
//     telling every live BasePin the corpus moved;
//   - the mutation reported its path set: the digest folds the prior
//     fingerprint together with each path's on-disk content identity (size,
//     modification time, mode, existence) and the pass's own outcome counts, so
//     two passes that wrote different bytes are different content;
//   - the mutation reported nothing at all: the digest folds the revision, so
//     the witness moves exactly as it did before any door reported content.
func gen0SourceFingerprint(source *graphview.RawRepositoryMutationLease, content *OutputSourceContent) string {
	prior := source.PriorFingerprint()
	if content != nil && !content.Moved && prior != "" {
		return prior
	}
	h := sha256.New()
	writeFingerprintString(h, "gortex.gen0.source.v1")
	writeFingerprintString(h, prior)
	if content == nil {
		writeFingerprintString(h, "revision")
		writeFingerprintUint(h, source.Revision())
		return "gen0:" + hex.EncodeToString(h.Sum(nil))
	}
	writeFingerprintString(h, "content")
	writeFingerprintBool(h, content.Full)
	writeFingerprintInt(h, content.Stale)
	writeFingerprintInt(h, content.Deleted)
	paths := append([]string(nil), content.Paths...)
	sort.Strings(paths)
	writeFingerprintInt(h, len(paths))
	for _, path := range paths {
		writeFingerprintString(h, path)
		writeFingerprintString(h, pathContentIdentity(content.Root, path))
	}
	if len(paths) == 0 {
		// A whole-repository pass names no path set, so the outcome counts
		// above are the only content it can offer. Fold the revision in too:
		// two full passes with identical counts are then still distinct, which
		// keeps the conservative direction for the case that cannot enumerate.
		writeFingerprintUint(h, source.Revision())
	}
	return "gen0:" + hex.EncodeToString(h.Sum(nil))
}

// pathContentIdentity is one path's on-disk identity: the same size/mtime pair
// the incremental indexer itself treats as "this file changed", plus the mode
// and whether the path exists at all (a deletion is content too).
func pathContentIdentity(root, path string) string {
	if !filepath.IsAbs(path) && root != "" {
		path = filepath.Join(root, path)
	}
	info, err := os.Lstat(path)
	if err != nil {
		return "absent"
	}
	return fmt.Sprintf("%d/%d/%o", info.Size(), info.ModTime().UnixNano(), info.Mode().Perm())
}

// Abandon settles a receipt that did not fulfil its generation. Idempotent.
func (r *OutputMutationReceipt) Abandon() {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.settled {
		return
	}
	r.settled = true
	r.authority.settle(r)
	if r.source != nil {
		// No Complete: graphview leaves partial or failed source data
		// unavailable on purpose, which is exactly what an abandoned mutation
		// of generation zero is.
		r.source.Release()
		r.source = nil
	}
}

// Complete fulfils every receipt in the batch. Every receipt is settled even
// when an earlier one refuses, so no owner is left holding a live receipt.
func (rs OutputMutationReceipts) Complete() error {
	var firstErr error
	for _, receipt := range rs {
		if err := receipt.Complete(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// Current is the batch's preventive check: it refuses before the payload runs
// when any receipt in the batch already lost its authority. See
// runUnderOutputReceipt for why admission and payload start are not the same
// instant.
func (rs OutputMutationReceipts) Current() error {
	for _, receipt := range rs {
		if receipt.Superseded() {
			return fmt.Errorf("%w: entry %q owner %q was taken over before its payload started",
				ErrOutputMutationReceiptSuperseded, receipt.Entry(), receipt.Target().OwnerKey)
		}
	}
	return nil
}

// Abandon settles every receipt in the batch without fulfilling it.
func (rs OutputMutationReceipts) Abandon() {
	for _, receipt := range rs {
		receipt.Abandon()
	}
}

func (a *OutputGenerationAuthority) settle(r *OutputMutationReceipt) bool {
	if a == nil {
		return false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	state := a.owners[r.target.OwnerKey]
	superseded := state == nil || state.latest != r.seq
	if state != nil {
		state.live--
		if state.live <= 0 {
			delete(a.owners, r.target.OwnerKey)
		}
	}
	a.settled++
	if superseded {
		a.superseded++
	}
	return superseded
}

// defaultOutputGenerationAuthority is the fallback for an Indexer or lifecycle
// nothing installed one on — hand-built fixtures and the embedded paths. It
// keeps "every mutation names one output generation" true everywhere;
// NewSharedServer installs a real, lease-backed authority for production.
var (
	defaultOutputAuthorityOnce sync.Once
	defaultOutputAuthority     *OutputGenerationAuthority
)

func defaultOutputGenerationAuthority() *OutputGenerationAuthority {
	defaultOutputAuthorityOnce.Do(func() {
		defaultOutputAuthority = NewOutputGenerationAuthority(nil)
	})
	return defaultOutputAuthority
}

// DefaultOutputGenerationAuthority is the process fallback above, exported for
// the mutation doors that live OUTSIDE this package — the MCP enrichment
// surface and the control socket's `gortex enrich`.
//
// It exists so those doors cannot construct a SECOND fallback of their own. Two
// authorities are two serialization universes: owner keys in one cannot
// supersede or order owner keys in the other, so an enrichment admitted through
// a package-local fallback would silently stop colliding with the indexer lanes
// that name the same corpus. There is one fallback per process, and this is it.
func DefaultOutputGenerationAuthority() *OutputGenerationAuthority {
	return defaultOutputGenerationAuthority()
}

// SetOutputGenerationAuthority installs the process authority on this Indexer.
func (idx *Indexer) SetOutputGenerationAuthority(a *OutputGenerationAuthority) {
	if idx == nil {
		return
	}
	idx.outputAuthority.Store(a)
}

// SetOutputGenerationAuthority installs the process authority on this
// MultiIndexer, so every per-repository Indexer it owns resolves to it.
func (mi *MultiIndexer) SetOutputGenerationAuthority(a *OutputGenerationAuthority) {
	if mi == nil {
		return
	}
	mi.outputAuthority.Store(a)
}

func (mi *MultiIndexer) outputGenerationAuthority() *OutputGenerationAuthority {
	if mi != nil {
		if a := mi.outputAuthority.Load(); a != nil {
			return a
		}
	}
	return defaultOutputGenerationAuthority()
}

// outputGenerationAuthority resolves this Indexer's authority: its own if one
// was installed, otherwise its owning MultiIndexer's, otherwise the default.
// The owned lane and the orphan standalone lane therefore resolve to the SAME
// authority in a stack that installed one.
func (idx *Indexer) outputGenerationAuthority() *OutputGenerationAuthority {
	if idx == nil {
		return defaultOutputGenerationAuthority()
	}
	if a := idx.outputAuthority.Load(); a != nil {
		return a
	}
	idx.repositoryMutationMu.Lock()
	owner := idx.repositoryMutationOwner
	idx.repositoryMutationMu.Unlock()
	if owner != nil {
		if a := owner.outputAuthority.Load(); a != nil {
			return a
		}
	}
	return defaultOutputGenerationAuthority()
}

// ResolvedOutputGenerationAuthority reports the authority this Indexer admits
// mutations through, after the owner and default fallbacks. It is the read a
// wiring test uses to prove the orphan lane and the owned lanes resolve to the
// one value the stack installed.
func (idx *Indexer) ResolvedOutputGenerationAuthority() *OutputGenerationAuthority {
	return idx.outputGenerationAuthority()
}

// ResolvedOutputGenerationAuthority reports the authority every per-repository
// lane this MultiIndexer owns admits through.
func (mi *MultiIndexer) ResolvedOutputGenerationAuthority() *OutputGenerationAuthority {
	return mi.outputGenerationAuthority()
}

// legacyOutputTarget names this Indexer's one generation-zero output.
//
// The owner key is the repository ROOT when one is known, not the prefix: the
// standalone Indexer a stack hands to the MCP server carries no prefix, and
// keying by root is what makes its orphan lane collide with the owned lane for
// the same repository instead of quietly naming a second owner for one corpus.
func (idx *Indexer) legacyOutputTarget() OutputMutationTarget {
	idx.repositoryMutationMu.Lock()
	root, prefix := idx.rootPath, idx.repoPrefix
	idx.repositoryMutationMu.Unlock()
	return legacyOutputTargetFor(outputStoreIdentity(idx.graph), prefix, root, fmt.Sprintf("indexer:%p", idx))
}

// outputStoreIdentity discriminates the OUTPUT a mutation writes into.
//
// A repository identity is NOT an output identity. A sparse generation build
// constructs a private Indexer over its own store handle with the live
// repository's prefix (SparseGenerationBuilder.runPass) and indexes the tree
// into it; that payload is a different output from the live corpus, and two
// such builds for one prefix are different outputs again. Keying the owner on
// the repository alone collapses them into one owner, and then two perfectly
// legitimate concurrent builds supersede each other.
//
// The store handle is that identity. Two Indexers writing the same handle for
// the same repository — the standalone orphan-lane Indexer a stack hands the
// MCP server and the MultiIndexer-owned per-repository lane — still collide on
// one owner, which is the case the fence exists for.
//
// The id is MONOTONICALLY ISSUED, not the handle's address. An address is
// unique only among live objects: a closed store whose memory is reused by a
// new store of the same type would inherit its identity, and two genuinely
// independent outputs would then supersede each other. Ids are issued once per
// store and never reused, so that cannot happen.
func outputStoreIdentity(store graph.Store) string {
	if store == nil {
		return "store:none"
	}
	return fmt.Sprintf("store:%T/%d", store, outputStoreIDs.idFor(store))
}

// outputStoreIDRegistry issues the monotonic store ids above.
//
// It is keyed by the handle's ADDRESS and deliberately holds no reference to
// the store itself: a daemon builds a private generation handle per sparse
// build, and a registry that retained them would keep every closed store (and
// its connection pool) alive for the life of the process.
//
// Retiring an entry is what makes an id safe to key on. A finalizer on the
// handle does it, and the runtime frees an object only AFTER its finalizer has
// run, so an address can never be handed to a new store while the previous
// store's entry is still in the map. A handle that already carries a finalizer
// (SetFinalizer panics on a second one) keeps its entry pinned instead; that is
// the conservative direction and is no weaker than keying on the address alone.
type outputStoreIDRegistry struct {
	mu   sync.Mutex
	next uint64
	ids  map[uintptr]uint64
}

var outputStoreIDs = &outputStoreIDRegistry{ids: make(map[uintptr]uint64)}

func (r *outputStoreIDRegistry) idFor(store graph.Store) uint64 {
	value := reflect.ValueOf(store)
	if value.Kind() != reflect.Pointer || value.IsNil() {
		// A non-pointer store has no stable address to key on. Merging every
		// such handle of one type into one output is conservative: it can only
		// make two mutations collide on one owner, never let two mutations of
		// one output miss each other.
		return 0
	}
	address := value.Pointer()
	r.mu.Lock()
	id, known := r.ids[address]
	if !known {
		r.next++
		id = r.next
		r.ids[address] = id
	}
	r.mu.Unlock()
	if !known {
		r.retireWith(store, address, id)
	}
	return id
}

func (r *outputStoreIDRegistry) retireWith(store graph.Store, address uintptr, id uint64) {
	defer func() {
		// SetFinalizer panics when the handle already carries a finalizer, or
		// when it is not the start of an allocation. Neither is a reason to
		// refuse the mutation: the entry simply stays.
		_ = recover()
	}()
	runtime.SetFinalizer(store, func(any) { r.retire(address, id) })
}

func (r *outputStoreIDRegistry) retire(address uintptr, id uint64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	// Only the entry this id was issued for is retired, so a registry rebuilt
	// for a later store at the same address survives a late finalizer.
	if r.ids[address] == id {
		delete(r.ids, address)
	}
}

func legacyOutputTargetFor(output, prefix, root, fallback string) OutputMutationTarget {
	target := OutputMutationTarget{Kind: OutputGenerationLegacy, RepoPrefix: prefix}
	if root != "" {
		if abs, err := filepath.Abs(root); err == nil {
			root = abs
		}
		target.RootPath = filepath.Clean(root)
	}
	switch {
	case target.RootPath != "":
		target.OwnerKey = "root:" + target.RootPath
	case prefix != "":
		target.OwnerKey = "prefix:" + prefix
	default:
		target.OwnerKey = fallback
	}
	if output != "" {
		target.OwnerKey = output + "|" + target.OwnerKey
	}
	return target
}

// withOutputGeneration runs one mutation body under one receipt. Callers hold
// their repository lane already, which is the order the source gate requires.
func (idx *Indexer) withOutputGeneration(ctx context.Context, entry OutputMutationEntry, fn func() error) error {
	if fn == nil {
		return nil
	}
	return idx.withOutputGenerationSource(ctx, entry,
		func(func(*OutputSourceContent)) error { return fn() })
}

// withOutputGenerationSource is withOutputGeneration for a door that can report
// the SOURCE content its payload moved. The body is handed an observer; what it
// reports becomes the receipt's content-derived gen0 source fingerprint, and a
// body that never calls the observer keeps the conservative revision-derived
// one.
func (idx *Indexer) withOutputGenerationSource(
	ctx context.Context,
	entry OutputMutationEntry,
	fn func(observe func(*OutputSourceContent)) error,
) error {
	if fn == nil {
		return nil
	}
	target := idx.legacyOutputTarget()
	receipt, err := idx.outputGenerationAuthority().Begin(ctx, entry, target)
	if err != nil {
		return err
	}
	return runUnderOutputReceipt(receipt, func() error {
		return fn(func(content *OutputSourceContent) {
			if content != nil && content.Root == "" {
				content.Root = target.RootPath
			}
			receipt.ObserveSourceContent(content)
		})
	})
}

// runUnderOutputReceipt runs one mutation body under one already-admitted
// receipt, and is the PREVENTIVE half of the fence.
//
// Admission is not instantaneous: Begin takes the repository's raw-source data
// gate, which blocks, so a newer mutation for the same owner can be admitted
// between "this receipt was issued" and "this receipt's payload starts". A
// receipt that already lost its authority therefore never gets to run its body
// — the refusal is raised before any byte moves, not reported after the fact.
// Supersession that happens *while* the body runs can only be caught at
// Complete; that residual case is the reporting half, and its caller gets the
// refusal so the work is retried rather than reported as fulfilled.
func runUnderOutputReceipt(receipt *OutputMutationReceipt, fn func() error) error {
	if fn == nil {
		receipt.Abandon()
		return nil
	}
	if receipt.Superseded() {
		receipt.Abandon()
		return fmt.Errorf("%w: entry %q owner %q generation %d was taken over before its payload started",
			ErrOutputMutationReceiptSuperseded, receipt.Entry(), receipt.Target().OwnerKey, receipt.Target().Generation)
	}
	if err := fn(); err != nil {
		receipt.Abandon()
		return err
	}
	return receipt.Complete()
}
