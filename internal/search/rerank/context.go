package rerank

import (
	"math"
	"strings"
	"time"

	"github.com/zzet/gortex/internal/graph"
)

// AnalysisMetric is the bounded per-candidate analysis state consumed by rerank.
// Values are already normalized where appropriate so callers can load one batch
// without exposing or retaining whole-graph analysis maps.
type AnalysisMetric struct {
	CommunityID string
	Authority   float64
	Hub         float64
}

// CentralityResult carries scores plus the bounded-walk receipt. Truncated
// results are lower bounds and are confidence-damped before scoring.
type CentralityResult struct {
	Scores      map[string]float64
	NodeCount   int
	EdgeCount   int
	NodeBatches int
	EdgeBatches int
	Truncated   bool
}

// Context bundles the read-only data signals need at scoring time.
// All fields are optional; signals must gracefully degrade when a
// data source is absent. The zero value is a valid Context.
type Context struct {
	// ObserveTiming receives request-local costs without changing ranking or reads.
	// Nil leaves clock/count collection disabled.
	ObserveTiming func(Timing)

	// Graph is the indexed knowledge graph reader. Required for any
	// signal that reads node metadata or walks edges (FanIn, FanOut,
	// MinHash). When nil, those signals contribute 0. Held as the
	// `graph.Reader` interface so the editor-overlay path can pass
	// an `*OverlaidView` here and have rerank signals score against
	// the overlay's shadow graph just like base.
	Graph graph.Reader

	// EdgeBatches, when set, answers prepare's two batched candidate edge
	// reads in place of Graph. It must serve exactly the content Graph
	// serves: the answer path sets it to a memoized reader over the same
	// immutable routed stack, so a candidate's edges are read once per
	// published generation rather than once per search. Every other read
	// still goes to Graph.
	EdgeBatches interface {
		GetOutEdgesByNodeIDs(ids []string) map[string][]*graph.Edge
		GetInEdgesByNodeIDs(ids []string) map[string][]*graph.Edge
	}

	// QueryClass is the detected shape of the query (symbol / concept
	// / path / signature). It scales the bm25 and semantic signal
	// weights inside Pipeline.Rerank. The zero value QueryClassUnknown
	// tells Rerank to auto-detect via ClassifyQuery; a caller — the
	// search_symbols query_class argument — may pin it instead.
	QueryClass QueryClass

	// Alpha, when > 0, switches the bm25/semantic weight scaling from
	// the discrete per-class classWeightTable to the continuous
	// interpolation continuousClassMultiplier(Alpha, …). Callers set
	// it to AlphaForContinuous(query) so a half-identifier query gets
	// an in-between blend instead of a hard class bucket. The zero
	// value preserves the legacy discrete behaviour, so any caller
	// that does not opt in (and every direct-Rerank test) is
	// unaffected.
	Alpha float64

	// ProseMode tunes the rerank for a documentation query -- one that
	// searches the prose-section (KindDoc) corpus. When set,
	// Pipeline.Rerank applies the proseWeightTable on top of the
	// per-signal weights: it lifts the bm25 and semantic channels
	// (the only signals that score prose well) and suppresses the
	// code-structural signals (api_signature / type_signature /
	// definition_bias) that are meaningless for a prose section with
	// no call graph, no signature, and no definition keyword. The
	// adjustment is INDEPENDENT of the Alpha / class lever -- it
	// multiplies whatever class-scaled weight those produce -- so a
	// docs query still gets its query-shape blend AND the prose
	// profile. The zero value is off; every code query is unaffected.
	ProseMode bool

	// CommunityOf maps a node ID to its detected community ID. When
	// nil, the community signal contributes 0.
	CommunityOf func(nodeID string) string

	// AnalysisMetricsOf loads community and normalized HITS values for the
	// current candidate batch. prepare calls it exactly once per distinct
	// candidate slice; it replaces whole-graph materialization on bounded
	// search paths. Missing rows fall back to CommunityOf/AuthorityOf/HubOf.
	AnalysisMetricsOf func(nodeIDs []string) map[string]AnalysisMetric

	// RepoPrefix and ProjectID name the session's home repo and
	// project. Used by the community signal to score candidates by
	// locality. Both empty disables the locality side of the signal.
	RepoPrefix string
	ProjectID  string

	// ChurnOf returns a modification-count proxy. When non-nil the
	// churn signal uses it (typical source: MCP symbol history). When
	// nil the churn signal falls back to Node.Meta["churn"] or, if
	// absent, the count of distinct authors in
	// Node.Meta["last_authored"]. Returning 0 means "no churn data".
	ChurnOf func(nodeID string) int

	// CoChangeOf returns, for a file path, the set of file paths that
	// co-change with it mapped to an association score in [0, 1].
	// Source: the EdgeCoChange enrichment, exposed by the MCP server.
	// When nil the co-change signal sits at 0.
	CoChangeOf func(filePath string) map[string]float64

	// FeedbackOf returns a per-symbol "useful to past tasks" score in
	// [-1, 1] (the same shape as feedbackManager.GetSymbolScore).
	// When nil the feedback component sits at 0.
	FeedbackOf func(nodeID string) float64

	// FrecencyBoostOf returns a frecency multiplier in
	// [1, maxFrecencyBoost] (the same shape as frecencyTracker.BoostFor).
	// 1.0 means "no boost". When nil it's treated as 1.0 everywhere.
	FrecencyBoostOf func(nodeID string) float64

	// ComboBoostOf returns a (query, symbol) co-occurrence multiplier
	// in [1, comboMaxBoost]. 1.0 means "no boost". When nil it's
	// treated as 1.0 everywhere.
	ComboBoostOf func(nodeID string) float64

	// EmbedText returns a normalised embedding vector for arbitrary
	// text, or nil when it cannot embed. It is the substrate for the
	// on-the-fly SemanticCosineSignal: the signal assembles a compact
	// text for each candidate (name + qualname + path + signature) and
	// embeds it here, then cosines the result against QueryVec. Wired by
	// the MCP server to the always-available in-process static provider;
	// nil disables the semantic-cosine channel (it then sits at 0). The
	// closure must be safe for concurrent use.
	EmbedText func(text string) []float32

	// QueryVec is the pre-computed embedding of the raw query, produced
	// once per request with the same provider EmbedText wraps. Empty
	// disables the semantic-cosine channel. Kept on the Context so the
	// per-candidate signal pays only for one candidate embed + one dot
	// product, never a second query embed.
	QueryVec []float32

	// Centrality runs a Random-Walk-with-Restart (Personalized
	// PageRank) from the given seed node IDs and returns each reachable
	// node's proximity score. It is the data source for ProximitySignal.
	// Set by the MCP server's buildRerankContext from the adjacency
	// snapshot; nil on a cold graph (the signal then sits at 0). The
	// returned scores need not be pre-normalised — prepare() rescales
	// them to [0,1] against the batch maximum.
	Centrality func(seeds []string) map[string]float64

	// BatchedCentrality is the bounded interactive variant. It receives the
	// complete candidate IDs so callers can build a capped neighborhood in a
	// fixed number of batch reads. When set it takes precedence over Centrality.
	BatchedCentrality func(seeds, candidateIDs []string) CentralityResult

	// CentralitySeedCount caps how many top candidates seed the RWR
	// walk. 0 means use defaultCentralitySeeds.
	CentralitySeedCount int

	// AuthorityOf and HubOf return a node's HITS authority and hub
	// scores, each normalised into [0, 1] against the graph maxima.
	// Authority measures "depended on by load-bearing code"; hub
	// measures "calls many load-bearing pieces". The HITS signal
	// uses both -- it rewards authority but penalises a high hub
	// score so a called-by-everything utility does not score like a
	// true authority. When either is nil the HITS signal sits at 0.
	AuthorityOf func(nodeID string) float64
	HubOf       func(nodeID string) float64

	// Now provides the current unix time in seconds. Overridable for
	// tests; zero means "use time.Now().Unix()".
	Now int64

	// --- Internal scratch space populated by prepare(). ---

	// centralityScores maps a candidate node ID → its RWR/PPR
	// proximity to the query seeds, normalised to [0,1] against the
	// batch maximum. Populated by prepare() when Centrality is wired;
	// read by ProximitySignal. Nil when no centrality provider is set.
	centralityScores  map[string]float64
	centralityReceipt CentralityResult

	// analysisMetrics is request-scoped state loaded in one batch by
	// AnalysisMetricsOf. It is reset for each Prepare call.
	analysisMetrics map[string]AnalysisMetric

	// communityCount maps community ID → number of candidates in that
	// community. Used by the community signal to detect topic clusters.
	communityCount map[string]int
	// maxCommunityCount is the largest value in communityCount.
	maxCommunityCount int
	// multiRepoBatch records whether this candidate batch spans more than
	// one repository (counting only owned candidates — synthetic global
	// externals carry no prefix and belong to none). The same-repo affinity
	// boost is only a discriminator when it is true; on a single-repo batch
	// every first-party candidate would saturate identically and the boost
	// would overwrite the community score that actually separates them.
	multiRepoBatch bool

	// fanInMax / fanOutMax cache the maximum fan counts across the
	// current candidate set so the log-normalised contributions stay
	// in [0,1].
	fanInMax  int
	fanOutMax int

	// churnMax caches max churn across the candidate set.
	churnMax int

	// candidateIDs is the set of node IDs in the current batch.
	// MinHash uses it to only count similarity edges that point to
	// other candidates in the same batch (cluster-cohesion signal).
	candidateIDs map[string]struct{}

	// nameGroupCount maps a lowercased candidate name → how many
	// candidates in the batch share it. OverloadProminenceSignal reads
	// it to fire only on a genuine same-name collision (an ambiguous
	// query where several symbols answer to the same identifier), so
	// non-colliding candidates are never perturbed.
	nameGroupCount map[string]int

	// fileGroups maps each file path → candidates from that file in
	// batch order. The file-coherence signal reads this to detect
	// "many candidates share this file" multi-chunk evidence and
	// boost the lead candidate from each file. Files with a single
	// candidate are present but contribute zero to the signal.
	fileGroups map[string][]*Candidate
	// fileScoreSum maps file path → sum of BM25-rank weights for the
	// candidates from that file (lower text rank = higher weight).
	// Drives the per-file evidence score; the multi-chunk signal
	// boosts the per-file lead by `fileScoreSum / maxFileScoreSum`.
	fileScoreSum map[string]float64
	// maxFileScoreSum is the largest value in fileScoreSum across
	// the batch; used to normalise the boost into [0, 1]. Zero when
	// no candidate has a usable text rank.
	maxFileScoreSum float64

	// pathPenaltyCache memoises the path-penalty multiplier per file
	// path within a single Rerank call so the regex-heavy rubric
	// runs once per file rather than once per candidate. Bounded by
	// the candidate set's file count.
	pathPenaltyCache map[string]float64

	// testNameStems holds the normalised name stems of every test
	// candidate in the batch (TestValidateToken -> validatetoken).
	// SourceBiasSignal reads it to promote a production symbol over
	// its test only when both co-occur in the result set.
	testNameStems map[string]struct{}

	// outEdgeCache / inEdgeCache hold the per-candidate edge slices
	// fetched in one batched round-trip from Graph at prepare() time.
	// FanInSignal / FanOutSignal / MinHashSignal read from these
	// instead of calling Graph.GetIn/OutEdges per-candidate, which on
	// a disk backend collapses ~6N per-search round-trips
	// (~150 calls × 14ms ≈ 2 s) into 2. Empty when Graph is nil.
	// Callers must use the inEdges / outEdges accessors so signals
	// stay graph-agnostic.
	outEdgeCache map[string][]*graph.Edge
	inEdgeCache  map[string][]*graph.Edge

	// preparedCands is the candidate slice identity prepare() was last
	// called against. Pipeline.Rerank skips re-prepare when the same
	// slice header is seen back-to-back so callers that pre-call
	// Prepare for per-phase timing do not pay for it twice. The check
	// is identity-only (same slice, same length) — any mutation that
	// reallocates resets it.
	preparedCands []*Candidate

	// cachePreSeeded is the caller's promise (via SeedEdgeCaches with
	// preSeeded=true) that outEdgeCache / inEdgeCache already cover
	// the candidate set the next Prepare call will see. When set,
	// prepare() skips the batched edge fetch entirely — the bundle
	// path's edges are authoritative and a second fetch is pure
	// overhead. Reset by the caller (typically the engine, after each
	// Search) to keep the flag from leaking across reranks.
	cachePreSeeded bool
}

