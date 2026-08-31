package serverstack

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gofrs/flock"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/astquery"
	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/contracts"
	"github.com/zzet/gortex/internal/daemon"
	"github.com/zzet/gortex/internal/embedding"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/graphview"
	"github.com/zzet/gortex/internal/indexer"
	gortexmcp "github.com/zzet/gortex/internal/mcp"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
	"github.com/zzet/gortex/internal/platform"
	"github.com/zzet/gortex/internal/query"
	"github.com/zzet/gortex/internal/savings"
	"github.com/zzet/gortex/internal/semantic"
	"github.com/zzet/gortex/internal/semantic/goanalysis"
	"github.com/zzet/gortex/internal/semantic/lsp"
	"github.com/zzet/gortex/internal/semantic/scip"
	"github.com/zzet/gortex/internal/semantic/tstypes"
	"github.com/zzet/gortex/internal/telemetry"
)

// Lifecycle selects whether the entry point's warm-restart machinery is
// appropriate and what the store-lock posture is. Every lifecycle runs
// against the sqlite store; they differ only in where that store lives and
// whether it is locked.
type Lifecycle int

const (
	// LifecycleDaemon is the durable, long-lived daemon — including its
	// HTTP surface: a shared store under the cross-process store lock.
	LifecycleDaemon Lifecycle = iota
	// LifecycleOneshot is the ephemeral embedded server: a sqlite store on
	// a private per-process path that the caller supplies and deletes, and
	// no store lock — its graph lives and dies with the process.
	LifecycleOneshot
)

// Writable reports whether the lifecycle owns a *shared* durable store and
// therefore takes the cross-process store lock. One-shot is deliberately
// excluded: it runs against a private temp file no other process can name,
// so an advisory flock would guard nothing while still making two
// concurrent `gortex mcp` fallbacks fight over the lock file.
func (l Lifecycle) Writable() bool { return l == LifecycleDaemon }

// SharedServerConfig carries the knobs that vary between the daemon, the
// HTTP surface, and the one-shot embedded path. The first block is the
// authoritative public surface consumers cite; the rest are
// entry-point-resolved options threaded through with the already-loaded
// config rather than re-derived here.
type SharedServerConfig struct {
	Lifecycle Lifecycle // store-lock posture
	Index     string    // workspace root the indexer/LSP/bind anchor at
	Backend   string    // "" or "sqlite" — the sqlite store is the only backend
	// BackendPath names the store file. Empty means the shared default
	// (~/.gortex/store/store.sqlite) — which only a lifecycle that takes
	// the store lock may use, so LifecycleOneshot must set it to a private
	// per-process path it owns and deletes.
	BackendPath string
	HTTPAddr    string // opts the lifecycle into the /mcp HTTP surface
	Watch       bool   // filesystem watcher / incremental reindex

	// Entry-point-resolved options (not part of the authoritative surface).
	Config            *config.Config       // loaded .gortex.yaml (required)
	Global            *config.GlobalConfig // loaded ~/.gortex/config.yaml
	Logger            *zap.Logger
	Version           string
	MigrationObserver store_sqlite.MigrationObserver
	Embedder          EmbedderRequest
	SideStores        SideStores
	ScopeWorkspace    string
	ScopeProject      string
	// ActiveProject names the project the MCP server should start scoped
	// to (multi-repo mode hint). Empty leaves it unset.
	ActiveProject string
	// SemanticMode selects the goanalysis provider mode: "callgraph"
	// builds the call graph; anything else (default) type-checks.
	SemanticMode string
	// SavingsPath overrides the token-savings ledger database; empty
	// defaults to the machine-global sidecar under the data dir
	// (savings.DefaultDBPath() — the same database the `gortex savings`
	// CLI reads), independent of SideStores: deriving it from a per-mode
	// side-store dir would split the ledger between writer and reader.
	// Tests MUST set it (with SavingsLegacyJSON) to temp paths or the
	// constructor mutates the developer's real ledger. SavingsRepo
	// scopes the accumulated totals (empty = workspace-global).
	// SavingsLegacyJSON names the flat-file era's cumulative
	// savings.json to import once — its sibling .jsonl event log rides
	// along; empty uses the historical default location under the
	// cache dir.
	SavingsPath       string
	SavingsLegacyJSON string
	SavingsRepo       string
}

