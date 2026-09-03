package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"math"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/zzet/gortex/internal/analysis"
	"github.com/zzet/gortex/internal/audit"
	"github.com/zzet/gortex/internal/blame"
	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/contracts"
	"github.com/zzet/gortex/internal/coverage"
	"github.com/zzet/gortex/internal/excludes"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graphpath"
	"github.com/zzet/gortex/internal/graphview"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/persistence"
	"github.com/zzet/gortex/internal/query"
	"github.com/zzet/gortex/internal/semantic"
	"github.com/zzet/gortex/internal/tokens"
	"go.uber.org/zap"
)

// ensureFresh re-indexes any of the given file paths whose on-disk content has
// drifted from the indexed snapshot, so a subsequent graph read serves current
// data instead of a stale body. Returns the paths that were refreshed (capped at
// a handful per call).
//
// The self-heal is gated on IsTrackedStale, which is false for untracked, new,
// or already-current files — only a genuinely changed, already-indexed file is
// re-indexed. That gate is what makes this safe in multi-repo mode: an earlier
// version keyed the staleness check off the lone single-Indexer, whose mtime
// map is empty for cross-repo paths, so every file looked stale and the
// resulting mass re-index raced the live read surface and crashed the transport.
// Routing each path to its owning per-repo indexer keeps the check accurate and
// the work bounded to the file the caller is about to read.
func (s *Server) ensureFresh(filePaths []string) []string {
	var refreshed []string
	const limit = 5
	for _, fp := range filePaths {
		if len(refreshed) >= limit {
			break
		}
		idx, absPath := s.freshnessIndexer(fp)
		if idx == nil {
			continue
		}
		root := idx.RootPath()
		if root == "" {
			continue
		}
		rel, ok := relativeWithinRoot(root, absPath)
		if !ok {
			continue
		}
		// IsTrackedStale is false for untracked / new / current files, so
		// only a known-and-changed file triggers a re-index — no mass churn.
		if !idx.IsTrackedStale(rel) {
			continue
		}
		if !s.reindexFile(absPath) {
			s.logger.Warn("auto re-index failed",
				zap.String("file", fp),
				zap.String("resolved", absPath))
			continue
		}
		// Advance the recorded mtime so a follow-up read in the same window
		// sees the file as current and skips a redundant re-index.
		idx.RefreshFileMtime(absPath)
		refreshed = append(refreshed, fp)
	}
	return refreshed
}

// freshnessIndexer resolves a repo-prefixed, repo-relative, or absolute path to
// the indexer that owns it and the file's absolute path, for both single- and
// multi-repo daemons. In single-repo mode an active file watcher already owns
// freshness, so on-read auto-refresh stands down rather than fight it; in
// multi-repo mode each path is routed to its per-repo indexer, whose mtime map
// is populated — which is what makes the staleness check trustworthy.
func (s *Server) freshnessIndexer(fp string) (*indexer.Indexer, string) {
	if s.multiIndexer != nil {
		abs := fp
		if !filepath.IsAbs(abs) {
			if resolved := s.multiIndexer.ResolveFilePath(fp); resolved != "" {
				abs = resolved
			}
		}
		idx, _ := s.multiIndexer.IndexerForFile(abs)
		return idx, abs
	}
	if s.indexer == nil || s.currentWatcher() != nil {
		return nil, ""
	}
	abs := fp
	if !filepath.IsAbs(abs) {
		if root := s.indexer.RootPath(); root != "" {
			abs = filepath.Join(root, fp)
		}
	}
	return s.indexer, abs
}

func (s *Server) registerEnhancementTools() {
	// verify_change
	s.addTool(
		mcp.NewTool("verify_change",
			mcp.WithDescription("Given proposed signature changes, checks all callers and interface implementors for contract violations. Use before refactoring to catch breaking changes."),
			mcp.WithString("changes", mcp.Required(), mcp.Description("JSON array of {symbol_id, new_signature} objects")),
			mcp.WithBoolean("compact", mcp.Description("One-line-per-violation text output")),
		),
		s.handleVerifyChange,
	)

	// check_guards
	s.addTool(
		mcp.NewTool("check_guards",
			mcp.WithDescription("Evaluates project-specific guard rules against a set of changed symbols. Reports co-change and boundary violations."),
			mcp.WithString("ids", mcp.Required(), mcp.Description("Comma-separated list of changed symbol IDs")),
			mcp.WithBoolean("compact", mcp.Description("One-line-per-rule text output")),
			mcp.WithString("format", mcp.Description("Output format: json (default), gcx (GCX1 compact wire format), or toon")),
			mcp.WithNumber("max_bytes", mcp.Description("Cap the marshaled response at this many bytes. The longest list is trimmed; truncation metadata rides on the response. Omit for no cap.")),
		),
		s.handleCheckGuards,
	)

	// prefetch_context
	s.addTool(
		mcp.NewTool("prefetch_context",
			mcp.WithDescription("Predicts what context you will need next based on recent activity and a task description. Returns ranked symbols with relevance reasons."),
			mcp.WithString("task", mcp.Description("Natural language task description")),
			mcp.WithString("recent_symbols", mcp.Description("Comma-separated list of recently viewed symbol IDs")),
			mcp.WithBoolean("include_source", mcp.Description("Include source code for top 5 candidates")),
			mcp.WithNumber("limit", mcp.Description("Max candidates to return (default: 10)")),
			mcp.WithString("cursor", mcp.Description("Opaque pagination cursor from a previous `next_cursor` to fetch the next page.")),
			mcp.WithBoolean("paginate", mcp.Description("When true, the server caps each page at the project default budget and returns `next_cursor` for any tail.")),
			mcp.WithString("fields", mcp.Description("Comma-separated list of fields to keep on each candidate (e.g. \"id,confidence,reason\").")),
			mcp.WithBoolean("compact", mcp.Description("One-line-per-symbol text output")),
			mcp.WithString("format", mcp.Description("Output format: json (default), gcx (GCX1 compact wire format), or toon")),
			mcp.WithNumber("max_bytes", mcp.Description("Cap the marshaled response at this many bytes. The longest list is trimmed; truncation metadata rides on the response. Omit for no cap.")),
			mcp.WithNumber("max_tokens", mcp.Description(tokenBudgetParamDescription)),
		),
		s.handlePrefetchContext,
	)

	// analyze — unified graph analysis tool (dead_code, hotspots, cycles, would_create_cycle)
	s.addTool(
		mcp.NewTool("analyze",
			mcp.WithDescription(analyzeGroupedSummary),
			mcp.WithString("kind", mcp.Required(), mcp.Description(fmt.Sprintf("Analysis kind, one of: %s. Pass \"help\" for the full one-line-per-kind reference.", analyzeKindsCSV()))),
			mcp.WithString("framework", mcp.Description("(dbt_models) Filter to one transformation framework — dbt or sqlmesh")),
			mcp.WithString("materialized", mcp.Description("(dbt_models) Substring match on the model materialization — table, view, incremental, …")),
			mcp.WithBoolean("compact", mcp.Description("One-line-per-result text output")),
			mcp.WithString("format", mcp.Description("Output format: json (default), gcx (GCX1 compact wire format, per-kind hand-tuned encoder), or toon")),
			mcp.WithNumber("max_bytes", mcp.Description("Cap the marshaled response at this many bytes. The longest list is trimmed; truncation metadata rides on the response. Omit for no cap.")),
			mcp.WithBoolean("include_variables", mcp.Description("(dead_code) Include variable nodes (default false — usually false positives without data-flow analysis)")),
			mcp.WithBoolean("include_fields", mcp.Description("(dead_code) Include struct/class field nodes (default false — graph can't always pick a candidate for intra-function field reads, so fields look dead even when used)")),
			mcp.WithBoolean("include_constants", mcp.Description("(dead_code) Include constant nodes (default false — same caveat as variables)")),
			mcp.WithBoolean("include_cgo_exports", mcp.Description("(dead_code) Include functions annotated //export — default false; CGo exports have no Go-level callers")),
			mcp.WithBoolean("include_linkname_targets", mcp.Description("(dead_code) Include //go:linkname targets — default false; they are linked by name from outside the package")),
			mcp.WithBoolean("skip_cross_repo_nodes", mcp.Description("(dead_code) Drop nodes whose RepoPrefix is set — useful when cross-repo linking is incomplete")),
			mcp.WithNumber("threshold", mcp.Description("(hotspots) Complexity score threshold (default: mean + 2σ)")),
			mcp.WithString("repo", mcp.Description("Narrow this analysis to a single repository prefix, clamped to the session workspace. Applies to graph-node, edge-walk, graph-algorithm, framework, and file/AST-scan kinds (dead_code, hotspots, cycles, health_score, todos, stale_code, ownership, coverage_gaps, impact, bottlenecks, k8s_resources, dbt_models, external_calls, channel_ops, pubsub, routes, models, pagerank, sast, …). Community / git-mining / per-id / synthesizer kinds are workspace-bound but not repo-narrowed in v1. NOTE: for kind=cross_repo this names the repo whose cross-repo boundary dependencies to report (its existing meaning), not a result narrow.")),
			mcp.WithString("project", mcp.Description("Narrow this analysis to the repositories in a project, clamped to the session workspace. Applies to graph-node kinds (see `repo`).")),
			mcp.WithString("workspace", mcp.Description("Restrict the analysis to the active workspace slug; daemon sessions may only name their own workspace.")),
			mcp.WithString("scope", mcp.Description("Name of a saved scope (see save_scope) — its repositories narrow graph-node analyses, clamped to the session workspace. NOTE: for kind=cycles this is instead a file-path / package prefix that limits the cycle search (its existing meaning), not a saved-scope name. Community / git-mining / per-id / synthesizer kinds (clusters, concepts, suggest_boundaries, blame, coverage, fixes_history, retrieval_log, temporal_verify, would_create_cycle, def_use, synthesizers, resolution_outcomes, sql_rebuild) are workspace-bound but not repo-narrowed in v1.")),
			mcp.WithString("from_id", mcp.Description("(would_create_cycle) Source symbol ID")),
			mcp.WithString("to_id", mcp.Description("(would_create_cycle) Target symbol ID")),
			mcp.WithString("profile", mcp.Description("(coverage) Path to a Go cover.out profile, absolute or relative to the indexed repo root")),
			mcp.WithNumber("older_than", mcp.Description("(stale_code) Symbols last touched more than this many days ago — default 365")),
			mcp.WithString("email", mcp.Description("(stale_code) Filter to a single author email")),
			mcp.WithString("kinds", mcp.Description("(stale_code, ownership) Comma-separated kinds — default function,method; pass 'all' for every blame-eligible kind")),
			mcp.WithNumber("min_symbols", mcp.Description("(ownership) Drop authors with fewer than this many symbols — default 1")),
			mcp.WithString("path_prefix", mcp.Description("(ownership, coverage_gaps) Scope to nodes under this file-path prefix — e.g. 'internal/auth/'")),
			mcp.WithNumber("min_pct", mcp.Description("(coverage_gaps) Lower-inclusive coverage threshold — default 0")),
			mcp.WithNumber("max_pct", mcp.Description("(coverage_gaps) Upper-exclusive coverage threshold — default 100, i.e. anything not fully covered")),
			mcp.WithString("provider", mcp.Description("(stale_flags) Filter to a single provider — launchdarkly, growthbook, unleash, internal")),
			mcp.WithString("tag", mcp.Description("(todos) Filter by tag — TODO / FIXME / HACK / XXX / NOTE — case-insensitive. (releases) Filter to one release tag — returns the file list whose meta.added_in matches; populate via enrich_releases first.")),
			mcp.WithString("assignee", mcp.Description("(todos) Filter by exact assignee — case-sensitive")),
			mcp.WithString("ticket", mcp.Description("(todos) Filter by exact ticket reference — e.g. PROJ-42")),
			mcp.WithBoolean("has_assignee", mcp.Description("(todos) Keep only TODOs that have an assignee set")),
			mcp.WithString("base_kind", mcp.Description("(cross_repo) Scope to one base relation — calls, implements, or extends")),
			mcp.WithNumber("limit", mcp.Description("(cross_repo, error_surface, unsafe_patterns) Cap the number of rows returned — default 200")),
			mcp.WithString("language", mcp.Description("(unsafe_patterns, sast, hygiene) Comma-separated subset of languages to keep — rust, python, javascript, typescript, go, java, ruby, php")),
			mcp.WithString("detector", mcp.Description("(unsafe_patterns, sast, hygiene) Comma-separated subset of bundled detector names. The full catalog is available via the search_ast tool with no args; for SAST the names follow `py-*` / `go-*` / `js-*` / `java-*` / `ruby-*` / `php-*` / `rust-*` / `hygiene-*` conventions.")),
			mcp.WithString("severity", mcp.Description("(unsafe_patterns, sast, hygiene) Comma-separated subset of severity labels to keep — error, warning, info")),
			mcp.WithBoolean("exclude_tests", mcp.Description("(unsafe_patterns, sast, hygiene) Override the per-detector default (defaults to true — test-only matches are dropped)")),
			mcp.WithString("cwe", mcp.Description("(sast) Comma-separated subset of MITRE CWE identifiers to keep — e.g. 'CWE-78,CWE-89'")),
			mcp.WithBoolean("kinds_only", mcp.Description("(sast) Return only the per-detector + per-CWE breakdown; omit per-site `matches` rows. Use for a SAST surface snapshot without paying row bytes.")),
			mcp.WithString("grade", mcp.Description("(health_score) Comma-separated A..F subset to keep — e.g. 'd,f' for the worst-scoring symbols only")),
			mcp.WithNumber("min_score", mcp.Description("(health_score) Drop rows whose composite score is below this (0..100)")),
			mcp.WithNumber("max_score", mcp.Description("(health_score) Drop rows whose composite score is above this (0..100)")),
			mcp.WithNumber("min_axes", mcp.Description("(health_score) Require at least this many populated axes per row (default 1; raise to demand multi-signal confidence)")),
			mcp.WithString("roll_up", mcp.Description("(health_score) Aggregate per-symbol scores up to a coarser scope — 'file' (per-file average + per-grade counts) or 'repo' (per-repo). Omit for per-symbol rows.")),
			mcp.WithObject("target", mcp.AdditionalProperties(true), mcp.Description("(impact, def_use) Analysis target: {\"symbol\": \"<symbol id>\"} — or, for impact, {\"file\": \"<path>\"}. Lowered to the id / ids / path fields below. A kind with nothing to rank refuses the call instead of ignoring the target.")),
			mcp.WithString("ids", mcp.Description("(impact) Comma-separated symbol IDs — score exactly these symbols. A fixed set, not a closure walk; pass `id` for the blast radius of one symbol.")),
			mcp.WithString("id", mcp.Description("(impact, def_use) Target symbol ID. For impact this scopes the ranking to that symbol's blast radius — the symbol itself plus its transitive dependents — instead of the repo-wide ranking.")),
			mcp.WithString("path", mcp.Description("(impact) Target file — rank the blast radius of every symbol defined in it.")),
			mcp.WithBoolean("refresh_cochange", mcp.Description("(impact) Start legacy lazy co-change mining when cold (default true). The compact public operation fixes this false.")),
			mcp.WithBoolean("materialize", mcp.Description("(sql_call_sites) Rebuild SQL table/query edges before reading (legacy default true). The compact public operation fixes this false.")),
			mcp.WithString("name", mcp.Description("(named) The query bundle to run. Omit to list every available bundle.")),
			mcp.WithString("group_by", mcp.Description("(tests_as_edges) symbol (default — tested symbol → its tests) or test (test → symbols it exercises).")),
			mcp.WithString("algorithm", mcp.Description("(clusters) Community-detection algorithm — leiden (default), louvain, or spectral (recursive Fiedler-vector bisection).")),
			mcp.WithNumber("min_size", mcp.Description("(clusters) Drop clusters with fewer than this many members — default 3.")),
			mcp.WithNumber("resolution", mcp.Description("(clusters, leiden) Modularity resolution γ — default 1.0. Higher γ (e.g. 2.0) yields more, smaller communities; lower γ (e.g. 0.5) yields fewer, larger ones. γ = 1.0 is standard modularity and uses the cached incremental partition.")),
		),
		s.handleAnalyze,
	)

	// winnow_symbols — multi-axis constraint-chain retrieval
	s.addTool(
		mcp.NewTool("winnow_symbols",
			mcp.WithDescription("Structured constraint-chain retrieval. Combines BM25 text matching with structural filters (kind, language, fan-in/out, community, path prefix, churn, test classification) and returns a ranked list with per-axis score contributions. Use when search_symbols' free-text-only query is too coarse — e.g. 'methods in the auth community with fan-in >= 5 touching handlers/' or 'production functions only, no tests'."),
			mcp.WithString("kind", mcp.Description("Comma-separated node kinds to keep (function, method, type, interface, variable, contract)")),
			mcp.WithString("language", mcp.Description("Filter to a single language (go, typescript, python, ...)")),
			mcp.WithString("path_prefix", mcp.Description("Comma-separated file path prefixes — any match passes")),
			mcp.WithString("community", mcp.Description("Community ID (community-0) or label to scope to a functional cluster")),
			mcp.WithString("text_match", mcp.Description("BM25 text query; when absent ranking is purely structural")),
			mcp.WithNumber("min_fan_in", mcp.Description("Minimum incoming calls+references (default: 0)")),
			mcp.WithNumber("min_fan_out", mcp.Description("Minimum outgoing calls (default: 0)")),
			mcp.WithNumber("min_churn", mcp.Description("Minimum session modification count (default: 0)")),
			mcp.WithBoolean("is_test", mcp.Description("Tri-state test filter: true keeps only test symbols, false keeps only production symbols. Omit for no constraint.")),
			mcp.WithString("test_role", mcp.Description("Comma-separated test roles to keep: test, benchmark, fuzz, example")),
			mcp.WithNumber("limit", mcp.Description("Max results (default: 20)")),
			mcp.WithString("cursor", mcp.Description("Opaque pagination cursor from a previous `next_cursor` to fetch the next page.")),
			mcp.WithBoolean("paginate", mcp.Description("When true, the server caps each page at the project default budget and returns `next_cursor` for any tail.")),
			mcp.WithNumber("max_bytes", mcp.Description("Cap the marshaled response at this many bytes. The longest list is trimmed; truncation metadata rides on the response.")),
			mcp.WithString("fields", mcp.Description("Comma-separated list of fields to keep on each result (e.g. \"id,score,fan_in\").")),
			mcp.WithBoolean("compact", mcp.Description("One-line-per-result text output")),
			mcp.WithString("format", mcp.Description("Output format: json (default), gcx (GCX1 compact wire format), or toon")),
			mcp.WithNumber("max_bytes", mcp.Description("Cap the marshaled response at this many bytes. The longest list is trimmed; truncation metadata rides on the response. Omit for no cap.")),
			mcp.WithString("repo", mcp.Description("Filter results to a specific repository prefix")),
			mcp.WithString("project", mcp.Description("Filter results to repositories in a specific project")),
			mcp.WithString("ref", mcp.Description("Filter results to repositories with a specific reference tag")),
		),
		s.handleWinnowSymbols,
	)

	// scaffold
	s.addTool(
		mcp.NewTool("scaffold",
			mcp.WithDescription("Generates code scaffolding from an existing symbol pattern, including registration wiring and test stubs."),
			mcp.WithString("id", mcp.Required(), mcp.Description("Symbol ID to use as the pattern example")),
			mcp.WithString("new_name", mcp.Required(), mcp.Description("Name for the new symbol")),
			mcp.WithBoolean("dry_run", mcp.Description("Return scaffold without writing files (default: true)")),
			mcp.WithBoolean("compact", mcp.Description("Compact text output")),
		),
		s.handleScaffold,
	)

	// diff_context
	s.addTool(
		mcp.NewTool("diff_context",
			mcp.WithDescription("Returns graph-enriched context for symbols affected by a git diff: source, callers, callees, community, processes, and per-file risk."),
			mcp.WithString("scope", mcp.Description("unstaged (default), staged, all, or compare")),
			mcp.WithString("base_ref", mcp.Description("Branch/commit for compare scope (default: main)")),
			mcp.WithBoolean("compact", mcp.Description("One-line-per-symbol condensed output")),
			mcp.WithString("repo", mcp.Description("Repository prefix or path (multi-repo mode); defaults to the lone tracked repo or the session's cwd-bound repo")),
		),
		s.handleDiffContext,
	)

	// index_health
	s.addTool(
		mcp.NewTool("index_health",
			mcp.WithDescription("Reports the health and completeness of the Gortex index: parse failures, stale files, language coverage, and health score."),
			mcp.WithBoolean("compact", mcp.Description("Single-line summary output")),
			mcp.WithString("format", mcp.Description("Output format: json (default), gcx (GCX1 compact wire format), or toon")),
			mcp.WithNumber("max_bytes", mcp.Description("Cap the marshaled response at this many bytes; truncation metadata rides on the response.")),
		),
		s.handleIndexHealth,
	)

	// get_symbol_history
	s.addTool(
		mcp.NewTool("get_symbol_history",
			mcp.WithDescription("Returns symbols modified during the current session with modification counts. Flags churning symbols (modified 3+ times)."),
			mcp.WithString("id", mcp.Description("Specific symbol ID (omit for all)")),
			mcp.WithBoolean("compact", mcp.Description("One-line-per-symbol text output")),
		),
		s.handleGetSymbolHistory,
	)

	// batch_edit
	s.addTool(
		mcp.NewTool("batch_edit",
			mcp.WithDescription("Atomically applies a dependency-ordered edit set. Every guard and replacement is evaluated against one locked snapshot before any file is written; a commit failure restores all touched files. The durable transaction receipt survives response loss and daemon restart. Retry with the same transaction_id and identical edits to receive the original result without writing again, or omit edits and pass transaction_id to query status. Each edit is one of four operations selected by `op`:\n  • edit_symbol (default): {id, old_source, new_source} — replace a fragment inside a symbol's body.\n  • edit_file: {op:\"edit_file\", path, old_string, new_string, replace_all?} — replace a string in any file (imports, config, comments).\n  • move_file: {op:\"move_file\", source, destination, expected_sha256?} — move one regular file without overwriting the destination.\n  • delete_file: {op:\"delete_file\", path, expected_sha256?} — delete one regular file.\nPass `edits` as a JSON array of objects (a JSON-encoded string is accepted for compatibility)."),
			mcp.WithArray("edits",
				mcp.Description("Edit operations. Required for execution; omit only when querying an existing transaction. Each item is an edit_symbol, edit_file, move_file, or delete_file object selected by `op`."),
				mcp.Items(batchEditItemsSchema()),
			),
			mcp.WithString("transaction_id", mcp.Description("Stable caller-chosen idempotency key. Reusing it with identical edits returns the same receipt; a different payload is rejected. When omitted, the server creates a unique transaction ID.")),
			mcp.WithBoolean("status_only", mcp.Description("Query transaction_id without executing edits. Edits may also simply be omitted.")),
			mcp.WithBoolean("dry_run", mcp.Description("Return the dependency-ordered plan without applying changes")),
			mcp.WithBoolean("compact", mcp.Description("One-line transaction and per-edit summary")),
		),
		s.handleBatchEdit,
	)

	// contracts — unified contracts tool (list + check + validate)
	s.addTool(
		mcp.NewTool("contracts",
			mcp.WithDescription("API contracts tool. action=list (default): lists detected contracts (HTTP, gRPC, Thrift, GraphQL, topics, WebSocket, env, OpenAPI). action=check: detects orphan providers/consumers across repos. action=validate: diffs provider↔consumer request/response shapes and flags breaking/warning/info issues. action=bridge: queries the persisted contract-bridge subgraph — one node per matched provider↔consumer group (HTTP route, gRPC/Thrift method, pub/sub topic) — ranked by reciprocal rank fusion over text, path/repo, graph-adjacency, and consumer-degree signals (mode=rank, pass query and/or symbol), or expanded from a symbol into its cross-service blast radius (mode=impact, pass symbol).\n\nDEFAULT SCOPE for list: auto-scopes to the active project's repos and hides dependency-origin contracts (type=dependency, vendored paths like vendor/, node_modules/). The response reports other_repos (count of contracts filtered out of scope) and dependencies_skipped (count of dep contracts hidden). To widen scope, pass repo=<prefix>, project=<name>, ref=<tag>, or all_repos=true. To include dependency contracts, pass include_deps=true."),
			mcp.WithString("action", mcp.Description("list (default), check, validate, or bridge")),
			mcp.WithString("repo", mcp.Description("Filter by repository prefix")),
			mcp.WithString("project", mcp.Description("Filter to repositories in a specific project (resolves to the project's repo set)")),
			mcp.WithString("ref", mcp.Description("Filter to repositories tagged with this ref")),
			mcp.WithBoolean("all_repos", mcp.Description("(list) Disable active-project auto-scope; return contracts from every indexed repo. Default false.")),
			mcp.WithBoolean("include_deps", mcp.Description("(list) Include type=dependency contracts and contracts from vendored paths (vendor/, node_modules/, Pods/, .venv/). Default false.")),
			mcp.WithString("type", mcp.Description("(list) Filter by type: http, grpc, thrift, graphql, topic, ws, env, openapi, dependency")),
			mcp.WithString("query", mcp.Description("(bridge) Free-text query ranked against bridge canonical keys, contract names, repos, and file paths")),
			mcp.WithString("symbol", mcp.Description("(bridge) Symbol ID anchoring the graph-adjacency signal (mode=rank) or the blast-radius expansion (mode=impact)")),
			mcp.WithString("mode", mcp.Description("(bridge) rank (default) or impact")),
			mcp.WithString("role", mcp.Description("(list) Filter by role: provider or consumer")),
			mcp.WithNumber("limit", mcp.Description("(list) Max contracts per page (default: 200)")),
			mcp.WithString("cursor", mcp.Description("(list) Opaque pagination cursor from a previous `next_cursor` to fetch the next page.")),
			mcp.WithBoolean("paginate", mcp.Description("(list) When true, caps each page at the project default budget and returns `next_cursor` for any tail.")),
			mcp.WithNumber("max_bytes", mcp.Description("(list) Cap the marshaled response at this many bytes; the longest list is trimmed with truncation metadata.")),
			mcp.WithString("fields", mcp.Description("(list) Comma-separated list of fields to keep on each contract (e.g. \"type,role,id\").")),
			mcp.WithBoolean("compact", mcp.Description("One-line-per-contract text output")),
			mcp.WithString("format", mcp.Description("Output format: json (default), gcx (GCX1 compact wire format), or toon")),
			mcp.WithNumber("max_bytes", mcp.Description("Cap the marshaled response at this many bytes. The longest list is trimmed; truncation metadata rides on the response. Omit for no cap.")),
		),
		s.handleContracts,
	)

	// api_impact — fused pre-change report for an API route handler.
	s.addTool(
		mcp.NewTool("api_impact",
			mcp.WithDescription("Fused pre-change impact report for an API route — call this BEFORE modifying any route handler. Given a route path substring (or handler file substring), returns ONE report composing: the route's response shape, every consumer (same-repo and cross-repo via contract pairing) with the fields it accesses, field-level response-shape mismatches (real type-aware diffing, not regex), best-effort middleware, the execution flows (processes) it triggers, the true blast radius (affected callers + test files to run), and a fused risk level. Beats hand-assembling routes + contracts validate + impact: one call, one answer to \"what breaks if I change this endpoint?\"."),
			mcp.WithString("route", mcp.Description("Route path substring to match (e.g. /v1/users). At least one of route|file is required.")),
			mcp.WithString("file", mcp.Description("Handler file path substring to match — an alternative to route.")),
			mcp.WithString("repo", mcp.Description("Filter by repository prefix")),
			mcp.WithString("project", mcp.Description("Filter to repositories in a specific project")),
			mcp.WithString("ref", mcp.Description("Filter to repositories tagged with this ref")),
			mcp.WithString("format", mcp.Description("Output format: json (default), gcx (GCX1 compact wire format), or toon")),
			mcp.WithNumber("max_bytes", mcp.Description("Cap the marshaled response at this many bytes. The longest list is trimmed; truncation metadata rides on the response. Omit for no cap.")),
		),
		s.handleAPIImpact,
	)

	// feedback — unified feedback tool (record + query)
	s.addTool(
		mcp.NewTool("feedback",
			mcp.WithDescription("Agent learning feedback. action=record: report which symbols from smart_context/prefetch_context were useful, not_needed, or missing (improves future context). action=query: aggregated stats — most useful, most missed, accuracy."),
			mcp.WithString("action", mcp.Required(), mcp.Description("record or query")),
			mcp.WithString("task", mcp.Description("(record) The task description used in the original context call")),
			mcp.WithString("useful", mcp.Description("(record) Comma-separated symbol IDs that were useful")),
			mcp.WithString("not_needed", mcp.Description("(record) Comma-separated symbol IDs that were returned but not needed")),
			mcp.WithString("missing", mcp.Description("(record) Comma-separated symbol IDs that should have been included")),
			mcp.WithString("tool_source", mcp.Description("Which tool produced the context: smart_context or prefetch_context (default: smart_context). For query: filter by source or 'all'")),
			mcp.WithNumber("top_n", mcp.Description("(query) Number of top symbols to return per category (default: 10)")),
			mcp.WithBoolean("compact", mcp.Description("(query) One-line-per-symbol text output")),
			mcp.WithString("format", mcp.Description("(query) Output format: json (default), gcx (GCX1 compact wire format), or toon")),
		),
		s.handleFeedback,
	)

	// export_context
	s.addTool(
		mcp.NewTool("export_context",
			mcp.WithDescription("Generates a portable context briefing for a task as self-contained markdown or JSON. Use for sharing context outside MCP — paste into Slack, PRs, docs, or non-MCP AI tools."),
			mcp.WithString("task", mcp.Required(), mcp.Description("Natural language task description")),
			mcp.WithString("entry_point", mcp.Description("Optional symbol ID or file path to start from")),
			mcp.WithNumber("max_symbols", mcp.Description("Max symbols to include (default: 5)")),
			mcp.WithString("format", mcp.Description("Output format: markdown (default) or json")),
			mcp.WithNumber("token_budget", mcp.Description("Approximate token budget for output (default: 2000, max: 8000)")),
		),
		s.handleExportContext,
	)

	// audit_agent_config
	s.addTool(
		mcp.NewTool("audit_agent_config",
			mcp.WithDescription("Scans agent config files (CLAUDE.md, AGENTS.md, .cursor/rules, .github/copilot-instructions.md, etc.) for stale symbol references, dead file paths, and bloat — validated against the Gortex graph."),
			mcp.WithString("files", mcp.Description("Optional comma-separated file paths to audit (relative to repo root). If omitted, auto-discovers known agent config files.")),
			mcp.WithString("root", mcp.Description("Optional repo root override. Defaults to the indexer's root.")),
			mcp.WithBoolean("compact", mcp.Description("One-line-per-finding text output")),
		),
		s.handleAuditAgentConfig,
	)
}