// Prepare populates the internal scratch fields used by every signal
// once per Rerank call. Exposed so callers that want to time prepare
// separately (the search hot path) can call it explicitly; in that
// case the subsequent Rerank call detects the prepared state and
// skips the duplicate work. Safe to call multiple times against the
// same slice — it's a full reset on each call.
func (c *Context) Prepare(cands []*Candidate) { c.prepare(cands) }

// SeedEdgeCaches installs pre-fetched in/out edge maps the caller
// already gathered (today: from the SymbolBundleSearcherBackend hot
// path). The maps are merged into the context — IDs already in the
// cache keep their existing entry, new IDs append. The accompanying
// flag tells prepare() the caches are authoritative for the
// candidate set so it can skip its own batched edge fetch on the
// next Prepare call.
//
// IDs missing from the caller's bundle (vector-channel hits, fallback
// substring matches) still get fetched the slow per-candidate way
// through the outEdges / inEdges accessors when a signal asks for
// them — the seed is a best-effort fast path, not a contract that
// every candidate's edges are present. Callers MUST set
// cachePreSeeded only when the seed covers the expected candidate set
// (i.e. when the bundle backend returned a result for every BM25
// hit in the merged candidate slice).
func (c *Context) SeedEdgeCaches(inEdges, outEdges map[string][]*graph.Edge, preSeeded bool) {
	if c.outEdgeCache == nil {
		c.outEdgeCache = make(map[string][]*graph.Edge, len(outEdges))
	}
	for id, es := range outEdges {
		if _, dup := c.outEdgeCache[id]; dup {
			continue
		}
		c.outEdgeCache[id] = es
	}
	if c.inEdgeCache == nil {
		c.inEdgeCache = make(map[string][]*graph.Edge, len(inEdges))
	}
	for id, es := range inEdges {
		if _, dup := c.inEdgeCache[id]; dup {
			continue
		}
		c.inEdgeCache[id] = es
	}
	if preSeeded {
		c.cachePreSeeded = true
	}
}

