package indexer

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/zzet/gortex/internal/gitcmd"
	"github.com/zzet/gortex/internal/graph"
	"go.uber.org/zap"
)

// Poller is a timer-driven fallback that complements the fsnotify
// per-file Watcher and the GitWatcher. fsnotify is not reliable
// everywhere: the Linux inotify backend silently stops delivering
// events once `max_user_watches` is exhausted on a huge tree, network
// filesystems (NFS, SMB, virtiofs) deliver events late or not at all,
// and any backend can drop events under load. The poller closes that
// gap by periodically re-checking two cheap signals — git HEAD
// movement and tracked-file mtimes — and dispatching the work the
// missed event would have triggered.
//
// The poll interval is adaptive: it scales with the size of the
// repository so a small project is checked often (changes land fast)
// while a large project is checked rarely (a sweep over thousands of
// tracked files is not free, and large repos should not be hammered).
// Project size is read from a real signal — the indexed node count of
// the repo's graph — and the resulting interval is clamped to a
// sensible [min, max] band.
type Poller struct {
	watcher  *Watcher
	indexer  *Indexer
	rootPath string
	logger   *zap.Logger

	interval time.Duration
	done     chan struct{}
	wg       sync.WaitGroup

	// notifyPath is the sentinel file an external trigger (a post-checkout
	// git hook, or an agent that just wrote files) touches to force an
	// immediate reconcile; notifyMtime is the last mtime the fast notify
	// loop observed. Empty notifyPath disables the fast loop.
	notifyPath  string
	notifyMtime int64

	mu          sync.Mutex
	lastSHA     string
	loopStarted bool
	stopCalled  bool

	// pollMu guards dirty-generation singleflight admission. Timer and notify
	// triggers never queue behind a running filesystem scan: they advance
	// pollRequested and return. The active owner observes that generation after
	// its sweep and performs one coalesced follow-up when necessary.
	pollMu        sync.Mutex
	pollRequested uint64
	pollRunning   bool

	// SQLite-backed pollers advance two independent bounded receipt rotations.
	// The mtime cursor may move through thousands of cheap stat rows while the
	// content cursor stops at its own byte budget; coupling them would let one
	// large file delay ordinary drift/deletion detection.
	receiptIndexer  *Indexer
	mtimeRotation   fileReceiptRotation
	contentRotation fileReceiptRotation
	// A failed filesystem batch is retried before any new receipt page is
	// consumed. One discovery page contributes at most the bounded mtime page
	// plus the 128-row content page, so repeated failures cannot grow this set.
	pendingFilesystemPaths map[string]struct{}

	// swept is a test hook fired after every poll cycle with the
	// number of files the cycle re-dispatched. nil in production.
	swept           func(int)
	readReceiptFile func(string) ([]byte, error)
	statReceiptFile func(string) (os.FileInfo, error)
}

// Poll-interval bounds. A small repo polls near the floor; a large
// repo polls near the ceiling. The band is wide enough that the
// fallback never becomes a hot loop on a tiny repo, nor a no-op on a
// huge one.
const (
	pollIntervalMin = 15 * time.Second
	pollIntervalMax = 10 * time.Minute

	// pollNodesPerStep is the indexed-node count that maps to one
	// pollIntervalMin step of additional interval. A repo of ~2k
	// nodes adds one floor-width step, ~4k adds two, and so on,
	// until the interval saturates at pollIntervalMax. Chosen so a
	// typical small service (a few hundred nodes) sits at the floor
	// and a large monorepo (tens of thousands of nodes) sits at the
	// ceiling.
	pollNodesPerStep = 2000

	pollMtimeRotationTarget = time.Hour
	pollMtimePageMinRows    = 128
	pollMtimePageMaxRows    = 4096
	pollContentPageRows     = 128
	pollContentPageBytes    = int64(4 << 20)
)

type fileReceiptRotation struct {
	afterPath     string
	highWaterPath string
}

func (r *fileReceiptRotation) reset() {
	r.afterPath = ""
	r.highWaterPath = ""
}

