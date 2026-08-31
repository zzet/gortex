package query

import (
	"container/heap"
	"context"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/search"
	"github.com/zzet/gortex/internal/search/rerank"
)

// SearchProvider is a function that returns the current search backend.
// This allows the engine to always use the latest backend even when the
// indexer replaces it (e.g., wrapping the text backend in HybridBackend
// for embeddings).
type SearchProvider func() search.Backend

// Engine provides higher-level query operations over the graph.
//
// The graph is held as a `graph.Reader` rather than a concrete
// `*graph.Graph` so the same engine instance can serve both base-
// graph queries and overlay-aware queries (an `*graph.OverlaidView`
// also implements `graph.Reader`). `WithReader` returns a shallow
// clone that swaps the reader; the MCP overlay middleware uses it
// to scope a tool call to the calling session's shadow view without
// constructing a fresh Engine per request.
type Engine struct {
	g              graph.Reader
	searchProvider SearchProvider
	rerank         *rerank.Pipeline
	// corpusSeen memoises a positive backendHasCorpus answer. The
	// DocCount fallback is a real COUNT(*) on the disk store and the
	// warm-restart window (delta Count 0, populated disk FTS) would
	// otherwise pay it on every gather; a corpus never un-populates
	// within a backend's lifetime. Reset by SetSearch/SetSearchProvider.
	// Pointer so WithReader clones share it (same backend, same corpus)
	// and the shallow copy stays vet-clean.
	corpusSeen *atomic.Bool
	// overlay is the layer of the per-request view the reader was
	// swapped to, nil for a plain base reader. The search backend
	// indexes the base graph, so any payload it hands back
	// pre-materialised (the bundle fast path) predates the overlay and
	// has to be re-read through the reader before it can be trusted.
	overlay *graph.OverlayLayer
	// viewLayers are the persisted generations stacked in the reader,
	// bottom first, empty for a plain base reader. Each carries its own
	// text corpus, so candidate enumeration queries them alongside the
	// backend's — see viewTextCandidates.
	viewLayers []ViewLayerSource
}

// overlayLayered is the view side of the reader swap: a reader that
// carries a per-request overlay layer exposes it so the engine can
// tell an overlaid read apart from a base read.
type overlayLayered interface {
	Layer() *graph.OverlayLayer
}

// WithReader returns a shallow clone of the engine that reads
// through r instead of the original graph. The search provider and
// rerank pipeline are shared with the source engine. Pass the
// base graph reader to undo a previous swap.
func (e *Engine) WithReader(r graph.Reader) *Engine {
	if e == nil {
		return nil
	}
	clone := *e
	clone.g = r
	clone.overlay = nil
	// A reader swap re-decides which corpora answer, so the stack the
	// previous reader named never carries over — WithViewLayers is the
	// only way one is bound.
	clone.viewLayers = nil
	if view, ok := r.(overlayLayered); ok {
		clone.overlay = view.Layer()
	}
	return &clone
}

// Reader returns the engine's currently-bound graph reader. Tool
// handlers that need to walk the same view the engine sees use this
// to keep their direct-graph reads consistent with the engine's
// internal walks.
func (e *Engine) Reader() graph.Reader { return e.g }

// NewEngine creates a query engine wrapping the given graph. The
// default 11-signal rerank.Pipeline is wired in; callers wanting a
// custom signal set / weights override via SetRerank.
func NewEngine(g graph.Store) *Engine {
	return &Engine{g: g, rerank: rerank.NewDefault(), corpusSeen: &atomic.Bool{}}
}

// SetRerank installs a custom rerank pipeline. Pass nil to disable
// the 11-signal pass and fall back to the BM25-rank-only ordering.
func (e *Engine) SetRerank(p *rerank.Pipeline) { e.rerank = p }

// Rerank returns the installed pipeline. May be nil.
func (e *Engine) Rerank() *rerank.Pipeline { return e.rerank }

// ApplyRerankWeights overlays a per-signal weight map (typically
// loaded from `.gortex.yaml::search::weights`) onto the engine's
// rerank pipeline. Keys not present in the map keep their default
// weight; setting a key to 0 disables that signal. No-op when the
// engine has no pipeline or the map is empty.
func (e *Engine) ApplyRerankWeights(weights map[string]float64) {
	if e.rerank == nil || len(weights) == 0 {
		return
	}
	for name, w := range weights {
		e.rerank.SetWeight(name, w)
	}
}

// SetSearch sets a static search backend (for backward compatibility).
func (e *Engine) SetSearch(s search.Backend) {
	e.searchProvider = func() search.Backend { return s }
	e.corpusSeen.Store(false)
}

// SetSearchProvider sets a dynamic search provider that is called on every query.
func (e *Engine) SetSearchProvider(p SearchProvider) {
	e.searchProvider = p
	e.corpusSeen.Store(false)
}

// getSearch returns the current search backend.
func (e *Engine) getSearch() search.Backend {
	if e.searchProvider == nil {
		return nil
	}
	return e.searchProvider()
}

// GetSymbol returns a node by ID.
func (e *Engine) GetSymbol(id string) *graph.Node {
	return e.g.GetNode(id)
}

// GetOutEdges returns outgoing edges for a node.
func (e *Engine) GetOutEdges(nodeID string) []*graph.Edge {
	return e.g.GetOutEdges(nodeID)
}

// GetInEdges returns incoming edges for a node.
func (e *Engine) GetInEdges(nodeID string) []*graph.Edge {
	return e.g.GetInEdges(nodeID)
}

// FindSymbols returns nodes matching the name, optionally filtered by kind.
func (e *Engine) FindSymbols(name string, kinds ...graph.NodeKind) []*graph.Node {
	candidates := e.g.FindNodesByName(name)
	if len(kinds) == 0 {
		return candidates
	}
	kindSet := make(map[graph.NodeKind]bool, len(kinds))
	for _, k := range kinds {
		kindSet[k] = true
	}
	var filtered []*graph.Node
	for _, n := range candidates {
		if kindSet[n.Kind] {
			filtered = append(filtered, n)
		}
	}
	return filtered
}

// GetFileSymbolsCounts returns the file's symbols and the count of
// edges adjacent to them, without materialising the edges themselves.
// Use it instead of GetFileSymbols when the caller only needs an
// edge total (gcx + compact output paths in get_file_summary), since
// the disk backends can collapse the edge round-trip into a server-
// side aggregate that's orders of magnitude cheaper than shipping
// every row back over cgo.
//
// Backends that implement graph.FileSubGraphCountReader handle the
// count server-side; others fall through to a full GetFileSymbols call
// and report len(sg.Edges) (correct, just not cheap).
func (e *Engine) GetFileSymbolsCounts(filePath string) *SubGraph {
	if pd, ok := e.g.(graph.FileSubGraphCountReader); ok {
		nodes, edgeCount := pd.GetFileSubGraphCounts(filePath)
		if len(nodes) == 0 {
			return &SubGraph{}
		}
		return &SubGraph{
			Nodes:      nodes,
			TotalNodes: len(nodes),
			TotalEdges: edgeCount,
		}
	}
	sg := e.GetFileSymbols(filePath)
	if sg == nil {
		return &SubGraph{}
	}
	// Strip edges — the caller asked for counts only and we don't
	// want stale edge buffers riding back on the SubGraph.
	sg.Edges = nil
	return sg
}

// GetFileSymbols returns the file node, every symbol the file
// defines or contains, and every edge adjacent to any of them.
//
// Backends that implement graph.FileSubGraphReader (the on-disk
// store, for instance) handle the whole walk in one method call so
// they can express the symbol enumeration as a primary-key probe +
// adjacency walk instead of a property-filter scan over Node.
// Backends without the capability fall through to the
// GetFileNodes + GetOut/InEdgesByNodeIDs trio — equivalent on the
// in-memory graph (the per-id lookups are already O(1)).
func (e *Engine) GetFileSymbols(filePath string) *SubGraph {
	if pd, ok := e.g.(graph.FileSubGraphReader); ok {
		nodes, edges := pd.GetFileSubGraph(filePath)
		if len(nodes) == 0 {
			return &SubGraph{}
		}
		return &SubGraph{
			Nodes: nodes, Edges: edges,
			TotalNodes: len(nodes), TotalEdges: len(edges),
		}
	}
	nodes := e.g.GetFileNodes(filePath)
	if len(nodes) == 0 {
		return &SubGraph{}
	}
	ids := make([]string, 0, len(nodes))
	for _, n := range nodes {
		ids = append(ids, n.ID)
	}
	outByID := e.g.GetOutEdgesByNodeIDs(ids)
	inByID := e.g.GetInEdgesByNodeIDs(ids)
	var edges []*graph.Edge
	for _, id := range ids {
		edges = append(edges, outByID[id]...)
		edges = append(edges, inByID[id]...)
	}
	return &SubGraph{
		Nodes: nodes, Edges: dedup(edges),
		TotalNodes: len(nodes), TotalEdges: len(edges),
	}
}

// dependencyEdgeKinds is the allowlist BFS follows for both
// GetDependencies (outgoing) and GetDependents (incoming). It covers
// the call-graph triple (imports/calls/references) plus the
// infrastructure edges (depends_on / configures / mounts / exposes /
// uses_env) so that "what does this Resource depend on" and "what
// depends on this ConfigMap" walks land on the manifest surface,
// not just the code surface.
var dependencyEdgeKinds = []graph.EdgeKind{
	graph.EdgeImports, graph.EdgeCalls, graph.EdgeReferences,
	graph.EdgeDependsOn, graph.EdgeConfigures, graph.EdgeMounts,
	graph.EdgeExposes, graph.EdgeUsesEnv,
}

