package resolver

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

// seedWarmCacheGraph builds a graph with enough distinct ids and shared names
// to push the parallel warm-cache helpers past their single-batch threshold
// (minLookupWarmBatch) and to exercise the per-name []*Node bucketing that the
// batch merge must preserve. Returns the graph plus the full id / name key
// sets the helpers will be asked to load.
func seedWarmCacheGraph(t *testing.T, n int) (graph.Store, []string, []string) {
	t.Helper()
	g := graph.New()
	ids := make([]string, 0, n)
	nameSet := make(map[string]struct{})
	for i := 0; i < n; i++ {
		// ~4 nodes share each name so the name lookup returns multi-element
		// buckets — the case the merge must not reorder or drop.
		name := fmt.Sprintf("sym%d", i%(n/4))
		id := fmt.Sprintf("pkg/f%d.go::%s", i, name)
		g.AddNode(&graph.Node{
			ID:       id,
			Kind:     graph.KindFunction,
			Name:     name,
			FilePath: fmt.Sprintf("pkg/f%d.go", i),
		})
		ids = append(ids, id)
		nameSet[name] = struct{}{}
	}
	names := make([]string, 0, len(nameSet))
	for nm := range nameSet {
		names = append(names, nm)
	}
	return g, ids, names
}

// TestWarmCacheParallel_GetNodesByIDsMatchesSerial pins the parallel id-batch
// helper to the serial Store call on a seeded fixture: the merged result must
// be byte-identical (same nodes, same absent keys for the injected misses).
func TestWarmCacheParallel_GetNodesByIDsMatchesSerial(t *testing.T) {
	g, ids, _ := seedWarmCacheGraph(t, 6000)
	// Interleave ids that don't exist so both paths agree on absent keys.
	query := make([]string, 0, len(ids)+4)
	query = append(query, ids...)
	query = append(query, "pkg/missing1.go::ghost", "pkg/missing2.go::ghost", "", "pkg/f0.go::sym0")

	require.Greater(t, lookupWarmBatches(len(query)), 0)

	r := New(g)
	serial := g.GetNodesByIDs(query)
	parallel := r.parallelGetNodesByIDs(query)

	assert.Equal(t, serial, parallel)
	assert.Len(t, parallel, len(ids)) // every real id present, ghosts + "" absent
}

// TestWarmCacheParallel_FindNodesByNamesMatchesSerial pins the parallel
// name-batch helper to the serial Store call, including the multi-node buckets
// and the injected missing names.
func TestWarmCacheParallel_FindNodesByNamesMatchesSerial(t *testing.T) {
	g, _, names := seedWarmCacheGraph(t, 6000)
	query := make([]string, 0, len(names)+3)
	query = append(query, names...)
	query = append(query, "nosuchname_a", "nosuchname_b", "")

	r := New(g)
	serial := g.FindNodesByNames(query)
	parallel := r.parallelFindNodesByNames(query)

	assert.Equal(t, serial, parallel)
	// Each present name bucketed all four of its nodes.
	for _, nm := range names {
		assert.Len(t, parallel[nm], 4, "name %q", nm)
	}
	_, missA := parallel["nosuchname_a"]
	assert.False(t, missA, "absent name must not appear in the result")
}

// TestWarmCacheParallel_WarmLookupCacheContents drives the full warmLookupCache
// over a seeded pending set and checks the assembled caches: every endpoint id
// and target name is present (or recorded as an authoritative negative), which
// is exactly what the parallel batching must preserve for the worker fast path.
func TestWarmCacheParallel_WarmLookupCacheContents(t *testing.T) {
	g, ids, _ := seedWarmCacheGraph(t, 6000)

	// One pending call edge per node, each From an existing node and To a bare
	// unresolved target name. Half the targets exist as node names, half don't
	// (authoritative negatives).
	pending := make([]*graph.Edge, 0, len(ids))
	for i, id := range ids {
		target := fmt.Sprintf("sym%d", i%(len(ids)/4))
		if i%2 == 0 {
			target = fmt.Sprintf("ghost%d", i) // no such node
		}
		pending = append(pending, &graph.Edge{
			From: id,
			To:   "unresolved::" + target,
			Kind: graph.EdgeCalls,
		})
	}

	r := New(g)
	if err := r.warmLookupCache(pending); err != nil {
		t.Fatal(err)
	}
	defer r.clearLookupCache()

	// Every edge endpoint id is cached to its node.
	for _, id := range ids {
		n, ok := r.nodeByID[id]
		require.True(t, ok, "id %q missing from warm id cache", id)
		require.NotNil(t, n)
		assert.Equal(t, id, n.ID)
	}
	// Every seeded source has unknown language, so the conservative exact-repo
	// fallback produces one authoritative grouped cache. Existing targets and
	// negative misses both live in that group; workers never fall through to a
	// per-edge store scan, and ordinary candidates are not duplicated globally.
	scope, languages := resolverNameScope("", "")
	require.Empty(t, languages, "unknown language must query all languages conservatively")
	grouped, ok := r.nodesByRepoLanguageName[scope]
	require.True(t, ok, "unknown-language repository group was not warmed")
	assert.NotEmpty(t, grouped["sym1"])
	neg, ok := grouped["ghost0"]
	require.True(t, ok, "authoritative grouped negative must be recorded for a missing target name")
	assert.Empty(t, neg)
	assert.Nil(t, r.nodesByName, "ordinary grouped candidates must not be duplicated globally")
}
