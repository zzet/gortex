package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/contracts"
	"github.com/zzet/gortex/internal/daemon"
	"github.com/zzet/gortex/internal/gitstate"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/intern"
	gortexmcp "github.com/zzet/gortex/internal/mcp"
	"github.com/zzet/gortex/internal/pathkey"
	"github.com/zzet/gortex/internal/platform"
	"github.com/zzet/gortex/internal/progress"
	"github.com/zzet/gortex/internal/semantic/lsp"
	"github.com/zzet/gortex/internal/serverstack"
)

// daemonState is the bundle of long-lived objects the daemon owns. One
// instance per running daemon; every session the daemon accepts shares
// these pointers.
type daemonState struct {
	graph         graph.Store
	indexer       *indexer.Indexer
	multiIndexer  *indexer.MultiIndexer
	configManager *config.ConfigManager
	// lifecycle is the shared owner of checkout lifecycle side effects —
	// track, forget, reload, the periodic sweep. The controller, the MCP
	// tools and the janitor all drive this one instance.
	lifecycle *indexer.CheckoutLifecycle
	mcpServer *gortexmcp.Server
	// readinessFilter composes the legacy/reference warmup phases with the
	// daemon's frozen configured-Git exact-view cohort. nil preserves the
	// single-process and test paths that have no daemon controller.
	readinessFilter func(string, bool, map[string]any) (string, bool, map[string]any)
	// proxyHydrator lazily fills cross-daemon proxy-edge nodes from the
	// owning remote's /v1/subgraph. nil unless federation.edges is on;
	// the read path hydrates a proxy target before traversing it.
	proxyHydrator *daemon.ProxyHydrator
	// MultiWatcher is built by warmupDaemonState (after tracked repos
	// have been re-indexed) and handed to realController via
	// AttachWatcher — it isn't held on daemonState because no caller
	// reads it from here.

	// resolverLSPRegistry composes per-repo ResolverHelpers consulted
	// by the cross-file resolver's hot path. Populated as repos
	// are tracked so each language-server instance is scoped to its owning
	// workspace. nil when the resolve-time LSP path is disabled
	// (GORTEX_LSP_RESOLVER=0) or when semantic enrichment is off.
	resolverLSPRegistry *lsp.ResolverHelperRegistry

	// lspRouter is the daemon-shared LSP server pool. Held here so
	// the warmup loop can register per-repo helpers via
	// ResolverHelperRegistry without re-deriving the router from the
	// semantic manager.
	lspRouter *lsp.Router

	// overlays is the editor-overlay manager, retained so the HTTP
	// handler can share the same instance the MCP server uses.
	overlays *daemon.OverlayManager
	// shared is the constructed server stack; its Close() runs the
	// teardown chain (savings flush, backend close) at daemon shutdown.
	shared *serverstack.SharedServer

	// backendPath is the graph store file this daemon actually opened, with
	// --backend-path already resolved. Published in the daemon's runtime
	// record so out-of-band readers (`gortex repos`) can find the same store
	// instead of assuming the platform default.
	backendPath string
}

// buildDaemonState builds the daemon's stack through the shared
// serverstack constructor and returns the long-lived daemonState the
// warmup loop and controller share.
func buildDaemonState(logger *zap.Logger, observers ...store_sqlite.MigrationObserver) (*daemonState, error) {
	cfg, err := config.Load(cfgFile)
	if err != nil {
		return nil, fmt.Errorf("load config: %w", err)
	}
	gc, _ := config.LoadGlobal()
	// Fold --tools / --tools-mode into mcp.tools (flag overrides config;
	// GORTEX_TOOLS still overrides). Survives --detach because the
	// re-exec'd child re-parses the same flags.
	applyToolPresetFlags(cfg, daemonTools, daemonToolsMode)

	var migrationObserver store_sqlite.MigrationObserver
	if len(observers) > 0 {
		migrationObserver = observers[0]
	}
	ss, err := serverstack.NewSharedServer(serverstack.SharedServerConfig{
		Lifecycle:         serverstack.LifecycleDaemon,
		Backend:           daemonBackend,
		BackendPath:       daemonBackendPath,
		Config:            cfg,
		Global:            gc,
		Logger:            logger,
		Version:           version,
		MigrationObserver: migrationObserver,
		Embedder: serverstack.EmbedderRequest{
			FlagChanged: daemonEmbeddingsChanged,
			FlagEnabled: daemonEmbeddings,
			FlagURL:     daemonEmbeddingsURL,
			FlagModel:   daemonEmbeddingsModel,
		},
		// Workspace-global side-store layout: notes/memories partition
		// under the "daemon" key in the shared DataDir sidecar; the
		// notebook lives in a cache dir (no single repo git tree to
		// anchor to); feedback/combo/frecency stay ephemeral.
		SideStores: serverstack.SideStores{
			NotesDir:     platform.DataDir(),
			NotesRepo:    "daemon",
			NotebookPath: filepath.Join(platform.DataDir(), "notebook-cache"),
		},
	})
	if err != nil {
		return nil, fmt.Errorf("build server stack: %w", err)
	}

	return &daemonState{
		graph:               ss.Graph,
		indexer:             ss.Indexer,
		multiIndexer:        ss.MultiIndexer,
		configManager:       ss.ConfigMgr,
		lifecycle:           ss.CheckoutLifecycle,
		mcpServer:           ss.MCP,
		overlays:            ss.Overlays,
		shared:              ss,
		backendPath:         ss.StorePath,
		resolverLSPRegistry: ss.ResolverLSPRegistry,
		lspRouter:           ss.LSPRouter,
	}, nil
}

const daemonStartupHeartbeatInterval = 2 * time.Second

type daemonStartupReporter struct {
	mu       sync.Mutex
	state    daemon.RuntimeState
	logger   *zap.Logger
	stop     chan struct{}
	done     chan struct{}
	stopOnce sync.Once
}

func newDaemonStartupReporter(logger *zap.Logger) *daemonStartupReporter {
	if logger == nil {
		logger = zap.NewNop()
	}
	now := time.Now().UnixMilli()
	r := &daemonStartupReporter{
		state: daemon.RuntimeState{
			StartupPhase:     daemon.StartupOpeningStore,
			StartupStartedAt: now,
		},
		logger: logger,
		stop:   make(chan struct{}),
		done:   make(chan struct{}),
	}
	r.publishLocked()
	go r.heartbeat()
	return r
}

func (r *daemonStartupReporter) heartbeat() {
	defer close(r.done)
	ticker := time.NewTicker(daemonStartupHeartbeatInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			r.mu.Lock()
			if r.state.StartupPhase == daemon.StartupOpeningStore || r.state.StartupPhase == daemon.StartupMigrating {
				r.publishLocked()
			}
			r.mu.Unlock()
		case <-r.stop:
			return
		}
	}
}

func (r *daemonStartupReporter) ObserveMigration(progress store_sqlite.MigrationProgress) {
	r.mu.Lock()
	defer r.mu.Unlock()

	switch progress.Phase {
	case store_sqlite.MigrationStarted:
		r.state.StartupPhase = daemon.StartupMigrating
		r.state.MigrationVersion = progress.Version
		r.state.MigrationName = progress.Name
		r.state.StartupError = ""
		r.logger.Info("daemon: schema migration starting",
			zap.Int("version", progress.Version), zap.String("name", progress.Name))
	case store_sqlite.MigrationFinished:
		r.logger.Info("daemon: schema migration complete",
			zap.Int("version", progress.Version), zap.String("name", progress.Name),
			zap.Duration("elapsed", progress.Elapsed))
		r.state.StartupPhase = daemon.StartupOpeningStore
		r.state.MigrationVersion = 0
		r.state.MigrationName = ""
	case store_sqlite.MigrationFailed:
		r.logger.Error("daemon: schema migration failed",
			zap.Int("version", progress.Version), zap.String("name", progress.Name),
			zap.Duration("elapsed", progress.Elapsed), zap.Error(progress.Error))
		r.state.StartupPhase = daemon.StartupFailed
		r.state.StartupError = boundedStartupError(progress.Error)
	}
	r.publishLocked()
}

