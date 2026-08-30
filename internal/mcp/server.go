package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"go.uber.org/zap"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"github.com/zzet/gortex/internal/agents"
	"github.com/zzet/gortex/internal/analysis"
	"github.com/zzet/gortex/internal/artifacts"
	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/contracts"
	"github.com/zzet/gortex/internal/daemon"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graphview"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/llm"
	"github.com/zzet/gortex/internal/llm/registry"
	"github.com/zzet/gortex/internal/llm/svc"
	"github.com/zzet/gortex/internal/modelhint"
	"github.com/zzet/gortex/internal/platform"
	"github.com/zzet/gortex/internal/profiles"
	"github.com/zzet/gortex/internal/query"
	"github.com/zzet/gortex/internal/review"
	"github.com/zzet/gortex/internal/runtimeactivity"
	"github.com/zzet/gortex/internal/savings"
	"github.com/zzet/gortex/internal/search"
	"github.com/zzet/gortex/internal/semantic"
	"github.com/zzet/gortex/internal/telemetry"
	"github.com/zzet/gortex/internal/tokens"
)

// Version is set at build time.
var Version = "dev"

// SymbolModification records a single modification event for a symbol.
type SymbolModification struct {
	Timestamp        time.Time `json:"timestamp"`
	SignatureChanged bool      `json:"signature_changed"`
}

const (
	maxSymbolHistorySymbols   = 256
	maxSymbolHistoryPerSymbol = 32
)

// symbolHistory tracks a bounded, daemon-wide working set of recent symbol
// modifications. A per-symbol cap prevents one hot file from growing forever;
// the LRU symbol cap bounds the aggregate to 8,192 modification records.
type symbolHistory struct {
	mu      sync.Mutex
	entries map[string][]SymbolModification // symbolID → oldest-to-newest modifications
	order   []string                        // least-to-most recently modified symbol IDs
}

// Record adds a modification entry for the given symbol.
func (sh *symbolHistory) Record(symbolID string, signatureChanged bool) {
	sh.mu.Lock()
	defer sh.mu.Unlock()
	if sh.entries == nil {
		sh.entries = make(map[string][]SymbolModification)
	}

	mods := append(sh.entries[symbolID], SymbolModification{
		Timestamp:        time.Now(),
		SignatureChanged: signatureChanged,
	})
	if len(mods) > maxSymbolHistoryPerSymbol {
		copy(mods, mods[len(mods)-maxSymbolHistoryPerSymbol:])
		mods = mods[:maxSymbolHistoryPerSymbol]
	}
	sh.entries[symbolID] = mods
	sh.touchSymbolLocked(symbolID)
	for len(sh.entries) > maxSymbolHistorySymbols && len(sh.order) > 0 {
		oldest := sh.order[0]
		copy(sh.order, sh.order[1:])
		sh.order[len(sh.order)-1] = ""
		sh.order = sh.order[:len(sh.order)-1]
		delete(sh.entries, oldest)
	}
}

func (sh *symbolHistory) touchSymbolLocked(symbolID string) {
	for i, id := range sh.order {
		if id != symbolID {
			continue
		}
		copy(sh.order[i:], sh.order[i+1:])
		sh.order[len(sh.order)-1] = symbolID
		return
	}
	sh.order = append(sh.order, symbolID)
}

// Get returns the modification history for a specific symbol.
func (sh *symbolHistory) Get(symbolID string) []SymbolModification {
	sh.mu.Lock()
	defer sh.mu.Unlock()
	mods := sh.entries[symbolID]
	out := make([]SymbolModification, len(mods))
	copy(out, mods)
	return out
}

// All returns a copy of the entire modification history.
func (sh *symbolHistory) All() map[string][]SymbolModification {
	sh.mu.Lock()
	defer sh.mu.Unlock()
	out := make(map[string][]SymbolModification, len(sh.entries))
	for k, v := range sh.entries {
		cp := make([]SymbolModification, len(v))
		copy(cp, v)
		out[k] = cp
	}
	return out
}

// Server wraps the MCP server with Gortex-specific tools.
type Server struct {
	mcpServer *server.MCPServer
	// connSessions tracks daemon-socket connections registered as live
	// mcp-go client sessions so server-initiated notifications reach
	// them (ConnectSession / DisconnectSession / WithClientSession in
	// conn_session.go). Keyed by daemon session id.
	connSessions  sync.Map
	engine        *query.Engine
	graph         graph.Store
	indexer       *indexer.Indexer
	watcherMu     sync.RWMutex
	watcher       watcherHistory
	multiIndexer  *indexer.MultiIndexer
	configManager *config.ConfigManager
	// lifecycle is the shared owner of checkout track / forget side effects.
	lifecycle     *indexer.CheckoutLifecycle
	activeProject string
	// testIndexProbe caches, per repo prefix, which language families the
	// graph carries test symbols for (see testLangsIndexed). The answer
	// changes with a reindex — and with the test-edge pass that stamps those
	// symbols, which lands after the first files are indexed — so
	// resetTestIndexProbe clears it on every analysis pass rather than
	// pinning the first (possibly mid-warmup) answer for the daemon's life.
	testIndexProbe sync.Map
	// trackInFlight guards first-index runs by absolute repo path.
	// track answers before the index finishes (#326), so without this a
	// second call for the same path would start a duplicate full index:
	// TrackRepoCtx's already-tracked check only sees repos that finished.
	trackInFlight sync.Map
	// scopeWorkspace / scopeProject default-scope every query at this
	// server instance to a single (workspace, project) tuple. Set by
	// `gortex server --workspace <slug> [--scope-project <slug>]`.
	// Tool handlers consult these via the QueryScope() helper rather
	// than reading the fields directly.
	scopeWorkspace string
	scopeProject   string
	// scopeIntentDefaults gates Layer-B intent defaults: locate tools
	// start at the session repo, reach/analyze tools at the workspace.
	scopeIntentDefaults bool
	logger              *zap.Logger
	// recorder counts allow-listed, consent-gated usage telemetry. nil or
	// disabled when telemetry is off; Record is nil-safe and fail-silent so
	// the dispatch hot path never branches on it.
	recorder    *telemetry.Recorder
	communities *analysis.CommunityResult
	processes   *analysis.ProcessResult
	pageRank    *analysis.PageRankResult
	// hits holds the HITS authority/hub scores over the call graph.
	// Authority measures "depended on by load-bearing code"; the
	// search rerank consumes it as a complement to raw fan-in.
	// Rebuilt each RunAnalysis pass; guarded by analysisMu.
	hits *analysis.HITSResult
	// autoConcepts is the per-repo, LLM-free concept vocabulary mined
	// from symbol names -- the deterministic complement to LLM query
	// expansion. Rebuilt on every RunAnalysis pass; guarded by
	// analysisMu and read via getAutoConcepts.
	autoConcepts *search.AutoConcepts
	// leidenCache carries the last Leiden partition between
	// `analyze kind=clusters` calls so a re-run after only a couple
	// of packages changed re-partitions just those packages instead
	// of the whole graph. nil until the first clusters request;
	// guarded by analysisMu.
	leidenCache *analysis.LeidenPartitionCache
	// communitiesToken snapshots the graph identity that backed
	// s.communities — (NodeCount, EdgeCount, EdgeIdentityRevisions).
	// handleAnalyzeClusters reads this before calling the incremental
	// detector: if the token still matches the live graph, the cached
	// communities are reused without scanning AllNodes / AllEdges to
	// fingerprint packages. On a disk backend the fingerprint scan alone is
	// ~140s; the cache check is three scalar reads.
	communitiesToken communityCacheToken
	// hotspots is the lazily-built default-threshold (mean + 2*stddev)
	// ranking. FindHotspots' inner ComputeBetweenness pass is too expensive
	// to put on every daemon cold-start path; RunAnalysis invalidates this
	// cache and the first hotspot-consuming request rebuilds it once for that
	// analysis epoch. Reads/cache state are guarded by analysisMu; the separate
	// build mutex coalesces concurrent first requests without blocking unrelated
	// analysis snapshots while betweenness runs.
	hotspots        []analysis.HotspotEntry
	hotspotsReady   bool
	hotspotsBuildMu sync.Mutex
	analysisEpoch   uint64
	// analysisRun tracks the background on-demand analysis pass so a tool
	// call can report "running, retry in Ns" instead of blocking for
	// minutes on a whole-graph computation.
	analysisRun analysisRunState
	// hotspotsFn is a test seam for deterministic concurrency/invalidation
	// tests. Production leaves it nil and uses analysis.FindHotspots.
	hotspotsFn func(graph.Reader, *analysis.CommunityResult, float64) []analysis.HotspotEntry
	// adjacency is the compact CSR snapshot of the call / reference
	// graph, built once per RunAnalysis pass so seeded random-walk
	// queries (context_closure proximity ranking) never re-scan
	// AllNodes / AllEdges. nil until the first RunAnalysis; guarded by
	// analysisMu and read via getAdjacency.
	adjacency *analysis.AdjacencySnapshot
	// adjacencyToken snapshots the graph identity that backed
	// s.adjacency, on the same (NodeCount, EdgeCount,
	// EdgeIdentityRevisions) discipline as communitiesToken — so a
	// consumer can tell whether the snapshot still matches the live
	// graph before trusting it.
	adjacencyToken communityCacheToken
	// analysisGeneration is the small active durable-generation manifest.
	// Warm startup publishes only this header; normalized metrics and bounded
	// memberships stay in SQLite until a request asks for them.
	analysisGeneration      graph.AnalysisGenerationHeader
	analysisGenerationReady bool
	analysisPruneScheduled  atomic.Bool
	// backgroundMaintenance joins the detached analysis-generation prune
	// goroutine. Anyone closing the backend store must DrainBackground
	// first: an in-flight prune batch survives sql.DB.Close on its pinned
	// connection and its next WAL commit recreates store files
	// mid-teardown. Add is gated by the mutex + drained flag so it never
	// races DrainBackground's Wait, and a prune requested after the drain
	// is dropped rather than run against a closing store.
	backgroundMaintenance        sync.WaitGroup
	backgroundMaintenanceMu      sync.Mutex
	backgroundMaintenanceDrained bool
	analysisMaterializeMu        sync.Mutex
	analysisMu                   sync.RWMutex

	// cochange caches the git-history co-change graph. cochangeByFile
	// maps a file path to its co-changing file paths and association
	// scores (0..1); cochangeCount holds the matching commit-overlap
	// counts. Lazily populated once per daemon lifetime by
	// ensureCoChange — mining git log and materialising EdgeCoChange
	// edges. The find_co_changing_symbols tool reads both maps; the
	// search rerank pipeline consumes cochangeByFile as the co_change
	// signal via buildRerankContext.
	cochangeOnce   sync.Once
	cochangeMu     sync.RWMutex
	cochangeByFile map[string]map[string]float64
	cochangeCount  map[string]map[string]int

	// autoIndexOnce guards the opt-in (GORTEX_AUTOINDEX=1) zero-config
	// background index of an untracked cwd, fired at most once per session
	// from the first tool call. See auto_index.go.
	autoIndexOnce sync.Once

	// worktreeMismatch is computed once per server: true when the working
	// directory is a linked git worktree the indexed graph does not cover,
	// so read tools can warn that results reflect another checkout.
	worktreeMismatchOnce sync.Once
	worktreeMismatch     bool

	// artifacts caches the materialised `.gortex.yaml::artifacts`
	// manifest. artifactEntries is the configured manifest (installed
	// via SetArtifacts); artifactList is the result of materialising
	// it into KindArtifact nodes + EdgeReferences edges, lazily and
	// once per daemon lifetime, by ensureArtifacts.
	artifactsOnce   sync.Once
	artifactsMu     sync.RWMutex
	artifactEntries []config.ArtifactEntry
	artifactList    []artifacts.Artifact

	// namedQueries holds the config-defined `queries:` bundles —
	// reusable detector selections runnable via analyze kind=named.
	// Installed via SetNamedQueries; merged with the built-in
	// bundles at call time.
	namedQueries []config.NamedQuery

	// session / symHistory / tokenStats are the shared-default per-client
	// state for the embedded stdio path (one implicit client per process).
	// Tool handlers reach per-session activity via sessionFor(ctx); that
	// helper returns this default when ctx carries no session ID.
	session      *sessionState
	symHistory   *symbolHistory
	tokenStats   *tokenStats
	localization *localizationTerminalState

	// sessions multiplexes per-client sessionLocal for the daemon
	// transport. When ctx carries a session ID (WithSessionID), handlers
	// resolve through this map; otherwise the shared fields above are
	// used.
	sessions *sessionMap

	// degradedNote is a one-line operating-mode warning prepended to the
	// initialize instructions — see SetDegradedNote. Guarded because the
	// embedded entry point sets it after NewServer, concurrently with
	// the first client handshakes.
	degradedMu   sync.RWMutex
	degradedNote string

	guardRules   []config.GuardRule
	architecture config.ArchitectureConfig
	// eventRules is the declarative event-boundary rule family, installed via
	// SetEventRules alongside SetArchitecture and evaluated by change_contract.
	eventRules []config.EventRule
	// searchCfg carries the `.gortex.yaml::search` block — rerank
	// weights plus the search-behaviour knobs (keyword-soup rewrite,
	// equivalence-class expansion, prose indexing). Installed via
	// SetSearchConfig right after NewServer; the zero value keeps
	// every knob at its documented default.
	searchCfg config.SearchConfig
	// equivalence is the curated software-concept synonym table
	// (plus any repo-custom classes from searchCfg.EquivalenceExtra).
	// Built once by SetSearchConfig; immutable thereafter so no lock
	// is needed. Nil until SetSearchConfig runs -- callers nil-check.
	equivalence      *search.EquivalenceTable
	contractRegistry *contracts.Registry
	semanticMgr      *semantic.Manager
	// refsConfirmed is the lazy-enrichment ledger: symbol IDs whose incoming
	// references have been confirmed on demand (via ConfirmSymbolRefs) this
	// daemon session, so a repeat usages query serves from the confirmed graph
	// instead of re-spawning the language server. An entry is recorded after
	// the first attempt (success or empty) to bound per-query LSP work.
	refsConfirmed sync.Map // symbolID → struct{}
	feedback      *feedbackManager
	notes         *notesManager
	memories      *memoryManager
	// promotedTools is the per-workspace learned tool surface: deferred
	// tools promoted into the eager surface after use, persisted so the
	// learning survives daemon restarts (with demotion after disuse). Nil
	// until InitLearnedTools wires it.
	promotedTools *promotedToolsManager
	// globalMemories holds the user-level memory store shared across
	// every workspace this user touches — lives at ~/.gortex/memories/.
	// Tools default to the workspace store; `scope:"global"` routes
	// to this one. Nil when InitMemories was called with the legacy
	// single-arg surface or when the user-home cannot be resolved.
	globalMemories *memoryManager
	// notebook is the repository-local persistent notebook —
	// .gortex/notebook/<id>.md files committed alongside the repo.
	// Nil until InitNotebook fires; tools surface a clear error.
	notebook *notebookManager
	combo    *comboManager
	frecency *frecencyTracker
	// suppressions holds the durable per-repo review-finding FP suppression
	// store (sidecar-backed). The review gate consults it to drop known false
	// positives by stable identity; the suppress_finding tool mutates it. Nil
	// until InitSuppressions fires; the review flow tolerates a nil store.
	suppressions *suppressionManager

	// mutationReceipts tracks watcher tickets that outlive an edit response.
	// Safety-critical graph consumers wait on these receipts before reading the
	// graph, so a slow patch is explicit rather than a silent stale read. Test
	// servers may shorten the two bounded waits; zero selects production
	// defaults. sync.Map keeps directly-constructed servers usable.
	mutationReceipts    sync.Map
	mutationReindexWait time.Duration
	mutationSafetyWait  time.Duration

	// mutationCommits is the durable disk-commit ledger for the single-file
	// mutating tools (mutation_commit.go). It answers the question the
	// transport cannot: when a tool call is abandoned at its deadline, did the
	// bytes actually land? The zero value is usable, so directly-constructed
	// test and embedded servers need no constructor wiring.
	mutationCommits mutationCommitLedger

	// mutationPreCommitHook is a fault-injection seam, nil in production. It
	// fires between registering a commit and the cancellation gate that guards
	// the write — the window a real deadline hits when it lands after the
	// mutation lock was taken but before the bytes are renamed into place.
	// That window cannot be entered from outside: the handler crosses it in
	// microseconds, so without this seam the gate is untestable and could
	// regress unnoticed.
	mutationPreCommitHook func(*mutationCommitRecord)

	// batchTransactions holds daemon-lifetime delivery receipts for atomic
	// batch edits. sync.Map's zero value keeps directly-constructed test and
	// embedded servers usable without constructor wiring. The write/remove
	// overrides are narrow target-mutation fault-injection seams;
	// batchDurabilityOverride covers journal, fsync, rollback, and cleanup
	// ordering. Production leaves all three nil.
	batchTransactions       sync.Map
	batchWriteOverride      func(string, []byte, os.FileMode) error
	batchRemoveOverride     func(string) error
	batchDurabilityOverride *batchDurabilityOps

	// physicalEvidenceOverride is a narrow fault-injection seam for read_file's
	// physical-evidence path. The post-read confinement guards only fire when
	// the observed resolution disagrees with the requested path, which on a real
	// filesystem needs a won symlink-flip race — untestable without this. Tests
	// hand back a resolution outside every repo root and assert the handler
	// refuses. Production leaves it nil.
	physicalEvidenceOverride func(string) ([]byte, physicalReadEvidence, error)

	// packCache retains recent smart_context pack views keyed by pack
	// root so a later call with delta_from=<root> returns only the
	// added/removed/changed symbols vs that prior pack. Always non-nil
	// after NewServer.
	packCache *packDeltaCache

	// pprCache memoizes seeded Random-Walk-with-Restart (Personalized
	// PageRank) results, keyed by content-addressed per-package Merkle
	// roots so only walks touching a changed package recompute. Shared
	// by the rerank ProximitySignal and context_closure. Always
	// non-nil after NewServer.
	pprCache *pprWalkCache

	// queryLog is the append-only retrieval query log (JSONL). It
	// records every retrieval-shaped tool call so offline recall
	// tuning and the eval harness have a substrate to measure. Always
	// non-nil after NewServer; a disabled logger is a cheap no-op.
	queryLog *queryLogger

	// prCache is a short-TTL cache of fetched forge.PR values keyed by
	// (repo, number), shared across the PR data tools so a triage
	// fan-out plus a follow-up impact call reuse one fetch. Always
	// non-nil after NewServer.
	prCache *prCache

	// diagBroadcaster forwards LSP `publishDiagnostics` payloads to
	// MCP clients as `notifications/diagnostics`. Lazy-initialised by
	// SetLSPDiagnosticsBroadcasting; nil until then.
	diagBroadcaster *diagnosticsBroadcaster

	// readinessBroadcaster fans `notifications/workspace_readiness`
	// at each warmup phase transition + re-index completion. Eagerly
	// constructed in NewServer; the publisher (daemon entrypoint)
	// calls Server.PublishReadiness at each phase.
	readinessBroadcaster *readinessBroadcaster

	// healthBroadcaster fans `notifications/daemon_health` on a
	// periodic ticker. Eagerly constructed in NewServer; the daemon
	// entrypoint wires the snapshot fn via AttachHealthSnapshot.
	healthBroadcaster *healthBroadcaster
	indexHealth       indexHealthCache

	// staleRefsBroadcaster fans `notifications/stale_refs` per session
	// when the watcher reports symbol churn in a file the session has
	// touched. Eagerly constructed in NewServer; wired to the
	// watcher's symbol-change callback via SetWatcher.
	staleRefsBroadcaster *staleRefsBroadcaster

	// graphInvalidatedBroadcaster fans `notifications/graph_invalidated`
	// to subscribers whenever the graph is rebuilt — a coarse
	// "drop your caches" signal. Fired from RunAnalysis.
	graphInvalidatedBroadcaster *graphInvalidatedBroadcaster

	// sanitizeInjection gates the prompt-injection screening
	// middleware (see sanitize.go). Set from GORTEX_MCP_SANITIZE in
	// NewServer; on by default.
	sanitizeInjection bool

	// ToolCallTimeout overrides the per-tool-call deadline enforced by the
	// hang firewall (see tool_deadline.go). Zero takes the value from
	// GORTEX_MCP_TOOL_TIMEOUT, falling back to DefaultToolCallTimeout;
	// negative disables the bound. Exported so tests and embedders can
	// tighten it without touching the environment.
	ToolCallTimeout time.Duration

	// llmService is the optional LLM service backing the `ask` MCP tool
	// and the `search_symbols` assist modes. nil until SetLLMService is
	// called by the daemon entrypoint. The service wraps whichever
	// provider `llm.provider` selects (local llama.cpp / Anthropic /
	// OpenAI / Ollama); when the provider can't be constructed it
	// reports Enabled() == false and the dependent tools stay absent.
	llmService *svc.Service

	// resourcesNotifier overrides the live mcpServer when pushing
	// `notifications/resources/updated`. Test-only: production code
	// leaves it nil so the live server is used.
	resourcesNotifier resourcesUpdatedNotifier

	// reviewLLMGenOverride substitutes the review tool's plain (non-usage) LLM
	// re-location seam. Test-only: production leaves it nil and
	// reviewLLMGenWithUsage builds the closure over llmService.GenerateWithUsage.
	// A non-nil override is adapted up to the usage-aware shape (reporting zero
	// usage) so a test that only needs to drive the LLM review phase — without
	// asserting cost — can still do so without constructing a real provider.
	reviewLLMGenOverride func() review.LLMGen

	// reviewLLMGenWithUsageOverride substitutes the review tool's usage-aware
	// LLM seam (the one that feeds the per-review CostBreakdown). Test-only:
	// production leaves it nil and reviewLLMGenWithUsage builds the closure
	// over llmService.GenerateWithUsage. A non-nil override is returned
	// directly (gated on use_llm) so a test can drive the cost-bearing review
	// path — and assert the returned usage shows up in the response — without
	// constructing a real provider.
	reviewLLMGenWithUsageOverride func() review.LLMGenWithUsage

	// reviewPricingOverride substitutes the rate card the review cost block
	// prices token usage against. Test-only: production reads it from the LLM
	// service (Pricing). A non-nil override lets a test assert a deterministic
	// USD estimate from a stubbed usage seam.
	reviewPricingOverride *llm.ProviderPricing

	// sourceLiteralRecallBudgetOverride replaces the per-anchor wall-clock
	// slice the bounded source-literal recall may spend. Test-only:
	// production leaves it zero and gets exploreSourceLiteralRecallBudget.
	// A test that asserts which owners the recall maps widens the slice so
	// the answer cannot depend on how loaded the machine is; the tests that
	// assert deadline behaviour leave it zero. See
	// pinExploreSourceLiteralRecallBudget.
	sourceLiteralRecallBudgetOverride time.Duration

	// divergentDefaultFallbackSLOOverride replaces the wall-clock slice
	// explore's divergent-default-owner fallback may spend. Test-only, same
	// contract as sourceLiteralRecallBudgetOverride above: production leaves
	// it zero and gets exploreDefaultOwnerFallbackSLO. A test that asserts
	// WHICH owner the fallback promotes widens the slice so the answer cannot
	// depend on how loaded the machine is. See
	// pinExploreDivergentDefaultFallbackSLO.
	divergentDefaultFallbackSLOOverride time.Duration

	// critiqueLLMGenOverride substitutes the critique_review tool's LLM seam.
	// Test-only: production leaves it nil and the handler builds the closure
	// over llmService.Generate. A non-nil override stands in for the real
	// provider so a test can drive the self-critique pass without constructing
	// a backend.
	critiqueLLMGenOverride func() review.LLMGen

	// proxyHydrate is the cross-daemon proxy-edge lazy hydration hook.
	// nil unless the daemon installed one (federation.edges on); the
	// traversal tools call it to pull a proxy node's neighbour ring before
	// crossing into it. See proxy_hydrate.go.
	proxyHydrate func(ctx context.Context, proxyID string) (int, error)

	// toolScopes is the per-Server tool-name → ToolScope registry.
	// Populated by registerToolWithScope as tools are added; consulted
	// by ResolveToolScope before each handler runs.
	toolScopes *scopeRegistry

	// agentReg is the in-memory multi-agent coordination registry backing
	// the agent_registry tool (presence, cursors, advisory path locks).
	agentReg *agentRegistry

	// overlays is the optional editor-overlay manager. When non-nil,
	// every `tools/call` whose session carries overlay buffers is
	// wrapped (via s.addTool → wrapToolHandler) with the per-request
	// shadow-graph middleware that builds an OverlaidView over the
	// immutable base graph. Wired post-construction by
	// SetOverlayManager.
	overlays *daemon.OverlayManager

	// materializer builds the composed reader a routed checkout is served
	// through. Nil when the backing store carries no view catalog, in which
	// case every request reads the base corpus exactly as it did before
	// routed views existed. Wired post-construction by SetMaterializer.
	materializer *graphview.Materializer

	// overlayLayerCache memoises per-session parsed overlay layers
	// keyed by (sessionID, content-hash sum). Cache hits avoid the
	// per-request re-parse when an editor pushes the same buffer to a
	// long sequence of tool calls. Entries are dropped on overlay
	// session Drop / Push / Delete via overlayCacheInvalidate.
	overlayLayerCache   sync.Map // map[string]*overlayLayerCacheEntry; key = layerCacheKey
	overlayLayerBuildMu sync.Mutex

	// registerOverlayToolsOnce gates the overlay MCP tool family
	// (overlay_register / overlay_push / overlay_list /
	// overlay_delete / overlay_drop / compare_with_overlay) so a
	// second SetOverlayManager call doesn't double-register them.
	registerOverlayToolsOnce sync.Once

	// remoteOverrides bridges the session proxy-toggle tools
	// (proxy_enable / proxy_disable / proxy_status) to the daemon's
	// per-session remote-override state. nil in embedded mode (no
	// per-connection daemon session). registerProxyToolsOnce gates the
	// tool registration the way the overlay family is gated.
	remoteOverrides        RemoteOverrideSink
	registerProxyToolsOnce sync.Once

	// lazy owns the deferred-tool catalog backing the tools_search
	// discovery tool. Tools whose names are not in hotEagerTools land
	// here instead of in the live MCP server; tools_search returns
	// their schemas on demand and promotes them via lazy.promote (a
	// closure wired in attachLazyRegistry). Nil only when the registry
	// is explicitly disabled — see lazyEnabledFromEnv.
	lazy *lazyToolRegistry

	// facades maps the stable facade-v1 operation surface to the existing
	// handlers. It captures registrations before lazy routing, so a facade can
	// invoke a cold legacy implementation without promoting or re-advertising
	// that legacy tool.
	facades *facadeRegistry

	// toolPolicy restricts the published tool surface to a named preset
	// / allow-deny set (see tool_presets.go). Resolved at construction
	// from MultiRepoOptions.ToolPolicy (the mcp.tools config block) plus
	// the GORTEX_TOOLS / GORTEX_TOOLS_MODE env overrides. Nil or
	// inactive means the full surface. Enforced in hide mode by
	// toolSurfaceFilter + checkToolGate and in defer mode by the lazy
	// registry's eager predicate.
	toolPolicy *toolPolicy
	// toolPolicyOperatorPinned records whether the global policy came
	// from a deliberate operator configuration (mcp.tools beyond the
	// shipped default, or GORTEX_TOOLS) — when true, the active
	// instruction profile does not adjust session tool surfaces.
	toolPolicyOperatorPinned bool

	// toolBudgetOnce / toolBudgetCached memoise the project-size-scaled
	// exploration-call budget appended to navigation tools' descriptions
	// (see tool_budget.go). Computed once from the graph node count.
	toolBudgetOnce   sync.Once
	toolBudgetCached string

	// scopesOnce / scopes is the lazily-initialised, JSON-file-backed
	// saved-scope store (see scopes.go).
	scopesOnce sync.Once
	scopes     *scopeStore
}

