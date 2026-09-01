package main

import (
	"context"
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
	"github.com/zzet/gortex/internal/pathkey"
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

type startupViewReadinessProbe func(context.Context, string) (daemon.TrackReadiness, error)

type startupViewCohortMember struct {
	Path        string
	CheckoutID  string
	Incarnation string
}

func startupTopologyKey(checkoutID, incarnation string) string {
	return checkoutID + "\x00" + incarnation
}

// startupViewReadinessMonitor owns the daemon's frozen configured-Git startup
// cohort. Its paths come from the same post-Seed startupOwnershipPlan consumed
// by legacy warmup, so a probe that temporarily says "legacy" is retained as
// building rather than silently removing the repository from both owners.
// Worker callbacks only update a tiny failure map and coalesce an edge;
// catalog/materialization reads stay off the lifecycle worker (one explicit
// post-Seed snapshot, then the one monitor goroutine).
type startupViewReadinessMonitor struct {
	paths        []string
	changed      chan struct{}
	snapshots    chan struct{}
	initialized  chan struct{}
	initOnce     sync.Once
	watching     chan struct{}
	watchOnce    sync.Once
	completeOnce sync.Once
	retryInitial time.Duration
	retryMax     time.Duration

	mu         sync.Mutex
	cohort     []startupViewCohortMember
	frozen     bool
	failures   map[string]struct{}
	topology   map[string]indexer.CheckoutTopologyEvent
	revision   uint64
	latest     startupViewReadiness
	hasLatest  bool
	confirmed  bool
	finalized  bool
	onComplete func()
}

func newStartupViewReadinessMonitor(paths []string) *startupViewReadinessMonitor {
	return &startupViewReadinessMonitor{
		paths:        append([]string(nil), paths...),
		changed:      make(chan struct{}, 1),
		snapshots:    make(chan struct{}, 1),
		initialized:  make(chan struct{}),
		watching:     make(chan struct{}),
		failures:     make(map[string]struct{}),
		topology:     make(map[string]indexer.CheckoutTopologyEvent),
		retryInitial: 250 * time.Millisecond,
		retryMax:     15 * time.Second,
	}
}

func (m *startupViewReadinessMonitor) notifySnapshot() {
	if m == nil {
		return
	}
	select {
	case m.snapshots <- struct{}{}:
	default:
	}
}

func (m *startupViewReadinessMonitor) retainSnapshot(snapshot startupViewReadiness) {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.latest = snapshot
	m.hasLatest = true
	if !snapshot.complete() {
		m.confirmed = false
		m.finalized = false
	}
	m.mu.Unlock()
	m.notifySnapshot()
}

// onConfirmedComplete registers the post-publication continuation. It never
// invokes user work in the caller: a retained completion only wakes the joined
// readiness watcher, which owns finalization and therefore cannot race daemon
// teardown even when the cohort completed before registration.
func (m *startupViewReadinessMonitor) onConfirmedComplete(fn func()) {
	if m == nil || fn == nil {
		return
	}
	m.mu.Lock()
	if m.onComplete == nil {
		m.onComplete = fn
	}
	m.mu.Unlock()
	m.requestRefresh()
}

func (m *startupViewReadinessMonitor) runCompleteCallback(callback func()) {
	if m == nil || callback == nil {
		return
	}
	m.completeOnce.Do(callback)
	m.mu.Lock()
	m.finalized = true
	m.mu.Unlock()
	m.notifySnapshot()
}

func (m *startupViewReadinessMonitor) finalizationComplete() bool {
	if m == nil {
		return true
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.finalized
}

func (m *startupViewReadinessMonitor) confirmComplete(snapshot startupViewReadiness) {
	if m == nil || !snapshot.complete() {
		return
	}
	m.mu.Lock()
	m.latest = snapshot
	m.hasLatest = true
	m.confirmed = true
	callback := m.onComplete
	m.mu.Unlock()
	// Keep post-publication work inside the startup activity window: terminal
	// waiters are notified only after the continuation finishes.
	if callback != nil {
		m.runCompleteCallback(callback)
		return
	}
	m.notifySnapshot()
}

func (m *startupViewReadinessMonitor) waitTerminal(
	ctx context.Context,
) (startupViewReadiness, error) {
	if m == nil {
		return startupViewReadiness{}, nil
	}
	for {
		m.mu.Lock()
		snapshot, hasLatest, finalized := m.latest, m.hasLatest, m.finalized
		m.mu.Unlock()
		if hasLatest && (finalized || (snapshot.terminal() && !snapshot.complete())) {
			return snapshot, nil
		}
		select {
		case <-ctx.Done():
			return startupViewReadiness{}, ctx.Err()
		case <-m.snapshots:
		}
	}
}

// setPaths installs the authoritative startup ownership plan before the first
// snapshot freezes it. The monitor is constructed before Seed so transition
// events cannot be lost; Seed and ownership planning finish before this write.
func (m *startupViewReadinessMonitor) setPaths(paths []string) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.frozen {
		return
	}
	m.paths = append([]string(nil), paths...)
}

func (m *startupViewReadinessMonitor) requestRefresh() {
	if m == nil {
		return
	}
	select {
	case m.changed <- struct{}{}:
	default:
	}
}

// finishInitialSnapshot is the only admission edge for the watcher. Seed and
// the first snapshot run before it; transition events that arrive earlier are
// retained in failures and coalesced in changed. The unconditional confirming
// refresh closes the race with an event arriving during the initial probe.
func (m *startupViewReadinessMonitor) finishInitialSnapshot() {
	if m == nil {
		return
	}
	m.initOnce.Do(func() { close(m.initialized) })
	m.requestRefresh()
}

func (m *startupViewReadinessMonitor) observe(event indexer.ModeTransitionEvent) {
	if m == nil {
		return
	}
	if event.CheckoutID != "" {
		m.mu.Lock()
		if m.frozen {
			belongs, allIDsKnown := false, true
			for _, member := range m.cohort {
				if member.CheckoutID == "" {
					allIDsKnown = false
				}
				if member.CheckoutID == event.CheckoutID {
					belongs = true
				}
			}
			if !belongs && allIDsKnown {
				m.mu.Unlock()
				return
			}
		}
		if event.Failed {
			m.failures[event.CheckoutID] = struct{}{}
		} else {
			delete(m.failures, event.CheckoutID)
		}
		m.revision++
		m.mu.Unlock()
	}
	m.requestRefresh()
}

func (m *startupViewReadinessMonitor) observeTopology(event indexer.CheckoutTopologyEvent) {
	if m == nil || event.CheckoutID == "" || event.Incarnation == "" {
		return
	}
	m.mu.Lock()
	key := startupTopologyKey(event.CheckoutID, event.Incarnation)
	standing, exists := m.topology[key]
	if !exists || standing.Kind != indexer.CheckoutTopologyForgetFinalized {
		if event.Kind == indexer.CheckoutTopologyRootMoveCompleted &&
			exists && standing.Kind == indexer.CheckoutTopologyRootMoveCompleted {
			// Collapse A→B→C to A→C. A pre-freeze cohort knows A, not B.
			event.PreviousRoot = standing.PreviousRoot
		}
		m.topology[key] = event
	}
	m.revision++
	m.mu.Unlock()
	m.requestRefresh()
}

func topologyMatchesMember(
	event indexer.CheckoutTopologyEvent, member startupViewCohortMember,
) bool {
	if member.CheckoutID != "" {
		return member.CheckoutID == event.CheckoutID &&
			(member.Incarnation == "" || member.Incarnation == event.Incarnation)
	}
	return event.PreviousRoot != "" && pathkey.EqualPaths(member.Path, event.PreviousRoot)
}

func applyStartupTopology(
	member startupViewCohortMember,
	events map[string]indexer.CheckoutTopologyEvent,
) (startupViewCohortMember, bool, bool) {
	for _, event := range events {
		if !topologyMatchesMember(event, member) {
			continue
		}
		switch event.Kind {
		case indexer.CheckoutTopologyForgetFinalized:
			return member, false, false
		case indexer.CheckoutTopologyRootMoveCompleted:
			if event.CurrentRoot == "" {
				continue
			}
			changed := !pathkey.EqualPaths(member.Path, event.CurrentRoot)
			member.Path = event.CurrentRoot
			member.CheckoutID = event.CheckoutID
			member.Incarnation = event.Incarnation
			return member, true, changed
		}
	}
	return member, true, false
}

func (m *startupViewReadinessMonitor) snapshot(
	ctx context.Context, probe startupViewReadinessProbe,
) startupViewReadiness {
	if m == nil || probe == nil {
		return startupViewReadiness{}
	}
	m.mu.Lock()
	members := make([]startupViewCohortMember, 0, len(m.paths))
	if m.frozen {
		members = append(members, m.cohort...)
	} else {
		for _, path := range m.paths {
			members = append(members, startupViewCohortMember{Path: path})
		}
	}
	failures := make(map[string]struct{}, len(m.failures))
	for checkoutID := range m.failures {
		failures[checkoutID] = struct{}{}
	}
	topology := make(map[string]indexer.CheckoutTopologyEvent, len(m.topology))
	for key, event := range m.topology {
		topology[key] = event
	}
	m.mu.Unlock()

	result := startupViewReadiness{}
	cohort := make([]startupViewCohortMember, 0, len(members))
	for _, member := range members {
		// Once identity is known, apply terminal topology before probing so a
		// completed move reads its new root and a finalized forget performs no
		// stale lookup. Identity-free members are probed first to preserve ABA:
		// a newly recreated checkout at the old path must beat an older event.
		if member.CheckoutID != "" {
			var keep bool
			member, keep, _ = applyStartupTopology(member, topology)
			if !keep {
				continue
			}
		}

		readiness, err := probe(ctx, member.Path)
		if err != nil {
			// Catalog/materialization reads are transient evidence. Retain the
			// member as building and let the monitor's one bounded timer retry;
			// only an explicit lifecycle/generation failure is terminal.
			cohort = append(cohort, member)
			result.Expected++
			result.Building++
			result.ProbeErrors++
			continue
		}
		if readiness.View != nil {
			if readiness.View.CheckoutID != "" {
				member.CheckoutID = readiness.View.CheckoutID
			}
			if readiness.View.Incarnation != "" {
				member.Incarnation = readiness.View.Incarnation
			}
		}
		var keep, pathChanged bool
		member, keep, pathChanged = applyStartupTopology(member, topology)
		if !keep {
			continue
		}
		if pathChanged {
			readiness, err = probe(ctx, member.Path)
			if err != nil {
				cohort = append(cohort, member)
				result.Expected++
				result.Building++
				result.ProbeErrors++
				continue
			}
			if readiness.View != nil {
				if readiness.View.CheckoutID != "" {
					member.CheckoutID = readiness.View.CheckoutID
				}
				if readiness.View.Incarnation != "" {
					member.Incarnation = readiness.View.Incarnation
				}
			}
		}
		cohort = append(cohort, member)
		result.Expected++
		if _, failed := failures[member.CheckoutID]; member.CheckoutID != "" && failed {
			result.Failed++
			continue
		}
		switch readiness.State {
		case daemon.TrackReadinessReady:
			result.Ready++
		case daemon.TrackReadinessFailed:
			result.Failed++
		default:
			result.Building++
		}
	}
	m.mu.Lock()
	m.cohort = append(m.cohort[:0], cohort...)
	m.frozen = true
	cohortIDs := make(map[string]struct{}, len(cohort))
	allIDsKnown := true
	for _, member := range cohort {
		if member.CheckoutID == "" {
			allIDsKnown = false
			continue
		}
		cohortIDs[member.CheckoutID] = struct{}{}
	}
	if allIDsKnown {
		for checkoutID := range m.failures {
			if _, belongs := cohortIDs[checkoutID]; !belongs {
				delete(m.failures, checkoutID)
			}
		}
	}
	m.mu.Unlock()
	m.retainSnapshot(result)
	return result
}

func (m *startupViewReadinessMonitor) currentRevision() uint64 {
	if m == nil {
		return 0
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.revision
}

func publishStartupViewReadiness(
	state *daemonState, controller *realController,
) {
	if controller == nil || !controller.referenceReady.Load() {
		return
	}
	phase := "ready"
	extra := map[string]any{"queryable": controller.IsReady()}
	if controller.IsEnriched() {
		phase = "enrichment_complete"
		extra["enriched"] = true
	}
	// publishReadinessPhase applies the same controller filter used by every
	// warmup phase, converting this candidate into pending/degraded until the
	// exact-view cohort is complete.
	publishReadinessPhase(state, phase, true, extra)
}

func nextStartupViewReadinessRetry(delay, maximum time.Duration) time.Duration {
	if delay <= 0 {
		delay = time.Millisecond
	}
	if maximum < delay {
		maximum = delay
	}
	if delay >= maximum || delay > maximum/2 {
		return maximum
	}
	return delay * 2
}

func watchStartupViewReadiness(
	ctx context.Context,
	state *daemonState,
	controller *realController,
	monitor *startupViewReadinessMonitor,
	probe startupViewReadinessProbe,
) {
	if monitor == nil || controller == nil || probe == nil {
		return
	}
	monitor.watchOnce.Do(func() { close(monitor.watching) })
	select {
	case <-ctx.Done():
		return
	case <-monitor.initialized:
	}
	retryDelay := monitor.retryInitial
	if retryDelay <= 0 {
		retryDelay = 250 * time.Millisecond
	}
	retryMax := monitor.retryMax
	if retryMax < retryDelay {
		retryMax = retryDelay
	}
	var retryTimer *time.Timer
	var retryC <-chan time.Time
	var lastLogged startupViewReadiness
	hasLogged := false
	stopRetry := func() {
		if retryTimer != nil && !retryTimer.Stop() {
			select {
			case <-retryTimer.C:
			default:
			}
		}
		retryC = nil
	}
	defer stopRetry()
	scheduleRetry := func() {
		if retryC != nil {
			return
		}
		if retryTimer == nil {
			retryTimer = time.NewTimer(retryDelay)
		} else {
			retryTimer.Reset(retryDelay)
		}
		retryC = retryTimer.C
		retryDelay = nextStartupViewReadinessRetry(retryDelay, retryMax)
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-monitor.changed:
		case <-retryC:
			retryC = nil
		}
		for {
			revision := monitor.currentRevision()
			snapshot := monitor.snapshot(ctx, probe)
			controller.setStartupViewReadiness(snapshot)
			publishStartupViewReadiness(state, controller)
			if !hasLogged || snapshot != lastLogged {
				if controller.logger != nil {
					controller.logger.Info("daemon: startup exact-view progress",
						zap.Int("expected", snapshot.Expected),
						zap.Int("ready", snapshot.Ready),
						zap.Int("building", snapshot.Building),
						zap.Int("failed", snapshot.Failed),
						zap.Int("probe_errors", snapshot.ProbeErrors))
				}
				lastLogged, hasLogged = snapshot, true
			}
			// Transition observers cover explicit promotion/demotion workers, but
			// ordinary checkout route construction is owned by the coordinator and
			// has no mode-transition edge. Keep one bounded safety poll armed while
			// any frozen member is still building so a successfully published route
			// cannot leave startup readiness parked forever. The exponential cap
			// keeps a genuinely long build to one cohort probe every retryMax.
			if snapshot.Building > 0 || snapshot.ProbeErrors > 0 {
				scheduleRetry()
			} else {
				stopRetry()
				retryDelay = monitor.retryInitial
				if retryDelay <= 0 {
					retryDelay = 250 * time.Millisecond
				}
			}
			if !snapshot.complete() {
				break
			}
			if monitor.currentRevision() != revision {
				continue
			}
			// An event that landed while the confirming probe ran already
			// updated failures before it queued this edge. Consume it before
			// choosing the completion linearization point, and re-probe now
			// rather than waiting for a third event that may never arrive.
			select {
			case <-monitor.changed:
				continue
			default:
			}
			if monitor.currentRevision() != revision {
				continue
			}
			monitor.confirmComplete(snapshot)
			if !monitor.finalizationComplete() {
				// Completion outran callback registration. The level state is
				// retained; registration queues the one edge that brings this
				// joined watcher back to run the continuation.
				break
			}
			if state != nil && state.lifecycle != nil {
				state.lifecycle.SetModeTransitionObserver(nil)
				state.lifecycle.SetCheckoutTopologyObserver(nil)
			}
			return
		}
	}
}

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
func runDaemonStart(cmd *cobra.Command, _ []string) (retErr error) {
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
	startupReporter := newDaemonStartupReporter(logger)
	defer startupReporter.Stop()

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
	state, err := buildDaemonState(logger, startupReporter.ObserveMigration)
	if err != nil {
		startupReporter.Fail(err)
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

	// View builds wait for warmup. A restart finds the graph, the routes and
	// every ref view already stored, and building them again would run through
	// the same store writer and the same topology gate the warmup tail is
	// holding — which is how a restart over a persisted graph ends up costing
	// more than the cold index that produced it. Installed here, before the
	// janitor and before the seeding that brings the first coordinator up, so
	// no build can start ahead of the gate.
	//
	// Nothing else is held: tracking a repository, seeding the catalog,
	// reading a route and serving a published generation never consult it.
	viewBuilds := indexer.NewViewBuildGate()
	state.lifecycle.SetBuildGate(viewBuilds)

	controller := &realController{
		graph:         state.graph,
		indexer:       state.indexer,
		multiIndexer:  state.multiIndexer,
		configManager: state.configManager,
		lifecycle:     state.lifecycle,
		logger:        logger,
	}
	state.readinessFilter = controller.filterReadinessPhase
	// The monitor must observe Seed transitions, but its exact cohort is frozen
	// only after Seed from the one ownership plan shared with legacy warmup.
	startupReadiness := newStartupViewReadinessMonitor(nil)
	warmupCtx, cancelWarmup := context.WithCancel(context.Background())
	startupReadinessCtx, cancelStartupReadiness := context.WithCancel(warmupCtx)
	state.lifecycle.SetModeTransitionObserver(startupReadiness.observe)
	state.lifecycle.SetCheckoutTopologyObserver(startupReadiness.observeTopology)
	var startupReadinessWG, warmupWG, eventHubWG sync.WaitGroup
	stopJanitor := func() {}
	stopEventHub := func() {}
	if state.mcpServer != nil {
		srv := state.mcpServer
		controller.toolSurface = func() (string, string, int) {
			preset, mode := srv.ActivePreset()
			return preset, mode, srv.LearnedToolCount()
		}
		// The stack builds exactly one materializer, over the lifecycle's own
		// lease manager. Taking it from here rather than building a second is
		// what keeps a control-socket probe's lease visible to the retirement
		// sweep the coordinators run.
		controller.viewMaterializer = srv.Materializer()
	}
	startupReadinessWG.Add(1)
	go func() {
		defer startupReadinessWG.Done()
		watchStartupViewReadiness(
			startupReadinessCtx, state, controller, startupReadiness, controller.TrackReadiness,
		)
	}()
	stopStartupProducers := sync.OnceFunc(func() {
		state.lifecycle.SetModeTransitionObserver(nil)
		state.lifecycle.SetCheckoutTopologyObserver(nil)
		cancelWarmup()
		cancelStartupReadiness()
		stopJanitor()
		warmupWG.Wait()
		startupReadinessWG.Wait()
		// Warmup is the only event-hub Run producer. Join it before stopping
		// and waiting the hub so Add can never race Wait.
		stopEventHub()
		eventHubWG.Wait()
	})

	// Teardown is wired into every exit path, not just the control-socket
	// one. The strict order is startup/readiness producer cancellation+join,
	// filesystem watcher drain, then the shared stack (which joins checkout
	// lifecycle workers before it closes the MultiIndexer and backend). Handler
	// drain happens inside Serve before this chain begins.
	runTeardown := installDaemonTeardown(
		controller.StopWatcher, stopStartupProducers, func() error {
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
	teardownComplete := false
	defer func() {
		if teardownComplete {
			return
		}
		// An abnormal exit still drains in-process ownership, but deliberately
		// retains the PID/runtime marker. If teardown itself panics or the
		// process aborts, advertising the store as free would be unsafe; a
		// successor will treat the marker as stale once this PID is dead.
		if teardownErr := runTeardown(); teardownErr != nil {
			logger.Error("daemon: shutdown teardown failed", zap.Error(teardownErr))
			retErr = errors.Join(retErr, teardownErr)
		}
	}()
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
		stopEventHub = v1EventHub.Stop
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
	stopJanitor = startReconcileJanitor(
		state.multiIndexer, state.lifecycle, reconcileInterval(), logger)

	if err := srv.Listen(); err != nil {
		startupReporter.Fail(err)
		return err
	}
	// The socket remains the authoritative readiness signal. The runtime
	// record now transitions from pre-socket progress to serving only after
	// Listen succeeds, and keeps the resolved backend path for out-of-band
	// readers such as `gortex repos`.
	startupReporter.Serving(state.backendPath)
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
	warmupWG.Add(1)
	go func() {
		defer warmupWG.Done()
		runtimeactivity.Begin("warmup")
		defer func() {
			runtimeactivity.End("warmup")
			releaseMemoryToOS(logger, "warmup_complete")
		}()

		start := time.Now()
		logger.Info("daemon: warmup starting")
		// Bring the checkout catalog in line with the configured repos
		// before anything re-indexes them. It is the migration path for an
		// installation that predates the catalog and the restart path for
		// one that does not: identities that already exist keep their rows
		// and their clocks (a repo that was mid-grace when the daemon
		// stopped resumes where it was, it does not restart the wait), and
		// any teardown interrupted by the stop is resumed.
		if state.lifecycle != nil {
			if err := state.lifecycle.Seed(warmupCtx); err != nil && warmupCtx.Err() == nil {
				logger.Warn("daemon: seeding the checkout catalog was incomplete", zap.Error(err))
			}
		}
		if err := warmupCtx.Err(); err != nil {
			logger.Info("daemon: warmup cancelled after catalog seed", zap.Error(err))
			return
		}
		ownership := daemonStartupOwnershipPlan(warmupCtx, state, logger)
		if err := warmupCtx.Err(); err != nil {
			logger.Info("daemon: warmup cancelled after ownership planning", zap.Error(err))
			return
		}
		startupReadiness.setPaths(ownership.managedPaths)
		startupSnapshot := startupReadiness.snapshot(startupReadinessCtx, controller.TrackReadiness)
		controller.setStartupViewReadiness(startupSnapshot)
		startupReadiness.finishInitialSnapshot()
		// markReady fires once references are resolved and the graph is
		// queryable — ahead of the slow enrichment pass — so clients can
		// start issuing find_usages / get_callers immediately. Enrichment
		// continues in this goroutine afterward and finishes at MarkEnriched.
		// Once-guarded: the warmup fires it from inside the master resolver
		// (at compute-done, the earliest queryable point) AND from its
		// unconditional post-resolve fallback, whichever comes first.
		var queryableElapsed time.Duration
		markReady := sync.OnceFunc(func() {
			if warmupCtx.Err() != nil {
				return
			}
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
		mw, warmup := warmupDaemonStateWithOwnershipContext(
			warmupCtx, state, logger, markReady, &ownership,
		)
		if err := warmupCtx.Err(); err != nil {
			if mw != nil {
				_ = mw.Stop()
			}
			logger.Info("daemon: warmup cancelled before watcher attachment", zap.Error(err))
			return
		}
		if err := controller.AttachWatcherContext(warmupCtx, mw); err != nil {
			if warmupCtx.Err() != nil || errors.Is(err, indexer.ErrIndexerClosed) {
				return
			}
			logger.Warn("daemon: watcher attachment reconciliation was incomplete", zap.Error(err))
		}
		// Drive the /v1/events SSE stream from the MultiWatcher. The hub is
		// the only consumer of mw.Events() (SetWatcher reads History(), not
		// the channel), so this can't starve any other reader. No-op when
		// the HTTP surface is disabled (v1EventHub stays nil).
		if v1EventHub != nil && mw != nil {
			eventHubWG.Add(1)
			go func() {
				defer eventHubWG.Done()
				v1EventHub.Run(mw.Events())
			}()
		}
		referenceElapsed := time.Since(start)
		logger.Info("daemon: reference enrichment complete; admitting exact startup views",
			zap.Duration("warmup", referenceElapsed),
			zap.Int("startup_views_expected", startupSnapshot.Expected))

		// This continuation is registered before the build gate opens and is
		// level-triggered by the monitor, so an empty/warm cohort and a very fast
		// transition cannot outrun it. MarkEnriched is deliberately last: status,
		// health, snapshots and agents must not observe full enrichment before
		// the routed exact corpus and all graph-dependent finalizers exist.
		finalizeEnrichment := func() {
			if warmupCtx.Err() != nil {
				return
			}
			runtimeactivity.Begin("startup_finalize")
			defer func() {
				runtimeactivity.End("startup_finalize")
				releaseMemoryToOS(logger, "startup_views_complete")
			}()

			// Community detection and process discovery remain first-use work.
			// Co-change mining is merely pre-warmed after the final corpus is
			// published, so it cannot race cold generation construction.
			if state.mcpServer != nil {
				publishReadinessPhase(state, "analysis_deferred", false, map[string]any{
					"reason": "analysis runs on first use",
				})
				state.mcpServer.PrewarmCoChange()
			}
			// These readers must observe the settled routed graph, not generation
			// zero or a partially published dedicated stack.
			logRepoOwnershipAudit(state.graph, logger)
			if state.mcpServer != nil {
				state.mcpServer.MigrateSymbolAnchors(logger)
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
		}
		startupReadiness.onConfirmedComplete(finalizeEnrichment)

		// The reference tail is done, so the exact view builds that were
		// competing with it are admitted. The level-triggered waiter below was
		// armed above; opening the gate before waiting is the progress edge.
		viewBuilds.Open()
		terminal, waitErr := startupReadiness.waitTerminal(startupReadinessCtx)
		if waitErr != nil {
			logger.Info("daemon: exact startup-view wait cancelled", zap.Error(waitErr))
			return
		}
		if !terminal.complete() {
			logger.Error("daemon: exact startup-view publication failed",
				zap.Int("expected", terminal.Expected),
				zap.Int("ready", terminal.Ready),
				zap.Int("failed", terminal.Failed))
		}
	}()

	retErr = srv.Serve()
	stopRuntimePublishers := func() {
		startupReporter.Stop()
		if state.mcpServer != nil {
			state.mcpServer.StopHealthBroadcaster()
		}
	}
	teardownErr := completeDaemonShutdown(stopRuntimePublishers, runTeardown, srv.ReleaseProcessState)
	teardownComplete = true
	if teardownErr != nil {
		logger.Error("daemon: shutdown teardown failed", zap.Error(teardownErr))
		retErr = errors.Join(retErr, teardownErr)
	}
	return retErr
}

// installDaemonTeardown builds the once-guarded teardown the caller defers
// around Server.Serve.
//
// Both exit paths close transport first. A `gortex daemon stop` is acknowledged
// by the control handler before Server.Shutdown closes its listeners; a signal
// closes them directly. Server.Serve joins admitted handlers before returning,
// and only then may this function stop producers and close the store. Running
// teardown from Controller.Shutdown instead closes SQLite beneath the handler
// that is still writing the acknowledgement and beneath every other session.
//
// Producer cancellation/join runs first while the store is still open. Warmup
// is itself the publisher of the MultiWatcher, so a blocking StopWatcher ahead
// of cancellation can deadlock inside attachment and can miss a later publish.
// Once producers have joined the pointer is stable; StopWatcher then drains its
// callbacks before the shared stack closes.
//
// closeShared is the stack teardown: the savings flush and the backend close
// that checkpoints the sqlite WAL.
func installDaemonTeardown(
	stopWatcher func(),
	stopProducers func(),
	closeShared func() error,
) func() error {
	teardown := newDaemonTeardown(stopWatcher, stopProducers, closeShared)
	return teardown
}

// newDaemonTeardown returns the once-guarded teardown itself. Split from the
// wiring so the exactly-once contract can be exercised on both call paths
// without standing up a daemon.
func newDaemonTeardown(
	stopWatcher func(), stopProducers func(), closeShared func() error,
) func() error {
	var (
		once sync.Once
		err  error
	)
	return func() error {
		once.Do(func() {
			if stopProducers != nil {
				stopProducers()
			}
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

// completeDaemonShutdown is the normal-exit ownership handoff. Runtime-state
// publishing stops before graph teardown, and the process marker is released
// only after the store has closed. The final release still runs when Close
// reports an error because the process is about to exit; it is intentionally
// not used by the abnormal/panic defer above.
func completeDaemonShutdown(
	stopRuntimePublishers func(),
	runTeardown func() error,
	releaseProcessState func(),
) error {
	if stopRuntimePublishers != nil {
		stopRuntimePublishers()
	}
	var err error
	if runTeardown != nil {
		err = runTeardown()
	}
	if releaseProcessState != nil {
		releaseProcessState()
	}
	return err
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
// interval tick, runs the checkout lifecycle sweep and then calls
// MultiIndexer.ReconcileAll. interval=0 is a no-op; the returned stop
// function can be called unconditionally.
//
// The sweep runs *before* ReconcileAll on purpose: a removed checkout's root
// no longer exists, so ReconcileAll's IncrementalReindexPaths would only
// error on the missing path without evicting anything. Sweeping first keeps
// the reconcile pass working on live repos and stops a removed checkout's
// snapshot slot and graph nodes from leaking forever.
//
// The sweep is evidence-and-clock driven: it resumes any teardown interrupted
// by a restart, then reconciles every known family, so an unreachable root
// waits out its availability grace instead of being evicted on one failed
// stat, and a genuinely removed one is forgotten only after its removal grace
// expires. The old check could tell neither apart.
func startReconcileJanitor(
	mi *indexer.MultiIndexer,
	lifecycle *indexer.CheckoutLifecycle,
	interval time.Duration,
	logger *zap.Logger,
) func() {
	if mi == nil || interval <= 0 {
		logger.Info("daemon: reconcile janitor disabled")
		return func() {}
	}
	logger.Info("daemon: reconcile janitor running", zap.Duration("interval", interval))
	return startJoinedReconcileLoop(interval, func(ctx context.Context) {
		gcedCount, reconciled := func() (int, int) {
			runtimeactivity.Begin("reconcile")
			defer runtimeactivity.End("reconcile")

			swept := 0
			if lifecycle != nil {
				report, err := lifecycle.Sweep(ctx)
				if err != nil {
					logger.Warn("janitor: checkout sweep incomplete", zap.Error(err))
				}
				swept = report.Removed
				if swept > 0 {
					logger.Info("janitor: pruned vanished checkouts",
						zap.Int("count", swept),
						zap.Int("families", report.Families))
				}
			}
			results := mi.ReconcileAllCtx(ctx)
			reconciled := 0
			for _, r := range results {
				if r != nil {
					reconciled += r.StaleFileCount + r.DeletedFileCount
				}
			}
			return swept, reconciled
		}()
		// Only a tick that changed the graph schedules reclamation. The
		// process-wide quiet gate postpones it if another subsystem is busy.
		if reconciled > 0 || gcedCount > 0 {
			releaseMemoryToOS(logger, "reconcile_janitor")
		}
	})
}

func startJoinedReconcileLoop(interval time.Duration, tick func(context.Context)) func() {
	ctx, cancel := context.WithCancel(context.Background())
	var joined sync.WaitGroup
	joined.Add(1)
	go func() {
		defer joined.Done()
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				if ctx.Err() != nil {
					return
				}
				if tick != nil {
					tick(ctx)
				}
			case <-ctx.Done():
				return
			}
		}
	}()
	return sync.OnceFunc(func() {
		cancel()
		joined.Wait()
	})
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

	// Wait until the socket is live or startup stops making progress. Store
	// migrations run before Listen and can legitimately exceed a minute on a
	// large existing database. The child publishes a PID-bound heartbeat while
	// opening/migrating, so fresh progress extends the inactivity deadline;
	// missing or stale progress still fails in 60 seconds.
	start := time.Now()
	const startupInactivityTimeout = 60 * time.Second
	const startupProgressFreshness = 10 * time.Second
	deadline := start.Add(startupInactivityTimeout)
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
		st, stateOK := daemon.ReadRuntimeState()
		if phase, active := daemonStartupProgress(st, stateOK, child.Process.Pid, time.Now(), startupProgressFreshness); active {
			deadline = time.Now().Add(startupInactivityTimeout)
			if sp != nil {
				sp.Set("", fmt.Sprintf("%s · %s", phase, time.Since(start).Truncate(100*time.Millisecond)))
			}
		}
		// Bail out early if the child has already exited — no point
		// waiting for an exited process's heartbeat to expire.
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
		time.Sleep(50 * time.Millisecond)
	}
	timeoutErr := fmt.Errorf("daemon startup made no progress for 60s; check %s", daemon.LogFilePath())
	if sp != nil {
		sp.Fail(timeoutErr)
	}
	return timeoutErr
}

func daemonStartupProgress(st daemon.RuntimeState, ok bool, childPID int, now time.Time, freshness time.Duration) (string, bool) {
	if !ok || st.PID != childPID || !st.StartupProgressFresh(now, freshness) {
		return "", false
	}
	if st.StartupPhase == daemon.StartupMigrating {
		return fmt.Sprintf("migrating schema v%d (%s)", st.MigrationVersion, st.MigrationName), true
	}
	return "opening store", true
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
	// The acknowledgement only accepts the stop. The server then closes its
	// transports, joins active request handlers, and tears down the graph stack
	// after Serve returns. Keep this budget for an unresponsive control socket;
	// it is not a store-flush deadline.
	resp, err := c.ControlWithTimeout(daemon.ControlShutdown, nil, daemonShutdownAckTimeout)
	_ = c.Close()
	// Whether or not the ack arrived, teardown may still be joining a cold
	// generation and checkpointing its WAL. Use the long graceful window in
	// both cases: a successful ack means shutdown started, not that persistence
	// has already completed.
	grace := daemonGracefulExitGrace
	switch {
	case errors.Is(err, daemon.ErrDaemonUnresponsive),
		err == nil && !resp.OK && resp.ErrorCode == daemon.ErrTimeout:
		fmt.Fprintln(w, "[gortex daemon] no shutdown ack yet — the daemon is busy; waiting for it to exit")
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
	// daemonGracefulExitGrace applies after both an accepted shutdown and an
	// unresponsive control request. In either case lifecycle workers and the WAL
	// may still be draining, so force-killing on the old 15-second schedule can
	// corrupt exactly the publication shutdown is trying to finish safely.
	daemonGracefulExitGrace = 2 * time.Minute
	// daemonStatusCardTimeout bounds the advisory Status lookups that only
	// decorate output. They must never be the reason a command hangs.
	daemonStatusCardTimeout = 3 * time.Second
	// daemonShutdownAckTimeout is how long the stop command waits for the
	// shutdown ack before switching to watching the process itself. The ack is
	// normally immediate and precedes transport/graph teardown.
	daemonShutdownAckTimeout = 30 * time.Second
)

// waitForDaemonExitWithin blocks until the daemon process pid has exited — and
// thus released the store's on-disk lock — force-killing it if a graceful
// shutdown stalls. This is what makes `daemon stop` honest: when it returns,
// the store is free for the next process, which is the foundation `daemon
// restart` stands on. Polls cheaply; the common case (a clean flush) clears in
// well under a second.
//
// grace is supplied by the caller so tests and future explicit force modes can
// choose a different policy. A normal acknowledged stop uses the same long
// window as an unacknowledged one because the ack now precedes teardown.
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
		t.AppendRow(table.Row{"state", formatDaemonWarmupState(st)})
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

func formatDaemonWarmupState(st daemon.StatusResponse) string {
	switch st.WarmupPhase {
	case "checkout_builds_pending", "degraded", "finalizing":
		state := "warming up"
		if st.WarmupPhase == "degraded" {
			state = "degraded"
		}
		if views := st.StartupViews; views != nil {
			state += fmt.Sprintf(" — exact startup views %d/%d ready", views.Ready, views.Expected)
			if views.Building > 0 {
				state += fmt.Sprintf(", %d building", views.Building)
			}
			if views.Failed > 0 {
				state += fmt.Sprintf(", %d failed", views.Failed)
			}
			if views.ProbeErrors > 0 {
				state += fmt.Sprintf(", %d probe errors", views.ProbeErrors)
			}
		} else if st.WarmupPhase == "finalizing" {
			state += " — finalizing startup"
		}
		if st.Views != nil && st.Views.BuildQueue != nil {
			queue := st.Views.BuildQueue
			parts := make([]string, 0, 2)
			if queue.InteractiveQueued > 0 {
				parts = append(parts, fmt.Sprintf("%d interactive queued", queue.InteractiveQueued))
			}
			if queue.BackgroundQueued > 0 {
				parts = append(parts, fmt.Sprintf("%d background queued", queue.BackgroundQueued))
			}
			if queue.Active || len(parts) > 0 {
				builds := "view builds"
				if queue.Active {
					builds += " active"
				}
				if len(parts) > 0 {
					builds += ", " + strings.Join(parts, ", ")
				}
				state += "; " + builds
			}
		}
		return state
	case "resolving_references":
		return "warming up (socket reachable, resolving references)"
	default:
		// Compatibility with an older daemon whose status payload predates the
		// explicit phase and exact-view progress fields.
		return "warming up (socket reachable, resolving references)"
	}
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
	// A daemon that could not take its own controller mutex inside the
	// request budget answers from the last table it computed. The heading
	// carries that caveat: an unmarked snapshot is indistinguishable from
	// the current inventory.
	suffix := daemonAggregateSuffix(st, time.Now())
	if len(st.TrackedRepos) == 0 {
		if suffix == "" {
			suffix = " (none)"
		}
		fmt.Fprintln(w, "\ntracked repos:"+suffix)
		return
	}

	rows := make([]daemon.TrackedRepoStatus, len(st.TrackedRepos))
	copy(rows, st.TrackedRepos)
	sort.Slice(rows, func(i, j int) bool {
		return rows[i].Memory.TotalBytes > rows[j].Memory.TotalBytes
	})

	fmt.Fprintln(w, "\ntracked repos:"+suffix)
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
		if r.Missing || r.Unloaded || r.ViewState == daemon.RepoViewStateBuilding ||
			r.ViewState == daemon.RepoViewStateDegraded || !repoStatusCountsKnown(r) ||
			repoIndexIsEmpty(r) {
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
		if repoStatusMemoryKnown(r) {
			attributed += r.Memory.TotalBytes
		}
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
		if repoStatusMemoryKnown(r) {
			row = append(row, formatBytes(r.Memory.TotalBytes))
		} else {
			row = append(row, "?")
		}
		if repoStatusCountsKnown(r) {
			row = append(row, r.Files, r.Nodes, r.Edges)
		} else {
			row = append(row, "?", "?", "?")
		}
		if repoStatusMemoryKnown(r) {
			row = append(row,
				formatBytes(r.Memory.NodesBytes),
				formatBytes(r.Memory.EdgesBytes),
				formatBytes(r.Memory.SearchBytes),
				formatBytes(r.Memory.VectorsBytes),
			)
		} else {
			row = append(row, "?", "?", "?", "?")
		}
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
	case r.ViewState == daemon.RepoViewStateBuilding:
		return "view building"
	case r.ViewState == daemon.RepoViewStateDegraded:
		return "view degraded"
	case !repoStatusCountsKnown(r):
		return "view counts unavailable"
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
	return !r.Missing && !r.Unloaded && repoStatusCountsKnown(r) &&
		r.Files == 0 && r.LastIndex > 0
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
		if workspaceStatusCountsKnown(ws) {
			t.AppendRow(table.Row{ws.Slug, len(ws.Repos), projects, ws.Files, ws.Nodes, ws.Edges})
		} else {
			t.AppendRow(table.Row{ws.Slug, len(ws.Repos), projects, "?", "?", "?"})
		}
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

// daemonAggregateSuffix qualifies the repo-table heading when the daemon
// served the aggregate half of its status from an earlier pass — the mutex
// that guards it was held by a track / reload / enrichment for the whole
// slice of the budget the wait was allowed. Empty for the ordinary case, so
// an uncontended status prints exactly what it has always printed.
func daemonAggregateSuffix(st daemon.StatusResponse, now time.Time) string {
	if !st.AggregateBusy {
		return ""
	}
	if st.AggregateCachedUnix <= 0 {
		// No pass has ever computed one. Any rows below came from the
		// tracked-repo registry, which is read without the mutex, so the
		// caveat is the counts rather than the table.
		return " (counts not computed — a track/reload is in progress)"
	}
	age := now.Sub(time.Unix(st.AggregateCachedUnix, 0))
	if age < 0 {
		age = 0 // clock skew must not print a negative age
	}
	return fmt.Sprintf(" (cached %s ago — a track/reload is in progress)",
		formatDuration(age.Round(time.Second)))
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
