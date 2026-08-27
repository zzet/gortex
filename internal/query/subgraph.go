package query

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/search/rerank"
)

// SubGraph is a JSON-serializable result from a graph query.
type SubGraph struct {
	Nodes      []*graph.Node `json:"nodes"`
	Edges      []*graph.Edge `json:"edges"`
	TotalNodes int           `json:"total_nodes"`
	TotalEdges int           `json:"total_edges"`
	Truncated  bool          `json:"truncated"`
	// TextMatchedSuppressed counts name-only (text_matched) edges dropped
	// by the adaptive default: once the result carries resolver-verified
	// evidence for the symbol, the name-only fan-out is redundant noise.
	// Zero — and omitted — when nothing was suppressed. Re-run with
	// min_tier:"text_matched" to include the hidden rows.
	TextMatchedSuppressed int `json:"text_matched_suppressed,omitempty"`
	// NameOnlyCandidates counts call sites that name this symbol but never
	// bound to it — the calls parked on a bare-name `unresolved::`
	// placeholder because no receiver-type evidence tied them here (or
	// because the cross-package guard reverted a weak guess). They are
	// NOT usages: each may belong to a different same-named symbol. But a
	// zero-or-thin edge list beside a non-zero count is an honestly
	// uncertain answer ("1 verified, 22 unverified") where the count's
	// absence read as proof of safety. Re-run with min_tier:"text_matched"
	// to get the rows themselves. Exact even when the rows are capped.
	NameOnlyCandidates int `json:"name_only_candidates,omitempty"`
	// SuppressionCaveat is attached by the adaptive text_matched default
	// (find_usages / get_callers) when TextMatchedSuppressed > 0 AND the
	// target's file was re-parsed on the live watch path without re-running
	// semantic enrichment — so the resolver-verified edges that triggered
	// suppression may be below the tier the enrichment pass would mint, and
	// the hidden name-only usages could be the real ones. Empty (omitted)
	// otherwise. Independent of Caveat, which only fires for a zero-edge
	// result — and since suppression only runs when a stronger edge exists,
	// the two never coexist.
	SuppressionCaveat string `json:"suppression_caveat,omitempty"`
	// RelatedTools is a one-line, additive completeness cue naming a
	// deferred tool whose trigger the response content just matched (e.g. a
	// find_usages on a dispatch-heavy interface → find_implementations). It
	// is emitted at most once per tool per session so a discovery hint
	// surfaces without repeating. Empty (omitted) when nothing matched or the
	// cue was already shown this session.
	RelatedTools string `json:"related_tools,omitempty"`
	// Caveat marks an edge-returning query (find_usages, get_callers)
	// whose answer must not be taken at face value. Two cases produce
	// one: the result came back with no edges, classifying whether that
	// reflects genuinely unused code or an extraction/coverage gap; or
	// the result is populated but every usage in it is a name-only
	// match, so the rows may all be false positives. Nil — and omitted
	// from the response — for any result carrying real usage evidence.
	Caveat *graph.ZeroEdgeCaveat `json:"caveat,omitempty"`
	// TierFiltered is attached when a min_tier filter dropped edges while
	// lower-tier edges still existed — so a min_tier that empties the result
	// is legible as "filtered", not "no usages". Set by FilterByMinTier;
	// omitted when no min_tier was applied or nothing was below the tier.
	TierFiltered *graph.TierFilteredCaveat `json:"tier_filtered,omitempty"`
	// CallerNotes carries concurrency-safety annotations keyed by node
	// ID. Populated only by get_callers (which classifies each caller);
	// other traversal tools share this struct and leave it nil, so it
	// is omitted from their responses. A node appears here only when at
	// least one concurrency flag is set, so an absent entry means
	// "neither sync_guarded nor cross_concurrent".
	CallerNotes map[string]*graph.ConcurrencyAnnotation `json:"caller_notes,omitempty"`
	// BudgetHit is set by token-budgeted traversals (WalkBudgeted) when
	// the walk stopped because the estimated encoded size of the result
	// reached the caller's token budget. False — and omitted — for a
	// traversal that completed within budget or never imposed one.
	BudgetHit bool `json:"budget_hit,omitempty"`
	// StoppedAtDepth records the BFS depth the budgeted traversal had
	// reached when it stopped — either the deepest depth fully expanded,
	// or the depth at which the budget / depth cap halted expansion.
	// Zero — and omitted — for traversals that don't track depth.
	StoppedAtDepth int `json:"stopped_at_depth,omitempty"`
	// LastSynced is the time the stalest federation proxy node in this
	// result was last pulled from its owning remote. Set only when the
	// traversal crossed into a remote-owned proxy node; omitted (and nil)
	// for a purely-local result, so a caller can see how fresh the
	// remote-derived part of the answer is.
	LastSynced *time.Time `json:"last_synced,omitempty"`
	// LowerBound is set by call-graph traversals (get_call_chain) when the
	// walk dropped one or more dynamic-dispatch / unresolved out-edges: the
	// reachable set is then a floor, not exhaustive. Omitted when false.
	LowerBound bool `json:"lower_bound,omitempty"`
	// DynamicBoundaries enriches a dispatch-bounded result with the body-level
	// {site, form, key, candidate-shortlist} of each runtime-dispatch site in
	// the seed symbol — so an agent reads "static path ends HERE, form X, key
	// Y, candidates A/B" instead of spiralling through the source. Computed on
	// demand (find_usages / smart_context / explore); never persisted.
	DynamicBoundaries []graph.DynamicBoundary `json:"dynamic_boundaries,omitempty"`
	// Boundaries names the unresolved/dispatch sites that made the result a
	// floor. Populated only by call-graph traversals; omitted when empty.
	Boundaries []graph.EpistemicBoundary `json:"boundaries,omitempty"`
	// UsageSummary is a compact completeness rollup attached only by
	// find_usages: the total reference count, the number of distinct
	// files those references span, and the test-file share. Nil — and
	// omitted — for every other traversal that shares this struct, and
	// for an empty result (the Caveat already explains that case). Lets
	// an agent see at a glance whether the usage list already covers
	// tests instead of re-grepping *_test.go files to find out.
	UsageSummary *UsageSummary `json:"usage_summary,omitempty"`
}

