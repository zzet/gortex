package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jedib0t/go-pretty/v6/table"
	"github.com/jedib0t/go-pretty/v6/text"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/daemon"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/llm/conversationlog"
	"github.com/zzet/gortex/internal/platform"
	"github.com/zzet/gortex/internal/progress"
	"github.com/zzet/gortex/internal/runtimeactivity"
	"github.com/zzet/gortex/internal/server"
	"github.com/zzet/gortex/internal/server/hub"
	"github.com/zzet/gortex/internal/tui"
)

var (
	daemonDetach     bool
	daemonTail       int
	daemonEmbeddings bool
	// daemonEmbeddingsChanged records whether `--embeddings` was given
	// explicitly on `gortex daemon start`. buildDaemonState reads it
	// (the function has no *cobra.Command of its own) to decide whether
	// the flag overrides the `embedding:` config block. Set once in
	// runDaemonStart before buildDaemonState runs.
	daemonEmbeddingsChanged bool
	// daemonEmbeddingsURL / daemonEmbeddingsModel mirror `gortex mcp`'s
	// --embeddings-url / --embeddings-model so the daemon can be started with an
	// explicit OpenAI-compatible (or Ollama) embedding API. A non-empty URL forces
	// the api provider in ResolveEmbedder, overriding the embedding: config block.
	daemonEmbeddingsURL         string
	daemonEmbeddingsModel       string
	daemonStatusWatch           bool
	daemonStatusInterval        time.Duration
	daemonHTTPAddr              string
	daemonHTTPAuthToken         string
	daemonHTTPAllowedOrigins    []string
	daemonHTTPCORSOrigin        string
	daemonHTTPConversationAllow []string
	daemonBackend               string
	daemonBackendPath           string
	daemonTools                 string
	daemonToolsMode             string
	// daemonBackendBufferPoolMBIgnored is the sink for the retired
	// --backend-buffer-pool-mb flag. Nothing reads it: SQLite sizes its page
	// cache via a pragma, so there is no advisory cap to honour.
	daemonBackendBufferPoolMBIgnored uint64
)

var daemonCmd = &cobra.Command{
	Use:   "daemon",
	Short: "Manage the long-living Gortex daemon",
	Long: `The daemon holds the graph for all tracked repositories and serves every
MCP client (Claude Code, Cursor, Kiro, ...) plus the CLI from one shared
index.

` + "`gortex mcp`" + ` connects to and may auto-start this daemon. If no compatible
daemon is available, it exits unless ` + "`mcp.allow_embedded`" + ` is enabled
in the user-level config.`,
}

// RunE is wired in init() rather than here: runDaemonStart reaches back for
// daemonStartCmd's flag set (to know what the re-exec'd child accepts), and
// naming runDaemonStart in this initializer would close an initialization
// cycle the compiler rejects.
var daemonStartCmd = &cobra.Command{
	Use:   "start",
	Short: "Start the daemon",
}

var daemonStopCmd = &cobra.Command{
	Use:   "stop",
	Short: "Stop the daemon gracefully (waits for the store to close cleanly)",
	RunE:  runDaemonStop,
}

var daemonRestartCmd = &cobra.Command{
	Use:   "restart",
	Short: "Stop and start the daemon (preserves tracked repos)",
	RunE:  runDaemonRestart,
}

var daemonReloadCmd = &cobra.Command{
	Use:   "reload",
	Short: "Re-read config and pick up new or removed repos without restart",
	RunE:  runDaemonReload,
}

// daemonStatusExact opts `daemon status` out of the maintained per-repo
// counters and into a full recount. See the flag help for the cost.
var daemonStatusExact bool

var daemonStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show daemon PID, uptime, tracked repos, memory, sessions",
	RunE:  runDaemonStatus,
}

var daemonLogsCmd = &cobra.Command{
	Use:   "logs",
	Short: "Tail the daemon log file",
	RunE:  runDaemonLogs,
}

func init() {
	daemonStartCmd.RunE = runDaemonStart
	daemonStatusCmd.Flags().BoolVar(&daemonStatusExact, "exact", false,
		"recount nodes and edges from the store instead of reading the counters "+
			"the indexer maintains, and repair any that have drifted. Proportional "+
			"to the whole corpus — tens of seconds on a large store")

	daemonStartCmd.Flags().BoolVar(&daemonDetach, "detach", false,
		"fork to background after starting (logs to the daemon log file — see `gortex daemon logs`)")
	daemonStartCmd.Flags().BoolVar(&daemonEmbeddings, "embeddings", false,
		"load a semantic embedding provider (opt-in — adds ~87 MB model download on first use and ~60 ms/symbol warmup)")
	daemonStartCmd.Flags().StringVar(&daemonEmbeddingsURL, "embeddings-url", "",
		"OpenAI-compatible (or Ollama) embedding API base URL (e.g. https://api.openai.com/v1). A non-empty URL forces the api provider, overriding the embedding: config. Key via $GORTEX_EMBEDDINGS_API_KEY or $OPENAI_API_KEY (openai.com only).")
	daemonStartCmd.Flags().StringVar(&daemonEmbeddingsModel, "embeddings-model", "",
		"embedding model for --embeddings-url (default: auto-detect — text-embedding-3-small for OpenAI, nomic-embed-text for Ollama)")
	daemonStartCmd.Flags().StringVar(&daemonHTTPAddr, "http-addr", "",
		"also expose the MCP 2026 Streamable HTTP transport on this TCP address (e.g. 127.0.0.1:7411); empty disables")
	daemonStartCmd.Flags().StringVar(&daemonHTTPAuthToken, "http-auth-token", "",
		"bearer token required on every Streamable HTTP request (default: read $GORTEX_DAEMON_HTTP_TOKEN; empty allows unauthenticated localhost binds)")
	daemonStartCmd.Flags().StringSliceVar(&daemonHTTPAllowedOrigins, "http-allowed-origin", nil,
		"web origin permitted to call /mcp cross-origin (e.g. https://ui.example); repeatable. Empty refuses every browser origin — a loopback bind is reachable from any page the user visits")
	daemonStartCmd.Flags().StringVar(&daemonHTTPCORSOrigin, "cors-origin", "*",
		"allowed CORS origin for the HTTP surface (use '*' for any); applies to both /mcp and /v1 when --http-addr is set")
	daemonStartCmd.Flags().StringSliceVar(&daemonHTTPConversationAllow, "conversation-host", nil,
		"extra Host values (beyond loopback) the conversation-log inspector accepts without a token; repeatable")
	daemonStartCmd.Flags().StringVar(&daemonBackend, "backend", "sqlite",
		"storage backend: sqlite (pure-Go embedded SQL, persists to --backend-path so warm restarts skip re-indexing). It is the only backend; point --backend-path at a throwaway file for a store that does not outlive the run")
	daemonStartCmd.Flags().StringVar(&daemonBackendPath, "backend-path", "",
		"path to the store file (its parent directory is created if absent). Defaults to ~/.gortex/store/store.sqlite")
	daemonStartCmd.Flags().Uint64Var(&daemonBackendBufferPoolMBIgnored, "backend-buffer-pool-mb", 0,
		"deprecated no-op; sqlite sizes its page cache via a pragma, so there is no advisory cap to set")
	// Hidden rather than removed: cobra hard-errors on an unknown flag, so a
	// deletion would break existing daemon-start scripts and the detach
	// re-exec path. Nothing should learn it from --help.
	_ = daemonStartCmd.Flags().MarkHidden("backend-buffer-pool-mb")
	daemonStartCmd.Flags().StringVar(&daemonTools, "tools", "",
		"restrict the published MCP tool surface to a preset: core (default)|full|readonly|edit|nav (optionally with ,+tool / ,-tool deltas). GORTEX_TOOLS overrides this")
	daemonStartCmd.Flags().StringVar(&daemonToolsMode, "tools-mode", "",
		"how a --tools preset hides tools: hide (remove from tools/list + block calls) or defer (keep reachable via tools_search). Default hide")
	daemonLogsCmd.Flags().IntVarP(&daemonTail, "tail", "n", 50,
		"show only the last N log lines")
	daemonStatusCmd.Flags().BoolVarP(&daemonStatusWatch, "watch", "w", false,
		"continuously refresh the status until interrupted (alt-screen buffer)")
	daemonStatusCmd.Flags().DurationVar(&daemonStatusInterval, "interval", 2*time.Second,
		"refresh interval in --watch mode (clamped to >=200ms)")

	daemonCmd.AddCommand(daemonStartCmd)
	daemonCmd.AddCommand(daemonStopCmd)
	daemonCmd.AddCommand(daemonRestartCmd)
	daemonCmd.AddCommand(daemonReloadCmd)
	daemonCmd.AddCommand(daemonStatusCmd)
	daemonCmd.AddCommand(daemonLogsCmd)
	rootCmd.AddCommand(daemonCmd)
}