func (r *daemonStartupReporter) Fail(err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.state.StartupPhase = daemon.StartupFailed
	r.state.StartupError = boundedStartupError(err)
	r.publishLocked()
}

func (r *daemonStartupReporter) Serving(backendPath string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.state.BackendPath = backendPath
	r.state.StartupPhase = daemon.StartupServing
	r.state.MigrationVersion = 0
	r.state.MigrationName = ""
	r.state.StartupError = ""
	r.publishLocked()
}

func (r *daemonStartupReporter) Stop() {
	r.stopOnce.Do(func() { close(r.stop) })
	<-r.done
}

func (r *daemonStartupReporter) publishLocked() {
	if err := daemon.WriteRuntimeState(r.state); err != nil && r.logger != nil {
		r.logger.Warn("daemon: could not record startup state", zap.Error(err))
	}
}

func boundedStartupError(err error) string {
	if err == nil {
		return ""
	}
	// Runtime state is consumed out of band and must not become a side
	// channel for SQL text, filesystem paths, DSNs, or credentials embedded in
	// wrapped errors. The daemon log retains the full structured error.
	return "startup failed; check the daemon log"
}

// warmupTimings collects the per-phase costs of one warmupDaemonState run so
// the caller can emit a single summary line instead of reconstructing what a
// restart did from five differently-shaped per-phase log lines. Populated by
// capturing the same phaseStart/time.Since pairs the existing per-phase logs
// already compute — those logs stay untouched; this just also keeps the
// values around instead of discarding them.
type warmupTimings struct {
	parse         time.Duration
	resolve       time.Duration
	enrich        time.Duration
	globalResolve time.Duration
	endBatch      time.Duration
	analysis      time.Duration
	// reposChanged is the number of tracked repos whose reindex actually did
	// work this warmup (cold track, or a reconcile with stale/deleted files).
	reposChanged int
	// filesReindexed sums, across every changed repo, either FileCount (a
	// full retrack) or StaleFileCount+DeletedFileCount (an incremental
	// reconcile).
	filesReindexed int
	// enrichScheduled is the number of repos whose deferred semantic
	// enrichment was actually dispatched, as returned by RunDeferredPassesAll.
	enrichScheduled int
}

// warmupDeltaFrontier carries the exact file/derived frontier out of the
// parallel reconcile pool. Any incomplete result fails closed to the existing
// repo/global pipeline; an ordinary warm delta stays file-scoped end to end.
type warmupDeltaFrontier struct {
	mu    sync.Mutex
	exact bool
	plans map[string]indexer.DerivedInvalidationPlan
}

func newWarmupDeltaFrontier() *warmupDeltaFrontier {
	return &warmupDeltaFrontier{
		exact: true,
		plans: make(map[string]indexer.DerivedInvalidationPlan),
	}
}

func (frontier *warmupDeltaFrontier) invalidate() {
	frontier.mu.Lock()
	frontier.exact = false
	frontier.mu.Unlock()
}

func (frontier *warmupDeltaFrontier) record(result *indexer.IndexResult) {
	frontier.mu.Lock()
	defer frontier.mu.Unlock()
	if result == nil || result.RepoPrefix == "" || result.FullRetrack || len(result.FailedFiles) > 0 {
		frontier.exact = false
		return
	}
	plan := frontier.plans[result.RepoPrefix]
	plan.Merge(result.DerivedInvalidation)
	frontier.plans[result.RepoPrefix] = plan
}

func (frontier *warmupDeltaFrontier) snapshot(changedRepos int, scopeUnknown bool) (map[string]indexer.DerivedInvalidationPlan, []string, bool) {
	frontier.mu.Lock()
	defer frontier.mu.Unlock()
	plans := make(map[string]indexer.DerivedInvalidationPlan, len(frontier.plans))
	fileSet := make(map[string]struct{})
	for prefix, plan := range frontier.plans {
		plans[prefix] = plan
		for _, file := range plan.Files {
			if file != "" {
				fileSet[file] = struct{}{}
			}
		}
	}
	files := make([]string, 0, len(fileSet))
	for file := range fileSet {
		files = append(files, file)
	}
	sort.Strings(files)
	exact := frontier.exact && !scopeUnknown && changedRepos > 0 && len(plans) == changedRepos
	return plans, files, exact
}

// warmupDaemonState performs the per-repo parse loop, resolves references,
// runs the background enrichment, and brings up the MultiWatcher. Split out
// from buildDaemonState so the daemon can open its socket and accept
// connections before this work finishes. markReady is invoked once references
// are resolved and the graph is queryable — ahead of the slow enrichment pass
// — so the daemon reports ready as soon as find_usages / get_callers return
// complete results, not after enrichment finishes. The returned *warmupTimings
// is never nil — daemon.go uses it to emit a one-line warmup summary.
// healDuplicateRepos drops tracked-repo entries that name the same
// directory as an earlier entry under a different path spelling — letter
// case or Unicode normalisation on a case-insensitive filesystem — logs
// each drop, and persists the cleaned config. Returns the number of
// entries removed. It is the startup / load-time repair for configs that
// accumulated a duplicate before folding was applied (#270): a duplicate
// reaching the warmup loop would flip the daemon into multi-repo mode and
// desync the graph. The FIRST (oldest) spelling of each directory is kept.
func healDuplicateRepos(gc *config.GlobalConfig, logger *zap.Logger) int {
	if gc == nil {
		return 0
	}
	removed := gc.DedupeRepos()
	if len(removed) == 0 {
		return 0
	}
	if logger == nil {
		logger = zap.NewNop()
	}
	for _, r := range removed {
		kept := r.Path
		for _, k := range gc.Repos {
			if pathkey.SamePathIdentity(k.Path, r.Path) {
				kept = k.Path
				break
			}
		}
		logger.Warn("dropping duplicate tracked-repo entry (same directory, different path spelling)",
			zap.String("dropped", r.Path),
			zap.String("kept", kept))
	}
	if err := gc.Save(); err != nil {
		logger.Warn("persisting deduped repo config failed", zap.Error(err))
	}
	return len(removed)
}

// drainGoModAndFinalizeWarmupBulk keeps the dependency-contract drain and the
// coordinated bulk finalizer in one explicitly ordered operation. The closures
// make that ordering testable without constructing a daemon or SQLite store.
func drainGoModAndFinalizeWarmupBulk(drain func(), finalize func() error) (drainElapsed, finalizeElapsed time.Duration, err error) {
	drainStart := time.Now()
	if drain != nil {
		drain()
	}
	drainElapsed = time.Since(drainStart)
	if finalize == nil {
		return drainElapsed, 0, nil
	}
	finalizeStart := time.Now()
	err = finalize()
	return drainElapsed, time.Since(finalizeStart), err
}

// rotateColdInternGeneration releases the process-wide canonical-string table
// after a cold producer phase has joined. Returned strings remain valid; the
// SQLite backend has already copied their values into durable rows, so keeping
// the table would only pin transient prefixed IDs and paths for process life.
// Reset does not force a collection — it merely makes the old generation
// eligible for the runtime's ordinary phase GC and scavenging.
func rotateColdInternGeneration(logger *zap.Logger, phase string) int {
	released := intern.Reset()
	if released > 0 && logger != nil {
		logger.Info("daemon: released cold intern generation",
			zap.String("phase", phase),
			zap.Int("strings", released))
	}
	return released
}

// legacyWarmupRepos partitions configured repositories by startup owner.
// Reachable Git checkouts are explicit lifecycle intent: Seed has already
// queued or restored their dedicated promotion, so sending them through the
// legacy generation-0 TrackRepoCtx/ReconcileRepoCtx loop would parse the same
// corpus twice and retain both copies. Non-Git roots keep the historical
// warmup path unchanged. An inaccessible or malformed Git checkout also stays
// on the legacy side so a failed inventory cannot silently suppress its only
// indexing attempt.
func legacyWarmupRepos(ctx context.Context, lifecycle *indexer.CheckoutLifecycle, repos []config.RepoEntry) (legacy []config.RepoEntry, lifecycleManaged int) {
	if lifecycle == nil {
		return repos, 0
	}
	legacy = make([]config.RepoEntry, 0, len(repos))
	for _, entry := range repos {
		if _, err := gitstate.Inventory(ctx, entry.Path); err == nil {
			lifecycleManaged++
			continue
		}
		legacy = append(legacy, entry)
	}
	return legacy, lifecycleManaged
}

