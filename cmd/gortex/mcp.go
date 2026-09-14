package main

import (
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"time"

	"github.com/gofrs/flock"
	"github.com/spf13/cobra"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/daemon"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/llm/conversationlog"
	gortexmcp "github.com/zzet/gortex/internal/mcp"
	"github.com/zzet/gortex/internal/platform"
	"github.com/zzet/gortex/internal/savings"
	"github.com/zzet/gortex/internal/server"
	"github.com/zzet/gortex/internal/server/hub"
	"github.com/zzet/gortex/internal/serverstack"
)

var (
	mcpIndex           string
	mcpTransport       string
	mcpPort            int
	mcpBind            string
	mcpAuthToken       string
	mcpWatch           bool
	mcpServerAPI       bool
	mcpCORSOrigin      string
	mcpDebounce        int
	mcpTrack           []string
	mcpProject         string
	mcpCacheDir        string
	mcpEmbeddings      bool
	mcpEmbeddingsURL   string
	mcpEmbeddingsModel string
	mcpSemantic        bool
	mcpNoSemantic      bool
	mcpSemanticMode    string
	mcpNoDaemon        bool
	mcpNoCache         bool
	mcpForceProxy      bool
	mcpTools           string
	mcpToolsMode       string
)

var mcpCmd = &cobra.Command{
	Use:   "mcp",
	Short: "Start the MCP server (stdio transport)",
	RunE:  runMCP,
}

func init() {
	mcpCmd.Flags().StringVar(&mcpIndex, "index", "", "repository path to index on startup")
	mcpCmd.Flags().StringVar(&mcpTransport, "transport", "stdio", "transport: stdio")
	mcpCmd.Flags().IntVar(&mcpPort, "port", 8765, "port for HTTP server API when --server is set")
	mcpCmd.Flags().BoolVar(&mcpWatch, "watch", false, "keep graph in sync with filesystem changes")
	mcpCmd.Flags().IntVar(&mcpDebounce, "debounce", 150, "debounce delay in ms")
	mcpCmd.Flags().BoolVar(&mcpServerAPI, "server", false, "start HTTP server API alongside MCP stdio")
	mcpCmd.Flags().StringVar(&mcpBind, "bind", "127.0.0.1", "bind address for --server; requires --auth-token when not localhost")
	mcpCmd.Flags().StringVar(&mcpAuthToken, "auth-token", "", "bearer token for --server's /v1/* requests (fallback: $GORTEX_SERVER_TOKEN)")
	mcpCmd.Flags().StringVar(&mcpCORSOrigin, "cors-origin", "*", "allowed CORS origin for server API")
	mcpCmd.Flags().StringSliceVar(&mcpTrack, "track", nil, "additional repository paths to track")
	mcpCmd.Flags().StringVar(&mcpProject, "project", "", "active project name")
	mcpCmd.Flags().StringVar(&mcpCacheDir, "cache-dir", "", "directory for the side stores — notes, feedback, server id (default ~/.gortex/cache/)")
	mcpCmd.Flags().BoolVar(&mcpEmbeddings, "embeddings", false, "enable semantic search (built-in word vectors or transformer if compiled in)")
	mcpCmd.Flags().StringVar(&mcpEmbeddingsURL, "embeddings-url", "", "embedding API URL (e.g. http://localhost:11434 for Ollama)")
	mcpCmd.Flags().StringVar(&mcpEmbeddingsModel, "embeddings-model", "", "embedding model name (default: auto-detect)")
	mcpCmd.Flags().BoolVar(&mcpSemantic, "semantic", false, "enable semantic enrichment (SCIP, go/types, LSP)")
	mcpCmd.Flags().BoolVar(&mcpNoSemantic, "no-semantic", false, "disable semantic enrichment")
	mcpCmd.Flags().StringVar(&mcpSemanticMode, "semantic-mode", "typecheck", "Go analysis mode: typecheck or callgraph")
	mcpCmd.Flags().BoolVar(&mcpNoDaemon, "no-daemon", false, "deprecated no-op (warns when set); embedded mode requires mcp.allow_embedded in the user-level config")
	mcpCmd.Flags().BoolVar(&mcpNoCache, "no-cache", false, "deprecated no-op (warns when set); the graph is served from the sqlite store, which has no separate cache to disable")
	// Hidden rather than removed: an editor config that still passes the flag
	// must keep starting, but nothing should learn it from --help.
	_ = mcpCmd.Flags().MarkHidden("no-cache")
	mcpCmd.Flags().BoolVar(&mcpForceProxy, "proxy", false, "require a running daemon and proxy through it (error if unavailable)")
	mcpCmd.Flags().StringVar(&mcpTools, "tools", "", "restrict the published MCP tool surface to a preset: core (default)|full|readonly|edit|nav (optionally with ,+tool / ,-tool deltas). GORTEX_TOOLS overrides this")
	mcpCmd.Flags().StringVar(&mcpToolsMode, "tools-mode", "", "how a --tools preset hides tools: hide (remove from tools/list + block calls) or defer (keep reachable via tools_search). Default hide")
	rootCmd.AddCommand(mcpCmd)
}

