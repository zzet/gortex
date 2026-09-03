package indexer

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/pathkey"
)

type topologyDispatchEpoch struct {
	number        uint64
	owner         string
	watcher       *GitWatcher
	accepting     bool
	inFlight      int
	drained       chan struct{}
	drainedClosed bool
}

type topologyDispatchContextKey struct{}

type topologyDispatchToken struct {
	mw     *MultiWatcher
	epoch  *topologyDispatchEpoch
	active bool
}

// WorktreeChangeCallback receives a private dispatch context. Teardown reached
// synchronously from the callback must propagate this context so MultiWatcher
// can defer only that callback's exact lease without weakening ordinary Stop
// and RemoveRepo completion boundaries.
type WorktreeChangeCallback func(ctx context.Context, repoPrefix, rootPath string)

type admittedTopologyDispatch struct {
	mw       *MultiWatcher
	epoch    *topologyDispatchEpoch
	token    *topologyDispatchToken
	callback WorktreeChangeCallback
	prefix   string
	rootPath string
}

func (dispatch admittedTopologyDispatch) invoke() {
	ctx := context.WithValue(context.Background(), topologyDispatchContextKey{}, dispatch.token)
	defer dispatch.mw.releaseTopologyDispatch(dispatch.epoch, dispatch.token)
	dispatch.callback(ctx, dispatch.prefix, dispatch.rootPath)
}

type watcherForwarder struct {
	cancel chan struct{}
	done   chan struct{}
}

type watcherRetirement struct {
	prefix        string
	watcher       *Watcher
	gitWatcher    *GitWatcher
	forwarder     *watcherForwarder
	started       bool
	topologyDrain <-chan struct{}
	done          chan struct{}
	err           error
}

type pendingWatcherAdd struct {
	cfg config.WatchConfig
}

type watcherStopTarget struct {
	prefix  string
	watcher *Watcher
}

type multiWatcherStopPlan struct {
	topologyDrains []<-chan struct{}
	forwarders     []*watcherForwarder
	watchers       []watcherStopTarget
	gitWatchers    []*GitWatcher
	retirements    []*watcherRetirement
}

type topologyWatchFamily struct {
	commonDir string
	owner     string
	members   map[string]*GitWatcher
	nextEpoch uint64
	dispatch  *topologyDispatchEpoch
}

// MultiWatcher manages file watchers across multiple repositories.
type MultiWatcher struct {
	watchers    map[string]*Watcher    // repoPrefix → file watcher
	gitWatchers map[string]*GitWatcher // repoPrefix → .git ref watcher
	started     map[string]bool        // tracks which watchers have been started
	// startFailures records why a per-repo watcher never came up. A watcher
	// that failed to start has no degraded reason of its own — it has no
	// running state at all — so without this the repo is simply absent from
	// every health surface and the daemon reports it watched. This is the
	// only record that it is not.
	startFailures        map[string]string
	topologyFamilies     map[string]*topologyWatchFamily     // canonical common dir → family
	topologyFamilyByRepo map[string]string                   // repo prefix → canonical common dir
	topologyDispatches   map[*topologyDispatchEpoch]struct{} // accepting or draining epochs
	forwarders           map[string]*watcherForwarder        // repo prefix → joined event forwarder
	retiringWatchers     map[string]*watcherRetirement       // repo prefix → removal tombstone
	pendingWatcherAdds   map[string]*pendingWatcherAdd       // coalesced reentrant replacement intent
	multi                *MultiIndexer
	logger               *zap.Logger
	events               chan GraphChangeEvent
	done                 chan struct{}
	mu                   sync.Mutex
	stopped              bool
	stopDone             chan struct{}
	stopErr              error

	// symbolChangeCb is the OnSymbolChange callback registered by the
	// MCP server (or any other consumer). It's fanned out to every
	// per-repo Watcher and re-applied at AddRepo time so newly-tracked
	// repos pick it up without a second registration call. Guarded by
	// callbackMu so registration and per-repo apply don't race. The
	// topology callback is instead guarded by mu because it is captured
	// atomically with an ownership-epoch dispatch admission.
	callbackMu     sync.Mutex
	symbolChangeCb SymbolChangeCallback
	// degradedCb is fanned out to every per-repo Watcher (and re-applied at
	// AddRepo) so a watcher entering a degraded state — inotify / FD
	// exhaustion — pushes a single health notice through the daemon.
	degradedCb       func(reason string)
	worktreeChangeCb WorktreeChangeCallback
}

// NewMultiWatcher creates a MultiWatcher that watches all configured repos.
// Each repo gets its own Watcher with repo-specific exclude patterns.
func NewMultiWatcher(
	mi *MultiIndexer,
	configs map[string]config.WatchConfig,
	logger *zap.Logger,
) (*MultiWatcher, error) {
	mw := &MultiWatcher{
		watchers:             make(map[string]*Watcher),
		gitWatchers:          make(map[string]*GitWatcher),
		started:              make(map[string]bool),
		startFailures:        make(map[string]string),
		topologyFamilies:     make(map[string]*topologyWatchFamily),
		topologyFamilyByRepo: make(map[string]string),
		topologyDispatches:   make(map[*topologyDispatchEpoch]struct{}),
		forwarders:           make(map[string]*watcherForwarder),
		retiringWatchers:     make(map[string]*watcherRetirement),
		pendingWatcherAdds:   make(map[string]*pendingWatcherAdd),
		multi:                mi,
		logger:               logger,
		events:               make(chan GraphChangeEvent, 128),
		done:                 make(chan struct{}),
	}

	for prefix, cfg := range configs {
		if err := mw.createWatcher(prefix, cfg); err != nil {
			// Log warning and continue if a repo root is inaccessible.
			logger.Warn("failed to create watcher for repo",
				zap.String("prefix", prefix),
				zap.Error(err),
			)
			continue
		}
	}

	return mw, nil
}