// ---------------------------------------------------------------------------
// 10.2 handleVerifyChange
// ---------------------------------------------------------------------------

func (s *Server) handleVerifyChange(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	changesStr, err := req.RequireString("changes")
	if err != nil {
		return mcp.NewToolResultError("changes is required"), nil
	}

	var changes []analysis.SignatureChange
	if err := json.Unmarshal([]byte(changesStr), &changes); err != nil {
		return mcp.NewToolResultError("invalid changes JSON: " + err.Error()), nil
	}
	if len(changes) == 0 {
		return mcp.NewToolResultError("changes array is empty"), nil
	}

	result := analysis.VerifyChanges(s.readerFor(ctx), s.engineFor(ctx), changes)

	if isCompact(req) {
		var b strings.Builder
		for _, v := range result.Violations {
			fmt.Fprintf(&b, "%s %s %s:%d %s\n", v.Kind, v.SymbolID, v.FilePath, v.Line, v.Description)
		}
		// One line per changed function: how its call sites consume the
		// return value, so a return-signature change shows exactly which
		// sites bind / return / branch on the result.
		for _, ru := range result.ReturnUsage {
			fmt.Fprintf(&b, "return_usage %s call_sites:%d", ru.SymbolID, ru.CallSites)
			labels := make([]string, 0, len(ru.Counts))
			for label := range ru.Counts {
				labels = append(labels, label)
			}
			sort.Strings(labels)
			for _, label := range labels {
				fmt.Fprintf(&b, " %s:%d", label, ru.Counts[label])
			}
			if ru.Unclassified > 0 {
				fmt.Fprintf(&b, " unclassified:%d", ru.Unclassified)
			}
			b.WriteString("\n")
		}
		if result.Clean {
			fmt.Fprintf(&b, "clean: checked %d callers, %d implementors\n", result.CheckedCallers, result.CheckedImpls)
		}
		return mcp.NewToolResultText(b.String()), nil
	}

	return s.respondJSONOrTOON(ctx, req, result)
}

// ---------------------------------------------------------------------------
// 10.3 handleCheckGuards
// ---------------------------------------------------------------------------

func (s *Server) handleCheckGuards(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	idsStr, err := req.RequireString("ids")
	if err != nil {
		return mcp.NewToolResultError("ids is required"), nil
	}

	ids := strings.Split(idsStr, ",")
	for i := range ids {
		ids[i] = strings.TrimSpace(ids[i])
	}

	if !s.hasGuardRules(ids) && s.architecture.IsEmpty() {
		// Honor compact here too. This branch used to return JSON regardless,
		// so a compact caller (the Stop hook) got a raw payload under a
		// "Guard Violations" heading that had nothing to report.
		if isCompact(req) {
			return mcp.NewToolResultText("no guard rules configured\n"), nil
		}
		if s.isGCX(ctx, req) {
			return s.gcxResponseWithBudget(req)(encodeCheckGuards(nil, true))
		}
		empty := map[string]any{
			"violations": []any{},
			"message":    "no guard rules configured",
		}
		if s.isTOON(ctx, req) {
			return returnTOON(empty)
		}
		return s.respondJSONOrTOON(ctx, req, empty)
	}

	guardReader := s.readerFor(ctx)
	violations := s.evaluateGuards(guardReader, ids)
	violations = append(violations, analysis.EvaluateArchitecture(guardReader, s.architecture, ids)...)

	if isCompact(req) {
		var b strings.Builder
		for _, v := range violations {
			fmt.Fprintf(&b, "%s %s %s\n", v.Kind, v.RuleName, v.Description)
		}
		if len(violations) == 0 {
			b.WriteString("no guard rule violations\n")
		}
		return mcp.NewToolResultText(b.String()), nil
	}

	if s.isGCX(ctx, req) {
		return s.gcxResponseWithBudget(req)(encodeCheckGuards(violations, false))
	}

	result := map[string]any{
		"violations": violations,
		"total":      len(violations),
	}
	if s.isTOON(ctx, req) {
		return returnTOON(result)
	}
	return s.respondJSONOrTOON(ctx, req, result)
}

// ---------------------------------------------------------------------------
// 10.4 handlePrefetchContext
// ---------------------------------------------------------------------------

// prefetchCandidate holds a scored symbol for prefetch ranking.
type prefetchCandidate struct {
	Node            *graph.Node `json:"-"`
	ID              string      `json:"id"`
	Kind            string      `json:"kind"`
	FilePath        string      `json:"file_path"`
	StartLine       int         `json:"start_line"`
	Reason          string      `json:"reason"`
	Confidence      float64     `json:"confidence"`
	SearchRelevance float64     `json:"-"`
	GraphProximity  float64     `json:"-"`
	CommunityBonus  float64     `json:"-"`
	Source          string      `json:"source,omitempty"`
}

func (s *Server) handlePrefetchContext(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	task := req.GetString("task", "")
	recentStr := req.GetString("recent_symbols", "")
	includeSource := false
	if v, ok := req.GetArguments()["include_source"].(bool); ok {
		includeSource = v
	}

	// Gather recent symbols from parameter or session state.
	var recentIDs []string
	if recentStr != "" {
		for id := range strings.SplitSeq(recentStr, ",") {
			recentIDs = append(recentIDs, strings.TrimSpace(id))
		}
	}
	if len(recentIDs) == 0 {
		sess := s.sessionFor(ctx)
		sess.mu.Lock()
		recentIDs = append(recentIDs, sess.viewedSymbols...)
		sess.mu.Unlock()
	}

	if task == "" && len(recentIDs) == 0 {
		return mcp.NewToolResultError("insufficient context for prefetch: provide a task description or recent_symbols"), nil
	}

	// Score map: symbolID → scores
	type scores struct {
		search    float64
		proximity float64
		community float64
		feedback  float64
		reason    string
		node      *graph.Node
	}
	scoreMap := make(map[string]*scores)

	getOrCreate := func(n *graph.Node) *scores {
		if sc, ok := scoreMap[n.ID]; ok {
			return sc
		}
		sc := &scores{node: n}
		scoreMap[n.ID] = sc
		return sc
	}

	// 1. BM25 search on task description (weight 0.4)
	if task != "" {
		searchResults := s.scopedNodeSlice(ctx, s.engineFor(ctx).SearchSymbols(task, 30))
		maxScore := 1.0
		for i, n := range searchResults {
			if n.Kind == graph.KindFile || n.Kind == graph.KindImport {
				continue
			}
			sc := getOrCreate(n)
			// Decay score by rank position
			relevance := 1.0 / float64(i+1)
			if relevance > maxScore {
				maxScore = relevance
			}
			sc.search = relevance
			if sc.reason == "" {
				sc.reason = "matches task keyword"
			}
		}
		// Normalize search scores
		if maxScore > 0 {
			for _, sc := range scoreMap {
				sc.search = sc.search / maxScore
			}
		}
	}

	// 2. Graph proximity from recent symbols (weight 0.4)
	communities := s.getCommunities()
	recentCommSet := make(map[string]bool)

	for _, rid := range recentIDs {
		if communities != nil {
			if cid, ok := communities.NodeToComm[rid]; ok {
				recentCommSet[cid] = true
			}
		}
		// Get neighbors at depth 1-2
		sg := s.engineFor(ctx).GetDependencies(rid, query.QueryOptions{Depth: 2, Limit: 30, Detail: "brief"})
		for _, n := range sg.Nodes {
			if n.Kind == graph.KindFile || n.Kind == graph.KindImport {
				continue
			}
			sc := getOrCreate(n)
			// Closer = higher score
			proximity := 0.5 // depth 2
			// Check if depth 1
			for _, e := range sg.Edges {
				if (e.From == rid && e.To == n.ID) || (e.To == rid && e.From == n.ID) {
					proximity = 1.0
					break
				}
			}
			if proximity > sc.proximity {
				sc.proximity = proximity
				sc.reason = fmt.Sprintf("graph neighbor of %s", rid)
			}
		}
		// Also check dependents (callers)
		callers := s.engineFor(ctx).GetCallers(rid, query.QueryOptions{Depth: 1, Limit: 20, Detail: "brief"})
		for _, n := range callers.Nodes {
			if n.ID == rid || n.Kind == graph.KindFile || n.Kind == graph.KindImport {
				continue
			}
			sc := getOrCreate(n)
			if 1.0 > sc.proximity {
				sc.proximity = 1.0
				sc.reason = fmt.Sprintf("caller of %s", rid)
			}
		}
	}

	// 3. Community bonus (weight 0.2)
	if communities != nil && len(recentCommSet) > 0 {
		for _, sc := range scoreMap {
			if cid, ok := communities.NodeToComm[sc.node.ID]; ok {
				if recentCommSet[cid] {
					sc.community = 1.0
					if sc.reason == "" {
						sc.reason = "same community as recent activity"
					}
				}
			}
		}
	}

	// 4. Feedback signal (weight 0.15 when data exists, else use original 3-signal weights).
	hasFeedback := s.feedback != nil && s.feedback.HasData()
	if hasFeedback {
		for _, sc := range scoreMap {
			fbScore := s.feedback.GetSymbolScore(sc.node.ID)
			// Normalize from [-1, 1] to [0, 1].
			sc.feedback = (fbScore + 1.0) / 2.0
		}
	}

	// Compute combined scores and build candidates
	var candidates []prefetchCandidate
	for id, sc := range scoreMap {
		// Exclude recently viewed symbols themselves
		if slices.Contains(recentIDs, id) {
			continue
		}

		var combined float64
		if hasFeedback {
			combined = 0.35*sc.search + 0.35*sc.proximity + 0.15*sc.community + 0.15*sc.feedback
		} else {
			combined = 0.4*sc.search + 0.4*sc.proximity + 0.2*sc.community
		}
		if combined <= 0 {
			continue
		}
		// Clamp confidence to [0, 1]
		confidence := math.Min(combined, 1.0)

		candidates = append(candidates, prefetchCandidate{
			Node:            sc.node,
			ID:              id,
			Kind:            string(sc.node.Kind),
			FilePath:        sc.node.FilePath,
			StartLine:       sc.node.StartLine,
			Reason:          sc.reason,
			Confidence:      math.Round(confidence*1000) / 1000,
			SearchRelevance: sc.search,
			GraphProximity:  sc.proximity,
			CommunityBonus:  sc.community,
		})
	}

	// Sort by confidence descending
	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].Confidence > candidates[j].Confidence
	})

	// Default page size 10, capped at totalCount. The cursor opens the
	// rare "I want more than the top 10" path without making it the
	// default — agents that don't paginate get the same first page
	// they always got.
	totalCount := len(candidates)
	limit := req.GetInt("limit", 10)
	if limit <= 0 {
		limit = 10
	}
	offset := min(decodeCursor(req.GetString("cursor", "")), totalCount)
	endIdx := min(offset+limit, totalCount)
	candidates = candidates[offset:endIdx]
	truncated := endIdx < totalCount
	nextCursor := ""
	if truncated {
		nextCursor = encodeCursor(endIdx)
	}

	// Include source for top 5 if requested
	if includeSource {
		for i := range candidates {
			if i >= 5 {
				break
			}
			n := candidates[i].Node
			if n.StartLine > 0 && n.EndLine > 0 {
				if absPath, err := s.resolveNodePath(ctx, n); err == nil {
					if source, _, _, err := readLines(absPath, n.StartLine, n.EndLine, 0); err == nil {
						candidates[i].Source = source
					}
				}
			}
		}
	}

	if isCompact(req) {
		var b strings.Builder
		for _, c := range candidates {
			fmt.Fprintf(&b, "%s %s %s:%d %.3f %s\n", c.Kind, c.ID, c.FilePath, c.StartLine, c.Confidence, c.Reason)
		}
		if truncated {
			fmt.Fprintf(&b, "... truncated (%d total)\n", totalCount)
		}
		return mcp.NewToolResultText(b.String()), nil
	}

	if s.isGCX(ctx, req) {
		return s.gcxResponseWithBudget(req)(encodePrefetchContext(candidates, totalCount, truncated, includeSource))
	}

	result := map[string]any{
		"candidates": candidates,
		"total":      totalCount,
		"truncated":  truncated,
	}
	if nextCursor != "" {
		result["next_cursor"] = nextCursor
	}
	if s.isTOON(ctx, req) {
		return returnTOON(result)
	}
	return s.respondJSONOrTOON(ctx, req, result)
}

// ---------------------------------------------------------------------------
// handleAnalyze — unified dispatcher for graph analysis (replaces 4 tools)
// ---------------------------------------------------------------------------

func (s *Server) handleAnalyze(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	kind, err := req.RequireString("kind")
	if err != nil {
		return mcp.NewToolResultError("kind is required (one of: " + analyzeKindsCSV() + ")"), nil
	}
	// kind:"help" dumps the full per-kind reference (moved out of the tool
	// description to keep the cold schema lean). No graph needed.
	if strings.EqualFold(kind, "help") {
		return mcp.NewToolResultText(analyzeHelpResult()), nil
	}

	releaseAnalysis, admissionErr := s.acquireAnalyzeAdmission(ctx, kind)
	if admissionErr != nil {
		return mcp.NewToolResultError(admissionErr.Error()), nil
	}
	defer releaseAnalysis()

	// Uniform repo/project/workspace/scope narrowing, clamped to the
	// session workspace. A few kinds own one of the uniform arg names as
	// a kind-specific filter, so strip those from the scope-resolution
	// view (the handlers still read their own arg off the untouched req):
	//   - cycles owns `scope` as a path/package prefix — left in, it would
	//     be looked up as a saved scope and hard-error.
	//   - cross_repo owns `repo` as a boundary filter it reads directly.
	//   - images owns `ref` as the image reference (e.g. "ghcr.io/acme") —
	//     left in, resolveScope mis-reads it as a git ref / scope dimension
	//     and errors ("configuration manager is not available") or narrows
	//     to nothing.
	resolveReq := req
	switch kind {
	case "cycles":
		resolveReq = requestWithoutArgs(req, "scope")
	case "cross_repo":
		resolveReq = requestWithoutArgs(req, "repo")
	case "images":
		resolveReq = requestWithoutArgs(req, "ref")
	}
	resolved, errResult := s.resolveScope(ctx, resolveReq, IntentAnalyze)
	if errResult != nil {
		return errResult, nil
	}
	ctx = withRepoAllow(ctx, resolved.RepoAllow)

	// The dispatch switch is wrapped in a closure so every analyze
	// response is uniformly decorated with scope_applied below. It stays
	// inline in handleAnalyze on purpose — the AST anti-drift test
	// (TestAnalyzeKinds_MatchesSwitch) requires the kind switch to live
	// in this method's body.
	res, err := func() (*mcp.CallToolResult, error) {
		switch kind {
		case "dead_code":
			return s.handleFindDeadCode(ctx, req)
		case "hotspots":
			return s.handleFindHotspots(ctx, req)
		case "cycles":
			return s.handleFindCycles(ctx, req)
		case "would_create_cycle":
			return s.handleWouldCreateCycle(ctx, req)
		case "todos":
			return s.handleAnalyzeTodos(ctx, req)
		case "blame":
			return s.handleAnalyzeBlame(ctx, req)
		case "coverage":
			return s.handleAnalyzeCoverage(ctx, req)
		case "stale_code":
			return s.handleAnalyzeStaleCode(ctx, req)
		case "ownership":
			return s.handleAnalyzeOwnership(ctx, req)
		case "coverage_gaps":
			return s.handleAnalyzeCoverageGaps(ctx, req)
		case "stale_flags":
			return s.handleAnalyzeStaleFlags(ctx, req)
		case "doc_staleness":
			return s.handleAnalyzeDocStaleness(ctx, req)
		case "releases":
			return s.handleAnalyzeReleases(ctx, req)
		case "cgo_users":
			return s.handleAnalyzeInteropUsers(ctx, req, "uses_cgo", "cgo_users")
		case "wasm_users":
			return s.handleAnalyzeInteropUsers(ctx, req, "uses_wasm_bindgen", "wasm_users")
		case "orphan_tables":
			return s.handleAnalyzeOrphanTables(ctx, req)
		case "unreferenced_tables":
			return s.handleAnalyzeUnreferencedTables(ctx, req)
		case "coverage_summary":
			return s.handleAnalyzeCoverageSummary(ctx, req)
		case "channel_ops":
			return s.handleAnalyzeChannelOps(ctx, req)
		case "def_use":
			return s.handleAnalyzeDefUse(ctx, req)
		case "goroutine_spawns":
			return s.handleAnalyzeGoroutineSpawns(ctx, req)
		case "field_writers":
			return s.handleAnalyzeFieldWriters(ctx, req)
		case "indirect_mutations":
			return s.handleAnalyzeIndirectMutations(ctx, req)
		case "speculative":
			return s.handleAnalyzeSpeculative(ctx, req)
		case "ref_facts":
			return s.handleAnalyzeRefFacts(ctx, req)
		case "race_writes":
			return s.handleAnalyzeRaceWrites(ctx, req)
		case "unclosed_channels":
			return s.handleAnalyzeUnclosedChannels(ctx, req)
		case "unsafe_patterns":
			return s.handleAnalyzeUnsafePatterns(ctx, req)
		case "sast", "hygiene":
			return s.handleAnalyzeSAST(ctx, req, kind)
		case "review":
			return s.handleAnalyzeSAST(ctx, req, "review")
		case "domain":
			return s.handleAnalyzeSAST(ctx, req, "domain")
		case "health_score":
			return s.handleAnalyzeHealthScore(ctx, req)
		case "annotation_users":
			return s.handleAnalyzeAnnotationUsers(ctx, req)
		case "config_readers":
			return s.handleAnalyzeConfigReaders(ctx, req)
		case "env_var_users":
			return s.handleAnalyzeEnvVarUsers(ctx, req)
		case "sql_call_sites":
			return s.handleAnalyzeSQLCallSites(ctx, req)
		case "fixes_history":
			return s.handleAnalyzeFixesHistory(ctx, req)
		case "edge_audit":
			return s.handleAnalyzeEdgeAudit(ctx, req)
		case "event_emitters":
			return s.handleAnalyzeEventEmitters(ctx, req)
		case "pubsub":
			return s.handleAnalyzePubsub(ctx, req)
		case "string_emitters":
			return s.handleAnalyzeStringEmitters(ctx, req)
		case "error_surface":
			return s.handleAnalyzeErrorSurface(ctx, req)
		case "log_events":
			return s.handleAnalyzeLogEvents(ctx, req)
		case "sql_rebuild":
			return s.handleAnalyzeSQLRebuild(ctx, req)
		case "external_calls":
			return s.handleAnalyzeExternalCalls(ctx, req)
		case "synthesizers":
			return s.handleAnalyzeSynthesizers(ctx, req)
		case "temporal_orphans":
			return s.handleAnalyzeTemporalOrphans(ctx, req)
		case "resolution_outcomes":
			return s.handleAnalyzeResolutionOutcomes(ctx, req)
		case "temporal_verify":
			return s.handleAnalyzeTemporalVerify(ctx, req)
		case "retrieval_log":
			return s.handleAnalyzeRetrievalLog(ctx, req)
		case "routes":
			return s.handleAnalyzeRoutes(ctx, req)
		case "route_frameworks":
			return s.handleAnalyzeRouteFrameworks(ctx, req)
		case "drupal_hooks":
			return s.handleAnalyzeDrupalHooks(ctx, req)
		case "swiftui_views":
			return s.handleAnalyzeSwiftUIViews(ctx, req)
		case "uikit_classes":
			return s.handleAnalyzeUIKitClasses(ctx, req)
		case "models":
			return s.handleAnalyzeModels(ctx, req)
		case "components":
			return s.handleAnalyzeComponents(ctx, req)
		case "k8s_resources":
			return s.handleAnalyzeK8sResources(ctx, req)
		case "images":
			return s.handleAnalyzeImages(ctx, req)
		case "kustomize":
			return s.handleAnalyzeKustomize(ctx, req)
		case "cross_repo":
			return s.handleAnalyzeCrossRepo(ctx, req)
		case "dbt_models":
			return s.handleAnalyzeDbtModels(ctx, req)
		case "role":
			return s.handleAnalyzeRole(ctx, req)
		case "constructors_missing_fields":
			return s.handleAnalyzeConstructorsMissingFields(ctx, req)
		case "clusters":
			return s.handleAnalyzeClusters(ctx, req)
		case "suggest_boundaries":
			return s.handleSuggestBoundaries(ctx, req)
		case "concepts":
			return s.handleAnalyzeConcepts(ctx, req)
		case "impact":
			return s.handleAnalyzeImpactComposite(ctx, req)
		case "bottlenecks":
			return s.handleAnalyzeBottlenecks(ctx, req)
		case "named":
			return s.handleAnalyzeNamed(ctx, req)
		case "tests_as_edges":
			return s.handleAnalyzeTestsAsEdges(ctx, req)
		case "connectivity_health":
			return s.handleAnalyzeConnectivityHealth(ctx, req)
		case "pagerank":
			return s.handleAnalyzePageRank(ctx, req)
		case "louvain":
			return s.handleAnalyzeLouvain(ctx, req)
		case "wcc":
			return s.handleAnalyzeConnectedComponents(ctx, req, false)
		case "scc":
			return s.handleAnalyzeConnectedComponents(ctx, req, true)
		case "kcore":
			return s.handleAnalyzeKCore(ctx, req)
		default:
			return mcp.NewToolResultError("unknown analyze kind: " + kind + " (expected: " + analyzeKindsCSV() + ")"), nil
		}
	}()

	// Disclose when the caller asked to narrow (repo/project/scope) but
	// the chosen kind does not repo-narrow its rows in v1 — it enumerates
	// edges / scans files / mines git. scope_applied stays uniform and
	// truthful about the resolved scope; scope_note prevents it from
	// misleading a caller whose kind ignored the narrowing ("no silent
	// no-ops").
	if err == nil && res != nil && resolved.RepoAllow != nil &&
		!analyzeScopeAwareKinds[kind] && !analyzeWorkspaceClampedKinds[kind] {
		stampScopeNote(res, kind)
	}
	return withScopeResult(res, err, resolved)
}