// UsageSummary is the compact completeness rollup on a find_usages
// result: NRefs total references, spread across NFiles distinct files,
// of which NTestRefs originate in test files. It is derived from the
// same edges and per-node test classification as the per-usage rows, so
// the rollup never disagrees with the references it summarizes.
type UsageSummary struct {
	NRefs     int `json:"n_refs" toon:"n_refs"`
	NFiles    int `json:"n_files" toon:"n_files"`
	NTestRefs int `json:"n_test_refs" toon:"n_test_refs"`
}

// QueryOptions controls traversal depth, result limits, and detail level.
type QueryOptions struct {
	Depth   int    `json:"depth"`
	Limit   int    `json:"limit"`
	Detail  string `json:"detail"`             // "brief" or "full"
	MinTier string `json:"min_tier,omitempty"` // see graph.Origin* constants; "" = no filter
	// WorkspaceID, when set, restricts traversal to nodes whose
	// effective workspace (Node.WorkspaceID || Node.RepoPrefix
	// fallback) equals this slug. Empty disables the filter —
	// preserves the legacy global-graph behaviour for callers that
	// don't care about the workspace boundary.
	WorkspaceID string `json:"workspace_id,omitempty"`
	// ProjectID applies the same scoping for the soft sub-boundary.
	// Honoured only when WorkspaceID is also set; on its own it would
	// be ambiguous (two workspaces could declare a project with the
	// same name).
	ProjectID string `json:"project_id,omitempty"`
	// RepoAllow, when non-empty, restricts traversal to nodes whose
	// RepoPrefix is present in the allow-set. Nil or empty preserves
	// the legacy no-repo-filter behaviour. This is intentionally a
	// soft breadth control inside any workspace boundary, not a
	// replacement for caller-side workspace isolation.
	RepoAllow map[string]bool `json:"repo_allow,omitempty"`
	// ExcludeTests, when true, drops edges originating in test code —
	// nodes flagged by the indexer's test-edge pass (Node.Meta["is_test"]
	// = true) plus unflagged node kinds whose file path matches the
	// canonical test predicate (see isTestSource). Lets find_usages /
	// get_callers answer "who depends on X *in production*" without
	// test-noise dilution.
	ExcludeTests bool `json:"exclude_tests,omitempty"`

	// IncludeDispatch makes a forward call-graph walk (get_call_chain /
	// trace / callees) polymorphic-dispatch aware: when the chain reaches an
	// interface / abstract method, it also expands through that method's
	// EdgeOverrides in-edges to the concrete implementations that override
	// it. The dedicated override edges are recorded AS-IS — never synthesized
	// into fake `calls` edges — so find_implementations / get_class_hierarchy
	// stay precise while a trace auto-reaches the impls. Off by default.
	IncludeDispatch bool `json:"include_dispatch,omitempty"`
	// DispatchMinTier gates which override edges qualify for dispatch
	// expansion by minimum provenance tier (see graph.Origin* constants);
	// "" admits every override edge. Lets a caller demand, e.g., only
	// LSP-confirmed overrides — a min_tier control codegraph cannot offer.
	DispatchMinTier string `json:"dispatch_min_tier,omitempty"`
	// DispatchFanout caps how many overriders a single method expands to
	// (0 → defaultDispatchFanout), bounding the blow-up on a hub interface
	// implemented by hundreds of types.
	DispatchFanout int `json:"dispatch_fanout,omitempty"`

	// SearchTimings, when non-nil, is populated by the search hot path
	// (SearchSymbolsScoped → gatherBackendCandidates) with per-phase
	// wall-clock breakdowns. Used by the MCP search_symbols handler's
	// debug log line; nil disables instrumentation. Single-call: the
	// caller MUST hand a fresh struct per query (the engine does not
	// reset). Never serialised — `json:"-"` keeps the option struct
	// JSON shape stable.
	SearchTimings *SearchTimings `json:"-"`

	// RerankContext is the optional rerank context the engine uses when
	// gathering bundle candidates: each bundle's in/out edges are
	// seeded into the context's edge caches so the handler-side
	// rerank.Pipeline.Rerank can skip its own batched edge fetch on
	// the merged candidate set. Pass nil — the engine's gather path
	// still works, the bundle's edges are just discarded after the
	// per-call rerank. Never serialised.
	RerankContext *rerank.Context `json:"-"`

	// SkipInnerRerank, when true, makes SearchSymbolsRanked skip its
	// own per-call rerank.Pipeline.Rerank pass. Callers that fan a
	// search across N expansion terms and merge the results themselves
	// (the MCP search_symbols handler) re-run the rerank once on the
	// merged candidate set with the full session-aware context — the
	// inner per-call rerank is wasted work whose output is mostly
	// discarded by the merge. Flipping this on collapses N+1
	// engine-side rerank invocations to zero. The merge-side rerank
	// is the source of truth either way.
	SkipInnerRerank bool `json:"-"`

	// SkipVectorChannel, when true, makes gatherBackendCandidates skip
	// the vector channel entirely — no embedder call, no ANN search.
	// Set by the MCP search_symbols handler on identifier-shape queries
	// (QueryClassSymbol / QueryClassPath / QueryClassSignature) where
	// the rerank's classWeightTable already proves the semantic
	// channel contributes near-zero useful signal (multipliers 0.65 /
	// 0.45 / 0.80 vs the baseline 1.00 for concept). Saves the embed
	// + vector search round-trip on the common-case identifier lookup.
	// The bundle path's vector-only branch and the legacy
	// SearchChannels path both honour this flag.
	SkipVectorChannel bool `json:"-"`

	// SkipExactNameSplice, when true, makes gatherBackendCandidates
	// skip the FindNodesByName(query) splice-in. Set by callers that
	// know the query string cannot match any exact node name — the
	// fetchAndMergeBM25 fan-out's combined-OR call is the canonical
	// case: a concatenated bag of expansion terms ("NewServer
	// StartServer Server.Init …") can't be the literal Name of any
	// node, so the FindNodesByName query round-trip is wasted work.
	// The primary query still runs the splice.
	SkipExactNameSplice bool `json:"-"`

	// CosineRerank, when true, runs the post-rerank exact-cosine
	// refinement stage after SearchSymbolsRanked's per-call rerank:
	// the top candidates are re-ordered by exact cosine similarity
	// between the query embedding and each candidate's stored
	// embedding, recovering the precise semantic distance the
	// rank-based SemanticSignal discards. The stage is a strict no-op
	// when the vector channel is inactive (no embedder, no stored
	// vectors, query fails to embed), so it can never regress a
	// text-only search. Honoured only when the engine has not been
	// asked to skip its inner rerank — the production handler runs
	// the refinement itself against its merged candidate set.
	CosineRerank bool `json:"-"`

	// CosineTopN bounds how many of the top ranked candidates the
	// cosine refinement re-scores. Zero uses the package default.
	CosineTopN int `json:"-"`
}