// GetDependencies returns outgoing dependencies (imports, calls,
// references, plus infrastructure edges) up to depth hops.
func (e *Engine) GetDependencies(nodeID string, opts QueryOptions) *SubGraph {
	return e.bfs(nodeID, opts, true, dependencyEdgeKinds)
}

// GetDependents returns incoming dependents (blast radius) up to depth hops.
func (e *Engine) GetDependents(nodeID string, opts QueryOptions) *SubGraph {
	return e.bfs(nodeID, opts, false, dependencyEdgeKinds)
}

// GetCallChain traces the call graph forward from a function. Follows
// EdgeCalls for intra-service traversal and EdgeMatches to cross service
// boundaries — a consumer function's outbound HTTP/gRPC/topic call is
// linked to the provider's handler via a matcher-produced edge, so the
// same BFS walks straight through.
func (e *Engine) GetCallChain(funcID string, opts QueryOptions) *SubGraph {
	return e.bfs(funcID, opts, true, []graph.EdgeKind{graph.EdgeCalls, graph.EdgeMatches})
}

// GetCallers returns all callers of a function. Traverses EdgeCalls,
// EdgeMatches, and EdgeReferences in reverse:
//   - EdgeCalls: direct `foo()` invocations.
//   - EdgeMatches: cross-service producer/consumer pairing from the matcher
//     (HTTP / gRPC / topic) — a provider handler's callers include every
//     consumer (possibly in another repo) that resolves to it.
//   - EdgeReferences: method-value references (`mux.HandleFunc("/p", h.foo)`,
//     command tables, callback maps, `defer x.Cleanup`). The handler isn't
//     called *at this site*, but it's wired in here — semantically a caller.
//     Without this kind, every routing-style codebase looks like its handlers
//     have zero callers.
func (e *Engine) GetCallers(funcID string, opts QueryOptions) *SubGraph {
	return e.bfs(funcID, opts, false, []graph.EdgeKind{graph.EdgeCalls, graph.EdgeMatches, graph.EdgeReferences})
}

// GetTesters returns the test functions that exercise a symbol via
// the persistent EdgeTests edges baked at index time. Direct
// inverse-edge walk; one hop, no BFS. Returns an empty slice when
// the symbol has no test coverage or when the index pre-dates the
// EdgeTests pass.
func (e *Engine) GetTesters(symbolID string) []*graph.Node {
	edges := e.g.GetInEdges(symbolID)
	var out []*graph.Node
	for _, edge := range edges {
		if edge.Kind != graph.EdgeTests {
			continue
		}
		if n := e.g.GetNode(edge.From); n != nil {
			out = append(out, n)
		}
	}
	return out
}

// FindImplementations returns all types implementing an interface.
func (e *Engine) FindImplementations(interfaceID string) []*graph.Node {
	return e.FindImplementationsMinTier(interfaceID, "")
}

// FindOverrides returns the methods that override the given method
// (i.e. children with EdgeOverrides → methodID). One-hop walk over
// the type-hierarchy edges.
func (e *Engine) FindOverrides(methodID string) []*graph.Node {
	return e.FindOverridesMinTier(methodID, "")
}

// FindOverridesMinTier filters override edges by minimum origin tier.
// Pass graph.OriginLSPDispatch to restrict to LSP-confirmed overrides.
func (e *Engine) FindOverridesMinTier(methodID, minTier string) []*graph.Node {
	edges := e.g.GetInEdges(methodID)
	out := make([]*graph.Node, 0, len(edges))
	for _, edge := range edges {
		if edge.Kind != graph.EdgeOverrides {
			continue
		}
		if minTier != "" {
			origin := edge.Origin
			if origin == "" {
				origin = graph.DefaultOriginFor(edge.Kind, edge.Confidence, "")
			}
			if !graph.MeetsMinTier(origin, minTier) {
				continue
			}
		}
		if n := e.g.GetNode(edge.From); n != nil {
			out = append(out, n)
		}
	}
	return out
}

// FindOverridden returns the parent-class / interface methods that
// the given method overrides (i.e. methodID -EdgeOverrides-> targets).
func (e *Engine) FindOverridden(methodID string) []*graph.Node {
	edges := e.g.GetOutEdges(methodID)
	out := make([]*graph.Node, 0, len(edges))
	for _, edge := range edges {
		if edge.Kind != graph.EdgeOverrides {
			continue
		}
		if n := e.g.GetNode(edge.To); n != nil {
			out = append(out, n)
		}
	}
	return out
}

// FindImplementationsMinTier is FindImplementations filtered by the origin
// tier of the implements-edge. Pass "" for no filter; pass
// graph.OriginLSPDispatch (or higher) to restrict to compiler-verified
// interface dispatches.
func (e *Engine) FindImplementationsMinTier(interfaceID, minTier string) []*graph.Node {
	impls, _ := e.FindImplementationsWithEdgesMinTier(interfaceID, minTier)
	return impls
}

// FindImplementationsWithEdgesMinTier additionally returns the
// implements-edges the node list was derived from, index-aligned by
// construction (edges[i].From == impls[i].ID). Callers rendering a
// subgraph need them — the wire format's `.edges` section carries the
// evidence tier (origin/confidence) that the bare node list drops.
func (e *Engine) FindImplementationsWithEdgesMinTier(interfaceID, minTier string) ([]*graph.Node, []*graph.Edge) {
	edges := e.g.GetInEdges(interfaceID)
	var impls []*graph.Node
	var kept []*graph.Edge
	for _, edge := range edges {
		if edge.Kind != graph.EdgeImplements {
			continue
		}
		if minTier != "" {
			origin := edge.Origin
			if origin == "" {
				src, _ := edge.Meta["semantic_source"].(string)
				origin = graph.DefaultOriginFor(edge.Kind, edge.Confidence, src)
			}
			if !graph.MeetsMinTier(origin, minTier) {
				continue
			}
		}
		if n := e.g.GetNode(edge.From); n != nil {
			impls = append(impls, n)
			kept = append(kept, edge)
		}
	}
	return impls, kept
}

// FindUsages returns all nodes that reference a symbol.
func (e *Engine) FindUsages(nodeID string) *SubGraph {
	return e.FindUsagesScoped(nodeID, QueryOptions{})
}

// FindUsagesScoped is FindUsages with an optional workspace scope.
// When opts.WorkspaceID is set, only callers from that workspace are
// returned — i.e. find_usages on a tuck symbol returns hits only
// from tuck. Empty WorkspaceID preserves the legacy global-graph
// behaviour.
func (e *Engine) FindUsagesScoped(nodeID string, opts QueryOptions) *SubGraph {
	edges := e.g.GetInEdges(nodeID)
	nodeMap := make(map[string]*graph.Node)
	var filtered []*graph.Edge

	// First pass: collect every From id whose edge kind qualifies as
	// a usage. We need the From *Node for the workspace / test
	// filters below, but the legacy loop fetched it with one GetNode
	// per edge — on a disk backend that's one query round-trip per
	// inbound edge, which for hot symbols (hundreds of callers) was
	// the dominant cost of find_usages. Pre-filter the kinds, then
	// batch the lookup so the disk backend issues one query instead
	// of N. The target nodeID rides on the same batch so the
	// "include the target node itself" step at the end of this
	// function does not need its own per-id call.
	fromIDs := make([]string, 0, len(edges)+1)
	seenFrom := make(map[string]struct{}, len(edges))
	for _, edge := range edges {
		if !isUsageEdgeKind(edge.Kind) {
			continue
		}
		if _, dup := seenFrom[edge.From]; dup {
			continue
		}
		seenFrom[edge.From] = struct{}{}
		fromIDs = append(fromIDs, edge.From)
	}
	fromIDs = append(fromIDs, nodeID)
	fromByID := e.g.GetNodesByIDs(fromIDs)

	// Framework-contract edges (a @Configuration class "provides" a @Bean
	// factory method, a handler "handles" a route, an emitter "produces" a
	// topic) are the plumbing graph that analyze kind=routes / contracts
	// consume — not usages of the code symbol they land on. When the queried
	// symbol is a callable code symbol, drop these incoming edges from its
	// usage set; they stay in the graph for the contract analyzers. On a
	// DI-token / infra / contract target the same kinds ARE the meaningful
	// relationship (find_usages on a token returns its providers), so they
	// still pass.
	dropContractOnCode := isCodeSymbolKind(fromByID[nodeID])

	for _, edge := range edges {
		// EdgeProvides + EdgeConsumes carry DI token relationships —
		// `@Inject(TOKEN)` and `{ provide: TOKEN, useValue: ... }`
		// both resolve into one of these, so find_usages on a token
		// returns its providers and consumers alongside the usual
		// call/reference/instantiate edges.
		//
		// Infrastructure edges complete the picture: find_usages
		// on a ConfigMap returns workloads that consume it via
		// `envFrom` (EdgeConfigures) or mount it (EdgeMounts);
		// find_usages on a config_key returns workloads / Dockerfile
		// stages that declare they use it (EdgeUsesEnv) plus code
		// callers via the legacy reads_config path; find_usages on a
		// Service returns Ingresses routing to it (EdgeDependsOn);
		// find_usages on an Image returns workloads pulling it.
		if isUsageEdgeKind(edge.Kind) {
			if dropContractOnCode && isFrameworkContractEdgeKind(edge.Kind) {
				continue
			}
			from := fromByID[edge.From]
			if opts.hasScopeFilter() && (from == nil || !opts.ScopeAllows(from)) {
				continue
			}
			if opts.ExcludeTests && e.isTestSource(from) {
				continue
			}
			filtered = append(filtered, edge)
			if from != nil {
				nodeMap[from.ID] = from
			}
		}
	}
	// Expand any edge carrying Meta["call_sites"] into one usage row per site
	// (a synthesized producer keeps one edge and records its extra sites there;
	// see internal/graph/call_sites.go). Done here so both the GCX and
	// plain-JSON find_usages paths render from the same edge slice.
	filtered = expandCallSites(filtered)
	// Include the target node itself (already in the batch above).
	if n := fromByID[nodeID]; n != nil {
		if !opts.hasScopeFilter() || opts.ScopeAllows(n) {
			nodeMap[n.ID] = n
		}
	}
	nodes := make([]*graph.Node, 0, len(nodeMap))
	for _, n := range nodeMap {
		nodes = append(nodes, n)
	}
	// Sort by ID — nodeMap is a map, so the extraction order is
	// otherwise randomised per call and leaks into the result set.
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].ID < nodes[j].ID })
	return &SubGraph{
		Nodes: nodes, Edges: filtered,
		TotalNodes: len(nodes), TotalEdges: len(filtered),
	}
}

