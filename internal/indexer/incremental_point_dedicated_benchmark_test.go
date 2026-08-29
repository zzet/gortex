package indexer

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestIncrementalPointDedicatedRouteBenchmarkCounters(t *testing.T) {
	f := newFamilyFixture(t, "point-dedicated-benchmark")
	defer f.close()
	ctx := context.Background()
	dirtyPath := filepath.Base(f.worktree) + ".go"

	promoted, err := f.lc.PromoteCheckout(ctx, f.automatic.CheckoutID, TrackSourceMCP)
	require.NoError(t, err)
	require.NotNil(t, promoted.Index)
	dedicatedBefore, found, err := f.catalog.GetDedicatedGraph(ctx, promoted.GraphID)
	require.NoError(t, err)
	require.True(t, found)
	baseBefore := contentIdentities(f.store.AtGeneration(dedicatedBefore.ActiveGenerationID), promoted.Prefix)

	var pointCalls int64
	var fullFallbackResults int64
	var routeErrors int64
	bench := testing.Benchmark(func(b *testing.B) {
		pointCalls = 0
		fullFallbackResults = 0
		routeErrors = 0
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			result, callErr := f.mi.incrementalPointRepoRaw(promoted.Prefix, filepath.Join(f.worktree, dirtyPath))
			pointCalls++
			if callErr != nil {
				routeErrors++
			}
			if result != nil {
				fullFallbackResults++
			}
		}
	})

	require.Equal(t, int64(bench.N), pointCalls)
	assert.Zero(t, routeErrors)
	assert.Zero(t, fullFallbackResults, "the dedicated guard must never enter point or full-index mutation")
	dedicatedAfter, found, err := f.catalog.GetDedicatedGraph(ctx, promoted.GraphID)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, dedicatedBefore.ActiveGenerationID, dedicatedAfter.ActiveGenerationID)
	assert.Equal(t, baseBefore, contentIdentities(f.store.AtGeneration(dedicatedAfter.ActiveGenerationID), promoted.Prefix))
	assert.Empty(t, contentIdentities(f.store, promoted.Prefix), "benchmark calls must leave generation zero empty")
	t.Logf("dedicated point guard: %s; %s; calls=%d full_fallback_results=%d route_errors=%d", bench.String(), bench.MemString(), pointCalls, fullFallbackResults, routeErrors)
}
