package mcp

import (
	"context"
	"path/filepath"
	"slices"
	"sort"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/search/rerank"
)

// openContractCoreExclusionStore holds one endpoint of every owned kind, the
// core kinds that share their edge kinds, and edges visible() drops on its own.
func openContractCoreExclusionStore(t *testing.T) *store_sqlite.Store {
	t.Helper()
	store, err := store_sqlite.Open(filepath.Join(t.TempDir(), "exclusions.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	store.AddBatch([]*graph.Node{
		{ID: "a", Kind: graph.KindFunction}, {ID: "b", Kind: graph.KindFunction}, {ID: "c", Kind: graph.KindFunction},
		{ID: "contract", Kind: graph.KindContract}, {ID: "bridge", Kind: graph.KindContractBridge}, {ID: "config", Kind: graph.KindConfigKey},
		{ID: "table", Kind: graph.KindTable}, {ID: "unknown", Kind: graph.NodeKind("future_kind")},
	}, []*graph.Edge{
		{From: "a", To: "b", Kind: graph.EdgeCalls, Line: 1, Origin: graph.OriginLSPResolved, Confidence: 0.8, Meta: map[string]any{"evidence": "keep"}},
		{From: "b", To: "c", Kind: graph.EdgeReferences, Line: 2},
		{From: "a", To: "b", Kind: graph.EdgeCalls, Line: 3, Meta: map[string]any{"via": "spring.Bean"}},
		{From: "a", To: "contract", Kind: graph.EdgeProvides},
		{From: "bridge", To: "a", Kind: graph.EdgeCalls},
		{From: "a", To: "config", Kind: graph.EdgeCalls},
		{From: "config", To: "c", Kind: graph.EdgeReferences},
		{From: "a", To: "table", Kind: graph.EdgeProvides},
		{From: "a", To: "unknown", Kind: graph.EdgeReferences},
		{From: "a", To: "missing", Kind: graph.EdgeCalls},
		{From: "a", To: "c", Kind: graph.EdgeReadsConfig},
	})
	return store
}

func contractCoreExclusionReader(reader graph.Reader, ctx context.Context, cache *contractCoreExclusionCache, timing *rerank.CoreEdgeTiming) graph.Reader {
	return wrapContractCoreEdges(&contractCoreEdges{Reader: reader, ctx: ctx, edgeTiming: timing, exclusions: cache})
}

type contractCoreEdgeReads struct {
	out, in, all, calls []*graph.Edge
	outBatch, inBatch   map[string][]*graph.Edge
}

// readContractCoreEdges reads through every filtered adjacency method.
func readContractCoreEdges(r graph.Reader) contractCoreEdgeReads {
	ids := []string{"a", "b", "c", "contract", "bridge", "config", "table", "unknown", "missing"}
	var calls []*graph.Edge
	for edge := range r.EdgesByKind(graph.EdgeCalls) {
		calls = append(calls, edge)
	}
	return contractCoreEdgeReads{
		out: r.GetOutEdges("a"), in: r.GetInEdges("c"), all: r.AllEdges(), calls: calls,
		outBatch: r.GetOutEdgesByNodeIDs(ids), inBatch: r.GetInEdgesByNodeIDs(ids),
	}
}

func contractCoreOutTargets(r graph.Reader, id string) []string {
	var out []string
	for _, edge := range r.GetOutEdges(id) {
		out = append(out, edge.To)
	}
	sort.Strings(out)
	return out
}