// createWatcher creates a per-repo Watcher for the given prefix.
func (mw *MultiWatcher) createWatcher(prefix string, cfg config.WatchConfig) error {
	meta := mw.multi.GetMetadata(prefix)
	if meta == nil {
		return fmt.Errorf("repository not found: %s", prefix)
	}

	// Verify the repo root is accessible.
	if _, err := os.Stat(meta.RootPath); err != nil {
		return fmt.Errorf("repo root inaccessible: %s: %w", meta.RootPath, err)
	}

	idx := mw.multi.GetIndexer(prefix)
	if idx == nil {
		return fmt.Errorf("no indexer for repo: %s", prefix)
	}
	idx.attachRepositoryMutationCoordinator(mw.multi.repositoryMutationCoordinator(prefix))

	w, err := NewWatcher(idx, cfg, mw.logger.With(zap.String("repo", prefix)))
	if err != nil {
		return fmt.Errorf("creating watcher for %s: %w", prefix, err)
	}
	// The watcher outlives an IndexRepo replacement. Its construction-time
	// Indexer remains the stable lane carrier; graph work selects the current
	// registry entry only after that lane admits it.
	w.currentIndexer = func() *Indexer {
		return mw.multi.GetIndexer(prefix)
	}
	w.batchReindex = func(paths []string) (*IndexResult, error) {
		return mw.multi.IncrementalReindexRepo(prefix, paths)
	}
	w.discoverReindex = func(paths []string) (*IndexResult, error) {
		return mw.multi.incrementalDiscoverRepo(prefix, paths)
	}
	w.pointReindexRaw = func(filePath string) (*IndexResult, error) {
		return mw.multi.incrementalPointRepoRaw(prefix, filePath)
	}

	mw.watchers[prefix] = w
	return nil
}

func (mw *MultiWatcher) configureGitWatcher(prefix string, gw *GitWatcher) {
	gw.currentIndexer = func() *Indexer {
		return mw.multi.GetIndexer(prefix)
	}
	gw.batchReindex = func(paths []string) (*IndexResult, error) {
		return mw.multi.IncrementalReindexRepo(prefix, paths)
	}
}

// dispatchWorktreeTopologyChange revalidates ownership after GitWatcher's
// debounce/refresh window. A callback already queued by a removed owner is
// therefore dropped instead of racing the newly promoted owner and delivering
// the same family event twice.
func (mw *MultiWatcher) beginTopologyDispatchEpochLocked(family *topologyWatchFamily, prefix string, gw *GitWatcher) *topologyDispatchEpoch {
	if family == nil || gw == nil || mw.stopped {
		return nil
	}
	family.nextEpoch++
	epoch := &topologyDispatchEpoch{
		number:    family.nextEpoch,
		owner:     prefix,
		watcher:   gw,
		accepting: true,
		drained:   make(chan struct{}),
	}
	family.dispatch = epoch
	if mw.topologyDispatches == nil {
		mw.topologyDispatches = make(map[*topologyDispatchEpoch]struct{})
	}
	mw.topologyDispatches[epoch] = struct{}{}
	gw.OnWorktreeChange(func(rootPath string) {
		mw.dispatchWorktreeTopologyChange(prefix, gw, epoch, rootPath)
	})
	return epoch
}

func (mw *MultiWatcher) finishTopologyDispatchEpochLocked(epoch *topologyDispatchEpoch) {
	if epoch == nil || epoch.drainedClosed {
		return
	}
	epoch.drainedClosed = true
	close(epoch.drained)
	delete(mw.topologyDispatches, epoch)
}

func (mw *MultiWatcher) invalidateTopologyDispatchEpochLocked(family *topologyWatchFamily) <-chan struct{} {
	if family == nil || family.dispatch == nil {
		return nil
	}
	epoch := family.dispatch
	family.dispatch = nil
	epoch.accepting = false
	if epoch.inFlight == 0 {
		mw.finishTopologyDispatchEpochLocked(epoch)
	}
	return epoch.drained
}

func (mw *MultiWatcher) admitWorktreeTopologyChange(prefix string, gw *GitWatcher, epoch *topologyDispatchEpoch, rootPath string) (admittedTopologyDispatch, bool) {
	mw.mu.Lock()
	defer mw.mu.Unlock()

	key, exists := mw.topologyFamilyByRepo[prefix]
	family := mw.topologyFamilies[key]
	current := !mw.stopped && exists && family != nil && family.owner == prefix &&
		family.members[prefix] == gw && family.dispatch == epoch && epoch != nil &&
		epoch.accepting && epoch.owner == prefix && epoch.watcher == gw
	callback := mw.worktreeChangeCb
	if !current || callback == nil {
		return admittedTopologyDispatch{}, false
	}
	token := &topologyDispatchToken{mw: mw, epoch: epoch, active: true}
	epoch.inFlight++
	return admittedTopologyDispatch{
		mw:       mw,
		epoch:    epoch,
		token:    token,
		callback: callback,
		prefix:   prefix,
		rootPath: rootPath,
	}, true
}

func (mw *MultiWatcher) releaseTopologyDispatch(epoch *topologyDispatchEpoch, token *topologyDispatchToken) {
	if epoch == nil {
		return
	}
	mw.mu.Lock()
	if token != nil {
		if !token.active {
			mw.mu.Unlock()
			return
		}
		token.active = false
	}
	if epoch.inFlight > 0 {
		epoch.inFlight--
	}
	if !epoch.accepting && epoch.inFlight == 0 {
		mw.finishTopologyDispatchEpochLocked(epoch)
	}
	mw.mu.Unlock()
}

