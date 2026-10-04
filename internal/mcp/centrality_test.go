package mcp

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/search/rerank"
)

// centralityTestServer is a server whose only wired state is the graph the
// rerank pass reads and an empty walk cache, so every cache statistic the
// assertions read is produced by the call under test.
func centralityTestServer(t *testing.T) *Server {
	t.Helper()
	return &Server{graph: walkTestGraph(t), pprCache: newPPRWalkCache()}
}

// The rerank pass runs one bounded CSR + one seeded walk per query. The walk
// is the expensive half, and keying caches by the selected snapshot identity
// left it uncacheable (the zero scope), so a repeated query re-walked the CSR
// every time. Memoising it under the snapshot's own identity restores the hit.
func TestBoundedCentralityForRequest_MemoisesTheWalkPerSnapshot(t *testing.T) {
	srv := centralityTestServer(t)
	ctx := context.Background()
	seeds := []string{"a.go::A"}
	candidates := []string{"a.go::A", "b.go::B", "c.go::C"}

	first := srv.boundedCentralityForRequest(ctx, seeds, candidates)
	require.NotEmpty(t, first.Scores, "the fixture must produce a scored neighbourhood")
	require.Positive(t, first.NodeCount)

	hits, misses, size, _, _ := srv.pprCache.stats()
	assert.Equal(t, 1, size, "the first walk must be memoised")
	assert.Zero(t, hits)
	assert.EqualValues(t, 1, misses)

	second := srv.boundedCentralityForRequest(ctx, seeds, candidates)
	assert.Equal(t, first.Scores, second.Scores, "the memoised walk must return the same scores")
	hits, _, size, _, _ = srv.pprCache.stats()
	assert.EqualValues(t, 1, hits, "a repeat of one query shape must hit its own entry")
	assert.Equal(t, 1, size, "…and must not add a second entry")

	// Telemetry is recomputed from the fresh bounded build, not memoised with
	// the walk: the caller reports the work IT did.
	assert.Equal(t, first.NodeCount, second.NodeCount)
	assert.Equal(t, first.EdgeCount, second.EdgeCount)
}

// The walk key is content-addressed on the seed neighbourhood, so a narrower
// and a wider candidate set over one graph can carry the same key over
// materially different CSRs. The root set is part of the snapshot's identity.
func TestBoundedCentralityForRequest_SeparatesCandidateSets(t *testing.T) {
	srv := centralityTestServer(t)
	ctx := context.Background()
	seeds := []string{"a.go::A"}

	require.NotEmpty(t, srv.boundedCentralityForRequest(ctx, seeds, []string{"a.go::A", "b.go::B"}).Scores)
	require.NotEmpty(t, srv.boundedCentralityForRequest(ctx, seeds, []string{"a.go::A", "b.go::B", "c.go::C"}).Scores)

	hits, _, size, _, _ := srv.pprCache.stats()
	assert.Equal(t, 2, size, "two bounded shapes must hold two entries")
	assert.Zero(t, hits, "the wider shape must not be answered by the narrower one's entry")

	// Order and duplicates do not change the root SET, and the bounded build
	// sorts and dedupes its roots, so the same set in another spelling hits.
	require.NotEmpty(t, srv.boundedCentralityForRequest(ctx, seeds,
		[]string{"b.go::B", "a.go::A", "b.go::B", ""}).Scores)
	hits, _, size, _, _ = srv.pprCache.stats()
	assert.EqualValues(t, 1, hits, "one root set in two spellings is one snapshot")
	assert.Equal(t, 2, size)
}