func warmupDaemonState(state *daemonState, logger *zap.Logger, markReady func()) (*indexer.MultiWatcher, *warmupTimings) {
	timings := &warmupTimings{}
	if state == nil {
		return nil, timings
	}
	if state.multiIndexer == nil || state.configManager == nil {
		return nil, timings
	}

	ctx := progress.WithReporter(context.Background(), progress.Nop{})
	// BeginParallelBatch / EndBatch tells every per-repo Indexer
	// constructed inside the loop to skip both the graph-wide
	// derivation passes (InferImplements / InferOverrides /
	// markTestSymbolsAndEmitEdges) AND the per-repo cross-cutting
	// passes (ResolveAll / semantic enrich / contract extract+commit).
	// The latter mutate the shared graph in ways that race when
	// goroutines run them concurrently across repos, so the parallel
	// loop below just parses; RunDeferredPassesAll drains the deferred
	// per-repo passes serially before the global resolve. Without this
	// batch wrapper, a 100+ repo warmup is O(R · global_size).
	state.multiIndexer.BeginParallelBatch()

	// Heal any duplicate tracked-repo entries that name the same directory
	// under a different path spelling (case, or Unicode normalisation)
	// BEFORE the warmup loop registers them. Two spellings of one directory
	// would otherwise both reach the indexer and flip the daemon into
	// multi-repo mode, desyncing the unprefixed graph (#270).
	healDuplicateRepos(state.configManager.Global(), logger)

	repos := state.configManager.Global().Repos

	// Purge orphaned repo prefixes BEFORE the warmup loop re-registers the
	// tracked repos, while the store still reflects the prior run's state. A
	// repo removed from config (or untracked before the sidecar-aware purge
	// existed) can leave its nodes+edges AND its fifteen repo_prefix-keyed
	// sidecar tables (file_mtimes, *_enrichment, symbol_fts, content_fts, ...)
	// behind — a long-lived store then carries thousands of rows for a repo
	// gone long ago. Key the tracked repos through the same EffectiveRepoPrefix
	// the warm mtime lookup + LSP-helper registry use (so the comparison
	// matches what the indexer actually wrote), ask the store for any DISTINCT
	// prefix outside that set, and PurgeRepo each. '' is never an orphan
	// (shared global externals / solo data). Capability-probed, so only the
	// sidecar-bearing on-disk store participates; the in-memory store skips it.
	if orphaner, ok := state.graph.(interface {
		OrphanRepoPrefixes(known []string) []string
	}); ok {
		if purger, ok := state.graph.(interface{ PurgeRepo(string) error }); ok {
			known := make([]string, 0, len(repos))
			for _, entry := range repos {
				p := strings.TrimPrefix(indexer.EffectiveRepoPrefix(state.configManager, entry), "/")
				if p != "" {
					known = append(known, p)
				}
			}
			for _, prefix := range orphaner.OrphanRepoPrefixes(known) {
				if err := purger.PurgeRepo(prefix); err != nil {
					logger.Warn("daemon: purging orphaned repo prefix failed",
						zap.String("prefix", prefix), zap.Error(err))
					continue
				}
				logger.Info("daemon: purged orphaned repo prefix (no tracked repo in config keys under it)",
					zap.String("prefix", prefix))
			}
		}
	}

	// One-time store compaction, guarded (see daemon_compact.go). Placed HERE
	// on purpose: after the orphan purge, so the pages it just freed are
	// measured and reclaimed in the same pass; before the warmup re-index loop
	// and the resolver/enrichment passes, whose writes would reuse freelist
	// pages mid-measurement and whose readers would contend with VACUUM's
	// exclusive lock. The socket is already listening (it opens before this
	// goroutine by design), but at this point in boot the daemon's own
	// workload is quiet and a rare early client query either finishes first or
	// makes the VACUUM fail cleanly at busy_timeout — a skip, not a fault.
	// Running before the socket opened instead would hold the daemon
	// unreachable for the whole VACUUM, turning a compacting boot into an
	// apparent hang.
	maybeCompactStore(state.graph, logger)

	// Register a per-repo resolver-time LSP helper for every
	// tracked repo BEFORE the parallel warmup loop fires. The
	// helpers are lazy: language servers are not spawned until the
	// resolver asks for a supported edge resolution, so there's no
	// startup cost for repos with no matching code.
	if state.resolverLSPRegistry != nil && state.lspRouter != nil {
		poolSize := lsp.ResolverPoolSizeFromEnv(1)
		registered, skipped, tsRepos, pythonRepos := 0, 0, 0, 0
		for _, entry := range repos {
			absRoot, err := filepath.Abs(entry.Path)
			if err != nil {
				continue
			}
			helper, specs := serverstack.BuildResolverLSPHelperForRepo(state.lspRouter, absRoot, poolSize, logger)
			if helper == nil {
				skipped++
				continue
			}
			prefix := strings.TrimPrefix(indexer.EffectiveRepoPrefix(state.configManager, entry), "/")
			state.resolverLSPRegistry.Register(prefix, helper)
			registered++
			for _, spec := range specs {
				switch spec {
				case "typescript-language-server", "tsgo":
					tsRepos++
				case "pyright", "pyrefly":
					pythonRepos++
				}
			}
		}
		logger.Info("daemon: resolve-time LSP helpers registered",
			zap.Int("repos", registered),
			zap.Int("skipped", skipped),
			zap.Int("ts_repos", tsRepos),
			zap.Int("python_repos", pythonRepos),
			zap.Int("pool_size", poolSize))
	}
	configuredRepos := len(repos)
	var lifecycleManaged int
	repos, lifecycleManaged = legacyWarmupRepos(ctx, state.lifecycle, repos)
	logger.Info("daemon: warmup ownership partition",
		zap.Int("configured_repos", configuredRepos),
		zap.Int("lifecycle_managed_git", lifecycleManaged),
		zap.Int("legacy_jobs", len(repos)))

	// Bounded worker pool — disk I/O dominates parsing for most repos,
	// but a few CPU-heavy ones overlap with disk waits on others. NumCPU
	// gives good throughput on local SSDs without thrashing slow
	// external mounts (which dominate at this scale). Capped so a 32-core
	// box doesn't over-subscribe a single spinning drive.
	workers := runtime.NumCPU()
	if workers < 2 {
		workers = 2
	}
	if workers > 12 {
		workers = 12
	}
	if workers > len(repos) {
		workers = len(repos)
	}
	parseBudget, currentMemory, budgetErr := currentWarmupMemoryBudgetBytes()
	state.multiIndexer.SetSharedParseMemoryBudget(parseBudget)
	if budgetErr != nil {
		logger.Warn("daemon: invalid warmup memory budget; using automatic budget",
			zap.String("env", warmupMemoryBudgetEnv),
			zap.String("value", os.Getenv(warmupMemoryBudgetEnv)),
			zap.Error(budgetErr))
	}
	logger.Info("daemon: warmup phase start",
		zap.String("phase", "parallel_parse"),
		zap.Int("repos", len(repos)),
		zap.Int("workers", workers),
		zap.Int64("shared_parse_budget_mb", parseBudget>>20),
		zap.Uint64("managed_memory_mb", currentMemory>>20))
	publishReadinessPhase(state, "parallel_parse", false, map[string]any{
		"tracked_repos":          len(repos),
		"workers":                workers,
		"shared_parse_budget_mb": parseBudget >> 20,
		"managed_memory_mb":      currentMemory >> 20,
	})
	phaseStart := time.Now()

	jobs := make(chan config.RepoEntry, len(repos))
	var wg sync.WaitGroup
	// changedRepos counts repos that actually did indexing work this
	// warmup: a cold full-track, or a reconcile that re-indexed / evicted
	// at least one file. When it stays zero, NOTHING on disk changed
	// since the last shutdown, so the persisted graph already holds every
	// resolved and derived edge — the global resolution passes below
	// (RunDeferredPassesAll / RunGlobalResolve / RunGlobalGraphPasses) are
	// pure recomputation and get skipped, which is what makes a true warm
	// restart near-instant instead of replaying the full cold-warmup cost.
	var changedRepos atomic.Int64
	// filesReindexed sums the actual file-level work behind changedRepos: a
	// full retrack counts its whole FileCount, an incremental reconcile
	// counts only the files it touched (stale + deleted). Read once into
	// timings.filesReindexed after wg.Wait() below.
	var filesReindexed atomic.Int64
	// changedPrefixes records the repo prefix of every repo that did indexing
	// work, so the end-of-warmup RunGlobalGraphPasses can scope the per-repo
	// clone detection + Rebuild to just those repos instead of every tracked
	// repo. scopeUnknown trips when a changed repo's prefix can't be
	// determined (e.g. a failed reconcile) — then the scope is dropped and the
	// whole-workspace clone pass runs, degrading toward correctness.
	var changedPrefixes sync.Map
	var scopeUnknown atomic.Bool
	deltaFrontier := newWarmupDeltaFrontier()
	if err := state.multiIndexer.RunRepositoryTopologyBatch(ctx, func(batchCtx context.Context) error {
		coordinatedBulk, _ := state.graph.(graph.CoordinatedBulkLoader)
		coordinatedBulkActive := coordinatedBulk != nil && coordinatedBulk.BeginCoordinatedBulkLoad()
		defer func() {
			if coordinatedBulkActive {
				if err := coordinatedBulk.EndCoordinatedBulkLoad(); err != nil {
					logger.Error("daemon: coordinated cold bulk-load cleanup failed", zap.Error(err))
				}
			}
		}()
		for i := 0; i < workers; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for entry := range jobs {
					// Per-entry panic guard so one repo's crash during
					// reindex doesn't kill the worker — the bad repo logs
					// and skips, the worker proceeds to the next job, and
					// warmup completes.
					func(entry config.RepoEntry) {
						defer func() {
							if r := recover(); r != nil {
								logger.Error("daemon: warmup repo panic recovered",
									zap.String("path", entry.Path),
									zap.Any("panic", r))
								changedRepos.Add(1)
								scopeUnknown.Store(true)
								deltaFrontier.invalidate()
							}
						}()
						// Route repos the store already knows through
						// ReconcileRepoCtx — it calls IncrementalReindexPaths, which
						// evicts files deleted while the daemon was down and
						// re-indexes only files whose mtime changed. Repos with no
						// persisted mtimes (newly tracked, or first startup after a
						// schema bump) fall back to TrackRepoCtx, which does a
						// full walk. Both paths end with the repo registered on
						// the MultiIndexer; contract reconciliation is deferred
						// to the single RunGlobalResolve call below.
						repoStart := time.Now()
						// The backend's FileMtime sidecar table is the sole source
						// of prior mtimes. An empty result means the store has
						// never seen this repo, which is a cold index rather than
						// a reconcile with nothing to do — normalise it to nil so
						// the full-walk branch below runs.
						priorMtimes := priorMtimesFromStore(state.graph, state.configManager, entry, logger)
						if len(priorMtimes) == 0 {
							priorMtimes = nil
						}
						// A backend that crossed a schema-rebuild migration rung
						// (NeedsRebuild) has on-disk rows in the old shape that an
						// incremental reconcile cannot fix. Drop prior mtimes so every
						// file re-indexes into the new schema (the nil branch below
						// runs a full TrackRepoCtx and marks the repo changed, so the
						// global resolve/derivation passes re-run too). No-op for
						// backends without the capability and whenever no rebuild rung
						// was crossed — the common case.
						if storeNeedsRebuild(state.graph) {
							if len(priorMtimes) > 0 {
								logger.Info("daemon: backend signalled schema rebuild; forcing full re-index",
									zap.String("path", entry.Path))
							}
							priorMtimes = nil
						}
						pathFn := "track"
						if priorMtimes != nil {
							pathFn = "reconcile"
							res, err := state.multiIndexer.ReconcileRepoCtx(batchCtx, entry, priorMtimes)
							switch {
							case err != nil:
								logger.Warn("daemon: startup reconcile failed",
									zap.String("path", entry.Path), zap.Error(err))
								// Treat a failed reconcile as "changed" so the global
								// passes still run — degrade toward correctness, not
								// toward the fast path, when we can't trust the delta.
								changedRepos.Add(1)
								scopeUnknown.Store(true)
								deltaFrontier.invalidate()
							case res != nil && (res.StaleFileCount > 0 || res.DeletedFileCount > 0 || len(res.FailedFiles) > 0 || res.FullRetrack):
								changedRepos.Add(1)
								filesReindexed.Add(int64(reconcileFileCount(res)))
								deltaFrontier.record(res)
								if res.RepoPrefix != "" {
									changedPrefixes.Store(res.RepoPrefix, struct{}{})
								} else {
									scopeUnknown.Store(true)
								}
							default:
								// Warm no-op path: the repo re-indexed nothing, so its
								// graph is served straight from the persisted store and
								// no global pass has to re-run for it.
							}
						} else {
							// No prior mtimes → full cold (re)index of this repo,
							// which is "changed" by definition.
							deltaFrontier.invalidate()
							changedRepos.Add(1)
							if res, err := state.multiIndexer.TrackRepoCtx(batchCtx, entry); err != nil {
								logger.Warn("daemon: startup track failed",
									zap.String("path", entry.Path), zap.Error(err))
								scopeUnknown.Store(true)
							} else if res != nil && res.RepoPrefix != "" {
								// A cold TrackRepoCtx is itself a full retrack — its
								// FileCount is the whole repo's file-level work.
								filesReindexed.Add(int64(res.FileCount))
								changedPrefixes.Store(res.RepoPrefix, struct{}{})
							} else {
								scopeUnknown.Store(true)
							}
						}
						elapsed := time.Since(repoStart)
						if elapsed > 2*time.Second {
							logger.Info("daemon: warmup repo elapsed",
								zap.String("path", entry.Path),
								zap.String("path_fn", pathFn),
								zap.Duration("elapsed", elapsed))
						}
					}(entry)
				}
			}()
		}
		for _, entry := range repos {
			jobs <- entry
		}
		close(jobs)
		wg.Wait()

		// Materialize go.mod dependency contract nodes while the coordinated
		// bulk connection is still pinned. The per-indexer drain is idempotent,
		// so RunPreEnrichResolve retains the same fallback for non-warmup paths
		// and becomes a cheap no-op here. Keeping these writes ahead of the
		// final seal includes them in bulk index creation, checkpointing, and
		// planner statistics instead of reopening ordinary write transactions
		// immediately after a large cold load.
		var finalizeBulk func() error
		if coordinatedBulkActive {
			finalizeBulk = coordinatedBulk.EndCoordinatedBulkLoad
		}
		goModElapsed, flushElapsed, finalizeErr := drainGoModAndFinalizeWarmupBulk(
			state.multiIndexer.RunDeferredGoModAll,
			finalizeBulk,
		)
		logger.Info("daemon: warmup pre-bulk go.mod drain complete",
			zap.Duration("elapsed", goModElapsed))

		if coordinatedBulkActive {
			if finalizeErr != nil {
				logger.Error("daemon: coordinated cold bulk-load finalize failed", zap.Error(finalizeErr))
			} else {
				logger.Info("daemon: coordinated cold bulk-load complete",
					zap.Duration("elapsed", flushElapsed))
			}
			coordinatedBulkActive = false
		}
		return nil
	}); err != nil {
		logger.Error("daemon: repository topology batch failed", zap.Error(err))
	}
	// RunRepositoryTopologyBatch has joined every parse worker and finalized
	// the coordinated bulk loader. No producer still needs the canonical
	// prefixed strings retained by the cold interner generation.
	rotateColdInternGeneration(logger, "parallel_parse")
	timings.parse = time.Since(phaseStart)
	timings.filesReindexed = int(filesReindexed.Load())
	logger.Info("daemon: warmup phase done",
		zap.String("phase", "parallel_parse"),
		zap.Duration("elapsed", time.Since(phaseStart)))
	parseStats := progress.Stats(string(progress.PhaseParse), phaseStart, len(repos), len(repos))
	publishReadinessPhase(state, "parallel_parse_done", false, map[string]any{
		"tracked_repos": len(repos),
		"elapsed_ms":    time.Since(phaseStart).Milliseconds(),
		"elapsed_human": parseStats.Elapsed,
		"repos_per_sec": parseStats.ItemsPerSec,
	})

	// Warm-restart fast path. When the reconcile loop above re-indexed
	// nothing, the persistent backend already carries every resolved and
	// derived edge from the prior run; the deferred per-repo passes, the
	// cross-repo resolve, and the graph-wide derivation passes would all
	// just recompute what's on disk. Skipping them is what turns a warm
	// restart from a multi-minute replay of the cold-warmup cost into a
	// near-instant "open store, reconcile zero files, start watching".
	// The in-memory backend reaches here too, but its snapshot replay
	// already restored the derived edges, so the skip is equally safe.
	anyChanged := changedRepos.Load() > 0
	timings.reposChanged = int(changedRepos.Load())
	logger.Info("daemon: warmup change detection",
		zap.Int64("changed_repos", changedRepos.Load()),
		zap.Int("tracked_repos", len(repos)),
		zap.Bool("global_passes", anyChanged))

	// Materialize the changed-repo prefix set once. It feeds two consumers:
	// the warm-restart resolve scope (RunPreEnrichResolve) and the end-batch
	// clone-pass scope (ArmBatchScope). Building it once here avoids two
	// separate walks of the sync.Map.
	changed := make(map[string]struct{})
	changedPrefixes.Range(func(k, _ any) bool {
		if p, ok := k.(string); ok {
			changed[p] = struct{}{}
		}
		return true
	})
	needsRebuild := storeNeedsRebuild(state.graph)
	forcedFullResolve := warmupFullResolveForced()
	deltaPlans, deltaFiles, exactWarmDelta := deltaFrontier.snapshot(
		int(changedRepos.Load()), scopeUnknown.Load(),
	)
	// The exact file frontier must not bypass lifecycle-wide safety signals or
	// the operator override. These conditions already force a full master
	// scope below; apply them to the branch selector too.
	exactWarmDelta = exactWarmDelta && len(deltaFiles) > 0 && !needsRebuild && !forcedFullResolve
	logger.Info("daemon: warmup delta frontier",
		zap.Bool("exact", exactWarmDelta),
		zap.Bool("forced_full_resolve", forcedFullResolve),
		zap.Int("repos", len(deltaPlans)),
		zap.Int("files", len(deltaFiles)))

	// Resolve scope: restrict the warm-restart master resolve to the repos
	// that re-indexed, but only when every whole-graph-safety precondition
	// holds (see warmupResolveScope). Any uncertainty drops to a nil scope ==
	// whole-graph resolve, exactly the pre-scoping behaviour. The scoped
	// global passes switch is honoured downstream in runMasterResolve,
	// matching ArmBatchScope.
	resolveScope := warmupResolveScope(changed, len(repos), anyChanged,
		scopeUnknown.Load(), needsRebuild)

	// Resume enrichment for any repo a prior process left partial / abandoned,
	// or that indexed files under a deferred-enrichment window it never closed.
	// Seeded BEFORE the resolve phase so the overlapped enrichment pool below
	// covers those repos too. Cheap for a fully-enriched workspace: each
	// already-complete repo pays a git rev-parse, one marker lookup, and one
	// repo-scoped file-node projection — bounded by file count, not symbols.
	enrichPending := state.multiIndexer.SeedPendingEnrichAll()
	if enrichPending > 0 && !anyChanged {
		logger.Info("daemon: warmup resuming incomplete enrichment on an otherwise-unchanged restart",
			zap.Int("repos_pending_enrich", enrichPending))
	}

	// Keep enrichment sequential by default. The resolver already saturates
	// the host on a cold workspace, while overlapped Go enrichment can retain
	// complete compiler programs behind this apply gate for the duration of
	// resolution. That measured as several GiB of extra footprint for at most
	// a few seconds of useful compute overlap. Operators can explicitly opt in
	// with GORTEX_ENRICH_OVERLAP=1 while comparing unusual workloads.
	var deferredRun *indexer.DeferredPassesRun
	openApplyGate := func() {}
	if (anyChanged || enrichPending > 0) && enrichmentOverlapEnabled() {
		applyGate := make(chan struct{})
		openApplyGate = sync.OnceFunc(func() {
			// Open the mutation receipt only after pre-enrichment resolution has
			// settled and strictly before any parked semantic apply can run. This
			// excludes resolver writes that used to void every overlap receipt,
			// while retaining exact coverage of applies and contract commits.
			deferredRun.BeginApplyMutationReceipt()
			logger.Info("daemon: enrichment apply gate opened")
			close(applyGate)
		})
		// FinishTail joins pool lanes that may be parked on the gate: the
		// gate MUST open on every path that reaches it, including panics.
		defer openApplyGate()
		deferredRun = state.multiIndexer.BeginDeferredPasses(ctx, applyGate)
		logger.Info("daemon: deferred enrichment pool started ahead of resolve")
	}

	// Resolve references ahead of the slow enrichment pass so find_usages /
	// get_callers return complete results as soon as the daemon reports ready
	// — independent of semantic enrichment. RunPreEnrichResolve materialises
	// go.mod dep nodes, runs the same-repo master resolver, and runs the
	// cross-repo resolver. On the warm-restart fast path nothing changed, so
	// the persisted graph already carries resolved edges and we skip straight
	// to marking ready.
	resolveOK := true
	if anyChanged {
		phaseStart = time.Now()
		publishStart := time.Now()
		publishReadinessPhase(state, "resolve", false, nil)
		logger.Info("daemon: resolve readiness phase published",
			zap.Duration("elapsed", time.Since(publishStart)))
		// markReady fires at the master resolver's compute-done point — the
		// earliest moment same-repo references are queryable — instead of
		// after the refinement tail + cross-repo pass (minutes on a large
		// workspace; they only refine confidence and bind cross-repo edges,
		// and keep running here after ready). The apply gate stays closed
		// for the whole phase: an apply holds the ResolveMutex in
		// multi-minute stretches, and admitting applies between the master
		// and cross-repo passes starved cross-repo to a standstill
		// (measured: 1,049s for a pass that runs in ~38s uncontended). The
		// gate opens right after this call returns.
		var resolveErr error
		if exactWarmDelta {
			resolveErr = state.multiIndexer.RunPreEnrichResolveFiles(ctx, deltaFiles, markReady)
		} else {
			resolveErr = state.multiIndexer.RunPreEnrichResolve(ctx, resolveScope, markReady)
		}
		if resolveErr != nil {
			resolveOK = false
			exactWarmDelta = false
			scopeUnknown.Store(true)
			logger.Error("daemon: warmup resolve failed", zap.Error(resolveErr))
		}
		timings.resolve = time.Since(phaseStart)
		logger.Info("daemon: warmup phase done",
			zap.String("phase", "resolve"),
			zap.Duration("elapsed", time.Since(phaseStart)))
		publishReadinessPhase(state, "resolve_done", false, map[string]any{
			"elapsed_ms": time.Since(phaseStart).Milliseconds(),
		})
	}

	// The resolve phase is over: parked enrichment applies may now take the
	// ResolveMutex without contending the warmup resolver.
	openApplyGate()

	// References are resolved (or unchanged and already resolved on the warm
	// path): the graph is queryable. Flip ready before the multi-minute
	// enrichment so clients can start issuing queries immediately. Everything
	// below runs in the background after ready and finishes at MarkEnriched.
	if markReady != nil && resolveOK {
		markReady()
	}

	// Drain deferred per-repo passes (semantic enrich / contract
	// extract+commit). These finish after ready: enrichment is a precision
	// upgrade on top of the already-queryable reference graph. The tail
	// re-runs the master resolver to lift placeholder edges the enrichment +
	// contract passes add. When the pool was started ahead of resolve, this
	// block only joins the surviving lanes and runs the tail.
	var deferredResult indexer.DeferredPassesResult
	if anyChanged || enrichPending > 0 {
		phaseStart = time.Now()
		publishReadinessPhase(state, "deferred_passes_all", true, nil)
		if deferredRun != nil {
			deferredResult = deferredRun.FinishTailResult()
		} else {
			deferredResult = state.multiIndexer.RunDeferredPassesAllResult(ctx)
		}
		// Deferred module/contract extraction can mint a small second wave of
		// prefixed strings after the main parse generation was released. Every
		// deferred lane is joined here, so rotate that tail generation too.
		rotateColdInternGeneration(logger, "deferred_passes_all")
		timings.enrichScheduled = deferredResult.EnrichScheduled
		timings.enrich = time.Since(phaseStart)
		logger.Info("daemon: warmup phase done",
			zap.String("phase", "deferred_passes_all"),
			zap.Duration("elapsed", time.Since(phaseStart)))
		publishReadinessPhase(state, "deferred_passes_all_done", true, map[string]any{
			"elapsed_ms": time.Since(phaseStart).Milliseconds(),
		})
	}

	// Rehydrate per-repo contract registries from the graph. Only
	// target indexers whose registry is still nil — a non-nil registry
	// means IncrementalReindexPaths (or a fresh TrackRepoCtx) re-extracted
	// contracts from source, and that result is authoritative. Without
	// this, every steady-state repo's ContractRegistry stays nil and
	// MergedContractRegistry skips them, so `contracts` returns only
	// the contracts of repos whose files happened to change since the
	// last shutdown.
	{
		phaseStart = time.Now()
		injectedRepos, injectedCount := 0, 0
		for prefix := range state.multiIndexer.AllMetadata() {
			idx := state.multiIndexer.GetIndexer(prefix)
			if idx == nil || idx.ContractRegistry() != nil {
				continue
			}
			// The indexer stamps every contract record onto Node.Meta at
			// commit time, so the KindContract nodes already in the store
			// are the authoritative source.
			reg := contracts.LoadRegistryFromGraph(state.graph, prefix)
			if reg == nil {
				continue
			}
			idx.SetContractRegistry(reg)
			injectedRepos++
			injectedCount += len(reg.All())
		}
		if injectedRepos > 0 {
			logger.Info("daemon: rehydrated contract registries from graph",
				zap.Int("repos", injectedRepos),
				zap.Int("contracts", injectedCount),
				zap.Duration("elapsed", time.Since(phaseStart)))
		}
	}
	// Backfill `WorkspaceID` / `ProjectID` onto nodes and contracts that
	// predate those fields — they persist as empty, and without this stamp
	// the matcher's EffectiveWorkspace falls back to RepoPrefix, so explicit
	// shared-workspace declarations stop working until every file is
	// touched. Idempotent — re-running on a stamped graph is a no-op.
	phaseStart = time.Now()
	backfillRevisionBefore, backfillRevisionBeforeKnown := state.multiIndexer.GraphMutationRevision()
	backfilledNodes, backfilledContracts, backfillResolutionAffected := state.multiIndexer.BackfillWorkspaceSlugsWithImpact()
	backfillRevisionAfter, backfillRevisionAfterKnown := state.multiIndexer.GraphMutationRevision()
	if backfilledNodes+backfilledContracts > 0 {
		logger.Info("daemon: backfilled workspace/project slugs from .gortex.yaml",
			zap.Int("nodes", backfilledNodes),
			zap.Int("contracts", backfilledContracts),
			zap.Int("resolution_affected", backfillResolutionAffected),
			zap.Duration("elapsed", time.Since(phaseStart)))
	}

	// Run a cross-repo resolution pass once warmup has stamped the workspace
	// slugs. The deferred tail may have completed catch-up either from an exact
	// receipt frontier or through its fail-closed full fallback. In both cases
	// the later full sweep is redundant only while the graph remains at the
	// revision captured at completion. Unsupported revisions, an interleaving
	// writer outside the bounded backfill, a failed pass, or a slug stamp that
	// changes effective workspace eligibility retain the historical full safety
	// sweep; physical-only stamps are rebased and contract bridges reconcile
	// separately.
	currentMutationRevision, currentMutationRevisionKnown := state.multiIndexer.GraphMutationRevision()
	globalResolveAction := selectWarmGlobalResolveAction(anyChanged, warmGlobalResolveSafety{
		resolveOK:                     resolveOK,
		deferredCrossRepoComplete:     deferredResult.CrossRepoComplete,
		deferredMutationRevision:      deferredResult.CrossRepoMutationRevision,
		deferredMutationRevisionKnown: deferredResult.CrossRepoMutationRevisionKnown,
		currentMutationRevision:       currentMutationRevision,
		currentMutationRevisionKnown:  currentMutationRevisionKnown,
		backfillRevisionBefore:        backfillRevisionBefore,
		backfillRevisionBeforeKnown:   backfillRevisionBeforeKnown,
		backfillRevisionAfter:         backfillRevisionAfter,
		backfillRevisionAfterKnown:    backfillRevisionAfterKnown,
		backfillResolutionAffected:    backfillResolutionAffected,
	}, backfilledContracts)
	if globalResolveAction != warmGlobalResolveNone {
		phaseStart = time.Now()
		publishReadinessPhase(state, "global_resolve", true, nil)
		switch globalResolveAction {
		case warmGlobalResolveContracts:
			reconciled := state.multiIndexer.ReconcileContractEdges()
			logger.Info("daemon: contract-edge catch-up complete; skipped full cross-repo sweep",
				zap.Int("contract_edges", reconciled))
		case warmGlobalResolveFull:
			state.multiIndexer.RunGlobalResolve()
		}
		fullCrossRepo := globalResolveAction == warmGlobalResolveFull
		timings.globalResolve = time.Since(phaseStart)
		logger.Info("daemon: warmup phase done",
			zap.String("phase", "global_resolve"),
			zap.Bool("full_cross_repo", fullCrossRepo),
			zap.Duration("elapsed", time.Since(phaseStart)))
		publishReadinessPhase(state, "global_resolve_done", true, map[string]any{
			"elapsed_ms":      time.Since(phaseStart).Milliseconds(),
			"full_cross_repo": fullCrossRepo,
		})
	}

	// Finish the batch: turn off the per-repo skip flag and run the
	// graph-wide derivation passes once. RunGlobalResolve above just
	// lifted the last cross-repo placeholder EdgeCalls, so EdgeTests
	// derivation here picks up cross-repo test→subject pairs that
	// were unresolved during the per-repo loop. On the warm-restart fast
	// path (nothing changed) ResetBatch clears the deferred-batch flags
	// without re-running those passes — the persisted graph already has
	// the derived edges.
	// Scope the per-repo clone detection + clone-index Rebuild in the
	// end_batch graph passes to the repos that actually re-indexed this
	// warmup. Dropped when a changed repo's prefix was indeterminate
	// (scopeUnknown) so a repo whose clones genuinely need recomputing is
	// never skipped. ArmBatchScope is a no-op when scoped global passes are
	// disabled or the set is empty (run every repo, the prior behaviour).
	if anyChanged && !exactWarmDelta && !scopeUnknown.Load() {
		state.multiIndexer.ArmBatchScope(changed)
		// Full-coverage attestation: every tracked repository re-indexed in
		// this warmup (a cold index, or a warm restart that reconciled the
		// whole workspace). The framework-synthesis admission census may
		// then read the raw store even though the batch scope is non-nil —
		// without this, the cold path's all-repos scope silently bypassed
		// every census gate. Computed here, against the daemon's own repo
		// registry, never inferred downstream from scope size.
		if len(changed) == len(repos) {
			state.multiIndexer.ArmBatchCensusEligible()
		}
	}

	phaseStart = time.Now()
	publishReadinessPhase(state, "end_batch", true, nil)
	switch {
	case !anyChanged:
		state.multiIndexer.ResetBatch()
	case exactWarmDelta:
		// Clear the outer batch flags first, then execute only the exact
		// invalidation families carried by the reconciled files.
		state.multiIndexer.ResetBatch()
		report := state.multiIndexer.RunIncrementalDerivedPasses(ctx, deltaPlans)
		logger.Info("daemon: exact warm derived passes complete", zap.Any("report", report))
	default:
		state.multiIndexer.EndBatch()
	}
	postBatchNodes, postBatchContracts, postBatchResolutionAffected := state.multiIndexer.BackfillWorkspaceSlugsWithImpact()
	if postBatchNodes+postBatchContracts > 0 {
		logger.Info("daemon: backfilled workspace/project slugs after derived passes",
			zap.Int("nodes", postBatchNodes),
			zap.Int("contracts", postBatchContracts),
			zap.Int("resolution_affected", postBatchResolutionAffected))
	}
	switch selectWarmGlobalResolveAction(false, warmGlobalResolveSafety{
		backfillResolutionAffected: postBatchResolutionAffected,
	}, postBatchContracts) {
	case warmGlobalResolveContracts:
		reconciled := state.multiIndexer.ReconcileContractEdges()
		logger.Info("daemon: reconciled post-batch contract edges",
			zap.Int("contract_edges", reconciled))
	case warmGlobalResolveFull:
		state.multiIndexer.RunGlobalResolve()
		logger.Info("daemon: post-batch workspace eligibility change forced full cross-repo catch-up")
	}
	timings.endBatch = time.Since(phaseStart)
	logger.Info("daemon: warmup phase done",
		zap.String("phase", "end_batch"),
		zap.Duration("elapsed", time.Since(phaseStart)))
	publishReadinessPhase(state, "end_batch_done", true, map[string]any{
		"elapsed_ms": time.Since(phaseStart).Milliseconds(),
	})

	watchCfgs := make(map[string]config.WatchConfig)
	for prefix := range state.multiIndexer.AllMetadata() {
		watchCfgs[prefix] = state.configManager.GetRepoConfig(prefix).Watch
	}
	mw, err := indexer.NewMultiWatcher(state.multiIndexer, watchCfgs, logger)
	if err != nil {
		logger.Warn("daemon: multi-watcher init failed", zap.Error(err))
		return nil, timings
	}
	if err := mw.Start(); err != nil {
		logger.Warn("daemon: multi-watcher start failed", zap.Error(err))
		return nil, timings
	}
	// Attach the live watcher before publishing watcher_started. This makes
	// readiness truthful: once clients observe the phase, mutation handlers
	// can already enqueue path-scoped work instead of falling back to a costly
	// synchronous IndexFile call.
	if state.mcpServer != nil {
		state.mcpServer.SetWatcher(mw)
		srv := state.mcpServer
		mw.OnDegraded(func(reason string) {
			srv.PublishReadiness("degraded", true, map[string]any{"watch_degraded": reason})
		})
	}
	// Announce what is actually being watched. Reporting the configured count
	// here made an install where every watcher had failed to start look fully
	// watched, so the one signal that the graph was going stale never fired.
	live, configured := mw.WatchedRepos()
	if live < configured {
		logger.Warn("daemon: some repositories are not being watched — their graphs go stale until the daemon restarts",
			zap.Int("live", live),
			zap.Int("configured", configured),
			zap.String("first_reason", mw.DegradedReason()),
		)
	}
	logger.Info("daemon: watching", zap.Int("repos", live), zap.Int("configured", configured))
	publishReadinessPhase(state, "watcher_started", true, map[string]any{
		"watched_repos":    live,
		"configured_repos": configured,
	})
	return mw, timings
}

