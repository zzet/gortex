package goanalysis

import (
	"context"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"
	"golang.org/x/tools/go/packages"

	"github.com/zzet/gortex/internal/semantic"
)

// Background warm-up of a checkout's closure metadata.
//
// A handle-rooted cached pass over a package no earlier pass listed pays a
// cold `go list -export -deps` of that package's closure (seconds for a
// large package). The warm-up lists the whole module once, in the
// background, when a checkout's compiler state is first wanted, and merges
// the listing into the checkout's state, so the first pass over any package
// finds its closure listed and validates it like any other hit. It warms
// metadata only (no export data is read, no types are retained), writes no
// facts, and never runs on a pass's path:
//
//   - every compiler load of the provider preempts a running warm-up (its
//     go command is killed through the context), and a preempted warm-up
//     starts again once no load has run for warmupQuiet. The go build cache
//     keeps the export data every attempt built, so retries converge;
//   - the listing runs the go command with a small build parallelism, and
//     every tool it runs (compile, asm, cgo, link) at the lowest scheduling
//     priority the host offers (-toolexec; the tool IDs, and so the build
//     cache keys, are unchanged);
//   - with a foreground-activity view installed (the daemon's), warm-ups are
//     staggered — one listing at a time across all checkouts, checkouts
//     touched since the daemon started ahead of the rest — and deferred
//     until the daemon has seen no foreground work (an edit, a refresh
//     ticket, a checkout's build) for defaultWarmupIdle, or for
//     defaultWarmupUntouchedIdle when nobody has touched the checkout since
//     the daemon started. Foreground work that appears while a listing runs
//     cancels it like a compiler load does;
//   - a listing older than a pass's never replaces the pass's metadata, a
//     warm-up never replaces retained metadata that still describes its
//     package (keepRetained, typecheck_retention.go: an edit in flight
//     elsewhere in the checkout makes the listing's entries degraded), and
//     a package whose dependency a later listing changed is marked
//     export-stale (mergeListing), so the warm-up can only add hits, never
//     stale ones, and never evicts what the checkout's passes touch;
//   - a module manifest change starts the state over and the next request
//     warms again.

// Warm-up states (CompilerCacheStats.WarmupState, CheckoutWarmupStatus.State).
const (
	warmupRunning    = "running"
	warmupPreempted  = "preempted"
	warmupWarm       = "warm"
	warmupFailed     = "failed"
	warmupSuperseded = "superseded"
	warmupCancelled  = "cancelled"
	// warmupDeferred is a warm-up waiting for its turn: the slot, a quiet
	// provider, or an idle daemon (CheckoutWarmupStatus.DeferredBy says
	// which).
	warmupDeferred = "deferred"
)

// Outcomes of WarmCheckoutCompiler.
const (
	warmupOutcomeStarted  = "started"
	warmupOutcomeRunning  = "running"
	warmupOutcomeWarm     = "warm"
	warmupOutcomeDisabled = "disabled"
)

const (
	// defaultWarmupQuiet is how long no compiler load must have run before a
	// warm-up attempt starts.
	defaultWarmupQuiet = 2 * time.Second
	// maxWarmupAttempts bounds the listings started for one set of module
	// manifests (preempted attempts included); maxWarmupFailures the ones
	// that failed outright.
	maxWarmupAttempts = 64
	maxWarmupFailures = 2
	// warmupBatchPackages is how many of the module's packages one warm-up
	// listing covers (with their dependencies). Each batch is merged as it
	// completes, so a warm-up that foreground work keeps interrupting still
	// makes durable progress, and the packages most likely to be edited
	// (recent handle roots, then the largest) are listed first.
	warmupBatchPackages = 24
	// warmupRecentRoots bounds the handle-root directories remembered per
	// checkout to order the next warm-up.
	warmupRecentRoots = 16
	// warmupTargetedPackages bounds the dirty and recently touched packages
	// the warm-up lists one at a time ahead of the module's batches.
	warmupTargetedPackages = 8
	// warmupPrecheckPackages is how many of the first-listed packages the
	// warm-up pre-parses (bodies stripped) and whose direct imports it reads
	// from export data, so a first pass over them only type-checks.
	warmupPrecheckPackages = 4
	// defaultWarmupMaxDeferral is how long foreground work elsewhere in the
	// daemon may defer the warm-up of a checkout someone touched; after it
	// the warm-up lists anyway (at the lowest priority, still yielding to
	// this provider's compiler loads) instead of starving behind a daemon
	// that is never idle.
	defaultWarmupMaxDeferral = 20 * time.Second
	// warmupRetryAfterFailure is when a failed warm-up may be asked again.
	warmupRetryAfterFailure = time.Minute
	// defaultWarmupIdle is how long the daemon must have seen no foreground
	// work before a warm-up of a checkout touched since the daemon started
	// begins; defaultWarmupUntouchedIdle is the same for a checkout nobody
	// has touched, which only an idle daemon warms.
	defaultWarmupIdle          = 5 * time.Second
	defaultWarmupUntouchedIdle = 30 * time.Second
	// defaultWarmupPoll is how often a waiting warm-up re-checks its turn and
	// a running one looks for foreground work.
	defaultWarmupPoll = 250 * time.Millisecond
	// defaultWarmupStall is how long one module batch's listing may run at
	// background priority before it is retried at normal priority. A batch
	// lists in well under a minute on an idle host.
	defaultWarmupStall = 2 * time.Minute
)

// CheckoutWarmupStatus is one checkout's background warm-up.
type CheckoutWarmupStatus struct {
	State       string
	Digest      string
	Attempts    int
	Preemptions int
	// Ms is the last completed warm-up's listing wall time (all batches);
	// Packages the packages it listed and Merged the ones it added to the
	// state. Batches counts the listings merged, Remaining the module
	// packages still waiting for one.
	Ms        int64
	Packages  int
	Merged    int
	Batches   int
	Remaining int
	// Forced reports that the warm-up stopped waiting for an idle daemon
	// (see defaultWarmupMaxDeferral).
	Forced   bool
	Error    string
	Started  time.Time
	Finished time.Time
	// Touched is whether the checkout had served foreground work when the
	// warm-up last asked; DeferredBy what a deferred warm-up waits for, or
	// the foreground work that preempted it last.
	Touched    bool
	DeferredBy string
	// Pauses counts the times a module batch's listing or merge waited for
	// answer-path work (an edit cycle, a tool call) and PausedMs how long
	// those waits took in all (see pauseForAnswerPath).
	Pauses   int
	PausedMs int64
	// Escalations counts the background listings that stalled past the
	// deadline and were retried at normal priority.
	Escalations int
}