// sessionFor returns the session-scoped state for the current request.
// If ctx was wrapped with WithSessionID, the per-session entry is used
// (created on first access). Otherwise the shared default is returned,
// preserving embedded-mode behavior exactly.
//
// Never returns nil — callers can chain `.recordFile(...)` etc.
// unconditionally.
func (s *Server) sessionFor(ctx context.Context) *sessionState {
	id := SessionIDFromContext(ctx)
	if id == "" || s.sessions == nil {
		return s.session
	}
	return s.sessions.get(id).session
}

// ReleaseSession drops per-session state for id. Called by the daemon
// when a proxy disconnects, so idle entries don't accumulate for the
// lifetime of the daemon process. Cascades into the diagnostics
// broadcaster so a disconnecting subscriber's slot is reclaimed.
func (s *Server) ReleaseSession(id string) {
	if id == "" {
		return
	}
	if s.sessions != nil {
		s.sessions.release(id)
	}
	if s.diagBroadcaster != nil {
		s.diagBroadcaster.unsubscribe(id)
	}
	if s.readinessBroadcaster != nil {
		s.readinessBroadcaster.unsubscribe(id)
	}
	if s.healthBroadcaster != nil {
		s.healthBroadcaster.unsubscribe(id)
	}
	if s.staleRefsBroadcaster != nil {
		s.staleRefsBroadcaster.unsubscribe(id)
	}
	if s.graphInvalidatedBroadcaster != nil {
		s.graphInvalidatedBroadcaster.unsubscribe(id)
	}
	// Editor-overlay sessions are pinned to the MCP session that
	// registered them. When the MCP session ends — for any reason —
	// the overlay must die immediately. Holding overlay state past
	// the disconnect would (a) leak unsaved buffer content into the
	// daemon's address space indefinitely and (b) let a future
	// connection that learns or guesses the same session ID re-
	// attach to abandoned buffers; that's a credential / data-leak
	// vector we don't want. The TTL is a fail-safe for "client
	// crashed and we never observed the disconnect"; this is the
	// fast path that closes the window when we DO observe it.
	if s.overlays != nil {
		s.overlays.Drop(id)
	}
	s.overlayCacheInvalidate(id)
}

// sessionState tracks recent agent activity for context recovery after compaction.
type sessionState struct {
	mu             sync.Mutex
	viewedSymbols  []string // recently viewed symbol IDs (most recent first)
	viewedFiles    []string // recently viewed file paths
	modifiedFiles  []string // files modified via edit_symbol
	recentSearches []string // recent search queries
	// clientName is the MCP client identifier (claude-code / cursor / vscode /
	// zed / …) captured from the protocol's `initialize.clientInfo.name`
	// field by the daemon dispatcher. Drives the per-session default
	// wire format: known-decoder clients get `gcx` by default, others
	// fall back to JSON. Empty until the dispatcher sees the
	// `initialize` frame.
	clientName string
	// toolSpec / toolMode are the client-forwarded tool-surface
	// preference (GORTEX_TOOLS / --tools + mode) relayed by the daemon
	// from the connection handshake. They drive the per-session effective
	// tool policy: a forwarded spec wins over the client-name default,
	// which wins over the server's global preset. Empty = no preference.
	toolSpec string
	toolMode string
	// resolvedToolPolicy caches the effective per-session policy so it is
	// derived once per session, not on every tools/list. Invalidated when
	// toolSpec / clientName is (re)recorded.
	resolvedToolPolicy *toolPolicy
	toolPolicyResolved bool
	// lastSearch captures the most recent search_symbols call so that a
	// subsequent get_symbol_source / get_editing_context on one of its
	// results can be attributed back to the query — this is the raw input
	// to the combo tracker. Reset on every search.
	lastSearch lastSearchState

	// Workspace scope for this session, resolved lazily from the
	// session cwd on first query and cached here. scopeResolved
	// guards the one-time resolution. scopeBound is true when the
	// session has a cwd: query handlers then confine every result to
	// scopeWorkspaceID — the hard, un-widenable workspace boundary.
	// When scopeBound is true and scopeWorkspaceID names no real
	// workspace, the cwd matched no tracked repo and handlers fail
	// closed rather than widening to the global graph. scopeRepoPrefix
	// / scopeProjectID feed relevance ranking (same-repo > same-project
	// > same-workspace); they never relax the boundary.
	//
	// scopeRepoAllow is the session's HARD repo ceiling. It is nil for
	// the common shapes, where scopeWorkspaceID is the whole boundary.
	// It is non-nil (and never empty) for a session opened at a root
	// ABOVE its repos whose contained repos span more than one
	// workspace: there is no single slug to bind to, so the contained
	// repo set is itself the boundary and scopeWorkspaceID stays empty.
	// Every scope consumer MUST fold it in — an empty workspace slug
	// with no repo ceiling is the unbounded global graph.
	// scopeWorkspaces are the slugs those repos resolve to; they let a
	// `workspace:` selector narrow within the session, never widen it.
	// scopeCWD records the cwd the binding was derived from so a
	// transport that revises a session's cwd (Streamable HTTP) re-resolves
	// instead of serving the stale — possibly unbound — boundary.
	scopeResolved    bool
	scopeBound       bool
	scopeWorkspaceID string
	scopeProjectID   string
	scopeRepoPrefix  string
	scopeRepoAllow   map[string]bool
	scopeWorkspaces  map[string]bool
	scopeCWD         string

	// planningMode, when true, removes every editing tool from this
	// session's tool surface and hard-blocks edit calls — a runtime
	// no-writes guarantee toggled by set_planning_mode (tools_mode.go).
	planningMode bool

	// workflow, when non-nil, is the active phase-enforcement state
	// machine for this session (see tools_workflow.go).
	workflow *workflowState

	// responses is the ring of recent large tool responses captured
	// for the post-filter tools (ctx_grep / ctx_slice / …). Allocated
	// lazily on first capture.
	responses *responseBuffer

	// momentumReads counts this session's read/navigate tool calls;
	// momentumNudged latches the one-shot momentum note (momentum.go).
	// momentumStreak counts CONSECUTIVE granular reads (broken by
	// explore / smart_context and by any non-read call);
	// momentumEscalated latches the one-shot escalation note and
	// momentumExploreUsed records whether explore ran this session
	// (it drives the escalation's wording).
	momentumReads       int
	momentumNudged      bool
	momentumStreak      int
	momentumEscalated   bool
	momentumExploreUsed bool

	// cursor is the per-session stateful navigation cursor used by the
	// nav tool — a current symbol plus a back-history. Allocated lazily
	// on the first nav call and freed with the rest of sessionState on
	// disconnect.
	cursor *navCursor

	// emittedCues records which proactive related_tools discovery cues have
	// already been shown this session, so each is emitted at most once (a
	// repeated hint is noise). Keyed by the cue's tool name.
	emittedCues map[string]bool
}