var (
	legacyMCPFlagsWarned    bool
	probeDaemonAvailability = daemon.ProbeAvailability
)

// legacyMCPProxyReason explains compatibility flags that no longer choose the
// startup path. The --proxy flag is different: it can still forbid embedded
// mode when a caller requires the shared daemon.
const legacyMCPProxyReason = "`gortex mcp` proxies to the daemon (auto-starting it) " +
	"and only uses an embedded server when mcp.allow_embedded is enabled"

// legacyMCPFlags are the retired `gortex mcp` flags kept as permanent no-op
// compat shims, each with the reason it stopped doing anything. Removing one
// is not an option: on-disk editor configs are never migrated and cobra hard-
// errors on an unknown flag, so a deletion turns a stale mcp.json into a
// server that will not start. Order fixes the warning order.
var legacyMCPFlags = []struct{ name, reason string }{
	{"index", legacyMCPProxyReason},
	{"watch", legacyMCPProxyReason},
	{"no-daemon", legacyMCPProxyReason},
	{"no-cache", "the graph is served from the sqlite store, which has no separate cache to disable"},
}

// warnLegacyMCPFlags emits one stderr line per explicitly-set legacy flag.
// stderr only — stdout is the MCP JSON-RPC stream and a stray byte corrupts
// the protocol.
func warnLegacyMCPFlags(cmd *cobra.Command) {
	if legacyMCPFlagsWarned {
		return
	}
	legacyMCPFlagsWarned = true
	for _, lf := range legacyMCPFlags {
		if f := cmd.Flags().Lookup(lf.name); f != nil && f.Changed {
			fmt.Fprintf(os.Stderr, "[gortex] note: --%s is deprecated and ignored; %s.\n", lf.name, lf.reason)
		}
	}
}

// embeddedStoreDirPattern is the temp-directory name the embedded server's
// throwaway sqlite store lives in. Shared by the allocator and the reaper.
const embeddedStoreDirPattern = "gortex-mcp-store-*"

// embeddedStoreLockName is the advisory lock file every live embedded store
// holds for its process lifetime, inside its own store directory. It is the
// reaper's liveness proof: the directory mtime is not one, because sqlite
// writes update files INSIDE the directory without touching the directory's
// own mtime, so a session that has been serving for days looks as old as its
// first second.
const embeddedStoreLockName = "store.lock"

// staleEmbeddedStoreTTL is a cheap pre-filter in front of the lock probe, not
// the liveness test. It keeps a launch from opening a lock file for every
// recent sibling directory; the lock is what decides whether a candidate is
// actually abandoned.
const staleEmbeddedStoreTTL = 48 * time.Hour

// reapStaleEmbeddedStores removes abandoned embedded-store directories. The
// one-shot server deletes its own directory on exit, but a SIGKILL skips that
// and strands a store that can run to gigabytes.
//
// A candidate is removed only after this process non-blockingly acquires that
// directory's advisory lock. A live server holds that lock until it exits (the
// kernel drops it even on SIGKILL), so a failed acquisition means "in use" and
// the directory is left alone however old it looks. Best effort otherwise:
// every error is a debug line, never a reason to fail startup.
func reapStaleEmbeddedStores(logger *zap.Logger) {
	matches, err := filepath.Glob(filepath.Join(os.TempDir(), embeddedStoreDirPattern))
	if err != nil {
		logger.Debug("mcp: could not scan for stale embedded stores", zap.Error(err))
		return
	}
	cutoff := time.Now().Add(-staleEmbeddedStoreTTL)
	for _, dir := range matches {
		info, statErr := os.Stat(dir)
		if statErr != nil || !info.IsDir() || info.ModTime().After(cutoff) {
			continue
		}
		lock := flock.New(filepath.Join(dir, embeddedStoreLockName))
		locked, lockErr := lock.TryLock()
		if lockErr != nil {
			logger.Debug("mcp: could not probe embedded store lock", zap.String("dir", dir), zap.Error(lockErr))
			continue
		}
		if !locked {
			// Owned by a live one-shot server — nothing to reap here.
			continue
		}
		// TryLock created (or reopened) the lock file INSIDE dir, and
		// Windows refuses to delete a file that is still open — leaving the
		// handle up until after RemoveAll makes the reap a silent no-op
		// there. Unlock closes the handle, and releasing early is safe: a
		// store directory is never adopted, only freshly created by
		// newEmbeddedStorePath, so the only racer is another reaper, whose
		// RemoveAll on an already-removed directory succeeds.
		_ = lock.Unlock()
		rmErr := os.RemoveAll(dir)
		if rmErr != nil {
			logger.Debug("mcp: could not reap stale embedded store", zap.String("dir", dir), zap.Error(rmErr))
			continue
		}
		logger.Debug("mcp: reaped stale embedded store", zap.String("dir", dir))
	}
}

