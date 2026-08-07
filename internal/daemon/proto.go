// Package daemon implements the long-living Gortex daemon plus the tiny
// stdio proxy that relays MCP traffic to it.
package daemon

import (
	"encoding/json"
	"fmt"
	"io"
	"time"
)

// ProtocolVersion is the daemon wire-protocol version. Both ends of a
// connection must agree; the handshake exchange rejects mismatches so an
// older proxy talking to a newer daemon (or vice versa) fails loudly
// instead of corrupting state.
const ProtocolVersion = 1

// ConnectionMode identifies the traffic shape the client wants after the
// handshake completes.
//
// ModeMCP — MCP JSON-RPC 2.0 pass-through (newline-delimited). The proxy
// dumps whatever its MCP client sent on stdin straight to the socket.
// ModeControl — Gortex control RPC (track, untrack, status, reload,
// shutdown). Used by the CLI and other daemon subcommands.
type ConnectionMode string

const (
	ModeMCP     ConnectionMode = "mcp"
	ModeControl ConnectionMode = "control"
)

// Handshake is the first message every client sends after dialing the
// socket. Everything flowing through after the ACK is mode-dependent.
//
// CWD lets the daemon derive a default repo scope when the client is an
// MCP proxy. Empty is acceptable for control clients.
type Handshake struct {
	Version    int            `json:"version"`
	Mode       ConnectionMode `json:"mode"`
	CWD        string         `json:"cwd,omitempty"`
	ClientName string         `json:"client,omitempty"` // e.g. "claude-code", "kiro", "cli"
	PID        int            `json:"pid,omitempty"`
	// LogicalSessionID is a proxy-owned opaque token that survives socket
	// reconnects. The daemon rebinds an existing MCP session with this token
	// when the originating proxy PID is still alive, preserving per-session
	// planning, localization, subscriptions, and client metadata. Empty keeps
	// the traditional connection-scoped lifecycle.
	LogicalSessionID string `json:"logical_session_id,omitempty"`
	// Tools / ToolsMode carry the client-side tool-surface preference
	// (GORTEX_TOOLS / --tools and GORTEX_TOOLS_MODE / --tools-mode of the
	// `gortex mcp` proxy). The daemon serves a shared graph, so a per-client
	// preset can only be honoured if the client hands its choice over at
	// connect time — the proxy's own byte-pump filter can subtract from the
	// daemon's list but never widen it. Forwarding the spec lets the daemon
	// resolve the effective surface for THIS session authoritatively (env
	// override wins), covering both narrower and wider presets. Empty means
	// "no client preference — use the daemon default / client-name default".
	Tools     string `json:"tools,omitempty"`
	ToolsMode string `json:"tools_mode,omitempty"`
}

// HandshakeAck is the daemon's reply to a handshake. On failure ErrorCode
// is non-empty and the connection is closed after the ack is written.
type HandshakeAck struct {
	// Protocol / status.
	OK        bool   `json:"ok"`
	ErrorCode string `json:"error_code,omitempty"`
	ErrorMsg  string `json:"error_msg,omitempty"`

	// Populated on success.
	SessionID     string `json:"session_id,omitempty"`
	DefaultRepo   string `json:"default_repo,omitempty"`
	ActiveProject string `json:"active_project,omitempty"`

	// For clients that want to compare before trusting the connection.
	DaemonVersion  string `json:"daemon_version,omitempty"`
	DaemonInstance string `json:"daemon_instance,omitempty"` // changes on every daemon process start

	// Warming is true when the handshake completed but the daemon has not
	// finished its warmup pipeline — the graph is still filling, so tool
	// results may be partial. The session is fully usable; this is advisory
	// so a connecting proxy / CLI can wait for full data (or just log it)
	// instead of guessing. WarmupPhase carries the last-published phase name
	// (snapshot_loaded → parallel_parse → … → ready) when known.
	Warming     bool   `json:"warming,omitempty"`
	WarmupPhase string `json:"warmup_phase,omitempty"`
}