// SearchTimings carries per-phase wall-clock measurements collected
// by the BM25 retrieval pipeline. Zero-valued fields mean the phase
// didn't run on this call (e.g. FallbackMS is 0 when the BM25 result
// already saturated the limit).
type SearchTimings struct {
	BM25PrimaryMS   int64 // time spent in the primary BM25 backend call
	BM25ExpansionMS int64 // time spent across all expansion-term BM25 calls
	GetNodesMS      int64 // time spent materialising BM25/vector IDs via GetNodesByIDs
	FindNameMS      int64 // time spent on the FindNodesByName splice-in
	FallbackMS      int64 // time spent in the substring/name-contains fallback
	// Sub-buckets of the BM25*MS totals — proves which phase inside
	// the wrapper is actually slow. Accumulated across every
	// primary + expansion BM25 invocation.
	TextBackendMS  int64 // strictly inside Backend.Search / text channel
	EmbedMS        int64 // inside embedder.Embed (vector path only)
	VectorSearchMS int64 // inside vector.Search ANN call (vector path only)
	EngineRerankMS int64 // inside rerank.Pipeline.Rerank in SearchSymbolsRanked
	// BundleMS accumulates the wall-clock spent inside
	// SymbolBundleSearcherBackend.SearchSymbolBundles (one query per
	// BM25 fan-out that returns Node + in/out edges in one bundle).
	// When the backend supports bundles, the bundle path replaces the
	// (TextBackend + GetNodes) sub-buckets; the bm25_backend_ms
	// derivation in the handler subtracts BundleMS so the existing
	// fields stay meaningful.
	BundleMS int64
	// CacheHitRate is the fraction of post-merge candidates whose
	// in/out edges were already in the rerank Context cache when the
	// handler-side prepare() ran. 1.0 means every candidate was
	// pre-seeded from a bundle; 0.0 means the rerank had to fetch
	// every candidate's edges itself. Populated by the handler when
	// the bundle path is active so the search_symbols debug log can
	// surface how often the seeding actually catches.
	CacheHitRate float64
}