// CachePreSeeded reports whether the caller has signaled (via
// SeedEdgeCaches with preSeeded=true) that the edge caches cover the
// candidate set the next Prepare call will see. Exposed so the
// MCP handler can report a cache-hit-rate / cache-pre-seeded boolean
// in its debug log without grepping internal state.
func (c *Context) CachePreSeeded() bool { return c.cachePreSeeded }

// CentralityTelemetry returns the receipt from the most recent Prepare call.
func (c *Context) CentralityTelemetry() CentralityResult { return c.centralityReceipt }

// InheritEdgeCacheFrom shares the source context's edge caches +
// cachePreSeeded flag onto c. Used by the engine to give per-call
// inner reranks access to the handler-built bundle cache without
// inheriting the handler's session-aware signals (locality, combo,
// frecency, feedback). Cheap pointer-copy of the map references; the
// inner rerank's prepare() reads through them and any backfills it
// triggers land in the SHARED map so subsequent calls benefit. Pass
// nil to clear.
func (c *Context) InheritEdgeCacheFrom(src *Context) {
	if c == nil || src == nil {
		return
	}
	c.outEdgeCache = src.outEdgeCache
	c.inEdgeCache = src.inEdgeCache
	c.cachePreSeeded = src.cachePreSeeded
	if c.EdgeBatches == nil {
		c.EdgeBatches = src.EdgeBatches
	}
}