// GetCluster returns the immediate neighbourhood within radius hops (bidirectional).
func (e *Engine) GetCluster(nodeID string, opts QueryOptions) *SubGraph {
	return e.bfs(nodeID, opts, true, nil) // nil = all edge kinds, bidirectional
}

// SearchSymbols performs full-text search across all nodes.
// When a search backend is configured, uses that backend's ranking —
// the store-native FTS index — with camelCase-aware tokenization.
// Falls back to substring matching otherwise.
func (e *Engine) SearchSymbols(query string, limit int) []*graph.Node {
	return e.SearchSymbolsScoped(query, limit, QueryOptions{})
}

// GatherSymbolCandidates retrieves the hybrid BM25/vector candidate union with
// channel ranks and scope preserved, but does not rerank or truncate the union.
// Fan-out callers merge multiple retrieval channels through this method and run
// one final session-aware rerank over the combined slice.
func (e *Engine) GatherSymbolCandidates(query string, limit int, opts QueryOptions, rctx *rerank.Context) []*rerank.Candidate {
	if limit <= 0 {
		limit = 20
	}
	fetchLimit := limit
	if opts.hasScopeFilter() {
		// Over-fetch so the post-fetch scope filter still fills the
		// page, but never clamp below the caller's own ask — an
		// explicit deep limit (the wipeout-escalation refetch) exists
		// precisely to dig past this cap, and flattening it turns the
		// deeper iterations into byte-identical re-queries.
		fetchLimit = limit * 4
		if fetchLimit > 200 {
			fetchLimit = 200
		}
		if fetchLimit < limit {
			fetchLimit = limit
		}
	}

	gatherCtx := rctx
	if gatherCtx == nil {
		gatherCtx = opts.RerankContext
	}

	var cands []*rerank.Candidate
	if s := e.getSearch(); s != nil && (e.viewLayersActive() || e.backendHasCorpus(s)) {
		cands = e.gatherBackendCandidates(query, fetchLimit, opts, gatherCtx)
	} else {
		start := time.Now()
		nodes := e.searchSubstring(query, fetchLimit)
		if opts.SearchTimings != nil {
			opts.SearchTimings.FallbackMS += time.Since(start).Milliseconds()
		}
		cands = make([]*rerank.Candidate, 0, len(nodes))
		for i, n := range nodes {
			cands = append(cands, &rerank.Candidate{Node: n, TextRank: i, VectorRank: -1})
		}
	}

	if opts.hasScopeFilter() {
		kept := cands[:0]
		for _, c := range cands {
			if !opts.ScopeAllows(c.Node) {
				continue
			}
			kept = append(kept, c)
		}
		cands = kept
	}

	crossRepoRerank(cands)
	return cands
}

// SearchSymbolsRanked is SearchSymbolsScoped that returns the full
// rerank.Candidate slice instead of just the nodes — callers can read
// the per-signal contributions and the final score off each candidate.
// rctx is optional session context (frecency / combo / feedback /
// repo + project locality); pass nil to score with structural signals
// only.
func (e *Engine) SearchSymbolsRanked(query string, limit int, opts QueryOptions, rctx *rerank.Context) []*rerank.Candidate {
	if limit <= 0 {
		limit = 20
	}
	cands := e.GatherSymbolCandidates(query, limit, opts, rctx)

	if e.rerank != nil && !opts.SkipInnerRerank {
		ctx := rctx
		if ctx == nil {
			ctx = &rerank.Context{}
		}
		ctx.Graph = e.g
		// When the caller supplied opts.RerankContext (the bundle-
		// seeding handler), inherit its cached edges so this per-call
		// rerank's prepare can read them — saves the 2 batched edge
		// fetches per BM25 fan-out on the bundle hot path. Session
		// signals stay scoped to the OUTER rerank (the one the handler
		// runs against the merged candidate set); the inner rerank
		// gets a structural-only context plus the bundle-cached edges.
		if rctx == nil && opts.RerankContext != nil {
			ctx.InheritEdgeCacheFrom(opts.RerankContext)
		}
		rerankStart := time.Now()
		e.rerank.Rerank(query, cands, ctx)
		if opts.SearchTimings != nil {
			opts.SearchTimings.EngineRerankMS += time.Since(rerankStart).Milliseconds()
		}

		// Post-rerank exact-cosine refinement. The rank-based
		// SemanticSignal scores the vector channel by RRF rank and
		// discards the raw cosine the store computed; this stage
		// recovers it by embedding the query once and re-ordering the
		// ranked head against the candidates' stored vectors. Strictly
		// best-effort: refineByCosine is a no-op whenever the vector
		// channel is inactive, so a text-only search is unaffected.
		if opts.CosineRerank {
			cands = e.RefineByCosine(query, cands, opts.CosineTopN)
		}
	}

	if len(cands) > limit {
		cands = cands[:limit]
	}
	return cands
}

// RefineByCosine runs the post-rerank cosine refinement against the
// engine's current embedder and vector store. It resolves the embedder
// from the active search backend and the stored vectors from the graph
// reader; when either is unavailable it returns cands unchanged.
// Exposed so callers that run their own merged rerank (the MCP
// search_symbols handler) can reuse the exact same refinement after
// their final rerank pass.
func (e *Engine) RefineByCosine(query string, cands []*rerank.Candidate, topN int) []*rerank.Candidate {
	embedder := backendEmbedder(e.getSearch())
	if embedder == nil {
		return cands
	}
	vectors, ok := e.g.(graph.VectorSearcher)
	if !ok {
		return cands
	}
	return refineByCosine(query, cands, embedder, vectors, topN)
}

// SearchSymbolsScoped is SearchSymbols with the optional
// workspace/project scope. When opts.WorkspaceID is set, results
// outside that scope are filtered out and the search re-fetches as
// needed to fill the requested limit. Empty scope preserves the
// legacy global behaviour.
func (e *Engine) SearchSymbolsScoped(query string, limit int, opts QueryOptions) []*graph.Node {
	cands := e.SearchSymbolsRanked(query, limit, opts, nil)
	out := make([]*graph.Node, 0, len(cands))
	for _, c := range cands {
		out = append(out, c.Node)
	}
	return out
}

// backendHasCorpus reports whether the search backend has anything to
// answer with. Count() alone is wrong here: it is a delta of THIS
// process's Add/Remove calls, so a daemon warm-started over a
// populated disk FTS reads 0 and every search would silently divert
// to the substring fallback until the first file edit. The
// authoritative disk corpus size rides DocCount when the backend
// exposes it (the SymbolSearcherBackend chain does); a positive
// answer is memoised — see Engine.corpusSeen.
func (e *Engine) backendHasCorpus(s search.Backend) bool {
	if e.corpusSeen != nil && e.corpusSeen.Load() {
		return true
	}
	has := false
	if s.Count() > 0 {
		has = true
	} else if dc, ok := s.(search.DocCounter); ok {
		if n, known := dc.DocCount(); known && n > 0 {
			has = true
		}
	}
	if has && e.corpusSeen != nil {
		e.corpusSeen.Store(true)
	}
	return has
}

// repoAllowList flattens a RepoAllow set into a sorted slice for the
// scoped backend call — sorted so the generated SQL is deterministic.
func repoAllowList(allow map[string]bool) []string {
	out := make([]string, 0, len(allow))
	for r, ok := range allow {
		if ok {
			out = append(out, r)
		}
	}
	sort.Strings(out)
	return out
}