// Error codes reported in HandshakeAck.ErrorCode. Kept small and stable so
// client code can branch on them without parsing prose.
const (
	ErrProtocolMismatch = "protocol_mismatch"
	ErrUnsupportedMode  = "unsupported_mode"
	ErrRepoNotTracked   = "repo_not_tracked"
	ErrInternal         = "internal"
	// ErrTimeout means the daemon accepted the control request but did not
	// finish it inside the kind's budget — almost always because a
	// minutes-long track / reload / enrichment is holding the controller
	// mutex. Distinct from ErrInternal so a caller can retry or degrade
	// instead of treating a busy daemon as a broken one.
	ErrTimeout = "timeout"
)

// ControlRequest is the envelope for every message sent in ModeControl.
// Kind identifies which operation; Params is operation-specific JSON.
type ControlRequest struct {
	Kind   string          `json:"kind"`
	Params json.RawMessage `json:"params,omitempty"`
}

// ControlResponse is the paired envelope from the daemon. One response per
// request, in order. Non-empty ErrorCode means the operation failed.
type ControlResponse struct {
	OK        bool            `json:"ok"`
	ErrorCode string          `json:"error_code,omitempty"`
	ErrorMsg  string          `json:"error_msg,omitempty"`
	Result    json.RawMessage `json:"result,omitempty"`
}

// Control operation kinds. One constant per kind so callers can't typo.
const (
	ControlTrack   = "track"
	ControlUntrack = "untrack"
	ControlReload  = "reload"
	// ControlProxy reloads servers.toml and rebuilds + atomically swaps
	// the daemon's multi-server Router, then invalidates the roster
	// cache — so `gortex proxy on/off/add/remove` apply to a running
	// daemon without a restart. Distinct from ControlReload, which runs
	// a full repo track/untrack reconcile and never touches the router.
	ControlProxy  = "proxy"
	ControlStatus = "status"
	// ControlProbe is the liveness/scope answer a short-lived caller needs
	// — is the daemon up, is it ready, and which repos does it track — with
	// none of the aggregation ControlStatus performs.
	//
	// It exists because ControlStatus cannot serve that question reliably.
	// Status computes per-repo memory estimates, whole-store node/edge
	// counts, a stop-the-world runtime.ReadMemStats, and a stat of every
	// configured repo path, then takes the controller mutex — which is held
	// for the entire duration of a track / reload / enrichment. Measured on
	// a 44-repo workspace, serial calls with no concurrent load returned the
	// identical payload in anywhere from 156 ms to 11.5 s depending only on
	// what the indexer happened to be doing.
	//
	// Callers that budget in hundreds of milliseconds — the agent hooks —
	// therefore timed out against a perfectly healthy daemon and rendered
	// their "daemon unreachable" fallback. Probe touches no store, no
	// MemStats, no filesystem, and no mutex: it reads atomics and the config
	// registry, so an in-flight reindex cannot delay it.
	ControlShutdown      = "shutdown"
	ControlProbe         = "probe"
	ControlSearchSymbols = "search_symbols"
	// ControlEnrichChurn dispatches to Controller.EnrichChurn — the daemon
	// runs the churn enricher against its in-process graph so the CLI
	// (and the post-commit / post-merge git hooks) don't have to fight
	// the on-disk store's write lock the daemon holds.
	ControlEnrichChurn = "enrich_churn"
	// ControlEnrichReleases dispatches to Controller.EnrichReleases.
	// Same routing rationale as ControlEnrichChurn — the CLI hands the
	// enrichment to the daemon when one is up so the write lock stays
	// uncontested.
	ControlEnrichReleases = "enrich_releases"
	// ControlEnrichBlame dispatches to Controller.EnrichBlame — git-blame
	// authorship stamping against the daemon's in-process graph. Same
	// routing rationale as ControlEnrichChurn.
	ControlEnrichBlame = "enrich_blame"
	// ControlEnrichCoverage dispatches to Controller.EnrichCoverage —
	// Go cover-profile projection onto the daemon's in-process graph.
	// The CLI parses the profile and hands the raw segments to the
	// daemon so the daemon never has to read the caller's filesystem.
	ControlEnrichCoverage = "enrich_coverage"
	// ControlEnrichCochange dispatches to Controller.EnrichCochange —
	// co-change edge mining against the daemon's in-process graph.
	ControlEnrichCochange = "enrich_cochange"
)

