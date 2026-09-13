package mcp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strconv"

	"github.com/zzet/gortex/internal/analysis"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/search/rerank"
)

// personalizedPageRankScoped runs a Random-Walk-with-Restart (Personalized
// PageRank) from the given seed node IDs over the adjacency snapshot
// and returns each reachable node's proximity score. It is the seam the
// rerank pipeline's ProximitySignal (and context_closure's proximity
// mode) reach centrality through.
//
// Walks flow through a Merkle-keyed cache (see ppr_cache.go) so repeated
// walks on an unchanged graph — or on packages that did not change
// between snapshots — return instantly instead of re-iterating the whole
// CSR. The cache is bypassed when disabled (GORTEX_PPR_CACHE_DISABLE) or
// when the snapshot has no package roots.
//
// Every entry the walk stores or reads is namespaced by an explicit scope
// naming the snapshot that produced it, so one request's ranking can never be
// served from a different snapshot's cached scores. The walk key alone cannot
// do that job: it is content-addressed on the seed neighbourhood only, so a
// routed view, a session's editor buffers and the shared corpus all collide on
// it. A caller that cannot name its snapshot passes the zero scope, which
// computes the walk and returns it without touching the cache at all.
func (s *Server) personalizedPageRankScoped(scope pprCacheScope, snap *analysis.AdjacencySnapshot, seeds []string) map[string]float64 {
	if snap == nil || len(seeds) == 0 {
		return nil
	}
	cache := s.pprCache
	// topK caps the walk to its highest-scoring nodes before it is cached,
	// so a single retained entry stays a few hundred KB instead of a full-
	// graph-sized map. Applied on both paths so a cached and an uncached
	// walk return the same result.
	topK := pprCacheDefaultTopK
	if cache != nil {
		topK = cache.topK
	}
	if cache == nil || !cache.enabled {
		return snap.PersonalizedPageRankTopK(seeds, 0, topK)
	}
	// Merkle-keyed walk cache: the key embeds the per-package content
	// roots the walk depends on, so an unchanged walk hits even across
	// a snapshot rebuild, and only a walk touching a changed package
	// recomputes. An empty key (no package roots, no seed resolves, or a
	// scope that names no shareable identity) falls through to an uncached
	// walk.
	key := scope.key(snap.WalkCacheKey(seeds, 0))
	if key != "" {
		if scores, ok := cache.get(key); ok {
			return scores
		}
	}
	scores := snap.PersonalizedPageRankTopK(seeds, 0, topK)
	cache.put(key, scores)
	return scores
}

// selectedSnapshotReader is the reader a request reads through when it is
// something other than the shared indexed corpus: the session's editor-buffer
// view, or a view routed to a checkout of its own. It returns nil for a
// request the base corpus answers, which is the one case where the shared
// analysis pass's whole-graph snapshot describes the same content the request
// reads.
//
// The routed test is readsOwnCheckout, NOT routed(). A labelled base selector
// carries a reader — a filter narrowing the shared corpus to the one
// repository its graph owns (view_request.go:1114-1119) — while reading the
// very bytes the shared analysis pass indexed. routed() is reader-presence
// and cannot tell the two apart (view_request.go:170-176); treating the
// narrowed base as a selected snapshot would trade the published whole-graph
// CSR for a rebuilt bounded one, and take a private cache namespace, for
// content the base snapshot already describes exactly.
func (s *Server) selectedSnapshotReader(ctx context.Context) graph.Reader {
	if v := OverlayViewFromContext(ctx); v != nil {
		return v
	}
	if view := requestViewFromContext(ctx); view.readsOwnCheckout() {
		return view.reader
	}
	return nil
}

// proximityAdjacencyDepth is how far past the scored node set a bounded
// proximity CSR expands. The scored set is already the CSR's root set, so
// every path between two scored nodes is present at depth 1; the extra ring
// exists so mass leaves the set the way it does in the whole-graph CSR the
// unrouted path walks. It matches rerankBoundedCentrality's depth.
const proximityAdjacencyDepth = 2