// The cached identity set classifies exactly like the per-batch membership
// read, per generation, for every adjacency method and for bound handles.
func TestContractCoreExclusionSetFiltersLikePerBatchClassifier(t *testing.T) {
	store := openContractCoreExclusionStore(t)
	derived := store.AtGeneration(7)
	derived.AddBatch([]*graph.Node{
		{ID: "a", Kind: graph.KindFunction}, {ID: "contract", Kind: graph.KindFunction}, {ID: "b", Kind: graph.KindConfigKey},
	}, []*graph.Edge{{From: "a", To: "contract", Kind: graph.EdgeCalls}, {From: "a", To: "b", Kind: graph.EdgeCalls}})
	ctx := withContractCoreReadErrors(t.Context())
	cache := newContractCoreExclusionCache(contractCoreExclusionLimit)
	for _, handle := range []*store_sqlite.Store{store, derived} {
		want := readContractCoreEdges(contractCoreExclusionReader(handle, ctx, nil, nil))
		var timing rerank.CoreEdgeTiming
		cached := contractCoreExclusionReader(handle, ctx, cache, &timing)
		for range 3 {
			require.Equal(t, want, readContractCoreEdges(cached))
		}
		require.Positive(t, timing.EndpointCacheHits)
		reads := timing.EndpointReads
		bound := graph.BindReadContext(cached, ctx)
		require.Equal(t, want, readContractCoreEdges(bound))
		require.Equal(t, reads, timing.EndpointReads, "a request-bound handle shares its generation's set")
	}
	require.Equal(t, []string{"b", "missing", "table", "unknown"}, contractCoreOutTargets(contractCoreExclusionReader(store, ctx, cache, nil), "a"))
	require.Equal(t, []string{"contract"}, contractCoreOutTargets(contractCoreExclusionReader(derived, ctx, cache, nil), "a"),
		"each generation is classified by its own kinds")
	require.NoError(t, contractCoreReadError(ctx))
}

// After a revision's set is read, batches classify without a statement until
// the store changes.
func TestContractCoreExclusionSetClassifiesWarmBatchesWithoutReads(t *testing.T) {
	store := openContractCoreExclusionStore(t)
	ctx := withContractCoreReadErrors(t.Context())
	var timing rerank.CoreEdgeTiming
	reader := contractCoreExclusionReader(store, ctx, newContractCoreExclusionCache(contractCoreExclusionLimit), &timing)
	reader.GetOutEdges("a")
	require.Equal(t, 1, timing.EndpointReads, "the first batch at a revision keeps its membership read")
	reader.GetOutEdges("a")
	require.Equal(t, 2, timing.EndpointReads, "the second batch reads the whole set once")
	require.Zero(t, timing.EndpointCacheHits)
	const warm = 10
	ids := []string{"a", "b", "c", "config"}
	for range warm {
		reader.GetOutEdges("a")
		reader.GetInEdges("c")
		reader.GetOutEdgesByNodeIDs(ids)
		reader.GetInEdgesByNodeIDs(ids)
	}
	require.Equal(t, 2, timing.EndpointReads, "warm batches issue no classification statement")
	require.Equal(t, 4*warm, timing.EndpointCacheHits)
	require.Equal(t, 2+4*warm, timing.CheckedClassifiers)
	require.Zero(t, timing.LegacyClassifiers)

	store.AddNode(&graph.Node{ID: "c", Kind: graph.KindFunction, Name: "c"})
	reader.GetOutEdges("a")
	require.Equal(t, 3, timing.EndpointReads, "a write retires the set")
}

// Every request wrapper the server builds shares the set: a later request's
// batches read nothing.
func TestContractCoreExclusionSetIsSharedAcrossRequests(t *testing.T) {
	store := openContractCoreExclusionStore(t)
	cache := newContractCoreExclusionCache(contractCoreExclusionLimit)
	srv := &Server{graph: store, contractCoreExclusions: cache}
	installContractCoreKindTestRuntime(t, srv)
	want := readContractCoreEdges(contractCoreExclusionReader(store, withContractCoreReadErrors(t.Context()), nil, nil))
	request := func() rerank.CoreEdgeTiming {
		var timing rerank.CoreEdgeTiming
		ctx := withCoreEdgeTiming(withContractCoreReadErrors(t.Context()), &timing)
		require.Equal(t, want.in, srv.readerFor(ctx).GetInEdges("c"))
		require.NoError(t, contractCoreReadError(ctx))
		return timing
	}
	request()
	request()
	for range 3 {
		timing := request()
		require.Zero(t, timing.EndpointReads)
		require.Equal(t, 1, timing.EndpointCacheHits)
		require.Equal(t, 1, timing.CheckedClassifiers)
	}
	_, key, _, ok := contractCoreExclusionSource(store)
	require.True(t, ok)
	require.Len(t, cache.entries, 1)
	require.NotNil(t, cache.entries[key].ids, "the server's requests share its own cache")
}