// runDaemonStart starts the daemon in foreground (default) or detached
// (when --detach is passed). Detach does a self-exec: re-runs this binary
// with GORTEX_DAEMON_CHILD=1 set, which the inner exec picks up and runs
// the actual serve loop.
func runDaemonStart(cmd *cobra.Command, _ []string) error {
	// An explicit start (user or supervisor) supersedes a prior `daemon stop` —
	// clear the stay-down mark so autostart works again. The autostart-spawned
	// child (GORTEX_DAEMON_CHILD=1) must NOT clear it: that would let an
	// in-flight autostart erase a stop-intent the user wrote in the meantime.
	if os.Getenv("GORTEX_DAEMON_CHILD") != "1" {
		daemon.ClearStopIntent()
	}
	if isDaemonRunning() {
		return fmt.Errorf("daemon already running (socket: %s)", daemon.SocketPath())
	}
	// IsRunning only probes the socket. A daemon that is mid-shutdown — or
	// one whose socket wedged — still owns the PID file and, crucially, still
	// holds the store's on-disk lock. Starting over the top of it makes the
	// backend open fail with an opaque "failed to open database" lock
	// conflict, so refuse early with the PID and an actionable next step. The
	// detached child reaches here too, but it hasn't written its own PID file
	// yet (that happens in the serve loop), so this can't false-positive on
	// the daemon we're in the middle of starting.
	if pid, ok := daemon.RunningPID(); ok {
		return fmt.Errorf("daemon already running (pid %d) — stop it with `gortex daemon stop`, or use `gortex daemon restart`", pid)
	}
	if daemonDetach && os.Getenv("GORTEX_DAEMON_CHILD") != "1" {
		return spawnDetachedDaemon(detachedDaemonArgs(cmd.Flags(), daemonStartAcceptedFlags()))
	}
	logger := newLogger()

	// Raise the per-process file-descriptor cap as early as possible.
	// fsnotify holds one FD per watched directory on Linux and one FD
	// per directory plus every file inside it on macOS, so a multi-repo
	// install easily blows past the inherited soft cap (256 on macOS,
	// 1024 on most Linuxes) and surfaces as "accept: too many open
	// files" once the daemon is hot.
	if fdl, err := daemon.RaiseFDLimit(); err != nil {
		logger.Warn("daemon: could not raise file-descriptor limit", zap.Error(err))
	} else {
		logger.Info("daemon: file-descriptor cap",
			zap.Uint64("soft", fdl.Soft), zap.Uint64("hard", fdl.Hard))
	}

	srv := daemon.New(daemon.SocketPath(), canonicalVersion(), logger)

	// Record whether `--embeddings` was set explicitly so
	// buildDaemonState can let it override the `embedding:` config
	// block. `--detach` forwards the flag verbatim (including
	// `--embeddings=false`), so the child sees the same explicit-ness
	// the parent did.
	daemonEmbeddingsChanged = cmd.Flags().Changed("embeddings")

	// Fast path: open the store + wire the indexer and MCP server. The
	// per-repo TrackRepoCtx loop and MultiWatcher init are deferred to
	// warmupDaemonState below so the socket opens immediately instead
	// of waiting 30–60s for contract re-extraction across every tracked
	// repo.
	state, err := buildDaemonState(logger)
	if err != nil {
		return fmt.Errorf("build daemon state: %w", err)
	}

	// Install the standing soft memory limit now — logging and config are
	// up and no warmup / indexing has allocated yet, so the cold-index
	// window's temporary override restores to this value rather than to
	// "no limit" (see applyStandingMemoryLimit).
	var daemonMemLimit string
	if gc := state.configManager.Global(); gc != nil {
		daemonMemLimit = gc.Daemon.MemoryLimit
	}
	applyStandingMemoryLimit(logger, daemonMemLimit)

	controller := &realController{
		graph:         state.graph,
		indexer:       state.indexer,
		multiIndexer:  state.multiIndexer,
		configManager: state.configManager,
		logger:        logger,
	}
	if state.mcpServer != nil {
		srv := state.mcpServer
		controller.toolSurface = func() (string, string, int) {
			preset, mode := srv.ActivePreset()
			return preset, mode, srv.LearnedToolCount()
		}
	}
	// Teardown is wired into every exit path, not just the control-socket
	// one. A SIGINT/SIGTERM is handled inside the daemon server: it calls
	// Server.Shutdown directly, Serve returns, and the controller's hook is
	// never reached — so installing the chain only on the hook meant a
	// signalled daemon skipped watcher shutdown, the savings flush, and the
	// final WAL checkpoint entirely. Both paths now run the same
	// once-guarded func, and the deferred call covers whichever exit
	// actually happens.
	runTeardown := installDaemonTeardown(controller, controller.StopWatcher, func() error {
		// Nothing has to be serialized here: per-file mtimes live in the
		// FileMtime sidecar table, contract records ride on
		// KindContract.Meta, and the vector index is persisted by the
		// backend itself. Warm restart reads everything it needs straight
		// from the on-disk store.
		//
		// The shared stack's teardown chain flushes the savings store and
		// closes the backend handle, checkpointing the sqlite WAL.
		if state.shared != nil {
			return state.shared.Close()
		}
		if state.mcpServer != nil {
			return state.mcpServer.FlushSavings()
		}
		return nil
	})
	defer runTeardown()
	srv.Controller = controller
	// Surface warmup state on the handshake ack: a proxy / CLI that connects
	// during the (minutes-long) warmup should know the graph is still filling
	// instead of guessing. controller.IsReady() is authoritative for the bool;
	// the readiness broadcaster carries the fine-grained phase name.
	srv.Ready = func() (bool, string) {
		ready := controller.IsReady()
		phase := "warming"
		if ready {
			phase = "ready"
		}
		if state.mcpServer != nil {
			if p, _ := state.mcpServer.ReadinessPhase(); p != "" {
				phase = p
			}
		}
		return ready, phase
	}
	disp := newMCPDispatcher(state.mcpServer, state.multiIndexer, logger)
	// The local executor + the dispatcher's SetRouter are handed to the
	// controller so ControlProxy can build/publish/tear-down the router
	// live (gortex proxy on/off/add/remove) without a daemon restart.
	localExec := newLocalToolExecutor(state.mcpServer, logger)
	controller.localExecute = localExec
	controller.publishRouter = disp.SetRouter
	// Wire the multi-server router into the daemon dispatcher when
	// servers.toml exists. Local-only
	// daemons (no servers.toml) leave router=nil and dispatch flows
	// straight to the in-process MCP server unchanged.
	if scfg, scfgErr := daemon.LoadServersConfig(""); scfgErr == nil && scfg != nil && len(scfg.Server) > 0 {
		rosters := daemon.NewWorkspaceRosterCache(60 * time.Second)
		// Local identity is a reserved sentinel, never DefaultServer().Slug:
		// a remote marked default=true must still be proxied to, not
		// treated as the daemon's own graph.
		router := daemon.NewRouter(daemon.RouterConfig{
			Servers:      scfg,
			Rosters:      rosters,
			LocalSlug:    daemon.LocalServerSentinel,
			LocalExecute: localExec,
			Logger:       logger,
			Federation:   resolveFederationConfig(),
		})
		disp.SetRouter(router)
		controller.liveRouter = router
		logger.Info("daemon: multi-server router wired",
			zap.Int("servers", len(scfg.Server)), zap.String("local_slug", daemon.LocalServerSentinel))
		// Cross-daemon proxy-edge minting: when federation.edges is on,
		// install the evidence prober on the indexer's resolver (so
		// cross-repo resolution mints proxy edges) and keep the hydrator
		// for the read path. No-op when the flag is off.
		if hyd := daemon.WireRemoteStitch(router, state.multiIndexer, state.graph, resolveFederationEdgesConfig(), logger); hyd != nil {
			state.proxyHydrator = hyd
			if state.mcpServer != nil {
				state.mcpServer.SetProxyHydrator(hyd.Hydrate)
			}
			logger.Info("daemon: federation proxy edges enabled (proxy minting + hydration)")
		}
	} else if scfgErr != nil {
		logger.Warn("daemon: servers.toml load error (running single-server)", zap.Error(scfgErr))
	}
	// Bridge the session proxy-toggle MCP tools to the daemon's
	// per-session overrides + the live router roster. The router
	// accessor is dynamic so a ControlProxy swap is reflected.
	if state.mcpServer != nil {
		state.mcpServer.SetRemoteOverrideSink(&sessionRemoteOverrideSink{
			sessions: srv.Sessions(),
			router:   disp.Router,
		})
	}
	srv.MCPDispatcher = disp

	// Event hub feeding the /v1 REST surface's graph-change SSE stream
	// (/v1/events). Created lazily when the HTTP surface is enabled below
	// and fed from the MultiWatcher once warmup attaches it — without this
	// the daemon's /v1/events would only ever emit the "watch mode not
	// active" frame even while the daemon is actively re-indexing changed
	// files, leaving the REST surface short of the former `gortex server`.
	var v1EventHub *hub.Hub

	// Optional MCP 2026 Streamable HTTP transport. Off by default
	// (--http-addr unset) so a fresh `gortex daemon start` keeps
	// the unix-socket-only behaviour every existing client already
	// expects. When set, the daemon mounts /mcp on the supplied
	// TCP address using the in-process streamable.Transport;
	// per-request session state is replayed out of an in-memory
	// store so multiple workers / reverse-proxies could later share
	// it. The auth token is mandatory for non-localhost binds —
	// exposing an unauthenticated MCP server on an external
	// interface is a footgun, not a feature.
	if daemonHTTPAddr != "" {
		token := daemonHTTPAuthToken
		if token == "" {
			token = os.Getenv("GORTEX_DAEMON_HTTP_TOKEN")
		}
		if err := httpTokenRequirementError(daemonHTTPAddr, token); err != nil {
			return err
		}
		// Resolve the expected token per request so $GORTEX_DAEMON_HTTP_TOKEN
		// can be rotated without restarting the daemon. The flag, when set,
		// is fixed for the process lifetime and wins; otherwise the env var
		// is re-read on every request.
		tokenFn := func() string {
			if daemonHTTPAuthToken != "" {
				return daemonHTTPAuthToken
			}
			return os.Getenv("GORTEX_DAEMON_HTTP_TOKEN")
		}
		// Router was already wired into the dispatcher above; reuse
		// it here so the streamable transport sees the same proxy
		// fan-out for cross-workspace tool calls.
		var router *daemon.Router
		if r := disp.Router(); r != nil {
			router = r
		}
		streamH := buildDaemonStreamableHandler(disp, srv.Sessions(), router, logger, tokenFn)

		// Mount the /v1 REST surface (the former `gortex server`) on the
		// same listener so a single daemon process serves both the MCP
		// Streamable transport and the REST API that gortex-cloud and the
		// web UI consume. The daemon already owns every dependency the
		// handler needs — the MCP server, graph, config manager, overlay
		// manager, and federation router — so this is pure composition.
		v1 := server.NewHandler(state.mcpServer.MCPServer(), state.graph, version, logger)

		if state.configManager != nil {
			v1.SetConfigManager(state.configManager)
		}
		if id, idErr := resolveServerID(platform.DataDir()); idErr == nil {
			v1.SetServerID(id)
		}
		if state.overlays != nil {
			v1.SetOverlayManager(state.overlays)
		}
		if router != nil {
			v1.SetRouter(router)
		}
		// Wire a graph-change event hub so /v1/events streams real events.
		// The hub is fed from the MultiWatcher once warmup attaches it
		// below; until then it has no source and /v1/events keepalives.
		v1EventHub = hub.New()
		v1.SetEventHub(v1EventHub)
		// Conversation-log inspector. The /v1/conversations* routes read
		// the opt-in sink's JSONL (raw LLM I/O), so they carry a
		// route-scoped DNS-rebind guard that cooperates with the existing
		// auth token: loopback / allowlisted hosts OR a valid token pass.
		// The directory resolves through the same env helper the sink
		// writer uses, so reader and writer always agree.
		v1.SetConversationDir(conversationlog.DirFromEnv())
		v1.SetConversationGuard(daemonHTTPConversationAllow, tokenFn)

		srv.HTTPHandler = composeDaemonHTTPHandler(streamH, v1, tokenFn, daemonHTTPCORSOrigin)
		srv.HTTPAddr = daemonHTTPAddr
		logger.Info("daemon: HTTP surface configured (/mcp + /v1)",
			zap.String("addr", daemonHTTPAddr),
			zap.Bool("authenticated", token != ""),
			zap.String("cors_origin", daemonHTTPCORSOrigin))
	}

	// Opt-in pprof endpoint. No-op unless GORTEX_DAEMON_PPROF_ADDR is
	// set — keeps profiling off by default so the daemon doesn't hand
	// its heap to anything on localhost.
	startPProfIfEnabled(logger)

	// Wire the daemon-health snapshot fn into the MCP server's
	// healthBroadcaster. Captures the live controller, daemon
	// server (for session count), and multi indexer so every periodic
	// tick reflects current state. Stopped in the deferred shutdown
	// below so the ticker goroutine doesn't outlive the process.
	daemonStart := time.Now()
	if state.mcpServer != nil {
		srvCapture := srv
		stateCapture := state
		controllerCapture := controller
		state.mcpServer.AttachHealthSnapshot(func() map[string]any {
			return buildDaemonHealthSnapshot(daemonStart, controllerCapture, stateCapture, srvCapture)
		})
	}
	defer func() {
		if state.mcpServer != nil {
			state.mcpServer.StopHealthBroadcaster()
		}
	}()

	// First workspace_readiness phase — the store is open and the daemon
	// state is built, but warmup hasn't started yet. The phase name is
	// part of the published readiness sequence clients order against.
	publishReadinessPhase(state, "snapshot_loaded", false, nil)

	// Periodic reconciliation — the "janitor". Walks each tracked repo
	// and runs IncrementalReindexPaths to evict files deleted offline and
	// re-index files whose mtime changed. Insurance against gaps in
	// fsnotify coverage (inotify watch limits, NFS mounts, kernel
	// event-queue overflow). Default interval 1 h; override via
	// GORTEX_RECONCILE_INTERVAL (a Go duration string, e.g. "15m").
	// Set to "0" to disable.
	stopJanitor := startReconcileJanitor(state.multiIndexer, reconcileInterval(), logger)
	defer stopJanitor()

	if err := srv.Listen(); err != nil {
		return err
	}
	// Publish the choices an out-of-band CLI cannot otherwise discover. The
	// store path is the one that matters: `gortex repos` reads the freshness
	// rows straight out of the store file, and a daemon started with
	// --backend-path put them somewhere the platform default does not name.
	// Advisory — a daemon that cannot write its record still serves, callers
	// just fall back to the default path.
	if err := daemon.WriteRuntimeState(daemon.RuntimeState{BackendPath: state.backendPath}); err != nil {
		logger.Warn("daemon: could not record runtime state", zap.Error(err))
	}
	fmt.Fprintf(cmd.ErrOrStderr(),
		"[gortex daemon] listening on %s (pid %d)\n",
		daemon.SocketPath(), os.Getpid())

	// Warmup runs in the background: re-index tracked repos, extract
	// contracts, attach file watchers. The daemon is already reachable
	// on the socket at this point, so clients can connect and start
	// issuing queries while this work continues. Queries against
	// not-yet-re-indexed repos are served from the persisted store — they
	// just won't reflect files that changed since the daemon last shut
	// down until warmup gets to that repo.
	go func() {
		runtimeactivity.Begin("warmup")
		defer func() {
			runtimeactivity.End("warmup")
			releaseMemoryToOS(logger, "warmup_complete")
		}()

		start := time.Now()
		logger.Info("daemon: warmup starting")
		// markReady fires once references are resolved and the graph is
		// queryable — ahead of the slow enrichment pass — so clients can
		// start issuing find_usages / get_callers immediately. Enrichment
		// continues in this goroutine afterward and finishes at MarkEnriched.
		// Once-guarded: the warmup fires it from inside the master resolver
		// (at compute-done, the earliest queryable point) AND from its
		// unconditional post-resolve fallback, whichever comes first.
		var queryableElapsed time.Duration
		markReady := sync.OnceFunc(func() {
			elapsed := time.Since(start)
			queryableElapsed = elapsed
			controller.MarkReady(elapsed)
			logger.Info("daemon: graph queryable", zap.Duration("warmup", elapsed))
			publishReadinessPhase(state, "ready", true, map[string]any{
				"queryable":      true,
				"warmup_seconds": int64(elapsed.Seconds()),
				"warmup_ms":      elapsed.Milliseconds(),
			})
		})
		mw, warmup := warmupDaemonState(state, logger, markReady)
		controller.AttachWatcher(mw)
		// Drive the /v1/events SSE stream from the MultiWatcher. The hub is
		// the only consumer of mw.Events() (SetWatcher reads History(), not
		// the channel), so this can't starve any other reader. No-op when
		// the HTTP surface is disabled (v1EventHub stays nil).
		if v1EventHub != nil && mw != nil {
			go v1EventHub.Run(mw.Events())
		}
		// Community detection and process discovery are a whole-graph pass
		// costing minutes on a large workspace, and most sessions never ask
		// for them. Running it here delayed readiness and kept the result
		// resident for every daemon whether or not anything read it, so the
		// pass is now started by the first consumer that needs it: those
		// tools answer with a retry hint while it runs in the background
		// instead of the old "run index_repository first", which was
		// misleading against a fully populated graph.
		if state.mcpServer != nil {
			publishReadinessPhase(state, "analysis_deferred", true, map[string]any{
				"reason": "analysis runs on first use",
			})
			// Co-change pre-warm: fire the git-history mine in the
			// background so the first user-visible
			// find_co_changing_symbols / search-rerank call sees a
			// populated cache. On a persistent backend the mine is
			// dominated by the AllNodes + per-pair AddEdge disk-persist
			// step that mineCoChange already defers into its own
			// goroutine — but even the git log itself can take 10–30s
			// on a large history, and we want that off every request
			// path.
			state.mcpServer.PrewarmCoChange()
		}
		elapsed := time.Since(start)
		controller.MarkEnriched(elapsed)
		logger.Info("daemon: enrichment complete", zap.Duration("warmup", elapsed))
		publishReadinessPhase(state, "enrichment_complete", true, map[string]any{
			"enriched":       true,
			"warmup_seconds": int64(elapsed.Seconds()),
			"warmup_ms":      elapsed.Milliseconds(),
		})
		logWarmupSummary(logger, warmup, queryableElapsed, elapsed)
		// Audit repo ownership once the graph has settled. Runs here, after
		// every reconcile and derived pass, so it observes the state the
		// daemon will actually serve rather than a mid-warmup transient.
		logRepoOwnershipAudit(state.graph, logger)
		// Carry stored note / memory symbol anchors onto prefixed node ids.
		// Also here, and for the same reason: the rewrite decides by asking
		// the graph which ids resolve, so it needs the settled graph. Marked
		// per repo in the sidecar, so this is a no-op on every later start.
		if state.mcpServer != nil {
			state.mcpServer.MigrateSymbolAnchors(logger)
		}
	}()

	return srv.Serve()
}