// markCueOnce records that a related_tools cue keyed by `key` has been
// emitted this session; it returns true only the FIRST time, so a caller
// emits the cue iff this returns true.
func (ss *sessionState) markCueOnce(key string) bool {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	if ss.emittedCues[key] {
		return false
	}
	if ss.emittedCues == nil {
		ss.emittedCues = make(map[string]bool, 4)
	}
	ss.emittedCues[key] = true
	return true
}

type lastSearchState struct {
	query string
	// returned is the result IDs in rank order (0 = top); returnedIDs
	// maps an ID to its rank for O(1) membership + rank lookup. fileIDs
	// maps each canonical returned Node.FilePath to the IDs on that page.
	// consumed tracks which returned IDs the agent went on to use, so a
	// later search can record an implicit "skip-above" negative for the
	// higher-ranked results that were passed over.
	returned    []string
	returnedIDs map[string]int
	fileIDs     map[string][]string
	consumed    map[string]struct{}
	at          time.Time
}

// tokenStats tracks estimated token savings for the current session. When a
// savings.Store is attached, each record() call also increments the persistent
// cumulative totals so "Gortex saved $X this month"-style narratives survive
// server restarts.
//
// parent, when non-nil, is the process-wide aggregate (s.tokenStats) that
// every per-session counter feeds. Without the fan-out, a fresh session's
// counter is zero-initialised and graph_stats called from any new session
// always reports `token_savings.calls_counted: 0` even when the daemon has
// served thousands of source-fetching calls — the shared default never
// receives observations because every real call carries a session ID. The
// parent link aggregates so graph_stats shows a meaningful live total.
type tokenStats struct {
	mu             sync.Mutex
	tokensSaved    int64 // cumulative tokens saved vs reading full files
	tokensReturned int64 // cumulative tokens actually returned
	callCount      int64 // number of source-reading tool invocations
	persistent     *savings.Store
	parent         *tokenStats // process-wide aggregate (nil for the root)
	repoPath       string      // forwarded to savings for per-repo aggregation
	sessionID      string      // rides on persisted events; "" for the shared default
	clientName     string      // MCP client app (claude-code / cursor / …) from initialize; rides on events
}

// setClientName records the MCP client app on the session's tokenStats so
// per-call savings observations can be attributed to it. Mirrors the
// sessionState.recordClientName the daemon dispatcher already calls.
func (ts *tokenStats) setClientName(name string) {
	if ts == nil || name == "" {
		return
	}
	ts.mu.Lock()
	ts.clientName = name
	ts.mu.Unlock()
}

// record adds a single savings observation. node is the symbol whose
// source was returned — its RepoPrefix and Language are folded into the
// per-repo / per-language buckets in the persistent store. node may be
// nil for code paths that don't have a node handle, in which case the
// observation only contributes to top-line totals. tool is the MCP tool
// name that produced the call (e.g. "get_symbol_source") and is recorded
// in the JSONL event log for the dashboard's per-tool breakdown.
//
// returned and fullFile are token counts (cl100k_base via internal/tokens).
func (ts *tokenStats) record(node *graph.Node, tool string, returned, fullFile int64) {
	ts.mu.Lock()
	saved := max(fullFile-returned, 0)
	ts.tokensSaved += saved
	ts.tokensReturned += returned
	ts.callCount++
	store := ts.persistent
	parent := ts.parent
	fallbackRepo := ts.repoPath
	sessionID := ts.sessionID
	clientName := ts.clientName
	ts.mu.Unlock()

	// Fan out to the process-wide aggregate so graph_stats called
	// outside this session — or from a freshly-created session that
	// has not itself made a recorded call yet — sees a live counter
	// that reflects daemon-wide activity. The parent never has a
	// further parent (we only nest one level deep), so this is bounded.
	if parent != nil {
		parent.mu.Lock()
		parent.tokensSaved += saved
		parent.tokensReturned += returned
		parent.callCount++
		parent.mu.Unlock()
	}

	// Repo: prefer the node's RepoPrefix so multi-repo daemons attribute
	// correctly to the actual repo the symbol lives in. Fall back to the
	// rootPath captured at InitSavings only when the node has no prefix
	// (single-repo mode).
	repo := fallbackRepo
	var language string
	if node != nil {
		if node.RepoPrefix != "" {
			repo = node.RepoPrefix
		}
		language = node.Language
	}

	// Model attribution: the MCP protocol never tells a tool server which
	// LLM is calling, but the host's hook layer can — it drops a model
	// hint keyed by the session's working directory that we read back
	// here. Best-effort: an absent/stale hint leaves the model empty and
	// the dashboard falls back to its provider-neutral estimate. Keyed by
	// the InitSavings root path (the cwd the connection was established
	// in), with the hint's own global-last fallback inside Read.
	var model string
	if h, ok := modelhint.Read(fallbackRepo); ok {
		// The hint is keyed by working directory, so on a machine where two
		// agents share a repo the wrong one can read it. Adopting a model
		// across agents is worse than recording none: it books the call
		// against another vendor's model and prices it at their rates. Only
		// take the model when the hint's agent is consistent with this
		// session's client.
		if modelhint.SameClient(h.Client, clientName) {
			model = h.Model
		}
		if clientName == "" {
			clientName = h.Client
		}
	}

	// Forward to the persistent store outside our lock — its own
	// synchronization guards concurrent writers, and the ledger write
	// shouldn't block new record() calls on the hot path.
	if store != nil {
		store.AddObservation(savings.Observation{
			Repo:      repo,
			Language:  language,
			Tool:      tool,
			SessionID: sessionID,
			Model:     model,
			Client:    clientName,
			Returned:  returned,
			Saved:     saved,
		})
	}
}

// snapshot returns a copy of the current counters for inclusion in responses.
func (ts *tokenStats) snapshot() map[string]any {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	ratio := 0.0
	if ts.tokensReturned > 0 {
		ratio = float64(ts.tokensSaved+ts.tokensReturned) / float64(ts.tokensReturned)
	}
	return map[string]any{
		"tokens_saved":     ts.tokensSaved,
		"tokens_returned":  ts.tokensReturned,
		"calls_counted":    ts.callCount,
		"efficiency_ratio": math.Round(ratio*10) / 10,
	}
}

const maxSessionItems = 20

func newSessionState() *sessionState {
	return &sessionState{}
}

func (ss *sessionState) recordSymbol(id string) {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	ss.viewedSymbols = prependUnique(ss.viewedSymbols, id, maxSessionItems)
}

func (ss *sessionState) recordFile(path string) {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	ss.viewedFiles = prependUnique(ss.viewedFiles, path, maxSessionItems)
}

func (ss *sessionState) recordModified(path string) {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	ss.modifiedFiles = prependUnique(ss.modifiedFiles, path, maxSessionItems)
}

func (ss *sessionState) recordSearch(query string) {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	ss.recentSearches = prependUnique(ss.recentSearches, query, 10)
}

// recordClientName captures the MCP client name from the `initialize`
// frame. Idempotent — re-init overwrites. Empty input is ignored so a
// late env-var fallback can't clobber a prior authoritative value.
func (ss *sessionState) recordClientName(name string) {
	if name == "" {
		return
	}
	ss.mu.Lock()
	defer ss.mu.Unlock()
	ss.clientName = name
	// The client name feeds the client-aware preset default, so a late
	// authoritative name (from the `initialize` frame) must re-resolve the
	// session's effective tool policy.
	ss.resolvedToolPolicy = nil
	ss.toolPolicyResolved = false
}

// recordToolPolicy captures the client-forwarded tool-surface preference
// relayed from the connection handshake. Invalidates the cached
// effective policy so the next tools/list re-resolves. Idempotent.
func (ss *sessionState) recordToolPolicy(spec, mode string) {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	if ss.toolSpec == spec && ss.toolMode == mode && ss.toolPolicyResolved {
		return
	}
	ss.toolSpec = spec
	ss.toolMode = mode
	ss.resolvedToolPolicy = nil
	ss.toolPolicyResolved = false
}

// snapshotClientName returns the captured client name under the
// session lock. Returns empty when the `initialize` frame hasn't
// arrived yet (very early tool calls — rare but possible during
// boot races).
func (ss *sessionState) snapshotClientName() string {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	return ss.clientName
}

// NoteSessionClient is called by the daemon dispatcher after it
// snoops the MCP `initialize.clientInfo.name` value, so the per-
// session sessionState can default tool wire-format based on the
// client's decoder capability. Idempotent and safe to call before
// the session is registered (no-op until sessionFor materialises
// the entry).
func (s *Server) NoteSessionClient(sessionID, name, version string) {
	if s == nil || sessionID == "" || name == "" {
		return
	}
	// Fold the client app into opt-in telemetry (consent-gated, fail-silent,
	// version dropped + name bounded by the recorder's sanitiser). Captures
	// which agents drive Gortex without ever recording a session identifier.
	telemetry.RecordClient(s.recorder, name)
	if s.sessions == nil {
		// Embedded mode — single shared session; just record on the
		// shared state.
		if s.session != nil {
			s.session.recordClientName(name)
		}
		s.tokenStats.setClientName(name)
		return
	}
	entry := s.sessions.get(sessionID)
	entry.session.recordClientName(name)
	// Mirror onto the session's tokenStats so per-call savings
	// observations carry the client app for the per-client breakdown.
	entry.tokenStats.setClientName(name)
	_ = version // reserved for per-version capability gates
}

// NoteSessionToolPolicy relays the client-forwarded tool-surface
// preference (from the daemon connection handshake) onto the per-session
// sessionState so tools/list and the call gate honour this client's
// preset authoritatively. Empty spec+mode is a no-op (no preference).
// Safe to call before the session is registered — sessionFor materialises
// the entry. Called once per session by the daemon dispatcher.
func (s *Server) NoteSessionToolPolicy(sessionID, spec, mode string) {
	if s == nil {
		return
	}
	if strings.TrimSpace(spec) == "" && strings.TrimSpace(mode) == "" {
		return
	}
	if s.sessions == nil {
		if s.session != nil {
			s.session.recordToolPolicy(spec, mode)
		}
		return
	}
	if sessionID == "" {
		return
	}
	s.sessions.get(sessionID).session.recordToolPolicy(spec, mode)
}

// defaultFormatForClient returns the most-compressed wire format the
// named MCP client is known to decode. Resolution order is gcx >
// toon > json:
//
//   - GCX-capable: the canonical coding clients and product aliases in
//     knownAgentClients / knownAgentHosts.
//   - TOON-capable but no GCX: kept for forward compat; today there
//     is no client we know to be in this bucket. Listed for the
//     mapping shape and as a placeholder — clients can be promoted
//     here when their plugin ships a TOON-only decoder.
//   - Everything else: empty string → JSON (the safe legacy default).
//
// Lower-cased client name is matched. Unknown clients are not a
// failure — they just keep the JSON default until they're added.
func defaultFormatForClient(name string) string {
	if isKnownAgentClient(name) {
		return "gcx"
	}
	return ""
}

// knownAgentClients is the set of canonical MCP client identifiers known to
// ship a GCX decoder. It controls wire-format negotiation only; every non-empty
// clientInfo.name receives the compact coding surface independently.
var knownAgentClients = map[string]bool{
	"claude-code":      true,
	"cursor":           true,
	"vscode":           true,
	"zed":              true,
	"aider":            true,
	"kilocode":         true,
	"opencode":         true,
	"openclaw":         true,
	"codex":            true,
	"omp-coding-agent": true,
	"pi":               true,
}

// knownAgentHosts are host-context families whose aliases name the same
// GCX-capable coding client (for example "Claude Code 1.4", "Visual Studio
// Code", or "openai-codex"). Editor-only hosts without the decoder remain on
// JSON, while still receiving the compact coding surface.
var knownAgentHosts = map[string]bool{
	"claude-code": true,
	"cursor":      true,
	"vscode":      true,
	"zed":         true,
	"codex":       true,
}

// isKnownAgentClient reports whether the named MCP client is a recognised
// coding agent (see knownAgentClients).
func isKnownAgentClient(name string) bool {
	name = strings.ToLower(strings.TrimSpace(name))
	if knownAgentClients[name] {
		return true
	}
	return knownAgentHosts[resolveHostContext(name).name]
}

// resolveSessionFormat returns the format the current session prefers
// when a tool's `format` arg is absent. Pure read — used by isGCX /
// isTOON when the caller didn't pin a format explicitly.
func (s *Server) resolveSessionFormat(ctx context.Context) string {
	if s == nil {
		return ""
	}
	sess := s.sessionFor(ctx)
	if sess == nil {
		return ""
	}
	return defaultFormatForClient(sess.snapshotClientName())
}

// comboWindow is how long after a search_symbols the session will still
// attribute a consume call (get_symbol_source / get_editing_context) back
// to that search's query for combo tracking. FFF uses a similar concept
// with a T-second window; 5 minutes is long enough for agents that
// interleave many tool calls but short enough that an unrelated later
// consume doesn't get mis-attributed.
const (
	comboWindow       = 5 * time.Minute
	lastSearchPageCap = 256
)

func normalizeSearchAttributionPath(path string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return ""
	}
	path = filepath.ToSlash(filepath.Clean(path))
	if path == "." {
		return ""
	}
	return path
}

// recordLastSearch captures one actually returned page so a later consume call
// can be credited to this query. At most lastSearchPageCap page positions are
// inspected; ranked IDs and per-file memberships are both unique and bounded.
func (ss *sessionState) recordLastSearchPage(query string, nodes []*graph.Node) {
	ss.mu.Lock()
	defer ss.mu.Unlock()

	positions := min(len(nodes), lastSearchPageCap)
	returned := make([]string, 0, positions)
	returnedIDs := make(map[string]int, positions)
	fileIDs := make(map[string][]string)
	for index := 0; index < positions; index++ {
		node := nodes[index]
		if node == nil || node.ID == "" {
			continue
		}
		if _, duplicate := returnedIDs[node.ID]; duplicate {
			continue
		}
		returnedIDs[node.ID] = len(returned)
		returned = append(returned, node.ID)
		path := normalizeSearchAttributionPath(node.FilePath)
		if path == "" {
			path = normalizeSearchAttributionPath(graph.IDFile(node.ID))
		}
		if path != "" {
			fileIDs[path] = append(fileIDs[path], node.ID)
		}
	}
	if strings.TrimSpace(query) == "" || len(returned) == 0 {
		ss.lastSearch = lastSearchState{}
		return
	}
	ss.lastSearch = lastSearchState{
		query:       query,
		returned:    returned,
		returnedIDs: returnedIDs,
		fileIDs:     fileIDs,
		consumed:    make(map[string]struct{}),
		at:          time.Now(),
	}
}

// recordLastSearch retains the legacy ID-only seam used by symbol consumers
// that do not have node metadata. File memberships are recovered from IDs when
// possible; search_symbols uses recordLastSearchPage with authoritative nodes.
func (ss *sessionState) recordLastSearch(query string, ids []string) {
	nodes := make([]*graph.Node, 0, min(len(ids), lastSearchPageCap))
	for index, id := range ids {
		if index >= lastSearchPageCap {
			break
		}
		nodes = append(nodes, &graph.Node{ID: id})
	}
	ss.recordLastSearchPage(query, nodes)
}

// attributedQuery returns the query string that should receive credit for
// consuming symbolID, or "" if no recent search eligibly returned it.
// Cleared from the caller's perspective but not from state — a single
// search can legitimately credit several consume calls.
func (ss *sessionState) attributedQuery(symbolID string) string {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	if ss.lastSearch.query == "" || symbolID == "" {
		return ""
	}
	if time.Since(ss.lastSearch.at) > comboWindow {
		return ""
	}
	if _, ok := ss.lastSearch.returnedIDs[symbolID]; !ok {
		return ""
	}
	if ss.lastSearch.consumed == nil {
		ss.lastSearch.consumed = make(map[string]struct{})
	}
	ss.lastSearch.consumed[symbolID] = struct{}{}
	return ss.lastSearch.query
}

// attributedFileConsumption returns the recent search query and the exact
// returned-page IDs recorded for path, marking each as consumed. The lookup is
// graph-free: file membership was captured with the page under this same lock.
func (ss *sessionState) attributedFileConsumption(path string) (string, []string) {
	path = normalizeSearchAttributionPath(path)
	ss.mu.Lock()
	defer ss.mu.Unlock()
	if path == "" || ss.lastSearch.query == "" || time.Since(ss.lastSearch.at) > comboWindow {
		return "", nil
	}
	ids := ss.lastSearch.fileIDs[path]
	if len(ids) == 0 {
		return "", nil
	}
	if ss.lastSearch.consumed == nil {
		ss.lastSearch.consumed = make(map[string]struct{})
	}
	matched := make([]string, 0, len(ids))
	for _, id := range ids {
		if _, consumed := ss.lastSearch.consumed[id]; consumed {
			continue
		}
		ss.lastSearch.consumed[id] = struct{}{}
		matched = append(matched, id)
	}
	if len(matched) == 0 {
		return "", nil
	}
	return ss.lastSearch.query, matched
}