// A set read while a write commits answers its own batch, as a per-ID lookup
// made at that moment would, but is not kept: the write's revision reads its
// own set.
func TestContractCoreExclusionSetReadAcrossAWriteIsNotKept(t *testing.T) {
	store := openContractCoreExclusionStore(t)
	ctx := withContractCoreReadErrors(t.Context())
	cache := newContractCoreExclusionCache(contractCoreExclusionLimit)
	reads := 0
	cache.read = func(s *store_sqlite.Store, ctx context.Context, kinds []graph.NodeKind, limit int) ([]string, bool, error) {
		ids, complete, err := s.NodeIDsByKindsBoundedContext(ctx, kinds, limit)
		reads++
		if reads == 1 {
			// b becomes owned after the first set was read.
			s.AddNode(&graph.Node{ID: "b", Kind: graph.KindConfigKey})
		}
		return ids, complete, err
	}
	var timing rerank.CoreEdgeTiming
	reader := contractCoreExclusionReader(store, ctx, cache, &timing)
	_, key, before, ok := contractCoreExclusionSource(store)
	require.True(t, ok)

	require.Equal(t, []string{"b", "missing", "table", "unknown"}, contractCoreOutTargets(reader, "a"))
	require.Zero(t, reads, "the first sighting keeps its membership read")
	require.Equal(t, []string{"b", "missing", "table", "unknown"}, contractCoreOutTargets(reader, "a"), "the set answers as of its read")
	require.Equal(t, 1, reads)
	entry := cache.entries[key]
	require.NotNil(t, entry)
	require.Equal(t, before, entry.revision)
	require.Nil(t, entry.ids, "a set read across a write is not kept")
	require.False(t, entry.building)
	require.False(t, entry.skip)

	for range 3 {
		require.Equal(t, []string{"missing", "table", "unknown"}, contractCoreOutTargets(reader, "a"), "the write's revision sees b owned")
	}
	require.Equal(t, 2, reads, "the write's revision reads its own set once")
	_, _, after, ok := contractCoreExclusionSource(store)
	require.True(t, ok)
	require.Greater(t, after, before)
	entry = cache.entries[key]
	require.Equal(t, after, entry.revision)
	require.NotNil(t, entry.ids)
	require.Equal(t, 1, timing.EndpointCacheHits)
	require.NoError(t, contractCoreReadError(ctx))
}

// Adding or removing an owned endpoint takes effect on the very next batch.
func TestContractCoreExclusionSetFollowsEndpointKindMutations(t *testing.T) {
	store := openContractCoreExclusionStore(t)
	ctx := withContractCoreReadErrors(t.Context())
	var timing rerank.CoreEdgeTiming
	reader := contractCoreExclusionReader(store, ctx, newContractCoreExclusionCache(contractCoreExclusionLimit), &timing)
	warm := func(want []string) {
		t.Helper()
		hits := timing.EndpointCacheHits
		for range 3 {
			require.Equal(t, want, contractCoreOutTargets(reader, "a"))
		}
		require.Greater(t, timing.EndpointCacheHits, hits)
	}
	warm([]string{"b", "missing", "table", "unknown"})

	store.AddNode(&graph.Node{ID: "missing", Kind: graph.KindContract})
	require.Equal(t, []string{"b", "table", "unknown"}, contractCoreOutTargets(reader, "a"), "a new owned node hides its edge at once")
	warm([]string{"b", "table", "unknown"})

	store.AddNode(&graph.Node{ID: "b", Kind: graph.KindConfigKey})
	require.Equal(t, []string{"table", "unknown"}, contractCoreOutTargets(reader, "a"), "a node that becomes owned hides its edges at once")
	warm([]string{"table", "unknown"})

	store.AddNode(&graph.Node{ID: "contract", Kind: graph.KindFunction})
	store.AddNode(&graph.Node{ID: "config", Kind: graph.KindFunction})
	require.Equal(t, []string{"config", "contract", "table", "unknown"}, contractCoreOutTargets(reader, "a"), "an endpoint that stops being owned shows its edges at once")
	warm([]string{"config", "contract", "table", "unknown"})
	require.NoError(t, contractCoreReadError(ctx))
}