// SideStores configures where the agent-authored knowledge stores
// persist. Each (dir, repo) pair selects the sidecar DB file (dir, which
// opens <dir>/sidecar.sqlite) and the partition key (repo, hashed via
// persistence.RepoCacheKey); an empty dir OR repo yields an in-memory
// store that never flushes. The zero value therefore makes every store
// ephemeral — entry points opt into persistence explicitly, encoding
// whether the server owns one repo (per-repo keying) or many (a
// workspace-global key).
type SideStores struct {
	// NotesDir / NotesRepo back the durable, repo-partitioned notes and
	// development-memory stores.
	NotesDir  string
	NotesRepo string
	// FeedbackDir / FeedbackRepo back feedback, the combo tracker, and the
	// frecency tracker (the implicit-signal stores).
	FeedbackDir  string
	FeedbackRepo string
	// NotebookPath is the repository-local notebook root (committed to
	// git so it travels with the repo); empty leaves the notebook
	// uninitialised.
	NotebookPath string
}

// SharedServer bundles the constructed stack plus the teardown chain.
type SharedServer struct {
	Graph        graph.Store
	Indexer      *indexer.Indexer
	MultiIndexer *indexer.MultiIndexer // nil in single-repo standalone
	Engine       *query.Engine
	MCP          *gortexmcp.Server
	ConfigMgr    *config.ConfigManager
	Overlays     *daemon.OverlayManager
	// CheckoutLifecycle owns every checkout lifecycle side effect — track,
	// forget, reload, the periodic sweep — for every entry point wired to
	// this stack. Nil in single-repo standalone, where there is no
	// multi-repo indexer to own a tracked set.
	CheckoutLifecycle *indexer.CheckoutLifecycle
	// StorePath is the graph store file this stack actually opened: the
	// caller's BackendPath expanded to an absolute path, or the platform
	// default when it was empty. Entry points publish it so out-of-band
	// readers can find the same store instead of re-deriving the default.
	StorePath string

	// ResolverLSPRegistry / LSPRouter are the resolve-time LSP wiring the
	// entry point's warmup hooks reference; nil when semantic enrichment
	// is off.
	ResolverLSPRegistry *lsp.ResolverHelperRegistry
	LSPRouter           *lsp.Router

	cleanup []func() // run LIFO by Close (backend close, savings flush).
}

// Close runs the teardown chain LIFO.
func (s *SharedServer) Close() error {
	for i := len(s.cleanup) - 1; i >= 0; i-- {
		s.cleanup[i]()
	}
	return nil
}

// telemetrySendInterval bounds how often the daemon flushes the in-memory
// usage recorder and attempts to send completed daily aggregates. Long, since
// only whole UTC days are ever sent and MaybeSend self-limits to once per day.
const telemetrySendInterval = 6 * time.Hour

// telemetryConsentTTL bounds how often the daemon recorder re-reads consent on
// the hot record path — short enough that `gortex telemetry off` stops
// recording promptly, long enough to avoid a consent-file read per tool call.
const telemetryConsentTTL = 5 * time.Second

// startTelemetrySender launches the daemon-only periodic flush-and-send loop
// and returns a stop function for the cleanup chain. It re-resolves consent
// each tick so `gortex telemetry off` stops transmission within one interval,
// and is a complete no-op while GORTEX_TELEMETRY_ENDPOINT is unset.
func startTelemetrySender(srv *gortexmcp.Server, store *telemetry.Store, dir, version string) func() {
	sender := telemetry.NewSender(store, dir, version, os.Getenv)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		t := time.NewTicker(telemetrySendInterval)
		defer t.Stop()
		flushAndSend := func() {
			srv.FlushTelemetry()
			consent := telemetry.ResolveConsent(telemetry.LoadConsentConfig(dir), os.Getenv)
			sender.MaybeSend(ctx, consent)
		}
		flushAndSend() // opportunistic send on startup
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				flushAndSend()
			}
		}
	}()
	return cancel
}