// gatherBackendCandidates fetches text + (optional) vector results,
// dedups them across channels, and supplements with exact-name /
// substring matches. Each candidate carries its
// 0-based TextRank and VectorRank (or -1 when the channel didn't
// return it) so the rerank pipeline can score per channel.
//
// Bundle fast path: when the backend implements
// SymbolBundleSearcherBackend, BM25 hits + their Node payload + their
// in/out edges all arrive in one engine round-trip. The bundle's
// edges seed rctx (when non-nil) so the rerank pipeline's prepare
// pass can skip its own batched fetch entirely. Vector channel IDs
// (which don't carry edges in the bundle) still route through the
// per-call GetNodesByIDs + GetIn/OutEdgesByNodeIDs path; bundle and
// vector candidates merge into one rerank slice.
//
// Fallback (no bundle support): the legacy path — Search() / channel
// for IDs, GetNodesByIDs to materialise. On a disk backend
// the bundle fast path collapses 3 round-trips (FTS + nodes +
// the rerank's 2 edge fetches) into 4 server-side queries with no
// engine→rerank boundary crossings; the GetNodesByIDs cost goes
// away entirely for the BM25 hits.
func (e *Engine) gatherBackendCandidates(query string, limit int, opts QueryOptions, rctx *rerank.Context) []*rerank.Candidate {
	backend := e.getSearch()
	timings := opts.SearchTimings

	// A composed view is answered corpus by corpus: the backend below
	// covers the indexed corpus, and viewTextCandidates queries each
	// stacked generation on its own handle and composes the results.
	//
	// The vector channel does not compose. Vectors are generation-scoped
	// rows like the text ones, but the channel reaching them is a
	// process-wide index — one embedder and one ANN structure, built over
	// the indexed corpus and owned by the search backend, with no way to
	// ask it for a generation's vectors short of standing up an embedder
	// and an index per generation per request. Serving the base corpus's
	// vectors as the view's would be the one thing worse than not
	// answering: they describe files the view has replaced or deleted, and
	// nothing downstream could tell. So a composed view ranks from the
	// text lanes alone. The exact-per-view-statistics successor named in
	// MergeRankedSources is where vector composition belongs — it needs
	// the same per-view corpus that makes scores comparable.
	//
	// The post-rerank cosine refinement is structurally out already:
	// RefineByCosine asks the reader for graph.VectorSearcher and a
	// composed view does not implement it.
	viewLayered := e.viewLayersActive()
	skipVectorChannel := opts.SkipVectorChannel || viewLayered

	// Bundle fast path. The SymbolBundleSearcherBackend assertion
	// chains through Swappable → HybridBackend → SymbolSearcherBackend
	// in production; both Swappable and HybridBackend forward when
	// the inner backend supports it. Vector IDs still need the
	// per-call materialise — bundles don't carry vector hits.
	var (
		textResults    []search.SearchResult
		vectorIDs      []string
		bundleHandled  bool
		bundleNodeByID = make(map[string]*graph.Node)
	)
	if bsb, ok := backend.(search.SymbolBundleSearcherBackend); ok {
		// Pull the vector channel separately when present. Bundles
		// cover BM25 only; the engine merges vector hits below.
		// VectorChannelOnly avoids re-running the text BM25 path —
		// the bundle already returned the BM25 hits and their full
		// node + edge payload. Falling back to SearchChannels here
		// would double-pay the FTS query cost per BM25 fan-out.
		type vectorOnly interface {
			VectorChannelOnly(query string, limit int) ([]string, search.ChannelTimings)
		}
		vectorOnlyBackend, vectorOnlyOK := backend.(vectorOnly)
		bundleStart := time.Now()
		// Repo-narrowed sessions take the scoped bundle path when the
		// backend can filter inside the FTS query. The post-fetch
		// ScopeAllows pass below starves whenever another repo owns the
		// whole BM25 head deeper than this fetch — only the backend can
		// narrow without a depth limit.
		var bundles []search.SymbolBundle
		// Gate on the flattened list, not the map: an all-false map is
		// "deny everything" under ScopeAllows and must not select the
		// scoped path with an empty (= unscoped!) allow list.
		scopedAnswered := false
		if allow := repoAllowList(opts.RepoAllow); len(allow) > 0 {
			if sb, ok := backend.(search.ScopedSymbolBundleSearcherBackend); ok {
				bundles = sb.SearchSymbolBundlesScoped(query, allow, limit*2)
				// nil = no scoped support (or error); anything non-nil —
				// an empty slice included — is the scoped path's answer.
				scopedAnswered = bundles != nil
			}
		}
		// nil = no scoped support (or error); a non-nil empty slice is
		// the scoped path's real answer and must NOT trigger the
		// unscoped flood fetch its emptiness proves useless.
		if bundles == nil {
			bundles = bsb.SearchSymbolBundles(query, limit*2)
		}
		if timings != nil {
			timings.BundleMS += time.Since(bundleStart).Milliseconds()
		}
		// A per-request overlay reader invalidates the bundle payload:
		// the backend indexes the base graph, so a symbol the buffer
		// replaced arrives carrying its pre-edit node, and a symbol the
		// buffer deleted arrives at all. Keep the BM25 ranking — the
		// scores are still the corpus's answer — and drop the payload:
		// every hit falls through to the view-aware batched lookup
		// below, which substitutes the overlay's node and omits the IDs
		// the view no longer exposes.
		//
		// A composed view invalidates it for the same reason one level
		// down: the bundle's payload is the indexed corpus's row, and a
		// generation in the stack may have replaced or deleted it.
		rehydrateThroughReader := e.overlay != nil || viewLayered
		if len(bundles) > 0 {
			bundleHandled = true
			textResults = make([]search.SearchResult, 0, len(bundles))
			outSeed := make(map[string][]*graph.Edge, len(bundles))
			inSeed := make(map[string][]*graph.Edge, len(bundles))
			for _, b := range bundles {
				if b.Node == nil {
					continue
				}
				textResults = append(textResults, search.SearchResult{ID: b.Node.ID, Score: b.Score})
				if rehydrateThroughReader {
					continue
				}
				bundleNodeByID[b.Node.ID] = b.Node
				outSeed[b.Node.ID] = b.OutEdges
				inSeed[b.Node.ID] = b.InEdges
			}
			// Seed the rerank context's edge caches so prepare() can
			// skip its own batched fetch for the bundle-covered IDs.
			// preSeeded=true is the contract that prepare's batched
			// edge fetch is now redundant — see rerank.Context for the
			// invariant the engine relies on (the next caller's
			// candidate set is fully covered by these maps for the
			// BM25 hits; vector / substring fallback hits are still
			// served by the per-candidate accessor fallback).
			//
			// Under an overlay the seed is skipped entirely rather than
			// filtered down to the uncovered files: the layer also
			// *introduces* edges no base bundle carries, so no subset of
			// base edges can honour the pre-seed contract. Dropping it
			// hands the fetch back to the rerank pass, which reads
			// through the same overlay-aware reader.
			if rctx != nil && !rehydrateThroughReader {
				rctx.SeedEdgeCaches(inSeed, outSeed, true)
			}
		} else if scopedAnswered {
			// A scoped query that answered zero IS the answer. Leaving
			// bundleHandled false here would send the miss down the
			// unscoped channel fallback — re-running the very cross-repo
			// flood fetch the scoped path exists to remove.
			bundleHandled = true
		}
		// Vector channel: only when the bundle path took the BM25
		// branch. Otherwise the fallback path below pulls both.
		// VectorChannelOnly skips the BM25 re-run (the bundle already
		// returned text hits + their full payload); a few hundred
		// microseconds of embed + ANN, not a second FTS query.
		//
		// opts.SkipVectorChannel suppresses the embed + ANN entirely.
		// The MCP handler flips this on for identifier-shape queries
		// (QueryClassSymbol / Path / Signature) where the rerank's
		// classWeightTable already proves semantic contributes near-
		// zero signal vs the BM25 channel — see classWeightTable in
		// internal/search/rerank/query_kind.go.
		if vectorOnlyOK && !skipVectorChannel {
			vecIDs, stats := vectorOnlyBackend.VectorChannelOnly(query, limit*2)
			vectorIDs = vecIDs
			if timings != nil {
				timings.EmbedMS += stats.EmbedMS
				timings.VectorSearchMS += stats.VectorSearchMS
			}
		}
	}

	// Legacy / fallback path: bundle backend absent OR returned no
	// hits. Pull text + vector channels separately when the backend
	// exposes them (HybridBackend). Otherwise treat plain Search()
	// output as text-only. The wall-clock for the backend search
	// call lands on the outer caller's BM25*MS bucket — measuring
	// around the engine boundary captures the full per-call cost
	// without double-counting against the post-call GetNodesByIDs /
	// FindNodesByName / Fallback phases that this function
	// instruments individually below.
	if !bundleHandled {
		type timedChan interface {
			SearchChannelsTimed(query string, limit int) ([]search.SearchResult, []string, search.ChannelTimings)
		}
		switch {
		case skipVectorChannel:
			// Identifier-shape fast path: skip the vector channel
			// (no embed, no ANN) and run text-only Search. The cost
			// saved is the per-call embedder + vector index hit; the
			// rerank's classWeightTable proves it's not earning its
			// keep for these query classes.
			textStart := time.Now()
			textResults = backend.Search(query, limit*2)
			if timings != nil {
				timings.TextBackendMS += time.Since(textStart).Milliseconds()
			}
		default:
			if tc, ok := backend.(timedChan); ok {
				var stats search.ChannelTimings
				textResults, vectorIDs, stats = tc.SearchChannelsTimed(query, limit*2)
				if timings != nil {
					timings.TextBackendMS += stats.TextMS
					timings.EmbedMS += stats.EmbedMS
					timings.VectorSearchMS += stats.VectorSearchMS
				}
			} else if cs, ok := backend.(search.ChannelSearcher); ok {
				textStart := time.Now()
				textResults, vectorIDs = cs.SearchChannels(query, limit*2)
				if timings != nil {
					timings.TextBackendMS += time.Since(textStart).Milliseconds()
				}
			} else {
				textStart := time.Now()
				textResults = backend.Search(query, limit*2)
				if timings != nil {
					timings.TextBackendMS += time.Since(textStart).Milliseconds()
				}
			}
		}
	}

	// Composed view: everything above answered for the indexed corpus
	// alone. Enumerate the stack's generations, compose the results, and
	// hand one merged ranked list to the materialisation below — which is
	// where every surviving candidate is resolved through the composed
	// reader and anything the view hides falls out.
	if viewLayered {
		textResults = e.viewTextCandidates(
			query,
			limit*2,
			textResults,
			viewBaseTextRefill(backend, query, repoAllowList(opts.RepoAllow)),
		)
	}

	// Collect every ID NOT covered by the bundle path (vector hits +
	// fallback path's text hits) and materialise them with one
	// batched fetch. Empty IDs are tolerated — the batch lookup
	// ignores them and the per-id insert short-circuits below.
	idBatch := make([]string, 0, len(textResults)+len(vectorIDs))
	for _, r := range textResults {
		if r.ID != "" {
			if _, covered := bundleNodeByID[r.ID]; covered {
				continue
			}
			idBatch = append(idBatch, r.ID)
		}
	}
	for _, id := range vectorIDs {
		if id != "" {
			if _, covered := bundleNodeByID[id]; covered {
				continue
			}
			idBatch = append(idBatch, id)
		}
	}
	getNodesStart := time.Now()
	nodeByID := e.g.GetNodesByIDs(idBatch)
	if timings != nil {
		timings.GetNodesMS += time.Since(getNodesStart).Milliseconds()
	}
	if nodeByID == nil {
		// GetNodesByIDs returns nil for empty input — we still need a
		// non-nil map below to merge the bundle's nodes into.
		nodeByID = make(map[string]*graph.Node, len(bundleNodeByID))
	}
	// Merge the bundle's already-materialised nodes into the same
	// lookup map the per-candidate insert step below reads from.
	for id, n := range bundleNodeByID {
		nodeByID[id] = n
	}

	idx := make(map[string]int) // node ID → slice index for dedup
	cands := make([]*rerank.Candidate, 0, len(textResults)+len(vectorIDs))

	insert := func(id string, textRank, vectorRank int) {
		if id == "" {
			return
		}
		node := nodeByID[id]
		if node == nil || node.Kind == graph.KindFile || node.Kind == graph.KindImport {
			return
		}
		if pos, ok := idx[id]; ok {
			c := cands[pos]
			if textRank >= 0 && (c.TextRank < 0 || textRank < c.TextRank) {
				c.TextRank = textRank
			}
			if vectorRank >= 0 && (c.VectorRank < 0 || vectorRank < c.VectorRank) {
				c.VectorRank = vectorRank
			}
			return
		}
		idx[id] = len(cands)
		cands = append(cands, &rerank.Candidate{
			Node: node, TextRank: textRank, VectorRank: vectorRank,
		})
	}

	for rank, r := range textResults {
		insert(r.ID, rank, -1)
	}
	for rank, id := range vectorIDs {
		insert(id, -1, rank)
	}

	// Stop early when the native text + vector union has already exceeded the
	// requested width; the supplementary tiers below are a fill, not a
	// boost.
	if len(cands) >= limit*2 {
		return cands
	}

	// Exact-name matches that BM25 might rank low — splice them in at
	// the tail of the text channel so they're still text-ranked. The
	// caller can suppress this when the query string is known to never
	// match a literal Name (the combined-OR fan-out's concatenated bag
	// of expansion terms, for example) — saves the query round-trip
	// that would unconditionally return zero rows.
	if !opts.SkipExactNameSplice {
		findNameStart := time.Now()
		for _, n := range e.g.FindNodesByName(query) {
			if n.Kind == graph.KindFile || n.Kind == graph.KindImport {
				continue
			}
			if _, seen := idx[n.ID]; seen {
				continue
			}
			idx[n.ID] = len(cands)
			cands = append(cands, &rerank.Candidate{Node: n, TextRank: len(textResults), VectorRank: -1})
		}
		if timings != nil {
			timings.FindNameMS += time.Since(findNameStart).Milliseconds()
		}
	}

	// Substring fallback for remaining slots — strictly TextRank=-1
	// (the rerank pipeline still considers them via signature/recency
	// signals, but BM25 can't speak to them). The store-side
	// FindNodesByNameContaining pushes the predicate into the backend
	// index instead of materialising every node over cgo and filtering
	// in Go — the old AllNodes loop is broken at Linux-kernel scale
	// (10M+ symbols, hundreds of MB of nodes per query). We over-fetch
	// by a small slack factor so dedup against existing cands still
	// leaves room to fill `limit`.
	if len(cands) < limit {
		fallbackStart := time.Now()
		fetch := (limit - len(cands)) * 2
		if fetch < limit {
			fetch = limit
		}
		subMatches := e.g.FindNodesByNameContaining(query, fetch)
		// Stable ordering — backends may return in catalog order, which
		// is not a meaningful relevance signal here.
		sort.Slice(subMatches, func(i, j int) bool { return subMatches[i].ID < subMatches[j].ID })
		for _, n := range subMatches {
			if n.Kind == graph.KindFile || n.Kind == graph.KindImport {
				continue
			}
			if _, seen := idx[n.ID]; seen {
				continue
			}
			idx[n.ID] = len(cands)
			cands = append(cands, &rerank.Candidate{Node: n, TextRank: -1, VectorRank: -1})
			if len(cands) >= limit {
				break
			}
		}
		if timings != nil {
			timings.FallbackMS += time.Since(fallbackStart).Milliseconds()
		}
	}

	return cands
}