// proximityAdjacencyNodeHeadroom is how many nodes past the root set the
// bounded build may admit. Adding it to the root count (rather than using a
// flat cap) is what guarantees every node the caller must score survives the
// node bound: BuildBoundedAdjacencySnapshot seeds `seen` with the roots
// before it expands (adjacency_bounded.go:47-63), so a cap below the root
// count would drop roots and score them zero.
const proximityAdjacencyNodeHeadroom = 4096

// requestProximityAdjacency returns the adjacency snapshot a request's seeded
// walks must run over, plus the cache scope that snapshot answers under.
//
// scored is the exact node set the caller will read scores for. It is the
// ROOT set of a bounded build, not the seed set: BuildBoundedAdjacencySnapshot
// expands `depth` hops out of its roots (adjacency_bounded.go:38-39,66), so a
// CSR rooted on the seeds alone simply does not contain a node further than
// `depth` hops away — and a walk over it returns no score for that node, which
// a caller reading a score map cannot tell from "unreachable". Rooting on the
// scored set instead puts every node the caller asks about in the CSR, and
// every path between two of them with it (a closure is a BFS from the seeds,
// so every intermediate on a seed→member path is itself a member). This is the
// construction rerankBoundedCentrality already uses: roots = the candidates it
// scores, walk seeds = the query seeds (analysis_lazy_consumers.go:174-176).
//
// A request that selected a view reads a graph the shared analysis pass never
// saw: the pass runs over the indexed corpus, and its snapshot is published
// process-wide. Ranking a selected view's closure with those scores is the
// mixed view hazard — a correct graph scored from another snapshot. An
// unrouted request keeps reading the shared whole-graph snapshot, which
// describes exactly the corpus it is reading and needs no bounding.
//
// A nil snapshot means "no proximity signal": the caller ranks by graph
// distance alone.
func (s *Server) requestProximityAdjacency(ctx context.Context, seeds, scored []string) (*analysis.AdjacencySnapshot, pprCacheScope) {
	if reader := s.selectedSnapshotReader(ctx); reader != nil {
		// The bounded build is a batched read loop over the selected reader
		// and BuildBoundedAdjacencySnapshot takes no context of its own, so
		// the abandonment check happens here, before the loop is entered. A
		// nil snapshot is the already-supported "no proximity signal"
		// degrade, so a cancelled request stops paying for a CSR nobody will
		// read instead of building one for a response that never ships.
		if ctx != nil && ctx.Err() != nil {
			return nil, pprCacheScope{}
		}
		roots := mergeSortedUniqueIDs(scored, seeds)
		maxNodes := len(roots) + proximityAdjacencyNodeHeadroom
		maxEdges := 4 * maxNodes
		snap, stats := analysis.BuildBoundedAdjacencySnapshot(
			reader, roots, proximityAdjacencyDepth, maxNodes, maxEdges)
		if stats.NodeCount == 0 {
			return nil, pprCacheScope{}
		}
		// The root set and the caps are part of this snapshot's identity: the
		// content-addressed walk key describes the seed neighbourhood only, so
		// two closures over one view that bounded differently would otherwise
		// share an entry.
		return snap, walkCacheScope(ctx, pprWalkSourceSelectedReader).
			withRoots(boundedSnapshotDigest(proximityAdjacencyDepth, maxNodes, maxEdges, roots))
	}
	snap := s.getAdjacency()
	if snap == nil {
		return nil, pprCacheScope{}
	}
	return snap, walkCacheScope(ctx, pprWalkSourceSharedAnalysis)
}

// rerankBoundedMaxNodes / rerankBoundedMaxEdges are the hard caps the search
// rerank pass's bounded CSR carries. They are flat (not root-relative like the
// closure path's headroom) because the rerank candidate set is already capped
// by the search limit, and a rerank that blew past them would turn an
// interactive query into a whole-graph materialization.
const (
	rerankBoundedMaxNodes = 4096
	rerankBoundedMaxEdges = 16384
)