// attributedConsumptionBatch preserves the legacy ID-batch seam while file
// consumers migrate to the graph-free attributedFileConsumption lookup.
func (ss *sessionState) attributedConsumptionBatch(ids []string) (string, []string) {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	if ss.lastSearch.query == "" || time.Since(ss.lastSearch.at) > comboWindow {
		return "", nil
	}
	if ss.lastSearch.consumed == nil {
		ss.lastSearch.consumed = make(map[string]struct{})
	}
	matched := make([]string, 0, len(ids))
	for _, id := range ids {
		if _, ok := ss.lastSearch.returnedIDs[id]; !ok || id == "" {
			continue
		}
		ss.lastSearch.consumed[id] = struct{}{}
		matched = append(matched, id)
	}
	return ss.lastSearch.query, matched
}

// drainSkippedNegatives computes the implicit "skip-above" negatives for
// the current last-search: the results ranked above the deepest one the
// agent actually consumed but that were themselves never consumed — i.e.
// passed over. Called when a search is about to be superseded (the state
// is overwritten right after), so each skip is emitted at most once.
// Returns ("", nil) when the agent consumed nothing or only the top hit.
func (ss *sessionState) drainSkippedNegatives() (string, []string) {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	ls := &ss.lastSearch
	if ls.query == "" || len(ls.consumed) == 0 || len(ls.returned) == 0 {
		return "", nil
	}
	maxRank := -1
	for id := range ls.consumed {
		if r, ok := ls.returnedIDs[id]; ok && r > maxRank {
			maxRank = r
		}
	}
	if maxRank <= 0 {
		return "", nil // top pick (or nothing) — nothing was skipped over
	}
	var skipped []string
	for i := 0; i < maxRank && i < len(ls.returned); i++ {
		id := ls.returned[i]
		if _, ok := ls.consumed[id]; ok {
			continue
		}
		skipped = append(skipped, id)
	}
	return ls.query, skipped
}

func (ss *sessionState) snapshot() map[string]any {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	return map[string]any{
		"viewed_symbols":  ss.viewedSymbols,
		"viewed_files":    ss.viewedFiles,
		"modified_files":  ss.modifiedFiles,
		"recent_searches": ss.recentSearches,
	}
}

func prependUnique(slice []string, item string, maxLen int) []string {
	// Remove existing occurrence.
	for i, s := range slice {
		if s == item {
			slice = append(slice[:i], slice[i+1:]...)
			break
		}
	}
	// Prepend.
	slice = append([]string{item}, slice...)
	if len(slice) > maxLen {
		slice = slice[:maxLen]
	}
	return slice
}

// MultiRepoOptions holds optional multi-repo components for the Server.
// When nil or zero-valued, the server operates in single-repo mode.
type MultiRepoOptions struct {
	MultiIndexer  *indexer.MultiIndexer
	ConfigManager *config.ConfigManager
	// CheckoutLifecycle owns every side effect of tracking and forgetting a
	// checkout. Nil leaves the track / untrack tools unavailable rather than
	// letting them run a second, divergent copy of that sequence.
	CheckoutLifecycle *indexer.CheckoutLifecycle
	ActiveProject     string
	// ScopeWorkspace is the workspace slug filter applied as the
	// default scope on every query. Set by `gortex server --workspace
	// <slug>`. Empty disables the filter.
	ScopeWorkspace string
	// ScopeProject narrows further inside ScopeWorkspace (no effect
	// without it).
	ScopeProject string
	// ToolPolicy restricts the published MCP tool surface to a named
	// preset / allow-deny set (the `mcp.tools` config block). Nil leaves
	// the full surface; GORTEX_TOOLS / GORTEX_TOOLS_MODE still override.
	ToolPolicy *ToolPolicyConfig
	// ScopeIntentDefaults overrides the default-on intent scoping flag
	// from `.gortex.yaml::scope.intent_defaults`.
	ScopeIntentDefaults *bool
}

// serverInstructions is the server-level `instructions` field returned
// in the MCP initialize response. MCP clients surface it to the agent
// as guidance on how to drive this server — the place to say "prefer
// the graph tools over raw file reads, and where to start."
const serverInstructions = `Gortex is a code-intelligence graph server — it indexes repositories into a queryable knowledge graph. Prefer its graph tools over raw file reads and text search:

- For a request whose deliverable is only files, symbols, evidence, or a location, call explore(operation:"localize"). Follow completion.required_action: answer immediately when state is answer_ready; when state is needs_exact_read, make only the named exact read. For diagnosis or modification, call explore(operation:"task") and continue from its evidence.
- For one known symbol: search_symbols (BM25, camelCase-aware) to find it, get_symbol_source to read it, batch_symbols for several bodies in one call; find_usages / get_callers for references and callers.
- Before editing, call get_editing_context on the file; for refactors use edit_symbol / rename_symbol / batch_edit.
- The cold tools/list shows a core set — call tools_search to discover the rest of the catalogue on demand.
- Pass format:"gcx" to list-shaped tools for a compact, round-trippable wire format (~27% fewer tokens).

Worktree and branch routing:

` + profiles.WorktreeBranchRoutingPolicy + `

` + sharedParamLegend

// codingAgentInstructions is intentionally terse because some MCP hosts repeat
// initialize instructions beside every rendered tool. It describes what to do,
// not how the server versions or implements its tool surface.
const codingAgentInstructions = `MUST use Gortex MCP. First explicit-file read per new user request: read(operation:"file", target:{file:"<path>"}, options:{new_user_task:true}); do not localize. Unknown file/symbol/evidence: explore(operation:"localize"); obey completion.required_action; stop at answer_ready. Diagnosis/change: explore(operation:"task"); 1 follow-up. Pre-edit: change(operation:"impact"); signature: change(operation:"verify"). Mutate only via edit/refactor. Post-edit: change(operation:"detect"), tests/guards/contract. capabilities only if fields unknown.

Worktree and branch routing:

` + profiles.WorktreeBranchRoutingPolicy

// ServerInstructionsUntracked is the inactive-state `instructions` variant
// returned when a session's cwd is not covered by any tracked repo. Rather than
// poisoning the connection with an errored initialize, the handshake succeeds
// and the response tells the agent exactly how to activate the server — the
// actionable affordance codegraph's silent empty-list lacks.
func ServerInstructionsUntracked(cwd string, roots ...string) string {
	target := cwd
	if strings.TrimSpace(target) == "" {
		target = "."
	}
	msg := fmt.Sprintf("Gortex is connected but INACTIVE for this directory: %q is not covered by any tracked repository, "+
		"so the graph tools have nothing to answer with yet.\n\n"+
		"To activate: run `gortex track %s`, then reconnect — the full tool catalogue and the "+
		"graph become available.\n\n"+
		"Until then only capabilities and workspace_admin(operation:\"track\") will answer; every other call "+
		"returns repo_not_tracked, so fall back to your own file reads and text search.", target, target)
	// Append the tracked roots so the mismatch is self-diagnosing: a
	// case-only or drive-letter difference between the cwd above and one
	// of these roots is the usual cause of an INACTIVE session (#277).
	if len(roots) > 0 {
		quoted := make([]string, len(roots))
		for i, r := range roots {
			quoted[i] = fmt.Sprintf("%q", r)
		}
		msg += fmt.Sprintf("\n\nTracked repository roots: [%s]. If one of these is the same directory as %q "+
			"under a different letter case, that is the mismatch.", strings.Join(quoted, ", "), target)
	}
	return msg
}

// ServerInstructionsPendingAutomaticCheckout is the inactive initialize
// variant for a checkout whose Git family is already tracked. The
// checkout reconciler owns this transition; recommending `track` here would
// turn a transient discovery lag into an unintended dedicated graph.
func ServerInstructionsPendingAutomaticCheckout(cwd string, roots ...string) string {
	target := strings.TrimSpace(cwd)
	if target == "" {
		target = "."
	}
	msg := fmt.Sprintf("Gortex is connected but this Git checkout is still awaiting automatic discovery: %q belongs to an already tracked Git family, but its overlay route is not published yet.\n\n"+
		"Do not run `gortex track` for this state: implicit/session checkouts are automatic overlays. Wait for reconciliation and reconnect; if it remains unavailable, report the discovery lag. Explicit tracking is only for a user-requested dedicated graph.\n\n"+
		"Until its route is published, graph calls may return repo_not_tracked rather than silently reading another checkout.\n\n"+
		"Worktree and branch routing:\n\n%s", target, profiles.WorktreeBranchRoutingPolicy)
	return appendTrackedRootsDiagnostic(msg, roots)
}

func appendTrackedRootsDiagnostic(msg string, roots []string) string {
	if len(roots) == 0 {
		return msg
	}
	quoted := make([]string, len(roots))
	for i, root := range roots {
		quoted[i] = fmt.Sprintf("%q", root)
	}
	return msg + fmt.Sprintf("\n\nTracked repository roots: [%s]. The worktree shares a Git common directory with one of these roots.",
		strings.Join(quoted, ", "))
}

// afterInitializeInstructions is the server's OnAfterInitialize hook. It
// records clientInfo on THIS session before tools/list is served, then rewrites
// the initialize result's Instructions to the cwd-aware variant. Recording the
// client here keeps embedded/raw HandleMessage users on the same client-aware
// tool policy as the daemon path, whose dispatcher also snoops initialize.
func (s *Server) afterInitializeInstructions(ctx context.Context, _ any, req *mcp.InitializeRequest, result *mcp.InitializeResult) {
	if s == nil {
		return
	}
	if req != nil {
		s.sessionFor(ctx).recordClientName(req.Params.ClientInfo.Name)
	}
	if result == nil {
		return
	}
	result.Instructions = s.stateAwareInstructionsForPolicy(
		SessionCWDFromContext(ctx), s.effectiveSessionPolicy(ctx))
}

// stateAwareInstructions chooses the initialize `instructions` text for a
// connection rooted at cwd. An uncovered multi-repo cwd gets the terse
// activation affordance (shared with the F4 handshake path); a covered cwd —
// or a single-repo / cwd-less control client — gets the base guidance plus a
// live-facts block describing the workspace and warmup state.
// trackedRepoRoots returns the sorted absolute root of every tracked repo,
// for the self-diagnosing INACTIVE instructions.
func (s *Server) trackedRepoRoots() []string {
	if s.multiIndexer == nil {
		return nil
	}
	var roots []string
	for _, meta := range s.multiIndexer.AllMetadata() {
		if meta != nil && meta.RootPath != "" {
			roots = append(roots, meta.RootPath)
		}
	}
	sort.Strings(roots)
	return roots
}

func (s *Server) stateAwareInstructions(cwd string) string {
	return s.stateAwareInstructionsWithBase(cwd, serverInstructions)
}

func (s *Server) stateAwareInstructionsForClient(cwd, client string) string {
	return s.stateAwareInstructionsForPolicy(cwd, s.clientDefaultPolicy(client))
}

func (s *Server) stateAwareInstructionsForPolicy(cwd string, policy *toolPolicy) string {
	base := serverInstructions
	if policy != nil && policy.preset == FacadeSurfaceVersion {
		base = codingAgentInstructions
	}
	return s.stateAwareInstructionsWithBase(cwd, base)
}

func (s *Server) stateAwareInstructionsWithBase(cwd, base string) string {
	// The reachability test must match the dispatcher's gate exactly.
	// Telling a session it is INACTIVE while every call succeeds is the
	// same divergence as the reverse, just quieter.
	if s.multiIndexer != nil && strings.TrimSpace(cwd) != "" {
		_, _, _, inside := s.multiIndexer.ScopeForCWD(cwd)
		_, _, contains := s.multiIndexer.ContainedReposScope(cwd)
		if !inside && !contains {
			roots := s.trackedRepoRoots()
			if agents.PendingAutomaticCheckout(cwd, roots) {
				return ServerInstructionsPendingAutomaticCheckout(cwd, roots...)
			}
			return ServerInstructionsUntracked(cwd, roots...)
		}
	}
	if facts := s.liveInstructionFacts(cwd); facts != "" {
		base = base + "\n\n" + facts
	}
	if note := s.DegradedNote(); note != "" {
		base = note + "\n\n" + base
	}
	return base
}

// DegradedNote returns the operating-mode warning prepended to this
// server's initialize instructions, or "" when it is operating normally.
func (s *Server) DegradedNote() string {
	if s == nil {
		return ""
	}
	s.degradedMu.RLock()
	defer s.degradedMu.RUnlock()
	return s.degradedNote
}

// SetDegradedNote records a one-line explanation of a degraded operating
// mode, delivered to every client in the initialize instructions.
//
// The embedded fallback is the case this exists for: when no daemon is
// reachable, `gortex mcp` serves one locally-indexed tree with no
// workspace scoping, and announced it only on stderr — which MCP clients
// routinely discard. An agent then attributed the difference to the graph
// rather than to the mode it was running in.
func (s *Server) SetDegradedNote(note string) {
	if s == nil {
		return
	}
	s.degradedMu.Lock()
	s.degradedNote = note
	s.degradedMu.Unlock()
}

// liveInstructionFacts renders the per-connection state block appended to the
// covered-cwd instructions: tracked-repo count + names, the active project
// (multi-repo only), and the daemon warmup phase / ready flag. Returns "" when
// there is nothing live to report so the base guidance stays clean.
func (s *Server) liveInstructionFacts(cwd string) string {
	var lines []string

	if repos := s.trackedRepoNames(cwd); len(repos) > 0 {
		const maxNames = 8
		shown, suffix := repos, ""
		if len(repos) > maxNames {
			shown = repos[:maxNames]
			suffix = fmt.Sprintf(", +%d more", len(repos)-maxNames)
		}
		lines = append(lines, fmt.Sprintf("Tracked repositories (%d): %s%s.",
			len(repos), strings.Join(shown, ", "), suffix))
	}

	if proj := s.activeProjectName(cwd); proj != "" {
		lines = append(lines, fmt.Sprintf("Active project: %s.", proj))
	}

	if s.readinessBroadcaster != nil {
		if snap := s.readinessBroadcaster.snapshot(); snap != nil {
			if phase, _ := snap["phase"].(string); phase != "" {
				ready, _ := snap["ready"].(bool)
				state := "warming up — results may be partial until ready"
				if ready {
					state = "ready"
				}
				lines = append(lines, fmt.Sprintf("Index status: %s (phase %q).", state, phase))
			}
		}
	}

	if len(lines) == 0 {
		return ""
	}
	return "Live state for this connection:\n- " + strings.Join(lines, "\n- ")
}

// trackedRepoNames returns the sorted repo prefixes visible to a connection
// rooted at cwd: the cwd's workspace siblings when it resolves to one, else the
// full tracked set. Empty in single-repo (no multi-indexer) mode.
func (s *Server) trackedRepoNames(cwd string) []string {
	if s.multiIndexer == nil {
		return nil
	}
	var set map[string]bool
	if strings.TrimSpace(cwd) != "" {
		if ws, _, _, ok := s.multiIndexer.ScopeForCWD(cwd); ok {
			set = s.multiIndexer.ReposInWorkspace(ws)
		}
	}
	var names []string
	if len(set) > 0 {
		for p := range set {
			names = append(names, p)
		}
	} else {
		names = append(names, s.multiIndexer.RepoPrefixes()...)
	}
	sort.Strings(names)
	return names
}

// activeProjectName resolves the project slug for a cwd in multi-repo mode: the
// cwd's own project when it resolves, else the server's configured active
// project. Empty in single-repo mode.
func (s *Server) activeProjectName(cwd string) string {
	if s.multiIndexer == nil {
		return ""
	}
	if strings.TrimSpace(cwd) != "" {
		if _, proj, _, ok := s.multiIndexer.ScopeForCWD(cwd); ok {
			// A resolved cwd owns the answer either way: a multi-repo
			// workspace-root binding resolves with an empty project, and
			// falling through to the server-wide default would advertise
			// a project the session's bound scope may not even see.
			return proj
		}
	}
	return s.activeProject
}