// RetainTopologyDispatch transfers one exact admitted callback lease to
// asynchronous reconciliation. The returned release is idempotent and must be
// called after that reconciliation is run, rejected, or superseded.
func (mw *MultiWatcher) RetainTopologyDispatch(ctx context.Context) (context.Context, func()) {
	if ctx == nil {
		ctx = context.Background()
	}
	retainedContext := context.WithoutCancel(ctx)

	mw.mu.Lock()
	token := mw.activeTopologyDispatchTokenLocked(ctx)
	if token == nil {
		mw.mu.Unlock()
		return retainedContext, func() {}
	}
	retained := &topologyDispatchToken{mw: mw, epoch: token.epoch, active: true}
	retained.epoch.inFlight++
	mw.mu.Unlock()

	retainedContext = context.WithValue(retainedContext, topologyDispatchContextKey{}, retained)
	var once sync.Once
	return retainedContext, func() {
		once.Do(func() { mw.releaseTopologyDispatch(retained.epoch, retained) })
	}
}

func (mw *MultiWatcher) activeTopologyDispatchTokenLocked(ctx context.Context) *topologyDispatchToken {
	if ctx == nil {
		return nil
	}
	token, _ := ctx.Value(topologyDispatchContextKey{}).(*topologyDispatchToken)
	if token == nil || token.mw != mw || token.epoch == nil || !token.active {
		return nil
	}
	if _, exists := mw.topologyDispatches[token.epoch]; !exists {
		return nil
	}
	return token
}

func (mw *MultiWatcher) topologyDispatchDrainsLocked() []<-chan struct{} {
	drains := make([]<-chan struct{}, 0, len(mw.topologyDispatches))
	for epoch := range mw.topologyDispatches {
		drains = append(drains, epoch.drained)
	}
	return drains
}

func waitTopologyDispatchDrains(drains ...<-chan struct{}) {
	var seen map[<-chan struct{}]struct{}
	for _, drain := range drains {
		if drain == nil {
			continue
		}
		if seen == nil {
			seen = make(map[<-chan struct{}]struct{}, len(drains))
		}
		if _, exists := seen[drain]; exists {
			continue
		}
		seen[drain] = struct{}{}
		<-drain
	}
}

func (mw *MultiWatcher) dispatchWorktreeTopologyChange(prefix string, gw *GitWatcher, epoch *topologyDispatchEpoch, rootPath string) {
	dispatch, admitted := mw.admitWorktreeTopologyChange(prefix, gw, epoch, rootPath)
	if !admitted {
		return
	}
	// The lease remains active through the external callback. Removal and Stop
	// invalidate its epoch under mw.mu, then wait without mw.mu, so callbacks
	// can safely re-enter ordinary MultiWatcher methods without lock inversion.
	dispatch.invoke()
}

func canonicalGitCommonDir(path string) string {
	path = filepath.Clean(path)
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		path = filepath.Clean(resolved)
	}
	return path
}

// topologyFamilyKeyLocked returns the already-established spelling for a Git
// common directory. SamePathIdentity handles case-folding and filesystem
// identity at this cold add/remove boundary without weakening distinct paths
// on case-sensitive volumes.
func (mw *MultiWatcher) topologyFamilyKeyLocked(commonDir string) string {
	canonical := canonicalGitCommonDir(commonDir)
	for key := range mw.topologyFamilies {
		if pathkey.SamePathIdentity(key, canonical) {
			return key
		}
	}
	return canonical
}

func (mw *MultiWatcher) electTopologyOwnerLocked(family *topologyWatchFamily) {
	if family == nil || len(family.members) == 0 {
		return
	}
	if gw, exists := family.members[family.owner]; exists {
		if family.dispatch == nil {
			mw.beginTopologyDispatchEpochLocked(family, family.owner, gw)
			gw.setTopologyOwner(true)
		}
		return
	}
	prefixes := make([]string, 0, len(family.members))
	for prefix := range family.members {
		prefixes = append(prefixes, prefix)
	}
	sort.Strings(prefixes)
	family.owner = prefixes[0]
	gw := family.members[family.owner]
	mw.beginTopologyDispatchEpochLocked(family, family.owner, gw)
	gw.setTopologyOwner(true)
}

func (mw *MultiWatcher) registerTopologyWatcherLocked(prefix string, gw *GitWatcher) <-chan struct{} {
	if gw == nil || mw.stopped {
		return nil
	}
	commonDir := gw.commonDirectory()
	if commonDir == "" {
		return nil
	}
	var drain <-chan struct{}
	if prior, exists := mw.topologyFamilyByRepo[prefix]; exists {
		if family := mw.topologyFamilies[prior]; family != nil && family.members[prefix] == gw {
			return nil
		}
		drain = mw.unregisterTopologyWatcherLocked(prefix)
	}

	key := mw.topologyFamilyKeyLocked(commonDir)
	family := mw.topologyFamilies[key]
	if family == nil {
		family = &topologyWatchFamily{
			commonDir: key,
			members:   make(map[string]*GitWatcher),
		}
		mw.topologyFamilies[key] = family
	}
	gw.OnWorktreeChange(nil)
	gw.setTopologyOwner(false)
	family.members[prefix] = gw
	mw.topologyFamilyByRepo[prefix] = key
	mw.electTopologyOwnerLocked(family)
	return drain
}

func (mw *MultiWatcher) installStartedGitWatcherLocked(prefix string, gw *GitWatcher) <-chan struct{} {
	if gw == nil {
		return nil
	}
	mw.gitWatchers[prefix] = gw
	return mw.registerTopologyWatcherLocked(prefix, gw)
}