// installDaemonTeardown wires one once-guarded teardown into both of the
// daemon's exit paths and returns the func the caller must defer.
//
// The two paths never meet on their own. A `gortex daemon stop` arrives over
// the control socket and reaches the controller's shutdown hook; a
// SIGINT/SIGTERM is handled inside the daemon server, which shuts the listener
// down directly and lets Serve return without the controller hearing about it.
// Whichever fires, the same func runs — and sync.Once makes the second call a
// no-op, so a signalled daemon whose controller shutdown also lands does not
// close the store twice.
//
// stopWatcher runs first so no late event races the close — the backend should
// be quiescent when it closes, not being mutated by an in-flight re-index. It
// is passed explicitly rather than reached through the controller so the
// ordering contract is visible at the call site. The controller's StopWatcher
// is lock-free by design: this is the first thing a stop does, and reading the
// watcher under the coarse controller mutex would queue it behind the
// long-running track / reload / enrichment the user is trying to end.
//
// closeShared is the stack teardown: the savings flush and the backend close
// that checkpoints the sqlite WAL.
func installDaemonTeardown(controller *realController, stopWatcher func(), closeShared func() error) func() {
	teardown := newDaemonTeardown(stopWatcher, closeShared)
	controller.setShutdownHook(teardown)
	return func() { _ = teardown() }
}

// newDaemonTeardown returns the once-guarded teardown itself. Split from the
// wiring so the exactly-once contract can be exercised on both call paths
// without standing up a daemon.
func newDaemonTeardown(stopWatcher func(), closeShared func() error) func() error {
	var (
		once sync.Once
		err  error
	)
	return func() error {
		once.Do(func() {
			if stopWatcher != nil {
				stopWatcher()
			}
			if closeShared != nil {
				err = closeShared()
			}
		})
		// Later callers see the same outcome the run produced rather than a
		// misleading nil: whoever exits second is often the one reporting.
		return err
	}
}