// Two views of one repository read different graphs. Their bounded CSRs can
// collide on the content-addressed walk key, so the view identity has to keep
// their entries apart — this is the mixed-view hazard, not a performance point.
func TestBoundedCentralityForRequest_NeverSharesAcrossViews(t *testing.T) {
	srv := centralityTestServer(t)
	seeds := []string{"a.go::A"}
	candidates := []string{"a.go::A", "b.go::B"}

	unrouted := context.Background()
	viewA := chainViewContext("c1", "fp1", "/wt/a", walkTestGraph(t))
	viewB := chainViewContext("c2", "fp2", "/wt/b", walkTestGraph(t))

	require.NotEmpty(t, srv.boundedCentralityForRequest(unrouted, seeds, candidates).Scores)
	require.NotEmpty(t, srv.boundedCentralityForRequest(viewA, seeds, candidates).Scores)
	require.NotEmpty(t, srv.boundedCentralityForRequest(viewB, seeds, candidates).Scores)

	hits, _, size, _, _ := srv.pprCache.stats()
	assert.Equal(t, 3, size, "the shared corpus and two routed views are three snapshots")
	assert.Zero(t, hits, "no view may be served another snapshot's scores")

	// Each identity still hits its own entry.
	require.NotEmpty(t, srv.boundedCentralityForRequest(viewA, seeds, candidates).Scores)
	hits, _, size, _, _ = srv.pprCache.stats()
	assert.EqualValues(t, 1, hits)
	assert.Equal(t, 3, size)
}

// Editor buffers are per-session mutable state no durable identity names, so
// a buffer-composed request must not share walk entries with anything.
func TestBoundedCentralityForRequest_EditorBuffersStayUncached(t *testing.T) {
	srv := centralityTestServer(t)
	ctx := WithOverlayView(context.Background(), graph.NewOverlaidView(walkTestGraph(t), nil))

	require.NotNil(t, srv.boundedCentralityForRequest(ctx, []string{"a.go::A"}, []string{"a.go::A", "b.go::B"}))
	require.NotNil(t, srv.boundedCentralityForRequest(ctx, []string{"a.go::A"}, []string{"a.go::A", "b.go::B"}))

	hits, _, size, _, _ := srv.pprCache.stats()
	assert.Zero(t, size, "a buffer-composed request must not enter the shared walk cache")
	assert.Zero(t, hits)
}

