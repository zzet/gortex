package indexer

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/viewmetrics"
	"go.uber.org/zap"
)

// The stuck-input repair. A primary core mutation opens a file's core-write
// window when it stages the file's receipt and closes it when its body ends,
// its accept commits or it settles. A mutation that stops in between leaves
// the window open, and a contract job that read the file waits on it for as
// long as nothing else writes the file:
//
//   - a body that failed after it staged (the journal marks it orphaned);
//   - a staging transaction that failed after opening the window;
//   - a write kept over a window still open, which a later settle of the
//     older seq cannot close;
//   - a cold door that flushed receipts and then failed;
//   - a baseline whose receipts were set and whose accept failed;
//   - a process that stopped mid-body or mid-accept: the next one sees
//     unaccepted rows no mutation of its own wrote (seq 0);
//   - an accept refused after the windows closed: the rows are complete
//     but the namespace's input-state header stays pending.
//
// One repair goroutine per repository finds these and heals them under the
// repository's own mutation lane, independent of the scheduler's dispatchers
// and job slot, so a job stuck on a window cannot hold up the repair of that
// window. It never waits for a lane: it takes one only while it is free.

// ContractRepairPeriod is the repair's period: how long a window may stay
// open, or a namespace header pending, before it is suspect, and how often
// the repair looks.
const ContractRepairPeriod = 5 * time.Second

// contractRepairOrphanGrace is how long after an orphan mark the repair
// waits: a failed mutation's caller may still retry the file itself.
const contractRepairOrphanGrace = time.Second

// contractRepairLanePoll is how often a repair asks again for a busy lane.
const contractRepairLanePoll = 100 * time.Millisecond

// contractRepairEnvelopeAfter is how many forced reindexes a path gets
// before the repair excludes it with a failure envelope.
const contractRepairEnvelopeAfter = 3

// contractBaselineReconcileCause names persisted baseline reconstruction work.
const contractBaselineReconcileCause = "namespace_baseline_reconciliation"

// ContractStuckRepairOptions wires the repair to the runtime it repairs.
type ContractStuckRepairOptions struct {
	Store        *store_sqlite.Store
	MultiIndexer *MultiIndexer
	// Baseline is the capture runtime the namespace header repair rebuilds
	// the baseline with.
	Baseline ContractFollowupCaptureOptions
	// Coordinator, when set, reports actors waiting on pending inputs and
	// defers its unsettled requeues to a due repair.
	Coordinator *ContractAnalysisCoordinator
	Logger      *zap.Logger
	// Period replaces ContractRepairPeriod.
	Period time.Duration
}

// ContractStuckRepair is the per-repository stuck-input repair.
type ContractStuckRepair struct {
	options ContractStuckRepairOptions
	period  time.Duration
	ctx     context.Context
	cancel  context.CancelFunc
	workers sync.WaitGroup
	mu      sync.Mutex
	closed  bool
	repos   map[string]*contractRepoRepair
	// Tests replace these: beforeLane runs as a pass starts; reindex,
	// envelope and evict replace the forced reindex, the envelope mutation
	// and its eviction of one file.
	beforeLane func(repo string)
	reindex    func(ctx context.Context, repo string, paths []string) error
	envelope   func(ctx context.Context, repo string, paths []string) error
	evict      func(repo, rel string) error
}

type contractRepoRepair struct {
	repo string
	wake chan struct{}
	// suspects are paths a job or a capture reported stuck; detected are
	// the windows found stuck when the pass was asked for.
	suspects, detected map[string]bool
	// attempts counts the forced reindexes of each path since the repair
	// last found it complete.
	attempts map[string]int
	// pendingKey and pendingSince are the pending namespace header the
	// repair first found with no open window, and when.
	pendingKey   string
	pendingSince time.Time
	// scan asks for a pass over the namespace's unaccepted rows.
	scan bool
	// retry keeps incomplete startup repairs on the periodic watch. It is
	// protected by the repair's mutex; attempts and pendingKey stay private
	// to the repository's repair goroutine.
	retry bool
}

// NewContractStuckRepair starts the repair. Each repository it sees gets a
// pass over its unaccepted rows; then it looks every period, and at each
// orphan mark once its grace passed.
func NewContractStuckRepair(options ContractStuckRepairOptions) (*ContractStuckRepair, error) {
	return newContractStuckRepair(options, nil)
}