// reconcileInterval returns the janitor tick interval, defaulting to 1
// hour. GORTEX_RECONCILE_INTERVAL overrides; "0" or "off" disables the
// janitor entirely (returns 0, which startReconcileJanitor treats as
// a no-op). Malformed values fall back to the default with a warning
// handled by the caller via the zero-return sentinel behaviour.
func reconcileInterval() time.Duration {
	raw := os.Getenv("GORTEX_RECONCILE_INTERVAL")
	if raw == "" {
		return time.Hour
	}
	if raw == "0" || raw == "off" {
		return 0
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d < 0 {
		return time.Hour
	}
	return d
}

// startReconcileJanitor launches a background goroutine that, on every
// interval tick, garbage-collects the index of any linked git worktree
// whose root directory has vanished from disk and then calls
// MultiIndexer.ReconcileAll. interval=0 is a no-op; the returned stop
// function can be called unconditionally.
//
// The worktree GC runs *before* ReconcileAll on purpose: a removed
// worktree's root no longer exists, so ReconcileAll's IncrementalReindexPaths
// would only error on the missing path without evicting anything.
// Pruning the vanished worktrees first keeps the reconcile sweep
// working on live repos and stops a deleted worktree's snapshot slot
// and graph nodes from leaking forever.
func startReconcileJanitor(mi *indexer.MultiIndexer, interval time.Duration, logger *zap.Logger) func() {
	if mi == nil || interval <= 0 {
		logger.Info("daemon: reconcile janitor disabled")
		return func() {}
	}
	stop := make(chan struct{})
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		logger.Info("daemon: reconcile janitor running", zap.Duration("interval", interval))
		for {
			select {
			case <-t.C:
				gcedCount, reconciled := func() (int, int) {
					runtimeactivity.Begin("reconcile")
					defer runtimeactivity.End("reconcile")

					gced := mi.GCVanishedWorktrees()
					if len(gced) > 0 {
						logger.Info("janitor: pruned vanished worktrees",
							zap.Int("count", len(gced)))
					}
					results := mi.ReconcileAll()
					reconciled := 0
					for _, r := range results {
						if r != nil {
							reconciled += r.StaleFileCount + r.DeletedFileCount
						}
					}
					return len(gced), reconciled
				}()
				// Only a tick that changed the graph schedules reclamation. The
				// process-wide quiet gate postpones it if another subsystem is busy.
				if reconciled > 0 || gcedCount > 0 {
					releaseMemoryToOS(logger, "reconcile_janitor")
				}
			case <-stop:
				return
			}
		}
	}()
	return func() { close(stop) }
}

// daemonStartAcceptedFlags returns every flag the re-exec'd `daemon start`
// can parse: its own, plus the persistent flags it inherits from the root
// command. cobra only folds the inherited half into Flags() when the command
// actually executes, so callers that reach the spawn from a *different*
// command (`daemon restart`, `install --start`) have to ask for it explicitly
// or --log-level / --config would look unrecognised and be dropped.
func daemonStartAcceptedFlags() *pflag.FlagSet {
	accepted := pflag.NewFlagSet("daemon start", pflag.ContinueOnError)
	accepted.AddFlagSet(daemonStartCmd.Flags())
	accepted.AddFlagSet(daemonStartCmd.InheritedFlags())
	return accepted
}

// detachedDaemonArgs rebuilds the argv the detached child needs so a
// `--detach` start behaves exactly like the same command without it.
// Only flags the user actually set are emitted (pflag's Visit walks the
// changed set), in `--name=value` form — there is no shell between us and
// the child, so values that would need quoting are safe, and defaults stay
// implicit so the child still resolves them from config the same way a
// foreground start does.
//
// `changed` is the flag set of the command being run (cobra merges the
// inherited persistent flags into it, so --log-level / --config ride along);
// `accepted` is the child's `daemon start` flag set. A changed flag the child
// wouldn't recognise is dropped rather than handed to a process that would
// exit on "unknown flag" — that is what lets `daemon restart` and
// `install --start` reuse this without leaking their own flags. --detach is
// dropped too: the child *is* the detached daemon.
func detachedDaemonArgs(changed, accepted *pflag.FlagSet) []string {
	var out []string
	if changed == nil {
		return out
	}
	changed.Visit(func(f *pflag.Flag) {
		if f.Name == "detach" {
			return
		}
		if accepted != nil && accepted.Lookup(f.Name) == nil {
			return
		}
		// Slice flags stringify as "[a,b]", which pflag can't parse back.
		// Repeat the flag instead — the second Set appends.
		if sv, ok := f.Value.(pflag.SliceValue); ok {
			for _, v := range sv.GetSlice() {
				out = append(out, "--"+f.Name+"="+v)
			}
			return
		}
		out = append(out, "--"+f.Name+"="+f.Value.String())
	})
	return out
}

// spawnDetachedDaemon re-invokes the binary with GORTEX_DAEMON_CHILD=1
// set, the log redirected to the daemon log file, and the child
// parented to init. Parent exits as soon as the child has the socket up.
// childArgs (from detachedDaemonArgs) carries the caller's flags through
// to the re-exec'd `daemon start`.
//
// On a TTY the parent shows a mesh-spinner banner and a styled "ready" card
// once the socket is live. On a non-TTY (CI scripts, automation) we keep the
// historical one-line "[gortex daemon] detached (pid X, log: Y)" message so
// existing parsers don't break.
func spawnDetachedDaemon(childArgs []string) error {
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve executable: %w", err)
	}
	if err := daemon.EnsureParentDir(daemon.LogFilePath()); err != nil {
		return err
	}
	logFile, err := os.OpenFile(daemon.LogFilePath(),
		os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("open log file: %w", err)
	}
	child := exec.Command(exe, append([]string{"daemon", "start"}, childArgs...)...)
	// The child re-parses the forwarded flags itself, so nothing has to
	// travel by env any more except the marker that stops it detaching
	// again (and stops it clearing a stop-intent written meanwhile).
	// Inherited GORTEX_* overrides keep the precedence they have on a
	// foreground start rather than being re-injected from a flag here.
	child.Env = append(os.Environ(), "GORTEX_DAEMON_CHILD=1")
	child.Stdout = logFile
	child.Stderr = logFile
	child.Stdin = nil
	// Detach the child from the parent's controlling terminal /
	// console so Ctrl-C on the parent doesn't kill the daemon.
	child.SysProcAttr = platform.DetachSysProcAttr()
	if err := child.Start(); err != nil {
		_ = logFile.Close()
		return fmt.Errorf("spawn daemon: %w", err)
	}
	// Don't block on the child — it's detached and inherits the log
	// file handle. Reap it in a background goroutine so a crash during
	// startup surfaces on `exited` instead of stalling the poll loop.
	exited := make(chan error, 1)
	go func() { exited <- child.Wait() }()

	emitDaemonStartBanner(os.Stderr)
	sp := newDaemonSpawnSpinner(os.Stderr)
	if sp != nil {
		sp.Start("Waiting for daemon socket")
	}

	// Wait until the socket is live or a timeout hits, so we fail fast
	// if the child died on startup. The socket opens after buildDaemonState
	// finishes opening the store, which on a large workspace can take
	// 10–20 s — 5 s used to time out a perfectly healthy daemon mid-load.
	// 60 s comfortably covers the biggest stores we see while still
	// failing fast on a child that crashed outright (those die in well
	// under a second).
	start := time.Now()
	deadline := start.Add(60 * time.Second)
	for time.Now().Before(deadline) {
		if daemon.IsRunning() {
			elapsed := time.Since(start).Truncate(10 * time.Millisecond)
			if sp != nil {
				sp.Set("", fmt.Sprintf("socket up · %s", elapsed))
				sp.Done()
				emitDaemonStartSummary(os.Stderr, child.Process.Pid, elapsed)
			} else {
				fmt.Fprintf(os.Stderr, "[gortex daemon] detached (pid %d, log: %s)\n",
					child.Process.Pid, daemon.LogFilePath())
			}
			return nil
		}
		// Bail out early if the child has already exited — no point
		// waiting another 59 seconds for a corpse.
		select {
		case werr := <-exited:
			failMsg := fmt.Errorf("daemon exited during startup (%v); check %s",
				werr, daemon.LogFilePath())
			if sp != nil {
				sp.Fail(failMsg)
			}
			return failMsg
		default:
		}
		if sp != nil {
			sp.Set("", fmt.Sprintf("opening store · %s", time.Since(start).Truncate(100*time.Millisecond)))
		}
		time.Sleep(50 * time.Millisecond)
	}
	timeoutErr := fmt.Errorf("daemon did not come up within 60s; check %s", daemon.LogFilePath())
	if sp != nil {
		sp.Fail(timeoutErr)
	}
	return timeoutErr
}

// newDaemonSpawnSpinner returns a spinner bound to w when it's a TTY (and the
// global --no-progress flag isn't set). Returns nil otherwise, so callers can
// branch on the spinner's presence to choose between the framed-card vs.
// one-line output paths.
func newDaemonSpawnSpinner(w io.Writer) *progress.Spinner {
	if noProgress || !progress.IsTTY(w) {
		return nil
	}
	return progress.NewSpinner(w)
}

// emitDaemonStartBanner prints the gortex mesh banner + subtitle for the
// detach flow. Only fires on a TTY — non-TTY callers stay quiet so script
// stderr remains parseable.
func emitDaemonStartBanner(w io.Writer) {
	if !progress.IsTTY(w) || noProgress || daemonRestartActive {
		return
	}
	banner := tui.Banner{
		Title:    "gortex daemon start",
		Subtitle: "Spawning daemon in the background.",
	}.Render()
	fmt.Fprintln(w)
	fmt.Fprintln(w, banner)
	fmt.Fprintln(w)
}

