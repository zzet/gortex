package indexer

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/zzet/gortex/internal/gitcmd"
	"github.com/zzet/gortex/internal/gitstate"
	"go.uber.org/zap"
)

// GitWatcher observes `.git/HEAD` (and the target ref) for a single
// tracked repository. When the resolved commit moves, it computes the
// diff between the old and new commits and dispatches EvictFile /
// IndexFileNoResolve per changed path, running the resolver once at
// the end. This is the branch-switch / rebase / reset path: the
// regular fsnotify per-file watcher sees 500 Remove+Create events for
// a checkout and walks each through per-file resolve + search, which
// is measurably slow and incorrect for renames (git sees them as
// atomic; fsnotify sees unrelated remove+create pairs).
//
// Gitwatcher complements the file watcher — it does not replace it.
// File edits outside git operations (save buffer, `git stash`,
// `git reset --mixed` leaving the working tree dirty) still fire
// normal fsnotify events and flow through the per-file path.
type GitWatcher struct {
	repoPath string
	// indexer is the construction-time stable repository-lane carrier.
	// MultiWatcher installs currentIndexer so the freshness tail resolves the
	// currently registered Indexer only after that lane admits it.
	indexer        *Indexer
	currentIndexer func() *Indexer
	logger         *zap.Logger
	fsw            *fsnotify.Watcher
	debounce       time.Duration
	done           chan struct{}
	stopped        chan struct{}
	mu             sync.Mutex
	lastSHA        string
	fireTimer      *time.Timer
	loopStarted    bool
	stopCalled     bool

	// Worktree topology belongs to the shared git common directory, not to
	// this checkout's private gitdir. The watcher observes the common
	// worktrees directory, each admin child, and the checkout roots themselves
	// so additions, removals, pruning and inaccessible roots all trigger a
	// family reconciliation promptly.
	gitDir            string
	commonDir         string
	worktreesDir      string
	topologyTimer     *time.Timer
	topologyChange    func(string)
	topologyRefreshMu sync.Mutex
	topologyOwned     bool
	topologyPaths     map[string]struct{}
	worktreeRoots     map[string]struct{}
	worktreeAdminDirs map[string]struct{}
	inventory         func(context.Context, string) (*gitstate.FamilyInventory, error)
	topologyAdd       func(string) error
	topologyRemove    func(string) error
	topologyDegraded  string
	refPaths          map[string]struct{}
	refAdd            func(string) error

	// topologyRetryMu serializes retry admission with Stop. A callback adds to
	// topologyRetryWG while holding this mutex, so Stop cannot race Wait with a
	// later Add and no fsnotify registration can outlive physical teardown.
	topologyRetryMu       sync.Mutex
	topologyRetryTimer    *time.Timer
	topologyRetryEpoch    uint64
	topologyRetryAttempts uint32
	topologyRetryBase     time.Duration
	topologyRetryMax      time.Duration
	topologyRetryClosing  bool
	topologyRetryWG       sync.WaitGroup

	// reconciling single-flights reconcile: a ref event landing while
	// one is in flight sets rerun instead of spawning a second
	// concurrent reconcile from the same stale base (each AfterFunc
	// fires on its own goroutine, and lastSHA only advances at the
	// END of a reconcile — overlapping runs would diff and apply the
	// same range twice and race applyChanges/ResolveAll).
	reconciling bool
	rerun       bool
	// drained is a test hook that fires after a reconcile completes
	// with the number of files patched. nil in production.
	drained      func(int)
	batchReindex watcherBatchReindex // MultiWatcher installs shared resolver + derived catch-up
}

// NewGitWatcher creates a watcher for repoPath/.git/HEAD. repoPath is
// the absolute path to the worktree root; the .git dir is discovered
// by looking at the HEAD file (handles worktrees, submodules via the
// `gitdir:` indirection).
func NewGitWatcher(repoPath string, idx *Indexer, logger *zap.Logger) (*GitWatcher, error) {
	absRepo, err := filepath.Abs(repoPath)
	if err != nil {
		return nil, err
	}
	fsw, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}
	return &GitWatcher{
		repoPath:          absRepo,
		indexer:           idx,
		logger:            logger,
		fsw:               fsw,
		debounce:          300 * time.Millisecond,
		done:              make(chan struct{}),
		stopped:           make(chan struct{}),
		topologyOwned:     true,
		topologyPaths:     make(map[string]struct{}),
		worktreeRoots:     make(map[string]struct{}),
		worktreeAdminDirs: make(map[string]struct{}),
		refPaths:          make(map[string]struct{}),
		inventory:         gitstate.Inventory,
		topologyRetryBase: time.Second,
		topologyRetryMax:  30 * time.Second,
	}, nil
}

// OnWorktreeChange installs the callback used to reconcile the checkout
// family after a topology change. The callback receives the watcher checkout
// root, which is a stable selector for resolving the family.
func (gw *GitWatcher) OnWorktreeChange(callback func(string)) {
	gw.mu.Lock()
	gw.topologyChange = callback
	gw.mu.Unlock()
}

