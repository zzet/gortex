package mcp

import "strings"

// analyzeKinds is the single source of truth for every `kind` the
// `analyze` dispatcher (handleAnalyze, tools_enhancements.go) accepts.
// It is the exact, sorted set of every `case` label in that switch —
// including the two that share one case (sast, hygiene) and every alias
// (review, domain, dbt_models, concepts, …). The AST-based anti-drift
// test in analyze_kinds_test.go asserts this slice equals the switch's
// case set exactly, so the two can never silently diverge.
//
// Keep it sorted: AnalyzeKinds returns a copy that callers may rely on
// being in stable order, and analyze_kinds_test.go asserts sortedness.
var analyzeKinds = []string{
	"annotation_users",
	"blame",
	"bottlenecks",
	"cgo_users",
	"channel_ops",
	"clusters",
	"components",
	"concepts",
	"config_readers",
	"connectivity_health",
	"constructors_missing_fields",
	"coverage",
	"coverage_gaps",
	"coverage_summary",
	"cross_repo",
	"cycles",
	"dbt_models",
	"dead_code",
	"def_use",
	"doc_staleness",
	"domain",
	"drupal_hooks",
	"edge_audit",
	"env_var_users",
	"error_surface",
	"event_emitters",
	"external_calls",
	"field_writers",
	"fixes_history",
	"goroutine_spawns",
	"health_score",
	"hotspots",
	"hygiene",
	"images",
	"impact",
	"indirect_mutations",
	"k8s_resources",
	"kcore",
	"kustomize",
	"log_events",
	"louvain",
	"models",
	"named",
	"orphan_tables",
	"ownership",
	"pagerank",
	"pubsub",
	"race_writes",
	"ref_facts",
	"releases",
	"resolution_outcomes",
	"retrieval_log",
	"review",
	"role",
	"route_frameworks",
	"routes",
	"sast",
	"scc",
	"speculative",
	"sql_call_sites",
	"sql_rebuild",
	"stale_code",
	"stale_flags",
	"string_emitters",
	"suggest_boundaries",
	"swiftui_views",
	"synthesizers",
	"temporal_orphans",
	"temporal_verify",
	"tests_as_edges",
	"todos",
	"uikit_classes",
	"unclosed_channels",
	"unreferenced_tables",
	"unsafe_patterns",
	"wasm_users",
	"wcc",
	"would_create_cycle",
}