// EdgeCacheHitRate reports the fraction of nodeIDs that have an entry
// in the in OR out edge cache. 0.0 when the caches are empty; 1.0 when
// every input id has a cache entry on both sides. Used by the
// MCP handler to surface "did the bundle path actually catch?" on
// the search_symbols debug log without exposing internal state.
func (c *Context) EdgeCacheHitRate(ids []string) float64 {
	if len(ids) == 0 {
		return 0
	}
	hits := 0
	for _, id := range ids {
		// An id counts as a hit if BOTH the in-edge cache and the
		// out-edge cache have an entry for it — that's the contract
		// the bundle pre-seed promises. A half-seeded id (only one
		// side cached) is a near-miss the prepare() pass would still
		// have to satisfy by fetching the missing side.
		_, hasOut := c.outEdgeCache[id]
		_, hasIn := c.inEdgeCache[id]
		if hasOut && hasIn {
			hits++
		}
	}
	return float64(hits) / float64(len(ids))
}

// now returns the active timestamp (test-injectable when Now != 0).
func (c *Context) now() int64 {
	if c.Now != 0 {
		return c.Now
	}
	return time.Now().Unix()
}

// prepare populates the internal scratch fields once per Rerank call.
// Idempotent — safe to call again after mutating the candidate slice.
//
// Edge fetches happen in two batched round-trips (one inbound, one
// outbound) collected from every candidate's ID up front. On a disk
// backend each per-candidate GetInEdges / GetOutEdges call
// costs ~14ms; batching collapses ~150 round-trips per Rerank
// into 2.
//
// Bundle pre-seed fast path: when the caller has set cachePreSeeded
// (via SeedEdgeCaches with preSeeded=true), prepare keeps the existing
// caches in place and skips the batched edge fetch entirely. The
// fanInMax / fanOutMax stats are computed from the already-cached
// maps — same numbers, no cgo. This is the load-bearing skip the
// SymbolBundleSearcherBackend path depends on: the bundle's edges
// were already gathered server-side; a second round-trip here would
// pure-overhead the win.
func (c *Context) prepare(cands []*Candidate) {
	observer := c.ObserveTiming
	timing := newPrepareTiming(observer)
	c.preparedCands = cands
	c.communityCount = make(map[string]int, len(cands))
	c.maxCommunityCount = 0
	c.candidateIDs = make(map[string]struct{}, len(cands))
	c.nameGroupCount = make(map[string]int, len(cands))
	c.fanInMax = 0
	c.fanOutMax = 0
	c.churnMax = 0
	c.fileGroups = make(map[string][]*Candidate, len(cands))
	c.fileScoreSum = make(map[string]float64, len(cands))
	c.maxFileScoreSum = 0
	c.pathPenaltyCache = make(map[string]float64, len(cands))
	c.testNameStems = make(map[string]struct{}, len(cands))
	// Preserve the seeded edge caches when the caller signaled
	// cachePreSeeded; the legacy reset path below the candidate walk
	// only runs when the caches are NOT authoritative.
	if !c.cachePreSeeded {
		c.outEdgeCache = nil
		c.inEdgeCache = nil
	}

	// Collect candidate IDs before scoring so analysis-backed signals can load
	// their rows in one bounded query instead of one lookup per candidate.
	ids := make([]string, 0, len(cands))
	for _, cand := range cands {
		if cand == nil || cand.Node == nil {
			continue
		}
		c.candidateIDs[cand.Node.ID] = struct{}{}
		ids = append(ids, cand.Node.ID)
	}
	if timing != nil {
		timing.Candidates = len(ids)
		timing.mark(&timing.Bookkeeping)
	}
	c.analysisMetrics = nil
	if c.AnalysisMetricsOf != nil && len(ids) > 0 {
		c.analysisMetrics = c.AnalysisMetricsOf(ids)
		if timing != nil {
			timing.mark(&timing.Metrics)
		}
	}

	// Populate the non-edge scratch fields from the candidate batch.
	batchRepo := ""
	c.multiRepoBatch = false
	for _, cand := range cands {
		if cand == nil || cand.Node == nil {
			continue
		}
		if p := cand.Node.RepoPrefix; p != "" {
			if batchRepo == "" {
				batchRepo = p
			} else if p != batchRepo {
				c.multiRepoBatch = true
			}
		}
		if nm := strings.ToLower(cand.Node.Name); nm != "" {
			c.nameGroupCount[nm]++
		}

		if com := c.communityFor(cand.Node.ID); com != "" {
			c.communityCount[com]++
			if c.communityCount[com] > c.maxCommunityCount {
				c.maxCommunityCount = c.communityCount[com]
			}
		}

		ch := c.churnFor(cand.Node)
		if ch > c.churnMax {
			c.churnMax = ch
		}

		// File grouping: collect candidates by FilePath and sum their
		// inverse-rank weights so the file-coherence signal can detect
		// multi-chunk evidence + identify the per-file lead candidate.
		fp := cand.Node.FilePath
		if fp != "" {
			c.fileGroups[fp] = append(c.fileGroups[fp], cand)
			if cand.TextRank >= 0 {
				w := 1.0 / float64(cand.TextRank+1)
				c.fileScoreSum[fp] += w
				if c.fileScoreSum[fp] > c.maxFileScoreSum {
					c.maxFileScoreSum = c.fileScoreSum[fp]
				}
			}
			// Record test-candidate name stems so SourceBiasSignal can
			// detect a co-occurring source/test pair without re-scanning.
			if isTestPath(fp) {
				if stem := testNameStem(cand.Node.Name); stem != "" {
					c.testNameStems[stem] = struct{}{}
				}
			}
		}
	}

	// Second pass: one batched in-edge + one out-edge round-trip
	// against Graph, scoped to the IDs that are NOT yet cached.
	// When cachePreSeeded covers every candidate (the bundle hot
	// path's typical shape), the missing slice is empty and the
	// round-trips are skipped entirely — pure cache-served fan-in /
	// fan-out. When the bundle only covers some IDs (vector or
	// fallback hits get appended without bundle edges), we fetch
	// only the uncovered tail and merge into the existing cache.
	// Skipped when Graph is nil — fan signals contribute 0.
	if c.Graph != nil && len(ids) > 0 {
		missingOut := missingEdgeIDs(ids, c.outEdgeCache)
		missingIn := missingEdgeIDs(ids, c.inEdgeCache)
		// Backfill — when the cache already covers everything, both
		// missing slices are empty and no cgo round-trip fires.
		var batches interface {
			GetOutEdgesByNodeIDs(ids []string) map[string][]*graph.Edge
			GetInEdgesByNodeIDs(ids []string) map[string][]*graph.Edge
		} = c.Graph
		if c.EdgeBatches != nil {
			batches = c.EdgeBatches
		}
		if timing != nil {
			timing.MissingOut = len(missingOut)
			timing.MissingIn = len(missingIn)
			timing.mark(&timing.Bookkeeping)
		}
		if len(missingOut) > 0 {
			if timing != nil {
				timing.mark(&timing.Bookkeeping)
			}
			fetched := batches.GetOutEdgesByNodeIDs(missingOut)
			if timing != nil {
				timing.mark(&timing.Outgoing)
			}
			if c.outEdgeCache == nil {
				c.outEdgeCache = make(map[string][]*graph.Edge, len(fetched))
			}
			for id, es := range fetched {
				c.outEdgeCache[id] = es
				if timing != nil {
					timing.OutRows += len(es)
				}
			}
			if timing != nil {
				timing.mark(&timing.MergeFan)
			}
		}
		if len(missingIn) > 0 {
			if timing != nil {
				timing.mark(&timing.Bookkeeping)
			}
			fetched := batches.GetInEdgesByNodeIDs(missingIn)
			if timing != nil {
				timing.mark(&timing.Incoming)
			}
			if c.inEdgeCache == nil {
				c.inEdgeCache = make(map[string][]*graph.Edge, len(fetched))
			}
			for id, es := range fetched {
				c.inEdgeCache[id] = es
				if timing != nil {
					timing.InRows += len(es)
				}
			}
			if timing != nil {
				timing.mark(&timing.MergeFan)
			}
		}
	}
	if timing != nil {
		timing.mark(&timing.Bookkeeping)
	}
	for _, id := range ids {
		if fi := len(c.inEdgeCache[id]); fi > c.fanInMax {
			c.fanInMax = fi
		}
		if fo := len(c.outEdgeCache[id]); fo > c.fanOutMax {
			c.fanOutMax = fo
		}
	}

	if timing != nil {
		timing.mark(&timing.MergeFan)
	}
	// Centrality: one Random-Walk-with-Restart per Rerank, seeded from
	// the strongest candidates, scored over the whole batch. Computed
	// here (not per-candidate) so the walk runs once; ProximitySignal
	// then reads the per-node result. Skipped when no provider is wired.
	c.computeCentrality(cands)
	if timing != nil {
		timing.mark(&timing.Centrality)
	}
	timing.finish(observer)
}