// ScopeAllows reports whether a node passes the workspace/project
// scope expressed in opts. Empty WorkspaceID means "no scope" — every
// node passes. Same effective-fallback rule as the matcher: missing
// WorkspaceID on the node falls back to its RepoPrefix.
//
// Exported so the MCP layer can enforce the session's workspace
// boundary on by-id and whole-graph handlers that don't route through
// the engine's scoped traversal.
func (o QueryOptions) ScopeAllows(n *graph.Node) bool {
	if n == nil {
		return true
	}
	if o.WorkspaceID != "" {
		ws := n.WorkspaceID
		if ws == "" {
			ws = n.RepoPrefix
		}
		if ws != o.WorkspaceID {
			return false
		}
		if o.ProjectID != "" {
			proj := n.ProjectID
			if proj == "" {
				proj = n.RepoPrefix
			}
			if proj != o.ProjectID {
				return false
			}
		}
	}
	// A node with an empty RepoPrefix is not owned by any repository, so a
	// repo narrow can only ever be satisfied vacuously — the RepoAllow keys
	// are registry prefixes, which such nodes never carry. Admit it: the
	// workspace / project checks above still bound it for scoped sessions.
	//
	// Two populations land here. Synthetic global externals (dep::,
	// external::, non-Go module::) model third-party symbols owned by no
	// repo and are visible from every scope by construction — that is the
	// durable reason. Nodes minted in single-repo (unprefixed) mode are the
	// historical one, and go away once prefixing is unconditional.
	//
	// This is the same carve-out the MCP layer's filterNodes / field-query /
	// API-impact filters apply. Every repo-narrow predicate in the codebase
	// must agree on it: one that rejects instead of admitting turns a scoped
	// query into an empty result rather than a narrower one.
	if len(o.RepoAllow) > 0 && n.RepoPrefix != "" && !o.RepoAllow[n.RepoPrefix] {
		return false
	}
	return true
}

func (o QueryOptions) hasScopeFilter() bool {
	return o.WorkspaceID != "" || len(o.RepoAllow) > 0
}