func (mw *MultiWatcher) unregisterTopologyWatcherLocked(prefix string) <-chan struct{} {
	key, exists := mw.topologyFamilyByRepo[prefix]
	if !exists {
		return nil
	}
	delete(mw.topologyFamilyByRepo, prefix)
	family := mw.topologyFamilies[key]
	if family == nil {
		return nil
	}
	gw := family.members[prefix]
	wasOwner := family.owner == prefix
	var drain <-chan struct{}
	if wasOwner {
		drain = mw.invalidateTopologyDispatchEpochLocked(family)
		family.owner = ""
	}
	delete(family.members, prefix)
	if gw != nil {
		gw.OnWorktreeChange(nil)
		gw.setTopologyOwner(false)
	}
	if len(family.members) == 0 {
		delete(mw.topologyFamilies, key)
		return drain
	}
	if wasOwner {
		mw.electTopologyOwnerLocked(family)
		if promoted := family.members[family.owner]; promoted != nil {
			// Demotion cancels the retired owner's debounce timer. Always queue
			// one coalesced new-epoch nudge so a pending topology mutation is not
			// lost merely because ownership moved during its debounce window.
			promoted.scheduleTopologyChange("owner-transfer")
		}
	}
	return drain
}

// Start begins watching all configured repos. Events from per-repo watchers
// are merged into the single Events() channel.
func (mw *MultiWatcher) Start() error {
	mw.mu.Lock()
	var topologyDrains []<-chan struct{}
	defer func() {
		mw.mu.Unlock()
		waitTopologyDispatchDrains(topologyDrains...)
	}()
	if mw.stopped {
		return fmt.Errorf("multi-watcher is stopped")
	}

	// Per-repo watcher startup is independent, and each w.Start blocks
	// ~150ms on macOS draining the FSEvents initial-replay storm (plus
	// OS stream setup and a .git/HEAD watcher). Run them concurrently
	// so an N-repo daemon pays one drain window instead of N serialised
	// ones — on a 20-repo install this cuts ~3.4s of warmup to ~0.4s.
	//
	// Each goroutine writes only its own slot in results[] (no shared
	// write), and mw's started/gitWatchers maps plus the forwardEvents
	// goroutines are folded in serially after the wait. mw.mu stays
	// held for the whole call so Start/Stop can't interleave; the
	// concurrency here is purely within one Start.
	type startResult struct {
		prefix  string
		w       *Watcher
		gw      *GitWatcher
		ok      bool
		failure string
	}
	prefixes := make([]string, 0, len(mw.watchers))
	for prefix := range mw.watchers {
		if !mw.started[prefix] {
			prefixes = append(prefixes, prefix)
		}
	}
	sort.Strings(prefixes)
	results := make([]startResult, len(prefixes))
	var wg sync.WaitGroup
	for i, prefix := range prefixes {
		w := mw.watchers[prefix]
		meta := mw.multi.GetMetadata(prefix)
		// A repo skipped here is as unwatched as one whose Start failed, so
		// it is recorded the same way. Logging alone left the daemon
		// reporting it watched.
		if meta == nil {
			mw.logger.Warn("skipping watcher start: repo metadata not found",
				zap.String("prefix", prefix))
			results[i] = startResult{prefix: prefix, failure: "repository metadata not found"}
			continue
		}

		// Verify root is still accessible before starting.
		if _, err := os.Stat(meta.RootPath); err != nil {
			mw.logger.Warn("repo root inaccessible, skipping watcher",
				zap.String("prefix", prefix),
				zap.String("root", meta.RootPath),
				zap.Error(err),
			)
			results[i] = startResult{prefix: prefix, failure: "repository root is inaccessible: " + err.Error()}
			continue
		}

		wg.Add(1)
		go func(slot int, prefix string, w *Watcher, rootPath string) {
			defer wg.Done()
			if err := w.Start([]string{rootPath}); err != nil {
				mw.logger.Warn("failed to start watcher for repo",
					zap.String("prefix", prefix),
					zap.String("root", rootPath),
					zap.Error(err),
				)
				results[slot] = startResult{prefix: prefix, failure: err.Error()}
				return
			}
			res := startResult{prefix: prefix, w: w, ok: true}

			// Start the .git/HEAD watcher alongside the file watcher.
			// It's best-effort — repos without a .git dir (uninitialised
			// worktrees, tarball checkouts) simply skip it.
			if idx := mw.multi.GetIndexer(prefix); idx != nil {
				gw, err := NewGitWatcher(rootPath, idx, mw.logger.With(zap.String("repo", prefix)))
				if err == nil {
					gw.setTopologyOwner(false)
					mw.configureGitWatcher(prefix, gw)
				}
				if err != nil {
					mw.logger.Debug("git-watcher: init failed",
						zap.String("prefix", prefix), zap.Error(err))
				} else if err := gw.Start(); err != nil {
					mw.logger.Debug("git-watcher: start failed",
						zap.String("prefix", prefix), zap.Error(err))
					_ = gw.Stop()
				} else {
					res.gw = gw
				}
			}
			results[slot] = res
		}(i, prefix, w, meta.RootPath)
	}
	wg.Wait()

	for _, res := range results {
		if !res.ok {
			if res.failure != "" {
				mw.startFailures[res.prefix] = res.failure
			}
			continue
		}
		delete(mw.startFailures, res.prefix)
		mw.started[res.prefix] = true
		if drain := mw.installStartedGitWatcherLocked(res.prefix, res.gw); drain != nil {
			topologyDrains = append(topologyDrains, drain)
		}
		// Forward events from this watcher and trigger cross-repo resolution.
		mw.startForwarderLocked(res.prefix, res.w)
	}

	return nil
}

func (mw *MultiWatcher) startForwarderLocked(prefix string, w *Watcher) *watcherForwarder {
	if w == nil || mw.stopped {
		return nil
	}
	forwarder := &watcherForwarder{cancel: make(chan struct{}), done: make(chan struct{})}
	if mw.forwarders == nil {
		mw.forwarders = make(map[string]*watcherForwarder)
	}
	mw.forwarders[prefix] = forwarder
	go mw.forwardEvents(prefix, w, forwarder)
	return forwarder
}