// logWarmupSummary emits the one-line warmup recap: reconstructing what a
// restart did otherwise means joining the parallel_parse / resolve /
// deferred_passes_all / global_resolve / end_batch per-phase log lines (each
// logged separately, above, as the phases run) plus the separate "graph
// queryable" / "enrichment complete" lines. queryable is the elapsed time at
// markReady (find_usages / get_callers become complete); total is the
// elapsed time at MarkEnriched (the full warmup, including enrichment).
func logWarmupSummary(logger *zap.Logger, warmup *warmupTimings, queryable, total time.Duration) {
	if logger == nil || warmup == nil {
		return
	}
	logger.Info("daemon: warmup summary",
		zap.Float64("parse_s", warmup.parse.Seconds()),
		zap.Float64("resolve_s", warmup.resolve.Seconds()),
		zap.Float64("enrich_s", warmup.enrich.Seconds()),
		zap.Float64("global_resolve_s", warmup.globalResolve.Seconds()),
		zap.Float64("end_batch_s", warmup.endBatch.Seconds()),
		zap.Float64("analysis_s", warmup.analysis.Seconds()),
		zap.Float64("queryable_s", queryable.Seconds()),
		zap.Int("repos_changed", warmup.reposChanged),
		zap.Int("files_reindexed", warmup.filesReindexed),
		zap.Int("enrich_scheduled", warmup.enrichScheduled),
		zap.Float64("total_s", total.Seconds()))
}