// requestWithoutArgs returns a shallow copy of req with the named
// argument keys removed, so resolveScope doesn't mistake a kind-specific
// arg (cross_repo's `repo`, cycles' `scope`) for the uniform scope
// dimension. The original req is untouched.
func requestWithoutArgs(req mcp.CallToolRequest, keys ...string) mcp.CallToolRequest {
	src := req.GetArguments()
	dst := make(map[string]any, len(src))
	for k, v := range src {
		dst[k] = v
	}
	for _, k := range keys {
		delete(dst, k)
	}
	out := req
	out.Params.Arguments = dst
	return out
}

// stampScopeNote records on the response that the caller asked to narrow
// but the chosen kind does not repo-narrow its rows in v1, keeping
// scope_applied uniform while disclosing the no-op.
func stampScopeNote(res *mcp.CallToolResult, kind string) {
	if res == nil {
		return
	}
	note := "kind '" + kind + "' is not scope-narrowed in v1 (a community / git-mining / per-id / synthesizer kind); it reads the full graph directly, so results may span the entire index / all workspaces — not just the session workspace"
	if res.Meta == nil {
		res.Meta = mcp.NewMetaFromMap(map[string]any{"scope_note": note})
		return
	}
	if res.Meta.AdditionalFields == nil {
		res.Meta.AdditionalFields = map[string]any{}
	}
	res.Meta.AdditionalFields["scope_note"] = note
}

// analyzeNodeVisible reports whether a node may appear in an analyze
// result for the current request: inside the session workspace ceiling
// AND inside the optional ctx RepoAllow narrowing. It reuses the same
// gates as the scoped-node accessors so the bypass kinds that filter
// through it (dead_code, hotspots, cycles) match the AUTO kinds and
// simultaneously honour the workspace boundary they would otherwise
// ignore (those kinds read s.graph directly). Returns true for an
// unbound session with no RepoAllow, so an unconditional filter is a
// strict no-op in that case.
func (s *Server) analyzeNodeVisible(ctx context.Context, n *graph.Node) bool {
	if n == nil {
		return false
	}
	if !s.nodeInSessionScope(ctx, n) {
		return false
	}
	if allow := repoAllowFromContext(ctx); len(allow) > 0 && !allow[n.RepoPrefix] {
		return false
	}
	return true
}

// scopeFiltersActive reports whether the current request narrows analyze
// output below the global graph — a workspace-bound session or a ctx
// RepoAllow. When false, analyzeNodeVisible passes every node, so the
// Tier-2 filters skip the work and preserve byte-for-byte output.
func (s *Server) scopeFiltersActive(ctx context.Context) bool {
	if _, _, bound := s.sessionScope(ctx); bound {
		return true
	}
	return len(repoAllowFromContext(ctx)) > 0
}

// ---------------------------------------------------------------------------
// handleAnalyzeTodos — list KindTodo nodes with filters
// ---------------------------------------------------------------------------

// handleAnalyzeTodos enumerates the KindTodo nodes in the graph,
// optionally filtering by tag (TODO/FIXME/HACK/XXX/NOTE), assignee,
// or ticket. Designed for the cleanup-loop workflow: find every
// TODO assigned to me, every FIXME without a ticket, every TODO
// older than the v1.4 release, etc. The temporal filter is left
// for a v2 refinement that consumes git-blame enrichment.
//
// Returns one row per matching todo with file, line, tag,
// assignee, due, ticket, and the truncated text.
func (s *Server) handleAnalyzeTodos(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := req.GetArguments()
	tagFilter := strings.ToLower(strings.TrimSpace(stringArg(args, "tag")))
	assigneeFilter := strings.TrimSpace(stringArg(args, "assignee"))
	ticketFilter := strings.TrimSpace(stringArg(args, "ticket"))
	requireAssignee, _ := args["has_assignee"].(bool)

	type todoRow struct {
		ID       string `json:"id"`
		Tag      string `json:"tag"`
		File     string `json:"file"`
		Line     int    `json:"line"`
		Assignee string `json:"assignee,omitempty"`
		Due      string `json:"due,omitempty"`
		Ticket   string `json:"ticket,omitempty"`
		Text     string `json:"text,omitempty"`
	}

	var rows []todoRow
	// Push the kind filter into the storage layer — todos are a
	// tiny slice of the node table, so the AllNodes scan was the
	// dominant cost on a disk backend.
	for _, n := range s.scopedNodesByKinds(ctx, []graph.NodeKind{graph.KindTodo}) {
		tag, _ := n.Meta["tag"].(string)
		assignee, _ := n.Meta["assignee"].(string)
		ticket, _ := n.Meta["ticket"].(string)
		due, _ := n.Meta["due"].(string)
		text, _ := n.Meta["text"].(string)

		if tagFilter != "" && strings.ToLower(tag) != tagFilter {
			continue
		}
		if assigneeFilter != "" && assignee != assigneeFilter {
			continue
		}
		if ticketFilter != "" && ticket != ticketFilter {
			continue
		}
		if requireAssignee && assignee == "" {
			continue
		}
		rows = append(rows, todoRow{
			ID:       n.ID,
			Tag:      tag,
			File:     n.FilePath,
			Line:     n.StartLine,
			Assignee: assignee,
			Due:      due,
			Ticket:   ticket,
			Text:     text,
		})
	}
	// Stable order: file then line. Predictable diffs across calls
	// matter for cleanup workflows that compare results over time.
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].File != rows[j].File {
			return rows[i].File < rows[j].File
		}
		return rows[i].Line < rows[j].Line
	})

	if isCompact(req) {
		var b strings.Builder
		for _, r := range rows {
			fmt.Fprintf(&b, "%s %s:%d", r.Tag, r.File, r.Line)
			if r.Assignee != "" {
				fmt.Fprintf(&b, " @%s", r.Assignee)
			}
			if r.Ticket != "" {
				fmt.Fprintf(&b, " %s", r.Ticket)
			}
			if r.Text != "" {
				fmt.Fprintf(&b, " — %s", r.Text)
			}
			b.WriteByte('\n')
		}
		if len(rows) == 0 {
			b.WriteString("no todos matched\n")
		}
		return mcp.NewToolResultText(b.String()), nil
	}

	return s.respondJSONOrTOON(ctx, req, map[string]any{
		"todos": rows,
		"total": len(rows),
	})
}

// stringArg returns args[key] as a trimmed string, or "" when the
// key is missing or the value isn't a string.
func stringArg(args map[string]any, key string) string {
	if v, ok := args[key].(string); ok {
		return v
	}
	return ""
}

// handleAnalyzeCoverage parses a Go cover profile and stamps
// meta.coverage_pct + meta.coverage on every executable symbol it
// can map to a profile segment by line range. Requires a `profile`
// argument with the path to the cover.out file (relative paths
// resolve against the indexed repo root).
//
// Re-runnable: each call re-reads the profile and overwrites
// existing meta — the desired behaviour after a fresh test run.
func (s *Server) handleAnalyzeCoverage(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	profileArg := stringArg(req.GetArguments(), "profile")
	if profileArg == "" {
		return mcp.NewToolResultError("coverage enrichment requires a `profile` argument with the cover.out path"), nil
	}
	if s.indexer == nil {
		return mcp.NewToolResultError("coverage enrichment requires an active indexer"), nil
	}
	root := s.indexer.RootPath()
	if !filepath.IsAbs(profileArg) {
		profileArg = filepath.Join(root, profileArg)
	}
	segments, err := coverage.ParseFile(profileArg)
	if err != nil {
		return mcp.NewToolResultError("read profile: " + err.Error()), nil
	}
	modulePath := coverage.ReadModulePath(root)
	count := coverage.EnrichGraph(s.graph, segments, modulePath)
	return s.respondJSONOrTOON(ctx, req, map[string]any{
		"enriched":    count,
		"segments":    len(segments),
		"profile":     profileArg,
		"module_path": modulePath,
	})
}

// handleAnalyzeStaleCode lists symbols whose meta.last_authored is
// older than the threshold. Requires that blame enrichment has
// already run (either through analyze kind=blame or `gortex enrich
// blame`); symbols without authorship metadata are silently
// skipped — they're either unenriched or hand-authored without git
// history (test fixtures, generated code), and lumping them in
// with "unchanged for ages" would be a lie.
//
// Filters:
//
//   - older_than: days, default 365. Symbols with a last-author
//     timestamp older than now - older_than days are included.
//   - email: exact author email match — useful for "find code
//     authored by someone who has left the team."
//   - kinds: comma-separated list, default function,method. Pass
//     "all" to include every blame-eligible kind.
//
// Sorted oldest-first so the cleanup loop sees the staleness
// gradient at a glance.
func (s *Server) handleAnalyzeStaleCode(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := req.GetArguments()
	olderThanDays := 365.0
	if v, ok := args["older_than"].(float64); ok && v > 0 {
		olderThanDays = v
	}
	emailFilter := strings.TrimSpace(stringArg(args, "email"))

	allowedKinds := map[graph.NodeKind]struct{}{
		graph.KindFunction: {},
		graph.KindMethod:   {},
	}
	if k := strings.TrimSpace(stringArg(args, "kinds")); k != "" {
		allowedKinds = parseAnalyzeKindsFilter(k)
	}

	cutoffSec := time.Now().Add(-time.Duration(olderThanDays*24) * time.Hour).Unix()

	type staleRow struct {
		ID        string `json:"id"`
		File      string `json:"file"`
		Line      int    `json:"line"`
		Email     string `json:"email"`
		Commit    string `json:"commit"`
		Timestamp int64  `json:"timestamp"`
		AgeDays   int    `json:"age_days"`
	}
	var rows []staleRow
	// Push the kind filter into the storage layer; the meta gate
	// (last_authored.timestamp) stays in Go since the meta column is
	// opaque to the query layer.
	blame := blameRowsByID(s.readerFor(ctx))
	for _, n := range s.scopedNodesByKinds(ctx, allowedKindsSlice(allowedKinds)) {
		la, ok := lastAuthoredFrom(blame, n)
		if !ok || la.Timestamp == 0 {
			continue
		}
		ts := la.Timestamp
		if ts > cutoffSec {
			continue
		}
		email := la.Email
		if emailFilter != "" && email != emailFilter {
			continue
		}
		commit := la.Commit
		ageSec := time.Now().Unix() - ts
		rows = append(rows, staleRow{
			ID:        n.ID,
			File:      n.FilePath,
			Line:      n.StartLine,
			Email:     email,
			Commit:    commit,
			Timestamp: ts,
			AgeDays:   int(ageSec / (24 * 3600)),
		})
	}
	sort.Slice(rows, func(i, j int) bool {
		return rows[i].Timestamp < rows[j].Timestamp
	})

	if isCompact(req) {
		var b strings.Builder
		for _, r := range rows {
			fmt.Fprintf(&b, "%dd %s:%d", r.AgeDays, r.File, r.Line)
			if r.Email != "" {
				fmt.Fprintf(&b, " @%s", r.Email)
			}
			fmt.Fprintf(&b, " %s\n", r.ID)
		}
		if len(rows) == 0 {
			b.WriteString("no stale code matched\n")
		}
		return mcp.NewToolResultText(b.String()), nil
	}
	return s.respondJSONOrTOON(ctx, req, map[string]any{
		"stale":          rows,
		"total":          len(rows),
		"older_than_day": olderThanDays,
	})
}

// allowedKindsSlice returns the keys of an analyzer's allowedKinds
// set so the caller can hand them to scopedNodesByKinds. Kept as a
// helper rather than inlined at every call site so the order is
// deterministic — not load-bearing for correctness (the capability
// dedupes), but it keeps test expectations stable when the IN list
// is logged.
func allowedKindsSlice(allowed map[graph.NodeKind]struct{}) []graph.NodeKind {
	out := make([]graph.NodeKind, 0, len(allowed))
	for k := range allowed {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

// parseAnalyzeKindsFilter parses a comma-separated kinds argument
// into the set used by handleAnalyzeStaleCode. The literal "all"
// returns the broadest blame-eligible kind set so callers can drop
// the default function/method scope when they want types and
// fields included too.
func parseAnalyzeKindsFilter(arg string) map[graph.NodeKind]struct{} {
	out := map[graph.NodeKind]struct{}{}
	for k := range strings.SplitSeq(arg, ",") {
		k = strings.TrimSpace(strings.ToLower(k))
		if k == "" {
			continue
		}
		if k == "all" {
			return map[graph.NodeKind]struct{}{
				graph.KindFunction:   {},
				graph.KindMethod:     {},
				graph.KindType:       {},
				graph.KindInterface:  {},
				graph.KindField:      {},
				graph.KindVariable:   {},
				graph.KindConstant:   {},
				graph.KindEnumMember: {},
			}
		}
		out[graph.NodeKind(k)] = struct{}{}
	}
	return out
}

// handleAnalyzeOwnership groups blame metadata by author email and
// returns one row per author with the symbol count, files
// touched, and the oldest/newest last-authored timestamp seen.
// Requires a blame-enriched graph (analyze kind=blame or `gortex
// enrich blame`) — symbols without authorship metadata are
// silently skipped, same as handleAnalyzeStaleCode.
//
// Filters:
//
//   - min_symbols: drop authors below this symbol count (default 1).
//     Useful for excluding drive-by contributions on large repos.
//   - kinds: comma-separated kind list, default function,method.
//     Pass "all" to include every blame-eligible kind.
//   - path_prefix: scope to nodes under this file-path prefix —
//     e.g. "internal/auth/" to ask "who owns the auth package".
//
// Sorted descending by symbol count so the top owners appear
// first. The combination (path_prefix + min_symbols + sorted
// output) is the cleanup-loop's "who do I ping for review on
// this area" query without needing a CODEOWNERS file.
func (s *Server) handleAnalyzeOwnership(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := req.GetArguments()
	minSymbols := 1
	if v, ok := args["min_symbols"].(float64); ok && v > 0 {
		minSymbols = int(v)
	}
	pathPrefix := strings.TrimSpace(stringArg(args, "path_prefix"))

	allowedKinds := map[graph.NodeKind]struct{}{
		graph.KindFunction: {},
		graph.KindMethod:   {},
	}
	if k := strings.TrimSpace(stringArg(args, "kinds")); k != "" {
		allowedKinds = parseAnalyzeKindsFilter(k)
	}

	type ownerStats struct {
		Email    string `json:"email"`
		Symbols  int    `json:"symbols"`
		Files    int    `json:"files"`
		OldestTS int64  `json:"oldest_timestamp"`
		NewestTS int64  `json:"newest_timestamp"`
		fileSet  map[string]struct{}
	}
	byEmail := map[string]*ownerStats{}

	// Kind pushdown — owners are derived from the blame meta on
	// function/method (or wider) nodes; the analyzer scans tens of
	// thousands of irrelevant nodes without it on a disk backend.
	ownBlame := blameRowsByID(s.readerFor(ctx))
	for _, n := range s.scopedNodesByKinds(ctx, allowedKindsSlice(allowedKinds)) {
		if !graphpath.HasPrefix(n.FilePath, pathPrefix) {
			continue
		}
		la, ok := lastAuthoredFrom(ownBlame, n)
		if !ok {
			continue
		}
		email := la.Email
		if email == "" {
			continue
		}
		ts := la.Timestamp
		if ts == 0 {
			continue
		}
		stats, ok := byEmail[email]
		if !ok {
			stats = &ownerStats{
				Email:    email,
				OldestTS: ts,
				NewestTS: ts,
				fileSet:  map[string]struct{}{},
			}
			byEmail[email] = stats
		}
		stats.Symbols++
		stats.fileSet[n.FilePath] = struct{}{}
		if ts < stats.OldestTS {
			stats.OldestTS = ts
		}
		if ts > stats.NewestTS {
			stats.NewestTS = ts
		}
	}

	rows := make([]*ownerStats, 0, len(byEmail))
	for _, s := range byEmail {
		s.Files = len(s.fileSet)
		s.fileSet = nil // hide from JSON output
		if s.Symbols < minSymbols {
			continue
		}
		rows = append(rows, s)
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Symbols != rows[j].Symbols {
			return rows[i].Symbols > rows[j].Symbols
		}
		return rows[i].Email < rows[j].Email
	})

	if isCompact(req) {
		var b strings.Builder
		for _, r := range rows {
			fmt.Fprintf(&b, "%-5d %-3d %s\n", r.Symbols, r.Files, r.Email)
		}
		if len(rows) == 0 {
			b.WriteString("no owners matched\n")
		}
		return mcp.NewToolResultText(b.String()), nil
	}
	return s.respondJSONOrTOON(ctx, req, map[string]any{
		"owners": rows,
		"total":  len(rows),
	})
}

// tsFromMeta normalises the timestamp field across the int64
// (in-process enrichment) and float64 (gob-decoded snapshot)
// shapes. Returns 0 when the value is missing or the wrong type
// — callers treat 0 as "skip this node" since blame timestamps
// are always positive.
func tsFromMeta(raw any) int64 {
	switch v := raw.(type) {
	case int64:
		return v
	case float64:
		return int64(v)
	}
	return 0
}

// handleAnalyzeCoverageGaps lists symbols whose meta.coverage_pct
// falls inside [min_pct, max_pct) — the half-open interval lets
// "everything below 50%" be expressed as max_pct=50 without
// dragging in fully-uncovered nodes that callers might want to
// distinguish via a separate query. Requires a coverage-enriched
// graph (analyze kind=coverage or `gortex enrich coverage`).
//
// Symbols without coverage_pct are silently skipped — a node
// could be unmeasured because the profile didn't cover it (real
// gap) or because it's a non-executable kind (no signal at all).
// Lumping the two together would be misleading, and the
// distinction lives in the static dead_code analyzer rather than
// here.
//
// Filters:
//
//   - max_pct: upper exclusive bound (default 100 — i.e. anything
//     not fully covered).
//   - min_pct: lower inclusive bound (default 0 — i.e. include
//     fully-uncovered too). Combine with max_pct to scope to a
//     coverage band: "20-50% coverage" is min_pct=20 max_pct=50.
//   - kinds: same shared kind filter as stale_code/ownership;
//     default function/method.
//   - path_prefix: scope to a directory subtree.
//
// Sorted ascending by coverage_pct so the most-undertested
// symbols surface first.
func (s *Server) handleAnalyzeCoverageGaps(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := req.GetArguments()
	maxPct := 100.0
	if v, ok := args["max_pct"].(float64); ok && v > 0 {
		maxPct = v
	}
	minPct := 0.0
	if v, ok := args["min_pct"].(float64); ok && v >= 0 {
		minPct = v
	}
	pathPrefix := strings.TrimSpace(stringArg(args, "path_prefix"))

	allowedKinds := map[graph.NodeKind]struct{}{
		graph.KindFunction: {},
		graph.KindMethod:   {},
	}
	if k := strings.TrimSpace(stringArg(args, "kinds")); k != "" {
		allowedKinds = parseAnalyzeKindsFilter(k)
	}

	type gapRow struct {
		ID      string  `json:"id"`
		File    string  `json:"file"`
		Line    int     `json:"line"`
		Pct     float64 `json:"coverage_pct"`
		NumStmt int     `json:"num_stmt"`
		Hit     int     `json:"hit"`
	}
	var rows []gapRow
	covRows := coverageRowsByID(s.readerFor(ctx))
	// Kind pushdown — coverage_pct only ever lands on executable
	// kinds, so the IN-list IS the candidate set.
	for _, n := range s.scopedNodesByKinds(ctx, allowedKindsSlice(allowedKinds)) {
		if !graphpath.HasPrefix(n.FilePath, pathPrefix) {
			continue
		}
		pct, ok := coveragePctFrom(covRows, n)
		if !ok {
			continue
		}
		if pct < minPct || pct >= maxPct {
			continue
		}
		row := gapRow{
			ID:   n.ID,
			File: n.FilePath,
			Line: n.StartLine,
			Pct:  pct,
		}
		if e, ok := covRows[n.ID]; ok {
			row.NumStmt = e.NumStmt
			row.Hit = e.Hit
		} else if cov, ok := n.Meta["coverage"].(map[string]any); ok {
			if v, ok := cov["num_stmt"].(int); ok {
				row.NumStmt = v
			} else if f, ok := cov["num_stmt"].(float64); ok {
				row.NumStmt = int(f)
			}
			if v, ok := cov["hit"].(int); ok {
				row.Hit = v
			} else if f, ok := cov["hit"].(float64); ok {
				row.Hit = int(f)
			}
		}
		rows = append(rows, row)
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Pct != rows[j].Pct {
			return rows[i].Pct < rows[j].Pct
		}
		// Tie-break by symbol size — bigger gaps surface above
		// smaller ones at the same percentage. NumStmt may be 0
		// when meta.coverage didn't decode cleanly; the secondary
		// fallback to file:line keeps the order stable.
		if rows[i].NumStmt != rows[j].NumStmt {
			return rows[i].NumStmt > rows[j].NumStmt
		}
		if rows[i].File != rows[j].File {
			return rows[i].File < rows[j].File
		}
		return rows[i].Line < rows[j].Line
	})

	if isCompact(req) {
		var b strings.Builder
		for _, r := range rows {
			fmt.Fprintf(&b, "%5.1f%% %s:%d  %s\n", r.Pct, r.File, r.Line, r.ID)
		}
		if len(rows) == 0 {
			b.WriteString("no coverage gaps matched\n")
		}
		return mcp.NewToolResultText(b.String()), nil
	}
	return s.respondJSONOrTOON(ctx, req, map[string]any{
		"gaps":    rows,
		"total":   len(rows),
		"min_pct": minPct,
		"max_pct": maxPct,
	})
}