func (mw *MultiWatcher) cancelForwarderLocked(prefix string) *watcherForwarder {
	forwarder := mw.forwarders[prefix]
	if forwarder == nil {
		return nil
	}
	delete(mw.forwarders, prefix)
	close(forwarder.cancel)
	return forwarder
}

func waitWatcherForwarders(forwarders ...*watcherForwarder) {
	for _, forwarder := range forwarders {
		if forwarder != nil {
			<-forwarder.done
		}
	}
}

func (mw *MultiWatcher) finishWatcherRetirement(retirement *watcherRetirement, err error) {
	mw.mu.Lock()
	retirement.err = err
	if current := mw.retiringWatchers[retirement.prefix]; current == retirement {
		delete(mw.retiringWatchers, retirement.prefix)
	}
	close(retirement.done)
	mw.mu.Unlock()
}

func (mw *MultiWatcher) completeWatcherRetirement(retirement *watcherRetirement) error {
	if retirement == nil {
		return nil
	}
	waitTopologyDispatchDrains(retirement.topologyDrain)
	waitWatcherForwarders(retirement.forwarder)

	var firstErr error
	if retirement.started && retirement.watcher != nil {
		if err := retirement.watcher.Stop(); err != nil {
			firstErr = err
			if mw.logger != nil {
				mw.logger.Warn("error stopping retired watcher",
					zap.String("prefix", retirement.prefix),
					zap.Error(err),
				)
			}
		}
	}
	if retirement.gitWatcher != nil {
		if err := retirement.gitWatcher.Stop(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	mw.finishWatcherRetirement(retirement, firstErr)
	return firstErr
}

func waitWatcherRetirements(retirements ...*watcherRetirement) {
	for _, retirement := range retirements {
		if retirement != nil {
			<-retirement.done
		}
	}
}

func (mw *MultiWatcher) queuePendingWatcherAddLocked(repoPrefix string, cfg config.WatchConfig) {
	if pending := mw.pendingWatcherAdds[repoPrefix]; pending != nil {
		pending.cfg = cfg
		return
	}
	pending := &pendingWatcherAdd{cfg: cfg}
	mw.pendingWatcherAdds[repoPrefix] = pending
	go mw.runPendingWatcherAdd(repoPrefix, pending)
}

func (mw *MultiWatcher) runPendingWatcherAdd(repoPrefix string, pending *pendingWatcherAdd) {
	backoff := 25 * time.Millisecond
	for {
		mw.mu.Lock()
		if mw.stopped || mw.pendingWatcherAdds[repoPrefix] != pending {
			mw.mu.Unlock()
			return
		}
		if retirement := mw.retiringWatchers[repoPrefix]; retirement != nil {
			done := retirement.done
			mw.mu.Unlock()
			select {
			case <-done:
				continue
			case <-mw.done:
				return
			}
		}
		if _, exists := mw.watchers[repoPrefix]; exists {
			delete(mw.pendingWatcherAdds, repoPrefix)
			mw.mu.Unlock()
			return
		}
		cfg := pending.cfg
		mw.mu.Unlock()

		if err := mw.AddRepoContext(context.Background(), repoPrefix, cfg); err == nil {
			mw.mu.Lock()
			if mw.pendingWatcherAdds[repoPrefix] == pending {
				delete(mw.pendingWatcherAdds, repoPrefix)
			}
			mw.mu.Unlock()
			return
		}

		timer := time.NewTimer(backoff)
		select {
		case <-timer.C:
			if backoff < time.Second {
				backoff *= 2
				if backoff > time.Second {
					backoff = time.Second
				}
			}
		case <-mw.done:
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			return
		}
	}
}

// forwardEvents reads events from a per-repo watcher and forwards them to the
// merged events channel. Each registration has an explicit cancel/join pair;
// Watcher.Stop does not close its events channel, so relying on that channel
// retained removed watchers and their indexers until global Stop.
func (mw *MultiWatcher) forwardEvents(_ string, w *Watcher, forwarder *watcherForwarder) {
	defer close(forwarder.done)
	for {
		select {
		case <-mw.done:
			return
		case <-forwarder.cancel:
			return
		case <-w.done:
			return
		case ev, ok := <-w.Events():
			if !ok {
				return
			}
			select {
			case mw.events <- ev:
			case <-mw.done:
				return
			case <-forwarder.cancel:
				return
			case <-w.done:
				return
			default:
			}
		}
	}
}

// Stop halts all per-repo watchers and cleans up resources.
func (mw *MultiWatcher) Stop() error {
	return mw.StopContext(context.Background())
}

// StopContext preserves the synchronous Stop boundary for ordinary callers.
// A topology callback carrying this MultiWatcher's exact private dispatch token
// cannot wait on its own lease; it hands completion to one joinable worker and
// returns only for that reentrant path. Every external and concurrent caller
// still waits for callback drain plus all physical watcher stops.
func (mw *MultiWatcher) StopContext(ctx context.Context) error {
	mw.mu.Lock()
	token := mw.activeTopologyDispatchTokenLocked(ctx)
	if mw.stopped {
		completed := mw.stopDone
		if token != nil {
			err := mw.stopErr
			mw.mu.Unlock()
			return err
		}
		mw.mu.Unlock()
		if completed != nil {
			<-completed
		}
		mw.mu.Lock()
		err := mw.stopErr
		mw.mu.Unlock()
		return err
	}
	mw.stopped = true
	mw.stopDone = make(chan struct{})
	close(mw.done)

	for _, family := range mw.topologyFamilies {
		mw.invalidateTopologyDispatchEpochLocked(family)
	}
	plan := multiWatcherStopPlan{
		topologyDrains: mw.topologyDispatchDrainsLocked(),
		watchers:       make([]watcherStopTarget, 0, len(mw.watchers)),
		gitWatchers:    make([]*GitWatcher, 0, len(mw.gitWatchers)),
		forwarders:     make([]*watcherForwarder, 0, len(mw.forwarders)),
		retirements:    make([]*watcherRetirement, 0, len(mw.retiringWatchers)),
	}
	for prefix, w := range mw.watchers {
		if mw.started[prefix] {
			plan.watchers = append(plan.watchers, watcherStopTarget{prefix: prefix, watcher: w})
		}
	}
	for _, gw := range mw.gitWatchers {
		if gw == nil {
			continue
		}
		gw.OnWorktreeChange(nil)
		gw.setTopologyOwner(false)
		plan.gitWatchers = append(plan.gitWatchers, gw)
	}
	for prefix, forwarder := range mw.forwarders {
		delete(mw.forwarders, prefix)
		close(forwarder.cancel)
		plan.forwarders = append(plan.forwarders, forwarder)
	}
	for _, retirement := range mw.retiringWatchers {
		plan.retirements = append(plan.retirements, retirement)
	}
	mw.pendingWatcherAdds = make(map[string]*pendingWatcherAdd)
	mw.topologyFamilies = make(map[string]*topologyWatchFamily)
	mw.topologyFamilyByRepo = make(map[string]string)
	reentrant := token != nil
	mw.mu.Unlock()

	if reentrant {
		go func() {
			if err := mw.completeStop(plan); err != nil {
				mw.logger.Warn("multi-watcher: deferred stop completion failed", zap.Error(err))
			}
		}()
		return nil
	}
	return mw.completeStop(plan)
}

func (mw *MultiWatcher) completeStop(plan multiWatcherStopPlan) error {
	// Callback drain precedes every physical stop so an already-admitted
	// reconciliation cannot touch a stopped watcher or stale controller state.
	waitTopologyDispatchDrains(plan.topologyDrains...)
	waitWatcherForwarders(plan.forwarders...)

	var firstErr error
	for _, entry := range plan.watchers {
		if err := entry.watcher.Stop(); err != nil && firstErr == nil {
			firstErr = err
			if mw.logger != nil {
				mw.logger.Warn("error stopping watcher",
					zap.String("prefix", entry.prefix),
					zap.Error(err),
				)
			}
		}
	}
	for _, gw := range plan.gitWatchers {
		if err := gw.Stop(); err != nil && firstErr == nil {
			firstErr = err
		}
	}

	// Removals that won the mutex before Stop own their physical watchers.
	// Their tombstones remain registered until both Watcher.Stop and
	// GitWatcher.Stop finish, making them part of the global Stop boundary.
	waitWatcherRetirements(plan.retirements...)
	if firstErr == nil {
		for _, retirement := range plan.retirements {
			if retirement != nil && retirement.err != nil {
				firstErr = retirement.err
				break
			}
		}
	}

	mw.mu.Lock()
	mw.stopErr = firstErr
	close(mw.stopDone)
	mw.mu.Unlock()
	return firstErr
}

// Events returns a read-only channel of merged graph change events from all repos.
func (mw *MultiWatcher) Events() <-chan GraphChangeEvent {
	return mw.events
}

// History returns the union of per-repo histories, sorted newest-first.
// Implements the same surface as Watcher.History so the MCP server can
// consume either a single Watcher or a MultiWatcher through the same
// interface and `get_recent_changes` lights up under the daemon.
func (mw *MultiWatcher) History() []GraphChangeEvent {
	mw.mu.Lock()
	watchers := make([]*Watcher, 0, len(mw.watchers))
	for _, w := range mw.watchers {
		watchers = append(watchers, w)
	}
	mw.mu.Unlock()

	var out []GraphChangeEvent
	for _, w := range watchers {
		out = append(out, w.History()...)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Timestamp.After(out[j].Timestamp) })
	return out
}

// HistorySince returns the union of per-repo events strictly after the
// given timestamp, sorted newest-first.
func (mw *MultiWatcher) HistorySince(since time.Time) []GraphChangeEvent {
	mw.mu.Lock()
	watchers := make([]*Watcher, 0, len(mw.watchers))
	for _, w := range mw.watchers {
		watchers = append(watchers, w)
	}
	mw.mu.Unlock()

	var out []GraphChangeEvent
	for _, w := range watchers {
		out = append(out, w.HistorySince(since)...)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Timestamp.After(out[j].Timestamp) })
	return out
}