// The bounded build is a batched read loop with no context of its own, so an
// abandoned request is refused at the door rather than paying for a CSR that
// no response will carry.
func TestBoundedCentralityForRequest_AbandonedRequestBuildsNothing(t *testing.T) {
	srv := centralityTestServer(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	got := srv.boundedCentralityForRequest(ctx, []string{"a.go::A"}, []string{"a.go::A", "b.go::B"})
	assert.Zero(t, got.NodeCount, "a cancelled request must not build a CSR")
	assert.Empty(t, got.Scores)
	_, _, size, _, _ := srv.pprCache.stats()
	assert.Zero(t, size, "…and must not populate the walk cache")
}

// A request with no candidates names no snapshot, so it stays uncacheable
// rather than sharing the empty root set's namespace.
func TestBoundedCentralityScope_NoCandidatesIsUncacheable(t *testing.T) {
	assert.False(t, boundedCentralityScope(context.Background(), nil).cacheable)
	assert.False(t, boundedCentralityScope(context.Background(), []string{""}).cacheable)
	assert.True(t, boundedCentralityScope(context.Background(), []string{"a.go::A"}).cacheable)
}

// Production entrypoint. rerank.Context.Prepare reaches centrality through
// BatchedCentrality, which buildRerankContext wires to rerankBoundedCentrality
// (rerank_context.go:28-30) and which now delegates to the memoised path. Two
// identical searches must therefore walk once.
func TestRerankContext_BatchedCentralityReachesTheMemoisedWalk(t *testing.T) {
	store, _ := buildAnalysisCacheTestGraph(t, 80)
	defer store.Close()
	srv, metrics := populateAnalysisForTest(store)
	require.NoError(t, metrics.cacheSaveErr)
	srv.pprCache = newPPRWalkCache()

	ctx := context.Background()
	ids := []string{
		"repo::pkg0::HandleRequest0",
		"repo::pkg0::HandleRequest1",
		"repo::pkg0::HandleRequest2",
	}
	candidates := func() []*rerank.Candidate {
		out := make([]*rerank.Candidate, 0, len(ids))
		for _, id := range ids {
			node := store.GetNode(id)
			require.NotNil(t, node, "fixture node %s", id)
			out = append(out, &rerank.Candidate{Node: node})
		}
		return out
	}

	first := srv.buildRerankContext(ctx, "HandleRequest")
	first.Prepare(candidates())
	require.Positive(t, first.CentralityTelemetry().NodeCount, "bounded centrality did not run")

	_, _, size, _, _ := srv.pprCache.stats()
	require.Equal(t, 1, size, "the search rerank path must memoise its walk")

	second := srv.buildRerankContext(ctx, "HandleRequest")
	second.Prepare(candidates())
	hits, _, size, _, _ := srv.pprCache.stats()
	assert.EqualValues(t, 1, hits, "a repeated search must hit the memoised walk")
	assert.Equal(t, 1, size)
}

// The closure-proximity bounded build is the same uninterruptible batched read
// loop boundedCentralityForRequest guards, reached through a different caller.
// A cancelled request must be refused at the door: the nil snapshot is the
// already-supported "no proximity signal" degrade (rank by graph distance),
// so refusing costs nothing a live request would have received.
func TestRequestProximityAdjacency_AbandonedRequestBuildsNothing(t *testing.T) {
	srv := centralityTestServer(t)
	seeds := []string{"a.go::A"}
	scored := []string{"a.go::A", "b.go::B", "c.go::C"}

	// A live request over the same routed view does build a CSR — so the
	// refusal below is the guard, not an empty fixture.
	live := chainViewContext("c1", "fp1", "/wt/a", walkTestGraph(t))
	liveSnap, liveScope := srv.requestProximityAdjacency(live, seeds, scored)
	require.NotNil(t, liveSnap, "the fixture must produce a bounded CSR for a live request")
	require.True(t, liveScope.cacheable)

	cancelled, cancel := context.WithCancel(
		chainViewContext("c2", "fp2", "/wt/b", walkTestGraph(t)))
	cancel()

	snap, scope := srv.requestProximityAdjacency(cancelled, seeds, scored)
	assert.Nil(t, snap, "a cancelled request must not build a proximity CSR")
	assert.False(t, scope.cacheable, "…and must name no snapshot to cache under")
}

// Cross-consumer fence. The closure path and the rerank path bound their CSRs
// differently over one view — closure scales max_nodes with the root count
// (centrality.go:143-144), rerank uses flat caps (centrality.go:170-171) — and
// the content-addressed walk key says nothing about either. On a graph large
// enough for a cap to bind, the two snapshots are materially different graphs
// carrying the same walk key, so the caps have to be part of the namespace.
// The fixture is small enough that the caps do not bind, which is exactly why
// the entries must be kept apart by the digest rather than by the fixture.
func TestBoundedSnapshotDigest_ClosureAndRerankNeverShareAWalkEntry(t *testing.T) {
	srv := centralityTestServer(t)
	ctx := chainViewContext("c1", "fp1", "/wt/a", walkTestGraph(t))
	seeds := []string{"a.go::A"}
	scored := []string{"a.go::A", "b.go::B", "c.go::C"}

	// context_closure rank=proximity: bounded build + seeded walk under the
	// scope requestProximityAdjacency returns (tools_closure.go:224-231).
	snap, scope := srv.requestProximityAdjacency(ctx, seeds, scored)
	require.NotNil(t, snap)
	require.True(t, scope.cacheable)
	require.NotEmpty(t, srv.personalizedPageRankScoped(scope, snap, seeds))

	_, _, size, _, _ := srv.pprCache.stats()
	require.Equal(t, 1, size, "the closure walk must be memoised")

	// Same view, same root set, same seeds — a different bounding.
	require.NotEmpty(t, srv.boundedCentralityForRequest(ctx, seeds, scored).Scores)

	hits, _, size, _, _ := srv.pprCache.stats()
	assert.Zero(t, hits, "a differently-bounded snapshot must not read the closure's entry")
	assert.Equal(t, 2, size, "two boundings over one view and one root set are two snapshots")

	// The digest is what separates them: identical but for the caps.
	roots := mergeSortedUniqueIDs(scored, seeds)
	closureDigest := boundedSnapshotDigest(
		proximityAdjacencyDepth, len(roots)+proximityAdjacencyNodeHeadroom, 4*(len(roots)+proximityAdjacencyNodeHeadroom), roots)
	rerankDigest := boundedSnapshotDigest(
		proximityAdjacencyDepth, rerankBoundedMaxNodes, rerankBoundedMaxEdges, roots)
	assert.NotEqual(t, closureDigest, rerankDigest,
		"one root set under two cap sets must not produce one digest")
}