// setTopologyOwner enables or disables the family-wide worktree topology
// watch on this checkout. MultiWatcher elects exactly one owner per Git common
// directory; direct GitWatcher users retain the historical owner-by-default
// behavior. The refresh mutex closes the disable-vs-refresh race so a demoted
// watcher cannot re-register paths after ownership moves.
func (gw *GitWatcher) setTopologyOwner(owner bool) {
	if err := gw.setTopologyOwnerChecked(owner); err != nil {
		gw.recordTopologyRefresh(err)
	}
}

// setTopologyOwnerChecked changes ownership and reports whether the required
// common-directory topology watches are actually live. Callers admitting a
// new Git watcher use the error to roll back publication; established owners
// use the best-effort wrapper above, which retains file watching and retries.
func (gw *GitWatcher) setTopologyOwnerChecked(owner bool) error {
	gw.topologyRefreshMu.Lock()
	err := gw.setTopologyOwnerCheckedLocked(owner)
	gw.topologyRefreshMu.Unlock()
	if err == nil {
		gw.recordTopologyRefresh(nil)
	}
	return err
}

func (gw *GitWatcher) setTopologyOwnerCheckedLocked(owner bool) error {
	gw.mu.Lock()
	if gw.stopCalled {
		gw.mu.Unlock()
		if owner {
			return fmt.Errorf("git watcher is stopped")
		}
		return nil
	}
	if gw.topologyOwned == owner {
		refsReady := gw.gitDir != "" && gw.commonDir != ""
		gw.mu.Unlock()
		if !owner && !refsReady {
			return nil
		}
		return gw.refreshRequiredWatchesLocked()
	}
	gw.topologyOwned = owner
	if !owner {
		if gw.topologyTimer != nil {
			gw.topologyTimer.Stop()
			gw.topologyTimer = nil
		}
		paths := clonePathSet(gw.topologyPaths)
		gw.topologyPaths = make(map[string]struct{})
		gw.worktreeRoots = make(map[string]struct{})
		gw.worktreeAdminDirs = make(map[string]struct{})
		gw.topologyDegraded = ""
		refsReady := gw.gitDir != "" && gw.commonDir != ""
		gw.mu.Unlock()
		gw.cancelTopologyRetry()
		for path := range paths {
			if gw.topologyRemove != nil {
				_ = gw.topologyRemove(path)
			} else {
				_ = gw.fsw.Remove(path)
			}
		}
		if !refsReady {
			return nil
		}
		return gw.refreshRefWatchesLocked()
	}
	gw.mu.Unlock()
	return gw.refreshRequiredWatchesLocked()
}

func (gw *GitWatcher) commonDirectory() string {
	gw.mu.Lock()
	defer gw.mu.Unlock()
	return gw.commonDir
}

func (gw *GitWatcher) registeredIndexer() *Indexer {
	if gw.currentIndexer != nil {
		return gw.currentIndexer()
	}
	return gw.indexer
}

// finalizeReconcile publishes commit freshness only while holding the stable
// repository lane. IndexRepo replacement uses the same lane, so the registry
// lookup and state restamp cannot land on a retired Indexer. lastSHA advances
// after the restamp; failed lane admission therefore leaves the prior SHA for
// the next ref notification to retry.
func (gw *GitWatcher) finalizeReconcile(ctx context.Context, newSHA string) error {
	return gw.indexer.coordinateRepositoryMutation(ctx, func() error {
		idx := gw.registeredIndexer()
		if idx == nil {
			return fmt.Errorf("git-watcher: repository indexer is no longer registered")
		}
		idx.reconcileRepoIndexState(gw.repoPath)
		gw.mu.Lock()
		gw.lastSHA = newSHA
		gw.mu.Unlock()
		return nil
	})
}