// OnSymbolChange registers the callback against every current per-repo
// Watcher and stores it so AddRepo applies it to future watchers too.
// Replaces any previously registered callback (matches Watcher.OnSymbolChange
// semantics).
func (mw *MultiWatcher) OnSymbolChange(cb SymbolChangeCallback) {
	mw.callbackMu.Lock()
	mw.symbolChangeCb = cb
	mw.callbackMu.Unlock()

	mw.mu.Lock()
	watchers := make([]*Watcher, 0, len(mw.watchers))
	for _, w := range mw.watchers {
		watchers = append(watchers, w)
	}
	mw.mu.Unlock()

	for _, w := range watchers {
		w.OnSymbolChange(cb)
	}
}

// OnDegraded registers a callback fired (once per repo) when a per-repo watcher
// first degrades — inotify / FD exhaustion. Fanned out to current watchers and
// re-applied at AddRepo, mirroring OnSymbolChange.
func (mw *MultiWatcher) OnDegraded(cb func(reason string)) {
	mw.callbackMu.Lock()
	mw.degradedCb = cb
	mw.callbackMu.Unlock()

	mw.mu.Lock()
	watchers := make([]*Watcher, 0, len(mw.watchers))
	for _, w := range mw.watchers {
		watchers = append(watchers, w)
	}
	mw.mu.Unlock()

	for _, w := range watchers {
		w.OnDegraded(cb)
	}
}