// handleAnalyzeStaleFlags lists feature flags whose every toggling
// call site was last touched more than `older_than` days ago. The
// staleness signal is derived: for each KindFlag node we walk its
// incoming EdgeTogglesFlag edges, look up each caller's
// meta.last_authored.timestamp, and take the maximum. If even the
// most-recently-touched check site is older than the cutoff, the
// flag is stale — every check is in code nobody's edited in a
// while, which is the operational signal that the rollout is
// done.
//
// Requires both flag detection (analyze kind=blame is enough to
// populate KindFlag nodes if the repo enables index.coverage.flags)
// AND blame enrichment (analyze kind=blame). Flags whose callers
// don't have blame metadata are silently skipped — without
// authorship data we can't compute the staleness — and reported
// in the response's `unscored` count so the agent can tell the
// difference between "no flags found" and "flags found but
// unscored."
//
// Filters:
//
//   - older_than: days, default 365.
//   - provider: filter to a single provider (launchdarkly,
//     growthbook, unleash, internal).
//
// Sorted oldest-first so cleanup priorities surface at the top.
func (s *Server) handleAnalyzeStaleFlags(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := req.GetArguments()
	olderThanDays := 365.0
	if v, ok := args["older_than"].(float64); ok && v > 0 {
		olderThanDays = v
	}
	providerFilter := strings.TrimSpace(stringArg(args, "provider"))
	cutoffSec := time.Now().Add(-time.Duration(olderThanDays*24) * time.Hour).Unix()

	type staleFlag struct {
		ID           string `json:"id"`
		Name         string `json:"name"`
		Provider     string `json:"provider"`
		Callers      int    `json:"callers"`
		NewestCallTS int64  `json:"newest_call_timestamp"`
		AgeDays      int    `json:"age_days"`
	}
	var rows []staleFlag
	unscored := 0

	// Kind pushdown — KindFlag is a few hundred nodes max even on
	// the biggest workspaces, so pulling AllNodes() to find them
	// was pure overhead. The caller batch below still does per-
	// flag GetInEdges; pushing that into a single query join is a
	// separate follow-up since the join semantics differ per flag.
	reader := s.readerFor(ctx)
	flagBlame := blameRowsByID(reader)
	for _, n := range s.scopedNodesByKinds(ctx, []graph.NodeKind{graph.KindFlag}) {
		provider, _ := n.Meta["provider"].(string)
		if providerFilter != "" && provider != providerFilter {
			continue
		}
		// Walk incoming EdgeTogglesFlag edges to collect callers.
		var callerIDs []string
		for _, e := range reader.GetInEdges(n.ID) {
			if e.Kind != graph.EdgeTogglesFlag {
				continue
			}
			callerIDs = append(callerIDs, e.From)
		}
		if len(callerIDs) == 0 {
			// Orphan flag — declared but never checked. Treat as
			// stale: a flag with zero call sites is tautologically
			// safe to delete.
			rows = append(rows, staleFlag{
				ID:       n.ID,
				Name:     stringFromMeta(n.Meta, "name"),
				Provider: provider,
				Callers:  0,
				AgeDays:  -1,
			})
			continue
		}
		var newestTS int64
		hasBlame := false
		for _, callerID := range callerIDs {
			caller := reader.GetNode(callerID)
			if caller == nil {
				continue
			}
			la, ok := lastAuthoredFrom(flagBlame, caller)
			if !ok {
				continue
			}
			ts := la.Timestamp
			if ts == 0 {
				continue
			}
			hasBlame = true
			if ts > newestTS {
				newestTS = ts
			}
		}
		if !hasBlame {
			unscored++
			continue
		}
		if newestTS > cutoffSec {
			continue // some caller is fresh
		}
		ageSec := time.Now().Unix() - newestTS
		rows = append(rows, staleFlag{
			ID:           n.ID,
			Name:         stringFromMeta(n.Meta, "name"),
			Provider:     provider,
			Callers:      len(callerIDs),
			NewestCallTS: newestTS,
			AgeDays:      int(ageSec / (24 * 3600)),
		})
	}

	sort.Slice(rows, func(i, j int) bool {
		// Orphans (AgeDays = -1) first, then oldest by timestamp.
		if rows[i].AgeDays < 0 && rows[j].AgeDays >= 0 {
			return true
		}
		if rows[j].AgeDays < 0 && rows[i].AgeDays >= 0 {
			return false
		}
		return rows[i].NewestCallTS < rows[j].NewestCallTS
	})

	if isCompact(req) {
		var b strings.Builder
		for _, r := range rows {
			if r.AgeDays < 0 {
				fmt.Fprintf(&b, "ORPHAN  %s (%s)\n", r.Name, r.Provider)
				continue
			}
			fmt.Fprintf(&b, "%4dd  %s (%s) — %d callers\n", r.AgeDays, r.Name, r.Provider, r.Callers)
		}
		if len(rows) == 0 {
			b.WriteString("no stale flags matched\n")
		}
		return mcp.NewToolResultText(b.String()), nil
	}
	return s.respondJSONOrTOON(ctx, req, map[string]any{
		"flags":          rows,
		"total":          len(rows),
		"unscored":       unscored,
		"older_than_day": olderThanDays,
	})
}

// stringFromMeta is a tiny helper for safe meta string extraction.
func stringFromMeta(meta map[string]any, key string) string {
	if v, ok := meta[key].(string); ok {
		return v
	}
	return ""
}

// handleAnalyzeOrphanTables lists tables that are referenced by
// at least one EdgeQueries call site but have no incoming
// EdgeProvides from a migration. Combines the two SQL extraction
// paths (query-string detection + migration-file declaration)
// into a single signal: tables likely missing a migration, or
// pointing at an external/legacy schema the agent should flag.
//
// Returns one row per orphan with the canonical id, table name,
// schema, dialect, and the count of EdgeQueries call sites
// pointing at it. Sorted by query count descending so the most-
// used orphans surface first — those are the highest-priority
// "we should declare this" candidates.
//
// Tables reachable via both EdgeProvides AND EdgeQueries are not
// orphans by definition. Tables with no EdgeQueries either (pure
// declaration with no users) aren't included either — they're
// the inverse problem ("orphan migration") which is a separate
// future analyzer.
func (s *Server) handleAnalyzeOrphanTables(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	type orphanRow struct {
		ID         string `json:"id"`
		Table      string `json:"table"`
		Schema     string `json:"schema,omitempty"`
		Dialect    string `json:"dialect"`
		QueryCount int    `json:"query_count"`
	}
	var rows []orphanRow
	tableCount, queryEdges := 0, 0
	reader := s.readerFor(ctx)
	// Kind pushdown — only KindTable carries the providers/queries
	// fan-in we care about; the rest of the node table is noise.
	for _, n := range s.scopedNodesByKinds(ctx, []graph.NodeKind{graph.KindTable}) {
		tableCount++
		// Walk incoming edges to detect both providers (migrations)
		// and consumers (query call sites).
		hasProvider := false
		queryCount := 0
		for _, e := range reader.GetInEdges(n.ID) {
			switch e.Kind {
			case graph.EdgeProvides:
				hasProvider = true
			case graph.EdgeQueries:
				queryCount++
			}
		}
		queryEdges += queryCount
		if hasProvider {
			continue
		}
		if queryCount == 0 {
			continue
		}
		dialect, _ := n.Meta["dialect"].(string)
		schema, _ := n.Meta["schema"].(string)
		table, _ := n.Meta["table"].(string)
		if table == "" {
			table = n.Name
		}
		rows = append(rows, orphanRow{
			ID:         n.ID,
			Table:      table,
			Schema:     schema,
			Dialect:    dialect,
			QueryCount: queryCount,
		})
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].QueryCount != rows[j].QueryCount {
			return rows[i].QueryCount > rows[j].QueryCount
		}
		return rows[i].ID < rows[j].ID
	})

	note := vacuousTableAnalysisNote(tableCount, queryEdges)
	if isCompact(req) {
		var b strings.Builder
		for _, r := range rows {
			fmt.Fprintf(&b, "%-3d  %s\n", r.QueryCount, r.ID)
		}
		if len(rows) == 0 {
			if note != "" {
				fmt.Fprintf(&b, "%s\n", note)
			} else {
				b.WriteString("no orphan tables\n")
			}
		}
		return mcp.NewToolResultText(b.String()), nil
	}
	out := map[string]any{
		"orphans":     rows,
		"total":       len(rows),
		"tables":      tableCount,
		"query_edges": queryEdges,
	}
	if note != "" {
		out["note"] = note
	}
	return s.respondJSONOrTOON(ctx, req, out)
}

// vacuousTableAnalysisNote returns the caveat to attach when a
// table analyzer's empty result carries no information.
//
// orphan_tables and unreferenced_tables both cross-reference migration
// providers against query call sites. When the graph holds no
// EdgeQueries at all — SQL query extraction is gated off, or the code
// side builds its queries in a shape the extractor does not see — each
// returns a number that reads like an all-clear but was never computed
// against anything. Saying so is the difference between "nothing to
// clean up" and "this layer is empty".
func vacuousTableAnalysisNote(tableCount, queryEdges int) string {
	switch {
	case tableCount == 0:
		return "tables: 0 — no table nodes in scope; result is not meaningful"
	case queryEdges == 0:
		return "query_edges: 0 — no SQL query edges in scope, so provider/consumer " +
			"cross-referencing had nothing to compare; result is not meaningful"
	}
	return ""
}

// handleAnalyzeUnreferencedTables is the inverse of
// orphan_tables: tables that have an incoming EdgeProvides from
// a migration but zero EdgeQueries call sites. Useful for
// "which migrations created tables we don't read or write" —
// dead schema candidates, cleanup signals after a feature
// removal, or tables that exist only for downstream replication.
//
// Returns one row per unreferenced table with the canonical id,
// table name, schema, dialect, and the count of providers
// (typically 1, but a table can appear in multiple migrations).
// Sorted alphabetically by id for diff-able output — there's no
// natural priority ordering for this list the way query_count
// gives orphan_tables.
func (s *Server) handleAnalyzeUnreferencedTables(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	type unrefRow struct {
		ID            string `json:"id"`
		Table         string `json:"table"`
		Schema        string `json:"schema,omitempty"`
		Dialect       string `json:"dialect"`
		ProviderCount int    `json:"provider_count"`
	}
	var rows []unrefRow
	tableCount, queryEdges := 0, 0
	reader := s.readerFor(ctx)
	// Kind pushdown — same story as orphan_tables.
	for _, n := range s.scopedNodesByKinds(ctx, []graph.NodeKind{graph.KindTable}) {
		tableCount++
		providerCount := 0
		queryCount := 0
		for _, e := range reader.GetInEdges(n.ID) {
			switch e.Kind {
			case graph.EdgeProvides:
				providerCount++
			case graph.EdgeQueries:
				queryCount++
			}
		}
		queryEdges += queryCount
		if providerCount == 0 || queryCount > 0 {
			continue
		}
		dialect, _ := n.Meta["dialect"].(string)
		schema, _ := n.Meta["schema"].(string)
		table, _ := n.Meta["table"].(string)
		if table == "" {
			table = n.Name
		}
		rows = append(rows, unrefRow{
			ID:            n.ID,
			Table:         table,
			Schema:        schema,
			Dialect:       dialect,
			ProviderCount: providerCount,
		})
	}
	sort.Slice(rows, func(i, j int) bool {
		return rows[i].ID < rows[j].ID
	})

	note := vacuousTableAnalysisNote(tableCount, queryEdges)
	if isCompact(req) {
		var b strings.Builder
		for _, r := range rows {
			fmt.Fprintln(&b, r.ID)
		}
		if len(rows) == 0 && note == "" {
			b.WriteString("no unreferenced tables\n")
		}
		if note != "" {
			fmt.Fprintf(&b, "%s\n", note)
		}
		return mcp.NewToolResultText(b.String()), nil
	}
	out := map[string]any{
		"unreferenced": rows,
		"total":        len(rows),
		"tables":       tableCount,
		"query_edges":  queryEdges,
	}
	if note != "" {
		out["note"] = note
	}
	return s.respondJSONOrTOON(ctx, req, out)
}

// handleAnalyzeCoverageSummary aggregates meta.coverage_pct per
// directory. Complements coverage_gaps (per-symbol view) with a
// package-level rollup useful for cleanup planning ("which
// directory needs the most test attention"). Each row carries
// the directory path, total measured symbols, average coverage,
// fully-covered count, fully-uncovered count, and partial count
// — the breakdown helps distinguish "package needs more
// branches tested" from "package has no tests at all".
//
// Sorted ascending by avg_pct so worst packages surface first.
// Filters mirror coverage_gaps: kinds (default function/method),
// path_prefix scoping.
func (s *Server) handleAnalyzeCoverageSummary(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := req.GetArguments()
	pathPrefix := strings.TrimSpace(stringArg(args, "path_prefix"))

	allowedKinds := map[graph.NodeKind]struct{}{
		graph.KindFunction: {},
		graph.KindMethod:   {},
	}
	if k := strings.TrimSpace(stringArg(args, "kinds")); k != "" {
		allowedKinds = parseAnalyzeKindsFilter(k)
	}

	type dirStats struct {
		Dir       string  `json:"dir"`
		Symbols   int     `json:"symbols"`
		AvgPct    float64 `json:"avg_pct"`
		Covered   int     `json:"covered"`
		Partial   int     `json:"partial"`
		Uncovered int     `json:"uncovered"`

		sumPct float64 // running sum, hidden from JSON
	}
	byDir := map[string]*dirStats{}
	covRows := coverageRowsByID(s.readerFor(ctx))

	// Kind pushdown — coverage_pct only lives on executable kinds.
	for _, n := range s.scopedNodesByKinds(ctx, allowedKindsSlice(allowedKinds)) {
		if !graphpath.HasPrefix(n.FilePath, pathPrefix) {
			continue
		}
		pct, ok := coveragePctFrom(covRows, n)
		if !ok {
			continue
		}
		dir := filepath.Dir(n.FilePath)
		ds, ok := byDir[dir]
		if !ok {
			ds = &dirStats{Dir: dir}
			byDir[dir] = ds
		}
		ds.Symbols++
		ds.sumPct += pct
		switch {
		case pct >= 100:
			ds.Covered++
		case pct == 0:
			ds.Uncovered++
		default:
			ds.Partial++
		}
	}

	rows := make([]*dirStats, 0, len(byDir))
	for _, s := range byDir {
		if s.Symbols == 0 {
			continue
		}
		s.AvgPct = roundTwoDecimal(s.sumPct / float64(s.Symbols))
		s.sumPct = 0
		rows = append(rows, s)
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].AvgPct != rows[j].AvgPct {
			return rows[i].AvgPct < rows[j].AvgPct
		}
		return rows[i].Dir < rows[j].Dir
	})

	if isCompact(req) {
		var b strings.Builder
		for _, r := range rows {
			fmt.Fprintf(&b, "%5.1f%% %3d sym (%d cov / %d part / %d unc)  %s\n",
				r.AvgPct, r.Symbols, r.Covered, r.Partial, r.Uncovered, r.Dir)
		}
		if len(rows) == 0 {
			b.WriteString("no coverage data\n")
		}
		return mcp.NewToolResultText(b.String()), nil
	}
	return s.respondJSONOrTOON(ctx, req, map[string]any{
		"directories": rows,
		"total":       len(rows),
	})
}

// roundTwoDecimal rounds to 2 decimal places, mirroring the
// coverage package's roundTwo. Local helper rather than an import
// dependency on internal/coverage so the mcp tool stays
// self-contained.
func roundTwoDecimal(v float64) float64 {
	if v < 0 {
		return v
	}
	return float64(int64(v*100+0.5)) / 100
}

// handleAnalyzeInteropUsers lists every file with the named
// cross-language interop meta flag set. Currently used for two
// sentinels: meta.uses_cgo (Go files that `import "C"`) and
// meta.uses_wasm_bindgen (Rust files with `#[wasm_bindgen]`).
// Each routes through the same handler with a different
// metaKey + resultKey pair — adding future interop kinds
// (jni, napi, ffi-style imports) is one switch case in the
// dispatcher.
//
// Useful for porting surveys ("how much surface uses cgo?"),
// CI gate questions ("did this PR add a new wasm-bindgen
// boundary?"), and non-interop build planning. Files are
// reported in path order so the result is diff-able across
// runs.
func (s *Server) handleAnalyzeInteropUsers(ctx context.Context, req mcp.CallToolRequest, metaKey, resultKey string) (*mcp.CallToolResult, error) {
	type interopFile struct {
		File string `json:"file"`
		ID   string `json:"id"`
	}
	var rows []interopFile
	// Kind pushdown — uses_cgo / uses_wasm_bindgen sentinels only
	// live on file nodes; pulling AllNodes() to find them was pure
	// overhead on a disk backend.
	for _, n := range s.scopedNodesByKinds(ctx, []graph.NodeKind{graph.KindFile}) {
		if v, _ := n.Meta[metaKey].(bool); !v {
			continue
		}
		rows = append(rows, interopFile{
			File: n.FilePath,
			ID:   n.ID,
		})
	}
	sort.Slice(rows, func(i, j int) bool {
		return rows[i].File < rows[j].File
	})

	if isCompact(req) {
		var b strings.Builder
		for _, r := range rows {
			b.WriteString(r.File)
			b.WriteByte('\n')
		}
		if len(rows) == 0 {
			fmt.Fprintf(&b, "no %s\n", resultKey)
		}
		return mcp.NewToolResultText(b.String()), nil
	}
	return s.respondJSONOrTOON(ctx, req, map[string]any{
		resultKey: rows,
		"total":   len(rows),
	})
}

// handleAnalyzeReleases reads the pre-computed release timeline from
// the graph. Inputs come from meta.added_in (stamped on KindFile
// nodes) and the KindRelease nodes the enricher materialises — one
// per tag, ordered, carrying file_count metadata. No git subprocess
// at read time.
//
// When nothing in scope carries release metadata the tool returns a
// structured error pointing the agent at `enrich_releases` (or the
// `gortex enrich releases` CLI) rather than silently returning an
// empty result; the latter would look like "this repo has no
// releases" even when the cause is "you haven't enriched yet".
//
// Optional filter `tag` returns only the named release with the list
// of files whose meta.added_in matches it — answers "what shipped in
// v1.4?" with a single graph scan.
func (s *Server) handleAnalyzeReleases(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if s.graph == nil {
		return mcp.NewToolResultError("graph not initialized"), nil
	}
	repoFilter := strings.TrimSpace(req.GetString("repo", ""))
	tagFilter := strings.TrimSpace(req.GetString("tag", ""))

	type releaseRow struct {
		ID         string   `json:"id"`
		Tag        string   `json:"tag"`
		RepoPrefix string   `json:"repo_prefix,omitempty"`
		FileCount  int      `json:"file_count"`
		Order      int      `json:"order"`
		Files      []string `json:"files,omitempty"`
	}
	// Every scan and sidecar read in this handler shares one reader, so
	// an overlay-active request grades the timeline against the state it
	// reads rather than mixing buffers with the indexed node set.
	reader := s.readerFor(ctx)
	releaseByTag := map[string]*releaseRow{}
	for _, n := range reader.AllNodes() {
		if n.Kind != graph.KindRelease {
			continue
		}
		// Gate every release node on the session workspace ceiling +
		// resolved RepoAllow narrowing (stamped into ctx by handleAnalyze).
		// Strict no-op for an unbound session with no RepoAllow.
		if !s.analyzeNodeVisible(ctx, n) {
			continue
		}
		if repoFilter != "" && n.RepoPrefix != repoFilter {
			continue
		}
		row := &releaseRow{
			ID:         n.ID,
			Tag:        n.Name,
			RepoPrefix: n.RepoPrefix,
		}
		if n.Meta != nil {
			row.FileCount = intFromAny(n.Meta["file_count"])
			row.Order = intFromAny(n.Meta["order"])
		}
		key := releaseKey(n.RepoPrefix, n.Name)
		releaseByTag[key] = row
	}

	if tagFilter != "" {
		// Caller wants the file list for one release. We surface it
		// from meta.added_in rather than a tree walk, so the answer
		// is whatever the last enrich pass observed.
		row, ok := releaseByTag[releaseKey(repoFilter, tagFilter)]
		if !ok {
			// Tolerate the no-prefix form: agents pass "v1.4" without
			// realising the graph stores multi-repo tags as
			// "<prefix>/v1.4". Fall back to a tag-name-only match.
			for k, r := range releaseByTag {
				if r.Tag == tagFilter {
					row = r
					_ = k
					break
				}
			}
		}
		if row == nil {
			return s.respondJSONOrTOON(ctx, req, map[string]any{
				"error":      fmt.Sprintf("no KindRelease node for tag %q; run `enrich_releases` first", tagFilter),
				"suggestion": "enrich_releases",
				"releases":   []releaseRow{},
				"total":      0,
			})
		}
		relByID := releaseRowsByID(reader)
		for _, n := range reader.AllNodes() {
			if n.Kind != graph.KindFile || n.FilePath == "" {
				continue
			}
			if !s.analyzeNodeVisible(ctx, n) {
				continue
			}
			if repoFilter != "" && n.RepoPrefix != repoFilter {
				continue
			}
			added, ok := addedInFrom(relByID, n)
			if !ok || added != row.Tag {
				continue
			}
			row.Files = append(row.Files, n.FilePath)
		}
		sort.Strings(row.Files)
		return s.respondJSONOrTOON(ctx, req, map[string]any{
			"releases":  []releaseRow{*row},
			"total":     1,
			"tag":       tagFilter,
			"file_hits": len(row.Files),
		})
	}

	// No tag filter: return the timeline. Use `order` (oldest=0) so
	// callers can flip to newest-first via reverse.
	if len(releaseByTag) == 0 {
		// Distinguish "no enrichment yet" from "repo has no tags" by
		// peeking at any file's meta.added_in. If even one file has
		// the field set the enrichment ran and produced no releases
		// (an unlikely combination; surface as an empty timeline);
		// otherwise return the structured error.
		hasAnyAddedIn := false
		if relByID := releaseRowsByID(reader); len(relByID) > 0 {
			hasAnyAddedIn = true
		} else {
			for _, n := range reader.AllNodes() {
				if !s.analyzeNodeVisible(ctx, n) {
					continue
				}
				if n.Kind == graph.KindFile && n.Meta != nil {
					if _, ok := n.Meta["added_in"].(string); ok {
						hasAnyAddedIn = true
						break
					}
				}
			}
		}
		if !hasAnyAddedIn {
			return s.respondJSONOrTOON(ctx, req, map[string]any{
				"error":      "no release timeline in scope; run `enrich_releases` (or `gortex enrich releases`) to populate KindRelease nodes and meta.added_in",
				"suggestion": "enrich_releases",
				"releases":   []releaseRow{},
				"total":      0,
			})
		}
	}
	rows := make([]releaseRow, 0, len(releaseByTag))
	for _, r := range releaseByTag {
		rows = append(rows, *r)
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Order != rows[j].Order {
			return rows[i].Order < rows[j].Order
		}
		return rows[i].Tag < rows[j].Tag
	})
	return s.respondJSONOrTOON(ctx, req, map[string]any{
		"releases": rows,
		"total":    len(rows),
	})
}

// releaseKey builds the lookup key from a (repoPrefix, tag) pair so
// the tag-filtered path can compare scoped IDs against the bare
// agent input.
func releaseKey(repoPrefix, tag string) string {
	if repoPrefix == "" {
		return tag
	}
	return repoPrefix + "/" + tag
}

// handleAnalyzeBlame runs `git blame -p` against the indexed
// repository and stamps meta.last_authored on each function /
// method / type / interface / field / variable / constant /
// enum_member node it can map to a real source line. Returns the
// number of nodes enriched.
//
// Blocking — large repos can take seconds — but explicit (the
// agent invoked it). Repeat invocations re-run blame and overwrite
// existing meta.last_authored, which is the desired behaviour for
// post-commit refresh.
func (s *Server) handleAnalyzeBlame(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	roots := s.collectRepoRoots(req.GetString("repo", ""))
	if len(roots) == 0 {
		return mcp.NewToolResultError("blame enrichment requires at least one indexed repo with a root path"), nil
	}
	total := 0
	perRepo := make(map[string]any, len(roots))
	for prefix, root := range roots {
		count, err := blame.EnrichGraph(s.graph, root)
		if err != nil {
			perRepo[prefix] = map[string]any{"root": root, "error": err.Error()}
			continue
		}
		total += count
		perRepo[prefix] = map[string]any{"root": root, "enriched": count}
	}
	return s.respondJSONOrTOON(ctx, req, map[string]any{
		"enriched": total,
		"per_repo": perRepo,
	})
}