// logRepoOwnershipAudit reports the graph's repo-prefix ownership split once
// warmup has settled.
//
// The prefix is the graph's ownership key — it keys the byRepo bucket, the
// per-repo memory estimates, the `nodes_by_repo` partial index and every
// repo-scoped query. When it is wrong nothing errors: the daemon serves a
// subset, or two copies of one repo under different IDs, and every other
// health signal stays green. This is the only place that says so out loud.
//
// A wholly-unowned graph is single-repo mode, not a defect, and logs at Info.
// Both populations coexisting, or a stamped prefix the node ID does not
// carry, is a real inconsistency and logs at Error with named samples.
func logRepoOwnershipAudit(g graph.Store, logger *zap.Logger) {
	if logger == nil || g == nil {
		return
	}
	d, ok := graph.ReadPrefixDiagnostics(g, 5)
	if !ok {
		return
	}
	fields := []zap.Field{
		zap.Int("owned_code_nodes", d.OwnedCodeNodes),
		zap.Int("unowned_code_nodes", d.UnownedCodeNodes),
		zap.Int("misprefixed_nodes", d.MisprefixedNodes),
	}
	if d.Clean() {
		logger.Info("daemon: repo ownership audit clean", fields...)
		return
	}
	if len(d.UnownedSamples) > 0 {
		fields = append(fields, zap.Strings("unowned_samples", d.UnownedSamples))
	}
	if len(d.MisprefixedSamples) > 0 {
		fields = append(fields, zap.Strings("misprefixed_samples", d.MisprefixedSamples))
	}
	logger.Error("daemon: repo ownership is inconsistent — repo-scoped reads will silently return a subset of the graph; "+
		"the store holds both prefixed and unprefixed copies of the same code, or a node's stamped repo differs from its id",
		fields...)
}

