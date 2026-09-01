package indexer

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"sync"

	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

const repositoryMutationPathCap = 2048

var errRepositoryMutationCoordinatorClosed = errors.New("repository mutation coordinator is closed")

type repositoryMutationExecutor func(paths []string) (*IndexResult, error)

type repositoryMutationGuard func() (skip bool, err error)

type immutableRepositoryMutationKey struct{}

func authorizeImmutableRepositoryMutation(ctx context.Context) context.Context {
	return context.WithValue(ctx, immutableRepositoryMutationKey{}, true)
}

func immutableRepositoryMutationAuthorized(ctx context.Context) bool {
	authorized, _ := ctx.Value(immutableRepositoryMutationKey{}).(bool)
	return authorized
}

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

	lane chan struct{}
	work sync.WaitGroup

	// batchMutationGate is shared by every stable repository lane owned by a
	// MultiIndexer. Admission takes the read side before a lane: a queued batch
	// writer can then never own the gate while a mutation owns a lane and waits
	// for that same gate. The read side stays held through the complete pipeline.
	batchMutationGate *sync.RWMutex
	// batchAdmissionHook is a deterministic test observation point. Production
	// coordinators leave it nil; tests set it before admitting concurrent work.
	batchAdmissionHook func()
	executor           repositoryMutationExecutor
	guard              repositoryMutationGuard
	closed             bool
	running            bool

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
		guard := c.guard
		c.mu.Unlock()

		var outcome repositoryMutationOutcome
		if guard != nil {
			skip, err := guard()
			if err != nil {
				outcome.err = err
			} else if !skip {
				outcome = executeRepositoryMutation(executor, paths)
			}
		} else {
			outcome = executeRepositoryMutation(executor, paths)
		}
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
	// Topology operations deliberately use runExclusiveLaneOnly: promotion
	// must be able to install an immutable Git-tree corpus while its intent
	// guard is armed. Ordinary watcher, janitor, point, and explicit reindex
	// work takes the batch gate and is suppressed while that corpus is owned.
	if acquireBatchGate && !immutableRepositoryMutationAuthorized(ctx) {
		c.mu.Lock()
		guard := c.guard
		c.mu.Unlock()
		if guard != nil {
			skip, err := guard()
			if err != nil {
				return err
			}
			if skip {
				return nil
			}
		}
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
	c.closeAdmission()
	return c.wait(ctx)
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
		coordinator.guard = func() (bool, error) {
			return mi.routeOwnsDedicatedCorpus(context.Background(), repoPrefix)
		}
		mi.repositoryMutations[repoPrefix] = coordinator
	}
	return coordinator
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
	}
	return idx.repositoryMutation
}

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
func (idx *Indexer) coordinateRepositoryMutation(ctx context.Context, fn func() error) error {
	return idx.repositoryMutations().runExclusive(ctx, fn)
}