// collectRepoRoots returns the set of repo prefix → root paths to enrich.
// In multi-repo mode iterates every tracked repo (or just the one matching
// `scope` when set). In single-repo mode returns the lone indexer's root
// keyed by an empty prefix. Empty roots are skipped so callers don't have
// to filter them downstream.
func (s *Server) collectRepoRoots(scope string) map[string]string {
	out := make(map[string]string)
	if s.multiIndexer != nil {
		if scope != "" {
			if root, ok := s.multiIndexer.RepoRoot(scope); ok {
				out[scope] = root
			}
			return out
		}
		for prefix, meta := range s.multiIndexer.AllMetadata() {
			if meta == nil || meta.RootPath == "" {
				continue
			}
			out[prefix] = meta.RootPath
		}
		return out
	}
	if s.indexer != nil {
		if root := s.indexer.RootPath(); root != "" {
			out[s.indexer.RepoPrefix()] = root
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// 10.5 handleFindDeadCode and handleFindHotspots
// ---------------------------------------------------------------------------

func (s *Server) handleFindDeadCode(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	opts := analysis.FindDeadCodeOptions{}
	args := req.GetArguments()
	if v, ok := args["include_variables"].(bool); ok && v {
		opts.IncludeVariables = true
	}
	if v, ok := args["include_fields"].(bool); ok && v {
		opts.IncludeFields = true
	}
	if v, ok := args["include_constants"].(bool); ok && v {
		opts.IncludeConstants = true
	}
	if v, ok := args["include_cgo_exports"].(bool); ok && v {
		opts.IncludeCgoExports = true
	}
	if v, ok := args["include_linkname_targets"].(bool); ok && v {
		opts.IncludeLinknameTargets = true
	}
	if v, ok := args["skip_cross_repo_nodes"].(bool); ok && v {
		opts.SkipCrossRepoNodes = true
	}

	reader := s.readerFor(ctx)
	entries := analysis.FindDeadCode(reader, s.getProcesses(), nil, opts)

	// dead_code reads the whole graph directly, bypassing the scoped-node
	// accessors, so narrow its rows to the session workspace + optional
	// repo allow-set here. This also closes the latent cross-workspace
	// leak for this kind. Strict no-op for an unbound session with no
	// RepoAllow. Counts below are computed after this filter.
	if s.scopeFiltersActive(ctx) {
		kept := make([]analysis.DeadCodeEntry, 0, len(entries))
		for _, e := range entries {
			if s.analyzeNodeVisible(ctx, reader.GetNode(e.ID)) {
				kept = append(kept, e)
			}
		}
		entries = kept
	}

	// Cap response size — large repos surface thousands of dead-code
	// candidates and the default JSON encoding spills past the MCP
	// per-response token cap. Callers that need the full list can
	// raise the limit explicitly.
	limit := 200
	if v, ok := args["limit"].(float64); ok && v > 0 {
		limit = int(v)
	}
	totalEntries := len(entries)
	deadTruncated := false
	if len(entries) > limit {
		entries = entries[:limit]
		deadTruncated = true
	}

	variablesNote := buildDeadCodeNote(opts)

	if s.isGCX(ctx, req) {
		items := make([]deadCodeItem, 0, len(entries))
		for _, e := range entries {
			items = append(items, deadCodeItem{
				ID:   e.ID,
				Kind: e.Kind,
				Name: e.Name,
				Path: e.FilePath,
				Line: e.Line,
			})
		}
		return s.gcxResponseWithBudget(req)(encodeAnalyze("dead_code", items))
	}

	if isCompact(req) {
		var b strings.Builder
		for _, e := range entries {
			fmt.Fprintf(&b, "%s %s %s:%d\n", e.Kind, e.ID, e.FilePath, e.Line)
		}
		if len(entries) == 0 {
			b.WriteString("no dead code found\n")
		}
		if variablesNote != "" {
			fmt.Fprintf(&b, "\nnote: %s\n", variablesNote)
		}
		return mcp.NewToolResultText(b.String()), nil
	}

	result := map[string]any{
		"dead_code": entries,
		"total":     totalEntries,
		"truncated": deadTruncated,
	}
	if deadTruncated {
		result["limit"] = limit
	}
	if variablesNote != "" {
		result["note"] = variablesNote
	}
	return s.respondJSONOrTOON(ctx, req, result)
}

// buildDeadCodeNote summarises which low-signal kinds the analyzer
// dropped by default, so callers know which include_* flag to flip
// if they want to broaden the scan. Returns the empty string when
// every opt-in flag is already set.
func buildDeadCodeNote(opts analysis.FindDeadCodeOptions) string {
	var off []string
	if !opts.IncludeVariables {
		off = append(off, "variables (include_variables=true)")
	}
	if !opts.IncludeFields {
		off = append(off, "fields (include_fields=true)")
	}
	if !opts.IncludeConstants {
		off = append(off, "constants (include_constants=true)")
	}
	if len(off) == 0 {
		return ""
	}
	return "Excluded by default — graph lacks intra-function data flow, so these always look dead: " + strings.Join(off, ", ") + "."
}

func (s *Server) handleFindHotspots(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	// Check minimum graph size
	if s.readerFor(ctx).NodeCount() < 10 {
		return mcp.NewToolResultError("codebase too small for meaningful hotspot analysis (need at least 10 symbols)"), nil
	}

	threshold := 0.0
	if v, ok := req.GetArguments()["threshold"].(float64); ok {
		threshold = v
	}
	if threshold != 0 {
		defer scheduleOSMemoryReleaseAfterBurst(s.logger, "analyze_hotspots")
	}

	var entries []analysis.HotspotEntry
	if threshold == 0 {
		entries = s.getHotspots()
	} else {
		entries = analysis.FindHotspots(s.graph, s.getCommunities(), threshold)
	}

	// K17: optional novelty / directional reranking modes. Default
	// "complexity" preserves the legacy ranking.
	mode := strings.TrimSpace(stringArg(req.GetArguments(), "mode"))
	if mode == "" {
		mode = "complexity"
	}
	if mode != "complexity" {
		windowDays := 30
		if v, ok := req.GetArguments()["window_days"].(float64); ok && v > 0 {
			windowDays = int(v)
		}
		direction := strings.TrimSpace(stringArg(req.GetArguments(), "direction"))
		if direction == "" {
			direction = "adds"
		}
		entries = rerankHotspots(entries, s.graph, mode, direction, windowDays)
	}

	// hotspots reads s.graph directly, bypassing the scoped-node
	// accessors, so narrow its rows to the session workspace + optional
	// repo allow-set here (also closing the latent cross-workspace leak).
	// Strict no-op for an unbound session with no RepoAllow.
	if s.scopeFiltersActive(ctx) {
		reader := s.readerFor(ctx)
		kept := make([]analysis.HotspotEntry, 0, len(entries))
		for _, e := range entries {
			if s.analyzeNodeVisible(ctx, reader.GetNode(e.ID)) {
				kept = append(kept, e)
			}
		}
		entries = kept
	}

	// Truncate to top 20
	totalCount := len(entries)
	truncated := false
	if len(entries) > 20 {
		entries = entries[:20]
		truncated = true
	}

	if s.isGCX(ctx, req) {
		items := make([]hotspotItem, 0, len(entries))
		for _, e := range entries {
			items = append(items, hotspotItem{
				ID:             e.ID,
				Name:           e.Name,
				Path:           e.FilePath,
				Line:           e.Line,
				FanIn:          e.FanIn,
				FanOut:         e.FanOut,
				CrossCommunity: e.CommunityCrossings,
				Betweenness:    e.Betweenness,
				Score:          e.ComplexityScore,
			})
		}
		return s.gcxResponseWithBudget(req)(encodeAnalyze("hotspots", items))
	}

	if isCompact(req) {
		var b strings.Builder
		for _, e := range entries {
			fmt.Fprintf(&b, "%s %s %s:%d score=%.1f fan_in=%d fan_out=%d crossings=%d betweenness=%.1f\n",
				e.Kind, e.ID, e.FilePath, e.Line, e.ComplexityScore, e.FanIn, e.FanOut, e.CommunityCrossings, e.Betweenness)
		}
		if truncated {
			fmt.Fprintf(&b, "... truncated (%d total)\n", totalCount)
		}
		return mcp.NewToolResultText(b.String()), nil
	}

	return s.respondJSONOrTOON(ctx, req, map[string]any{
		"hotspots":  entries,
		"total":     totalCount,
		"truncated": truncated,
	})
}

// ---------------------------------------------------------------------------
// 10.6 handleScaffold
// ---------------------------------------------------------------------------

// scaffoldReader bridges Server to analysis.SourceReader so scaffolding can
// resolve file paths through the multi-repo aware Server.resolveGraphPath
// instead of relying on a single Indexer.RootPath which is empty in
// multi-repo mode.
// The reader carries the request it was built for: analysis.SourceReader takes
// no context, and path resolution needs one to place a path in the checkout the
// request reads.
type scaffoldReader struct {
	s   *Server
	ctx context.Context
}

func (r scaffoldReader) Graph() graph.Store { return r.s.graph }
func (r scaffoldReader) ResolveFilePath(graphPath string) string {
	abs, err := r.s.resolveGraphPath(r.ctx, graphPath)
	if err != nil {
		return ""
	}
	return abs
}

func (s *Server) handleScaffold(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	exampleID, err := req.RequireString("id")
	if err != nil {
		return mcp.NewToolResultError("id is required"), nil
	}
	newName, err := req.RequireString("new_name")
	if err != nil {
		return mcp.NewToolResultError("new_name is required"), nil
	}

	// dry_run defaults to true (scaffold never writes by default)
	dryRun := true
	if v, ok := req.GetArguments()["dry_run"].(bool); ok {
		dryRun = v
	}

	result, err := analysis.GenerateScaffold(s.engine, scaffoldReader{s: s, ctx: ctx}, exampleID, newName)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	resp := map[string]any{
		"edits":   result.Edits,
		"notes":   result.Notes,
		"dry_run": dryRun,
	}

	if !dryRun && s.indexer != nil {
		// Apply edits by writing files
		for _, edit := range result.Edits {
			absPath := edit.FilePath
			if root := s.indexer.RootPath(); root != "" {
				absPath = filepath.Join(root, edit.FilePath)
			}
			content, readErr := os.ReadFile(absPath)
			if readErr != nil {
				return mcp.NewToolResultError(fmt.Sprintf("could not read %s: %v", edit.FilePath, readErr)), nil
			}
			lines := strings.Split(string(content), "\n")
			insertIdx := max(edit.InsertionLine-1, 0)
			insertIdx = min(insertIdx, len(lines))
			newLines := make([]string, 0, len(lines)+strings.Count(edit.Code, "\n")+2)
			newLines = append(newLines, lines[:insertIdx]...)
			newLines = append(newLines, "")
			newLines = append(newLines, edit.Code)
			newLines = append(newLines, lines[insertIdx:]...)
			if writeErr := os.WriteFile(absPath, []byte(strings.Join(newLines, "\n")), 0o644); writeErr != nil {
				return mcp.NewToolResultError(fmt.Sprintf("could not write %s: %v", edit.FilePath, writeErr)), nil
			}
		}
		resp["applied"] = true
	}

	return s.respondJSONOrTOON(ctx, req, resp)
}

// ---------------------------------------------------------------------------
// 10.7 handleFindCycles and handleWouldCreateCycle
// ---------------------------------------------------------------------------

// cycleVisible reports whether every node on a cycle's path is visible
// to the current request (workspace ceiling + optional repo allow-set).
// A cycle is surfaced only when it is entirely in scope, so a chain that
// crosses the boundary is dropped rather than leaking its out-of-scope
// members.
func (s *Server) cycleVisible(ctx context.Context, c analysis.Cycle) bool {
	reader := s.readerFor(ctx)
	for _, id := range c.Path {
		if !s.analyzeNodeVisible(ctx, reader.GetNode(id)) {
			return false
		}
	}
	return true
}

func (s *Server) handleFindCycles(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	scope := req.GetString("scope", "")

	cycles := analysis.DetectCycles(s.readerFor(ctx), s.getCommunities(), scope)

	// cycles reads the whole graph directly, bypassing the scoped-node accessors,
	// so narrow here to the session workspace + optional repo allow-set.
	// A cycle is kept only when EVERY node on its path is visible, so a
	// chain that crosses the boundary is dropped rather than leaking its
	// cross-repo / cross-workspace members. Strict no-op for an unbound
	// session with no RepoAllow. Runs before the GCX / empty / total
	// blocks so all of them observe the filtered set.
	if s.scopeFiltersActive(ctx) {
		kept := make([]analysis.Cycle, 0, len(cycles))
		for _, c := range cycles {
			if s.cycleVisible(ctx, c) {
				kept = append(kept, c)
			}
		}
		cycles = kept
	}

	if s.isGCX(ctx, req) {
		items := make([]cycleItem, 0, len(cycles))
		for _, c := range cycles {
			items = append(items, cycleItem{
				Size:     len(c.Path),
				Severity: c.Kind,
				Nodes:    c.Path,
			})
		}
		return s.gcxResponseWithBudget(req)(encodeAnalyze("cycles", items))
	}

	if len(cycles) == 0 {
		return s.respondJSONOrTOON(ctx, req, map[string]any{
			"cycles":  []any{},
			"message": "no dependency cycles detected",
		})
	}

	// Truncate to 20 highest-severity (already sorted by severity desc)
	totalCount := len(cycles)
	truncated := false
	if len(cycles) > 20 {
		cycles = cycles[:20]
		truncated = true
	}

	if isCompact(req) {
		var b strings.Builder
		for _, c := range cycles {
			fmt.Fprintf(&b, "%s severity=%d %s\n", c.Kind, c.Severity, strings.Join(c.Path, " → "))
		}
		if truncated {
			fmt.Fprintf(&b, "... truncated (%d total)\n", totalCount)
		}
		return mcp.NewToolResultText(b.String()), nil
	}

	return s.respondJSONOrTOON(ctx, req, map[string]any{
		"cycles":    cycles,
		"total":     totalCount,
		"truncated": truncated,
	})
}

func (s *Server) handleWouldCreateCycle(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	fromID, err := req.RequireString("from_id")
	if err != nil {
		return mcp.NewToolResultError("from_id is required"), nil
	}
	toID, err := req.RequireString("to_id")
	if err != nil {
		return mcp.NewToolResultError("to_id is required"), nil
	}

	// Validate both symbols exist — against the request's reader, so a
	// symbol that only exists in the caller's buffer is not rejected.
	reader := s.readerFor(ctx)
	if reader.GetNode(fromID) == nil {
		return mcp.NewToolResultError("symbol not found: " + fromID), nil
	}
	if reader.GetNode(toID) == nil {
		return mcp.NewToolResultError("symbol not found: " + toID), nil
	}

	wouldCycle, path := analysis.WouldCreateCycle(reader, fromID, toID)

	if s.isGCX(ctx, req) {
		return s.gcxResponseWithBudget(req)(encodeAnalyze("would_create_cycle", map[string]any{
			"would_cycle": wouldCycle,
			"path":        path,
		}))
	}

	if isCompact(req) {
		if wouldCycle {
			return mcp.NewToolResultText(fmt.Sprintf("would_cycle=true %s\n", strings.Join(path, " → "))), nil
		}
		return mcp.NewToolResultText("would_cycle=false\n"), nil
	}

	return s.respondJSONOrTOON(ctx, req, map[string]any{
		"would_cycle": wouldCycle,
		"path":        path,
	})
}

// ---------------------------------------------------------------------------
// 10.8 handleDiffContext
// ---------------------------------------------------------------------------

// diffFileGroup groups changed symbols by file with risk assessment.
type diffFileGroup struct {
	FilePath string           `json:"file_path"`
	Risk     string           `json:"risk"`
	Symbols  []diffSymbolInfo `json:"symbols"`
}

// diffSymbolInfo holds enriched context for a single changed symbol.
type diffSymbolInfo struct {
	ID        string   `json:"id"`
	Name      string   `json:"name"`
	Kind      string   `json:"kind"`
	StartLine int      `json:"start_line"`
	Signature string   `json:"signature,omitempty"`
	Source    string   `json:"source,omitempty"`
	Callers   []string `json:"callers,omitempty"`
	Callees   []string `json:"callees,omitempty"`
	Community string   `json:"community,omitempty"`
	Processes []string `json:"processes,omitempty"`
}

func (s *Server) handleDiffContext(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	scope := req.GetString("scope", "unstaged")
	baseRef := req.GetString("base_ref", "main")

	// Resolve the working tree: explicit repo selector, lone tracked repo,
	// the session's cwd-bound repo, then its sole contained repo. "." is
	// reserved for the standalone (indexer-less) server, which is started
	// in the tree it serves.
	repoRoot, repoPrefix, rootErr := s.resolveDiffRoot(ctx, strings.TrimSpace(req.GetString("repo", "")))
	if rootErr != nil {
		return mcp.NewToolResultError(rootErr.Error()), nil
	}

	diff, err := analysis.MapGitDiff(s.readerFor(ctx), repoRoot, repoPrefix, scope, baseRef)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	if len(diff.ChangedSymbols) == 0 {
		return s.respondJSONOrTOON(ctx, req, map[string]any{
			"files":   []any{},
			"message": "no changes detected",
		})
	}

	communities := s.getCommunities()
	processes := s.getProcesses()

	// Build enriched symbol info. The lookups run on the request's
	// reader, matching the caller/chain walks below which already go
	// through the request's engine.
	reader := s.readerFor(ctx)
	var allSymbols []diffSymbolInfo
	for _, cs := range diff.ChangedSymbols {
		node := reader.GetNode(cs.ID)
		if node == nil {
			continue
		}

		info := diffSymbolInfo{
			ID:        cs.ID,
			Name:      cs.Name,
			Kind:      cs.Kind,
			StartLine: cs.Line,
		}

		// Signature
		if sig, ok := node.Meta["signature"].(string); ok {
			info.Signature = sig
		}

		// Source
		if node.StartLine > 0 && node.EndLine > 0 {
			if absPath, err := s.resolveNodePath(ctx, node); err == nil {
				if source, _, _, readErr := readLines(absPath, node.StartLine, node.EndLine, 0); readErr == nil {
					info.Source = source
				}
			}
		}

		// Callers (depth 1)
		callers := s.engineFor(ctx).GetCallers(cs.ID, query.QueryOptions{Depth: 1, Limit: 10, Detail: "brief"})
		for _, cn := range callers.Nodes {
			if cn.ID != cs.ID {
				info.Callers = append(info.Callers, cn.ID)
			}
		}

		// Callees (depth 1)
		callees := s.engineFor(ctx).GetCallChain(cs.ID, query.QueryOptions{Depth: 1, Limit: 10, Detail: "brief"})
		for _, cn := range callees.Nodes {
			if cn.ID != cs.ID {
				info.Callees = append(info.Callees, cn.ID)
			}
		}

		// Community
		if communities != nil {
			if cid, ok := communities.NodeToComm[cs.ID]; ok {
				info.Community = cid
			}
		}

		// Processes
		if processes != nil {
			info.Processes = processes.NodeToProcs[cs.ID]
		}

		allSymbols = append(allSymbols, info)
	}

	// Group by file
	fileMap := make(map[string][]diffSymbolInfo)
	for _, sym := range allSymbols {
		fp := ""
		if n := reader.GetNode(sym.ID); n != nil {
			fp = n.FilePath
		}
		if fp == "" {
			continue
		}
		fileMap[fp] = append(fileMap[fp], sym)
	}

	// Compute per-file risk
	var groups []diffFileGroup
	for fp, syms := range fileMap {
		// Compute risk based on blast radius of symbols in this file
		symbolIDs := make([]string, len(syms))
		for i, sym := range syms {
			symbolIDs[i] = sym.ID
		}
		impact := analysis.AnalyzeImpact(reader, symbolIDs, communities, processes)

		groups = append(groups, diffFileGroup{
			FilePath: fp,
			Risk:     string(impact.Risk),
			Symbols:  syms,
		})
	}

	// Sort by risk (CRITICAL > HIGH > MEDIUM > LOW)
	riskOrder := map[string]int{"CRITICAL": 0, "HIGH": 1, "MEDIUM": 2, "LOW": 3}
	sort.Slice(groups, func(i, j int) bool {
		ri := riskOrder[groups[i].Risk]
		rj := riskOrder[groups[j].Risk]
		if ri != rj {
			return ri < rj
		}
		return groups[i].FilePath < groups[j].FilePath
	})

	// Truncate to 50 symbols total
	totalSymbols := 0
	for _, g := range groups {
		totalSymbols += len(g.Symbols)
	}
	truncated := false
	if totalSymbols > 50 {
		truncated = true
		count := 0
		var truncGroups []diffFileGroup
		for _, g := range groups {
			if count >= 50 {
				break
			}
			remaining := 50 - count
			if len(g.Symbols) > remaining {
				g.Symbols = g.Symbols[:remaining]
			}
			truncGroups = append(truncGroups, g)
			count += len(g.Symbols)
		}
		groups = truncGroups
	}

	if isCompact(req) {
		var b strings.Builder
		for _, g := range groups {
			for _, sym := range g.Symbols {
				fmt.Fprintf(&b, "%s %s %s callers=%d callees=%d\n",
					sym.ID, sym.Kind, g.Risk, len(sym.Callers), len(sym.Callees))
			}
		}
		if truncated {
			fmt.Fprintf(&b, "... truncated (%d total symbols)\n", totalSymbols)
		}
		return mcp.NewToolResultText(b.String()), nil
	}

	return s.respondJSONOrTOON(ctx, req, map[string]any{
		"files":         groups,
		"total_symbols": totalSymbols,
		"total_files":   len(groups),
		"truncated":     truncated,
	})
}

// ---------------------------------------------------------------------------
// 10.9 handleIndexHealth
// ---------------------------------------------------------------------------

func (s *Server) handleIndexHealth(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if s.indexer == nil {
		return mcp.NewToolResultError("no indexer available"), nil
	}
	if err := ctx.Err(); err != nil {
		return mcp.NewToolResultError("index_health was cancelled before it finished: " + err.Error()), nil
	}

	result, updatedAt, refreshing := s.indexHealthSnapshot()
	if result == nil || s.indexHealthNeedsRefresh(updatedAt) {
		s.refreshIndexHealthInBackground()
		result, updatedAt, refreshing = s.indexHealthSnapshot()
	}

	if isCompact(req) {
		if result == nil {
			return mcp.NewToolResultText("health=unknown nodes=unknown stale=unknown failures=unknown status=refreshing\n"), nil
		}
		return mcp.NewToolResultText(compactIndexHealth(result, updatedAt, refreshing)), nil
	}

	if result == nil {
		return s.respondJSONOrTOON(ctx, req, map[string]any{
			"status":  "refreshing",
			"message": "index health is being computed in the background; retry for the completed report",
		})
	}
	return s.respondJSONOrTOON(ctx, req, result)
}

// buildIndexHealthPayload returns the same data the `index_health`
// tool emits. Shared with the `gortex://index-health` resource.
// Returns nil when no indexer is wired.
//
// Prefer buildIndexHealthPayloadCtx on any request path: this variant cannot
// be cancelled, and the work below is proportional to workspace size.
func (s *Server) buildIndexHealthPayload() map[string]any {
	payload, _ := s.buildIndexHealthPayloadCtx(context.Background())
	return payload
}

// healthScanCancelStride is how many iterations pass between context checks in
// the payload's per-file loops. Checking every iteration would cost more than
// the syscall it guards on a warm cache; a few hundred keeps the worst-case
// overshoot in the millisecond range.
const healthScanCancelStride = 512

// healthOrphanScanCap bounds the path-liveness stat loop. index_health is the
// call an agent makes to find out whether the daemon is up, so it must stay
// cheap on a workspace whose file count runs to six figures.
//
// Past the cap the audit describes a prefix of the node walk rather than the
// whole graph — not a uniform sample, so an orphan population concentrated in
// one directory can be over- or under-represented. That is why a truncated
// scan says so in the payload and its extrapolated total is indicative. What
// it still answers reliably is the question that matters here: whether the
// graph is clean at all.
const healthOrphanScanCap = 20000

// healthOrphanSampleLimit is how many orphan node IDs the payload names.
const healthOrphanSampleLimit = 3

// buildIndexHealthPayloadCtx is buildIndexHealthPayload with cancellation.
//
// "Minimal health probe" is what the tool looks like from outside, but the work
// is proportional to the workspace: one os.Stat per tracked file to answer
// staleness, plus a whole-graph scan. On a 20k-file workspace under indexing
// load that is tens of seconds of syscalls — and, before this, not one of them
// checked whether the caller was still there. A client that gave up at 30s left
// the daemon finishing a scan for nobody, holding a transport slot the whole
// time.
func (s *Server) buildIndexHealthPayloadCtx(ctx context.Context) (map[string]any, error) {
	if s.indexer == nil {
		return nil, nil
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	totalDetected := s.indexer.TotalDetected()
	parseErrors := s.indexer.ParseErrors()

	// One whole-graph Stats() for the whole payload. It used to be computed
	// twice — once for the totalDetected fallback and again below — which on a
	// large graph doubled the most expensive read in the function for no
	// additional information.
	stats := s.graph.Stats()

	// When totalDetected is 0 (e.g., graph restored from cache without a full re-index),
	// fall back to counting file nodes in the graph.
	if totalDetected == 0 {
		if fileCount, ok := stats.ByKind[string(graph.KindFile)]; ok {
			totalDetected = fileCount
		}
	}

	successfullyIndexed := max(totalDetected-len(parseErrors), 0)

	var healthScore float64
	if totalDetected > 0 {
		healthScore = math.Round(float64(successfullyIndexed)/float64(totalDetected)*1000) / 10
	}

	var staleFiles []string
	mtimes := s.indexer.FileMtimes()
	scanned := 0
	for relPath := range mtimes {
		if scanned%healthScanCancelStride == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		scanned++
		if s.indexer.IsStale(relPath) {
			staleFiles = append(staleFiles, relPath)
		}
	}

	langCoverage := make(map[string]bool)
	for lang := range stats.ByLanguage {
		langCoverage[lang] = true
	}

	// Skip rollup: count synthetic file nodes by skip_reason so an agent
	// can see WHY a file is missing from the graph (size / timeout /
	// minified / parse_failed / parse_panic) instead of guessing.
	skipped := map[string]int{}
	repoPrefixes := map[string]struct{}{}
	// Path-liveness audit, folded into the same walk. Every other freshness
	// signal in this payload is derived from the files the daemon already
	// tracks — the mtime ledger, the parse-error list, the skip rollup — so
	// none of them can see a node whose file left the disk without the daemon
	// witnessing the departure. Those nodes keep answering searches with code
	// that no longer exists while this call reports 100%. Asking the
	// filesystem about the paths the graph itself claims is the only probe
	// that can see them.
	orphans := graph.OrphanDiagnostics{}
	checkPath := s.newPathLivenessProbe()
	scanned = 0
	for n := range s.graph.NodesByKind(graph.KindFile) {
		if scanned%healthScanCancelStride == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		scanned++
		if n == nil {
			continue
		}
		repoPrefixes[n.RepoPrefix] = struct{}{}
		// Synthetic attribution paths (external::, external-call::) name no
		// file on disk by design, so they are outside the liveness question
		// for the same reason they are outside the ownership audit.
		if graph.IsAuditableRepoSourcePath(n.FilePath) {
			orphans.Candidates++
			if orphans.Candidates > healthOrphanScanCap {
				orphans.Truncated = true
			} else {
				orphans.Record(n.ID, n.RepoPrefix, checkPath(n), healthOrphanSampleLimit)
			}
		}
		if n.Meta == nil {
			continue
		}
		if reason, _ := n.Meta["skip_reason"].(string); reason != "" {
			skipped[reason]++
		}
	}

	// A graph holding files that no longer exist is not a healthy graph,
	// whatever the parse ratio says. Worst-of rather than a blend: the two
	// scores answer different questions ("did the files we found parse" vs
	// "are the files we hold still there"), and health should track whichever
	// one is failing.
	if liveScore := orphans.LiveScore(); liveScore < healthScore {
		healthScore = liveScore
	}

	// Per-file rollup from the files sidecar (when the backend records it):
	// the files that failed to parse, with their error locations + node
	// counts. Bounded so a pathological repo can't bloat the payload.
	filesWithErrors, indexedFileCount := s.buildIndexHealthFileRollup(repoPrefixes)

	// Density plausibility: even a trivial source file yields a file node
	// plus at least one symbol, so a populated graph averaging barely more
	// than one node per file means extraction produced little beyond the
	// file shells — a broken grammar or an aborted reindex. A soft warning,
	// never a reject.
	var nodesPerFile float64
	fileNodes := stats.ByKind[string(graph.KindFile)]
	if fileNodes > 0 {
		nodesPerFile = math.Round(float64(stats.TotalNodes)/float64(fileNodes)*100) / 100
	}
	densityDegenerate := fileNodes >= 5 && nodesPerFile > 0 && nodesPerFile < 1.2

	// Repo-ownership audit. A node's repo prefix keys every per-repo bucket,
	// scope filter and partial index, so a graph holding both prefixed and
	// unprefixed copies of the same code answers every repo-scoped read with
	// half its contents — while every other signal in this payload stays
	// green. Nothing else here can see that, so it gets its own block.
	prefixAudit, prefixAuditOK := graph.ReadPrefixDiagnostics(s.graph, 3)

	lastIndexTime := s.indexer.LastIndexTime()
	lastIndexStr := ""
	if !lastIndexTime.IsZero() {
		lastIndexStr = lastIndexTime.Format("2006-01-02T15:04:05Z07:00")
	}

	var recommendation string
	if healthScore < 80 {
		recommendation = "Health score below 80%. Run index_repository with path \".\" to re-index the codebase."
	}
	if !orphans.Clean() {
		msg := "Graph holds nodes for files that no longer exist on disk (" + orphans.Summary() + "). " +
			"search_symbols and find_usages will keep returning those symbols, and no staleness signal covers " +
			"them — stale_files only tracks files the daemon still knows about, and the daemon never witnessed " +
			"these deletions. Re-index the affected repo with reindex_repository and no `paths` argument — only a " +
			"full-tree pass evicts them, a scoped one cannot; if they survive it, untrack and re-track the repo."
		if recommendation == "" {
			recommendation = msg
		} else {
			recommendation = msg + " " + recommendation
		}
	}
	if prefixAuditOK && !prefixAudit.Clean() {
		msg := "Graph holds inconsistent repository ownership (" + prefixAudit.Summary() + "). " +
			"Repo-scoped reads (find_usages, search_symbols with a repo filter, per-repo counts) will silently " +
			"see only part of the graph. Re-index with index_repository path \".\"; if it persists, untrack and re-track the repo."
		if recommendation == "" {
			recommendation = msg
		} else {
			recommendation = msg + " " + recommendation
		}
	}

	// Edgeless-index sanity check: a populated graph with files and
	// symbol nodes but zero edges means edge extraction failed
	// wholesale (a broken grammar, an aborted reindex). Even a single
	// one-function file yields containment edges, so this only trips on
	// a real regression.
	edgesOK := totalDetected <= 0 || stats.TotalNodes <= 0 || stats.TotalEdges != 0
	if !edgesOK {
		msg := "Index has files and symbol nodes but zero edges — edge extraction failed. Re-index with index_repository path \".\"; if it persists the language grammar may be broken."
		if recommendation == "" {
			recommendation = msg
		} else {
			recommendation = msg + " " + recommendation
		}
	}
	if densityDegenerate {
		msg := "Index has files but almost no symbol nodes (nodes_per_file < 1.2) — extraction produced little beyond file shells. Re-index with index_repository path \".\"; if it persists the language grammar may be broken or the files unsupported."
		if recommendation == "" {
			recommendation = msg
		} else {
			recommendation = msg + " " + recommendation
		}
	}

	// Semantic-enrichment lifecycle per (repo, provider): a graph can be
	// 100% parsed yet carry zero lsp_resolved edges when the enrichment
	// pass was cut (partial) or discarded (abandoned) at its deadline.
	// Surfacing the state here is what lets an agent — or a benchmark —
	// distinguish "fully enriched" from "looks green, tiers missing".
	var enrichStatuses []semantic.EnrichmentStatus
	if s.semanticMgr != nil {
		enrichStatuses = s.semanticMgr.EnrichmentStatuses()
	}
	enrichmentIncomplete := false
	for _, st := range enrichStatuses {
		if st.State == semantic.EnrichStatePartial || st.State == semantic.EnrichStateAbandoned || st.State == semantic.EnrichStateFailed {
			enrichmentIncomplete = true
			break
		}
	}
	if enrichmentIncomplete {
		msg := "Semantic enrichment did not complete for at least one repo/provider (see semantic_enrichment) — LSP-tier edges may be missing and tier-filtered queries (e.g. find_usages min_tier=lsp_resolved) may under-report. Re-run enrichment (reindex_repository) or raise GORTEX_LSP_ENRICH_TIMEOUT."
		if recommendation == "" {
			recommendation = msg
		} else {
			recommendation = msg + " " + recommendation
		}
	}

	// Degraded providers split by whether the pass still landed work.
	//
	// REDUCED is the shape this concept was written for: a clangd pass with no
	// compile_commands.json runs reference confirmation only. Edges still land,
	// so it is a genuine partial success — semantic_enrichment_ok stays true
	// and the remediation points at the compilation database.
	//
	// INERT is different in kind: the provider degraded and landed nothing at
	// all — no edges, no nodes, no symbols. For that language the semantic tier
	// does not exist, and calling that healthy is how a repository sits at
	// health_score 100 with lsp_resolved_edges_by_language 0 for a language it
	// is full of. It counts as incomplete, and it reports the provider's own
	// reason rather than a canned compilation-database remediation that may not
	// even name the right toolchain.
	var reducedProviders, inertProviders, inertReasons []string
	for _, st := range enrichStatuses {
		if !st.Degraded {
			continue
		}
		label := st.Provider + " in " + st.Repo
		landed := st.EdgesConfirmed + st.EdgesRebound + st.EdgesAdded + st.NodesEnriched + st.SymbolsCovered
		// A provider that degrades for a language the graph does not contain is
		// correct and expected — the Go pass on a Rust tree is the case the
		// module gate exists to skip cheaply. Only a language actually present
		// in the graph can be under-enriched, so only that can lower health.
		presentInGraph := st.Language != "" && stats.ByLanguage[st.Language] > 0
		if landed == 0 && presentInGraph {
			inertProviders = append(inertProviders, label)
			if st.DegradedReason != "" {
				inertReasons = append(inertReasons, label+": "+st.DegradedReason)
			}
			continue
		}
		reducedProviders = append(reducedProviders, label)
	}
	if len(reducedProviders) > 0 {
		msg := "Semantic enrichment ran in degraded (reference-confirmation-only) mode for " + strings.Join(reducedProviders, ", ") + " because no compilation database was found — hover types and call/type-hierarchy edges were skipped. Generate compile_commands.json (cmake -DCMAKE_EXPORT_COMPILE_COMMANDS=ON, bear -- make, or meson) at the repo root, then reindex_repository."
		if recommendation == "" {
			recommendation = msg
		} else {
			recommendation = msg + " " + recommendation
		}
	}
	if len(inertProviders) > 0 {
		enrichmentIncomplete = true
		msg := "Semantic enrichment produced nothing for " + strings.Join(inertProviders, ", ") + ", so no LSP-tier edges exist for those languages: tier-filtered queries (min_tier=lsp_resolved) return nothing there rather than under-reporting, and every edge you do see came from the AST pass."
		if len(inertReasons) > 0 {
			msg += " Reported reason — " + strings.Join(inertReasons, "; ") + "."
		}
		if recommendation == "" {
			recommendation = msg
		} else {
			recommendation = msg + " " + recommendation
		}
	}

	result := map[string]any{
		"health_score":         healthScore,
		"total_detected":       totalDetected,
		"successfully_indexed": successfullyIndexed,
		"language_coverage":    langCoverage,
		"last_index_time":      lastIndexStr,
		"node_count":           stats.TotalNodes,
		"edge_count":           stats.TotalEdges,
		"edges_ok":             edgesOK,
		"nodes_per_file":       nodesPerFile,
		// Shape-degradation guard firings since process start. Nonzero means
		// the daemon caught (and self-healed) a live-patch or boot-reload
		// resolution regression rather than silently serving a shrunken graph.
		"resolution_regressions": indexer.ResolutionRegressions(),
	}
	if prefixAuditOK {
		ownership := map[string]any{
			"owned_code_nodes":   prefixAudit.OwnedCodeNodes,
			"unowned_code_nodes": prefixAudit.UnownedCodeNodes,
			"consistent":         prefixAudit.Clean(),
		}
		if prefixAudit.MisprefixedNodes > 0 {
			ownership["misprefixed_nodes"] = prefixAudit.MisprefixedNodes
			ownership["misprefixed_samples"] = prefixAudit.MisprefixedSamples
		}
		// Only surface samples when they point at a defect; on a
		// single-repo daemon every node is legitimately unowned and a
		// sample list would read as an error.
		if prefixAudit.Mixed() {
			ownership["mixed"] = true
			ownership["unowned_samples"] = prefixAudit.UnownedSamples
		}
		result["repo_ownership"] = ownership
	}
	// Path liveness. Reported whenever the audit had something to look at, not
	// only when it found a defect: "checked 2581, 0 orphans" is the evidence
	// that the 100% above means what it says, and its absence is what tells a
	// caller the probe could not run.
	if orphans.Candidates > 0 {
		liveness := map[string]any{
			"checked":      orphans.Checked,
			"orphan_files": orphans.Orphans,
			"orphan_rate":  math.Round(orphans.Rate()*10000) / 10000,
			"clean":        orphans.Clean(),
		}
		if orphans.Truncated {
			liveness["truncated"] = true
			liveness["candidates"] = orphans.Candidates
			liveness["estimated_orphan_files"] = orphans.EstimatedOrphans()
		}
		if orphans.Unresolvable > 0 {
			liveness["unresolvable"] = orphans.Unresolvable
		}
		if len(orphans.OrphanSamples) > 0 {
			liveness["orphan_samples"] = orphans.OrphanSamples
		}
		if len(orphans.OrphansByRepo) > 0 {
			liveness["orphans_by_repo"] = orphans.OrphansByRepo
		}
		result["path_liveness"] = liveness
	}
	// Tool-surface state: the active global preset (the per-session default
	// may differ by client) and the per-workspace learned surface size, so
	// the tool policy is inspectable without a separate call.
	preset, presetMode := s.ActivePreset()
	result["tool_preset"] = preset
	result["tool_preset_mode"] = presetMode
	if n := s.LearnedToolCount(); n > 0 {
		result["learned_tools"] = n
		result["learned_tool_names"] = s.LearnedToolNames()
	}
	if indexedFileCount > 0 {
		result["indexed_file_count"] = indexedFileCount
	}
	if len(filesWithErrors) > 0 {
		result["files_with_parse_errors"] = filesWithErrors
	}
	if len(skipped) > 0 {
		result["skipped"] = skipped
	}
	if len(parseErrors) > 0 {
		result["parse_failures"] = parseErrors
	}
	if len(staleFiles) > 0 {
		result["stale_files"] = staleFiles
	}
	if len(enrichStatuses) > 0 {
		result["semantic_enrichment"] = enrichStatuses
		result["semantic_enrichment_ok"] = !enrichmentIncomplete
		// Per-language lsp-tier edge rollup from the enrichment statuses — a
		// cheap, graph-pass-free signal for whether min_tier=lsp_resolved is
		// usable on a given language. Near-zero for a language means
		// tier-filtered queries will under-report (the find_usages
		// tier_filtered caveat explains it per query; this shows it up front).
		lspEdgesByLang := map[string]int{}
		for _, st := range enrichStatuses {
			if st.Language == "" {
				continue
			}
			lspEdgesByLang[st.Language] += st.EdgesAdded + st.EdgesConfirmed + st.EdgesRebound
		}
		if len(lspEdgesByLang) > 0 {
			result["lsp_resolved_edges_by_language"] = lspEdgesByLang
		}
	}
	// Tracked-repo path liveness. A repo whose directory was deleted keeps
	// its registration forever: it stops indexing, its nodes go stale, and
	// every remaining signal in this payload stays green because they all
	// describe what IS in the graph, never what was supposed to be. Health
	// is the one place an agent looks before trusting a whole-workspace
	// answer, so a dead registration has to show up here (#312).
	if missingRepos := s.missingTrackedRepoPaths(); len(missingRepos) > 0 {
		result["missing_repo_paths"] = missingRepos
		result["tracked_repo_paths_ok"] = false
		msg := "Tracked repositories whose directory no longer exists on disk: " + strings.Join(missingRepos, ", ") +
			". They can never be re-indexed, so workspace-wide answers silently omit them. Remove each with `gortex untrack <path>`."
		if recommendation == "" {
			recommendation = msg
		} else {
			recommendation = msg + " " + recommendation
		}
	} else {
		result["tracked_repo_paths_ok"] = true
	}

	// A tracked repo holding zero indexed files is the one failure that
	// looks identical to success from every query: find_usages answers
	// "no callers", analyze answers "likely unused", and nothing marks the
	// answer as coming from an empty index (#624). Like a dead
	// registration, it can only be seen from here.
	if emptyRepos := s.emptyTrackedRepos(); len(emptyRepos) > 0 {
		result["empty_repos"] = emptyRepos
		result["repos_hold_files_ok"] = false
		subject := "Tracked repository"
		if len(emptyRepos) > 1 {
			subject = "Tracked repositories"
		}
		msg := subject + " holding no indexed files: " + strings.Join(emptyRepos, ", ") +
			". Every query scoped to them returns an empty answer that reads like a real one. Either the path holds no " +
			"source Gortex can parse, or an ignore rule (.gitignore, .gortexignore, .gortex.yaml excludes) excluded all of " +
			"it — the daemon log names the pattern. Re-index with index_repository path \".\" once it is fixed."
		if recommendation == "" {
			recommendation = msg
		} else {
			recommendation = msg + " " + recommendation
		}
	} else {
		result["repos_hold_files_ok"] = true
	}

	if recommendation != "" {
		result["recommendation"] = recommendation
	}

	return result, nil
}

// repoRootFor resolves the local checkout a repo prefix's nodes were indexed
// from, or "" when no root answers for it. The multi-repo registry is
// authoritative; a standalone server has none, and there the lone indexer
// answers for its own prefix and for the unprefixed nodes it mints.
func (s *Server) repoRootFor(repoPrefix string) string {
	if s.multiIndexer != nil {
		if root, ok := s.multiIndexer.RepoRoot(repoPrefix); ok {
			return root
		}
	}
	if s.indexer != nil && (repoPrefix == "" || repoPrefix == s.indexer.RepoPrefix()) {
		return s.indexer.RootPath()
	}
	return ""
}

// newPathLivenessProbe returns a classifier for one file node's on-disk path.
//
// The repo-root lookup is cached per prefix across the returned closure's life:
// a workspace-wide walk asks about the same handful of prefixes tens of
// thousands of times, and RepoRoot takes the multi-indexer's lock each call.
//
// Failure is never reported as absence. A prefix with no resolvable local root
// yields FilePathUnknown rather than condemning every node under it — that is
// a tracked-repo defect of its own, and inferring "deleted" from "I could not
// look" is how a health probe starts lying in the other direction.
func (s *Server) newPathLivenessProbe() func(*graph.Node) graph.FilePathState {
	roots := map[string]string{}
	return func(n *graph.Node) graph.FilePathState {
		if n == nil {
			return graph.FilePathUnknown
		}
		root, cached := roots[n.RepoPrefix]
		if !cached {
			root = s.repoRootFor(n.RepoPrefix)
			roots[n.RepoPrefix] = root
		}
		if root == "" {
			return graph.FilePathUnknown
		}
		rel := n.FilePath
		if n.RepoPrefix != "" {
			var trimmed bool
			rel, trimmed = strings.CutPrefix(rel, n.RepoPrefix+"/")
			if !trimmed {
				// The node claims a repo its own path does not sit under, so
				// joining it against that repo's root would stat a path this
				// node never described. The ownership audit above is what
				// reports this population; liveness abstains.
				return graph.FilePathUnknown
			}
		}
		if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(rel))); err != nil {
			if os.IsNotExist(err) {
				return graph.FilePathGone
			}
			return graph.FilePathUnknown
		}
		return graph.FilePathLive
	}
}

// missingTrackedRepoPaths returns, sorted and de-duplicated, the roots of
// every tracked repository whose directory is gone from disk.
//
// Both registries are consulted because they disagree in exactly the case
// that matters: right after a delete the dead root is still live in the
// indexer's metadata, while after a restart the failed repo is only in the
// config. Reading one alone would report the ghost in one window and lose
// it in the other.
func (s *Server) missingTrackedRepoPaths() []string {
	roots := map[string]struct{}{}
	if s.multiIndexer != nil {
		for _, meta := range s.multiIndexer.AllMetadata() {
			if meta != nil && meta.RootPath != "" {
				roots[meta.RootPath] = struct{}{}
			}
		}
	}
	if s.configManager != nil {
		if gc := s.configManager.Global(); gc != nil {
			for _, entry := range gc.Repos {
				if entry.Path != "" {
					roots[entry.Path] = struct{}{}
				}
			}
		}
	}
	missing := make([]string, 0, len(roots))
	for root := range roots {
		if config.RepoPathMissing(root) {
			missing = append(missing, root)
		}
	}
	sort.Strings(missing)
	return missing
}

// buildIndexHealthFileRollup reads the per-file metadata sidecar (when the
// backend implements graph.FileMetaReader) across the supplied repo prefixes
// and returns the files that recorded parse errors — each with its error
// locations + node count — plus the total indexed-file count. The error list
// is bounded so a badly-broken repo can't bloat the index_health payload.
func (s *Server) buildIndexHealthFileRollup(repoPrefixes map[string]struct{}) (filesWithErrors []map[string]any, indexedFileCount int) {
	reader, ok := s.graph.(graph.FileMetaReader)
	if !ok {
		return nil, 0
	}
	const maxErrorFiles = 100
	for prefix := range repoPrefixes {
		rows, err := reader.FileMetasForRepo(prefix)
		if err != nil {
			continue
		}
		indexedFileCount += len(rows)
		for _, r := range rows {
			if r.Errors == "" || len(filesWithErrors) >= maxErrorFiles {
				continue
			}
			entry := map[string]any{
				"file":       r.FilePath,
				"node_count": r.NodeCount,
			}
			var locs []string
			if json.Unmarshal([]byte(r.Errors), &locs) == nil && len(locs) > 0 {
				entry["errors"] = locs
			}
			filesWithErrors = append(filesWithErrors, entry)
		}
	}
	return filesWithErrors, indexedFileCount
}

// emptyTrackedRepos names every tracked repository whose last index pass
// admitted no source file at all.
//
// This is the failure behind #624, and it is invisible from everywhere
// else in the payload: an ignore rule that matches every path leaves a
// repo tracked, loaded, and answering — with an empty graph. find_usages
// says "no callers", analyze says "likely unused", and nothing marks the
// answer as coming from a repo that indexed nothing.
//
// The per-repo file count is the authoritative measure, not the graph's
// file nodes: a repo that indexed zero files can still hold a synthetic
// file node minted from a root manifest (go.mod, package.json,
// pyproject.toml), which is exactly what made the pandas report read as
// a one-file index. A repo that has never finished an index pass is
// pending, not empty, and is skipped.
func (s *Server) emptyTrackedRepos() []string {
	if s.multiIndexer == nil {
		return nil
	}
	var empty []string
	for _, meta := range s.multiIndexer.AllMetadata() {
		if meta == nil || meta.LastIndexTime.IsZero() || meta.FileCount > 0 {
			continue
		}
		name := meta.RepoPrefix
		if name == "" {
			name = meta.RootPath
		}
		if name != "" {
			empty = append(empty, name)
		}
	}
	sort.Strings(empty)
	return empty
}

// ---------------------------------------------------------------------------
// 10.10 handleGetSymbolHistory
// ---------------------------------------------------------------------------

func (s *Server) handleGetSymbolHistory(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	symbolID := req.GetString("id", "")
	symbolID = s.resolveSymbolID(ctx, symbolID)

	if symbolID != "" {
		// Single symbol history
		mods := s.symHistory.Get(symbolID)
		if len(mods) == 0 {
			return s.respondJSONOrTOON(ctx, req, map[string]any{
				"symbol_id":     symbolID,
				"modifications": []any{},
				"message":       "no modifications recorded for this symbol",
			})
		}

		churning := len(mods) >= 3

		if isCompact(req) {
			churnFlag := ""
			if churning {
				churnFlag = " [churning]"
			}
			return mcp.NewToolResultText(fmt.Sprintf("%s count=%d%s\n", symbolID, len(mods), churnFlag)), nil
		}

		return s.respondJSONOrTOON(ctx, req, map[string]any{
			"symbol_id":     symbolID,
			"count":         len(mods),
			"modifications": mods,
			"churning":      churning,
		})
	}

	// All symbols, sorted by count descending
	all := s.symHistory.All()
	if len(all) == 0 {
		return s.respondJSONOrTOON(ctx, req, map[string]any{
			"symbols": []any{},
			"message": "no modifications recorded this session",
		})
	}

	type symbolEntry struct {
		SymbolID      string               `json:"symbol_id"`
		Count         int                  `json:"count"`
		Churning      bool                 `json:"churning"`
		Modifications []SymbolModification `json:"modifications"`
	}

	var entries []symbolEntry
	for id, mods := range all {
		entries = append(entries, symbolEntry{
			SymbolID:      id,
			Count:         len(mods),
			Churning:      len(mods) >= 3,
			Modifications: mods,
		})
	}

	// Sort by count descending
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].Count > entries[j].Count
	})

	if isCompact(req) {
		var b strings.Builder
		for _, e := range entries {
			churnFlag := ""
			if e.Churning {
				churnFlag = " [churning]"
			}
			fmt.Fprintf(&b, "%s count=%d%s\n", e.SymbolID, e.Count, churnFlag)
		}
		return mcp.NewToolResultText(b.String()), nil
	}

	return s.respondJSONOrTOON(ctx, req, map[string]any{
		"symbols": entries,
		"total":   len(entries),
	})
}