// NewServer creates an MCP server with all Gortex tools registered.
func NewServer(engine *query.Engine, g graph.Store, idx *indexer.Indexer, watcher *indexer.Watcher, logger *zap.Logger, guardRules []config.GuardRule, opts ...MultiRepoOptions) *Server {
	s := &Server{
		engine:              engine,
		graph:               g,
		indexer:             idx,
		logger:              logger,
		session:             newSessionState(),
		localization:        newLocalizationTerminalState(),
		scopeIntentDefaults: true,
		tokenStats:          &tokenStats{},
		symHistory: &symbolHistory{
			entries: make(map[string][]SymbolModification),
		},
		sessions:   newSessionMap(),
		guardRules: guardRules,
		toolScopes: newScopeRegistry(),
		agentReg:   newAgentRegistry(),
		queryLog:   newQueryLogger(),
		pprCache:   newPPRWalkCache(),
		packCache:  newPackDeltaCache(),
		prCache:    newPRCache(prCacheTTL),
		facades:    newFacadeRegistry(),
	}
	// Wire the process-wide tokenStats as the parent of every
	// per-session counter so record() fanout aggregates daemon-wide.
	s.sessions.setParentTokenStats(s.tokenStats)
	// Per-connection state-aware initialize instructions: the after-init
	// hook rewrites result.Instructions for THIS session's cwd — the terse
	// activation affordance for an uncovered cwd, or the base guidance plus
	// live workspace + warmup facts for a covered one. It fires for every
	// MCP client straight from the handshake, including non-Claude-Code
	// clients that never run a SessionStart hook (where the daemon-side
	// rewrite for the proxy path would not reach the embedded stdio server).
	initHooks := &server.Hooks{}
	initHooks.AddAfterInitialize(s.afterInitializeInstructions)

	// mcpServer is constructed after s exists so the per-session tool
	// filter can close over s — toolSurfaceFilter varies the tools/list
	// surface by the session's planning mode (see tools_mode.go).
	s.mcpServer = server.NewMCPServer("gortex", Version,
		// Surface "how to drive this server" to MCP clients in the
		// initialize response — see serverInstructions.
		server.WithInstructions(serverInstructions),
		// Rewrite that instructions field per connection (see above).
		server.WithHooks(initHooks),
		// listChanged=true: tools_search promotes deferred tools into
		// the live MCP server on demand, and a planning-mode flip
		// re-filters the surface — both rely on tools/list_changed.
		server.WithToolCapabilities(true),
		// subscribe=true lets clients call resources/subscribe for
		// bootstrap URIs and receive notifications/resources/updated
		// after each graph re-warm. listChanged=false — the resource
		// set is static for the server's lifetime.
		server.WithResourceCapabilities(true, false),
		server.WithRecovery(),
		// Per-session tools/list filter — hides editing tools while a
		// session is in planning mode (see tools_mode.go).
		server.WithToolFilter(s.toolSurfaceFilter),
	)

	// Assign the watcher only when the caller actually supplied one.
	// Storing a typed-nil *indexer.Watcher in the watcherHistory
	// interface field would produce a non-nil interface wrapping a
	// nil pointer — `s.watcher == nil` checks in handlers would then
	// pass through to method calls and panic.
	if watcher != nil {
		s.watcher = watcher
	}

	// Apply multi-repo options if provided.
	if len(opts) > 0 {
		o := opts[0]
		s.multiIndexer = o.MultiIndexer
		s.configManager = o.ConfigManager
		s.lifecycle = o.CheckoutLifecycle
		s.activeProject = o.ActiveProject
		s.scopeWorkspace = o.ScopeWorkspace
		s.scopeProject = o.ScopeProject
		if o.ScopeIntentDefaults != nil {
			s.scopeIntentDefaults = *o.ScopeIntentDefaults
		}
		// A stack-built server is handed the lifecycle every other entry
		// point shares. A server constructed on its own — the embedded and
		// test paths — owns the only tracked set there is, so it builds the
		// same coordinator over the same inputs rather than growing a second
		// copy of the track / forget sequence.
		if s.lifecycle == nil && s.multiIndexer != nil {
			lifecycle, err := indexer.NewCheckoutLifecycle(indexer.CheckoutLifecycleConfig{
				MultiIndexer:  s.multiIndexer,
				ConfigManager: s.configManager,
				Graph:         g,
				Logger:        logger,
			})
			if err != nil {
				if logger != nil {
					logger.Warn("mcp: could not build the checkout lifecycle", zap.Error(err))
				}
			} else {
				s.lifecycle = lifecycle
			}
		}
		s.lifecycle.SetNotifier(s)
	}

	// Proactive-notification broadcasters. Constructed up-front so
	// subscribe handlers can register listeners as soon as the first
	// session connects; the *publishers* are wired by the daemon
	// entrypoint (PublishReadiness at warmup phases,
	// AttachHealthSnapshot for the periodic ticker) and by
	// SetWatcher (stale_refs hooks into the symbol-change callback).
	s.readinessBroadcaster = newReadinessBroadcaster(s.mcpServer, logger)
	s.healthBroadcaster = newHealthBroadcaster(s.mcpServer, nil, logger)
	s.staleRefsBroadcaster = newStaleRefsBroadcaster(s.mcpServer, s.sessions, s.session, logger)
	s.graphInvalidatedBroadcaster = newGraphInvalidatedBroadcaster(s.mcpServer, logger)

	// Lazy-tool registry MUST be installed before any addTool calls so
	// non-hot tools land in the deferred catalog instead of the live
	// MCP server. attachLazyRegistry wires the promotion closure that
	// tools_search uses to migrate a tool from deferred → live on
	// first use. See lazy_tools.go for the hot-set selection rules.
	s.sanitizeInjection = sanitizeEnabledFromEnv()
	// Tool-surface preset (mcp.tools config + GORTEX_TOOLS env). In
	// defer mode the preset's allow-set becomes the lazy registry's
	// eager surface; hide mode is enforced later by toolSurfaceFilter /
	// checkToolGate. Resolved before the register sweep so every
	// addTool sees the policy.
	toolPolicyBase := toolPolicyBaseFromOptions(opts)
	s.toolPolicyOperatorPinned = operatorPinnedToolPolicy(toolPolicyBase)
	s.toolPolicy = resolveToolPolicy(toolPolicyBase, logger)
	s.lazy = newLazyToolRegistry(lazyEnabledFromEnv() || s.toolPolicy.deferMode())
	if s.toolPolicy.deferMode() {
		s.lazy.SetEagerPredicate(s.toolPolicy.allows)
	}
	s.attachLazyRegistry()
	s.registerToolsSearch()

	s.registerCoreTools()
	s.registerExploreTool()
	s.registerFindFilesTool()
	s.registerCodingTools()
	s.registerMoveInlineTools()
	s.registerPostFilterTools()
	s.registerPlanningModeTool()
	s.registerWorkflowTool()
	s.registerScopeTools()
	s.registerAnalysisTools()
	s.registerEnhancementTools()
	s.registerLSPTools()
	s.registerLintTools()
	s.registerAgentRegistryTools()
	s.registerDiagnosticsTools()
	s.registerReadinessTools()
	s.registerHealthTools()
	s.registerStaleRefsTools()
	s.registerGraphInvalidatedTools()
	s.registerToolProfileTool()
	s.registerDataflowTools()
	s.registerCFGTools()
	s.registerASTTools()
	s.registerCloneTools()
	s.registerSimulationTools()
	s.registerChangeContractTools()
	s.registerNotesTools()
	s.registerMemoriesTools()
	s.registerWhyTool()
	s.registerNotebookTools()
	s.registerCitationTools()
	s.registerKnowledgeGapsTool()
	s.registerSurprisingConnectionsTool()
	s.registerReviewQuestionsTool()
	s.registerPRReviewContextTool()
	s.registerCritiqueReviewTool()
	s.registerArchitectureTool()
	s.registerReplayEpisodeTool()
	s.registerSafeDeleteSymbolTool()
	s.registerGenerateSkillTool()
	s.registerInspectionsTools()
	s.registerChurnRateTool()
	s.registerEnrichChurnTool()
	s.registerEnrichReleasesTool()
	s.registerCoChangeTool()
	s.registerArtifactTools()
	s.registerCouplingMetricsTool()
	s.registerExtractionCandidatesTool()
	s.registerCheckReferencesTool()
	s.registerWakeupTool()
	s.registerGraphCompletionTool()
	s.registerWikiTools()
	s.registerExportTools()
	s.registerAuditTool()
	s.registerWalkGraphTool()
	s.registerContextClosureTool()
	s.registerGraphQueryTool()
	s.registerNavTool()
	s.registerFindDeclarationTool()
	s.registerPRRiskTool()
	s.registerSuggestReviewersTool()
	s.registerReviewTools()
	s.registerPRTools()
	s.registerConflictsPRTool()
	s.registerResources()
	s.registerPrompts()

	// Register multi-repo tools when multi-repo components are available.
	if s.multiIndexer != nil || s.configManager != nil {
		s.registerMultiRepoTools()
		// The checkout administration surface reads and drives the same
		// lifecycle the track / untrack tools do, so it is registered on the
		// same condition.
		s.registerCheckoutAdminTools()
	}

	// Workspace-scope bootstrap tools (list_repos, workspace_info).
	// Always registered — they degrade cleanly in single-project mode
	// to a one-member view.
	s.registerWorkspaceTools()

	// Register the stable facade surface only after the legacy sweep, so every
	// operation has captured its existing implementation. Facade dispatchers
	// are installed live and session-filtered; they never rely on promotion.
	s.registerFacadeTools()

	// LLM-backed tools (`ask`) are NOT registered here — they're
	// gated on SetLLMService being called with an enabled service,
	// which happens post-construction from the daemon entrypoint.

	s.applyDefaultToolScopes()

	return s
}

// SetLLMService attaches the LLM service to the server and registers
// the `ask` MCP tool. Call after NewServer; without this, the `ask`
// MCP tool is not registered (clean degradation for deployments
// without an LLM).
//
// Safe to call with a disabled service (no provider configured, or
// provider construction failed) — registerLLMTools gates on
// Service.Enabled() and skips registration in that case.
//
// Lifecycle: the server does NOT take ownership of the service; the
// daemon entrypoint that constructed the service is responsible for
// calling svc.Close() on shutdown.
func (s *Server) SetLLMService(service *svc.Service) {
	s.llmService = service
	s.registerLLMTools()
}

// LLMService returns the attached LLM service, or nil when none was configured
// (no provider, or provider construction failed). The daemon's shared-server
// wiring uses it to register the service's Close in the process cleanup chain —
// honouring the "caller owns Close" lifecycle noted on SetLLMService — so a
// graceful shutdown unloads the loaded model instead of leaning on the idle
// reaper as the only unload path.
func (s *Server) LLMService() *svc.Service {
	return s.llmService
}

// SetupLLM is the convenience constructor used by daemon entrypoints.
// It builds an in-process backend wired to this server's engine +
// contract registry, constructs the service from cfg, and attaches
// it. A zero or disabled cfg is a no-op — safe to call
// unconditionally.
//
// The provider is chosen by cfg.Provider. Selecting "local" in a
// binary built without `-tags llama` — or any provider with a missing
// model / API key — leaves the service disabled; the construction
// error is logged as a warning rather than failing daemon startup, so
// a misconfigured `llm:` block degrades cleanly (the `ask` tool and
// `search_symbols` assist modes are simply absent).
func (s *Server) SetupLLM(cfg llm.Config) {
	cfg = cfg.MergeEnv()
	var customWarnings []string
	cfg, customWarnings = registry.Augment(cfg)
	for _, w := range customWarnings {
		s.logger.Warn("custom LLM provider", zap.String("warning", w))
	}
	if !cfg.IsEnabled() {
		return
	}
	backend := svc.NewInProcessBackend(s.engine, s.effectiveContractRegistry)
	// The busy predicate lets the search-assist gate defer an expensive
	// local-model cold load while a background enrichment pass is in
	// flight — the two must not contend for CPU/GPU/RAM. Reads the
	// semantic manager lazily so it sees a manager attached after
	// SetupLLM, and reports "not busy" until one is.
	busy := func() bool {
		return s.semanticMgr != nil && s.semanticMgr.EnrichmentActive()
	}
	service := svc.NewService(cfg, backend,
		svc.WithBusyPredicate(busy),
		svc.WithLogger(s.logger))
	s.SetLLMService(service)
	if err := service.ProviderErr(); err != nil {
		s.logger.Warn("LLM provider unavailable — `ask` tool and search assist disabled",
			zap.String("provider", cfg.ProviderName()),
			zap.Error(err))
	}
}

// InitFeedback initializes the feedback manager for cross-session feedback persistence.
// Call after NewServer with the cache directory and primary repo path.
func (s *Server) InitFeedback(cacheDir, repoPath string) {
	s.feedback = newFeedbackManager(cacheDir, repoPath)
}

// InitNotes initializes the session-memory manager used by the
// save_note / query_notes / distill_session tools. Call after
// NewServer with the cache directory and primary repo path.
// Empty arguments yield an in-memory-only manager (still wired
// to the tools, just doesn't flush to disk).
func (s *Server) InitNotes(cacheDir, repoPath string) {
	s.notes = newNotesManager(cacheDir, repoPath)
}

// InitLearnedTools wires the per-workspace learned tool surface and hydrates
// it: it advances the session epoch, demotes promotions unused past the
// hysteresis window, and re-promotes the survivors into the eager surface so
// a tool the team promoted last session is already live this session. Call
// after NewServer (once the register sweep has deferred the cold tools).
// Empty arguments yield an in-memory-only, non-persistent surface.
func (s *Server) InitLearnedTools(cacheDir, repoPath string) {
	s.promotedTools = newPromotedToolsManager(cacheDir, repoPath)
	for _, name := range s.promotedTools.Load() {
		// Re-promote persisted tools so they ship in the cold tools/list.
		// EnsureToolPromoted is a no-op for a tool that is not deferred
		// (already live, or absent), so this is safe under any preset.
		s.EnsureToolPromoted(name)
	}
}

// RecordLearnedPromotion persists that a deferred tool was promoted (or
// re-used) for the session's workspace, so the learned surface survives a
// daemon restart. cwd resolves the workspace for diagnostics. A no-op when
// the learned surface is not wired.
func (s *Server) RecordLearnedPromotion(name, cwd string) {
	if s == nil || s.promotedTools == nil || name == "" {
		return
	}
	// Floor / always-eager tools are never "learned" — only tools that were
	// genuinely deferred and promoted on demand belong in the learned set.
	if isAlwaysKeptTool(name) {
		return
	}
	s.promotedTools.Record(name, s.workspaceIDForCWD(cwd))
}

// isLearnedPromoted reports whether a tool is in the per-workspace learned
// surface — used to keep a learned tool visible on the lean agent surface
// (which would otherwise narrow it out).
func (s *Server) isLearnedPromoted(name string) bool {
	return s != nil && s.promotedTools.Has(name)
}

// ActivePreset returns the server's global tool-surface preset label + mode
// for status / introspection. The per-session default may differ (a known
// coding-agent client defaults to `agent`); this reports the server-level
// baseline.
func (s *Server) ActivePreset() (preset, mode string) {
	if s == nil || s.toolPolicy == nil {
		return "full", ""
	}
	return s.toolPolicy.preset, s.toolPolicy.mode
}

// LearnedToolCount returns the size of the per-workspace learned tool
// surface (deferred tools promoted through use, persisted across restarts).
func (s *Server) LearnedToolCount() int {
	if s == nil {
		return 0
	}
	return s.promotedTools.Count()
}

// LearnedToolNames returns the learned tool surface, sorted.
func (s *Server) LearnedToolNames() []string {
	if s == nil {
		return nil
	}
	return s.promotedTools.Names()
}

// NoteToolUse records a call to a tool for the learned-surface. It persists
// only tools that were genuinely deferred (newly promoted this call) or are
// already in the learned set — never the eager floor — so the learned
// surface tracks exactly the tools worth promoting for this workspace.
func (s *Server) NoteToolUse(name, cwd string, newlyPromoted bool) {
	if s == nil || s.promotedTools == nil || name == "" {
		return
	}
	if newlyPromoted || s.isLearnedPromoted(name) {
		s.RecordLearnedPromotion(name, cwd)
	}
}

// workspaceIDForCWD best-effort resolves a workspace slug from a session
// cwd for learned-promotion diagnostics; returns "" when it cannot.
func (s *Server) workspaceIDForCWD(cwd string) string {
	if s == nil || s.multiIndexer == nil || cwd == "" {
		return ""
	}
	if ws, _, _, ok := s.multiIndexer.ScopeForCWD(cwd); ok {
		return ws
	}
	return ""
}

// InitMemories initializes the cross-session development-memory
// manager used by the store_memory / query_memories /
// surface_memories tools. Call after NewServer with the cache
// directory and primary repo path. Empty arguments yield an
// in-memory-only manager (still wired to the tools, just doesn't
// flush to disk).
//
// Memories persist across daemon restarts and context compactions
// and are workspace-wide — every agent in the same workspace
// shares the store.
// InitNotebook mounts the repository-local persistent notebook
// store at <repoPath>/.gortex/notebook/. Empty repoPath leaves
// s.notebook nil; tools surface that as "notebook not initialised".
// Distinct from notes (per-session) and memories (cache-dir cross-
// session) — notebook entries are committed to git so they travel
// with the repo and surface in PR reviews.
func (s *Server) InitNotebook(repoPath string) {
	s.notebook = newNotebookManager(repoPath)
}

// InitSuppressions initializes the durable per-repo review-finding
// false-positive suppression store used by the review gate and the
// suppress_finding tool. Call after NewServer with the cache directory and
// primary repo path. Empty arguments yield an in-memory-only store (still wired
// to the tools, just doesn't flush to disk). Suppressions persist across daemon
// restarts and are per-repo, scoped by the same cache key as notes / memories.
func (s *Server) InitSuppressions(cacheDir, repoPath string) {
	s.suppressions = newSuppressionManager(cacheDir, repoPath)
}

func (s *Server) InitMemories(cacheDir, repoPath string) {
	s.memories = newMemoryManager(cacheDir, repoPath)
	// Mount the user-level global store. Defaults to ~/.gortex/memories;
	// an absolute $XDG_DATA_HOME relocates it to
	// <XDG_DATA_HOME>/gortex/memories. Failures (no $HOME, unreadable
	// home) leave globalMemories nil; tools detect that and surface a
	// clear error rather than silently dropping global writes.
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		s.globalMemories = newMemoryManager(platform.MemoriesDir(), "global")
	}
}

// resolveMemoryStore picks the right memoryManager for the requested
// scope. Defaults to the workspace store; "global" returns the
// user-level store mounted at ~/.gortex/memories-cache/. Unknown
// scope values fall through to workspace so callers don't have to
// guard against typos.
func (s *Server) resolveMemoryStore(scope string) *memoryManager {
	switch strings.ToLower(strings.TrimSpace(scope)) {
	case "global":
		return s.globalMemories
	default:
		return s.memories
	}
}