// publishReadinessPhase forwards a workspace_readiness phase
// transition to the MCP server's readiness broadcaster. Safe to
// call when the server isn't wired (single-process modes that
// bypass the daemon).
func publishReadinessPhase(state *daemonState, phase string, ready bool, extra map[string]any) {
	if state == nil || state.mcpServer == nil {
		return
	}
	if state.readinessFilter != nil {
		phase, ready, extra = state.readinessFilter(phase, ready, extra)
	}
	state.mcpServer.PublishReadiness(phase, ready, extra)
}

// priorMtimesFromStore asks the backend for its persisted FileMtime
// rows for the repo described by entry. Returns nil when the backend
// doesn't implement the reader (in-memory backend) or has no recorded
// mtimes for the repo (fresh cold start). When non-nil it short-
// circuits the gob-snapshot lookup so the warm path is driven by
// data the backend persisted itself.
func priorMtimesFromStore(g graph.Store, cm *config.ConfigManager, entry config.RepoEntry, logger *zap.Logger) map[string]int64 {
	reader, ok := g.(graph.FileMtimeReader)
	if !ok {
		if logger != nil {
			logger.Info("daemon: priorMtimesFromStore: store does not implement FileMtimeReader")
		}
		return nil
	}
	// Key by the prefix the indexer actually registers the repo under —
	// a worktree instance persists its mtimes under `<base>@<workspace>`,
	// not the bare basename, so a plain ResolvePrefix would load the
	// canonical checkout's mtimes and force a full re-index every restart.
	effective := strings.TrimPrefix(indexer.EffectiveRepoPrefix(cm, entry), "/")
	prefix, ok := warmMtimePrefix(effective)
	if !ok {
		if logger != nil {
			logger.Warn("daemon: priorMtimesFromStore: no effective repo prefix — falling back to a cold index",
				zap.String("entry_path", entry.Path),
				zap.String("entry_name", entry.Name))
		}
		return nil
	}
	mtimes := reader.LoadFileMtimes(prefix)
	if logger != nil {
		logger.Info("daemon: priorMtimesFromStore loaded",
			zap.String("prefix", prefix),
			zap.Int("count", len(mtimes)))
	}
	// Zero rows here is the difference between a warm restart and a full
	// cold re-index (plus, with an API embedder, a full paid re-embed) —
	// and it is invisible in the Info line above, which reports count=0 the
	// same way for "fresh repo" and "we looked under the wrong key". Naming
	// the prefixes the store DOES hold separates the two: an already-indexed
	// repo whose rows sit under a different prefix is a keying bug, not a
	// cold start.
	if len(mtimes) == 0 && logger != nil {
		stored := storedRepoPrefixes(g)
		if len(stored) > 0 {
			logger.Warn("daemon: no persisted file mtimes for this repo — falling back to a full cold re-index; "+
				"the store holds mtimes under other prefixes, so this is a prefix-keying mismatch rather than a first index",
				zap.String("looked_up_prefix", prefix),
				zap.String("entry_path", entry.Path),
				zap.Strings("store_repo_prefixes", stored))
		}
	}
	return mtimes
}