// FilterByMinTier drops edges whose Origin rank is below minTier.
//
// Nodes are left untouched — a hop that gets filtered can leave an
// unreachable node in Nodes. That's acceptable for the current surface
// area (agents filter by tier mainly for one-hop questions like "who
// calls this?"), and pruning orphans would silently change the node set
// when a caller might still want to see them. Callers that care can
// post-prune themselves.
//
// Edges without Origin set fall back to graph.DefaultOriginFor (derived
// from kind + confidence + semantic_source meta) so filters work on
// edges produced before this field existed or by providers not yet
// updated.
func (sg *SubGraph) FilterByMinTier(minTier string) {
	if minTier == "" || sg == nil {
		return
	}
	kept := make([]*graph.Edge, 0, len(sg.Edges))
	dropped := 0
	maxDroppedRank := -1
	maxDroppedOrigin := ""
	for _, e := range sg.Edges {
		origin := effectiveOrigin(e)
		if graph.MeetsMinTier(origin, minTier) {
			kept = append(kept, e)
			continue
		}
		dropped++
		if r := graph.OriginRank(origin); r > maxDroppedRank {
			maxDroppedRank = r
			maxDroppedOrigin = origin
		}
	}
	sg.Edges = kept
	// Record a caveat when the filter dropped edges that still exist below the
	// tier — so an empty (or thinned) result reads as "tier-filtered", not as
	// "no usages". Only meaningful when the filter actually emptied the visible
	// set; a min_tier that leaves edges keeps its own rows as the signal.
	if dropped > 0 && len(kept) == 0 {
		sg.TierFiltered = &graph.TierFilteredCaveat{
			Class:             graph.TierFilteredClass,
			EdgesBelowMinTier: dropped,
			MaxAvailableTier:  maxDroppedOrigin,
		}
	}
}

// SortEdgesForPage orders usage edges into the one stable global order
// a row cap may consume: strongest evidence first (origin tier rank,
// then confidence), with file/line/kind/from/to as deterministic
// tie-breakers. Backends iterate in-edges differently (the memory
// store by insertion, SQLite grouped by kind), so a page cut without
// this sort would depend on which store served it and could evict an
// lsp_resolved row in favor of an earlier weak one.
func SortEdgesForPage(edges []*graph.Edge) {
	sort.SliceStable(edges, func(i, j int) bool {
		a, b := edges[i], edges[j]
		ar, br := graph.OriginRank(effectiveOrigin(a)), graph.OriginRank(effectiveOrigin(b))
		if ar != br {
			return ar > br
		}
		if a.Confidence != b.Confidence {
			return a.Confidence > b.Confidence
		}
		// Edge.Confidence is excluded from JSON, so rows that crossed a
		// serialization boundary (federation peers) read zero and carry
		// only the label — the sortable rank that survives the wire.
		if ar, br := graph.ConfidenceLabelRank(a.ConfidenceLabel), graph.ConfidenceLabelRank(b.ConfidenceLabel); ar != br {
			return ar > br
		}
		if a.FilePath != b.FilePath {
			return a.FilePath < b.FilePath
		}
		if a.Line != b.Line {
			return a.Line < b.Line
		}
		if a.Kind != b.Kind {
			return a.Kind < b.Kind
		}
		if a.From != b.From {
			return a.From < b.From
		}
		return a.To < b.To
	})
}

// UsageSummaryOf computes the completeness rollup over a usage
// SubGraph: total references, distinct files (per-edge path with a
// from-node fallback), and test-originated references via the shared
// classifier (graph.NodeIsTest — the same authority order the
// exclude_tests filter applies, so the rollup never disagrees with the
// rows). g resolves child-node owners; nil skips only that hop. Nil
// for an empty result — the zero-edge caveat explains that case.
func UsageSummaryOf(sg *SubGraph, g graph.NodeGetter) *UsageSummary {
	if sg == nil || len(sg.Edges) == 0 {
		return nil
	}
	nodeByID := make(map[string]*graph.Node, len(sg.Nodes))
	for _, n := range sg.Nodes {
		if n != nil {
			nodeByID[n.ID] = n
		}
	}
	files := make(map[string]struct{}, len(sg.Edges))
	testRefs := 0
	for _, e := range sg.Edges {
		from := nodeByID[e.From]
		file := e.FilePath
		if file == "" && from != nil {
			file = from.FilePath
		}
		if file == "" {
			file = "(unknown)"
		}
		files[file] = struct{}{}
		if graph.NodeIsTest(g, from) {
			testRefs++
		}
	}
	return &UsageSummary{NRefs: len(sg.Edges), NFiles: len(files), NTestRefs: testRefs}
}