func (c *Context) communityFor(nodeID string) string {
	if metric, ok := c.analysisMetrics[nodeID]; ok && metric.CommunityID != "" {
		return metric.CommunityID
	}
	if c.CommunityOf != nil {
		return c.CommunityOf(nodeID)
	}
	return ""
}

func (c *Context) authorityFor(nodeID string) (float64, bool) {
	if metric, ok := c.analysisMetrics[nodeID]; ok {
		return metric.Authority, true
	}
	if c.AuthorityOf != nil {
		return c.AuthorityOf(nodeID), true
	}
	return 0, false
}

func (c *Context) hubFor(nodeID string) (float64, bool) {
	if metric, ok := c.analysisMetrics[nodeID]; ok {
		return metric.Hub, true
	}
	if c.HubOf != nil {
		return c.HubOf(nodeID), true
	}
	return 0, false
}

// computeCentrality runs the RWR walk from the batch's strongest seeds
// and stores per-candidate proximity normalised to [0,1]. No-op when
// Centrality is nil or the walk returns nothing.
func (c *Context) computeCentrality(cands []*Candidate) {
	c.centralityScores = nil
	c.centralityReceipt = CentralityResult{}
	if (c.BatchedCentrality == nil && c.Centrality == nil) || len(cands) == 0 {
		return
	}
	seeds := selectCentralitySeeds(cands, c.CentralitySeedCount)
	if len(seeds) == 0 {
		return
	}
	var raw map[string]float64
	if c.BatchedCentrality != nil {
		candidateIDs := make([]string, 0, len(cands))
		for _, cand := range cands {
			if cand != nil && cand.Node != nil {
				candidateIDs = append(candidateIDs, cand.Node.ID)
			}
		}
		c.centralityReceipt = c.BatchedCentrality(seeds, candidateIDs)
		raw = c.centralityReceipt.Scores
		c.centralityReceipt.Scores = nil
	} else {
		raw = c.Centrality(seeds)
	}
	if len(raw) == 0 {
		return
	}
	var max float64
	for _, v := range raw {
		if v > max {
			max = v
		}
	}
	if max <= 0 {
		return
	}
	scores := make(map[string]float64, len(raw))
	for id, v := range raw {
		if v <= 0 {
			continue
		}
		scores[id] = v / max
	}
	c.centralityScores = scores
}

