package indexer

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
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
const defaultGitTopologyProbeInterval = time.Second

var errGitTopologyProbeUnstable = errors.New("git topology changed while inventory was sampled")

type gitWatcherTimerLease struct {
	once sync.Once
	wg   *sync.WaitGroup
}

func (lease *gitWatcherTimerLease) release() {
	if lease == nil || lease.wg == nil {
		return
	}
	lease.once.Do(lease.wg.Done)
}

type GitWatcher struct {
	repoPath string
	// indexer is the construction-time stable repository-lane carrier.
	// MultiWatcher installs currentIndexer so the freshness tail resolves the
	// currently registered Indexer only after that lane admits it.
	indexer                       *Indexer
	currentIndexer                func() *Indexer
	logger                        *zap.Logger
	fsw                           *fsnotify.Watcher
	debounce                      time.Duration
	done                          chan struct{}
	stopped                       chan struct{}
	mu                            sync.Mutex
	lastSHA                       string
	fireTimer                     *time.Timer
	fireTimerLease                *gitWatcherTimerLease
	reconcileTimerWG              sync.WaitGroup
	reconcileTimerEntered         func() // deterministic lifecycle hook; nil in production
	reconcileContinuationAdmitted func() // deterministic lifecycle hook; nil in production
	loopStarted                   bool
	stopCalled                    bool
	stopComplete                  chan struct{}

	// Worktree topology belongs to the shared git common directory, not to
	// this checkout's private gitdir. Every watcher runs one bounded control
	// probe for its exact ref state; exactly one elected watcher also samples
	// family topology. Stable ticks inspect only bounded Git admin metadata and
	// stat known roots; a full git inventory runs only after that signature
	// changes. Source roots are never handed to fsnotify: Darwin's kqueue
	// backend otherwise opens one descriptor per child file.
	gitDir                 string
	commonDir              string
	worktreesDir           string
	topologyTimer          *time.Timer
	topologyChange         func(string)
	topologyRefreshMu      sync.Mutex
	topologyOwned          bool
	topologyOwnerEpoch     uint64
	topologySignature      string
	topologyProbeSignature string
	topologyProbeInterval  time.Duration
	worktreeRoots          map[string]struct{}
	inventory              func(context.Context, string) (*gitstate.FamilyInventory, error)
	topologyDegraded       string
	refProbeSignature      string
	refPaths               map[string]struct{}
	refAdd                 func(string) error
	refRemove              func(string) error

	// The control probe has an independent epoch and cancellation domain. Stop
	// cancels and joins the exact old epoch before teardown, preventing an ABA
	// resurrection. Topology owner epochs separately guard family publication.
	topologyProbeMu     sync.Mutex
	topologyProbeCancel context.CancelFunc
	topologyProbeDone   chan struct{}
	topologyProbeEpoch  uint64

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

// NewGitWatcher creates a watcher for repoPath/.git/HEAD. idx is the already
// warmed repository indexer: GitWatcher reconciles commit movement and does
// not replace initial source discovery. repoPath is the absolute worktree
// root; the .git dir is discovered from HEAD (including worktree/submodule
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
		repoPath:              absRepo,
		indexer:               idx,
		logger:                logger,
		fsw:                   fsw,
		debounce:              300 * time.Millisecond,
		done:                  make(chan struct{}),
		stopped:               make(chan struct{}),
		stopComplete:          make(chan struct{}),
		topologyOwned:         true,
		topologyOwnerEpoch:    1,
		topologyProbeInterval: defaultGitTopologyProbeInterval,
		worktreeRoots:         make(map[string]struct{}),
		refPaths:              make(map[string]struct{}),
		inventory:             gitstate.Inventory,
		topologyRetryBase:     time.Second,
		topologyRetryMax:      30 * time.Second,
	}, nil
}