// newEmbeddedStorePath allocates the per-process sqlite store the embedded
// MCP server runs against, inside a fresh temp directory nothing else can
// name, and takes the directory's advisory lock so a later launch's reaper
// treats this store as live for as long as this process runs. It returns the
// store path and a func that releases the lock and removes the directory; the
// caller runs that only after the store handle is closed.
//
// A lock that cannot be taken is a hard failure rather than a warning: without
// it the store is indistinguishable from abandoned debris, and a session that
// outlives staleEmbeddedStoreTTL would have its database deleted underneath it.
func newEmbeddedStorePath() (string, func(), error) {
	dir, err := os.MkdirTemp("", embeddedStoreDirPattern)
	if err != nil {
		return "", nil, err
	}
	lock := flock.New(filepath.Join(dir, embeddedStoreLockName))
	locked, lockErr := lock.TryLock()
	if lockErr != nil || !locked {
		_ = os.RemoveAll(dir)
		if lockErr == nil {
			lockErr = fmt.Errorf("lock already held")
		}
		return "", nil, fmt.Errorf("lock embedded store dir %q: %w", dir, lockErr)
	}
	return filepath.Join(dir, "embedded.sqlite"), func() {
		_ = lock.Unlock()
		_ = os.RemoveAll(dir)
	}, nil
}

// loadEmbeddedMCPGlobalConfig enforces the machine-level permission for the
// one-shot MCP server. Repository config is deliberately not consulted: a
// checked-in .gortex.yaml must not be able to authorize extra processes.
func loadEmbeddedMCPGlobalConfig(path string) (*config.GlobalConfig, error) {
	global, err := config.LoadGlobal(path)
	if err != nil {
		return nil, fmt.Errorf("load global config %q for embedded MCP policy: %w", path, err)
	}
	if !global.MCP.AllowEmbedded {
		return nil, fmt.Errorf(
			"gortex daemon is unavailable and embedded MCP mode is disabled by default; run `gortex daemon start` or set `mcp.allow_embedded: true` in the machine-global config %q",
			path,
		)
	}
	return global, nil
}