// pollInterval derives an adaptive poll interval from a project-size
// signal — the indexed node count. The mapping is linear in the node
// count and clamped to [pollIntervalMin, pollIntervalMax]: an empty
// or tiny repo polls at the floor, and the interval grows by one
// floor-width step per pollNodesPerStep nodes until it saturates at
// the ceiling. A non-positive node count (a repo indexed to nothing,
// or a missing graph) falls back to the floor rather than to zero.
func pollInterval(nodeCount int) time.Duration {
	if nodeCount <= 0 {
		return pollIntervalMin
	}
	steps := nodeCount / pollNodesPerStep
	d := pollIntervalMin + time.Duration(steps)*pollIntervalMin
	if d < pollIntervalMin {
		return pollIntervalMin
	}
	if d > pollIntervalMax {
		return pollIntervalMax
	}
	return d
}

// pollMtimePageRows sizes one cheap-stat page so an uncapped rotation finishes
// in roughly one janitor interval. The hard row cap bounds receipt heap for a
// very large workspace; the hourly full-tree janitor remains the outer latency
// bound for ordinary mtime drift when that cap is reached.
func pollMtimePageRows(tracked int, interval time.Duration) int {
	if tracked <= 0 {
		return 0
	}
	if interval <= 0 {
		interval = pollIntervalMin
	}
	ticks := max(int(pollMtimeRotationTarget/interval), 1)
	rows := (tracked + ticks - 1) / ticks
	rows = max(rows, pollMtimePageMinRows)
	rows = min(rows, pollMtimePageMaxRows)
	return min(rows, tracked)
}

// newPoller builds an adaptive-interval poller for a Watcher. The
// interval is computed once at construction from the indexer's
// current graph size; it is stable for the poller's lifetime, which
// matches how the rest of the watcher subsystem treats per-repo
// tuning (set at start, not re-derived per tick).
func newPoller(w *Watcher, idx *Indexer, logger *zap.Logger) *Poller {
	root := ""
	if idx != nil {
		root = idx.rootPath
	}
	nodeCount := 0
	if idx != nil && idx.graph != nil {
		nodeCount = idx.graph.NodeCount()
	}
	// Capture the baseline HEAD SHA at construction so the very first
	// poll cycle can already detect a branch switch that happened
	// between newPoller and the first tick. A repo with no .git
	// directory yields an empty SHA and the HEAD check stays a no-op.
	lastSHA := ""
	if root != "" {
		lastSHA, _ = pollerHeadSHA(root)
	}
	return &Poller{
		watcher:    w,
		indexer:    idx,
		rootPath:   root,
		logger:     logger,
		interval:   pollInterval(nodeCount),
		lastSHA:    lastSHA,
		done:       make(chan struct{}),
		notifyPath: notifyFilePath(root),
	}
}

// Start launches the polling goroutine. Safe to call once per Poller
// instance. A poller with no usable indexer or root path is inert —
// Start records the no-op and returns without launching the loop, and
// Stop stays safe to call.
func (p *Poller) Start() {
	if p.indexer == nil || p.rootPath == "" {
		return
	}
	p.mu.Lock()
	p.loopStarted = true
	p.mu.Unlock()
	p.wg.Add(1)
	go p.loop()
	if p.notifyPath != "" {
		// Seed the baseline mtime so the first observed change is a real
		// touch, not the file's pre-existing state at startup.
		if info, err := os.Stat(p.notifyPath); err == nil {
			p.mu.Lock()
			p.notifyMtime = info.ModTime().UnixNano()
			p.mu.Unlock()
		}
		p.wg.Add(1)
		go p.notifyLoop()
	}
}

// Stop halts the poller. Idempotent — safe whether Start launched the
// loop or was a no-op. The wait on `stopped` only happens when the
// loop goroutine is actually running, so Stop never deadlocks on a
// channel nobody will close.
func (p *Poller) Stop() {
	p.mu.Lock()
	started := p.loopStarted
	already := p.stopCalled
	p.stopCalled = true
	p.mu.Unlock()
	if already {
		return
	}
	close(p.done)
	if started {
		p.wg.Wait()
	}
}

func (p *Poller) loop() {
	defer p.wg.Done()
	t := time.NewTicker(p.interval)
	defer t.Stop()
	if p.logger != nil {
		p.logger.Debug("watcher: adaptive poller running",
			zap.String("root", p.rootPath),
			zap.Duration("interval", p.interval))
	}
	for {
		select {
		case <-p.done:
			return
		case <-t.C:
			p.poll()
		}
	}
}