// newContractStuckRepair is NewContractStuckRepair with adjust, which tests
// use to set the repair's seams, run before the repair starts.
func newContractStuckRepair(options ContractStuckRepairOptions, adjust func(*ContractStuckRepair)) (*ContractStuckRepair, error) {
	if options.Store == nil || options.MultiIndexer == nil {
		return nil, fmt.Errorf("contract stuck repair: incomplete options")
	}
	if options.Logger == nil {
		options.Logger = zap.NewNop()
	}
	period := options.Period
	if period <= 0 {
		period = ContractRepairPeriod
	}
	ctx, cancel := context.WithCancel(context.Background())
	r := &ContractStuckRepair{options: options, period: period, ctx: ctx, cancel: cancel, repos: make(map[string]*contractRepoRepair)}
	if adjust != nil {
		adjust(r)
	}
	if options.Coordinator != nil {
		options.Coordinator.attachRepair(r)
	}
	r.workers.Add(1)
	go r.watch()
	return r, nil
}

// Close stops the repair and waits for every repository's pass to end.
func (r *ContractStuckRepair) Close() {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.closed = true
	for _, state := range r.repos {
		state.retry = false
	}
	r.mu.Unlock()
	r.cancel()
	r.workers.Wait()
}

// Suspect reports paths of repo a job found blocked on one unchanged stamp
// for a period, or a capture found with no write of this process able to
// complete them. The repair re-checks them under the lane.
func (r *ContractStuckRepair) Suspect(repo string, paths []string) {
	if r == nil || len(paths) == 0 {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	state := r.repoLocked(repo)
	if state == nil {
		return
	}
	for _, path := range paths {
		state.suspects[path] = true
	}
	signalContractRepair(state.wake)
}

// Scan asks for a pass over repos' unaccepted rows, as after a job that
// could not settle its reads.
func (r *ContractStuckRepair) Scan(repos []string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, repo := range repos {
		if state := r.repoLocked(repo); state != nil {
			state.scan = true
			signalContractRepair(state.wake)
		}
	}
}

// Pending reports whether a repair of any of repos is due: a stuck window,
// a suspect, or a scan asked for. It reads only the store's window registry
// and the repair's own state.
func (r *ContractStuckRepair) Pending(repos []string) bool {
	if r == nil {
		return false
	}
	now := time.Now()
	for _, repo := range repos {
		if len(r.stuckWindows(repo, now)) > 0 {
			return true
		}
		r.mu.Lock()
		state := r.repos[repo]
		suspect := state != nil && (len(state.suspects) > 0 || state.scan || state.retry)
		r.mu.Unlock()
		if suspect {
			return true
		}
	}
	return false
}

func signalContractRepair(wake chan struct{}) {
	select {
	case wake <- struct{}{}:
	default:
	}
}

// repoLocked is repo's repair state, its goroutine started with a scan the
// first time the repository is seen: rows an earlier process left
// unaccepted are found then.
func (r *ContractStuckRepair) repoLocked(repo string) *contractRepoRepair {
	if r.closed || repo == "" {
		return nil
	}
	state := r.repos[repo]
	if state == nil {
		state = &contractRepoRepair{repo: repo, wake: make(chan struct{}, 1), suspects: make(map[string]bool), detected: make(map[string]bool), attempts: make(map[string]int), scan: true}
		r.repos[repo] = state
		r.options.Logger.Info("contract repair started", zap.String("repo", repo))
		signalContractRepair(state.wake)
		r.workers.Add(1)
		go r.run(state)
	}
	return state
}

// stuckWindows lists repo's open primary windows that are due a repair: one
// a failed mutation marked orphaned once its grace passed, or one open for a
// period.
func (r *ContractStuckRepair) stuckWindows(repo string, now time.Time) []string {
	var out []string
	for _, window := range r.options.Store.OpenContractCoreWindows(repo) {
		if window.Checkout != "" {
			continue
		}
		marked := !window.MarkedAt.IsZero() && now.Sub(window.MarkedAt) >= contractRepairOrphanGrace
		if marked || now.Sub(window.OpenedAt) >= r.period {
			out = append(out, window.Path)
		}
	}
	return out
}

// watch asks for each repository's pass when it is due: every period, and
// at each orphan mark once its grace passed. It takes the store's window
// registry and the scheduler's lock only, never a lane.
func (r *ContractStuckRepair) watch() {
	defer r.workers.Done()
	ticker := time.NewTicker(r.period)
	defer ticker.Stop()
	for {
		// Subscribed before the look: a mark landing during it wakes the
		// next one.
		_, orphaned := r.options.Store.ContractCoreOrphanEvents()
		now := time.Now()
		for _, repo := range r.options.MultiIndexer.RepoPrefixes() {
			stuck := r.stuckWindows(repo, now)
			deferred := false
			if c := r.options.Coordinator; c != nil {
				since := c.deferredOnInputsSince(repo)
				deferred = !since.IsZero() && now.Sub(since) >= r.period
			}
			r.mu.Lock()
			state, seen := r.repos[repo], r.repos[repo] != nil
			if !seen || len(stuck) > 0 || deferred || len(state.suspects) > 0 || state.scan || state.retry {
				state = r.repoLocked(repo)
				if state != nil {
					for _, path := range stuck {
						state.detected[path] = true
					}
					signalContractRepair(state.wake)
				}
			}
			r.mu.Unlock()
		}
		select {
		case <-r.ctx.Done():
			return
		case <-ticker.C:
		case <-orphaned:
			select {
			case <-r.ctx.Done():
				return
			case <-time.After(contractRepairOrphanGrace):
			}
		}
	}
}

// run is one repository's repair goroutine. It ends, and drops the
// repository's state, once the repository is no longer tracked; a later
// report about it starts again.
func (r *ContractStuckRepair) run(state *contractRepoRepair) {
	defer r.workers.Done()
	for {
		select {
		case <-r.ctx.Done():
			return
		case <-state.wake:
		}
		if _, _, err := r.repoIndexer(state.repo); err != nil {
			r.mu.Lock()
			if r.repos[state.repo] == state {
				delete(r.repos, state.repo)
			}
			r.mu.Unlock()
			return
		}
		r.pass(state)
	}
}

// pass takes the repository's lane when it is free, retrying every poll, and
// repairs under it. A lane busy for a whole period is logged, and the next
// pass goes on asking for it.
func (r *ContractStuckRepair) pass(state *contractRepoRepair) {
	defer func() {
		if recovered := recover(); recovered != nil {
			r.options.Logger.Error("contract stuck repair recovered", zap.String("repo", state.repo), zap.Any("panic", recovered))
		}
	}()
	if c := r.options.Coordinator; c != nil {
		c.repairPassStarted(state.repo)
		defer c.repairPassEnded(state.repo)
	}
	if r.beforeLane != nil {
		r.beforeLane(state.repo)
	}
	deadline := time.Now().Add(r.period)
	for {
		release, ok := r.options.MultiIndexer.TryRepositoryMutationLanes([]string{state.repo})
		if ok {
			defer release()
			break
		}
		if !time.Now().Before(deadline) {
			r.options.Logger.Info("contract stuck repair", zap.String("repo", state.repo), zap.String("outcome", "busy"))
			signalContractRepair(state.wake)
			return
		}
		select {
		case <-r.ctx.Done():
			return
		case <-time.After(contractRepairLanePoll):
		}
	}
	r.repairHeld(withHeldRepositoryLanes(r.ctx, []string{state.repo}), state)
}

// repairHeld repairs a repository under its held lane, in order: the stuck
// paths re-checked, one forced reindex of every stuck path with attempts
// left, the paths still stuck re-scanned, a failure envelope for each path
// whose attempts ran out, and the namespace header last, only once no window
// is left open.
func (r *ContractStuckRepair) repairHeld(ctx context.Context, state *contractRepoRepair) {
	defer func() {
		r.mu.Lock()
		state.retry = !r.closed && (len(state.attempts) > 0 || state.pendingKey != "")
		r.mu.Unlock()
	}()
	repo := state.repo
	store := r.options.Store.AtGeneration(0)
	r.mu.Lock()
	suspects, detected, scan := state.suspects, state.detected, state.scan
	state.suspects, state.detected, state.scan = make(map[string]bool), make(map[string]bool), false
	r.mu.Unlock()
	// No mutation of the repository is live while its lane is held, so every
	// open primary window is stuck, whatever its age or seq: a failed body's,
	// a failed staging write's, or one a kept write never closed. The
	// re-check runs under the lane; what was found before it is only a hint.
	stuck := make(map[string]bool)
	for _, window := range r.options.Store.OpenContractCoreWindows(repo) {
		if window.Checkout == "" {
			stuck[window.Path] = true
		}
	}
	candidates := make(map[string]bool, len(suspects))
	for path := range suspects {
		candidates[path] = true
	}
	for path := range state.attempts {
		candidates[path] = true
	}
	if scan {
		for _, path := range r.unacceptedRows(ctx, store, repo) {
			candidates[path] = true
		}
	}
	for path := range candidates {
		if !stuck[path] && r.restartLeft(ctx, store, repo, path) {
			stuck[path] = true
		}
	}
	var healed []string
	for path := range detected {
		if !stuck[path] {
			healed = append(healed, path)
		}
	}
	for path := range suspects {
		if !stuck[path] && !detected[path] {
			healed = append(healed, path)
		}
	}
	if len(healed) > 0 {
		sort.Strings(healed)
		r.options.Logger.Info("contract stuck repair", zap.String("repo", repo), zap.String("outcome", "healed"), zap.Strings("paths", healed))
	}
	for path := range state.attempts {
		if !stuck[path] && !r.inFlight(ctx, store, repo, path) {
			// Complete again: a save, or a repair, accepted the path.
			delete(state.attempts, path)
		}
	}
	var reindex, envelope []string
	for path := range stuck {
		if state.attempts[path] >= contractRepairEnvelopeAfter {
			envelope = append(envelope, path)
		} else {
			reindex = append(reindex, path)
		}
	}
	sort.Strings(reindex)
	if len(reindex) > 0 {
		// One forced reindex for every stuck path of the repository.
		countContractRepair(viewmetrics.ContractRepairStuckReceipt)
		err := r.reindexPaths(ctx, repo, reindex)
		r.logRepair(repo, viewmetrics.ContractRepairStuckReceipt, reindex, err)
		for _, path := range reindex {
			if !r.inFlight(ctx, store, repo, path) {
				delete(state.attempts, path)
				continue
			}
			// The repair's own write moved the seq; only a complete row
			// resets the count.
			state.attempts[path]++
			if state.attempts[path] >= contractRepairEnvelopeAfter {
				envelope = append(envelope, path)
			}
		}
	}
	sort.Strings(envelope)
	for _, path := range slices.Compact(envelope) {
		countContractRepair(viewmetrics.ContractRepairStuckEnvelope)
		err := r.envelopePaths(ctx, repo, []string{path})
		r.logRepair(repo, viewmetrics.ContractRepairStuckEnvelope, []string{path}, err)
		if !r.inFlight(ctx, store, repo, path) {
			delete(state.attempts, path)
		}
	}
	for _, window := range store.OpenContractCoreWindows(repo) {
		if window.Checkout == "" {
			// The header waits until no window is left: the baseline
			// must never close a window it did not open.
			return
		}
	}
	r.repairHeader(ctx, state)
}

func (r *ContractStuckRepair) logRepair(repo, kind string, paths []string, err error) {
	outcome := "repaired"
	if err != nil {
		outcome = "failed"
	}
	fields := []zap.Field{zap.String("repo", repo), zap.String("kind", kind), zap.Strings("paths", paths), zap.String("outcome", outcome)}
	if err != nil {
		fields = append(fields, zap.Error(err))
	}
	r.options.Logger.Info("contract stuck repair", fields...)
}

// unacceptedRows lists the repository's unaccepted primary receipt rows.
func (r *ContractStuckRepair) unacceptedRows(ctx context.Context, store *store_sqlite.Store, repo string) []string {
	stamps, err := store.ContractBoundaryReceiptStampsContext(ctx, repo, "", nil)
	if err != nil {
		return nil
	}
	var out []string
	for path, stamp := range stamps {
		if !stamp.Accepted {
			out = append(out, path)
		}
	}
	return out
}

// inFlight reports whether path is still in flight: its primary core-write
// window is open, whatever its age, or its row was left by an earlier
// process.
func (r *ContractStuckRepair) inFlight(ctx context.Context, store *store_sqlite.Store, repo, path string) bool {
	if store.ContractReceiptWriteStates(repo, "", []string{path})[path].Writing {
		return true
	}
	return r.restartLeft(ctx, store, repo, path)
}

// restartLeft reports whether path's primary row is unaccepted and no
// mutation of this process wrote it: an earlier process stopped mid-body or
// mid-accept. A failure envelope is never stuck.
func (r *ContractStuckRepair) restartLeft(ctx context.Context, store *store_sqlite.Store, repo, path string) bool {
	row, _, err := store.ContractBoundaryReceiptContext(ctx, repo, "", path)
	if err != nil || row == nil || row.Accepted || contractReceiptIsFailureEnvelope(row) {
		return false
	}
	return store.ContractReceiptWriteStates(repo, "", []string{path})[path].Seq == 0
}

// repoIndexer is the repository's live indexer and root.
func (r *ContractStuckRepair) repoIndexer(repo string) (*Indexer, string, error) {
	mi := r.options.MultiIndexer
	mi.mu.RLock()
	idx, meta := mi.indexers[repo], mi.repos[repo]
	mi.mu.RUnlock()
	if idx == nil || meta == nil {
		return nil, "", fmt.Errorf("contract stuck repair: repository %q is not tracked", repo)
	}
	return idx, meta.RootPath, nil
}

// reindexPaths reparses paths in one forced reindex under the held lane.
func (r *ContractStuckRepair) reindexPaths(ctx context.Context, repo string, paths []string) error {
	if r.reindex != nil {
		return r.reindex(ctx, repo, paths)
	}
	return r.realReindex(ctx, repo, paths)
}

// realReindex is the forced reindex an explicit watcher reindex runs: its
// begin stages the paths' receipts again, and its accept closes their
// windows and announces Published.
func (r *ContractStuckRepair) realReindex(ctx context.Context, repo string, paths []string) error {
	idx, root, err := r.repoIndexer(repo)
	if err != nil {
		return err
	}
	abs := make([]string, 0, len(paths))
	for _, path := range paths {
		abs = append(abs, filepath.Join(root, filepath.FromSlash(strings.TrimPrefix(path, repo+"/"))))
	}
	return idx.withOutputGeneration(ctx, OutputEntryContractRepair, func() error {
		_, err := r.options.MultiIndexer.incrementalReindexRepoRawMode(repo, abs, incrementalPathMode{detectDeletions: true, forceExplicitFiles: true})
		return err
	})
}

// envelopePaths excludes paths a forced reindex could not repair, in one
// mutation: the envelope's receipt is staged first, opening the window,
// then the file's core rows are evicted, and the accept closes the window.
// An evict before the staging would leave torn rows no window covers; an
// envelope without the evict would leave the torn rows readable as proven.
func (r *ContractStuckRepair) envelopePaths(ctx context.Context, repo string, paths []string) error {
	if r.envelope != nil {
		return r.envelope(ctx, repo, paths)
	}
	idx, _, err := r.repoIndexer(repo)
	if err != nil {
		return err
	}
	return idx.withOutputGeneration(ctx, OutputEntryContractRepair, func() error {
		current, err := idx.currentRepositoryMutationIndexer()
		if err != nil {
			return err
		}
		journal := current.contractCoreInputs
		if journal == nil {
			return errors.New("contract stuck repair: no contract journal for the envelope")
		}
		if err := journal.stageFailureEnvelopesOver(ctx, paths, true); err != nil {
			return err
		}
		for _, path := range paths {
			if err := r.evictPath(repo, strings.TrimPrefix(path, repo+"/")); err != nil {
				return err
			}
		}
		return nil
	})
}

func (r *ContractStuckRepair) evictPath(repo, rel string) error {
	if r.evict != nil {
		return r.evict(repo, rel)
	}
	return r.realEvict(repo, rel)
}

func (r *ContractStuckRepair) realEvict(repo, rel string) error {
	_, _, err := r.options.MultiIndexer.incrementalEvictRepoRaw(repo, rel)
	return err
}

// repairHeader accepts a namespace header left pending with every window
// closed and every row complete (an accept refused after its windows
// closed): once it stayed pending for a period, or an actor waited on it for
// one, the baseline is rebuilt with its current short-circuit bypassed and
// the accepted header is announced. A header a live baseline job owns is that
// job's.
func (r *ContractStuckRepair) repairHeader(ctx context.Context, state *contractRepoRepair) {
	repo := state.repo
	store := r.options.Store.AtGeneration(0)
	current, found, err := store.ContractInputStateContext(ctx, repo, "")
	if err != nil || !found || current.Accepted {
		state.pendingKey = ""
		return
	}
	work, err := store.PendingContractWorkForScopeContext(ctx, repo, "")
	if err != nil {
		return
	}
	key := current.InputVersion + "\x00" + current.InputFingerprint
	now := time.Now()
	if state.pendingKey != key {
		state.pendingKey, state.pendingSince = key, now
	}
	for _, row := range work {
		if slices.Contains(row.Scope.Causes, contractBaselineReconcileCause) && r.options.MultiIndexer.contractBaselineActive(repo) {
			return
		}
	}
	waited := now.Sub(state.pendingSince) >= r.period
	if c := r.options.Coordinator; c != nil {
		if since := c.deferredOnInputsSince(repo); !since.IsZero() && now.Sub(since) >= r.period {
			waited = true
		}
	}
	if !waited {
		return
	}
	countContractRepair(viewmetrics.ContractRepairPendingHeader)
	options := r.options.Baseline
	options.Context = ctx
	err = reconcilePrimaryContractBaseline(ctx, options, repo)
	r.logRepair(repo, viewmetrics.ContractRepairPendingHeader, nil, err)
	if err != nil {
		return
	}
	state.pendingKey = ""
	if hooks := r.options.MultiIndexer.contractCoreRuntime.Load(); hooks != nil && hooks.Published != nil {
		hooks.Published(ctx, repo, "")
	}
}