// OnWorktreeChange registers a legacy callback for changes to a Git family's
// checkout topology. Callbacks that can reach teardown must instead use
// OnWorktreeChangeContext and propagate its context.
func (mw *MultiWatcher) OnWorktreeChange(cb func(repoPrefix, rootPath string)) {
	if cb == nil {
		mw.OnWorktreeChangeContext(nil)
		return
	}
	mw.OnWorktreeChangeContext(func(_ context.Context, repoPrefix, rootPath string) {
		cb(repoPrefix, rootPath)
	})
}

// OnWorktreeChangeContext registers a context-aware topology callback. The
// context carries a private exact-dispatch token used only to break teardown
// cycles caused by that callback; ordinary callers retain synchronous drains.
func (mw *MultiWatcher) OnWorktreeChangeContext(cb WorktreeChangeCallback) {
	type registered struct {
		prefix string
		gw     *GitWatcher
		epoch  *topologyDispatchEpoch
	}
	mw.mu.Lock()
	mw.worktreeChangeCb = cb
	owners := make([]registered, 0, len(mw.topologyFamilies))
	for _, family := range mw.topologyFamilies {
		if gw := family.members[family.owner]; gw != nil && family.dispatch != nil {
			owners = append(owners, registered{prefix: family.owner, gw: gw, epoch: family.dispatch})
		}
	}
	mw.mu.Unlock()

	if cb != nil {
		for _, owner := range owners {
			go mw.dispatchWorktreeTopologyChange(owner.prefix, owner.gw, owner.epoch, owner.gw.repoPath)
		}
	}
}

// EnqueueFileMutation routes a committed file mutation to the active watcher
// that owns the path. Routing is path-scoped: an unrelated degraded watcher
// cannot force a synchronous fallback, and a watcher that failed to start is
// never reported as accepting work.
func (mw *MultiWatcher) EnqueueFileMutation(ctx context.Context, filePath string) (*MutationTicket, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	prefix := mw.multi.RepoForFile(filePath)
	mw.mu.Lock()
	w := mw.watchers[prefix]
	started := mw.started[prefix]
	// A single-repo MultiIndexer may intentionally use the empty prefix;
	// when RepoForFile cannot distinguish it from "not covered", let the
	// per-repo watcher perform the authoritative containment check.
	if w == nil && prefix == "" && len(mw.watchers) == 1 {
		for candidatePrefix, candidate := range mw.watchers {
			w = candidate
			started = mw.started[candidatePrefix]
		}
	}
	mw.mu.Unlock()
	if w == nil || !started {
		return nil, nil
	}
	return w.EnqueueFileMutation(ctx, filePath)
}

// DegradedReason returns the first non-empty per-repo degraded reason, prefixed
// with the repo it came from, or "" when every watcher is healthy. Lets the
// daemon-mode freshness rider surface a whole-index "frozen" banner the same
// way the single-repo embedded watcher does.
func (mw *MultiWatcher) DegradedReason() string {
	mw.mu.Lock()
	defer mw.mu.Unlock()
	prefixes := make([]string, 0, len(mw.watchers))
	for prefix := range mw.watchers {
		prefixes = append(prefixes, prefix)
	}
	sort.Strings(prefixes)
	// A watcher that never started outranks a degraded one: it is not
	// watching at all, and it has no DegradedReason of its own to report.
	for _, prefix := range prefixes {
		if failure, dead := mw.startFailures[prefix]; dead {
			return qualifyRepoReason(prefix,
				"the file watcher failed to start — edits are not reaching the graph until the daemon restarts ("+failure+")")
		}
	}
	for _, prefix := range prefixes {
		if r := mw.watchers[prefix].DegradedReason(); r != "" {
			return qualifyRepoReason(prefix, r)
		}
	}
	return ""
}

func qualifyRepoReason(prefix, reason string) string {
	if prefix == "" {
		return reason
	}
	return prefix + ": " + reason
}

// WatchedRepos reports how many per-repo watchers are live out of how many
// were configured. The daemon announces readiness with these counts, and
// reporting the configured total as if it were the live one is what let an
// install where every watcher had failed still look fully watched.
func (mw *MultiWatcher) WatchedRepos() (live, configured int) {
	mw.mu.Lock()
	defer mw.mu.Unlock()
	for _, started := range mw.started {
		if started {
			live++
		}
	}
	return live, len(mw.watchers)
}

// AddRepo creates and starts a watcher for a newly tracked repo.
func (mw *MultiWatcher) AddRepo(repoPrefix string, cfg config.WatchConfig) error {
	return mw.AddRepoContext(context.Background(), repoPrefix, cfg)
}