// checkoutWarmup is the bookkeeping of one checkout's warm-up, guarded by
// warmupRegistry.mu.
type checkoutWarmup struct {
	loadDir string
	// want is the manifest digest the warm-up must cover; status.Digest the
	// one it covered last.
	want    string
	running bool
	cancel  context.CancelFunc
	// state is the typecheck state the last completed listing was merged
	// into; a different current state (evicted, or started over) is cold.
	state    *checkoutTypecheckState
	failures int
	status   CheckoutWarmupStatus
	// requested orders waiting warm-ups of the same class; waiting is set
	// while the warm-up waits for its turn.
	requested time.Time
	waiting   bool
	// planned is set once the module's packages are enumerated into queue
	// (import paths, in listing order) for the wanted manifests.
	planned bool
	queue   []string
	// targeted counts the listings still to run one package at a time: the
	// checkout's dirty and recently touched packages lead the queue and are
	// listed (and merged) each on its own, before the rest of the module.
	targeted int
	// pending is the directories (symbolic links resolved) of the
	// targeted packages not merged yet, and targetDirs each targeted
	// package's directory: a pass over one of them waits for its listing
	// instead of listing itself (waitTargetedListing).
	pending    map[string]bool
	targetDirs map[string]string
	// inflight is the directories of the targeted listing running now: a
	// pass over one of them adopts that listing (waits for its merge,
	// unbounded while the warm-up runs) instead of starting a second
	// listing of the same closure. closureFiles is each targeted
	// directory's module-internal closure size in Go files (capped at the
	// threshold, sized by the first pass that asks): below
	// targetedWaitMinFiles a pass lists itself unless the listing runs.
	inflight     map[string]bool
	closureFiles map[string]int
	// deferredSince is when foreground work first deferred this warm-up;
	// forced is set once it waited longer than the maximum deferral.
	deferredSince time.Time
	forced        bool
	// escalate lists the next module batch at normal priority: the last
	// background listing stalled (backgroundListingDeadline).
	escalate bool
}

// warmupRegistry tracks the provider's warm-ups and its interactive
// compiler loads, which warm-ups yield to.
type warmupRegistry struct {
	mu      sync.Mutex
	entries map[string]*checkoutWarmup
	// loads counts the interactive compiler loads running per module
	// directory and lastLoad is when the last one per directory ended: a
	// warm-up (or an export relist) yields only to loads over its own
	// module directory (see beginCompilerLoad).
	loads    map[string]int
	lastLoad map[string]time.Time
	// relistCancel preempts the running export relist of a module
	// directory (see typecheck_relist.go); relists tracks the running ones.
	relistCancel map[string]context.CancelFunc
	relists      sync.WaitGroup
	// progress is closed and replaced whenever a warm-up merges a
	// targeted package, changes state or exits (waitTargetedListing).
	progress chan struct{}
	ctx      context.Context
	stop     context.CancelFunc
	// quiet overrides defaultWarmupQuiet (tests).
	quiet time.Duration
	// activity is the daemon's foreground-activity view (nil: warm-ups yield
	// to compiler loads only). active is the warm-up whose listing holds the
	// one warm-up slot; lastBusy is when foreground work was last seen and
	// since when the registry has run (the idle clock starts there).
	activity semantic.ForegroundActivity
	active   *checkoutWarmup
	lastBusy time.Time
	since    time.Time
	// idle, untouchedIdle and poll override their defaults (tests).
	idle          time.Duration
	untouchedIdle time.Duration
	poll          time.Duration
	// done is closed and replaced whenever a warm-up goroutine exits (tests
	// and measurement wait on it).
	done chan struct{}
	// maxDeferral overrides defaultWarmupMaxDeferral and batchSize
	// warmupBatchPackages (tests).
	maxDeferral time.Duration
	batchSize   int
	// toolCalls overrides the count of tool calls in flight and maxPause
	// the bound of one answer-path pause (tests).
	toolCalls func() int64
	maxPause  time.Duration
	// stall overrides defaultWarmupStall (tests).
	stall time.Duration
	// recent is, per module directory, the handle-root directories of the
	// latest passes, newest first. It is persisted per module directory
	// under rootsDir (typecheck_roots_persist.go) and read back on first
	// use, so a restarted daemon keeps the targeted order.
	recent      map[string][]string
	rootsDir    string
	rootsDirSet bool
	rootsLoaded map[string]bool
	persistMu   sync.Mutex
	persisted   map[string]uint64
	rootsSeq    map[string]uint64
	persists    sync.WaitGroup
}

func (r *warmupRegistry) maxDeferralPeriod() time.Duration {
	if r.maxDeferral > 0 {
		return r.maxDeferral
	}
	return defaultWarmupMaxDeferral
}