// DefaultControlTimeout bounds the control kinds that are supposed to answer
// promptly. It is deliberately generous: a busy daemon can take seconds to
// aggregate status over a 20-repo workspace, and a bound that fires during
// normal load would be worse than none. What it rules out is the unbounded
// case — a controller wedged behind a minutes-long track / reload / enrichment
// leaving `gortex daemon stop`, `gortex daemon status` and every `gortex call`
// blocked forever with no output and no error.
const DefaultControlTimeout = 30 * time.Second

// ControlHandshakeTimeout bounds the handshake exchange. The daemon stamps
// warmup state onto the ack, so the ack read is a real round trip that a
// wedged daemon can stall; without a bound every caller — including the
// liveness probes that are supposed to degrade gracefully — blocks forever.
const ControlHandshakeTimeout = 10 * time.Second

// ControlTimeoutFor reports the round-trip budget for one control kind, or 0
// for kinds with no bound. Client and daemon both consult it so the two ends
// agree on which requests are allowed to run long.
//
// Track / untrack / reload / enrich_* legitimately run for many minutes on a
// large workspace — bounding them would break the operations users start on
// purpose. Everything else is a status/lifecycle call on the critical path of
// ordinary commands and must never be able to hang.
//
// Shutdown is unbounded too, for a different reason: the daemon flushes and
// closes its store *before* acking, and abandoning that flush half-done is
// worse than a slow stop. The stop command bounds its own wait instead and
// falls through to waiting for the process to exit, so the user is never left
// blocked either way.
func ControlTimeoutFor(kind string) time.Duration {
	switch kind {
	case ControlTrack, ControlUntrack, ControlReload, ControlShutdown,
		ControlEnrichChurn, ControlEnrichReleases, ControlEnrichBlame,
		ControlEnrichCoverage, ControlEnrichCochange:
		return 0
	default:
		return DefaultControlTimeout
	}
}

// TrackParams is the payload for ControlTrack.
type TrackParams struct {
	Path    string `json:"path"`
	Name    string `json:"name,omitempty"`
	Project string `json:"project,omitempty"`
	Ref     string `json:"ref,omitempty"`
	// AsWorktree forces a linked git worktree of an already-tracked repo
	// to be registered as an independent instance (the `--as-worktree`
	// flag) rather than coalescing into the canonical checkout.
	AsWorktree bool `json:"as_worktree,omitempty"`
}

// UntrackParams is the payload for ControlUntrack.
type UntrackParams struct {
	PathOrPrefix string `json:"path_or_prefix"`
}

// ProbeResponse is the payload returned under Result on a successful
// ControlProbe call: enough to decide whether the daemon can answer and
// which repos it owns, and nothing that costs a store read.
//
// Deliberately not a subset-typed StatusResponse. Sharing the type would
// leave every aggregate field present but zero, and a caller cannot tell a
// real zero from an unpopulated one — which is the same ambiguity that made
// a timed-out status read as "nothing is tracked".
type ProbeResponse struct {
	Version       string `json:"version"`
	PID           int    `json:"pid"`
	UptimeSeconds int64  `json:"uptime_seconds"`
	// Ready reports resolved references and a queryable graph; Enriched
	// reports the slow semantic passes finished on top of that. A caller
	// deciding whether to trust a graph answer wants Ready.
	Ready    bool `json:"ready"`
	Enriched bool `json:"enriched"`
	// TrackedRepos carries identity only — path, prefix, and the workspace
	// grouping. No counts, no byte estimates, and no on-disk liveness stat:
	// each of those is a scan, and identity is what a scope decision needs.
	TrackedRepos []ProbeRepo `json:"tracked_repos"`
	Sessions     int         `json:"sessions"`
}

