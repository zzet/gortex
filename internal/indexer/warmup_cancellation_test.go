package indexer

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/search"
)

type canceledWarmupReadStore struct{ graph.Store }

func (*canceledWarmupReadStore) AllNodes() []*graph.Node {
	panic("canceled startup admitted a whole-graph pass")
}

func TestEndBatchContextCancellationReleasesModesWithoutGlobalPass(t *testing.T) {
	t.Setenv("GORTEX_INDEX_SCOPED_GLOBAL_PASSES", "1")
	store := &canceledWarmupReadStore{Store: graph.New()}
	mi := NewMultiIndexer(store, parser.NewRegistry(), search.NewNull(), nil, zap.NewNop())
	idx := &Indexer{}
	mi.indexers["repo"] = idx
	mi.BeginParallelBatch()
	mi.ArmBatchScope(map[string]struct{}{"repo": {}})
	mi.ArmBatchCensusEligible()
	require.True(t, idx.deferGlobalPasses.Load())
	require.True(t, idx.deferResolve.Load())
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	mi.EndBatchContext(ctx)
	require.False(t, mi.deferGlobalPasses)
	require.False(t, mi.deferResolve)
	require.Nil(t, mi.batchChangedPrefixes)
	require.False(t, mi.batchCensusEligible)
	require.False(t, idx.deferGlobalPasses.Load())
	require.False(t, idx.deferResolve.Load())
	// The transition gate must be released, not merely its boolean flags.
	require.True(t, mi.batchMutationGate.TryLock())
	mi.batchMutationGate.Unlock()
}

func TestRunGlobalResolveContextRejectsCanceledProducerBeforeGraphRead(t *testing.T) {
	store := &canceledWarmupReadStore{Store: graph.New()}
	mi := NewMultiIndexer(store, parser.NewRegistry(), search.NewNull(), nil, zap.NewNop())
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	require.ErrorIs(t, mi.RunGlobalResolveContext(ctx), context.Canceled)
}