// A failed set read is the batch's checked read failure: recorded, nothing
// returned, and the next batch reads again.
func TestContractCoreExclusionSetReadFailureIsRecorded(t *testing.T) {
	t.Run("cancelled", func(t *testing.T) {
		store := openContractCoreExclusionStore(t)
		cache := newContractCoreExclusionCache(contractCoreExclusionLimit)
		live := withContractCoreReadErrors(t.Context())
		contractCoreExclusionReader(store, live, cache, nil).GetOutEdges("a")

		ctx, cancel := context.WithCancel(t.Context())
		ctx = withContractCoreReadErrors(ctx)
		cancel()
		var timing rerank.CoreEdgeTiming
		require.Empty(t, contractCoreExclusionReader(store, ctx, cache, &timing).GetOutEdges("a"))
		require.ErrorIs(t, contractCoreReadError(ctx), context.Canceled)
		require.Equal(t, 1, timing.EndpointReads)

		var liveTiming rerank.CoreEdgeTiming
		reader := contractCoreExclusionReader(store, live, cache, &liveTiming)
		for range 3 {
			require.Equal(t, []string{"b", "missing", "table", "unknown"}, contractCoreOutTargets(reader, "a"))
		}
		require.Equal(t, 1, liveTiming.EndpointReads, "the abandoned read left the set to the next batch")
		require.Equal(t, 2, liveTiming.EndpointCacheHits)
		require.NoError(t, contractCoreReadError(live))

		// A cached answer still ends with its request, as a read would.
		ended, end := context.WithCancel(t.Context())
		ended = withContractCoreReadErrors(ended)
		end()
		require.Empty(t, contractCoreExclusionReader(store, ended, cache, nil).GetOutEdges("a"))
		require.ErrorIs(t, contractCoreReadError(ended), context.Canceled)
	})
	t.Run("closed", func(t *testing.T) {
		store := openContractCoreExclusionStore(t)
		cache := newContractCoreExclusionCache(contractCoreExclusionLimit)
		ctx := withContractCoreReadErrors(t.Context())
		reader := contractCoreExclusionReader(store, ctx, cache, nil)
		rows := store.GetOutEdges("a")
		require.Len(t, reader.GetOutEdges("a"), 4)
		require.NoError(t, store.Close())
		require.Nil(t, reader.(contractCoreWrapped).contractCore().filter(rows))
		require.Error(t, contractCoreReadError(ctx))
	})
}

// Potential writers keep the errorless full-node classifier; the cache is
// never consulted for them.
func TestContractCoreExclusionSetLeavesLegacyClassifierAlone(t *testing.T) {
	store := openContractCoreExclusionStore(t)
	cache := newContractCoreExclusionCache(contractCoreExclusionLimit)
	var timing rerank.CoreEdgeTiming
	reader := contractCoreExclusionReader(store, t.Context(), cache, &timing)
	for range 3 {
		require.Equal(t, []string{"b", "missing", "table", "unknown"}, contractCoreOutTargets(reader, "a"))
	}
	require.Equal(t, 3, timing.LegacyClassifiers)
	require.Equal(t, 3, timing.EndpointReads)
	require.Zero(t, timing.CheckedClassifiers)
	require.Zero(t, timing.EndpointCacheHits)
	require.Empty(t, cache.entries)
}