// effectiveOrigin returns the edge's origin tier, backfilled for edges
// produced before Origin existed (or by providers not yet stamping it).
func effectiveOrigin(e *graph.Edge) string {
	if e.Origin != "" {
		return e.Origin
	}
	src, _ := e.Meta["semantic_source"].(string)
	return graph.DefaultOriginFor(e.Kind, e.Confidence, src)
}

// SuppressRedundantTextMatches drops text_matched edges when the result
// also carries resolver-verified evidence (ast_inferred or better): once a
// symbol has real resolved references, the name-only fan-out — every
// same-named token in the repo — buries them. When text matches are the
// ONLY evidence they are all kept, so recall through dynamic code paths
// never regresses. Orphaned nodes are pruned; the drop count lands in
// TextMatchedSuppressed. Callers apply this only when the user did not
// pass an explicit min_tier.
func (sg *SubGraph) SuppressRedundantTextMatches() {
	if sg == nil || len(sg.Edges) == 0 {
		return
	}
	textRank := graph.OriginRank(graph.OriginTextMatched)
	stronger := false
	for _, e := range sg.Edges {
		if graph.OriginRank(effectiveOrigin(e)) > textRank {
			stronger = true
			break
		}
	}
	if !stronger {
		return
	}
	kept := make([]*graph.Edge, 0, len(sg.Edges))
	dropped := 0
	for _, e := range sg.Edges {
		// Drop exactly the text_matched tier. Untagged edges (rank 0)
		// stay: they predate origin stamping and carry unknown — not
		// low — confidence.
		if e.Origin == graph.OriginTextMatched {
			dropped++
			continue
		}
		kept = append(kept, e)
	}
	if dropped == 0 {
		return
	}
	sg.Edges = kept
	sg.TextMatchedSuppressed = dropped
	referenced := make(map[string]struct{}, len(kept)*2)
	for _, e := range kept {
		referenced[e.From] = struct{}{}
		referenced[e.To] = struct{}{}
	}
	nodes := make([]*graph.Node, 0, len(sg.Nodes))
	for _, n := range sg.Nodes {
		if _, ok := referenced[n.ID]; ok {
			nodes = append(nodes, n)
		}
	}
	sg.Nodes = nodes
}

// FilterSpeculative drops best-guess speculative edges (Meta[speculative]=true)
// unless include is true. Called with include=false by default on every
// edge-returning query, so speculative dynamic-dispatch edges never pollute a
// default result — they are opt-in only.
func (sg *SubGraph) FilterSpeculative(include bool) {
	if include || sg == nil {
		return
	}
	kept := sg.Edges[:0]
	for _, e := range sg.Edges {
		if !e.IsSpeculative() {
			kept = append(kept, e)
		}
	}
	sg.Edges = kept
}