// resolveMemoryStores returns every memoryManager that matches the
// scope argument. `both` returns workspace + global; `workspace`
// (default) returns just workspace; `global` returns just global.
// Nil managers are excluded from the result so callers can rely on
// the slice being non-empty before iterating.
func (s *Server) resolveMemoryStores(scope string) []*memoryManager {
	switch strings.ToLower(strings.TrimSpace(scope)) {
	case "both":
		stores := []*memoryManager{}
		if s.memories != nil {
			stores = append(stores, s.memories)
		}
		if s.globalMemories != nil {
			stores = append(stores, s.globalMemories)
		}
		return stores
	case "global":
		if s.globalMemories != nil {
			return []*memoryManager{s.globalMemories}
		}
		return nil
	default:
		if s.memories != nil {
			return []*memoryManager{s.memories}
		}
		return nil
	}
}

// WorkspaceScope returns the default workspace slug filter applied
// to every query (set by `gortex server --workspace`). Empty
// means no scope; tools that consult it should fall back to the
// global multi-workspace view.
func (s *Server) WorkspaceScope() string { return s.scopeWorkspace }

// ProjectScope returns the project slug filter; meaningful only
// when WorkspaceScope() is non-empty.
func (s *Server) ProjectScope() string { return s.scopeProject }

// unresolvedWorkspacePrefix marks a session whose cwd is non-empty but
// resolves to no tracked repo. Used as a QueryOptions.WorkspaceID
// sentinel: it can never equal a real node's WorkspaceID/RepoPrefix,
// so every node is rejected and the session fails closed instead of
// widening to the global graph.
const unresolvedWorkspacePrefix = "\x00gortex-unresolved-workspace:"

// sessionScope resolves the workspace/project boundary for the current
// request. When bound is true the session is confined to workspaceID
// and query handlers MUST NOT return data outside it; an empty (or
// sentinel) workspaceID with bound==true means the cwd resolved to no
// tracked repo and handlers fail closed. When bound is false the
// session is unbound (embedded stdio / control client / `gortex
// server --workspace`) and callers fall back to the server-default
// scope.
//
// Resolution is cached on sessionState and recomputed only when the
// session's cwd changes — the stdio proxy fixes it for the connection's
// life, but the Streamable HTTP transport carries it per request, and a
// binding resolved from one cwd must never be served to another.
// repoPrefix is the session's home repo, used only for relevance ranking.
func (s *Server) sessionScope(ctx context.Context) (workspaceID, projectID string, bound bool) {
	ss := s.sessionFor(ctx)
	if ss == nil {
		return "", "", false
	}
	cwd := SessionCWDFromContext(ctx)
	ss.mu.Lock()
	defer ss.mu.Unlock()
	if ss.scopeResolved && ss.scopeCWD == cwd {
		return ss.scopeWorkspaceID, ss.scopeProjectID, ss.scopeBound
	}
	ss.resetScopeLocked()
	ss.scopeResolved = true
	ss.scopeCWD = cwd

	if cwd == "" || s.multiIndexer == nil {
		// No cwd (embedded stdio, control clients) or no multi-repo
		// indexer: unbound — the server-default scope applies.
		return "", "", false
	}

	ss.scopeBound = true
	ws, proj, repoPrefix, ok := s.multiIndexer.ScopeForCWD(cwd)
	if ok {
		ss.scopeWorkspaceID = ws
		ss.scopeProjectID = proj
		ss.scopeRepoPrefix = repoPrefix
		return ss.scopeWorkspaceID, ss.scopeProjectID, true
	}

	// cwd lies inside no tracked repo. It may still CONTAIN them — an
	// agent opened at the root above its repos. Bind to that repo set
	// directly: it is a filesystem fact, and a strictly narrower
	// boundary than any workspace slug (a slug also names repos rooted
	// elsewhere). scopeWorkspaceID stays empty here, so scopeRepoAllow
	// is the ONLY thing standing between this session and the global
	// graph — it is never empty on this path.
	if repos, workspaces, contained := s.multiIndexer.ContainedReposScope(cwd); contained {
		ss.scopeRepoAllow = setOfStrings(repos)
		ss.scopeWorkspaces = setOfStrings(workspaces)
		if len(repos) == 1 {
			// A single contained repo has a real home: keep its full
			// scope so locality ranking still works.
			ss.scopeWorkspaceID = workspaces[0]
			ss.scopeRepoPrefix = repos[0]
		}
		return ss.scopeWorkspaceID, ss.scopeProjectID, true
	}

	// cwd lies inside no tracked repo and contains none — but the view
	// catalog may still know it as an automatic checkout of a tracked
	// family. That is a directory the repository registry never covers, so
	// without this arm a session sitting in a worktree binds the unresolved
	// form below and every scope-narrowed read comes back empty, including
	// the reads its own routed view was built to serve.
	if ws, proj, prefix, resolved := s.scopeForAutomaticCheckout(ctx, cwd); resolved {
		ss.scopeWorkspaceID = ws
		ss.scopeProjectID = proj
		ss.scopeRepoPrefix = prefix
		return ss.scopeWorkspaceID, ss.scopeProjectID, true
	}

	// cwd neither lives inside nor contains a tracked repo. The daemon
	// dispatcher rejects unreachable cwds before dispatch, so this is
	// defensive: the sentinel matches no node, so the session sees
	// nothing rather than the whole global graph.
	ss.scopeWorkspaceID = unresolvedWorkspacePrefix + cwd
	return ss.scopeWorkspaceID, ss.scopeProjectID, true
}

// resetScopeLocked clears every cached scope field so a re-resolution
// cannot inherit a stale boundary from the previous cwd. Caller holds
// ss.mu.
func (ss *sessionState) resetScopeLocked() {
	ss.scopeResolved = false
	ss.scopeBound = false
	ss.scopeWorkspaceID = ""
	ss.scopeProjectID = ""
	ss.scopeRepoPrefix = ""
	ss.scopeRepoAllow = nil
	ss.scopeWorkspaces = nil
	ss.scopeCWD = ""
}

// setOfStrings turns a slice into a lookup set. Returns nil for an empty
// slice so "no ceiling" and "an empty ceiling" stay distinguishable — the
// difference between an unbounded session and a blind one.
func setOfStrings(vals []string) map[string]bool {
	if len(vals) == 0 {
		return nil
	}
	out := make(map[string]bool, len(vals))
	for _, v := range vals {
		out[v] = true
	}
	return out
}

// sessionRepoCeiling returns the session's hard repo allow-set, or nil
// when the session's workspace slug is the whole boundary. Non-nil only
// for a workspace-root session bound to the repos it contains.
//
// This is the single accessor every scope consumer folds in; a bound
// session always satisfies `workspaceID != "" || len(ceiling) > 0`.
func (s *Server) sessionRepoCeiling(ctx context.Context) map[string]bool {
	ss := s.sessionFor(ctx)
	if ss == nil {
		return nil
	}
	// Ensure the binding is resolved (populates scopeRepoAllow).
	s.sessionScope(ctx)
	ss.mu.Lock()
	defer ss.mu.Unlock()
	return ss.scopeRepoAllow
}

// sessionScopeWorkspaces returns the workspace slugs a contained-repo
// session spans, for the `workspace:` selector. Nil for every other shape.
func (s *Server) sessionScopeWorkspaces(ctx context.Context) map[string]bool {
	ss := s.sessionFor(ctx)
	if ss == nil {
		return nil
	}
	s.sessionScope(ctx)
	ss.mu.Lock()
	defer ss.mu.Unlock()
	return ss.scopeWorkspaces
}

// sessionScopeOptions builds the QueryOptions that express the current
// session's boundary — workspace slug plus repo ceiling. bound is false
// for an unbound session, whose callers must not clamp.
//
// Every per-node session gate routes through this so the two axes can
// never drift apart: a shape that has no workspace slug carries a repo
// ceiling instead, and both are applied together.
func (s *Server) sessionScopeOptions(ctx context.Context) (query.QueryOptions, bool) {
	sessWS, _, bound := s.sessionScope(ctx)
	if !bound {
		return query.QueryOptions{}, false
	}
	return query.QueryOptions{WorkspaceID: sessWS, RepoAllow: s.sessionRepoCeiling(ctx)}, true
}

// InvalidateSessionScopes drops every session's cached workspace binding
// so the next call re-resolves it against the current tracked-repo set.
// Called after a repo is tracked or untracked: without it a session that
// bound before the change keeps serving the boundary it latched — which
// is what made the untracked-cwd bootstrap exemption decorative, since a
// session that tracked its own repo stayed blind to it.
func (s *Server) InvalidateSessionScopes() {
	if s == nil {
		return
	}
	states := []*sessionState{s.session}
	if s.sessions != nil {
		states = append(states, s.sessions.snapshotSessions()...)
	}
	for _, ss := range states {
		if ss == nil {
			continue
		}
		ss.mu.Lock()
		ss.resetScopeLocked()
		ss.mu.Unlock()
	}
}

// sessionLocality returns the session's home repo prefix and project
// slug for relevance ranking — same-repo hits rank above same-project
// above same-workspace. Both empty for unbound sessions.
func (s *Server) sessionLocality(ctx context.Context) (repoPrefix, projectID string) {
	ss := s.sessionFor(ctx)
	if ss == nil {
		return "", ""
	}
	// Ensure scope is resolved (populates scopeRepoPrefix).
	s.sessionScope(ctx)
	ss.mu.Lock()
	defer ss.mu.Unlock()
	return ss.scopeRepoPrefix, ss.scopeProjectID
}

// sessionWorkspaceRepos returns {name, path} for every repo in the
// current session's workspace, sorted by name. Empty for an unbound
// session or when the multi-repo indexer is unavailable. Used by the
// introspection tools so they report the session's real boundary.
func (s *Server) sessionWorkspaceRepos(ctx context.Context) []map[string]string {
	prefixes, bound := s.sessionWorkspaceRepoSet(ctx)
	if !bound {
		return nil
	}
	meta := s.multiIndexer.AllMetadata()
	out := make([]map[string]string, 0, len(prefixes))
	for p := range prefixes {
		entry := map[string]string{"name": p}
		if m := meta[p]; m != nil {
			entry["path"] = m.RootPath
		}
		out = append(out, entry)
	}
	sort.Slice(out, func(i, j int) bool { return out[i]["name"] < out[j]["name"] })
	return out
}

// nodeInSessionScope reports whether a node may be surfaced to the
// current session. For a workspace-bound session only nodes inside
// the session's workspace pass; for an unbound session every node
// passes (the server-default scope applies). This is the universal
// per-node enforcement of the workspace boundary — used by by-id and
// whole-graph handlers that don't route through the engine's scoped
// traversal.
func (s *Server) nodeInSessionScope(ctx context.Context, n *graph.Node) bool {
	opts, bound := s.sessionScopeOptions(ctx)
	if !bound {
		return true
	}
	if n == nil {
		return false
	}
	return opts.ScopeAllows(n)
}

// scopedNodes returns the graph nodes visible to the current session:
// every node for an unbound session, only the session workspace's
// nodes for a bound one. Whole-graph handlers (analyze, outline,
// untested, resource rollups, …) iterate this instead of
// graph.AllNodes() so a workspace-bound session can never observe
// another workspace's nodes — not even in aggregate counts.
//
// The scan runs on the request's reader, so an overlay-active call sees
// the pushed buffers instead of what is on disk.
func (s *Server) scopedNodes(ctx context.Context) []*graph.Node {
	return s.scopeNodes(ctx, s.readerFor(ctx).AllNodes())
}

// scopedNodesLight is scopedNodes over the projected node scan: the same
// scoping, but the store stops before the opaque meta blob and the
// promoted columns, so no per-node map[string]any is decoded. On a
// 600k-node store that is the difference between a few hundred MiB of
// transient allocation per whole-graph handler call and almost none.
//
// The returned nodes carry ID, Kind, Name, QualName, FilePath, the line
// and column span, Language, RepoPrefix, WorkspaceID and ProjectID.
// Node.Meta is NIL — and every promoted key that normally rides in it
// (signature, doc, visibility, complexity, churn, framework, …) is
// therefore absent, not empty.
//
// Only call this from a handler proven to read none of them. Note that
// the in-memory graph has no projection and hands back canonical nodes
// with full Meta, so a wrong call site passes every in-memory test and
// fails only against SQLite.
//
// The projection is asked of the request's reader. An overlay view is not
// a light scanner, so an overlay-active call falls back to AllNodes and
// gets fully materialized nodes — the conservative direction.
func (s *Server) scopedNodesLight(ctx context.Context) []*graph.Node {
	return s.scopeNodes(ctx, graph.AllNodesLight(s.readerFor(ctx)))
}

// scopeNodes applies the session's workspace / repo scope to an
// already-materialized node set. ScopeAllows reads only WorkspaceID,
// RepoPrefix and ProjectID, all of which the light projection carries, so
// this is shared by both entry points.
func (s *Server) scopeNodes(ctx context.Context, all []*graph.Node) []*graph.Node {
	opts, active := s.scopeOptionsWithRepoNarrow(ctx, repoAllowFromContext(ctx))
	if !active {
		return all
	}
	out := make([]*graph.Node, 0, len(all))
	for _, n := range all {
		if opts.ScopeAllows(n) {
			out = append(out, n)
		}
	}
	return out
}

// scopeOptionsWithRepoNarrow folds a handler-supplied repo narrow into the
// session boundary. active is false only when there is nothing to apply —
// an unbound session with no narrow.
//
// The narrow can only narrow: against a session repo ceiling the two sets
// are intersected, so a handler narrow can never name a repo the session
// may not see. A narrow disjoint from the ceiling collapses to the
// unresolved-workspace sentinel rather than to an empty allow-set — an
// empty RepoAllow with an empty workspace slug reads as "no filter" to
// ScopeAllows, which would turn a fully-excluded narrow into a global scan.
func (s *Server) scopeOptionsWithRepoNarrow(ctx context.Context, narrow map[string]bool) (query.QueryOptions, bool) {
	opts, bound := s.sessionScopeOptions(ctx)
	if !bound {
		return query.QueryOptions{RepoAllow: narrow}, len(narrow) > 0
	}
	switch {
	case len(narrow) == 0:
		// Nothing to fold in — the session boundary stands alone.
	case len(opts.RepoAllow) == 0:
		opts.RepoAllow = narrow
	default:
		if merged := intersectRepoSets(narrow, opts.RepoAllow); len(merged) > 0 {
			opts.RepoAllow = merged
		} else {
			opts.WorkspaceID = unresolvedWorkspacePrefix + "disjoint-repo-narrow"
			opts.RepoAllow = nil
		}
	}
	return opts, true
}

// scopedNodesByKinds is the kind-pushdown sibling of scopedNodes for
// handlers that only need a specific kind set. When the backend
// implements graph.NodesByKindsScanner the kind predicate runs server-
// side (one kind-filtered scan over the node table) instead of
// the legacy AllNodes()-then-Go-side filter. The metadata analyzers
// (todos, stale_code, stale_flags, ownership, coverage_gaps,
// coverage_summary, cgo_users, wasm_users, orphan_tables,
// unreferenced_tables) each keep one or two kinds out of the whole
// node table; pushing that filter is the entire win.
//
// Workspace-bound sessions still narrow Go-side: the capability does
// not know about ScopeAllows, and adding workspace_id to every analyze
// query would tie the capability to the session-scope concept. The
// secondary filter is cheap because the kind pushdown already shrank
// the row count by 1-2 orders of magnitude.
//
// Empty kinds returns nil — defensive against caller bugs that would
// otherwise drop into the full-AllNodes fallback path.
//
// The pushdown is probed on the request's reader, never on the base
// store: an overlay view does not implement the scanner, so an
// overlay-active call takes the AllNodes fallback and reads the pushed
// buffers rather than the backend's kind index.
func (s *Server) scopedNodesByKinds(ctx context.Context, kinds []graph.NodeKind) []*graph.Node {
	if len(kinds) == 0 {
		return nil
	}
	reader := s.readerFor(ctx)
	var nodes []*graph.Node
	if scan, ok := reader.(graph.NodesByKindsScanner); ok {
		nodes = scan.NodesByKinds(kinds)
	} else {
		// Fallback: same behaviour as scopedNodes, kind-filtered Go-side.
		all := reader.AllNodes()
		allowed := make(map[graph.NodeKind]struct{}, len(kinds))
		for _, k := range kinds {
			allowed[k] = struct{}{}
		}
		nodes = make([]*graph.Node, 0, len(all))
		for _, n := range all {
			if _, ok := allowed[n.Kind]; ok {
				nodes = append(nodes, n)
			}
		}
	}
	opts, active := s.scopeOptionsWithRepoNarrow(ctx, repoAllowFromContext(ctx))
	if !active {
		return nodes
	}
	out := make([]*graph.Node, 0, len(nodes))
	for _, n := range nodes {
		if opts.ScopeAllows(n) {
			out = append(out, n)
		}
	}
	return out
}

// scopedNodeSlice filters an existing node slice to the session's
// workspace. Convenience for handlers that already hold a node list
// (engine list methods that don't take QueryOptions).
func (s *Server) scopedNodeSlice(ctx context.Context, nodes []*graph.Node) []*graph.Node {
	opts, active := s.scopeOptionsWithRepoNarrow(ctx, repoAllowFromContext(ctx))
	if !active {
		return nodes
	}
	out := make([]*graph.Node, 0, len(nodes))
	for _, n := range nodes {
		if opts.ScopeAllows(n) {
			out = append(out, n)
		}
	}
	return out
}