// A set past the bound is not kept: batches keep their membership reads, and
// the set is not reread until the store changes.
func TestContractCoreExclusionSetPastBoundKeepsPerBatchReads(t *testing.T) {
	store := openContractCoreExclusionStore(t)
	ctx := withContractCoreReadErrors(t.Context())
	cache := newContractCoreExclusionCache(2)
	want := readContractCoreEdges(contractCoreExclusionReader(store, ctx, nil, nil))
	var timing rerank.CoreEdgeTiming
	reader := contractCoreExclusionReader(store, ctx, cache, &timing)
	for range 3 {
		require.Equal(t, want, readContractCoreEdges(reader))
	}
	require.Zero(t, timing.EndpointCacheHits)
	require.Equal(t, timing.CheckedClassifiers, timing.EndpointReads)
	_, key, revision, ok := contractCoreExclusionSource(store)
	require.True(t, ok)
	entry := cache.entries[key]
	require.NotNil(t, entry)
	require.Equal(t, revision, entry.revision)
	require.True(t, entry.skip)
	require.Nil(t, entry.ids)
	require.False(t, entry.building)

	store.AddNode(&graph.Node{ID: "bridge", Kind: graph.KindFunction})
	hits := timing.EndpointCacheHits
	for range 3 {
		require.Equal(t, []string{"b", "missing", "table", "unknown"}, contractCoreOutTargets(reader, "a"))
	}
	require.Greater(t, timing.EndpointCacheHits, hits, "a set within the bound after a write is kept")
}

// The cache keeps a bounded number of readers, dropping the least recently
// used.
func TestContractCoreExclusionCacheKeepsBoundedReaders(t *testing.T) {
	cache := newContractCoreExclusionCache(contractCoreExclusionLimit)
	key := func(i int) contractCoreExclusionKey { return contractCoreExclusionKey{core: i} }
	for i := range contractCoreExclusionReaders {
		cache.claim(key(i), 1)
	}
	cache.claim(key(0), 1)
	cache.claim(key(contractCoreExclusionReaders), 1)
	require.Len(t, cache.entries, contractCoreExclusionReaders)
	require.Contains(t, cache.entries, key(0))
	require.NotContains(t, cache.entries, key(1))
	require.Contains(t, cache.entries, key(contractCoreExclusionReaders))
}

// Requests share one set while a writer flips an endpoint's kind; once the
// writer stops, every request sees its final kind.
func TestContractCoreExclusionSetConcurrentRequestsAndWrites(t *testing.T) {
	store := openContractCoreExclusionStore(t)
	cache := newContractCoreExclusionCache(contractCoreExclusionLimit)
	withB := []string{"b", "missing", "table", "unknown"}
	withoutB := []string{"missing", "table", "unknown"}
	var wg sync.WaitGroup
	done := make(chan struct{})
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-done:
					return
				default:
				}
				ctx := withContractCoreReadErrors(t.Context())
				got := contractCoreOutTargets(contractCoreExclusionReader(store, ctx, cache, nil), "a")
				if !slices.Equal(got, withB) && !slices.Equal(got, withoutB) {
					t.Errorf("unexpected targets %v", got)
					return
				}
				if err := contractCoreReadError(ctx); err != nil {
					t.Errorf("unexpected read error: %v", err)
					return
				}
			}
		}()
	}
	for i := range 20 {
		kind := graph.KindConfigKey
		if i%2 == 1 {
			kind = graph.KindFunction
		}
		store.AddNode(&graph.Node{ID: "b", Kind: kind})
	}
	close(done)
	wg.Wait()
	ctx := withContractCoreReadErrors(t.Context())
	for range 3 {
		require.Equal(t, withB, contractCoreOutTargets(contractCoreExclusionReader(store, ctx, cache, nil), "a"))
	}
	require.NoError(t, contractCoreReadError(ctx))
}