// NewSharedServer builds the whole server stack — graph.Store ->
// parser.Registry -> indexer -> query.Engine -> mcp.Server plus every
// side-effect init — through one wiring shared by the daemon, the HTTP
// surface, and the one-shot embedded path. Snapshot warm-start and the
// per-repo warmup loop stay with the entry point (they orchestrate around
// the returned graph/indexer); this constructor is the dedup spine for
// the stack wiring itself.
func NewSharedServer(cfg SharedServerConfig) (*SharedServer, error) {
	logger := cfg.Logger
	if logger == nil {
		logger = zap.NewNop()
	}
	conf := cfg.Config
	if conf == nil {
		conf = config.Default()
	}
	// Reject an unusable backend name here, before the store lock is taken
	// and before any path is resolved, so the caller gets the naming error
	// rather than a downstream symptom of it.
	if err := checkBackend(cfg.Backend); err != nil {
		return nil, err
	}
	// A one-shot server takes no store lock, so it must not be able to
	// reach the shared default store: resolveBackendPath("") would hand it
	// ~/.gortex/store/store.sqlite, and a second unlocked writer on the
	// daemon's database is how that file gets corrupted. Refuse rather
	// than silently share.
	if cfg.Lifecycle == LifecycleOneshot &&
		strings.TrimSpace(cfg.BackendPath) == "" {
		return nil, errors.New("one-shot server requires an explicit BackendPath: " +
			"it holds no store lock and must not open the shared default store")
	}

	// Load user-defined domain-extractor rules (TOML tree-sitter patterns).
	for _, pattern := range conf.RuleFiles {
		matches, _ := filepath.Glob(pattern)
		if len(matches) == 0 {
			matches = []string{pattern}
		}
		for _, rp := range matches {
			if n, lerr := astquery.LoadUserRulesFile(rp); lerr != nil {
				logger.Warn("serverstack: failed to load domain rule file",
					zap.String("file", rp), zap.Error(lerr))
			} else if n > 0 {
				logger.Info("serverstack: loaded domain-extractor rules",
					zap.String("file", rp), zap.Int("rules", n))
			}
		}
	}

	s := &SharedServer{}

	// Resolve the store path once: the lock, the open, and the path published
	// on SharedServer must all name the same file. The backend name was
	// already rejected above, before anything touched the filesystem.
	storePath, err := resolveBackendPath(cfg.BackendPath, "store.sqlite")
	if err != nil {
		return nil, err
	}
	s.StorePath = storePath

	// Cross-process store lock: a writable, on-disk lifecycle
	// acquires an advisory flock on store.sqlite.lock and fails fast if
	// another process owns the store. SQLite's in-process writeMu +
	// busy_timeout serialise in-process writers only; nothing else stops
	// a second OS process opening the same file and corrupting it.
	storeLockHeld := false // set true once the exclusive store flock is owned below
	if cfg.Lifecycle.Writable() {
		lock := flock.New(storePath + ".lock")
		locked, lerr := lock.TryLock()
		if lerr != nil {
			return nil, fmt.Errorf("acquire store lock %q: %w", storePath+".lock", lerr)
		}
		if !locked {
			hint := "the store is already owned by another gortex process"
			if pid, ok := daemon.RunningPID(); ok {
				hint = fmt.Sprintf("store already owned by daemon pid %d", pid)
			}
			return nil, fmt.Errorf("%s — stop it first (`gortex daemon stop`)", hint)
		}
		s.cleanup = append(s.cleanup, func() { _ = lock.Unlock() })
		storeLockHeld = true
	}

	// allowRebuild is gated on actually holding the store lock: only then may
	// the sqlite backend drop and recreate an incompatible-schema DB.
	g, backendCleanup, err := OpenBackend(cfg.Backend, storePath, logger, storeLockHeld, cfg.MigrationObserver)
	if err != nil {
		return nil, err
	}
	s.cleanup = append(s.cleanup, backendCleanup)
	s.Graph = g

	reg := parser.NewRegistry()
	languages.RegisterAll(reg)
	languages.RegisterCustomGrammars(reg, conf.Index.Grammars, logger)
	languages.RegisterExtractorPlugins(reg, conf.Index.ExtractorPlugins, logger)
	languages.RegisterFallbackChunkers(reg, conf.Index.FallbackChunkers, logger)

	idx := indexer.New(g, reg, conf.Index, logger)
	s.Indexer = idx

	// Semantic enrichment (opt-in). A daemon-managed LSP router owns
	// subprocess lifecycle; SCIP + goanalysis providers register eagerly;
	// Manager.SetLSPRouter installs the bridge so EnrichAll can lazy-spawn
	// LSPs on demand. RegisterAvailable auto-registers every known LSP
	// spec whose binary resolves on PATH (per-spec / GORTEX_LSP_DISABLE
	// opt-out). All lifecycles get this (reconciles the prior mcp-only
	// drift where the embedded path skipped auto-registration).
	var semMgr *semantic.Manager
	if conf.Semantic.Enabled {
		semCfg := semantic.Config{
			Enabled:           conf.Semantic.Enabled,
			TimeoutSeconds:    conf.Semantic.TimeoutSeconds,
			EnrichOnWatch:     conf.Semantic.EnrichOnWatch,
			WatchDebounceMs:   conf.Semantic.WatchDebounceMs,
			RefuteUnconfirmed: conf.Semantic.RefuteUnconfirmed,
			ExcludeGlobs:      conf.Semantic.ExcludeGlobs,
			LSPSweep:          conf.Semantic.LSPSweep,
			LSPOpenDocs:       conf.Semantic.LSPOpenDocs,
			LSPMaxParallel:    conf.Semantic.LSPMaxParallel,
			EagerLSP:          eagerLSPEnabled(conf.Semantic),
		}
		for _, pc := range conf.Semantic.Providers {
			out := semantic.ProviderConfig{
				Name:      pc.Name,
				Languages: pc.Languages,
				Command:   pc.Command,
				Args:      pc.Args,
				Env:       pc.Env,
				Mode:      pc.Mode,
				Daemon:    pc.Daemon,
				Priority:  pc.Priority,
				Enabled:   pc.Enabled,
			}
			if pc.Connect != nil {
				out.Connect = &semantic.ConnectConfig{
					Network:       pc.Connect.Network,
					Address:       pc.Connect.Address,
					FallbackSpawn: pc.Connect.FallbackSpawn,
				}
			}
			semCfg.Providers = append(semCfg.Providers, out)
		}
		semMgr = semantic.NewManager(semCfg, logger)

		goMode := goanalysis.ModeTypeCheck
		if cfg.SemanticMode == "callgraph" {
			goMode = goanalysis.ModeCallGraph
		}
		goProvider := goanalysis.NewProvider(goMode, goTypesIncludeTests(), logger)
		// The go/packages "go-types" provider owns Go enrichment when the
		// toolchain is present: it type-checks the module once in-process and
		// stamps every symbol's exact type plus resolves stdlib/dep calls to
		// real nodes, in a fraction of the time the per-request gopls LSP pass
		// takes (which additionally leaves external calls as dead stubs). As an
		// eager provider it claims the "go" language slot, so the LSP router's
		// gopls spec is skipped as a gap-filler and never spawns. Registration
		// is gated on Available() (a `go` toolchain on PATH): without it the
		// slot stays open and gopls / the tree-sitter floor serve Go instead.
		if goTypesEnrichEnabled(conf.Semantic) && goProvider.Available() {
			semMgr.RegisterProvider(goProvider)
		}
		contracts.SetBindingResolver(goProvider)

		// In-process tree-sitter type resolvers — always-available
		// (no external binary), supplemental (they coexist with LSP /
		// SCIP providers instead of competing for the language slot).
		// Disable one via a `semantic.providers` entry with
		// `enabled: false` under its name (e.g. "java-types").
		for _, tp := range tstypes.DefaultProviders(logger) {
			semMgr.RegisterProvider(tp)
		}

		// Only the embedded single-repo path (cfg.Index set) has one
		// meaningful default workspace. The daemon tracks many repos and is
		// routinely launched from none of them, so its default stays empty:
		// every caller passes the tracked repo's own root per request, and
		// one that forgets now fails loudly instead of spawning a language
		// server in the daemon's launch directory.
		lspRouter := lsp.NewRouter(cfg.Index, logger).
			WithIdleTimeout(lsp.IdleTimeoutFromEnv(10 * time.Minute)).
			WithReaperInterval(time.Minute).
			WithMaxAlive(6).
			WithAdditionalWorkspaceFolders(conf.Semantic.AdditionalWorkspaceFolders).
			WithEnrichExcludeGlobs(conf.Semantic.ExcludeGlobs).
			WithEnrichSweepMode(semCfg.LSPSweep).
			WithEnrichOpenDocs(semCfg.LSPOpenDocs).
			WithEnrichMaxParallel(semCfg.LSPMaxParallel)
		semMgr.SetLSPRouter(lspRouter)

		for _, pc := range semCfg.Providers {
			if !pc.Enabled {
				continue
			}
			switch {
			case strings.HasPrefix(pc.Name, "scip-") && pc.Command != "":
				scipProv := scip.NewProvider(pc.Command, pc.Args, pc.Languages, semCfg.TimeoutSeconds, logger)
				if pc.Mode == "definitions" {
					scipProv = scipProv.WithDefinitionsOnly()
				}
				semMgr.RegisterProvider(scipProv)
			case lsp.SpecByName(pc.Name) != nil:
				var connect *lsp.ConnectSpec
				if pc.Connect != nil {
					connect = &lsp.ConnectSpec{
						Network:       pc.Connect.Network,
						Address:       pc.Connect.Address,
						FallbackSpawn: pc.Connect.FallbackSpawn,
					}
				}
				lspRouter.RegisterSpec(lsp.SpecWithOverridesConnect(
					lsp.SpecByName(pc.Name), pc.Command, pc.Args, pc.Env, connect))
			case pc.Daemon:
				semMgr.RegisterProvider(lsp.NewProvider(pc.Command, pc.Args, pc.Languages, pc.Daemon, pc.MaxParallel, logger))
			}
		}

		disabled := LspDisabledSet(conf.Semantic.Providers, os.Getenv("GORTEX_LSP_DISABLE"))
		var autoRegistered []string
		if !disabled["__all__"] {
			autoRegistered = lspRouter.RegisterAvailable(disabled)
		}

		idx.SetSemanticManager(semMgr)

		if !IsFalsyEnv("GORTEX_LSP_RESOLVER") {
			s.ResolverLSPRegistry = lsp.NewResolverHelperRegistry()
			s.LSPRouter = lspRouter
			idx.SetResolverLSPHelper(s.ResolverLSPRegistry)
			// Single-repo standalone mode: anchor a "" prefix helper at
			// the workspace the server points at. Only fires when Index is
			// set (the embedded path); the daemon leaves Index empty and
			// registers per-repo helpers via the OnRepoTracked hook.
			if cfg.Index != "" {
				if abs, aerr := filepath.Abs(cfg.Index); aerr == nil {
					helper, specs := BuildResolverLSPHelperForRepo(lspRouter, abs, lsp.ResolverPoolSizeFromEnv(1), logger)
					if helper != nil {
						s.ResolverLSPRegistry.Register("", helper)
						logger.Info("serverstack: single-repo resolve-time LSP helpers registered",
							zap.Strings("specs", specs))
					}
				}
			}
			logger.Info("serverstack: resolve-time LSP hot path enabled")
		} else {
			logger.Info("serverstack: resolve-time LSP hot path disabled (GORTEX_LSP_RESOLVER=0)")
		}

		logger.Info("serverstack: semantic enrichment enabled",
			zap.Int("providers", len(semCfg.Providers)),
			zap.Strings("lsp_auto_registered", autoRegistered))
	}

	// Layer the global ~/.gortex/config.yaml `embedding:` block under the
	// repo-local one before resolving the embedder, so an `embedding:` block in
	// the global file is honoured while any repo-local non-zero field still
	// wins. Done here (not at the LLM merge below) so it precedes ResolveEmbedder
	// and leaves its flag/env precedence untouched. Warn about any unrecognised
	// top-level keys — most often an `embedding:` block written in the wrong file.
	embGlobal := cfg.Global
	if embGlobal == nil {
		var gerr error
		if embGlobal, gerr = config.LoadGlobal(); gerr != nil {
			// A malformed global file is otherwise silently ignored — exactly
			// when its embedding/llm block would have taken effect.
			logger.Warn("serverstack: global config could not be parsed — its embedding/llm block is ignored",
				zap.Error(gerr))
		}
	}
	conf.Embedding = embGlobal.MergeEmbeddingInto(conf.Embedding)
	// Diagnose unknown keys in the file that was actually loaded (which may be an
	// overridden path from cfg.Global), not always the default location.
	globalPath := ""
	if embGlobal != nil {
		globalPath = embGlobal.ConfigPath()
	}
	if unknown := config.UnknownGlobalKeys(globalPath); len(unknown) > 0 {
		logger.Warn("serverstack: ~/.gortex/config.yaml contains keys gortex does not recognize",
			zap.Strings("keys", unknown),
			zap.String("hint", "see docs/semantic-search.md for embedding config placement"))
	}

	// Embeddings: explicit flag/env > `embedding:` config > default (on,
	// static GloVe).
	// The rerank semantic-cosine channel loads its own bundled model,
	// separately from the vector-index embedder resolved just below, and
	// used to do so with no way to say no. Honour the resolved config
	// before anything can trigger the first search.
	embedding.SetCodeEmbedderEnabled(conf.Search.RerankEmbedderEnabled())

	embedder, embDesc, embReport, embErr := ResolveEmbedder(cfg.Embedder, conf)
	// Probe API-backed providers up front so Dimensions() is truthful before
	// we log it and before anything gates on the embedder width. An
	// APIProvider reports 0 until its first embed; without this the log reads
	// dim:0 and any width comparison against already-persisted vectors sees a
	// mismatch that isn't there.
	// Static / local providers know their width natively and don't implement
	// the prober, so they skip this. Best-effort: a probe failure (bad key /
	// unreachable URL) only warns — indexing still falls back to BM25.
	if embedder != nil {
		if prober, ok := embedder.(interface {
			ProbeDimensions(context.Context) (int, error)
		}); ok {
			if dim, perr := prober.ProbeDimensions(context.Background()); perr != nil {
				logger.Warn("serverstack: embedding dimension probe failed — width unknown until first embed",
					zap.Error(perr))
			} else {
				logger.Info("serverstack: embedding dimension probed", zap.Int("dim", dim))
			}
		}
	}
	// Surface every backend the local auto-selection tried. Warn per backend
	// only when the outcome was actually degraded (fell to the static fallback,
	// or built nothing) AND the backend could really have worked — a backend
	// that is simply not compiled into this build (the onnx/gomlx stubs) is
	// benign noise and stays at debug even on a degradation.
	outcomeDegraded := (embedder == nil || embReport.Chosen == "static") && len(embReport.Attempts) > 0
	for _, a := range embReport.Attempts {
		if outcomeDegraded && !errors.Is(a.Err, embedding.ErrBackendNotCompiled) {
			logger.Warn("serverstack: embedding backend unavailable — degraded to static fallback",
				zap.String("backend", a.Backend), zap.Error(a.Err))
		} else {
			logger.Debug("serverstack: embedding backend not available",
				zap.String("backend", a.Backend), zap.Error(a.Err))
		}
	}
	switch {
	case embErr != nil:
		logger.Warn("serverstack: embeddings requested but unavailable", zap.Error(embErr))
	case embedder != nil:
		logger.Info("serverstack: embeddings enabled",
			zap.String("provider", embDesc),
			zap.Int("dim", embedder.Dimensions()))
	default:
		logger.Info("serverstack: embeddings disabled")
	}
	if embedder != nil {
		idx.SetEmbedder(embedder)
		idx.SetEmbeddingChunkOptions(EmbeddingChunkOptions(conf))
		idx.SetEmbeddingMaxSymbols(conf.Embedding.MaxSymbols)
		idx.SetEmbeddingAPIConcurrency(conf.Embedding.APIConcurrency)
	}

	cm, err := config.NewConfigManager("")
	if err != nil {
		logger.Warn("serverstack: could not load global config", zap.Error(err))
	}
	s.ConfigMgr = cm

	var mi *indexer.MultiIndexer
	if cm != nil {
		mi = indexer.NewMultiIndexer(g, reg, idx.Search(), cm, logger)
		if embedder != nil {
			mi.SetEmbedder(embedder)
			mi.SetEmbeddingChunkOptions(EmbeddingChunkOptions(conf))
			mi.SetEmbeddingMaxSymbols(conf.Embedding.MaxSymbols)
			mi.SetEmbeddingAPIConcurrency(conf.Embedding.APIConcurrency)
		}
		// Propagate the semantic manager so per-repo Indexers
		// created by the MultiIndexer can run LSP enrichment.
		// Without this, daemon-mode enrichment produces zero
		// results (enrich:0 in DEFERRED-TIMING).
		if semMgr != nil {
			mi.SetSemanticManager(semMgr)
		}
		if s.ResolverLSPRegistry != nil {
			mi.SetResolverLSPHelper(s.ResolverLSPRegistry)
			if s.LSPRouter != nil {
				routerRef := s.LSPRouter
				registryRef := s.ResolverLSPRegistry
				poolSize := lsp.ResolverPoolSizeFromEnv(1)
				mi.SetOnRepoTracked(func(prefix, absPath string) {
					helper, _ := BuildResolverLSPHelperForRepo(routerRef, absPath, poolSize, logger)
					if helper != nil {
						registryRef.Register(prefix, helper)
					}
				})
			}
		}
	}
	s.MultiIndexer = mi
	// One reconciler per stack, built here so every entry point — the daemon
	// controller, the MCP tools, the auto-index path, the janitor — drives
	// the same identities, clocks and cleanup hooks. It binds to the store's
	// catalog when the backend has one and degrades to the plain
	// index/watcher/config side effects when it does not.
	if mi != nil {
		if conf.Views.RetainInactive != "" && conf.Views.RetainInactiveDuration() == 0 {
			logger.Warn("serverstack: views.retain_inactive is not a positive duration; using the shipped window",
				zap.String("value", conf.Views.RetainInactive))
		}
		lifecycle, lerr := indexer.NewCheckoutLifecycle(indexer.CheckoutLifecycleConfig{
			MultiIndexer:  mi,
			ConfigManager: cm,
			Graph:         g,
			Logger:        logger,
			RefViews: indexer.RefViewRetention{
				RetainInactive:       conf.Views.RetainInactiveDuration(),
				MaxCachedGenerations: conf.Views.MaxCachedGenerations,
				MaxBytesPerGraph:     conf.Views.MaxBytesPerGraph,
			},
		})
		if lerr != nil {
			return nil, fmt.Errorf("build checkout lifecycle: %w", lerr)
		}
		s.CheckoutLifecycle = lifecycle
	}
	// Appended after backendCleanup but before MCP background drain. LIFO
	// teardown therefore drains detached MCP work first, then cancels and joins
	// every checkout transition/coordinator, then closes per-repo parser
	// workers and the standalone Indexer, and only then closes the graph and
	// store lock. Closing MultiIndexer/backend before the lifecycle was joined
	// let a cold promotion keep writing into an already-closing SQLite store.
	s.cleanup = append(s.cleanup, func() {
		if s.CheckoutLifecycle != nil {
			if err := s.CheckoutLifecycle.Close(); err != nil {
				logger.Warn("serverstack: checkout lifecycle shutdown failed", zap.Error(err))
			}
		}
		if mi != nil {
			if err := mi.Close(context.Background()); err != nil {
				logger.Warn("serverstack: multi-indexer shutdown failed", zap.Error(err))
			}
		}
		idx.Close()
	})

	toolPolicyCfg := gortexmcp.ToolPolicyConfig{
		Preset:         conf.MCP.Tools.Preset,
		Mode:           conf.MCP.Tools.Mode,
		Allow:          conf.MCP.Tools.Allow,
		Deny:           conf.MCP.Tools.Deny,
		OperatorPinned: conf.MCP.Tools.Explicit,
	}
	scopeIntentDefaults := conf.Scope.IntentDefaults
	multiOpts := []gortexmcp.MultiRepoOptions{{
		MultiIndexer:        mi,
		ConfigManager:       cm,
		CheckoutLifecycle:   s.CheckoutLifecycle,
		ActiveProject:       cfg.ActiveProject,
		ScopeWorkspace:      cfg.ScopeWorkspace,
		ScopeProject:        cfg.ScopeProject,
		ToolPolicy:          &toolPolicyCfg,
		ScopeIntentDefaults: &scopeIntentDefaults,
	}}

	eng := query.NewEngine(g)
	eng.SetSearchProvider(idx.Search)
	eng.ApplyRerankWeights(conf.Search.Weights)
	s.Engine = eng

	gortexmcp.Version = cfg.Version
	srv := gortexmcp.NewServer(eng, g, idx, nil, logger, conf.Guards.Rules, multiOpts...)
	// Appended after backendCleanup so LIFO teardown drains the server's
	// detached store-writing goroutines (analysis-generation prune) before
	// the backend store closes underneath them.
	s.cleanup = append(s.cleanup, srv.DrainBackground)
	// The server is the session/analysis fan-out, so it can only be bound
	// after it exists. Every lifecycle side effect — including the ones the
	// daemon controller drives — reaches sessions through it from here on.
	s.CheckoutLifecycle.SetNotifier(srv)
	// One materializer per stack, over the same store and catalog every other
	// collaborator uses, so a routed view and the sweep that retires its
	// generations agree on which leases are live. The lease manager must be
	// the lifecycle's own: retirement runs in the coordinators with that
	// manager as its in-use predicate, so a request leasing through any other
	// manager would be invisible to the sweep and its generations could be
	// deleted mid-read. A backend without a catalog hands the server nothing
	// and every request stays on the base corpus.
	if store, ok := g.(*store_sqlite.Store); ok {
		leases := s.CheckoutLifecycle.ViewLeases()
		if leases == nil {
			leases = graphview.NewLeaseManager()
		}
		srv.SetMaterializer(&graphview.Materializer{
			Store:   store,
			Catalog: store.Catalog(),
			Leases:  leases,
			Logger:  logger,
		})
	}
	srv.SetArchitecture(conf.Architecture)
	srv.SetEventRules(conf.Events.Rules)
	srv.SetArtifacts(conf.Artifacts)
	srv.SetNamedQueries(conf.Queries)
	srv.SetSearchConfig(conf.Search)
	s.MCP = srv

	overlays := daemon.NewOverlayManager(daemon.OverlayIdleTTLFromEnv(0))
	srv.SetOverlayManager(overlays)
	s.Overlays = overlays
	stopOverlayJanitor := overlays.StartJanitor(0, func(dropped int) {
		logger.Info("overlay janitor: swept idle sessions", zap.Int("dropped", dropped))
	})
	s.cleanup = append(s.cleanup, stopOverlayJanitor)

	if semMgr := idx.SemanticManager(); semMgr != nil {
		srv.SetSemanticManager(semMgr)
		srv.SetLSPDiagnosticsBroadcasting()
	}

	// Opt-in usage telemetry. Every entry point records (consent-gated and
	// fail-silent — nothing happens unless the user enabled it); the daemon
	// additionally flushes and sends completed daily aggregates on a long
	// interval, dormant until GORTEX_TELEMETRY_ENDPOINT is configured. Both the
	// record path (via a TTL-cached resolver) and the send path re-resolve
	// consent live, so `gortex telemetry off` stops recording within
	// telemetryConsentTTL and stops transmission within one send interval —
	// rather than freezing the decision at startup.
	teleDir := platform.TelemetryDir()
	teleStore := telemetry.NewStore(teleDir)
	teleRec := telemetry.NewRecorderFunc(
		telemetry.CachedConsentResolver(teleDir, telemetryConsentTTL), teleStore)
	srv.SetTelemetryRecorder(teleRec)
	s.cleanup = append(s.cleanup, func() { srv.FlushTelemetry() })
	if cfg.Lifecycle == LifecycleDaemon {
		// One daemon-session event per process start, dimensioned by backend
		// — a constant now that sqlite is the only one, kept as a dimension
		// so the event shape survives a future second store.
		// Opt-in + fail-silent: a disabled recorder drops it.
		telemetry.RecordDaemonSession(teleRec, "sqlite")
		s.cleanup = append(s.cleanup, startTelemetrySender(srv, teleStore, teleDir, cfg.Version))
	}

	// Side-stores. The HTTP path previously wired NONE of these; routing it
	// through the shared constructor adds notes/memories/savings to it.
	sideCfg := cfg.SideStores
	srv.InitFeedback(sideCfg.FeedbackDir, sideCfg.FeedbackRepo)
	srv.InitNotes(sideCfg.NotesDir, sideCfg.NotesRepo)
	// Learned tool surface: re-promote tools this workspace promoted in
	// prior sessions (demoting the ones that fell out of use). Runs after
	// the NewServer register sweep, so the cold tools are already deferred.
	srv.InitLearnedTools(sideCfg.NotesDir, sideCfg.NotesRepo)
	srv.InitMemories(sideCfg.NotesDir, sideCfg.NotesRepo)
	srv.InitSuppressions(sideCfg.NotesDir, sideCfg.NotesRepo)
	srv.InitNotebook(sideCfg.NotebookPath)
	srv.InitCombo(sideCfg.FeedbackDir, sideCfg.FeedbackRepo, gortexmcp.ModeAI)
	srv.InitFrecency(sideCfg.FeedbackDir, sideCfg.FeedbackRepo, gortexmcp.ModeAI)

	// The savings ledger is machine-global: every entry point defaults to
	// the same sidecar database the `gortex savings` CLI reads. Deriving
	// it from a per-mode side-store dir would split the ledger between
	// writer and reader — the failure mode the flat files had.
	savingsPath := cfg.SavingsPath
	if savingsPath == "" {
		savingsPath = savings.DefaultDBPath()
	}
	if savingsStore, err := savings.Open(savingsPath); err == nil {
		legacyJSON := cfg.SavingsLegacyJSON
		if legacyJSON == "" {
			legacyJSON = savings.DefaultPath()
		}
		if ierr := savingsStore.ImportLegacy(legacyJSON); ierr != nil {
			logger.Warn("serverstack: legacy savings import failed", zap.Error(ierr))
		}
		srv.InitSavings(savingsStore, cfg.SavingsRepo)
		s.cleanup = append(s.cleanup, func() { _ = srv.FlushSavings() })
	} else {
		logger.Warn("serverstack: savings persistence disabled", zap.Error(err))
	}

	global := cfg.Global
	if global == nil {
		global, _ = config.LoadGlobal()
	}
	if global != nil {
		srv.SetupLLM(global.MergeLLMInto(conf.LLM))
		// SetupLLM constructs the LLM service lazily and attaches it; nothing
		// else frees it, so a graceful shutdown would leave a loaded local
		// model resident (only the idle reaper unloads at runtime). Register
		// its Close in the cleanup chain — the "caller owns Close" contract
		// SetLLMService documents.
		if llmSvc := srv.LLMService(); llmSvc != nil {
			s.cleanup = append(s.cleanup, func() { _ = llmSvc.Close() })
		}
	}

	return s, nil
}