// InitCombo initializes the query→symbol combo tracker. Persists per-repo,
// same cache directory as feedback; zero-effect no-op when either argument
// is empty. mode selects the max-age reap schedule (AI: 7 days, human: 30).
func (s *Server) InitCombo(cacheDir, repoPath string, mode AgentMode) {
	s.combo = newComboManager(cacheDir, repoPath, mode)
}

// InitFrecency initializes the implicit symbol frecency tracker. mode
// selects the decay regime — ModeAI (3-day half-life) for MCP server use;
// ModeHuman (10-day) for interactive sessions.
func (s *Server) InitFrecency(cacheDir, repoPath string, mode AgentMode) {
	s.frecency = newFrecencyTracker(cacheDir, repoPath, mode)
}

// InitSavings wires the persistent token-savings store into tokenStats so
// every source-reading tool call accumulates cumulative totals. Call once
// after NewServer; safe to skip when persistence isn't desired.
//
// Propagates to the sessionMap too so per-session counters (daemon path)
// also flush to the shared persistent store. Without this propagation a
// proxy that connects before InitSavings runs would hold a tokenStats
// with nil persistent and silently drop observations.
func (s *Server) InitSavings(store *savings.Store, repoPath string) {
	if store == nil || s.tokenStats == nil {
		return
	}
	s.tokenStats.mu.Lock()
	s.tokenStats.persistent = store
	s.tokenStats.repoPath = repoPath
	s.tokenStats.mu.Unlock()
	if s.sessions != nil {
		s.sessions.setPersistent(store, repoPath)
	}
}

// tokenStatsFor returns the tokenStats for the current request. Mirrors
// sessionFor: when ctx carries a session ID the per-session counter is
// returned, otherwise the shared default. Per-session counters share
// the same persistent store so disk totals accumulate across clients.
func (s *Server) tokenStatsFor(ctx context.Context) *tokenStats {
	id := SessionIDFromContext(ctx)
	if id == "" || s.sessions == nil {
		return s.tokenStats
	}
	return s.sessions.get(id).tokenStats
}

// FlushSavings is kept for shutdown-path compatibility. The sidecar-backed
// ledger commits every observation as it is recorded, so there is nothing
// buffered to write.
func (s *Server) FlushSavings() error {
	store := s.savingsStore()
	if store == nil {
		return nil
	}
	return store.Flush()
}

// savingsStore extracts the persistent savings store via tokenStats. Returns
// nil when persistence isn't initialized.
func (s *Server) savingsStore() *savings.Store {
	if s == nil || s.tokenStats == nil {
		return nil
	}
	s.tokenStats.mu.Lock()
	store := s.tokenStats.persistent
	s.tokenStats.mu.Unlock()
	return store
}

// cumulativeSavingsSnapshot exposes the persistent savings state for
// inclusion in graph_stats. Returns nil when persistence isn't wired so
// single-shot CLI calls don't emit confusing empty totals.
func (s *Server) cumulativeSavingsSnapshot() map[string]any {
	if s.tokenStats == nil {
		return nil
	}
	s.tokenStats.mu.Lock()
	store := s.tokenStats.persistent
	s.tokenStats.mu.Unlock()
	if store == nil {
		return nil
	}

	snap, err := store.Snapshot()
	if err != nil && s.logger != nil {
		s.logger.Warn("cumulative savings snapshot failed", zap.Error(err))
	}
	costs := savings.CostAvoidedAll(snap.Totals.TokensSaved)
	out := map[string]any{
		"first_seen":       snap.FirstSeen.Format(time.RFC3339),
		"last_updated":     snap.LastUpdated.Format(time.RFC3339),
		"tokens_saved":     snap.Totals.TokensSaved,
		"tokens_returned":  snap.Totals.TokensReturned,
		"calls_counted":    snap.Totals.CallsCounted,
		"cost_avoided_usd": costs,
	}
	// cost_avoided_usd above is a counterfactual — the same neutral token
	// count priced against every model. When the host surfaced the model
	// that actually drove calls (via the hook model-hint bridge), add the
	// real per-model attribution: tokens rescaled into the model's own
	// tokenizer and priced at that model's rate. per_client mirrors it for
	// the MCP client app.
	if models, err := store.ModelTotals(time.Time{}); err == nil && len(models) > 0 {
		rows := make([]map[string]any, 0, len(models))
		for _, m := range models {
			adj := tokens.ScaleFromCL100K(m.Name, m.TokensSaved)
			rows = append(rows, map[string]any{
				"model":            m.Name,
				"calls_counted":    m.CallsCounted,
				"tokens_saved":     adj,
				"cost_avoided_usd": savings.CostAvoided(adj, m.Name),
			})
		}
		out["per_model_actual"] = rows
	}
	if clients, err := store.ClientTotals(time.Time{}); err == nil && len(clients) > 0 {
		rows := make([]map[string]any, 0, len(clients))
		for _, c := range clients {
			rows = append(rows, map[string]any{
				"client":        c.Name,
				"calls_counted": c.CallsCounted,
				"tokens_saved":  c.TokensSaved,
			})
		}
		out["per_client"] = rows
	}
	if snap.DroppedObservations > 0 {
		out["dropped_observations"] = snap.DroppedObservations
	}
	return out
}

// ExportContext generates a portable context briefing for the given task.
// This is the public API for the CLI command, delegating to the MCP handler.
func (s *Server) ExportContext(ctx context.Context, task, entryPoint, format string, maxSymbols, tokenBudget int) (*mcp.CallToolResult, error) {
	args := map[string]any{
		"task":         task,
		"format":       format,
		"max_symbols":  float64(maxSymbols),
		"token_budget": float64(tokenBudget),
	}
	if entryPoint != "" {
		args["entry_point"] = entryPoint
	}
	argsJSON, _ := json.Marshal(args)
	req := mcp.CallToolRequest{}
	req.Params.Name = "export_context"
	_ = json.Unmarshal(argsJSON, &req.Params.Arguments)
	return s.handleExportContext(ctx, req)
}

// RegisterToolScope records the ToolScope for toolName so the
// dispatcher can validate `repo` per call. Tools that don't register a
// scope behave as if unscoped — legacy single-repo behavior — until
// every tool is migrated.
func (s *Server) RegisterToolScope(toolName string, scope ToolScope) {
	s.toolScopes.set(toolName, scope)
}

// ToolScope returns the registered scope for toolName and whether one
// has been declared. Used by tests asserting that every tool has a
// scope, and by the dispatcher.
func (s *Server) ToolScope(toolName string) (ToolScope, bool) {
	return s.toolScopes.get(toolName)
}

// ToolScopeMap returns a snapshot of every registered tool name →
// scope-name mapping. Used for diagnostics and for the
// `workspace_info`-style introspection tool (scope: workspace).
func (s *Server) ToolScopeMap() map[string]string {
	return s.toolScopes.snapshot()
}

// RegisteredScopedTools returns the registered tool names sorted
// lexically. Test convenience.
func (s *Server) RegisteredScopedTools() []string {
	return s.toolScopes.allTools()
}

// ResolveToolScope is the public entry point used by the dispatcher:
// looks up the tool's scope, then validates the request's `repo`
// argument against it. Returns either the resolved repo set or a
// structured protocol error suitable for the caller to surface
// verbatim.
//
// When the tool isn't in the registry, returns a nil ScopedRepos and
// nil error — callers treat this as "unscoped, do not enforce" so
// gradual migration doesn't break anything.
func (s *Server) ResolveToolScope(toolName string, repo any) (*ScopedRepos, *mcp.CallToolResult) {
	scope, ok := s.toolScopes.get(toolName)
	if !ok {
		return nil, nil
	}
	return ResolveScopedRepos(scope, repo)
}

// communityCacheToken is the per-graph identity tuple
// handleAnalyzeClusters checks before re-running the incremental
// detector. EdgeIdentity moves on provenance churn; NodeCount and EdgeCount
// cover additions/removals. analysisRevision closes the remaining same-count
// mutation gap on durable stores (for example a rebind or source-location
// shift). A zero token is "never populated".
type communityCacheToken struct {
	edgeIdentity     int
	nodeCount        int
	edgeCount        int
	analysisRevision uint64
}

func (s *Server) currentCommunityToken() communityCacheToken {
	token := communityCacheToken{
		edgeIdentity: s.graph.EdgeIdentityRevisions(),
		nodeCount:    s.graph.NodeCount(),
		edgeCount:    s.graph.EdgeCount(),
	}
	if writer, _ := s.analysisGenerationBackends(); writer != nil {
		token.analysisRevision = writer.AnalysisMutationRevision()
	}
	return token
}

// RunAnalysis performs community detection and process discovery on
// the current graph, then pushes a `notifications/resources/updated`
// for every bootstrap resource so subscribed clients can refresh
// without polling.
func (s *Server) RunAnalysis() {
	runtimeactivity.Begin("analysis")
	analysisStarted := time.Now()
	var memoryBefore runtime.MemStats
	runtime.ReadMemStats(&memoryBefore)
	defer func() {
		runtimeactivity.End("analysis")
		s.releaseTransientAnalysisIfIdle()
		writer, _ := s.analysisGenerationBackends()
		s.scheduleAnalysisGenerationPrune(writer)
		scheduleOSMemoryReleaseAfterBurst(s.logger, "mcp_analysis")
	}()

	s.analysisMu.Lock()
	analysisMetrics := s.populateAnalysisLocked()
	s.analysisMu.Unlock()

	// The graph was just rebuilt, so the lazy-enrichment ledger — symbol
	// IDs whose incoming refs were confirmed against the *previous* graph
	// — is both potentially stale (a reindex can re-mint those IDs) and
	// unbounded across a long daemon session. Reset it so re-confirmation
	// runs against the new graph and the ledger stays scoped to one
	// analysis epoch. Kept outside analysisMu: refsConfirmed carries its
	// own synchronisation and a racing reader just re-confirms, which is
	// idempotent.
	s.resetConfirmedRefs()

	// The test-symbol probe backing the review's coverage attestation is a
	// property of the graph that was just rebuilt: a reindex under a new
	// exclude set, or simply the test-edge pass finishing after a probe ran
	// mid-warmup, changes the answer. Without this reset the first answer
	// stuck for the daemon's lifetime and every review claimed the index
	// carried no test symbols.
	s.resetTestIndexProbe()

	// Bootstrap-resource payloads (graph_stats, index_health, etc.)
	// can change after re-warm even when the analysis itself didn't
	// — node counts move on every reindex. Fire updates regardless.
	s.notifyBootstrapResourcesUpdated()

	// Coarse hot-reload signal: the graph has just been rebuilt, so
	// any cached query result a long-lived client holds may be stale.
	if s.graphInvalidatedBroadcaster != nil && s.graph != nil {
		s.graphInvalidatedBroadcaster.broadcast(s.graph.NodeCount(), s.graph.EdgeCount(), "reanalysis")
	}

	var memoryAfter runtime.MemStats
	runtime.ReadMemStats(&memoryAfter)
	if s.logger != nil {
		s.logger.Info("mcp: analysis pass complete",
			zap.Bool("cache_hit", analysisMetrics.cacheHit),
			zap.Duration("cache_load", analysisMetrics.cacheLoad),
			zap.Duration("cache_save", analysisMetrics.cacheSave),
			zap.Duration("snapshot", analysisMetrics.snapshot),
			zap.Duration("leiden", analysisMetrics.leiden),
			zap.Duration("processes", analysisMetrics.processes),
			zap.Duration("pagerank", analysisMetrics.pageRank),
			zap.Duration("adjacency", analysisMetrics.adjacency),
			zap.Duration("auto_concepts", analysisMetrics.autoConcepts),
			zap.Duration("hits", analysisMetrics.hits),
			zap.Duration("total", time.Since(analysisStarted)),
			zap.Uint64("heap_alloc_before_bytes", memoryBefore.HeapAlloc),
			zap.Uint64("heap_alloc_after_bytes", memoryAfter.HeapAlloc),
			zap.Uint64("heap_inuse_before_bytes", memoryBefore.HeapInuse),
			zap.Uint64("heap_inuse_after_bytes", memoryAfter.HeapInuse),
			zap.Uint64("heap_idle_before_bytes", memoryBefore.HeapIdle),
			zap.Uint64("heap_idle_after_bytes", memoryAfter.HeapIdle),
			zap.Uint64("heap_released_before_bytes", memoryBefore.HeapReleased),
			zap.Uint64("heap_released_after_bytes", memoryAfter.HeapReleased),
			zap.Uint64("heap_sys_before_bytes", memoryBefore.HeapSys),
			zap.Uint64("heap_sys_after_bytes", memoryAfter.HeapSys),
			zap.Uint64("stack_inuse_before_bytes", memoryBefore.StackInuse),
			zap.Uint64("stack_inuse_after_bytes", memoryAfter.StackInuse))
	}
}

var osMemoryReleaseScheduled atomic.Bool

// scheduleOSMemoryReleaseAfterBurst coalesces full-graph query bursts and
// releases their heap high-water after the response has had time to leave the
// handler. The short delay keeps debug.FreeOSMemory's stop-the-world work out
// of the user-visible tool latency while bounding idle RSS after analysis.
func scheduleOSMemoryReleaseAfterBurst(logger *zap.Logger, reason string) {
	ScheduleMemoryReleaseAfterBurst(logger, reason)
}

// ScheduleMemoryReleaseAfterBurst coalesces process-wide allocation bursts and
// reclaims their idle heap only after all tracked foreground and background work
// has remained quiet. It is exported for the daemon lifecycle, which shares the
// same process and therefore must share the same gate and scheduler.
func ScheduleMemoryReleaseAfterBurst(logger *zap.Logger, reason string) {
	if !memoryReleaseEnabled() {
		return
	}
	if !osMemoryReleaseScheduled.CompareAndSwap(false, true) {
		return
	}
	go runScheduledMCPMemoryRelease(logger, reason)
}

// resetConfirmedRefs clears the lazy-enrichment ledger (see the
// refsConfirmed field). sync.Map has no clear-all, so this ranges and
// deletes; it is safe against concurrent confirmSymbolRefsOnDemand
// readers because a racing miss just re-confirms the symbol, which is
// idempotent — it re-lands the same lsp_resolved edges.
func (s *Server) resetConfirmedRefs() {
	s.refsConfirmed.Range(func(k, _ any) bool {
		s.refsConfirmed.Delete(k)
		return true
	})
}

// resetTestIndexProbe clears the per-repo-prefix test-symbol probe (see the
// testIndexProbe field) so the next review re-scans the rebuilt graph. Like
// resetConfirmedRefs it ranges-and-deletes because sync.Map has no clear-all,
// and it is safe against a concurrent testLangsIndexed reader: a racing miss
// just re-scans, which is idempotent.
func (s *Server) resetTestIndexProbe() {
	s.testIndexProbe.Range(func(k, _ any) bool {
		s.testIndexProbe.Delete(k)
		return true
	})
}

func (s *Server) analysisSnapshotCurrentLocked() bool {
	return s.analysisEpoch > 0 && s.communitiesToken == s.currentCommunityToken()
}

func (s *Server) getCommunities() *analysis.CommunityResult {
	_ = s.ensureCommunitiesMaterialized()
	s.analysisMu.RLock()
	defer s.analysisMu.RUnlock()
	if !s.analysisSnapshotCurrentLocked() {
		return nil
	}
	return s.communities
}

// incrementalCommunities runs Leiden community detection through the
// incremental path, threading the per-server partition cache so a
// re-run after only a few packages changed re-partitions just those
// packages. The cache it returns is stored back under analysisMu so
// the next clusters request can build on it. The accompanying stats
// describe whether the fast path or a full recompute ran.
//
// Short-circuits when the cached communities are still valid for the
// live graph: the (NodeCount, EdgeCount, EdgeIdentityRevisions) token
// captured by the last detector run is compared against the current
// graph identity in three scalar reads. On a disk backend a match skips the
// AllNodes / AllEdges fingerprint scan that otherwise dominates the
// call (~140s on a fresh daemon) and serves the existing partition
// straight from the cache. The reported stats describe a no-op
// incremental run (no changed packages, no repartitioned nodes) so
// callers see the cache hit on the wire.
func (s *Server) incrementalCommunities() (*analysis.CommunityResult, analysis.IncrementalCommunityStats) {
	_ = s.ensureCommunitiesMaterialized()
	_ = s.ensureLeidenMaterialized()
	s.analysisMu.Lock()
	defer s.analysisMu.Unlock()
	cur := s.currentCommunityToken()
	if s.communities != nil && s.communitiesToken == cur {
		stats := analysis.IncrementalCommunityStats{
			Incremental: true,
		}
		if s.leidenCache != nil {
			stats.TotalPackages = len(s.leidenCache.PackageFingerprints())
		}
		if s.logger != nil {
			s.logger.Debug("incrementalCommunities cache hit",
				zap.Int("nodes", cur.nodeCount),
				zap.Int("edges", cur.edgeCount),
				zap.Int("edge_identity_rev", cur.edgeIdentity))
		}
		return s.communities, stats
	}
	if s.logger != nil {
		// INFO-level on the miss path so a regression that re-introduces
		// a steady-state cache miss is visible without flipping the
		// daemon to debug. The full token diff is here precisely to
		// catch background-mutation regressions (some pass keeps drifting
		// the edge count under the cache and the Leiden walk runs every
		// call). A real first-call miss is a single line in the log.
		s.logger.Info("incrementalCommunities cache miss",
			zap.Bool("communities_nil", s.communities == nil),
			zap.Int("cached_nodes", s.communitiesToken.nodeCount),
			zap.Int("cur_nodes", cur.nodeCount),
			zap.Int("cached_edges", s.communitiesToken.edgeCount),
			zap.Int("cur_edges", cur.edgeCount),
			zap.Int("cached_edge_rev", s.communitiesToken.edgeIdentity),
			zap.Int("cur_edge_rev", cur.edgeIdentity))
	}
	result, cache, stats := analysis.DetectCommunitiesLeidenIncremental(s.graph, s.leidenCache)
	s.communities = result
	s.leidenCache = cache
	// Capture the token AFTER the algo finishes — if the graph mutated
	// during the (potentially slow) detector run, the token reflects
	// the state the result was actually computed against, and the next
	// call's token comparison stays meaningful.
	s.communitiesToken = s.currentCommunityToken()
	// A cache miss ran a real community recompute. Invalidate the dependent
	// hotspot ranking and advance its epoch while holding analysisMu: an
	// in-flight getHotspots build that captured the old communities will see
	// the epoch change, discard its result, and rebuild before publishing.
	s.hotspots = nil
	s.hotspotsReady = false
	s.analysisEpoch++
	return result, stats
}