// ProbeRepo is one tracked repo as ControlProbe reports it.
type ProbeRepo struct {
	Path      string `json:"path"`
	Prefix    string `json:"prefix"`
	Name      string `json:"name,omitempty"`
	Workspace string `json:"workspace,omitempty"`
	Project   string `json:"project,omitempty"`
}

// StatusResponse is the payload returned under Result on a successful
// ControlStatus call.
type StatusResponse struct {
	Version       string              `json:"version"`
	PID           int                 `json:"pid"`
	UptimeSeconds int64               `json:"uptime_seconds"`
	SocketPath    string              `json:"socket_path"`
	TrackedRepos  []TrackedRepoStatus `json:"tracked_repos"`
	Sessions      int                 `json:"sessions"`
	// MemoryBytes is runtime.MemStats.Alloc — live allocated heap.
	// Retained for backwards compatibility with older clients; new
	// clients should read from Runtime.
	MemoryBytes   uint64             `json:"memory_bytes"`
	Runtime       RuntimeStats       `json:"runtime"`
	SearchBackend SearchBackendStats `json:"search_backend"`
	// TrigramCache reports the in-memory literal-search index, which is
	// per repo, lazily built, and can be the largest single structure in
	// the daemon — while SearchBackend above may simultaneously report
	// the symbol index as disk-resident. Nil when no indexer is wired.
	TrigramCache *TrigramCacheStats `json:"trigram_cache,omitempty"`
	// PProfAddr is set when the daemon has opened an HTTP pprof
	// listener (via the GORTEX_DAEMON_PPROF_ADDR env var). Empty
	// string means pprof is not enabled on this daemon.
	PProfAddr string `json:"pprof_addr,omitempty"`
	// Ready is false while the daemon is still loading the snapshot and
	// resolving references in the background. It flips true once references
	// are resolved and the graph is queryable — which can be well before
	// EnrichmentComplete. The socket is reachable even when Ready=false.
	Ready         bool  `json:"ready"`
	WarmupSeconds int64 `json:"warmup_seconds"`

	// EnrichmentComplete is false while semantic enrichment and the
	// graph-wide derivation passes are still running in the background.
	// Ready can be true (references resolved and queryable) while this is
	// still false. EnrichSeconds records the full warmup duration.
	EnrichmentComplete bool  `json:"enrichment_complete"`
	EnrichSeconds      int64 `json:"enrich_seconds,omitempty"`

	// Enrichment reports live semantic-enrichment progress (repos
	// finished vs total, and the in-flight pass) so a client sees a
	// number instead of a mute "enrichment in progress". Nil when no
	// semantic manager is wired or it has never run a pass.
	Enrichment *EnrichmentProgress `json:"enrichment,omitempty"`

	// Workspaces aggregates TrackedRepos by workspace slug. Empty
	// when no repo declares one (every repo defaults to its own
	// workspace; the table form is more compact in that case).
	Workspaces []WorkspaceSummary `json:"workspaces,omitempty"`

	// MCPSessions lists every connected proxy client (Claude Code,
	// Cursor, Codex, etc.). Empty when the daemon hasn't yet been
	// asked to track sessions.
	MCPSessions []MCPSessionStatus `json:"mcp_sessions,omitempty"`

	// ConfiguredServers reflects `~/.gortex/servers.toml` when
	// present. Empty when running single-server (the file is missing
	// or empty); in that case the daemon serves locally and the
	// multi-server router is disabled.
	ConfiguredServers []ConfiguredServerStatus `json:"configured_servers,omitempty"`
	// LocalServerSlug is the slug this daemon recognises as itself
	// when matching against ConfiguredServers — set from servers.toml
	// `default` or the first entry. Empty when no servers.toml.
	LocalServerSlug string `json:"local_server_slug,omitempty"`

	// ToolPreset / ToolPresetMode report the active MCP tool-surface preset
	// and mode; LearnedTools is the per-workspace learned-promotion count
	// (deferred tools promoted through use, persisted across restarts). Empty
	// / zero on a control-only daemon.
	ToolPreset     string `json:"tool_preset,omitempty"`
	ToolPresetMode string `json:"tool_preset_mode,omitempty"`
	LearnedTools   int    `json:"learned_tools,omitempty"`

	// LSPRouter reports the daemon's LSP-router state — every
	// registered spec, whether its binary resolves on PATH, and
	// every alive (spec, workspace) subprocess. Empty when no LSP
	// router is wired (`semantic.enabled: false` in `.gortex.yaml`).
	LSPRouter *LSPRouterStatus `json:"lsp_router,omitempty"`
}