// poll admits one fallback request. Timer and notify callers never wait behind
// an active scan: they only dirty the generation and return. The active owner
// folds every overlapping request into the minimum necessary follow-up sweep.
func (p *Poller) poll() {
	p.pollMu.Lock()
	p.pollRequested++
	generation := p.pollRequested
	if p.pollRunning {
		p.pollMu.Unlock()
		return
	}
	p.pollRunning = true
	p.pollMu.Unlock()

	for {
		p.pollOnce()

		p.pollMu.Lock()
		if p.pollRequested == generation {
			p.pollRunning = false
			p.pollMu.Unlock()
			return
		}
		generation = p.pollRequested
		p.pollMu.Unlock()
	}
}

// pollOnce discovers Git and filesystem changes independently, unions their
// exact paths, and submits one bounded watcher batch. Discovery never mutates
// freshness bookkeeping; the indexing batch owns those stamps under the
// repository mutation lane.
func (p *Poller) pollOnce() {
	git := p.observeGitHead()
	filesystemPaths := p.pendingFilesystemRetryPaths()
	if len(filesystemPaths) == 0 {
		filesystemPaths = p.pollFilesystem()
	}
	pathSet := make(map[string]struct{}, len(git.paths))
	for _, path := range git.paths {
		if path != "" {
			pathSet[filepath.Clean(path)] = struct{}{}
		}
	}
	for _, path := range filesystemPaths {
		if path != "" {
			pathSet[filepath.Clean(path)] = struct{}{}
		}
	}
	paths := make([]string, 0, len(pathSet))
	for path := range pathSet {
		paths = append(paths, path)
	}
	sort.Strings(paths)

	reconciled := len(paths) == 0
	var result *IndexResult
	var reconcileErr error
	if len(paths) > 0 {
		if p.watcher == nil {
			reconcileErr = fmt.Errorf("watcher: poller has no watcher for %d changed paths", len(paths))
		} else {
			result, reconcileErr = p.watcher.reindexStormPaths(paths)
			if reconcileErr == nil && result != nil && len(result.FailedFiles) > 0 {
				reconcileErr = fmt.Errorf("watcher: poller batch left %d failed files", len(result.FailedFiles))
			}
		}
		reconciled = reconcileErr == nil
		if reconcileErr != nil && p.logger != nil {
			p.logger.Warn("watcher: poller batch reconcile failed",
				zap.Int("paths", len(paths)), zap.Error(reconcileErr))
		} else if p.logger != nil {
			p.logger.Info("watcher: poller reconciled changes missed by live watchers",
				zap.Int("paths", len(paths)))
		}
	}
	if len(filesystemPaths) > 0 {
		if reconciled {
			p.pendingFilesystemPaths = nil
		} else {
			p.retainFilesystemRetryPaths(filesystemPaths)
		}
	}

	// A changed Git range is committed only after all of its paths shared the
	// successful batch above. An empty successful diff has no batch work and can
	// enter the same lane-owned freshness finalization directly. Diff failures
	// never reach this point, so the old range remains retryable.
	if git.diffSucceeded && (len(git.paths) == 0 || reconciled) {
		if err := p.finalizeGitHead(git); err != nil {
			if p.logger != nil {
				p.logger.Warn("watcher: poller Git freshness finalization failed",
					zap.String("from", git.oldSHA), zap.String("to", git.newSHA),
					zap.Error(err))
			}
		} else if git.oldSHA != "" && git.newSHA != "" && git.oldSHA != git.newSHA && p.logger != nil {
			p.logger.Info("watcher: poller reconciled missed ref change",
				zap.String("from", git.oldSHA[:min(len(git.oldSHA), 12)]),
				zap.String("to", git.newSHA[:min(len(git.newSHA), 12)]),
				zap.Int("paths", len(git.paths)))
		}
	}

	if p.swept != nil {
		p.swept(len(paths))
	}
}