func (s *Server) getProcesses() *analysis.ProcessResult {
	_ = s.ensureProcessesMaterialized()
	s.analysisMu.RLock()
	defer s.analysisMu.RUnlock()
	if !s.analysisSnapshotCurrentLocked() {
		return nil
	}
	return s.processes
}

func (s *Server) getPageRank() *analysis.PageRankResult {
	_ = s.ensureNodeMetricsMaterialized()
	s.analysisMu.RLock()
	defer s.analysisMu.RUnlock()
	if !s.analysisSnapshotCurrentLocked() {
		return nil
	}
	return s.pageRank
}

// getAdjacency returns the cached CSR adjacency snapshot built by the
// last RunAnalysis pass, or nil before the first pass. The snapshot is
// immutable after construction, so the caller may run seeded walks over
// it after releasing the read lock.
func (s *Server) getAdjacency() *analysis.AdjacencySnapshot {
	_ = s.ensureAdjacencyMaterialized()
	s.analysisMu.RLock()
	defer s.analysisMu.RUnlock()
	if !s.analysisSnapshotCurrentLocked() {
		return nil
	}
	return s.adjacency
}

// getAutoConcepts returns the per-repo auto-mined concept
// vocabulary. Nil until the first RunAnalysis pass; callers
// nil-check (AutoConcepts.Expand is itself nil-safe).
func (s *Server) getAutoConcepts() *search.AutoConcepts {
	_ = s.ensureAutoConceptsMaterialized()
	s.analysisMu.RLock()
	defer s.analysisMu.RUnlock()
	if !s.analysisSnapshotCurrentLocked() {
		return nil
	}
	return s.autoConcepts
}

// getHITS returns the HITS authority/hub result. Nil until the
// first RunAnalysis pass; callers nil-check (HITSResult accessors
// are themselves nil-safe).
func (s *Server) getHITS() *analysis.HITSResult {
	_ = s.ensureNodeMetricsMaterialized()
	s.analysisMu.RLock()
	defer s.analysisMu.RUnlock()
	if !s.analysisSnapshotCurrentLocked() {
		return nil
	}
	return s.hits
}

// getHotspots returns the default-threshold hotspot ranking for the current
// analysis epoch, computing it on first demand. Concurrent first callers are
// coalesced. If RunAnalysis invalidates the epoch while a build is in flight,
// the stale result is discarded and rebuilt against the new communities.
// The returned slice is shared and must not be mutated by the caller.
func (s *Server) getHotspots() []analysis.HotspotEntry {
	if !s.ensureCommunitiesMaterialized() && s.hotspotsFn == nil {
		return nil
	}
	s.analysisMu.RLock()
	if !s.analysisSnapshotCurrentLocked() {
		s.analysisMu.RUnlock()
		return nil
	}
	if s.hotspotsReady {
		hotspots := s.hotspots
		s.analysisMu.RUnlock()
		return hotspots
	}
	s.analysisMu.RUnlock()

	s.hotspotsBuildMu.Lock()
	defer s.hotspotsBuildMu.Unlock()
	for {
		s.analysisMu.RLock()
		if !s.analysisSnapshotCurrentLocked() {
			s.analysisMu.RUnlock()
			return nil
		}
		if s.hotspotsReady {
			hotspots := s.hotspots
			s.analysisMu.RUnlock()
			return hotspots
		}
		epoch := s.analysisEpoch
		token := s.communitiesToken
		communities := s.communities
		s.analysisMu.RUnlock()

		build := analysis.FindHotspots
		if s.hotspotsFn != nil {
			build = s.hotspotsFn
		}
		hotspots := build(s.graph, communities, 0)

		s.analysisMu.Lock()
		if s.analysisEpoch == epoch && s.communitiesToken == token && s.analysisSnapshotCurrentLocked() {
			s.hotspots = hotspots
			s.hotspotsReady = true
			s.analysisMu.Unlock()
			scheduleOSMemoryReleaseAfterBurst(s.logger, "lazy_hotspots")
			return hotspots
		}
		epochChanged := s.analysisEpoch != epoch
		s.analysisMu.Unlock()
		if !epochChanged {
			// The graph changed before a new analysis snapshot was published.
			// Fail closed instead of spinning or serving hotspots built from the
			// stale community assignment.
			return nil
		}
	}
}

// SetArchitecture installs the declarative architecture-rules DSL so
// check_guards evaluates layered violations alongside the flat guard
// rules. Called by the server / daemon entrypoint right after
// NewServer; a no-op effect when the config carries no layers.
func (s *Server) SetArchitecture(arch config.ArchitectureConfig) {
	s.architecture = arch
}

// SetEventRules installs the declarative event-boundary rule family so
// change_contract evaluates pub/sub producer/consumer constraints. Called by
// the server / daemon entrypoint right after NewServer; a no-op when the
// config carries no event rules.
func (s *Server) SetEventRules(rules []config.EventRule) {
	s.eventRules = rules
}

// Analysis deliberately has no watcher-driven trigger. Re-running it on every
// graph change was self-defeating: the pass is minutes of whole-graph work
// started by a change, so it ran while further changes landed and could never
// be published against a stable revision. Nothing has to replace it — the
// currency token is derived from graph state, so a change already makes the
// snapshot non-current, and the next consumer starts a fresh pass through
// ensureAnalysis.

// ServeStdio starts the MCP server on stdin/stdout.
func (s *Server) ServeStdio() error {
	return server.ServeStdio(s.mcpServer)
}

// MCPServer returns the underlying MCP server instance.
// This is used by the eval-server to wire tool dispatch into an HTTP handler.
func (s *Server) MCPServer() *server.MCPServer {
	return s.mcpServer
}

// addTool registers a tool whose handler is wrapped with the overlay
// apply/revert middleware (see overlay.go::wrapToolHandler). Every
// tool added through this helper picks up overlay-aware behaviour
// transparently — graph-walking tools see the editor-buffer view,
// source-reading tools see overlay content. Tools registered the old
// way (s.mcpServer.AddTool) still work but bypass the middleware.
//
// Routing every internal registration through this helper means both
// the daemon-dispatched path (HandleMessage) and the in-process HTTP
// path (Handler.CallToolStrict) get identical overlay semantics — the
// latter bypasses mcp-go's Hooks, so handler-level wrapping is the
// only place that covers both transports.
//
// Lazy-tool routing (N50): when the registry is enabled and the tool
// name is not in hotEagerTools, the (tool, handler) pair is stashed
// in s.lazy instead of registered with the live MCP server. The
// tools_search discovery tool returns the schema on demand and calls
// lazy.Promote(name), which lands the tool in mcpServer via the
// closure wired in attachLazyRegistry. Net effect: the initial
// tools/list payload drops from ~88 tools to ~25, reducing per-
// session context burn for token-economical clients while keeping
// the full surface reachable through a one-call discovery hop.
func (s *Server) addTool(tool mcp.Tool, handler server.ToolHandlerFunc) {
	handler = s.prepareTool(&tool, handler)
	if s.lazy != nil && s.lazy.IsDeferred(tool.Name) {
		s.lazy.Register(tool, handler)
		return
	}
	s.mcpServer.AddTool(tool, s.wrapToolHandler(handler))
}

// addControlTool registers a live tool that must manage overlay state/views
// itself. Unlike a bare mcpServer.AddTool it still captures facade adapters and
// applies gates, sanitization, telemetry, logging, panic recovery, and response
// middleware. These tools stay live because they are session/control recovery
// paths or optional subsystems registered after the initial lazy sweep.
func (s *Server) addControlTool(tool mcp.Tool, handler server.ToolHandlerFunc) {
	handler = s.prepareTool(&tool, handler)
	s.mcpServer.AddTool(tool, s.wrapControlToolHandler(handler))
}

func (s *Server) prepareTool(tool *mcp.Tool, handler server.ToolHandlerFunc) server.ToolHandlerFunc {
	// Scrub control characters / ANSI escapes out of the tool's text
	// before it reaches any client's tools/list rendering. tool is a
	// value copy, so this mutates only the registered instance.
	scrubToolText(tool)
	// Embed a project-size-scaled exploration-call budget in navigation
	// tools' descriptions so the model self-throttles. Runs before the
	// deferred-vs-live split so a tool keeps the hint after promotion.
	s.annotateToolBudget(tool)
	// Replace the recurring-parameter prose (format / max_bytes / cursor /
	// fields / scope / repo / project / workspace / ref) with a terse gloss;
	// the full semantics live once in the server instructions legend. Runs
	// before the split so deferred tools carry the compact schema too.
	compactSharedToolParams(tool)
	// View selection is handled and stripped by universal request middleware,
	// but it is still part of every callable tool's public input contract.
	publishViewSelectorSchema(tool)
	// #597: dispatch reads only the keys it knows, so an unknown option
	// used to vanish silently — the caller paid the full un-windowed
	// response with no signal to self-correct. Close any structured schema
	// that never took a position so the published contract states what the
	// guard below enforces. Facade names are exempt: their compatibility
	// wrapper deliberately accepts legacy call shapes the facade schema
	// does not declare, and their options envelopes are open by design.
	guarded := !isFacadeToolName(tool.Name)
	if guarded && tool.RawInputSchema == nil && tool.InputSchema.AdditionalProperties == nil {
		tool.InputSchema.AdditionalProperties = false
	}
	// Capture the finished schema plus the unwrapped legacy implementation
	// before lazy routing. Reused facade names receive a compatibility wrapper
	// that keeps their old call shape outside facade-v1 sessions.
	if s.facades != nil {
		s.facades.capture(*tool, handler)
		handler = s.wrapLegacyFacade(tool.Name, handler)
	}
	if guarded {
		handler = wrapToolArgGuard(*tool, handler)
	}
	return handler
}

// attachLazyRegistry wires the deferred catalog to the live MCP
// server so a tools_search-driven promotion lands the tool in
// mcpServer (which then fires notifications/tools/list_changed for
// subscribed clients). Safe to call multiple times; the latest
// closure wins.
func (s *Server) attachLazyRegistry() {
	if s.lazy == nil {
		return
	}
	s.lazy.promote = func(dt *deferredTool) {
		s.mcpServer.AddTool(dt.tool, s.wrapToolHandler(dt.handler))
	}
}

// EnsureToolPromoted makes a deferred tool callable by name. Under the
// defer-mode tool surface (the `core` default) a tool that is held out of the
// eager tools/list lives in the lazy registry and is unknown to the underlying
// MCP server until tools_search promotes it — so a direct tools/call for it
// returns "tool not found". A caller that dispatches a tools/call by name —
// notably the CLI's `gortex call` and the curated `gortex` verbs, which reach
// the daemon over the same socket — calls this first so the "reachable by name
// regardless of tools/list visibility" contract actually holds. tools_search
// stays the discovery path; this only makes an already-known name reachable
// without a discovery round-trip. It is a no-op (returns false) when there is
// no lazy registry or the tool is live, absent, or already promoted; a hidden
// (hide-mode) tool is never deferred, so this never bypasses the hide gate.
func (s *Server) EnsureToolPromoted(name string) bool {
	if s == nil || s.lazy == nil || name == "" {
		return false
	}
	if !s.lazy.IsDeferred(name) {
		return false
	}
	return len(s.lazy.Promote(name)) > 0
}

// EnsureToolPromotedForSession is the per-connection promote-on-demand entry
// point used by the daemon dispatcher. Live/absent names take the cheap no-op
// path; a genuinely deferred name is promoted only when ctx's effective
// surface permits calling it. This prevents a hide-mode request from mutating
// the shared lazy registry before the MCP call gate rejects the tool.
func (s *Server) EnsureToolPromotedForSession(ctx context.Context, name string) bool {
	if s == nil || s.lazy == nil || name == "" || !s.lazy.IsDeferred(name) {
		return false
	}
	if !s.IsToolEnabledForSession(ctx, name) {
		return false
	}
	return s.EnsureToolPromoted(name)
}

// SetContractRegistry sets an explicit contract registry override for the MCP
// server. Used by single-indexer callers and tests. In multi-repo mode the
// server prefers a freshly-merged registry from MultiIndexer (see
// effectiveContractRegistry) so that repos tracked or re-indexed at runtime
// are visible immediately.
func (s *Server) SetContractRegistry(r *contracts.Registry) {
	s.contractRegistry = r
}

// effectiveContractRegistry resolves the current contract registry. It prefers
// a live view over any snapshot: in multi-repo mode it re-merges per-repo
// registries on every call so that track_repository / index_repository at
// runtime take effect without a restart. Falls back to the single indexer,
// then to the explicit override.
func (s *Server) effectiveContractRegistry() *contracts.Registry {
	if s.multiIndexer != nil {
		return s.multiIndexer.MergedContractRegistry()
	}
	if s.indexer != nil {
		if cr := s.indexer.ContractRegistry(); cr != nil {
			return cr
		}
	}
	return s.contractRegistry
}

// SetSemanticManager sets the semantic enrichment manager for the MCP server.
func (s *Server) SetSemanticManager(m *semantic.Manager) {
	s.semanticMgr = m
}

// SetTelemetryRecorder installs the consent-gated usage recorder. Passing a
// disabled or nil recorder leaves telemetry off; the dispatch path is unchanged
// either way because Record is nil-safe and fail-silent.
func (s *Server) SetTelemetryRecorder(r *telemetry.Recorder) {
	s.recorder = r
}

// FlushTelemetry persists any buffered usage counts. Called on daemon
// shutdown; a no-op when telemetry is off.
func (s *Server) FlushTelemetry() {
	s.recorder.Flush()
}

// SemanticManager returns the semantic enrichment manager.
func (s *Server) SemanticManager() *semantic.Manager {
	return s.semanticMgr
}

// watcherHistory is the subset of indexer.Watcher / indexer.MultiWatcher
// the MCP server consumes. Defined as an interface so the server can
// accept either a single-repo Watcher (legacy `gortex mcp --watch` path)
// or a MultiWatcher (daemon path) through one SetWatcher call. The two
// concrete types already expose the same surface; this interface just
// names it.
type watcherHistory interface {
	History() []indexer.GraphChangeEvent
	HistorySince(since time.Time) []indexer.GraphChangeEvent
	OnSymbolChange(cb indexer.SymbolChangeCallback)
}

// SetWatcher sets the watcher after background initialization and registers
// a symbol change callback to record modifications in symbolHistory.
// Accepts either a single-repo *indexer.Watcher or a multi-repo
// *indexer.MultiWatcher — both satisfy watcherHistory.
func (s *Server) currentWatcher() watcherHistory {
	s.watcherMu.RLock()
	defer s.watcherMu.RUnlock()
	return s.watcher
}

func (s *Server) SetWatcher(w watcherHistory) {
	s.watcherMu.Lock()
	s.watcher = w
	s.watcherMu.Unlock()

	// Register callback to track symbol modifications for
	// get_symbol_history AND fan stale_refs notifications to any
	// subscribed sessions whose working set intersects the change.
	w.OnSymbolChange(func(filePath string, oldSymbols, newSymbols []*graph.Node) {
		// stale_refs broadcaster runs first so a slow notification
		// path (e.g. a clogged session channel) can't delay the
		// in-process symbol history bookkeeping below.
		if s.staleRefsBroadcaster != nil {
			s.staleRefsBroadcaster.handleSymbolChange(filePath, oldSymbols, newSymbols)
		}
		oldMap := make(map[string]string, len(oldSymbols)) // ID → signature
		for _, n := range oldSymbols {
			sig, _ := n.Meta["signature"].(string)
			oldMap[n.ID] = sig
		}

		newMap := make(map[string]string, len(newSymbols)) // ID → signature
		for _, n := range newSymbols {
			if n.Kind == graph.KindFile || n.Kind == graph.KindImport {
				continue
			}
			sig, _ := n.Meta["signature"].(string)
			newMap[n.ID] = sig
		}

		// Detect modified symbols (present in both old and new with changed signature).
		for id, oldSig := range oldMap {
			if newSig, exists := newMap[id]; exists {
				sigChanged := oldSig != newSig
				s.symHistory.Record(id, sigChanged)
			}
		}

		// Detect removed symbols (in old but not in new).
		for id := range oldMap {
			if _, exists := newMap[id]; !exists {
				s.symHistory.Record(id, true)
			}
		}

		// Detect added symbols (in new but not in old).
		for id := range newMap {
			if _, exists := oldMap[id]; !exists {
				s.symHistory.Record(id, false)
			}
		}
	})
}