// LSPRouterStatus reflects one daemon's LSP-router state for the
// daemon-status TUI / `gortex daemon status` JSON.
type LSPRouterStatus struct {
	// DefaultWorkspace is the rootURI used when callers don't supply
	// a workspace (single-repo daemons).
	DefaultWorkspace string `json:"default_workspace,omitempty"`
	// EnabledSpecs lists every spec the user opted into via
	// `.gortex.yaml`, with a flag indicating whether its command
	// resolved on PATH at boot. Pure metadata — no spawn implied.
	EnabledSpecs []LSPSpecStatus `json:"enabled_specs"`
	// ActiveProviders lists every (spec, workspace) pair currently
	// owning an alive LSP subprocess. May be empty even when several
	// specs are enabled — the router lazy-spawns on first request.
	ActiveProviders []LSPActiveProvider `json:"active_providers,omitempty"`
}

// LSPSpecStatus is one row in LSPRouterStatus.EnabledSpecs.
type LSPSpecStatus struct {
	Name      string `json:"name"`
	Available bool   `json:"available"` // command on PATH right now
	Languages string `json:"languages"` // comma-separated for readability
}

// LSPActiveProvider is one alive subprocess.
type LSPActiveProvider struct {
	Spec      string `json:"spec"`
	Workspace string `json:"workspace"`
	LastUsed  string `json:"last_used"` // RFC3339
}

// EnrichmentProgress summarizes the semantic-enrichment manager's
// per-(repo, provider) status list into the counts a status line
// needs: how many repos have finished, and which pass (if any) is
// running right now.
type EnrichmentProgress struct {
	// Running is true while at least one (repo, provider) pass is in
	// the "running" state.
	Running bool `json:"running"`
	// ReposTotal is the number of distinct repos the manager has ever
	// recorded a pass for. ReposDone is the subset of those where
	// every recorded provider has reached a terminal state (completed,
	// partial, abandoned, or failed).
	ReposTotal int `json:"repos_total"`
	ReposDone  int `json:"repos_done"`
	// Current is the in-flight pass, when Running is true.
	Current *EnrichmentCurrent `json:"current,omitempty"`
}

// EnrichmentCurrent is the one (repo, provider) enrichment pass
// currently running, with how long it has been running and (when
// known) its deadline.
type EnrichmentCurrent struct {
	Repo            string  `json:"repo"`
	Provider        string  `json:"provider"`
	ElapsedSeconds  float64 `json:"elapsed_seconds"`
	DeadlineSeconds float64 `json:"deadline_seconds,omitempty"`
}

// RuntimeStats captures Go runtime.MemStats fields users care about
// when diagnosing daemon memory: live vs reserve, GC pressure, and
// goroutine count. All byte fields are raw (not human-formatted).
type RuntimeStats struct {
	Alloc        uint64 `json:"alloc"`         // live heap allocations
	Sys          uint64 `json:"sys"`           // total bytes from OS
	HeapInuse    uint64 `json:"heap_inuse"`    // bytes in in-use spans
	HeapIdle     uint64 `json:"heap_idle"`     // bytes in idle spans (released or reserve)
	HeapReleased uint64 `json:"heap_released"` // bytes returned to OS
	StackInuse   uint64 `json:"stack_inuse"`   // goroutine stacks
	NumGC        uint32 `json:"num_gc"`        // completed GC cycles
	NumGoroutine int    `json:"num_goroutine"` // live goroutines
}