// analyzeScopeAwareKinds is the set of analyze kinds whose result rows
// are genuinely narrowed by the uniform repo/project/workspace/scope
// overrides in v1. Three tiers populate it:
//
//   - AUTO: kinds that obtain their working node set through the
//     scopedNodes / scopedNodesByKinds / scopedNodeSlice accessors, so
//     RepoAllow narrows them centrally with no per-handler code.
//   - Per-row-filtered: kinds that read s.graph directly but gate every
//     emitted row on analyzeNodeVisible (workspace ceiling + optional
//     repo allow-set) — the three flagship kinds (dead_code, hotspots,
//     cycles) plus the category-(a) edge-walk / graph-algorithm /
//     framework kinds that prune their rows and re-tally their counts.
//   - File-path-filtered: the category-(c) file/AST scans (sast, hygiene,
//     review, domain, named, unsafe_patterns), already narrowed by
//     resolveRepoFilter / buildASTTargets before the handler runs.
//
// handleAnalyze consults this set so that when a caller asks to narrow
// (resolved.RepoAllow != nil) but picks a kind that is NOT in the set,
// the response carries a `scope_note` disclosing that the kind is not
// repo-narrowed in v1 (a community / git-mining / per-id / synthesizer
// kind). This keeps the uniform `scope_applied` truthful while flagging
// the remaining long-tail no-op kinds — "no silent no-ops".
var analyzeScopeAwareKinds = map[string]bool{
	// AUTO — narrowed centrally by the scoped-node accessors.
	"todos":                       true,
	"stale_code":                  true,
	"ownership":                   true,
	"coverage_gaps":               true,
	"stale_flags":                 true,
	"cgo_users":                   true,
	"wasm_users":                  true,
	"orphan_tables":               true,
	"unreferenced_tables":         true,
	"coverage_summary":            true,
	"health_score":                true,
	"external_calls":              true,
	"k8s_resources":               true,
	"images":                      true,
	"kustomize":                   true,
	"dbt_models":                  true,
	"role":                        true,
	"constructors_missing_fields": true,
	"impact":                      true,
	"bottlenecks":                 true,
	"connectivity_health":         true,
	// Tier-2 — bypass the accessors but filtered via analyzeNodeVisible.
	"dead_code": true,
	"hotspots":  true,
	"cycles":    true,
	// releases reads s.graph directly (NOT via the scoped-node accessors);
	// per-row filtered via analyzeNodeVisible like the three flagship kinds.
	"releases": true,
	// Category (a) — per-row visibility filtered (workspace + repo allow-set).
	"channel_ops":        true,
	"goroutine_spawns":   true,
	"field_writers":      true,
	"indirect_mutations": true,
	"config_readers":     true,
	"env_var_users":      true,
	"event_emitters":     true,
	"pubsub":             true,
	"error_surface":      true,
	"speculative":        true,
	"ref_facts":          true,
	"annotation_users":   true,
	"race_writes":        true,
	"unclosed_channels":  true,
	"log_events":         true,
	"sql_call_sites":     true,
	"string_emitters":    true,
	"routes":             true,
	"route_frameworks":   true,
	"swiftui_views":      true,
	"uikit_classes":      true,
	"drupal_hooks":       true,
	"models":             true,
	"components":         true,
	"pagerank":           true,
	"louvain":            true,
	"kcore":              true,
	"wcc":                true,
	"scc":                true,
	"edge_audit":         true,
	"tests_as_edges":     true,
	"doc_staleness":      true,
	"temporal_orphans":   true,
	// Category (c) — file/AST scans, already narrowed via resolveRepoFilter/buildASTTargets.
	"sast":            true,
	"hygiene":         true,
	"review":          true,
	"domain":          true,
	"named":           true,
	"unsafe_patterns": true,
}

// analyzeWorkspaceClampedKinds are the not-repo-narrowed analyze kinds
// (community detection + synthesizer enumeration) that now clamp their
// rows to the session workspace and self-disclose that via a body-
// visible `scope` block (workspaceScopeBlock). They are excluded from
// the _meta stampScopeNote path, whose text predates the clamp and would
// otherwise wrongly claim the result "may span the entire index / all
// workspaces".
var analyzeWorkspaceClampedKinds = map[string]bool{
	"clusters":           true,
	"concepts":           true,
	"suggest_boundaries": true,
	"synthesizers":       true,
}

// AnalyzeKinds returns a defensive copy of the canonical analyze-kind
// set, in sorted order. Callers must not mutate the returned slice's
// backing array of the package-level source.
func AnalyzeKinds() []string {
	out := make([]string, len(analyzeKinds))
	copy(out, analyzeKinds)
	return out
}

// analyzeKindsCSV returns the canonical analyze kinds comma-joined, for
// interpolation into tool descriptions and the dispatcher's error
// strings so those lists always match the switch exactly.
func analyzeKindsCSV() string {
	return strings.Join(analyzeKinds, ", ")
}