// storedRepoPrefixes returns the repo prefixes the store actually holds
// nodes under, for diagnostics that need to distinguish "nothing indexed"
// from "indexed under a different key". Returns nil when the backend cannot
// answer.
func storedRepoPrefixes(g graph.Store) []string {
	lister, ok := g.(interface{ RepoPrefixes() []string })
	if !ok {
		return nil
	}
	return lister.RepoPrefixes()
}

// warmMtimePrefix picks the repo_prefix to look up persisted file mtimes
// (and, by extension, to decide whether the warm-restart reconcile can run)
// for a repo whose EffectiveRepoPrefix is `effective`.
//
// It must key mtimes exactly where the indexer wrote them. Since every repo
// is prefixed — a lone repo included — that is always the effective prefix,
// and this function is now a thin trust check rather than a mode decision.
//
// It used to mirror the indexer's single-vs-multi gate, returning "" for a
// lone repo because indexSingleRepo / ReconcileRepoCtx wrote that repo's
// rows under the empty prefix. That mirror was load-bearing and fragile: the
// two decisions had to agree exactly, and when they drifted the symptom was
// not an error but a full cold re-index (plus, with an API embedder, a paid
// re-embed) on every single restart.
//
// An empty effective prefix is untrustworthy — it would collide across repos
// and now names only the synthetic global externals — so report ok=false and
// let the caller fall back to a cold index.
//
// KEYWORDS: warm-restart, repo-prefix, file_mtimes, re-embed
func warmMtimePrefix(effective string) (prefix string, ok bool) {
	if effective == "" {
		return "", false
	}
	return effective, true
}