// TrigramCacheStats reports the process-wide trigram searcher cache: how
// many repos hold a built index, its estimated heap, and the ceilings it
// is held under.
type TrigramCacheStats struct {
	Live      int   `json:"live"`
	MaxLive   int   `json:"max_live"`
	Bytes     int64 `json:"bytes"`
	MaxBytes  int64 `json:"max_bytes"`
	IdleTTLMs int64 `json:"idle_ttl_ms"`
	BuildsOff bool  `json:"builds_off,omitempty"`
	Evictions int64 `json:"evictions,omitempty"`
}

// SearchBackendStats identifies which search backend is currently
// serving queries, so users can read the `search_b` column in the
// repo breakdown with the right mental model. Bleve with the default
// gtreap KV store costs ~32 KiB per document; BM25 costs ~2 KiB.
type SearchBackendStats struct {
	Name     string `json:"name"`      // "bm25" | "bleve-memory" | "bleve-disk" | "sqlite-fts5"
	DocCount int    `json:"doc_count"` // indexed documents across all repos
	// DocCountKnown distinguishes "the index holds zero documents" from
	// "this backend cannot report a document count". Backends whose only
	// available figure is a since-construction Add/Remove delta leave it
	// false so renderers omit the number instead of presenting the delta
	// as a corpus size.
	DocCountKnown bool   `json:"doc_count_known,omitempty"`
	Bytes         uint64 `json:"bytes"`                // approximate heap footprint
	DiskPath      string `json:"disk_path,omitempty"`  // set only when Name == "bleve-disk"
	DiskBytes     uint64 `json:"disk_bytes,omitempty"` // current on-disk size for "bleve-disk"
	// DiskResident marks a backend (e.g. "sqlite-fts5") that has no
	// meaningful heap footprint of its own — its index lives inside the
	// graph store's own file — and no cheap byte count is available
	// either. Rendering must not print a fabricated "heap=0 B" for it.
	DiskResident bool `json:"disk_resident,omitempty"`
}

// SearchSymbolsParams is the payload for ControlSearchSymbols.
// Repo (optional) limits results to one repo prefix; Limit caps the result
// count (zero defaults to a small built-in cap so unbounded calls can't
// stall the hook).
type SearchSymbolsParams struct {
	Query string `json:"query"`
	Limit int    `json:"limit,omitempty"`
	Repo  string `json:"repo,omitempty"`
}

// SymbolHit is one entry in SearchSymbolsResult.Hits.
type SymbolHit struct {
	Name     string `json:"name"`
	Kind     string `json:"kind,omitempty"`
	FilePath string `json:"file_path"`
	Line     int    `json:"line,omitempty"`
}

// SearchSymbolsResult is the payload returned under Result for a
// successful ControlSearchSymbols call.
type SearchSymbolsResult struct {
	Hits []SymbolHit `json:"hits"`
}

// EnrichChurnParams is the payload for ControlEnrichChurn.
//
// Path scopes the enrichment to a single tracked repo (matched by
// prefix, abs path, or "" for "every tracked repo"). Branch overrides
// the default-branch resolution — pass "origin/main" / "main" / a tag
// / a SHA. Empty Branch means the daemon picks the default branch
// from each repo's working tree.
type EnrichChurnParams struct {
	Path   string `json:"path,omitempty"`
	Branch string `json:"branch,omitempty"`
}

// EnrichChurnResult is the payload returned under Result for a
// successful ControlEnrichChurn call. Counts are summed across every
// repo that participated (typically one).
type EnrichChurnResult struct {
	Files      int    `json:"files"`
	Symbols    int    `json:"symbols"`
	Branch     string `json:"branch"`
	HeadSHA    string `json:"head_sha"`
	DurationMS int64  `json:"duration_ms"`
}

// EnrichReleasesParams is the payload for ControlEnrichReleases.
//
// Path scopes the enrichment to a single tracked repo (prefix or
// absolute root, "" for "every tracked repo"). Branch restricts the
// considered tags to those reachable from that branch; empty Branch
// means "every tag in the repo" — matches the legacy `analyze
// kind=releases` behaviour.
type EnrichReleasesParams struct {
	Path   string `json:"path,omitempty"`
	Branch string `json:"branch,omitempty"`
}