// noteRoots remembers a pass's handle-root directories for loadDir's next
// warm-up.
func (r *warmupRegistry) noteRoots(loadDir string, dirs map[string]struct{}) {
	if len(dirs) == 0 {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.loadPersistedRootsLocked(loadDir)
	if r.recent == nil {
		r.recent = map[string][]string{}
	}
	cur := r.recent[loadDir]
	fresh := make([]string, 0, len(dirs)+len(cur))
	for dir := range dirs {
		fresh = append(fresh, filepath.Clean(dir))
	}
	sort.Strings(fresh)
	seen := make(map[string]bool, len(fresh))
	for _, dir := range fresh {
		seen[dir] = true
	}
	for _, dir := range cur {
		if !seen[dir] {
			seen[dir] = true
			fresh = append(fresh, dir)
		}
	}
	if len(fresh) > warmupRecentRoots {
		fresh = fresh[:warmupRecentRoots]
	}
	changed := len(fresh) != len(cur)
	for i := 0; !changed && i < len(fresh); i++ {
		changed = fresh[i] != cur[i]
	}
	r.recent[loadDir] = fresh
	if changed {
		r.persistRootsLocked(loadDir, fresh)
	}
}

func (r *warmupRegistry) recentRoots(loadDir string) map[string]int {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.loadPersistedRootsLocked(loadDir)
	out := map[string]int{}
	for i, dir := range r.recent[loadDir] {
		out[dir] = i
	}
	return out
}

func (r *warmupRegistry) quietPeriod() time.Duration {
	if r.quiet > 0 {
		return r.quiet
	}
	return defaultWarmupQuiet
}

func (r *warmupRegistry) idlePeriod(touched bool) time.Duration {
	if touched {
		if r.idle > 0 {
			return r.idle
		}
		return defaultWarmupIdle
	}
	if r.untouchedIdle > 0 {
		return r.untouchedIdle
	}
	return defaultWarmupUntouchedIdle
}

func (r *warmupRegistry) pollInterval() time.Duration {
	if r.poll > 0 {
		return r.poll
	}
	return defaultWarmupPoll
}

// SetForegroundActivity installs the daemon's foreground-activity view:
// from then on warm-ups are staggered, wait for an idle daemon, and are
// cancelled by foreground work (see the comment at the top of this file).
func (p *Provider) SetForegroundActivity(activity semantic.ForegroundActivity) {
	if p == nil || activity == nil {
		return
	}
	r := &p.warm
	r.mu.Lock()
	r.activity = activity
	r.mu.Unlock()
}

// SetWarmupIdle overrides how long the daemon must be idle before a warm-up
// of a touched (touched) and of an untouched (untouched) checkout starts; a
// non-positive value keeps the default.
func (p *Provider) SetWarmupIdle(touched, untouched time.Duration) {
	if p == nil {
		return
	}
	r := &p.warm
	r.mu.Lock()
	r.idle, r.untouchedIdle = touched, untouched
	r.mu.Unlock()
}

// warmupBuildFlags keeps the warm-up's go command to a small build
// parallelism — a quarter of the CPUs, at most GOMAXPROCS, at least one —
// and runs every tool it invokes at the lowest priority (lowPriorityToolexec).
func warmupBuildFlags() []string {
	n := runtime.NumCPU() / 4
	if procs := runtime.GOMAXPROCS(0); n > procs {
		n = procs
	}
	if n < 1 {
		n = 1
	}
	flags := []string{"-p=" + strconv.Itoa(n)}
	if wrapper := lowPriorityToolexec(); wrapper != "" {
		flags = append(flags, "-toolexec="+wrapper)
	}
	return flags
}

// targetedWarmupBuildFlags is the build flags of a targeted batch (one
// dirty or recently touched package): a quarter of the CPUs, not capped at
// GOMAXPROCS, at normal scheduling priority. Such a batch compiles the
// export data of exactly the packages the checkout's next delta would
// otherwise compile in its own listing, synchronously and at full
// priority; the lowest priority made that same compile 5-10x slower on a
// busy host (mcp's closure: 53 s at background QoS, 5 s at normal).
func targetedWarmupBuildFlags() []string {
	n := runtime.NumCPU() / 4
	if n < 1 {
		n = 1
	}
	return []string{"-p=" + strconv.Itoa(n)}
}

// lowPriorityToolexec is the -toolexec wrapper that runs a warm-up's
// compiler, assembler, cgo and linker invocations at the lowest scheduling
// priority: background QoS on macOS (taskpolicy -b: the efficiency cores and
// throttled I/O), nice 19 elsewhere, nothing on Windows or when neither tool
// exists. The go command derives a tool's ID from `<wrapper> <tool> -V=full`,
// whose output the wrapper does not change, so the listing shares every
// build-cache entry with ordinary builds.
var lowPriorityToolexec = sync.OnceValue(func() string {
	switch runtime.GOOS {
	case "windows", "plan9", "js", "wasip1":
		return ""
	case "darwin", "ios":
		if isExecutable("/usr/sbin/taskpolicy") {
			return "/usr/sbin/taskpolicy -b"
		}
	}
	nice, err := exec.LookPath("nice")
	if err != nil || strings.ContainsAny(nice, " \t'\"") {
		return ""
	}
	return nice + " -n 19"
})

func isExecutable(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular() && info.Mode().Perm()&0o111 != 0
}

// beginCompilerLoad marks an interactive compiler load over the module
// directory dir: it preempts that directory's running warm-up and export
// relist, and holds new ones back until the load ends. The returned func
// ends the load.
//
// Policy: a load never preempts, nor holds back, another module directory's
// warm-up. A daemon runs a checkout's whole-module load for minutes (a cold
// primary: 606 s measured); cancelling a worktree's warm-up for it threw
// away the listing in flight and kept the worktree cold until long after
// its first edit. The worktree's warm-up lists a few targeted packages at a
// time at the lowest scheduling priority, so running it alongside the other
// load costs that load little; within one module directory the pass's load
// still wins, as its listing would race the warm-up's over the same state.
func (p *Provider) beginCompilerLoad(dir string) func() {
	dir = filepath.Clean(dir)
	r := &p.warm
	r.mu.Lock()
	if r.loads == nil {
		r.loads = map[string]int{}
	}
	r.loads[dir]++
	for _, e := range r.entries {
		if e.cancel != nil && filepath.Clean(e.loadDir) == dir {
			e.cancel()
			e.cancel = nil
			e.status.Preemptions++
			e.status.State = warmupPreempted
			r.notifyLocked()
		}
	}
	if cancel := r.relistCancel[dir]; cancel != nil {
		cancel()
		delete(r.relistCancel, dir)
	}
	r.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			r.mu.Lock()
			r.loads[dir]--
			if r.loads[dir] <= 0 {
				delete(r.loads, dir)
			}
			if r.lastLoad == nil {
				r.lastLoad = map[string]time.Time{}
			}
			r.lastLoad[dir] = time.Now()
			r.mu.Unlock()
		})
	}
}

// loadWaitLocked is how long work over dir must wait for that directory's
// compiler loads (running, or ended within the quiet period); the caller
// holds r.mu.
func (r *warmupRegistry) loadWaitLocked(dir string) time.Duration {
	dir = filepath.Clean(dir)
	if r.loads[dir] > 0 {
		return r.quietPeriod()
	}
	if since := time.Since(r.lastLoad[dir]); since < r.quietPeriod() {
		return r.quietPeriod() - since
	}
	return 0
}

// warmupEligible reports the module directory a warm-up would list, or why
// the checkout's passes never use the retained state.
func (p *Provider) warmupEligible(root string) (loadDir, reason string) {
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return "", "ineligible_path"
	}
	if p.includeTest {
		return "", "ineligible_tests"
	}
	if goTypesNeedDepsClosure() {
		return "", "ineligible_needdeps_closure"
	}
	loadDir, moduleRoots := goLoadDir(absRoot)
	if moduleRoots == 0 {
		return "", "ineligible_no_module"
	}
	if filepath.Clean(loadDir) != filepath.Clean(absRoot) || fileExists(filepath.Join(loadDir, "go.work")) {
		return "", "ineligible_multi_module"
	}
	if fileExists(filepath.Join(loadDir, "vendor", "modules.txt")) {
		return "", "ineligible_vendor"
	}
	return loadDir, ""
}

// WarmCheckoutCompiler starts the background whole-module listing of the
// checkout rooted at root (see the comment at the top of this file). It
// returns at once.
func (p *Provider) WarmCheckoutCompiler(root string, scope semantic.CheckoutCompilerScope) string {
	if !scope.HandleRoots || !scope.TypecheckCache {
		return warmupOutcomeDisabled
	}
	loadDir, reason := p.warmupEligible(root)
	if reason != "" {
		return reason
	}
	digest := goManifestDigest(loadDir)
	p.tcMu.Lock()
	current := p.tcStates[loadDir]
	p.tcMu.Unlock()

	r := &p.warm
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.entries == nil {
		r.entries = map[string]*checkoutWarmup{}
	}
	if r.ctx == nil {
		r.ctx, r.stop = context.WithCancel(context.Background())
		r.since = time.Now()
	}
	if r.ctx.Err() != nil {
		return warmupCancelled
	}
	e := r.entries[loadDir]
	if e != nil && e.running {
		e.want = digest
		return warmupOutcomeRunning
	}
	if e != nil && e.status.State == warmupWarm && e.status.Digest == digest && e.state != nil && e.state == current {
		return warmupOutcomeWarm
	}
	if e != nil && e.status.State == warmupFailed && e.want == digest && time.Since(e.status.Finished) < warmupRetryAfterFailure {
		return warmupFailed
	}
	if e == nil || e.want != digest {
		e = &checkoutWarmup{loadDir: loadDir}
		r.entries[loadDir] = e
		r.evictLocked()
	}
	e.want = digest
	e.running = true
	e.failures = 0
	e.status.Attempts = 0
	e.status.Error = ""
	e.status.Started = time.Now()
	e.requested = e.status.Started
	go p.runWarmup(e)
	return warmupOutcomeStarted
}