// ---------------------------------------------------------------------------
// 10.11 handleBatchEdit
// ---------------------------------------------------------------------------

// batchEditItem is one operation in a batch_edit call. It is a discriminated
// union over `op`: an edit_symbol op carries {id, old_source, new_source}; an
// edit_file op carries {path, old_string, new_string, replace_all?}. When `op`
// is omitted it is inferred only from one complete, unambiguous field set, so
// both legacy item shapes remain supported without silently misclassifying
// malformed payloads. move_file and delete_file require an explicit op and may
// pin source bytes with expected_sha256 under the transaction lock.
type batchEditItem struct {
	Op string `json:"op,omitempty"`
	// edit_symbol
	SymbolID  string `json:"id,omitempty"`
	OldSource string `json:"old_source,omitempty"`
	NewSource string `json:"new_source,omitempty"`
	// edit_file and delete_file
	Path       string `json:"path,omitempty"`
	OldString  string `json:"old_string,omitempty"`
	NewString  string `json:"new_string,omitempty"`
	ReplaceAll bool   `json:"replace_all,omitempty"`
	// move_file
	SourcePath      string `json:"source,omitempty"`
	DestinationPath string `json:"destination,omitempty"`
	// move_file and delete_file
	ExpectedSHA256 string `json:"expected_sha256,omitempty"`
}

// kind returns a normalized operation kind. Runtime payloads are normalized by
// parseBatchEdits; preserving an explicit unknown value here ensures internal
// callers also fail closed instead of silently becoming edit_symbol.
func (it batchEditItem) kind() string {
	if it.Op != "" {
		return it.Op
	}
	if it.Path != "" {
		return "edit_file"
	}
	return "edit_symbol"
}

// batchEditResult represents the outcome of a single edit in the batch.
type batchEditResult struct {
	Op                       string `json:"op,omitempty"`
	SymbolID                 string `json:"id,omitempty"`
	FilePath                 string `json:"path"`
	DestinationPath          string `json:"destination,omitempty"`
	Status                   string `json:"status"` // "applied", "failed", "skipped"
	Error                    string `json:"error,omitempty"`
	Reindexed                bool   `json:"reindexed"`
	ReindexPending           bool   `json:"reindex_pending,omitempty"`
	ReindexReceipt           string `json:"reindex_receipt,omitempty"`
	ReindexGeneration        uint64 `json:"reindex_generation,omitempty"`
	ReindexAppliedGeneration uint64 `json:"reindex_applied_generation,omitempty"`
	ReindexError             string `json:"reindex_error,omitempty"`
	// EOLNormalized is true when the fragment only matched through the
	// CRLF<->LF-tolerant fallback and the replacement was written with the
	// file's own line terminators.
	EOLNormalized bool `json:"eol_normalized,omitempty"`
}