// EnrichReleasesResult is the payload returned under Result for a
// successful ControlEnrichReleases call. Files is the count of file
// nodes stamped with meta.added_in across every repo that
// participated.
type EnrichReleasesResult struct {
	Files      int    `json:"files"`
	Branch     string `json:"branch,omitempty"`
	DurationMS int64  `json:"duration_ms"`
}

// EnrichBlameParams is the payload for ControlEnrichBlame.
//
// Path scopes the enrichment to a single tracked repo (matched by
// prefix, abs path, or "" for "every tracked repo").
type EnrichBlameParams struct {
	Path string `json:"path,omitempty"`
}

// EnrichBlameResult is the payload returned under Result for a
// successful ControlEnrichBlame call. Nodes is the count of symbol /
// file nodes stamped with meta.last_authored across every repo that
// participated.
type EnrichBlameResult struct {
	Nodes      int   `json:"nodes"`
	DurationMS int64 `json:"duration_ms"`
}

// EnrichCoverageSegment mirrors coverage.Segment on the wire so the
// CLI can parse the cover profile against its own filesystem (the
// profile path is relative to the caller, not the daemon) and hand the
// parsed segments to the daemon. Field shape matches coverage.Segment
// exactly.
type EnrichCoverageSegment struct {
	File      string `json:"file"`
	StartLine int    `json:"start_line"`
	EndLine   int    `json:"end_line"`
	NumStmt   int    `json:"num_stmt"`
	Count     int    `json:"count"`
}

// EnrichCoverageParams is the payload for ControlEnrichCoverage.
//
// Path scopes the enrichment to a single tracked repo (matched by
// prefix, abs path, or "" for "every tracked repo"). Segments are the
// pre-parsed cover-profile entries; the CLI parses the profile so the
// daemon never has to read the caller's filesystem.
type EnrichCoverageParams struct {
	Path     string                  `json:"path,omitempty"`
	Segments []EnrichCoverageSegment `json:"segments"`
}

// EnrichCoverageResult is the payload returned under Result for a
// successful ControlEnrichCoverage call. Symbols is the count of nodes
// stamped with meta.coverage_pct across every repo that participated;
// Segments echoes how many profile segments were supplied.
type EnrichCoverageResult struct {
	Symbols    int   `json:"symbols"`
	Segments   int   `json:"segments"`
	DurationMS int64 `json:"duration_ms"`
}

// EnrichCochangeParams is the payload for ControlEnrichCochange.
//
// Path scopes the enrichment to a single tracked repo (matched by
// prefix, abs path, or "" for "every tracked repo").
type EnrichCochangeParams struct {
	Path string `json:"path,omitempty"`
}

// EnrichCochangeResult is the payload returned under Result for a
// successful ControlEnrichCochange call. Edges is the count of
// co_change edges added across every repo that participated.
type EnrichCochangeResult struct {
	Edges      int   `json:"edges"`
	DurationMS int64 `json:"duration_ms"`
}