// evictLocked bounds the tracked checkouts like the retained states:
// beyond maxTypecheckCheckouts, the least recently started idle entries go.
func (r *warmupRegistry) evictLocked() {
	for len(r.entries) > maxTypecheckCheckouts {
		var oldest *checkoutWarmup
		for _, e := range r.entries {
			if !e.running && (oldest == nil || e.status.Started.Before(oldest.status.Started)) {
				oldest = e
			}
		}
		if oldest == nil {
			return
		}
		delete(r.entries, oldest.loadDir)
	}
}

// waitWarmupTurn blocks until the warm-up may list, then takes the one
// warm-up slot and registers the attempt's cancel func, atomically with the
// last check. It may list when no compiler load runs or ran within the quiet
// period, no other warm-up holds the slot, no waiting warm-up goes first
// (touched checkouts before untouched ones, then by request time), and —
// with a foreground-activity view installed — no foreground work is in
// flight and none was seen for the idle period of the checkout's class. It
// returns the attempt context, or nil once the provider closed.
func (p *Provider) waitWarmupTurn(e *checkoutWarmup) (context.Context, context.CancelFunc) {
	r := &p.warm
	for {
		r.mu.Lock()
		parent := r.ctx
		activity := r.activity
		r.mu.Unlock()
		if parent.Err() != nil {
			return nil, nil
		}

		// The view takes the daemon's own locks: never call it under r.mu.
		// Touched is read first, so the queue orders by the current class.
		touched := false
		if activity != nil {
			touched = activity.CheckoutTouched(e.loadDir)
		}
		r.mu.Lock()
		e.status.Touched = touched
		wait, reason := r.queueWaitLocked(e)
		r.mu.Unlock()
		busy := ""
		var last time.Time
		if wait == 0 && activity != nil {
			busy, last = activity.ForegroundWork()
		}

		r.mu.Lock()
		if parent.Err() != nil {
			r.mu.Unlock()
			return nil, nil
		}
		forced := false
		if activity != nil && touched && !e.deferredSince.IsZero() && time.Since(e.deferredSince) >= r.maxDeferralPeriod() {
			// Foreground work kept this touched checkout's warm-up waiting
			// too long: list anyway, at the lowest priority, yielding only
			// to this provider's compiler loads.
			forced = true
			e.forced = true
			e.status.Forced = true
		}
		if activity != nil && wait == 0 && !forced {
			now := time.Now()
			if last.After(r.lastBusy) {
				r.lastBusy = last
			}
			if busy != "" {
				r.lastBusy = now
				wait, reason = r.pollInterval(), busy
			} else {
				quietSince := r.since
				if r.lastBusy.After(quietSince) {
					quietSince = r.lastBusy
				}
				if last := r.lastLoad[filepath.Clean(e.loadDir)]; last.After(quietSince) {
					quietSince = last
				}
				if need := r.idlePeriod(touched); now.Sub(quietSince) < need {
					wait, reason = need-now.Sub(quietSince), "idle"
				}
			}
			if wait == 0 {
				// Another warm-up may have taken the slot while the view
				// was consulted.
				wait, reason = r.queueWaitLocked(e)
			}
		}
		if wait == 0 {
			r.active = e
			e.waiting = false
			ctx, cancel := context.WithCancel(parent)
			e.cancel = cancel
			e.status.State = warmupRunning
			e.status.DeferredBy = ""
			r.mu.Unlock()
			if activity != nil && !forced {
				go p.yieldToForeground(ctx, e, activity)
			}
			return ctx, cancel
		}
		if activity != nil && touched && e.deferredSince.IsZero() && (reason == "idle" || (reason != "compiler_load" && reason != "another_warmup" && reason != "queued")) {
			e.deferredSince = time.Now()
		}
		e.waiting = true
		e.status.State = warmupDeferred
		r.notifyLocked()
		e.status.DeferredBy = reason
		r.mu.Unlock()
		if poll := r.pollInterval(); wait > poll {
			wait = poll
		}
		timer := time.NewTimer(wait)
		select {
		case <-parent.Done():
			timer.Stop()
			return nil, nil
		case <-timer.C:
		}
	}
}

// queueWaitLocked is how long e must wait for compiler loads, the slot and
// the queue, and why; zero when none of them holds it back. The caller holds
// r.mu.
func (r *warmupRegistry) queueWaitLocked(e *checkoutWarmup) (time.Duration, string) {
	if wait := r.loadWaitLocked(e.loadDir); wait > 0 {
		return wait, "compiler_load"
	}
	if r.active != nil && r.active != e {
		return r.pollInterval(), "another_warmup"
	}
	for _, other := range r.entries {
		if other == e || !other.waiting || !other.running {
			continue
		}
		if warmupGoesFirst(other, e) {
			return r.pollInterval(), "queued"
		}
	}
	return 0, ""
}

// warmupGoesFirst orders waiting warm-ups: touched checkouts first, then by
// request time, then by module directory.
func warmupGoesFirst(a, b *checkoutWarmup) bool {
	if a.status.Touched != b.status.Touched {
		return a.status.Touched
	}
	if !a.requested.Equal(b.requested) {
		return a.requested.Before(b.requested)
	}
	return a.loadDir < b.loadDir
}

// yieldToForeground watches a running listing's attempt and cancels it as
// soon as foreground work appears, exactly as a compiler load preempts it;
// the warm-up then waits for an idle daemon again. It returns when the
// attempt ends.
func (p *Provider) yieldToForeground(ctx context.Context, e *checkoutWarmup, activity semantic.ForegroundActivity) {
	r := &p.warm
	ticker := time.NewTicker(r.pollInterval())
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		busy, _ := activity.ForegroundWork()
		if busy == "" {
			continue
		}
		r.mu.Lock()
		r.lastBusy = time.Now()
		if r.active == e && e.cancel != nil && ctx.Err() == nil {
			e.cancel()
			e.cancel = nil
			e.status.Preemptions++
			e.status.State = warmupPreempted
			r.notifyLocked()
			e.status.DeferredBy = busy
		}
		r.mu.Unlock()
		return
	}
}

// releaseWarmupSlot gives the slot back after e's attempt.
func (r *warmupRegistry) releaseWarmupSlot(e *checkoutWarmup) {
	r.mu.Lock()
	if r.active == e {
		r.active = nil
	}
	r.mu.Unlock()
}

