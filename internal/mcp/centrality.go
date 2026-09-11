package mcp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"sort"

	"github.com/zzet/gortex/internal/analysis"
	"github.com/zzet/gortex/internal/graph"
)

// personalizedPageRank runs a Random-Walk-with-Restart (Personalized
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
// This entry point takes no context and so can name no snapshot identity for
// the walk it runs: its caller built the snapshot from whatever reader its
// request reads through — a routed view, a session's editor buffers, or the
// shared corpus — and the walk key is content-addressed on the seed
// neighbourhood only, so those are all one namespace. Rather than share it,
// the walk is computed uncached (the zero scope). A caller that CAN name the
// selected snapshot must call personalizedPageRankScoped, which is what
// restores caching for it.
func (s *Server) personalizedPageRank(snap *analysis.AdjacencySnapshot, seeds []string) map[string]float64 {
	return s.personalizedPageRankScoped(pprCacheScope{}, snap, seeds)
}

// personalizedPageRankScoped is personalizedPageRank with an explicit cache
// scope: every entry it stores or reads is namespaced by the snapshot that
// produced the walk, so one request's ranking can never be served from a
// different snapshot's cached scores. An uncacheable scope computes the walk
// and returns it without touching the cache at all.
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
		roots := mergeSortedUniqueIDs(scored, seeds)
		maxNodes := len(roots) + proximityAdjacencyNodeHeadroom
		snap, stats := analysis.BuildBoundedAdjacencySnapshot(
			reader, roots, proximityAdjacencyDepth, maxNodes, 4*maxNodes)
		if stats.NodeCount == 0 {
			return nil, pprCacheScope{}
		}
		// The root set is part of this snapshot's identity: the content-
		// addressed walk key describes the seed neighbourhood only, so two
		// closures over one view that bounded differently would otherwise
		// share an entry.
		return snap, walkCacheScope(ctx, pprWalkSourceSelectedReader).
			withRoots(boundedRootsDigest(roots))
	}
	snap := s.getAdjacency()
	if snap == nil {
		return nil, pprCacheScope{}
	}
	return snap, walkCacheScope(ctx, pprWalkSourceSharedAnalysis)
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

// boundedRootsDigest names a bounded snapshot's root set in a cache key. The
// IDs arrive sorted and deduplicated, so one root set has one digest.
func boundedRootsDigest(roots []string) string {
	if len(roots) == 0 {
		return ""
	}
	h := sha256.New()
	for _, id := range roots {
		_, _ = h.Write([]byte(id))
		_, _ = h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil)[:16])
}