// TrackedRepoStatus is one row in StatusResponse.TrackedRepos.
type TrackedRepoStatus struct {
	Prefix string `json:"prefix"`
	Path   string `json:"path"`
	Name   string `json:"name,omitempty"`
	// Project is the GlobalConfig active-project slug — a named
	// grouping of repos in `~/.gortex/config.yaml::projects`.
	// Distinct from `WorkspaceProject` below, which is the project
	// slug from `.gortex.yaml::project`. Kept here for backwards
	// compatibility with older daemon clients that read the field.
	Project string `json:"project,omitempty"`
	// Workspace is the workspace slug stamped onto every node emitted
	// from this repo. Falls back to Prefix when no
	// `.gortex.yaml::workspace` is declared. Two repos that share a
	// Workspace pair their contracts as one logical service.
	Workspace string `json:"workspace,omitempty"`
	// WorkspaceProject is the project slug — the soft sub-boundary
	// inside Workspace. Falls back to Prefix when no
	// `.gortex.yaml::project` is declared.
	WorkspaceProject string          `json:"workspace_project,omitempty"`
	Ref              string          `json:"ref,omitempty"`
	Files            int             `json:"files"`
	Nodes            int             `json:"nodes"`
	Edges            int             `json:"edges"`
	LastIndex        int64           `json:"last_index_unix"`
	Memory           MemoryBreakdown `json:"memory"`
	// Missing is true when Path no longer names a directory on disk —
	// the checkout was deleted, renamed, or unmounted while the entry
	// stayed in `~/.gortex/config.yaml`. Such a repo can never be
	// re-indexed, so it is reported rather than silently carried.
	Missing bool `json:"missing,omitempty"`
	// Unloaded is true for a repo that is registered in the tracked-repo
	// config but that the daemon holds no index for — typically because
	// startup could not index it (its path had already been deleted).
	// The row is synthesised from the config registry so `daemon status`
	// and `gortex repos` report the same inventory instead of one view
	// dropping a repo the other still lists. Counts are zero.
	Unloaded bool `json:"unloaded,omitempty"`
}

// WorkspaceSummary aggregates per-workspace stats so `gortex daemon
// status` can render a "workspaces" block above the per-repo table.
// Reflects the workspace hard boundary: counts roll up across every
// repo declaring the same workspace slug.
type WorkspaceSummary struct {
	Slug     string   `json:"slug"`
	Repos    []string `json:"repos"`
	Projects []string `json:"projects"`
	Files    int      `json:"files"`
	Nodes    int      `json:"nodes"`
	Edges    int      `json:"edges"`
}

// MCPSessionStatus is one row in the sessions list. Reports the
// per-client state the daemon tracks: connection time, current cwd
// (used by the router for cwd-based workspace resolution), and the
// remote agent identifier when the client supplied one in MCP's
// `clientInfo`.
type MCPSessionStatus struct {
	ID            string `json:"id"`
	Cwd           string `json:"cwd,omitempty"`
	ClientName    string `json:"client_name,omitempty"`
	ClientVersion string `json:"client_version,omitempty"`
	ConnectedSecs int64  `json:"connected_secs"`
}

// ConfiguredServerStatus is one row in the servers list, mirroring a
// `~/.gortex/servers.toml::[[server]]` entry plus a "this is us"
// flag set when Slug equals the daemon's local slug.
type ConfiguredServerStatus struct {
	Slug       string   `json:"slug"`
	URL        string   `json:"url"`
	Default    bool     `json:"default,omitempty"`
	Local      bool     `json:"local,omitempty"`
	Workspaces []string `json:"workspaces,omitempty"`
	HasAuth    bool     `json:"has_auth,omitempty"`
}

// MemoryBreakdown is a per-repo memory estimate split across the
// data structures that dominate the daemon's footprint. All values
// are approximate — exact accounting would require walking Go's
// heap, which is too expensive for a status call. See the individual
// estimators (graph.RepoMemoryEstimate, search.BleveBackend.SizeBytes,
// search.VectorBackend.SizeBytes) for methodology.
type MemoryBreakdown struct {
	NodesBytes   uint64 `json:"nodes_bytes"`
	EdgesBytes   uint64 `json:"edges_bytes"`
	SearchBytes  uint64 `json:"search_bytes"`
	VectorsBytes uint64 `json:"vectors_bytes"`
	// DiskBytes is populated only when the Bleve backend is running in
	// disk mode (GORTEX_BLEVE_DISK_DIR set). Each repo gets a
	// node-proportional share of the on-disk index size. Zero in
	// memory-only mode.
	DiskBytes  uint64 `json:"disk_bytes,omitempty"`
	TotalBytes uint64 `json:"total_bytes"`
}

// WriteJSONLine writes v as one JSON object followed by a newline. The
// daemon protocol is newline-delimited JSON (NDJSON) so we can scan it
// cheaply with bufio.Scanner.
func WriteJSONLine(w io.Writer, v any) error {
	buf, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	buf = append(buf, '\n')
	_, err = w.Write(buf)
	return err
}