// runWarmup lists the module, one batch of packages at a time, until every
// package is merged for the wanted manifests, yielding to every interactive
// load between and during batches.
func (p *Provider) runWarmup(e *checkoutWarmup) {
	r := &p.warm
	defer func() {
		r.mu.Lock()
		e.running = false
		e.waiting = false
		e.cancel = nil
		e.pending = nil
		e.inflight = nil
		r.notifyLocked()
		if r.active == e {
			r.active = nil
		}
		if r.done != nil {
			close(r.done)
			r.done = nil
		}
		r.mu.Unlock()
	}()
	var (
		listed   time.Duration
		packages int
		merged   int
		batches  int
		first    []string
	)
	for {
		r.mu.Lock()
		digest := e.want
		exhausted := e.status.Preemptions >= maxWarmupAttempts
		if exhausted {
			e.status.State = warmupFailed
			e.status.Error = "attempts exhausted"
			e.status.Finished = time.Now()
		}
		r.mu.Unlock()
		if exhausted {
			return
		}
		if e.planned && !p.nextBatchTargeted(e) && !p.pauseForAnswerPath(e, "listing") {
			r.mu.Lock()
			e.status.State = warmupCancelled
			r.mu.Unlock()
			return
		}
		ctx, cancel := p.waitWarmupTurn(e)
		if ctx == nil {
			r.mu.Lock()
			e.status.State = warmupCancelled
			r.mu.Unlock()
			return
		}
		if !e.planned {
			// While the module is enumerated, passes over dirty packages
			// already wait (bounded) for their listing, unless the
			// package's closure is small (waitTargetedListing sizes it on
			// the pass's side: nothing here delays the first listing).
			dirty := dirtyGoDirs(ctx, e.loadDir)
			r.mu.Lock()
			e.pending, e.closureFiles = map[string]bool{}, map[string]int{}
			for dir := range dirty {
				e.pending[dir] = true
			}
			r.notifyLocked()
			r.mu.Unlock()
			queue, targetDirs, err := p.planWarmupTargets(ctx, e.loadDir, dirty)
			targeted := len(targetDirs)
			preempted := ctx.Err() != nil
			cancel()
			r.releaseWarmupSlot(e)
			if p.warmupClosed(e) {
				return
			}
			if preempted {
				continue
			}
			if err != nil {
				if p.warmupFailed(e, err) {
					return
				}
				continue
			}
			r.mu.Lock()
			e.queue, e.planned, e.targeted = queue, true, targeted
			e.targetDirs = targetDirs
			e.pending = map[string]bool{}
			for _, dir := range targetDirs {
				e.pending[dir] = true
			}
			r.notifyLocked()
			e.status.Remaining = len(queue)
			r.mu.Unlock()
			continue
		}
		st := p.typecheckState(e.loadDir, digest)
		r.mu.Lock()
		queue := append([]string(nil), e.queue...)
		r.mu.Unlock()
		size := warmupBatchPackages
		r.mu.Lock()
		if r.batchSize > 0 {
			size = r.batchSize
		}
		targetedBatch := e.targeted > 0
		if targetedBatch {
			// A package a delta is about to hit: list it alone, so it
			// is retained as soon as its own closure is listed.
			size = 1
			e.targeted--
		}
		r.mu.Unlock()
		batch, queue := st.nextWarmupBatch(queue, size)
		r.mu.Lock()
		e.queue = queue
		remaining := len(queue)
		e.status.Remaining = remaining
		r.mu.Unlock()
		if len(batch) == 0 {
			cancel()
			r.releaseWarmupSlot(e)
			break
		}
		r.mu.Lock()
		e.status.Attempts++
		r.mu.Unlock()
		if p.logger != nil {
			r.mu.Lock()
			status := e.status
			r.mu.Unlock()
			p.logger.Info("go-types: checkout warm-up listing started",
				zap.String("module_dir", e.loadDir),
				zap.Int("attempt", status.Attempts),
				zap.Int("preemptions", status.Preemptions),
				zap.Bool("touched", status.Touched),
				zap.Bool("forced", status.Forced),
				zap.Int("batch", len(batch)),
				zap.Bool("targeted", targetedBatch),
				zap.Strings("packages", firstPaths(batch, 4)),
				zap.Int("remaining", remaining),
				zap.Duration("since_request", time.Since(status.Started)))
		}
		flags := warmupBuildFlags()
		r.mu.Lock()
		escalated := e.escalate
		r.mu.Unlock()
		if targetedBatch || escalated {
			flags = targetedWarmupBuildFlags()
		}
		if targetedBatch {
			r.mu.Lock()
			e.inflight = map[string]bool{}
			for _, path := range batch {
				e.inflight[e.targetDirs[path]] = true
			}
			r.notifyLocked()
			r.mu.Unlock()
		}
		listCtx, stalled := p.backgroundListingDeadline(ctx, !targetedBatch && !escalated)
		listing, err := p.runListing(listCtx, e.loadDir, batch, flags)
		preempted := ctx.Err() != nil
		stall := stalled() && !preempted
		cancel()
		if stall {
			// A background listing that has not finished within the stall
			// deadline is retried at normal priority: under sustained load the
			// lowest QoS can starve a compile indefinitely (live: one compile
			// at 0.7 % CPU for 56 minutes). The go build cache keeps what it
			// compiled, and the batch stays queued.
			r.releaseWarmupSlot(e)
			if p.warmupClosed(e) {
				return
			}
			r.mu.Lock()
			e.escalate = true
			e.status.Escalations++
			r.notifyLocked()
			r.mu.Unlock()
			if p.logger != nil {
				p.logger.Info("go-types: checkout warm-up listing stalled at background priority; retrying at normal priority",
					zap.String("module_dir", e.loadDir),
					zap.Int("batch", len(batch)),
					zap.Duration("deadline", r.stallPeriod()))
			}
			continue
		}
		if preempted || err != nil {
			// Passes adopting this listing list themselves now.
			r.mu.Lock()
			e.inflight = nil
			r.notifyLocked()
			r.mu.Unlock()
		}
		r.releaseWarmupSlot(e)
		if p.warmupClosed(e) {
			return
		}
		if preempted {
			// The batch stays queued; the go build cache keeps the export
			// data this attempt built.
			continue
		}
		if err != nil {
			if p.warmupFailed(e, err) {
				return
			}
			continue
		}
		// A module batch's merge waits out answer-path work too; the
		// manifest check below then sees the files as they are after it.
		if !targetedBatch && !p.pauseForAnswerPath(e, "merge") {
			r.mu.Lock()
			e.status.State = warmupCancelled
			r.mu.Unlock()
			return
		}
		if now := goManifestDigest(e.loadDir); now != digest {
			r.mu.Lock()
			e.want = now
			e.planned, e.queue = false, nil
			e.inflight = nil
			e.status.State = warmupSuperseded
			r.notifyLocked()
			r.mu.Unlock()
			listed, packages, merged, batches, first = 0, 0, 0, 0, nil
			continue
		}
		listing.warmup = true
		if escalated {
			// The stalled batch listed at normal priority; the rest of the
			// module goes back to the background.
			r.mu.Lock()
			e.escalate = false
			r.mu.Unlock()
		}
		st.mu.Lock()
		taken := st.mergeListing(listing)
		st.mu.Unlock()
		listed += listing.elapsed
		packages += len(listing.pkgs)
		merged += taken
		batches++
		if len(first) < warmupPrecheckPackages {
			first = append(first, batch...)
		}
		if targetedBatch {
			r.mu.Lock()
			for _, path := range batch {
				delete(e.pending, e.targetDirs[path])
			}
			e.inflight = nil
			r.notifyLocked()
			r.mu.Unlock()
			// The package a delta is about to hit: parse it and read its
			// direct imports now, so its first pass only type-checks.
			p.precheckWarmPackages(st, batch)
		}
		if p.logger != nil && targetedBatch {
			p.logger.Info("go-types: checkout warm-up targeted package retained",
				zap.String("module_dir", e.loadDir),
				zap.Strings("packages", batch),
				zap.Int("merged", taken),
				zap.Duration("listing", listing.elapsed),
				zap.Duration("since_request", time.Since(e.status.Started)))
		}
		r.mu.Lock()
		e.queue = dropListed(e.queue, batch)
		e.status.Remaining = len(e.queue)
		e.status.Batches = batches
		r.mu.Unlock()
	}

	r.mu.Lock()
	digest := e.want
	r.mu.Unlock()
	// The first handle-rooted pass per manifest digest also walks the
	// module for hand-written line directives; do that walk here.
	p.prewarmLineDirectives(e.loadDir, digest)
	st := p.typecheckState(e.loadDir, digest)
	p.precheckWarmPackages(st, first)
	r.mu.Lock()
	e.state = st
	e.status.Digest = digest
	e.status.Ms = listed.Milliseconds()
	e.status.Packages = packages
	e.status.Merged = merged
	e.status.Batches = batches
	e.status.Finished = time.Now()
	e.status.State = warmupWarm
	e.planned, e.queue = false, nil
	e.deferredSince, e.forced = time.Time{}, false
	again := e.want != digest
	attempts, preemptions, forced := e.status.Attempts, e.status.Preemptions, e.status.Forced
	if listed > 0 {
		st.warmList = listed
	}
	r.mu.Unlock()
	if p.logger != nil {
		p.logger.Info("go-types: checkout warm-up listing merged",
			zap.String("module_dir", e.loadDir),
			zap.Int("attempts", attempts),
			zap.Int("preemptions", preemptions),
			zap.Bool("forced", forced),
			zap.Int("batches", batches),
			zap.Int("packages", packages),
			zap.Int("merged", merged),
			zap.Strings("prechecked", first),
			zap.Duration("elapsed", listed))
	}
	if again {
		r.mu.Lock()
		e.running = true
		r.mu.Unlock()
		p.runWarmup(e)
	}
}