// Start sets up fsnotify watches on the repo's git control files and
// launches the event-processing goroutine. Safe to call once per
// GitWatcher instance. Returns an error (and does not launch the loop)
// when the repo has no .git directory — Stop remains safe to call.
func (gw *GitWatcher) Start() error {
	gitDir, err := resolveGitDir(gw.repoPath)
	if err != nil {
		return fmt.Errorf("resolve .git dir for %s: %w", gw.repoPath, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	commonDir, err := resolveGitCommonDir(ctx, gw.repoPath)
	cancel()
	if err != nil {
		return fmt.Errorf("resolve git common dir for %s: %w", gw.repoPath, err)
	}
	gw.mu.Lock()
	gw.gitDir = filepath.Clean(gitDir)
	gw.commonDir = filepath.Clean(commonDir)
	gw.worktreesDir = filepath.Join(gw.commonDir, "worktrees")
	gw.mu.Unlock()

	// Ref state is required for branch-switch correctness. Worktree topology is
	// additionally required for the elected family owner. Admission is checked:
	// callers must not publish a Git watcher that silently missed either class.
	if err := gw.refreshRequiredWatchesChecked(); err != nil {
		return fmt.Errorf("start git state watcher: %w", err)
	}

	gw.lastSHA, _ = gw.currentSHA(context.Background())
	gw.mu.Lock()
	gw.loopStarted = true
	gw.mu.Unlock()
	go gw.loop()
	return nil
}

// Stop halts the watcher. Idempotent — safe whether Start succeeded,
// failed, or was never called. We only block on `stopped` when the
// loop goroutine is actually running; otherwise Stop would deadlock
// on a channel nobody's going to close.
func (gw *GitWatcher) Stop() error {
	gw.mu.Lock()
	started := gw.loopStarted
	already := gw.stopCalled
	gw.stopCalled = true
	if gw.fireTimer != nil {
		gw.fireTimer.Stop()
	}
	if gw.topologyTimer != nil {
		gw.topologyTimer.Stop()
	}
	gw.mu.Unlock()
	if already {
		return nil
	}

	gw.topologyRetryMu.Lock()
	gw.topologyRetryClosing = true
	gw.topologyRetryEpoch++
	if gw.topologyRetryTimer != nil {
		gw.topologyRetryTimer.Stop()
		gw.topologyRetryTimer = nil
	}
	gw.topologyRetryMu.Unlock()
	gw.topologyRetryWG.Wait()

	close(gw.done)
	_ = gw.fsw.Close()
	if started {
		<-gw.stopped
	}
	return nil
}

func (gw *GitWatcher) loop() {
	defer close(gw.stopped)
	for {
		select {
		case <-gw.done:
			return
		case event, ok := <-gw.fsw.Events:
			if !ok {
				return
			}
			if gw.isTopologyEvent(event) {
				// Debounce the refresh itself, not only the callback. A single
				// worktree mutation fans out across the common dir, admin dir,
				// and root watches; inventorying on every raw fsnotify record
				// recreates the family N+1 storm inside the elected owner.
				gw.scheduleTopologyChange(event.Name)
			}
			if gw.isRefEvent(event.Name) {
				if event.Op&(fsnotify.Remove|fsnotify.Rename) != 0 && gw.invalidateRefWatch(event.Name) {
					gw.recordTopologyRefresh(fmt.Errorf("git ref watch invalidated: %s", filepath.Clean(event.Name)))
				}
				gw.scheduleReconcile(event.Name)
			}
		case err, ok := <-gw.fsw.Errors:
			if !ok {
				return
			}
			gw.logger.Warn("git-watcher: fsnotify error", zap.Error(err))
		}
	}
}

// scheduleReconcile coalesces bursts of ref-file events (a branch
// switch touches HEAD, refs/heads/<new>, and often packed-refs in
// rapid succession) into a single reconcile. Resets the debounce
// timer on every event and lets the last one win.
func (gw *GitWatcher) scheduleReconcile(trigger string) {
	gw.mu.Lock()
	defer gw.mu.Unlock()
	if gw.stopCalled {
		return
	}
	if gw.fireTimer != nil {
		gw.fireTimer.Stop()
	}
	gw.fireTimer = time.AfterFunc(gw.debounce, func() {
		gw.reconcile(trigger)
	})
}

func (gw *GitWatcher) scheduleTopologyChange(trigger string) {
	gw.mu.Lock()
	defer gw.mu.Unlock()
	if gw.stopCalled || !gw.topologyOwned {
		return
	}
	if gw.topologyTimer != nil {
		gw.topologyTimer.Stop()
	}
	gw.topologyTimer = time.AfterFunc(gw.debounce, func() {
		// Re-sample after the burst settles. A common-dir create event can
		// arrive before git has finished writing the new admin record.
		gw.refreshTopologyWatches()
		gw.mu.Lock()
		callback := gw.topologyChange
		stopped := gw.stopCalled
		owner := gw.topologyOwned
		gw.mu.Unlock()
		if stopped || !owner || callback == nil {
			return
		}
		gw.logger.Debug("git-watcher: worktree topology changed", zap.String("trigger", trigger))
		callback(gw.repoPath)
	})
}

func (gw *GitWatcher) isRefEvent(name string) bool {
	gw.mu.Lock()
	gitDir, commonDir := gw.gitDir, gw.commonDir
	gw.mu.Unlock()
	name = filepath.Clean(name)
	if name == filepath.Join(gitDir, "HEAD") || name == filepath.Join(commonDir, "packed-refs") {
		return true
	}
	return pathWithin(filepath.Join(commonDir, "refs", "heads"), name)
}

func (gw *GitWatcher) isTopologyEvent(event fsnotify.Event) bool {
	name := filepath.Clean(event.Name)
	gw.mu.Lock()
	owned := gw.topologyOwned
	worktreesDir := gw.worktreesDir
	_, root := gw.worktreeRoots[name]
	gw.mu.Unlock()
	if !owned {
		return false
	}
	if root && event.Op&(fsnotify.Remove|fsnotify.Rename) != 0 {
		return true
	}
	if worktreesDir == "" {
		return false
	}
	if name == filepath.Clean(worktreesDir) {
		return event.Op&(fsnotify.Create|fsnotify.Remove|fsnotify.Rename) != 0
	}
	rel, err := filepath.Rel(worktreesDir, name)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return false
	}
	parts := strings.Split(rel, string(filepath.Separator))
	if len(parts) == 1 {
		// The administration directory for one linked worktree appeared or
		// disappeared. Ordinary writes to its children are classified below.
		return event.Op&(fsnotify.Create|fsnotify.Remove|fsnotify.Rename) != 0
	}
	if len(parts) != 2 {
		return false
	}
	switch parts[1] {
	case "HEAD", "gitdir", "commondir", "locked":
		return true
	default:
		// In particular, a checkout coordinator refreshes the per-worktree
		// index roughly every poll interval. Treating that payload write as a
		// topology event created a permanent event storm and could consume the
		// old controller debounce immediately before the one removal event.
		return false
	}
}

func pathWithin(root, candidate string) bool {
	if root == "" || candidate == "" {
		return false
	}
	rel, err := filepath.Rel(filepath.Clean(root), filepath.Clean(candidate))
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

func (gw *GitWatcher) refreshRequiredWatchesChecked() error {
	gw.topologyRefreshMu.Lock()
	defer gw.topologyRefreshMu.Unlock()
	return gw.refreshRequiredWatchesLocked()
}

func (gw *GitWatcher) refreshRequiredWatchesLocked() error {
	refErr := gw.refreshRefWatchesLocked()
	gw.mu.Lock()
	owner := gw.topologyOwned
	commonDir := gw.commonDir
	gw.mu.Unlock()
	if !owner {
		return refErr
	}
	return errors.Join(
		refErr,
		gw.addTopologyPathChecked(commonDir),
		gw.refreshTopologyWatchesLocked(),
	)
}

func (gw *GitWatcher) refreshRefWatchesLocked() error {
	gw.mu.Lock()
	gitDir := gw.gitDir
	commonDir := gw.commonDir
	stopped := gw.stopCalled
	gw.mu.Unlock()
	if stopped {
		return fmt.Errorf("git watcher is stopped")
	}
	if gitDir == "" || commonDir == "" {
		return fmt.Errorf("git ref directories are unavailable")
	}

	type refWatch struct {
		path     string
		required bool
	}
	watches := []refWatch{
		{path: filepath.Join(gitDir, "HEAD"), required: true},
		{path: filepath.Join(commonDir, "packed-refs")},
		{path: filepath.Join(commonDir, "refs", "heads")},
	}
	var refreshErr error
	for _, watch := range watches {
		path := filepath.Clean(watch.path)
		if _, err := os.Stat(path); err != nil {
			if errors.Is(err, os.ErrNotExist) && !watch.required {
				continue
			}
			refreshErr = errors.Join(refreshErr, fmt.Errorf("stat git ref path %s: %w", path, err))
			continue
		}

		gw.mu.Lock()
		if gw.refPaths == nil {
			gw.refPaths = make(map[string]struct{})
		}
		_, exists := gw.refPaths[path]
		if exists {
			gw.mu.Unlock()
			continue
		}
		var err error
		if gw.refAdd != nil {
			err = gw.refAdd(path)
		} else {
			err = gw.fsw.Add(path)
		}
		if err == nil {
			gw.refPaths[path] = struct{}{}
		}
		gw.mu.Unlock()
		if err != nil {
			refreshErr = errors.Join(refreshErr, fmt.Errorf("watch git ref path %s: %w", path, err))
		}
	}
	return refreshErr
}

func (gw *GitWatcher) invalidateRefWatch(path string) bool {
	path = filepath.Clean(path)
	gw.mu.Lock()
	defer gw.mu.Unlock()
	if _, exists := gw.refPaths[path]; !exists {
		return false
	}
	delete(gw.refPaths, path)
	return true
}

func (gw *GitWatcher) addTopologyPath(path string) {
	if err := gw.addTopologyPathChecked(path); err != nil {
		gw.logger.Warn("git-watcher: failed to watch worktree topology",
			zap.String("path", filepath.Clean(path)), zap.Error(err))
	}
}

func (gw *GitWatcher) addTopologyPathChecked(path string) error {
	path = filepath.Clean(path)
	if _, err := os.Stat(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("stat topology path %s: %w", path, err)
	}
	gw.mu.Lock()
	defer gw.mu.Unlock()
	if gw.stopCalled {
		return fmt.Errorf("git watcher is stopped")
	}
	if _, exists := gw.topologyPaths[path]; exists {
		return nil
	}
	var err error
	if gw.topologyAdd != nil {
		err = gw.topologyAdd(path)
	} else {
		err = gw.fsw.Add(path)
	}
	if err != nil {
		return fmt.Errorf("watch topology path %s: %w", path, err)
	}
	gw.topologyPaths[path] = struct{}{}
	return nil
}

func (gw *GitWatcher) removeTopologyPath(path string) {
	path = filepath.Clean(path)
	gw.mu.Lock()
	if _, exists := gw.topologyPaths[path]; !exists {
		gw.mu.Unlock()
		return
	}
	delete(gw.topologyPaths, path)
	gw.mu.Unlock()
	if gw.topologyRemove != nil {
		_ = gw.topologyRemove(path)
	} else {
		_ = gw.fsw.Remove(path)
	}
}

func (gw *GitWatcher) refreshTopologyWatches() {
	err := gw.refreshRequiredWatchesChecked()
	gw.recordTopologyRefresh(err)
}

func (gw *GitWatcher) refreshTopologyWatchesChecked() error {
	gw.topologyRefreshMu.Lock()
	defer gw.topologyRefreshMu.Unlock()
	return gw.refreshTopologyWatchesLocked()
}

func (gw *GitWatcher) refreshTopologyWatchesLocked() error {
	gw.mu.Lock()
	if !gw.topologyOwned || gw.stopCalled {
		gw.mu.Unlock()
		return nil
	}
	worktreesDir := gw.worktreesDir
	previousRoots := clonePathSet(gw.worktreeRoots)
	previousAdmins := clonePathSet(gw.worktreeAdminDirs)
	previousPaths := clonePathSet(gw.topologyPaths)
	inventoryFn := gw.inventory
	gw.mu.Unlock()
	if worktreesDir == "" {
		return fmt.Errorf("git worktrees directory is unavailable")
	}
	if inventoryFn == nil {
		inventoryFn = gitstate.Inventory
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	inventory, err := inventoryFn(ctx, gw.repoPath)
	cancel()
	if err != nil {
		return fmt.Errorf("inventory worktree topology: %w", err)
	}
	desiredRoots := make(map[string]struct{}, len(inventory.Records))
	for _, record := range inventory.Records {
		if record.RootAccessible && !record.Bare {
			desiredRoots[filepath.Clean(record.Path)] = struct{}{}
		}
	}

	desiredAdmins := make(map[string]struct{})
	worktreesExists := false
	entries, readErr := os.ReadDir(worktreesDir)
	if readErr == nil {
		worktreesExists = true
		for _, entry := range entries {
			if entry.IsDir() {
				desiredAdmins[filepath.Join(worktreesDir, entry.Name())] = struct{}{}
			}
		}
	} else if !errors.Is(readErr, os.ErrNotExist) {
		return fmt.Errorf("read git worktrees directory %s: %w", worktreesDir, readErr)
	}

	var addFailures []error
	if worktreesExists {
		addFailures = append(addFailures, gw.addTopologyPathChecked(worktreesDir))
	}
	for path := range desiredAdmins {
		addFailures = append(addFailures, gw.addTopologyPathChecked(path))
	}
	for path := range desiredRoots {
		addFailures = append(addFailures, gw.addTopologyPathChecked(path))
	}
	if addErr := errors.Join(addFailures...); addErr != nil {
		gw.mu.Lock()
		currentPaths := clonePathSet(gw.topologyPaths)
		gw.mu.Unlock()
		for path := range currentPaths {
			if _, existed := previousPaths[path]; !existed {
				gw.removeTopologyPath(path)
			}
		}
		return addErr
	}

	gw.mu.Lock()
	gw.worktreeRoots = desiredRoots
	gw.worktreeAdminDirs = desiredAdmins
	gw.mu.Unlock()
	for path := range previousRoots {
		if _, keep := desiredRoots[path]; !keep {
			gw.removeTopologyPath(path)
		}
	}
	for path := range previousAdmins {
		if _, keep := desiredAdmins[path]; !keep {
			gw.removeTopologyPath(path)
		}
	}
	if !worktreesExists {
		gw.removeTopologyPath(worktreesDir)
	}
	return nil
}

func (gw *GitWatcher) topologyDegradedReason() string {
	gw.mu.Lock()
	defer gw.mu.Unlock()
	return gw.topologyDegraded
}

func (gw *GitWatcher) recordTopologyRefresh(err error) {
	gw.mu.Lock()
	active := (gw.loopStarted || gw.topologyOwned) && !gw.stopCalled
	if err == nil {
		gw.topologyDegraded = ""
	} else {
		gw.topologyDegraded = err.Error()
	}
	gw.mu.Unlock()
	if err == nil || !active {
		gw.cancelTopologyRetry()
		return
	}
	gw.scheduleTopologyRetry()
}

func (gw *GitWatcher) cancelTopologyRetry() {
	gw.topologyRetryMu.Lock()
	gw.topologyRetryEpoch++
	gw.topologyRetryAttempts = 0
	if gw.topologyRetryTimer != nil {
		gw.topologyRetryTimer.Stop()
		gw.topologyRetryTimer = nil
	}
	gw.topologyRetryMu.Unlock()
}

func topologyRetryDelay(base, maximum time.Duration, attempt uint32) time.Duration {
	if base <= 0 {
		base = time.Second
	}
	if maximum <= 0 {
		maximum = 30 * time.Second
	}
	if maximum < base {
		maximum = base
	}
	delay := base
	for remaining := attempt; remaining > 0 && delay < maximum; remaining-- {
		if delay >= maximum/2 {
			return maximum
		}
		delay *= 2
	}
	if delay > maximum {
		return maximum
	}
	return delay
}

func (gw *GitWatcher) scheduleTopologyRetry() {
	gw.topologyRetryMu.Lock()
	if gw.topologyRetryClosing || gw.topologyRetryTimer != nil {
		gw.topologyRetryMu.Unlock()
		return
	}
	delay := topologyRetryDelay(gw.topologyRetryBase, gw.topologyRetryMax, gw.topologyRetryAttempts)
	if gw.topologyRetryAttempts < ^uint32(0) {
		gw.topologyRetryAttempts++
	}
	gw.topologyRetryEpoch++
	epoch := gw.topologyRetryEpoch
	gw.topologyRetryTimer = time.AfterFunc(delay, func() {
		gw.runTopologyRetry(epoch)
	})
	gw.topologyRetryMu.Unlock()
}

func (gw *GitWatcher) runTopologyRetry(epoch uint64) {
	gw.topologyRetryMu.Lock()
	if gw.topologyRetryClosing || gw.topologyRetryEpoch != epoch {
		gw.topologyRetryMu.Unlock()
		return
	}
	gw.topologyRetryTimer = nil
	gw.topologyRetryWG.Add(1)
	gw.topologyRetryMu.Unlock()
	defer gw.topologyRetryWG.Done()

	gw.topologyRefreshMu.Lock()
	gw.topologyRetryMu.Lock()
	current := !gw.topologyRetryClosing && gw.topologyRetryEpoch == epoch
	gw.topologyRetryMu.Unlock()
	if !current {
		gw.topologyRefreshMu.Unlock()
		return
	}
	err := gw.refreshRequiredWatchesLocked()
	gw.topologyRefreshMu.Unlock()
	if err != nil {
		gw.recordTopologyRefresh(err)
		return
	}
	gw.recordTopologyRefresh(nil)
	// A ref event can be lost while HEAD/packed-refs/refs-heads registration is
	// degraded. Every recovered watcher therefore re-samples its own HEAD; the
	// elected owner also nudges family topology through the ordinary debounced
	// path. Both timers fire after this retry lease returns, so a callback may
	// synchronously remove/stop the watcher without waiting on its own lease.
	gw.scheduleReconcile("git-watch-recovered")
	gw.scheduleTopologyChange("topology-watch-recovered")
}

func clonePathSet(source map[string]struct{}) map[string]struct{} {
	cloned := make(map[string]struct{}, len(source))
	for path := range source {
		cloned[path] = struct{}{}
	}
	return cloned
}

// gitWatcherScopedResolveMaxFiles caps how many changed files a ref
// reconcile resolves through the scoped incremental path before it
// falls back to a single whole-graph ResolveAll. Below the cap a
// commit-sized change resolves only its own files (and their incoming
// refs) -- the same path edit_file uses; above it (branch switches,
// large merges) one amortised whole-graph pass beats many per-file
// resolves. A package var so tests can lower it to exercise the
// fallback.
var gitWatcherScopedResolveMaxFiles = 100

// changedAbsPaths collects the absolute on-disk paths a diff touched --
// the new path for every status plus the old path of a rename --
// deduplicated. Disk existence is resolved later by
// IncrementalReindexPaths: a path present on disk is reindexed, one
// absent is evicted, so a delete needs no special-casing here.
func (gw *GitWatcher) changedAbsPaths(changes []gitChange) []string {
	seen := make(map[string]struct{}, len(changes)+4)
	out := make([]string, 0, len(changes)+4)
	add := func(rel string) {
		if rel == "" {
			return
		}
		abs := filepath.Join(gw.repoPath, rel)
		if _, ok := seen[abs]; ok {
			return
		}
		seen[abs] = struct{}{}
		out = append(out, abs)
	}
	for _, c := range changes {
		add(c.Path)
		add(c.OldPath)
	}
	return out
}

// reconcile reads the current HEAD SHA, diffs against the previously
// seen SHA via `git diff --name-status`, and dispatches
// the changed paths to a scoped reindex + resolve (whole-graph only
// above gitWatcherScopedResolveMaxFiles). Silently no-ops when HEAD hasn't moved — branches can
// touch packed-refs without the resolved commit actually changing.
func (gw *GitWatcher) reconcile(trigger string) {
	// Single-flight: one reconcile at a time. A ref change observed
	// mid-flight is coalesced into exactly one follow-up run that
	// diffs the (now advanced) lastSHA against wherever HEAD ended
	// up — so we always converge on the latest state without ever
	// running two whole-graph passes concurrently.
	gw.mu.Lock()
	if gw.reconciling {
		gw.rerun = true
		gw.mu.Unlock()
		return
	}
	gw.reconciling = true
	gw.mu.Unlock()
	defer func() {
		gw.mu.Lock()
		gw.reconciling = false
		again := gw.rerun && !gw.stopCalled
		gw.rerun = false
		gw.mu.Unlock()
		if again {
			go gw.reconcile("coalesced")
		}
	}()

	if gw.rebaseInProgress() {
		// Defer until the rebase lands. The final rebase state
		// either updates HEAD (triggering another fsnotify fire) or
		// leaves the branch where it was — in both cases we'll pick
		// up the right end state on the next event.
		gw.logger.Debug("git-watcher: rebase in progress, deferring",
			zap.String("trigger", trigger))
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	newSHA, err := gw.currentSHA(ctx)
	if err != nil || newSHA == "" {
		return
	}

	gw.mu.Lock()
	oldSHA := gw.lastSHA
	gw.mu.Unlock()
	if oldSHA == newSHA {
		return
	}

	// First observation (no prior SHA) needs no diff because the caller
	// warmed up with a full index already, but its SHA/restamp publication
	// still uses the repository lane and current Indexer.
	if oldSHA == "" {
		if err := gw.finalizeReconcile(context.Background(), newSHA); err != nil {
			gw.logger.Warn("git-watcher: freshness finalization failed",
				zap.String("trigger", trigger), zap.Error(err))
		}
		return
	}

	start := time.Now()
	changes, err := gw.diffNameStatus(ctx, oldSHA, newSHA)
	if err != nil {
		gw.logger.Warn("git-watcher: diff failed",
			zap.String("from", oldSHA), zap.String("to", newSHA), zap.Error(err))
		return
	}

	// Every ref-sized change set now uses the same bounded multi-file
	// parse/evict pipeline. The historical threshold remains observable so
	// branch-switch tests and logs distinguish large reconciles, but it no
	// longer selects the N-per-file + whole-graph fallback that caused the
	// warm/storm regression.
	changedPaths := gw.changedAbsPaths(changes)
	largeBatch := len(changedPaths) > gitWatcherScopedResolveMaxFiles
	patched, failed := 0, 0
	if len(changedPaths) > 0 {
		res, rerr := gw.reindexChangedPaths(changedPaths)
		if rerr != nil {
			// The bounded pipeline isolates individual unreadable/parse-failed
			// files in FailedFiles. A returned error is batch-level: keep the old
			// SHA so the next ref notification retries instead of hiding drift.
			gw.logger.Warn("git-watcher: batched reindex failed",
				zap.String("trigger", trigger),
				zap.Int("paths", len(changedPaths)),
				zap.Bool("large_batch", largeBatch),
				zap.Error(rerr))
			return
		}
		if res != nil {
			patched = res.StaleFileCount + res.DeletedFileCount
			failed = len(res.FailedFiles)
			if failed > 0 {
				gw.logger.Warn("git-watcher: batched reindex left failed files",
					zap.String("trigger", trigger),
					zap.Int("paths", len(changedPaths)),
					zap.Int("failed", failed),
					zap.Bool("large_batch", largeBatch))
				return
			}
		}
	}

	// Publish the new SHA and on-disk freshness row together under the stable
	// repository lane. Empty commits reach this tail without calling the batch
	// reindexer; lane admission failure intentionally keeps oldSHA for retry.
	if err := gw.finalizeReconcile(context.Background(), newSHA); err != nil {
		gw.logger.Warn("git-watcher: freshness finalization failed",
			zap.String("trigger", trigger),
			zap.String("from", oldSHA),
			zap.String("to", newSHA),
			zap.Error(err))
		return
	}
	gw.mu.Lock()
	drained := gw.drained
	gw.mu.Unlock()

	gw.logger.Info("git-watcher: reconciled ref change",
		zap.String("from", oldSHA[:min(len(oldSHA), 12)]),
		zap.String("to", newSHA[:min(len(newSHA), 12)]),
		zap.Int("paths", patched),
		zap.Int("failed", failed),
		zap.Bool("large_batch", largeBatch),
		zap.Duration("elapsed", time.Since(start)))

	if drained != nil {
		drained(patched)
	}
}

// gitChange describes one entry in `git diff --name-status` output.
// Status is a single char (A/M/D/T) or R/C with a similarity score.
type gitChange struct {
	Status  byte
	Path    string
	OldPath string // only populated for R/C
}

// applyChanges is retained for in-package compatibility. It delegates the
// entire diff to the bounded batch runner; it never loops through point
// IndexFileNoResolve/EvictFile mutations.
func (gw *GitWatcher) applyChanges(changes []gitChange) int {
	paths := gw.changedAbsPaths(changes)
	if len(paths) == 0 {
		return 0
	}
	result, err := gw.reindexChangedPaths(paths)
	if err != nil {
		gw.logger.Warn("git-watcher: batched apply failed",
			zap.Int("paths", len(paths)), zap.Error(err))
		return 0
	}
	if result == nil {
		return 0
	}
	return result.StaleFileCount + result.DeletedFileCount
}

// currentSHA returns the resolved commit SHA of HEAD. Shells out to
// git rather than parsing .git/HEAD directly so symbolic refs,
// packed-refs, and worktree indirection all work without us
// reimplementing git's ref resolution.
func (gw *GitWatcher) currentSHA(ctx context.Context) (string, error) {
	return gitcmd.Output(ctx, gw.repoPath, "rev-parse", "HEAD")
}

// diffNameStatus shells out to `git diff --name-status -M -C oldSHA..newSHA`
// and decodes the output into gitChange records. -M enables rename
// detection, -C enables copy detection.
func (gw *GitWatcher) diffNameStatus(ctx context.Context, oldSHA, newSHA string) ([]gitChange, error) {
	out, err := gitcmd.Run(ctx, gw.repoPath,
		"diff", "--name-status", "-M", "-C", "-z", oldSHA, newSHA)
	if err != nil {
		return nil, err
	}
	return parseDiffNameStatus(out), nil
}

// parseDiffNameStatus decodes the `-z` NUL-delimited output of
// `git diff --name-status`. Each entry is: STATUS\0path[\0newpath].
// Rename (R) and copy (C) statuses carry a similarity score appended
// to the letter (e.g., "R100") and come with two paths separated by
// a NUL. Everything else is a single path.
func parseDiffNameStatus(out []byte) []gitChange {
	var changes []gitChange
	// bufio.Scanner with a NUL split function gives us one token per
	// field; we consume them in pairs (or triples for R/C).
	scanner := bufio.NewScanner(strings.NewReader(string(out)))
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	scanner.Split(scanNul)

	for scanner.Scan() {
		status := scanner.Text()
		if status == "" {
			continue
		}
		letter := status[0]
		if !scanner.Scan() {
			break
		}
		path := scanner.Text()
		c := gitChange{Status: letter, Path: path}
		if letter == 'R' || letter == 'C' {
			if !scanner.Scan() {
				break
			}
			c.OldPath = path
			c.Path = scanner.Text()
		}
		changes = append(changes, c)
	}
	return changes
}

// scanNul is a bufio.SplitFunc that tokenises on NUL bytes. Used for
// `git diff -z` output where paths may contain whitespace.
func scanNul(data []byte, atEOF bool) (advance int, token []byte, err error) {
	if atEOF && len(data) == 0 {
		return 0, nil, nil
	}
	for i, b := range data {
		if b == 0 {
			return i + 1, data[:i], nil
		}
	}
	if atEOF {
		return len(data), data, nil
	}
	return 0, nil, nil
}

// rebaseInProgress reports whether a rebase, merge, or cherry-pick is
// currently in flight — any of which touch HEAD/refs rapidly and
// produce intermediate states we don't want to reconcile against.
// Detection is by sentinel file presence in .git (the canonical way
// other git tooling does it).
func (gw *GitWatcher) rebaseInProgress() bool {
	gitDir, err := resolveGitDir(gw.repoPath)
	if err != nil {
		return false
	}
	for _, sentinel := range []string{"rebase-merge", "rebase-apply",
		"MERGE_HEAD", "CHERRY_PICK_HEAD", "BISECT_LOG"} {
		if _, err := os.Stat(filepath.Join(gitDir, sentinel)); err == nil {
			return true
		}
	}
	return false
}

// resolveGitCommonDir returns the shared git administrative directory for
// the checkout family. Prefer git's absolute-path form and retain compatibility
// with older git releases that only return a path relative to the checkout.
func resolveGitCommonDir(ctx context.Context, repoPath string) (string, error) {
	out, err := gitcmd.Output(ctx, repoPath, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		out, err = gitcmd.Output(ctx, repoPath, "rev-parse", "--git-common-dir")
		if err != nil {
			return "", err
		}
	}
	if !filepath.IsAbs(out) {
		out = filepath.Join(repoPath, out)
	}
	return filepath.Clean(out), nil
}

// resolveGitDir returns the absolute path to the .git directory for a
// worktree. Handles the worktree / submodule case where .git is a
// file containing `gitdir: <path>` instead of a directory.
func resolveGitDir(repoPath string) (string, error) {
	candidate := filepath.Join(repoPath, ".git")
	info, err := os.Stat(candidate)
	if err != nil {
		return "", err
	}
	if info.IsDir() {
		return candidate, nil
	}
	// .git is a file pointing at the real gitdir — common for
	// worktrees (under modules/<name>) and submodules.
	content, err := os.ReadFile(candidate)
	if err != nil {
		return "", err
	}
	line := strings.TrimSpace(string(content))
	const prefix = "gitdir:"
	if !strings.HasPrefix(line, prefix) {
		return "", fmt.Errorf("malformed .git file: %s", candidate)
	}
	dir := strings.TrimSpace(strings.TrimPrefix(line, prefix))
	if !filepath.IsAbs(dir) {
		dir = filepath.Join(repoPath, dir)
	}
	return dir, nil
}