// goTypesEnrichEnabled reports whether the in-process go/packages "go-types"
// enrichment provider should be registered for Go. Default ON: it is both
// faster and more complete than driving gopls over LSP (see the registration
// site), and registration is separately gated on the toolchain being present.
// Precedence: GORTEX_GO_TYPES=1/0 wins, then an explicit semantic.go_types
// config value, then the on-by-default.
func goTypesEnrichEnabled(sem config.SemanticConfig) bool {
	if v := os.Getenv("GORTEX_GO_TYPES"); v != "" {
		return v == "1" || strings.EqualFold(v, "true")
	}
	if sem.GoTypes != nil {
		return *sem.GoTypes
	}
	return true
}

// goTypesIncludeTests reports whether the go-types provider should load
// _test.go files (and each package's external test variant). Loading them
// roughly doubles the go/packages work per module, so it is OFF by default;
// GORTEX_GO_TYPES_TESTS=1 opts in to stamp test-file symbols too (LSP hover
// covers them, so the opt-in closes that coverage gap when it matters).
func goTypesIncludeTests() bool {
	return os.Getenv("GORTEX_GO_TYPES_TESTS") == "1" ||
		strings.EqualFold(os.Getenv("GORTEX_GO_TYPES_TESTS"), "true")
}

// eagerLSPEnabled reports whether the subprocess LSP servers run during the
// synchronous enrichment pass. Default OFF — LSP is the slowest part of a cold
// index and its net-new value over the in-process tiers (go-types for Go, the
// tree-sitter floor for every language) is narrow, so it is kept off the
// cold/warm-start path and the router lazy-spawns a server on demand instead.
// GORTEX_LSP_EAGER=1 (or semantic.eager_lsp in config) restores the eager
// behaviour.
func eagerLSPEnabled(sem config.SemanticConfig) bool {
	if v := os.Getenv("GORTEX_LSP_EAGER"); v != "" {
		return v == "1" || strings.EqualFold(v, "true")
	}
	return sem.EagerLSP
}