// missingEdgeIDs returns the subset of ids whose edge slice is NOT
// already in cache. Used by prepare's backfill: when the bundle path
// pre-seeded most candidates but not all (vector / fallback hits get
// appended without bundle edges), only the uncovered ids cross the
// engine boundary. An empty result means the cache is complete — the
// fetch round-trip can be skipped entirely.
func missingEdgeIDs(ids []string, cache map[string][]*graph.Edge) []string {
	if cache == nil {
		// No pre-seed at all — caller has to fetch the full set; return
		// the input unchanged so the existing batched fetch path runs.
		return ids
	}
	missing := make([]string, 0, len(ids))
	for _, id := range ids {
		if _, ok := cache[id]; !ok {
			missing = append(missing, id)
		}
	}
	return missing
}

// outEdges returns the prepared outgoing-edge slice for nodeID. Reads
// from the prepare()-populated cache when available; falls back to a
// direct Graph.GetOutEdges call when prepare did not cache the node
// (a signal calling outside the candidate set, or Graph was nil at
// prepare time but a later mutation set it). Signals must use this
// accessor instead of calling Graph directly so the batched-fetch
// invariant holds.
func (c *Context) outEdges(nodeID string) []*graph.Edge {
	if c.outEdgeCache != nil {
		if edges, ok := c.outEdgeCache[nodeID]; ok {
			return edges
		}
	}
	if c.Graph == nil {
		return nil
	}
	return c.Graph.GetOutEdges(nodeID)
}