// warmupClosed reports (and records) that the provider closed.
func (p *Provider) warmupClosed(e *checkoutWarmup) bool {
	r := &p.warm
	r.mu.Lock()
	defer r.mu.Unlock()
	e.cancel = nil
	if r.ctx.Err() != nil {
		e.status.State = warmupCancelled
		return true
	}
	return false
}

// warmupFailed records a failed listing; it reports whether to give up.
func (p *Provider) warmupFailed(e *checkoutWarmup, err error) bool {
	r := &p.warm
	r.mu.Lock()
	e.failures++
	e.status.Error = err.Error()
	e.status.State = warmupFailed
	e.status.Finished = time.Now()
	giveUp := e.failures >= maxWarmupFailures
	r.mu.Unlock()
	if p.logger != nil {
		p.logger.Warn("go-types: checkout warm-up listing failed",
			zap.String("module_dir", e.loadDir), zap.Error(err))
	}
	return giveUp
}

// planWarmup enumerates the module's packages (names and files only: no
// build, no dependencies) and orders them for listing: the packages with a
// dirty Go file in the checkout's working tree first (smallest first), then
// the checkout's recent handle roots (newest first), then the packages with
// the most files, whose first pass would otherwise pay the longest listing. It
// returns how many packages lead the queue as targeted (dirty or recent;
// at most warmupTargetedPackages), which the warm-up lists one at a time.
func (p *Provider) planWarmup(ctx context.Context, loadDir string) ([]string, int, error) {
	queue, targets, err := p.planWarmupTargets(ctx, loadDir, dirtyGoDirs(ctx, loadDir))
	return queue, len(targets), err
}

// planWarmupTargets is planWarmup with the dirty directories given; it
// returns the targeted packages' directories (symbolic links resolved) by
// package path, in queue order.
func (p *Provider) planWarmupTargets(ctx context.Context, loadDir string, dirty map[string]bool) ([]string, map[string]string, error) {
	cfg := &packages.Config{
		Context: ctx,
		Mode:    packages.NeedName | packages.NeedFiles,
		Dir:     loadDir,
		Tests:   false,
	}
	load := p.packagesLoad
	if load == nil {
		load = packages.Load
	}
	pkgs, err := load(cfg, "./...")
	if err != nil {
		return nil, nil, err
	}
	recent := p.warm.recentRoots(loadDir)
	type entry struct {
		path  string
		dir   string
		dirty bool
		// recent is the root's recency rank; len(recent)+1 when none.
		recent int
		files  int
	}
	var entries []entry
	seen := map[string]bool{}
	for _, pkg := range pkgs {
		if pkg == nil || pkg.PkgPath == "" || seen[pkg.PkgPath] || len(pkg.GoFiles) == 0 {
			continue
		}
		seen[pkg.PkgPath] = true
		dir := filepath.Clean(filepath.Dir(pkg.GoFiles[0]))
		en := entry{path: pkg.PkgPath, dir: resolvedDir(dir), recent: len(recent) + 1, files: len(pkg.GoFiles)}
		en.dirty = dirty[en.dir]
		if i, ok := recent[dir]; ok {
			en.recent = i
		}
		entries = append(entries, en)
	}
	sort.Slice(entries, func(i, j int) bool {
		a, b := entries[i], entries[j]
		if a.dirty != b.dirty {
			return a.dirty
		}
		if a.dirty && a.files != b.files {
			// Dirty packages: the smallest first, so each is retained
			// as soon as its own (usually smaller) closure is listed.
			return a.files < b.files
		}
		if a.recent != b.recent {
			return a.recent < b.recent
		}
		if a.files != b.files {
			return a.files > b.files
		}
		return a.path < b.path
	})
	out := make([]string, len(entries))
	targets := map[string]string{}
	for i, en := range entries {
		out[i] = en.path
		if (en.dirty || en.recent <= len(recent)) && len(targets) < warmupTargetedPackages {
			targets[en.path] = en.dir
		}
	}
	return out, targets, nil
}

// fileImports returns the import paths the files' import clauses name.
func fileImports(files []string) []string {
	var out []string
	seen := map[string]bool{}
	fset := token.NewFileSet()
	for _, name := range files {
		file, err := parser.ParseFile(fset, name, nil, parser.ImportsOnly)
		if err != nil || file == nil {
			continue
		}
		for _, spec := range file.Imports {
			path := strings.Trim(spec.Path.Value, "\"`")
			if !seen[path] {
				seen[path] = true
				out = append(out, path)
			}
		}
	}
	return out
}