// ToDot returns a Graphviz DOT representation of the subgraph.
func (sg *SubGraph) ToDot() string {
	var b strings.Builder
	b.WriteString("digraph gortex {\n")
	b.WriteString("  rankdir=LR;\n")
	b.WriteString("  node [fontname=\"monospace\" fontsize=10];\n")
	b.WriteString("  edge [fontname=\"monospace\" fontsize=8];\n\n")

	kindColors := map[graph.NodeKind]string{
		graph.KindFile:      "#607D8B",
		graph.KindPackage:   "#bb9af7",
		graph.KindFunction:  "#7aa2f7",
		graph.KindMethod:    "#7dcfff",
		graph.KindType:      "#9ece6a",
		graph.KindInterface: "#73daca",
		graph.KindVariable:  "#ff9e64",
		graph.KindImport:    "#795548",
	}

	kindShapes := map[graph.NodeKind]string{
		graph.KindFile:      "folder",
		graph.KindFunction:  "ellipse",
		graph.KindMethod:    "ellipse",
		graph.KindType:      "box",
		graph.KindInterface: "box",
		graph.KindVariable:  "triangle",
		graph.KindImport:    "note",
		graph.KindPackage:   "diamond",
	}

	for _, n := range sg.Nodes {
		color := kindColors[n.Kind]
		if color == "" {
			color = "#565f89"
		}
		shape := kindShapes[n.Kind]
		if shape == "" {
			shape = "ellipse"
		}
		label := fmt.Sprintf("%s\\n%s", n.Name, n.Kind)
		fmt.Fprintf(&b, "  %q [label=%q shape=%s style=filled fillcolor=%q fontcolor=white];\n",
			n.ID, label, shape, color)
	}

	b.WriteString("\n")

	edgeColors := map[graph.EdgeKind]string{
		graph.EdgeCalls:        "#7aa2f7",
		graph.EdgeImports:      "#565f89",
		graph.EdgeDefines:      "#414868",
		graph.EdgeImplements:   "#9ece6a",
		graph.EdgeExtends:      "#bb9af7",
		graph.EdgeOverrides:    "#f7768e",
		graph.EdgeReferences:   "#3b4261",
		graph.EdgeMemberOf:     "#3b4261",
		graph.EdgeInstantiates: "#e0af68",
	}

	for _, e := range sg.Edges {
		color := edgeColors[e.Kind]
		if color == "" {
			color = "#3b4261"
		}
		fmt.Fprintf(&b, "  %q -> %q [label=%q color=%q];\n",
			e.From, e.To, e.Kind, color)
	}

	b.WriteString("}\n")
	return b.String()
}

// ToMermaid returns a Mermaid flowchart representation of the subgraph.
// Renders in GitHub, Notion, and most markdown viewers.
func (sg *SubGraph) ToMermaid() string {
	var b strings.Builder
	b.WriteString("graph LR\n")

	// Mermaid node shapes by kind.
	// [text] = rectangle, ([text]) = rounded, ((text)) = circle,
	// {text} = diamond, >text] = flag, [(text)] = stadium
	for _, n := range sg.Nodes {
		safeID := mermaidID(n.ID)
		label := fmt.Sprintf("%s\n%s", n.Name, n.Kind)

		switch n.Kind {
		case graph.KindFile:
			fmt.Fprintf(&b, "  %s[\"%s\"]\n", safeID, mermaidEscape(label))
		case graph.KindFunction, graph.KindMethod:
			fmt.Fprintf(&b, "  %s([\"%s\"])\n", safeID, mermaidEscape(label))
		case graph.KindType, graph.KindInterface:
			fmt.Fprintf(&b, "  %s[\"%s\"]\n", safeID, mermaidEscape(label))
		case graph.KindVariable:
			fmt.Fprintf(&b, "  %s>\"%s\"]\n", safeID, mermaidEscape(label))
		case graph.KindPackage:
			fmt.Fprintf(&b, "  %s{\"%s\"}\n", safeID, mermaidEscape(label))
		default:
			fmt.Fprintf(&b, "  %s[\"%s\"]\n", safeID, mermaidEscape(label))
		}
	}

	b.WriteString("\n")

	// Mermaid edge styles by kind.
	edgeStyles := map[graph.EdgeKind]string{
		graph.EdgeCalls:        "-->",
		graph.EdgeImports:      "-.->",
		graph.EdgeDefines:      "-->",
		graph.EdgeImplements:   "-. implements .->",
		graph.EdgeExtends:      "-. extends .->",
		graph.EdgeOverrides:    "-. overrides .->",
		graph.EdgeReferences:   "-->",
		graph.EdgeMemberOf:     "-->",
		graph.EdgeInstantiates: "-. new .->",
	}

	for _, e := range sg.Edges {
		style := edgeStyles[e.Kind]
		if style == "" {
			style = "-->"
		}
		fromID := mermaidID(e.From)
		toID := mermaidID(e.To)

		// For simple arrow styles, add the edge kind as label.
		if style == "-->" || style == "-.->" {
			fmt.Fprintf(&b, "  %s %s|%s| %s\n", fromID, style, e.Kind, toID)
		} else {
			fmt.Fprintf(&b, "  %s %s %s\n", fromID, style, toID)
		}
	}

	// Style classes for node coloring.
	b.WriteString("\n")
	kindCSS := map[graph.NodeKind]string{
		graph.KindFile:      "fill:#607D8B,color:#fff",
		graph.KindPackage:   "fill:#bb9af7,color:#fff",
		graph.KindFunction:  "fill:#7aa2f7,color:#fff",
		graph.KindMethod:    "fill:#7dcfff,color:#fff",
		graph.KindType:      "fill:#9ece6a,color:#fff",
		graph.KindInterface: "fill:#73daca,color:#fff",
		graph.KindVariable:  "fill:#ff9e64,color:#fff",
		graph.KindImport:    "fill:#795548,color:#fff",
	}

	// Group nodes by kind for class assignment.
	byKind := make(map[graph.NodeKind][]string)
	for _, n := range sg.Nodes {
		byKind[n.Kind] = append(byKind[n.Kind], mermaidID(n.ID))
	}
	for kind, ids := range byKind {
		css := kindCSS[kind]
		if css == "" {
			continue
		}
		fmt.Fprintf(&b, "  classDef %s %s\n", kind, css)
		fmt.Fprintf(&b, "  class %s %s\n", strings.Join(ids, ","), kind)
	}

	return b.String()
}