// inEdges is the inbound sibling of outEdges. See that doc-comment
// for the contract.
func (c *Context) inEdges(nodeID string) []*graph.Edge {
	if c.inEdgeCache != nil {
		if edges, ok := c.inEdgeCache[nodeID]; ok {
			return edges
		}
	}
	if c.Graph == nil {
		return nil
	}
	return c.Graph.GetInEdges(nodeID)
}

// churnFor consults the ChurnOf hook, then Node.Meta["churn"], then
// the distinct-author proxy. Returns 0 when no source has data.
func (c *Context) churnFor(n *graph.Node) int {
	if n == nil {
		return 0
	}
	if c.ChurnOf != nil {
		if v := c.ChurnOf(n.ID); v > 0 {
			return v
		}
	}
	if n.Meta == nil {
		return 0
	}
	switch v := n.Meta["churn"].(type) {
	case int:
		if v > 0 {
			return v
		}
	case int64:
		if v > 0 {
			return int(v)
		}
	case float64:
		if v > 0 {
			return int(v)
		}
	}
	// Fall back: distinct-author count if blame enrichment ran on
	// multiple commits. last_authored stores only the latest, so the
	// best we can do without a richer enrich pass is treat
	// authors_count when present, else 1 when at least one author
	// stamp exists, else 0.
	if v, ok := n.Meta["authors_count"]; ok {
		switch x := v.(type) {
		case int:
			if x > 0 {
				return x
			}
		case int64:
			if x > 0 {
				return int(x)
			}
		case float64:
			if x > 0 {
				return int(x)
			}
		}
	}
	if _, ok := n.Meta["last_authored"]; ok {
		return 1
	}
	return 0
}

// lastAuthoredUnix extracts the timestamp from Node.Meta["last_authored"].
// Returns 0 when absent or malformed.
func lastAuthoredUnix(n *graph.Node) int64 {
	if n == nil || n.Meta == nil {
		return 0
	}
	raw, ok := n.Meta["last_authored"]
	if !ok {
		return 0
	}
	switch v := raw.(type) {
	case map[string]any:
		switch ts := v["timestamp"].(type) {
		case int:
			return int64(ts)
		case int64:
			return ts
		case float64:
			return int64(ts)
		}
	}
	return 0
}

// normLog returns log(1+value) / log(1+max), clamped to [0, 1]. The
// log scale keeps a single dominant outlier from drowning the rest of
// the candidate set.
func normLog(value, max int) float64 {
	if value <= 0 || max <= 0 {
		return 0
	}
	out := math.Log1p(float64(value)) / math.Log1p(float64(max))
	if out < 0 {
		return 0
	}
	if out > 1 {
		return 1
	}
	return out
}