// batchEditItemsSchema is the JSON Schema for one batch_edit item: a
// discriminated union over the `op` field. Declaring the branches as a oneOf
// gives the model per-operation field validation instead of a single
// permissive, stringly-typed payload — the dominant source of malformed
// batch_edit calls.
func batchEditItemsSchema() map[string]any {
	return map[string]any{
		"oneOf": []any{
			map[string]any{
				"type":        "object",
				"description": "Replace a fragment inside a symbol's body.",
				"properties": map[string]any{
					"op":         map[string]any{"const": "edit_symbol", "description": "Operation kind (optional; inferred as edit_symbol when omitted and `id` is present)."},
					"id":         map[string]any{"type": "string", "description": "Symbol ID, e.g. pkg/foo.go::Bar."},
					"old_source": map[string]any{"type": "string", "description": "Exact fragment to replace within the symbol's source. CRLF/LF line-ending differences against the file are tolerated."},
					"new_source": map[string]any{"type": "string", "description": "Replacement fragment."},
				},
				"required": []any{"id", "old_source", "new_source"},
			},
			map[string]any{
				"type":        "object",
				"description": "Replace a string in any file (imports, config, comments — non-symbol edits).",
				"properties": map[string]any{
					"op":          map[string]any{"const": "edit_file", "description": "Operation kind (optional; inferred as edit_file when omitted and the complete file field set is present)."},
					"path":        map[string]any{"type": "string", "description": "File path (repo-relative or absolute)."},
					"old_string":  map[string]any{"type": "string", "description": "Exact text to replace; must be unique unless replace_all is set. CRLF/LF line-ending differences against the file are tolerated."},
					"new_string":  map[string]any{"type": "string", "description": "Replacement text."},
					"replace_all": map[string]any{"type": "boolean", "description": "Replace every occurrence instead of requiring uniqueness."},
				},
				"required": []any{"path", "old_string", "new_string"},
			},
			map[string]any{
				"type":        "object",
				"description": "Move a whole file atomically within an indexed repository.",
				"properties": map[string]any{
					"op":              map[string]any{"const": "move_file"},
					"source":          map[string]any{"type": "string", "description": "Existing source path (repo-relative or absolute)."},
					"destination":     map[string]any{"type": "string", "description": "Non-existing destination path inside an indexed repository."},
					"expected_sha256": map[string]any{"type": "string", "pattern": "^[0-9a-fA-F]{64}$", "description": "Optional SHA-256 precondition for the complete source bytes."},
				},
				"required": []any{"op", "source", "destination"},
			},
			map[string]any{
				"type":        "object",
				"description": "Delete a whole file atomically within an indexed repository.",
				"properties": map[string]any{
					"op":              map[string]any{"const": "delete_file"},
					"path":            map[string]any{"type": "string", "description": "Existing file path (repo-relative or absolute)."},
					"expected_sha256": map[string]any{"type": "string", "pattern": "^[0-9a-fA-F]{64}$", "description": "Optional SHA-256 precondition for the complete file bytes."},
				},
				"required": []any{"op", "path"},
			},
		},
	}
}

var batchEditAcceptedShapes = [...]string{
	`{"op":"edit_file","path":"<file>","old_string":"<text>","new_string":"<text>"}`,
	`{"op":"edit_symbol","id":"<symbol_id>","old_source":"<source>","new_source":"<source>"}`,
	`{"op":"move_file","source":"<file>","destination":"<file>"}`,
	`{"op":"delete_file","path":"<file>"}`,
}

type batchEditArgumentError struct {
	index  int
	reason string
}

func (e *batchEditArgumentError) Error() string {
	return fmt.Sprintf("edits[%d]: %s; accepted shapes: %s", e.index, e.reason, strings.Join(batchEditAcceptedShapes[:], " or "))
}

func batchEditHasAny(fields map[string]json.RawMessage, names ...string) bool {
	for _, name := range names {
		if _, ok := fields[name]; ok {
			return true
		}
	}
	return false
}

func missingBatchEditFields(fields map[string]json.RawMessage, names ...string) []string {
	missing := make([]string, 0, len(names))
	for _, name := range names {
		if _, ok := fields[name]; !ok {
			missing = append(missing, name)
		}
	}
	return missing
}

func classifyBatchEditItem(fields map[string]json.RawMessage, op string) (string, error) {
	hasPath := batchEditHasAny(fields, "path")
	hasFileFields := batchEditHasAny(fields, "old_string", "new_string", "replace_all")
	hasSymbolFields := batchEditHasAny(fields, "id", "old_source", "new_source")
	hasMoveFields := batchEditHasAny(fields, "source", "destination")
	hasDigest := batchEditHasAny(fields, "expected_sha256")

	if _, explicit := fields["op"]; explicit {
		switch op {
		case "edit_file", "edit_symbol", "move_file", "delete_file":
		default:
			return "", fmt.Errorf("unknown op %q (accepted values: edit_file, edit_symbol, move_file, delete_file)", op)
		}
	}

	kind := op
	if kind == "" {
		switch {
		case hasMoveFields || hasDigest:
			return "", fmt.Errorf("move_file and delete_file require an explicit op")
		case (hasPath || hasFileFields) && hasSymbolFields:
			return "", fmt.Errorf("item mixes edit_file and edit_symbol fields")
		case hasPath || hasFileFields:
			kind = "edit_file"
		case hasSymbolFields:
			kind = "edit_symbol"
		default:
			return "", fmt.Errorf("item does not match a supported batch edit shape")
		}
	}

	var missing []string
	switch kind {
	case "edit_file":
		if hasSymbolFields || hasMoveFields || hasDigest {
			return "", fmt.Errorf("item mixes fields from multiple batch edit operations")
		}
		missing = missingBatchEditFields(fields, "path", "old_string", "new_string")
	case "edit_symbol":
		if hasPath || hasFileFields || hasMoveFields || hasDigest {
			return "", fmt.Errorf("item mixes edit_file and edit_symbol fields")
		}
		missing = missingBatchEditFields(fields, "id", "old_source", "new_source")
	case "move_file":
		if hasPath || hasFileFields || hasSymbolFields {
			return "", fmt.Errorf("item mixes fields from multiple batch edit operations")
		}
		missing = missingBatchEditFields(fields, "source", "destination")
	case "delete_file":
		if hasFileFields || hasSymbolFields || hasMoveFields {
			return "", fmt.Errorf("item mixes fields from multiple batch edit operations")
		}
		missing = missingBatchEditFields(fields, "path")
	}
	if len(missing) > 0 {
		return "", fmt.Errorf("incomplete %s shape (missing: %s)", kind, strings.Join(missing, ", "))
	}
	return kind, nil
}

// parseBatchEdits accepts the `edits` argument as either a structured JSON
// array (the typed-schema path) or a JSON-encoded string of the same array
// (the legacy path). It validates every item's discriminator and complete field
// set before returning anything to the transaction layer.
func parseBatchEdits(raw any) ([]batchEditItem, error) {
	var data []byte
	switch v := raw.(type) {
	case nil:
		return nil, fmt.Errorf("edits is required")
	case string:
		data = []byte(v)
	default:
		b, err := json.Marshal(v)
		if err != nil {
			return nil, fmt.Errorf("invalid edits: %v", err)
		}
		data = b
	}

	var rawItems []json.RawMessage
	if err := json.Unmarshal(data, &rawItems); err != nil {
		return nil, fmt.Errorf("invalid edits JSON: %v", err)
	}
	edits := make([]batchEditItem, 0, len(rawItems))
	for i, rawItem := range rawItems {
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(rawItem, &fields); err != nil {
			return nil, &batchEditArgumentError{index: i, reason: "item must be an object: " + err.Error()}
		}
		var edit batchEditItem
		if err := json.Unmarshal(rawItem, &edit); err != nil {
			return nil, &batchEditArgumentError{index: i, reason: "invalid item fields: " + err.Error()}
		}
		kind, err := classifyBatchEditItem(fields, edit.Op)
		if err != nil {
			return nil, &batchEditArgumentError{index: i, reason: err.Error()}
		}
		edit.Op = kind
		edits = append(edits, edit)
	}
	return edits, nil
}

func batchEditInvalidArgumentResult(err error) *mcp.CallToolResult {
	data := map[string]any{
		"accepted_values": []string{"edit_file", "edit_symbol", "move_file", "delete_file"},
		"accepted_shapes": batchEditAcceptedShapes[:],
	}
	if itemErr, ok := err.(*batchEditArgumentError); ok {
		data["item_index"] = itemErr.index
	}
	return NewStructuredErrorResult(StructuredError{
		ErrorCode: ErrCodeInvalidArgument,
		Message:   err.Error(),
		Data:      data,
	})
}

func (s *Server) handleBatchEdit(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	return s.handleAtomicBatchEdit(ctx, req)
}

// applyBatchSymbolEdit applies one edit_symbol operation: it locates the
// symbol's source range, replaces old_source with new_source inside it, writes
// the file, and re-indexes. Semantics match the legacy single-op batch_edit.
func (s *Server) applyBatchSymbolEdit(ctx context.Context, edit batchEditItem, write bool) batchEditResult {
	res := batchEditResult{Op: "edit_symbol", SymbolID: edit.SymbolID}
	if edit.OldSource == edit.NewSource {
		res.Status, res.Error = "failed", "old_source and new_source are identical"
		return res
	}
	node := s.engineFor(ctx).GetSymbol(edit.SymbolID)
	if node == nil {
		res.Status, res.Error = "failed", "symbol not found: "+edit.SymbolID
		return res
	}
	res.FilePath = node.FilePath
	if node.StartLine == 0 || node.EndLine == 0 {
		res.Status, res.Error = "failed", "symbol has no line range"
		return res
	}
	absPath, resolveErr := s.resolveNodePath(ctx, node)
	if resolveErr != nil {
		res.Status, res.Error = "failed", resolveErr.Error()
		return res
	}
	if write {
		releaseMutation, lockErr := acquireMutationPath(ctx, absPath)
		if lockErr != nil {
			res.Status, res.Error = "failed", "edit cancelled while waiting for exclusive file access: "+lockErr.Error()
			return res
		}
		defer releaseMutation()
	}
	content, readErr := os.ReadFile(absPath)
	if readErr != nil {
		res.Status, res.Error = "failed", fmt.Sprintf("could not read file: %v", readErr)
		return res
	}
	fileStr := string(content)
	lines := strings.Split(fileStr, "\n")

	// Prefer the indexed symbol range, including preceding documentation. If a
	// prior edit in this batch shifted line numbers while watcher reindex is
	// pending, fall back only when old_source is unique in the current file.
	regionMatches := findEOLMatches(fileStr, edit.OldSource)
	symbolStart := 0
	rangeMatched := false
	if node.StartLine <= len(lines) && node.EndLine <= len(lines) {
		symbolSource := strings.Join(lines[node.StartLine-1:node.EndLine], "\n")
		effectiveStart := node.StartLine
		if findEOLMatches(symbolSource, edit.OldSource).count == 0 {
			expandedStart := node.StartLine - 1
			for expandedStart > 0 {
				trimmed := strings.TrimSpace(lines[expandedStart-1])
				if strings.HasPrefix(trimmed, "//") || strings.HasPrefix(trimmed, "/*") ||
					strings.HasPrefix(trimmed, "*") || trimmed == "" {
					expandedStart--
				} else {
					break
				}
			}
			if expandedStart < node.StartLine-1 {
				expanded := strings.Join(lines[expandedStart:node.EndLine], "\n")
				if findEOLMatches(expanded, edit.OldSource).count > 0 {
					symbolSource = expanded
					effectiveStart = expandedStart + 1
				}
			}
		}
		for i := 0; i < effectiveStart-1 && i < len(lines); i++ {
			symbolStart += len(lines[i]) + 1
		}
		symbolEnd := min(symbolStart+len(symbolSource), len(fileStr))
		candidate := findEOLMatches(fileStr[symbolStart:symbolEnd], edit.OldSource)
		if candidate.count == 1 {
			regionMatches = candidate
			rangeMatched = true
		}
	}
	if !rangeMatched {
		symbolStart = 0
		switch regionMatches.count {
		case 0:
			res.Status, res.Error = "failed", "old_source not found within symbol or current file"
			return res
		case 1:
			// Safe stale-range fallback.
		default:
			res.Status, res.Error = "failed", "symbol range is stale and old_source is not unique in the current file"
			return res
		}
	}
	span := regionMatches.spans[0]
	editStart := symbolStart + span.start
	editEnd := symbolStart + span.end
	effectiveNew := edit.NewSource
	if regionMatches.normalized {
		effectiveNew = adaptToDominantEOL(edit.NewSource, fileStr[editStart:editEnd])
		res.EOLNormalized = true
	}
	newContent := fileStr[:editStart] + effectiveNew + fileStr[editEnd:]
	if newContent == fileStr {
		res.Status, res.Error = "failed", "old_source and new_source are identical after line-ending normalization"
		return res
	}
	if !write {
		res.Status = "validated"
		return res
	}
	perm := os.FileMode(0o644)
	if info, statErr := os.Stat(absPath); statErr == nil {
		perm = info.Mode().Perm()
	}
	commit, writeErr := s.commitFileMutation(ctx, "batch_edit", "", "", node.FilePath, absPath, []byte(newContent), perm)
	if writeErr != nil {
		res.Status, res.Error = "failed", fmt.Sprintf("could not write file: %v", writeErr)
		return res
	}
	sess := s.sessionFor(ctx)
	sess.recordModified(node.FilePath)
	sess.recordSymbol(edit.SymbolID)
	reindexOutcome := s.mutationReindexState(ctx, absPath)
	commit.recordGraph(reindexOutcome)
	res.Reindexed, res.ReindexPending = reindexOutcome.Reindexed, reindexOutcome.Pending
	res.ReindexReceipt = reindexOutcome.Receipt
	res.ReindexGeneration = reindexOutcome.Generation
	res.ReindexAppliedGeneration = reindexOutcome.AppliedGeneration
	if reindexOutcome.Err != nil {
		res.ReindexError = reindexOutcome.Err.Error()
	}
	res.Status = "applied"
	return res
}

// applyBatchFileEdit applies one edit_file operation: it replaces old_string
// with new_string in the file at path, mirroring edit_file's uniqueness and
// replace_all semantics, then re-indexes.
func (s *Server) applyBatchFileEdit(ctx context.Context, edit batchEditItem, write bool) batchEditResult {
	res := batchEditResult{Op: "edit_file", FilePath: edit.Path}
	if edit.Path == "" {
		res.Status, res.Error = "failed", "edit_file op requires path"
		return res
	}
	if edit.OldString == edit.NewString {
		res.Status, res.Error = "failed", "old_string and new_string are identical"
		return res
	}
	absPath, relPath, resolveErr := s.resolveFilePath(ctx, edit.Path)
	if resolveErr != nil {
		res.Status, res.Error = "failed", resolveErr.Error()
		return res
	}
	res.FilePath = relPath
	if write {
		releaseMutation, lockErr := acquireMutationPath(ctx, absPath)
		if lockErr != nil {
			res.Status, res.Error = "failed", "edit cancelled while waiting for exclusive file access: "+lockErr.Error()
			return res
		}
		defer releaseMutation()
	}
	content, readErr := os.ReadFile(absPath)
	if readErr != nil {
		res.Status, res.Error = "failed", fmt.Sprintf("could not read file: %v", readErr)
		return res
	}
	fileStr := string(content)
	matches := findEOLMatches(fileStr, edit.OldString)
	count := matches.count
	if count == 0 {
		res.Status, res.Error = "failed", "old_string not found in file"
		return res
	}
	if count > 1 && !edit.ReplaceAll {
		res.Status, res.Error = "failed", fmt.Sprintf(
			"old_string matches %d locations%s. Provide a larger fragment for uniqueness or set replace_all=true.",
			count, matchSpansHint(fileStr, matches.spans))
		return res
	}
	var newContent string
	switch {
	case matches.normalized:
		// The CRLF<->LF fallback matched: splice the real byte spans and
		// write new_string with each region's own line terminators so the
		// edit never introduces mixed endings.
		limit := 1
		if edit.ReplaceAll {
			limit = -1
		}
		newContent = spliceSpansEOL(fileStr, matches.spans, edit.NewString, limit)
		if newContent == fileStr {
			res.Status, res.Error = "failed", "old_string and new_string are identical after line-ending normalization"
			return res
		}
		res.EOLNormalized = true
	case edit.ReplaceAll:
		newContent = strings.ReplaceAll(fileStr, edit.OldString, edit.NewString)
	default:
		newContent = strings.Replace(fileStr, edit.OldString, edit.NewString, 1)
	}
	if !write {
		res.Status = "validated"
		return res
	}
	perm := os.FileMode(0o644)
	if info, statErr := os.Stat(absPath); statErr == nil {
		perm = info.Mode().Perm()
	}
	commit, writeErr := s.commitFileMutation(ctx, "batch_edit", "", "", relPath, absPath, []byte(newContent), perm)
	if writeErr != nil {
		res.Status, res.Error = "failed", fmt.Sprintf("could not write file: %v", writeErr)
		return res
	}
	s.sessionFor(ctx).recordModified(relPath)
	reindexOutcome := s.mutationReindexState(ctx, absPath)
	commit.recordGraph(reindexOutcome)
	res.Reindexed, res.ReindexPending = reindexOutcome.Reindexed, reindexOutcome.Pending
	res.ReindexReceipt = reindexOutcome.Receipt
	res.ReindexGeneration = reindexOutcome.Generation
	res.ReindexAppliedGeneration = reindexOutcome.AppliedGeneration
	if reindexOutcome.Err != nil {
		res.ReindexError = reindexOutcome.Err.Error()
	}
	res.Status = "applied"
	return res
}

// ---------------------------------------------------------------------------
// handleContracts — unified dispatcher for contracts (replaces 2 tools)
// ---------------------------------------------------------------------------

func (s *Server) handleContracts(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	action := req.GetString("action", "list")
	switch action {
	case "list", "":
		return s.handleGetContracts(ctx, req)
	case "check":
		return s.handleCheckContracts(ctx, req)
	case "validate":
		return s.handleValidateContracts(ctx, req)
	case "bridge":
		return s.handleContractBridges(ctx, req)
	default:
		return mcp.NewToolResultError("unknown contracts action: " + action + " (expected: list, check, validate, or bridge)"), nil
	}
}

// ---------------------------------------------------------------------------
// handleGetContracts
// ---------------------------------------------------------------------------

func (s *Server) handleGetContracts(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	registry := s.effectiveContractRegistry()
	if registry == nil {
		// A repository indexed with a lost contract tail reaches here too:
		// nothing ever committed the tier, so no registry was retained. Name
		// that possibility instead of sending the caller off to re-index a
		// repository that is already indexed.
		msg := "no contract registry available — index a repository first"
		if unbuilt := s.contractTierUnbuiltRepos(ctx, nil); len(unbuilt) > 0 {
			msg += "; " + contractTierCaveatLine(unbuilt)
		}
		return mcp.NewToolResultError(msg), nil
	}

	contractType := req.GetString("type", "")
	role := req.GetString("role", "")

	args := req.GetArguments()
	allRepos := false
	if v, ok := args["all_repos"].(bool); ok {
		allRepos = v
	}
	includeDeps := false
	if v, ok := args["include_deps"].(bool); ok {
		includeDeps = v
	}

	var resolved ResolvedScope
	var contractRepoAllow map[string]bool
	if !allRepos {
		var errResult *mcp.CallToolResult
		resolved, errResult = s.resolveScope(ctx, req, IntentReach)
		if errResult != nil {
			return errResult, nil
		}
		contractRepoAllow = s.contractRepoAllowForRequest(ctx, req, resolved)
	}

	all := registry.All()

	// Apply filters.
	var filtered []contracts.Contract
	otherRepos := make(map[string]int)
	depsSkipped := 0
	for _, c := range all {
		isDep := c.Type == contracts.ContractDependency || excludes.IsVendored(c.FilePath)

		if !allRepos && !contractInResolvedScope(c, resolved, contractRepoAllow) {
			if includeDeps || !isDep {
				otherRepos[c.RepoPrefix]++
			}
			continue
		}
		if contractType != "" && string(c.Type) != contractType {
			continue
		}
		if role != "" && string(c.Role) != role {
			continue
		}
		if !includeDeps && isDep {
			depsSkipped++
			continue
		}
		filtered = append(filtered, c)
	}

	otherReposTotal := 0
	for _, n := range otherRepos {
		otherReposTotal += n
	}

	// Cap response — every per-contract row carries handler trails and
	// schema metadata, so a few hundred contracts blows past the MCP
	// per-response token cap. Default 200 surfaces enough to be useful;
	// callers that need every contract pass `limit` explicitly. With
	// pagination on, the contract list is sliced [offset, offset+limit)
	// with a `next_cursor` returned when the tail is unread.
	contractsLimit := 200
	if v, ok := args["limit"].(float64); ok && v > 0 {
		contractsLimit = int(v)
	}
	contractsOffset := decodeCursor(req.GetString("cursor", ""))
	contractsTotal := len(filtered)
	if contractsOffset > contractsTotal {
		contractsOffset = contractsTotal
	}
	contractsEnd := min(contractsOffset+contractsLimit, contractsTotal)
	filtered = filtered[contractsOffset:contractsEnd]
	contractsTruncated := contractsEnd < contractsTotal
	contractsNextCursor := ""
	if contractsTruncated {
		contractsNextCursor = encodeCursor(contractsEnd)
	}

	// An in-scope total of zero is ambiguous: these repositories may declare
	// no contracts, or their contract tier may never have been built (a lost
	// index tail commits nothing, and per-file mtime admission never
	// re-extracts it). Qualify the empty answer when the graph knows — see
	// contract_tier.go.
	var unbuiltTier []string
	if contractsTotal == 0 {
		unbuiltTier = s.contractTierUnbuiltRepos(ctx, contractRepoAllow)
	}

	if isCompact(req) {
		var b strings.Builder
		// Group by repo for readability in multi-repo mode.
		byRepo := make(map[string][]contracts.Contract)
		for _, c := range filtered {
			repo := c.RepoPrefix
			if repo == "" {
				repo = "(default)"
			}
			byRepo[repo] = append(byRepo[repo], c)
		}
		for repoName, items := range byRepo {
			if len(byRepo) > 1 {
				fmt.Fprintf(&b, "\n[%s] (%d contracts)\n", repoName, len(items))
			}
			for _, c := range items {
				fmt.Fprintf(&b, "%s %s %s %s:%d\n", c.Type, c.Role, c.ID, c.FilePath, c.Line)
			}
		}
		if len(filtered) == 0 {
			b.WriteString("no contracts found\n")
		}
		fmt.Fprintf(&b, "total: %d contracts\n", len(filtered))
		if otherReposTotal > 0 {
			fmt.Fprintf(&b, "other_repos: %d contracts in %d repo(s) (pass all_repos=true or repo=<prefix> to include)\n", otherReposTotal, len(otherRepos))
		}
		if depsSkipped > 0 {
			fmt.Fprintf(&b, "dependencies_skipped: %d (pass include_deps=true to include)\n", depsSkipped)
		}
		if len(unbuiltTier) > 0 {
			b.WriteString(contractTierCaveatLine(unbuiltTier))
		}
		res := mcp.NewToolResultText(b.String())
		if !allRepos {
			res = decorateResultWithScope(res, resolved)
		}
		return res, nil
	}

	if s.isGCX(ctx, req) {
		extra := []string{}
		if otherReposTotal > 0 {
			extra = append(extra, "other_repos_contracts", fmt.Sprintf("%d", otherReposTotal),
				"other_repos", fmt.Sprintf("%d", len(otherRepos)))
		}
		if depsSkipped > 0 {
			extra = append(extra, "dependencies_skipped", fmt.Sprintf("%d", depsSkipped))
		}
		res, err := s.gcxResponseWithBudget(req)(encodeContractsList(filtered, len(filtered), extra...))
		if err == nil {
			stampContractTierCaveat(res, unbuiltTier)
		}
		if !allRepos {
			return withScopeResult(res, err, resolved)
		}
		return res, err
	}

	// Group by repo, then by type for structured output.
	type repoGroup struct {
		Contracts map[string][]contracts.Contract `json:"contracts"`
		Total     int                             `json:"total"`
	}
	byRepo := make(map[string]*repoGroup)
	for _, c := range filtered {
		repo := c.RepoPrefix
		if repo == "" {
			repo = "(default)"
		}
		if byRepo[repo] == nil {
			byRepo[repo] = &repoGroup{Contracts: make(map[string][]contracts.Contract)}
		}
		byRepo[repo].Contracts[string(c.Type)] = append(byRepo[repo].Contracts[string(c.Type)], c)
		byRepo[repo].Total++
	}

	payload := map[string]any{
		"by_repo":   byRepo,
		"total":     contractsTotal,
		"truncated": contractsTruncated,
	}
	if contractsTruncated {
		payload["limit"] = contractsLimit
	}
	if contractsNextCursor != "" {
		payload["next_cursor"] = contractsNextCursor
	}
	if otherReposTotal > 0 {
		payload["other_repos"] = map[string]any{
			"total":      otherReposTotal,
			"repo_count": len(otherRepos),
			"by_repo":    otherRepos,
			"hint":       "pass all_repos=true or repo=<prefix>/project=<name> to include these",
		}
	}
	if depsSkipped > 0 {
		payload["dependencies_skipped"] = map[string]any{
			"total": depsSkipped,
			"hint":  "pass include_deps=true to include type=dependency and vendor-pathed contracts",
		}
	}
	if len(unbuiltTier) > 0 {
		payload["contract_tier"] = contractTierCaveat(unbuiltTier)
	}
	if !allRepos {
		return s.respondScopedJSONOrTOON(ctx, req, payload, resolved)
	}
	return s.respondJSONOrTOON(ctx, req, payload)
}