const substringSearchPageSize = 256

type substringCandidate struct {
	id      string
	nameLen int
	score   int
}

func substringCandidateBetter(a, b substringCandidate) bool {
	if a.score != b.score {
		return a.score < b.score
	}
	if a.nameLen != b.nameLen {
		return a.nameLen < b.nameLen
	}
	return a.id < b.id
}

// substringCandidateHeap keeps the worst retained candidate at its root.
type substringCandidateHeap []substringCandidate

func (h substringCandidateHeap) Len() int { return len(h) }
func (h substringCandidateHeap) Less(i, j int) bool {
	return substringCandidateBetter(h[j], h[i])
}
func (h substringCandidateHeap) Swap(i, j int) { h[i], h[j] = h[j], h[i] }
func (h *substringCandidateHeap) Push(x any) {
	*h = append(*h, x.(substringCandidate))
}
func (h *substringCandidateHeap) Pop() any {
	old := *h
	n := len(old)
	x := old[n-1]
	*h = old[:n-1]
	return x
}

type nodeSearchMutationRevisioner interface {
	SearchMutationRevision() (uint64, bool)
}

type nodeMutationRevisioner interface {
	MutationRevision() uint64
}

func nodeMutationRevision(r graph.Reader) (uint64, bool) {
	if revisioned, ok := r.(nodeSearchMutationRevisioner); ok {
		return revisioned.SearchMutationRevision()
	}
	if revisioned, ok := r.(nodeMutationRevisioner); ok {
		return revisioned.MutationRevision(), true
	}
	return 0, false
}

func nodeMutationChanged(r graph.Reader, before uint64, known bool) bool {
	if !known {
		return false
	}
	after, ok := nodeMutationRevision(r)
	return ok && after != before
}

func substringScore(id, name string, kind graph.NodeKind, query, lower string) (int, bool) {
	if kind == graph.KindFile || kind == graph.KindImport {
		return 0, false
	}
	switch {
	case name == query:
		return 0, true
	case hasPrefixFold(name, lower):
		return 1, true
	case containsFold(name, lower):
		return 2, true
	case containsFold(id, lower):
		return 3, true
	default:
		return 0, false
	}
}