// OnWorktreeChange installs the callback used to reconcile the checkout
// family after a topology change. The callback receives the watcher checkout
// root, which is a stable selector for resolving the family.
//
// Stop suppresses callbacks that have not been admitted yet, but a callback
// already admitted by the topology debounce may finish after GitWatcher.Stop
// returns. Consumers must make the callback reentrant-safe and epoch-gate its
// effects. MultiWatcher provides that stronger dispatch-and-drain lifecycle.
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
// common-directory topology baseline is coherent. Callers admitting a new
// Git watcher use the error to roll back publication; established owners
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
	gw.topologyOwnerEpoch++
	if !owner {
		if gw.topologyTimer != nil {
			gw.topologyTimer.Stop()
			gw.topologyTimer = nil
		}
		gw.worktreeRoots = make(map[string]struct{})
		gw.topologySignature = ""
		gw.topologyProbeSignature = ""
		gw.topologyDegraded = ""
		refsReady := gw.gitDir != "" && gw.commonDir != ""
		gw.mu.Unlock()
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
// lookup and state restamp cannot land on a retired Indexer.
//
// The watcher baseline is acknowledged after successful lane admission, not
// inside the callback. A dedicated immutable corpus can intentionally suppress
// ordinary repository mutations through the lane guard; that is a successful
// handoff, but the callback does not run. Keeping lastSHA inside the callback
// made every later ref notification replay and log the same transition. A real
// lane or callback error still leaves the prior SHA for the next notification
// to retry.
func (gw *GitWatcher) finalizeReconcile(ctx context.Context, newSHA string) error {
	if err := gw.indexer.coordinateRepositoryMutation(ctx, func() error {
		idx := gw.registeredIndexer()
		if idx == nil {
			return fmt.Errorf("git-watcher: repository indexer is no longer registered")
		}
		idx.reconcileRepoIndexState(gw.repoPath)
		return nil
	}); err != nil {
		return err
	}
	gw.mu.Lock()
	gw.lastSHA = newSHA
	gw.mu.Unlock()
	return nil
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

	initialSHA, err := gw.currentSHA(context.Background())
	if err != nil {
		// An unborn symbolic HEAD makes git print the token "HEAD" while
		// returning an error. Do not publish that unresolved token as a commit
		// baseline: the already-warm first observation is intentionally empty.
		initialSHA = ""
	}
	gw.mu.Lock()
	gw.lastSHA = initialSHA
	gw.loopStarted = true
	gw.mu.Unlock()
	go gw.loop()
	// Every checkout probes its bounded ref-control signature. Only the elected
	// family owner additionally samples worktree topology.
	gw.startTopologyProbe()
	return nil
}

// Stop halts the watcher. Idempotent — safe whether Start succeeded,
// failed, or was never called. We only block on `stopped` when the
// loop goroutine is actually running; otherwise Stop would deadlock
// on a channel nobody's going to close.
func (gw *GitWatcher) Stop() error {
	gw.mu.Lock()
	if gw.stopComplete == nil {
		gw.stopComplete = make(chan struct{})
	}
	stopComplete := gw.stopComplete
	if gw.stopCalled {
		gw.mu.Unlock()
		<-stopComplete
		return nil
	}
	started := gw.loopStarted
	gw.stopCalled = true
	gw.topologyOwnerEpoch++
	if gw.fireTimer != nil && gw.fireTimer.Stop() {
		gw.fireTimerLease.release()
	}
	gw.fireTimer = nil
	gw.fireTimerLease = nil
	if gw.topologyTimer != nil {
		gw.topologyTimer.Stop()
	}
	gw.mu.Unlock()
	defer close(stopComplete)

	gw.stopTopologyProbe()

	gw.topologyRetryMu.Lock()
	gw.topologyRetryClosing = true
	gw.topologyRetryEpoch++
	if gw.topologyRetryTimer != nil {
		gw.topologyRetryTimer.Stop()
		gw.topologyRetryTimer = nil
	}
	gw.topologyRetryMu.Unlock()
	gw.topologyRetryWG.Wait()
	// A timer that has entered its callback owns a lease until ref-watch
	// refresh and reconcile return. Stop cannot close fsnotify or return while
	// that old callback can still mutate or overlap a replacement watcher.
	gw.reconcileTimerWG.Wait()

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
			if gw.isRefEvent(event.Name) {
				if event.Op&(fsnotify.Remove|fsnotify.Rename) != 0 {
					// Atomic ref replacement invalidates the old exact file watch, but
					// that is an expected transition, not a degraded family. The
					// debounced refresh below re-adds the new inode and only schedules
					// exponential retry if registration actually fails.
					gw.invalidateRefWatch(event.Name)
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
	if gw.stopCalled {
		gw.mu.Unlock()
		return
	}
	if gw.fireTimer != nil && gw.fireTimer.Stop() {
		gw.fireTimerLease.release()
	}
	gw.reconcileTimerWG.Add(1)
	lease := &gitWatcherTimerLease{wg: &gw.reconcileTimerWG}
	gw.fireTimerLease = lease
	gw.fireTimer = time.AfterFunc(gw.debounce, func() {
		defer lease.release()
		gw.mu.Lock()
		if gw.fireTimerLease == lease {
			gw.fireTimer = nil
			gw.fireTimerLease = nil
		}
		stopped := gw.stopCalled
		entered := gw.reconcileTimerEntered
		gw.mu.Unlock()
		if stopped {
			return
		}
		if entered != nil {
			entered()
		}
		// Stop may begin while an entered callback is blocked on admission.
		// Recheck immediately before touching watches; if it begins later, its
		// Wait below keeps the callback leased until all mutation returns.
		gw.mu.Lock()
		stopped = gw.stopCalled
		gw.mu.Unlock()
		if stopped {
			return
		}
		gw.topologyRefreshMu.Lock()
		err := gw.refreshRefWatchesLocked()
		gw.topologyRefreshMu.Unlock()
		if err != nil {
			gw.recordTopologyRefresh(err)
		}
		gw.reconcile(trigger)
	})
	gw.mu.Unlock()
}

func (gw *GitWatcher) scheduleTopologyChange(trigger string) {
	gw.mu.Lock()
	defer gw.mu.Unlock()
	if gw.stopCalled || !gw.topologyOwned || gw.topologyChange == nil {
		return
	}
	epoch := gw.topologyOwnerEpoch
	if gw.topologyTimer != nil {
		gw.topologyTimer.Stop()
	}
	gw.topologyTimer = time.AfterFunc(gw.debounce, func() {
		gw.mu.Lock()
		callback := gw.topologyChange
		stopped := gw.stopCalled
		owner := gw.topologyOwned
		currentEpoch := gw.topologyOwnerEpoch
		gw.mu.Unlock()
		if stopped || !owner || currentEpoch != epoch || callback == nil {
			return
		}
		gw.logger.Debug("git-watcher: worktree topology changed", zap.String("trigger", trigger))
		callback(gw.repoPath)
	})
}

func (gw *GitWatcher) isRefEvent(name string) bool {
	name = filepath.Clean(name)
	gw.mu.Lock()
	_, watched := gw.refPaths[name]
	gw.mu.Unlock()
	return watched
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
	gw.mu.Unlock()
	if !owner {
		return refErr
	}
	return errors.Join(refErr, gw.refreshTopologyWatchesLocked())
}

type gitRefProbeObservation struct {
	signature string
	desired   map[string]struct{}
}

func appendGitRefStat(builder *strings.Builder, label string, info os.FileInfo) {
	appendTopologyField(builder, label+":present")
	appendTopologyField(builder, strconv.FormatInt(info.Size(), 10))
	appendTopologyField(builder, strconv.FormatInt(info.ModTime().UnixNano(), 10))
	appendTopologyField(builder, info.Mode().String())
}

// observeGitRefProbe reads a constant number of control files. It deliberately
// never watches a refs directory: on Darwin that would make kqueue open every
// child. The content of HEAD and its one active loose ref catches unborn-ref
// creation; packed-refs needs only bounded metadata because an existing file
// also has an exact fsnotify registration.
func observeGitRefProbe(gitDir, commonDir string) (gitRefProbeObservation, error) {
	headPath := filepath.Clean(filepath.Join(gitDir, "HEAD"))
	head, err := os.ReadFile(headPath)
	if err != nil {
		return gitRefProbeObservation{}, fmt.Errorf("read git HEAD %s: %w", headPath, err)
	}
	observation := gitRefProbeObservation{desired: map[string]struct{}{headPath: {}}}
	var signature strings.Builder
	appendTopologyField(&signature, headPath)
	appendTopologyField(&signature, string(head))
	var observeErr error

	packedRefs := filepath.Clean(filepath.Join(commonDir, "packed-refs"))
	if info, statErr := os.Stat(packedRefs); statErr == nil {
		observation.desired[packedRefs] = struct{}{}
		appendGitRefStat(&signature, "packed-refs", info)
	} else if errors.Is(statErr, os.ErrNotExist) {
		appendTopologyField(&signature, "packed-refs:absent")
	} else {
		observeErr = errors.Join(observeErr, fmt.Errorf("stat packed refs %s: %w", packedRefs, statErr))
	}

	headValue := strings.TrimSpace(string(head))
	if strings.HasPrefix(headValue, "ref:") {
		refName := strings.TrimSpace(strings.TrimPrefix(headValue, "ref:"))
		if !strings.HasPrefix(refName, "refs/") || filepath.IsAbs(refName) {
			return observation, errors.Join(observeErr, fmt.Errorf("invalid symbolic HEAD ref %q", refName))
		}
		refPath := filepath.Clean(filepath.Join(commonDir, filepath.FromSlash(refName)))
		if !pathWithin(commonDir, refPath) {
			return observation, errors.Join(observeErr, fmt.Errorf("symbolic HEAD ref escapes common directory: %q", refName))
		}
		appendTopologyField(&signature, refPath)
		if info, statErr := os.Stat(refPath); statErr == nil {
			observation.desired[refPath] = struct{}{}
			appendGitRefStat(&signature, "active-ref", info)
			contents, readErr := os.ReadFile(refPath)
			if readErr != nil {
				observeErr = errors.Join(observeErr, fmt.Errorf("read active loose ref %s: %w", refPath, readErr))
			} else {
				appendTopologyField(&signature, string(contents))
			}
		} else if errors.Is(statErr, os.ErrNotExist) {
			appendTopologyField(&signature, "active-ref:absent")
		} else {
			observeErr = errors.Join(observeErr, fmt.Errorf("stat active loose ref %s: %w", refPath, statErr))
		}
	} else {
		appendTopologyField(&signature, "HEAD:detached")
	}
	observation.signature = signature.String()
	return observation, observeErr
}

func (gw *GitWatcher) removeRefWatch(path string) {
	gw.mu.Lock()
	remove := gw.refRemove
	gw.mu.Unlock()
	if remove != nil {
		_ = remove(path)
		return
	}
	_ = gw.fsw.Remove(path)
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

	observation, discoverErr := observeGitRefProbe(gitDir, commonDir)
	desired := observation.desired
	if observation.signature != "" {
		// Publish the observed state before registration. If Add fails, stable
		// control-probe ticks defer to the unified exponential retry instead of
		// repeating the same syscall every second.
		gw.mu.Lock()
		if !gw.stopCalled {
			gw.refProbeSignature = observation.signature
		}
		gw.mu.Unlock()
	}
	var refreshErr error
	for path := range desired {
		gw.mu.Lock()
		if gw.stopCalled {
			gw.mu.Unlock()
			return errors.Join(discoverErr, refreshErr, fmt.Errorf("git watcher is stopped"))
		}
		if gw.refPaths == nil {
			gw.refPaths = make(map[string]struct{})
		}
		if _, exists := gw.refPaths[path]; exists {
			gw.mu.Unlock()
			continue
		}
		// Publish the path before the syscall so a Remove/Rename delivered
		// immediately after Add can invalidate this exact registration.
		gw.refPaths[path] = struct{}{}
		add := gw.refAdd
		gw.mu.Unlock()

		var err error
		if add != nil {
			err = add(path)
		} else {
			err = gw.fsw.Add(path)
		}

		gw.mu.Lock()
		_, stillRegistered := gw.refPaths[path]
		stopped = gw.stopCalled
		if err != nil || stopped {
			delete(gw.refPaths, path)
		}
		gw.mu.Unlock()
		if err != nil {
			refreshErr = errors.Join(refreshErr, fmt.Errorf("watch git ref path %s: %w", path, err))
			continue
		}
		if !stillRegistered || stopped {
			gw.removeRefWatch(path)
			refreshErr = errors.Join(refreshErr, fmt.Errorf("git ref watch invalidated during registration: %s", path))
		}
	}

	// Do not retire a previously healthy exact watch from a partial discovery
	// result. The unified retry will re-discover the complete desired set.
	if discoverErr == nil {
		gw.mu.Lock()
		existing := clonePathSet(gw.refPaths)
		gw.mu.Unlock()
		for path := range existing {
			if _, keep := desired[path]; keep {
				continue
			}
			gw.mu.Lock()
			delete(gw.refPaths, path)
			gw.mu.Unlock()
			gw.removeRefWatch(path)
		}
	}
	return errors.Join(discoverErr, refreshErr)
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
	active := gw.topologyOwned && !gw.stopCalled
	worktreesDir := gw.worktreesDir
	roots := clonePathSet(gw.worktreeRoots)
	ownerEpoch := gw.topologyOwnerEpoch
	gw.mu.Unlock()
	if !active {
		return nil
	}
	if worktreesDir == "" {
		return fmt.Errorf("git worktrees directory is unavailable")
	}

	// Inventory is a multi-file Git sample. Gate it with the cheap admin
	// observation and require the same observation afterward so an admin added
	// mid-command cannot be paired with an old inventory indefinitely.
	before, err := observeGitTopologyProbe(worktreesDir, roots)
	if err != nil {
		return err
	}
	gw.mu.Lock()
	if !gw.topologyOwned || gw.stopCalled || gw.topologyOwnerEpoch != ownerEpoch {
		gw.mu.Unlock()
		return nil
	}
	gw.topologyProbeSignature = before.signature
	gw.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	snapshot, err := gw.inventoryTopologySnapshot(ctx, before.adminSignature)
	cancel()
	if err != nil {
		return err
	}

	gw.mu.Lock()
	if !gw.topologyOwned || gw.stopCalled || gw.topologyOwnerEpoch != ownerEpoch {
		gw.mu.Unlock()
		return nil
	}
	changed := gw.topologySignature != "" && gw.topologySignature != snapshot.signature
	gw.topologySignature = snapshot.signature
	gw.topologyProbeSignature = snapshot.probeSignature
	gw.worktreeRoots = snapshot.roots
	gw.mu.Unlock()

	if changed {
		gw.scheduleTopologyChange("topology-refresh")
	}
	return nil
}

type gitTopologyProbeObservation struct {
	signature      string
	adminSignature string
	rootAccessible map[string]bool
}

type gitTopologySnapshot struct {
	signature      string
	probeSignature string
	roots          map[string]struct{}
}

func appendTopologyField(builder *strings.Builder, value string) {
	builder.WriteString(strconv.Itoa(len(value)))
	builder.WriteByte(':')
	builder.WriteString(value)
	builder.WriteByte('|')
}

func topologyBool(value bool) string {
	if value {
		return "1"
	}
	return "0"
}

// appendWorktreeHeadIdentity adds one linked worktree's accepted HEAD identity
// to the bounded topology signature. A symbolic HEAD is incomplete without the
// active loose ref: advancing the branch changes only that common-dir file.
// Missing files are valid transient/unborn states and get stable sentinels;
// every other read or validation failure enters the existing topology retry.
func appendWorktreeHeadIdentity(builder *strings.Builder, adminDir, commonDir string) error {
	headPath := filepath.Clean(filepath.Join(adminDir, "HEAD"))
	head, err := os.ReadFile(headPath)
	if errors.Is(err, os.ErrNotExist) {
		appendTopologyField(builder, "HEAD:absent")
		return nil
	}
	if err != nil {
		return fmt.Errorf("read worktree HEAD %s: %w", headPath, err)
	}
	appendTopologyField(builder, "HEAD:"+string(head))
	headInfo, err := os.Stat(headPath)
	if err != nil {
		return fmt.Errorf("stat worktree HEAD %s: %w", headPath, err)
	}
	// Content alone cannot observe a complete A -> B -> A transition between
	// two probe ticks. Git replaces HEAD through its lockfile protocol, so its
	// stable modification time is a bounded revision token for that ABA case.
	appendTopologyField(builder, "HEAD:mtime:"+strconv.FormatInt(headInfo.ModTime().UnixNano(), 10))
	appendTopologyField(builder, "HEAD:size:"+strconv.FormatInt(headInfo.Size(), 10))

	headValue := strings.TrimSpace(string(head))
	if !strings.HasPrefix(headValue, "ref:") {
		appendTopologyField(builder, "HEAD:detached")
		return nil
	}
	refName := strings.TrimSpace(strings.TrimPrefix(headValue, "ref:"))
	if !strings.HasPrefix(refName, "refs/") || filepath.IsAbs(refName) {
		return fmt.Errorf("invalid worktree symbolic HEAD ref %q", refName)
	}
	refPath := filepath.Clean(filepath.Join(commonDir, filepath.FromSlash(refName)))
	if !pathWithin(commonDir, refPath) {
		return fmt.Errorf("worktree symbolic HEAD ref escapes common directory: %q", refName)
	}
	appendTopologyField(builder, "active-ref:"+refName)
	contents, err := os.ReadFile(refPath)
	if errors.Is(err, os.ErrNotExist) {
		appendTopologyField(builder, "active-ref:absent")
		return nil
	}
	if err != nil {
		return fmt.Errorf("read worktree active loose ref %s: %w", refPath, err)
	}
	appendTopologyField(builder, "active-ref:"+string(contents))
	refInfo, err := os.Stat(refPath)
	if err != nil {
		return fmt.Errorf("stat worktree active loose ref %s: %w", refPath, err)
	}
	appendTopologyField(builder, "active-ref:mtime:"+strconv.FormatInt(refInfo.ModTime().UnixNano(), 10))
	appendTopologyField(builder, "active-ref:size:"+strconv.FormatInt(refInfo.Size(), 10))
	return nil
}

// observeGitTopologyProbe is the steady-state path. It reads only the bounded
// Git worktree administration inventory and stats known roots for reachability;
// it never walks a checkout and never spawns Git.
func observeGitTopologyProbe(worktreesDir string, roots map[string]struct{}) (gitTopologyProbeObservation, error) {
	observation := gitTopologyProbeObservation{
		rootAccessible: make(map[string]bool, len(roots)),
	}
	var admin strings.Builder
	commonDir := filepath.Clean(filepath.Dir(worktreesDir))
	entries, err := os.ReadDir(worktreesDir)
	if errors.Is(err, os.ErrNotExist) {
		appendTopologyField(&admin, "worktrees:absent")
	} else if err != nil {
		return observation, fmt.Errorf("read git worktrees directory %s: %w", worktreesDir, err)
	} else {
		appendTopologyField(&admin, "worktrees:present")
		for _, entry := range entries {
			appendTopologyField(&admin, entry.Name())
			appendTopologyField(&admin, topologyBool(entry.IsDir()))
			if !entry.IsDir() {
				continue
			}
			adminDir := filepath.Clean(filepath.Join(worktreesDir, entry.Name()))
			for _, name := range []string{"gitdir", "commondir"} {
				path := filepath.Join(adminDir, name)
				contents, readErr := os.ReadFile(path)
				if errors.Is(readErr, os.ErrNotExist) {
					appendTopologyField(&admin, name+":absent")
					continue
				}
				if readErr != nil {
					return observation, fmt.Errorf("read worktree admin metadata %s: %w", path, readErr)
				}
				appendTopologyField(&admin, name+":"+strings.TrimSpace(string(contents)))
			}
			if err := appendWorktreeHeadIdentity(&admin, adminDir, commonDir); err != nil {
				return observation, err
			}
			lockedPath := filepath.Join(adminDir, "locked")
			if _, statErr := os.Stat(lockedPath); statErr == nil {
				appendTopologyField(&admin, "locked:present")
			} else if errors.Is(statErr, os.ErrNotExist) {
				appendTopologyField(&admin, "locked:absent")
			} else {
				return observation, fmt.Errorf("stat worktree lock %s: %w", lockedPath, statErr)
			}
		}
	}
	observation.adminSignature = admin.String()

	rootPaths := make([]string, 0, len(roots))
	for root := range roots {
		rootPaths = append(rootPaths, filepath.Clean(root))
	}
	sort.Strings(rootPaths)
	var signature strings.Builder
	appendTopologyField(&signature, observation.adminSignature)
	for _, root := range rootPaths {
		accessible := false
		if _, statErr := os.Stat(root); statErr == nil {
			accessible = true
		}
		observation.rootAccessible[root] = accessible
		appendTopologyField(&signature, root)
		appendTopologyField(&signature, topologyBool(accessible))
	}
	observation.signature = signature.String()
	return observation, nil
}

func (gw *GitWatcher) inventoryTopologySnapshot(ctx context.Context, expectedAdminSignature string) (gitTopologySnapshot, error) {
	gw.mu.Lock()
	inventoryFn := gw.inventory
	repoPath := gw.repoPath
	worktreesDir := gw.worktreesDir
	gw.mu.Unlock()
	if inventoryFn == nil {
		inventoryFn = gitstate.Inventory
	}

	inventory, err := inventoryFn(ctx, repoPath)
	if err != nil {
		return gitTopologySnapshot{}, fmt.Errorf("inventory worktree topology: %w", err)
	}
	if inventory == nil {
		return gitTopologySnapshot{}, fmt.Errorf("inventory worktree topology: empty result")
	}
	roots := make(map[string]struct{}, len(inventory.Records))
	for _, record := range inventory.Records {
		if !record.Bare && record.Path != "" {
			roots[filepath.Clean(record.Path)] = struct{}{}
		}
	}
	observation, err := observeGitTopologyProbe(worktreesDir, roots)
	if err != nil {
		return gitTopologySnapshot{}, err
	}
	if expectedAdminSignature != "" && observation.adminSignature != expectedAdminSignature {
		return gitTopologySnapshot{}, errGitTopologyProbeUnstable
	}
	for _, record := range inventory.Records {
		if record.Bare || record.Path == "" {
			continue
		}
		if observation.rootAccessible[filepath.Clean(record.Path)] != record.RootAccessible {
			return gitTopologySnapshot{}, errGitTopologyProbeUnstable
		}
	}

	records := make([]string, 0, len(inventory.Records))
	for _, record := range inventory.Records {
		var encoded strings.Builder
		appendTopologyField(&encoded, filepath.Clean(record.Path))
		appendTopologyField(&encoded, record.AdminName)
		appendTopologyField(&encoded, topologyBool(record.IsMain))
		appendTopologyField(&encoded, topologyBool(record.Bare))
		appendTopologyField(&encoded, topologyBool(record.RootAccessible))
		appendTopologyField(&encoded, topologyBool(record.Locked))
		appendTopologyField(&encoded, topologyBool(record.Prunable))
		records = append(records, encoded.String())
	}
	sort.Strings(records)
	var signature strings.Builder
	appendTopologyField(&signature, observation.adminSignature)
	for _, record := range records {
		appendTopologyField(&signature, record)
	}
	return gitTopologySnapshot{
		signature:      signature.String(),
		probeSignature: observation.signature,
		roots:          roots,
	}, nil
}

func (gw *GitWatcher) startTopologyProbe() {
	gw.topologyProbeMu.Lock()
	if gw.topologyProbeCancel != nil {
		gw.topologyProbeMu.Unlock()
		return
	}
	gw.mu.Lock()
	active := !gw.stopCalled && gw.gitDir != "" && gw.commonDir != ""
	interval := gw.topologyProbeInterval
	gw.mu.Unlock()
	if !active {
		gw.topologyProbeMu.Unlock()
		return
	}
	if interval <= 0 {
		interval = defaultGitTopologyProbeInterval
	}
	ctx, cancel := context.WithCancel(context.Background())
	gw.topologyProbeEpoch++
	epoch := gw.topologyProbeEpoch
	done := make(chan struct{})
	gw.topologyProbeCancel = cancel
	gw.topologyProbeDone = done
	go gw.runTopologyProbe(ctx, epoch, interval, done)
	gw.topologyProbeMu.Unlock()
}

func (gw *GitWatcher) stopTopologyProbe() {
	gw.topologyProbeMu.Lock()
	gw.topologyProbeEpoch++
	cancel := gw.topologyProbeCancel
	done := gw.topologyProbeDone
	if cancel != nil {
		cancel()
	}
	gw.topologyProbeMu.Unlock()
	if done == nil {
		return
	}
	<-done
	gw.topologyProbeMu.Lock()
	if gw.topologyProbeDone == done {
		gw.topologyProbeCancel = nil
		gw.topologyProbeDone = nil
	}
	gw.topologyProbeMu.Unlock()
}

func (gw *GitWatcher) topologyProbeCurrent(epoch uint64) bool {
	gw.topologyProbeMu.Lock()
	current := gw.topologyProbeCancel != nil && gw.topologyProbeEpoch == epoch
	gw.topologyProbeMu.Unlock()
	return current
}

func (gw *GitWatcher) runTopologyProbe(ctx context.Context, epoch uint64, interval time.Duration, done chan struct{}) {
	defer close(done)
	timer := time.NewTimer(interval)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}

		if err := gw.probeGitStateOnce(ctx, epoch); err != nil {
			if ctx.Err() != nil {
				return
			}
			// Stable failed observations are marked pending by the probe. This
			// one error schedules the bounded exponential retry; later 1s ticks
			// remain cheap and never run inventory in parallel with it.
			gw.recordTopologyRefresh(err)
		}
		timer.Reset(interval)
	}
}

func (gw *GitWatcher) probeGitStateOnce(ctx context.Context, epoch uint64) error {
	gw.topologyRefreshMu.Lock()
	defer gw.topologyRefreshMu.Unlock()
	if !gw.topologyProbeCurrent(epoch) {
		return nil
	}
	refErr := gw.probeRefOnceLocked()
	topologyErr := gw.probeTopologyOnceLocked(ctx, epoch)
	return errors.Join(refErr, topologyErr)
}

func (gw *GitWatcher) probeRefOnceLocked() error {
	gw.mu.Lock()
	gitDir := gw.gitDir
	commonDir := gw.commonDir
	previous := gw.refProbeSignature
	stopped := gw.stopCalled
	gw.mu.Unlock()
	if stopped || gitDir == "" || commonDir == "" {
		return nil
	}
	observation, err := observeGitRefProbe(gitDir, commonDir)
	if err != nil {
		return err
	}
	if observation.signature == previous {
		return nil
	}
	gw.mu.Lock()
	if gw.stopCalled {
		gw.mu.Unlock()
		return nil
	}
	gw.refProbeSignature = observation.signature
	gw.mu.Unlock()
	if err := gw.refreshRefWatchesLocked(); err != nil {
		return err
	}
	gw.scheduleReconcile("git-ref-probe")
	return nil
}

func (gw *GitWatcher) topologyRetryPending() bool {
	gw.topologyRetryMu.Lock()
	pending := gw.topologyRetryTimer != nil
	gw.topologyRetryMu.Unlock()
	return pending
}

func (gw *GitWatcher) probeTopologyOnce(ctx context.Context, epoch uint64) error {
	gw.topologyRefreshMu.Lock()
	defer gw.topologyRefreshMu.Unlock()
	return gw.probeTopologyOnceLocked(ctx, epoch)
}

func (gw *GitWatcher) probeTopologyOnceLocked(ctx context.Context, epoch uint64) error {
	if !gw.topologyProbeCurrent(epoch) {
		return nil
	}
	gw.mu.Lock()
	if !gw.topologyOwned || gw.stopCalled {
		gw.mu.Unlock()
		return nil
	}
	worktreesDir := gw.worktreesDir
	roots := clonePathSet(gw.worktreeRoots)
	previousProbe := gw.topologyProbeSignature
	ownerEpoch := gw.topologyOwnerEpoch
	gw.mu.Unlock()

	observation, err := observeGitTopologyProbe(worktreesDir, roots)
	if err != nil {
		return err
	}
	if observation.signature == previousProbe {
		return nil
	}
	if !gw.topologyProbeCurrent(epoch) {
		return nil
	}
	// Mark this exact cheap observation pending before the expensive sample.
	// Regardless of ordinary or unstable inventory failure, stable ticks now
	// defer to one unified exponential-backoff retry.
	gw.mu.Lock()
	if !gw.topologyOwned || gw.stopCalled || gw.topologyOwnerEpoch != ownerEpoch {
		gw.mu.Unlock()
		return nil
	}
	gw.topologyProbeSignature = observation.signature
	gw.mu.Unlock()
	if gw.topologyRetryPending() {
		return nil
	}

	snapshot, err := gw.inventoryTopologySnapshot(ctx, observation.adminSignature)
	if err != nil {
		return err
	}
	if !gw.topologyProbeCurrent(epoch) {
		return nil
	}
	gw.mu.Lock()
	if !gw.topologyOwned || gw.stopCalled || gw.topologyOwnerEpoch != ownerEpoch {
		gw.mu.Unlock()
		return nil
	}
	changed := gw.topologySignature != "" && gw.topologySignature != snapshot.signature
	gw.topologySignature = snapshot.signature
	gw.topologyProbeSignature = snapshot.probeSignature
	gw.worktreeRoots = snapshot.roots
	gw.mu.Unlock()
	if changed {
		gw.scheduleTopologyChange("topology-probe")
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
	// Publish retry/backoff state before releasing the refresh serialization;
	// a 1s probe can never slip into a second inventory between failure and
	// scheduling the next exponential retry.
	gw.recordTopologyRefresh(err)
	gw.topologyRefreshMu.Unlock()
	if err != nil {
		return
	}
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
	if gw.stopCalled {
		gw.mu.Unlock()
		return
	}
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
		var continuationLease *gitWatcherTimerLease
		var continuationAdmitted func()
		if again {
			// Admit the coalesced continuation under the same mutex that closes
			// timer admission in Stop. Stop therefore cannot observe a zero
			// WaitGroup count and return before this goroutine starts.
			gw.reconcileTimerWG.Add(1)
			continuationLease = &gitWatcherTimerLease{wg: &gw.reconcileTimerWG}
			continuationAdmitted = gw.reconcileContinuationAdmitted
		}
		gw.mu.Unlock()
		if again {
			if continuationAdmitted != nil {
				continuationAdmitted()
			}
			go func() {
				defer continuationLease.release()
				gw.reconcile("coalesced")
			}()
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