func (s *Server) contractRepoAllowForRequest(ctx context.Context, req mcp.CallToolRequest, resolved ResolvedScope) map[string]bool {
	if resolved.RepoAllow != nil {
		return resolved.RepoAllow
	}
	if strings.TrimSpace(req.GetString("repo", "")) == "" &&
		strings.TrimSpace(req.GetString("project", "")) == "" &&
		strings.TrimSpace(req.GetString("ref", "")) == "" &&
		strings.TrimSpace(req.GetString("scope", "")) == "" {
		return nil
	}
	allowed, err := s.resolveRepoFilter(ctx, req)
	if err != nil {
		return nil
	}
	return allowed
}

func contractInResolvedScope(c contracts.Contract, resolved ResolvedScope, repoAllow map[string]bool) bool {
	if resolved.WorkspaceID != "" && c.EffectiveWorkspace() != resolved.WorkspaceID {
		return false
	}
	if len(repoAllow) > 0 && c.RepoPrefix != "" && !repoAllow[c.RepoPrefix] {
		return false
	}
	if len(repoAllow) == 0 && resolved.ProjectID != "" && c.EffectiveProject() != resolved.ProjectID {
		return false
	}
	return true
}

// ---------------------------------------------------------------------------
// handleCheckContracts
// ---------------------------------------------------------------------------

func (s *Server) handleCheckContracts(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	registry := s.effectiveContractRegistry()
	if registry == nil {
		return mcp.NewToolResultError("no contract registry available — index a repository first"), nil
	}

	resolved, errResult := s.resolveScope(ctx, req, IntentReach)
	if errResult != nil {
		return errResult, nil
	}
	contractRepoAllow := s.contractRepoAllowForRequest(ctx, req, resolved)

	reg := contracts.NewRegistry()
	for _, c := range registry.All() {
		if contractInResolvedScope(c, resolved, contractRepoAllow) {
			reg.Add(c)
		}
	}

	result := contracts.Match(reg)

	if isCompact(req) {
		var b strings.Builder
		fmt.Fprintf(&b, "matched: %d pairs\n", len(result.Matched))
		for _, m := range result.Matched {
			cross := ""
			if m.CrossRepo {
				cross = " [cross-repo]"
			}
			provRepo := m.Provider.RepoPrefix
			consRepo := m.Consumer.RepoPrefix
			if provRepo == "" {
				provRepo = "(default)"
			}
			if consRepo == "" {
				consRepo = "(default)"
			}
			fmt.Fprintf(&b, "  %s: [%s] %s:%d -> [%s] %s:%d%s\n",
				m.ContractID,
				provRepo, m.Provider.FilePath, m.Provider.Line,
				consRepo, m.Consumer.FilePath, m.Consumer.Line,
				cross)
		}
		fmt.Fprintf(&b, "orphan providers: %d\n", len(result.OrphanProviders))
		for _, o := range result.OrphanProviders {
			repoLabel := o.RepoPrefix
			if repoLabel == "" {
				repoLabel = "(default)"
			}
			fmt.Fprintf(&b, "  [%s] %s %s:%d\n", repoLabel, o.ID, o.FilePath, o.Line)
		}
		fmt.Fprintf(&b, "orphan consumers: %d\n", len(result.OrphanConsumers))
		for _, o := range result.OrphanConsumers {
			repoLabel := o.RepoPrefix
			if repoLabel == "" {
				repoLabel = "(default)"
			}
			fmt.Fprintf(&b, "  [%s] %s %s:%d\n", repoLabel, o.ID, o.FilePath, o.Line)
		}
		return decorateResultWithScope(mcp.NewToolResultText(b.String()), resolved), nil
	}

	if s.isGCX(ctx, req) {
		res, err := s.gcxResponseWithBudget(req)(encodeContractsCheck(result))
		return withScopeResult(res, err, resolved)
	}

	payload := map[string]any{
		"matched":          result.Matched,
		"orphan_providers": result.OrphanProviders,
		"orphan_consumers": result.OrphanConsumers,
		"summary": map[string]int{
			"matched_pairs":    len(result.Matched),
			"orphan_providers": len(result.OrphanProviders),
			"orphan_consumers": len(result.OrphanConsumers),
		},
	}
	return s.respondScopedJSONOrTOON(ctx, req, payload, resolved)
}

// ---------------------------------------------------------------------------
// handleValidateContracts
// ---------------------------------------------------------------------------
//
// Pairs each contract's provider and consumer sides, diffs their
// request/response shapes (populated by the Stage 2 snapshotting
// pass), and returns a list of issues classified as breaking,
// warning, or info. Accepts the same repo/project/ref scoping
// parameters as `check` so callers can limit the diff to one project.

func (s *Server) handleValidateContracts(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	registry := s.effectiveContractRegistry()
	if registry == nil {
		return mcp.NewToolResultError("no contract registry available — index a repository first"), nil
	}

	resolved, errResult := s.resolveScope(ctx, req, IntentReach)
	if errResult != nil {
		return errResult, nil
	}
	contractRepoAllow := s.contractRepoAllowForRequest(ctx, req, resolved)

	reg := contracts.NewRegistry()
	for _, c := range registry.All() {
		if contractInResolvedScope(c, resolved, contractRepoAllow) {
			reg.Add(c)
		}
	}

	// Shape lookup pulls Shape out of the type node's meta — the
	// indexer attaches it during commitContracts (see
	// snapshotContractShapes in internal/indexer/indexer.go).
	lookup := contracts.ShapeLookup(func(symbolID string) *contracts.Shape {
		// Base read on purpose: only the indexer stamps the shape meta this
		// reads, so no other view can carry it.
		n := s.graph.GetNode(symbolID)
		if n == nil || n.Meta == nil {
			return nil
		}
		switch v := n.Meta["shape"].(type) {
		case *contracts.Shape:
			return v
		case contracts.Shape:
			return &v
		}
		return nil
	})

	issues := contracts.Validate(reg, lookup)

	// Severity rollup for easy at-a-glance counts.
	summary := map[string]int{"breaking": 0, "warning": 0, "info": 0, "total": len(issues)}
	for _, is := range issues {
		switch is.Severity {
		case contracts.SeverityBreaking:
			summary["breaking"]++
		case contracts.SeverityWarning:
			summary["warning"]++
		case contracts.SeverityInfo:
			summary["info"]++
		}
	}

	if isCompact(req) {
		var b strings.Builder
		fmt.Fprintf(&b, "issues: %d (breaking=%d warning=%d info=%d)\n",
			summary["total"], summary["breaking"], summary["warning"], summary["info"])
		for _, is := range issues {
			field := is.Field
			if field == "" {
				field = "-"
			}
			fmt.Fprintf(&b, "  [%s] %s %s field=%s prov=%s cons=%s %s\n",
				is.Severity, is.ContractID, is.Kind, field, is.Provider, is.Consumer, is.Details)
		}
		return decorateResultWithScope(mcp.NewToolResultText(b.String()), resolved), nil
	}

	payload := map[string]any{
		"issues":  issues,
		"summary": summary,
	}
	return s.respondScopedJSONOrTOON(ctx, req, payload, resolved)
}

// ---------------------------------------------------------------------------
// handleFeedback — unified dispatcher for feedback (replaces 2 tools)
// ---------------------------------------------------------------------------

func (s *Server) handleFeedback(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	action, err := req.RequireString("action")
	if err != nil {
		return mcp.NewToolResultError("action is required (one of: record, query)"), nil
	}
	switch action {
	case "record":
		return s.handleRecordFeedback(ctx, req)
	case "query":
		return s.handleQueryFeedback(ctx, req)
	default:
		return mcp.NewToolResultError("unknown feedback action: " + action + " (expected: record or query)"), nil
	}
}

// ---------------------------------------------------------------------------
// 12.1 handleRecordFeedback / handleQueryFeedback
// ---------------------------------------------------------------------------

func (s *Server) handleRecordFeedback(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	task := req.GetString("task", "")
	if task == "" {
		return mcp.NewToolResultError("task is required"), nil
	}

	useful := splitCSV(req.GetString("useful", ""))
	notNeeded := splitCSV(req.GetString("not_needed", ""))
	missing := splitCSV(req.GetString("missing", ""))

	if len(useful) == 0 && len(notNeeded) == 0 && len(missing) == 0 {
		return mcp.NewToolResultError("at least one of useful, not_needed, or missing must be provided"), nil
	}

	source := req.GetString("tool_source", "smart_context")

	entry := persistence.FeedbackEntry{
		Task:      task,
		Useful:    useful,
		NotNeeded: notNeeded,
		Missing:   missing,
		Source:    source,
	}

	if s.feedback == nil {
		return mcp.NewToolResultError("feedback storage not initialized (no cache directory)"), nil
	}

	if err := s.feedback.Record(entry); err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("failed to record feedback: %v", err)), nil
	}

	return s.respondJSONOrTOON(ctx, req, map[string]any{
		"recorded":         true,
		"useful_count":     len(useful),
		"not_needed_count": len(notNeeded),
		"missing_count":    len(missing),
	})
}

func (s *Server) handleQueryFeedback(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if s.feedback == nil || !s.feedback.HasData() {
		empty := map[string]any{
			"total_entries": 0,
			"accuracy":      0.0,
			"most_useful":   []any{},
			"most_missed":   []any{},
			"most_demoted":  []any{},
		}
		if s.isGCX(ctx, req) {
			return s.gcxResponseWithBudget(req)(encodeFeedbackQuery(empty))
		}
		if s.isTOON(ctx, req) {
			return returnTOON(empty)
		}
		return s.respondJSONOrTOON(ctx, req, empty)
	}

	topN := 10
	if n := req.GetInt("top_n", 0); n > 0 {
		topN = n
	}

	toolSource := req.GetString("tool_source", "all")

	stats := s.feedback.AggregatedStats(toolSource, topN)

	if isCompact(req) {
		var sb strings.Builder
		fmt.Fprintf(&sb, "Feedback: %v entries, %.0f%% accuracy\n",
			stats["total_entries"], stats["accuracy"].(float64)*100)
		return mcp.NewToolResultText(sb.String()), nil
	}

	if s.isGCX(ctx, req) {
		return s.gcxResponseWithBudget(req)(encodeFeedbackQuery(stats))
	}
	if s.isTOON(ctx, req) {
		return returnTOON(stats)
	}

	return s.respondJSONOrTOON(ctx, req, stats)
}

// splitCSV splits a comma-separated string into trimmed, non-empty parts.
func splitCSV(s string) []string {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	var result []string
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			result = append(result, p)
		}
	}
	return result
}

// ---------------------------------------------------------------------------
// 12.2 handleExportContext
// ---------------------------------------------------------------------------

func (s *Server) handleExportContext(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	// Delegate to smart_context for the raw data. Force `format: json`
	// on the inner call so the unmarshal below always sees JSON,
	// regardless of the caller's outer format preference or the
	// server's client-aware default (which auto-selects GCX1 for
	// known clients and would otherwise blow up our json.Unmarshal
	// with "invalid character 'G'").
	smartReq := req
	args, _ := smartReq.Params.Arguments.(map[string]any)
	innerArgs := make(map[string]any, len(args)+1)
	maps.Copy(innerArgs, args)
	innerArgs["format"] = "json"
	smartReq.Params.Arguments = innerArgs

	smartResult, err := s.handleSmartContext(ctx, smartReq)
	if err != nil {
		return nil, err
	}
	// If smart_context returned an error result, pass it through.
	if smartResult.IsError {
		return smartResult, nil
	}

	format := req.GetString("format", "markdown")
	tokenBudget := req.GetInt("token_budget", 2000)
	if tokenBudget <= 0 {
		tokenBudget = 2000
	}
	if tokenBudget > 8000 {
		tokenBudget = 8000
	}

	// Extract the JSON data from smart_context result.
	var data map[string]any
	for _, content := range smartResult.Content {
		if textContent, ok := content.(mcp.TextContent); ok {
			if jsonErr := json.Unmarshal([]byte(textContent.Text), &data); jsonErr != nil {
				return mcp.NewToolResultError(fmt.Sprintf("failed to parse smart_context output: %v", jsonErr)), nil
			}
			break
		}
	}
	if data == nil {
		return mcp.NewToolResultError("no data from smart_context"), nil
	}

	if format == "json" {
		return s.respondJSONOrTOON(ctx, req, data)
	}

	// Render as markdown briefing.
	md := renderContextMarkdown(data, tokenBudget)
	return mcp.NewToolResultText(md), nil
}

// markdownFenceLang picks the Markdown code-fence info string for an embedded
// source snippet. It prefers the symbol's indexed language (authoritative — the
// graph already resolved it during indexing) and falls back to deriving one
// from the file extension. An unrecognised language yields an empty info string
// (a plain fence) rather than a wrong one, so a snippet is never mislabelled.
func markdownFenceLang(language, filePath string) string {
	if lang := strings.TrimSpace(strings.ToLower(language)); lang != "" {
		return lang
	}
	return languageForExtension(filePath)
}

// contextSymbolEntries returns the symbol entries the briefing should render
// and how many the source pipeline omitted for budget. The graded-fidelity
// context_manifest is preferred when present — it carries the tiered,
// budget-packed source — otherwise the flat relevant_symbols list is used. In
// graded mode relevant_symbols is built without source, so falling back to it
// blindly would render an empty briefing.
func contextSymbolEntries(data map[string]any) (entries []any, omitted int) {
	if mani, ok := data["context_manifest"].(map[string]any); ok {
		if me, ok := mani["entries"].([]any); ok && len(me) > 0 {
			if o, ok := mani["omitted"].(float64); ok {
				omitted = int(o)
			}
			return me, omitted
		}
	}
	entries, _ = data["relevant_symbols"].([]any)
	return entries, 0
}

// renderSymbolEntries writes the "Key Symbols" body for a list of symbol
// entries. The entry shape is shared by the flat relevant_symbols list and the
// graded manifest, so manifest-only fields (tier, compressed) are rendered when
// present and skipped otherwise. Each source snippet is fenced with its own
// language rather than a hardcoded "go".
func renderSymbolEntries(sb *strings.Builder, entries []any, charBudget int) {
	for _, raw := range entries {
		em, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		name, _ := em["name"].(string)
		kind, _ := em["kind"].(string)
		id, _ := em["id"].(string)
		filePath, _ := em["file_path"].(string)
		language, _ := em["language"].(string)
		startLine, _ := em["start_line"].(float64)

		fmt.Fprintf(sb, "### `%s` (%s)\n\n", name, kind)
		fmt.Fprintf(sb, "- **ID:** `%s`\n", id)
		fmt.Fprintf(sb, "- **File:** `%s:%d`\n", filePath, int(startLine))
		if tier, ok := em["tier"].(string); ok && tier != "" {
			fmt.Fprintf(sb, "- **Tier:** %s\n", tier)
		}
		if sig, ok := em["signature"].(string); ok && sig != "" {
			fmt.Fprintf(sb, "- **Signature:** `%s`\n", sig)
		}

		// Include source if within budget. The fence language tracks the
		// symbol's own language rather than a hardcoded "go" so a snippet from
		// any indexed language is highlighted correctly.
		if source, ok := em["source"].(string); ok && source != "" {
			if sb.Len()+len(source) < charBudget {
				if compressed, _ := em["compressed"].(bool); compressed {
					sb.WriteString("- *(source compressed — bodies elided)*\n")
				}
				fmt.Fprintf(sb, "\n```%s\n", markdownFenceLang(language, filePath))
				sb.WriteString(source)
				sb.WriteString("\n```\n")
			} else {
				sb.WriteString("- *(source omitted — token budget exceeded)*\n")
			}
		}
		sb.WriteString("\n")
	}
}

// renderContextMarkdown converts smart_context JSON output into a self-contained
// markdown briefing suitable for sharing outside MCP.
func renderContextMarkdown(data map[string]any, tokenBudget int) string {
	var sb strings.Builder
	// Conservative char budget calibrated for cl100k_base on code-heavy input.
	charBudget := tokens.TokensToChars(tokenBudget)

	// Header.
	task, _ := data["task"].(string)
	sb.WriteString("# Context Briefing\n\n")
	fmt.Fprintf(&sb, "**Task:** %s\n\n", task)

	// Keywords.
	if kws, ok := data["keywords"].([]any); ok && len(kws) > 0 {
		sb.WriteString("**Keywords:** ")
		for i, kw := range kws {
			if i > 0 {
				sb.WriteString(", ")
			}
			fmt.Fprintf(&sb, "`%v`", kw)
		}
		sb.WriteString("\n\n")
	}

	// Key symbols. The graded-fidelity manifest carries the richest
	// (tiered, budget-packed) source set; prefer it when present and fall
	// back to the flat relevant_symbols list otherwise. Both share the same
	// entry shape, so one renderer handles either — without this, a graded
	// briefing showed no source at all, because the flat list it falls back
	// to is built source-less in graded mode.
	if symbols, omitted := contextSymbolEntries(data); len(symbols) > 0 {
		sb.WriteString("## Key Symbols\n\n")
		renderSymbolEntries(&sb, symbols, charBudget)
		if omitted > 0 {
			fmt.Fprintf(&sb, "*(%d more symbol(s) omitted — token budget)*\n\n", omitted)
		}
	}

	// Callers and callees.
	if callers, ok := data["callers"].([]any); ok && len(callers) > 0 {
		sb.WriteString("## Callers\n\n")
		for _, c := range callers {
			fmt.Fprintf(&sb, "- `%v`\n", c)
		}
		sb.WriteString("\n")
	}

	if callees, ok := data["callees"].([]any); ok && len(callees) > 0 {
		sb.WriteString("## Callees\n\n")
		for _, c := range callees {
			fmt.Fprintf(&sb, "- `%v`\n", c)
		}
		sb.WriteString("\n")
	}

	// Cross-repo dependencies.
	if crossDeps, ok := data["cross_repo_dependencies"].([]any); ok && len(crossDeps) > 0 {
		sb.WriteString("## Cross-Repo Dependencies\n\n")
		for _, dep := range crossDeps {
			depMap, ok := dep.(map[string]any)
			if !ok {
				continue
			}
			name, _ := depMap["name"].(string)
			repo, _ := depMap["repo_prefix"].(string)
			edgeKind, _ := depMap["edge_kind"].(string)
			fmt.Fprintf(&sb, "- `%s` (repo: %s, %s)\n", name, repo, edgeKind)
		}
		sb.WriteString("\n")
	}

	// Test files.
	if tests, ok := data["related_test_files"].([]any); ok && len(tests) > 0 {
		sb.WriteString("## Related Tests\n\n")
		for _, t := range tests {
			fmt.Fprintf(&sb, "- `%v`\n", t)
		}
		sb.WriteString("\n")
	}

	// Files to edit.
	if files, ok := data["files_to_edit"].([]any); ok && len(files) > 0 {
		sb.WriteString("## Files to Edit\n\n")
		for _, f := range files {
			fmt.Fprintf(&sb, "- `%v`\n", f)
		}
		sb.WriteString("\n")
	}

	// Footer.
	sb.WriteString("---\n*Generated by `gortex export_context`*\n")

	return sb.String()
}

// ---------------------------------------------------------------------------
// 13.2 handleAuditAgentConfig — scans CLAUDE.md / AGENTS.md / Cursor rules /
// Copilot instructions for stale symbol refs, dead file paths, and bloat.
// ---------------------------------------------------------------------------

func (s *Server) handleAuditAgentConfig(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	root := req.GetString("root", "")
	if root == "" {
		if s.indexer != nil {
			root = s.indexer.RootPath()
		}
	}
	if root == "" {
		if cwd, err := os.Getwd(); err == nil {
			root = cwd
		}
	}
	if root == "" {
		return mcp.NewToolResultError("could not determine repo root — pass 'root' argument"), nil
	}

	var files []string
	if filesArg := req.GetString("files", ""); filesArg != "" {
		for _, f := range strings.Split(filesArg, ",") {
			f = strings.TrimSpace(f)
			if f != "" {
				files = append(files, f)
			}
		}
	} else {
		files = audit.DiscoverConfigFiles(root)
	}

	if len(files) == 0 {
		return s.respondJSONOrTOON(ctx, req, map[string]any{
			"files_scanned": 0,
			"message":       "no agent config files found",
		})
	}

	// The audit scans config files on disk against the indexed corpus, so its
	// stale-ref verdicts are computed over the base corpus even when the
	// request carries an overlay or a routed view, and say so under one.
	report := audit.Audit(s.graph, root, files)
	annotateBaseScoped(ctx, graphview.CapSyntaxGraph)

	if isCompact(req) {
		var b strings.Builder
		fmt.Fprintf(&b, "scanned=%d stale=%d dead=%d bloat=%d\n",
			report.FilesScanned, len(report.StaleRefs), len(report.DeadPaths), report.BloatScore)
		for _, r := range report.StaleRefs {
			fmt.Fprintf(&b, "stale %s:%d `%s`\n", r.File, r.Line, r.Token)
		}
		for _, d := range report.DeadPaths {
			fmt.Fprintf(&b, "dead %s:%d `%s`\n", d.File, d.Line, d.Path)
		}
		for _, f := range report.Files {
			if f.Bloat.Score >= 40 {
				fmt.Fprintf(&b, "bloat %s score=%d lines=%d dup=%d\n",
					f.File, f.Bloat.Score, f.Bloat.Lines, f.Bloat.Duplicates)
			}
		}
		return mcp.NewToolResultText(b.String()), nil
	}

	return s.respondJSONOrTOON(ctx, req, report)
}

// coverageByID batch-loads the coverage sidecar off the base store.
// Handlers that must honour the caller's buffers pass their request
// reader to coverageRowsByID instead.
func (s *Server) coverageByID() map[string]graph.CoverageEnrichment {
	return coverageRowsByID(s.graph)
}

// coverageRowsByID batch-loads the coverage sidecar (change A) into an
// id->row map; nil when the reader lacks the capability (callers then
// fall back to Node.Meta). One read per handler call, not per-node.
//
// An overlay view has no sidecar, so an overlay-active request gets nil
// and each row falls back to the node's own meta — the buffer's symbols
// simply carry no coverage rather than borrowing the indexed numbers.
func coverageRowsByID(g graph.Reader) map[string]graph.CoverageEnrichment {
	r, ok := g.(graph.CoverageEnrichmentReader)
	if !ok {
		return nil
	}
	rows := r.CoverageRows("")
	m := make(map[string]graph.CoverageEnrichment, len(rows))
	for _, e := range rows {
		m[e.NodeID] = e
	}
	return m
}

// coveragePctFrom returns a node's coverage %, preferring the sidecar map
// and falling back to Meta["coverage_pct"] for un-migrated DBs.
func coveragePctFrom(cov map[string]graph.CoverageEnrichment, n *graph.Node) (float64, bool) {
	if e, ok := cov[n.ID]; ok {
		return e.CoveragePct, true
	}
	if pct, ok := n.Meta["coverage_pct"].(float64); ok {
		return pct, true
	}
	return 0, false
}

// releaseRowsByID batch-loads the release sidecar (change A) into an
// id->tag map; nil when the reader lacks the capability. An overlay
// view has none, so an overlay-active request falls back to each
// node's meta.
func releaseRowsByID(g graph.Reader) map[string]string {
	r, ok := g.(graph.ReleaseEnrichmentReader)
	if !ok {
		return nil
	}
	rows := r.ReleaseRows("")
	m := make(map[string]string, len(rows))
	for _, e := range rows {
		m[e.NodeID] = e.AddedIn
	}
	return m
}

// addedInFrom returns a node's "added_in" tag, preferring the sidecar
// map and falling back to Meta["added_in"] for un-migrated DBs.
func addedInFrom(rel map[string]string, n *graph.Node) (string, bool) {
	if tag, ok := rel[n.ID]; ok {
		return tag, true
	}
	if n.Meta != nil {
		if tag, ok := n.Meta["added_in"].(string); ok {
			return tag, true
		}
	}
	return "", false
}

// blameRowsByID batch-loads the blame sidecar (change A) into an
// id->row map; nil when the backend lacks the capability.
func blameRowsByID(g graph.Reader) map[string]graph.BlameEnrichment {
	r, ok := g.(graph.BlameEnrichmentReader)
	if !ok {
		return nil
	}
	rows := r.BlameRows("")
	m := make(map[string]graph.BlameEnrichment, len(rows))
	for _, e := range rows {
		m[e.NodeID] = e
	}
	return m
}

// lastAuthoredFrom returns a node's blame, preferring the sidecar map and
// falling back to Meta["last_authored"] for un-migrated DBs.
func lastAuthoredFrom(blame map[string]graph.BlameEnrichment, n *graph.Node) (graph.BlameEnrichment, bool) {
	if e, ok := blame[n.ID]; ok {
		return e, true
	}
	if n.Meta != nil {
		if la, ok := n.Meta["last_authored"].(map[string]any); ok {
			e := graph.BlameEnrichment{NodeID: n.ID}
			e.Commit, _ = la["commit"].(string)
			e.Email, _ = la["email"].(string)
			e.Timestamp = tsFromMeta(la["timestamp"])
			return e, true
		}
	}
	return graph.BlameEnrichment{}, false
}

// lastAuthoredTSFrom is the timestamp-only convenience over lastAuthoredFrom.
func lastAuthoredTSFrom(blame map[string]graph.BlameEnrichment, n *graph.Node) (int64, bool) {
	if e, ok := lastAuthoredFrom(blame, n); ok && e.Timestamp != 0 {
		return e.Timestamp, true
	}
	return 0, false
}