// AddRepoContext serializes replacement with authoritative removal. Ordinary
// callers join the retiring prefix and retry. An exact topology callback cannot
// join a retirement that is waiting for that callback's lease, so it records a
// coalesced replacement intent that is completed immediately after retirement.
func (mw *MultiWatcher) AddRepoContext(ctx context.Context, repoPrefix string, cfg config.WatchConfig) error {
	waitedForRetirement := false
	for {
		mw.mu.Lock()
		if mw.stopped {
			mw.mu.Unlock()
			return fmt.Errorf("multi-watcher is stopped")
		}
		token := mw.activeTopologyDispatchTokenLocked(ctx)
		if retirement := mw.retiringWatchers[repoPrefix]; retirement != nil {
			if token != nil && retirement.topologyDrain == token.epoch.drained {
				mw.queuePendingWatcherAddLocked(repoPrefix, cfg)
				mw.mu.Unlock()
				return nil
			}
			done := retirement.done
			mw.mu.Unlock()
			// Replacement intent is a state transition, not a best-effort
			// request. Once accepted it survives caller cancellation.
			<-done
			waitedForRetirement = true
			continue
		}
		if _, exists := mw.watchers[repoPrefix]; exists {
			mw.mu.Unlock()
			if waitedForRetirement {
				// A coalesced callback replacement or another waiter won the race.
				// The requested post-retirement state already exists.
				return nil
			}
			return fmt.Errorf("watcher already exists for repo: %s", repoPrefix)
		}

		var topologyDrain <-chan struct{}
		if err := mw.createWatcher(repoPrefix, cfg); err != nil {
			if mw.logger != nil {
				mw.logger.Warn("failed to add watcher for repo",
					zap.String("prefix", repoPrefix),
					zap.Error(err),
				)
			}
			mw.mu.Unlock()
			return err
		}

		w := mw.watchers[repoPrefix]
		meta := mw.multi.GetMetadata(repoPrefix)
		if meta == nil {
			delete(mw.watchers, repoPrefix)
			mw.mu.Unlock()
			return fmt.Errorf("repository metadata not found: %s", repoPrefix)
		}

		if err := w.Start([]string{meta.RootPath}); err != nil {
			delete(mw.watchers, repoPrefix)
			mw.mu.Unlock()
			return fmt.Errorf("starting watcher for %s: %w", repoPrefix, err)
		}

		mw.started[repoPrefix] = true
		delete(mw.startFailures, repoPrefix)
		if idx := mw.multi.GetIndexer(repoPrefix); idx != nil {
			logger := mw.logger
			if logger == nil {
				logger = zap.NewNop()
			}
			if gw, err := NewGitWatcher(meta.RootPath, idx, logger.With(zap.String("repo", repoPrefix))); err == nil {
				gw.setTopologyOwner(false)
				mw.configureGitWatcher(repoPrefix, gw)
				if err := gw.Start(); err == nil {
					topologyDrain = mw.installStartedGitWatcherLocked(repoPrefix, gw)
				} else {
					_ = gw.Stop()
				}
			}
		}

		// Apply any previously-registered callbacks so a runtime replacement
		// contributes exactly like a watcher created at construction time.
		mw.callbackMu.Lock()
		cb := mw.symbolChangeCb
		degradedCb := mw.degradedCb
		mw.callbackMu.Unlock()
		if cb != nil {
			w.OnSymbolChange(cb)
		}
		if degradedCb != nil {
			w.OnDegraded(degradedCb)
		}
		mw.startForwarderLocked(repoPrefix, w)
		mw.mu.Unlock()
		waitTopologyDispatchDrains(topologyDrain)
		return nil
	}
}

// RemoveRepo stops and removes the watcher for a repo.
func (mw *MultiWatcher) RemoveRepo(repoPrefix string) error {
	return mw.RemoveRepoContext(context.Background(), repoPrefix)
}

// RemoveRepoContext publishes a per-prefix tombstone before removing active
// map entries. That tombstone spans callback drain, forwarder join, and both
// physical watcher stops, preventing a replacement from overlapping teardown.
func (mw *MultiWatcher) RemoveRepoContext(ctx context.Context, repoPrefix string) error {
	mw.mu.Lock()
	token := mw.activeTopologyDispatchTokenLocked(ctx)
	_, canceledPendingAdd := mw.pendingWatcherAdds[repoPrefix]
	if canceledPendingAdd {
		// A later remove supersedes an accepted but not-yet-installed
		// replacement. The worker checks pointer identity under this mutex, so
		// deleting the intent prevents resurrection after this call returns.
		delete(mw.pendingWatcherAdds, repoPrefix)
	}
	w, exists := mw.watchers[repoPrefix]
	if !exists {
		retirement := mw.retiringWatchers[repoPrefix]
		stopping := mw.stopped
		stopDone := mw.stopDone
		if retirement != nil {
			if token != nil && retirement.topologyDrain == token.epoch.drained {
				mw.mu.Unlock()
				return nil
			}
			done := retirement.done
			mw.mu.Unlock()
			<-done
			return retirement.err
		}
		if stopping {
			if token != nil {
				mw.mu.Unlock()
				return nil
			}
			mw.mu.Unlock()
			if stopDone != nil {
				<-stopDone
			}
			return nil
		}
		mw.mu.Unlock()
		if canceledPendingAdd {
			return nil
		}
		return fmt.Errorf("no watcher for repo: %s", repoPrefix)
	}

	if mw.stopped {
		stopDone := mw.stopDone
		delete(mw.gitWatchers, repoPrefix)
		delete(mw.watchers, repoPrefix)
		delete(mw.started, repoPrefix)
		delete(mw.startFailures, repoPrefix)
		if token != nil {
			mw.mu.Unlock()
			return nil
		}
		mw.mu.Unlock()
		if stopDone != nil {
			<-stopDone
		}
		return nil
	}

	started := mw.started[repoPrefix]
	gw := mw.gitWatchers[repoPrefix]
	var drain <-chan struct{}
	if gw != nil {
		drain = mw.unregisterTopologyWatcherLocked(repoPrefix)
	}
	forwarder := mw.cancelForwarderLocked(repoPrefix)
	retirement := &watcherRetirement{
		prefix:        repoPrefix,
		watcher:       w,
		gitWatcher:    gw,
		forwarder:     forwarder,
		started:       started,
		topologyDrain: drain,
		done:          make(chan struct{}),
	}
	if mw.retiringWatchers == nil {
		mw.retiringWatchers = make(map[string]*watcherRetirement)
	}
	mw.retiringWatchers[repoPrefix] = retirement
	delete(mw.gitWatchers, repoPrefix)
	delete(mw.watchers, repoPrefix)
	delete(mw.started, repoPrefix)
	delete(mw.startFailures, repoPrefix)
	reentrant := token != nil && drain == token.epoch.drained
	mw.mu.Unlock()

	if reentrant {
		go func() {
			if err := mw.completeWatcherRetirement(retirement); err != nil {
				mw.logger.Warn("multi-watcher: deferred watcher retirement failed", zap.Error(err))
			}
		}()
		return nil
	}
	return mw.completeWatcherRetirement(retirement)
}