// dirtyGoDirs returns the directories holding a modified, added or
// untracked non-test Go file in dir's working tree (git status, without
// taking the index lock); none when dir is not a git working tree.
func dirtyGoDirs(ctx context.Context, dir string) map[string]bool {
	out := map[string]bool{}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	top, err := exec.CommandContext(ctx, "git", "-C", dir, "rev-parse", "--show-toplevel").Output()
	if err != nil {
		return out
	}
	toplevel := strings.TrimSpace(string(top))
	if resolved, err := filepath.EvalSymlinks(toplevel); err == nil {
		toplevel = resolved
	}
	status, err := exec.CommandContext(ctx, "git", "--no-optional-locks", "-C", dir, "status", "--porcelain=v1", "-z", "--untracked-files=all").Output()
	if err != nil {
		return out
	}
	fields := strings.Split(string(status), "\x00")
	for i := 0; i < len(fields); i++ {
		entry := fields[i]
		if len(entry) < 4 {
			continue
		}
		code, rel := entry[:2], entry[3:]
		if code[0] == 'R' || code[0] == 'C' {
			i++ // the rename's or copy's source path follows
		}
		if code[0] == 'D' || code[1] == 'D' {
			continue
		}
		name := filepath.Base(rel)
		if !goSourceName(name) {
			continue
		}
		out[filepath.Clean(filepath.Join(toplevel, filepath.FromSlash(filepath.Dir(rel))))] = true
	}
	return out
}

// resolvedDir is dir with symbolic links resolved (dir when that fails).
func resolvedDir(dir string) string {
	if resolved, err := filepath.EvalSymlinks(dir); err == nil {
		return filepath.Clean(resolved)
	}
	return filepath.Clean(dir)
}

// firstPaths returns at most n paths, for logging.
func firstPaths(paths []string, n int) []string {
	if len(paths) > n {
		return paths[:n]
	}
	return paths
}

// nextWarmupBatch returns the next queued packages the state does not hold
// current metadata for (a pass or an earlier batch may have listed them),
// dropping the held ones from the queue. Metadata listed without export
// data (the package or a dependency did not compile then) is not current.
func (st *checkoutTypecheckState) nextWarmupBatch(queue []string, size int) (batch, rest []string) {
	st.mu.Lock()
	defer st.mu.Unlock()
	for _, path := range queue {
		if len(batch) >= size {
			rest = append(rest, path)
			continue
		}
		if meta := st.meta[path]; meta != nil && !st.exportStale[path] && (meta.ExportFile != "" || !mutablePackage(meta)) {
			continue
		}
		batch = append(batch, path)
	}
	return batch, append(batch[:len(batch):len(batch)], rest...)
}

// dropListed removes the listed paths from the queue.
func dropListed(queue, listed []string) []string {
	done := make(map[string]bool, len(listed))
	for _, path := range listed {
		done[path] = true
	}
	out := queue[:0]
	for _, path := range queue {
		if !done[path] {
			out = append(out, path)
		}
	}
	return out
}

// precheckWarmPackages pre-parses the given packages' files with bodies
// stripped and reads their direct imports from export data into the
// retained state, one package per state lock, stopping as soon as a
// compiler load wants the provider. A first pass over one of them then
// re-parses only its handle files and type-checks.
func (p *Provider) precheckWarmPackages(st *checkoutTypecheckState, paths []string) {
	if len(paths) > warmupPrecheckPackages {
		paths = paths[:warmupPrecheckPackages]
	}
	r := &p.warm
	for _, path := range paths {
		r.mu.Lock()
		busy := r.loads[filepath.Clean(st.loadDir)] > 0 || (r.ctx != nil && r.ctx.Err() != nil)
		r.mu.Unlock()
		if busy {
			return
		}
		st.mu.Lock()
		meta := st.meta[path]
		if meta != nil && !st.exportStale[path] && mutablePackage(meta) {
			scratch := &semantic.CompilerCacheStats{}
			_, _, imports := st.parseRootFiles([]*packages.Package{meta}, nil, true, scratch)
			for _, imp := range imports[path] {
				if imp == "C" || imp == "unsafe" || st.exportStale[imp] {
					continue
				}
				_, _ = st.importDependency(imp, scratch, false)
			}
		}
		st.mu.Unlock()
	}
}

// CheckoutWarmup reports the warm-up of the module directory loadDir (the
// zero status when none was asked for).
func (p *Provider) CheckoutWarmup(loadDir string) CheckoutWarmupStatus {
	r := &p.warm
	r.mu.Lock()
	defer r.mu.Unlock()
	if e := r.entries[loadDir]; e != nil {
		return e.status
	}
	return CheckoutWarmupStatus{}
}

// CheckoutWarmups reports every tracked warm-up, by module directory.
func (p *Provider) CheckoutWarmups() []CheckoutWarmupStatus {
	r := &p.warm
	r.mu.Lock()
	defer r.mu.Unlock()
	dirs := make([]string, 0, len(r.entries))
	for dir := range r.entries {
		dirs = append(dirs, dir)
	}
	sort.Strings(dirs)
	out := make([]CheckoutWarmupStatus, 0, len(dirs))
	for _, dir := range dirs {
		out = append(out, r.entries[dir].status)
	}
	return out
}

// warmupSnapshot copies the checkout's warm-up status onto a pass's cache
// counters.
func (p *Provider) warmupSnapshot(loadDir string, stats *semantic.CompilerCacheStats) {
	status := p.CheckoutWarmup(loadDir)
	stats.WarmupState = status.State
	stats.WarmupMs = status.Ms
	stats.WarmupPackages = status.Packages
	stats.WarmupAttempts = status.Attempts
	stats.WarmupPreemptions = status.Preemptions
}

// waitCheckoutWarmup blocks until the checkout's warm-up is not running or
// ctx ends. Tests and measurement only.
func (p *Provider) waitCheckoutWarmup(ctx context.Context, loadDir string) CheckoutWarmupStatus {
	r := &p.warm
	for {
		r.mu.Lock()
		e := r.entries[loadDir]
		if e == nil || !e.running {
			var status CheckoutWarmupStatus
			if e != nil {
				status = e.status
			}
			r.mu.Unlock()
			return status
		}
		if r.done == nil {
			r.done = make(chan struct{})
		}
		done := r.done
		r.mu.Unlock()
		select {
		case <-ctx.Done():
			return p.CheckoutWarmup(loadDir)
		case <-done:
		}
	}
}

// stopWarmups cancels every warm-up for good (Close).
func (p *Provider) stopWarmups() {
	r := &p.warm
	r.mu.Lock()
	if r.ctx == nil {
		r.ctx, r.stop = context.WithCancel(context.Background())
	}
	stop := r.stop
	r.mu.Unlock()
	stop()
}

// prewarmLineDirectives runs the module walk lineDirectiveDirs does on its
// first call per manifest digest, outside the scope lock (a pass never
// waits for it), and installs the result unless a pass got there first.
func (p *Provider) prewarmLineDirectives(loadDir, digest string) {
	p.scopeMu.Lock()
	done := p.checkoutCacheLocked(loadDir, digest).lineFiles != nil
	p.scopeMu.Unlock()
	if done {
		return
	}
	found := p.scanLineDirectives(loadDir)
	p.scopeMu.Lock()
	if entry := p.checkoutCacheLocked(loadDir, digest); entry.lineFiles == nil {
		entry.lineFiles = found
	}
	p.scopeMu.Unlock()
}

// notifyLocked wakes every pass waiting for a targeted listing; the caller
// holds r.mu.
func (r *warmupRegistry) notifyLocked() {
	if r.progress != nil {
		close(r.progress)
		r.progress = nil
	}
}