// emitDaemonStartSummary prints the post-spawn card showing pid, socket, log
// path, elapsed time, and useful next-step hints. Only fires on a TTY.
func emitDaemonStartSummary(w io.Writer, pid int, elapsed time.Duration) {
	if !progress.IsTTY(w) || noProgress {
		return
	}
	stats := []string{
		progress.Stat(fmt.Sprintf("%d", pid), "pid", progress.StatGood),
		progress.Stat(elapsed.String(), "boot", progress.StatGood),
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, "  "+progress.StyleOK.Render("✓")+"  "+progress.StyleStrong.Render("daemon ready"))
	fmt.Fprintln(w, "     "+progress.StatStrip(stats...))
	fmt.Fprintln(w, "     "+progress.Row("socket", daemon.SocketPath(), 8))
	fmt.Fprintln(w, "     "+progress.Row("log", daemon.LogFilePath(), 8))
	fmt.Fprintln(w)
	fmt.Fprintln(w, "     "+progress.Heading("next"))
	fmt.Fprintln(w, "     "+progress.NumberedStep(1, "track a repo:    gortex track <path>"))
	fmt.Fprintln(w, "     "+progress.NumberedStep(2, "watch status:    gortex daemon status --watch"))
	fmt.Fprintln(w, "     "+progress.NumberedStep(3, "shut down:       gortex daemon stop"))
	fmt.Fprintln(w)
}

func runDaemonStop(cmd *cobra.Command, _ []string) error {
	w := cmd.ErrOrStderr()
	// Record the user's "stay down" intent so the autostart path (a live
	// `gortex mcp` proxy relaunched by an editor) doesn't immediately respawn
	// the daemon we're about to stop. A `daemon restart` re-clears it via the
	// following start, so only a standalone stop is sticky.
	if !daemonRestartActive {
		if err := daemon.MarkStopIntent(); err != nil {
			fmt.Fprintf(w, "[gortex daemon] warning: could not record stop intent (%v); daemon may auto-respawn\n", err)
		}
		// If an OS supervisor (systemd --user / launchd) owns the daemon, stop
		// it THROUGH the supervisor — a socket-level stop just kills the worker
		// and the supervisor restarts it. `daemon restart` skips this and
		// bounces via the supervisor instead.
		if serviceActive() {
			return serviceStop(w)
		}
	}
	if !daemon.IsRunning() {
		// The socket is gone, but a process may still be alive and holding
		// the store lock — a daemon mid-shutdown, or one whose socket wedged.
		// killByPID terminates it AND blocks until it has actually exited,
		// which is what `daemon restart` relies on to not race the lock.
		if _, ok := daemon.RunningPID(); ok {
			return killByPID()
		}
		emitDaemonStopAlreadyDown(w)
		return nil
	}

	// Capture uptime + socket *before* shutdown so we can show them in the
	// post-stop summary (the socket file vanishes on clean shutdown).
	socket := daemon.SocketPath()
	uptime := daemonUptimeBeforeStop()
	// Capture the PID too. ControlShutdown only *acks* — the daemon then
	// flushes and closes the store (releasing its on-disk lock) and exits
	// asynchronously (see server.go: the handler Shutdown()s ~100ms later in
	// a goroutine). We must block until that process is gone, or a following
	// `daemon start` races the still-held lock and dies with the opaque
	// "failed to open database with status 1".
	pid, havePID := daemon.RunningPID()

	c, err := daemon.Dial(daemon.Handshake{Mode: daemon.ModeControl, ClientName: "cli"})
	if err != nil {
		// Daemon said it was alive but won't talk — probably a stale PID file
		// the daemon hasn't cleaned up. Fall back to killing by PID.
		return killByPID()
	}
	// Explicit budget: the daemon flushes and closes its store before acking,
	// so the ack is unbounded by policy on the server side (abandoning a
	// half-done flush is worse than a slow stop). The wait belongs here
	// instead, where giving up is safe — the daemon is already on its way
	// down and the exit wait below finishes the job.
	resp, err := c.ControlWithTimeout(daemon.ControlShutdown, nil, daemonShutdownAckTimeout)
	_ = c.Close()
	// A daemon that accepted the request but hasn't acked is not a reason to
	// abandon the stop. The ack is synchronous with the controller's
	// flush-and-close, so on a large workspace — or behind a track / reload
	// holding the controller — it can legitimately overrun the budget while
	// the process is still on its way down. Returning an error here left the
	// user with a daemon they could only `kill -9`. Fall through to the exit
	// wait instead: it force-kills and cleans up if the daemon really is
	// wedged, so `daemon stop` always terminates and always leaves the socket
	// and PID file in a startable state.
	grace := daemonExitGrace
	switch {
	case errors.Is(err, daemon.ErrDaemonUnresponsive),
		err == nil && !resp.OK && resp.ErrorCode == daemon.ErrTimeout:
		fmt.Fprintln(w, "[gortex daemon] no shutdown ack yet — the daemon is busy; waiting for it to exit")
		grace = daemonBusyExitGrace
	case err != nil:
		return err
	case !resp.OK:
		return fmt.Errorf("shutdown rejected: %s %s", resp.ErrorCode, resp.ErrorMsg)
	}
	if havePID {
		waitForDaemonExitWithin(pid, grace)
	}
	emitDaemonStopSummary(w, socket, uptime)
	return nil
}

const (
	// daemonExitGrace is how long a normal stop waits for the process to go
	// away after a successful ack — the flush has already happened by then.
	daemonExitGrace = 15 * time.Second
	// daemonBusyExitGrace applies when the daemon never acked. The flush is
	// presumed still running, so force-killing on the normal schedule would
	// interrupt exactly the store write we want to complete.
	daemonBusyExitGrace = 2 * time.Minute
	// daemonStatusCardTimeout bounds the advisory Status lookups that only
	// decorate output. They must never be the reason a command hangs.
	daemonStatusCardTimeout = 3 * time.Second
	// daemonShutdownAckTimeout is how long the stop command waits for the
	// shutdown ack before switching to watching the process itself. Generous,
	// because the ack trails a real store flush.
	daemonShutdownAckTimeout = 30 * time.Second
)

