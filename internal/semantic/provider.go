package semantic

import (
	"context"
	"errors"
	"time"

	"github.com/zzet/gortex/internal/graph"
)

// Provider enriches graph edges and nodes with semantic information
// from a language-aware analysis backend (SCIP, go/types, LSP, etc.).
type Provider interface {
	// Name returns a human-readable identifier (e.g., "scip-go", "go-types", "gopls").
	Name() string

	// Languages returns the language codes this provider handles (e.g., ["go"]).
	Languages() []string

	// Available reports whether this provider can run. Checks for
	// external tool availability (e.g., scip-go on PATH, go command present).
	Available() bool

	// Enrich performs a full enrichment pass over the graph for the given repo root.
	// It upgrades edge confidence, adds missing edges, and fills Node.Meta fields.
	// Called after tree-sitter indexing + resolver pass completes.
	Enrich(g graph.Store, repoRoot string) (*EnrichResult, error)

	// EnrichFile performs a targeted enrichment for a single file and its
	// immediate dependents. Used in watch mode for incremental updates.
	// Returns nil result if incremental enrichment is not supported.
	EnrichFile(g graph.Store, repoRoot string, filePath string) (*EnrichResult, error)

	// Close releases any resources held by the provider (daemon processes,
	// temp files, connections).
	Close() error
}

// FileBatchEnricher is an optional provider capability for a known, repo-scoped
// file frontier. Implementations must process the complete deduplicated batch
// without issuing one heavyweight workspace load per file.
type FileBatchEnricher interface {
	EnrichFiles(g graph.Store, repoPrefix, repoRoot string, filePaths []string) (*EnrichResult, error)
}

// ContextFileBatchEnricher is the cancellation-aware sibling of
// FileBatchEnricher. The manager uses it for partial frontiers so heavyweight
// compiler loads inherit the manager's deadline instead of escaping through a
// context.Background root. Providers keep FileBatchEnricher for compatibility;
// implementations that support cancellation should implement both.
type ContextFileBatchEnricher interface {
	EnrichFilesContext(ctx context.Context, g graph.Store, repoPrefix, repoRoot string, filePaths []string) (*EnrichResult, error)
}

// RepoScopedProvider is an optional interface a Provider MAY implement to
// receive the repo prefix of the enrichment root alongside the root path.
// In a multi-repo daemon the shared graph holds file nodes from every
// tracked repo, and two repos can share a relative path; a provider that
// selects its work by walking graph file nodes needs the prefix to scope
// to the repo actually being enriched rather than guessing from disk
// existence (which a path collision defeats). The Manager calls EnrichRepo
// when the provider implements it, passing the repo's prefix (empty in
// single-repo mode); otherwise it falls back to Enrich.
type RepoScopedProvider interface {
	EnrichRepo(g graph.Store, repoPrefix, repoRoot string) (*EnrichResult, error)
}

// EnrichDeadlinePolicy computes the per-repo enrichment context deadline
// from the post-filter unenriched candidate count — the symbols a prior
// pass has NOT already stamped, i.e. the work that actually remains. It
// lets a ContextEnricher size its own window AFTER candidate selection: a
// warm restart with few unstamped nodes lands a small budget, while a cold
// repo (nothing stamped) keeps the full size-scaled headroom. A non-positive
// return means "no deadline" — the pass runs unbounded. A nil policy is
// equivalent (the un-contexted Enrich / EnrichRepo entry points pass nil).
type EnrichDeadlinePolicy func(candidates int) time.Duration

// ContextEnricher is an optional interface a Provider MAY implement to
// receive a cancellation context for its per-repo pass. Providers that
// implement it are cancelled *cooperatively* at the Manager's per-repo
// deadline instead of being detached: the provider lands whatever work it
// has completed, marks the result Partial, and returns — so a deadline
// never discards finished enrichment and never leaks a goroutine that
// keeps mutating the graph after the pass was "abandoned".
//
// deadline (may be nil) sizes the pass's own context bound lazily, from the
// count of candidates left after already-stamped nodes are skipped — so the
// budget tracks the real remaining work rather than the whole-repo node
// count. The Manager keeps a generous outer ceiling on the context it passes
// in; the provider narrows it via deadline once selection is done.
type ContextEnricher interface {
	EnrichRepoContext(ctx context.Context, g graph.Store, repoPrefix, repoRoot string, deadline EnrichDeadlinePolicy) (*EnrichResult, error)
}

// PreselectionDeadlineEnricher marks a ContextEnricher whose expensive work
// begins before it can count a post-filter candidate frontier (for example a
// SCIP provider must first run its external indexer). The Manager gives these
// providers the same eager, repo-node-scaled deadline used by the legacy path
// instead of the generous lazy-selection outer ceiling.
type PreselectionDeadlineEnricher interface {
	UsePreselectionDeadline()
}