func runMCP(cmd *cobra.Command, args []string) error {
	warnLegacyMCPFlags(cmd)

	// A boolean liveness check cannot distinguish an absent daemon from a
	// permission or broken-socket failure. Preserve that distinction before
	// startup selection so even an opted-in embedded server cannot mask a live
	// daemon or a system error.
	if err := probeDaemonAvailability(); err != nil && !daemon.ShouldFallBackToEmbedded(err) {
		return fmt.Errorf("check gortex daemon availability: %w", err)
	}

	// Daemon-first: ensure a daemon is up (auto-starting it under a
	// single-flight lock when GORTEX_AUTOSTART allows), then relay stdio
	// over its socket. The old stdin-TTY heuristic is gone — behavior is
	// identical from a terminal or a pipe given the same daemon state. The
	// legacy --no-daemon flag is an inert no-op (warned above); embedded mode
	// is available only through the machine-global opt-in checked below.
	decision := resolveDaemonDecision()
	switch decision {
	case daemonReady, daemonAutostarted:
		ran, proxyErr := runProxy(cmd.Context(), proxyToolSurface())
		if proxyErr != nil {
			return proxyErr
		}
		if ran {
			return nil
		}
		// Lost the daemon between ensure and dial (rare) — proceed to the
		// same explicit embedded-mode policy as every other unavailable case.
	}

	// Re-probe after an unavailable decision: a peer may have started the
	// daemon while we were deciding. Prefer that daemon over an embedded copy,
	// and preserve any non-recoverable socket error that appeared meanwhile.
	if decision == daemonUnavailable {
		if probeErr := probeDaemonAvailability(); probeErr == nil {
			ran, proxyErr := runProxy(cmd.Context(), proxyToolSurface())
			if proxyErr != nil {
				return proxyErr
			}
			if ran {
				return nil
			}
		} else if !daemon.ShouldFallBackToEmbedded(probeErr) {
			return fmt.Errorf("check gortex daemon availability: %w", probeErr)
		}
	}

	if ctx := cmd.Context(); ctx != nil {
		if err := ctx.Err(); err != nil {
			return err
		}
	}
	if mcpForceProxy {
		return fmt.Errorf("gortex daemon is unavailable and --proxy requires it; start the daemon with `gortex daemon start`")
	}

	global, err := loadEmbeddedMCPGlobalConfig(config.DefaultGlobalConfigPath())
	if err != nil {
		return err
	}

	logger := newLogger()
	defer func() { _ = logger.Sync() }()

	// We only reach here when no daemon is available (or autostart is off).
	// A bare `gortex mcp` with no --index used to serve an EMPTY embedded
	// graph — every tool returned nothing, which reads to the user as "gortex
	// is broken." So the fallback infers an index root from the launch cwd.
	// resolveEmbeddedIndex bounds that inference: an explicit --index wins,
	// an unsafe root or a directory that merely CONTAINS tracked repos is
	// refused rather than crawled, and an inferred root never gets the
	// repo-local notebook — a guess must not leave a .gortex/ directory in
	// whatever tree the MCP client happened to launch from.
	launchCWD, cwdErr := resolveLaunchCWD()
	if cwdErr != nil {
		launchCWD = ""
	}
	plan := resolveEmbeddedIndex(mcpIndex, launchCWD, global)
	mcpIndex = plan.Index
	switch {
	case plan.Refusal != "":
		fmt.Fprintf(os.Stderr, "[gortex] no daemon available; embedded server serving an empty graph: %s\n", plan.Refusal)
	case plan.Inferred:
		fmt.Fprintf(os.Stderr, "[gortex] no daemon available; embedded server indexing %s\n", mcpIndex)
	}

	// The embedded server runs in single-repo mode over --index: it
	// indexes that tree and serves the whole graph (no marker handshake,
	// no .gortex/workspace.toml). Multi-repo scoping is the daemon's job.
	cfg, err := config.Load(cfgFile)
	if err != nil {
		return err
	}
	// Resolve the embedded semantic decision: --no-semantic forces off,
	// --semantic forces on, otherwise semantic.enabled in config decides.
	cfg.Semantic.Enabled = !mcpNoSemantic && (mcpSemantic || cfg.Semantic.Enabled)
	// Fold --tools / --tools-mode into the mcp.tools config the server
	// stack reads (flag overrides config; GORTEX_TOOLS still overrides).
	applyToolPresetFlags(cfg, mcpTools, mcpToolsMode)

	// Side-store layout for the embedded path: every store partitions
	// per-repo under the indexed repo path, and the notebook is repo-local
	// (committed to git). When --cache-dir is unset, notes / memories still
	// persist via the shared cache dir's sidecar.
	sideStoreCacheDir := mcpCacheDir
	if sideStoreCacheDir == "" {
		sideStoreCacheDir = platform.CacheDir()
	}
	// The savings ledger is machine-global — the same sidecar database
	// every entry point writes and the `gortex savings` CLI reads.
	// --cache-dir deliberately does NOT relocate it: users set that flag
	// to move the notes / feedback side stores, and quietly splitting the
	// ledger away from the dashboard's default read path recreates the
	// empty-dashboard failure mode. Isolation (tests, sandboxes) comes
	// from XDG_DATA_HOME / XDG_CACHE_HOME, which both ledger paths
	// honour.

	// The embedded server needs a graph store of its own. It must be a
	// private temp file: it takes no store lock, so pointing it at the
	// shared default store would put a second unsynchronised writer on the
	// daemon's database. Removed on shutdown — the graph is rebuilt from
	// the tree on every launch. Sweep whatever earlier launches were killed
	// before they could remove theirs.
	reapStaleEmbeddedStores(logger)
	storePath, removeStoreDir, err := newEmbeddedStorePath()
	if err != nil {
		return fmt.Errorf("create embedded store: %w", err)
	}
	// Registered before the stack's Close so it runs after it: the sqlite
	// handle must be shut before the directory under it disappears.
	defer removeStoreDir()

	ss, err := serverstack.NewSharedServer(serverstack.SharedServerConfig{
		Lifecycle:   serverstack.LifecycleOneshot,
		Index:       mcpIndex,
		BackendPath: storePath,
		Config:      cfg,
		Logger:      logger,
		Version:     version,
		Embedder: serverstack.EmbedderRequest{
			FlagChanged: cmd.Flags().Changed("embeddings"),
			FlagEnabled: mcpEmbeddings,
			FlagURL:     mcpEmbeddingsURL,
			FlagModel:   mcpEmbeddingsModel,
		},
		ActiveProject: mcpProject,
		SemanticMode:  mcpSemanticMode,
		SideStores: serverstack.SideStores{
			NotesDir:     sideStoreCacheDir,
			NotesRepo:    mcpIndex,
			FeedbackDir:  mcpCacheDir,
			FeedbackRepo: mcpIndex,
			NotebookPath: plan.Notebook,
		},
		SavingsRepo: mcpIndex,
	})
	if err != nil {
		return fmt.Errorf("build server stack: %w", err)
	}
	defer ss.Close()

	g := ss.Graph
	idx := ss.Indexer
	cm := ss.ConfigMgr
	mi := ss.MultiIndexer
	srv := ss.MCP

	// Bound this process's savings window and commit it on the way out.
	// Registered AFTER `defer ss.Close()`, so LIFO runs it BEFORE the stack
	// teardown — covering every exit path from here on: the errCh return,
	// the SIGINT/SIGTERM return, and any error return in between. A SIGKILL
	// still loses whatever is buffered, which is why the bound is seconds
	// and a handful of events rather than the daemon's minute.
	defer installOneshotSavingsFlush(srv)()

	// Announce the degraded mode to the CLIENT, not just to stderr. An
	// MCP host that discards stderr — most of them — otherwise cannot
	// tell an embedded single-tree answer from a daemon-backed one, and
	// neither can the agent reading the results.
	srv.SetDegradedNote(embeddedModeNote(plan))

	// Persist the resolved active project so the MCP server and
	// set_active_project agree on the default scope.
	activeProject := mcpProject
	if activeProject == "" && cm != nil {
		activeProject = cm.Global().ActiveProject
	}
	if cm != nil {
		cm.Global().ActiveProject = activeProject
	}

	// Register --track repos for the background watch / track loop.
	if cm != nil {
		for _, trackPath := range mcpTrack {
			absPath, aerr := filepath.Abs(trackPath)
			if aerr != nil {
				fmt.Fprintf(os.Stderr, "[gortex] warning: could not resolve --track path %s: %v\n", trackPath, aerr)
				continue
			}
			if aerr := cm.Global().AddRepo(config.RepoEntry{Path: absPath}); aerr != nil {
				fmt.Fprintf(os.Stderr, "[gortex] warning: could not add --track repo %s: %v\n", absPath, aerr)
			}
		}
	}

	fmt.Fprintf(os.Stderr, "[gortex] MCP server ready (transport: %s)\n", mcpTransport)

	// Start server HTTP API if requested.
	if mcpServerAPI {
		authToken := mcpAuthToken
		if authToken == "" {
			authToken = os.Getenv("GORTEX_SERVER_TOKEN")
		}
		if authToken == "" {
			if !isLocalhostBind(mcpBind) {
				return fmt.Errorf("--bind %q requires --auth-token (or $GORTEX_SERVER_TOKEN); refusing to expose unauthenticated server on external interface", mcpBind)
			}
			fmt.Fprintln(os.Stderr, "[gortex] server: unauthenticated mode; localhost only")
		}

		serverHandler := server.NewHandler(srv.MCPServer(), g, version, logger)
		if cm != nil {
			serverHandler.SetConfigManager(cm)
		}
		if id, err := resolveServerID(mcpCacheDir); err == nil {
			serverHandler.SetServerID(id)
		} else {
			fmt.Fprintf(os.Stderr, "[gortex] server: id persistence disabled: %v\n", err)
		}
		// Conversation-log inspector: enable the /v1/conversations* routes
		// (opt-in sink) with the route-scoped DNS-rebind guard cooperating
		// with this server's auth token.
		serverHandler.SetConversationDir(conversationlog.DirFromEnv())
		authTokenForGuard := authToken
		serverHandler.SetConversationGuard(nil, func() string { return authTokenForGuard })
		handler := server.WithAuth(serverHandler, authToken)
		corsOpts := server.CORSOptions{AllowOrigins: []string{mcpCORSOrigin}}
		handler = server.WithCORS(handler, corsOpts)
		go func() {
			serverAddr := fmt.Sprintf("%s:%d", mcpBind, mcpPort)
			fmt.Fprintf(os.Stderr, "[gortex] server API at http://%s\n", serverAddr)
			if err := http.ListenAndServe(serverAddr, handler); err != nil && err != http.ErrServerClosed {
				fmt.Fprintf(os.Stderr, "[gortex] server API error: %v\n", err)
			}
		}()
	}

	// Start MCP stdio in a goroutine so we can do background init.
	errCh := make(chan error, 1)
	go func() {
		errCh <- srv.ServeStdio()
	}()

	// Background: index, watch, analyze — graph populates while MCP is live.
	go func() {
		if mcpIndex != "" {
			// The embedded server holds its graph for the life of the
			// process only: it indexes the tree on every launch. A
			// long-lived graph is what the daemon is for.
			fmt.Fprintf(os.Stderr, "[gortex] indexing %s...\n", mcpIndex)
			result, err := idx.Index(mcpIndex)
			if err != nil {
				fmt.Fprintf(os.Stderr, "[gortex] indexing failed: %v\n", err)
				return
			}
			fmt.Fprintf(os.Stderr, "[gortex] indexed %d files (%d nodes, %d edges) in %dms\n",
				result.FileCount, result.NodeCount, result.EdgeCount, result.DurationMs)
		}

		// Search backend is auto-updated via SearchProvider (idx.Search)

		// Pass contract registry to MCP server.
		// In multi-repo mode, merge all per-repo registries.
		if mi != nil {
			srv.SetContractRegistry(mi.MergedContractRegistry())
		} else if cr := idx.ContractRegistry(); cr != nil {
			srv.SetContractRegistry(cr)
		}

		// Start watcher if requested.
		if mcpWatch {
			wcfg := cfg.Watch
			wcfg.Enabled = true
			if mcpDebounce > 0 {
				wcfg.DebounceMs = mcpDebounce
			}

			watcher, err := indexer.NewWatcher(idx, wcfg, logger)
			if err != nil {
				fmt.Fprintf(os.Stderr, "[gortex] watcher setup failed: %v\n", err)
				return
			}

			watchPaths := wcfg.Paths
			if len(watchPaths) == 0 && mcpIndex != "" {
				watchPaths = []string{mcpIndex}
			}
			if len(watchPaths) == 0 {
				watchPaths = []string{"."}
			}

			if err := watcher.Start(watchPaths); err != nil {
				fmt.Fprintf(os.Stderr, "[gortex] watcher start failed: %v\n", err)
				return
			}
			srv.SetWatcher(watcher)

			// Create hub for fan-out of watcher events.
			eventHub := hub.New()
			go eventHub.Run(watcher.Events())

			fmt.Fprintf(os.Stderr, "[gortex] watch mode active\n")
		}
	}()

	// Handle graceful shutdown.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, platform.ShutdownSignals()...)

	select {
	case err := <-errCh:
		return err
	case sig := <-sigCh:
		fmt.Fprintf(os.Stderr, "\n[gortex] received %s, shutting down\n", sig)
		return nil
	}
}

// installOneshotSavingsFlush tightens the token-savings ledger's coalescing
// window for a one-shot stdio server and returns the flush to defer.
//
// The coalescing that makes an idle agent session cheap was built for the
// daemon: a long-lived process where losing the last minute of accounting on
// a crash is nothing against the ~37 KB sidecar transaction every read-only
// tool call used to pay. A `gortex mcp` / `gortex server` process is the
// other shape — MCP hosts SIGKILL their stdio servers, and a whole session
// can be shorter than the default window, which is the flat-file era's
// "permanently empty under SIGKILLing MCP clients" bug all over again. So the
// window here is savings.OneshotFlushInterval / savings.OneshotFlushMax, and
// every graceful exit commits explicitly rather than relying on the stack's
// teardown chain reaching its savings step.
//
// Returns a no-op closure when the server has no ledger wired (embedded
// fixtures, persistence disabled), so the call site can defer it blind.
func installOneshotSavingsFlush(srv *gortexmcp.Server) func() {
	if srv == nil {
		return func() {}
	}
	srv.SetSavingsFlushBounds(savings.OneshotFlushInterval, savings.OneshotFlushMax)
	return func() { _ = srv.FlushSavings() }
}