func (e *Engine) scanSubstringCandidates(query, lower string, limit int) ([]substringCandidate, error) {
	top := make(substringCandidateHeap, 0, limit)
	err := graph.ScanNodeSearchKeys(context.Background(), e.g, substringSearchPageSize, func(page []graph.NodeSearchKey) bool {
		for _, key := range page {
			score, ok := substringScore(key.ID, key.Name, key.Kind, query, lower)
			if !ok {
				continue
			}
			candidate := substringCandidate{id: key.ID, nameLen: len(key.Name), score: score}
			if len(top) < limit {
				candidate.id = strings.Clone(candidate.id)
				heap.Push(&top, candidate)
				continue
			}
			if substringCandidateBetter(candidate, top[0]) {
				candidate.id = strings.Clone(candidate.id)
				top[0] = candidate
				heap.Fix(&top, 0)
			}
		}
		return true
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(top, func(i, j int) bool {
		return substringCandidateBetter(top[i], top[j])
	})
	return top, nil
}

func (e *Engine) hydrateSubstringCandidates(candidates []substringCandidate, query, lower string, limit int) []*graph.Node {
	ids := make([]string, len(candidates))
	for i := range candidates {
		ids[i] = candidates[i].id
	}
	nodeByID := e.g.GetNodesByIDs(ids)

	type hydratedCandidate struct {
		node  *graph.Node
		score int
	}
	hydrated := make([]hydratedCandidate, 0, len(candidates))
	for _, candidate := range candidates {
		node := nodeByID[candidate.id]
		if node == nil {
			continue
		}
		score, ok := substringScore(node.ID, node.Name, node.Kind, query, lower)
		if !ok {
			continue
		}
		hydrated = append(hydrated, hydratedCandidate{node: node, score: score})
	}
	sort.Slice(hydrated, func(i, j int) bool {
		if hydrated[i].score != hydrated[j].score {
			return hydrated[i].score < hydrated[j].score
		}
		if len(hydrated[i].node.Name) != len(hydrated[j].node.Name) {
			return len(hydrated[i].node.Name) < len(hydrated[j].node.Name)
		}
		return hydrated[i].node.ID < hydrated[j].node.ID
	})
	if len(hydrated) > limit {
		hydrated = hydrated[:limit]
	}
	out := make([]*graph.Node, len(hydrated))
	for i := range hydrated {
		out[i] = hydrated[i].node
	}
	return out
}

func (e *Engine) searchSubstring(query string, limit int) []*graph.Node {
	if limit <= 0 {
		return nil
	}
	lower := strings.ToLower(query)
	for attempt := 0; attempt < 2; attempt++ {
		before, revisionKnown := nodeMutationRevision(e.g)
		candidates, err := e.scanSubstringCandidates(query, lower, limit)
		if err != nil {
			// SearchSymbols cannot return an error. Preserve the Reader contract:
			// unexpected storage failures must remain visible, not look like an
			// ordinary empty result.
			panic(err)
		}
		if attempt == 0 && nodeMutationChanged(e.g, before, revisionKnown) {
			continue
		}
		out := e.hydrateSubstringCandidates(candidates, query, lower, limit)
		if attempt == 0 && nodeMutationChanged(e.g, before, revisionKnown) {
			continue
		}
		return out
	}
	return nil
}

// SearchSymbolsInRepo performs full-text search filtered to a specific repository.
func (e *Engine) SearchSymbolsInRepo(query string, repoPrefix string, limit int) []*graph.Node {
	if limit <= 0 {
		limit = 20
	}
	// Fetch extra results since some will be filtered out.
	candidates := e.SearchSymbols(query, limit*2)
	var out []*graph.Node
	for _, n := range candidates {
		if n.RepoPrefix == repoPrefix {
			out = append(out, n)
			if len(out) >= limit {
				break
			}
		}
	}
	return out
}

// GetFileSymbolsInRepo returns all symbols defined in a file, scoped to a specific repository.
func (e *Engine) GetFileSymbolsInRepo(filePath string, repoPrefix string) *SubGraph {
	sg := e.GetFileSymbols(filePath)
	var nodes []*graph.Node
	for _, n := range sg.Nodes {
		if n.RepoPrefix == repoPrefix {
			nodes = append(nodes, n)
		}
	}
	var edges []*graph.Edge
	nodeSet := make(map[string]bool, len(nodes))
	for _, n := range nodes {
		nodeSet[n.ID] = true
	}
	for _, edge := range sg.Edges {
		if nodeSet[edge.From] || nodeSet[edge.To] {
			edges = append(edges, edge)
		}
	}
	return &SubGraph{
		Nodes:      nodes,
		Edges:      dedup(edges),
		TotalNodes: len(nodes),
		TotalEdges: len(edges),
	}
}

// AllNodes returns all nodes in the graph.
func (e *Engine) AllNodes() []*graph.Node {
	return e.g.AllNodes()
}

// Stats returns summary statistics for the graph.
func (e *Engine) Stats() *graph.GraphStats {
	s := e.g.Stats()
	return &s
}

// defaultDispatchFanout bounds how many overriders one interface/abstract
// method expands to during polymorphic dispatch expansion, so a hub interface
// with hundreds of implementors cannot blow up a call-chain walk.
const defaultDispatchFanout = 24

// bfs performs breadth-first traversal from nodeID.
// If forward is true, follows outgoing edges; if false, follows incoming.
// If edgeKinds is nil, follows all edge kinds bidirectionally (for cluster).
func (e *Engine) bfs(nodeID string, opts QueryOptions, forward bool, edgeKinds []graph.EdgeKind) *SubGraph {
	if opts.Depth <= 0 {
		opts.Depth = 3
	}
	if opts.Limit <= 0 {
		opts.Limit = 50
	}
	seed := e.g.GetNode(nodeID)
	if opts.hasScopeFilter() && (seed == nil || !opts.ScopeAllows(seed)) {
		return &SubGraph{}
	}

	bidir := edgeKinds == nil
	kindSet := make(map[graph.EdgeKind]bool, len(edgeKinds))
	for _, k := range edgeKinds {
		kindSet[k] = true
	}

	// Prefer a single-round-trip BFS capability (the disk backend lowers it
	// to one recursive CTE) over the layer-by-layer GetOutEdges / GetInEdges
	// walk below, when the walk needs none of the per-layer hooks the flat
	// hop-set cannot carry: workspace scope, test exclusion, dispatch
	// expansion, or a bidirectional cluster walk. The in-memory graph
	// implements the same capability, so both backends take this path and
	// agree by construction; a backend without it (or a capability error)
	// falls through to the Go walk, which stays the correctness oracle.
	if capStore, ok := e.g.(graph.BFSCapable); ok && !bidir && len(edgeKinds) > 0 &&
		!opts.hasScopeFilter() && !opts.ExcludeTests && !opts.IncludeDispatch {
		if sg, ok := e.bfsViaCapability(capStore, nodeID, opts, forward, edgeKinds, kindSet); ok {
			return sg
		}
	}

	visited := map[string]bool{nodeID: true}
	var allNodes []*graph.Node
	var allEdges []*graph.Edge
	truncated := false

	// On a forward call-graph walk, record dropped dynamic-dispatch /
	// unresolved out-edges as epistemic boundaries so get_call_chain can flag
	// the reachable set as a floor rather than silently undercounting.
	recordBoundaries := forward && !bidir && kindSet[graph.EdgeCalls]
	var boundaries []graph.EpistemicBoundary
	boundarySeen := map[string]bool{}

	if seed != nil {
		// The seed enters only after the scope gate above; neighbours
		// discovered by traversal pass through the same scope check in
		// admit.
		allNodes = append(allNodes, seed)
	}

	// admit is the single place edge/node bookkeeping lives, shared by
	// the batched and per-node expansion paths. It records the edge
	// (unless the node budget is already full — the legacy code grew
	// allEdges without bound, so a high-degree hub could pin gigabytes
	// of edge structs), then admits a new, in-scope, non-test neighbour
	// and returns its id to enqueue ("" = skip).
	admit := func(edge *graph.Edge, neighborID string, neighbor *graph.Node) string {
		// Skip unresolved/external targets — but on a call-graph walk, record
		// the dropped dynamic-dispatch / external target as an epistemic
		// boundary first, so the reachable set is honestly flagged as a floor.
		if graph.IsUnresolvedTarget(neighborID) || strings.HasPrefix(neighborID, "external::") {
			if recordBoundaries && edge != nil {
				if reason, ok := graph.ClassifyDroppedTarget(neighborID, edge.Kind); ok {
					key := edge.From + "\x00" + neighborID
					if !boundarySeen[key] && len(boundaries) < 50 {
						boundarySeen[key] = true
						target := neighborID
						if graph.IsUnresolvedTarget(neighborID) {
							target = graph.UnresolvedName(neighborID)
						}
						boundaries = append(boundaries, graph.EpistemicBoundary{
							SeedID:    edge.From,
							Target:    target,
							EdgeKind:  string(edge.Kind),
							Reason:    reason,
							Direction: "callees",
						})
					}
				}
			}
			return ""
		}
		// Once the node budget is full, stop recording edges too: the
		// result is already truncated and an unbounded allEdges is the
		// memory-blowup vector this guard closes.
		if len(allNodes) >= opts.Limit {
			truncated = true
			return ""
		}
		// ExcludeTests drops neighbours flagged as tests during a reverse
		// traversal — a no-op for forward/bidirectional walks.
		if opts.ExcludeTests && !forward && !bidir && e.isTestSource(neighbor) {
			return ""
		}
		// Workspace/project scope: neighbours outside the bound scope are
		// dropped along with the edge that pointed at them.
		if opts.hasScopeFilter() && neighbor != nil && !opts.ScopeAllows(neighbor) {
			return ""
		}
		allEdges = append(allEdges, edge)
		if visited[neighborID] {
			return ""
		}
		visited[neighborID] = true
		if neighbor == nil {
			return ""
		}
		allNodes = append(allNodes, neighbor)
		return neighborID
	}

	// A backend that implements graph.FrontierExpander (the on-disk
	// store) returns a whole frontier's edges + neighbour nodes in one
	// round-trip — no GetNode per edge, no meta decode. Bidirectional
	// (cluster) walks and capability-less backends (the in-memory graph,
	// whose reads are already O(1)) keep the per-node path.
	//
	// Post-fetch filters also force the per-node path: the expander caps
	// RAW rows per call, so a filter (test exclusion, workspace scope)
	// could discard an entire raw page while eligible rows sit beyond
	// the cap — starving the result and reporting it untruncated. The
	// per-node walk filters before any cap and fetches to true
	// exhaustion; it is the correctness oracle, same as the
	// BFSCapable gate above.
	expander, batched := e.g.(graph.FrontierExpander)
	batched = batched && !bidir && len(edgeKinds) > 0 &&
		!opts.ExcludeTests && !opts.hasScopeFilter()

	frontier := []string{nodeID}
	for depth := 0; depth < opts.Depth && len(frontier) > 0 && len(allNodes) < opts.Limit; depth++ {
		var next []string
		if batched {
			for _, h := range expander.ExpandFrontier(frontier, forward, edgeKinds, opts.Limit) {
				if h.Edge == nil {
					continue
				}
				neighborID := h.Edge.To
				if !forward {
					neighborID = h.Edge.From
				}
				if id := admit(h.Edge, neighborID, h.Neighbor); id != "" {
					next = append(next, id)
				}
				if len(allNodes) >= opts.Limit {
					truncated = true
					break
				}
			}
		} else {
			for _, cur := range frontier {
				var edges []*graph.Edge
				switch {
				case bidir:
					edges = append(e.g.GetOutEdges(cur), e.g.GetInEdges(cur)...)
				case forward:
					edges = e.g.GetOutEdges(cur)
				default:
					edges = e.g.GetInEdges(cur)
				}
				// One deterministic admission order before the cap: the
				// backends hand back raw edges in storage order (insertion
				// vs kind-grouped), so without this sort a capped page's
				// membership would depend on the storage engine and the
				// graph's write history. Strongest evidence first — the
				// same order the usage page cap consumes. Sorted only
				// when this expansion can reach the cap: an uncapped
				// expansion admits every eligible edge, so order cannot
				// change membership and a hub's edge list is not worth
				// E·log E comparator calls. Scope: this per-node path —
				// the one post-fetch filters force onto every backend;
				// the batched expander and BFSCapable fast paths keep
				// their own store order. The backends return fresh
				// slices, so the in-place sort is safe.
				if opts.Limit > 0 && len(allNodes)+len(edges) >= opts.Limit {
					SortEdgesForPage(edges)
				}
				for _, edge := range edges {
					if !bidir && !kindSet[edge.Kind] {
						continue
					}
					var neighborID string
					switch {
					case forward || bidir:
						if edge.From == cur {
							neighborID = edge.To
						} else if bidir {
							neighborID = edge.From
						} else {
							continue
						}
					default:
						if edge.To == cur {
							neighborID = edge.From
						} else {
							continue
						}
					}
					// One GetNode per neighbour (the legacy path fetched
					// it twice — scope check, then materialise).
					var neighbor *graph.Node
					if !graph.IsUnresolvedTarget(neighborID) && !strings.HasPrefix(neighborID, "external::") {
						neighbor = e.g.GetNode(neighborID)
					}
					if id := admit(edge, neighborID, neighbor); id != "" {
						next = append(next, id)
					}
					if len(allNodes) >= opts.Limit {
						truncated = true
						break
					}
				}
				if len(allNodes) >= opts.Limit {
					break
				}
			}
		}

		// Polymorphic dispatch expansion: on a forward call walk, a node that
		// is an interface / abstract method is also expanded through its
		// EdgeOverrides IN-edges to the concrete implementations, so the trace
		// auto-reaches the impls. The override edge is recorded unchanged (not
		// faked into a call), gated by DispatchMinTier and capped per method.
		if opts.IncludeDispatch && forward && len(allNodes) < opts.Limit {
			fanout := opts.DispatchFanout
			if fanout <= 0 {
				fanout = defaultDispatchFanout
			}
			for _, cur := range frontier {
				expanded := 0
				for _, edge := range e.g.GetInEdges(cur) {
					if edge.Kind != graph.EdgeOverrides {
						continue
					}
					if opts.DispatchMinTier != "" {
						origin := edge.Origin
						if origin == "" {
							origin = graph.DefaultOriginFor(edge.Kind, edge.Confidence, "")
						}
						if !graph.MeetsMinTier(origin, opts.DispatchMinTier) {
							continue
						}
					}
					var impl *graph.Node
					if !graph.IsUnresolvedTarget(edge.From) && !strings.HasPrefix(edge.From, "external::") {
						impl = e.g.GetNode(edge.From)
					}
					if id := admit(edge, edge.From, impl); id != "" {
						next = append(next, id)
					}
					expanded++
					if expanded >= fanout || len(allNodes) >= opts.Limit {
						break
					}
				}
				if len(allNodes) >= opts.Limit {
					break
				}
			}
		}
		frontier = next
	}

	// ExpandFrontier returns meta-free neighbours; a full-detail caller
	// (e.g. one reading Meta["signature"]) gets them re-hydrated in one
	// batched round-trip. Brief callers (smart_context's ring, step-7)
	// skip this — stripMeta would drop the meta anyway.
	if batched && opts.Detail != "brief" && len(allNodes) > 1 {
		if hyd, ok := e.g.(interface {
			GetNodesByIDs(ids []string) map[string]*graph.Node
		}); ok {
			ids := make([]string, 0, len(allNodes))
			for _, n := range allNodes {
				ids = append(ids, n.ID)
			}
			if full := hyd.GetNodesByIDs(ids); full != nil {
				for i, n := range allNodes {
					if fn := full[n.ID]; fn != nil {
						allNodes[i] = fn
					}
				}
			}
		}
	}

	sg := &SubGraph{
		Nodes:      allNodes,
		Edges:      allEdges,
		TotalNodes: len(visited),
		TotalEdges: len(allEdges),
		Truncated:  truncated,
	}
	if len(boundaries) > 0 {
		sg.Boundaries = boundaries
		sg.LowerBound = graph.LowerBoundCaveat(boundaries)
	}

	if opts.Detail == "brief" {
		stripMeta(sg)
	}
	return sg
}

func stripMeta(sg *SubGraph) {
	for _, n := range sg.Nodes {
		n.Meta = nil
	}
}

// bfsViaCapability builds the same SubGraph the layer walk would, but
// seeds it from a backend BFSCapable.BFS hop-set fetched in one
// round-trip. It is taken only for non-bidirectional, kind-filtered walks
// without workspace scope / test exclusion / dispatch expansion (gated by
// the caller in bfs); ok=false signals a capability error so the caller
// falls back to the in-memory layer walk.
//
// The reachable hop-set is materialised into nodes (one batched
// GetNodesByIDs) and discovery edges (one batched adjacency fetch of the
// expanded frontier, reused for the callee-boundary scan on forward call
// walks). Node / edge selection mirrors the layer walk's admit(): the
// seed always enters, unresolved / external-prefixed targets never do.
func (e *Engine) bfsViaCapability(
	capStore graph.BFSCapable,
	nodeID string,
	opts QueryOptions,
	forward bool,
	edgeKinds []graph.EdgeKind,
	kindSet map[graph.EdgeKind]bool,
) (*SubGraph, bool) {
	dir := graph.DirectionForward
	if !forward {
		dir = graph.DirectionBackward
	}
	hops, err := capStore.BFS([]string{nodeID}, dir, edgeKinds, opts.Depth, opts.Limit)
	if err != nil {
		return nil, false
	}

	ids := make([]string, 0, len(hops))
	var expandedIDs []string
	for _, h := range hops {
		ids = append(ids, h.NodeID)
		// A node at depth < maxDepth had its neighbours followed — its
		// adjacency carries both the discovery edges of its children and
		// (forward) the dropped dynamic-dispatch out-edges for boundaries.
		if h.Depth < opts.Depth {
			expandedIDs = append(expandedIDs, h.NodeID)
		}
	}
	nodesByID := e.g.GetNodesByIDs(ids)

	var adj map[string][]*graph.Edge
	if forward {
		adj = e.g.GetOutEdgesByNodeIDs(expandedIDs)
	} else {
		adj = e.g.GetInEdgesByNodeIDs(expandedIDs)
	}

	allNodes := make([]*graph.Node, 0, len(hops))
	allEdges := make([]*graph.Edge, 0, len(hops))
	for _, h := range hops {
		if h.ParentID == "" { // seed: always enters, regardless of scope
			if n := nodesByID[h.NodeID]; n != nil {
				allNodes = append(allNodes, n)
			}
			continue
		}
		if graph.IsUnresolvedTarget(h.NodeID) || strings.HasPrefix(h.NodeID, "external::") {
			continue
		}
		n := nodesByID[h.NodeID]
		if n == nil {
			continue
		}
		allNodes = append(allNodes, n)
		if edge := matchDiscoveryEdge(adj[h.ParentID], h, forward); edge != nil {
			allEdges = append(allEdges, edge)
		}
	}

	sg := &SubGraph{
		Nodes:      allNodes,
		Edges:      allEdges,
		TotalNodes: len(allNodes),
		TotalEdges: len(allEdges),
		Truncated:  opts.Limit > 0 && len(hops) >= opts.Limit,
	}

	// A forward call walk flags its reachable set as a floor when an
	// expanded node drops a dynamic-dispatch / external out-edge — the same
	// boundaries the layer walk records inline during admit(), rebuilt from
	// the expanded frontier's out-edges (already fetched into adj).
	if forward && kindSet[graph.EdgeCalls] {
		if bs := calleeBoundariesFromAdjacency(expandedIDs, adj, kindSet); len(bs) > 0 {
			sg.Boundaries = bs
			sg.LowerBound = graph.LowerBoundCaveat(bs)
		}
	}

	if opts.Detail == "brief" {
		stripMeta(sg)
	}
	return sg, true
}

// matchDiscoveryEdge finds, in a parent node's adjacency slice, the real
// edge that the hop's (ParentID, NodeID, EdgeKind) discovery tuple names,
// so the SubGraph carries the persisted edge (with its provenance) rather
// than a synthetic one. Forward: parent -> node; backward: node -> parent.
func matchDiscoveryEdge(edges []*graph.Edge, h graph.BFSHop, forward bool) *graph.Edge {
	for _, e := range edges {
		if e == nil || e.Kind != h.EdgeKind {
			continue
		}
		if forward {
			if e.From == h.ParentID && e.To == h.NodeID {
				return e
			}
		} else if e.From == h.NodeID && e.To == h.ParentID {
			return e
		}
	}
	return nil
}

// calleeBoundariesFromAdjacency reconstructs the epistemic boundaries a
// forward call walk records: for each expanded node, every out-edge of a
// traversed kind whose target the walk could not follow (an unresolved /
// external-prefixed stub) is one boundary, deduplicated by (source,
// target) and capped. Mirrors the inline recordBoundaries path in bfs:
// same kind gate (the walk's kindSet), same dropped-target set, same
// "callees" direction, no SeedName.
func calleeBoundariesFromAdjacency(
	expandedIDs []string,
	outAdj map[string][]*graph.Edge,
	kindSet map[graph.EdgeKind]bool,
) []graph.EpistemicBoundary {
	seen := make(map[string]bool)
	var out []graph.EpistemicBoundary
	for _, id := range expandedIDs {
		for _, edge := range outAdj[id] {
			if edge == nil || !kindSet[edge.Kind] {
				continue
			}
			if !graph.IsUnresolvedTarget(edge.To) && !strings.HasPrefix(edge.To, "external::") {
				continue
			}
			reason, ok := graph.ClassifyDroppedTarget(edge.To, edge.Kind)
			if !ok {
				continue
			}
			key := edge.From + "\x00" + edge.To
			if seen[key] {
				continue
			}
			seen[key] = true
			target := edge.To
			if graph.IsUnresolvedTarget(edge.To) {
				target = graph.UnresolvedName(edge.To)
			}
			out = append(out, graph.EpistemicBoundary{
				SeedID:    edge.From,
				Target:    target,
				EdgeKind:  string(edge.Kind),
				Reason:    reason,
				Direction: "callees",
			})
			if len(out) >= 50 {
				return out
			}
		}
	}
	return out
}

// expandCallSites fans an edge that carries additional Meta["call_sites"]
// into one usage edge per recorded site (the primary stays in FilePath/Line).
// Rows are deduped by (from, file, line) so a site that already exists as its
// own edge is never double-counted; real edges are emitted first so they win
// over a call_sites clone at the same location. Cheap no-op when no edge in
// the set carries call_sites (the overwhelmingly common case).
func expandCallSites(edges []*graph.Edge) []*graph.Edge {
	hasSites := false
	for _, e := range edges {
		if len(graph.CallSites(e)) > 0 {
			hasSites = true
			break
		}
	}
	if !hasSites {
		return edges
	}
	key := func(from, file string, line int) string {
		return from + "\x00" + file + ":" + strconv.Itoa(line)
	}
	seen := make(map[string]struct{}, len(edges))
	out := make([]*graph.Edge, 0, len(edges))
	// Pass 1: the real edges, deduped by location.
	for _, e := range edges {
		k := key(e.From, e.FilePath, e.Line)
		if _, dup := seen[k]; dup {
			continue
		}
		seen[k] = struct{}{}
		out = append(out, e)
	}
	// Pass 2: expand call_sites, skipping any location already emitted.
	for _, e := range edges {
		for _, site := range graph.CallSites(e) {
			file, line := graph.SplitCallSite(site)
			if file == "" {
				continue
			}
			k := key(e.From, file, line)
			if _, dup := seen[k]; dup {
				continue
			}
			seen[k] = struct{}{}
			clone := *e
			clone.FilePath = file
			clone.Line = line
			out = append(out, &clone)
		}
	}
	return out
}

// isUsageEdgeKind reports whether an edge kind counts as a "usage"
// for FindUsages — the same predicate the legacy inline if-chain
// evaluated. Hoisted into a function so the kind set can be reused
// across the pre-filter pass and the materialisation pass without
// drifting.
func isUsageEdgeKind(k graph.EdgeKind) bool {
	switch k {
	case graph.EdgeCalls, graph.EdgeReferences,
		graph.EdgeInstantiates,
		graph.EdgeReturns, graph.EdgeTypedAs,
		graph.EdgeImplements, graph.EdgeExtends,
		graph.EdgeComposes,
		// Import / re-export statements ARE usages: every LSP reference
		// set counts the `from x import name` / `export {name} from …`
		// line, and a symbol whose only consumers import it through a
		// façade otherwise looks unused. These edges point at symbol
		// nodes only where extractors bind them (Python, JS/TS); Go
		// imports target package nodes, so Go results are unaffected.
		graph.EdgeImports, graph.EdgeReExports,
		// Value-side uses of variables and fields ARE usages: a field
		// provably read inside its own class answered find_usages empty
		// while the store held its resolved reads/writes edges (the C#
		// field-identifier emitter's whole point). EdgeAccessesField is
		// deliberately absent — it is the synthesized union of these two
		// riding the same sites and would double-count every one.
		graph.EdgeReads, graph.EdgeWrites,
		graph.EdgeProvides, graph.EdgeConsumes,
		graph.EdgeReadsConfig, graph.EdgeWritesConfig,
		graph.EdgeUsesEnv, graph.EdgeConfigures,
		graph.EdgeMounts, graph.EdgeExposes,
		graph.EdgeDependsOn:
		return true
	}
	return false
}

// isFrameworkContractEdgeKind reports whether an edge kind is framework
// plumbing — the contract graph analyze kind=routes / contracts / models
// consume — rather than a genuine code usage. find_usages suppresses these
// when the queried symbol is a callable code symbol (a @Bean factory method's
// only "usage" was the provides contract edge from its @Configuration class),
// while keeping them for DI-token / topic / table / route contract targets.
func isFrameworkContractEdgeKind(k graph.EdgeKind) bool {
	switch k {
	case graph.EdgeProvides, graph.EdgeConsumes,
		graph.EdgeHandlesRoute, graph.EdgeProducesTopic, graph.EdgeConsumesTopic,
		graph.EdgeModelsTable, graph.EdgeRendersChild:
		return true
	}
	return false
}

// isCodeSymbolKind reports whether a node is a callable code symbol —
// a function or method (constructors are methods). find_usages drops
// framework-contract incoming edges only for these targets.
func isCodeSymbolKind(n *graph.Node) bool {
	return n != nil && (n.Kind == graph.KindFunction || n.Kind == graph.KindMethod)
}

// isTestSource reports whether a node originates in test code. Used by
// QueryOptions.ExcludeTests to drop callers/users that originate in
// tests, leaving production callers. Delegates to the shared classifier
// (see graph.NodeIsTest for the stamp → owner → path authority order),
// so the filter and the output metadata can never disagree.
func (e *Engine) isTestSource(n *graph.Node) bool {
	return graph.NodeIsTest(e.g, n)
}

func dedup(edges []*graph.Edge) []*graph.Edge {
	if len(edges) == 0 {
		return edges
	}
	// Struct key avoids the per-edge string concatenation the old
	// implementation paid (e.From + "->" + e.To + ":" + kind) — on a
	// 4 000-edge file the alloc storm dominated GetFileSymbols.
	type dedupKey struct {
		from string
		to   string
		kind graph.EdgeKind
	}
	seen := make(map[dedupKey]struct{}, len(edges))
	out := make([]*graph.Edge, 0, len(edges))
	for _, e := range edges {
		if e == nil {
			continue
		}
		k := dedupKey{from: e.From, to: e.To, kind: e.Kind}
		if _, ok := seen[k]; ok {
			continue
		}
		seen[k] = struct{}{}
		out = append(out, e)
	}
	return out
}

// hasPrefixFold reports whether s begins with prefix under simple case
// folding, without allocating. prefix is already lowercased by the caller.
func hasPrefixFold(s, prefix string) bool {
	return len(s) >= len(prefix) && strings.EqualFold(s[:len(prefix)], prefix)
}

// containsFold reports whether sub occurs in s under simple case folding,
// without allocating. sub is already lowercased by the caller.
//
// The window loop only pays an EqualFold where the first byte can actually
// fold-match: an ASCII byte is prechecked with |0x20 case-folding, and any
// non-ASCII byte always takes the EqualFold path. Known limitation shared by
// every byte-windowed fold: length-changing Unicode foldings (the three-byte
// Kelvin sign folding to a one-byte k) cannot match, because the window is
// sized in bytes. Identifiers do not contain those; the degraded-mode
// fallback accepts the trade, and production search uses the indexed
// backend.
func containsFold(s, sub string) bool {
	if len(sub) == 0 {
		return true
	}
	c := sub[0] | 0x20
	for i := 0; i+len(sub) <= len(s); i++ {
		if b := s[i]; b < 0x80 && b|0x20 != c {
			continue
		}
		if strings.EqualFold(s[i:i+len(sub)], sub) {
			return true
		}
	}
	return false
}