// ReadinessProber is an optional interface a Provider MAY implement when its
// server answers `initialize` quickly but is not ready to serve semantic
// queries until a slower background load finishes — the Roslyn / MSBuild
// solution load a csharp-ls / OmniSharp pass sits behind. The Manager calls
// WaitReady BEFORE it starts the per-repo enrichment deadline, so that cold
// load does not consume the query budget (which otherwise elapses during the
// load, leaving the pass with zero useful edges). WaitReady must respect ctx
// (a bounded readiness budget) and should return promptly for a server that is
// already ready — providers whose servers serve queries immediately after
// initialize simply do not implement this interface. The returned error is
// best-effort: enrichment proceeds regardless, so a probe failure never blocks
// the pass.
type ReadinessProber interface {
	WaitReady(ctx context.Context, repoRoot string) error
}

// ErrWorkspaceNotReady is returned by a ReadinessProber.WaitReady when the
// server never finished loading its workspace within the readiness budget. The
// Manager treats it specially: rather than run a futile sweep against a server
// that will answer every query empty (the "completed in 8s, 0 coverage"
// pathology), it records an honest not-ready state and skips the pass, leaving
// the repo un-enriched so a later cycle retries. Any other WaitReady error is
// best-effort and the pass proceeds.
var ErrWorkspaceNotReady = errors.New("semantic: workspace did not become ready within the readiness budget")

// EnrichResult contains statistics from an enrichment pass.
type EnrichResult struct {
	Provider       string `json:"provider"`
	Language       string `json:"language"`
	EdgesConfirmed int    `json:"edges_confirmed"`
	EdgesRefuted   int    `json:"edges_refuted"`
	// EdgesRebound counts unconfirmed edges whose target the definition
	// answer REWROTE to a different same-name declaration (tagged
	// rebound_from on the edge). A rebind corrects the heuristic graph —
	// kept out of EdgesConfirmed so that count only ever means "the
	// heuristic target was right".
	EdgesRebound    int     `json:"edges_rebound"`
	EdgesAdded      int     `json:"edges_added"`
	NodesEnriched   int     `json:"nodes_enriched"`
	SymbolsCovered  int     `json:"symbols_covered"`
	SymbolsTotal    int     `json:"symbols_total"`
	CoveragePercent float64 `json:"coverage_percent"`
	DurationMs      int64   `json:"duration_ms"`
	// HoverCandidates is the post-filter count of symbols this pass selected
	// for hover enrichment — total symbols minus file/import nodes and minus
	// the nodes a prior pass already stamped. Deadline budgeting scales the
	// per-repo enrichment window on this number.
	HoverCandidates int `json:"hover_candidates,omitempty"`
	// BudgetSeconds is the per-repo enrichment deadline this pass derived
	// lazily from HoverCandidates via the EnrichDeadlinePolicy, in seconds.
	// The Manager surfaces it as the enrichment status deadline so the health
	// surface reflects the actual (candidate-scaled) window, not the whole-repo
	// estimate. 0 means the pass ran unbounded (nil policy / non-positive bound).
	BudgetSeconds float64 `json:"budget_seconds,omitempty"`
	// LockWaitMs is the time this pass spent queued on the graph-wide
	// resolve mutex before its apply phase could start. It is inside
	// DurationMs but outside the pass's budget accounting — the wait is
	// another pass's work — so a 400s duration with 390s of lock wait reads
	// as the queueing it was, not as this provider's cost.
	LockWaitMs int64 `json:"lock_wait_ms,omitempty"`
	// Partial reports that the pass was cut short (per-repo deadline /
	// context cancellation) after landing some — but not all — of its
	// work. The counters above reflect only what actually reached the
	// graph. AbortReason carries the cause when Partial is true.
	Partial     bool   `json:"partial,omitempty"`
	AbortReason string `json:"abort_reason,omitempty"`
	// BoundReason states why the add-phase stopped where it did, so a
	// "completed" state that covered < 100% of its targets is never read as
	// full coverage: "budget" (a deadline cut the pass), "cap" (the pass
	// finished but some targets were skipped, e.g. no source position or an
	// unservable file), or "completed_all" (every target visited).
	BoundReason string `json:"bound_reason,omitempty"`
	// ReferencesAddPass reports that the enrich pass added edges via
	// textDocument/references (rather than call hierarchy) because the
	// server advertised references but no call hierarchy — e.g. intelephense.
	// Lets index_health distinguish this add mode from the hierarchy mode.
	ReferencesAddPass bool `json:"references_add_pass,omitempty"`
	// Degraded reports that the pass ran in a reduced mode — reference
	// confirmation only, no hover / hierarchy sweep — because a server that
	// needs a compilation database (clangd) found none. It is an intentional
	// degradation, not a failure: the confirmed edges are trustworthy, but
	// hover types and hierarchy edges are absent. DegradedReason carries why.
	Degraded       bool   `json:"degraded,omitempty"`
	DegradedReason string `json:"degraded_reason,omitempty"`
	// Compiler is the compiler-context work of a go/packages-backed pass:
	// loads, type-checked packages and compiled files, and the load scope.
	// nil when the provider reports none.
	Compiler *CompilerLoadStats `json:"compiler,omitempty"`
}