// mermaidID converts a node ID to a Mermaid-safe identifier.
// Mermaid IDs can't contain ::, /, or dots.
func mermaidID(id string) string {
	r := strings.NewReplacer(
		"::", "_",
		"/", "_",
		".", "_",
		"-", "_",
		" ", "_",
		"<", "_",
		">", "_",
		"(", "_",
		")", "_",
	)
	return r.Replace(id)
}

// mermaidEscape escapes characters that break Mermaid labels.
func mermaidEscape(s string) string {
	s = strings.ReplaceAll(s, "\"", "#quot;")
	return s
}

// DefaultOptions returns options with sensible defaults.
func DefaultOptions() QueryOptions {
	return QueryOptions{
		Depth:  3,
		Limit:  50,
		Detail: "brief",
	}
}

// WalkOptions controls a token-budgeted free-form graph traversal
// (Engine.WalkBudgeted). It is deliberately a separate struct from
// QueryOptions: a budgeted walk stops on an encoded-size estimate
// rather than a node count, and lets the caller pick an arbitrary set
// of edge kinds and a traversal direction — neither of which the
// fixed-purpose QueryOptions traversals expose.
type WalkOptions struct {
	// EdgeKinds is the set of edge kinds the walk follows. An empty
	// slice means "every edge kind" and, combined with Direction
	// "both", reproduces an undirected neighbourhood walk.
	EdgeKinds []graph.EdgeKind
	// Direction is "out" (follow outgoing edges — the default when
	// empty), "in" (follow incoming edges), or "both" (undirected).
	Direction string
	// TokenBudget is the approximate token ceiling for the encoded
	// result. The walk stops appending nodes once the running estimate
	// would exceed it. A non-positive value disables the budget.
	TokenBudget int
	// MaxDepth is a hard safety cap on BFS depth, applied even when the
	// token budget would allow deeper expansion. A non-positive value
	// falls back to a built-in default.
	MaxDepth int
	// WorkspaceID / ProjectID / RepoAllow scope the traversal exactly as the
	// matching QueryOptions fields do — neighbours outside the scope
	// are dropped along with the edge that reached them.
	WorkspaceID string
	ProjectID   string
	RepoAllow   map[string]bool
	// CommunityID, when non-empty, constrains the walk to a single
	// detected community: a neighbour is admitted only when it has no
	// community membership (a structural node Leiden never partitioned
	// — file / import / param) OR its membership equals CommunityID.
	// A neighbour with a *different* membership is dropped along with
	// the edge that reached it. NodeToComm must be supplied for the
	// filter to engage; an empty CommunityID disables it entirely.
	CommunityID string
	// NodeToComm maps node ID to its community ID, as produced by the
	// community-detection pass. Only nodes Leiden partitioned over the
	// call / reference graph appear here; an absent entry means "no
	// defined membership" and the CommunityID filter lets such a node
	// pass (it never had a community to be excluded from).
	NodeToComm map[string]string
}

// scopeAllows reports whether n passes this walk's workspace/project
// scope. Mirrors QueryOptions.ScopeAllows so budgeted walks enforce the
// same boundary without duplicating the fallback rules.
func (o WalkOptions) scopeAllows(n *graph.Node) bool {
	return QueryOptions{WorkspaceID: o.WorkspaceID, ProjectID: o.ProjectID, RepoAllow: o.RepoAllow}.ScopeAllows(n)
}