// defaultTargetedWait bounds how long a pass waits for its package's
// targeted listing to start (the package is queued behind another targeted
// listing); GORTEX_GOTYPES_TARGETED_WAIT overrides it and 0 disables
// waiting altogether. Once the listing runs the pass adopts it: it waits
// for the merge as long as the warm-up keeps running, and lists itself
// only when the warm-up stops (preempted, failed, superseded, closed).
const defaultTargetedWait = 10 * time.Second

func targetedWaitBound() time.Duration {
	if raw := strings.TrimSpace(os.Getenv("GORTEX_GOTYPES_TARGETED_WAIT")); raw != "" {
		if d, err := time.ParseDuration(raw); err == nil && d >= 0 {
			return d
		}
	}
	return defaultTargetedWait
}

// defaultTargetedWaitMinFiles is the module-internal closure size (Go
// files) below which a pass never waits for a targeted listing: listing
// such a package itself takes about as long at full parallelism as the
// warm-up's quarter-parallelism listing (0.2-1 s measured up to about 100
// files), and waiting only adds the warm-up's queueing and hand-off
// (+0.3-0.8 s measured for gitstate, 19 files). GORTEX_GOTYPES_TARGETED_WAIT_MIN_FILES
// overrides it.
const defaultTargetedWaitMinFiles = 100

func targetedWaitMinFiles() int {
	if raw := strings.TrimSpace(os.Getenv("GORTEX_GOTYPES_TARGETED_WAIT_MIN_FILES")); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n >= 0 {
			return n
		}
	}
	return defaultTargetedWaitMinFiles
}

// waitTargetedListing lets a pass over rootDirs use the checkout's
// targeted warm-up listing of its own package instead of listing the same
// closure a second time. A root whose closure is small never waits for its
// listing to start ("small"). While the package's listing runs, the pass adopts it: it
// waits for the merge with no time bound, as long as the warm-up runs.
// While the package is still queued behind another targeted listing, it
// waits at most bound for its listing to start. It reports how long it
// waited and how the wait ended: "adopted" (the listing it joined merged),
// "merged", "stopped" (the warm-up was preempted, failed or ended first),
// "timeout" (still queued after bound), "small", or "" when there was
// nothing to wait for.
func (p *Provider) waitTargetedListing(ctx context.Context, loadDir string, rootDirs map[string]struct{}, bound time.Duration) (time.Duration, string) {
	if bound <= 0 || len(rootDirs) == 0 {
		return 0, ""
	}
	dirs := make([]string, 0, len(rootDirs))
	for dir := range rootDirs {
		dirs = append(dirs, resolvedDir(dir))
	}
	minFiles := targetedWaitMinFiles()
	r := &p.warm
	start := time.Now()
	// Size the roots' closures (capped at the threshold: a few dozen
	// import clauses) once per targeted directory, on the pass's side.
	r.mu.Lock()
	var unsized []string
	if e := r.entries[loadDir]; e != nil {
		for _, dir := range dirs {
			if _, ok := e.closureFiles[dir]; e.pending[dir] && !ok {
				unsized = append(unsized, dir)
			}
		}
	}
	r.mu.Unlock()
	if len(unsized) > 0 {
		sized := map[string]int{}
		if modPath := goModulePath(loadDir); modPath != "" {
			for _, dir := range unsized {
				sized[dir] = dirClosureFiles(loadDir, modPath, dir, minFiles)
			}
		}
		r.mu.Lock()
		if e := r.entries[loadDir]; e != nil {
			if e.closureFiles == nil {
				e.closureFiles = map[string]int{}
			}
			for dir, files := range sized {
				e.closureFiles[dir] = files
			}
		}
		r.mu.Unlock()
	}
	var (
		deadline *time.Timer
		expired  bool
		adopted  bool
	)
	defer func() {
		if deadline != nil {
			deadline.Stop()
		}
	}()
	for {
		r.mu.Lock()
		e := r.entries[loadDir]
		pending, inflight, small := false, false, true
		if e != nil {
			for _, dir := range dirs {
				if e.pending[dir] {
					pending = true
					if files, ok := e.closureFiles[dir]; !ok || files >= minFiles {
						small = false
					}
				}
				if e.inflight[dir] {
					inflight = true
				}
			}
		}
		running := e != nil && e.running && e.status.State == warmupRunning
		elapsed := time.Since(start)
		switch {
		case !pending:
			r.mu.Unlock()
			if elapsed < time.Millisecond {
				return 0, ""
			}
			if adopted {
				return elapsed, "adopted"
			}
			return elapsed, "merged"
		case small && !inflight:
			// A small package not being listed yet: its own listing is
			// as fast as the warm-up's. One already being listed is
			// adopted like any other (never listed twice).
			r.mu.Unlock()
			return elapsed, "small"
		case !running:
			r.mu.Unlock()
			return elapsed, "stopped"
		case !inflight && expired:
			r.mu.Unlock()
			return elapsed, "timeout"
		}
		if inflight {
			adopted = true
		}
		if r.progress == nil {
			r.progress = make(chan struct{})
		}
		progress := r.progress
		r.mu.Unlock()
		var timeout <-chan time.Time
		if !inflight && !expired {
			if deadline == nil {
				deadline = time.NewTimer(bound)
			}
			timeout = deadline.C
		}
		select {
		case <-progress:
		case <-timeout:
			expired = true
		case <-ctx.Done():
			return time.Since(start), "stopped"
		}
	}
}

// dropPending forgets the targeted directories a pass listed itself: the
// warm-up no longer holds passes back for them.
func (r *warmupRegistry) dropPending(loadDir string, dirs map[string]struct{}) {
	r.mu.Lock()
	defer r.mu.Unlock()
	e := r.entries[loadDir]
	if e == nil || len(e.pending) == 0 {
		return
	}
	for dir := range dirs {
		delete(e.pending, resolvedDir(dir))
	}
	r.notifyLocked()
}

// goModulePath is the module path go.mod in dir declares ("" if none).
func goModulePath(dir string) string {
	data, err := os.ReadFile(filepath.Join(dir, "go.mod"))
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if rest, ok := strings.CutPrefix(line, "module "); ok {
			return strings.Trim(strings.TrimSpace(rest), "\"")
		}
	}
	return ""
}

// dirClosureFiles estimates, before the module is enumerated, the
// module-internal closure size (Go files) of the package in dir: its
// candidate files, and those of every module directory their import
// clauses name, directly or not, stopping at limit. Build constraints are
// not evaluated (an estimate for the wait threshold only).
func dirClosureFiles(loadDir, modPath, dir string, limit int) int {
	seen := map[string]bool{}
	files := 0
	var visit func(string)
	visit = func(d string) {
		if seen[d] || files >= limit {
			return
		}
		seen[d] = true
		names, err := listGoSources(d)
		if err != nil {
			return
		}
		paths := make([]string, len(names))
		for i, name := range names {
			paths[i] = filepath.Join(d, name)
		}
		files += len(names)
		for _, imp := range fileImports(paths) {
			if imp == modPath {
				visit(resolvedDir(loadDir))
			} else if rest, ok := strings.CutPrefix(imp, modPath+"/"); ok {
				visit(resolvedDir(filepath.Join(loadDir, filepath.FromSlash(rest))))
			}
		}
	}
	visit(dir)
	return files
}