// waitForDaemonExitWithin blocks until the daemon process pid has exited — and
// thus released the store's on-disk lock — force-killing it if a graceful
// shutdown stalls. This is what makes `daemon stop` honest: when it returns,
// the store is free for the next process, which is the foundation `daemon
// restart` stands on. Polls cheaply; the common case (a clean flush) clears in
// well under a second.
//
// grace is the caller's, not a constant, because how long to wait depends on
// what the ack told us: daemonExitGrace after a successful ack (the flush has
// already happened), daemonBusyExitGrace when none arrived (it probably has
// not, and force-killing on the normal schedule would interrupt the store
// write we want to complete).
func waitForDaemonExitWithin(pid int, grace time.Duration) {
	deadline := time.Now().Add(grace)
	for time.Now().Before(deadline) {
		if !platform.ProcessAlive(pid) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	// Graceful shutdown stalled (e.g. a wedged cgo call). Don't leave a
	// half-exited daemon clutching the lock — force it, then clean up the
	// socket/PID so the next start isn't tripped by stale files.
	fmt.Fprintln(os.Stderr, "[gortex daemon] graceful shutdown timed out — force-killing")
	_ = platform.KillProcess(pid)
	for i := 0; i < 60 && platform.ProcessAlive(pid); i++ {
		time.Sleep(50 * time.Millisecond)
	}
	_ = os.Remove(daemon.PIDFilePath())
	_ = os.Remove(daemon.SocketPath())
}

// daemonUptimeBeforeStop best-effort-fetches the daemon's reported uptime via
// a Status control before shutdown so the summary card can show how long the
// process ran. Returns 0 on any error — we'd rather degrade the card than
// fail the stop.
//
// Bounded hard: Status aggregates the whole store and serialises behind the
// controller mutex, so on a busy daemon this decorative lookup was the first
// thing `daemon stop` blocked on — the stop request had not even been sent
// yet. A card without an uptime is a fine outcome; a stop that never returns
// is not.
func daemonUptimeBeforeStop() time.Duration {
	c, err := daemonControlClient()
	if err != nil {
		return 0
	}
	defer c.Close()
	resp, err := c.ControlWithTimeout(daemon.ControlStatus, nil, daemonStatusCardTimeout)
	if err != nil || !resp.OK {
		return 0
	}
	var st daemon.StatusResponse
	if jerr := json.Unmarshal(resp.Result, &st); jerr != nil {
		return 0
	}
	return time.Duration(st.UptimeSeconds) * time.Second
}

// emitDaemonStopAlreadyDown prints the "not running" message: a one-liner on
// non-TTY for script compat, a styled hint card on TTY.
func emitDaemonStopAlreadyDown(w io.Writer) {
	if !progress.IsTTY(w) || noProgress {
		fmt.Fprintln(w, "[gortex daemon] not running")
		return
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, "  "+progress.StyleHint.Render("◌  daemon was not running — nothing to stop"))
	fmt.Fprintln(w, "     "+progress.Caption("start with `gortex daemon start --detach`"))
	fmt.Fprintln(w)
}

// emitDaemonStopSummary prints the post-shutdown banner + result card mirroring
// daemon start's surface: uptime + socket path so the user can confirm the
// right daemon went down.
func emitDaemonStopSummary(w io.Writer, socket string, uptime time.Duration) {
	if !progress.IsTTY(w) || noProgress {
		fmt.Fprintln(w, "[gortex daemon] stopped")
		return
	}
	if !daemonRestartActive {
		banner := tui.Banner{
			Title:    "gortex daemon stop",
			Subtitle: "Daemon shut down cleanly.",
		}.Render()
		fmt.Fprintln(w)
		fmt.Fprintln(w, banner)
	}
	stats := []string{progress.Stat("clean shutdown", "", progress.StatGood)}
	if uptime > 0 {
		stats = append(stats, progress.Stat(uptime.Truncate(time.Second).String(), "uptime", progress.StatNeutral))
	}
	fmt.Fprintln(w, "  "+progress.StyleOK.Render("✓")+"  "+progress.StyleStrong.Render("stopped"))
	fmt.Fprintln(w, "     "+progress.StatStrip(stats...))
	if socket != "" {
		fmt.Fprintln(w, "     "+progress.Row("socket", socket+" (removed)", 8))
	}
	fmt.Fprintln(w)
}

// daemonRestartActive flips on for the duration of runDaemonRestart so the
// inner stop / start emit functions skip their own banners — restart shows the
// logo once at the top and then lists the stop + start cards underneath.
var daemonRestartActive bool

func runDaemonRestart(cmd *cobra.Command, args []string) error {
	daemonRestartActive = true
	defer func() { daemonRestartActive = false }()

	emitDaemonRestartBanner(cmd.ErrOrStderr())

	// When an OS supervisor owns the daemon, bounce it THROUGH the supervisor so
	// the supervisor keeps ownership; a manual stop+start would orphan the new
	// daemon from the unit (the unit reads inactive while a hand-started process
	// holds the socket).
	if serviceActive() {
		return serviceRestart(cmd.ErrOrStderr())
	}

	// Stop is idempotent when not running and now blocks until the old
	// process has fully exited — releasing the store's on-disk lock — before
	// returning. That's what lets the start below reuse the store without
	// racing the lock. The old code polled `daemon.IsRunning()` here, which
	// watched the wrong resource: the socket is torn down ~100ms after the
	// shutdown ack, long before the process exits and the lock clears, so the
	// poll fell through early and the restart died on "failed to open
	// database with status 1".
	if err := runDaemonStop(cmd, args); err != nil {
		return err
	}
	daemonDetach = true
	return runDaemonStart(cmd, args)
}

// emitDaemonRestartBanner prints the unified header for `gortex daemon
// restart` so the user sees the mesh logo once instead of twice.
func emitDaemonRestartBanner(w io.Writer) {
	if !progress.IsTTY(w) || noProgress {
		return
	}
	banner := tui.Banner{
		Title:    "gortex daemon restart",
		Subtitle: "Cycling daemon: stop then start.",
	}.Render()
	fmt.Fprintln(w)
	fmt.Fprintln(w, banner)
	fmt.Fprintln(w)
}

func runDaemonReload(_ *cobra.Command, _ []string) error {
	c, err := daemonControlClient()
	if err != nil {
		return err
	}
	defer c.Close()
	resp, err := c.Control(daemon.ControlReload, nil)
	if err != nil {
		return err
	}
	if !resp.OK {
		return fmt.Errorf("reload rejected: %s %s", resp.ErrorCode, resp.ErrorMsg)
	}
	fmt.Fprintln(os.Stderr, "[gortex daemon] reloaded")
	return nil
}

func runDaemonStatus(cmd *cobra.Command, _ []string) error {
	if daemonStatusWatch {
		return runDaemonStatusWatch(cmd)
	}
	st, err := fetchDaemonStatusWithOptions(daemon.StatusParams{Exact: daemonStatusExact})
	if err != nil {
		return err
	}
	w := cmd.OutOrStdout()
	renderDaemonHeader(w, st)
	renderDaemonWorkspaces(w, st)
	renderDaemonRepos(w, st)
	renderDaemonSessions(w, st)
	renderDaemonServers(w, st)
	return nil
}

// fetchDaemonStatusForCLI dials the control socket once and returns a parsed
// StatusResponse. Shared by the one-shot and watch paths.
func fetchDaemonStatusForCLI() (daemon.StatusResponse, error) {
	return fetchDaemonStatusWithOptions(daemon.StatusParams{})
}

// fetchDaemonStatusWithOptions is fetchDaemonStatusForCLI with the control
// payload spelled out. An exact recount can outrun the default control budget
// by a wide margin on a large store, so that request waits without a bound —
// the user asked for the scan and is watching it happen.
func fetchDaemonStatusWithOptions(params daemon.StatusParams) (daemon.StatusResponse, error) {
	var st daemon.StatusResponse
	c, err := daemonControlClient()
	if err != nil {
		return st, err
	}
	defer c.Close()
	timeout := daemon.ControlTimeoutFor(daemon.ControlStatus)
	if params.Exact {
		timeout = 0
	}
	resp, err := c.ControlWithTimeout(daemon.ControlStatus, params, timeout)
	if err != nil {
		return st, err
	}
	if !resp.OK {
		return st, fmt.Errorf("status rejected: %s %s", resp.ErrorCode, resp.ErrorMsg)
	}
	if err := json.Unmarshal(resp.Result, &st); err != nil {
		return st, fmt.Errorf("parse status: %w", err)
	}
	return st, nil
}

// renderDaemonHeader writes the fixed-schema key/value facts about the
// daemon process as a borderless two-column table.
func renderDaemonHeader(w io.Writer, st daemon.StatusResponse) {
	t := table.NewWriter()
	t.SetOutputMirror(w)
	t.SetStyle(table.StyleLight)
	t.Style().Options.DrawBorder = false
	t.Style().Options.SeparateColumns = false
	t.Style().Options.SeparateHeader = false
	t.Style().Options.SeparateRows = false
	t.SetColumnConfigs([]table.ColumnConfig{
		{Number: 1, Align: text.AlignLeft, WidthMax: 12},
		{Number: 2, Align: text.AlignLeft},
	})
	t.AppendRow(table.Row{"daemon", st.Version})
	t.AppendRow(table.Row{"pid", st.PID})
	// Upgrade-skew facts belong next to the daemon version they qualify.
	// The cli row appears only when this binary's build differs from the
	// daemon's — the same compare runProxy warns on at connect time — so
	// a matching pair keeps the table exactly as terse as before. The
	// binary row surfaces the daemon's own on-disk drift probe and only
	// when it actually ran: an unchecked binary is unknown, not fresh,
	// and must not be reported as either.
	local := canonicalVersion()
	if warn := daemonSkewWarning(st.Version, local); warn != "" {
		t.AppendRow(table.Row{"cli", local + " (differs from daemon)"})
	}
	if st.BinaryChecked && st.BinaryStale {
		t.AppendRow(table.Row{"binary", "stale — on-disk image newer than running image; run 'gortex daemon restart'"})
	}
	t.AppendRow(table.Row{"socket", st.SocketPath})
	t.AppendRow(table.Row{"uptime", formatDuration(time.Duration(st.UptimeSeconds) * time.Second)})
	switch {
	case st.Ready && st.EnrichmentComplete:
		t.AppendRow(table.Row{
			"state",
			fmt.Sprintf("ready (warmup %s)", formatDuration(time.Duration(st.EnrichSeconds)*time.Second)),
		})
	case st.Ready:
		t.AppendRow(table.Row{
			"state",
			fmt.Sprintf("ready — queryable (warmup %s);%s",
				formatDuration(time.Duration(st.WarmupSeconds)*time.Second),
				formatEnrichmentProgress(st.Enrichment)),
		})
	default:
		t.AppendRow(table.Row{"state", "warming up (socket reachable, resolving references)"})
	}
	t.AppendRow(table.Row{"sessions", st.Sessions})
	if st.MemoryBytes > 0 {
		t.AppendRow(table.Row{"memory", formatBytes(st.MemoryBytes)})
	}
	if sb := st.SearchBackend; sb.Name != "" {
		// formatSearchDocs renders the trailing-space "docs=N  " fragment,
		// or nothing when the backend cannot report a real count. Backends
		// whose only figure is a since-construction Add/Remove delta must
		// print nothing rather than pass the delta off as a corpus size.
		formatSearchDocs := func(sb daemon.SearchBackendStats) string {
			if !sb.DocCountKnown {
				return ""
			}
			return fmt.Sprintf("docs=%d  ", sb.DocCount)
		}
		switch {
		case sb.DiskResident:
			// No heap footprint to report — the index lives inside the
			// graph store's own file, not a separate in-memory
			// structure. Printing "heap=0 B" here would read as "this
			// backend costs nothing", which is false.
			t.AppendRow(table.Row{"search", fmt.Sprintf(
				"%s  %sdisk-resident (indexed in the graph store)",
				sb.Name, formatSearchDocs(sb))})
		default:
			t.AppendRow(table.Row{"search", fmt.Sprintf(
				"%s  %sheap=%s", sb.Name, formatSearchDocs(sb), formatBytes(sb.Bytes))})
		}
	}
	if tc := st.TrigramCache; tc != nil {
		// The literal-search index is per repo, lazily built and can be the
		// largest single structure in the daemon, so it gets its own line
		// rather than hiding behind a "disk-resident" search backend.
		switch {
		case tc.BuildsOff:
			t.AppendRow(table.Row{"trigram", "off (GORTEX_TRIGRAM_MAX_LIVE=0) — text search streams"})
		default:
			budget := "unbounded"
			if tc.MaxBytes > 0 {
				budget = formatBytes(uint64(tc.MaxBytes))
			}
			row := fmt.Sprintf("live=%d/%d  heap=%s/%s  idle_ttl=%s",
				tc.Live, tc.MaxLive,
				formatBytes(uint64(tc.Bytes)), budget,
				(time.Duration(tc.IdleTTLMs) * time.Millisecond).String())
			if tc.Evictions > 0 {
				row += fmt.Sprintf("  evictions=%d", tc.Evictions)
			}
			t.AppendRow(table.Row{"trigram", row})
		}
	}
	// Alive language servers are subprocesses the daemon is holding
	// open — the one class of daemon-owned resource that survives
	// outside the Go heap, and the one whose leak is invisible in every
	// other row here. The section is omitted entirely when no router is
	// wired or nothing is alive, so the common case stays as terse as
	// it was.
	if summary, providers := formatLSPRouterRows(st.LSPRouter, time.Now()); summary != "" {
		t.AppendRow(table.Row{"lsp", summary})
		for _, line := range providers {
			t.AppendRow(table.Row{"", line})
		}
	}
	rt := st.Runtime
	if rt.Sys > 0 {
		t.AppendRow(table.Row{"runtime", fmt.Sprintf(
			"alloc=%s  sys=%s  heap_inuse=%s  heap_idle=%s  heap_released=%s  stacks=%s  gc=%d  goroutines=%d",
			formatBytes(rt.Alloc),
			formatBytes(rt.Sys),
			formatBytes(rt.HeapInuse),
			formatBytes(rt.HeapIdle),
			formatBytes(rt.HeapReleased),
			formatBytes(rt.StackInuse),
			rt.NumGC,
			rt.NumGoroutine,
		)})
	}
	if st.PProfAddr != "" {
		t.AppendRow(table.Row{"pprof", fmt.Sprintf(
			"http://%s/debug/pprof/  (example: go tool pprof -http=: http://%s/debug/pprof/heap)",
			st.PProfAddr, st.PProfAddr)})
	}
	t.Render()
}

// formatLSPRouterRows renders the daemon's LSP-router state into the
// `lsp` summary cell plus one indented line per alive language-server
// subprocess. Returns an empty summary when there is nothing to show
// — no router wired (nil status) or no provider alive — so the header
// table keeps its previous shape on daemons that never spawn one.
//
// The per-provider line reports last_used as an age rather than a
// timestamp because the question it answers is "is the 10-minute idle
// reaper about to take this one?", and in_use because a pin count
// that never falls back to zero is precisely what keeps a provider
// out of the reaper's and the LRU evictor's reach.
//
// now is passed in rather than read here so the rendering is
// deterministic under test.
func formatLSPRouterRows(r *daemon.LSPRouterStatus, now time.Time) (string, []string) {
	if r == nil || len(r.ActiveProviders) == 0 {
		return "", nil
	}
	summary := fmt.Sprintf("alive=%d", len(r.ActiveProviders))
	if r.MaxAlive > 0 {
		summary = fmt.Sprintf("alive=%d/%d", len(r.ActiveProviders), r.MaxAlive)
	}
	if r.Evictions > 0 {
		summary += fmt.Sprintf("  evictions=%d", r.Evictions)
	}

	lines := make([]string, 0, len(r.ActiveProviders))
	for _, p := range r.ActiveProviders {
		lines = append(lines, fmt.Sprintf("  %s@%s  last_used=%s  in_use=%d",
			p.Spec, p.Workspace, formatLSPLastUsed(p.LastUsed, now), p.InUse))
	}
	return summary, lines
}

// formatLSPLastUsed turns the RFC3339 timestamp the daemon sends into
// a "3m12s ago" age. An unparseable value is passed through verbatim
// rather than dropped — a malformed timestamp from an older daemon
// build should still be visible, not silently rendered as "now".
func formatLSPLastUsed(raw string, now time.Time) string {
	ts, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return raw
	}
	age := now.Sub(ts)
	if age < 0 {
		// Clock skew between the status read and the daemon's own
		// clock; "0s ago" beats a negative duration.
		age = 0
	}
	return formatDuration(age.Truncate(time.Second)) + " ago"
}