// reconcileFileCount returns the number of files to count as "reindexed"
// for one repo's ReconcileRepoCtx result, for the warmup summary's
// filesReindexed total. A full retrack has no enumerated changed-file set
// (see the fullRetrack comment in ReconcileRepoCtx), so its whole FileCount
// stands in for the work done; an incremental reconcile counts only the
// files it actually touched (stale + deleted).
func reconcileFileCount(res *indexer.IndexResult) int {
	if res == nil {
		return 0
	}
	if res.FullRetrack {
		return res.FileCount
	}
	return res.StaleFileCount + res.DeletedFileCount
}

// storeNeedsRebuild reports whether the backend signalled, via the optional
// NeedsRebuild capability, that a schema migration crossed a rung an ALTER
// could not satisfy — so its persisted rows are in an old shape and the
// warm/incremental reconcile must be bypassed for a full re-index. This is a
// generic, opt-in capability probe: a backend implements NeedsRebuild() bool
// to participate. The on-disk sqlite store does — it reports true for the one
// open in which it dropped an incompatible-schema database and recreated it
// empty (see store_sqlite.Store.NeedsRebuild). The in-memory store does not.
func storeNeedsRebuild(g any) bool {
	rb, ok := g.(interface{ NeedsRebuild() bool })
	return ok && rb.NeedsRebuild()
}

type warmGlobalResolveSafety struct {
	resolveOK                     bool
	deferredCrossRepoComplete     bool
	deferredMutationRevision      uint64
	deferredMutationRevisionKnown bool
	currentMutationRevision       uint64
	currentMutationRevisionKnown  bool
	backfillRevisionBefore        uint64
	backfillRevisionBeforeKnown   bool
	backfillRevisionAfter         uint64
	backfillRevisionAfterKnown    bool
	backfillResolutionAffected    int
}

type warmGlobalResolveAction uint8

const (
	warmGlobalResolveNone warmGlobalResolveAction = iota
	warmGlobalResolveContracts
	warmGlobalResolveFull
)

// canSkipWarmGlobalResolve requires both successful earlier resolution and a
// generation proof around the bounded workspace-slug backfill. Physical
// project/default-workspace stamps may advance a backend's coarse revision but
// preserve cross-repo eligibility, so the deferred frontier must match the
// pre-backfill revision and the post-backfill revision must still be current.
// Unknown revision capability and any writer outside that interval fail closed.
func canSkipWarmGlobalResolve(s warmGlobalResolveSafety) bool {
	return s.resolveOK &&
		s.deferredCrossRepoComplete &&
		s.deferredMutationRevisionKnown &&
		s.currentMutationRevisionKnown &&
		s.backfillRevisionBeforeKnown &&
		s.backfillRevisionAfterKnown &&
		s.deferredMutationRevision == s.backfillRevisionBefore &&
		s.backfillRevisionAfter == s.currentMutationRevision &&
		s.backfillResolutionAffected == 0
}

func selectWarmGlobalResolveAction(anyChanged bool, safety warmGlobalResolveSafety, backfilledContracts int) warmGlobalResolveAction {
	if safety.backfillResolutionAffected > 0 {
		return warmGlobalResolveFull
	}
	if anyChanged {
		if canSkipWarmGlobalResolve(safety) {
			return warmGlobalResolveContracts
		}
		return warmGlobalResolveFull
	}
	if backfilledContracts > 0 {
		return warmGlobalResolveContracts
	}
	return warmGlobalResolveNone
}

// enrichmentOverlapEnabled reports whether the operator explicitly enabled
// cold-warmup enrichment compute during the resolver phase. Sequential is the
// safe default: on a saturated workspace overlap increases both resolver time
// and retained compiler-program memory.
func enrichmentOverlapEnabled() bool {
	return os.Getenv("GORTEX_ENRICH_OVERLAP") == "1"
}

// warmupFullResolveForced reports whether the operator pinned the warm-restart
// master resolve to the whole graph via GORTEX_WARMUP_FULL_RESOLVE=1 (or
// "true"), overriding the changed-repo scoping. An escape hatch for the case
// where a scoped resolve is suspected of missing edges.
func warmupFullResolveForced() bool {
	v := os.Getenv("GORTEX_WARMUP_FULL_RESOLVE")
	return v == "1" || strings.EqualFold(v, "true")
}

// warmupResolveScope decides the changed-repo scope for the warm-restart
// master resolve. It returns the changed set only when scoping is both safe
// and beneficial; any whole-graph-safety precondition failure returns nil,
// which runMasterResolve treats as a whole-graph resolve (the pre-scoping
// behaviour). The guards mirror the reasons the reconcile loop already
// distrusts a partial delta:
//
//   - !anyChanged: nothing re-indexed, the resolve is skipped entirely.
//   - scopeUnknown: a changed repo's prefix was indeterminate — any per-repo
//     uncertainty forces the whole-graph pass.
//   - needsRebuild: the backend crossed a schema-rebuild rung.
//   - GORTEX_WARMUP_FULL_RESOLVE=1: operator override.
//   - empty / all-repos-changed: scoping gains nothing over the full pass.
func warmupResolveScope(changed map[string]struct{}, totalRepos int, anyChanged, scopeUnknown, needsRebuild bool) map[string]struct{} {
	switch {
	case !anyChanged, scopeUnknown, needsRebuild, warmupFullResolveForced():
		return nil
	case len(changed) == 0, len(changed) >= totalRepos:
		return nil
	default:
		return changed
	}
}