// analyzeKindDescriptions is a one-line summary for every analyze kind,
// so `gortex analyze kinds` is self-documenting and a user need not reach
// for docs/source to learn what a kind does. It must stay in lock-step
// with analyzeKinds: the anti-drift test in analyze_kinds_test.go asserts
// every kind has a non-empty description here and that no description key
// is an orphan (a kind that no longer exists). Keep each value to a single
// terse line — the CLI aligns them into a two-column reference listing.
var analyzeKindDescriptions = map[string]string{
	"annotation_users":            "EdgeAnnotated rollup — sites using an annotation/decorator; scope with id/name (e.g. @Deprecated)",
	"blame":                       "Stamp meta.last_authored on every blame-eligible node from git blame",
	"bottlenecks":                 "Rank functions by computation-bottleneck risk — complexity, loop depth, transitive/hidden-O(n^k) nesting, unguarded recursion",
	"cgo_users":                   "Files that import C (uses_cgo edge)",
	"channel_ops":                 "Channels grouped by sends/receives — producer/consumer mismatches",
	"clusters":                    "Community detection as an analyzer (algorithm=leiden/louvain/spectral, min_size)",
	"components":                  "UI parent↔child fan-in/out from render edges (JSX/TSX + Phoenix HEEx)",
	"concepts":                    "Concept clusters mined over the graph",
	"config_readers":              "config_key nodes grouped by reads_config edges; name filter",
	"connectivity_health":         "Graph-extraction quality — isolated nodes, leaf/source/sink counts, dead-weight by file",
	"constructors_missing_fields": "Constructors that leave one or more struct fields unset",
	"coverage":                    "Stamp meta.coverage_pct on executable symbols from a cover profile",
	"coverage_gaps":               "Symbols inside [min_pct, max_pct) — undertested code",
	"coverage_summary":            "Per-directory coverage rollup (avg, covered, partial, uncovered)",
	"cross_repo":                  "Repo-boundary-crossing call/implements/extends edges grouped by repo pair + relation",
	"cycles":                      "Dependency cycles via Tarjan's SCC, with severity classification",
	"dbt_models":                  "dbt/SQLMesh models, seeds, snapshots, sources with column count + lineage fan-in/out",
	"dead_code":                   "Symbols with zero incoming edges (excludes entry points, tests, exports)",
	"def_use":                     "Per-function reaching-definition def→use chains over the on-demand CFG (pairs with get_cfg)",
	"doc_staleness":               "Docs/decisions whose content→code links have gone dangling or pending — stale references",
	"domain":                      "Results of pluggable TOML domain-extractor rules — project-specific node/edge kinds",
	"drupal_hooks":                "Detected Drupal hook implementations grouped by the hook they implement",
	"edge_audit":                  "Graph-completeness / edge-sanity diagnostic — missing or suspect edges",
	"env_var_users":               "reads_config edges restricted to env-var keys, grouped by variable",
	"error_surface":               "Function/method nodes with their throws targets — refactor blast radius",
	"event_emitters":              "Event/log/metric emit sites grouped by emit edges; level/name filters",
	"external_calls":              "Stdlib/module-cache attribution — module rollup of call/symbol counts",
	"field_writers":               "Mutability hotspots — fields ranked by write edges; id scopes to one field",
	"fixes_history":               "Mine git for bug-fix commits and rank fix-prone files",
	"goroutine_spawns":            "Spawn edges grouped by spawned target + mode (goroutine/async/promise)",
	"health_score":                "Composite per-symbol health 0-100 + A–F grade (coverage/complexity/recency/churn)",
	"hotspots":                    "Over-coupled symbols ranked by fan-in, fan-out, and community crossings",
	"hygiene":                     "SAST hygiene scan (alias of sast) — CWE/OWASP-tagged rules across 8 languages",
	"images":                      "Container images (Dockerfile FROM / K8s container.image) with consumer count",
	"impact":                      "Composite 0-100 change-impact score + risk label over 5 axes (PageRank, reach, complexity, co-change, span); pass target:{symbol} to rank that symbol's blast radius instead of the whole repo",
	"indirect_mutations":          "Fields mutated indirectly via a method/sibling call on the receiver — surfaces the via method",
	"k8s_resources":               "Kubernetes resource fan-out (depends_on/configures/mounts/exposes/uses_env); k8s_kind/namespace filters",
	"kcore":                       "k-core decomposition — the densely-connected graph core by k-degree (the infrastructure every layer leans on)",
	"kustomize":                   "Kustomize overlay tree with base/resource fan-out; dir filter",
	"log_events":                  "Logging-call sites surfaced as events",
	"louvain":                     "Louvain community partitioning (typically more granular than the Leiden clusters kind)",
	"models":                      "ORM model→table edges (gorm/SQLAlchemy/Django/ActiveRecord/JPA/TypeORM/Ecto); orm/table/model filters",
	"named":                       "Run a named query bundle — built-ins cover sql-injection/xss/ssrf/hardcoded-secrets/…; repo bundles from .gortex.yaml",
	"orphan_tables":               "Tables queried but missing a migration that provides them",
	"ownership":                   "Per-author rollup with symbol/file counts and oldest/newest timestamps. An empty or partial answer carries data_state saying whether blame was ever mined for the scope — a zero without it is not absence evidence",
	"pagerank":                    "PageRank centrality — symbols ranked by random-walk authority",
	"pubsub":                      "Pub/sub topics with publishers + subscribers (NATS/Kafka/RabbitMQ/Redis/EventEmitter/Socket.IO)",
	"race_writes":                 "Concurrent-write race detection",
	"ref_facts":                   "Resolved-reference facts — each reference edge + the provenance tier that resolved it",
	"releases":                    "Read the precomputed release timeline and per-tag files from graph metadata",
	"resolution_outcomes":         "Classify unresolved call/ref edges by why the resolver gave up (ambiguous/out-of-scope/cross-language/stub/no-def)",
	"retrieval_log":               "Mine the retrieval query log — zero-result queries + per-tool latency/result-size rollups for recall tuning",
	"review":                      "Idiomatic/correctness rulepack (NPE, thread-safety check-then-act, N+1, logic-error; Go+Python) — the engine behind gortex review",
	"role":                        "Per-symbol architectural-role classification",
	"route_frameworks":            "Registered structural route passes + route-contract node count per framework",
	"routes":                      "Handler↔route pairs from route edges (HTTP/gRPC/WS/GraphQL/topic); method/path/type filters",
	"sast":                        "Bandit-parity SAST — 190+ rules across 8 languages, CWE/OWASP tagged; severity/cwe/tag/detector filters",
	"scc":                         "Strongly connected components of the directed graph",
	"speculative":                 "Audit best-guess speculative dynamic-dispatch edges grouped by shape with a candidate histogram",
	"sql_call_sites":              "Query edges grouped by calling symbol with table read/write split",
	"sql_rebuild":                 "Re-derive the SQL table layer from the string-literal registry",
	"stale_code":                  "Symbols whose last-author timestamp is older than older_than days",
	"stale_flags":                 "Feature flags whose every toggling caller is older than older_than days",
	"string_emitters":             "String-literal emission sites surfaced as events",
	"suggest_boundaries":          "Seed an architecture: block from detected Leiden communities — a ready-to-paste .gortex.yaml layer map",
	"swiftui_views":               "SwiftUI types grouped by classified role (component/app_entry)",
	"synthesizers":                "Framework-dispatch-synthesized edges grouped by the pass that produced them",
	"temporal_orphans":            "Temporal call-graph integrity — broken dispatches, handler-less signals/queries, undispatched activities/workflows",
	"temporal_verify":             "LLM cleaning pass over Temporal dispatch edges (requires a configured LLM provider)",
	"tests_as_edges":              "View over the test→code edge layer; group_by symbol or test",
	"todos":                       "TODO/FIXME/HACK/XXX/NOTE nodes; filter by tag/assignee/ticket",
	"uikit_classes":               "UIKit types grouped by classified role (view_controller/view/cell)",
	"unclosed_channels":           "Channels that are opened but never closed",
	"unreferenced_tables":         "Tables provided by a migration but with zero queries against them",
	"unsafe_patterns":             "Panic-prone / undefined-behavior primitive scan across all languages",
	"wasm_users":                  "Files that use #[wasm_bindgen] (uses_wasm_bindgen edge)",
	"wcc":                         "Weakly connected components of the graph",
	"would_create_cycle":          "Pre-flight check before adding a dependency — would the new edge close a cycle?",
}

// AnalyzeKindDescription returns the one-line summary for an analyze kind,
// or the empty string if kind is not a canonical analyze kind. Every kind
// in AnalyzeKinds() is guaranteed (by analyze_kinds_test.go) to have a
// non-empty description.
func AnalyzeKindDescription(kind string) string {
	return analyzeKindDescriptions[kind]
}