// boundedCentralityForRequest is the rerank pipeline's centrality seam: it
// builds the bounded call/reference CSR the candidates are scored over from
// the reader THIS request reads through, and runs the seeded walk over it
// through the snapshot-scoped walk cache.
//
// The memoisation is what makes a repeated query cheap. The walk is the
// expensive half — BuildBoundedAdjacencySnapshot is a bounded batched read,
// the walk iterates the CSR to convergence — and without a namespace the
// walk could not be cached at all: the content-addressed walk key describes
// the seed neighbourhood only, so the base corpus, a routed checkout and a
// session's editor buffers all collide on it. The scope supplies the missing
// identity (which view produced the CSR, and which root set bounded it), so
// one request's ranking can never be served from another snapshot's scores.
//
// The snapshot is determined by the root SET, the reader and the caps —
// BuildBoundedAdjacencySnapshot sorts and dedupes its roots before expanding
// (adjacency_bounded.go:46) — so the digest names it exactly, truncated or
// not.
func (s *Server) boundedCentralityForRequest(ctx context.Context, seeds, candidateIDs []string) rerank.CentralityResult {
	// Same reasoning as requestProximityAdjacency: the build is uninterruptible
	// once entered, so an abandoned request is refused at the door. An empty
	// result is the shape the caller already handles for an empty neighbourhood.
	if ctx != nil && ctx.Err() != nil {
		return rerank.CentralityResult{}
	}
	snapshot, stats := analysis.BuildBoundedAdjacencySnapshot(
		s.readerFor(ctx), candidateIDs, proximityAdjacencyDepth, rerankBoundedMaxNodes, rerankBoundedMaxEdges)
	return rerank.CentralityResult{
		Scores:      s.personalizedPageRankScoped(boundedCentralityScope(ctx, candidateIDs), snapshot, seeds),
		NodeCount:   stats.NodeCount,
		EdgeCount:   stats.EdgeCount,
		NodeBatches: stats.NodeBatches,
		EdgeBatches: stats.EdgeBatches,
		Truncated:   stats.Truncated,
	}
}

// boundedCentralityScope names the snapshot boundedCentralityForRequest just
// built: the view the request reads (empty for the shared corpus, uncacheable
// for editor buffers) plus the root set and caps that bounded the CSR. A
// request with no candidates names no snapshot and stays uncacheable.
func boundedCentralityScope(ctx context.Context, candidateIDs []string) pprCacheScope {
	roots := mergeSortedUniqueIDs(candidateIDs)
	if len(roots) == 0 {
		return pprCacheScope{}
	}
	return walkCacheScope(ctx, pprWalkSourceSelectedReader).
		withRoots(boundedSnapshotDigest(proximityAdjacencyDepth, rerankBoundedMaxNodes, rerankBoundedMaxEdges, roots))
}

// mergeSortedUniqueIDs returns the union of the given ID sets, sorted and
// deduplicated, so a bounded build's root set (and the digest naming it) is
// deterministic for one request shape.
func mergeSortedUniqueIDs(sets ...[]string) []string {
	seen := make(map[string]struct{})
	out := make([]string, 0)
	for _, set := range sets {
		for _, id := range set {
			if id == "" {
				continue
			}
			if _, ok := seen[id]; ok {
				continue
			}
			seen[id] = struct{}{}
			out = append(out, id)
		}
	}
	sort.Strings(out)
	return out
}

// boundedSnapshotDigest names a bounded snapshot in a cache key: the caps that
// bounded it plus its root set. The IDs arrive sorted and deduplicated, and
// BuildBoundedAdjacencySnapshot sorts and dedupes its own roots before
// expanding, so one (caps, root set) pair names exactly one snapshot over a
// given reader — which is what makes an entry under this digest safe to serve
// to a later request of the same shape.
//
// The caps are folded in because two callers bound their CSR differently over
// one view (the closure path scales max_nodes with the root count; the rerank
// path uses flat caps) and the walk key itself says nothing about either.
func boundedSnapshotDigest(depth, maxNodes, maxEdges int, roots []string) string {
	if len(roots) == 0 {
		return ""
	}
	h := sha256.New()
	_, _ = h.Write([]byte("d" + strconv.Itoa(depth) + ":n" + strconv.Itoa(maxNodes) + ":e" + strconv.Itoa(maxEdges)))
	_, _ = h.Write([]byte{0})
	for _, id := range roots {
		_, _ = h.Write([]byte(id))
		_, _ = h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil)[:16])
}