// formatEnrichmentProgress renders the semantic-enrichment progress
// suffix appended to the "state" row while Ready but not yet
// EnrichmentComplete. Falls back to the old mute message when no
// progress summary is available (no semantic manager wired, or an
// older daemon build reporting a nil Enrichment) — the row still says
// something instead of going blank.
func formatEnrichmentProgress(e *daemon.EnrichmentProgress) string {
	if e == nil {
		return " enrichment in progress"
	}
	if e.Current == nil {
		return fmt.Sprintf(" enriching %d/%d repos", e.ReposDone, e.ReposTotal)
	}
	elapsed := formatDuration(time.Duration(e.Current.ElapsedSeconds * float64(time.Second)))
	if e.Current.DeadlineSeconds > 0 {
		deadline := formatDuration(time.Duration(e.Current.DeadlineSeconds * float64(time.Second)))
		return fmt.Sprintf(" enriching %d/%d (%s, %s/%s)",
			e.ReposDone, e.ReposTotal, e.Current.Repo, elapsed, deadline)
	}
	return fmt.Sprintf(" enriching %d/%d (%s, %s)",
		e.ReposDone, e.ReposTotal, e.Current.Repo, elapsed)
}

// renderDaemonRepos writes the per-repo breakdown as a single table.
// Rows sort by attributed memory descending so the largest consumers
// appear first. An "other" row at the bottom covers the delta between
// process-total memory and the sum of attributed per-repo memory —
// embedder model weights, runtime heap headroom, semantic caches, etc.
func renderDaemonRepos(w io.Writer, st daemon.StatusResponse) {
	if len(st.TrackedRepos) == 0 {
		fmt.Fprintln(w, "\ntracked repos: (none)")
		return
	}

	rows := make([]daemon.TrackedRepoStatus, len(st.TrackedRepos))
	copy(rows, st.TrackedRepos)
	sort.Slice(rows, func(i, j int) bool {
		return rows[i].Memory.TotalBytes > rows[j].Memory.TotalBytes
	})

	fmt.Fprintln(w, "\ntracked repos:")
	t := table.NewWriter()
	t.SetOutputMirror(w)
	t.SetStyle(table.StyleLight)
	t.Style().Format.Header = text.FormatDefault
	t.Style().Format.Footer = text.FormatDefault

	// The workspace column only adds signal when at least one repo
	// declares an explicit workspace (i.e. one that doesn't equal the
	// repo prefix). Pure-default setups already see the prefix in
	// column 1; printing the same string twice is just noise.
	showWS := false
	for _, r := range rows {
		if r.Workspace != "" && r.Workspace != r.Prefix {
			showWS = true
			break
		}
	}

	// The state column appears only once a repo is in trouble — its
	// directory is gone, the daemon holds no index for it, or it indexed
	// nothing at all. On a healthy workspace every row would read "ok",
	// so it stays hidden.
	showState := false
	for _, r := range rows {
		if r.Missing || r.Unloaded || repoIndexIsEmpty(r) {
			showState = true
			break
		}
	}

	header := table.Row{"repo"}
	colConfigs := []table.ColumnConfig{{Number: 1, Align: text.AlignLeft}}
	if showWS {
		header = append(header, "workspace")
		colConfigs = append(colConfigs, table.ColumnConfig{Number: len(colConfigs) + 1, Align: text.AlignLeft})
	}
	if showState {
		header = append(header, "state")
		colConfigs = append(colConfigs, table.ColumnConfig{Number: len(colConfigs) + 1, Align: text.AlignLeft})
	}
	header = append(header, "total", "files", "nodes", "edges",
		"nodes_b", "edges_b", "search_b", "vectors_b")
	for i := 0; i < 8; i++ {
		colConfigs = append(colConfigs, table.ColumnConfig{Number: len(colConfigs) + 1, Align: text.AlignRight})
	}
	header = append(header, "path")
	colConfigs = append(colConfigs, table.ColumnConfig{Number: len(colConfigs) + 1, Align: text.AlignLeft})
	t.AppendHeader(header)
	t.SetColumnConfigs(colConfigs)

	var attributed uint64
	for _, r := range rows {
		attributed += r.Memory.TotalBytes
		row := table.Row{r.Prefix}
		if showWS {
			ws := r.Workspace
			if r.WorkspaceProject != "" && r.WorkspaceProject != ws {
				ws = ws + "/" + r.WorkspaceProject
			}
			row = append(row, ws)
		}
		if showState {
			row = append(row, repoStateLabel(r))
		}
		row = append(row,
			formatBytes(r.Memory.TotalBytes),
			r.Files,
			r.Nodes,
			r.Edges,
			formatBytes(r.Memory.NodesBytes),
			formatBytes(r.Memory.EdgesBytes),
			formatBytes(r.Memory.SearchBytes),
			formatBytes(r.Memory.VectorsBytes),
		)
		row = append(row, r.Path)
		t.AppendRow(row)
	}

	if st.MemoryBytes > attributed {
		other := st.MemoryBytes - attributed
		footer := table.Row{"other"}
		if showWS {
			footer = append(footer, "")
		}
		if showState {
			footer = append(footer, "")
		}
		footer = append(footer, formatBytes(other), "", "", "", "", "", "", "")
		footer = append(footer, "embedder + runtime + caches (not attributed)")
		t.AppendFooter(footer)
	}

	t.Render()
	renderMissingRepoWarning(w, rows)
	renderEmptyIndexWarning(w, rows)
}

// repoStateLabel names a tracked repo's inventory state for the status
// table. MISSING is deliberately shouted: it is the only state a user
// has to act on, and the eight-day-old ghost of #312 went unnoticed
// precisely because nothing in any view said it out loud.
func repoStateLabel(r daemon.TrackedRepoStatus) string {
	switch {
	case r.Missing:
		return "MISSING"
	case r.Unloaded:
		return "not indexed"
	case repoIndexIsEmpty(r):
		return "EMPTY"
	default:
		return "ok"
	}
}

// repoIndexIsEmpty reports whether a repo finished indexing and came back
// with no files at all. That state used to render as an ordinary
// zero-count row, which is how a repo of 1,519 Python files could report
// as healthy while holding nothing (#624) — and why every query against
// it answered "no callers" with full confidence. A repo still waiting for
// its first index (LastIndex unset) is not yet empty, only pending.
func repoIndexIsEmpty(r daemon.TrackedRepoStatus) bool {
	return !r.Missing && !r.Unloaded && r.Files == 0 && r.LastIndex > 0
}

// renderEmptyIndexWarning prints the remediation block for every tracked
// repo that indexed zero files. The daemon log names the exact ignore
// pattern responsible; this points the operator at it.
func renderEmptyIndexWarning(w io.Writer, rows []daemon.TrackedRepoStatus) {
	var empty []daemon.TrackedRepoStatus
	for _, r := range rows {
		if repoIndexIsEmpty(r) {
			empty = append(empty, r)
		}
	}
	if len(empty) == 0 {
		return
	}
	subject := "repo indexed no files"
	if len(empty) > 1 {
		subject = "repos indexed no files"
	}
	fmt.Fprintf(w, "\n!! %d tracked %s — queries against them return empty answers, not missing ones:\n",
		len(empty), subject)
	for _, r := range empty {
		fmt.Fprintf(w, "     %-24s %s\n", r.Prefix, r.Path)
	}
	fmt.Fprintln(w, "   Either the path holds no source Gortex can parse, or an ignore rule excluded all of it.")
	fmt.Fprintln(w, "   The daemon log names the pattern:")
	fmt.Fprintln(w, "     gortex daemon logs | grep 'no source files were indexed'")
}