// Bounding reasons for the enrichment add-phase (EnrichResult.BoundReason /
// EnrichmentStatus.BoundReason).
const (
	EnrichBoundBudget       = "budget"
	EnrichBoundCap          = "cap"
	EnrichBoundCompletedAll = "completed_all"
)

// Enrichment lifecycle states surfaced per (repo, provider) via
// Manager.EnrichmentStatuses — the health signal that lets an agent see
// an un-enriched or partially-enriched graph instead of assuming green.
const (
	EnrichStateRunning   = "running"
	EnrichStateDraining  = "draining" // deadline/cancel hit; manager is waiting for the in-process writer to stop
	EnrichStateCompleted = "completed"
	EnrichStatePartial   = "partial"   // deadline hit; completed work landed and is counted
	EnrichStateAbandoned = "abandoned" // timed-out provider stopped; terminal result discarded
	EnrichStateFailed    = "failed"
	EnrichStateNotReady  = "not_ready" // readiness prober timed out; sweep skipped, repo left for retry
)

// EnrichmentStatus reports the lifecycle state of one provider's per-repo
// enrichment pass. Exposed through index_health so consumers can tell a
// fully-enriched graph from one whose LSP pass was cut or abandoned.
type EnrichmentStatus struct {
	Repo     string `json:"repo"`
	Provider string `json:"provider"`
	Language string `json:"language,omitempty"`
	State    string `json:"state"`
	// StartedAt is stamped when the pass enters EnrichStateRunning and carried
	// through EnrichStateDraining while a timed-out writer is stopping.
	// Consumed by the daemon status surface to render a live elapsed
	// time next to the per-repo deadline instead of a mute "in progress".
	StartedAt       time.Time `json:"started_at,omitempty"`
	DeadlineSeconds float64   `json:"deadline_seconds,omitempty"`
	DurationMs      int64     `json:"duration_ms,omitempty"`
	EdgesConfirmed  int       `json:"edges_confirmed"`
	EdgesRebound    int       `json:"edges_rebound"`
	EdgesAdded      int       `json:"edges_added"`
	NodesEnriched   int       `json:"nodes_enriched"`
	// Add-phase coverage — the targets eligible for the hover/references
	// pass, how many were visited, and why the pass stopped. Always emitted
	// so a "completed" state that covered < 100% of targets is legible as a
	// coverage sliver rather than trusted as full enrichment.
	SymbolsTotal    int     `json:"symbols_total"`
	SymbolsCovered  int     `json:"symbols_covered"`
	CoveragePercent float64 `json:"coverage_percent"`
	BoundReason     string  `json:"bound_reason,omitempty"`
	// ReferencesAddPass marks that this provider added edges via
	// textDocument/references (no call hierarchy) — the intelephense-style
	// add mode. Distinguishes it from the call-hierarchy add mode.
	ReferencesAddPass bool `json:"references_add_pass,omitempty"`
	// Degraded marks that this provider could not run its full pass — e.g. a
	// clangd sweep with no compilation database, or a language toolchain that
	// would not load. State stays "completed"; DegradedReason says why.
	//
	// Degraded alone does not say whether anything landed, and the difference
	// matters to index_health: a pass that still confirmed edges is a partial
	// success, while one whose counters are all zero means the semantic tier
	// does not exist for that language at all. index_health separates the two
	// and only the second lowers semantic_enrichment_ok.
	Degraded       bool   `json:"degraded,omitempty"`
	DegradedReason string `json:"degraded_reason,omitempty"`
	Detail         string `json:"detail,omitempty"`
}

// ProviderStatus represents the current state of a semantic provider.
type ProviderStatus struct {
	Name            string        `json:"name"`
	Language        string        `json:"language"`
	Status          string        `json:"status"` // "ready", "unavailable", "error"
	CoveragePercent float64       `json:"coverage_percent,omitempty"`
	LastResult      *EnrichResult `json:"last_result,omitempty"`
}