func (p *Poller) pendingFilesystemRetryPaths() []string {
	paths := make([]string, 0, len(p.pendingFilesystemPaths))
	for path := range p.pendingFilesystemPaths {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	return paths
}

func (p *Poller) retainFilesystemRetryPaths(paths []string) {
	if p.pendingFilesystemPaths == nil {
		p.pendingFilesystemPaths = make(map[string]struct{}, len(paths))
	}
	for _, path := range paths {
		if path != "" {
			p.pendingFilesystemPaths[filepath.Clean(path)] = struct{}{}
		}
	}
}

// pollGitObservation separates range discovery from freshness publication.
// pollOnce can therefore union the range with filesystem evidence before one
// repository batch, then advance lastSHA only when that batch succeeds.
type pollGitObservation struct {
	oldSHA        string
	newSHA        string
	paths         []string
	diffSucceeded bool
}

func (p *Poller) observeGitHead() pollGitObservation {
	newSHA, err := pollerHeadSHA(p.rootPath)
	if err != nil || newSHA == "" {
		return pollGitObservation{}
	}
	p.mu.Lock()
	oldSHA := p.lastSHA
	p.mu.Unlock()
	if oldSHA == newSHA {
		return pollGitObservation{}
	}
	if oldSHA == "" {
		// First observation has no range to reconcile. Return a successful
		// zero-path observation so pollOnce commits the baseline uniformly.
		return pollGitObservation{newSHA: newSHA, diffSucceeded: true}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	changes, err := pollerDiffNameStatus(ctx, p.rootPath, oldSHA, newSHA)
	if err != nil {
		// Leave lastSHA at oldSHA so the next cycle retries this exact range.
		if p.logger != nil {
			p.logger.Debug("watcher: poller git diff failed",
				zap.String("from", oldSHA), zap.String("to", newSHA),
				zap.Error(err))
		}
		return pollGitObservation{}
	}

	pathSet := make(map[string]struct{}, len(changes)+1)
	add := func(relPath string) {
		if relPath == "" {
			return
		}
		pathSet[filepath.Clean(filepath.Join(p.rootPath, filepath.FromSlash(relPath)))] = struct{}{}
	}
	for _, change := range changes {
		switch change.Status {
		case 'A', 'M', 'T', 'C', 'D':
			add(change.Path)
		case 'R':
			// Both sides matter: the new path is parsed and the old path is
			// deletion-detected by the same bounded batch.
			add(change.OldPath)
			add(change.Path)
		}
	}
	paths := make([]string, 0, len(pathSet))
	for path := range pathSet {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	return pollGitObservation{
		oldSHA: oldSHA, newSHA: newSHA, paths: paths, diffSucceeded: true,
	}
}

// pollGitHead is retained for focused in-package callers. Production sweeps use
// observeGitHead so Git and filesystem paths can share one batch; this helper
// still reconciles its Git-only observation as one bounded batch.
func (p *Poller) pollGitHead() bool {
	observation := p.observeGitHead()
	if !observation.diffSucceeded {
		return false
	}
	if len(observation.paths) == 0 {
		if err := p.finalizeGitHead(observation); err != nil && p.logger != nil {
			p.logger.Warn("watcher: poller Git freshness finalization failed",
				zap.String("from", observation.oldSHA), zap.String("to", observation.newSHA),
				zap.Error(err))
		}
		return false
	}
	if p.watcher == nil {
		return false
	}
	result, err := p.watcher.reindexStormPaths(observation.paths)
	if err == nil && result != nil && len(result.FailedFiles) > 0 {
		err = fmt.Errorf("watcher: poller batch left %d failed files", len(result.FailedFiles))
	}
	if err != nil {
		if p.logger != nil {
			p.logger.Warn("watcher: poller git reconcile failed",
				zap.Int("paths", len(observation.paths)), zap.Error(err))
		}
		return false
	}
	if err := p.finalizeGitHead(observation); err != nil {
		if p.logger != nil {
			p.logger.Warn("watcher: poller Git freshness finalization failed",
				zap.String("from", observation.oldSHA), zap.String("to", observation.newSHA),
				zap.Error(err))
		}
		return false
	}
	return true
}

func (p *Poller) registeredIndexer() *Indexer {
	if p.watcher != nil {
		return p.watcher.currentMutationIndexer()
	}
	return p.indexer
}

// finalizeGitHead publishes Git and repository freshness together while
// holding the construction-time Indexer's stable mutation lane. The current
// registry entry is resolved only after admission, so IndexRepo replacement
// cannot redirect the restamp to a retired Indexer. A fresh background context
// keeps lane admission independent of the short-lived Git diff timeout.
func (p *Poller) finalizeGitHead(observation pollGitObservation) error {
	if observation.newSHA == "" {
		return nil
	}
	if p.indexer == nil {
		return fmt.Errorf("watcher: poller has no stable repository lane")
	}
	return p.indexer.coordinateRepositoryMutation(context.Background(), OutputEntryPollerFinalizeGitHead, func() error {
		idx := p.registeredIndexer()
		if idx == nil {
			return fmt.Errorf("watcher: poller repository indexer is no longer registered")
		}

		p.mu.Lock()
		stillCurrent := p.lastSHA == observation.oldSHA
		p.mu.Unlock()
		if !stillCurrent {
			return nil
		}

		idx.reconcileRepoIndexState(context.Background(), p.rootPath)
		p.mu.Lock()
		// Singleflight owns Git observations, but keep this conditional so a test
		// or future explicit reset cannot be overwritten by an older completion.
		if p.lastSHA == observation.oldSHA {
			p.lastSHA = observation.newSHA
		}
		p.mu.Unlock()
		return nil
	})
}

// pollFilesystem returns tracked paths whose durable receipt no longer matches
// disk. SQLite stores use two independent, high-water-frozen keyset rotations:
// a dynamically-sized mtime page and a smaller byte-bounded content page that
// catches edits whose timestamp was restored. Other stores retain the original
// immutable-map scan.
func (p *Poller) pollFilesystem() []string {
	idx := p.indexer
	if p.watcher != nil {
		// MultiWatcher instances outlive IndexRepo replacement. Resolve the
		// current registry entry for discovery too; the construction-time
		// Indexer is retained only as the stable lane carrier.
		idx = p.watcher.currentMutationIndexer()
	}
	if idx == nil {
		return nil
	}
	snapshot := idx.publishFileMtimes()
	if len(snapshot) == 0 {
		return nil
	}
	if !idx.fileReceiptPagingReliable() {
		// A later authoritative repair must start both rotations from its fresh
		// durable keyset, not resume a high-water frozen before the failed write.
		p.mtimeRotation.reset()
		p.contentRotation.reset()
		return p.pollFilesystemSnapshot(snapshot)
	}
	pager, ok := idx.graph.(graph.FileReceiptPager)
	if !ok {
		return p.pollFilesystemSnapshot(snapshot)
	}
	if p.receiptIndexer != idx {
		p.receiptIndexer = idx
		p.mtimeRotation.reset()
		p.contentRotation.reset()
	}

	mtimeRows, mtimeExhausted, mtimeScan, err := p.readReceiptPage(
		pager, idx.repoPrefix, &p.mtimeRotation,
		pollMtimePageRows(len(snapshot), p.interval), false,
	)
	if err != nil || !mtimeScan {
		if err != nil && p.logger != nil {
			p.logger.Debug("watcher: poller receipt page failed; using mtime snapshot",
				zap.String("root", p.rootPath), zap.Error(err))
		}
		// A non-empty in-memory snapshot with no durable receipt page can occur
		// after a persistence failure. Preserve the prior correctness path.
		p.mtimeRotation.reset()
		return p.pollFilesystemSnapshot(snapshot)
	}
	paths := p.changedMtimeReceiptPaths(snapshot, mtimeRows)
	p.advanceReceiptRotation(&p.mtimeRotation, mtimeRows, len(mtimeRows), mtimeExhausted)

	contentRows, contentExhausted, _, contentErr := p.readReceiptPage(
		pager, idx.repoPrefix, &p.contentRotation,
		pollContentPageRows, true,
	)
	if contentErr != nil {
		if p.logger != nil {
			p.logger.Debug("watcher: poller content receipt page failed",
				zap.String("root", p.rootPath), zap.Error(contentErr))
		}
		return appendUniqueSorted(nil, paths...)
	}
	contentPaths, processed := p.changedContentReceiptPaths(idx, snapshot, contentRows)
	paths = append(paths, contentPaths...)
	p.advanceReceiptRotation(&p.contentRotation, contentRows, processed, contentExhausted)
	return appendUniqueSorted(nil, paths...)
}

func (p *Poller) pollFilesystemSnapshot(snapshot map[string]int64) []string {
	paths := make([]string, 0)
	for relPath, recordedMtime := range snapshot {
		abs := filepath.Clean(filepath.Join(p.rootPath, filepath.FromSlash(relPath)))
		info, err := os.Stat(abs)
		if err != nil {
			if os.IsNotExist(err) {
				paths = append(paths, abs)
			} else if p.logger != nil {
				// A transient permission/I/O error must not abort the shared batch
				// and prevent unrelated valid paths from converging.
				p.logger.Debug("watcher: poller stat failed; preserving tracked path",
					zap.String("path", abs), zap.Error(err))
			}
			continue
		}
		if info.IsDir() {
			continue
		}
		if info.ModTime().UnixNano() != recordedMtime {
			paths = append(paths, abs)
		}
	}
	sort.Strings(paths)
	return paths
}

func (p *Poller) readReceiptPage(
	pager graph.FileReceiptPager,
	repoPrefix string,
	rotation *fileReceiptRotation,
	limit int,
	includeContent bool,
) ([]graph.FileReceipt, bool, bool, error) {
	if limit <= 0 {
		return nil, true, false, nil
	}
	if rotation.highWaterPath == "" {
		highWater, err := pager.FileReceiptHighWater(repoPrefix)
		if err != nil {
			return nil, false, false, err
		}
		if highWater == "" {
			return nil, true, false, nil
		}
		rotation.afterPath = ""
		rotation.highWaterPath = highWater
	}
	rows, err := pager.FileReceiptPage(
		repoPrefix, rotation.afterPath, rotation.highWaterPath, limit, includeContent,
	)
	if err != nil {
		return nil, false, true, err
	}
	exhausted := len(rows) < limit
	if len(rows) == 0 || rows[len(rows)-1].FilePath >= rotation.highWaterPath {
		exhausted = true
	}
	return rows, exhausted, true, nil
}

func (p *Poller) advanceReceiptRotation(
	rotation *fileReceiptRotation,
	rows []graph.FileReceipt,
	processed int,
	pageExhausted bool,
) {
	processed = min(max(processed, 0), len(rows))
	if processed > 0 {
		rotation.afterPath = rows[processed-1].FilePath
	}
	if processed == len(rows) && pageExhausted {
		rotation.reset()
	}
}

func (p *Poller) changedMtimeReceiptPaths(
	snapshot map[string]int64,
	receipts []graph.FileReceipt,
) []string {
	paths := make([]string, 0)
	for _, receipt := range receipts {
		recordedMtime, tracked := snapshot[receipt.FilePath]
		if !tracked {
			continue
		}
		abs := filepath.Clean(filepath.Join(p.rootPath, filepath.FromSlash(receipt.FilePath)))
		info, err := os.Stat(abs)
		if err != nil {
			if os.IsNotExist(err) {
				paths = append(paths, abs)
			} else if p.logger != nil {
				p.logger.Debug("watcher: poller stat failed; preserving tracked path",
					zap.String("path", abs), zap.Error(err))
			}
			continue
		}
		if !info.IsDir() && info.ModTime().UnixNano() != recordedMtime {
			paths = append(paths, abs)
		}
	}
	return paths
}

func (p *Poller) changedContentReceiptPaths(
	idx *Indexer,
	snapshot map[string]int64,
	receipts []graph.FileReceipt,
) ([]string, int) {
	paths := make([]string, 0)
	processed := 0
	var bytesRead int64
	for _, receipt := range receipts {
		recordedMtime, tracked := snapshot[receipt.FilePath]
		if !tracked {
			processed++
			continue
		}
		abs := filepath.Clean(filepath.Join(p.rootPath, filepath.FromSlash(receipt.FilePath)))
		before, err := p.receiptStat(abs)
		if err != nil {
			if os.IsNotExist(err) {
				paths = append(paths, abs)
			} else if p.logger != nil {
				p.logger.Debug("watcher: poller content stat failed; preserving tracked path",
					zap.String("path", abs), zap.Error(err))
			}
			if os.IsNotExist(err) {
				processed++
				continue
			}
			// A transient stat failure is uncomparable. Keep this row at the
			// cursor so the next poll retries it instead of waiting a rotation.
			break
		}
		if before.IsDir() {
			processed++
			continue
		}
		if before.ModTime().UnixNano() != recordedMtime {
			paths = append(paths, abs)
			processed++
			continue
		}
		if receipt.ContentHash == "" {
			processed++
			continue
		}

		size := max(before.Size(), int64(0))
		if bytesRead > 0 && size > pollContentPageBytes-bytesRead {
			break
		}
		matches, comparable, read := p.contentReceiptMatches(idx, abs, before, receipt)
		bytesRead += read
		if !comparable {
			// A transient read/stat race is not evidence that the indexed bytes
			// changed, but advancing would postpone its next verification for a
			// full rotation. Retry this exact row on the next poll.
			break
		}
		if !matches {
			// Do not advance past a proven mismatch. A failed reconcile retries
			// it next poll; after success the refreshed receipt matches and the
			// independent content cursor can continue.
			paths = append(paths, abs)
			break
		}
		processed++
	}
	return paths, processed
}

func (p *Poller) contentReceiptMatches(
	idx *Indexer,
	absPath string,
	before os.FileInfo,
	receipt graph.FileReceipt,
) (matches bool, comparable bool, bytesRead int64) {
	readFile := os.ReadFile
	if p.readReceiptFile != nil {
		readFile = p.readReceiptFile
	}
	src, err := readFile(absPath)
	if err != nil {
		return false, false, 0
	}
	bytesRead = int64(len(src))
	after, err := p.receiptStat(absPath)
	if err != nil || !sameFileVersion(before, after) {
		return false, false, bytesRead
	}
	relPath := idx.relKey(absPath)
	src = idx.transforms.run(relPath, src)
	return int64(len(src)) == receipt.Size && contentHashForSource(src) == receipt.ContentHash,
		true, bytesRead
}

func (p *Poller) receiptStat(path string) (os.FileInfo, error) {
	if p.statReceiptFile != nil {
		return p.statReceiptFile(path)
	}
	return os.Stat(path)
}

// pollerHeadSHA resolves the current HEAD commit SHA of a worktree.
// Shells out to git so symbolic refs, packed-refs, and worktree
// indirection resolve without re-implementing git ref logic. A repo
// with no .git directory yields an empty string and no error from the
// caller's perspective — the poller simply skips the HEAD check.
func pollerHeadSHA(repoPath string) (string, error) {
	return gitcmd.Output(context.Background(), repoPath, "rev-parse", "HEAD")
}

// pollerDiffNameStatus runs `git diff --name-status -M -C -z` between
// two commits and decodes the result, reusing the GitWatcher's
// NUL-delimited parser so rename / copy detection behaves identically
// on the fallback path.
func pollerDiffNameStatus(ctx context.Context, repoPath, oldSHA, newSHA string) ([]gitChange, error) {
	out, err := gitcmd.Run(ctx, repoPath,
		"diff", "--name-status", "-M", "-C", "-z", oldSHA, newSHA)
	if err != nil {
		return nil, err
	}
	return parseDiffNameStatus(out), nil
}

// notifyFileInterval is the fast poll cadence for the agent-triggered
// notify file — tight enough for sub-second re-index latency, cheap enough
// (one stat per tick) to run alongside the adaptive poller.
const notifyFileInterval = 250 * time.Millisecond

// notifyFilePath resolves the sentinel file an external trigger (a
// post-checkout hook, or an agent that just wrote files) touches to force
// an immediate reconcile. GORTEX_NOTIFY_FILE overrides the per-repo default
// of <root>/.gortex/reindex.notify.
func notifyFilePath(root string) string {
	if env := strings.TrimSpace(os.Getenv("GORTEX_NOTIFY_FILE")); env != "" {
		return env
	}
	if root == "" {
		return ""
	}
	return filepath.Join(root, ".gortex", "reindex.notify")
}

// notifyLoop watches the notify file's mtime on a tight cadence and runs a
// full poll cycle (HEAD + filesystem sweep) the instant it is touched, so an
// agent or git hook can force a sub-second reconcile instead of waiting out
// the adaptive interval. A missing file is a cheap no-op until it appears;
// the file's first sighting only seeds the baseline (it is not itself a
// trigger).
func (p *Poller) notifyLoop() {
	defer p.wg.Done()
	t := time.NewTicker(notifyFileInterval)
	defer t.Stop()
	for {
		select {
		case <-p.done:
			return
		case <-t.C:
			info, err := os.Stat(p.notifyPath)
			if err != nil {
				continue
			}
			mt := info.ModTime().UnixNano()
			p.mu.Lock()
			prev := p.notifyMtime
			p.notifyMtime = mt
			p.mu.Unlock()
			if prev == 0 || mt == prev {
				continue
			}
			if p.logger != nil {
				p.logger.Debug("watcher: notify file touched; forcing reconcile",
					zap.String("notify", p.notifyPath))
			}
			p.poll()
		}
	}
}