// renderMissingRepoWarning prints the remediation block below the repos
// table for every tracked repo whose directory is gone. Nothing else in
// the daemon ever says so: the repo simply stops indexing, and each view
// shows a different symptom (an empty git HEAD, a zero-count row, or no
// row at all). Naming the exact `gortex untrack` command turns a
// confusing inventory into a one-line fix.
func renderMissingRepoWarning(w io.Writer, rows []daemon.TrackedRepoStatus) {
	var gone []daemon.TrackedRepoStatus
	for _, r := range rows {
		if r.Missing {
			gone = append(gone, r)
		}
	}
	if len(gone) == 0 {
		return
	}
	subject := "repo no longer exists"
	if len(gone) > 1 {
		subject = "repos no longer exist"
	}
	fmt.Fprintf(w, "\n!! %d tracked %s on disk — the path was deleted, renamed, or unmounted:\n",
		len(gone), subject)
	for _, r := range gone {
		fmt.Fprintf(w, "     %-24s %s\n", r.Prefix, r.Path)
	}
	fmt.Fprintln(w, "   They can never be re-indexed. Drop each from the inventory with:")
	for _, r := range gone {
		fmt.Fprintf(w, "     gortex untrack %s\n", r.Path)
	}
}

// renderDaemonWorkspaces prints the per-workspace rollup above the
// repos table. When every workspace is a default singleton (each
// repo in its own auto-named workspace), it emits a one-line hint
// pointing at `gortex workspace set` instead of a wall-of-text
// table that just duplicates the per-repo view.
func renderDaemonWorkspaces(w io.Writer, st daemon.StatusResponse) {
	if len(st.Workspaces) == 0 {
		return
	}
	multiRepo := false
	for _, ws := range st.Workspaces {
		if len(ws.Repos) > 1 {
			multiRepo = true
			break
		}
	}

	if !multiRepo {
		// Compact form: tell the user the workspace boundary is in
		// default mode and how to opt repos into a shared workspace.
		// Avoids
		// printing a 33-row table where every row says "1 repo".
		fmt.Fprintf(w,
			"\nworkspaces: %d (one per repo, default — every repo is its own workspace)\n",
			len(st.Workspaces))
		fmt.Fprintln(w,
			"  Group repos into a shared workspace with `gortex workspace set <repo> <slug> --global`")
		fmt.Fprintln(w,
			"  or `gortex workspace set-all <slug> --root <path> --global`. See `gortex workspace --help`.")
		return
	}

	fmt.Fprintln(w, "\nworkspaces:")
	t := table.NewWriter()
	t.SetOutputMirror(w)
	t.SetStyle(table.StyleLight)
	t.Style().Format.Header = text.FormatDefault
	t.AppendHeader(table.Row{"workspace", "repos", "projects", "files", "nodes", "edges"})
	t.SetColumnConfigs([]table.ColumnConfig{
		{Number: 1, Align: text.AlignLeft},
		{Number: 2, Align: text.AlignRight},
		{Number: 3, Align: text.AlignLeft},
		{Number: 4, Align: text.AlignRight},
		{Number: 5, Align: text.AlignRight},
		{Number: 6, Align: text.AlignRight},
	})
	for _, ws := range st.Workspaces {
		projects := strings.Join(ws.Projects, ", ")
		if len(projects) > 50 {
			projects = projects[:47] + "..."
		}
		t.AppendRow(table.Row{ws.Slug, len(ws.Repos), projects, ws.Files, ws.Nodes, ws.Edges})
	}
	t.Render()
}

// renderDaemonSessions lists every connected MCP client. Skipped
// when no sessions are registered — single-process stdio embeds
// don't go through the daemon socket so they never show up here.
func renderDaemonSessions(w io.Writer, st daemon.StatusResponse) {
	if len(st.MCPSessions) == 0 {
		return
	}
	fmt.Fprintln(w, "\nMCP sessions:")
	t := table.NewWriter()
	t.SetOutputMirror(w)
	t.SetStyle(table.StyleLight)
	t.Style().Format.Header = text.FormatDefault
	t.AppendHeader(table.Row{"id", "client", "version", "connected", "cwd"})
	t.SetColumnConfigs([]table.ColumnConfig{
		{Number: 1, Align: text.AlignLeft},
		{Number: 2, Align: text.AlignLeft},
		{Number: 3, Align: text.AlignLeft},
		{Number: 4, Align: text.AlignRight},
		{Number: 5, Align: text.AlignLeft},
	})
	for _, s := range st.MCPSessions {
		client := s.ClientName
		if client == "" {
			client = "unknown"
		}
		t.AppendRow(table.Row{
			s.ID,
			client,
			s.ClientVersion,
			formatDuration(time.Duration(s.ConnectedSecs) * time.Second),
			s.Cwd,
		})
	}
	t.Render()
}

// renderDaemonServers shows the `~/.gortex/servers.toml` roster.
// Skipped when no file is present — the daemon is in single-server
// mode and there's nothing to list. The "local" column flags the
// entry the multi-server router treats as this daemon itself; auth
// is reported as "yes/no" (token values stay private to the
// daemon).
func renderDaemonServers(w io.Writer, st daemon.StatusResponse) {
	if len(st.ConfiguredServers) == 0 {
		return
	}
	fmt.Fprintln(w, "\nconfigured servers (~/.gortex/servers.toml):")
	t := table.NewWriter()
	t.SetOutputMirror(w)
	t.SetStyle(table.StyleLight)
	t.Style().Format.Header = text.FormatDefault
	t.AppendHeader(table.Row{"slug", "url", "local", "default", "auth", "workspaces"})
	t.SetColumnConfigs([]table.ColumnConfig{
		{Number: 1, Align: text.AlignLeft},
		{Number: 2, Align: text.AlignLeft},
		{Number: 3, Align: text.AlignCenter},
		{Number: 4, Align: text.AlignCenter},
		{Number: 5, Align: text.AlignCenter},
		{Number: 6, Align: text.AlignLeft},
	})
	yesno := func(b bool) string {
		if b {
			return "yes"
		}
		return ""
	}
	for _, s := range st.ConfiguredServers {
		t.AppendRow(table.Row{
			s.Slug,
			s.URL,
			yesno(s.Local),
			yesno(s.Default),
			yesno(s.HasAuth),
			strings.Join(s.Workspaces, ", "),
		})
	}
	t.Render()
}

func runDaemonLogs(cmd *cobra.Command, _ []string) error {
	path := daemon.LogFilePath()
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open log %s: %w", path, err)
	}
	defer f.Close()
	lines, err := tailLines(f, daemonTail)
	if err != nil {
		return err
	}
	for _, l := range lines {
		fmt.Fprintln(cmd.OutOrStdout(), l)
	}
	return nil
}

// daemonControlClient is the shared "dial + expect running" helper for
// the read-only control subcommands. Returns a clear error instead of
// a misleading ErrDaemonUnavailable.
func daemonControlClient() (*daemon.Client, error) {
	c, err := daemon.Dial(daemon.Handshake{Mode: daemon.ModeControl, ClientName: "cli"})
	if err != nil {
		return nil, fmt.Errorf("daemon not reachable (%v) — is it running? Try `gortex daemon start`", err)
	}
	return c, nil
}

// killByPID is the fallback stop path for stale daemons that have a PID
// file but don't respond on the socket. Asks the process to terminate,
// waits, then force-kills. Silently returns nil if the PID no longer
// exists.
func killByPID() error {
	pidBytes, err := os.ReadFile(daemon.PIDFilePath())
	if err != nil {
		return nil
	}
	pid, _ := strconv.Atoi(string(pidBytes))
	if pid <= 0 {
		return nil
	}
	_ = platform.TerminateProcess(pid)

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if !platform.ProcessAlive(pid) {
			// Process gone.
			_ = os.Remove(daemon.PIDFilePath())
			_ = os.Remove(daemon.SocketPath())
			fmt.Fprintln(os.Stderr, "[gortex daemon] stopped")
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	// Last resort.
	_ = platform.KillProcess(pid)
	_ = os.Remove(daemon.PIDFilePath())
	_ = os.Remove(daemon.SocketPath())
	fmt.Fprintln(os.Stderr, "[gortex daemon] stopped (force-killed)")
	return nil
}

// tailLines returns the last n lines of f. Used by `daemon logs`. Small
// implementation — log files are capped at a few MB so we can afford a
// full read and slice rather than seeking from the end.
func tailLines(f io.Reader, n int) ([]string, error) {
	buf, err := io.ReadAll(f)
	if err != nil {
		return nil, err
	}
	// Split on newline without pulling in bufio.Scanner buffer-size gotchas.
	var out []string
	start := 0
	for i, b := range buf {
		if b == '\n' {
			out = append(out, string(buf[start:i]))
			start = i + 1
		}
	}
	if start < len(buf) {
		out = append(out, string(buf[start:]))
	}
	if len(out) > n {
		out = out[len(out)-n:]
	}
	return out, nil
}

func formatDuration(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm%ds", int(d.Minutes()), int(d.Seconds())%60)
	}
	if d < 24*time.Hour {
		return fmt.Sprintf("%dh%dm", int(d.Hours()), int(d.Minutes())%60)
	}
	return fmt.Sprintf("%dd%dh", int(d.Hours())/24, int(d.Hours())%24)
}

func formatBytes(n uint64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := uint64(unit), 0
	for x := n / unit; x >= unit; x /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

// stubController is a placeholder Controller so `gortex daemon start`
// works end-to-end before the real MultiIndexer integration lands. It
